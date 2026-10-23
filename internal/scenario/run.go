package scenario

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/Anefu/otelcov/internal/alerting"
	"github.com/Anefu/otelcov/internal/collector"
	"github.com/Anefu/otelcov/internal/graph"
	"github.com/Anefu/otelcov/internal/kube"
	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/nodes/alerts"
	"github.com/Anefu/otelcov/internal/nodes/k8sattributes"
	"github.com/Anefu/otelcov/internal/nodes/mimir"
	"github.com/Anefu/otelcov/internal/nodes/route"
	"github.com/Anefu/otelcov/internal/nodes/rules"
	"github.com/Anefu/otelcov/internal/nodes/sdkenv"
	"github.com/Anefu/otelcov/internal/nodes/source"
	"github.com/Anefu/otelcov/internal/report"
	"github.com/Anefu/otelcov/internal/requirements"
	"github.com/Anefu/otelcov/internal/rulefile"
	"github.com/Anefu/otelcov/internal/unit"
)

// collectorPrefix names collector nodes in lineage. Scenarios have one
// collector; topologies with agent and gateway will use their own names.
const collectorPrefix = "collector"

// Result is the outcome of a run.
type Result struct {
	Log *lineage.Log
	// Items are the series after recording rules.
	Items []unit.Metric
	// Alerts are the alerts the alerting rules could produce.
	Alerts []alerting.Alert

	pods []corev1.Pod
}

// Run executes the scenario's graph as far as it is implemented: replay,
// sdk-env, the collector pipeline's processors, the backend, recording rules,
// alerting rules, then Alertmanager routing.
func Run(ctx context.Context, s *Scenario) (*Result, error) {
	snap, err := LoadSnapshot(s.Data.Snapshot)
	if err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	snapPods, err := kube.LoadPods(snap.Pods)
	if err != nil {
		return nil, fmt.Errorf("snapshot pods: %w", err)
	}
	pods, err := kube.LoadPods(s.Config.Pods)
	if err != nil {
		return nil, fmt.Errorf("pods: %w", err)
	}
	col, err := collector.Load(s.Config.Collector)
	if err != nil {
		return nil, fmt.Errorf("collector: %w", err)
	}
	pipeline, err := col.Pipeline(s.Config.CollectorPipeline)
	if err != nil {
		return nil, fmt.Errorf("collector: %w", err)
	}

	limits, err := mimir.Load(s.Config.Backend)
	if err != nil {
		return nil, fmt.Errorf("backend: %w", err)
	}

	nodes := []graph.MetricsNode{sdkenv.New(snapPods, pods)}
	for _, id := range pipeline.Processors {
		nodes = append(nodes, processorNode(collectorPrefix+"/"+id, id, col.Processors[id], pods))
	}
	nodes = append(nodes, mimir.New(limits))

	at, err := time.Parse(time.RFC3339, s.EvalTime)
	if err != nil {
		return nil, fmt.Errorf("eval_time: %w", err)
	}
	ruleSet, err := rulefile.Load(s.Config.Rules)
	if err != nil {
		return nil, fmt.Errorf("rules: %w", err)
	}
	ruleNode, err := rules.New(ruleSet.Recording, at)
	if err != nil {
		return nil, fmt.Errorf("rules: %w", err)
	}
	nodes = append(nodes, ruleNode)

	log := lineage.NewLog()
	src := source.New(s.Data.Recording, s.Rel(s.Data.Recording))
	items, err := graph.RunMetrics(ctx, src, nodes, log)
	if err != nil {
		return nil, err
	}

	ao, err := alerts.New(ruleSet.Alerting, at).Process(ctx, items)
	if err != nil {
		return nil, err
	}
	ids := make([]lineage.SignalID, len(ao.Alerts))
	for i, a := range ao.Alerts {
		ids[i] = a.ID
	}
	if err := graph.Record(log, "alerts", ao.Events, ao.Signals, ids); err != nil {
		return nil, err
	}

	router, err := route.Load(s.Config.Alertmanager)
	if err != nil {
		return nil, fmt.Errorf("alertmanager: %w", err)
	}
	if err := graph.Record(log, route.Name, router.Process(ao.Alerts), nil, nil); err != nil {
		return nil, err
	}
	return &Result{Log: log, Items: items, Alerts: ao.Alerts, pods: pods}, nil
}

// Report runs the scenario and answers its requirements.
func Report(ctx context.Context, s *Scenario) (*report.Report, error) {
	res, err := Run(ctx, s)
	if err != nil {
		return nil, err
	}
	reqs, err := requirements.Load(s.Requirements)
	if err != nil {
		return nil, fmt.Errorf("requirements: %w", err)
	}
	return report.Build(s.Name, s.EvalTime, res.Log, reqs, res.pods), nil
}

// processorNode builds the node for one collector processor. Types and
// settings that aren't modeled yet become Unsupported nodes, so the run goes on
// and their effect shows up as unverified.
func processorNode(name, id string, raw map[string]any, pods []corev1.Pod) graph.MetricsNode {
	switch typ := collector.Type(id); typ {
	case "batch":
		return graph.PassThrough{NodeName: name}
	case "k8sattributes":
		n, err := k8sattributes.New(name, raw, pods)
		if err != nil {
			return graph.Unsupported{NodeName: name, Reason: err.Error()}
		}
		return n
	default:
		return graph.Unsupported{NodeName: name, Reason: "processor type " + typ + " isn't modeled yet"}
	}
}
