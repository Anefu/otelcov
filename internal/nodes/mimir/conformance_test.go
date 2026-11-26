package mimir

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/value"
	"github.com/prometheus/prometheus/promql"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"sigs.k8s.io/yaml"

	"github.com/Anefu/otelcov/internal/collector"
	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/unit"
)

// EnvMimir names the environment variable with the path of a Mimir binary
// for TestConformance.
const EnvMimir = "OTELCOV_MIMIR"

// TestConformance is the Mimir model's differential suite (decisions 71 to
// 73). It starts the real Mimir at $OTELCOV_MIMIR, sends every generated
// input under every set of limits (one tenant each) to both Mimir and the
// model, and requires the stored series, labels and samples, to agree. CI
// runs it at the ends of the tested range (TestedMin, TestedMax).
func TestConformance(t *testing.T) {
	bin := os.Getenv(EnvMimir)
	if bin == "" {
		t.Skip("set " + EnvMimir + " to a Mimir binary to compare the model with it")
	}
	m := startMimir(t, bin)
	t.Logf("Mimir %s", m.version)

	// Samples lie in the minutes before base, and base is before now.
	base := time.Now().Add(-time.Minute).Truncate(time.Second)
	type run struct {
		tenant string
		in     input
		set    limitSet
		push   []string
	}
	var runs []*run
	for _, set := range limitSets {
		if missing := m.missing(set.needs); missing != "" {
			t.Logf("limits %s: skipped, Mimir %s has no %s", set.name, m.version, missing)
			continue
		}
		for _, in := range inputs {
			runs = append(runs, &run{tenant: in.name + "--" + set.name, in: in, set: set})
		}
	}
	overrides := map[string]any{}
	for _, r := range runs {
		overrides[r.tenant] = map[string]any{}
		if r.set.limits != nil {
			overrides[r.tenant] = r.set.limits
		}
	}
	m.start(t, overrides)

	for _, r := range runs {
		for _, md := range r.in.requests(base) {
			r.push = append(r.push, m.push(t, r.tenant, md))
		}
	}
	for _, r := range runs {
		t.Run(r.in.name+"/"+r.set.name, func(t *testing.T) {
			got := m.stored(t, r.tenant, base)
			want := model(t, r.set.limits, r.in.requests(base))
			if len(got) == 0 {
				t.Fatalf("Mimir stored nothing; push responses: %q", r.push)
			}
			if diff := compare(want, got); diff != "" {
				t.Errorf("the model and Mimir %s disagree (model -, Mimir +):\n%s\npush responses: %q", m.version, diff, r.push)
				return
			}
			samples := 0
			for _, s := range got {
				samples += len(s)
			}
			t.Logf("%d series and %d samples agree", len(got), samples)
		})
	}
}

// series is a stored series' samples by label set (Prometheus's text form),
// as Mimir's query API shows them (decisions 72, 73):
//
//   - a sample is "<unix ms> <value>", the value formatted like Prometheus's
//     API does, a native histogram's as its count, sum and buckets;
//   - the API never returns stale markers, so a series whose latest sample is
//     one ends with "ended" instead (Mimir's instant query doesn't return it);
//   - target_info keeps only its latest sample: Mimir writes its samples per
//     OTLP request, from the resource's first to its last point, and the
//     model, which can't know how points were batched into requests, writes
//     one per point. Queries see the latest one either way.
type series map[string][]string

func sample(t time.Time, v float64) string {
	return strconv.FormatInt(t.UnixMilli(), 10) + " " + strconv.FormatFloat(v, 'f', -1, 64)
}

func histogramSample(t time.Time, h *histogram.FloatHistogram) string {
	b, err := promql.HPoint{T: t.UnixMilli(), H: h}.MarshalJSON()
	if err != nil {
		panic(err)
	}
	var p [2]any
	if err := json.Unmarshal(b, &p); err != nil {
		panic(err)
	}
	return strconv.FormatInt(t.UnixMilli(), 10) + " " + histogramText(p[1])
}

// histogramText formats a histogram as Prometheus's API encodes it, with
// numbers reformatted so both sides read the same.
func histogramText(v any) string {
	obj, _ := v.(map[string]any)
	num := func(x any) string {
		switch x := x.(type) {
		case string:
			f, err := strconv.ParseFloat(x, 64)
			if err != nil {
				return x
			}
			return strconv.FormatFloat(f, 'f', -1, 64)
		case float64:
			return strconv.FormatFloat(x, 'f', -1, 64)
		}
		return fmt.Sprint(x)
	}
	out := "histogram count=" + num(obj["count"]) + " sum=" + num(obj["sum"])
	buckets, _ := obj["buckets"].([]any)
	for _, b := range buckets {
		f, _ := b.([]any)
		parts := make([]string, len(f))
		for i, x := range f {
			parts[i] = num(x)
		}
		out += " [" + strings.Join(parts, " ") + "]"
	}
	return out
}

// finish applies the comparison's rules for target_info.
func finish(s series) series {
	for k, v := range s {
		if strings.HasPrefix(k, `{__name__="target_info"`) && len(v) > 1 {
			s[k] = v[len(v)-1:]
		}
	}
	return s
}

