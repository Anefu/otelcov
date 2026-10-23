// Package htmlreport writes otelcov reports as one self-contained HTML page:
// a coverage matrix across scenarios, and for any pod and scenario, its route
// through the flow graph with the events at each node, where it breaks and why.
// When the kind lab's compare has run, the page also shows what the real stack
// did.
//
// The page needs no build step and no server: data is embedded as JSON and
// rendered by plain JavaScript in the page.
package htmlreport

import (
	_ "embed"
	"html/template"
	"io"

	"github.com/Anefu/otelcov/internal/flowgraph"
	"github.com/Anefu/otelcov/internal/report"
)

//go:embed page.html
var page string

var tmpl = template.Must(template.New("page").Parse(page))

// Data is everything the page shows.
type Data struct {
	// Generated is when the page was written (RFC 3339).
	Generated string     `json:"generated"`
	Scenarios []Scenario `json:"scenarios"`
}

// Scenario is one scenario's report and flow graph.
type Scenario struct {
	Report *report.Report  `json:"report"`
	Flow   flowgraph.Graph `json:"flow"`
	// Truth is what the real stack did, by "requirement|pod", when compare
	// checked this scenario.
	Truth map[string]Truth `json:"truth,omitempty"`
	// NotCompared says why compare didn't check this scenario, if it ran.
	NotCompared string `json:"not_compared,omitempty"`
}

// Truth is compare's finding for one pod.
type Truth struct {
	// Real describes what the stack did, e.g. "alert routed to inference-pager".
	Real string `json:"real"`
	// Verdict is compare's verdict: "agree", "DISAGREE: ...", "wait: ...", "n/a: ...".
	Verdict string `json:"verdict"`
}

// TruthKey is the key of a pod's Truth.
func TruthKey(requirement, pod string) string { return requirement + "|" + pod }

// Write writes the page.
func Write(w io.Writer, d Data) error {
	return tmpl.Execute(w, d)
}
