// Package version says which otelcov this is (decision 63) and which
// upstream releases it models.
//
// otelcov's versions track the collector-contrib release its bundled
// otelcol-contrib plugin is built against: v0.139.x while pinned to
// collector-contrib v0.139.0 (decision 66), a new minor when the pin moves.
// The versions come from build information, so they can't drift from
// go.mod: the otelcov version is the module version Go stamps from the git
// tag ("devel" for a local build without one), and a plugin reports the
// versions of the modules linked into it (decision 67).
package version

import "runtime/debug"

// Info is the version information otelcov reports.
type Info struct {
	Otelcov string `json:"version"`
	// Plugins are the stage plugins a run used, or, for `otelcov version`,
	// the ones installed.
	Plugins []Plugin `json:"plugins,omitempty"`
}

// Plugin describes a stage plugin: the distribution and version it models,
// the deployed versions it was reviewed against, and the upstream modules
// it links.
type Plugin struct {
	Distribution string            `json:"distribution"`
	Version      string            `json:"version"`
	Reviewed     string            `json:"reviewed"`
	Modules      map[string]string `json:"modules,omitempty"`
}

// Upstream modules plugins report.
const (
	ContribModule = "github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl"
	PdataModule   = "go.opentelemetry.io/collector/pdata"
)

// Get reads otelcov's version from the running binary.
func Get() Info {
	info := Info{Otelcov: "devel"}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			info.Otelcov = v
		}
	}
	return info
}

// Module is the version of a module linked into the running binary, or
// "unknown".
func Module(path string) string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, d := range bi.Deps {
		if d.Path == path {
			if d.Replace != nil {
				return d.Replace.Version
			}
			return d.Version
		}
	}
	return "unknown"
}
