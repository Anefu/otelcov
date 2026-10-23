package kube

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestExpand(t *testing.T) {
	vars := map[string]string{"A": "1", "B": "two"}
	mapping := func(n string) (string, bool) { v, ok := vars[n]; return v, ok }
	for _, tc := range []struct {
		in, want string
		refs     []string
	}{
		{"plain", "plain", nil},
		{"$(A)", "1", []string{"A"}},
		{"x=$(A),y=$(B)", "x=1,y=two", []string{"A", "B"}},
		{"$(MISSING)", "$(MISSING)", nil}, // undefined: kept as written
		{"$$(A)", "$(A)", nil},            // $$ escapes
		{"cost $5", "cost $5", nil},       // lone $ kept
		{"$(A", "$(A", nil},               // unclosed: kept
		{"end$", "end$", nil},
	} {
		got, refs := expand(tc.in, mapping)
		if got != tc.want || strings.Join(refs, ",") != strings.Join(tc.refs, ",") {
			t.Errorf("expand(%q) = %q %v, want %q %v", tc.in, got, refs, tc.want, tc.refs)
		}
	}
}

func TestResolveEnv(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", UID: "u-1", Labels: map[string]string{"app": "vllm"}},
		Spec:       corev1.PodSpec{NodeName: "node-1"},
	}
	c := &corev1.Container{Env: []corev1.EnvVar{
		{Name: "LATER_REF", Value: "$(POD_UID)"}, // defined before POD_UID: not expanded
		{Name: "POD_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}}},
		{Name: "APP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.labels['app']"}}},
		{Name: "ATTRS", Value: "k8s.pod.uid=$(POD_UID),app=$(APP)"},
		{Name: "FROM_CM", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "cm"}, Key: "k"}}},
		{Name: "USES_CM", Value: "$(FROM_CM)"},
	}}
	env := ResolveEnv(pod, c)

	check := func(name, want string) {
		t.Helper()
		v, found, certain := env.Get(name)
		if !found || !certain || v.Err != nil || v.Value != want {
			t.Errorf("%s = %+v (found %v, certain %v), want %q", name, v, found, certain, want)
		}
	}
	check("LATER_REF", "$(POD_UID)")
	check("POD_UID", "u-1")
	check("APP", "vllm")
	check("ATTRS", "k8s.pod.uid=u-1,app=vllm")
	check("USES_CM", "$(FROM_CM)") // unresolvable references stay as written

	if v, _, _ := env.Get("ATTRS"); v.Raw != "k8s.pod.uid=$(POD_UID),app=$(APP)" || v.Source != "literal" {
		t.Errorf("ATTRS raw/source = %q/%q", v.Raw, v.Source)
	}
	if v, _, _ := env.Get("FROM_CM"); v.Err == nil {
		t.Error("ConfigMap reference should be unresolved")
	}
	if _, found, certain := env.Get("NOPE"); found || !certain {
		t.Errorf("undefined var without envFrom: found %v certain %v", found, certain)
	}

	c.EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "cm"}}}}
	if _, _, certain := ResolveEnv(pod, c).Get("NOPE"); certain {
		t.Error("with envFrom, an undefined var might exist")
	}
}
