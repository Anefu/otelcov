package k8sattributes

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Anefu/otelcov/internal/kube"
	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/nodes/nodetest"
)

// More upstream cases (decision 51), for the pod, namespace, node and
// container attributes: collector-contrib v0.134.0,
// processor/k8sattributesprocessor, internal/kube/client_test.go and
// processor_test.go, and internal/common/docker/images_test.go. Upstream's
// client tests check the client's cache entries (a pod's attributes before
// the processor applies them); each case here is the processor's output for
// the same pod, so notes say where the two differ.
//
// Copyright The OpenTelemetry Authors (the cases), SPDX-License-Identifier:
// Apache-2.0.

var created = time.Date(2026, 10, 23, 9, 30, 0, 0, time.UTC)

// extractionPod is TestExtractionRules's pod.
func extractionPod() corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "auth-service-abc12-xyz3",
			UID:               "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			Namespace:         "ns1",
			CreationTimestamp: metav1.NewTime(created),
			Labels:            map[string]string{"label1": "lv1", "label2": "k1=v1 k5=v5 extra!"},
			Annotations:       map[string]string{"annotation1": "av1"},
			OwnerReferences: []metav1.OwnerReference{
				{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "auth-service-66f5996c7c", UID: "207ea729-c779-401d-8347-008ecbc137e3"},
				{APIVersion: "apps/v1", Kind: "DaemonSet", Name: "auth-daemonset", UID: "c94d3814-2253-427a-ab13-2cf609e4dafa"},
				{APIVersion: "batch/v1", Kind: "Job", Name: "auth-cronjob-27667920", UID: "59f27ac1-5c71-42e5-abe9-2c499d603706"},
				{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "pi-statefulset", UID: "03755eb1-6175-47d5-afd5-05cfc30244d7"},
			},
		},
		Spec:   corev1.PodSpec{NodeName: "node1", Hostname: "host1"},
		Status: corev1.PodStatus{PodIP: "1.1.1.1"},
	}
}

// containersPod is Test_extractPodContainersAttributes's pod.
func containersPod() corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "test-namespace", UID: "test-pod-uid"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "container1", Image: "example.com:5000/test/image1:0.1.0"},
				{Name: "container2", Image: "example.com:81/image2@sha256:430ac608abaa332de4ce45d68534447c7a206edc5e98aaff9923ecc12f8a80d9"},
				{Name: "container3", Image: "example-website.com/image3:1.0@sha256:4b0b1b6f6cdd3e5b9e55f74a1e8d19ed93a3f5a04c6b6c3c57c4e6d19f6b7c4d"},
			},
			InitContainers: []corev1.Container{{Name: "init_container", Image: "test/init-image"}},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "container1", ContainerID: "docker://container1-id-123", ImageID: "docker.io/otel/collector@sha256:55d008bc28344c3178645d40e7d07df30f9d90abe4b53c3fc4e5e9c0295533da"},
				{Name: "container2", ContainerID: "docker://container2-id-456", ImageID: "sha256:4b0b1b6f6cdd3e5b9e55f74a1e8d19ed93a3f5a04c6b6c3c57c4e6d19f6b7c4d", RestartCount: 2},
				{Name: "container3", ContainerID: "docker://container3-id-abc", ImageID: "docker.io/otel/collector:2.0.0@sha256:430ac608abaa332de4ce45d68534447c7a206edc5e98aaff9923ecc12f8a80d9", RestartCount: 2},
			},
			InitContainerStatuses: []corev1.ContainerStatus{
				{Name: "init_container", ContainerID: "containerd://init-container-id-789", ImageID: "ghcr.io/initimage1@sha256:42e8ba40f9f70d604684c3a2a0ed321206b7e2e3509fdb2c8836d34f2edfb57b"},
			},
		},
	}
}