// model runs the Mimir model over the requests, split into units as a
// replay splits recordings.
func model(t *testing.T, raw map[string]any, requests []pmetric.Metrics) series {
	var l Limits
	if err := collector.Decode(raw, &l); err != nil {
		t.Fatal(err)
	}
	var items []unit.Metric
	for _, md := range requests {
		for _, u := range unit.SplitMetrics(md) {
			items = append(items, unit.Metric{ID: lineage.SignalID("s" + strconv.Itoa(len(items)+1)), Data: u})
		}
	}
	out, err := New("conformance", l).ProcessMetrics(context.Background(), items)
	if err != nil {
		t.Fatal(err)
	}
	got := series{}
	for _, it := range out.Items {
		var s []string
		stale := false
		for _, smp := range unit.SeriesSamples(it.Data) {
			switch stale = smp.H == nil && value.IsStaleNaN(smp.V) || smp.H != nil && value.IsStaleNaN(smp.H.Sum); {
			case stale:
			case smp.H != nil:
				s = append(s, histogramSample(smp.T, smp.H))
			default:
				s = append(s, sample(smp.T, smp.V))
			}
		}
		if len(s) == 0 {
			continue // only stale markers: invisible to queries
		}
		if stale {
			s = append(s, "ended")
		}
		got[unit.SeriesLabels(it.Data).String()] = s
	}
	return finish(got)
}

// compare lists the differences, one line per series.
func compare(want, got series) string {
	keys := map[string]bool{}
	for k := range want {
		keys[k] = true
	}
	for k := range got {
		keys[k] = true
	}
	var sorted []string
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	var b strings.Builder
	for _, k := range sorted {
		w, inModel := want[k]
		g, inMimir := got[k]
		switch {
		case !inMimir:
			fmt.Fprintf(&b, "- %s %v\n", k, w)
		case !inModel:
			fmt.Fprintf(&b, "+ %s %v\n", k, g)
		case strings.Join(w, ",") != strings.Join(g, ","):
			fmt.Fprintf(&b, "- %s %v\n+ %s %v\n", k, w, k, g)
		}
	}
	return b.String()
}

// mimirProc is a Mimir process with one tenant per run.
type mimirProc struct {
	bin, dir, version, help string
	http                    string
	cmd                     *exec.Cmd
	log                     *bytes.Buffer
}

func startMimir(t *testing.T, bin string) *mimirProc {
	t.Helper()
	m := &mimirProc{bin: bin, dir: t.TempDir()}
	out, err := exec.Command(bin, "-version").CombinedOutput()
	if err != nil {
		t.Fatalf("%s -version: %v: %s", bin, err, out)
	}
	v := regexp.MustCompile(`version (\S+)`).FindSubmatch(out)
	if v == nil {
		t.Fatalf("%s -version: no version in %q", bin, out)
	}
	m.version = string(v[1])
	help, _ := exec.Command(bin, "-help-all").CombinedOutput()
	m.help = string(help)
	return m
}

// missing returns the first flag this Mimir doesn't have.
func (m *mimirProc) missing(flags []string) string {
	for _, f := range flags {
		if !strings.Contains(m.help, f+" ") && !strings.Contains(m.help, f+"\n") {
			return f
		}
	}
	return ""
}

