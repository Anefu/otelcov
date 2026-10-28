package k8sattributes

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Anefu/otelcov/internal/kube"
	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/nodes/nodetest"
)

func node(t *testing.T, raw map[string]any, pods ...corev1.Pod) *Node {
	t.Helper()
	n, err := New("collector/k8sattributes", raw, kube.Objects{Pods: pods})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

var extractAll = map[string]any{"extract": map[string]any{"metadata": []any{
	"k8s.pod.name", "k8s.container.name", "container.id", "k8s.deployment.name", "k8s.pod.start_time",
}}}

func TestContainerIdentification(t *testing.T) {
	pod := nodetest.Pod("web-7d9f-x", "10.0.0.1", "u1", nodetest.Container("app"), nodetest.Container("sidecar"))
	pod.Labels = map[string]string{"pod-template-hash": "7d9f"}
	pod.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-7d9f"}}
	n := node(t, extractAll, pod)

	// container.id identifies the container even in a multi-container pod.
	out := nodetest.Run(t, n, nodetest.Item("s1", map[string]string{
		"k8s.pod.ip": "10.0.0.1", "container.id": "id-web-7d9f-x-sidecar",
	}))
	got := nodetest.Attrs(out.Items[0])
	if got["k8s.container.name"] != "sidecar" || got["k8s.deployment.name"] != "web" || got["k8s.pod.name"] != "web-7d9f-x" {
		t.Errorf("attrs: %v", got)
	}
	if e, ok := nodetest.Find(out.Events, "container.id", lineage.Kept); !ok || e.Value != "id-web-7d9f-x-sidecar" {
		t.Errorf("container.id should be kept: %+v", out.Events)
	}
	// A pod without a creation time gets no start time.
	if e, ok := nodetest.Find(out.Events, "k8s.pod.start_time", lineage.Skipped); !ok || e.Reason != "the pod has no `creationTimestamp`" {
		t.Errorf("start_time should be skipped: %+v", out.Events)
	}

	// Without either, a multi-container pod can't name the container.
	out = nodetest.Run(t, n, nodetest.Item("s2", map[string]string{"k8s.pod.ip": "10.0.0.1"}))
	if _, ok := nodetest.Attrs(out.Items[0])["k8s.container.name"]; ok {
		t.Error("container name must not be set")
	}
	if e, ok := nodetest.Find(out.Events, "k8s.container.name", lineage.Skipped); !ok || !strings.Contains(e.Reason, "app, sidecar") {
		t.Errorf("want skipped with container names: %+v", out.Events)
	}
}

func TestAssociation(t *testing.T) {
	pod := nodetest.Pod("p", "10.0.0.1", "u1", nodetest.Container("app"))
	n := node(t, nil, pod) // default rules and metadata

	// No pod IP: falls back to the k8s.pod.uid rule.
	out := nodetest.Run(t, n, nodetest.Item("s1", map[string]string{"k8s.pod.uid": "u1"}))
	if e, ok := nodetest.Find(out.Events, "", lineage.Associated); !ok || e.Detail["via"] != "resource_attribute k8s.pod.uid=u1" {
		t.Errorf("want association by uid: %+v", out.Events)
	}
	if nodetest.Attrs(out.Items[0])["k8s.pod.uid"] != "u1" {
		t.Error("uid should be kept")
	}

	// Nothing matches: nothing is added, and the node says so.
	out = nodetest.Run(t, n, nodetest.Item("s2", map[string]string{"k8s.pod.ip": "10.9.9.9"}))
	if len(nodetest.Attrs(out.Items[0])) != 1 {
		t.Errorf("attrs changed: %v", nodetest.Attrs(out.Items[0]))
	}
	if _, ok := nodetest.Find(out.Events, "", lineage.Skipped); !ok {
		t.Errorf("want skipped: %+v", out.Events)
	}

	// Name + namespace together.
	n = node(t, map[string]any{"pod_association": []any{map[string]any{"sources": []any{
		map[string]any{"from": "resource_attribute", "name": "k8s.pod.name"},
		map[string]any{"from": "resource_attribute", "name": "k8s.namespace.name"},
	}}}}, pod)
	out = nodetest.Run(t, n, nodetest.Item("s3", map[string]string{"k8s.pod.name": "p", "k8s.namespace.name": "ns"}))
	if _, ok := nodetest.Find(out.Events, "", lineage.Associated); !ok {
		t.Errorf("want association by name: %+v", out.Events)
	}
}

func TestPassthroughAndUnknownConfig(t *testing.T) {
	pod := nodetest.Pod("p", "10.0.0.1", "u1", nodetest.Container("app"))
	out := nodetest.Run(t, node(t, map[string]any{"passthrough": true}, pod),
		nodetest.Item("s1", map[string]string{"k8s.pod.ip": "10.0.0.1"}))
	if len(out.Events) != 0 || len(nodetest.Attrs(out.Items[0])) != 1 {
		t.Errorf("passthrough should change nothing: %+v", out.Events)
	}

	if _, err := New("x", map[string]any{"filter": map[string]any{"namespace": "a"}}, kube.Objects{}); err == nil {
		t.Error("unmodeled settings must be rejected, not ignored")
	}
}
