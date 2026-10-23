package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Anefu/otelcov/internal/htmlreport"
	"github.com/Anefu/otelcov/internal/report"
)

const fixtures = "../../../testdata/agent-gateway"

// fakeStack answers like Mimir and Alertmanager would if vllm-chat's alert
// reached inference-pager and vllm-embed's series never reached the rule.
func fakeStack(t *testing.T) (mimir, am *httptest.Server) {
	t.Helper()
	chat := "vllm-chat-6f9d8b7c4d-k2x7p"
	mimir = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		var result []map[string]any
		switch {
		case strings.HasPrefix(q, "count by (__name__)"):
			result = append(result, map[string]any{"metric": map[string]string{"__name__": "process_memory_usage_bytes"}})
		case strings.HasPrefix(q, "ALERTS{") && strings.Contains(q, chat):
			result = append(result, map[string]any{"metric": map[string]string{"alertstate": "firing"}})
		}
		json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"result": result}})
	}))
	am = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		alerts := []any{}
		for _, f := range r.URL.Query()["filter"] {
			if strings.Contains(f, chat) {
				alerts = append(alerts, map[string]any{"receivers": []map[string]string{{"name": "inference-pager"}}})
			}
		}
		json.NewEncoder(w).Encode(alerts)
	}))
	t.Cleanup(mimir.Close)
	t.Cleanup(am.Close)
	return mimir, am
}

func TestCompare(t *testing.T) {
	mimir, am := fakeStack(t)
	// Deployed: the baseline gateway config, so 02 (the PR) is skipped.
	o := options{
		mimir: mimir.URL, alertmanager: am.URL, podLabel: "k8s_pod_name", namespaceLabel: "k8s_namespace_name",
		configMapData: func(_ context.Context, cm configMap) (string, error) {
			b, err := os.ReadFile(filepath.Join(fixtures, "collector", cm.key))
			return string(b), err
		},
		podExists: func(context.Context, string, string) (bool, error) { return true, nil },
	}
	var out bytes.Buffer
	page := &htmlreport.Data{}
	disagree, err := run(context.Background(), o, []string{
		filepath.Join(fixtures, "scenarios/01-baseline.yaml"),
		filepath.Join(fixtures, "scenarios/02-team-condition-pr.yaml"),
	}, &out, page)
	if err != nil {
		t.Fatal(err)
	}
	got := out.String()
	t.Log("\n" + got)
	for _, want := range []string{
		"02-team-condition-pr: skipped, not what's deployed (the gateway config differs from collector/gateway-pr.yaml)",
		"vllm-chat-6f9d8b7c4d-k2x7p   covered  series: process_memory_usage_bytes; alert routed to inference-pager  agree",
		"vllm-embed-5c8b9d6f7b-q4m9t  covered  series: process_memory_usage_bytes; no alert",
		"DISAGREE: no alert",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if !disagree {
		t.Error("vllm-embed disagrees: want disagree reported")
	}

	// The page has both scenarios: 01 with the real stack's answers, 02 with
	// why it wasn't compared.
	if len(page.Scenarios) != 2 {
		t.Fatalf("want 2 scenarios on the page, got %d", len(page.Scenarios))
	}
	chat := page.Scenarios[0].Truth[htmlreport.TruthKey("vllm-memory-watched", "inference/vllm-chat-6f9d8b7c4d-k2x7p")]
	if chat.Verdict != "agree" || !strings.Contains(chat.Real, "inference-pager") {
		t.Errorf("truth for vllm-chat: %+v", chat)
	}
	if page.Scenarios[1].Truth != nil || !strings.Contains(page.Scenarios[1].NotCompared, "gateway config differs") {
		t.Errorf("02: %+v", page.Scenarios[1].NotCompared)
	}
	if os.Getenv("OTELCOV_KEEP_PAGE") != "" {
		f, _ := os.Create(os.Getenv("OTELCOV_KEEP_PAGE"))
		htmlreport.Write(f, *page)
		f.Close()
	}
}

func TestVerdict(t *testing.T) {
	for _, tc := range []struct {
		status string
		t      truth
		want   string
	}{
		{report.Covered, truth{receivers: []string{"pager"}}, "agree"},
		{report.CoveredSimulated, truth{receivers: []string{"pager"}}, "agree"},
		{report.Covered, truth{receivers: []string{"default"}}, "DISAGREE: routed elsewhere"},
		{report.Covered, truth{ruler: "pending"}, "wait"},
		{report.Covered, truth{metrics: []string{"m"}}, "DISAGREE: no alert"},
		{report.Covered, truth{}, "DISAGREE: no series in Mimir"},
		{report.NotCovered, truth{gone: true}, "n/a: the pod was replaced"},
		{report.Covered, truth{gone: true}, "n/a: the pod was replaced"},
		{report.NotCovered, truth{}, "agree"},
		{report.NotCovered, truth{ruler: "firing"}, "DISAGREE: the alert exists"},
		{report.Unverified, truth{}, "n/a"},
	} {
		if got := verdict(tc.status, tc.t, "pager"); !strings.HasPrefix(got, tc.want) {
			t.Errorf("verdict(%s, %+v) = %q, want %q...", tc.status, tc.t, got, tc.want)
		}
	}
}
