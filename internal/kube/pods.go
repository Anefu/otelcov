// Package kube loads Kubernetes objects and answers the questions nodes ask
// about them, using the real Kubernetes API types.
package kube

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	kjson "sigs.k8s.io/json"
	"sigs.k8s.io/yaml"
)

// APIVersion is the Kubernetes API version of the k8s.io/api module otelcov
// is built with (see go.mod).
const APIVersion = "1.32"

// Objects are the cluster objects a scenario describes: pods, and the
// namespaces and nodes whose labels and annotations k8sattributes can copy.
type Objects struct {
	Pods       []corev1.Pod
	Namespaces []corev1.Namespace
	Nodes      []corev1.Node
}

// Namespace finds a namespace by name.
func (o Objects) Namespace(name string) (*corev1.Namespace, bool) {
	for i := range o.Namespaces {
		if o.Namespaces[i].Name == name {
			return &o.Namespaces[i], true
		}
	}
	return nil, false
}

// Node finds a node by name.
func (o Objects) Node(name string) (*corev1.Node, bool) {
	for i := range o.Nodes {
		if o.Nodes[i].Name == name {
			return &o.Nodes[i], true
		}
	}
	return nil, false
}

// LoadPods reads the pods in a file Load accepts; namespaces and nodes in it
// are left out.
func LoadPods(path string) (pods []corev1.Pod, unknown []string, err error) {
	o, unknown, err := Load(path)
	return o.Pods, unknown, err
}

// Load reads Pods, Namespaces and Nodes from a YAML or JSON file: one object,
// a PodList, NamespaceList or NodeList, or a List of any of them (as
// `kubectl get pods,namespaces,nodes -o yaml` writes).
//
// Fields the API types don't know are ignored and returned in unknown, with
// list indexes collapsed ("items[*].status.observedGeneration"): objects from
// a cluster newer than APIVersion carry fields otelcov can't model yet.
// Callers report them; hand-written fixtures should have none.
func Load(path string) (o Objects, unknown []string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Objects{}, nil, err
	}
	js, err := yaml.YAMLToJSON(data)
	if err != nil {
		return Objects{}, nil, fmt.Errorf("%s: %w", path, err)
	}
	var head struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(js, &head); err != nil {
		return Objects{}, nil, fmt.Errorf("%s: %w", path, err)
	}
	switch head.Kind {
	case "Pod", "Namespace", "Node":
		if unknown, err = o.add(head.Kind, js); err != nil {
			return Objects{}, nil, fmt.Errorf("%s: %w", path, err)
		}
		return o, unknown, nil
	case "List", "PodList", "NamespaceList", "NodeList":
		var l struct {
			APIVersion string            `json:"apiVersion"`
			Kind       string            `json:"kind"`
			Metadata   map[string]any    `json:"metadata,omitempty"`
			Items      []json.RawMessage `json:"items"`
		}
		if unknown, err = decode(js, &l); err != nil {
			return Objects{}, nil, fmt.Errorf("%s: %w", path, err)
		}
		seen := map[string]bool{}
		for _, u := range unknown {
			seen[u] = true
		}
		for i, item := range l.Items {
			var ih struct {
				Kind string `json:"kind"`
			}
			if err := json.Unmarshal(item, &ih); err != nil {
				return Objects{}, nil, fmt.Errorf("%s: item %d: %w", path, i, err)
			}
			kind := ih.Kind
			if kind == "" && l.Kind != "List" {
				kind = strings.TrimSuffix(l.Kind, "List")
			}
			switch kind {
			case "Pod", "Namespace", "Node":
			default:
				return Objects{}, nil, fmt.Errorf("%s: item %d is a %s, not a Pod, Namespace or Node", path, i, ih.Kind)
			}
			itemUnknown, err := o.add(kind, item)
			if err != nil {
				return Objects{}, nil, fmt.Errorf("%s: item %d: %w", path, i, err)
			}
			for _, u := range itemUnknown {
				if u = "items[*]." + u; !seen[u] {
					seen[u] = true
					unknown = append(unknown, u)
				}
			}
		}
		sort.Strings(unknown)
		return o, unknown, nil
	default:
		return Objects{}, nil, fmt.Errorf("%s: kind %q is not a Pod, Namespace, Node or a list of them", path, head.Kind)
	}
}

