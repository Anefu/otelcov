package htmlreport

import (
	"bytes"
	"encoding/json"
	"regexp"
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
			Summary: []report.Entry{{Requirement: "r", Pod: "ns/p", Status: report.NotCovered, BrokenAt: "rule/x"}},
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
