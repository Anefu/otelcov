// Package promqlx evaluates PromQL with the real Prometheus engine over
// in-memory series, and explains which input series each output came from.
package promqlx

import (
	"context"
	"sort"
	"time"

	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/util/annotations"
)

// DefaultLookback matches Prometheus and Mimir.
const DefaultLookback = 5 * time.Minute

// Sample is one value of a series.
type Sample struct {
	T time.Time
	V float64
}

// Series is one input series.
type Series struct {
	Labels  labels.Labels
	Samples []Sample
}

// Engine evaluates instant queries over in-memory series.
type Engine struct {
	engine   *promql.Engine
	lookback time.Duration
}

// NewEngine returns an engine with Prometheus defaults.
func NewEngine() *Engine {
	return &Engine{
		lookback: DefaultLookback,
		engine: promql.NewEngine(promql.EngineOpts{
			MaxSamples:               50_000_000,
			Timeout:                  time.Minute,
			LookbackDelta:            DefaultLookback,
			NoStepSubqueryIntervalFn: func(int64) int64 { return time.Minute.Milliseconds() },
			EnableAtModifier:         true,
			EnableNegativeOffset:     true,
		}),
	}
}

// Instant evaluates query at ts over series and returns the result vector.
func (e *Engine) Instant(ctx context.Context, query string, series []Series, ts time.Time) (promql.Vector, error) {
	q, err := e.engine.NewInstantQuery(ctx, newQueryable(series), promql.NewPrometheusQueryOpts(false, e.lookback), query, ts)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	res := q.Exec(ctx)
	if res.Err != nil {
		return nil, res.Err
	}
	switch v := res.Value.(type) {
	case promql.Vector:
		return v, nil
	case promql.Scalar:
		return promql.Vector{{T: v.T, F: v.V, Metric: labels.EmptyLabels()}}, nil
	default:
		return res.Vector()
	}
}

// queryable serves series from memory.
type queryable struct{ series []storage.Series }

// newQueryable stores series in memory. Series with identical labels are
// merged; at equal timestamps the first sample wins.
func newQueryable(in []Series) *queryable {
	q := &queryable{}
	byKey := map[string]int{}
	var lsets []labels.Labels
	var samples [][]fSample
	for _, s := range in {
		k := s.Labels.String()
		i, ok := byKey[k]
		if !ok {
			i = len(lsets)
			byKey[k] = i
			lsets = append(lsets, s.Labels)
			samples = append(samples, nil)
		}
	next:
		for _, smp := range s.Samples {
			t := smp.T.UnixMilli()
			for _, have := range samples[i] {
				if have.t == t {
					continue next
				}
			}
			samples[i] = append(samples[i], fSample{t: t, f: smp.V})
		}
	}
	for i, ls := range lsets {
		sort.Slice(samples[i], func(a, b int) bool { return samples[i][a].t < samples[i][b].t })
		cs := make([]chunks.Sample, len(samples[i]))
		for j, smp := range samples[i] {
			cs[j] = smp
		}
		q.series = append(q.series, storage.NewListSeries(ls, cs))
	}
	sort.Slice(q.series, func(i, j int) bool {
		return labels.Compare(q.series[i].Labels(), q.series[j].Labels()) < 0
	})
	return q
}

func (q *queryable) Querier(int64, int64) (storage.Querier, error) { return q, nil }

func (q *queryable) Select(_ context.Context, _ bool, _ *storage.SelectHints, ms ...*labels.Matcher) storage.SeriesSet {
	var out []storage.Series
	for _, s := range q.series {
		if matchesAll(s.Labels(), ms) {
			out = append(out, s)
		}
	}
	return &seriesSet{series: out, i: -1}
}

func (q *queryable) LabelValues(_ context.Context, name string, _ *storage.LabelHints, ms ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	seen := map[string]bool{}
	var out []string
	for _, s := range q.series {
		if v := s.Labels().Get(name); v != "" && matchesAll(s.Labels(), ms) && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out, nil, nil
}

func (q *queryable) LabelNames(_ context.Context, _ *storage.LabelHints, ms ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	seen := map[string]bool{}
	var out []string
	for _, s := range q.series {
		if !matchesAll(s.Labels(), ms) {
			continue
		}
		s.Labels().Range(func(l labels.Label) {
			if !seen[l.Name] {
				seen[l.Name] = true
				out = append(out, l.Name)
			}
		})
	}
	sort.Strings(out)
	return out, nil, nil
}

func (q *queryable) Close() error { return nil }

func matchesAll(ls labels.Labels, ms []*labels.Matcher) bool {
	for _, m := range ms {
		if !m.Matches(ls.Get(m.Name)) {
			return false
		}
	}
	return true
}

type seriesSet struct {
	series []storage.Series
	i      int
}

func (s *seriesSet) Next() bool                        { s.i++; return s.i < len(s.series) }
func (s *seriesSet) At() storage.Series                { return s.series[s.i] }
func (s *seriesSet) Err() error                        { return nil }
func (s *seriesSet) Warnings() annotations.Annotations { return nil }

// fSample is a float sample.
type fSample struct {
	t int64
	f float64
}

func (s fSample) T() int64                      { return s.t }
func (s fSample) F() float64                    { return s.f }
func (s fSample) H() *histogram.Histogram       { return nil }
func (s fSample) FH() *histogram.FloatHistogram { return nil }
func (s fSample) Type() chunkenc.ValueType      { return chunkenc.ValFloat }
func (s fSample) Copy() chunks.Sample           { return s }
