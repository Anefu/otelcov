package k8sattributes

import (
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"

	"github.com/Anefu/otelcov/internal/collector"
)

// Filter is the processor's filter section: which pods, and which node, its
// informers watch (contrib 0.134 to 0.139: config.go FilterConfig, options.go
// withFilterNode/Namespace/Labels/Fields, internal/kube/client.go
// selectorsFromFilters). A pod the filter leaves out is invisible to the
// processor, as if it didn't exist.
type Filter struct {
	Node string `json:"node"`
	// NodeFromEnvVar names an env var holding the node, usually set from
	// spec.nodeName with the downward API on a daemonset agent. It wins
	// over Node.
	NodeFromEnvVar string       `json:"node_from_env_var"`
	Namespace      string       `json:"namespace"`
	Fields         []FilterRule `json:"fields"`
	Labels         []FilterRule `json:"labels"`
}

// FilterRule is one label or field filter (FieldFilterConfig).
type FilterRule struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Op    string `json:"op"`
}

// Filter operations (options.go). "" means equals.
const (
	opEquals       = "equals"
	opNotEquals    = "not-equals"
	opExists       = "exists"
	opDoesNotExist = "does-not-exist"
)

// Instance is the collector instance the processor runs in: what its filter
// can depend on.
type Instance struct {
	// Node is the Kubernetes node the instance runs on (a daemonset agent's
	// node); "" when unknown.
	Node string
	// Env is the collector's environment, from the topology.
	Env map[string]collector.EnvVar
}

// selectableFields are the pod fields the API server accepts in a field
// selector (Kubernetes pkg/registry/core/pod: ToSelectableFields and the v1
// field label conversion, which also takes status.podIPs as status.podIP).
// Any other key makes the processor's pod watch fail.
var selectableFields = map[string]bool{
	"metadata.name": true, "metadata.namespace": true, "spec.nodeName": true, "spec.restartPolicy": true,
	"spec.schedulerName": true, "spec.serviceAccountName": true, "spec.hostNetwork": true,
	"status.phase": true, "status.podIP": true, "status.podIPs": true, "status.nominatedNodeName": true,
}

// podFields are a pod's selectable fields with their values, as the API
// server matches field selectors against them.
func podFields(pod *corev1.Pod) fields.Set {
	ip := pod.Status.PodIP
	if len(pod.Status.PodIPs) > 0 {
		ip = pod.Status.PodIPs[0].IP
	}
	return fields.Set{
		"metadata.name":            pod.Name,
		"metadata.namespace":       pod.Namespace,
		"spec.nodeName":            pod.Spec.NodeName,
		"spec.restartPolicy":       string(pod.Spec.RestartPolicy),
		"spec.schedulerName":       pod.Spec.SchedulerName,
		"spec.serviceAccountName":  pod.Spec.ServiceAccountName,
		"spec.hostNetwork":         strconv.FormatBool(pod.Spec.HostNetwork),
		"status.phase":             string(pod.Status.Phase),
		"status.podIP":             ip,
		"status.podIPs":            ip,
		"status.nominatedNodeName": pod.Status.NominatedNodeName,
	}
}

// watch is what the processor's informers watch, compiled from its filter
// for one collector instance.
type watch struct {
	// node is the only node whose pods (and Node object) are watched; ""
	// for every node.
	node string
	// nodeUnknown says why the node isn't known, or "". Pods are then
	// looked up on every node, and results that depend on it are
	// unverified.
	nodeUnknown string
	// nodeSetting is the config that names the node, for explanations.
	nodeSetting string
	namespace   string
	labels      labels.Selector
	// fields is the field selector without the node term.
	fields fields.Selector
}

