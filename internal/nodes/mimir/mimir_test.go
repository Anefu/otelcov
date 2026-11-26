package mimir

import (
	"context"
	"os"
	"path/filepath"
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
	n := New("mimir", Limits{PromoteOTelResourceAttributes: "k8s.pod.name", OTelMetricSuffixesEnabled: true})
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

func TestSamplesOfOneSeriesAreKept(t *testing.T) {
	n := New("mimir", Limits{})
	a := item("s1", pmetric.AggregationTemporalityCumulative, "requests")
	b := item("s2", pmetric.AggregationTemporalityCumulative, "requests")
	// Same series, a minute later.
	dp := unit.FirstMetric(b.Data).Sum().DataPoints().At(0)
	dp.SetTimestamp(dp.Timestamp() + 60_000_000_000)
	dp.SetDoubleValue(9)
	out, err := n.ProcessMetrics(context.Background(), []unit.Metric{a, b})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range out.Items {
		if unit.SeriesLabels(it.Data).Get("__name__") == "target_info" {
			continue
		}
		if s := unit.SeriesSamples(it.Data); len(s) != 2 || s[1].V != 9 || len(it.From) != 2 {
			t.Errorf("want one series with both samples and both parents, got %v from %v", s, it.From)
		}
	}
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mimir.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadFullConfig(t *testing.T) {
	// A real Mimir config: only limits matter for translation.
	l, err := Load(writeConfig(t, `
multitenancy_enabled: false
server: {http_listen_port: 8080}
blocks_storage: {backend: filesystem}
limits:
  promote_otel_resource_attributes: "k8s.pod.name,team"
  otel_metric_suffixes_enabled: true
  otel_label_name_underscore_sanitization: false
`))
	if err != nil {
		t.Fatal(err)
	}
	if l.PromoteOTelResourceAttributes != "k8s.pod.name,team" || !l.OTelMetricSuffixesEnabled || l.RuntimeConfig != "" ||
		l.OTelLabelNameUnderscoreSanitization == nil || *l.OTelLabelNameUnderscoreSanitization || l.OTelLabelNamePreserveMultipleUnderscores != nil {
		t.Errorf("limits: %+v", l)
	}

	// A limit otelcov doesn't model might change translation: refuse.
	if _, err := Load(writeConfig(t, "limits:\n  otel_translation_strategy: NoTranslation\n")); err == nil || !strings.Contains(err.Error(), "otel_translation_strategy") {
		t.Errorf("want an error naming the unmodeled limit, got %v", err)
	}
}

func TestRuntimeConfigIsUnverified(t *testing.T) {
	l, err := Load(writeConfig(t, "runtime_config: {file: /etc/mimir/runtime.yaml}\nlimits: {otel_metric_suffixes_enabled: true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := New("mimir", l).ProcessMetrics(context.Background(), []unit.Metric{item("s1", pmetric.AggregationTemporalityCumulative, "m")})
	if err != nil {
		t.Fatal(err)
	}
	var unverified bool
	for _, e := range out.Events {
		unverified = unverified || (e.Signal == "s1" && e.Action == lineage.Unverified && strings.Contains(e.Reason, "/etc/mimir/runtime.yaml"))
	}
	if !unverified {
		t.Errorf("want s1 unverified because of the runtime config: %+v", out.Events)
	}
}

// Label names keep Mimir's defaults: "key" before a single leading
// underscore, and consecutive underscores kept. Both are limits.
func TestLabelNameSanitization(t *testing.T) {
	no := false
	for name, tc := range map[string]struct {
		limits Limits
		want   []string
	}{
		"defaults": {Limits{PromoteOTelResourceAttributes: "_team,cost..center"}, []string{"key_team", "cost__center"}},
		"off": {Limits{PromoteOTelResourceAttributes: "_team,cost..center",
			OTelLabelNameUnderscoreSanitization: &no, OTelLabelNamePreserveMultipleUnderscores: &no}, []string{"_team", "cost_center"}},
	} {
		it := item("s1", pmetric.AggregationTemporalityCumulative, "m")
		res := unit.Resource(it.Data).Attributes()
		res.PutStr("_team", "a")
		res.PutStr("cost..center", "b")
		out, err := New("mimir", tc.limits).ProcessMetrics(context.Background(), []unit.Metric{it})
		if err != nil {
			t.Fatal(err)
		}
		got := unit.SeriesLabels(out.Items[0].Data)
		if got.Get(tc.want[0]) != "a" || got.Get(tc.want[1]) != "b" {
			t.Errorf("%s: %s, want %v", name, got, tc.want)
		}
	}
	if LabelName("_team") != "key_team" || LabelName("cost..center") != "cost__center" {
		t.Errorf("LabelName: %q %q", LabelName("_team"), LabelName("cost..center"))
	}
}

// Exponential histograms are stored as native histograms (decision 73).
func TestExponentialHistograms(t *testing.T) {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr("service.name", "api")
	m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName("rpc.duration")
	m.SetUnit("ms")
	eh := m.SetEmptyExponentialHistogram()
	eh.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
	dp := eh.DataPoints().AppendEmpty()
	dp.SetTimestamp(1_700_000_000_000_000_000)
	dp.SetScale(1)
	dp.Positive().SetOffset(2)
	dp.Positive().BucketCounts().FromRaw([]uint64{1, 3})
	dp.SetCount(4)
	dp.SetSum(10)
	out, err := New("mimir", Limits{OTelMetricSuffixesEnabled: true}).ProcessMetrics(context.Background(), []unit.Metric{{ID: "s1", Data: md}})
	if err != nil {
		t.Fatal(err)
	}
	it := out.Items[0]
	lset := unit.SeriesLabels(it.Data)
	samples := unit.SeriesSamples(it.Data)
	if lset.Get("__name__") != "rpc_duration_milliseconds" || lset.Get("job") != "api" || len(samples) != 1 || samples[0].H == nil ||
		samples[0].H.Count != 4 || samples[0].H.Sum != 10 {
		t.Fatalf("got %s %+v", lset, samples)
	}
	if v, ok := out.Signals[0].Value.(string); !ok || !strings.Contains(v, "count:4") {
		t.Errorf("lineage value %v", out.Signals[0].Value)
	}
}

func TestFidelity(t *testing.T) {
	for v, tested := range map[string]bool{"3.0.0": true, "3.1.11": true, "v3.2.3": true, "3.2": true, "2.17.1": false, "3.3.0": false, "": false, "latest": false} {
		st := Fidelity(v)
		if st.Fidelity != lineage.Reimplementation || st.Conformance != tested || !strings.HasPrefix(st.Note, note) {
			t.Errorf("%q: %+v", v, st)
		}
	}
	if st := Fidelity("3.3.0"); !strings.Contains(st.Note, "tested for Mimir 3.0–3.2, not 3.3.0") {
		t.Errorf("note: %s", st.Note)
	}
}

// CI runs the conformance suite at both ends of the tested range.
func TestWorkflowTestsTheRange(t *testing.T) {
	b, err := os.ReadFile("../../../.github/workflows/mimir-conformance.yml")
	if err != nil {
		t.Fatal(err)
	}
	var versions []string
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "- mimir: "); ok {
			versions = append(versions, v)
		}
	}
	if len(versions) != 2 || !strings.HasPrefix(versions[0], TestedMin+".") || !strings.HasPrefix(versions[1], TestedMax+".") {
		t.Errorf("the workflow runs Mimir %v; the model claims %s–%s", versions, TestedMin, TestedMax)
	}
}
