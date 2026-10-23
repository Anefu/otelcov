package alerts

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

func series(id string, v float64, kv ...string) unit.Metric {
	return unit.Metric{ID: lineage.SignalID(id), Data: unit.NewSeries(labels.FromStrings(kv...), unit.Sample{T: at.Add(-30 * time.Second), V: v})}
}

func TestThresholdsIgnored(t *testing.T) {
	expr, _ := parser.ParseExpr(`queue_depth > 100 and on(job) up == 1`)
	n := New([]rulefile.Alerting{{
		Alert: "QueueBacklog", Expr: expr,
		Labels: map[string]string{"severity": "page", "summary_level": "{{ $labels.job }}"},
	}}, at, nil)
	out, err := n.Process(context.Background(), []unit.Metric{
		series("p1", 5, "__name__", "queue_depth", "job", "a"),   // below threshold
		series("p2", 500, "__name__", "queue_depth", "job", "b"), // above
		series("p3", 0, "__name__", "up", "job", "a"),            // down
		series("p4", 1, "__name__", "up", "job", "b"),
		series("p5", 9, "__name__", "queue_depth", "job", "c"), // no up series
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, a := range out.Alerts {
		got[a.Labels.Get("job")] = true
		if a.Name() != "QueueBacklog" || a.Labels.Get("severity") != "page" || a.Labels.Has("__name__") {
			t.Errorf("labels: %s", a.Labels)
		}
	}
	if !got["a"] || !got["b"] || got["c"] || len(out.Alerts) != 2 {
		t.Errorf("want alerts for jobs a and b regardless of values, got %v", got)
	}
	var droppedC, templated bool
	for _, e := range out.Events {
		droppedC = droppedC || (e.Signal == "p5" && e.Action == lineage.Dropped && strings.Contains(e.Reason, "and"))
		templated = templated || (e.Action == lineage.Unverified && strings.Contains(e.Reason, "summary_level"))
	}
	if !droppedC {
		t.Errorf("job c has no up series: want dropped by and: %+v", out.Events)
	}
	if !templated {
		t.Error("templated label should be unverified")
	}
}
