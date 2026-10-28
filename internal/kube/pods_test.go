package kube

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Pods from a newer cluster: fields the 1.32 types don't know are ignored and
// listed once, with list indexes collapsed.
func TestLoadPodsFromNewerCluster(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pods.yaml")
	if err := os.WriteFile(path, []byte(`
apiVersion: v1
kind: List
items:
  - apiVersion: v1
    kind: Pod
    metadata: {name: a, namespace: ns}
    spec:
      nodeName: n1
      containers: [{name: app, image: app}]
    status: {podIP: 10.0.0.1, observedGeneration: 1}
  - apiVersion: v1
    kind: Pod
    metadata: {name: b, namespace: ns}
    spec:
      futureField: true
      containers: [{name: app, image: app}]
    status: {podIP: 10.0.0.2, observedGeneration: 1}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	pods, unknown, err := LoadPods(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 2 || pods[0].Spec.NodeName != "n1" || pods[1].Status.PodIP != "10.0.0.2" {
		t.Errorf("pods not decoded: %+v", pods)
	}
	want := []string{"items[*].spec.futureField", "items[*].status.observedGeneration"}
	if !reflect.DeepEqual(unknown, want) {
		t.Errorf("unknown = %q, want %q", unknown, want)
	}
}

// Hand-written fixtures must use only known fields, so typos don't hide.
func TestFixturesHaveNoUnknownFields(t *testing.T) {
	paths, _ := filepath.Glob("../../testdata/*/k8s/*.yaml")
	if len(paths) == 0 {
		t.Fatal("no fixture pods found")
	}
	for _, p := range paths {
		if _, unknown, err := LoadPods(p); err != nil || len(unknown) > 0 {
			t.Errorf("%s: err %v, unknown fields %q", p, err, unknown)
		}
	}
}

// A List from `kubectl get pods,namespaces,nodes -o yaml` mixes kinds.
func TestLoadMixedList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	if err := os.WriteFile(path, []byte(`
apiVersion: v1
kind: List
items:
  - apiVersion: v1
    kind: Pod
    metadata: {name: a, namespace: ns}
    spec: {nodeName: n1, containers: [{name: app, image: app}]}
  - apiVersion: v1
    kind: Namespace
    metadata: {name: ns, labels: {team: search}}
  - apiVersion: v1
    kind: Node
    metadata: {name: n1, uid: node-uid, labels: {zone: eu-1}}
    status: {futureField: 1}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	o, unknown, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ns, okNS := o.Namespace("ns")
	node, okNode := o.Node("n1")
	if len(o.Pods) != 1 || !okNS || ns.Labels["team"] != "search" || !okNode || node.UID != "node-uid" {
		t.Errorf("objects not decoded: %+v", o)
	}
	if !reflect.DeepEqual(unknown, []string{"items[*].status.futureField"}) {
		t.Errorf("unknown = %q", unknown)
	}
	pods, _, err := LoadPods(path)
	if err != nil || len(pods) != 1 {
		t.Errorf("LoadPods = %d pods, %v", len(pods), err)
	}
	if err := os.WriteFile(path, []byte("kind: List\nitems: [{kind: Service, metadata: {name: s}}]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil || !strings.Contains(err.Error(), "is a Service, not a Pod, Namespace or Node") {
		t.Errorf("want an error for a Service, got %v", err)
	}
}