// freePort returns a TCP port nothing listens on just now.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// start runs Mimir as a single process with multitenancy on and each
// tenant's limits in the runtime config, and waits until it's ready.
func (m *mimirProc) start(t *testing.T, overrides map[string]any) {
	t.Helper()
	httpPort, grpcPort, memberlistPort := freePort(t), freePort(t), freePort(t)
	data := func(p string) string { return filepath.Join(m.dir, p) }
	inmemory := map[string]any{"store": "inmemory"}
	cfg := map[string]any{
		"multitenancy_enabled": true,
		"server":               map[string]any{"http_listen_address": "127.0.0.1", "http_listen_port": httpPort, "grpc_listen_address": "127.0.0.1", "grpc_listen_port": grpcPort, "log_level": "warn"},
		"memberlist":           map[string]any{"bind_addr": []string{"127.0.0.1"}, "bind_port": memberlistPort, "advertise_addr": "127.0.0.1"},
		"blocks_storage": map[string]any{
			"backend":      "filesystem",
			"bucket_store": map[string]any{"sync_dir": data("tsdb-sync")},
			"filesystem":   map[string]any{"dir": data("blocks")},
			"tsdb":         map[string]any{"dir": data("tsdb")},
		},
		"compactor":      map[string]any{"data_dir": data("compactor"), "sharding_ring": map[string]any{"kvstore": inmemory, "instance_addr": "127.0.0.1"}},
		"distributor":    map[string]any{"ring": map[string]any{"kvstore": inmemory, "instance_addr": "127.0.0.1"}},
		"ingester":       map[string]any{"ring": map[string]any{"kvstore": inmemory, "instance_addr": "127.0.0.1", "replication_factor": 1}},
		"store_gateway":  map[string]any{"sharding_ring": map[string]any{"kvstore": inmemory, "instance_addr": "127.0.0.1", "replication_factor": 1}},
		"ruler":          map[string]any{"ring": map[string]any{"kvstore": inmemory, "instance_addr": "127.0.0.1"}},
		"ruler_storage":  map[string]any{"backend": "local", "local": map[string]any{"directory": data("rules")}},
		"runtime_config": map[string]any{"file": data("runtime.yaml")},
		// Not ./metrics-activity.log, the package directory.
		"activity_tracker": map[string]any{"filepath": data("metrics-activity.log")},
	}
	write := func(name string, v any) {
		b, err := yaml.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(data(name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("mimir.yaml", cfg)
	write("runtime.yaml", map[string]any{"overrides": overrides})
	if err := os.MkdirAll(data("rules"), 0o755); err != nil {
		t.Fatal(err)
	}

	m.log = &bytes.Buffer{}
	m.cmd = exec.Command(m.bin, "-config.file="+data("mimir.yaml"), "-target=all",
		"-query-frontend.instance-addr=127.0.0.1", "-query-scheduler.ring.instance-addr=127.0.0.1")
	m.cmd.Stdout, m.cmd.Stderr = m.log, m.log
	if err := m.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = m.cmd.Process.Kill()
		_, _ = m.cmd.Process.Wait()
		if t.Failed() {
			t.Logf("Mimir's log:\n%s", tail(m.log.String(), 40))
		}
	})
	m.http = "http://127.0.0.1:" + strconv.Itoa(httpPort)
	deadline := time.Now().Add(2 * time.Minute)
	for {
		resp, err := http.Get(m.http + "/ready")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			err = fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
		}
		if time.Now().After(deadline) || m.cmd.ProcessState != nil {
			t.Fatalf("Mimir isn't ready: %v\n%s", err, tail(m.log.String(), 40))
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// push sends one OTLP request (protobuf) for a tenant and returns Mimir's
// answer, "<status> <body>". A refusal isn't an error: comparing what's
// stored is what decides (decision 72). An unavailable Mimir is retried.
func (m *mimirProc) push(t *testing.T, tenant string, md pmetric.Metrics) string {
	t.Helper()
	// pmetric's protobuf form is an OTLP export request's.
	body, err := (&pmetric.ProtoMarshaler{}).MarshalMetrics(md)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; ; attempt++ {
		req, _ := http.NewRequest(http.MethodPost, m.http+"/otlp/v1/metrics", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/x-protobuf")
		req.Header.Set("X-Scope-OrgID", tenant)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		answer, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 500 && attempt < 20 {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		return strconv.Itoa(resp.StatusCode) + " " + strings.TrimSpace(string(answer))
	}
}

// result is a query API result: a matrix or a vector.
type result []struct {
	Metric     map[string]string `json:"metric"`
	Values     [][2]any          `json:"values"`
	Histograms [][2]any          `json:"histograms"`
}

// query runs an instant query for a tenant.
func (m *mimirProc) query(t *testing.T, tenant, query string, at time.Time) result {
	t.Helper()
	q := url.Values{"query": {query}, "time": {strconv.FormatFloat(float64(at.UnixMilli())/1000, 'f', 3, 64)}}
	req, _ := http.NewRequest(http.MethodGet, m.http+"/prometheus/api/v1/query?"+q.Encode(), nil)
	req.Header.Set("X-Scope-OrgID", tenant)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("query %s: %s: %s", query, resp.Status, body)
	}
	var r struct {
		Data struct {
			Result result `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("query %s: %v: %s", query, err, body)
	}
	return r.Data.Result
}

// stored reads every series of a tenant with its raw samples, and which
// series an instant query still returns.
func (m *mimirProc) stored(t *testing.T, tenant string, base time.Time) series {
	t.Helper()
	at := base.Add(time.Minute)
	live := map[string]bool{}
	for _, s := range m.query(t, tenant, `{__name__=~".+"}`, at) {
		live[labelsOf(s.Metric)] = true
	}
	ms := func(v any) time.Time { return time.UnixMilli(int64(math.Round(v.(float64) * 1000))) }
	got := series{}
	for _, s := range m.query(t, tenant, `{__name__=~".+"}[1h]`, at) {
		var smp []string
		for _, v := range s.Values {
			f, err := strconv.ParseFloat(v[1].(string), 64)
			if err != nil {
				t.Fatalf("value %v: %v", v[1], err)
			}
			smp = append(smp, sample(ms(v[0]), f))
		}
		for _, h := range s.Histograms {
			smp = append(smp, strconv.FormatInt(ms(h[0]).UnixMilli(), 10)+" "+histogramText(h[1]))
		}
		sort.SliceStable(smp, func(i, j int) bool { return smp[i] < smp[j] })
		k := labelsOf(s.Metric)
		if !live[k] {
			smp = append(smp, "ended")
		}
		got[k] = smp
	}
	return finish(got)
}

// labelsOf formats labels as the model's are.
func labelsOf(m map[string]string) string { return labels.FromMap(m).String() }
