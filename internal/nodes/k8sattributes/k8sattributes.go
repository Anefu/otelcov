// Package k8sattributes models the collector's k8sattributes processor.
//
// It's a custom node, not the real processor: its decisions depend on pod data,
// and the decision that matters most (not naming the container in a
// multi-container pod) is a non-event that comparing input and output can't
// explain. The model follows the processor's documented behavior:
//
//   - pods are associated by the pod_association rules, in order;
//   - attributes already on the resource are kept, never overwritten;
//   - container-level attributes need the container identified: by
//     k8s.container.name or container.id on the resource, or because the pod
//     has a single container.
package k8sattributes

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/Anefu/otelcov/internal/collector"
	"github.com/Anefu/otelcov/internal/graph"
	"github.com/Anefu/otelcov/internal/kube"
	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/unit"
)

// Config is the subset of the processor's config the node models.
type Config struct {
	Passthrough    bool              `json:"passthrough"`
	AuthType       string            `json:"auth_type"`
	PodAssociation []AssociationRule `json:"pod_association"`
	Extract        struct {
		Metadata []string `json:"metadata"`
	} `json:"extract"`
}

// AssociationRule matches when all its sources resolve to the same pod.
type AssociationRule struct {
	Sources []Source `json:"sources"`
}

// Source is one association source.
type Source struct {
	From string `json:"from"`
	Name string `json:"name"`
}

// defaultMetadata is extracted when extract.metadata is not set.
var defaultMetadata = []string{
	"k8s.namespace.name", "k8s.pod.name", "k8s.pod.uid", "k8s.pod.start_time",
	"k8s.deployment.name", "k8s.node.name",
}

// defaultAssociation is used when pod_association is not set.
var defaultAssociation = []AssociationRule{
	{Sources: []Source{{From: "resource_attribute", Name: "k8s.pod.ip"}}},
	{Sources: []Source{{From: "resource_attribute", Name: "k8s.pod.uid"}}},
	{Sources: []Source{{From: "connection"}}},
}

// Node is the k8sattributes node.
type Node struct {
	name string
	cfg  Config
	pods *kube.Pods
}

// New builds the node from the processor's raw config and the pods it sees.
// name is the node name in lineage, e.g. "collector/k8sattributes".
func New(name string, raw map[string]any, pods []corev1.Pod) (*Node, error) {
	var cfg Config
	if err := collector.Decode(raw, &cfg); err != nil {
		return nil, fmt.Errorf("unsupported k8sattributes config: %w", err)
	}
	if len(cfg.PodAssociation) == 0 {
		cfg.PodAssociation = defaultAssociation
	}
	if cfg.Extract.Metadata == nil {
		cfg.Extract.Metadata = defaultMetadata
	}
	return &Node{name: name, cfg: cfg, pods: kube.NewPods(pods)}, nil
}

func (n *Node) Name() string { return n.name }

func (n *Node) ProcessMetrics(_ context.Context, in []unit.Metric) (graph.Output, error) {
	out := graph.Output{Items: in}
	if n.cfg.Passthrough {
		// Passthrough only records k8s.pod.ip from the connection, which a
		// replay doesn't have; the tap already recorded it.
		return out, nil
	}
	for _, it := range in {
		out.Events = append(out.Events, n.process(it)...)
	}
	return out, nil
}

func (n *Node) process(it unit.Metric) []lineage.Event {
	attrs := unit.Resource(it.Data).Attributes()
	var events []lineage.Event
	emit := func(e lineage.Event) {
		e.Node, e.Signal = n.name, it.ID
		events = append(events, e)
	}
	str := func(k string) (string, bool) {
		v, ok := attrs.Get(k)
		if !ok {
			return "", false
		}
		return v.AsString(), true
	}

	pod, via := n.associate(str)
	if pod == nil {
		emit(lineage.Event{Action: lineage.Skipped, Reason: "no pod matched the pod_association rules, so no attributes were added"})
		return events
	}
	emit(lineage.Event{Action: lineage.Associated, Detail: map[string]any{"pod": kube.Ref(pod), "via": via}})

	container, how := identifyContainer(pod, str)
	for _, key := range n.cfg.Extract.Metadata {
		if v, ok := str(key); ok {
			reason := "already on the input resource"
			if key == "k8s.container.name" && container != nil {
				reason += fmt.Sprintf("; identifies container %s of %d", container.Name, len(pod.Spec.Containers))
			}
			emit(lineage.Event{Attribute: key, Action: lineage.Kept, Value: v, Reason: reason})
			continue
		}
		val, reason, err := extract(key, pod, container, how)
		switch {
		case err != nil:
			emit(lineage.Event{Attribute: key, Action: lineage.Unverified, Reason: err.Error()})
		case val == "":
			emit(lineage.Event{Attribute: key, Action: lineage.Skipped, Reason: reason})
		default:
			attrs.PutStr(key, val)
			emit(lineage.Event{Attribute: key, Action: lineage.Set, Value: val, Reason: reason})
		}
	}
	return events
}

