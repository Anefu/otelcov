package unit

import (
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

// Series items are Prometheus-shaped pmetric: an empty resource and scope, one
// gauge named after the series, and one data point whose attributes are all
// other labels. This is what the backend stores and what rules read.

// NewSeries builds a series item from its labels (including __name__) and one
// sample.
func NewSeries(lset labels.Labels, ts time.Time, value float64) pmetric.Metrics {
	md := pmetric.NewMetrics()
	m := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName(lset.Get(labels.MetricName))
	dp := m.SetEmptyGauge().DataPoints().AppendEmpty()
	dp.SetTimestamp(pcommon.NewTimestampFromTime(ts))
	dp.SetDoubleValue(value)
	lset.Range(func(l labels.Label) {
		if l.Name != labels.MetricName {
			dp.Attributes().PutStr(l.Name, l.Value)
		}
	})
	return md
}

// SeriesLabels returns a series item's labels, including __name__.
func SeriesLabels(md pmetric.Metrics) labels.Labels {
	m := FirstMetric(md)
	b := labels.NewScratchBuilder(0)
	b.Add(labels.MetricName, m.Name())
	for k, v := range m.Gauge().DataPoints().At(0).Attributes().All() {
		b.Add(k, v.AsString())
	}
	b.Sort()
	return b.Labels()
}

// SeriesSample returns a series item's sample.
func SeriesSample(md pmetric.Metrics) (time.Time, float64) {
	dp := FirstMetric(md).Gauge().DataPoints().At(0)
	return dp.Timestamp().AsTime(), dp.DoubleValue()
}
