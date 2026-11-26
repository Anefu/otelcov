package pluginwire

import (
	"encoding/json"
	"reflect"
	"testing"

	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/unit"
)

// What crosses the boundary comes back the same, and lineage JSON reads as
// it did before: numbers keep their digits, empty fields stay empty.
func TestRoundTrip(t *testing.T) {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr("k8s.pod.name", "")
	rm.Resource().Attributes().PutInt("big", 1700000000123456789)
	dp := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty().SetEmptyGauge().DataPoints().AppendEmpty()
	dp.SetDoubleValue(0.1)
	in := []unit.Metric{{ID: "s1", From: []lineage.SignalID{"s0"}, Data: md}}
	units, err := Units(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := FromUnits(units)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].ID != "s1" || !reflect.DeepEqual(out[0].From, in[0].From) {
		t.Errorf("envelope: %+v", out[0])
	}
	var a, b pmetric.JSONMarshaler
	ja, _ := a.MarshalMetrics(md)
	jb, _ := b.MarshalMetrics(out[0].Data)
	if string(ja) != string(jb) {
		t.Errorf("data:\n%s\n%s", ja, jb)
	}

	events := []lineage.Event{
		{Node: "agent/x", Signal: "s1", Attribute: "a", Action: lineage.Set, Value: int64(1700000000123456789),
			Detail: map[string]any{"n": 3, "list": []string{"x"}, "m": map[string]string{"k": "v"}}, From: []lineage.SignalID{"s0"}},
		{Node: "agent/x", Signal: "s1", Action: lineage.Skipped, Value: "", Reason: "r", Simulated: true},
		{Node: "agent/x", Signal: "s1", Action: lineage.Dropped},
	}
	pe, err := Events(events)
	if err != nil {
		t.Fatal(err)
	}
	back, err := FromEvents(pe)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(events)
	got, _ := json.Marshal(back)
	if string(got) != string(want) {
		t.Errorf("events:\n%s\n%s", want, got)
	}
	if back[2].Value != nil || back[2].Detail != nil || back[1].Value != "" {
		t.Errorf("empty fields: %#v", back)
	}

	signals := []lineage.Signal{{ID: "c1", Node: "n", Kind: "otlp_metric", Name: "m", From: []lineage.SignalID{"s1"},
		Resource: map[string]any{"service.name": "api", "n": 2}, Value: 0.1}}
	ps, err := Signals(signals)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := FromSignals(ps)
	if err != nil {
		t.Fatal(err)
	}
	want, _ = json.Marshal(signals)
	got, _ = json.Marshal(sb)
	if string(got) != string(want) || sb[0].ID != "c1" || sb[0].Labels != nil {
		t.Errorf("signals:\n%s\n%s %+v", want, got, sb[0])
	}
}
