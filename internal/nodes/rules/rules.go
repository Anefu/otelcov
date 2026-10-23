// Package rules evaluates recording rules over series items.
//
// The real PromQL engine decides which outputs exist; the explainer
// (promqlx.Explain) says which inputs each output came from and why inputs
// drop out. Rules are evaluated once, at a single instant, in dependency
// order, so a rule sees the outputs of rules it reads (steady state).
package rules

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"github.com/Anefu/otelcov/internal/graph"
	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/promqlx"
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
}

// New builds the node. Rules are reordered by dependency.
func New(rules []rulefile.Recording, at time.Time) (*Node, error) {
	ordered, err := rulefile.OrderRecording(rules)
	if err != nil {
		return nil, err
	}
	return &Node{rules: ordered, at: at, engine: promqlx.NewEngine()}, nil
}

func (n *Node) Name() string { return "rules" }

func (n *Node) ProcessMetrics(ctx context.Context, in []unit.Metric) (graph.Output, error) {
	out := graph.Output{Items: append([]unit.Metric(nil), in...)}
	next := 0
	for _, r := range n.rules {
		res, err := n.evaluate(ctx, r, out.Items, &next)
		if err != nil {
			return graph.Output{}, err
		}
		out.Items = append(out.Items, res.Items...)
		out.Events = append(out.Events, res.Events...)
		out.Signals = append(out.Signals, res.Signals...)
	}
	return out, nil
}

// positioned orders a rule's events by the input they concern.
type positioned struct {
	pos   int
	order int
	e     lineage.Event
}

func (n *Node) evaluate(ctx context.Context, r rulefile.Recording, items []unit.Metric, next *int) (graph.Output, error) {
	name := Prefix + r.Record
	series := make([]promqlx.Series, len(items))
	lsets := make([]labels.Labels, len(items))
	for i, it := range items {
		lsets[i] = unit.SeriesLabels(it.Data)
		series[i] = promqlx.Series{Labels: lsets[i]}
		for _, smp := range unit.SeriesSamples(it.Data) {
			series[i].Samples = append(series[i].Samples, promqlx.Sample{T: smp.T, V: smp.V})
		}
	}

	var events []positioned
	add := func(pos, order int, e lineage.Event) {
		e.Node = name
		events = append(events, positioned{pos, order, e})
	}

	vec, err := n.engine.Instant(ctx, r.Expr.String(), series, n.at)
	if err != nil {
		for _, i := range selected(r, lsets) {
			add(i, 0, lineage.Event{Signal: items[i].ID, Action: lineage.Unverified,
				Reason: "the PromQL engine failed to evaluate the rule: " + err.Error()})
		}
		return graph.Output{Events: flatten(events)}, nil
	}

	ex, exErr := promqlx.Explain(r.Expr, lsets)
	groups := map[string]promqlx.Group{}
	if exErr == nil {
		for _, g := range ex.Groups {
			groups[withoutName(g.Labels).String()] = g
		}
	}

	type output struct {
		lset    labels.Labels
		value   float64
		parents []int
		note    string
	}
	var outs []output
	used := map[int]bool{}
	for _, s := range vec {
		o := output{value: s.F}
		g, ok := groups[withoutName(s.Metric).String()]
		switch {
		case exErr != nil:
			o.parents, o.note = selected(r, lsets), exErr.Error()+"; parents are every series the rule selects"
		case !ok:
			o.parents, o.note = selected(r, lsets), "the explainer disagrees with the PromQL engine about this output; parents are every series the rule selects"
		default:
			o.parents = g.Parents
		}
		b := labels.NewBuilder(s.Metric).Set(labels.MetricName, r.Record)
		for k, v := range r.Labels {
			b.Set(k, v)
		}
		o.lset = b.Labels()
		for _, p := range o.parents {
			used[p] = true
		}
		outs = append(outs, o)
	}
	sort.SliceStable(outs, func(i, j int) bool { return first(outs[i].parents) < first(outs[j].parents) })

	var res graph.Output
	for _, o := range outs {
		*next++
		id := lineage.SignalID("r" + strconv.Itoa(*next))
		from := make([]lineage.SignalID, len(o.parents))
		for i, p := range o.parents {
			from[i] = items[p].ID
		}
		res.Items = append(res.Items, unit.Metric{ID: id, From: from, Data: unit.NewSeries(o.lset, unit.Sample{T: n.at, V: o.value})})
		res.Signals = append(res.Signals, lineage.Signal{
			ID: id, Node: name, Kind: "series", From: from, Labels: o.lset.Map(), Value: o.value,
		})
		add(first(o.parents), 0, lineage.Event{Signal: id, Action: lineage.Derived, From: from,
			Detail: map[string]any{"expr": r.Expr.String()}})
		if o.note != "" {
			add(first(o.parents), 1, lineage.Event{Signal: id, Action: lineage.Unverified, Reason: o.note})
		}
	}

	if exErr == nil {
		for _, d := range ex.Drops {
			e := lineage.Event{Signal: items[d.Input].ID, Attribute: d.Label, Action: lineage.Dropped, Reason: d.Reason}
			if d.Clause == "selector" {
				e.Detail = map[string]any{"requires": map[string]any{"label": d.Label, "matcher": d.Matcher, "clause": d.Clause}}
			} else {
				e.Detail = map[string]any{"clause": d.Clause}
			}
			add(d.Input, 2, e)
		}
		// Possible outputs the engine didn't produce at this instant. That
		// depends on values or on how many samples the recording has, so it
		// says nothing about coverage either way: unverified, not dropped.
		reason := "the rule could produce an output from this series, but didn't at the evaluation time: " + whyNoOutput(r)
		for _, g := range ex.Groups {
			for _, p := range g.Parents {
				if !used[p] {
					used[p] = true
					add(p, 2, lineage.Event{Signal: items[p].ID, Action: lineage.Unverified, Reason: reason})
				}
			}
		}
	}
	res.Events = flatten(events)
	return res, nil
}

