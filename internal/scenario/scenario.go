// Package scenario loads scenario files: one run's data (snapshot metadata,
// which lists the recordings), config under test, requirements and expected
// report.
package scenario

import (
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"

	"github.com/Anefu/otelcov/internal/nodes/resourcedetection"
)

// Scenario is one run. All paths are absolute after Load.
type Scenario struct {
	Name string `json:"name"`
	// Description says in a sentence what the scenario changes, for people
	// reading the report page.
	Description string `json:"description,omitempty"`
	EvalTime    string `json:"eval_time"`
	Data        struct {
		// Snapshot is the snapshot metadata, which lists the recordings.
		Snapshot string `json:"snapshot"`
	} `json:"data"`
	Config struct {
		// Pods in the config under test.
		Pods string `json:"pods"`
		// Topology names every other config: collectors, backends, rules,
		// Alertmanager.
		Topology string `json:"topology"`
	} `json:"config"`
	Requirements string `json:"requirements"`
	Expected     string `json:"expected"`

	// Root is the directory report paths are relative to.
	Root string `json:"-"`
	// PluginDir is where stage plugins are found (pluginhost.New); "" for
	// $OTELCOV_PLUGINS, else next to the running executable.
	PluginDir string `json:"-"`
}

// Snapshot is the metadata stored with recordings: the recordings themselves
// and the config that was running when they were captured.
type Snapshot struct {
	RecordedAt string `json:"recorded_at"`
	// Recordings are the recorded files, in replay order (decision 64).
	Recordings []Recording `json:"recordings"`
	// Pods is the path of the pod specs running at recording time.
	Pods string `json:"pods"`
	// ResourceDetection holds what each resourcedetection processor detected
	// at recording time, by collector name then processor ID: the processor's
	// config then, and attributes per Kubernetes node name (for daemonsets) or
	// "*".
	ResourceDetection map[string]map[string]resourcedetection.Recorded `json:"resource_detection,omitempty"`
}

// Recording is one recorded file: what one collector instance's tap wrote.
type Recording struct {
	File string `json:"file"`
	// Collector is the tapped collector that received the data; it may be
	// left out when only one collector has a tap.
	Collector string `json:"collector,omitempty"`
	// Node is the Kubernetes node of the collector instance (a daemonset
	// agent's node). "" when unknown: then a collector whose behavior
	// depends on its node (k8sattributes filter) gives unverified results.
	Node string `json:"node,omitempty"`
}

// Load reads a scenario file and makes its paths absolute. Report paths are
// made relative to root.
func Load(path, root string) (*Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Scenario
	if err := yaml.UnmarshalStrict(data, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	for _, p := range []*string{
		&s.Data.Snapshot, &s.Config.Pods, &s.Config.Topology,
		&s.Requirements, &s.Expected,
	} {
		*p = abs(dir, *p)
	}
	if s.Root, err = filepath.Abs(root); err != nil {
		return nil, err
	}
	return &s, nil
}

// LoadSnapshot reads snapshot metadata and makes its paths absolute.
func LoadSnapshot(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Snapshot
	if err := yaml.UnmarshalStrict(data, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	s.Pods = abs(dir, s.Pods)
	if len(s.Recordings) == 0 {
		return nil, fmt.Errorf("%s: lists no recordings", path)
	}
	seen := map[string]bool{}
	for i := range s.Recordings {
		r := &s.Recordings[i]
		if r.File == "" {
			return nil, fmt.Errorf("%s: recording %d has no file", path, i)
		}
		r.File = abs(dir, r.File)
		if seen[r.File] {
			return nil, fmt.Errorf("%s: recording %s is listed twice", path, r.File)
		}
		seen[r.File] = true
	}
	return &s, nil
}

// Rel returns path relative to the scenario root, with forward slashes.
func (s *Scenario) Rel(path string) string {
	r, err := filepath.Rel(s.Root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(r)
}

func abs(dir, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Clean(filepath.Join(dir, p))
}
