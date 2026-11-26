package mimir

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Every limit of every Mimir version in the tested range has one class, and
// the classification names no limit those versions don't have.
func TestEveryLimitIsClassified(t *testing.T) {
	files, err := filepath.Glob("testdata/limits-*.txt")
	if err != nil || len(files) == 0 {
		t.Fatalf("no limit lists: %v", err)
	}
	known := map[string]bool{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			if line = strings.TrimSpace(line); line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			known[line] = true
			n := 0
			for _, class := range []map[string]bool{modelledLimits, assumedLimits, ignoredLimits} {
				if class[line] {
					n++
				}
			}
			if _, ok := unmodelledLimits[line]; ok {
				n++
			}
			if n != 1 {
				t.Errorf("%s: %s is in %d classes, want 1", f, line, n)
			}
		}
	}
	var stray []string
	for _, class := range []map[string]bool{modelledLimits, assumedLimits, ignoredLimits} {
		for name := range class {
			if !known[name] {
				stray = append(stray, name)
			}
		}
	}
	for name := range unmodelledLimits {
		if !known[name] {
			stray = append(stray, name)
		}
	}
	sort.Strings(stray)
	if len(stray) > 0 {
		t.Errorf("classified, but no tested Mimir version has them: %v", stray)
	}
}
