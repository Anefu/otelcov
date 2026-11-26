// Package otelcolcontrib is the stage plugin for the otelcol-contrib
// collector distribution at the collector-contrib version otelcov's go.mod
// pins (decisions 66 to 68). It runs in its own process
// (cmd/otelcov-plugin-otelcol-contrib) and models the distribution's
// processors: the real collector-contrib code where otelcov runs it,
// otelcov's own model of k8sattributes (held to upstream's tests),
// resourcedetection's recorded results, and batch and memory_limiter as
// pass-throughs.
package otelcolcontrib

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"go.opentelemetry.io/collector/featuregate"
	corev1 "k8s.io/api/core/v1"

	"github.com/Anefu/otelcov/internal/collector"
	"github.com/Anefu/otelcov/internal/graph"
	"github.com/Anefu/otelcov/internal/kube"
	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/nodes/k8sattributes"
	"github.com/Anefu/otelcov/internal/nodes/realproc"
	"github.com/Anefu/otelcov/internal/nodes/resourcedetection"
	"github.com/Anefu/otelcov/internal/pluginwire"
	"github.com/Anefu/otelcov/internal/version"
	"github.com/Anefu/otelcov/pkg/plugin/pluginv1"
)

// Distribution is the collector distribution this plugin models.
const Distribution = "otelcol-contrib"

// The deployed versions this plugin has been reviewed against (decision 66:
// changelogs and code of every modeled component from 0.134 to 0.139).
const (
	ReviewedMin = "0.134"
	ReviewedMax = "0.139"
)

// Server implements the stage plugin protocol.
type Server struct {
	pluginv1.UnimplementedStagePluginServer

	mu     sync.Mutex
	inited bool
	in     inputs
	ids    *lineage.IDs
	stages map[string]graph.MetricsNode
	next   int
}

type inputs struct {
	cluster           kube.Objects
	snapshotPods      []corev1.Pod
	resourceDetection map[string]map[string]resourcedetection.Recorded
}

// New returns a server with no stages.
func New() *Server {
	return &Server{ids: lineage.NewIDs("c"), stages: map[string]graph.MetricsNode{}}
}

func (s *Server) Describe(context.Context, *pluginv1.DescribeRequest) (*pluginv1.DescribeResponse, error) {
	contrib := version.Module(version.ContribModule)
	resp := &pluginv1.DescribeResponse{
		Distribution: Distribution,
		Version:      strings.TrimPrefix(contrib, "v"),
		Reviewed:     &pluginv1.VersionRange{Min: ReviewedMin, Max: ReviewedMax},
		Modules:      map[string]string{"collector-contrib": contrib, "pdata": version.Module(version.PdataModule)},
	}
	for _, t := range realproc.Types() {
		resp.Components = append(resp.Components, &pluginv1.Component{Kind: "processor", Type: t, Fidelity: pluginv1.Fidelity_FIDELITY_REAL})
	}
	resp.Components = append(resp.Components,
		// Held to upstream's own test cases, ported (decisions 51 and 69).
		&pluginv1.Component{Kind: "processor", Type: "k8sattributes", Fidelity: pluginv1.Fidelity_FIDELITY_REIMPLEMENTATION, Conformance: true},
		// Applies what the real processor detected when the data was recorded
		// (decision 71).
		&pluginv1.Component{Kind: "processor", Type: "resourcedetection", Fidelity: pluginv1.Fidelity_FIDELITY_REAL},
		// Data passes through; batching and load shedding aren't modeled.
		&pluginv1.Component{Kind: "processor", Type: "batch", Fidelity: pluginv1.Fidelity_FIDELITY_APPROXIMATION, Note: "passes data through"},
		&pluginv1.Component{Kind: "processor", Type: "memory_limiter", Fidelity: pluginv1.Fidelity_FIDELITY_APPROXIMATION, Note: "passes data through"},
	)
	return resp, nil
}

func (s *Server) Init(_ context.Context, req *pluginv1.InitRequest) (*pluginv1.InitResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inited {
		return nil, errors.New("Init was already called; start a new plugin process per run")
	}
	// Feature gates are process-wide, as in the collector.
	for _, g := range req.GetFeatureGates() {
		id, on := g, true
		if g != "" && g[0] == '-' {
			id, on = g[1:], false
		}
		if err := featuregate.GlobalRegistry().Set(id, on); err != nil {
			return nil, fmt.Errorf("feature gate %s: %w", id, err)
		}
	}
	var in inputs
	if b := req.GetCluster(); len(b) > 0 {
		o, _, err := kube.Parse(b, "cluster")
		if err != nil {
			return nil, err
		}
		in.cluster = o
	}
	if b := req.GetSnapshotPods(); len(b) > 0 {
		o, _, err := kube.Parse(b, "snapshot pods")
		if err != nil {
			return nil, err
		}
		in.snapshotPods = o.Pods
	}
	if b := req.GetResourceDetection(); len(b) > 0 {
		if err := json.Unmarshal(b, &in.resourceDetection); err != nil {
			return nil, fmt.Errorf("resource detection: %w", err)
		}
	}
	s.in, s.inited = in, true
	return &pluginv1.InitResponse{}, nil
}

