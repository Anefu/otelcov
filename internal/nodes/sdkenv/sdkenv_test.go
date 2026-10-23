package sdkenv

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/nodes/nodetest"
)

func run(t *testing.T, snap, cfg []corev1.Pod, attrs map[string]string) (map[string]string, []lineage.Event) {
	t.Helper()
	it := nodetest.Item("s1", attrs)
	out := nodetest.Run(t, New(snap, cfg), it)
	return nodetest.Attrs(out.Items[0]), out.Events
}

func TestRemovedAndChanged(t *testing.T) {
	snap := []corev1.Pod{nodetest.Pod("p", "10.0.0.1", "u1",
		nodetest.Container("app", "OTEL_SERVICE_NAME", "app", "OTEL_RESOURCE_ATTRIBUTES", "team=ml,tier=gold"))}
	cfg := []corev1.Pod{nodetest.Pod("p", "10.0.0.1", "u1",
		nodetest.Container("app", "OTEL_SERVICE_NAME", "app", "OTEL_RESOURCE_ATTRIBUTES", "team=search"))}
	got, events := run(t, snap, cfg, map[string]string{
		"service.name": "app", "team": "ml", "tier": "gold", "k8s.pod.ip": "10.0.0.1",
	})
	if got["team"] != "search" {
		t.Errorf("team = %q, want search", got["team"])
	}
	if _, ok := got["tier"]; ok {
		t.Error("tier should be removed")
	}
	if e, ok := nodetest.Find(events, "team", lineage.Set); !ok || !e.Simulated || !strings.Contains(e.Reason, `changes it from "ml"`) {
		t.Errorf("team set event: %+v", e)
	}
	if _, ok := nodetest.Find(events, "tier", lineage.Removed); !ok {
		t.Error("missing removed event for tier")
	}
}

func TestRecordingDisagreesWithSnapshot(t *testing.T) {
	pods := []corev1.Pod{nodetest.Pod("p", "10.0.0.1", "u1",
		nodetest.Container("app", "OTEL_SERVICE_NAME", "app", "OTEL_RESOURCE_ATTRIBUTES", "team=ml"))}
	got, events := run(t, pods, pods, map[string]string{
		"service.name": "app", "team": "other", "k8s.pod.ip": "10.0.0.1",
	})
	if got["team"] != "other" {
		t.Error("data must not change when the snapshot is inconsistent")
	}
	if e, ok := nodetest.Find(events, "", lineage.Unverified); !ok || !strings.Contains(e.Reason, "disagrees") {
		t.Errorf("want unverified, got %+v", events)
	}
}

func TestAttribution(t *testing.T) {
	for _, tc := range []struct {
		name       string
		containers []corev1.Container
		attrs      map[string]string
		want       string // container name, or "" for unverified
	}{
		{
			name: "by service name",
			containers: []corev1.Container{
				nodetest.Container("app", "OTEL_SERVICE_NAME", "app"),
				nodetest.Container("shipper", "OTEL_SERVICE_NAME", "shipper"),
			},
			attrs: map[string]string{"service.name": "shipper"},
			want:  "shipper",
		},
		{
			name: "only instrumented container",
			containers: []corev1.Container{
				nodetest.Container("app", "OTEL_EXPORTER_OTLP_ENDPOINT", "http://x"),
				nodetest.Container("shipper"),
			},
			attrs: map[string]string{"service.name": "unknown_service"},
			want:  "app",
		},
		{
			name: "ambiguous",
			containers: []corev1.Container{
				nodetest.Container("a", "OTEL_SERVICE_NAME", "same"),
				nodetest.Container("b", "OTEL_SERVICE_NAME", "same"),
			},
			attrs: map[string]string{"service.name": "same"},
		},
		{
			name: "two instrumented, no name match",
			containers: []corev1.Container{
				nodetest.Container("a", "OTEL_EXPORTER_OTLP_ENDPOINT", "x"),
				nodetest.Container("b", "OTEL_EXPORTER_OTLP_ENDPOINT", "x"),
			},
			attrs: map[string]string{"service.name": "nope"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pods := []corev1.Pod{nodetest.Pod("p", "10.0.0.1", "u1", tc.containers...)}
			tc.attrs["k8s.pod.ip"] = "10.0.0.1"
			_, events := run(t, pods, pods, tc.attrs)
			e, attributed := nodetest.Find(events, "", lineage.Attributed)
			switch {
			case tc.want == "" && attributed:
				t.Errorf("want unverified, got %+v", e)
			case tc.want == "":
				if _, ok := nodetest.Find(events, "", lineage.Unverified); !ok {
					t.Errorf("want unverified, got %+v", events)
				}
			case !attributed || e.Detail["container"] != tc.want:
				t.Errorf("want container %s, got %+v", tc.want, events)
			}
		})
	}
}

func TestPodMissingFromConfig(t *testing.T) {
	snap := []corev1.Pod{nodetest.Pod("p", "10.0.0.1", "u1", nodetest.Container("app", "OTEL_SERVICE_NAME", "app"))}
	_, events := run(t, snap, nil, map[string]string{"service.name": "app", "k8s.pod.ip": "10.0.0.1"})
	if e, ok := nodetest.Find(events, "", lineage.Unverified); !ok || !strings.Contains(e.Reason, "isn't in the config under test") {
		t.Errorf("got %+v", events)
	}
}
