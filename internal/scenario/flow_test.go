package scenario

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Anefu/otelcov/internal/flowgraph"
)

// The agent-gateway flow: nodes in flow order with lineage node names, so
// report events land on them.
func TestFlow(t *testing.T) {
	s, err := Load(filepath.Join("../../testdata/agent-gateway/scenarios/01-baseline.yaml"), "../../testdata/agent-gateway")
	if err != nil {
		t.Fatal(err)
	}
	rep, g, err := ReportWithFlow(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, n := range g.Nodes {
		ids = append(ids, n.ID)
	}
	want := []string{
		"source/otlp-replay", "source/sdk-env",
		"agent/k8sattributes", "agent/resourcedetection", "agent/resource", "agent/batch", "agent/otlp/gateway",
		"gateway/memory_limiter", "gateway/filter/drop-debug", "gateway/transform/team", "gateway/deltatocumulative", "gateway/batch",
		"gateway/otlphttp/mimir", "gateway/debug", "backend/mimir",
		"rule/team:vllm_memory_bytes:max", "alert/VLLMMemoryHigh", "alertmanager/route",
		"receiver/platform-default", "receiver/inference-pager",
	}
	if strings.Join(ids, " ") != strings.Join(want, " ") {
		t.Errorf("nodes:\n got %v\nwant %v", ids, want)
	}
	edges := map[string]bool{}
	for _, e := range g.Edges {
		edges[e.From+" -> "+e.To] = true
	}
	for _, e := range []string{
		"source/sdk-env -> agent/k8sattributes",
		"agent/otlp/gateway -> gateway/memory_limiter",
		"gateway/batch -> gateway/debug",
		"backend/mimir -> rule/team:vllm_memory_bytes:max",
		"rule/team:vllm_memory_bytes:max -> alert/VLLMMemoryHigh",
		"alert/VLLMMemoryHigh -> alertmanager/route",
		"alertmanager/route -> receiver/inference-pager",
	} {
		if !edges[e] {
			t.Errorf("missing edge %s", e)
		}
	}
	if edges["backend/mimir -> alert/VLLMMemoryHigh"] {
		t.Error("the alert reads the rule's output, not the backend")
	}
	// Every node an event names is on the graph.
	for _, e := range rep.Events {
		if !g.Has(e.Node) && e.Node != "topology" {
			t.Errorf("event node %s isn't on the graph", e.Node)
		}
	}
	for _, n := range g.Nodes {
		if n.ID == "agent/batch" && n.Note != "passes data through" || n.ID == "gateway/debug" && !strings.Contains(n.Note, "external") {
			t.Errorf("note for %s: %q", n.ID, n.Note)
		}
	}
	if g.Nodes[2].Kind != flowgraph.Processor || g.Nodes[13].Kind != flowgraph.Exporter {
		t.Errorf("kinds: %+v %+v", g.Nodes[2], g.Nodes[13])
	}
}

// A bad join's edge carries validation's reason.
func TestFlowBadJoin(t *testing.T) {
	s, err := Load("../../testdata/agent-gateway/scenarios/03-bad-join.yaml", "../../testdata/agent-gateway")
	if err != nil {
		t.Fatal(err)
	}
	_, g, err := ReportWithFlow(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range g.Edges {
		if e.From == "agent/otlp/gateway" && e.To == "gateway/memory_limiter" {
			if !strings.Contains(e.Note, "port 4318") {
				t.Errorf("want the port problem on the join's edge, got %q", e.Note)
			}
			return
		}
	}
	t.Error("join edge missing")
}
