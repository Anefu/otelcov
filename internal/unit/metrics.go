// Package unit splits pdata into the units nodes process (items and metric
// groups) and merges them back.
//
// Real processors run once per unit, never on whole batches, so outputs can be
// matched to inputs exactly. The lineage envelope (ID, parents) lives in Item,
// outside pdata, where processors can't strip or react to it.
package unit

import (
	"encoding/json"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/Anefu/otelcov/internal/lineage"
)

// Item wraps one unit of telemetry with its lineage envelope.
type Item[T any] struct {
	ID   lineage.SignalID
	From []lineage.SignalID
	Data T
}

// Metric is a metrics item: exactly one resource, one scope, one metric and,
// for items, one data point.
type Metric = Item[pmetric.Metrics]

// SplitMetrics returns one Metrics per data point. Each carries copies of its
// resource, scope and metric fields. Metrics without data points produce no
// items.
func SplitMetrics(md pmetric.Metrics) []pmetric.Metrics {
	var out []pmetric.Metrics
	rms := md.ResourceMetrics()
	for i := 0; i < rms.Len(); i++ {
		rm := rms.At(i)
		sms := rm.ScopeMetrics()
		for j := 0; j < sms.Len(); j++ {
			sm := sms.At(j)
			ms := sm.Metrics()
			for k := 0; k < ms.Len(); k++ {
				m := ms.At(k)
				for p := 0; p < DataPointCount(m); p++ {
					out = append(out, single(rm, sm, m, p))
				}
			}
		}
	}
	return out
}

// single copies data point p of m, with its resource and scope.
func single(rm pmetric.ResourceMetrics, sm pmetric.ScopeMetrics, m pmetric.Metric, p int) pmetric.Metrics {
	out := pmetric.NewMetrics()
	orm := out.ResourceMetrics().AppendEmpty()
	rm.Resource().CopyTo(orm.Resource())
	orm.SetSchemaUrl(rm.SchemaUrl())
	osm := orm.ScopeMetrics().AppendEmpty()
	sm.Scope().CopyTo(osm.Scope())
	osm.SetSchemaUrl(sm.SchemaUrl())
	om := osm.Metrics().AppendEmpty()
	m.CopyTo(om)
	keepOnly(om, p)
	return out
}

// keepOnly removes every data point of m except index p.
func keepOnly(m pmetric.Metric, p int) {
	i := 0
	drop := func() bool { keep := i == p; i++; return !keep }
	switch m.Type() {
	case pmetric.MetricTypeGauge:
		m.Gauge().DataPoints().RemoveIf(func(pmetric.NumberDataPoint) bool { return drop() })
	case pmetric.MetricTypeSum:
		m.Sum().DataPoints().RemoveIf(func(pmetric.NumberDataPoint) bool { return drop() })
	case pmetric.MetricTypeHistogram:
		m.Histogram().DataPoints().RemoveIf(func(pmetric.HistogramDataPoint) bool { return drop() })
	case pmetric.MetricTypeExponentialHistogram:
		m.ExponentialHistogram().DataPoints().RemoveIf(func(pmetric.ExponentialHistogramDataPoint) bool { return drop() })
	case pmetric.MetricTypeSummary:
		m.Summary().DataPoints().RemoveIf(func(pmetric.SummaryDataPoint) bool { return drop() })
	}
}

// DataPointCount returns the number of data points of m.
func DataPointCount(m pmetric.Metric) int {
	switch m.Type() {
	case pmetric.MetricTypeGauge:
		return m.Gauge().DataPoints().Len()
	case pmetric.MetricTypeSum:
		return m.Sum().DataPoints().Len()
	case pmetric.MetricTypeHistogram:
		return m.Histogram().DataPoints().Len()
	case pmetric.MetricTypeExponentialHistogram:
		return m.ExponentialHistogram().DataPoints().Len()
	case pmetric.MetricTypeSummary:
		return m.Summary().DataPoints().Len()
	}
	return 0
}

// Resource returns the resource of a single-resource Metrics, such as an item.
func Resource(md pmetric.Metrics) pcommon.Resource {
	return md.ResourceMetrics().At(0).Resource()
}