func (o *Objects) add(kind string, js []byte) ([]string, error) {
	switch kind {
	case "Pod":
		var p corev1.Pod
		unknown, err := decode(js, &p)
		o.Pods = append(o.Pods, p)
		return unknown, err
	case "Namespace":
		var n corev1.Namespace
		unknown, err := decode(js, &n)
		o.Namespaces = append(o.Namespaces, n)
		return unknown, err
	default:
		var n corev1.Node
		unknown, err := decode(js, &n)
		o.Nodes = append(o.Nodes, n)
		return unknown, err
	}
}

var (
	unknownField = regexp.MustCompile(`unknown field "(.*)"`)
	listIndex    = regexp.MustCompile(`\[\d+\]`)
)

// decode unmarshals JSON into v, like the API server: case-sensitive, and
// with every unknown field reported rather than only the first.
func decode(js []byte, v any) ([]string, error) {
	strict, err := kjson.UnmarshalStrict(js, v, kjson.DisallowUnknownFields)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var unknown []string
	for _, e := range strict {
		f := e.Error()
		if m := unknownField.FindStringSubmatch(f); m != nil {
			f = m[1]
		}
		f = listIndex.ReplaceAllString(f, "[*]")
		if !seen[f] {
			seen[f] = true
			unknown = append(unknown, f)
		}
	}
	sort.Strings(unknown)
	return unknown, nil
}

// Pods indexes pods for the lookups nodes need.
type Pods struct {
	list        []corev1.Pod
	byIP        map[string]int
	byUID       map[string]int
	byName      map[string]int
	byContainer map[string]int
}

// NewPods indexes list. Later pods win when keys collide.
func NewPods(list []corev1.Pod) *Pods {
	p := &Pods{
		list:        list,
		byIP:        map[string]int{},
		byUID:       map[string]int{},
		byName:      map[string]int{},
		byContainer: map[string]int{},
	}
	for i := range list {
		pod := &list[i]
		if pod.Status.PodIP != "" {
			p.byIP[pod.Status.PodIP] = i
		}
		for _, ip := range pod.Status.PodIPs {
			p.byIP[ip.IP] = i
		}
		if pod.UID != "" {
			p.byUID[string(pod.UID)] = i
		}
		p.byName[pod.Namespace+"/"+pod.Name] = i
		for _, cs := range pod.Status.ContainerStatuses {
			if id := ContainerID(cs.ContainerID); id != "" {
				p.byContainer[id] = i
			}
		}
	}
	return p
}

// ByIP finds a pod by pod IP.
func (p *Pods) ByIP(ip string) (*corev1.Pod, bool) { return p.get(p.byIP, ip) }

// ByUID finds a pod by UID.
func (p *Pods) ByUID(uid string) (*corev1.Pod, bool) { return p.get(p.byUID, uid) }

// ByName finds a pod by namespace and name.
func (p *Pods) ByName(namespace, name string) (*corev1.Pod, bool) {
	return p.get(p.byName, namespace+"/"+name)
}

// ByContainerID finds the pod running a container, by its runtime ID with or
// without the "containerd://" style prefix.
func (p *Pods) ByContainerID(id string) (*corev1.Pod, bool) {
	return p.get(p.byContainer, ContainerID(id))
}

func (p *Pods) get(index map[string]int, key string) (*corev1.Pod, bool) {
	i, ok := index[key]
	if !ok {
		return nil, false
	}
	return &p.list[i], true
}

// ContainerID strips the runtime prefix from a container ID.
func ContainerID(id string) string {
	if i := strings.Index(id, "://"); i >= 0 {
		return id[i+3:]
	}
	return id
}

// Container returns the container with the given name.
func Container(pod *corev1.Pod, name string) (*corev1.Container, bool) {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return &pod.Spec.Containers[i], true
		}
	}
	return nil, false
}

// ContainerNames lists the pod's (non-init) container names.
func ContainerNames(pod *corev1.Pod) []string {
	names := make([]string, len(pod.Spec.Containers))
	for i, c := range pod.Spec.Containers {
		names[i] = c.Name
	}
	return names
}

// Ref formats a pod as namespace/name.
func Ref(pod *corev1.Pod) string { return pod.Namespace + "/" + pod.Name }