func TestUpstreamAttributeCases(t *testing.T) {
	const (
		extraction = "internal/kube/client_test.go TestExtractionRules/"
		namespaces = "internal/kube/client_test.go TestNamespaceExtractionRules/"
		nodes      = "internal/kube/client_test.go TestNodeExtractionRules/"
		containers = "internal/kube/client_test.go Test_extractPodContainersAttributes/"
	)
	serviceRules := map[string]any{
		"metadata":         []any{"service.namespace", "service.name", "service.version", "service.instance.id"},
		"otel_annotations": true,
	}
	withLabels := func(p corev1.Pod, labels, annotations map[string]string) corev1.Pod {
		for k, v := range labels {
			p.Labels[k] = v
		}
		for k, v := range annotations {
			p.Annotations[k] = v
		}
		return p
	}
	ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns1", UID: "ns-uid",
		Labels: map[string]string{"label1": "lv1"}, Annotations: map[string]string{"annotation1": "av1"}}}
	node1 := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1", UID: "asdfasdf-asdfasdf-asdf",
		Labels: map[string]string{"label1": "lv1"}, Annotations: map[string]string{"annotation1": "av1"}}}

	for _, tc := range []struct {
		upstream string
		note     string
		pod      corev1.Pod
		objs     kube.Objects // namespaces and nodes
		extract  map[string]any
		resource map[string]string // besides k8s.pod.uid, which associates
		want     map[string]string
		absent   []string
	}{
		{upstream: extraction + "replicasetId", pod: extractionPod(),
			extract: map[string]any{"metadata": []any{"k8s.replicaset.uid"}},
			want:    map[string]string{"k8s.replicaset.uid": "207ea729-c779-401d-8347-008ecbc137e3"}},
		{upstream: extraction + "replicasetName", pod: extractionPod(),
			extract: map[string]any{"metadata": []any{"k8s.replicaset.name"}},
			want:    map[string]string{"k8s.replicaset.name": "auth-service-66f5996c7c"}},
		{upstream: extraction + "daemonsetUID", pod: extractionPod(),
			extract: map[string]any{"metadata": []any{"k8s.daemonset.uid"}},
			want:    map[string]string{"k8s.daemonset.uid": "c94d3814-2253-427a-ab13-2cf609e4dafa"}},
		{upstream: extraction + "daemonsetName", pod: extractionPod(),
			extract: map[string]any{"metadata": []any{"k8s.daemonset.name"}},
			want:    map[string]string{"k8s.daemonset.name": "auth-daemonset"}},
		{upstream: extraction + "jobUID", pod: extractionPod(),
			extract: map[string]any{"metadata": []any{"k8s.job.uid"}},
			want:    map[string]string{"k8s.job.uid": "59f27ac1-5c71-42e5-abe9-2c499d603706"}},
		{upstream: extraction + "jobName", pod: extractionPod(),
			extract: map[string]any{"metadata": []any{"k8s.job.name"}},
			want:    map[string]string{"k8s.job.name": "auth-cronjob-27667920"}},
		{upstream: extraction + "cronJob", pod: extractionPod(),
			extract: map[string]any{"metadata": []any{"k8s.cronjob.name"}},
			want:    map[string]string{"k8s.cronjob.name": "auth-cronjob"}},
		{upstream: extraction + "statefulsetUID", pod: extractionPod(),
			extract: map[string]any{"metadata": []any{"k8s.statefulset.uid"}},
			want:    map[string]string{"k8s.statefulset.uid": "03755eb1-6175-47d5-afd5-05cfc30244d7"}},
		{upstream: extraction + "jobName (the StatefulSet case)", pod: extractionPod(),
			extract: map[string]any{"metadata": []any{"k8s.statefulset.name"}},
			want:    map[string]string{"k8s.statefulset.name": "pi-statefulset"}},
		{upstream: extraction + "deployment",
			note: "upstream reads the Deployment from the ReplicaSet object; otelcov derives its name from the pod-template-hash label, " +
				"so the case adds the label, and the Deployment's UID is unverified",
			pod:     withLabels(extractionPod(), map[string]string{"pod-template-hash": "66f5996c7c"}, nil),
			extract: map[string]any{"metadata": []any{"k8s.deployment.name"}},
			want:    map[string]string{"k8s.deployment.name": "auth-service"}},
		{upstream: extraction + "metadata",
			note:     "the processor formats the start time in its time zone; otelcov assumes UTC",
			pod:      extractionPod(),
			extract:  map[string]any{"metadata": []any{"k8s.namespace.name", "k8s.pod.name", "k8s.pod.uid", "k8s.pod.hostname", "k8s.pod.ip", "k8s.node.name", "k8s.pod.start_time"}},
			resource: map[string]string{},
			want: map[string]string{
				"k8s.namespace.name": "ns1", "k8s.node.name": "node1", "k8s.pod.name": "auth-service-abc12-xyz3",
				"k8s.pod.hostname": "host1", "k8s.pod.uid": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "k8s.pod.ip": "1.1.1.1",
				"k8s.pod.start_time": "2026-10-23T09:30:00Z",
			}},
		{upstream: extraction + "labels-annotations-default-pod",
			note: "upstream's case also extracts part of a label with a value regex, which the 0.134 config can't set",
			pod:  extractionPod(),
			extract: map[string]any{"metadata": []any{},
				"labels":      []any{map[string]any{"tag_name": "l1", "key": "label1"}},
				"annotations": []any{map[string]any{"tag_name": "a1", "key": "annotation1"}}},
			want: map[string]string{"l1": "lv1", "a1": "av1"}},
		{upstream: extraction + "all-labels", pod: extractionPod(),
			extract: map[string]any{"metadata": []any{}, "labels": []any{map[string]any{"key_regex": "la.*", "from": "pod"}}},
			want:    map[string]string{"k8s.pod.labels.label1": "lv1", "k8s.pod.labels.label2": "k1=v1 k5=v5 extra!"}},
		{upstream: extraction + "all-annotations", pod: extractionPod(),
			extract: map[string]any{"metadata": []any{}, "annotations": []any{map[string]any{"key_regex": "an.*"}}},
			want:    map[string]string{"k8s.pod.annotations.annotation1": "av1"}},
		{upstream: extraction + "all-annotations-not-match", pod: extractionPod(),
			extract: map[string]any{"metadata": []any{}, "annotations": []any{map[string]any{"key_regex": "an*"}}},
			absent:  []string{"k8s.pod.annotations.annotation1"}},
		{upstream: extraction + "captured-groups", pod: extractionPod(),
			extract: map[string]any{"metadata": []any{}, "annotations": []any{map[string]any{"tag_name": "$1", "key_regex": `annotation(\d+)`}}},
			want:    map[string]string{"1": "av1"}},
		{upstream: extraction + "captured-groups-$0", pod: extractionPod(),
			extract: map[string]any{"metadata": []any{}, "annotations": []any{map[string]any{"tag_name": "prefix-$0", "key_regex": `annotation(\d+)`}}},
			want:    map[string]string{"prefix-annotation1": "av1"}},
		{upstream: extraction + "service-name", pod: extractionPod(), extract: serviceRules,
			want:   map[string]string{"service.name": "pi-statefulset", "service.namespace": "ns1"},
			absent: []string{"service.version", "service.instance.id"}},
		{upstream: extraction + "service-attributes-label-values",
			pod:     withLabels(extractionPod(), map[string]string{"app.kubernetes.io/name": "label-service", "app.kubernetes.io/version": "label-version"}, nil),
			extract: serviceRules,
			want:    map[string]string{"service.name": "label-service", "service.version": "label-version", "service.namespace": "ns1"}},
		{upstream: extraction + "service-attributes-label-values-instance",
			pod: withLabels(extractionPod(), map[string]string{
				"app.kubernetes.io/instance": "instance-service", "app.kubernetes.io/name": "label-service", "app.kubernetes.io/version": "label-version"}, nil),
			extract: serviceRules,
			want:    map[string]string{"service.name": "instance-service", "service.version": "label-version", "service.namespace": "ns1"}},
		{upstream: extraction + "service-attributes-annotation-override",
			note: "the client's entry has service.namespace from the annotation; the processor then always sets service.namespace to the namespace " +
				"(processor.go, processResource)",
			pod: withLabels(extractionPod(), nil, map[string]string{
				"resource.opentelemetry.io/service.instance.id": "annotation-id", "resource.opentelemetry.io/service.version": "annotation-version",
				"resource.opentelemetry.io/service.name": "annotation-service", "resource.opentelemetry.io/service.namespace": "annotation-namespace"}),
			extract: serviceRules,
			want: map[string]string{"service.instance.id": "annotation-id", "service.name": "annotation-service",
				"service.version": "annotation-version", "service.namespace": "ns1"}},

		{upstream: namespaces + "labels + processor_test.go TestAddNamespaceLabels", pod: extractionPod(),
			objs: kube.Objects{Namespaces: []corev1.Namespace{ns}},
			extract: map[string]any{"metadata": []any{},
				"labels":      []any{map[string]any{"tag_name": "l1", "key": "label1", "from": "namespace"}},
				"annotations": []any{map[string]any{"tag_name": "a1", "key": "annotation1", "from": "namespace"}}},
			want: map[string]string{"l1": "lv1", "a1": "av1"}},
		{upstream: namespaces + "all-labels", pod: extractionPod(), objs: kube.Objects{Namespaces: []corev1.Namespace{ns}},
			extract: map[string]any{"metadata": []any{}, "labels": []any{map[string]any{"key_regex": "la.*", "from": "namespace"}}},
			want:    map[string]string{"k8s.namespace.labels.label1": "lv1"}},
		{upstream: namespaces + "all-annotations", pod: extractionPod(), objs: kube.Objects{Namespaces: []corev1.Namespace{ns}},
			extract: map[string]any{"metadata": []any{}, "annotations": []any{map[string]any{"key_regex": "an.*", "from": "namespace"}}},
			want:    map[string]string{"k8s.namespace.annotations.annotation1": "av1"}},
		{upstream: nodes + "labels + processor_test.go TestAddNodeLabels",
			note: "upstream's processor test hands the processor a cached pod with its node name; the real processor only keeps a pod's node name " +
				"when k8s.node.name is extracted (removeUnnecessaryPodData), so the case extracts it",
			pod:  extractionPod(),
			objs: kube.Objects{Nodes: []corev1.Node{node1}},
			extract: map[string]any{"metadata": []any{"k8s.node.name"},
				"labels":      []any{map[string]any{"tag_name": "l1", "key": "label1", "from": "node"}},
				"annotations": []any{map[string]any{"tag_name": "a1", "key": "annotation1", "from": "node"}}},
			want: map[string]string{"l1": "lv1", "a1": "av1"}},
		{upstream: nodes + "all-labels", note: "with k8s.node.name, as above; the node's UID comes along",
			pod: extractionPod(), objs: kube.Objects{Nodes: []corev1.Node{node1}},
			extract: map[string]any{"metadata": []any{"k8s.node.name"}, "labels": []any{map[string]any{"key_regex": "la.*", "from": "node"}}},
			want:    map[string]string{"k8s.node.labels.label1": "lv1", "k8s.node.uid": "asdfasdf-asdfasdf-asdf"}},
		{upstream: "processor_test.go TestAddNodeUID", note: "with k8s.node.name, as for TestAddNodeLabels",
			pod: extractionPod(), objs: kube.Objects{Nodes: []corev1.Node{node1}},
			extract: map[string]any{"metadata": []any{"k8s.node.name", "k8s.node.uid"}},
			want:    map[string]string{"k8s.node.uid": "asdfasdf-asdfasdf-asdf"}},
	} {
		t.Run(tc.upstream, func(t *testing.T) {
			if tc.note != "" {
				t.Log(tc.note)
			}
			objs := tc.objs
			objs.Pods = []corev1.Pod{tc.pod}
			cfg := map[string]any{
				"pod_association": []any{map[string]any{"sources": []any{map[string]any{"from": "resource_attribute", "name": "k8s.pod.uid"}}}},
				"extract":         tc.extract,
			}
			n, err := New("collector/k8sattributes", cfg, objs)
			if err != nil {
				t.Fatal(err)
			}
			res := map[string]string{"k8s.pod.uid": string(tc.pod.UID)}
			for k, v := range tc.resource {
				res[k] = v
			}
			got := nodetest.Attrs(nodetest.Run(t, n, nodetest.Item("s1", res)).Items[0])
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("%s = %q, want %q (all: %v)", k, got[k], v, got)
				}
			}
			for _, k := range tc.absent {
				if v, ok := got[k]; ok {
					t.Errorf("%s = %q, want it absent", k, v)
				}
			}
		})
	}

	t.Run(containers+"all-container-attributes and automatic-container-level-attributes", func(t *testing.T) {
		pod := containersPod()
		n, err := New("collector/k8sattributes", map[string]any{
			"pod_association": []any{map[string]any{"sources": []any{map[string]any{"from": "resource_attribute", "name": "k8s.pod.uid"}}}},
			"extract": map[string]any{"metadata": []any{"container.image.name", "container.image.tag", "container.id",
				"container.image.repo_digests", "service.version", "service.instance.id"}},
		}, kube.Objects{Pods: []corev1.Pod{pod}})
		if err != nil {
			t.Fatal(err)
		}
		for name, want := range map[string]map[string]string{
			"container1": {"container.image.name": "example.com:5000/test/image1", "container.image.tag": "0.1.0", "container.id": "container1-id-123",
				"container.image.repo_digests": `["docker.io/otel/collector@sha256:55d008bc28344c3178645d40e7d07df30f9d90abe4b53c3fc4e5e9c0295533da"]`,
				"service.version":              "0.1.0", "service.instance.id": "test-namespace.test-pod.container1"},
			"container2": {"container.image.name": "example.com:81/image2", "container.image.tag": "latest", "container.id": "container2-id-456",
				"service.version":     "sha256:430ac608abaa332de4ce45d68534447c7a206edc5e98aaff9923ecc12f8a80d9",
				"service.instance.id": "test-namespace.test-pod.container2"},
			"container3": {"container.image.name": "example-website.com/image3", "container.image.tag": "1.0", "container.id": "container3-id-abc",
				"container.image.repo_digests": `["docker.io/otel/collector:2.0.0@sha256:430ac608abaa332de4ce45d68534447c7a206edc5e98aaff9923ecc12f8a80d9"]`,
				"service.version":              "1.0@sha256:4b0b1b6f6cdd3e5b9e55f74a1e8d19ed93a3f5a04c6b6c3c57c4e6d19f6b7c4d",
				"service.instance.id":          "test-namespace.test-pod.container3"},
			"init_container": {"container.image.name": "test/init-image", "container.image.tag": "latest", "container.id": "init-container-id-789",
				"container.image.repo_digests": `["ghcr.io/initimage1@sha256:42e8ba40f9f70d604684c3a2a0ed321206b7e2e3509fdb2c8836d34f2edfb57b"]`,
				"service.instance.id":          "test-namespace.test-pod.init_container"},
		} {
			out := nodetest.Run(t, n, nodetest.Item("s1", map[string]string{"k8s.pod.uid": "test-pod-uid", "k8s.container.name": name}))
			got := nodetest.Attrs(out.Items[0])
			for k, v := range want {
				if got[k] != v {
					t.Errorf("%s: %s = %q, want %q", name, k, got[k], v)
				}
			}
			for _, k := range []string{"container.image.repo_digests", "service.version"} {
				if _, ok := want[k]; !ok {
					if v, present := got[k]; present {
						t.Errorf("%s: %s = %q, want it absent", name, k, v)
					}
				}
			}
		}
	})
}

