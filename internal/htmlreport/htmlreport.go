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
	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/report"
	"github.com/Anefu/otelcov/internal/topology"
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
	// Description says what the scenario changes (from the scenario file).
	Description string          `json:"description,omitempty"`
	Report      *report.Report  `json:"report"`
	Flow        flowgraph.Graph `json:"flow"`
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

// Write writes the page. Each report is cut down to what the page shows (see
// routeOnly); the JSON report keeps the full lineage.
func Write(w io.Writer, d Data) error {
	p := pageData{Generated: d.Generated, Scenarios: make([]pageScenario, len(d.Scenarios))}
	for i, s := range d.Scenarios {
		p.Scenarios[i] = pageScenario{Scenario: s, EventCounts: countEvents(s.Report)}
		p.Scenarios[i].Report = routeOnly(s.Report)
	}
	return tmpl.Execute(w, p)
}

// pageData is Data as the page embeds it.
type pageData struct {
	Generated string         `json:"generated"`
	Scenarios []pageScenario `json:"scenarios"`
}

type pageScenario struct {
	Scenario
	// EventCounts counts every event of the run by node and action, for the
	// counts beside each station.
	EventCounts map[string]map[lineage.Action]int `json:"event_counts"`
}

func countEvents(r *report.Report) map[string]map[lineage.Action]int {
	counts := map[string]map[lineage.Action]int{}
	if r == nil {
		return counts
	}
	for _, ev := range r.Events {
		c := counts[ev.Node]
		if c == nil {
			c = map[lineage.Action]int{}
			counts[ev.Node] = c
		}
		c[ev.Action]++
	}
	return counts
}

// routeOnly keeps what the page shows: the signals on each entry's path, the
// copies made from them where data fans out, their events, and the events the
// summary and diagnosis refer to. A run's full lineage has an event for every
// data point at every node; on a two-hour recording that's tens of megabytes,
// while a route needs a few dozen events.
func routeOnly(r *report.Report) *report.Report {
	if r == nil {
		return nil
	}
	keep := map[lineage.SignalID]bool{}
	events := map[string]bool{}
	for _, e := range r.Summary {
		for _, id := range e.Path {
			keep[id] = true
		}
		for _, id := range append(append([]string{}, e.SimulatedBy...), e.UnverifiedBy...) {
			events[id] = true
		}
	}
	for _, d := range r.Diagnosis {
		for _, id := range d.Chain {
			events[id] = true
		}
		events[d.RootCause] = true
	}
	// Copies of kept signals, and copies of those.
	for added := true; added; {
		added = false
		for id, s := range r.Signals {
			if keep[id] || s.Node != topology.CopyNode {
				continue
			}
			for _, f := range s.From {
				if keep[f] {
					keep[id], added = true, true
					break
				}
			}
		}
	}
	out := *r
	out.Events = []lineage.Event{}
	out.Signals = map[lineage.SignalID]lineage.Signal{}
	for _, ev := range r.Events {
		if !keep[ev.Signal] && !events[ev.ID] {
			continue
		}
		out.Events = append(out.Events, ev)
		if s, ok := r.Signals[ev.Signal]; ok {
			out.Signals[ev.Signal] = s
		}
	}
	for id := range keep {
		if s, ok := r.Signals[id]; ok {
			out.Signals[id] = s
		}
	}
	return &out
}
