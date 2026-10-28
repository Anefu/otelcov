package requirements

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func load(t *testing.T, body string) ([]Requirement, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "r.yaml")
	if err := os.WriteFile(p, []byte("requirements:\n"+body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func TestLoad(t *testing.T) {
	rs, err := load(t, `
  - id: full
    subjects: { kind: pod, namespace: ns, labels: { app: vllm, team: "*" }, container: vllm }
    signal: { metrics: [a, b] }
    reaches:
      - { alert: A, receivers: ["{{ subject.labels.team }}-pager", oncall], labels: [k8s_pod_name] }
      - dashboard: { uid: d, panel: Memory }
    must_not_reach: [ { backend: mimir } ]
  - id: node
    subjects: { kind: node }
    signal: { metrics: [disk] }
    reaches: [ { alert: Disk } ]
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || len(rs[0].Reaches) != 2 || rs[0].Reaches[1].Dashboard.Panel != "Memory" {
		t.Errorf("%+v", rs)
	}

	for _, tc := range []struct{ body, want string }{
		{"  - { id: x, subjects: { kind: pod }, signal: { metrics: [m] } }\n", "at least one destination"},
		{"  - { id: x, subjects: { kind: cluster }, signal: { metrics: [m] }, reaches: [ { alert: A } ] }\n", "subjects.kind"},
		{"  - { id: x, subjects: { kind: pod }, signal: {}, reaches: [ { alert: A } ] }\n", "signal.metrics"},
		{"  - { id: x, subjects: { kind: node, container: c }, signal: { metrics: [m] }, reaches: [ { alert: A } ] }\n", "only applies to pods"},
		{"  - { id: x, subjects: { kind: pod }, signal: { metrics: [m] }, reaches: [ { alert: A, backend: mimir } ] }\n", "exactly one of"},
		{"  - { id: x, subjects: { kind: pod }, signal: { metrics: [m] }, reaches: [ { backend: mimir, receivers: [r] } ] }\n", "only apply to alert"},
		{"  - { id: x, subjects: { kind: pod }, signal: { metrics: [m] }, reaches: [ { alert: A, receivers: [\"{{ pod.team }}\"] } ] }\n", "templates can only use"},
		{"  - { id: x, subjects: { kind: entity }, signal: { metrics: [m] }, reaches: [ { alert: A } ] }\n", "needs subjects.attributes"},
		{"  - { id: x, subjects: { kind: pod }, signal: { metrics: [m] }, reaches: [ { alert: A } ] }\n  - { id: x, subjects: { kind: pod }, signal: { metrics: [m] }, reaches: [ { alert: A } ] }\n", "used twice"},
		{"  - { id: x, subjects: { kind: pod }, signal: { metric: m }, reaches: [ { alert: A } ] }\n", "unknown field"},
	} {
		if _, err := load(t, tc.body); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: got %v, want %q", tc.body, err, tc.want)
		}
	}
}

func TestPodsAndTemplates(t *testing.T) {
	pod := func(name, ns string, labels map[string]string) corev1.Pod {
		return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels}}
	}
	pods := []corev1.Pod{
		pod("a", "ns", map[string]string{"app": "vllm", "team": "search"}),
		pod("b", "ns", map[string]string{"app": "vllm"}),
		pod("c", "other", map[string]string{"app": "vllm", "team": "search"}),
	}
	r := Requirement{Subjects: Subjects{Kind: KindPod, Namespace: "ns", Labels: map[string]string{"app": "vllm", "team": "*"}}}
	got := r.Pods(pods)
	if len(got) != 1 || got[0].Name != "a" {
		t.Errorf("\"*\" means the label must exist: %v", got)
	}
	if (Requirement{Subjects: Subjects{Kind: KindNode}}).Pods(pods) != nil {
		t.Error("only pod requirements select pods")
	}

	if s, err := Resolve("{{ subject.labels.team }}-pager@{{subject.namespace}}/{{ subject.name }}", &pods[0]); err != nil || s != "search-pager@ns/a" {
		t.Errorf("%q, %v", s, err)
	}
	if _, err := Resolve("{{ subject.labels.team }}-pager", &pods[1]); err == nil || !strings.Contains(err.Error(), "needs pod label `team`") {
		t.Errorf("want a missing-label error, got %v", err)
	}
	if s, err := Resolve("inference-pager", &pods[1]); err != nil || s != "inference-pager" {
		t.Errorf("plain receivers pass through: %q, %v", s, err)
	}
}
