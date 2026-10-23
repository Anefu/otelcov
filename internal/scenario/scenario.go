// Package scenario loads scenario files: one run's data (a recording and its
// snapshot metadata), config under test, requirements and expected report.
package scenario

import (
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"
)

// Scenario is one run. All paths are absolute after Load.
type Scenario struct {
	Name     string `json:"name"`
	EvalTime string `json:"eval_time"`
	Data     struct {
		Recording string `json:"recording"`
		Snapshot  string `json:"snapshot"`
	} `json:"data"`
	Config struct {
		Pods              string   `json:"pods"`
		Collector         string   `json:"collector"`
		CollectorPipeline string   `json:"collector_pipeline"`
		Backend           string   `json:"backend"`
		Rules             []string `json:"rules"`
		Alertmanager      string   `json:"alertmanager"`
	} `json:"config"`
	Requirements string `json:"requirements"`
	Expected     string `json:"expected"`

	// Root is the directory report paths are relative to.
	Root string `json:"-"`
}

// Snapshot is the metadata stored with a recording: the config that was
// running when it was captured.
type Snapshot struct {
	RecordedAt string `json:"recorded_at"`
	// Pods is the path of the pod specs running at recording time.
	Pods string `json:"pods"`
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
		&s.Data.Recording, &s.Data.Snapshot,
		&s.Config.Pods, &s.Config.Collector, &s.Config.Backend, &s.Config.Alertmanager,
		&s.Requirements, &s.Expected,
	} {
		*p = abs(dir, *p)
	}
	for i := range s.Config.Rules {
		s.Config.Rules[i] = abs(dir, s.Config.Rules[i])
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