// FirstMetric returns the first metric of md, such as an item's only metric.
func FirstMetric(md pmetric.Metrics) pmetric.Metric {
	return md.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0)
}

// GroupByMetric groups items that belong to the same metric: same resource,
// scope and metric identity (name, type, unit). Groups keep first-seen order,
// and items keep their order within a group.
func GroupByMetric(items []Metric) [][]Metric {
	var groups [][]Metric
	index := map[string]int{}
	for _, it := range items {
		k := metricKey(it.Data)
		g, ok := index[k]
		if !ok {
			g = len(groups)
			index[k] = g
			groups = append(groups, nil)
		}
		groups[g] = append(groups[g], it)
	}
	return groups
}

func metricKey(md pmetric.Metrics) string {
	rm := md.ResourceMetrics().At(0)
	sm := rm.ScopeMetrics().At(0)
	m := sm.Metrics().At(0)
	// json.Marshal sorts map keys, so equal attribute sets give equal keys.
	b, _ := json.Marshal([]any{
		rm.Resource().Attributes().AsRaw(), rm.SchemaUrl(),
		sm.Scope().Name(), sm.Scope().Version(), sm.Scope().Attributes().AsRaw(), sm.SchemaUrl(),
		m.Name(), m.Type().String(), m.Unit(),
	})
	return string(b)
}

// MergeMetrics concatenates parts into one Metrics, copying them. It does not
// regroup resources or scopes.
func MergeMetrics(parts ...pmetric.Metrics) pmetric.Metrics {
	out := pmetric.NewMetrics()
	for _, p := range parts {
		rms := p.ResourceMetrics()
		for i := 0; i < rms.Len(); i++ {
			rms.At(i).CopyTo(out.ResourceMetrics().AppendEmpty())
		}
	}
	return out
}

// CombineMetric merges items of one metric (same resource, scope and metric
// identity, as grouped by GroupByMetric) into a single Metrics with one
// metric holding all their data points, in item order.
func CombineMetric(items []Metric) pmetric.Metrics {
	out := pmetric.NewMetrics()
	if len(items) == 0 {
		return out
	}
	first := items[0].Data.ResourceMetrics().At(0)
	orm := out.ResourceMetrics().AppendEmpty()
	first.Resource().CopyTo(orm.Resource())
	orm.SetSchemaUrl(first.SchemaUrl())
	fsm := first.ScopeMetrics().At(0)
	osm := orm.ScopeMetrics().AppendEmpty()
	fsm.Scope().CopyTo(osm.Scope())
	osm.SetSchemaUrl(fsm.SchemaUrl())
	om := osm.Metrics().AppendEmpty()
	FirstMetric(items[0].Data).CopyTo(om)
	for _, it := range items[1:] {
		appendPoints(FirstMetric(it.Data), om)
	}
	return out
}

// appendPoints appends copies of the data points of src to dst (same type).
func appendPoints(src, dst pmetric.Metric) {
	switch src.Type() {
	case pmetric.MetricTypeGauge:
		for i := 0; i < src.Gauge().DataPoints().Len(); i++ {
			src.Gauge().DataPoints().At(i).CopyTo(dst.Gauge().DataPoints().AppendEmpty())
		}
	case pmetric.MetricTypeSum:
		for i := 0; i < src.Sum().DataPoints().Len(); i++ {
			src.Sum().DataPoints().At(i).CopyTo(dst.Sum().DataPoints().AppendEmpty())
		}
	case pmetric.MetricTypeHistogram:
		for i := 0; i < src.Histogram().DataPoints().Len(); i++ {
			src.Histogram().DataPoints().At(i).CopyTo(dst.Histogram().DataPoints().AppendEmpty())
		}
	case pmetric.MetricTypeExponentialHistogram:
		for i := 0; i < src.ExponentialHistogram().DataPoints().Len(); i++ {
			src.ExponentialHistogram().DataPoints().At(i).CopyTo(dst.ExponentialHistogram().DataPoints().AppendEmpty())
		}
	case pmetric.MetricTypeSummary:
		for i := 0; i < src.Summary().DataPoints().Len(); i++ {
			src.Summary().DataPoints().At(i).CopyTo(dst.Summary().DataPoints().AppendEmpty())
		}
	}
}