// selected returns the inputs any selector of the rule matches, in order.
func selected(r rulefile.Recording, lsets []labels.Labels) []int {
	ms := selectors(r)
	var out []int
	for i, ls := range lsets {
		for _, set := range ms {
			if matchesAll(ls, set) {
				out = append(out, i)
				break
			}
		}
	}
	return out
}

func flatten(ps []positioned) []lineage.Event {
	sort.SliceStable(ps, func(i, j int) bool {
		if ps[i].pos != ps[j].pos {
			return ps[i].pos < ps[j].pos
		}
		return ps[i].order < ps[j].order
	})
	out := make([]lineage.Event, len(ps))
	for i, p := range ps {
		out[i] = p.e
	}
	return out
}

func first(ps []int) int {
	if len(ps) == 0 {
		return 1 << 30
	}
	return ps[0]
}

func withoutName(ls labels.Labels) labels.Labels {
	return labels.NewBuilder(ls).Del(labels.MetricName).Labels()
}

func matchesAll(ls labels.Labels, ms []*labels.Matcher) bool {
	for _, m := range ms {
		if !m.Matches(ls.Get(m.Name)) {
			return false
		}
	}
	return true
}

func selectors(r rulefile.Recording) [][]*labels.Matcher {
	var out [][]*labels.Matcher
	for _, vs := range promqlx.Selectors(r.Expr) {
		out = append(out, vs.LabelMatchers)
	}
	return out
}

// whyNoOutput names the likely reason a possible output is missing.
func whyNoOutput(r rulefile.Recording) string {
	var reasons []string
	if promqlx.UsesRange(r.Expr) {
		reasons = append(reasons, "range functions such as rate() need at least two samples in their window, and the recording may have fewer")
	}
	if promqlx.FiltersByValue(r.Expr) {
		reasons = append(reasons, "a comparison or topk-like function excluded it at the recorded values")
	}
	if len(reasons) == 0 {
		return "no sample within the lookback window"
	}
	return strings.Join(reasons, "; or ")
}

// String describes the node in errors.
func (n *Node) String() string { return fmt.Sprintf("rules(%d)", len(n.rules)) }
