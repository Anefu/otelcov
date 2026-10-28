package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/Anefu/otelcov/internal/kube"
)

// Apps what-ifs: a manifest like k8s/apps.yaml with a changed Deployment
// template (whatif/apps-<name>.yaml). For otelcov to predict its effect from
// the current recording, the captured pods are rewritten as that template
// would make them: pods-<name>.yaml next to pods.yaml, with the captured
// namespaces and nodes.
//
// Only what the lab's what-ifs change is applied: each container's env and
// envFrom, and the pod labels. A template that adds, removes or renames
// containers is refused, since the recording can't say what they'd send.

// writeAppsWhatIfs writes pods-<name>.yaml in dir for each manifest matching
// glob.
func writeAppsWhatIfs(dir, glob string) error {
	files, err := filepath.Glob(glob)
	if err != nil || len(files) == 0 {
		return err
	}
	objs, _, err := kube.Load(filepath.Join(dir, "pods.yaml"))
	if err != nil {
		return err
	}
	pods := objs.Pods
	for _, f := range files {
		deps, err := loadDeployments(f)
		if err != nil {
			return err
		}
		out, changed, err := applyTemplates(pods, deps)
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "apps-"), ".yaml")
		// Namespaces and nodes stay as captured.
		var items []any
		for _, p := range out {
			p.APIVersion, p.Kind = "v1", "Pod"
			items = append(items, p)
		}
		for _, n := range objs.Namespaces {
			n.APIVersion, n.Kind = "v1", "Namespace"
			items = append(items, n)
		}
		for _, n := range objs.Nodes {
			n.APIVersion, n.Kind = "v1", "Node"
			items = append(items, n)
		}
		b, err := yaml.Marshal(struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Items      []any  `json:"items"`
		}{"v1", "List", items})
		if err != nil {
			return err
		}
		path := filepath.Join(dir, "pods-"+name+".yaml")
		if err := os.WriteFile(path, b, 0o644); err != nil {
			return err
		}
		fmt.Printf("%s: %s applied to %v\n", filepath.Base(path), filepath.Base(f), changed)
	}
	return nil
}

var docSeparator = regexp.MustCompile(`(?m)^---\s*$`)

func loadDeployments(path string) ([]appsv1.Deployment, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []appsv1.Deployment
	for _, doc := range docSeparator.Split(string(data), -1) {
		var head struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal([]byte(doc), &head); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if head.Kind != "Deployment" {
			continue
		}
		var d appsv1.Deployment
		if err := yaml.UnmarshalStrict([]byte(doc), &d); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, d)
	}
	return out, nil
}

// applyTemplates returns the pods as the Deployments' templates would make
// them, and the names of the pods that changed. A pod belongs to Deployment D
// when its ReplicaSet is named D-<pod-template-hash>.
func applyTemplates(pods []corev1.Pod, deps []appsv1.Deployment) ([]corev1.Pod, []string, error) {
	byName := map[string]appsv1.Deployment{}
	for _, d := range deps {
		byName[d.Namespace+"/"+d.Name] = d
	}
	var (
		out     []corev1.Pod
		changed []string
	)
	for _, p := range pods {
		p := *p.DeepCopy()
		d, ok := byName[p.Namespace+"/"+deployment(p)]
		if !ok {
			out = append(out, p)
			continue
		}
		before := p.DeepCopy()
		tmpl := d.Spec.Template
		if got, want := containerNames(p.Spec.Containers), containerNames(tmpl.Spec.Containers); !reflect.DeepEqual(got, want) {
			return nil, nil, fmt.Errorf("pod %s has containers %v but Deployment %s's template has %v; only env and label changes can be applied to a recording", p.Name, got, d.Name, want)
		}
		for i := range p.Spec.Containers {
			for _, c := range tmpl.Spec.Containers {
				if c.Name == p.Spec.Containers[i].Name {
					p.Spec.Containers[i].Env = c.Env
					p.Spec.Containers[i].EnvFrom = c.EnvFrom
				}
			}
		}
		labels := map[string]string{}
		for k, v := range tmpl.Labels {
			labels[k] = v
		}
		if h, ok := p.Labels["pod-template-hash"]; ok {
			labels["pod-template-hash"] = h
		}
		p.Labels = labels
		if !reflect.DeepEqual(before.Spec.Containers, p.Spec.Containers) || !reflect.DeepEqual(before.Labels, p.Labels) {
			changed = append(changed, p.Namespace+"/"+p.Name)
		}
		out = append(out, p)
	}
	return out, changed, nil
}

// deployment returns the name of the Deployment that owns a pod through its
// ReplicaSet, or "".
func deployment(p corev1.Pod) string {
	hash := p.Labels["pod-template-hash"]
	for _, o := range p.OwnerReferences {
		if o.Kind == "ReplicaSet" && hash != "" && strings.HasSuffix(o.Name, "-"+hash) {
			return strings.TrimSuffix(o.Name, "-"+hash)
		}
	}
	return ""
}

func containerNames(cs []corev1.Container) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Name)
	}
	sort.Strings(out)
	return out
}
