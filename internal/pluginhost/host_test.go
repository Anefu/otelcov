package pluginhost

import (
	"strings"
	"testing"

	"github.com/Anefu/otelcov/pkg/plugin/pluginv1"
)

func host(plugins ...Info) *Host {
	h := &Host{discovered: true, dirs: []string{"/plugins"}}
	for _, p := range plugins {
		r := strings.Split(p.Reviewed, "–")
		c := &candidate{info: p}
		c.min, _ = minor(r[0])
		c.max, _ = minor(r[1])
		h.candidates = append(h.candidates, c)
	}
	return h
}

func TestResolve(t *testing.T) {
	p134 := Info{Distribution: "otelcol-contrib", Version: "0.134.0", Reviewed: "0.130–0.134"}
	p139 := Info{Distribution: "otelcol-contrib", Version: "0.139.0", Reviewed: "0.134–0.139"}
	p166 := Info{Distribution: "otelcol-contrib", Version: "0.166.0", Reviewed: "0.166–0.166"}
	h := host(p139, p134, p166)
	for version, want := range map[string]string{
		"0.134.1":  "0.134.0", // both cover it: the one built for 0.134 wins
		"v0.137.2": "0.139.0",
		"0.139":    "0.139.0",
		"0.166.3":  "0.166.0",
		"0.129.0":  "",
		"0.150.0":  "",
		"1.139.0":  "",
	} {
		p, why, err := h.Resolve("otelcol-contrib", version)
		switch {
		case err != nil:
			t.Errorf("%s: %v", version, err)
		case want == "" && (p != nil || !strings.HasPrefix(why, "no plugin was reviewed for otelcol-contrib "+version)):
			t.Errorf("%s: want none, got %v %q", version, p, why)
		case want != "" && (p == nil || p.Info().Version != want):
			t.Errorf("%s: want %s, got %v %q", version, want, p, why)
		}
	}
	// The same plugin every time, so it's started once.
	a, _, _ := h.Resolve("otelcol-contrib", "0.137.0")
	b, _, _ := h.Resolve("otelcol-contrib", "0.138.0")
	if a != b {
		t.Error("Resolve should return one Plugin per installed plugin")
	}

	if _, why, _ := h.Resolve("otelcol-k8s", "0.139.0"); !strings.HasPrefix(why, "no plugin models distribution otelcol-k8s; installed: ") {
		t.Errorf("unknown distribution: %q", why)
	}
	if _, why, _ := h.Resolve("otelcol-contrib", "latest"); why != `version "latest" isn't a version like 0.139.0` {
		t.Errorf("bad version: %q", why)
	}
	if _, why, _ := host().Resolve("otelcol-contrib", "0.139.0"); why != "no stage plugins are installed (looked in /plugins)" {
		t.Errorf("none installed: %q", why)
	}
}

func TestProcessorNote(t *testing.T) {
	i := Info{Processors: map[string]Component{
		"transform": {Fidelity: pluginv1.Fidelity_FIDELITY_REAL},
		"batch":     {Fidelity: pluginv1.Fidelity_FIDELITY_APPROXIMATION, Note: "passes data through"},
		"foo":       {Fidelity: pluginv1.Fidelity_FIDELITY_UNMODELLED},
	}}
	for typ, want := range map[string]string{"transform": "", "batch": "passes data through", "foo": NotModeled, "spanmetrics": NotModeled} {
		if got := i.ProcessorNote(typ); got != want {
			t.Errorf("%s: %q", typ, got)
		}
	}
}
