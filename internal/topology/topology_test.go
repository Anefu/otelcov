package topology

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

const agentCfg = `
receivers:
  otlp:
    protocols:
      grpc: {endpoint: "0.0.0.0:4317"}
processors:
  batch: {}
exporters:
  otlp/gateway: {endpoint: "gateway:4317"}
  file/tap: {path: /tmp/tap.jsonl}
service:
  pipelines:
    metrics/tap: {receivers: [otlp], exporters: [file/tap]}
    metrics: {receivers: [otlp], processors: [batch], exporters: [otlp/gateway]}
`

const gatewayCfg = `
receivers:
  otlp:
    protocols:
      grpc: {endpoint: "0.0.0.0:4317"}
processors:
  batch: {}
exporters:
  otlphttp/mimir: {endpoint: "http://mimir:8080/otlp"}
  debug: {}
service:
  pipelines:
    metrics: {receivers: [otlp], processors: [batch], exporters: [otlphttp/mimir, debug]}
`

const topo = `
version: 1
collectors:
  agent: {config: agent.yaml, tap: metrics/tap}
  gateway: {config: gateway.yaml}
backends:
  mimir: {type: mimir-otlp, config: mimir.yaml}
consumers:
  rules: {type: prometheus-rules, paths: [rules.yaml], reads: mimir}
external: [gateway/exporters/debug]
joins:
  - {from: agent/exporters/otlp/gateway, to: gateway/receivers/otlp}
  - {from: gateway/exporters/otlphttp/mimir, to: mimir}
`

