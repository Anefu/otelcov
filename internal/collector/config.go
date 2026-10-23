// Package collector reads OpenTelemetry Collector configuration files.
//
// This is a minimal reader for the parts otelcov models. Real-processor nodes
// will decode their own sections with the processors' config types.
package collector

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/yaml"
)

// Config is a collector configuration.
type Config struct {
	Receivers  map[string]map[string]any `json:"receivers"`
	Processors map[string]map[string]any `json:"processors"`
	Exporters  map[string]map[string]any `json:"exporters"`
	Connectors map[string]map[string]any `json:"connectors"`
	Extensions map[string]map[string]any `json:"extensions"`
	Service    Service                   `json:"service"`
}

// Service holds the pipelines.
type Service struct {
	Pipelines  map[string]Pipeline `json:"pipelines"`
	Extensions []string            `json:"extensions"`
	Telemetry  map[string]any      `json:"telemetry"`
}

// Pipeline lists component IDs in order.
type Pipeline struct {
	Receivers  []string `json:"receivers"`
	Processors []string `json:"processors"`
	Exporters  []string `json:"exporters"`
}

// Load reads a collector config. Environment variable references are not
// expanded.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.UnmarshalStrict(data, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// Pipeline returns the pipeline with the given ID, such as "metrics".
func (c *Config) Pipeline(id string) (Pipeline, error) {
	p, ok := c.Service.Pipelines[id]
	if !ok {
		return Pipeline{}, fmt.Errorf("no pipeline %q", id)
	}
	for _, proc := range p.Processors {
		if _, ok := c.Processors[proc]; !ok {
			return Pipeline{}, fmt.Errorf("pipeline %q uses undefined processor %q", id, proc)
		}
	}
	return p, nil
}

// Type returns the component type of an ID: "k8sattributes/tap" is a
// "k8sattributes".
func Type(id string) string {
	t, _, _ := strings.Cut(id, "/")
	return t
}

// Decode converts a component's raw config into a typed struct. Unknown keys
// are an error, so unmodeled settings can't be ignored silently.
func Decode(raw map[string]any, into any) error {
	if raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	return dec.Decode(into)
}
