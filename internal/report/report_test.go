package report

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/requirements"
)

func req() requirements.Requirement {
	var r requirements.Requirement
	r.ID = "watched"
	r.ForEachPod.Namespace = "ns"
	r.Signal.Metric = "m"
	r.Signal.Container = "app"
	r.MustReach.Alert = "A"
	r.MustReach.Receiver = "pager"
	return r
}

func pods(names ...string) []corev1.Pod {
	var out []corev1.Pod
	for _, n := range names {
		out = append(out, corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: "ns"}})
	}
	return out
}

// chain builds the log of one pod's signal reaching alert a1, with extra
// events for the route stage.
func chain(t *testing.T, routeEvents ...lineage.Event) *lineage.Log {
	t.Helper()
	log := lineage.NewLog()
	for _, s := range []lineage.Signal{
		{ID: "s1", Node: "source/otlp-replay", Kind: "otlp_metric", Name: "m"},
		{ID: "p1", Node: "backend/mimir-otlp", Kind: "series", From: []lineage.SignalID{"s1"}, Labels: map[string]string{"__name__": "m"}},
		{ID: "a1", Node: "alert/A", Kind: "alert", From: []lineage.SignalID{"p1"}},
	} {
		if err := log.AddSignal(s); err != nil {
			t.Fatal(err)
		}
	}
	log.Record(
		lineage.Event{Node: "source/sdk-env", Signal: "s1", Action: lineage.Attributed, Detail: map[string]any{"pod": "ns/p", "container": "app"}},
		lineage.Event{Node: "collector/k8sattributes", Signal: "s1", Attribute: "k8s.pod.start_time", Action: lineage.Unverified, Reason: "not modeled"},
		lineage.Event{Node: "alert/A", Signal: "a1", Action: lineage.Derived, From: []lineage.SignalID{"p1"}, Detail: map[string]any{"expr": "m"}},
	)
	log.Record(routeEvents...)
	return log
}

func routed(receiver string) lineage.Event {
	return lineage.Event{Node: "alertmanager/route", Signal: "a1", Action: lineage.Routed,
		Detail: map[string]any{"receiver": receiver, "route": []any{map[string]any{"receiver": receiver, "matchers": []any{}}}}}
}

func TestStatuses(t *testing.T) {
	t.Run("covered, irrelevant unverified attribute ignored", func(t *testing.T) {
		r := Build("x", "", chain(t, routed("pager")), []requirements.Requirement{req()}, pods("p"))
		if e := r.Summary[0]; e.Status != Covered || e.Receiver != "pager" || len(e.Path) != 3 {
			t.Errorf("%+v", e)
		}
	})
	t.Run("routed elsewhere", func(t *testing.T) {
		r := Build("x", "", chain(t, routed("email")), []requirements.Requirement{req()}, pods("p"))
		e := r.Summary[0]
		if e.Status != NotCovered || e.BrokenAt != "alertmanager/route" {
			t.Errorf("%+v", e)
		}
		if len(r.Diagnosis) != 1 || !strings.Contains(r.Diagnosis[0].Explanation, "sends it to `email`, not `pager`") {
			t.Errorf("%+v", r.Diagnosis)
		}
	})
	t.Run("uncertain route", func(t *testing.T) {
		r := Build("x", "", chain(t, routed("pager"), lineage.Event{
			Node: "alertmanager/route", Signal: "a1", Action: lineage.Unverified, Reason: "muted at weekends",
		}), []requirements.Requirement{req()}, pods("p"))
		if e := r.Summary[0]; e.Status != Unverified || len(e.UnverifiedBy) != 1 {
			t.Errorf("%+v", e)
		}
	})
	t.Run("no telemetry from the pod", func(t *testing.T) {
		r := Build("x", "", chain(t, routed("pager")), []requirements.Requirement{req()}, pods("other"))
		if e := r.Summary[0]; e.Status != Unverified || e.BrokenAt != "source/otlp-replay" || !strings.Contains(e.Reason, "app container") {
			t.Errorf("%+v", e)
		}
	})
}