// associate applies the pod_association rules in order.
func (n *Node) associate(str func(string) (string, bool)) (*corev1.Pod, string) {
rules:
	for _, rule := range n.cfg.PodAssociation {
		values := map[string]string{}
		var vias []string
		for _, s := range rule.Sources {
			if s.From != "resource_attribute" {
				// "connection" needs the client address, which a replay
				// doesn't have.
				continue rules
			}
			v, ok := str(s.Name)
			if !ok {
				continue rules
			}
			values[s.Name] = v
			vias = append(vias, "resource_attribute "+s.Name+"="+v)
		}
		if pod := n.lookup(values); pod != nil {
			return pod, strings.Join(vias, " + ")
		}
	}
	return nil, ""
}

// lookup finds the pod that all of a rule's source values point to.
func (n *Node) lookup(values map[string]string) *corev1.Pod {
	var found *corev1.Pod
	match := func(p *corev1.Pod, ok bool) bool {
		if !ok || (found != nil && found != p) {
			return false
		}
		found = p
		return true
	}
	for k, v := range values {
		var ok bool
		switch k {
		case "k8s.pod.ip":
			ok = match(n.pods.ByIP(v))
		case "k8s.pod.uid":
			ok = match(n.pods.ByUID(v))
		case "container.id":
			ok = match(n.pods.ByContainerID(v))
		case "k8s.pod.name":
			ns, hasNS := values["k8s.namespace.name"]
			if !hasNS {
				return nil
			}
			ok = match(n.pods.ByName(ns, v))
		case "k8s.namespace.name":
			ok = true // used together with k8s.pod.name
		default:
			return nil
		}
		if !ok {
			return nil
		}
	}
	return found
}

// identifyContainer works out which container sent the signal, if possible.
func identifyContainer(pod *corev1.Pod, str func(string) (string, bool)) (*corev1.Container, string) {
	if name, ok := str("k8s.container.name"); ok {
		if c, ok := kube.Container(pod, name); ok {
			return c, "k8s.container.name on the resource"
		}
		return nil, ""
	}
	if id, ok := str("container.id"); ok {
		for _, cs := range pod.Status.ContainerStatuses {
			if kube.ContainerID(cs.ContainerID) == kube.ContainerID(id) {
				if c, ok := kube.Container(pod, cs.Name); ok {
					return c, "container.id on the resource"
				}
			}
		}
		return nil, ""
	}
	if len(pod.Spec.Containers) == 1 {
		return &pod.Spec.Containers[0], "pod has a single container"
	}
	return nil, ""
}

// extract returns an attribute's value for the pod. An empty value means the
// processor wouldn't set it, and reason says why.
func extract(key string, pod *corev1.Pod, c *corev1.Container, how string) (value, reason string, err error) {
	owner := func(kind string) (string, string) {
		for _, o := range pod.OwnerReferences {
			if o.Kind == kind {
				return o.Name, ""
			}
		}
		return "", "pod has no " + kind + " owner"
	}
	switch key {
	case "k8s.namespace.name":
		return pod.Namespace, "", nil
	case "k8s.pod.name":
		return pod.Name, "", nil
	case "k8s.pod.uid":
		return string(pod.UID), "", nil
	case "k8s.node.name":
		return pod.Spec.NodeName, "", nil
	case "k8s.replicaset.name":
		v, r := owner("ReplicaSet")
		return v, r, nil
	case "k8s.statefulset.name":
		v, r := owner("StatefulSet")
		return v, r, nil
	case "k8s.daemonset.name":
		v, r := owner("DaemonSet")
		return v, r, nil
	case "k8s.job.name":
		v, r := owner("Job")
		return v, r, nil
	case "k8s.deployment.name":
		rs, r := owner("ReplicaSet")
		if rs == "" {
			return "", r, nil
		}
		hash := pod.Labels["pod-template-hash"]
		if hash == "" || !strings.HasSuffix(rs, "-"+hash) {
			return "", "can't derive the Deployment from ReplicaSet " + rs + " without its pod-template-hash", nil
		}
		return strings.TrimSuffix(rs, "-"+hash), "", nil
	case "k8s.container.name", "container.id", "container.image.name", "container.image.tag":
		if c == nil {
			return "", fmt.Sprintf("pod has %d containers (%s) and the resource has neither container.id nor k8s.container.name, so the container can't be identified",
				len(pod.Spec.Containers), strings.Join(kube.ContainerNames(pod), ", ")), nil
		}
		return containerValue(key, pod, c, how)
	}
	return "", "", fmt.Errorf("%s isn't modeled yet", key)
}

func containerValue(key string, pod *corev1.Pod, c *corev1.Container, how string) (string, string, error) {
	switch key {
	case "k8s.container.name":
		return c.Name, how, nil
	case "container.id":
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name == c.Name && cs.ContainerID != "" {
				return kube.ContainerID(cs.ContainerID), how, nil
			}
		}
		return "", "container " + c.Name + " has no container ID in the pod status", nil
	}
	return "", "", fmt.Errorf("%s isn't modeled yet", key)
}
