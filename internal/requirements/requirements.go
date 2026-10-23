// Package requirements loads coverage requirements, written in OpenTelemetry
// terms: which pods, which signal, and which alert and receiver it must reach.
package requirements

import (
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/yaml"
)

// Requirement is one coverage requirement.
type Requirement struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	ForEachPod  struct {
		Namespace string            `json:"namespace"`
		Selector  map[string]string `json:"selector"`
	} `json:"for_each_pod"`
	Signal struct {
		// Metric is the OpenTelemetry metric name, as recorded.
		Metric string `json:"metric"`
		// Container is the container expected to emit it.
		Container string `json:"container"`
	} `json:"signal"`
	MustReach struct {
		Alert    string `json:"alert"`
		Receiver string `json:"receiver"`
	} `json:"must_reach"`
}

// Load reads a requirements file.
func Load(path string) ([]Requirement, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f struct {
		Requirements []Requirement `json:"requirements"`
	}
	if err := yaml.UnmarshalStrict(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for i, r := range f.Requirements {
		if r.ID == "" || r.Signal.Metric == "" || r.MustReach.Alert == "" {
			return nil, fmt.Errorf("%s: requirement %d needs id, signal.metric and must_reach.alert", path, i)
		}
	}
	return f.Requirements, nil
}

// Pods returns the pods a requirement applies to, in input order.
func (r Requirement) Pods(pods []corev1.Pod) []*corev1.Pod {
	sel := labels.SelectorFromSet(r.ForEachPod.Selector)
	var out []*corev1.Pod
	for i := range pods {
		p := &pods[i]
		if r.ForEachPod.Namespace != "" && p.Namespace != r.ForEachPod.Namespace {
			continue
		}
		if sel.Matches(labels.Set(p.Labels)) {
			out = append(out, p)
		}
	}
	return out
}