func TestUpstreamImageCases(t *testing.T) {
	// internal/common/docker/images_test.go TestDockerImageToElements.
	for image, want := range map[string]struct{ repo, tag string }{
		"alpine":                              {"alpine", "latest"},
		"alpine:v1.0.0":                       {"alpine", "v1.0.0"},
		"alpine/alpine":                       {"alpine/alpine", "latest"},
		"alpine/alpine:2.0.0":                 {"alpine/alpine", "2.0.0"},
		"example.com/alpine/alpine":           {"example.com/alpine/alpine", "latest"},
		"example.com/alpine/alpine:1":         {"example.com/alpine/alpine", "1"},
		"example.com:3000/alpine/alpine":      {"example.com:3000/alpine/alpine", "latest"},
		"example.com:3000/alpine/alpine:test": {"example.com:3000/alpine/alpine", "test"},
		"alpine:test@sha256:dbc66f8c46d4cf4793527ca0737d73527a2bb830019953c2371b5f45f515f1a8":                     {"alpine", "test"},
		"example.com/alpine/alpine@sha256:dbc66f8c46d4cf4793527ca0737d73527a2bb830019953c2371b5f45f515f1a8":       {"example.com/alpine/alpine", "latest"},
		"example.com/alpine/alpine:1.0.0@sha256:dbc66f8c46d4cf4793527ca0737d73527a2bb830019953c2371b5f45f515f1a8": {"example.com/alpine/alpine", "1.0.0"},
	} {
		got, err := parseImageName(image)
		if err != nil || got.repository != want.repo || got.tag != want.tag {
			t.Errorf("parseImageName(%q) = %+v, %v; want %+v", image, got, err, want)
		}
	}
	for _, image := range []string{"", "aaa:"} {
		if _, err := parseImageName(image); err == nil {
			t.Errorf("parseImageName(%q) should fail", image)
		}
	}
	// internal/kube/client_test.go Test_parseServiceVersionFromImage.
	for image, want := range map[string]string{
		"img":   "",
		"img:1": "1",
		"img:1@sha256:232a180dbcbcfa7250917507f3827d88a9ae89bb1cdd8fe3ac4db7b764ebb25a": "1@sha256:232a180dbcbcfa7250917507f3827d88a9ae89bb1cdd8fe3ac4db7b764ebb25a",
		"img@sha256:232a180dbcbcfa7250917507f3827d88a9ae89bb1cdd8fe3ac4db7b764ebb25a":   "sha256:232a180dbcbcfa7250917507f3827d88a9ae89bb1cdd8fe3ac4db7b764ebb25a",
		"registry.io/img":      "",
		"registry.io:8080/img": "",
		"img:latest":           "latest",
	} {
		if got, _ := serviceVersionFromImage(image); got != want {
			t.Errorf("serviceVersionFromImage(%q) = %q, want %q", image, got, want)
		}
	}
}

