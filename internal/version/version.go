// Package version says which otelcov this is and which collector release it
// models (decision 63).
//
// otelcov's versions track the collector-contrib release it's built against:
// v0.139.x while pinned to collector-contrib v0.139.0 (decision 66), a new
// minor when the pin moves. Both come from the binary's build information, so they can't
// drift from go.mod: the otelcov version is the module version Go stamps from
// the git tag ("devel" for a local build without one), and the collector
// versions are those of the modules linked in.
package version

import "runtime/debug"

// Info is the version information otelcov reports.
type Info struct {
	Otelcov string `json:"version"`
	// CollectorContrib is the collector-contrib release whose processors
	// otelcov runs and models.
	CollectorContrib string `json:"collector_contrib"`
	// Pdata is the collector's pdata module, which carries the telemetry.
	Pdata string `json:"pdata"`
}

const (
	contribModule = "github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl"
	pdataModule   = "go.opentelemetry.io/collector/pdata"
)

// Get reads the versions from the running binary.
func Get() Info {
	info := Info{Otelcov: "devel", CollectorContrib: "unknown", Pdata: "unknown"}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		info.Otelcov = v
	}
	for _, d := range bi.Deps {
		m := d
		if d.Replace != nil {
			m = d.Replace
		}
		switch d.Path {
		case contribModule:
			info.CollectorContrib = m.Version
		case pdataModule:
			info.Pdata = m.Version
		}
	}
	return info
}
