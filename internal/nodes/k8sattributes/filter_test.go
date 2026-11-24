package k8sattributes

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Anefu/otelcov/internal/collector"
	"github.com/Anefu/otelcov/internal/kube"
	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/nodes/nodetest"
)

// Pods on two nodes, the way a daemonset agent's filter sees them.
func twoNodePods() []corev1.Pod {
	a := nodetest.Pod("a", "10.0.0.1", "ua", nodetest.Container("app"))
	a.Spec.NodeName = "kind-worker"
	b := nodetest.Pod("b", "10.0.0.2", "ub", nodetest.Container("app"))
	b.Spec.NodeName = "kind-worker2"
	return []corev1.Pod{a, b}
}

func filtered(t *testing.T, filter map[string]any, inst Instance, objs kube.Objects) *Node {
	t.Helper()
	n, err := New("agent/k8sattributes", map[string]any{"filter": filter,
		"extract": map[string]any{"metadata": []any{"k8s.pod.name"}}}, objs, inst)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

var nodeFromEnv = map[string]any{"node_from_env_var": "K8S_NODE_NAME"}

func nodeNameEnv() map[string]collector.EnvVar {
	return map[string]collector.EnvVar{"K8S_NODE_NAME": {FieldRef: collector.NodeNameField}}
}

func TestFilterNodeFromEnv(t *testing.T) {
	objs := kube.Objects{Pods: twoNodePods()}
	n := filtered(t, nodeFromEnv, Instance{Node: "kind-worker", Env: nodeNameEnv()}, objs)

	// The agent's own node: associated as without a filter.
	out := nodetest.Run(t, n, nodetest.Item("s1", map[string]string{"k8s.pod.ip": "10.0.0.1"}))
	if nodetest.Attrs(out.Items[0])["k8s.pod.name"] != "a" {
		t.Errorf("pod a is on the agent's node: %+v", out.Events)
	}
	if _, ok := nodetest.Find(out.Events, "", lineage.Unverified); ok {
		t.Errorf("the node is known: %+v", out.Events)
	}

	// Another node's pod: the processor never sees it, and says why.
	out = nodetest.Run(t, n, nodetest.Item("s2", map[string]string{"k8s.pod.ip": "10.0.0.2"}))
	e, ok := nodetest.Find(out.Events, "", lineage.Skipped)
	if !ok || nodetest.Attrs(out.Items[0])["k8s.pod.name"] != "" {
		t.Fatalf("pod b is on another node: %+v", out.Events)
	}
	for _, want := range []string{"pod `ns/b` matches", "never sees it", "runs on node `kind-worker2`", "only watches pods on node `kind-worker`", "`filter.node_from_env_var: K8S_NODE_NAME`"} {
		if !strings.Contains(e.Reason, want) {
			t.Errorf("reason %q lacks %q", e.Reason, want)
		}
	}
	if e.Detail["pod"] != "ns/b" {
		t.Errorf("detail: %+v", e.Detail)
	}
	// The attributes it would have added are skipped, with the same reason,
	// for the diagnosis.
	if e, ok := nodetest.Find(out.Events, "k8s.pod.name", lineage.Skipped); !ok || !strings.Contains(e.Reason, "never sees it") ||
		e.Detail["association"] != "filtered" || e.Detail["filter"] != "node" || e.Detail["pod_node"] != "kind-worker2" || e.Detail["watched_node"] != "kind-worker" {
		t.Errorf("want k8s.pod.name skipped: %+v", out.Events)
	}

	// A literal value names the node; node_from_env_var wins over node.
	n = filtered(t, map[string]any{"node_from_env_var": "K8S_NODE_NAME", "node": "kind-worker"},
		Instance{Env: map[string]collector.EnvVar{"K8S_NODE_NAME": {Value: "kind-worker2"}}}, objs)
	if out := nodetest.Run(t, n, nodetest.Item("s3", map[string]string{"k8s.pod.ip": "10.0.0.2"})); nodetest.Attrs(out.Items[0])["k8s.pod.name"] != "b" {
		t.Errorf("the env var names kind-worker2: %+v", out.Events)
	}
	// An empty value means no node filter (os.Getenv gives "").
	n = filtered(t, nodeFromEnv, Instance{Env: map[string]collector.EnvVar{"K8S_NODE_NAME": {}}}, objs)
	if out := nodetest.Run(t, n, nodetest.Item("s4", map[string]string{"k8s.pod.ip": "10.0.0.2"})); nodetest.Attrs(out.Items[0])["k8s.pod.name"] != "b" {
		t.Errorf("an empty node filters nothing: %+v", out.Events)
	}
	// filter.node alone.
	n = filtered(t, map[string]any{"node": "kind-worker"}, Instance{}, objs)
	out = nodetest.Run(t, n, nodetest.Item("s5", map[string]string{"k8s.pod.ip": "10.0.0.2"}))
	if e, _ := nodetest.Find(out.Events, "", lineage.Skipped); !strings.Contains(e.Reason, "(`filter.node`)") {
		t.Errorf("want the filter.node explanation: %+v", out.Events)
	}
}

func TestFilterNodeUnknown(t *testing.T) {
	objs := kube.Objects{Pods: twoNodePods()}
	for name, tc := range map[string]struct {
		inst Instance
		why  string
	}{
		"recording without a node": {Instance{Env: nodeNameEnv()}, "the recording doesn't say which node that is"},
		"topology without the var": {Instance{Node: "kind-worker"}, "the topology doesn't set `K8S_NODE_NAME` for this collector"},
	} {
		t.Run(name, func(t *testing.T) {
			n := filtered(t, nodeFromEnv, tc.inst, objs)
			// The pod is looked up on every node, and the result is unverified.
			out := nodetest.Run(t, n, nodetest.Item("s1", map[string]string{"k8s.pod.ip": "10.0.0.2"}))
			if nodetest.Attrs(out.Items[0])["k8s.pod.name"] != "b" {
				t.Errorf("pods are looked up on every node: %+v", out.Events)
			}
			e, ok := nodetest.Find(out.Events, "", lineage.Unverified)
			if !ok || !strings.Contains(e.Reason, tc.why) || !strings.Contains(e.Reason, "pod on node `kind-worker2` isn't known") {
				t.Errorf("want an unverified event saying why: %+v", out.Events)
			}
			// No pod matches on any node: nothing depends on the node.
			out = nodetest.Run(t, n, nodetest.Item("s2", map[string]string{"k8s.pod.ip": "10.9.9.9"}))
			if _, ok := nodetest.Find(out.Events, "", lineage.Unverified); ok {
				t.Errorf("no pod matches anywhere: %+v", out.Events)
			}
		})
	}
}

func TestFilterNamespaceLabelsFields(t *testing.T) {
	a := nodetest.Pod("a", "10.0.0.1", "ua", nodetest.Container("app"))
	a.Labels = map[string]string{"app": "web"}
	a.Status.Phase = corev1.PodRunning
	b := nodetest.Pod("b", "10.0.0.2", "ub", nodetest.Container("app"))
	b.Namespace = "other"
	c := nodetest.Pod("c", "10.0.0.3", "uc", nodetest.Container("app"))
	c.Labels = map[string]string{"app": "db"}
	c.Status.Phase = corev1.PodRunning
	d := nodetest.Pod("d", "10.0.0.4", "ud", nodetest.Container("app"))
	d.Labels = map[string]string{"app": "web"}
	d.Status.Phase = corev1.PodPending
	objs := kube.Objects{Pods: []corev1.Pod{a, b, c, d}}
	n := filtered(t, map[string]any{
		"namespace": "ns",
		"labels":    []any{map[string]any{"key": "app", "value": "db", "op": "not-equals"}, map[string]any{"key": "app", "op": "exists"}},
		"fields":    []any{map[string]any{"key": "status.phase", "value": "Running"}},
	}, Instance{}, objs)
	for ip, want := range map[string]string{
		"10.0.0.1": "",
		"10.0.0.2": "it's in namespace `other`, and the processor only watches namespace `ns` (`filter.namespace`)",
		"10.0.0.3": "its labels don't match `filter.labels` (`app!=db,app`)",
		"10.0.0.4": "its fields don't match `filter.fields` (`status.phase=Running`)",
	} {
		out := nodetest.Run(t, n, nodetest.Item("s1", map[string]string{"k8s.pod.ip": ip}))
		_, associated := nodetest.Find(out.Events, "", lineage.Associated)
		e, _ := nodetest.Find(out.Events, "", lineage.Skipped)
		if associated != (want == "") || !strings.Contains(e.Reason, want) {
			t.Errorf("%s: want %q: %+v", ip, want, out.Events)
		}
	}
}

// When pods share an identifier, only the pods the processor sees compete:
// the hidden pod started later, and would win without the filter.
func TestFilterSharedIdentifier(t *testing.T) {
	pods := twoNodePods()
	pods[1].Status.PodIP = "10.0.0.1"
	pods[0].Status.StartTime = &metav1.Time{Time: metav1.Now().Add(-1e12)}
	pods[1].Status.StartTime = &metav1.Time{Time: metav1.Now().Time}
	objs := kube.Objects{Pods: pods}
	n := filtered(t, nodeFromEnv, Instance{Node: "kind-worker", Env: nodeNameEnv()}, objs)
	out := nodetest.Run(t, n, nodetest.Item("s1", map[string]string{"k8s.pod.ip": "10.0.0.1"}))
	if got := nodetest.Attrs(out.Items[0])["k8s.pod.name"]; got != "a" {
		t.Errorf("the agent only sees pod a: got %q", got)
	}
	n = filtered(t, map[string]any{}, Instance{}, objs)
	out = nodetest.Run(t, n, nodetest.Item("s2", map[string]string{"k8s.pod.ip": "10.0.0.1"}))
	if got := nodetest.Attrs(out.Items[0])["k8s.pod.name"]; got != "b" {
		t.Errorf("without a filter the later pod wins: got %q", got)
	}
}

// The processor's node informer only watches the filter's node.
func TestFilterNodeObjects(t *testing.T) {
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "kind-worker", UID: "n1", Labels: map[string]string{"zone": "a"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "kind-worker2", UID: "n2", Labels: map[string]string{"zone": "b"}}},
	}
	raw := func() map[string]any {
		return map[string]any{"filter": nodeFromEnv, "extract": map[string]any{
			"metadata": []any{"k8s.node.uid"},
			"labels":   []any{map[string]any{"tag_name": "zone", "key": "zone", "from": "node"}, map[string]any{"key_regex": "z.*", "from": "node"}},
		}}
	}
	n, err := New("agent/k8sattributes", raw(), kube.Objects{Nodes: nodes}, Instance{Node: "kind-worker", Env: nodeNameEnv()})
	if err != nil {
		t.Fatal(err)
	}
	out := nodetest.Run(t, n, nodetest.Item("s1", map[string]string{"k8s.node.name": "kind-worker"}))
	if got := nodetest.Attrs(out.Items[0]); got["zone"] != "a" || got["k8s.node.uid"] != "n1" {
		t.Errorf("the agent's own node: %v", got)
	}
	out = nodetest.Run(t, n, nodetest.Item("s2", map[string]string{"k8s.node.name": "kind-worker2"}))
	for _, attr := range []string{"zone", "k8s.node.uid"} {
		e, ok := nodetest.Find(out.Events, attr, lineage.Skipped)
		if !ok || !strings.Contains(e.Reason, "only watches node `kind-worker`") {
			t.Errorf("%q: want skipped, another node: %+v", attr, out.Events)
		}
	}
	if e := out.Events[len(out.Events)-1]; e.Attribute != "" || !strings.HasPrefix(e.Reason, "labels matching `^(?:z.*)$` from the node aren't copied: the processor only watches node") {
		t.Errorf("want the key_regex rule skipped: %+v", e)
	}
	if got := nodetest.Attrs(out.Items[0]); got["zone"] != "" || got["k8s.node.uid"] != "" {
		t.Errorf("another node's labels aren't known: %v", got)
	}

	// Node unknown: whether the processor knows the node isn't known.
	n, err = New("agent/k8sattributes", raw(), kube.Objects{Nodes: nodes}, Instance{Env: nodeNameEnv()})
	if err != nil {
		t.Fatal(err)
	}
	out = nodetest.Run(t, n, nodetest.Item("s3", map[string]string{"k8s.node.name": "kind-worker2"}))
	if e, ok := nodetest.Find(out.Events, "zone", lineage.Unverified); !ok || !strings.Contains(e.Reason, "whether the processor knows node `kind-worker2`") {
		t.Errorf("want unverified node labels: %+v", out.Events)
	}
}

