package rules

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/rulefile"
	"github.com/Anefu/otelcov/internal/unit"
)

var at = time.Date(2026, 10, 23, 10, 0, 30, 0, time.UTC)

func series(id string, samples int, kv ...string) unit.Metric {
	var s []unit.Sample
	for i := 0; i < samples; i++ {
		s = append(s, unit.Sample{T: at.Add(-time.Duration(samples-i) * 30 * time.Second), V: float64(10 * (i + 1))})
	}
	return unit.Metric{ID: lineage.SignalID(id), Data: unit.NewSeries(labels.FromStrings(kv...), s...)}
}

func rule(t *testing.T, record, expr string) rulefile.Recording {
	t.Helper()
	e, err := parser.ParseExpr(expr)
	if err != nil {
		t.Fatal(err)
	}
	return rulefile.Recording{Record: record, Expr: e}
}

func run(t *testing.T, rs []rulefile.Recording, in ...unit.Metric) map[string][]lineage.Event {
	t.Helper()
	n, err := New(rs, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := n.ProcessMetrics(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	bySignal := map[string][]lineage.Event{}
	for _, e := range out.Events {
		bySignal[string(e.Signal)] = append(bySignal[string(e.Signal)], e)
	}
	return bySignal
}

func TestRateNeedsTwoSamples(t *testing.T) {
	rs := []rulefile.Recording{rule(t, "job:requests:rate5m", `sum by (job) (rate(http_requests_total[5m]))`)}
	ev := run(t, rs,
		series("p1", 3, "__name__", "http_requests_total", "job", "api", "instance", "a"),
		series("p2", 1, "__name__", "http_requests_total", "job", "web", "instance", "b"),
	)
	if len(ev["r1"]) != 1 || ev["r1"][0].Action != lineage.Derived || ev["r1"][0].From[0] != "p1" {
		t.Errorf("api should produce r1 from p1: %+v", ev["r1"])
	}
	if e := ev["p2"]; len(e) != 1 || e[0].Action != lineage.Unverified || !strings.Contains(e[0].Reason, "two samples") {
		t.Errorf("web has one sample: want unverified, got %+v", e)
	}
}

func TestDependencyOrder(t *testing.T) {
	// Listed in the wrong order on purpose: the second reads the first.
	rs := []rulefile.Recording{
		rule(t, "job:up:sum", `sum by (job) (instance:up:max)`),
		rule(t, "instance:up:max", `max by (job, instance) (up)`),
	}
	ev := run(t, rs, series("p1", 1, "__name__", "up", "job", "api", "instance", "a"))
	if e := ev["r1"]; len(e) != 1 || e[0].Node != "rule/instance:up:max" {
		t.Errorf("instance:up:max should run first: %+v", e)
	}
	if e := ev["r2"]; len(e) != 1 || e[0].Node != "rule/job:up:sum" || e[0].From[0] != "r1" {
		t.Errorf("job:up:sum should read r1: %+v", e)
	}

	cyc := []rulefile.Recording{rule(t, "a", `b`), rule(t, "b", `a`)}
	if _, err := New(cyc, at, nil); err == nil {
		t.Error("a cycle should be an error")
	}
}

func TestUnsupportedFallsBackToSelected(t *testing.T) {
	rs := []rulefile.Recording{rule(t, "x", `count_values("v", up)`)}
	ev := run(t, rs, series("p1", 1, "__name__", "up", "job", "api"))
	var derived, unverified bool
	for _, e := range ev["r1"] {
		derived = derived || (e.Action == lineage.Derived && e.From[0] == "p1")
		unverified = unverified || (e.Action == lineage.Unverified && strings.Contains(e.Reason, "count_values"))
	}
	if !derived || !unverified {
		t.Errorf("want output with over-approximated parents, marked unverified: %+v", ev["r1"])
	}
}