// compileFilter checks the filter as the processor does (config.go
// Validate, selectorsFromFilters) and resolves its node for inst.
func compileFilter(f Filter, inst Instance) (watch, error) {
	w := watch{namespace: f.Namespace, labels: labels.Everything()}
	for _, r := range f.Labels {
		var op selection.Operator
		var vals []string
		switch r.Op {
		case "", opEquals:
			op, vals = selection.Equals, []string{r.Value}
		case opNotEquals:
			op, vals = selection.NotEquals, []string{r.Value}
		case opExists:
			op = selection.Exists
		case opDoesNotExist:
			op = selection.DoesNotExist
		default:
			return w, fmt.Errorf("k8sattributes config: '%s' is not a valid label filter operation for key=%s, value=%s", r.Op, r.Key, r.Value)
		}
		req, err := labels.NewRequirement(r.Key, op, vals)
		if err != nil {
			return w, fmt.Errorf("k8sattributes config: filter.labels: %w", err)
		}
		w.labels = w.labels.Add(*req)
	}
	var sels []fields.Selector
	for _, r := range f.Fields {
		switch r.Op {
		case "", opEquals:
			sels = append(sels, fields.OneTermEqualSelector(r.Key, r.Value))
		case opNotEquals:
			sels = append(sels, fields.OneTermNotEqualSelector(r.Key, r.Value))
		default:
			return w, fmt.Errorf("k8sattributes config: '%s' is not a valid label filter operation for key=%s, value=%s", r.Op, r.Key, r.Value)
		}
		if !selectableFields[r.Key] {
			return w, fmt.Errorf("k8sattributes config: filter.fields key %q isn't a field the Kubernetes API server can select pods by, so the processor's pod watch would fail", r.Key)
		}
	}
	w.fields = fields.AndSelectors(sels...)

	switch v := f.NodeFromEnvVar; {
	case v != "":
		w.nodeSetting = "`filter.node_from_env_var: " + v + "`"
		env, ok := inst.Env[v]
		switch {
		case !ok:
			w.nodeUnknown = "the processor watches the node named in env var `" + v + "` (`filter.node_from_env_var`), and the topology doesn't set `" + v + "` for this collector"
		case env.FieldRef == collector.NodeNameField && inst.Node == "":
			w.nodeUnknown = "the processor watches the node named in env var `" + v + "` (`filter.node_from_env_var`), this collector instance's own node, and the recording doesn't say which node that is"
		case env.FieldRef == collector.NodeNameField:
			w.node = inst.Node
		default:
			w.node = env.Value
		}
	case f.Node != "":
		w.node, w.nodeSetting = f.Node, "`filter.node`"
	}
	return w, nil
}

// filtered reports whether the filter leaves any pod out.
func (w watch) filtered() bool {
	return w.node != "" || w.nodeUnknown != "" || w.namespace != "" || !w.labels.Empty() || !w.fields.Empty()
}

// hides says why the processor never sees pod, and which filter setting
// leaves it out (node, namespace, labels or fields); "" if it sees the pod.
// With the node unknown, the node isn't checked.
func (w watch) hides(pod *corev1.Pod) (why, filter string) {
	switch {
	case w.node != "" && pod.Spec.NodeName != w.node:
		where := "it runs on node `" + pod.Spec.NodeName + "`"
		if pod.Spec.NodeName == "" {
			where = "it isn't scheduled on a node"
		}
		return where + ", and the processor only watches pods on node `" + w.node + "` (" + w.nodeSetting + ")", "node"
	case w.namespace != "" && pod.Namespace != w.namespace:
		return "it's in namespace `" + pod.Namespace + "`, and the processor only watches namespace `" + w.namespace + "` (`filter.namespace`)", "namespace"
	case !w.labels.Matches(labels.Set(pod.Labels)):
		return "its labels don't match `filter.labels` (`" + w.labels.String() + "`)", "labels"
	case !w.fields.Matches(podFields(pod)):
		return "its fields don't match `filter.fields` (`" + w.fields.String() + "`)", "fields"
	}
	return "", ""
}

// hidesNode says why the processor doesn't know a Node object, or "". It
// only watches the filter's node (k8sconfig.NewNodeSharedInformer).
func (w watch) hidesNode(name string) string {
	if w.node != "" && name != w.node {
		return "the processor only watches node `" + w.node + "` (" + w.nodeSetting + "), so node `" + name + "`'s labels, annotations and UID aren't known to it"
	}
	return ""
}
