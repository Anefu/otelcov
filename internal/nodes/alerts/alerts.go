// Package alerts evaluates alerting rules over series items for coverage.
//
// Thresholds are ignored (the rule is rewritten so comparisons keep every
// series that could pass) and `for` is treated as met: the question is
// whether a series could make the alert fire, not whether it fires now.
package alerts

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"github.com/Anefu/otelcov/internal/alerting"
	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/nodes/rules"
	"github.com/Anefu/otelcov/internal/promqlx"
	"github.com/Anefu/otelcov/internal/ruleeval"
	"github.com/Anefu/otelcov/internal/rulefile"
	"github.com/Anefu/otelcov/internal/unit"
)

// Prefix starts the node name of each alerting rule: "alert/<alertname>".
const Prefix = "alert/"

const coverageNote = "thresholds are ignored: coverage asks whether a series could make the alert fire, not whether it fires now"

// Node evaluates alerting rules.
type Node struct {
	rules  []rulefile.Alerting
	at     time.Time
	engine *promqlx.Engine
}

// New builds the node.
func New(rs []rulefile.Alerting, at time.Time) *Node {
	return &Node{rules: rs, at: at, engine: promqlx.NewEngine()}
}

// Process evaluates every alerting rule over items.
func (n *Node) Process(ctx context.Context, items []unit.Metric) (alerting.Output, error) {
	var out alerting.Output
	next := 0
	for _, r := range n.rules {
		if err := ctx.Err(); err != nil {
			return alerting.Output{}, err
		}
		name := Prefix + r.Alert
		res := ruleeval.Evaluate(ctx, n.engine, r.Expr, items, n.at)
		var ev rules.Positioned
		templated := templatedLabels(r.Labels)

		for _, o := range res.Outputs {
			next++
			id := lineage.SignalID("a" + strconv.Itoa(next))
			from := rules.IDs(items, o.Parents)
			b := labels.NewBuilder(o.Labels).Del(labels.MetricName)
			for k, v := range r.Labels {
				b.Set(k, v)
			}
			b.Set(labels.AlertName, r.Alert)
			lset := b.Labels()
			out.Alerts = append(out.Alerts, alerting.Alert{ID: id, From: from, Labels: lset, Annotations: r.Annotations, For: r.For})
			out.Signals = append(out.Signals, lineage.Signal{ID: id, Node: name, Kind: "alert", From: from, Labels: lset.Map()})

			detail := rules.ExprDetail(r.Expr.String(), res)
			if r.For > 0 {
				detail["for"] = r.For.String()
			}
			reason := coverageNote
			if r.For > 0 {
				reason += "; `for` is treated as met"
			}
			pos := rules.First(o.Parents)
			ev.Add(pos, 0, lineage.Event{Node: name, Signal: id, Action: lineage.Derived, From: from, Reason: reason, Detail: detail})
			if o.Note != "" {
				ev.Add(pos, 1, lineage.Event{Node: name, Signal: id, Action: lineage.Unverified, Reason: o.Note})
			}
			if len(templated) > 0 {
				ev.Add(pos, 1, lineage.Event{Node: name, Signal: id, Action: lineage.Unverified,
					Reason: "labels " + strings.Join(templated, ", ") + " are templates, which aren't expanded yet; routing on them is uncertain"})
			}
		}
		for _, ie := range res.Inputs {
			e := ie.Event
			e.Node, e.Signal = name, items[ie.Input].ID
			ev.Add(ie.Input, 2, e)
		}
		out.Events = append(out.Events, ev.Events()...)
	}
	return out, nil
}

func templatedLabels(ls map[string]string) []string {
	var out []string
	for k, v := range ls {
		if strings.Contains(v, "{{") {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
