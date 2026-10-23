package unit

import (
	"sort"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

// Series items are Prometheus-shaped pmetric: an empty resource and scope, one
// gauge named after the series, and one data point per sample, each with all
// other labels as attributes. This is what the backend stores and what rules
// read.

// Sample is one value of a series.
type Sample struct {
	T time.Time
	V float64
}

// NewSeries builds a series item from its labels (including __name__) and
// samples, which are stored in time order.
func NewSeries(lset labels.Labels, samples ...Sample) pmetric.Metrics {
	md := pmetric.NewMetrics()
	m := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName(lset.Get(labels.MetricName))
	dps := m.SetEmptyGauge().DataPoints()
	sorted := append([]Sample(nil), samples...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].T.Before(sorted[j].T) })
	for _, s := range sorted {
		dp := dps.AppendEmpty()
		dp.SetTimestamp(pcommon.NewTimestampFromTime(s.T))
		dp.SetDoubleValue(s.V)
		lset.Range(func(l labels.Label) {
			if l.Name != labels.MetricName {
				dp.Attributes().PutStr(l.Name, l.Value)
			}
		})
	}
	return md
}

// SeriesLabels returns a series item's labels, including __name__.
func SeriesLabels(md pmetric.Metrics) labels.Labels {
	m := FirstMetric(md)
	b := labels.NewScratchBuilder(0)
	b.Add(labels.MetricName, m.Name())
	if m.Gauge().DataPoints().Len() > 0 {
		for k, v := range m.Gauge().DataPoints().At(0).Attributes().All() {
			b.Add(k, v.AsString())
		}
	}
	b.Sort()
	return b.Labels()
}

// SeriesSamples returns a series item's samples in time order.
func SeriesSamples(md pmetric.Metrics) []Sample {
	dps := FirstMetric(md).Gauge().DataPoints()
	out := make([]Sample, dps.Len())
	for i := range out {
		out[i] = Sample{T: dps.At(i).Timestamp().AsTime(), V: dps.At(i).DoubleValue()}
	}
	return out
}

// LastValue returns the value of a series item's latest sample.
func LastValue(md pmetric.Metrics) float64 {
	s := SeriesSamples(md)
	if len(s) == 0 {
		return 0
	}
	return s[len(s)-1].V
}
