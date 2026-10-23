// Package rules evaluates recording rules over series items.
//
// Each rule is evaluated for coverage by ruleeval: values don't decide which
// outputs exist, the real engine does the evaluation, and the explainer gives
// parents. Rules are evaluated once, at a single instant, in dependency order,
// so a rule sees the outputs of rules it reads (steady state).
package rules

import (
	"context"
	"sort"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"github.com/Anefu/otelcov/internal/graph"
	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/promqlx"
	"github.com/Anefu/otelcov/internal/ruleeval"
	"github.com/Anefu/otelcov/internal/rulefile"
	"github.com/Anefu/otelcov/internal/unit"
)

// Prefix starts the node name of each rule in lineage: "rule/<record>".
const Prefix = "rule/"

// Node evaluates recording rules.
type Node struct {
	rules  []rulefile.Recording
	at     time.Time
	engine *promqlx.Engine
	ids    *lineage.IDs
}

// New builds the node. Rules are reordered by dependency. ids hands out
// output IDs (r1, r2, ...); nil gives the node its own.
func New(rules []rulefile.Recording, at time.Time, ids *lineage.IDs) (*Node, error) {
	ordered, err := rulefile.OrderRecording(rules)
	if err != nil {
		return nil, err
	}
	if ids == nil {
		ids = lineage.NewIDs("r")
	}
	return &Node{rules: ordered, at: at, engine: promqlx.NewEngine(), ids: ids}, nil
}

func (n *Node) Name() string { return "rules" }

func (n *Node) ProcessMetrics(ctx context.Context, in []unit.Metric) (graph.Output, error) {
	out := graph.Output{Items: append([]unit.Metric(nil), in...)}
	for _, r := range n.rules {
		if err := ctx.Err(); err != nil {
			return graph.Output{}, err
		}
		res := n.evaluate(ctx, r, out.Items)
		out.Items = append(out.Items, res.Items...)
		out.Events = append(out.Events, res.Events...)
		out.Signals = append(out.Signals, res.Signals...)
	}
	return out, nil
}

func (n *Node) evaluate(ctx context.Context, r rulefile.Recording, items []unit.Metric) graph.Output {
	name := Prefix + r.Record
	res := ruleeval.Evaluate(ctx, n.engine, r.Expr, items, n.at)
	var ev Positioned

	var out graph.Output
	for _, o := range res.Outputs {
		id := n.ids.Next()
		from := IDs(items, o.Parents)
		b := labels.NewBuilder(o.Labels).Set(labels.MetricName, r.Record)
		for k, v := range r.Labels {
			b.Set(k, v)
		}
		lset := b.Labels()
		out.Items = append(out.Items, unit.Metric{ID: id, From: from, Data: unit.NewSeries(lset, unit.Sample{T: n.at, V: o.Value})})
		out.Signals = append(out.Signals, lineage.Signal{
			ID: id, Node: name, Kind: "series", From: from, Labels: lset.Map(), Value: o.Value,
		})
		pos := First(o.Parents)
		ev.Add(pos, 0, lineage.Event{Node: name, Signal: id, Action: lineage.Derived, From: from, Detail: ExprDetail(r.Expr.String(), res)})
		if o.Note != "" {
			ev.Add(pos, 1, lineage.Event{Node: name, Signal: id, Action: lineage.Unverified, Reason: o.Note})
		}
	}
	for _, ie := range res.Inputs {
		e := ie.Event
		e.Node, e.Signal = name, items[ie.Input].ID
		ev.Add(ie.Input, 2, e)
	}
	out.Events = ev.Events()
	return out
}

// ExprDetail describes a rule's expression, and what was actually evaluated
// when the structural rewrite changed it.
func ExprDetail(expr string, res ruleeval.Result) map[string]any {
	d := map[string]any{"expr": expr}
	if s := res.Structural.String(); s != expr {
		d["evaluated_as"] = s
	}
	return d
}

// IDs maps input indexes to signal IDs.
func IDs(items []unit.Metric, idx []int) []lineage.SignalID {
	out := make([]lineage.SignalID, len(idx))
	for i, p := range idx {
		out[i] = items[p].ID
	}
	return out
}

// First returns the first index, or a large number for none.
func First(idx []int) int {
	if len(idx) == 0 {
		return 1 << 30
	}
	return idx[0]
}

// Positioned orders a rule's events by the input they concern, so each
// input's story reads in order.
type Positioned struct{ ps []positioned }

type positioned struct {
	pos, order int
	e          lineage.Event
}

// Add records an event at an input position; order breaks ties.
func (p *Positioned) Add(pos, order int, e lineage.Event) {
	p.ps = append(p.ps, positioned{pos, order, e})
}

// Events returns the events sorted by position.
func (p *Positioned) Events() []lineage.Event {
	sort.SliceStable(p.ps, func(i, j int) bool {
		if p.ps[i].pos != p.ps[j].pos {
			return p.ps[i].pos < p.ps[j].pos
		}
		return p.ps[i].order < p.ps[j].order
	})
	out := make([]lineage.Event, len(p.ps))
	for i, x := range p.ps {
		out[i] = x.e
	}
	return out
}
