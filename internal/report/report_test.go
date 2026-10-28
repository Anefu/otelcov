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
	return requirements.Requirement{
		ID:       "watched",
		Subjects: requirements.Subjects{Kind: requirements.KindPod, Namespace: "ns", Container: "app"},
		Signal:   requirements.Signal{Metrics: []string{"m"}},
		Reaches:  []requirements.Destination{{Alert: "A", Receivers: []string{"pager"}}},
	}
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

func withReaches(r requirements.Requirement, d ...requirements.Destination) requirements.Requirement {
	r.Reaches = d
	return r
}

// Format A destinations (decision 57).
func TestDestinations(t *testing.T) {
	build := func(r requirements.Requirement, pod corev1.Pod, route ...lineage.Event) *Report {
		return Build("x", "", chain(t, route...), []requirements.Requirement{r}, []corev1.Pod{pod})
	}
	pod := pods("p")[0]

	t.Run("every receiver must be reached", func(t *testing.T) {
		r := build(withReaches(req(), requirements.Destination{Alert: "A", Receivers: []string{"pager", "email"}}), pod, routed("pager"))
		if e := r.Summary[0]; e.Status != NotCovered || e.Destination != "alert A → email" || e.BrokenAt != "alertmanager/route" {
			t.Errorf("%+v", e)
		}
	})
	t.Run("no receivers means any", func(t *testing.T) {
		r := build(withReaches(req(), requirements.Destination{Alert: "A"}), pod, routed("email"))
		if e := r.Summary[0]; e.Status != Covered || e.Receiver != "email" || e.Destination != "" {
			t.Errorf("%+v", e)
		}
	})
	t.Run("receiver templated from a pod label", func(t *testing.T) {
		labeled := pod
		labeled.Labels = map[string]string{"team": "search"}
		tmpl := withReaches(req(), requirements.Destination{Alert: "A", Receivers: []string{"{{ subject.labels.team }}-pager"}})
		if e := build(tmpl, labeled, routed("search-pager")).Summary[0]; e.Status != Covered {
			t.Errorf("%+v", e)
		}
		if e := build(tmpl, pod, routed("search-pager")).Summary[0]; e.Status != Unverified || !strings.Contains(e.Reason, "needs pod label `team`") {
			t.Errorf("a pod without the label can't be checked: %+v", e)
		}
	})
	t.Run("the alert must carry the required labels", func(t *testing.T) {
		r := build(withReaches(req(), requirements.Destination{Alert: "A", Receivers: []string{"pager"}, Labels: []string{"k8s_pod_name"}}), pod, routed("pager"))
		e := r.Summary[0]
		if e.Status != NotCovered || e.BrokenAt != "alert/A" || e.Receiver != "pager" {
			t.Errorf("%+v", e)
		}
		if len(r.Diagnosis) != 1 || !strings.Contains(r.Diagnosis[0].Explanation, "has no `k8s_pod_name` label") || r.Diagnosis[0].RootCause == "" {
			t.Errorf("%+v", r.Diagnosis)
		}
	})
	t.Run("unsupported destinations are unverified, after what fails", func(t *testing.T) {
		r := withReaches(req(),
			requirements.Destination{Alert: "A", Receivers: []string{"pager"}},
			requirements.Destination{Dashboard: &requirements.Dashboard{UID: "d", Panel: "p"}})
		e := build(r, pod, routed("pager")).Summary[0]
		if e.Status != Unverified || e.Reason != "dashboard destinations aren't supported yet" || !strings.HasPrefix(e.Destination, "dashboard d") {
			t.Errorf("%+v", e)
		}
		// A destination that isn't covered wins over one that can't be checked.
		e = build(r, pod, routed("email")).Summary[0]
		if e.Status != NotCovered {
			t.Errorf("%+v", e)
		}
		r = req()
		r.MustNotReach = []requirements.Destination{{Backend: "mimir"}}
		if e := build(r, pod, routed("pager")).Summary[0]; e.Status != Unverified || e.Reason != "must_not_reach isn't supported yet" {
			t.Errorf("%+v", e)
		}
	})
	t.Run("other subject kinds are reported, not checked", func(t *testing.T) {
		r := req()
		r.Subjects = requirements.Subjects{Kind: requirements.KindNode}
		rep := build(r, pod, routed("pager"))
		if len(rep.Summary) != 0 || len(rep.Problems) != 1 || !strings.Contains(rep.Problems[0].Message, "kind node aren't supported yet") {
			t.Errorf("summary %+v, problems %+v", rep.Summary, rep.Problems)
		}
	})
	t.Run("any of the metrics counts", func(t *testing.T) {
		r := req()
		r.Signal.Metrics = []string{"other", "m"}
		if e := build(r, pod, routed("pager")).Summary[0]; e.Status != Covered {
			t.Errorf("%+v", e)
		}
	})
}
