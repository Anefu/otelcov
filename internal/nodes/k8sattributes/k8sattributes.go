// Package k8sattributes models the collector's k8sattributes processor.
//
// It's a custom node, not the real processor: its decisions depend on pod data,
// and the decision that matters most (not naming the container in a
// multi-container pod) is a non-event that comparing input and output can't
// explain. The model follows collector-contrib 0.134's processor.go and
// internal/kube/client.go:
//
//   - pods are associated by the pod_association rules, in order;
//   - attributes come in the processor's order: the pod's (metadata, then
//     label and annotation rules), its container's, then the namespace's and
//     the node's. Each is only set if the resource doesn't have it yet or has
//     it empty (container.id and container.image.repo_digests are kept even
//     when empty), so an earlier source wins; service.namespace is the
//     exception, always set to the namespace;
//   - container-level attributes need the container identified: by
//     k8s.container.name or container.id on the resource, or because the pod
//     has exactly one container, counting init containers (see containers);
//   - namespace and node attributes are added even when no pod matched, from
//     the resource's k8s.namespace.name and k8s.node.name.
package k8sattributes

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

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
	Extract        Extract           `json:"extract"`
}

// Extract says which attributes to add.
type Extract struct {
	Metadata    []string       `json:"metadata"`
	Labels      []FieldExtract `json:"labels"`
	Annotations []FieldExtract `json:"annotations"`
	// OtelAnnotations adds the pod's resource.opentelemetry.io/<name>
	// annotations as attribute <name>.
	OtelAnnotations bool `json:"otel_annotations"`
}

