package unit

import (
	"math"
	"sort"
	"time"

	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

// Series items are Prometheus-shaped pmetric: an empty resource and scope, one
// gauge named after the series, and one data point per float sample, each
// with all other labels as attributes. This is what the backend stores and
// what rules read.
//
// Native histogram samples go in an exponential histogram metric of the same
// name, one point per sample, with the same attributes. The points hold
// Prometheus's histogram, not OTLP's: the scale is the schema, bucket offsets
// are Prometheus bucket indexes, every count (total, zero, buckets) is a
// float64's bits since PromQL's counts are floats, the flags hold the counter
// reset hint, and the exemplars' values are the custom bucket bounds of a
// custom-buckets schema. Only this package reads them.
//
// A series without samples keeps its labels in the gauge's metadata.

// Sample is one value of a series: a float, or a native histogram when H is
// set.
type Sample struct {
	T time.Time
	V float64
	H *histogram.FloatHistogram
}

// NewSeries builds a series item from its labels (including __name__) and
// samples, which are stored in time order.
func NewSeries(lset labels.Labels, samples ...Sample) pmetric.Metrics {
	md := pmetric.NewMetrics()
	sm := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty()
	m := sm.Metrics().AppendEmpty()
	m.SetName(lset.Get(labels.MetricName))
	dps := m.SetEmptyGauge().DataPoints()
	sorted := append([]Sample(nil), samples...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].T.Before(sorted[j].T) })
	attrs := func(a pcommon.Map) {
		lset.Range(func(l labels.Label) {
			if l.Name != labels.MetricName {
				a.PutStr(l.Name, l.Value)
			}
		})
	}
	var hps pmetric.ExponentialHistogramDataPointSlice
	haveHistograms := false
	for _, s := range sorted {
		if s.H != nil {
			if !haveHistograms {
				hm := sm.Metrics().AppendEmpty()
				hm.SetName(m.Name())
				hps, haveHistograms = hm.SetEmptyExponentialHistogram().DataPoints(), true
			}
			dp := hps.AppendEmpty()
			dp.SetTimestamp(pcommon.NewTimestampFromTime(s.T))
			putHistogram(dp, s.H)
			attrs(dp.Attributes())
			continue
		}
		dp := dps.AppendEmpty()
		dp.SetTimestamp(pcommon.NewTimestampFromTime(s.T))
		dp.SetDoubleValue(s.V)
		attrs(dp.Attributes())
	}
	if len(sorted) == 0 {
		attrs(m.Metadata())
	}
	return md
}

// SeriesLabels returns a series item's labels, including __name__.
func SeriesLabels(md pmetric.Metrics) labels.Labels {
	m := FirstMetric(md)
	b := labels.NewScratchBuilder(0)
	b.Add(labels.MetricName, m.Name())
	from := m.Metadata()
	if m.Gauge().DataPoints().Len() > 0 {
		from = m.Gauge().DataPoints().At(0).Attributes()
	} else if h, ok := histograms(md); ok && h.Len() > 0 {
		from = h.At(0).Attributes()
	}
	for k, v := range from.All() {
		b.Add(k, v.AsString())
	}
	b.Sort()
	return b.Labels()
}

// histograms returns a series item's native histogram points.
func histograms(md pmetric.Metrics) (pmetric.ExponentialHistogramDataPointSlice, bool) {
	ms := md.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics()
	for i := 0; i < ms.Len(); i++ {
		if ms.At(i).Type() == pmetric.MetricTypeExponentialHistogram {
			return ms.At(i).ExponentialHistogram().DataPoints(), true
		}
	}
	return pmetric.ExponentialHistogramDataPointSlice{}, false
}

// SeriesSamples returns a series item's samples in time order.
func SeriesSamples(md pmetric.Metrics) []Sample {
	dps := FirstMetric(md).Gauge().DataPoints()
	out := make([]Sample, dps.Len())
	for i := range out {
		out[i] = Sample{T: dps.At(i).Timestamp().AsTime(), V: dps.At(i).DoubleValue()}
	}
	if hps, ok := histograms(md); ok {
		for i := 0; i < hps.Len(); i++ {
			out = append(out, Sample{T: hps.At(i).Timestamp().AsTime(), H: getHistogram(hps.At(i))})
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].T.Before(out[j].T) })
	}
	return out
}

// LastValue returns the value of a series item's latest sample: its float, or
// a native histogram's count.
func LastValue(md pmetric.Metrics) float64 {
	s := SeriesSamples(md)
	if len(s) == 0 {
		return 0
	}
	if h := s[len(s)-1].H; h != nil {
		return h.Count
	}
	return s[len(s)-1].V
}

func putHistogram(dp pmetric.ExponentialHistogramDataPoint, h *histogram.FloatHistogram) {
	dp.SetScale(h.Schema)
	dp.SetZeroThreshold(h.ZeroThreshold)
	dp.SetZeroCount(math.Float64bits(h.ZeroCount))
	dp.SetCount(math.Float64bits(h.Count))
	dp.SetSum(h.Sum)
	dp.SetFlags(pmetric.DataPointFlags(uint32(h.CounterResetHint) << 1))
	putBuckets(dp.Positive(), h.PositiveSpans, h.PositiveBuckets)
	putBuckets(dp.Negative(), h.NegativeSpans, h.NegativeBuckets)
	for _, v := range h.CustomValues {
		dp.Exemplars().AppendEmpty().SetDoubleValue(v)
	}
}

// putBuckets stores sparse buckets densely from the first bucket's index.
func putBuckets(b pmetric.ExponentialHistogramDataPointBuckets, spans []histogram.Span, counts []float64) {
	var dense []uint64
	idx, first, n := int32(0), int32(0), 0
	for i, sp := range spans {
		idx += sp.Offset
		if i == 0 {
			first = idx
		} else {
			for range sp.Offset {
				dense = append(dense, math.Float64bits(0))
			}
		}
		for range sp.Length {
			dense = append(dense, math.Float64bits(counts[n]))
			n++
		}
		idx += int32(sp.Length)
	}
	b.SetOffset(first)
	b.BucketCounts().FromRaw(dense)
}

func getHistogram(dp pmetric.ExponentialHistogramDataPoint) *histogram.FloatHistogram {
	h := &histogram.FloatHistogram{
		CounterResetHint: histogram.CounterResetHint(uint32(dp.Flags()) >> 1),
		Schema:           dp.Scale(),
		ZeroThreshold:    dp.ZeroThreshold(),
		ZeroCount:        math.Float64frombits(dp.ZeroCount()),
		Count:            math.Float64frombits(dp.Count()),
		Sum:              dp.Sum(),
	}
	h.PositiveSpans, h.PositiveBuckets = getBuckets(dp.Positive())
	h.NegativeSpans, h.NegativeBuckets = getBuckets(dp.Negative())
	for i := 0; i < dp.Exemplars().Len(); i++ {
		h.CustomValues = append(h.CustomValues, dp.Exemplars().At(i).DoubleValue())
	}
	return h.Compact(0)
}

func getBuckets(b pmetric.ExponentialHistogramDataPointBuckets) ([]histogram.Span, []float64) {
	raw := b.BucketCounts().AsRaw()
	if len(raw) == 0 {
		return nil, nil
	}
	counts := make([]float64, len(raw))
	for i, c := range raw {
		counts[i] = math.Float64frombits(c)
	}
	return []histogram.Span{{Offset: b.Offset(), Length: uint32(len(raw))}}, counts
}
