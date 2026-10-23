package scenario

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// fixtures are the directories with scenarios and golden reports.
var fixtures = []string{"../../testdata/thin-slice", "../../testdata/agent-gateway"}

var update = flag.Bool("update", false, "rewrite the golden reports from the current output (review the diff before committing)")

// TestGoldenReports runs every scenario and compares the whole report with
// its golden file: summary, diagnosis, every signal and every event.
func TestGoldenReports(t *testing.T) {
	for _, dir := range fixtures {
		paths, err := filepath.Glob(filepath.Join(dir, "scenarios", "*.yaml"))
		if err != nil || len(paths) == 0 {
			t.Fatalf("no scenarios found in %s: %v", dir, err)
		}
		for _, path := range paths {
			golden(t, dir, path)
		}
	}
}

func golden(t *testing.T, dir, path string) {
	t.Run(filepath.Base(dir)+"/"+filepath.Base(path), func(t *testing.T) {
		s, err := Load(path, dir)
		if err != nil {
			t.Fatal(err)
		}
		rep, err := Report(context.Background(), s)
		if err != nil {
			t.Fatal(err)
		}
		got := roundTrip(t, rep)
		if *update {
			b, _ := json.MarshalIndent(rep, "", "  ")
			if err := os.WriteFile(s.Expected, append(b, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
			return
		}
		want := loadGolden(t, s.Expected)
		for _, section := range []string{"scenario", "eval_time", "summary", "diagnosis", "problems", "signals", "events"} {
			diff(t, section, got[section], want[section])
		}
		for k := range want {
			if _, ok := got[k]; !ok {
				t.Errorf("report lacks golden section %q", k)
			}
		}
	})
}

func loadGolden(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var g map[string]any
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

// roundTrip converts v to its JSON object form, so it compares like golden data.
func roundTrip(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func diff(t *testing.T, what string, got, want any) {
	t.Helper()
	if reflect.DeepEqual(got, want) {
		return
	}
	g, _ := json.MarshalIndent(got, "", "  ")
	w, _ := json.MarshalIndent(want, "", "  ")
	t.Errorf("%s differ from the golden report\n--- got\n%s\n--- want\n%s", what, g, w)
}
