// Package otelenv computes the resource attributes an OpenTelemetry SDK takes
// from its environment: OTEL_RESOURCE_ATTRIBUTES and OTEL_SERVICE_NAME.
package otelenv

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/Anefu/otelcov/internal/kube"
)

const (
	// ResourceAttributesEnv holds comma-separated key=value pairs.
	ResourceAttributesEnv = "OTEL_RESOURCE_ATTRIBUTES"
	// ServiceNameEnv sets service.name and takes precedence over
	// service.name in OTEL_RESOURCE_ATTRIBUTES.
	ServiceNameEnv = "OTEL_SERVICE_NAME"
)

// Attribute is one resource attribute set through the environment.
type Attribute struct {
	Key   string
	Value string
	// Env is the variable that set it.
	Env string
	// RawValue is the value as written in the pod spec, before $(VAR) expansion.
	RawValue string
}

// Resource is the SDK's environment-derived resource, in declaration order.
type Resource struct {
	Attrs []Attribute
	// Raw maps each variable that contributed to its value as written in the
	// pod spec. A variable that's absent has no entry.
	Raw map[string]string
}

// Get returns the attribute with key k.
func (r Resource) Get(k string) (Attribute, bool) {
	for _, a := range r.Attrs {
		if a.Key == k {
			return a, true
		}
	}
	return Attribute{}, false
}

// FromEnv derives the resource from a resolved container environment. It fails
// when a variable it needs can't be resolved, or when OTEL_RESOURCE_ATTRIBUTES
// is malformed (SDKs disagree on how to handle that).
func FromEnv(env kube.Env) (Resource, error) {
	r := Resource{Raw: map[string]string{}}
	if v, found, certain := env.Get(ResourceAttributesEnv); !certain {
		return r, fmt.Errorf("%s may come from envFrom, which isn't resolved yet", ResourceAttributesEnv)
	} else if found {
		if v.Err != nil {
			return r, v.Err
		}
		r.Raw[ResourceAttributesEnv] = v.Raw
		pairs, err := Parse(v.Value)
		if err != nil {
			return r, fmt.Errorf("%s: %w", ResourceAttributesEnv, err)
		}
		raw, _ := Parse(v.Raw) // same structure before expansion, for reporting
		rawByKey := map[string]string{}
		for _, p := range raw {
			rawByKey[p.Key] = p.Value
		}
		for _, p := range pairs {
			r.put(Attribute{Key: p.Key, Value: p.Value, Env: ResourceAttributesEnv, RawValue: rawByKey[p.Key]})
		}
	}
	if v, found, certain := env.Get(ServiceNameEnv); !certain {
		return r, fmt.Errorf("%s may come from envFrom, which isn't resolved yet", ServiceNameEnv)
	} else if found {
		if v.Err != nil {
			return r, v.Err
		}
		r.Raw[ServiceNameEnv] = v.Raw
		if v.Value != "" {
			r.put(Attribute{Key: "service.name", Value: v.Value, Env: ServiceNameEnv, RawValue: v.Raw})
		}
	}
	return r, nil
}

// put adds or replaces an attribute, keeping its first position.
func (r *Resource) put(a Attribute) {
	for i := range r.Attrs {
		if r.Attrs[i].Key == a.Key {
			r.Attrs[i] = a
			return
		}
	}
	r.Attrs = append(r.Attrs, a)
}

// Pair is one key=value entry of OTEL_RESOURCE_ATTRIBUTES.
type Pair struct{ Key, Value string }

// Parse splits an OTEL_RESOURCE_ATTRIBUTES value into pairs. Keys and values
// are trimmed and values are percent-decoded, as the specification requires.
// Later duplicates win, keeping the first position.
func Parse(s string) ([]Pair, error) {
	var out []Pair
	pos := map[string]int{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid pair %q", part)
		}
		dv, err := url.PathUnescape(strings.TrimSpace(v))
		if err != nil {
			return nil, fmt.Errorf("invalid value in %q: %w", part, err)
		}
		if i, dup := pos[k]; dup {
			out[i].Value = dv
			continue
		}
		pos[k] = len(out)
		out = append(out, Pair{Key: k, Value: dv})
	}
	return out, nil
}
