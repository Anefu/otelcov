// Package requirements loads coverage requirements (format A, decision 57).
//
// A requirement names its subjects (what is watched), a signal (what to follow
// from them, in OpenTelemetry terms) and the destinations the signal must
// reach, all of them. Only pod subjects with alert destinations are checked so
// far; everything else in the format is accepted and reported unverified.
package requirements

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"sigs.k8s.io/yaml"
)

// Subject kinds. Each is a shortcut for an OpenTelemetry entity, identified
// by resource attributes; entity spells those attributes out.
const (
	KindPod       = "pod"
	KindNode      = "node"
	KindWorkload  = "workload"
	KindNamespace = "namespace"
	KindEntity    = "entity"
)

var kinds = map[string]bool{KindPod: true, KindNode: true, KindWorkload: true, KindNamespace: true, KindEntity: true}

// Requirement is one coverage requirement.
type Requirement struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	Subjects    Subjects `json:"subjects"`
	Signal      Signal   `json:"signal"`
	// Reaches lists destinations the signal must reach, all of them.
	Reaches []Destination `json:"reaches"`
	// MustNotReach lists destinations the signal must not reach.
	MustNotReach []Destination `json:"must_not_reach"`
}

// Subjects says what is watched.
type Subjects struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	// Labels the subject must have; the value "*" means the label must exist.
	Labels map[string]string `json:"labels"`
	// Container is the pod container expected to send the signal (pods only).
	Container string `json:"container"`
	// Attributes identify an entity by its resource attributes (kind: entity).
	Attributes map[string]string `json:"attributes"`
}

// Signal is what to follow from each subject.
type Signal struct {
	// Metrics are OpenTelemetry metric names, as recorded; any of them counts.
	Metrics []string `json:"metrics"`
}

// Destination is where the signal must (or must not) end up: an alert, a
// dashboard panel or a backend.
type Destination struct {
	Alert string `json:"alert,omitempty"`
	// Receivers the alert must be routed to, all of them; none means any
	// receiver. Each may be a template, resolved per subject.
	Receivers []string `json:"receivers,omitempty"`
	// Labels the alert must carry, e.g. so its notification says which pod.
	Labels    []string   `json:"labels,omitempty"`
	Dashboard *Dashboard `json:"dashboard,omitempty"`
	Backend   string     `json:"backend,omitempty"`
}

// Dashboard is a Grafana dashboard panel.
type Dashboard struct {
	UID   string `json:"uid"`
	Panel string `json:"panel"`
}

// String describes the destination for reports.
func (d Destination) String() string {
	switch {
	case d.Alert != "":
		return "alert " + d.Alert
	case d.Dashboard != nil:
		return fmt.Sprintf("dashboard %s, panel %q", d.Dashboard.UID, d.Dashboard.Panel)
	default:
		return "backend " + d.Backend
	}
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
	seen := map[string]bool{}
	for i, r := range f.Requirements {
		if err := r.validate(); err != nil {
			return nil, fmt.Errorf("%s: requirement %d (%s): %w", path, i, r.ID, err)
		}
		if seen[r.ID] {
			return nil, fmt.Errorf("%s: requirement id %q is used twice", path, r.ID)
		}
		seen[r.ID] = true
	}
	return f.Requirements, nil
}

func (r Requirement) validate() error {
	switch {
	case r.ID == "":
		return fmt.Errorf("needs an id")
	case !kinds[r.Subjects.Kind]:
		return fmt.Errorf("subjects.kind must be one of pod, node, workload, namespace, entity; got %q", r.Subjects.Kind)
	case r.Subjects.Container != "" && r.Subjects.Kind != KindPod:
		return fmt.Errorf("subjects.container only applies to pods")
	case len(r.Subjects.Attributes) > 0 && r.Subjects.Kind != KindEntity:
		return fmt.Errorf("subjects.attributes only applies to kind entity")
	case r.Subjects.Kind == KindEntity && len(r.Subjects.Attributes) == 0:
		return fmt.Errorf("kind entity needs subjects.attributes")
	case len(r.Signal.Metrics) == 0:
		return fmt.Errorf("needs signal.metrics")
	case len(r.Reaches)+len(r.MustNotReach) == 0:
		return fmt.Errorf("needs at least one destination under reaches or must_not_reach")
	}
	for _, d := range append(append([]Destination(nil), r.Reaches...), r.MustNotReach...) {
		if err := d.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (d Destination) validate() error {
	n := 0
	for _, set := range []bool{d.Alert != "", d.Dashboard != nil, d.Backend != ""} {
		if set {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("a destination is exactly one of alert, dashboard or backend")
	}
	if d.Alert == "" && (len(d.Receivers) > 0 || len(d.Labels) > 0) {
		return fmt.Errorf("receivers and labels only apply to alert destinations")
	}
	if d.Dashboard != nil && (d.Dashboard.UID == "" || d.Dashboard.Panel == "") {
		return fmt.Errorf("a dashboard destination needs uid and panel")
	}
	for _, rcv := range d.Receivers {
		if err := checkTemplate(rcv); err != nil {
			return err
		}
	}
	return nil
}

// Pods returns the pods a pod requirement applies to, in input order.
func (r Requirement) Pods(pods []corev1.Pod) []*corev1.Pod {
	if r.Subjects.Kind != KindPod {
		return nil
	}
	sel := r.selector()
	var out []*corev1.Pod
	for i := range pods {
		p := &pods[i]
		if r.Subjects.Namespace != "" && p.Namespace != r.Subjects.Namespace {
			continue
		}
		if sel.Matches(labels.Set(p.Labels)) {
			out = append(out, p)
		}
	}
	return out
}

func (r Requirement) selector() labels.Selector {
	sel := labels.NewSelector()
	keys := make([]string, 0, len(r.Subjects.Labels))
	for k := range r.Subjects.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		op, vals := selection.Equals, []string{r.Subjects.Labels[k]}
		if vals[0] == "*" {
			op, vals = selection.Exists, nil
		}
		req, err := labels.NewRequirement(k, op, vals)
		if err != nil {
			// An invalid label key or value matches nothing.
			return labels.Nothing()
		}
		sel = sel.Add(*req)
	}
	return sel
}

// templateRef matches {{ subject.labels.<key> }}, {{ subject.name }} and
// {{ subject.namespace }}.
var templateRef = regexp.MustCompile(`\{\{\s*subject\.(labels\.(\S+?)|name|namespace)\s*\}\}`)

func checkTemplate(s string) error {
	rest := templateRef.ReplaceAllString(s, "")
	if strings.Contains(rest, "{{") || strings.Contains(rest, "}}") {
		return fmt.Errorf("receiver %q: templates can only use subject.labels.<label>, subject.name or subject.namespace", s)
	}
	return nil
}

// Resolve fills in a template for a pod. It fails if the pod lacks a label
// the template uses.
func Resolve(s string, pod *corev1.Pod) (string, error) {
	var missing []string
	out := templateRef.ReplaceAllStringFunc(s, func(ref string) string {
		m := templateRef.FindStringSubmatch(ref)
		switch {
		case m[1] == "name":
			return pod.Name
		case m[1] == "namespace":
			return pod.Namespace
		}
		v, ok := pod.Labels[m[2]]
		if !ok || v == "" {
			missing = append(missing, m[2])
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("receiver `%s` needs pod label `%s`, which this pod doesn't have", s, strings.Join(missing, "`, `"))
	}
	return out, nil
}
