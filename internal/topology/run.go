package topology

import (
	"context"
	"fmt"

	"go.opentelemetry.io/collector/featuregate"
	"go.opentelemetry.io/collector/pdata/pmetric"
	corev1 "k8s.io/api/core/v1"

	"github.com/Anefu/otelcov/internal/collector"
	"github.com/Anefu/otelcov/internal/graph"
	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/nodes/k8sattributes"
	"github.com/Anefu/otelcov/internal/nodes/realproc"
	"github.com/Anefu/otelcov/internal/nodes/resourcedetection"
	"github.com/Anefu/otelcov/internal/unit"
)

// copyNode names lineage for copies made where data fans out.
const copyNode = "topology"

// Inputs are what a run needs besides the topology.
type Inputs struct {
	// Pods in the config under test (k8sattributes looks them up).
	Pods []corev1.Pod
	// SnapshotPods were running at recording time.
	SnapshotPods []corev1.Pod
	// ResourceDetection holds recorded detections per collector, then
	// processor ID.
	ResourceDetection map[string]map[string]resourcedetection.Recorded
}

// Runner moves items through the collectors, following joins.
type Runner struct {
	f       *File
	v       *Validation
	in      Inputs
	log     *lineage.Log
	ids     *lineage.IDs
	configs map[string]*collector.Config
	nodes   map[string][]graph.MetricsNode // per collector/pipeline
	// Delivered collects, per backend, the items that reach it.
	Delivered map[string][]unit.Metric
}

// NewRunner prepares a run. Feature gates of all collectors are applied to
// the process-wide registry (Validate rejects conflicting settings).
func NewRunner(f *File, v *Validation, in Inputs, log *lineage.Log) (*Runner, error) {
	r := &Runner{f: f, v: v, in: in, log: log, ids: lineage.NewIDs("c"),
		configs: map[string]*collector.Config{}, nodes: map[string][]graph.MetricsNode{}, Delivered: map[string][]unit.Metric{}}
	for _, name := range Names(f.Collectors) {
		cfg, err := f.LoadCollector(name)
		if err != nil {
			return nil, err
		}
		r.configs[name] = cfg
		for _, g := range f.Collectors[name].FeatureGates {
			id, on := g, true
			if g != "" && g[0] == '-' {
				id, on = g[1:], false
			}
			if err := featuregate.GlobalRegistry().Set(id, on); err != nil {
				return nil, fmt.Errorf("%s: feature gate %s: %w", name, id, err)
			}
		}
	}
	return r, nil
}

