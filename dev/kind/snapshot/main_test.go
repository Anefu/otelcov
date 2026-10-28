package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/Anefu/otelcov/internal/kube"
	"github.com/Anefu/otelcov/internal/report"
	"github.com/Anefu/otelcov/internal/scenario"
)

// start is when the fake agents start recording.
var start = time.Date(2026, 10, 23, 19, 0, 0, 0, time.UTC)

type sender struct {
	node, ip string
	extra    map[string]string // SDK resource attributes besides service.name
}

var senders = []sender{
	{node: "otelcov-worker", ip: "10.244.1.5"},
	{node: "otelcov-worker2", ip: "10.244.2.7", extra: map[string]string{"k8s.container.name": "vllm"}},
}

// request is one export from the demo app, as the tap records it: the SDK
// resource plus k8s.pod.ip from k8sattributes/tap. detected adds what
// resourcedetection/capture would.
func requestLine(t *testing.T, s sender, at time.Time, detected bool) string {
	t.Helper()
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	attrs := rm.Resource().Attributes()
	attrs.PutStr("service.name", "vllm")
	attrs.PutStr("telemetry.sdk.name", "opentelemetry")
	attrs.PutStr("telemetry.sdk.language", "python")
	for k, v := range s.extra {
		attrs.PutStr(k, v)
	}
	attrs.PutStr("k8s.pod.ip", s.ip)
	if detected {
		attrs.PutStr("host.name", s.node)
		attrs.PutStr("deployment.environment.name", "kind")
		attrs.PutStr("os.type", "linux")
	}
	ms := rm.ScopeMetrics().AppendEmpty().Metrics()
	ts := pcommon.NewTimestampFromTime(at)

	mem := ms.AppendEmpty()
	mem.SetName("process.memory.usage")
	mem.SetUnit("By")
	dp := mem.SetEmptyGauge().DataPoints().AppendEmpty()
	dp.SetTimestamp(ts)
	dp.SetIntValue(48_000_000)

	hb := ms.AppendEmpty()
	hb.SetName("debug.heartbeat")
	hb.SetEmptyGauge().DataPoints().AppendEmpty().SetIntValue(1)
	hb.Gauge().DataPoints().At(0).SetTimestamp(ts)

	req := ms.AppendEmpty()
	req.SetName("vllm.requests")
	req.SetUnit("{request}")
	sum := req.SetEmptySum()
	sum.SetIsMonotonic(true)
	sum.SetAggregationTemporality(pmetric.AggregationTemporalityDelta)
	rdp := sum.DataPoints().AppendEmpty()
	rdp.SetStartTimestamp(pcommon.NewTimestampFromTime(at.Add(-15 * time.Second)))
	rdp.SetTimestamp(ts)
	rdp.SetIntValue(42)

	var m pmetric.JSONMarshaler
	b, err := m.MarshalMetrics(md)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// pods as kubectl prints them from a newer cluster: status.observedGeneration
// isn't in the 1.32 API types. Running pods list their containers in
// status.containerStatuses, which k8sattributes uses to identify them.
func podsYAML() string {
	// Pods are named <deployment>-<pod-template-hash>-<suffix>, owned by the
	// ReplicaSet <deployment>-<pod-template-hash>, as Kubernetes makes them.
	pod := func(name, node, ip, containers, env string, names ...string) string {
		var statuses strings.Builder
		for _, c := range names {
			fmt.Fprintf(&statuses, "        - {name: %s, containerID: containerd://%s-%s, ready: true, restartCount: 0}\n", c, name, c)
		}
		parts := strings.Split(name, "-")
		rs := strings.Join(parts[:len(parts)-1], "-")
		hash := parts[len(parts)-2]
		instance := strings.Join(parts[:len(parts)-2], "-")
		return fmt.Sprintf(`  - apiVersion: v1
    kind: Pod
    metadata:
      name: %s
      namespace: inference
      uid: uid-%[1]s
      labels: {app.kubernetes.io/name: vllm, app.kubernetes.io/instance: %[2]s, pod-template-hash: %[3]s}
      ownerReferences:
        - {apiVersion: apps/v1, kind: ReplicaSet, name: %[4]s, uid: rs-%[4]s, controller: true}
    spec:
      nodeName: %s
      containers:
%s
    status:
      phase: Running
      podIP: %s
      podIPs: [{ip: %s}]
      observedGeneration: 1
      containerStatuses:
%s`, name, instance, hash, rs, node, strings.ReplaceAll(containers, "ENV", env), ip, ip, statuses.String())
	}
	vllm := "        - name: vllm\n          image: otelcov-demo\n          env:\n            - {name: OTEL_SERVICE_NAME, value: vllm}\nENV"
	return "apiVersion: v1\nkind: List\nmetadata: {resourceVersion: \"\"}\nitems:\n" +
		pod("vllm-chat-7d4b9c-x1", "otelcov-worker", "10.244.1.5", vllm, "", "vllm") +
		pod("vllm-embed-5f6a7b-y2", "otelcov-worker2", "10.244.2.7",
			vllm+"\n        - name: log-shipper\n          image: busybox:1.37",
			"            - {name: OTEL_RESOURCE_ATTRIBUTES, value: k8s.container.name=vllm}", "vllm", "log-shipper") +
		// kubectl get pods,namespaces,nodes lists the namespaces and nodes too.
		"  - {apiVersion: v1, kind: Namespace, metadata: {name: inference, uid: ns-inference, labels: {kubernetes.io/metadata.name: inference}}}\n" +
		"  - {apiVersion: v1, kind: Node, metadata: {name: otelcov-worker, uid: node-1, labels: {kubernetes.io/hostname: otelcov-worker}}}\n" +
		"  - {apiVersion: v1, kind: Node, metadata: {name: otelcov-worker2, uid: node-2, labels: {kubernetes.io/hostname: otelcov-worker2}}}\n"
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeCapture writes what capture.sh would copy off the cluster: 5 minutes
// of exports every 15s from each app, on its node's agent. The last tap line
// is cut off, as if the file was being written during the copy.
func fakeCapture(t *testing.T, dir string) {
	t.Helper()
	for _, s := range senders {
		var tap, detected []string
		for at := start.Add(15 * time.Second); !at.After(start.Add(5 * time.Minute)); at = at.Add(15 * time.Second) {
			tap = append(tap, requestLine(t, s, at, false))
			detected = append(detected, requestLine(t, s, at, true))
		}
		cut := requestLine(t, s, start.Add(6*time.Minute), false)
		write(t, filepath.Join(dir, "tap", s.node+".jsonl"), strings.Join(tap, "\n")+"\n"+cut[:len(cut)/2])
		write(t, filepath.Join(dir, "detected", s.node+".jsonl"), strings.Join(detected, "\n")+"\n")
	}
	write(t, filepath.Join(dir, "pods.yaml"), podsYAML())
}

// The kind setup, end to end without a cluster: a fake capture, the
// snapshot, then otelcov on the generated scenarios with the real kind
// configs (topologies, collector configs, Mimir config, rules, Alertmanager).
func TestKindSetup(t *testing.T) {
	// The capture lives where capture.sh puts it, so the templates' relative
	// paths (../../otelcov/...) resolve to the real kind configs.
	kind, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(kind, "captured-test-")
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("OTELCOV_KEEP_CAPTURE") == "" {
		t.Cleanup(func() { os.RemoveAll(dir) })
	} else {
		t.Logf("capture kept in %s", dir)
	}
	fakeCapture(t, dir)

	err = run(options{
		dir: dir, agentConfig: filepath.Join(kind, "config/agent.yaml"),
		collector: "agent", processor: "resourcedetection", capture: "resourcedetection/capture",
		templates: filepath.Join(kind, "otelcov/scenarios"), appsWhatIfs: filepath.Join(kind, "whatif/apps-*.yaml"),
	}, start.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	// Apps what-ifs keep the captured namespaces and nodes.
	whatifs, _ := filepath.Glob(filepath.Join(dir, "pods-*.yaml"))
	if len(whatifs) == 0 {
		t.Fatal("no apps what-if pods written")
	}
	for _, p := range whatifs {
		objs, _, err := kube.Load(p)
		if err != nil || len(objs.Pods) != 2 || len(objs.Namespaces) != 1 || len(objs.Nodes) != 2 {
			t.Errorf("%s: %d pods, %d namespaces, %d nodes, %v", filepath.Base(p), len(objs.Pods), len(objs.Namespaces), len(objs.Nodes), err)
		}
	}

	snap, err := scenario.LoadSnapshot(filepath.Join(dir, "snapshot.meta.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	rec := snap.ResourceDetection["agent"]["resourcedetection"]
	if rec.Values["otelcov-worker2"]["host.name"] != "otelcov-worker2" || rec.Values["otelcov-worker"]["os.type"] != "linux" || rec.Config["override"] != false {
		t.Errorf("resource detection: %+v", rec)
	}
	if _, raw := rec.Values["otelcov-worker"]["k8s.pod.ip"]; raw {
		t.Error("attributes the tap already has aren't detected values")
	}

	// Status per scenario and pod (vllm-chat, vllm-embed).
	want := map[string][2]string{
		"01-baseline.yaml":      {report.Covered, report.Covered},
		"02-team-pr.yaml":       {report.NotCovered, report.NotCovered},
		"03-agent-badport.yaml": {report.Unverified, report.Unverified},
		// The motivating bug: without its env var, vllm-embed's container
		// can't be named (it has a sidecar). vllm-chat is unaffected.
		"04-embed-no-env.yaml": {report.Covered, report.NotCovered},
	}
	for name, statuses := range want {
		t.Run(name, func(t *testing.T) {
			s, err := scenario.Load(filepath.Join(dir, "scenarios", name), dir)
			if err != nil {
				t.Fatal(err)
			}
			// The last complete tap line is at start+5m; the cut line is skipped.
			if s.EvalTime != "2026-10-23T19:05:00Z" {
				t.Errorf("eval_time = %s", s.EvalTime)
			}
			rep, err := scenario.Report(context.Background(), s)
			if err != nil {
				t.Fatal(err)
			}
			if len(rep.Summary) != 2 {
				t.Fatalf("want both pods, got %+v", rep.Summary)
			}
			for _, e := range rep.Summary {
				status := statuses[0]
				if strings.Contains(e.Pod, "vllm-embed") {
					status = statuses[1]
				}
				if e.Status != status {
					t.Errorf("%s: %s, want %s (broken at %s, unverified by %v)", e.Pod, e.Status, status, e.BrokenAt, e.UnverifiedBy)
				}
			}
			if name == "04-embed-no-env.yaml" {
				if len(rep.Diagnosis) != 1 || rep.Diagnosis[0].MissingLabel != "k8s_container_name" ||
					!strings.Contains(rep.Diagnosis[0].Explanation, "k8sattributes") || !strings.Contains(rep.Diagnosis[0].Explanation, "2 containers") {
					t.Errorf("want the k8sattributes diagnosis for vllm-embed: %+v", rep.Diagnosis)
				}
			}
			var podsWarning bool
			for _, p := range rep.Problems {
				podsWarning = podsWarning || (p.Where == "pods.yaml" && strings.Contains(p.Message, "status.observedGeneration"))
			}
			if !podsWarning {
				t.Errorf("want a warning for the unknown pod field: %+v", rep.Problems)
			}
		})
	}
}

func TestCaptureProcessorMustMatch(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "agent.yaml"), `
processors:
  resourcedetection: {detectors: [env, system], override: false}
  resourcedetection/capture: {detectors: [env], override: true}
`)
	_, err := detectionConfig(options{agentConfig: filepath.Join(dir, "agent.yaml"), processor: "resourcedetection", capture: "resourcedetection/capture"})
	if err == nil || !strings.Contains(err.Error(), "same settings apart from override") {
		t.Errorf("want a mismatch error, got %v", err)
	}
}

func TestBadLineInTheMiddle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tap.jsonl")
	write(t, path, requestLine(t, senders[0], start, false)+"\n{not json\n"+requestLine(t, senders[0], start, false)+"\n")
	if _, err := readRequests(path); err == nil {
		t.Error("a bad line before the last one is an error")
	}
}
