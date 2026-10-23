package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScenarioCommand(t *testing.T) {
	var out, errs bytes.Buffer
	code := run([]string{"scenario", "../../testdata/thin-slice/scenarios/02-sidecar-added.yaml"}, &out, &errs)
	if code != 1 {
		t.Fatalf("exit %d, want 1 (not covered); stderr: %s", code, errs.String())
	}
	for _, want := range []string{"NOT COVERED", "vllm-embed", "collector/k8sattributes didn't set", "fix: Set k8s.container.name=vllm"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if code := run([]string{"scenario", "-json", "../../testdata/thin-slice/scenarios/01-single-container.yaml"}, &out, &errs); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if !strings.Contains(out.String(), `"status": "covered"`) {
		t.Errorf("JSON output lacks statuses:\n%s", out.String())
	}

	out.Reset()
	if code := run([]string{"scenario", "../../testdata/agent-gateway/scenarios/02-team-condition-pr.yaml", "../../testdata/agent-gateway/scenarios/03-bad-join.yaml"}, &out, &errs); code != 1 {
		t.Fatalf("exit %d, want 1; stderr: %s", code, errs.String())
	}
	for _, want := range []string{
		"would set team=inference, but its condition is false",
		"  unverified: agent/exporters/otlp/gateway -> gateway/receivers/otlp: otlp/gateway sends to port 4318",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}

	page := filepath.Join(t.TempDir(), "report.html")
	out.Reset()
	if code := run([]string{"scenario", "-html", page, "../../testdata/agent-gateway/scenarios/01-baseline.yaml"}, &out, &errs); code != 0 {
		t.Fatalf("exit %d, want 0; stderr: %s", code, errs.String())
	}
	if b, err := os.ReadFile(page); err != nil || !strings.Contains(string(b), `"scenario":"01-baseline"`) {
		t.Errorf("report page: %v", err)
	}

	if code := run([]string{"bogus"}, &out, &errs); code != 2 {
		t.Errorf("unknown command: exit %d, want 2", code)
	}
}
