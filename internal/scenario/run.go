package scenario

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	corev1 "k8s.io/api/core/v1"

	"github.com/Anefu/otelcov/internal/alerting"
	"github.com/Anefu/otelcov/internal/flowgraph"
	"github.com/Anefu/otelcov/internal/graph"
	"github.com/Anefu/otelcov/internal/kube"
	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/nodes/alerts"
	"github.com/Anefu/otelcov/internal/nodes/mimir"
	"github.com/Anefu/otelcov/internal/nodes/route"
	"github.com/Anefu/otelcov/internal/nodes/rules"
	"github.com/Anefu/otelcov/internal/nodes/sdkenv"
	"github.com/Anefu/otelcov/internal/nodes/source"
	"github.com/Anefu/otelcov/internal/report"
	"github.com/Anefu/otelcov/internal/requirements"
	"github.com/Anefu/otelcov/internal/rulefile"
	"github.com/Anefu/otelcov/internal/topology"
	"github.com/Anefu/otelcov/internal/unit"
)

// Result is the outcome of a run.
type Result struct {
	Log *lineage.Log
	// Series per backend, after recording rules.
	Series map[string][]unit.Metric
	// Alerts the alerting rules could produce.
	Alerts []alerting.Alert
	// Problems found in the inputs: unknown pod fields, topology validation
	// (none fatal).
	Problems []topology.Problem
	// Flow is how the components connect, with lineage node names as IDs.
	Flow flowgraph.Graph

	pods []corev1.Pod
}

