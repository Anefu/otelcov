package scenario

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Anefu/otelcov/internal/nodes/alerts"
	"github.com/Anefu/otelcov/internal/nodes/mimir"
	"github.com/Anefu/otelcov/internal/nodes/route"
	"github.com/Anefu/otelcov/internal/nodes/rules"
	"github.com/Anefu/otelcov/internal/nodes/sdkenv"
	"github.com/Anefu/otelcov/internal/nodes/source"
)

const fixtures = "../../testdata/thin-slice"

// implemented lists the nodes whose lineage the golden tests compare. Add a
// node here once it's built; the rest of each golden report is ignored until
// then.
var implementedNodes = map[string]bool{
	sdkenv.Name:               true,
	"collector/k8sattributes": true,
	mimir.Name:                true,
	route.Name:                true,
}

func implemented(node string) bool {
	return implementedNodes[node] || strings.HasPrefix(node, rules.Prefix) || strings.HasPrefix(node, alerts.Prefix)
}

type golden struct {
	Scenario string                     `json:"scenario"`
	Signals  map[string]json.RawMessage `json:"signals"`
	Events   []map[string]any           `json:"events"`
}

func TestGoldenReports(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(fixtures, "scenarios", "*.yaml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no scenarios found: %v", err)
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			s, err := Load(path, fixtures)
			if err != nil {
				t.Fatal(err)
			}
			res, err := Run(context.Background(), s)
			if err != nil {
				t.Fatal(err)
			}
			want := loadGolden(t, s.Expected)
			if want.Scenario != s.Name {
				t.Fatalf("golden report is for %q, scenario is %q", want.Scenario, s.Name)
			}

			// Source signals.
			got := map[string]any{}
			for _, sig := range res.Log.Signals() {
				if sig.Node == source.Name || implemented(sig.Node) {
					got[string(sig.ID)] = roundTrip(t, sig)
				}
			}
			wantSignals := map[string]any{}
			for id, raw := range want.Signals {
				var m map[string]any
				if err := json.Unmarshal(raw, &m); err != nil {
					t.Fatal(err)
				}
				if node, _ := m["node"].(string); node == source.Name || implemented(node) {
					wantSignals[id] = m
				}
			}
			diff(t, "signals", got, wantSignals)

			// Events of implemented nodes, in order, ignoring event IDs.
			var gotEvents, wantEvents []any
			for _, e := range res.Log.Events() {
				if implemented(e.Node) {
					m := roundTrip(t, e)
					delete(m, "id")
					gotEvents = append(gotEvents, m)
				}
			}
			for _, e := range want.Events {
				if implemented(e["node"].(string)) {
					delete(e, "id")
					wantEvents = append(wantEvents, e)
				}
			}
			if len(wantEvents) == 0 || len(wantSignals) == 0 {
				t.Fatal("golden report has nothing to compare for the implemented nodes")
			}
			diff(t, "events", gotEvents, wantEvents)
		})
	}
}

func loadGolden(t *testing.T, path string) golden {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var g golden
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
