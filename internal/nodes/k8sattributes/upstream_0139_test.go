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

// Settings collector-contrib added between 0.134 and 0.139.

// Ported from collector-contrib v0.139.0,
// processor/k8sattributesprocessor/internal/kube/client_test.go,
// TestExtractDeploymentNameFromReplicaSet (Apache-2.0), through the model:
// with deployment_name_from_replicaset, the Deployment is the ReplicaSet's
// name without a trailing 10-character hash.
func TestDeploymentNameFromReplicaSet(t *testing.T) {
	cases := map[string]struct{ rs, want string }{
		"valid replicaset name with pod template hash":                 {"my-deployment-7b9f4c8d5e", "my-deployment"},
		"complex deployment name with dashes":                          {"my-complex-deployment-name-7b9f4c8d5e", "my-complex-deployment-name"},
		"single word deployment":                                       {"nginx-7b9f4c8d5e", "nginx"},
		"replicaset name without valid pod template hash":              {"my-deployment-invalidhash", ""},
		"replicaset name with short hash":                              {"my-deployment-7b9f4c", ""},
		"replicaset name with long hash":                               {"my-deployment-7b9f4c8d5e123", ""},
		"replicaset name with uppercase in hash":                       {"my-deployment-7B9F4C8D5E", ""},
		"replicaset name with special characters in hash":              {"my-deployment-7b9f4c8d5_", ""},
		"empty replicaset name":                                        {"", ""},
		"replicaset name without dashes":                               {"deployment7b9f4c8d5e", ""},
		"replicaset name with only hash part":                          {"7b9f4c8d5e", ""},
		"otelcov: the pod-template-hash label isn't needed (nor used)": {"web-7b9f4c8d5e", "web"},
	}
	for name, tc := range cases {
		pod := nodetest.Pod("p", "10.0.0.1", "u1", nodetest.Container("app"))
		pod.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: tc.rs, UID: "rs1"}}
		pod.Labels = map[string]string{"pod-template-hash": "different"}
		n, err := New("x", map[string]any{"extract": map[string]any{
			"metadata": []any{"k8s.deployment.name", "service.name"}, "deployment_name_from_replicaset": true,
		}}, kube.Objects{Pods: []corev1.Pod{pod}}, Instance{})
		if err != nil {
			t.Fatal(err)
		}
		out := nodetest.Run(t, n, nodetest.Item("s1", map[string]string{"k8s.pod.ip": "10.0.0.1"}))
		attrs := nodetest.Attrs(out.Items[0])
		if attrs["k8s.deployment.name"] != tc.want {
			t.Errorf("%s: k8s.deployment.name = %q, want %q", name, attrs["k8s.deployment.name"], tc.want)
		}
		// service.name: the Deployment, else the ReplicaSet's name, which
		// upstream sets first (even an empty one).
		want := tc.want
		if want == "" {
			want = tc.rs
		}
		if attrs["service.name"] != want {
			t.Errorf("%s: service.name = %q, want %q", name, attrs["service.name"], want)
		}
		if _, ok := nodetest.Find(out.Events, "service.name", lineage.Unverified); ok {
			t.Errorf("%s: the heuristic needs no ReplicaSet object: %+v", name, out.Events)
		}
	}
}

// k8s.cronjob.uid (contrib 0.136) comes from the Job object's owner.
func TestCronJobUID(t *testing.T) {
	pod := nodetest.Pod("p", "10.0.0.1", "u1", nodetest.Container("app"))
	other := nodetest.Pod("q", "10.0.0.2", "u2", nodetest.Container("app"))
	pod.OwnerReferences = []metav1.OwnerReference{{Kind: "Job", Name: "backup-29000000", UID: "j1"}}
	n, err := New("x", map[string]any{"extract": map[string]any{"metadata": []any{"k8s.cronjob.uid"}}},
		kube.Objects{Pods: []corev1.Pod{pod, other}}, Instance{})
	if err != nil {
		t.Fatal(err)
	}
	out := nodetest.Run(t, n, nodetest.Item("s1", map[string]string{"k8s.pod.ip": "10.0.0.1"}))
	if e, ok := nodetest.Find(out.Events, "k8s.cronjob.uid", lineage.Unverified); !ok || !strings.Contains(e.Reason, "Job object") {
		t.Errorf("a Job's pod: want unverified: %+v", out.Events)
	}
	out = nodetest.Run(t, n, nodetest.Item("s2", map[string]string{"k8s.pod.ip": "10.0.0.2"}))
	if _, ok := nodetest.Find(out.Events, "k8s.cronjob.uid", lineage.Skipped); !ok {
		t.Errorf("no Job owner: want skipped: %+v", out.Events)
	}
}

// Labels and annotations from DaemonSets and Jobs (contrib 0.135) need those
// objects: unsupported, like Deployments and StatefulSets.
func TestWorkloadSourcesUnsupported(t *testing.T) {
	for _, from := range []string{"daemonset", "job"} {
		_, err := New("x", map[string]any{"extract": map[string]any{"labels": []any{map[string]any{"key": "team", "from": from}}}}, kube.Objects{}, Instance{})
		if err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Errorf("from %s: %v", from, err)
		}
	}
}
