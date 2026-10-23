// Package sdkenv simulates changes to a container's OpenTelemetry SDK
// environment (OTEL_RESOURCE_ATTRIBUTES, OTEL_SERVICE_NAME).
//
// The SDK applies these inside the app, before the tap, so their effect is
// baked into recordings. To answer "what if this PR changes them", the node
// works out which container sent each signal, undoes the attributes the
// snapshot's environment set, and applies the ones the config under test sets.
package sdkenv

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/Anefu/otelcov/internal/graph"
	"github.com/Anefu/otelcov/internal/kube"
	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/otelenv"
	"github.com/Anefu/otelcov/internal/unit"
)

// Name is the node name used in lineage.
const Name = "source/sdk-env"

// Node is the sdk-env node.
type Node struct {
	snapshot *kube.Pods // pods running when the data was recorded
	config   *kube.Pods // pods in the config under test
}

// New builds the node from the snapshot's pods and the config's pods.
func New(snapshotPods, configPods []corev1.Pod) *Node {
	return &Node{snapshot: kube.NewPods(snapshotPods), config: kube.NewPods(configPods)}
}

func (n *Node) Name() string { return Name }

func (n *Node) ProcessMetrics(_ context.Context, in []unit.Metric) (graph.Output, error) {
	out := graph.Output{Items: in}
	for _, it := range in {
		out.Events = append(out.Events, n.process(it)...)
	}
	return out, nil
}

// process applies the environment diff to one item and returns its events.
func (n *Node) process(it unit.Metric) []lineage.Event {
	attrs := unit.Resource(it.Data).Attributes()
	var events []lineage.Event
	emit := func(e lineage.Event) {
		e.Node, e.Signal = Name, it.ID
		events = append(events, e)
	}
	unverified := func(format string, args ...any) []lineage.Event {
		emit(lineage.Event{Action: lineage.Unverified, Reason: fmt.Sprintf(format, args...)})
		return events
	}
	str := func(k string) (string, bool) {
		v, ok := attrs.Get(k)
		if !ok {
			return "", false
		}
		return v.AsString(), true
	}

	// 1. Which pod sent it, as of recording time.
	pod, podVia := n.snapshotPod(str)
	if pod == nil {
		return unverified("no pod in the snapshot pod specs matches this signal's k8s.pod.ip or k8s.pod.uid")
	}

	// 2. Which container.
	svc, _ := str("service.name")
	c, containerVia, err := attribute(pod, svc)
	if err != nil {
		return unverified("%s", err)
	}
	emit(lineage.Event{Action: lineage.Attributed, Detail: map[string]any{
		"pod": kube.Ref(pod), "pod_via": podVia, "container": c.Name, "container_via": containerVia,
	}})

	// 3. What the snapshot's environment set, which the recording must reflect.
	old, err := otelenv.FromEnv(kube.ResolveEnv(pod, c))
	if err != nil {
		return unverified("can't resolve the snapshot's SDK environment of container %s: %s", c.Name, err)
	}
	for _, a := range old.Attrs {
		if got, ok := str(a.Key); !ok || got != a.Value {
			return unverified("the recording disagrees with the snapshot: %s of container %s sets %s=%q, but the signal has %q; the snapshot metadata is probably wrong",
				a.Env, c.Name, a.Key, a.Value, got)
		}
	}

	// 4. What the config under test sets.
	newPod, ok := n.config.ByName(pod.Namespace, pod.Name)
	if !ok {
		return unverified("pod %s isn't in the config under test", kube.Ref(pod))
	}
	newC, ok := kube.Container(newPod, c.Name)
	if !ok {
		return unverified("container %s isn't in pod %s in the config under test", c.Name, kube.Ref(newPod))
	}
	newEnv := kube.ResolveEnv(newPod, newC)
	nw, err := otelenv.FromEnv(newEnv)
	if err != nil {
		return unverified("can't resolve the SDK environment of container %s in the config under test: %s", c.Name, err)
	}

	// 5. Undo what's gone, apply what's new or changed.
	for _, a := range old.Attrs {
		if _, still := nw.Get(a.Key); still {
			continue
		}
		attrs.Remove(a.Key)
		emit(lineage.Event{
			Attribute: a.Key, Action: lineage.Removed, Simulated: true,
			Reason: fmt.Sprintf("the config under test no longer sets it in %s of container %s", a.Env, c.Name),
			Detail: envDetail(a.Env, old, nw, ""),
		})
	}
	for _, a := range nw.Attrs {
		prev, had := old.Get(a.Key)
		if had && prev.Value == a.Value {
			continue
		}
		attrs.PutStr(a.Key, a.Value)
		reason := fmt.Sprintf("the config under test sets it in %s of container %s; the snapshot predates it", a.Env, c.Name)
		if had {
			reason = fmt.Sprintf("the config under test changes it from %q in %s of container %s", prev.Value, a.Env, c.Name)
		}
		emit(lineage.Event{
			Attribute: a.Key, Action: lineage.Set, Value: a.Value, Simulated: true, Reason: reason,
			Detail: envDetail(a.Env, old, nw, resolved(a.RawValue, newEnv)),
		})
	}
	return events
}

