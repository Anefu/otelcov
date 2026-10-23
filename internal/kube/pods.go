// Package kube loads Kubernetes objects and answers the questions nodes ask
// about them, using the real Kubernetes API types.
package kube

import (
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// LoadPods reads a Pod, a PodList or a List of pods from a YAML or JSON file.
func LoadPods(path string) ([]corev1.Pod, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var head struct {
		Kind string `json:"kind"`
	}
	if err := yaml.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	switch head.Kind {
	case "Pod":
		var p corev1.Pod
		if err := yaml.UnmarshalStrict(data, &p); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return []corev1.Pod{p}, nil
	case "List", "PodList":
		var l struct {
			APIVersion string         `json:"apiVersion"`
			Kind       string         `json:"kind"`
			Metadata   map[string]any `json:"metadata,omitempty"`
			Items      []corev1.Pod   `json:"items"`
		}
		if err := yaml.UnmarshalStrict(data, &l); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		for i, p := range l.Items {
			if p.Kind != "" && p.Kind != "Pod" {
				return nil, fmt.Errorf("%s: item %d is a %s, not a Pod", path, i, p.Kind)
			}
		}
		return l.Items, nil
	default:
		return nil, fmt.Errorf("%s: kind %q is not a Pod, PodList or List", path, head.Kind)
	}
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