// FieldExtract copies a label or annotation of the pod, its namespace or its
// node.
type FieldExtract struct {
	TagName  string `json:"tag_name"`
	Key      string `json:"key"`
	KeyRegex string `json:"key_regex"`
	From     string `json:"from"`
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

// supportedMetadata are the extract.metadata fields the processor accepts
// (config.go, Validate); anything else is a config error.
var supportedMetadata = map[string]bool{
	"k8s.namespace.name": true, "k8s.pod.name": true, "k8s.pod.uid": true, "k8s.pod.hostname": true,
	"k8s.pod.start_time": true, "k8s.pod.ip": true, "k8s.deployment.name": true, "k8s.deployment.uid": true,
	"k8s.replicaset.name": true, "k8s.replicaset.uid": true, "k8s.daemonset.name": true, "k8s.daemonset.uid": true,
	"k8s.statefulset.name": true, "k8s.statefulset.uid": true, "k8s.job.name": true, "k8s.job.uid": true,
	"k8s.cronjob.name": true, "k8s.node.name": true, "k8s.node.uid": true, "k8s.container.name": true,
	"container.id": true, "container.image.name": true, "container.image.tag": true,
	"service.namespace": true, "service.name": true, "service.version": true, "service.instance.id": true,
	"container.image.repo_digests": true, "k8s.cluster.uid": true,
}

// containerKeys are set from the identified container (addContainerAttributes).
var containerKeys = map[string]bool{
	"k8s.container.name": true, "container.id": true, "container.image.name": true, "container.image.tag": true,
	"container.image.repo_digests": true, "service.instance.id": true, "service.version": true,
}

// defaultAssociation is used when pod_association is not set.
var defaultAssociation = []AssociationRule{
	{Sources: []Source{{From: "resource_attribute", Name: "k8s.pod.ip"}}},
	{Sources: []Source{{From: "resource_attribute", Name: "k8s.pod.uid"}}},
	{Sources: []Source{{From: "connection"}}},
}

// Node is the k8sattributes node.
type Node struct {
	name     string
	cfg      Config
	metadata map[string]bool
	rules    []rule
	pods     *kube.Pods
	objs     kube.Objects
}

// New builds the node from the processor's raw config and the cluster's
// objects. name is the node name in lineage, e.g. "collector/k8sattributes".
func New(name string, raw map[string]any, objs kube.Objects) (*Node, error) {
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
	n := &Node{name: name, cfg: cfg, metadata: map[string]bool{}, pods: kube.NewPods(objs.Pods), objs: objs}
	for _, k := range cfg.Extract.Metadata {
		if !supportedMetadata[k] {
			return nil, fmt.Errorf("k8sattributes config: %q is not a supported metadata field", k)
		}
		n.metadata[k] = true
	}
	var err error
	if n.rules, err = compileRules(cfg.Extract); err != nil {
		return nil, err
	}
	return n, nil
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

	ps := newProposals(n.cfg.Extract.Metadata)
	pod, via := n.associate(str)
	var id identified
	if pod == nil {
		emit(lineage.Event{Action: lineage.Skipped, Reason: "no pod matched the pod_association rules, so no attributes were added"})
	} else {
		emit(lineage.Event{Action: lineage.Associated, Detail: map[string]any{"pod": kube.Ref(pod), "via": via}})
		id = identifyContainer(pod, attrs, n.specNames())
		n.podStage(ps, pod)
		n.containerStage(ps, pod, id, attrs)
	}
	ns := stringAttr(attrs, "k8s.namespace.name")
	if pod != nil {
		ns = pod.Namespace
	}
	if ns != "" {
		n.namespaceStage(ps, ns)
	}
	node := stringAttr(attrs, "k8s.node.name")
	if pod != nil && pod.Spec.NodeName != "" {
		node = pod.Spec.NodeName
	}
	if node != "" {
		n.nodeStage(ps, node)
	}

	for _, e := range ps.resolve(attrs, pod, id) {
		emit(e)
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
	// status names the containers with a status; only those get a name,
	// a service.instance.id and a container ID.
	status map[string]bool
}

func podContainers(pod *corev1.Pod, specNames bool) containers {
	cs := containers{byName: map[string]bool{}, byID: map[string]string{}, status: map[string]bool{}}
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
		cs.status[st.Name] = true
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
	name      string // empty when not identified
	how       string
	why       string // why not, when name is empty
	hasStatus bool
}

// identifyContainer works out which container sent the signal, the way the
// processor's addContainerAttributes does (processor.go): by the resource's
// k8s.container.name, else its container.id (string values only, compared as
// given), else the pod's only container.
func identifyContainer(pod *corev1.Pod, attrs pcommon.Map, specNames bool) identified {
	cs := podContainers(pod, specNames)
	found := func(name, how string) identified {
		return identified{name: name, how: how, hasStatus: cs.status[name]}
	}
	name, id := stringAttr(attrs, "k8s.container.name"), stringAttr(attrs, "container.id")
	switch {
	case name != "":
		if cs.byName[name] {
			return found(name, "k8s.container.name on the resource")
		}
		return identified{why: fmt.Sprintf("the resource's `k8s.container.name` is %q, which isn't a container in the pod's status", name)}
	case id != "":
		if n, ok := cs.byID[id]; ok {
			return found(n, "container.id on the resource")
		}
		return identified{why: fmt.Sprintf("the resource's `container.id` %q matches no container in the pod's status", id)}
	case len(cs.byID) == 1:
		for idv, n := range cs.byID {
			if idv == "" && len(cs.byName) > 1 {
				return found(n, "no container in the pod's status has an ID yet, and the processor then takes the last one listed")
			}
			return found(n, "pod has a single container")
		}
	case len(cs.byName) == 1:
		for n := range cs.byName {
			return found(n, "pod has a single container")
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

// podStage proposes the pod's attributes (client.go, extractPodAttributes):
// metadata fields, then label rules, the service attributes taken from
// well-known labels, and annotation rules. A later write replaces an earlier
// one for the same attribute, as in the processor's map.
func (n *Node) podStage(ps *proposals, pod *corev1.Pod) {
	for _, key := range n.cfg.Extract.Metadata {
		if containerKeys[key] {
			// service.version too: here it only comes from a label (below);
			// the image's comes with the container.
			continue
		}
		if p, ok := n.podValue(key, pod); ok {
			ps.add(stagePod, key, p)
		}
	}
	for _, r := range n.rules {
		if r.from == fromPod && r.field == "labels" {
			r.propose(ps, stagePod, pod.Labels, "the pod's")
		}
	}
	if n.metadata["service.name"] {
		for _, l := range []string{"app.kubernetes.io/name", "app.kubernetes.io/instance"} {
			if v, ok := pod.Labels[l]; ok {
				ps.add(stagePod, "service.name", write(v, "from the pod's label `"+l+"`"))
			}
		}
	}
	if n.metadata["service.version"] {
		if v, ok := pod.Labels["app.kubernetes.io/version"]; ok {
			ps.add(stagePod, "service.version", write(v, "from the pod's label `app.kubernetes.io/version`"))
		}
	}
	for _, r := range n.rules {
		if r.from == fromPod && r.field == "annotations" {
			r.propose(ps, stagePod, pod.Annotations, "the pod's")
		}
	}
}

// podValue is one metadata field of the pod. ok is false for fields that
// come from elsewhere.
func (n *Node) podValue(key string, pod *corev1.Pod) (proposal, bool) {
	owner := func(kind string) (*ownerRef, string) {
		var found *ownerRef
		for _, o := range pod.OwnerReferences {
			if o.Kind == kind {
				found = &ownerRef{name: o.Name, uid: string(o.UID)}
			}
		}
		if found == nil {
			return nil, "pod has no " + kind + " owner"
		}
		return found, ""
	}
	ownerName := func(kind string) proposal {
		o, why := owner(kind)
		if o == nil {
			return skip(why)
		}
		return write(o.name, "")
	}
	ownerUID := func(kind string) proposal {
		o, why := owner(kind)
		if o == nil {
			return skip(why)
		}
		return write(o.uid, "")
	}
	switch key {
	case "k8s.namespace.name", "service.namespace":
		return write(pod.Namespace, ""), true
	case "k8s.pod.name":
		return write(pod.Name, ""), true
	case "k8s.pod.uid":
		return write(string(pod.UID), ""), true
	case "k8s.pod.hostname":
		if pod.Spec.Hostname == "" {
			return write("", "the pod spec sets no hostname, so the processor sets an empty value"), true
		}
		return write(pod.Spec.Hostname, ""), true
	case "k8s.pod.ip":
		if pod.Status.PodIP == "" {
			return write("", "the pod status has no IP yet, so the processor sets an empty value"), true
		}
		return write(pod.Status.PodIP, ""), true
	case "k8s.pod.start_time":
		if pod.CreationTimestamp.IsZero() {
			return skip("the pod has no `creationTimestamp`"), true
		}
		// The processor formats the time in its own time zone; collectors
		// run in UTC unless TZ is set.
		return write(pod.CreationTimestamp.UTC().Format(time.RFC3339Nano), "the pod's `creationTimestamp`, in UTC"), true
	case "k8s.node.name":
		return write(pod.Spec.NodeName, ""), true
	case "k8s.replicaset.name":
		return ownerName("ReplicaSet"), true
	case "k8s.replicaset.uid":
		return ownerUID("ReplicaSet"), true
	case "k8s.daemonset.name":
		return ownerName("DaemonSet"), true
	case "k8s.daemonset.uid":
		return ownerUID("DaemonSet"), true
	case "k8s.statefulset.name":
		return ownerName("StatefulSet"), true
	case "k8s.statefulset.uid":
		return ownerUID("StatefulSet"), true
	case "k8s.job.name":
		return ownerName("Job"), true
	case "k8s.job.uid":
		return ownerUID("Job"), true
	case "k8s.cronjob.name":
		o, why := owner("Job")
		if o == nil {
			return skip(why), true
		}
		if m := cronJobJob.FindStringSubmatch(o.name); m != nil {
			return write(m[1], "from Job "+o.name), true
		}
		return skip("Job " + o.name + " isn't named like a CronJob's job (`<cronjob>-<number>`)"), true
	case "k8s.deployment.name":
		name, why := deployment(pod)
		if name == "" {
			return skip(why), true
		}
		return write(name, ""), true
	case "k8s.deployment.uid":
		if o, why := owner("ReplicaSet"); o == nil {
			return skip(why), true
		}
		return unknown("the Deployment's UID comes from the pod's ReplicaSet object, which otelcov doesn't load"), true
	case "k8s.cluster.uid":
		if len(n.objs.Namespaces) == 0 {
			return unknown("the cluster UID is the `kube-system` namespace's UID, and the cluster file has no Namespace objects"), true
		}
		ks, ok := n.objs.Namespace("kube-system")
		if !ok {
			return unknown("the cluster UID is the `kube-system` namespace's UID, and the cluster file has no `kube-system` namespace"), true
		}
		return write(string(ks.UID), "the `kube-system` namespace's UID"), true
	case "service.name":
		return serviceName(pod), true
	}
	return proposal{}, false
}

type ownerRef struct{ name, uid string }

// cronJobJob is the processor's cronJobRegex: a CronJob's jobs are named
// <cronjob>-<number>.
var cronJobJob = regexp.MustCompile(`^(.*)-\d+$`)

// deployment derives the Deployment from the pod's ReplicaSet owner and its
// pod-template-hash label. The processor reads it from the ReplicaSet object;
// a ReplicaSet made by a Deployment is named <deployment>-<pod-template-hash>.
func deployment(pod *corev1.Pod) (string, string) {
	var rs string
	for _, o := range pod.OwnerReferences {
		if o.Kind == "ReplicaSet" {
			rs = o.Name
		}
	}
	if rs == "" {
		return "", "pod has no ReplicaSet owner"
	}
	hash := pod.Labels["pod-template-hash"]
	if hash == "" || !strings.HasSuffix(rs, "-"+hash) {
		return "", "can't derive the Deployment from ReplicaSet " + rs + " without its pod-template-hash"
	}
	return strings.TrimSuffix(rs, "-"+hash), ""
}

// serviceName is the processor's service.name before the label rules: the
// pod's name, replaced by its owners' (the Deployment over the ReplicaSet,
// the CronJob over the Job). The app.kubernetes.io labels come after.
func serviceName(pod *corev1.Pod) proposal {
	p := write(pod.Name, "the pod's name")
	for _, o := range pod.OwnerReferences {
		switch o.Kind {
		case "ReplicaSet":
			p = write(o.Name, "from ReplicaSet "+o.Name)
			if d, why := deployment(pod); d != "" {
				p = write(d, "from Deployment "+d)
			} else if strings.HasPrefix(why, "can't derive") {
				// The processor takes the Deployment from the ReplicaSet
				// object, if it has one.
				p = unknown("the processor names the service after ReplicaSet " + o.Name + "'s Deployment, if it has one, and otelcov " + why)
			}
		case "DaemonSet", "StatefulSet":
			p = write(o.Name, "from "+o.Kind+" "+o.Name)
		case "Job":
			p = write(o.Name, "from Job "+o.Name)
			if m := cronJobJob.FindStringSubmatch(o.Name); m != nil {
				p = write(m[1], "from CronJob "+m[1])
			}
		}
	}
	return p
}

// containerStage proposes the identified container's attributes
// (processor.go, addContainerAttributes).
func (n *Node) containerStage(ps *proposals, pod *corev1.Pod, id identified, attrs pcommon.Map) {
	for _, key := range n.cfg.Extract.Metadata {
		if !containerKeys[key] {
			continue
		}
		if id.name == "" {
			ps.add(stageContainer, key, skip(id.why))
			continue
		}
		ps.add(stageContainer, key, containerValue(key, pod, id, attrs))
	}
}

func containerValue(key string, pod *corev1.Pod, id identified, attrs pcommon.Map) proposal {
	spec := func() (*corev1.Container, bool) {
		for _, list := range [][]corev1.Container{pod.Spec.Containers, pod.Spec.InitContainers} {
			for i := range list {
				if list[i].Name == id.name {
					return &list[i], true
				}
			}
		}
		return nil, false
	}
	noStatus := "container " + id.name + " has no status in the pod yet"
	switch key {
	case "k8s.container.name":
		if !id.hasStatus {
			return skip(noStatus + ", and the processor names containers from their status")
		}
		return write(id.name, id.how)
	case "service.instance.id":
		if !id.hasStatus {
			return skip(noStatus)
		}
		return write(pod.Namespace+"."+pod.Name+"."+id.name, "<namespace>.<pod>.<container>")
	case "container.image.name", "container.image.tag", "service.version":
		c, ok := spec()
		if !ok {
			return skip("container " + id.name + " isn't in the pod spec, so its image isn't known")
		}
		img, err := parseImageName(c.Image)
		if err != nil {
			return skip(fmt.Sprintf("the processor can't parse container %s's image %q: %v", id.name, c.Image, err))
		}
		switch key {
		case "container.image.name":
			return write(img.repository, "from image `"+c.Image+"`")
		case "container.image.tag":
			if !img.tagged {
				return write(img.tag, "image `"+c.Image+"` has no tag, so the processor sets `latest`")
			}
			return write(img.tag, "from image `"+c.Image+"`")
		}
		v, err := serviceVersionFromImage(c.Image)
		if err != nil {
			return skip("image `" + c.Image + "` has neither a tag nor a digest")
		}
		return write(v, "from image `"+c.Image+"`")
	case "container.id", "container.image.repo_digests":
		st, why := containerStatus(pod, id, attrs)
		if st == nil {
			return skip(why)
		}
		if key == "container.id" {
			if st.ContainerID == "" {
				return skip("container " + id.name + " has no container ID in the pod status")
			}
			return write(kube.ContainerID(st.ContainerID), id.how)
		}
		d, err := canonicalImageRef(st.ImageID)
		if err != nil || d == "" {
			return skip(fmt.Sprintf("container %s's status `imageID` %q isn't a canonical image reference", id.name, st.ImageID))
		}
		p := write(d, "from the container status's `imageID`")
		p.slice = true
		return p
	}
	return unknown(key + " isn't modeled yet")
}

// containerStatus picks the container's status the way the processor picks a
// run: by the resource's k8s.container.restart_count, else the latest. A pod
// object only has the current run.
func containerStatus(pod *corev1.Pod, id identified, attrs pcommon.Map) (*corev1.ContainerStatus, string) {
	var st *corev1.ContainerStatus
	for _, s := range allStatuses(pod) {
		if s.Name == id.name {
			st = &s
			break
		}
	}
	if st == nil {
		return nil, "container " + id.name + " has no status in the pod"
	}
	if v, ok := attrs.Get("k8s.container.restart_count"); ok {
		run, err := intAttr(v)
		if err != nil {
			return nil, "the resource's `k8s.container.restart_count` isn't a number, so the processor can't pick a container run"
		}
		if run != int(st.RestartCount) {
			return nil, fmt.Sprintf("the resource's `k8s.container.restart_count` is %d, but the pod status has container %s at restart %d", run, id.name, st.RestartCount)
		}
	}
	return st, ""
}

// namespaceStage proposes the namespace's labels and annotations, and
// service.namespace, which the processor always overwrites.
func (n *Node) namespaceStage(ps *proposals, ns string) {
	obj, missing := n.object(fromNamespace, ns)
	for _, r := range n.rules {
		if r.from != fromNamespace {
			continue
		}
		if obj == nil {
			r.unknown(ps, stageNamespace, missing)
			continue
		}
		src := obj.Labels
		if r.field == "annotations" {
			src = obj.Annotations
		}
		r.propose(ps, stageNamespace, src, "namespace `"+ns+"`'s")
	}
	if n.metadata["service.namespace"] {
		p := write(ns, "the processor always sets `service.namespace` to the namespace")
		p.overwrite = true
		ps.add(stageNamespace, "service.namespace", p)
	}
}

// nodeStage proposes the node's labels and annotations, and its UID.
func (n *Node) nodeStage(ps *proposals, name string) {
	obj, missing := n.object(fromNode, name)
	for _, r := range n.rules {
		if r.from != fromNode {
			continue
		}
		if obj == nil {
			r.unknown(ps, stageNode, missing)
			continue
		}
		src := obj.Labels
		if r.field == "annotations" {
			src = obj.Annotations
		}
		r.propose(ps, stageNode, src, "node `"+name+"`'s")
	}
	if n.metadata["k8s.node.uid"] {
		if obj == nil {
			ps.add(stageNode, "k8s.node.uid", unknown(missing))
		} else {
			ps.add(stageNode, "k8s.node.uid", write(string(obj.UID), "node `"+name+"`'s UID"))
		}
	}
}

// object finds a namespace's or node's metadata in the cluster file, or says
// why it can't.
func (n *Node) object(from, name string) (*objectMeta, string) {
	switch from {
	case fromNamespace:
		if len(n.objs.Namespaces) == 0 {
			return nil, "the cluster file has no Namespace objects, so namespace `" + name + "`'s labels and annotations aren't known"
		}
		if o, ok := n.objs.Namespace(name); ok {
			return &objectMeta{o.Labels, o.Annotations, string(o.UID)}, ""
		}
		return nil, "namespace `" + name + "` isn't in the cluster file"
	default:
		if len(n.objs.Nodes) == 0 {
			return nil, "the cluster file has no Node objects, so node `" + name + "`'s labels, annotations and UID aren't known"
		}
		if o, ok := n.objs.Node(name); ok {
			return &objectMeta{o.Labels, o.Annotations, string(o.UID)}, ""
		}
		return nil, "node `" + name + "` isn't in the cluster file"
	}
}

type objectMeta struct {
	Labels, Annotations map[string]string
	UID                 string
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

// Stages, in the order the processor applies them.
const (
	stagePod = iota
	stageContainer
	stageNamespace
	stageNode
)

type action int

const (
	actWrite   action = iota // the processor sets value
	actSkip                  // it sets nothing; reason says why
	actUnknown               // otelcov can't tell
)

// proposal is what one stage would do with one attribute.
type proposal struct {
	act    action
	value  string
	reason string
	stage  int
	slice  bool // container.image.repo_digests is a list
	// overwrite: set even when the resource has a value (service.namespace).
	overwrite bool
}

func write(v, reason string) proposal { return proposal{act: actWrite, value: v, reason: reason} }
func skip(reason string) proposal     { return proposal{act: actSkip, reason: reason} }
func unknown(reason string) proposal  { return proposal{act: actUnknown, reason: reason} }
func (p proposal) writes() bool       { return p.act != actSkip }
func (p proposal) setsValue() bool    { return p.act == actWrite && p.value != "" }

// proposals collects every stage's proposals per attribute, in event order:
// the metadata fields as configured, then other attributes as they come.
type proposals struct {
	order []string
	by    map[string][]proposal
	// notes are events without an attribute: rules whose attributes can't
	// be named because their source is missing.
	notes []lineage.Event
}

func newProposals(metadata []string) *proposals {
	ps := &proposals{by: map[string][]proposal{}}
	for _, k := range metadata {
		if _, ok := ps.by[k]; !ok {
			ps.by[k] = nil
			ps.order = append(ps.order, k)
		}
	}
	return ps
}

// add records a stage's proposal. Within a stage a write replaces an earlier
// proposal, and a skip doesn't replace a write.
func (ps *proposals) add(stage int, key string, p proposal) {
	p.stage = stage
	list, seen := ps.by[key]
	if !seen {
		ps.order = append(ps.order, key)
	}
	for i := range list {
		if list[i].stage == stage {
			if p.writes() || !list[i].writes() {
				list[i] = p
			}
			ps.by[key] = list
			return
		}
	}
	ps.by[key] = append(list, p)
}

// resolve decides each attribute the way setResourceAttribute does, applies
// the result to attrs and returns the events.
func (ps *proposals) resolve(attrs pcommon.Map, pod *corev1.Pod, id identified) []lineage.Event {
	var out []lineage.Event
	for _, key := range ps.order {
		list := ps.by[key]
		if len(list) == 0 {
			continue
		}
		v, present := attrs.Get(key)
		was := ""
		if present {
			was = v.AsString()
		}
		// container.id and the repo digests are kept whenever present.
		keepAny := key == "container.id" || key == "container.image.repo_digests"
		var ow *proposal
		for i := range list {
			if list[i].overwrite {
				ow = &list[i]
			}
		}
		switch {
		case ow != nil && was != ow.value:
			reason := ow.reason
			if present && was != "" {
				reason += ", replacing `" + was + "`"
			}
			attrs.PutStr(key, ow.value)
			out = append(out, lineage.Event{Attribute: key, Action: lineage.Set, Value: ow.value, Reason: reason})
			continue
		case present && (was != "" || keepAny):
			reason := "already on the input resource"
			if key == "k8s.container.name" && id.name != "" {
				reason += fmt.Sprintf("; identifies container %s of %d", id.name, len(pod.Spec.Containers)+len(pod.Spec.InitContainers))
			}
			out = append(out, lineage.Event{Attribute: key, Action: lineage.Kept, Value: was, Reason: reason})
			continue
		}
		// The first stage that sets a value wins; an empty value only holds
		// if no later stage sets one.
		var chosen, empty, skipped *proposal
	pick:
		for i := range list {
			p := &list[i]
			switch {
			case p.setsValue(), p.act == actUnknown:
				chosen = p
				break pick
			case p.act == actWrite && empty == nil:
				empty = p
			case p.act == actSkip && skipped == nil:
				skipped = p
			}
		}
		if chosen == nil {
			chosen = empty
		}
		switch {
		case chosen == nil:
			out = append(out, lineage.Event{Attribute: key, Action: lineage.Skipped, Reason: skipped.reason})
		case chosen.act == actUnknown:
			out = append(out, lineage.Event{Attribute: key, Action: lineage.Unverified, Reason: chosen.reason})
		default:
			reason := chosen.reason
			if present {
				reason = strings.TrimPrefix(reason+"; ", "; ") + "replaces the empty value on the input resource"
			}
			if chosen.slice {
				attrs.PutEmptySlice(key).AppendEmpty().SetStr(chosen.value)
			} else {
				attrs.PutStr(key, chosen.value)
			}
			out = append(out, lineage.Event{Attribute: key, Action: lineage.Set, Value: chosen.value, Reason: reason})
		}
	}
	return append(out, ps.notes...)
}

// Label and annotation rule sources.
const (
	fromPod       = "pod"
	fromNamespace = "namespace"
	fromNode      = "node"
)

// rule is a compiled label or annotation rule (the processor's
// FieldExtractionRule).
type rule struct {
	field  string // labels or annotations
	from   string
	name   string // the attribute, or with a key_regex its template
	key    string
	regex  *regexp.Regexp
	hasRef bool
}

// compileRules is the processor's extractFieldRules, with its Validate
// checks. Rules on Deployments and StatefulSets need those objects, which
// otelcov doesn't load.
func compileRules(e Extract) ([]rule, error) {
	var out []rule
	add := func(field string, list []FieldExtract) error {
		for _, f := range list {
			if f.Key != "" && f.KeyRegex != "" {
				return fmt.Errorf("k8sattributes config: a %s rule has both key %q and key_regex %q", field, f.Key, f.KeyRegex)
			}
			from := f.From
			switch from {
			case "":
				from = fromPod
			case fromPod, fromNamespace, fromNode:
			case "deployment", "statefulset":
				return fmt.Errorf("unsupported k8sattributes config: %s from %s objects aren't modeled yet", field, from)
			default:
				return fmt.Errorf("k8sattributes config: %q is not a valid choice for from", f.From)
			}
			r := rule{field: field, from: from, name: f.TagName, key: f.Key}
			if r.name == "" && f.Key != "" {
				r.name = fmt.Sprintf("k8s.%s.%s.%s", from, field, f.Key)
			}
			if f.KeyRegex != "" {
				re, err := regexp.Compile("^(?:" + f.KeyRegex + ")$")
				if err != nil {
					return fmt.Errorf("k8sattributes config: %w", err)
				}
				r.regex, r.hasRef = re, re.NumSubexp() > 0
			}
			out = append(out, r)
		}
		return nil
	}
	if err := add("labels", e.Labels); err != nil {
		return nil, err
	}
	if err := add("annotations", e.Annotations); err != nil {
		return nil, err
	}
	if e.OtelAnnotations {
		out = append(out, rule{field: "annotations", from: fromPod, name: "$1",
			regex: regexp.MustCompile(`^resource\.opentelemetry\.io/(.+)$`), hasRef: true})
	}
	return out, nil
}

func (r rule) singular() string { return strings.TrimSuffix(r.field, "s") }

// propose adds what the rule extracts from an object's labels or
// annotations; whose names the object ("the pod's").
func (r rule) propose(ps *proposals, stage int, src map[string]string, whose string) {
	if r.regex == nil {
		if v, ok := src[r.key]; ok {
			ps.add(stage, r.name, write(v, fmt.Sprintf("from %s %s `%s`", whose, r.singular(), r.key)))
		} else {
			ps.add(stage, r.name, skip(fmt.Sprintf("%s %s don't include `%s`", capitalize(whose), r.field, r.key)))
		}
		return
	}
	keys := make([]string, 0, len(src))
	for k := range src {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := src[k]
		if !r.regex.MatchString(k) || v == "" {
			continue
		}
		name := fmt.Sprintf("k8s.%s.%s.%s", r.from, r.field, k)
		if r.hasRef {
			name = string(r.regex.ExpandString(nil, r.name, k, r.regex.FindStringSubmatchIndex(k)))
		}
		ps.add(stage, name, write(v, fmt.Sprintf("from %s %s `%s`", whose, r.singular(), k)))
	}
}

// unknown records that the rule's source object is missing.
func (r rule) unknown(ps *proposals, stage int, why string) {
	if r.regex == nil {
		ps.add(stage, r.name, unknown(why))
		return
	}
	ps.notes = append(ps.notes, lineage.Event{Action: lineage.Unverified,
		Reason: fmt.Sprintf("%s matching `%s` from the %s can't be copied: %s", r.field, r.regex.String(), r.from, why)})
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
