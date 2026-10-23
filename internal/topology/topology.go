// Package topology reads the topology file: which config is which component,
// and how components connect. Everything inside a collector (pipelines,
// processors, their order) comes from the collector's own config.
package topology

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/Anefu/otelcov/internal/collector"
)

// File is a topology file. Paths are absolute after Load.
type File struct {
	Version    int                  `json:"version"`
	Collectors map[string]Collector `json:"collectors"`
	Backends   map[string]Backend   `json:"backends"`
	Consumers  map[string]Consumer  `json:"consumers"`
	// External lists exporters whose data leaves the modeled system, as
	// "<collector>/exporters/<exporter id>".
	External []string `json:"external"`
	Joins    []Join   `json:"joins"`

	dir string
}

// Collector is one collector deployment.
type Collector struct {
	Config string `json:"config"`
	SHA256 string `json:"sha256,omitempty"`
	// Tap is the pipeline that records this collector's input, if any.
	Tap string `json:"tap,omitempty"`
	// SnapshotPipelines exist only to record snapshot metadata (e.g. what
	// resourcedetection detects). Like the tap, runs and validation skip them.
	SnapshotPipelines []string `json:"snapshot_pipelines,omitempty"`
	// FeatureGates as passed with --feature-gates: "gate" or "-gate".
	FeatureGates []string `json:"feature_gates,omitempty"`
	// Env provides values for ${env:VAR} references in the config.
	Env map[string]string `json:"env,omitempty"`
}

// Recording reports whether a pipeline only records data for otelcov (the
// tap or a snapshot pipeline), so it isn't part of the modeled system.
func (c Collector) Recording(pipelineID string) bool {
	if pipelineID == c.Tap {
		return true
	}
	for _, p := range c.SnapshotPipelines {
		if p == pipelineID {
			return true
		}
	}
	return false
}

// Backend is a storage backend.
type Backend struct {
	Type   string `json:"type"`
	Config string `json:"config"`
	SHA256 string `json:"sha256,omitempty"`
}

// Consumer reads from a backend or another consumer.
type Consumer struct {
	Type   string   `json:"type"` // prometheus-rules, alertmanager
	Paths  []string `json:"paths,omitempty"`
	Config string   `json:"config,omitempty"`
	SHA256 string   `json:"sha256,omitempty"`
	Reads  string   `json:"reads"`
}

// Join connects an exporter to a receiver of another collector, or to a
// backend.
type Join struct {
	From string `json:"from"` // <collector>/exporters/<id>
	To   string `json:"to"`   // <collector>/receivers/<id>, or a backend name
}

// Endpoint is a parsed join end.
type Endpoint struct {
	Collector string
	Kind      string // "exporters" or "receivers"; empty for a backend
	ID        string
	Backend   string
}

func (e Endpoint) String() string {
	if e.Backend != "" {
		return e.Backend
	}
	return e.Collector + "/" + e.Kind + "/" + e.ID
}

// ParseEndpoint parses "<collector>/<kind>/<id>" or a backend name.
func (f *File) ParseEndpoint(s string) (Endpoint, error) {
	if _, ok := f.Backends[s]; ok {
		return Endpoint{Backend: s}, nil
	}
	parts := strings.SplitN(s, "/", 3)
	if len(parts) != 3 || (parts[1] != "exporters" && parts[1] != "receivers") {
		return Endpoint{}, fmt.Errorf("%q is neither a backend nor <collector>/exporters|receivers/<id>", s)
	}
	return Endpoint{Collector: parts[0], Kind: parts[1], ID: parts[2]}, nil
}

// Load reads a topology file.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f File
	if err := yaml.UnmarshalStrict(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if f.Version != 1 {
		return nil, fmt.Errorf("%s: unsupported version %d", path, f.Version)
	}
	f.dir, err = filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	abs := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(f.dir, p)
	}
	for k, c := range f.Collectors {
		c.Config = abs(c.Config)
		f.Collectors[k] = c
	}
	for k, b := range f.Backends {
		b.Config = abs(b.Config)
		f.Backends[k] = b
	}
	for k, c := range f.Consumers {
		c.Config = abs(c.Config)
		for i := range c.Paths {
			c.Paths[i] = abs(c.Paths[i])
		}
		f.Consumers[k] = c
	}
	return &f, nil
}

// Rel returns a path relative to the topology file, for messages.
func (f *File) Rel(p string) string {
	if r, err := filepath.Rel(f.dir, p); err == nil {
		return r
	}
	return p
}

// Names returns map keys sorted, for deterministic iteration.
func Names[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// FileHash returns the hex SHA-256 of a file.
func FileHash(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// LoadCollector reads a collector's config, with ${env:VAR} and ${VAR}
// references replaced from the topology's env values. Unresolved references
// stay as written.
func (f *File) LoadCollector(name string) (*collector.Config, error) {
	c, ok := f.Collectors[name]
	if !ok {
		return nil, fmt.Errorf("no collector %q", name)
	}
	return collector.LoadWithEnv(c.Config, c.Env)
}
