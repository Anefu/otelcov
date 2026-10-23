package unit

import (
	"testing"

	"go.opentelemetry.io/collector/pdata/pmetric"
)

func TestSplitAndGroup(t *testing.T) {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr("k8s.pod.name", "p")
	sm := rm.ScopeMetrics().AppendEmpty()
	sm.Scope().SetName("s")

	g := sm.Metrics().AppendEmpty()
	g.SetName("gauge")
	gauge := g.SetEmptyGauge()
	for i := 0; i < 3; i++ {
		gauge.DataPoints().AppendEmpty().SetIntValue(int64(i))
	}

	s := sm.Metrics().AppendEmpty()
	s.SetName("sum")
	sum := s.SetEmptySum()
	sum.SetAggregationTemporality(pmetric.AggregationTemporalityDelta)
	sum.SetIsMonotonic(true)
	for i := 0; i < 2; i++ {
		sum.DataPoints().AppendEmpty().SetDoubleValue(float64(10 + i))
	}

	sm.Metrics().AppendEmpty().SetName("empty") // no data points: no items

	parts := SplitMetrics(md)
	if len(parts) != 5 {
		t.Fatalf("got %d items, want 5", len(parts))
	}
	for i, p := range parts {
		if p.DataPointCount() != 1 {
			t.Errorf("item %d has %d data points", i, p.DataPointCount())
		}
		if v, _ := Resource(p).Attributes().Get("k8s.pod.name"); v.Str() != "p" {
			t.Errorf("item %d lost its resource", i)
		}
	}
	if v := FirstMetric(parts[1]).Gauge().DataPoints().At(0).IntValue(); v != 1 {
		t.Errorf("second gauge item has value %d, want 1", v)
	}
	last := FirstMetric(parts[4]).Sum()
	if last.DataPoints().At(0).DoubleValue() != 11 || !last.IsMonotonic() ||
		last.AggregationTemporality() != pmetric.AggregationTemporalityDelta {
		t.Errorf("sum item lost its fields")
	}

	items := make([]Metric, len(parts))
	for i, p := range parts {
		items[i] = Metric{ID: "x", Data: p}
	}
	groups := GroupByMetric(items)
	if len(groups) != 2 || len(groups[0]) != 3 || len(groups[1]) != 2 {
		t.Fatalf("groups: %d", len(groups))
	}
	merged := MergeMetrics(parts[0], parts[1])
	if merged.DataPointCount() != 2 {
		t.Errorf("merged has %d points", merged.DataPointCount())
	}
}
