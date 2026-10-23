// Package nodetest has helpers for node tests.
package nodetest

import (
	"context"
	"testing"

	"go.opentelemetry.io/collector/pdata/pmetric"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/Anefu/otelcov/internal/graph"
	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/unit"
)

// Item builds a one-point gauge item with the given resource attributes.
func Item(id string, attrs map[string]string) unit.Metric {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	for k, v := range attrs {
		rm.Resource().Attributes().PutStr(k, v)
	}
	m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName("m")
	m.SetEmptyGauge().DataPoints().AppendEmpty().SetIntValue(1)
	return unit.Metric{ID: lineage.SignalID(id), Data: md}
}

// Attrs returns an item's resource attributes as strings.
func Attrs(it unit.Metric) map[string]string {
	out := map[string]string{}
	for k, v := range unit.Resource(it.Data).Attributes().All() {
		out[k] = v.AsString()
	}
	return out
}

// Run processes one item and returns the output.
func Run(t *testing.T, n graph.MetricsNode, it unit.Metric) graph.Output {
	t.Helper()
	out, err := n.ProcessMetrics(context.Background(), []unit.Metric{it})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Find returns the first event with the given attribute and action.
func Find(events []lineage.Event, attribute string, action lineage.Action) (lineage.Event, bool) {
	for _, e := range events {
		if e.Attribute == attribute && e.Action == action {
			return e, true
		}
	}
	return lineage.Event{}, false
}

// Pod builds a running pod with the given containers.
func Pod(name, ip, uid string, containers ...corev1.Container) corev1.Pod {
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: k8stypes.UID(uid)},
		Spec:       corev1.PodSpec{NodeName: "node-1", Containers: containers},
		Status:     corev1.PodStatus{PodIP: ip},
	}
	for _, c := range containers {
		p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, corev1.ContainerStatus{
			Name: c.Name, ContainerID: "containerd://id-" + name + "-" + c.Name,
		})
	}
	return p
}

// Container builds a container with literal env vars, given as name, value pairs.
func Container(name string, env ...string) corev1.Container {
	c := corev1.Container{Name: name}
	for i := 0; i+1 < len(env); i += 2 {
		c.Env = append(c.Env, corev1.EnvVar{Name: env[i], Value: env[i+1]})
	}
	return c
}
