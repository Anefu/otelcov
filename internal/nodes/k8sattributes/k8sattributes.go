// Package k8sattributes models the collector's k8sattributes processor.
//
// It's a custom node, not the real processor: its decisions depend on pod data,
// and the decision that matters most (not naming the container in a
// multi-container pod) is a non-event that comparing input and output can't
// explain. The model follows collector-contrib 0.134's processor.go and
// internal/kube/client.go:
//
//   - pods are associated by the pod_association rules, in order;
//   - attributes already on the resource are kept, unless their value is
//     empty (container.id is kept even when empty);
//   - container-level attributes need the container identified: by
//     k8s.container.name or container.id on the resource, or because the pod
//     has exactly one container, counting init containers (see containers).
package k8sattributes

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"go.opentelemetry.io/collector/pdata/pcommon"
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

// defaultMetadata is extracted when extract.metadata is not set: the
// attributes enabled by default in contrib 0.134's metadata.yaml.
var defaultMetadata = []string{
	"k8s.namespace.name", "k8s.pod.name", "k8s.pod.uid", "k8s.pod.start_time",
	"k8s.deployment.name", "k8s.node.name", "container.image.name", "container.image.tag",
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

	id := identifyContainer(pod, attrs, n.specNames())
	for _, key := range n.cfg.Extract.Metadata {
		// The processor keeps an attribute that's already set, unless its
		// value is empty. container.id is kept whenever it's present.
		v, present := str(key)
		if present && (v != "" || key == "container.id") {
			reason := "already on the input resource"
			if key == "k8s.container.name" && id.name != "" {
				reason += fmt.Sprintf("; identifies container %s of %d", id.name, len(pod.Spec.Containers)+len(pod.Spec.InitContainers))
			}
			emit(lineage.Event{Attribute: key, Action: lineage.Kept, Value: v, Reason: reason})
			continue
		}
		val, reason, err := extract(key, pod, id, attrs)
		switch {
		case err != nil:
			emit(lineage.Event{Attribute: key, Action: lineage.Unverified, Reason: err.Error()})
		case val == "":
			emit(lineage.Event{Attribute: key, Action: lineage.Skipped, Reason: reason})
		default:
			if present {
				reason = strings.TrimPrefix(reason+"; ", "; ") + "replaces the empty value on the input resource"
			}
			attrs.PutStr(key, val)
			emit(lineage.Event{Attribute: key, Action: lineage.Set, Value: val, Reason: reason})
		}
	}
	return events
}

