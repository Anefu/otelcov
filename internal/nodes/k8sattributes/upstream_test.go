package k8sattributes

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/Anefu/otelcov/internal/nodes/nodetest"
)

// Upstream cases, for parity with the real processor (decision 51).
//
// Each case comes from collector-contrib v0.134.0,
// processor/k8sattributesprocessor, and names its source as
// "<file> <test>/<case>". Upstream tests the processor with pre-built cache
// entries (processor_test.go) and, separately, how a pod becomes those
// entries (internal/kube/client_test.go); otelcov's model goes from the pod
// straight to attributes, so each case is translated to the pod that would
// produce its cache entries. Upstream cases also check image and service
// attributes, which otelcov doesn't model (they're unverified); those parts
// are left out.
//
// Copyright The OpenTelemetry Authors (the cases), SPDX-License-Identifier:
// Apache-2.0.

type status struct {
	name, id string
	restart  int32
	init     bool
}

func upstreamPod(containers, initContainers []string, statuses ...status) corev1.Pod {
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "ns", UID: k8stypes.UID("19f651bc-73e4-410f-b3e9-f0241679d3b8")},
		Status:     corev1.PodStatus{PodIP: "1.1.1.1"},
	}
	for _, c := range containers {
		p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: c})
	}
	for _, c := range initContainers {
		p.Spec.InitContainers = append(p.Spec.InitContainers, corev1.Container{Name: c})
	}
	for _, s := range statuses {
		cs := corev1.ContainerStatus{Name: s.name, ContainerID: s.id, RestartCount: s.restart}
		if s.init {
			p.Status.InitContainerStatuses = append(p.Status.InitContainerStatuses, cs)
		} else {
			p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, cs)
		}
	}
	return p
}

