// Package graph defines the node interfaces and runs nodes in order.
//
// Like the collector, pipelines are typed by signal; only metrics exist so far.
// Every node is f(data, config): config is fixed when the node is built, data
// flows through ProcessMetrics.
package graph

import (
	"context"
	"fmt"

	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/unit"
)

// Output is what a node returns: its output items plus the lineage it wants
// logged (Writer-monad style).
type Output struct {
	Items []unit.Metric
	// Events to log. Node and Signal must be set; IDs are assigned by the runner.
	Events []lineage.Event
	// Signals describes every output item whose ID is new.
	Signals []lineage.Signal
}

// MetricsSource produces the first items of a run.
type MetricsSource interface {
	Name() string
	ReadMetrics(ctx context.Context) (Output, error)
}

// MetricsNode transforms items.
type MetricsNode interface {
	Name() string
	ProcessMetrics(ctx context.Context, in []unit.Metric) (Output, error)
}

// RunMetrics reads from src and passes the items through nodes in order,
// recording all lineage in log. It returns the final items.
func RunMetrics(ctx context.Context, src MetricsSource, nodes []MetricsNode, log *lineage.Log) ([]unit.Metric, error) {
	out, err := src.ReadMetrics(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", src.Name(), err)
	}
	if err := apply(src.Name(), out, log); err != nil {
		return nil, err
	}
	items := out.Items
	for _, n := range nodes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out, err := n.ProcessMetrics(ctx, items)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n.Name(), err)
		}
		if err := apply(n.Name(), out, log); err != nil {
			return nil, err
		}
		items = out.Items
	}
	return items, nil
}

// apply logs a node's output and checks the lineage invariants.
func apply(node string, out Output, log *lineage.Log) error {
	ids := make([]lineage.SignalID, len(out.Items))
	for i, it := range out.Items {
		ids[i] = it.ID
	}
	return Record(log, node, out.Events, out.Signals, ids)
}

// Record logs a stage's lineage after checking it: new signals are unique,
// events name a node and a known signal, and every output ID was described.
// Non-metrics stages (alerts, routing) use it directly.
func Record(log *lineage.Log, node string, events []lineage.Event, signals []lineage.Signal, outputs []lineage.SignalID) error {
	for _, s := range signals {
		if err := log.AddSignal(s); err != nil {
			return fmt.Errorf("%s: %w", node, err)
		}
	}
	for _, e := range events {
		if e.Node == "" || e.Signal == "" {
			return fmt.Errorf("%s: event %q without node or signal", node, e.Action)
		}
		if !log.HasSignal(e.Signal) {
			return fmt.Errorf("%s: event %q on unknown signal %s", node, e.Action, e.Signal)
		}
	}
	for _, id := range outputs {
		if !log.HasSignal(id) {
			return fmt.Errorf("%s: output %s was never described", node, id)
		}
	}
	log.Record(events...)
	return nil
}

// PassThrough is a node that changes nothing and logs nothing, e.g. batch,
// which only regroups data.
type PassThrough struct{ NodeName string }

func (p PassThrough) Name() string { return p.NodeName }

func (p PassThrough) ProcessMetrics(_ context.Context, in []unit.Metric) (Output, error) {
	return Output{Items: in}, nil
}

// Unsupported stands in for a component otelcov can't model yet. Data passes
// through unchanged, and every item is marked unverified with the reason.
type Unsupported struct {
	NodeName string
	Reason   string
}

func (u Unsupported) Name() string { return u.NodeName }

func (u Unsupported) ProcessMetrics(_ context.Context, in []unit.Metric) (Output, error) {
	out := Output{Items: in}
	for _, it := range in {
		out.Events = append(out.Events, lineage.Event{
			Node: u.NodeName, Signal: it.ID, Action: lineage.Unverified, Reason: u.Reason,
		})
	}
	return out, nil
}
