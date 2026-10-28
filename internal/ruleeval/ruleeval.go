// Package ruleeval evaluates one rule expression over series items for
// coverage, shared by recording and alerting rules.
//
// The expression is rewritten so outputs don't depend on values
// (promqlx.Structural), the real PromQL engine decides which outputs exist,
// and the explainer gives each output's parents and explains why inputs drop
// out.
package ruleeval

import (
	"context"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/promqlx"
	"github.com/Anefu/otelcov/internal/unit"
)

// Output is one output of the expression.
type Output struct {
	// Labels as the engine returned them.
	Labels labels.Labels
	// Value of the structural expression: not the real value.
	Value   float64
	Parents []int // indexes into the inputs
	// Note is set when the parents are an over-approximation.
	Note string
}

// InputEvent is an event about one input (dropped, unverified).
type InputEvent struct {
	Input int
	Event lineage.Event // Node and Signal are filled in by the caller's node
}

// Result of evaluating one expression.
type Result struct {
	Structural parser.Expr
	Outputs    []Output // ordered by first parent
	Inputs     []InputEvent
}

// Evaluate runs expr at `at` over items (series items).
func Evaluate(ctx context.Context, eng *promqlx.Engine, expr parser.Expr, items []unit.Metric, at time.Time) Result {
	lsets := make([]labels.Labels, len(items))
	series := make([]promqlx.Series, len(items))
	for i, it := range items {
		lsets[i] = unit.SeriesLabels(it.Data)
		series[i] = promqlx.Series{Labels: lsets[i]}
		for _, s := range unit.SeriesSamples(it.Data) {
			series[i].Samples = append(series[i].Samples, promqlx.Sample{T: s.T, V: s.V})
		}
	}
	sel := selected(expr, lsets)

	structural, err := promqlx.Structural(expr)
	if err != nil {
		return failed(expr, sel, err)
	}
	res := Result{Structural: structural}
	vec, err := eng.Instant(ctx, structural.String(), series, at)
	if err != nil {
		return failed(structural, sel, err)
	}

	ex, exErr := promqlx.Explain(structural, lsets)
	groups := map[string]promqlx.Group{}
	if exErr == nil {
		for _, g := range ex.Groups {
			groups[withoutName(g.Labels).String()] = g
		}
	}
	used := map[int]bool{}
	for _, s := range vec {
		o := Output{Labels: s.Metric, Value: s.F}
		g, ok := groups[withoutName(s.Metric).String()]
		switch {
		case exErr != nil:
			o.Parents, o.Note = sel, exErr.Error()+"; parents are every series the expression selects"
		case !ok:
			o.Parents, o.Note = sel, "the explainer disagrees with the PromQL engine about this output; parents are every series the expression selects"
		default:
			o.Parents = g.Parents
		}
		for _, p := range o.Parents {
			used[p] = true
		}
		res.Outputs = append(res.Outputs, o)
	}
	sortByFirstParent(res.Outputs)
	if exErr != nil {
		return res
	}

	for _, d := range ex.Drops {
		e := lineage.Event{Attribute: d.Label, Action: lineage.Dropped, Reason: d.Reason}
		if d.Clause == "selector" {
			e.Detail = map[string]any{"requires": map[string]any{"label": d.Label, "matcher": d.Matcher, "clause": d.Clause}}
			if d.Name != nil {
				e.Detail["metric_name"] = map[string]any{
					"want": d.Name.Want, "got": d.Name.Got, "want_suffix": d.Name.WantSuffix, "got_suffix": d.Name.GotSuffix,
				}
			}
		} else {
			e.Detail = map[string]any{"clause": d.Clause}
		}
		res.Inputs = append(res.Inputs, InputEvent{Input: d.Input, Event: e})
	}
	// Possible outputs the engine didn't produce. Values no longer matter
	// after the rewrite, so the cause is missing data, which says nothing
	// about coverage: unverified, not dropped.
	reason := "the expression could produce an output from this series, but didn't at the evaluation time: no sample within the lookback window"
	if promqlx.UsesRange(structural) {
		reason = "the expression could produce an output from this series, but didn't at the evaluation time: range functions such as rate() need at least two samples in their window, and the recording may have fewer"
	}
	for _, g := range ex.Groups {
		for _, p := range g.Parents {
			if !used[p] {
				used[p] = true
				res.Inputs = append(res.Inputs, InputEvent{Input: p, Event: lineage.Event{Action: lineage.Unverified, Reason: reason}})
			}
		}
	}
	return res
}

func failed(expr parser.Expr, sel []int, err error) Result {
	res := Result{Structural: expr}
	for _, i := range sel {
		res.Inputs = append(res.Inputs, InputEvent{Input: i, Event: lineage.Event{
			Action: lineage.Unverified, Reason: "the PromQL engine failed to evaluate the expression: " + err.Error(),
		}})
	}
	return res
}

// selected returns the inputs any selector of expr matches, in order.
func selected(expr parser.Expr, lsets []labels.Labels) []int {
	sels := promqlx.Selectors(expr)
	var out []int
	for i, ls := range lsets {
		for _, vs := range sels {
			if matchesAll(ls, vs.LabelMatchers) {
				out = append(out, i)
				break
			}
		}
	}
	return out
}

func matchesAll(ls labels.Labels, ms []*labels.Matcher) bool {
	for _, m := range ms {
		if !m.Matches(ls.Get(m.Name)) {
			return false
		}
	}
	return true
}

func withoutName(ls labels.Labels) labels.Labels {
	return labels.NewBuilder(ls).Del(labels.MetricName).Labels()
}

func sortByFirstParent(outs []Output) {
	first := func(o Output) int {
		if len(o.Parents) == 0 {
			return 1 << 30
		}
		return o.Parents[0]
	}
	for i := 1; i < len(outs); i++ {
		for j := i; j > 0 && first(outs[j]) < first(outs[j-1]); j-- {
			outs[j], outs[j-1] = outs[j-1], outs[j]
		}
	}
}