// Namespace and node attributes come even without a pod, from the
// resource's k8s.namespace.name and k8s.node.name; a namespace or node
// missing from the cluster file is unverified, not guessed.
func TestNamespaceAndNodeWithoutPod(t *testing.T) {
	ns := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "inference", Labels: map[string]string{"team": "ml"}}}
	cfg := map[string]any{"extract": map[string]any{
		"metadata": []any{"service.namespace", "k8s.node.uid"},
		"labels": []any{
			map[string]any{"tag_name": "team", "key": "team", "from": "namespace"},
			map[string]any{"tag_name": "zone", "key": "topology.kubernetes.io/zone", "from": "node"},
		},
	}}
	n, err := New("collector/k8sattributes", cfg, kube.Objects{Namespaces: []corev1.Namespace{ns}})
	if err != nil {
		t.Fatal(err)
	}
	out := nodetest.Run(t, n, nodetest.Item("s1", map[string]string{
		"k8s.pod.ip": "10.9.9.9", "k8s.namespace.name": "inference", "k8s.node.name": "kind-worker", "service.namespace": "other",
	}))
	got := nodetest.Attrs(out.Items[0])
	if got["team"] != "ml" || got["service.namespace"] != "inference" {
		t.Errorf("attrs = %v", got)
	}
	if e, ok := nodetest.Find(out.Events, "service.namespace", lineage.Set); !ok || e.Reason != "the processor always sets `service.namespace` to the namespace, replacing `other`" {
		t.Errorf("service.namespace event: %+v", out.Events)
	}
	for _, attr := range []string{"zone", "k8s.node.uid"} {
		if e, ok := nodetest.Find(out.Events, attr, lineage.Unverified); !ok || e.Reason != "the cluster file has no Node objects, so node `kind-worker`'s labels, annotations and UID aren't known" {
			t.Errorf("%s should be unverified: %+v", attr, out.Events)
		}
	}

	// A label the namespace doesn't have is a non-event the diagnosis can name.
	n, err = New("collector/k8sattributes", map[string]any{"extract": map[string]any{"metadata": []any{},
		"labels": []any{map[string]any{"tag_name": "owner", "key": "owner", "from": "namespace"}}}},
		kube.Objects{Namespaces: []corev1.Namespace{ns}})
	if err != nil {
		t.Fatal(err)
	}
	out = nodetest.Run(t, n, nodetest.Item("s2", map[string]string{"k8s.namespace.name": "inference"}))
	if e, ok := nodetest.Find(out.Events, "owner", lineage.Skipped); !ok || e.Reason != "Namespace `inference`'s labels don't include `owner`" {
		t.Errorf("owner should be skipped: %+v", out.Events)
	}
}

