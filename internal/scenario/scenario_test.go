package scenario

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/nodes/source"
	"github.com/Anefu/otelcov/internal/topology"
	"github.com/Anefu/otelcov/internal/unit"
)

func TestLoadSnapshot(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "s.meta.yaml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	s, err := LoadSnapshot(write("recordings:\n  - {file: a.jsonl, collector: agent, node: n1}\n  - {file: b.jsonl}\npods: pods.yaml\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Recordings) != 2 || s.Recordings[0].File != filepath.Join(dir, "a.jsonl") || s.Recordings[0].Node != "n1" || s.Recordings[1].Collector != "" {
		t.Errorf("%+v", s.Recordings)
	}
	for body, want := range map[string]string{
		"pods: pods.yaml\n": "lists no recordings",
		"recordings:\n  - {file: a.jsonl}\n  - {file: ./a.jsonl}\n": "listed twice",
		"recordings:\n  - {node: n1}\n":                             "has no file",
		"recording: a.jsonl\n":                                      "unknown field",
	} {
		if _, err := LoadSnapshot(write(body)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", body, err, want)
		}
	}
}

func TestGroupByRecording(t *testing.T) {
	s := &Scenario{Root: "/r"}
	snap := &Snapshot{Recordings: []Recording{{File: "/r/a.jsonl", Node: "n1"}, {File: "/r/b.jsonl", Node: "n2"}}}
	src := source.New()
	src.FileOf = map[lineage.SignalID]int{"s1": 0, "s2": 1, "s3": 0}
	items := []unit.Metric{{ID: "s1"}, {ID: "s2"}, {ID: "s3"}}

	one := &topology.File{Collectors: map[string]topology.Collector{"agent": {Tap: "metrics/tap"}, "gateway": {}}}
	groups, err := groupByRecording(s, snap, one, src, items)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 || groups[0].Collector != "agent" || groups[0].Node != "n1" || len(groups[0].Items) != 2 ||
		groups[1].Node != "n2" || len(groups[1].Items) != 1 || groups[1].Items[0].ID != "s2" {
		t.Errorf("%+v", groups)
	}

	two := &topology.File{Collectors: map[string]topology.Collector{"agent": {Tap: "metrics/tap"}, "edge": {Tap: "metrics/tap"}}}
	if _, err := groupByRecording(s, snap, two, src, items); err == nil || !strings.Contains(err.Error(), "doesn't say which collector") {
		t.Errorf("two tapped collectors: %v", err)
	}
	snap.Recordings[1].Collector = "gateway"
	if _, err := groupByRecording(s, snap, one, src, items); err == nil || !strings.Contains(err.Error(), `collector "gateway", which has no tap`) {
		t.Errorf("untapped collector: %v", err)
	}
}
