package main

import (
	"bytes"
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

	if code := run([]string{"bogus"}, &out, &errs); code != 2 {
		t.Errorf("unknown command: exit %d, want 2", code)
	}
}