func TestConfigErrors(t *testing.T) {
	for name, extract := range map[string]map[string]any{
		"unsupported metadata": {"metadata": []any{"k8s.pod.colour"}},
		"key and key_regex":    {"labels": []any{map[string]any{"key": "a", "key_regex": "b"}}},
		"bad from":             {"labels": []any{map[string]any{"key": "a", "from": "cluster"}}},
		"deployment objects":   {"labels": []any{map[string]any{"key": "a", "from": "deployment"}}},
		"bad regex":            {"annotations": []any{map[string]any{"key_regex": "("}}},
	} {
		if _, err := New("x", map[string]any{"extract": extract}, kube.Objects{}); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// Cases from a review of the model against the 0.134 source (processor.go,
// client.go), beyond upstream's own tests.
func TestProcessorOrder(t *testing.T) {
	assoc := []any{map[string]any{"sources": []any{map[string]any{"from": "resource_attribute", "name": "k8s.pod.ip"}}}}
	two := nodetest.Pod("web-1", "10.0.0.1", "u1", nodetest.Container("app"), nodetest.Container("sidecar"))
	run := func(t *testing.T, pod corev1.Pod, objs kube.Objects, extract map[string]any, res map[string]string) (map[string]string, []lineage.Event) {
		t.Helper()
		objs.Pods = []corev1.Pod{pod}
		n, err := New("collector/k8sattributes", map[string]any{"pod_association": assoc, "extract": extract}, objs)
		if err != nil {
			t.Fatal(err)
		}
		if res == nil {
			res = map[string]string{}
		}
		res["k8s.pod.ip"] = pod.Status.PodIP
		out := nodetest.Run(t, n, nodetest.Item("s1", res))
		return nodetest.Attrs(out.Items[0]), out.Events
	}

	// The processor identifies the container after the pod's attributes are
	// on the resource, so a resource.opentelemetry.io/k8s.container.name
	// annotation names the container in a multi-container pod.
	t.Run("container named by the pod's annotation", func(t *testing.T) {
		pod := two
		pod.Annotations = map[string]string{"resource.opentelemetry.io/k8s.container.name": "app"}
		got, _ := run(t, pod, kube.Objects{}, map[string]any{"metadata": []any{"k8s.container.name", "container.id"}, "otel_annotations": true}, nil)
		if got["k8s.container.name"] != "app" || got["container.id"] != "id-web-1-app" {
			t.Errorf("attrs = %v", got)
		}
	})
	t.Run("the pod's attributes name a container the pod doesn't have", func(t *testing.T) {
		pod := nodetest.Pod("web-1", "10.0.0.1", "u1", nodetest.Container("app"))
		pod.Labels = map[string]string{"container": "worker"}
		got, events := run(t, pod, kube.Objects{}, map[string]any{"metadata": []any{"container.id"},
			"labels": []any{map[string]any{"tag_name": "k8s.container.name", "key": "container"}}}, nil)
		if _, ok := got["container.id"]; ok {
			t.Errorf("container.id should not be set: %v", got)
		}
		if _, ok := nodetest.Find(events, "container.id", lineage.Skipped); !ok {
			t.Errorf("want container.id skipped: %+v", events)
		}
	})

	node1 := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1", UID: "node-uid", Labels: map[string]string{"zone": "eu-1", "id": "label-id"}}}
	nodes := kube.Objects{Nodes: []corev1.Node{node1}}
	// Without k8s.node.name extracted, the processor drops the pod's node
	// name and only the resource's counts.
	t.Run("node from the resource only", func(t *testing.T) {
		extract := map[string]any{"metadata": []any{}, "labels": []any{map[string]any{"tag_name": "zone", "key": "zone", "from": "node"}}}
		got, _ := run(t, nodetest.Pod("p", "10.0.0.1", "u1", nodetest.Container("app")), nodes, extract, nil)
		if _, ok := got["zone"]; ok {
			t.Errorf("zone should not be set without a node name: %v", got)
		}
		got, _ = run(t, nodetest.Pod("p", "10.0.0.1", "u1", nodetest.Container("app")), nodes, extract, map[string]string{"k8s.node.name": "node-1"})
		if got["zone"] != "eu-1" || got["k8s.node.uid"] != "node-uid" {
			t.Errorf("want zone and the node's UID from the resource's node: %v", got)
		}
	})
	// The node's UID comes whenever the processor watches nodes, after the
	// node's labels, so a label written to k8s.node.uid wins.
	t.Run("node UID after node labels", func(t *testing.T) {
		got, _ := run(t, nodetest.Pod("p", "10.0.0.1", "u1", nodetest.Container("app")), nodes, map[string]any{"metadata": []any{"k8s.node.name"},
			"labels": []any{map[string]any{"tag_name": "k8s.node.uid", "key": "id", "from": "node"}}}, nil)
		if got["k8s.node.uid"] != "label-id" {
			t.Errorf("k8s.node.uid = %q, want the label's value", got["k8s.node.uid"])
		}
	})

	t.Run("ignored pods", func(t *testing.T) {
		jaeger := nodetest.Pod("my-jaeger-collector-5f7d", "10.0.0.1", "u1", nodetest.Container("app"))
		got, events := run(t, jaeger, kube.Objects{}, map[string]any{"metadata": []any{"k8s.pod.name"}}, nil)
		if _, ok := got["k8s.pod.name"]; ok {
			t.Errorf("an excluded pod gets no attributes: %v", got)
		}
		if e, ok := nodetest.Find(events, "", lineage.Skipped); !ok || !strings.Contains(e.Reason, "exclude pattern `jaeger-collector`") {
			t.Errorf("want skipped for the default exclude: %+v", events)
		}
		annotated := nodetest.Pod("p", "10.0.0.1", "u1", nodetest.Container("app"))
		annotated.Annotations = map[string]string{"opentelemetry.io/k8s-processor/ignore": " True "}
		// The annotation only counts when the processor keeps annotations.
		if got, _ := run(t, annotated, kube.Objects{}, map[string]any{"metadata": []any{"k8s.pod.name"}}, nil); got["k8s.pod.name"] != "p" {
			t.Errorf("without annotation rules the annotation is stripped: %v", got)
		}
		if got, _ := run(t, annotated, kube.Objects{}, map[string]any{"metadata": []any{"k8s.pod.name"}, "otel_annotations": true}, nil); got["k8s.pod.name"] != "" {
			t.Errorf("with annotation rules the pod is ignored: %v", got)
		}
	})

	t.Run("regex rule keys that become the same attribute", func(t *testing.T) {
		pod := nodetest.Pod("p", "10.0.0.1", "u1", nodetest.Container("app"))
		pod.Labels = map[string]string{"a.example/team": "x", "b.example/team": "y"}
		_, events := run(t, pod, kube.Objects{}, map[string]any{"metadata": []any{},
			"labels": []any{map[string]any{"tag_name": "$1", "key_regex": `.*/(team)`}}}, nil)
		if _, ok := nodetest.Find(events, "team", lineage.Unverified); !ok {
			t.Errorf("want team unverified: %+v", events)
		}
	})
}

func TestMoreConfigErrors(t *testing.T) {
	for name, cfg := range map[string]map[string]any{
		"auth_type":           {"auth_type": "token"},
		"five sources":        {"pod_association": []any{map[string]any{"sources": []any{source(), source(), source(), source(), source()}}}},
		"bad exclude":         {"exclude": map[string]any{"pods": []any{map[string]any{"name": "("}}}},
		"unknown source from": {"pod_association": []any{map[string]any{"sources": []any{map[string]any{"from": "label", "name": "x"}}}}},
	} {
		if _, err := New("x", cfg, kube.Objects{}); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, err := New("x", map[string]any{"wait_for_metadata": true, "wait_for_metadata_timeout": "10s", "auth_type": "kubeConfig", "context": "kind"}, kube.Objects{}); err != nil {
		t.Errorf("settings that don't change output should be accepted: %v", err)
	}
}

func source() map[string]any {
	return map[string]any{"from": "resource_attribute", "name": "k8s.pod.uid"}
}
