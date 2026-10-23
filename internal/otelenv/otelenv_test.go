package otelenv

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/Anefu/otelcov/internal/kube"
)

func TestParse(t *testing.T) {
	got, err := Parse(" a=1 , b = x%20y ,, a=2,c=")
	if err != nil {
		t.Fatal(err)
	}
	want := []Pair{{"a", "2"}, {"b", "x y"}, {"c", ""}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	for _, bad := range []string{"novalue", "=v", "a=%zz"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) should fail", bad)
		}
	}
}

func TestFromEnv(t *testing.T) {
	pod := &corev1.Pod{}
	c := &corev1.Container{Env: []corev1.EnvVar{
		{Name: "OTEL_RESOURCE_ATTRIBUTES", Value: "service.name=from-attrs,team=ml"},
		{Name: "OTEL_SERVICE_NAME", Value: "vllm"},
	}}
	r, err := FromEnv(kube.ResolveEnv(pod, c))
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := r.Get("service.name"); a.Value != "vllm" || a.Env != ServiceNameEnv {
		t.Errorf("OTEL_SERVICE_NAME should win: %+v", a)
	}
	if a, _ := r.Get("team"); a.Value != "ml" || a.Env != ResourceAttributesEnv {
		t.Errorf("team: %+v", a)
	}
	if r.Attrs[0].Key != "service.name" {
		t.Errorf("service.name should keep its first position: %+v", r.Attrs)
	}

	c.Env[0].Value = "broken"
	if _, err := FromEnv(kube.ResolveEnv(pod, c)); err == nil {
		t.Error("malformed OTEL_RESOURCE_ATTRIBUTES should fail")
	}

	c.Env = nil
	c.EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{}}}
	if _, err := FromEnv(kube.ResolveEnv(pod, c)); err == nil {
		t.Error("envFrom could set the variables: should be an error")
	}
}
