package unit

import (
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
)

func TestSeriesKeepsNativeHistograms(t *testing.T) {
	t0 := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	lset := labels.FromStrings("__name__", "rpc_duration", "job", "shop/api")
	h := &histogram.FloatHistogram{
		CounterResetHint: histogram.GaugeType,
		Schema:           2,
		ZeroThreshold:    1e-128,
		ZeroCount:        1,
		Count:            9.5,
		Sum:              12.25,
		// A gap between spans, and fractional counts, as rates have.
		PositiveSpans:   []histogram.Span{{Offset: 3, Length: 2}, {Offset: 2, Length: 1}},
		PositiveBuckets: []float64{1, 2.5, 3},
		NegativeSpans:   []histogram.Span{{Offset: -1, Length: 1}},
		NegativeBuckets: []float64{2},
	}
	custom := &histogram.FloatHistogram{
		Schema: histogram.CustomBucketsSchema, Count: 3, Sum: 4,
		PositiveSpans: []histogram.Span{{Offset: 0, Length: 2}}, PositiveBuckets: []float64{1, 2},
		CustomValues: []float64{0.5, 1},
	}
	md := NewSeries(lset,
		Sample{T: t0.Add(time.Minute), H: custom},
		Sample{T: t0, V: 7},
		Sample{T: t0.Add(30 * time.Second), H: h},
	)
	if got := SeriesLabels(md); !labels.Equal(got, lset) {
		t.Errorf("labels: %s", got)
	}
	got := SeriesSamples(md)
	if len(got) != 3 || got[0].H != nil || got[0].V != 7 || !got[1].T.Equal(t0.Add(30*time.Second)) {
		t.Fatalf("samples: %+v", got)
	}
	if !got[1].H.Equals(h.Copy().Compact(0)) || got[1].H.CounterResetHint != histogram.GaugeType {
		t.Errorf("histogram: got %s, want %s", got[1].H, h)
	}
	if !got[2].H.Equals(custom) {
		t.Errorf("custom buckets: got %s, want %s", got[2].H, custom)
	}
	if v := LastValue(md); v != 3 {
		t.Errorf("last value %v, want the latest histogram's count", v)
	}
}

func TestSeriesWithoutSamplesKeepsLabels(t *testing.T) {
	lset := labels.FromStrings("__name__", "m", "job", "api")
	if got := SeriesLabels(NewSeries(lset)); !labels.Equal(got, lset) {
		t.Errorf("labels: %s", got)
	}
}
