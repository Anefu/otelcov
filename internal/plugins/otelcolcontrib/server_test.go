package otelcolcontrib

import (
	"context"
	"encoding/json"
	"testing"

	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/Anefu/otelcov/internal/pluginwire"
	"github.com/Anefu/otelcov/internal/unit"
	"github.com/Anefu/otelcov/pkg/plugin/pluginv1"
)

func TestServer(t *testing.T) {
	ctx := context.Background()
	s := New()
	d, _ := s.Describe(ctx, &pluginv1.DescribeRequest{})
	if d.GetDistribution() != "otelcol-contrib" || d.GetReviewed().GetMin() != "0.134" || d.GetReviewed().GetMax() != "0.139" {
		t.Errorf("describe: %v", d)
	}
	kinds := map[string]pluginv1.Fidelity{}
	for _, c := range d.GetComponents() {
		kinds[c.GetType()] = c.GetFidelity()
	}
	if kinds["transform"] != pluginv1.Fidelity_FIDELITY_REAL || kinds["k8sattributes"] != pluginv1.Fidelity_FIDELITY_REIMPLEMENTATION || kinds["batch"] != pluginv1.Fidelity_FIDELITY_APPROXIMATION {
		t.Errorf("components: %v", kinds)
	}

	if _, err := s.Init(ctx, &pluginv1.InitRequest{FeatureGates: []string{"no.such.gate"}}); err == nil {
		t.Error("an unknown feature gate is an error, as in the collector")
	}
	s = New()
	if _, err := s.Init(ctx, &pluginv1.InitRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Init(ctx, &pluginv1.InitRequest{}); err == nil {
		t.Error("Init twice must fail: feature gates are process-wide")
	}

	configure := func(id string, cfg any) *pluginv1.ConfigureResponse {
		b, _ := json.Marshal(cfg)
		r, err := s.Configure(ctx, &pluginv1.ConfigureRequest{Kind: "processor", ComponentId: id, NodeName: "agent/" + id, Config: b, Instance: &pluginv1.Instance{Collector: "agent"}})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := configure("batch", nil); !r.GetPassThrough() {
		t.Errorf("batch: %v", r)
	}
	if r := configure("spanmetrics", nil); r.GetUnsupported() != "processor type spanmetrics isn't modeled yet" {
		t.Errorf("unknown type: %v", r)
	}
	if r := configure("transform", map[string]any{"no_such_setting": 1}); r.GetUnsupported() == "" {
		t.Errorf("an invalid config is unsupported: %v", r)
	}

	// A filter that drops nothing and a transform that copies the metric
	// to a new one: the copy is numbered from next_id.
	r := configure("transform/copy", map[string]any{"metric_statements": []any{map[string]any{
		"context": "metric", "statements": []any{`copy_metric(name="m2") where metric.name == "m"`}}}})
	if r.GetHandle() == "" || r.GetFidelity() != pluginv1.Fidelity_FIDELITY_REAL {
		t.Fatalf("transform: %v", r)
	}
	md := pmetric.NewMetrics()
	m := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName("m")
	m.SetEmptyGauge().DataPoints().AppendEmpty().SetIntValue(1)
	units, _ := pluginwire.Units([]unit.Metric{{ID: "s1", Data: md}})
	resp, err := s.Apply(ctx, &pluginv1.ApplyRequest{Handle: r.GetHandle(), Units: units, NextId: 7})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, u := range resp.GetUnits() {
		ids = append(ids, u.GetId())
	}
	if resp.GetIdsUsed() != 1 || len(ids) != 2 || ids[0] != "s1" || ids[1] != "c7" {
		t.Errorf("units %v, ids used %d", ids, resp.GetIdsUsed())
	}
	if _, err := s.Close(ctx, &pluginv1.CloseRequest{}); err != nil || len(s.stages) != 0 {
		t.Errorf("close: %v, %d left", err, len(s.stages))
	}
}
