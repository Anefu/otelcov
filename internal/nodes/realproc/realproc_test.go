package realproc

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/nodes/nodetest"
	"github.com/Anefu/otelcov/internal/unit"
)

func sumItem(id, name string, temporality pmetric.AggregationTemporality, v float64, pointAttrs map[string]string, res map[string]string) unit.Metric {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	for k, val := range res {
		rm.Resource().Attributes().PutStr(k, val)
	}
	m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName(name)
	s := m.SetEmptySum()
	s.SetAggregationTemporality(temporality)
	s.SetIsMonotonic(true)
	dp := s.DataPoints().AppendEmpty()
	dp.SetStartTimestamp(1_000_000_000)
	dp.SetTimestamp(2_000_000_000)
	dp.SetDoubleValue(v)
	for k, val := range pointAttrs {
		dp.Attributes().PutStr(k, val)
	}
	return unit.Metric{ID: lineage.SignalID(id), Data: md}
}

func run(t *testing.T, id string, cfg map[string]any, in ...unit.Metric) (map[lineage.SignalID]unit.Metric, []lineage.Event, []lineage.Signal) {
	t.Helper()
	n, err := New(context.Background(), "agent/"+id, id, cfg, lineage.NewIDs("c"))
	if err != nil {
		t.Fatal(err)
	}
	defer n.Shutdown(context.Background())
	out, err := n.ProcessMetrics(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	items := map[lineage.SignalID]unit.Metric{}
	for _, it := range out.Items {
		items[it.ID] = it
	}
	return items, out.Events, out.Signals
}

func find(events []lineage.Event, signal lineage.SignalID, attr string, action lineage.Action) bool {
	for _, e := range events {
		if e.Signal == signal && e.Attribute == attr && e.Action == action {
			return true
		}
	}
	return false
}

func TestTransform(t *testing.T) {
	cfg := map[string]any{"metric_statements": []any{
		map[string]any{"context": "resource", "statements": []any{`set(attributes["team"], "ml") where attributes["service.name"] == "vllm"`}},
		map[string]any{"context": "datapoint", "statements": []any{`delete_key(attributes, "drop_me")`}},
	}}
	items, events, _ := run(t, "transform", cfg,
		sumItem("s1", "m", pmetric.AggregationTemporalityCumulative, 1, map[string]string{"drop_me": "x", "keep": "y"}, map[string]string{"service.name": "vllm"}),
		sumItem("s2", "m", pmetric.AggregationTemporalityCumulative, 2, nil, map[string]string{"service.name": "other"}),
	)
	if v, _ := unit.Resource(items["s1"].Data).Attributes().Get("team"); v.Str() != "ml" {
		t.Errorf("team not set on s1")
	}
	if !find(events, "s1", "team", lineage.Set) || !find(events, "s1", "drop_me", lineage.Removed) {
		t.Errorf("events: %+v", events)
	}
	if find(events, "s2", "team", lineage.Set) {
		t.Error("condition was false for s2")
	}
}

func TestFilterDrops(t *testing.T) {
	cfg := map[string]any{"metrics": map[string]any{"metric": []any{`name == "junk"`}}}
	items, events, _ := run(t, "filter/junk", cfg,
		sumItem("s1", "junk", pmetric.AggregationTemporalityCumulative, 1, nil, nil),
		sumItem("s2", "useful", pmetric.AggregationTemporalityCumulative, 1, nil, nil),
	)
	if _, ok := items["s1"]; ok || !find(events, "s1", "", lineage.Dropped) {
		t.Errorf("s1 should be dropped: %+v", events)
	}
	if _, ok := items["s2"]; !ok {
		t.Error("s2 should pass")
	}
}

func TestAttributesResourceMetricsTransform(t *testing.T) {
	in := sumItem("s1", "old", pmetric.AggregationTemporalityCumulative, 1, nil, nil)

	items, events, _ := run(t, "attributes", map[string]any{"actions": []any{map[string]any{"key": "env", "value": "staging", "action": "upsert"}}}, in)
	if !find(events, "s1", "env", lineage.Set) || nodetest.Attrs(items["s1"])["env"] != "" {
		t.Errorf("attributes: env should be a datapoint attribute: %+v", events)
	}

	items, events, _ = run(t, "resource", map[string]any{"attributes": []any{map[string]any{"key": "cluster", "value": "kind", "action": "upsert"}}}, in)
	if !find(events, "s1", "cluster", lineage.Set) || nodetest.Attrs(items["s1"])["cluster"] != "kind" {
		t.Errorf("resource: %+v", events)
	}

	items, events, _ = run(t, "metricstransform", map[string]any{"transforms": []any{map[string]any{"include": "old", "action": "update", "new_name": "new"}}}, in)
	if !find(events, "s1", "metric.name", lineage.Set) || unit.FirstMetric(items["s1"].Data).Name() != "new" {
		t.Errorf("metricstransform: %+v", events)
	}
}

func TestDeltaToCumulative(t *testing.T) {
	items, events, _ := run(t, "deltatocumulative", nil,
		sumItem("s1", "requests", pmetric.AggregationTemporalityDelta, 5, nil, nil))
	it, ok := items["s1"]
	if !ok {
		t.Fatalf("no output: %+v", events)
	}
	if got := unit.FirstMetric(it.Data).Sum().AggregationTemporality(); got != pmetric.AggregationTemporalityCumulative {
		t.Errorf("temporality %s", got)
	}
	if !find(events, "s1", "metric.temporality", lineage.Set) {
		t.Errorf("events: %+v", events)
	}
}

func TestAggregationHasEveryInputAsParent(t *testing.T) {
	cfg := map[string]any{"metric_statements": []any{
		map[string]any{"context": "metric", "statements": []any{`aggregate_on_attributes("sum", []) where metric.name == "m"`}},
	}}
	items, events, signals := run(t, "transform/agg", cfg,
		sumItem("s1", "m", pmetric.AggregationTemporalityCumulative, 1, map[string]string{"pod": "a"}, nil),
		sumItem("s2", "m", pmetric.AggregationTemporalityCumulative, 2, map[string]string{"pod": "b"}, nil),
	)
	if len(items) != 1 || len(signals) != 1 || len(signals[0].From) != 2 {
		t.Fatalf("want one aggregated signal from both inputs; items %d, signals %+v, events %+v", len(items), signals, events)
	}
}

func TestUnknownConfigIsRejected(t *testing.T) {
	if _, err := New(context.Background(), "x", "resource", map[string]any{"nope": 1}, lineage.NewIDs("c")); err == nil {
		t.Error("unknown keys must be rejected")
	}
	if _, err := New(context.Background(), "x", "groupbyattrs", nil, lineage.NewIDs("c")); err == nil {
		t.Error("unsupported types must be rejected")
	}
}

func TestStatementExplanations(t *testing.T) {
	cfg := map[string]any{"metric_statements": []any{
		map[string]any{"context": "resource", "statements": []any{
			`set(attributes["team"], "search") where attributes["k8s.namespace.name"] == "search"`,
			`set(attributes["tier"], "gpu") where attributes["k8s.namespace.name"] == "inference"`,
		}},
		map[string]any{"context": "datapoint", "statements": []any{`delete_key(attributes, "user_id")`}},
	}}
	_, events, _ := run(t, "transform/team", cfg,
		sumItem("s1", "m", pmetric.AggregationTemporalityCumulative, 1, map[string]string{"user_id": "u"}, map[string]string{"k8s.namespace.name": "inference"}))
	var set, skipped, removed *lineage.Event
	for i, e := range events {
		switch {
		case e.Attribute == "tier" && e.Action == lineage.Set:
			set = &events[i]
		case e.Attribute == "team" && e.Action == lineage.Skipped:
			skipped = &events[i]
		case e.Attribute == "user_id" && e.Action == lineage.Removed:
			removed = &events[i]
		case e.Action == lineage.Unverified:
			t.Errorf("the statement explanation should match the processor: %+v", e)
		}
	}
	if set == nil || !strings.Contains(set.Reason, `"gpu"`) {
		t.Errorf("tier set should name its statement: %+v", set)
	}
	if skipped == nil || skipped.Detail["would_set"] != "search" || skipped.Detail["condition"] != false ||
		skipped.Detail["where"] != `attributes["k8s.namespace.name"] == "search"` {
		t.Errorf("the search statement should be skipped with what it would set: %+v", skipped)
	}
	if removed == nil || removed.Detail["statement"] != `delete_key(attributes, "user_id")` {
		t.Errorf("removal should name its statement: %+v", removed)
	}
}

// A statement that didn't run isn't reported when another statement set the
// same attribute: the diagnosis would otherwise blame a statement that
// doesn't matter.
func TestSkippedHiddenWhenAnotherStatementSets(t *testing.T) {
	cfg := map[string]any{"metric_statements": []any{
		map[string]any{"context": "resource", "statements": []any{
			`set(attributes["team"], "search") where attributes["k8s.namespace.name"] == "search"`,
			`set(attributes["team"], "ml") where attributes["k8s.namespace.name"] == "inference"`,
		}},
	}}
	_, events, _ := run(t, "transform/team", cfg,
		sumItem("s1", "m", pmetric.AggregationTemporalityCumulative, 1, nil, map[string]string{"k8s.namespace.name": "inference"}))
	var set bool
	for _, e := range events {
		if e.Attribute == "team" && e.Action == lineage.Skipped {
			t.Errorf("unexpected skipped event: %+v", e)
		}
		set = set || (e.Attribute == "team" && e.Action == lineage.Set && strings.Contains(e.Reason, `"ml"`))
	}
	if !set {
		t.Errorf("team should be set by the ml statement: %+v", events)
	}
}

// eventsFor returns the events about one attribute, by action.
func eventsFor(events []lineage.Event, attr string) map[lineage.Action]lineage.Event {
	out := map[lineage.Action]lineage.Event{}
	for _, e := range events {
		if e.Attribute == attr {
			out[e.Action] = e
		}
	}
	return out
}

func noUnverified(t *testing.T, events []lineage.Event) {
	t.Helper()
	for _, e := range events {
		if e.Action == lineage.Unverified {
			t.Errorf("the statement explanation should match the processor: %+v", e)
		}
	}
}

// convert_sum_to_gauge is a transform-processor function, not standard OTTL;
// the statement explainer runs the real processor, so it names the statement
// like any other.
func TestTransformOnlyFunction(t *testing.T) {
	stmt := `convert_sum_to_gauge() where metric.name == "m"`
	cfg := map[string]any{"metric_statements": []any{
		map[string]any{"context": "metric", "statements": []any{stmt}},
	}}
	_, events, _ := run(t, "transform", cfg, sumItem("s1", "m", pmetric.AggregationTemporalityCumulative, 1, nil, nil))
	noUnverified(t, events)
	set, ok := eventsFor(events, "metric.type")[lineage.Set]
	if !ok || set.Detail["statement"] != stmt || set.Reason != "set by metric statement `"+stmt+"`" {
		t.Errorf("the conversion should name its statement: %+v", events)
	}
}

// A group's conditions apply to all its statements, ORed; a statement the
// conditions stop is skipped, with the conditions as its where.
func TestGroupConditions(t *testing.T) {
	cfg := map[string]any{"metric_statements": []any{
		map[string]any{
			"context":    "resource",
			"conditions": []any{`attributes["k8s.namespace.name"] == "search"`, `attributes["k8s.namespace.name"] == "web"`},
			"statements": []any{
				`set(attributes["team"], "frontend")`,
				`set(attributes["tier"], "cpu") where attributes["tier"] == nil`,
			},
		},
		map[string]any{
			"context":    "resource",
			"conditions": []any{`attributes["k8s.namespace.name"] == "inference"`},
			"statements": []any{`set(attributes["tier"], "gpu")`},
		},
	}}
	_, events, _ := run(t, "transform/team", cfg,
		sumItem("s1", "m", pmetric.AggregationTemporalityCumulative, 1, nil, map[string]string{"k8s.namespace.name": "inference"}))
	noUnverified(t, events)
	team := eventsFor(events, "team")[lineage.Skipped]
	if team.Detail["would_set"] != "frontend" ||
		team.Detail["where"] != `attributes["k8s.namespace.name"] == "search" or attributes["k8s.namespace.name"] == "web"` {
		t.Errorf("team should be skipped by the group's conditions: %+v", events)
	}
	tier := eventsFor(events, "tier")
	if tier[lineage.Set].Detail["statement"] != `set(attributes["tier"], "gpu")` {
		t.Errorf("tier should be set by the second group: %+v", events)
	}
	if _, ok := tier[lineage.Skipped]; ok {
		t.Errorf("the cpu statement shouldn't be reported: another statement set tier: %+v", events)
	}
}

func TestCombine(t *testing.T) {
	for _, tc := range []struct {
		conds []string
		where string
		want  string
	}{
		{nil, "", ""},
		{nil, "a", "a"},
		{[]string{"c"}, "", "c"},
		{[]string{"c"}, "a", "c and a"},
		{[]string{"c", "d"}, "", "c or d"},
		{[]string{"c", "d"}, "a", "(c or d) and a"},
	} {
		if got := combine(tc.conds, tc.where); got != tc.want {
			t.Errorf("combine(%q, %q) = %q, want %q", tc.conds, tc.where, got, tc.want)
		}
	}
}

// Statements without a context block (basic config) have their context
// inferred by the processor; reasons then name only the statement.
func TestStatementsWithoutContext(t *testing.T) {
	setTeam := `set(resource.attributes["team"], "search") where resource.attributes["k8s.namespace.name"] == "search"`
	setTier := `set(resource.attributes["tier"], "gpu") where resource.attributes["k8s.namespace.name"] == "inference"`
	cfg := map[string]any{"metric_statements": []any{setTeam, setTier}}
	_, events, _ := run(t, "transform", cfg,
		sumItem("s1", "m", pmetric.AggregationTemporalityCumulative, 1, nil, map[string]string{"k8s.namespace.name": "inference"}))
	noUnverified(t, events)
	tier := eventsFor(events, "tier")[lineage.Set]
	if tier.Reason != "set by statement `"+setTier+"`" || tier.Detail["context"] != nil {
		t.Errorf("tier should be set by its statement, with no context: %+v", tier)
	}
	team := eventsFor(events, "team")[lineage.Skipped]
	if team.Detail["would_set"] != "search" || team.Reason != "statement `"+setTeam+"` didn't run: its condition is false for this signal" {
		t.Errorf("team should be skipped: %+v", team)
	}
}

// A later statement that reads what an earlier one set sees it, as in the
// processor.
func TestStatementsSeeEarlierChanges(t *testing.T) {
	cfg := map[string]any{"metric_statements": []any{
		map[string]any{"context": "resource", "statements": []any{
			`set(attributes["team"], "inference")`,
			`set(attributes["pager"], "inference-pager") where attributes["team"] == "inference"`,
		}},
	}}
	_, events, _ := run(t, "transform", cfg, sumItem("s1", "m", pmetric.AggregationTemporalityCumulative, 1, nil, nil))
	noUnverified(t, events)
	if !strings.Contains(eventsFor(events, "pager")[lineage.Set].Reason, `"inference-pager"`) {
		t.Errorf("pager should be set by the second statement: %+v", events)
	}
}

func TestEditorPart(t *testing.T) {
	for in, want := range map[string]string{
		`set(a, "x") where b == "y"`:         `set(a, "x")`,
		`set(a, "has where inside") where c`: `set(a, "has where inside")`,
		`delete_key(attributes, "k")`:        `delete_key(attributes, "k")`,
		`set(a, 'quoted \' where ') where x`: `set(a, 'quoted \' where ')`,
	} {
		if got := editorPart(in); got != want {
			t.Errorf("editorPart(%q) = %q, want %q", in, got, want)
		}
	}
}

// gaugePoint is a one-point gauge with a "pod" point attribute, all at the
// same timestamp, as from one scrape.
func gaugePoint(id string, v float64, pod string) unit.Metric {
	md := pmetric.NewMetrics()
	m := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName("m")
	dp := m.SetEmptyGauge().DataPoints().AppendEmpty()
	dp.SetTimestamp(2_000_000_000)
	dp.SetDoubleValue(v)
	dp.Attributes().PutStr("pod", pod)
	return unit.Metric{ID: lineage.SignalID(id), Data: md}
}

func podOf(it unit.Metric) string {
	v, _ := unit.FirstMetric(it.Data).Gauge().DataPoints().At(0).Attributes().Get("pod")
	return v.AsString()
}

// Two points with the same timestamp and value: the one the filter drops is
// the one reported dropped (otelcov used to swap them).
func TestGroupMatchingUsesPointAttributes(t *testing.T) {
	cfg := map[string]any{"metrics": map[string]any{"datapoint": []any{`attributes["pod"] == "a"`}}}
	items, events, _ := run(t, "filter/drop-a", cfg, gaugePoint("pod-a", 0, "a"), gaugePoint("pod-b", 0, "b"))
	if _, kept := items["pod-a"]; kept {
		t.Errorf("pod a's point should be dropped: %+v", events)
	}
	if b, ok := items["pod-b"]; !ok || podOf(b) != "b" {
		t.Errorf("pod b's point should pass unchanged: %+v", items)
	}
	if !find(events, "pod-a", "", lineage.Dropped) || find(events, "pod-b", "", lineage.Dropped) || find(events, "pod-b", "pod", lineage.Set) {
		t.Errorf("events: %+v", events)
	}
}

// Points with the same timestamp and value whose attributes the processor
// changes differently can't be told apart: the match is unverified, not
// guessed. When every output is identical, the order can't matter.
func TestAmbiguousGroupMatchIsUnverified(t *testing.T) {
	stmts := func(s ...any) map[string]any {
		return map[string]any{"metric_statements": []any{map[string]any{"context": "datapoint", "statements": s}}}
	}
	_, events, _ := run(t, "transform/rename", stmts(
		`set(attributes["pod"], "x") where attributes["pod"] == "a"`,
		`set(attributes["pod"], "y") where attributes["pod"] == "b"`,
	), gaugePoint("pod-a", 0, "a"), gaugePoint("pod-b", 0, "b"))
	if !find(events, "pod-a", "", lineage.Unverified) || !find(events, "pod-b", "", lineage.Unverified) {
		t.Errorf("both matches should be unverified: %+v", events)
	}

	items, events, _ := run(t, "transform/same", stmts(`set(attributes["pod"], "x")`),
		gaugePoint("pod-a", 0, "a"), gaugePoint("pod-b", 0, "b"))
	if len(items) != 2 || find(events, "pod-a", "", lineage.Unverified) || find(events, "pod-b", "", lineage.Unverified) {
		t.Errorf("identical outputs need no uncertainty: items %d, events %+v", len(items), events)
	}

	// Changed attributes, but only one input left with that timestamp and
	// value: a certain match.
	items, events, _ = run(t, "transform/one", stmts(`set(attributes["pod"], "x") where attributes["pod"] == "a"`),
		gaugePoint("pod-a", 0, "a"), gaugePoint("pod-b", 0, "b"))
	if podOf(items["pod-a"]) != "x" || podOf(items["pod-b"]) != "b" || find(events, "pod-a", "", lineage.Unverified) {
		t.Errorf("want pod a renamed and pod b unchanged: %+v", events)
	}
}
