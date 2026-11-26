package pluginhost

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Anefu/otelcov/internal/collector"
	"github.com/Anefu/otelcov/internal/graph"
	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/pluginwire"
	"github.com/Anefu/otelcov/internal/unit"
	"github.com/Anefu/otelcov/pkg/plugin"
	"github.com/Anefu/otelcov/pkg/plugin/pluginv1"
)

// Instance is the collector instance a stage runs in.
type Instance struct {
	Collector string
	// Node is the instance's Kubernetes node; "" when unknown.
	Node string
	Env  map[string]collector.EnvVar
}

// Stage builds a stage instance in the plugin. A stage the plugin can't
// model becomes graph.Unsupported (data passes through, unverified), and a
// pass-through stage graph.PassThrough. ids is the run's sequence for new
// items.
func (p *Plugin) Stage(ctx context.Context, kind, componentID, nodeName string, raw map[string]any, inst Instance, ids *lineage.IDs) (graph.MetricsNode, error) {
	cfg, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", nodeName, err)
	}
	req := &pluginv1.ConfigureRequest{
		Kind: kind, ComponentId: componentID, NodeName: nodeName, Config: cfg,
		Instance: &pluginv1.Instance{Collector: inst.Collector, Node: inst.Node},
	}
	if len(inst.Env) > 0 {
		req.Instance.Env = map[string]*pluginv1.EnvVar{}
		for k, v := range inst.Env {
			req.Instance.Env[k] = &pluginv1.EnvVar{Value: v.Value, FieldRef: v.FieldRef}
		}
	}
	resp, err := p.c.conn.Configure(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("plugin %s: %s: %w", p.c.info, nodeName, err)
	}
	switch {
	case resp.GetUnsupported() != "":
		return graph.Unsupported{NodeName: nodeName, Reason: resp.GetUnsupported()}, nil
	case resp.GetPassThrough():
		return graph.PassThrough{NodeName: nodeName}, nil
	}
	return &stage{name: nodeName, conn: p.c.conn, handle: resp.GetHandle(), ids: ids}, nil
}

// stage is a stage instance in a plugin process.
type stage struct {
	name   string
	conn   *plugin.Conn
	handle string
	ids    *lineage.IDs
}

func (s *stage) Name() string { return s.name }

// batchBytes keeps Apply requests well under the message limit.
const batchBytes = plugin.MaxMessage / 4

// ProcessMetrics sends the units in batches, in order. Units are processed
// one at a time in the plugin, so batching doesn't change the result.
func (s *stage) ProcessMetrics(ctx context.Context, in []unit.Metric) (graph.Output, error) {
	units, err := pluginwire.Units(in)
	if err != nil {
		return graph.Output{}, fmt.Errorf("%s: %w", s.name, err)
	}
	var out graph.Output
	for start := 0; start < len(units); {
		end, size := start, 0
		for end < len(units) && (end == start || size+pluginwire.Size(units[end]) <= batchBytes) {
			size += pluginwire.Size(units[end])
			end++
		}
		next := s.ids.NextNumber()
		resp, err := s.conn.Apply(ctx, &pluginv1.ApplyRequest{Handle: s.handle, Units: units[start:end], NextId: next})
		if err != nil {
			return graph.Output{}, fmt.Errorf("%s: %w", s.name, err)
		}
		s.ids.SetNextNumber(next + resp.GetIdsUsed())
		items, err := pluginwire.FromUnits(resp.GetUnits())
		if err != nil {
			return graph.Output{}, fmt.Errorf("%s: %w", s.name, err)
		}
		events, err := pluginwire.FromEvents(resp.GetEvents())
		if err != nil {
			return graph.Output{}, fmt.Errorf("%s: %w", s.name, err)
		}
		signals, err := pluginwire.FromSignals(resp.GetSignals())
		if err != nil {
			return graph.Output{}, fmt.Errorf("%s: %w", s.name, err)
		}
		out.Items = append(out.Items, items...)
		out.Events = append(out.Events, events...)
		out.Signals = append(out.Signals, signals...)
		start = end
	}
	return out, nil
}

// Shutdown releases the stage in the plugin.
func (s *stage) Shutdown(ctx context.Context) error {
	_, err := s.conn.Close(ctx, &pluginv1.CloseRequest{Handles: []string{s.handle}})
	return err
}