// specNames reports whether the processor's container lookup includes the
// names from the pod spec, which it does when image or service attributes
// are extracted (client.go, extractPodContainersAttributes).
func (n *Node) specNames() bool {
	for _, k := range n.cfg.Extract.Metadata {
		switch k {
		case "container.image.name", "container.image.tag", "service.version", "service.instance.id":
			return true
		}
	}
	return false
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

// containers is the processor's per-pod container lookup (contrib 0.134,
// internal/kube/client.go, extractPodContainersAttributes): every container
// and init container in the pod status, by name and by container ID with its
// runtime prefix removed, plus the pod spec's names when specNames is set.
type containers struct {
	byName map[string]bool
	byID   map[string]string // container ID -> name
}

func podContainers(pod *corev1.Pod, specNames bool) containers {
	cs := containers{byName: map[string]bool{}, byID: map[string]string{}}
	if specNames {
		for _, c := range pod.Spec.Containers {
			cs.byName[c.Name] = true
		}
		for _, c := range pod.Spec.InitContainers {
			cs.byName[c.Name] = true
		}
	}
	for _, st := range allStatuses(pod) {
		cs.byName[st.Name] = true
		// A container without an ID yet is stored under "", as upstream does:
		// several such containers collapse into one entry, the last one.
		cs.byID[kube.ContainerID(st.ContainerID)] = st.Name
	}
	return cs
}

func allStatuses(pod *corev1.Pod) []corev1.ContainerStatus {
	return append(append([]corev1.ContainerStatus(nil), pod.Status.ContainerStatuses...), pod.Status.InitContainerStatuses...)
}

// identified is the container that sent the signal, or why it can't be told.
type identified struct {
	name string // empty when not identified
	how  string
	why  string // why not, when name is empty
}

// identifyContainer works out which container sent the signal, the way the
// processor's addContainerAttributes does (processor.go): by the resource's
// k8s.container.name, else its container.id (string values only, compared as
// given), else the pod's only container.
func identifyContainer(pod *corev1.Pod, attrs pcommon.Map, specNames bool) identified {
	cs := podContainers(pod, specNames)
	name, id := stringAttr(attrs, "k8s.container.name"), stringAttr(attrs, "container.id")
	switch {
	case name != "":
		if cs.byName[name] {
			return identified{name: name, how: "k8s.container.name on the resource"}
		}
		return identified{why: fmt.Sprintf("the resource's `k8s.container.name` is %q, which isn't a container in the pod's status", name)}
	case id != "":
		if n, ok := cs.byID[id]; ok {
			return identified{name: n, how: "container.id on the resource"}
		}
		return identified{why: fmt.Sprintf("the resource's `container.id` %q matches no container in the pod's status", id)}
	case len(cs.byID) == 1:
		for idv, n := range cs.byID {
			if idv == "" && len(cs.byName) > 1 {
				return identified{name: n, how: "no container in the pod's status has an ID yet, and the processor then takes the last one listed"}
			}
			return identified{name: n, how: "pod has a single container"}
		}
	case len(cs.byName) == 1:
		for n := range cs.byName {
			return identified{name: n, how: "pod has a single container"}
		}
	}
	if len(cs.byName) == 0 {
		return identified{why: "the pod's status lists no containers yet, so the container can't be identified"}
	}
	return identified{why: fmt.Sprintf("pod has %d containers (%s) and the resource has neither `container.id` nor `k8s.container.name`, so the container can't be identified",
		len(cs.byName), strings.Join(containerNames(pod, cs), ", "))}
}

// containerNames lists the lookup's names: spec containers first, then init
// containers (marked), then any others, in order.
func containerNames(pod *corev1.Pod, cs containers) []string {
	var out []string
	seen := map[string]bool{}
	add := func(name, label string) {
		if cs.byName[name] && !seen[name] {
			seen[name] = true
			out = append(out, label)
		}
	}
	for _, c := range pod.Spec.Containers {
		add(c.Name, c.Name)
	}
	for _, c := range pod.Spec.InitContainers {
		add(c.Name, c.Name+" (init container)")
	}
	for _, st := range allStatuses(pod) {
		add(st.Name, st.Name)
	}
	return out
}

// stringAttr is the processor's stringAttributeFromMap: the value if it's a
// string, else "".
func stringAttr(attrs pcommon.Map, key string) string {
	if v, ok := attrs.Get(key); ok && v.Type() == pcommon.ValueTypeStr {
		return v.Str()
	}
	return ""
}

// extract returns an attribute's value for the pod. An empty value means the
// processor wouldn't set it, and reason says why.
func extract(key string, pod *corev1.Pod, id identified, attrs pcommon.Map) (value, reason string, err error) {
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
		if id.name == "" {
			return "", id.why, nil
		}
		return containerValue(key, pod, id, attrs)
	}
	return "", "", fmt.Errorf("%s isn't modeled yet", key)
}

func containerValue(key string, pod *corev1.Pod, id identified, attrs pcommon.Map) (string, string, error) {
	switch key {
	case "k8s.container.name":
		return id.name, id.how, nil
	case "container.id":
		// The processor picks the container's status by the resource's
		// k8s.container.restart_count, else the latest one, and sets its ID.
		var st *corev1.ContainerStatus
		for _, s := range allStatuses(pod) {
			if s.Name == id.name {
				st = &s
				break
			}
		}
		if st == nil {
			return "", "container " + id.name + " has no status in the pod", nil
		}
		if v, ok := attrs.Get("k8s.container.restart_count"); ok {
			run, err := intAttr(v)
			if err != nil {
				return "", "the resource's `k8s.container.restart_count` isn't a number, so the processor can't pick a container run", nil
			}
			if run != int(st.RestartCount) {
				return "", fmt.Sprintf("the resource's `k8s.container.restart_count` is %d, but the pod status has container %s at restart %d", run, id.name, st.RestartCount), nil
			}
		}
		if st.ContainerID == "" {
			return "", "container " + id.name + " has no container ID in the pod status", nil
		}
		return kube.ContainerID(st.ContainerID), id.how, nil
	}
	return "", "", fmt.Errorf("%s isn't modeled yet", key)
}

// intAttr is the processor's intFromAttribute.
func intAttr(v pcommon.Value) (int, error) {
	switch v.Type() {
	case pcommon.ValueTypeInt:
		return int(v.Int()), nil
	case pcommon.ValueTypeStr:
		return strconv.Atoi(v.Str())
	}
	return 0, fmt.Errorf("wrong attribute type %v", v.Type())
}
