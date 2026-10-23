package scenario

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"

	"github.com/Anefu/otelcov/internal/collector"
	"github.com/Anefu/otelcov/internal/graph"
	"github.com/Anefu/otelcov/internal/kube"
	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/nodes/k8sattributes"
	"github.com/Anefu/otelcov/internal/nodes/sdkenv"
	"github.com/Anefu/otelcov/internal/nodes/source"
	"github.com/Anefu/otelcov/internal/unit"
)

// collectorPrefix names collector nodes in lineage. Scenarios have one
// collector; topologies with agent and gateway will use their own names.
const collectorPrefix = "collector"

// Result is the outcome of a run.
type Result struct {
	Log *lineage.Log
	// Items are the signals that left the last implemented node.
	Items []unit.Metric
}

// Run executes the scenario's graph as far as it is implemented: replay,
// sdk-env, then the collector pipeline's processors.
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

	nodes := []graph.MetricsNode{sdkenv.New(snapPods, pods)}
	for _, id := range pipeline.Processors {
		nodes = append(nodes, processorNode(collectorPrefix+"/"+id, id, col.Processors[id], pods))
	}

	log := lineage.NewLog()
	src := source.New(s.Data.Recording, s.Rel(s.Data.Recording))
	items, err := graph.RunMetrics(ctx, src, nodes, log)
	if err != nil {
		return nil, err
	}
	return &Result{Log: log, Items: items}, nil
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
