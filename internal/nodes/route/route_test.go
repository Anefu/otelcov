package route

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/model/labels"

	"github.com/Anefu/otelcov/internal/alerting"
	"github.com/Anefu/otelcov/internal/lineage"
)

const config = `
route:
  receiver: default
  routes:
    - receiver: blackhole
      matchers: ['severity="info"']
    - receiver: pager
      matchers: ['team="db"']
      mute_time_intervals: [weekends]
receivers:
  - name: default
    webhook_configs: [{url: "http://x/default"}]
  - name: blackhole
  - name: pager
    webhook_configs: [{url: "http://x/pager"}]
time_intervals:
  - name: weekends
    time_intervals: [{weekdays: [saturday, sunday]}]
inhibit_rules:
  - source_matchers: ['severity="critical"']
    target_matchers: ['severity="warning"']
    equal: [cluster]
`

func TestRouting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "am.yaml")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	alert := func(id string, kv ...string) alerting.Alert {
		return alerting.Alert{ID: lineage.SignalID(id), Labels: labels.FromStrings(append([]string{"alertname", "X"}, kv...)...)}
	}
	events := map[lineage.SignalID][]lineage.Event{}
	for _, e := range n.Process([]alerting.Alert{
		alert("a1", "severity", "info"),
		alert("a2", "team", "db"),
		alert("a3", "severity", "warning"),
		alert("a4"),
	}) {
		events[e.Signal] = append(events[e.Signal], e)
	}

	if e := events["a1"]; len(e) != 1 || e[0].Action != lineage.Dropped || !strings.Contains(e[0].Reason, "no integrations") {
		t.Errorf("null receiver: %+v", e)
	}
	if e := events["a2"]; len(e) != 2 || e[0].Action != lineage.Routed || e[1].Action != lineage.Unverified || !strings.Contains(e[1].Reason, "weekends") {
		t.Errorf("muted route: %+v", e)
	}
	if e := events["a3"]; len(e) != 2 || e[1].Action != lineage.Unverified || !strings.Contains(e[1].Reason, "inhibit") {
		t.Errorf("inhibition: %+v", e)
	}
	if e := events["a4"]; len(e) != 1 || e[0].Detail["receiver"] != "default" {
		t.Errorf("fallback to root: %+v", e)
	}
}
