package mimir

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/unit"
)

func item(id string, temporality pmetric.AggregationTemporality, name string) unit.Metric {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr("service.name", "api")
	rm.Resource().Attributes().PutStr("service.namespace", "shop")
	rm.Resource().Attributes().PutStr("k8s.pod.name", "api-1")
	m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName(name)
	m.SetUnit("s")
	s := m.SetEmptySum()
	s.SetAggregationTemporality(temporality)
	s.SetIsMonotonic(true)
	dp := s.DataPoints().AppendEmpty()
	dp.SetTimestamp(1_700_000_000_000_000_000)
	dp.SetDoubleValue(3)
	return unit.Metric{ID: lineage.SignalID(id), Data: md}
}

func TestBackend(t *testing.T) {
	n := New(Limits{PromoteOTelResourceAttributes: "k8s.pod.name", OTelMetricSuffixesEnabled: true})
	out, err := n.ProcessMetrics(context.Background(), []unit.Metric{
		item("s1", pmetric.AggregationTemporalityCumulative, "http.server.duration"),
		item("s2", pmetric.AggregationTemporalityCumulative, "queue.wait"),
		item("s3", pmetric.AggregationTemporalityDelta, "batch.time"),
	})
	if err != nil {
		t.Fatal(err)
	}

	names := map[string]unit.Metric{}
	for _, it := range out.Items {
		names[unit.SeriesLabels(it.Data).Get("__name__")] = it
	}
	got := unit.SeriesLabels(names["http_server_duration_seconds_total"].Data)
	if got.Get("job") != "shop/api" || got.Get("k8s_pod_name") != "api-1" {
		t.Errorf("labels: %s", got)
	}

	// All items share one resource: one target_info with three parents. The
	// translator writes it even for the rejected delta metric's resource.
	ti, ok := names["target_info"]
	if !ok || len(ti.From) != 3 || len(out.Items) != 3 {
		t.Fatalf("want 2 data series and 1 shared target_info, got %d items, target_info from %v", len(out.Items), ti.From)
	}

	// Mimir rejects delta temporality by default.
	dropped := false
	for _, e := range out.Events {
		if e.Signal == "s3" && e.Action == lineage.Dropped && strings.Contains(e.Reason, "temporality") {
			dropped = true
		}
	}
	if !dropped {
		t.Errorf("delta sum should be dropped: %+v", out.Events)
	}
}
