package source

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Anefu/otelcov/internal/lineage"
)

func request(metric string) string {
	return `{"resourceMetrics":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"app"}}]},` +
		`"scopeMetrics":[{"metrics":[{"name":"` + metric + `","gauge":{"dataPoints":[{"timeUnixNano":"1","asInt":"1"},{"timeUnixNano":"2","asInt":"2"}]}}]}]}]}`
}

// Recordings replay in the order given; IDs continue across files, and each
// item knows its file.
func TestReplaysFilesInOrder(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.jsonl"), filepath.Join(dir, "b.jsonl")
	if err := os.WriteFile(a, []byte(request("x")+"\n\n"+request("y")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte(request("z")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := New(File{Path: a, Display: "rec/a.jsonl"}, File{Path: b, Display: "rec/b.jsonl"})
	out, err := r.ReadMetrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		id     lineage.SignalID
		name   string
		origin string
		file   int
	}{
		{"s1", "x", "rec/a.jsonl#L1", 0}, {"s2", "x", "rec/a.jsonl#L1", 0},
		{"s3", "y", "rec/a.jsonl#L3", 0}, {"s4", "y", "rec/a.jsonl#L3", 0},
		{"s5", "z", "rec/b.jsonl#L1", 1}, {"s6", "z", "rec/b.jsonl#L1", 1},
	}
	if len(out.Items) != len(want) {
		t.Fatalf("got %d items", len(out.Items))
	}
	for i, w := range want {
		s := out.Signals[i]
		if out.Items[i].ID != w.id || s.ID != w.id || s.Name != w.name || s.Origin != w.origin || r.FileOf[w.id] != w.file {
			t.Errorf("item %d: %+v, file %d; want %+v", i, s, r.FileOf[w.id], w)
		}
	}
}

func TestBadLineNamesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.jsonl")
	if err := os.WriteFile(path, []byte(request("x")+"\n{\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := New(File{Path: path, Display: "rec/bad.jsonl"}).ReadMetrics(context.Background())
	if err == nil || err.Error()[:15] != "rec/bad.jsonl:2" {
		t.Errorf("want the file and line in the error: %v", err)
	}
}
