package mimir

import (
	"math"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

// The conformance suite's generated cases (decision 72): inputs written to
// probe each rule of Mimir's OTLP translation, each sent under several sets of
// limits. They are otelcov's own; Mimir's tests are AGPL and must not be
// copied or ported (see AGENTS.md, clean room).

// limitSet is one set of per-tenant limits, as Mimir's runtime config and
// otelcov's Load read them.
type limitSet struct {
	name   string
	limits map[string]any
	// needs lists the Mimir flags these limits need; a Mimir without them
	// skips the set.
	needs []string
}

var limitSets = []limitSet{
	{name: "defaults"},
	{name: "suffixes", limits: map[string]any{"otel_metric_suffixes_enabled": true}},
	{name: "promote", limits: map[string]any{
		"otel_metric_suffixes_enabled": true,
		// A missing attribute, an identifying one, odd names.
		"promote_otel_resource_attributes": "k8s.namespace.name,k8s.pod.name,k8s.container.name,team,service.name,_private,a..b,host.name",
	}},
	{name: "keep-identifying", limits: map[string]any{
		"otel_keep_identifying_resource_attributes": true,
		"promote_otel_resource_attributes":          "k8s.pod.name",
	}},
	{name: "no-underscore-sanitization", limits: map[string]any{
		"otel_label_name_underscore_sanitization": false,
		"promote_otel_resource_attributes":        "_private,__double",
	}, needs: []string{"-distributor.otel-label-name-underscore-sanitization"}},
	{name: "no-multiple-underscores", limits: map[string]any{
		"otel_label_name_preserve_multiple_underscores": false,
		"promote_otel_resource_attributes":              "a__b,__double",
	}, needs: []string{"-distributor.otel-label-name-preserve-underscores"}},
}

// input is one generated input: the OTLP requests sent, in order.
type input struct {
	name     string
	requests func(base time.Time) []pmetric.Metrics
}

// step is the interval between a case's samples. All samples of a case lie
// within a few minutes before base, which is a little before now, since Mimir
// refuses samples from the future.
const step = 15 * time.Second

func at(base time.Time, i int) pcommon.Timestamp {
	return pcommon.NewTimestampFromTime(base.Add(time.Duration(i-8) * step))
}

// start is the start time of cumulative points: an hour before base.
func start(base time.Time) pcommon.Timestamp {
	return pcommon.NewTimestampFromTime(base.Add(-time.Hour))
}

// resource adds a resource with the usual attributes of a pod's telemetry.
func resource(md pmetric.Metrics, attrs map[string]string) pmetric.ScopeMetrics {
	rm := md.ResourceMetrics().AppendEmpty()
	for k, v := range attrs {
		rm.Resource().Attributes().PutStr(k, v)
	}
	sm := rm.ScopeMetrics().AppendEmpty()
	sm.Scope().SetName("conformance")
	sm.Scope().SetVersion("1.0.0")
	sm.Scope().Attributes().PutStr("scope.attr", "x")
	return sm
}

var pod = map[string]string{
	"service.name":        "api",
	"service.namespace":   "shop",
	"service.instance.id": "api-7d9f-x2",
	"k8s.namespace.name":  "shop",
	"k8s.pod.name":        "api-7d9f-x2",
	"k8s.container.name":  "api",
	"host.name":           "node-1",
	"telemetry.sdk.name":  "opentelemetry",
}

func metric(sm pmetric.ScopeMetrics, name, unit, desc string) pmetric.Metric {
	m := sm.Metrics().AppendEmpty()
	m.SetName(name)
	m.SetUnit(unit)
	m.SetDescription(desc)
	return m
}

func gauge(sm pmetric.ScopeMetrics, name, unit string, base time.Time, values ...float64) pmetric.NumberDataPointSlice {
	dps := metric(sm, name, unit, "a gauge").SetEmptyGauge().DataPoints()
	for i, v := range values {
		dp := dps.AppendEmpty()
		dp.SetTimestamp(at(base, i))
		dp.SetDoubleValue(v)
	}
	return dps
}

func sum(sm pmetric.ScopeMetrics, name, unit string, monotonic bool, temporality pmetric.AggregationTemporality, base time.Time, values ...float64) pmetric.NumberDataPointSlice {
	s := metric(sm, name, unit, "a sum").SetEmptySum()
	s.SetIsMonotonic(monotonic)
	s.SetAggregationTemporality(temporality)
	for i, v := range values {
		dp := s.DataPoints().AppendEmpty()
		dp.SetStartTimestamp(start(base))
		dp.SetTimestamp(at(base, i))
		dp.SetDoubleValue(v)
	}
	return s.DataPoints()
}

func explicitHistogram(sm pmetric.ScopeMetrics, name, unit string, temporality pmetric.AggregationTemporality, base time.Time, withSum bool) {
	h := metric(sm, name, unit, "a histogram").SetEmptyHistogram()
	h.SetAggregationTemporality(temporality)
	for i := 0; i < 2; i++ {
		dp := h.DataPoints().AppendEmpty()
		dp.SetStartTimestamp(start(base))
		dp.SetTimestamp(at(base, i))
		dp.ExplicitBounds().FromRaw([]float64{0.1, 0.5, 1, 5})
		dp.BucketCounts().FromRaw([]uint64{uint64(1 + i), 2, 3, 0, uint64(2 * i)})
		dp.SetCount(uint64(6 + 3*i))
		if withSum {
			dp.SetSum(4.25 + float64(i))
		}
		dp.SetMin(0.01)
		dp.SetMax(9)
		dp.Attributes().PutStr("http.route", "/cart")
	}
}

func exponential(sm pmetric.ScopeMetrics, name, unit string, base time.Time) {
	h := metric(sm, name, unit, "an exponential histogram").SetEmptyExponentialHistogram()
	h.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
	for i := 0; i < 2; i++ {
		dp := h.DataPoints().AppendEmpty()
		dp.SetStartTimestamp(start(base))
		dp.SetTimestamp(at(base, i))
		dp.SetScale(2)
		dp.SetZeroCount(1)
		dp.Positive().SetOffset(3)
		dp.Positive().BucketCounts().FromRaw([]uint64{1, 2, uint64(3 + i)})
		dp.SetCount(uint64(7 + i))
		dp.SetSum(12.5 + float64(i))
	}
}

func summary(sm pmetric.ScopeMetrics, name, unit string, base time.Time) {
	s := metric(sm, name, unit, "a summary").SetEmptySummary()
	for i := 0; i < 2; i++ {
		dp := s.DataPoints().AppendEmpty()
		dp.SetStartTimestamp(start(base))
		dp.SetTimestamp(at(base, i))
		dp.SetCount(uint64(10 + i))
		dp.SetSum(3.5 + float64(i))
		for _, q := range []float64{0, 0.5, 0.99, 1} {
			qv := dp.QuantileValues().AppendEmpty()
			qv.SetQuantile(q)
			qv.SetValue(q * 2)
		}
	}
}

var inputs = []input{
	{name: "numbers", requests: func(base time.Time) []pmetric.Metrics {
		md := pmetric.NewMetrics()
		sm := resource(md, pod)
		gauge(sm, "process.memory.usage", "By", base, 1.5e9, 1.6e9, 1.55e9)
		sum(sm, "http.server.requests", "{requests}", true, pmetric.AggregationTemporalityCumulative, base, 10, 25, 40)
		sum(sm, "queue.depth", "1", false, pmetric.AggregationTemporalityCumulative, base, 3, 1, 4)
		gauge(sm, "cpu.utilization", "1", base, 0.25, 0.5)
		sum(sm, "network.io", "By/s", true, pmetric.AggregationTemporalityCumulative, base, 100, 200)
		gauge(sm, "request.latency", "ms", base, 12, 15)
		gauge(sm, "disk.free", "MiBy", base, 512)
		gauge(sm, "temperature", "Cel", base, 41.5)
		gauge(sm, "uptime", "d", base, 3)
		gauge(sm, "jobs.pending", "", base, 7)
		// Int values, one too large for a float64 to hold exactly.
		ints := sum(sm, "bytes.sent", "By", true, pmetric.AggregationTemporalityCumulative, base, 0)
		ints.At(0).SetIntValue(9007199254740993)
		return []pmetric.Metrics{md}
	}},
	{name: "names", requests: func(base time.Time) []pmetric.Metrics {
		md := pmetric.NewMetrics()
		sm := resource(md, pod)
		gauge(sm, "http.server.request.duration", "s", base, 0.2)
		gauge(sm, "my-metric", "", base, 1)
		gauge(sm, "1starts.with.digit", "", base, 1)
		gauge(sm, "already_seconds", "s", base, 1)
		sum(sm, "requests_total", "1", true, pmetric.AggregationTemporalityCumulative, base, 5)
		sum(sm, "errors.total", "", true, pmetric.AggregationTemporalityCumulative, base, 5)
		gauge(sm, "dots..and__underscores", "", base, 1)
		gauge(sm, "ratio_already", "1", base, 0.5)
		gauge(sm, "ünïcode.metric", "", base, 1)
		gauge(sm, "with space", "", base, 1)
		gauge(sm, "per.thing", "1/s", base, 2)
		return []pmetric.Metrics{md}
	}},
	{name: "attributes", requests: func(base time.Time) []pmetric.Metrics {
		md := pmetric.NewMetrics()
		attrs := map[string]string{"_private": "p", "__double": "d", "a..b": "ab", "a__b": "a_b", "1abc": "n", "ünï": "u", "team": "payments"}
		for k, v := range pod {
			attrs[k] = v
		}
		sm := resource(md, attrs)
		dps := gauge(sm, "point.attributes", "", base, 1, 2)
		for i := 0; i < dps.Len(); i++ {
			a := dps.At(i).Attributes()
			a.PutStr("http.request.method", "GET")
			a.PutStr("_leading", "l")
			a.PutStr("__two", "t")
			a.PutStr("x..y", "xy")
			a.PutStr("x__y", "x_y")
			a.PutInt("int.attr", 42)
			a.PutBool("bool.attr", true)
			a.PutDouble("double.attr", 1.5)
			a.PutStr("empty.value", "")
			a.PutEmptySlice("slice.attr").AppendEmpty().SetStr("one")
		}
		// A point attribute that collides with a resource label once
		// translated, and with a reserved label.
		c := gauge(sm, "collisions", "", base, 1)
		c.At(0).Attributes().PutStr("k8s.pod.name", "from-point")
		c.At(0).Attributes().PutStr("job", "from-point")
		c.At(0).Attributes().PutStr("x.y", "dot")
		c.At(0).Attributes().PutStr("x_y", "underscore")
		return []pmetric.Metrics{md}
	}},
	{name: "histograms", requests: func(base time.Time) []pmetric.Metrics {
		md := pmetric.NewMetrics()
		sm := resource(md, pod)
		explicitHistogram(sm, "http.server.duration", "s", pmetric.AggregationTemporalityCumulative, base, true)
		explicitHistogram(sm, "payload.size", "By", pmetric.AggregationTemporalityCumulative, base, false)
		return []pmetric.Metrics{md}
	}},
	{name: "exponential-histograms", requests: func(base time.Time) []pmetric.Metrics {
		md := pmetric.NewMetrics()
		exponential(resource(md, pod), "rpc.duration", "ms", base)
		return []pmetric.Metrics{md}
	}},
	{name: "summaries", requests: func(base time.Time) []pmetric.Metrics {
		md := pmetric.NewMetrics()
		summary(resource(md, pod), "gc.pause", "s", base)
		return []pmetric.Metrics{md}
	}},
	{name: "delta", requests: func(base time.Time) []pmetric.Metrics {
		md := pmetric.NewMetrics()
		sm := resource(md, pod)
		sum(sm, "delta.requests", "{requests}", true, pmetric.AggregationTemporalityDelta, base, 3, 4)
		explicitHistogram(sm, "delta.duration", "s", pmetric.AggregationTemporalityDelta, base, true)
		gauge(sm, "beside.delta", "", base, 1)
		return []pmetric.Metrics{md}
	}},
	{name: "resources", requests: func(base time.Time) []pmetric.Metrics {
		md := pmetric.NewMetrics()
		// No service.name; only an instance; nothing at all; a namespace
		// without a name.
		gauge(resource(md, map[string]string{"k8s.pod.name": "lonely", "host.name": "node-2"}), "no.service", "", base, 1)
		gauge(resource(md, map[string]string{"service.instance.id": "i-1"}), "instance.only", "", base, 1)
		gauge(resource(md, map[string]string{}), "no.resource", "", base, 1)
		gauge(resource(md, map[string]string{"service.namespace": "ns", "k8s.pod.name": "p"}), "namespace.only", "", base, 1)
		gauge(resource(md, map[string]string{"service.name": "only-name"}), "name.only", "", base, 1)
		return []pmetric.Metrics{md}
	}},
	{name: "over-time", requests: func(base time.Time) []pmetric.Metrics {
		// One series over two requests; a repeated timestamp with another
		// value, in the same request and in the next.
		first := pmetric.NewMetrics()
		dps := gauge(resource(first, pod), "load", "", base, 1, 2)
		dup := dps.AppendEmpty()
		dup.SetTimestamp(dps.At(1).Timestamp())
		dup.SetDoubleValue(99)
		second := pmetric.NewMetrics()
		later := gauge(resource(second, pod), "load", "", base, 0, 0, 0, 5)
		later.At(1).SetDoubleValue(77) // same time as the first request's second point
		later.RemoveIf(func(dp pmetric.NumberDataPoint) bool {
			return dp.Timestamp() == at(base, 0) || dp.Timestamp() == at(base, 2)
		})
		return []pmetric.Metrics{first, second}
	}},
	{name: "special-values", requests: func(base time.Time) []pmetric.Metrics {
		md := pmetric.NewMetrics()
		sm := resource(md, pod)
		gauge(sm, "special", "", base, math.NaN(), math.Inf(1), math.Inf(-1), -0.0, 1e-300)
		stale := gauge(sm, "no.recorded.value", "", base, 1, 2)
		stale.At(1).SetFlags(pmetric.DefaultDataPointFlags.WithNoRecordedValue(true))
		empty := metric(sm, "no.points", "", "no data points")
		empty.SetEmptyGauge()
		return []pmetric.Metrics{md}
	}},
}
