package collector

import (
	"os"
	"path/filepath"
	"testing"
)

// The resolver expands env references and turns $$ into $, so a
// k8sattributes tag_name written $$1 is the regex reference $1.
func TestLoadWithEnvExpansion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte(`
processors:
  k8sattributes:
    extract:
      labels:
        - {tag_name: $$1, key_regex: "app.kubernetes.io/(.*)"}
        - {tag_name: "${env:TAG}", key: team}
        - {tag_name: "$${env:TAG}", key: other}
service:
  pipelines: {}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadWithEnv(path, map[string]string{"TAG": "owner"})
	if err != nil {
		t.Fatal(err)
	}
	labels := c.Processors["k8sattributes"]["extract"].(map[string]any)["labels"].([]any)
	for i, want := range []string{"$1", "owner", "${env:TAG}"} {
		if got := labels[i].(map[string]any)["tag_name"]; got != want {
			t.Errorf("label %d tag_name = %q, want %q", i, got, want)
		}
	}
}