// snapshotPod finds the sending pod by the attributes the tap records.
func (n *Node) snapshotPod(str func(string) (string, bool)) (*corev1.Pod, string) {
	if ip, ok := str("k8s.pod.ip"); ok {
		if p, ok := n.snapshot.ByIP(ip); ok {
			return p, "k8s.pod.ip=" + ip + " in the snapshot pod specs"
		}
	}
	if uid, ok := str("k8s.pod.uid"); ok {
		if p, ok := n.snapshot.ByUID(uid); ok {
			return p, "k8s.pod.uid=" + uid + " in the snapshot pod specs"
		}
	}
	return nil, ""
}

// attribute works out which container sent a signal: the one whose SDK
// service name matches the recorded service.name, otherwise the only
// container with OTEL_* variables.
func attribute(pod *corev1.Pod, serviceName string) (*corev1.Container, string, error) {
	var byName, instrumented []int
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		env := kube.ResolveEnv(pod, c)
		if env.HasPrefix("OTEL_") || env.UsesEnvFrom() {
			instrumented = append(instrumented, i)
		}
		if serviceName == "" {
			continue
		}
		if r, err := otelenv.FromEnv(env); err == nil {
			if a, ok := r.Get("service.name"); ok && a.Value == serviceName {
				byName = append(byName, i)
			}
		}
	}
	names := func(idx []int) string {
		var s []string
		for _, i := range idx {
			s = append(s, pod.Spec.Containers[i].Name)
		}
		return strings.Join(s, ", ")
	}
	switch {
	case len(byName) == 1:
		c := &pod.Spec.Containers[byName[0]]
		r, _ := otelenv.FromEnv(kube.ResolveEnv(pod, c))
		a, _ := r.Get("service.name")
		return c, fmt.Sprintf("service.name %q matches %s of container %s", serviceName, a.Env, c.Name), nil
	case len(byName) > 1:
		return nil, "", fmt.Errorf("service.name %q matches the SDK environment of several containers (%s) in pod %s", serviceName, names(byName), kube.Ref(pod))
	case len(instrumented) == 1:
		c := &pod.Spec.Containers[instrumented[0]]
		return c, "only container with OTEL_* environment variables", nil
	case len(instrumented) == 0:
		return nil, "", fmt.Errorf("no container in pod %s has OTEL_* environment variables", kube.Ref(pod))
	default:
		return nil, "", fmt.Errorf("can't tell which container sent this signal: its service.name %q matches none, and several containers (%s) in pod %s have OTEL_* variables", serviceName, names(instrumented), kube.Ref(pod))
	}
}

// envDetail describes the variable that changed, before and after.
func envDetail(env string, old, nw otelenv.Resource, resolvedNote string) map[string]any {
	d := map[string]any{"env": env, "snapshot": rawOrNil(old, env), "config": rawOrNil(nw, env)}
	if resolvedNote != "" {
		d["resolved"] = resolvedNote
	}
	return d
}

func rawOrNil(r otelenv.Resource, env string) any {
	if v, ok := r.Raw[env]; ok {
		return v
	}
	return nil
}

// resolved explains the $(VAR) references in a raw value, e.g.
// "$(POD_UID) -> fieldRef metadata.uid".
func resolved(raw string, env kube.Env) string {
	var parts []string
	seen := map[string]bool{}
	for _, ref := range kube.References(raw) {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		src := "not defined earlier, kept as written"
		if v, found, _ := env.Get(ref); found {
			src = v.Source
		}
		parts = append(parts, "$("+ref+") -> "+src)
	}
	return strings.Join(parts, ", ")
}
