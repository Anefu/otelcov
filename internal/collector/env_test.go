package collector

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestEnvVar(t *testing.T) {
	var env map[string]EnvVar
	if err := yaml.UnmarshalStrict([]byte("A: x\nB: {fieldRef: spec.nodeName}\nC: \"\"\n"), &env); err != nil {
		t.Fatal(err)
	}
	if env["A"] != (EnvVar{Value: "x"}) || env["B"] != (EnvVar{FieldRef: NodeNameField}) || env["C"] != (EnvVar{}) {
		t.Errorf("%+v", env)
	}
	// A fieldRef's value differs per instance, so ${env:B} stays as written.
	if lit := Literals(env); len(lit) != 2 || lit["A"] != "x" || lit["C"] != "" {
		t.Errorf("literals: %v", lit)
	}
	b, err := yaml.Marshal(env)
	if err != nil || !strings.Contains(string(b), "fieldRef: spec.nodeName") {
		t.Errorf("round trip: %s %v", b, err)
	}

	for in, want := range map[string]string{
		"A: {fieldRef: metadata.name}":  "only spec.nodeName is",
		"A: {fieldPath: spec.nodeName}": "a string or {fieldRef: spec.nodeName}",
		"A: [x]":                        "a string or {fieldRef: spec.nodeName}",
	} {
		var e map[string]EnvVar
		if err := yaml.UnmarshalStrict([]byte(in), &e); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", in, err, want)
		}
	}
}