func TestUpstreamCases(t *testing.T) {
	const (
		processorTest = "processor_test.go TestProcessorAddContainerAttributes/"
		clientTest    = "internal/kube/client_test.go Test_extractPodContainersAttributes"
		setAttrTest   = "processor_test.go Test_setResourceAttribute/"
		id767         = "767dc30d4fece77038e8ec2585a33471944d0b754659af7aa7e101181418f0dd"
		id6a7         = "6a7f1a598b5dafec9c193f8f8d63f6e5839b8b0acd2fe780f94285e26c05580e"
		id5ba         = "5ba4e0e5a5eb1f37bc6e7fc76495914400a3ee309d8828d16407e4b3d5410848"
		idFcd         = "fcd58c97330c1dc6615bd520031f6a703a7317cd92adc96013c4dd57daad0b5f"
	)
	for _, tc := range []struct {
		upstream string
		note     string // how the case was translated, when it isn't direct
		pod      corev1.Pod
		resource map[string]string
		metadata []string
		want     map[string]string
		absent   []string
	}{
		{
			upstream: processorTest + "all-by-name",
			pod:      upstreamPod([]string{"app"}, nil, status{name: "app", id: "containerd://" + id767}),
			resource: map[string]string{"k8s.container.name": "app"},
			metadata: []string{"k8s.container.name"},
			want:     map[string]string{"k8s.container.name": "app"},
		},
		{
			upstream: processorTest + "all-by-id",
			pod:      upstreamPod([]string{"app"}, nil, status{name: "app", id: "containerd://" + id767}),
			resource: map[string]string{"container.id": id767},
			metadata: []string{"k8s.container.name", "container.id"},
			want:     map[string]string{"k8s.container.name": "app", "container.id": id767},
		},
		{
			upstream: processorTest + "container-id-with-runid",
			note:     "upstream's cache holds the container's runs 0, 1 and 2; a pod object only has the current one, here run 1",
			pod:      upstreamPod([]string{"app"}, nil, status{name: "app", id: "containerd://" + id6a7, restart: 1}),
			resource: map[string]string{"k8s.container.name": "app", "k8s.container.restart_count": "1"},
			metadata: []string{"container.id"},
			want:     map[string]string{"container.id": id6a7},
		},
		{
			upstream: processorTest + "container-id-latest",
			note:     "the latest run is the pod's current status",
			pod:      upstreamPod([]string{"app"}, nil, status{name: "app", id: "containerd://" + id5ba, restart: 2}),
			resource: map[string]string{"k8s.container.name": "app"},
			metadata: []string{"container.id"},
			want:     map[string]string{"container.id": id5ba},
		},
		{
			upstream: processorTest + "container-name-mismatch",
			pod:      upstreamPod([]string{"app"}, nil, status{name: "app", id: "containerd://" + idFcd}),
			resource: map[string]string{"k8s.container.name": "new-app", "k8s.container.restart_count": "0"},
			metadata: []string{"k8s.container.name", "container.id"},
			want:     map[string]string{"k8s.container.name": "new-app"},
			absent:   []string{"container.id"},
		},
		{
			upstream: processorTest + "container-run-id-mismatch",
			pod:      upstreamPod([]string{"app"}, nil, status{name: "app", id: "containerd://" + idFcd}),
			resource: map[string]string{"k8s.container.name": "app", "k8s.container.restart_count": "1"},
			metadata: []string{"container.id"},
			absent:   []string{"container.id"},
		},
		{
			upstream: processorTest + "fall back to only container",
			pod:      upstreamPod([]string{"app"}, nil, status{name: "app", id: "containerd://" + idFcd}),
			metadata: []string{"k8s.container.name"},
			want:     map[string]string{"k8s.container.name": "app"},
		},
		{
			upstream: processorTest + "multiple containers in the pod - do not fall back to any container",
			pod: upstreamPod([]string{"app", "app2"}, nil,
				status{name: "app", id: "containerd://a"}, status{name: "app2", id: "containerd://b"}),
			metadata: []string{"k8s.container.name"},
			absent:   []string{"k8s.container.name"},
		},
		{
			upstream: clientTest + " + " + processorTest + "fall back to only container",
			note:     "the client puts init containers in the same lookup the fallback counts, so one container plus one init container is two",
			pod: upstreamPod([]string{"vllm"}, []string{"istio-init"},
				status{name: "vllm", id: "containerd://a"}, status{name: "istio-init", id: "containerd://b", init: true}),
			metadata: []string{"k8s.container.name"},
			absent:   []string{"k8s.container.name"},
		},
		{
			upstream: clientTest,
			note:     "init_container is in the lookup by ID",
			pod: upstreamPod([]string{"container1"}, []string{"init_container"},
				status{name: "container1", id: "docker://container1-id-123"},
				status{name: "init_container", id: "containerd://init-container-id-789", init: true}),
			resource: map[string]string{"container.id": "init-container-id-789"},
			metadata: []string{"k8s.container.name"},
			want:     map[string]string{"k8s.container.name": "init_container"},
		},
		{
			upstream: clientTest,
			note:     "the runtime prefix is removed from the pod status's IDs",
			pod: upstreamPod([]string{"container1", "container2"}, nil,
				status{name: "container1", id: "docker://container1-id-123"}, status{name: "container2", id: "docker://container2-id-456"}),
			resource: map[string]string{"container.id": "container2-id-456"},
			metadata: []string{"k8s.container.name"},
			want:     map[string]string{"k8s.container.name": "container2"},
		},
		{
			upstream: clientTest + " + " + processorTest + "all-by-id",
			note:     "the resource's container.id is compared as given, so a prefixed one matches nothing",
			pod: upstreamPod([]string{"container1", "container2"}, nil,
				status{name: "container1", id: "docker://container1-id-123"}, status{name: "container2", id: "docker://container2-id-456"}),
			resource: map[string]string{"container.id": "docker://container2-id-456"},
			metadata: []string{"k8s.container.name"},
			absent:   []string{"k8s.container.name"},
		},
		{
			upstream: setAttrTest + "attribute present with empty value - set value",
			pod:      upstreamPod([]string{"app"}, nil, status{name: "app", id: "containerd://a"}),
			resource: map[string]string{"k8s.pod.name": ""},
			metadata: []string{"k8s.pod.name"},
			want:     map[string]string{"k8s.pod.name": "test-pod"},
		},
		{
			upstream: setAttrTest + "attribute present with non-empty value - do not overwrite value",
			pod:      upstreamPod([]string{"app"}, nil, status{name: "app", id: "containerd://a"}),
			resource: map[string]string{"k8s.pod.name": "other"},
			metadata: []string{"k8s.pod.name"},
			want:     map[string]string{"k8s.pod.name": "other"},
		},
	} {
		t.Run(tc.upstream, func(t *testing.T) {
			if tc.note != "" {
				t.Log(tc.note)
			}
			var md []any
			for _, m := range tc.metadata {
				md = append(md, m)
			}
			n := node(t, map[string]any{
				"pod_association": []any{map[string]any{"sources": []any{map[string]any{"from": "resource_attribute", "name": "k8s.pod.uid"}}}},
				"extract":         map[string]any{"metadata": md},
			}, tc.pod)
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
}
