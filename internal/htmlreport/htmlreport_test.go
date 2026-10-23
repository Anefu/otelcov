package htmlreport

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/Anefu/otelcov/internal/flowgraph"
	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/report"
)

var dataScript = regexp.MustCompile(`(?s)<script id="otelcov-data" type="application/json">(.*?)</script>`)

// Data is embedded as JSON the page parses back. Text from configs can hold
// anything, "</script>" included; it must not end the script element.
func TestEmbeddedData(t *testing.T) {
	hostile := `set(attributes["x"], "</script><img src=x onerror=alert(1)>")`
	d := Data{Generated: "2026-10-23T19:00:00Z", Scenarios: []Scenario{{
		Report: &report.Report{
			Scenario: "01-baseline", EvalTime: "2026-10-23T18:38:52Z",
			Summary: []report.Entry{{Requirement: "r", Pod: "ns/p", Status: report.NotCovered, Path: []lineage.SignalID{"s1"}, BrokenAt: "rule/x"}},
			Signals: map[lineage.SignalID]lineage.Signal{},
			Events:  []lineage.Event{{ID: "e1", Node: "gateway/transform/team", Signal: "s1", Action: lineage.Skipped, Reason: hostile}},
		},
		Flow:  flowgraph.Graph{Nodes: []flowgraph.Node{{ID: "gateway/transform/team", Group: "gateway", Label: "transform/team", Kind: flowgraph.Processor}}},
		Truth: map[string]Truth{TruthKey("r", "ns/p"): {Real: "no alert", Verdict: "agree"}},
	}}}
	var buf bytes.Buffer
	if err := Write(&buf, d); err != nil {
		t.Fatal(err)
	}
	page := buf.String()
	if strings.Count(page, "</script>") != 2 {
		t.Errorf("want exactly the two real </script> tags, got %d", strings.Count(page, "</script>"))
	}
	m := dataScript.FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no data script")
	}
	var back Data
	if err := json.Unmarshal([]byte(m[1]), &back); err != nil {
		t.Fatalf("embedded data isn't JSON: %v", err)
	}
	if back.Scenarios[0].Report.Events[0].Reason != hostile || back.Scenarios[0].Truth["r|ns/p"].Verdict != "agree" {
		t.Errorf("data didn't round-trip: %+v", back.Scenarios[0])
	}
}

// The page embeds only the events and signals its routes show, plus counts of
// every event.
func TestRouteOnly(t *testing.T) {
	sig := func(node string, from ...lineage.SignalID) lineage.Signal {
		return lineage.Signal{Node: node, Kind: "otlp_metric", From: from}
	}
	ev := func(id, node string, s lineage.SignalID, a lineage.Action) lineage.Event {
		return lineage.Event{ID: id, Node: node, Signal: s, Action: a}
	}
	r := &report.Report{
		Scenario: "01", EvalTime: "2026-10-23T18:38:52Z",
		Summary: []report.Entry{{Requirement: "r", Pod: "ns/p", Status: report.NotCovered,
			Path: []lineage.SignalID{"s1", "p1"}, UnverifiedBy: []string{"e9"}}},
		Diagnosis: []report.Diagnosis{{Requirement: "r", Pod: "ns/p", Chain: []string{"e6", "e7"}, RootCause: "e7"}},
		Signals: map[lineage.SignalID]lineage.Signal{
			"s1": sig("source/otlp-replay"),
			"s2": sig("source/otlp-replay"),
			"s3": sig("source/otlp-replay"),
			"c1": sig("topology", "s1"),
			"c2": sig("topology", "c1"),
			"c3": sig("topology", "s3"),
			"p1": sig("backend/mimir", "s1"),
		},
		Events: []lineage.Event{
			ev("e1", "agent/k8sattributes", "s1", lineage.Set),
			ev("e2", "agent/k8sattributes", "s3", lineage.Set),
			ev("e3", "topology", "c1", lineage.Derived),
			ev("e4", "gateway/debug", "c2", lineage.Exported),
			ev("e5", "gateway/debug", "c3", lineage.Exported),
			ev("e6", "backend/mimir", "p1", lineage.Derived),
			ev("e7", "agent/k8sattributes", "s2", lineage.Skipped),
			ev("e8", "agent/k8sattributes", "s3", lineage.Skipped),
			ev("e9", "agent/otlp/gateway", "s2", lineage.Unverified),
		},
	}
	var buf bytes.Buffer
	if err := Write(&buf, Data{Scenarios: []Scenario{{Report: r}}}); err != nil {
		t.Fatal(err)
	}
	var back struct {
		Scenarios []struct {
			Report      report.Report                     `json:"report"`
			EventCounts map[string]map[lineage.Action]int `json:"event_counts"`
		} `json:"scenarios"`
	}
	if err := json.Unmarshal([]byte(dataScript.FindStringSubmatch(buf.String())[1]), &back); err != nil {
		t.Fatal(err)
	}
	got := back.Scenarios[0]
	var ids []string
	for _, e := range got.Report.Events {
		ids = append(ids, e.ID)
	}
	if want := "e1 e3 e4 e6 e7 e9"; strings.Join(ids, " ") != want {
		t.Errorf("events: got %v, want %s", ids, want)
	}
	var sigs []string
	for id := range got.Report.Signals {
		sigs = append(sigs, string(id))
	}
	sort.Strings(sigs)
	if want := "c1 c2 p1 s1 s2"; strings.Join(sigs, " ") != want {
		t.Errorf("signals: got %v, want %s", sigs, want)
	}
	if n := got.EventCounts["agent/k8sattributes"]; n[lineage.Set] != 2 || n[lineage.Skipped] != 2 {
		t.Errorf("counts cover every event: %v", got.EventCounts)
	}
	if len(r.Events) != 9 || len(r.Signals) != 7 {
		t.Error("Write changed the caller's report")
	}
}