// fixture writes a topology and its configs to a temp dir. edits replace text
// in the named file first.
func fixture(t *testing.T, edits ...[3]string) *File {
	t.Helper()
	files := map[string]string{
		"agent.yaml": agentCfg, "gateway.yaml": gatewayCfg, "topology.yaml": topo,
		"mimir.yaml": "{}\n", "rules.yaml": "groups: []\n",
	}
	for _, e := range edits {
		if !strings.Contains(files[e[0]], e[1]) {
			t.Fatalf("edit: %q isn't in %s", e[1], e[0])
		}
		files[e[0]] = strings.Replace(files[e[0]], e[1], e[2], 1)
	}
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f, err := Load(filepath.Join(dir, "topology.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func problems(v *Validation, s Severity) []string {
	var out []string
	for _, p := range v.Problems {
		if p.Severity == s {
			out = append(out, p.Where+": "+p.Message)
		}
	}
	return out
}

func TestValidTopology(t *testing.T) {
	v := Validate(fixture(t))
	if len(v.Problems) != 0 || len(v.BadJoins) != 0 {
		t.Errorf("want no problems, got %+v %v", v.Problems, v.BadJoins)
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edits [][3]string
		sev   Severity
		want  []string // substrings of every problem of that severity, in order
		join  int      // index of the join that must be bad, or -1
	}{
		{"port mismatch", [][3]string{{"agent.yaml", "gateway:4317", "gateway:4318"}},
			Unverified, []string{"sends to port 4318, but receiver gateway/otlp listens on port 4317"}, 0},
		{"receiver default localhost", [][3]string{{"gateway.yaml", `grpc: {endpoint: "0.0.0.0:4317"}`, "grpc: {}"}},
			Unverified, []string{"listens on localhost:4317 only"}, 0},
		{"protocol not enabled", [][3]string{{"gateway.yaml", `grpc: {endpoint: "0.0.0.0:4317"}`, `http: {endpoint: "0.0.0.0:4318"}`}},
			Unverified, []string{"sends OTLP over grpc, but receiver gateway/otlp doesn't enable grpc"}, 0},
		{"grpc exporter to mimir", [][3]string{
			{"gateway.yaml", "otlphttp/mimir: {endpoint: \"http://mimir:8080/otlp\"}", "otlp/mimir: {endpoint: \"mimir:9095\"}"},
			{"gateway.yaml", "exporters: [otlphttp/mimir, debug]", "exporters: [otlp/mimir, debug]"},
			{"topology.yaml", "gateway/exporters/otlphttp/mimir", "gateway/exporters/otlp/mimir"}},
			Unverified, []string{"Mimir's OTLP endpoint takes OTLP over HTTP"}, 1},
		{"exporter not joined or external", [][3]string{{"topology.yaml", "external: [gateway/exporters/debug]", "external: []"}},
			Fatal, []string{"gateway/exporters/debug: exporter isn't joined"}, -1},
		{"join from unused exporter", [][3]string{{"topology.yaml", "agent/exporters/otlp/gateway", "agent/exporters/file/tap"}},
			Fatal, []string{"exporter file/tap isn't defined or isn't used", "agent/exporters/otlp/gateway: exporter isn't joined"}, -1},
		{"no tap", [][3]string{{"topology.yaml", ", tap: metrics/tap", ""}},
			Fatal, []string{"no collector has a tap pipeline", "agent/exporters/file/tap: exporter isn't joined"}, -1},
		{"tap pipeline missing", [][3]string{{"topology.yaml", "tap: metrics/tap", "tap: metrics/rec"}},
			Fatal, []string{`tap pipeline "metrics/rec" doesn't exist`, "agent/exporters/file/tap: exporter isn't joined"}, -1},
		{"feature gate conflict", [][3]string{
			{"topology.yaml", "{config: agent.yaml, tap: metrics/tap}", "{config: agent.yaml, tap: metrics/tap, feature_gates: [some.gate]}"},
			{"topology.yaml", "{config: gateway.yaml}", "{config: gateway.yaml, feature_gates: [-some.gate]}"}},
			Fatal, []string{"feature gate some.gate is off here but agent:on"}, -1},
		{"untapped receiver", [][3]string{
			{"agent.yaml", "receivers:\n  otlp:", "receivers:\n  prometheus: {}\n  otlp:"},
			{"agent.yaml", "metrics: {receivers: [otlp]", "metrics: {receivers: [otlp, prometheus]"}},
			Warning, []string{"receiver prometheus feeds pipeline metrics but isn't tapped"}, -1},
		{"hash changed", [][3]string{{"topology.yaml", "{config: gateway.yaml}", "{config: gateway.yaml, sha256: 0000}"}},
			Warning, []string{"gateway.yaml changed since the topology was written"}, -1},
		{"unresolved env", [][3]string{{"agent.yaml", "gateway:4317", "${env:GATEWAY}"}},
			Warning, []string{`endpoint "${env:GATEWAY}" has an unresolved env reference`}, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := Validate(fixture(t, tc.edits...))
			got := problems(v, tc.sev)
			ok := len(got) == len(tc.want)
			for i := 0; ok && i < len(got); i++ {
				ok = strings.Contains(got[i], tc.want[i])
			}
			if !ok {
				t.Errorf("want %s problems %q, got %q (all: %+v)", tc.sev, tc.want, got, v.Problems)
			}
			if tc.join >= 0 {
				if _, bad := v.BadJoins[tc.join]; !bad || len(v.BadJoins) != 1 {
					t.Errorf("want join %d bad, got %v", tc.join, v.BadJoins)
				}
			} else if len(v.BadJoins) != 0 {
				t.Errorf("want no bad joins, got %v", v.BadJoins)
			}
		})
	}
}

func TestEnvResolvesEndpoint(t *testing.T) {
	f := fixture(t,
		[3]string{"agent.yaml", "gateway:4317", "${env:GATEWAY}"},
		[3]string{"topology.yaml", "{config: agent.yaml, tap: metrics/tap}", "{config: agent.yaml, tap: metrics/tap, env: {GATEWAY: \"gateway:4318\"}}"})
	v := Validate(f)
	if w := problems(v, Warning); len(w) != 0 {
		t.Errorf("env value given: want no warning, got %q", w)
	}
	if u := problems(v, Unverified); len(u) != 1 || !strings.Contains(u[0], "port 4318") {
		t.Errorf("the env value should be compared: %q", u)
	}
}

func item(id string) unit.Metric {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr("service.name", "app")
	m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName("m")
	m.SetEmptyGauge().DataPoints().AppendEmpty().SetIntValue(1)
	return unit.Metric{ID: lineage.SignalID(id), Data: md}
}

func run(t *testing.T, f *File) (*Runner, *lineage.Log) {
	t.Helper()
	v := Validate(f)
	if fatal := v.Fatal(); len(fatal) > 0 {
		t.Fatalf("fatal: %+v", fatal)
	}
	log := lineage.NewLog()
	in := []unit.Metric{item("s1")}
	log.AddSignal(lineage.Signal{ID: "s1", Node: "source", Kind: "otlp_metric", Name: "m"})
	r, err := NewRunner(f, v, Inputs{}, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Run(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	return r, log
}

func TestRunFollowsJoins(t *testing.T) {
	r, log := run(t, fixture(t))
	got := r.Delivered["mimir"]
	if len(got) != 1 || got[0].ID != "s1" {
		t.Fatalf("s1 should reach mimir unchanged in identity: %+v", got)
	}
	// The gateway exports to mimir and debug: debug gets a copy.
	var copied, exported bool
	for _, e := range log.Events() {
		if e.Node == "topology" && e.Action == lineage.Derived && e.Signal == "c1" && e.From[0] == "s1" {
			copied = true
		}
		if e.Node == "gateway/debug" && e.Action == lineage.Exported && e.Signal == "c1" {
			exported = true
		}
		if e.Action == lineage.Unverified {
			t.Errorf("unexpected unverified event: %+v", e)
		}
	}
	if !copied || !exported {
		t.Errorf("want c1 copied from s1 and exported by debug: %+v", log.Events())
	}
}

func TestRunMarksBadJoins(t *testing.T) {
	r, log := run(t, fixture(t, [3]string{"agent.yaml", "gateway:4317", "gateway:4318"}))
	if len(r.Delivered["mimir"]) != 1 {
		t.Errorf("data still flows through a bad join (to keep the rest of the path checked)")
	}
	var marked bool
	for _, e := range log.Events() {
		marked = marked || (e.Node == "agent/otlp/gateway" && e.Signal == "s1" && e.Action == lineage.Unverified &&
			strings.Contains(e.Reason, "can't be trusted"))
	}
	if !marked {
		t.Errorf("want s1 unverified at agent/otlp/gateway: %+v", log.Events())
	}
}

func TestRunUnmodeledProcessor(t *testing.T) {
	_, log := run(t, fixture(t,
		[3]string{"gateway.yaml", "batch: {}", "batch: {}\n  groupbyattrs: {}"},
		[3]string{"gateway.yaml", "processors: [batch]", "processors: [groupbyattrs, batch]"}))
	var unsupported bool
	for _, e := range log.Events() {
		unsupported = unsupported || (e.Node == "gateway/groupbyattrs" && e.Action == lineage.Unverified)
	}
	if !unsupported {
		t.Errorf("want groupbyattrs unverified: %+v", log.Events())
	}
}

func TestSharedProcessorNames(t *testing.T) {
	f := fixture(t,
		[3]string{"gateway.yaml", "metrics: {receivers: [otlp], processors: [batch], exporters: [otlphttp/mimir, debug]}",
			"metrics: {receivers: [otlp], processors: [batch], exporters: [otlphttp/mimir]}\n    metrics/debug: {receivers: [otlp], processors: [batch], exporters: [debug]}"})
	_, log := run(t, f)
	nodes := map[string]bool{}
	for _, e := range log.Events() {
		nodes[e.Node] = true
	}
	// batch is a pass-through and logs nothing; the copy for the second
	// pipeline names it.
	var second bool
	for _, e := range log.Events() {
		second = second || (e.Node == "topology" && strings.Contains(e.Reason, "gateway pipeline metrics/debug"))
	}
	if !second || !nodes["gateway/debug"] {
		t.Errorf("want a copy for metrics/debug, exported by debug: %+v", log.Events())
	}
}

// A pipeline that only records snapshot metadata is skipped like the tap:
// its exporter needs no join, and no data flows through it.
func TestSnapshotPipelines(t *testing.T) {
	withDetect := [][3]string{
		{"agent.yaml", "  file/tap: {path: /tmp/tap.jsonl}", "  file/tap: {path: /tmp/tap.jsonl}\n  file/detected: {path: /tmp/detected.jsonl}"},
		{"agent.yaml", "    metrics/tap: {receivers: [otlp], exporters: [file/tap]}", "    metrics/tap: {receivers: [otlp], exporters: [file/tap]}\n    metrics/detected: {receivers: [otlp], processors: [batch], exporters: [file/detected]}"},
	}
	listed := append(withDetect, [3]string{"topology.yaml", "{config: agent.yaml, tap: metrics/tap}", "{config: agent.yaml, tap: metrics/tap, snapshot_pipelines: [metrics/detected]}"})

	if got := problems(Validate(fixture(t, withDetect...)), Fatal); len(got) != 1 || !strings.Contains(got[0], "agent/exporters/file/detected: exporter isn't joined") {
		t.Errorf("unlisted, the pipeline is modeled and its exporter unknown: %q", got)
	}
	f := fixture(t, listed...)
	if v := Validate(f); len(v.Problems) != 0 {
		t.Errorf("listed: want no problems, got %+v", v.Problems)
	}
	r, log := run(t, f)
	if len(r.Delivered["mimir"]) != 1 {
		t.Errorf("want s1 delivered once: %+v", r.Delivered["mimir"])
	}
	for _, e := range log.Events() {
		if strings.Contains(e.Reason, "metrics/detected") || e.Node == "agent/file/detected" {
			t.Errorf("no data should flow through the snapshot pipeline: %+v", e)
		}
	}

	missing := fixture(t, [3]string{"topology.yaml", "{config: agent.yaml, tap: metrics/tap}", "{config: agent.yaml, tap: metrics/tap, snapshot_pipelines: [metrics/nope]}"})
	if got := problems(Validate(missing), Fatal); len(got) != 1 || !strings.Contains(got[0], `snapshot pipeline "metrics/nope" doesn't exist`) {
		t.Errorf("want missing snapshot pipeline fatal: %q", got)
	}
}