// Run feeds recorded items into the tapped collectors and follows them to the
// backends.
func (r *Runner) Run(ctx context.Context, items []unit.Metric) error {
	defer r.shutdown(ctx)
	for _, name := range Names(r.f.Collectors) {
		c := r.f.Collectors[name]
		if c.Tap == "" {
			continue
		}
		tap := r.configs[name].Service.Pipelines[c.Tap]
		receivers := map[string]bool{}
		for _, rc := range tap.Receivers {
			receivers[rc] = true
		}
		if err := r.flow(ctx, name, receivers, items, 0); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) flow(ctx context.Context, coll string, receivers map[string]bool, items []unit.Metric, depth int) error {
	if depth > 16 {
		return fmt.Errorf("joins form a loop through %s", coll)
	}
	if len(items) == 0 {
		return nil
	}
	cfg := r.configs[coll]
	c := r.f.Collectors[coll]
	var pipelines []string
	for _, pid := range Names(cfg.Service.Pipelines) {
		if c.Recording(pid) || !IsMetrics(pid) {
			continue
		}
		for _, rc := range cfg.Service.Pipelines[pid].Receivers {
			if receivers[rc] {
				pipelines = append(pipelines, pid)
				break
			}
		}
	}
	if len(pipelines) == 0 {
		return r.record(coll, eachEvent(items, lineage.Event{Node: coll, Action: lineage.Dropped,
			Reason: "no metrics pipeline of " + coll + " uses the receiver it arrives on"}))
	}

	for pi, pid := range pipelines {
		in := items
		if pi > 0 {
			var err error
			if in, err = r.copies(items, "the receiver also feeds "+coll+" pipeline "+pid); err != nil {
				return err
			}
		}
		nodes, err := r.pipelineNodes(ctx, coll, pid)
		if err != nil {
			return err
		}
		out, err := graph.Process(ctx, nodes, in, r.log)
		if err != nil {
			return err
		}
		for ei, exp := range cfg.Service.Pipelines[pid].Exporters {
			x := out
			if ei > 0 {
				if x, err = r.copies(out, coll+" pipeline "+pid+" also exports to "+exp); err != nil {
					return err
				}
			}
			if err := r.export(ctx, coll, exp, x, depth); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Runner) export(ctx context.Context, coll, exp string, items []unit.Metric, depth int) error {
	key := coll + "/exporters/" + exp
	node := coll + "/" + exp
	for _, e := range r.f.External {
		if e == key {
			return r.record(node, eachEvent(items, lineage.Event{Node: node, Action: lineage.Exported,
				Reason: "leaves the modeled system: the exporter is listed as external in the topology"}))
		}
	}
	for i, j := range r.f.Joins {
		if j.From != key {
			continue
		}
		if reason, bad := r.v.BadJoins[i]; bad {
			if err := r.record(node, eachEvent(items, lineage.Event{Node: node, Action: lineage.Unverified,
				Reason: "the join " + j.From + " -> " + j.To + " can't be trusted: " + reason})); err != nil {
				return err
			}
		}
		to, err := r.f.ParseEndpoint(j.To)
		if err != nil {
			return err
		}
		if to.Backend != "" {
			r.Delivered[to.Backend] = append(r.Delivered[to.Backend], items...)
			return nil
		}
		return r.flow(ctx, to.Collector, map[string]bool{to.ID: true}, items, depth+1)
	}
	return fmt.Errorf("%s is neither joined nor external", key)
}

// pipelineNodes builds (once) the processor nodes of a pipeline.
func (r *Runner) pipelineNodes(ctx context.Context, coll, pid string) ([]graph.MetricsNode, error) {
	key := coll + "/" + pid
	if n, ok := r.nodes[key]; ok {
		return n, nil
	}
	cfg := r.configs[coll]
	shared := map[string]int{}
	for p, pl := range cfg.Service.Pipelines {
		if IsMetrics(p) && !r.f.Collectors[coll].Recording(p) {
			for _, id := range pl.Processors {
				shared[id]++
			}
		}
	}
	var nodes []graph.MetricsNode
	for _, id := range cfg.Service.Pipelines[pid].Processors {
		name := coll + "/" + id
		if shared[id] > 1 {
			name = coll + "/" + pid + "/" + id
		}
		nodes = append(nodes, r.processorNode(ctx, coll, name, id, cfg.Processors[id]))
	}
	r.nodes[key] = nodes
	return nodes, nil
}

func (r *Runner) processorNode(ctx context.Context, coll, name, id string, raw map[string]any) graph.MetricsNode {
	switch typ := collector.Type(id); {
	case typ == "batch", typ == "memory_limiter":
		// They only regroup or shed load; they don't change data.
		return graph.PassThrough{NodeName: name}
	case typ == "k8sattributes":
		n, err := k8sattributes.New(name, raw, r.in.Pods)
		if err != nil {
			return graph.Unsupported{NodeName: name, Reason: err.Error()}
		}
		return n
	case typ == "resourcedetection":
		var rec *resourcedetection.Recorded
		if v, ok := r.in.ResourceDetection[coll][id]; ok {
			rec = &v
		}
		n, err := resourcedetection.New(name, raw, rec, r.in.SnapshotPods)
		if err != nil {
			return graph.Unsupported{NodeName: name, Reason: err.Error()}
		}
		return n
	case realproc.Supported(typ):
		n, err := realproc.New(ctx, name, id, raw, r.ids)
		if err != nil {
			return graph.Unsupported{NodeName: name, Reason: err.Error()}
		}
		return n
	default:
		return graph.Unsupported{NodeName: name, Reason: "processor type " + typ + " isn't modeled yet"}
	}
}

// copies duplicates items for another branch, with new IDs.
func (r *Runner) copies(items []unit.Metric, reason string) ([]unit.Metric, error) {
	out := make([]unit.Metric, len(items))
	var events []lineage.Event
	var signals []lineage.Signal
	for i, it := range items {
		id := r.ids.Next()
		data := pmetric.NewMetrics()
		it.Data.CopyTo(data)
		from := []lineage.SignalID{it.ID}
		out[i] = unit.Metric{ID: id, From: from, Data: data}
		signals = append(signals, lineage.Signal{ID: id, Node: copyNode, Kind: "otlp_metric",
			Name: unit.FirstMetric(data).Name(), From: from, Resource: unit.Resource(data).Attributes().AsRaw()})
		events = append(events, lineage.Event{Node: copyNode, Signal: id, Action: lineage.Derived, From: from, Reason: reason})
	}
	ids := make([]lineage.SignalID, len(out))
	for i, it := range out {
		ids[i] = it.ID
	}
	return out, graph.Record(r.log, copyNode, events, signals, ids)
}

func (r *Runner) record(node string, events []lineage.Event) error {
	return graph.Record(r.log, node, events, nil, nil)
}

func (r *Runner) shutdown(ctx context.Context) {
	for _, ns := range r.nodes {
		graph.Shutdown(ctx, ns)
	}
}

func eachEvent(items []unit.Metric, e lineage.Event) []lineage.Event {
	out := make([]lineage.Event, len(items))
	for i, it := range items {
		out[i] = e
		out[i].Signal = it.ID
	}
	return out
}
