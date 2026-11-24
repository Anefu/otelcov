package collector

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// NodeNameField is the downward API field holding a pod's node name.
const NodeNameField = "spec.nodeName"

// EnvVar is a collector environment variable as the topology gives it: a
// literal value, or a field of the collector's own pod, like the downward
// API's fieldRef. Only spec.nodeName is supported: a daemonset agent's node,
// which differs per agent instance.
type EnvVar struct {
	Value string
	// FieldRef is the pod field the value comes from; "" for a literal.
	FieldRef string
}

// UnmarshalJSON accepts "value" or {fieldRef: spec.nodeName}.
func (e *EnvVar) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*e = EnvVar{Value: s}
		return nil
	}
	var o struct {
		FieldRef string `json:"fieldRef"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil || o.FieldRef == "" {
		return fmt.Errorf("an env value is a string or {fieldRef: %s}", NodeNameField)
	}
	if o.FieldRef != NodeNameField {
		return fmt.Errorf("env fieldRef %q isn't supported; only %s is", o.FieldRef, NodeNameField)
	}
	*e = EnvVar{FieldRef: o.FieldRef}
	return nil
}

// MarshalJSON writes the form UnmarshalJSON reads.
func (e EnvVar) MarshalJSON() ([]byte, error) {
	if e.FieldRef != "" {
		return json.Marshal(map[string]string{"fieldRef": e.FieldRef})
	}
	return json.Marshal(e.Value)
}

// Literals returns the variables with literal values, for ${env:VAR}
// substitution. A fieldRef's value differs per collector instance, so
// references to it stay as written.
func Literals(env map[string]EnvVar) map[string]string {
	out := map[string]string{}
	for k, v := range env {
		if v.FieldRef == "" {
			out[k] = v.Value
		}
	}
	return out
}