// Run executes the scenario: replay, sdk-env, the collectors as the topology
// connects them, each backend, recording and alerting rules, then
// Alertmanager routing.
func Run(ctx context.Context, s *Scenario) (*Result, error) {
	snap, err := LoadSnapshot(s.Data.Snapshot)
	if err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	var problems []topology.Problem
	warned := map[string]bool{}
	load := func(path string) (kube.Objects, error) {
		objs, unknown, err := kube.Load(path)
		if err == nil && len(unknown) > 0 && !warned[path] {
			warned[path] = true
			problems = append(problems, topology.Problem{Severity: topology.Warning, Where: s.Rel(path),
				Message: fmt.Sprintf("fields the Kubernetes %s API types in otelcov don't know were ignored: %s", kube.APIVersion, strings.Join(unknown, ", "))})
		}
		return objs, err
	}
	snapObjs, err := load(snap.Pods)
	if err != nil {
		return nil, fmt.Errorf("snapshot pods: %w", err)
	}
	snapPods := snapObjs.Pods
	cluster, err := load(s.Config.Pods)
	if err != nil {
		return nil, fmt.Errorf("pods: %w", err)
	}
	pods := cluster.Pods
	topo, err := topology.Load(s.Config.Topology)
	if err != nil {
		return nil, err
	}
	val := topology.Validate(topo)
	if fatal := val.Fatal(); len(fatal) > 0 {
		var msgs []string
		for _, p := range fatal {
			msgs = append(msgs, p.Where+": "+p.Message)
		}
		return nil, fmt.Errorf("topology doesn't match the configs:\n  %s", strings.Join(msgs, "\n  "))
	}
	at, err := time.Parse(time.RFC3339, s.EvalTime)
	if err != nil {
		return nil, fmt.Errorf("eval_time: %w", err)
	}

	log := lineage.NewLog()
	items, err := graph.ReadSource(ctx, source.New(s.Data.Recording, s.Rel(s.Data.Recording)), log)
	if err != nil {
		return nil, err
	}
	if items, err = graph.Process(ctx, []graph.MetricsNode{sdkenv.New(snapPods, pods)}, items, log); err != nil {
		return nil, err
	}

	runner, err := topology.NewRunner(topo, val, topology.Inputs{Cluster: cluster, SnapshotPods: snapPods, ResourceDetection: snap.ResourceDetection}, log)
	if err != nil {
		return nil, err
	}
	if err := runner.Run(ctx, items); err != nil {
		return nil, err
	}

	flow, err := topology.Flow(topo, val)
	if err != nil {
		return nil, err
	}
	res := &Result{Log: log, Series: map[string][]unit.Metric{}, Problems: append(problems, val.Problems...), Flow: flow, pods: pods}
	for _, name := range topology.Names(topo.Backends) {
		res.Flow.AddNode(topology.BackendFlowNode(name, topo.Backends[name].Type))
		limits, err := mimir.Load(topo.Backends[name].Config)
		if err != nil {
			return nil, fmt.Errorf("backend %s: %w", name, err)
		}
		series, err := graph.Process(ctx, []graph.MetricsNode{mimir.New(name, limits)}, runner.Delivered[name], log)
		if err != nil {
			return nil, err
		}
		res.Series[name] = series
	}

	// Consumers: rules read a backend; Alertmanager reads rules.
	ruleIDs, alertIDs := lineage.NewIDs("r"), lineage.NewIDs("a")
	alertsBy := map[string][]alerting.Alert{}
	alertNames := map[string][]string{} // flow node IDs of each rules consumer's alerts
	for _, name := range topology.Names(topo.Consumers) {
		c := topo.Consumers[name]
		if c.Type != "prometheus-rules" {
			continue
		}
		set, err := rulefile.Load(c.Paths)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		rn, err := rules.New(set.Recording, at, ruleIDs)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		alertNames[name] = addRules(&res.Flow, topology.BackendNode(c.Reads), set)
		series, err := graph.Process(ctx, []graph.MetricsNode{rn}, res.Series[c.Reads], log)
		if err != nil {
			return nil, err
		}
		res.Series[c.Reads] = series
		ao, err := alerts.New(set.Alerting, at, alertIDs).Process(ctx, series)
		if err != nil {
			return nil, err
		}
		ids := make([]lineage.SignalID, len(ao.Alerts))
		for i, a := range ao.Alerts {
			ids[i] = a.ID
		}
		if err := graph.Record(log, name, ao.Events, ao.Signals, ids); err != nil {
			return nil, err
		}
		alertsBy[name] = ao.Alerts
		res.Alerts = append(res.Alerts, ao.Alerts...)
	}
	for _, name := range topology.Names(topo.Consumers) {
		c := topo.Consumers[name]
		if c.Type != "alertmanager" {
			continue
		}
		router, err := route.Load(c.Config)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		res.Flow.AddNode(flowgraph.Node{ID: route.Name, Group: "alerting", Label: "Alertmanager routes", Kind: flowgraph.Route})
		for _, a := range alertNames[c.Reads] {
			res.Flow.AddEdge(flowgraph.Edge{From: a, To: route.Name})
		}
		for _, rcv := range router.Receivers() {
			id := ReceiverNode(rcv)
			res.Flow.AddNode(flowgraph.Node{ID: id, Group: "receivers", Label: rcv, Kind: flowgraph.Receiver})
			res.Flow.AddEdge(flowgraph.Edge{From: route.Name, To: id})
		}
		if err := graph.Record(log, route.Name, router.Process(alertsBy[c.Reads]), nil, nil); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// ReceiverNode is a receiver's node ID in the flow graph. Lineage has no
// receiver nodes: routed events name the receiver in their detail.
func ReceiverNode(name string) string { return "receiver/" + name }

// addRules adds a rules consumer's recording and alerting rules to the flow,
// each fed by the backend or by the recording rules it selects. It returns
// the alerts' node IDs.
func addRules(g *flowgraph.Graph, backend string, set *rulefile.Rules) []string {
	ordered, err := rulefile.OrderRecording(set.Recording)
	if err != nil {
		ordered = set.Recording
	}
	recorded := map[string]bool{}
	for _, r := range ordered {
		recorded[r.Record] = true
	}
	// A rule reads the recording rules its __name__ matchers match, and the
	// backend for anything else (no name matcher, or one that isn't exactly a
	// recorded name).
	inputs := func(id string, ms []*labels.Matcher) {
		fromBackend := len(ms) == 0
		for _, m := range ms {
			if m.Type == labels.MatchEqual && recorded[m.Value] {
				g.AddEdge(flowgraph.Edge{From: "rule/" + m.Value, To: id})
				continue
			}
			for _, r := range ordered {
				if m.Matches(r.Record) {
					g.AddEdge(flowgraph.Edge{From: "rule/" + r.Record, To: id})
				}
			}
			fromBackend = true
		}
		if fromBackend {
			g.AddEdge(flowgraph.Edge{From: backend, To: id})
		}
	}
	for _, r := range ordered {
		id := "rule/" + r.Record
		g.AddNode(flowgraph.Node{ID: id, Group: "rules", Label: r.Record, Kind: flowgraph.Rule})
		inputs(id, rulefile.SelectedNames(r.Expr))
	}
	var alerts []string
	for _, a := range set.Alerting {
		id := "alert/" + a.Alert
		g.AddNode(flowgraph.Node{ID: id, Group: "alerting", Label: a.Alert, Kind: flowgraph.Alert})
		inputs(id, rulefile.SelectedNames(a.Expr))
		alerts = append(alerts, id)
	}
	return alerts
}

// Report runs the scenario and answers its requirements.
func Report(ctx context.Context, s *Scenario) (*report.Report, error) {
	rep, _, err := ReportWithFlow(ctx, s)
	return rep, err
}

// ReportWithFlow is Report, plus the scenario's flow graph.
func ReportWithFlow(ctx context.Context, s *Scenario) (*report.Report, flowgraph.Graph, error) {
	res, err := Run(ctx, s)
	if err != nil {
		return nil, flowgraph.Graph{}, err
	}
	reqs, err := requirements.Load(s.Requirements)
	if err != nil {
		return nil, flowgraph.Graph{}, fmt.Errorf("requirements: %w", err)
	}
	rep := report.Build(s.Name, s.EvalTime, res.Log, reqs, res.pods)
	rep.Problems = append(res.Problems, rep.Problems...)
	return rep, res.Flow, nil
}
