package kube

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// EnvVar is one resolved container environment variable.
type EnvVar struct {
	Name string
	// Value as the process sees it, after $(VAR) expansion. Empty when Err is set.
	Value string
	// Raw is the value as written in the pod spec, before expansion.
	// Empty for valueFrom variables.
	Raw string
	// Source describes where the value came from: "literal" or "fieldRef <path>".
	Source string
	// Refs lists the $(VAR) references that were expanded, in order.
	Refs []string
	// Err is set when the value can't be resolved from the pod alone
	// (ConfigMap or Secret references, resource fields, unknown field paths).
	Err error
}

// Env is a container's environment, resolved the way the kubelet does it.
type Env struct {
	vars  map[string]EnvVar
	order []string
	// fromUnresolved is set when the container uses envFrom, which isn't
	// resolved yet: any name might be defined there.
	fromUnresolved bool
}

// ResolveEnv resolves the env vars of container c in pod: literal values,
// fieldRef (downward API) values, and $(VAR) references to variables defined
// earlier in the list. Other sources are kept with Err set.
func ResolveEnv(pod *corev1.Pod, c *corev1.Container) Env {
	e := Env{vars: map[string]EnvVar{}, fromUnresolved: len(c.EnvFrom) > 0}
	for _, v := range c.Env {
		ev := EnvVar{Name: v.Name}
		switch {
		case v.ValueFrom == nil:
			ev.Raw = v.Value
			ev.Source = "literal"
			ev.Value, ev.Refs = expand(v.Value, func(name string) (string, bool) {
				prev, ok := e.vars[name]
				if !ok || prev.Err != nil {
					return "", false
				}
				return prev.Value, true
			})
		case v.ValueFrom.FieldRef != nil:
			path := v.ValueFrom.FieldRef.FieldPath
			ev.Source = "fieldRef " + path
			ev.Value, ev.Err = fieldValue(pod, path)
		case v.ValueFrom.ConfigMapKeyRef != nil:
			ev.Source = "configMapKeyRef " + v.ValueFrom.ConfigMapKeyRef.Name
			ev.Err = fmt.Errorf("%s comes from ConfigMap %s, which isn't resolved yet", v.Name, v.ValueFrom.ConfigMapKeyRef.Name)
		case v.ValueFrom.SecretKeyRef != nil:
			ev.Source = "secretKeyRef " + v.ValueFrom.SecretKeyRef.Name
			ev.Err = fmt.Errorf("%s comes from Secret %s, which isn't resolved", v.Name, v.ValueFrom.SecretKeyRef.Name)
		default:
			ev.Source = "valueFrom"
			ev.Err = fmt.Errorf("%s uses a valueFrom source that isn't resolved yet", v.Name)
		}
		if _, seen := e.vars[v.Name]; !seen {
			e.order = append(e.order, v.Name)
		}
		e.vars[v.Name] = ev
	}
	return e
}

// Get returns a variable. certain is false when the variable isn't in env but
// might come from an unresolved envFrom.
func (e Env) Get(name string) (v EnvVar, found, certain bool) {
	v, found = e.vars[name]
	return v, found, found || !e.fromUnresolved
}

// Names lists the variables defined in env, in order.
func (e Env) Names() []string { return append([]string(nil), e.order...) }

// HasPrefix reports whether any variable name starts with prefix.
func (e Env) HasPrefix(prefix string) bool {
	for _, n := range e.order {
		if strings.HasPrefix(n, prefix) {
			return true
		}
	}
	return false
}

// UsesEnvFrom reports whether the container has envFrom sources.
func (e Env) UsesEnvFrom() bool { return e.fromUnresolved }

// expand applies Kubernetes $(VAR) expansion: $(NAME) is replaced when
// mapping knows NAME and kept as written otherwise; $$ is an escaped $.
func expand(s string, mapping func(string) (string, bool)) (string, []string) {
	var b strings.Builder
	var refs []string
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case '$':
			b.WriteByte('$')
			i++
		case '(':
			end := strings.IndexByte(s[i+2:], ')')
			if end < 0 {
				b.WriteString(s[i:])
				return b.String(), refs
			}
			name := s[i+2 : i+2+end]
			if val, ok := mapping(name); ok {
				b.WriteString(val)
				refs = append(refs, name)
			} else {
				b.WriteString("$(" + name + ")")
			}
			i += 2 + end
		default:
			b.WriteByte('$')
		}
	}
	return b.String(), refs
}

// References returns the $(VAR) names in s, in order, skipping $$ escapes.
func References(s string) []string {
	var refs []string
	expand(s, func(name string) (string, bool) {
		refs = append(refs, name)
		return "", true
	})
	return refs
}

// fieldValue resolves a downward API field path for env vars.
func fieldValue(pod *corev1.Pod, path string) (string, error) {
	switch path {
	case "metadata.name":
		return pod.Name, nil
	case "metadata.namespace":
		return pod.Namespace, nil
	case "metadata.uid":
		return string(pod.UID), nil
	case "spec.nodeName":
		return pod.Spec.NodeName, nil
	case "spec.serviceAccountName":
		return pod.Spec.ServiceAccountName, nil
	case "status.hostIP":
		return pod.Status.HostIP, nil
	case "status.podIP":
		return pod.Status.PodIP, nil
	}
	for _, m := range []struct {
		prefix string
		values map[string]string
	}{
		{"metadata.labels['", pod.Labels},
		{"metadata.annotations['", pod.Annotations},
	} {
		if strings.HasPrefix(path, m.prefix) && strings.HasSuffix(path, "']") {
			return m.values[path[len(m.prefix):len(path)-2]], nil
		}
	}
	return "", fmt.Errorf("field path %q isn't supported", path)
}