func (s *Server) Configure(ctx context.Context, req *pluginv1.ConfigureRequest) (*pluginv1.ConfigureResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.GetKind() != "processor" {
		return &pluginv1.ConfigureResponse{Unsupported: req.GetKind() + "s aren't modeled by this plugin"}, nil
	}
	var raw map[string]any
	if b := req.GetConfig(); len(b) > 0 {
		if err := json.Unmarshal(b, &raw); err != nil {
			return nil, fmt.Errorf("%s: config: %w", req.GetComponentId(), err)
		}
	}
	id, name := req.GetComponentId(), req.GetNodeName()
	inst := req.GetInstance()
	var (
		n        graph.MetricsNode
		err      error
		fidelity = pluginv1.Fidelity_FIDELITY_REIMPLEMENTATION
	)
	switch typ := collector.Type(id); {
	case typ == "batch", typ == "memory_limiter":
		// They only regroup or shed load; they don't change data.
		return &pluginv1.ConfigureResponse{PassThrough: true, Fidelity: pluginv1.Fidelity_FIDELITY_APPROXIMATION}, nil
	case typ == "k8sattributes":
		n, err = k8sattributes.New(name, raw, s.in.cluster, k8sattributes.Instance{Node: inst.GetNode(), Env: env(inst.GetEnv())})
	case typ == "resourcedetection":
		var rec *resourcedetection.Recorded
		if v, ok := s.in.resourceDetection[inst.GetCollector()][id]; ok {
			rec = &v
		}
		n, err = resourcedetection.New(name, raw, rec, s.in.snapshotPods, inst.GetNode())
		fidelity = pluginv1.Fidelity_FIDELITY_REAL
	case realproc.Supported(typ):
		n, err = realproc.New(ctx, name, id, raw, s.ids)
		fidelity = pluginv1.Fidelity_FIDELITY_REAL
	default:
		return &pluginv1.ConfigureResponse{Unsupported: "processor type " + typ + " isn't modeled yet", Fidelity: pluginv1.Fidelity_FIDELITY_UNMODELLED}, nil
	}
	if err != nil {
		return &pluginv1.ConfigureResponse{Unsupported: err.Error(), Fidelity: fidelity}, nil
	}
	s.next++
	handle := strconv.Itoa(s.next)
	s.stages[handle] = n
	return &pluginv1.ConfigureResponse{Handle: handle, Fidelity: fidelity}, nil
}

func (s *Server) Apply(ctx context.Context, req *pluginv1.ApplyRequest) (*pluginv1.ApplyResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.stages[req.GetHandle()]
	if !ok {
		return nil, fmt.Errorf("no stage %q", req.GetHandle())
	}
	items, err := pluginwire.FromUnits(req.GetUnits())
	if err != nil {
		return nil, err
	}
	s.ids.SetNextNumber(req.GetNextId())
	out, err := n.ProcessMetrics(ctx, items)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", n.Name(), err)
	}
	resp := &pluginv1.ApplyResponse{IdsUsed: s.ids.NextNumber() - req.GetNextId()}
	if resp.Units, err = pluginwire.Units(out.Items); err != nil {
		return nil, err
	}
	if resp.Events, err = pluginwire.Events(out.Events); err != nil {
		return nil, err
	}
	if resp.Signals, err = pluginwire.Signals(out.Signals); err != nil {
		return nil, err
	}
	return resp, nil
}

func (s *Server) Close(ctx context.Context, req *pluginv1.CloseRequest) (*pluginv1.CloseResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	handles := req.GetHandles()
	if len(handles) == 0 {
		for h := range s.stages {
			handles = append(handles, h)
		}
	}
	var errs []error
	for _, h := range handles {
		if sd, ok := s.stages[h].(graph.Shutdowner); ok {
			errs = append(errs, sd.Shutdown(ctx))
		}
		delete(s.stages, h)
	}
	return &pluginv1.CloseResponse{}, errors.Join(errs...)
}

func env(in map[string]*pluginv1.EnvVar) map[string]collector.EnvVar {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]collector.EnvVar, len(in))
	for k, v := range in {
		out[k] = collector.EnvVar{Value: v.GetValue(), FieldRef: v.GetFieldRef()}
	}
	return out
}
