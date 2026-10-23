package resourcedetection

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/collector/pdata/pmetric"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/unit"
)

func pod(name, ip, node string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{PodIP: ip, PodIPs: []corev1.PodIP{{IP: ip}}},
	}
}

func item(id string, res map[string]string) unit.Metric {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	for k, v := range res {
		rm.Resource().Attributes().PutStr(k, v)
	}
	m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName("m")
	m.SetEmptyGauge().DataPoints().AppendEmpty().SetIntValue(1)
	return unit.Metric{ID: lineage.SignalID(id), Data: md}
}

var rec = &Recorded{
	Config: map[string]any{"detectors": []any{"env", "system"}},
	Values: Values{
		"node-a": {"host.name": "node-a", "os.type": "linux"},
		"node-b": {"host.name": "node-b", "os.type": "linux"},
	},
}

var pods = []corev1.Pod{pod("app-1", "10.0.0.1", "node-a"), pod("app-2", "10.0.0.2", "node-b")}

func process(t *testing.T, raw map[string]any, r *Recorded, in ...unit.Metric) map[lineage.SignalID][]lineage.Event {
	t.Helper()
	n, err := New("agent/resourcedetection", raw, r, pods)
	if err != nil {
		t.Fatal(err)
	}
	out, err := n.ProcessMetrics(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	by := map[lineage.SignalID][]lineage.Event{}
	for _, e := range out.Events {
		by[e.Signal] = append(by[e.Signal], e)
	}
	return by
}

func TestValuesPerNode(t *testing.T) {
	a := item("s1", map[string]string{"k8s.pod.ip": "10.0.0.1"})
	b := item("s2", map[string]string{"k8s.pod.ip": "10.0.0.2", "host.name": "from-app"})
	ev := process(t, map[string]any{"detectors": []any{"env", "system"}}, rec, a, b)
	if v, _ := unit.Resource(a.Data).Attributes().Get("host.name"); v.Str() != "node-a" {
		t.Errorf("s1 runs on node-a: host.name = %q", v.Str())
	}
	// override defaults to true: the detected value replaces the app's.
	if v, _ := unit.Resource(b.Data).Attributes().Get("host.name"); v.Str() != "node-b" {
		t.Errorf("override is on by default: host.name = %q", v.Str())
	}
	if len(ev["s1"]) != 2 || ev["s1"][0].Action != lineage.Set || ev["s1"][0].Attribute != "host.name" {
		t.Errorf("want host.name then os.type set: %+v", ev["s1"])
	}
}

func TestOverrideOff(t *testing.T) {
	b := item("s2", map[string]string{"k8s.pod.ip": "10.0.0.2", "host.name": "from-app"})
	ev := process(t, map[string]any{"detectors": []any{"env", "system"}, "override": false}, rec, b)
	if v, _ := unit.Resource(b.Data).Attributes().Get("host.name"); v.Str() != "from-app" {
		t.Errorf("override off keeps the app's value, got %q", v.Str())
	}
	if e := ev["s2"][0]; e.Action != lineage.Kept || e.Value != "from-app" {
		t.Errorf("want host.name kept: %+v", e)
	}
}

func TestUnverified(t *testing.T) {
	cfg := map[string]any{"detectors": []any{"env", "system"}}
	for _, tc := range []struct {
		name string
		raw  map[string]any
		rec  *Recorded
		res  map[string]string
		want string
	}{
		{"nothing recorded", cfg, nil, map[string]string{"k8s.pod.ip": "10.0.0.1"}, "no recorded resource detection"},
		{"unknown node", cfg, rec, map[string]string{"k8s.pod.ip": "10.9.9.9"}, `on node ""`},
		{"detector added", map[string]any{"detectors": []any{"env", "system", "eks"}}, rec, map[string]string{"k8s.pod.ip": "10.0.0.1"}, "detector settings differ"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := process(t, tc.raw, tc.rec, item("s1", tc.res))
			var found bool
			for _, e := range ev["s1"] {
				found = found || (e.Action == lineage.Unverified && strings.Contains(e.Reason, tc.want))
			}
			if !found {
				t.Errorf("want unverified %q: %+v", tc.want, ev["s1"])
			}
		})
	}
}

func TestStarForNonDaemonsets(t *testing.T) {
	r := &Recorded{Config: map[string]any{}, Values: Values{"*": {"cloud.region": "eu-west-1"}}}
	a := item("s1", map[string]string{"k8s.pod.ip": "10.0.0.1"})
	ev := process(t, map[string]any{"override": true}, r, a)
	if v, _ := unit.Resource(a.Data).Attributes().Get("cloud.region"); v.Str() != "eu-west-1" {
		t.Errorf("want the * values: %+v", ev)
	}
	for _, e := range ev["s1"] {
		if e.Action == lineage.Unverified {
			t.Errorf("only override changed, which the node models: %+v", e)
		}
	}
}