// Config checks, ported from contrib 0.134: config_test.go (bad_filter_label_op,
// bad_filter_field_op), internal/kube/client_test.go (TestBadFilters,
// TestErrorSelectorsFromFilters, TestFilters).
func TestFilterConfig(t *testing.T) {
	bad := map[string]map[string]any{
		"bad_filter_label_op": {"labels": []any{map[string]any{"key": "app", "value": "v", "op": "in"}}},
		"bad_filter_field_op": {"fields": []any{map[string]any{"key": "spec.nodeName", "value": "v", "op": "exists"}}},
		"label/invalid-key":   {"labels": []any{map[string]any{"key": "no spaces", "value": "v"}}},
		// Upstream's fake informer takes any field; the API server only
		// selects pods by some.
		"field/unsupported-key": {"fields": []any{map[string]any{"key": "k1", "value": "v1"}}},
	}
	for name, filter := range bad {
		if _, err := New("x", map[string]any{"filter": filter}, kube.Objects{}, Instance{}); err == nil {
			t.Errorf("%s: want a config error", name)
		}
	}
	good := map[string]struct {
		filter         map[string]any
		labels, fields string
	}{
		"no-filters": {map[string]any{}, "", ""},
		"namespace":  {map[string]any{"namespace": "default"}, "", ""},
		"labels-and-fields": {map[string]any{
			"labels": []any{map[string]any{"key": "k1", "value": "v1", "op": "equals"}, map[string]any{"key": "k2", "value": "v2", "op": "not-equals"},
				map[string]any{"key": "k3", "op": "exists"}, map[string]any{"key": "k4", "op": "does-not-exist"}},
			"fields": []any{map[string]any{"key": "metadata.name", "value": "v1", "op": "equals"}, map[string]any{"key": "status.phase", "value": "v2", "op": "not-equals"}},
		}, "k1=v1,k2!=v2,k3,!k4", "metadata.name=v1,status.phase!=v2"},
	}
	for name, tc := range good {
		n, err := New("x", map[string]any{"filter": tc.filter}, kube.Objects{}, Instance{})
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if n.watch.labels.String() != tc.labels || n.watch.fields.String() != tc.fields {
			t.Errorf("%s: labels %q, fields %q", name, n.watch.labels, n.watch.fields)
		}
	}
	// The node term, as the processor adds it to the field selector.
	n, err := New("x", map[string]any{"filter": map[string]any{"node": "ec2-test"}}, kube.Objects{}, Instance{})
	if err != nil || n.watch.node != "ec2-test" {
		t.Errorf("node: %+v, %v", n, err)
	}
}

// Without a pod, the pod's attributes are skipped (or kept, when the
// resource has them), and the node's UID isn't the pod's to skip.
func TestNoPodSkipsPodAttributes(t *testing.T) {
	n, err := New("agent/k8sattributes", map[string]any{"extract": map[string]any{
		"metadata": []any{"k8s.pod.name", "k8s.container.name", "k8s.node.uid"},
		"labels":   []any{map[string]any{"tag_name": "team", "key": "team"}},
	}}, kube.Objects{Pods: twoNodePods()}, Instance{})
	if err != nil {
		t.Fatal(err)
	}
	out := nodetest.Run(t, n, nodetest.Item("s1", map[string]string{"k8s.pod.ip": "10.9.9.9", "k8s.container.name": "app"}))
	for _, attr := range []string{"k8s.pod.name", "team"} {
		if e, ok := nodetest.Find(out.Events, attr, lineage.Skipped); !ok || e.Reason != "no pod matches k8s.pod.ip=10.9.9.9 (as the connection address, since pod_association isn't set)" || e.Detail["association"] != "no_match" {
			t.Errorf("%s: %+v", attr, out.Events)
		}
	}
	if _, ok := nodetest.Find(out.Events, "k8s.container.name", lineage.Kept); !ok {
		t.Errorf("the resource's container name is kept: %+v", out.Events)
	}
	for _, e := range out.Events {
		if e.Attribute == "k8s.node.uid" {
			t.Errorf("k8s.node.uid comes from the node, not the pod: %+v", e)
		}
	}
}
