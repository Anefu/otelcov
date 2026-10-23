// Package realproc runs real collector processors as graph nodes.
//
// Each processor is built from its collector-contrib factory with the config
// from the collector file, and run once per unit: an item, or for processors
// whose statements can look across a metric's data points, a metric group.
// Outputs are matched to inputs (exactly for items; by timestamps and value
// within a metric group) and compared, which gives set, removed and dropped
// events. The processor's own output is always what flows on.
package realproc

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/attributesprocessor"
	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/deltatocumulativeprocessor"
	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/filterprocessor"
	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/metricstransformprocessor"
	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/resourceprocessor"
	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/transformprocessor"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/confmap/xconfmap"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/processortest"

	"github.com/Anefu/otelcov/internal/collector"
	"github.com/Anefu/otelcov/internal/graph"
	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/unit"
)

var factories = map[string]processor.Factory{}

func init() {
	for _, f := range []processor.Factory{
		attributesprocessor.NewFactory(),
		deltatocumulativeprocessor.NewFactory(),
		filterprocessor.NewFactory(),
		metricstransformprocessor.NewFactory(),
		resourceprocessor.NewFactory(),
		transformprocessor.NewFactory(),
	} {
		factories[f.Type().String()] = f
	}
}

// Supported reports whether a processor type runs as real code.
func Supported(typ string) bool {
	_, ok := factories[typ]
	return ok
}

// byMetric lists processors that can look across a metric's data points
// (metric-context OTTL, aggregations), so they run on whole metric groups.
var byMetric = map[string]bool{"transform": true, "filter": true, "metricstransform": true}

// Node is a real processor.
type Node struct {
	name     string
	typ      string
	proc     processor.Metrics
	sink     *consumertest.MetricsSink
	byMetric bool
	ids      *lineage.IDs
	// uncertain is set when part of the config can't be checked per unit.
	uncertain string
	// stmts explains transform statements; filter explains filter drops.
	stmts  *stmtExplainer
	filter *filterExplainer
}

// New builds the processor with component ID id (e.g. "transform/fix") from
// its raw collector config. name is the node name in lineage. ids hands out
// IDs for signals the processor creates.
func New(ctx context.Context, name, id string, raw map[string]any, ids *lineage.IDs) (*Node, error) {
	typ := collector.Type(id)
	f, ok := factories[typ]
	if !ok {
		return nil, fmt.Errorf("processor type %s doesn't run as real code yet", typ)
	}
	cfg := f.CreateDefaultConfig()
	if raw != nil {
		if err := confmap.NewFromStringMap(raw).Unmarshal(cfg); err != nil {
			return nil, fmt.Errorf("%s config: %w", id, err)
		}
	}
	if err := xconfmap.Validate(cfg); err != nil {
		return nil, fmt.Errorf("%s config: %w", id, err)
	}
	set := processortest.NewNopSettings(f.Type())
	if _, n, found := strings.Cut(id, "/"); found {
		set.ID = component.NewIDWithName(f.Type(), n)
	}
	sink := new(consumertest.MetricsSink)
	proc, err := f.CreateMetrics(ctx, set, cfg, sink)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", id, err)
	}
	if err := proc.Start(ctx, componenttest.NewNopHost()); err != nil {
		return nil, fmt.Errorf("%s: start: %w", id, err)
	}
	n := &Node{name: name, typ: typ, proc: proc, sink: sink, byMetric: byMetric[typ], ids: ids}
	if typ == "metricstransform" && usesCombine(raw) {
		n.uncertain = "metricstransform's combine merges different metrics, which needs a batch unit; not supported yet"
	}
	switch typ {
	case "transform":
		n.stmts = newTransformExplainer(raw)
	case "filter":
		n.filter = newFilterExplainer(raw)
	}
	return n, nil
}

func (n *Node) Name() string { return n.name }

// Shutdown stops the processor.
func (n *Node) Shutdown(ctx context.Context) error { return n.proc.Shutdown(ctx) }

func (n *Node) ProcessMetrics(ctx context.Context, in []unit.Metric) (graph.Output, error) {
	var groups [][]unit.Metric
	if n.byMetric {
		groups = unit.GroupByMetric(in)
	} else {
		for _, it := range in {
			groups = append(groups, []unit.Metric{it})
		}
	}
	var out graph.Output
	for _, g := range groups {
		res, err := n.run(ctx, g)
		if err != nil {
			return graph.Output{}, err
		}
		out.Items = append(out.Items, res.Items...)
		out.Events = append(out.Events, res.Events...)
		out.Signals = append(out.Signals, res.Signals...)
	}
	return out, nil
}

// run processes one unit.
func (n *Node) run(ctx context.Context, g []unit.Metric) (graph.Output, error) {
	var out graph.Output
	emit := func(id lineage.SignalID, e lineage.Event) {
		e.Node, e.Signal = n.name, id
		out.Events = append(out.Events, e)
	}
	before := make([]pmetric.Metrics, len(g))
	for i, it := range g {
		before[i] = pmetric.NewMetrics()
		it.Data.CopyTo(before[i])
	}
	var input pmetric.Metrics
	if len(g) == 1 {
		input = pmetric.NewMetrics()
		g[0].Data.CopyTo(input)
	} else {
		input = unit.CombineMetric(g)
	}

	n.sink.Reset()
	if err := n.proc.ConsumeMetrics(ctx, input); err != nil {
		for _, it := range g {
			emit(it.ID, lineage.Event{Action: lineage.Unverified, Reason: "the " + n.typ + " processor returned an error: " + err.Error()})
		}
		out.Items = append(out.Items, g...)
		return out, nil
	}
	var outs []pmetric.Metrics
	for _, md := range n.sink.AllMetrics() {
		outs = append(outs, unit.SplitMetrics(md)...)
	}

	// Match outputs to inputs.
	claimed := make([]bool, len(g))
	for _, o := range outs {
		parent, exact := n.match(g, before, o, claimed)
		switch {
		case exact && !claimed[parent]:
			claimed[parent] = true
			out.Items = append(out.Items, unit.Metric{ID: g[parent].ID, From: g[parent].From, Data: o})
			for _, e := range n.explainChanges(ctx, before[parent], o) {
				emit(g[parent].ID, e)
			}
			if n.uncertain != "" {
				emit(g[parent].ID, lineage.Event{Action: lineage.Unverified, Reason: n.uncertain})
			}
		default:
			// A new signal: a copy of one input, or an output no single
			// input explains (then every input of the unit is a parent).
			var from []lineage.SignalID
			reason := "created by the " + n.typ + " processor"
			if exact {
				from = []lineage.SignalID{g[parent].ID}
			} else {
				for _, it := range g {
					from = append(from, it.ID)
				}
				reason += "; no single input explains it, so every input of the metric is a parent"
			}
			id := n.ids.Next()
			out.Items = append(out.Items, unit.Metric{ID: id, From: from, Data: o})
			out.Signals = append(out.Signals, lineage.Signal{
				ID: id, Node: n.name, Kind: "otlp_metric", Name: unit.FirstMetric(o).Name(), From: from,
				Resource: unit.Resource(o).Attributes().AsRaw(),
			})
			emit(id, lineage.Event{Action: lineage.Derived, From: from, Reason: reason})
		}
	}
	for i, it := range g {
		if !claimed[i] && !hasChild(out.Signals, it.ID) {
			reason := "the " + n.typ + " processor dropped it"
			if n.filter != nil {
				if why := n.filter.why(ctx, before[i]); why != "" {
					reason = "dropped by filter " + why
				}
			}
			emit(it.ID, lineage.Event{Action: lineage.Dropped, Reason: reason})
		}
	}
	return out, nil
}

// match finds the input an output came from. Within a unit of one item it's
// that item; within a metric group it's the input with the same timestamps
// and value, if exactly one has them.
func (n *Node) match(g []unit.Metric, before []pmetric.Metrics, o pmetric.Metrics, claimed []bool) (int, bool) {
	if len(g) == 1 {
		return 0, true
	}
	k := pointKey(o)
	var cands []int
	for i := range g {
		if pointKey(before[i]) == k {
			cands = append(cands, i)
		}
	}
	if len(cands) == 0 {
		return 0, false
	}
	// Several inputs may share timestamps and value: take them in order.
	for _, c := range cands {
		if !claimed[c] {
			return c, true
		}
	}
	return cands[0], true // all taken: a copy of the first
}

func hasChild(signals []lineage.Signal, id lineage.SignalID) bool {
	for _, s := range signals {
		for _, f := range s.From {
			if f == id {
				return true
			}
		}
	}
	return false
}

// pointKey identifies a single data point by its timestamps and value.
func pointKey(md pmetric.Metrics) string {
	m := unit.FirstMetric(md)
	var start, ts pcommon.Timestamp
	var v float64
	switch m.Type() {
	case pmetric.MetricTypeGauge:
		p := m.Gauge().DataPoints().At(0)
		start, ts, v = p.StartTimestamp(), p.Timestamp(), number(p)
	case pmetric.MetricTypeSum:
		p := m.Sum().DataPoints().At(0)
		start, ts, v = p.StartTimestamp(), p.Timestamp(), number(p)
	case pmetric.MetricTypeHistogram:
		p := m.Histogram().DataPoints().At(0)
		start, ts, v = p.StartTimestamp(), p.Timestamp(), p.Sum()
	case pmetric.MetricTypeExponentialHistogram:
		p := m.ExponentialHistogram().DataPoints().At(0)
		start, ts, v = p.StartTimestamp(), p.Timestamp(), p.Sum()
	case pmetric.MetricTypeSummary:
		p := m.Summary().DataPoints().At(0)
		start, ts, v = p.StartTimestamp(), p.Timestamp(), p.Sum()
	}
	return fmt.Sprintf("%d|%d|%x", start, ts, math.Float64bits(v))
}

func number(p pmetric.NumberDataPoint) float64 {
	if p.ValueType() == pmetric.NumberDataPointValueTypeInt {
		return float64(p.IntValue())
	}
	return p.DoubleValue()
}

// diff compares one item before and after: metric fields, resource, scope and
// data point attributes. Resource attributes use their key as the event
// attribute; the others say their level in the detail.
func diff(a, b pmetric.Metrics) []lineage.Event {
	var out []lineage.Event
	ma, mb := unit.FirstMetric(a), unit.FirstMetric(b)
	field := func(name, va, vb string) {
		if va != vb {
			out = append(out, lineage.Event{Attribute: "metric." + name, Action: lineage.Set, Value: vb,
				Detail: map[string]any{"level": "metric", "was": va}})
		}
	}
	field("name", ma.Name(), mb.Name())
	field("unit", ma.Unit(), mb.Unit())
	field("type", ma.Type().String(), mb.Type().String())
	field("temporality", temporality(ma), temporality(mb))

	out = append(out, attrDiff(unit.Resource(a).Attributes(), unit.Resource(b).Attributes(), "")...)
	sa, sb := a.ResourceMetrics().At(0).ScopeMetrics().At(0).Scope(), b.ResourceMetrics().At(0).ScopeMetrics().At(0).Scope()
	if sa.Name() != sb.Name() {
		out = append(out, lineage.Event{Attribute: "scope.name", Action: lineage.Set, Value: sb.Name(), Detail: map[string]any{"level": "scope", "was": sa.Name()}})
	}
	out = append(out, attrDiff(sa.Attributes(), sb.Attributes(), "scope")...)
	out = append(out, attrDiff(pointAttrs(ma), pointAttrs(mb), "datapoint")...)
	return out
}

func attrDiff(a, b pcommon.Map, level string) []lineage.Event {
	var out []lineage.Event
	detail := func() map[string]any {
		if level == "" {
			return nil
		}
		return map[string]any{"level": level}
	}
	ra, rb := a.AsRaw(), b.AsRaw()
	keys := map[string]bool{}
	for k := range ra {
		keys[k] = true
	}
	for k := range rb {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		va, ina := ra[k]
		vb, inb := rb[k]
		switch {
		case ina && !inb:
			out = append(out, lineage.Event{Attribute: k, Action: lineage.Removed, Detail: detail()})
		case inb && (!ina || !equal(va, vb)):
			out = append(out, lineage.Event{Attribute: k, Action: lineage.Set, Value: vb, Detail: detail()})
		}
	}
	return out
}

func equal(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

func pointAttrs(m pmetric.Metric) pcommon.Map {
	switch m.Type() {
	case pmetric.MetricTypeGauge:
		return m.Gauge().DataPoints().At(0).Attributes()
	case pmetric.MetricTypeSum:
		return m.Sum().DataPoints().At(0).Attributes()
	case pmetric.MetricTypeHistogram:
		return m.Histogram().DataPoints().At(0).Attributes()
	case pmetric.MetricTypeExponentialHistogram:
		return m.ExponentialHistogram().DataPoints().At(0).Attributes()
	case pmetric.MetricTypeSummary:
		return m.Summary().DataPoints().At(0).Attributes()
	}
	return pcommon.NewMap()
}

func temporality(m pmetric.Metric) string {
	switch m.Type() {
	case pmetric.MetricTypeSum:
		return m.Sum().AggregationTemporality().String()
	case pmetric.MetricTypeHistogram:
		return m.Histogram().AggregationTemporality().String()
	case pmetric.MetricTypeExponentialHistogram:
		return m.ExponentialHistogram().AggregationTemporality().String()
	}
	return ""
}

func usesCombine(raw map[string]any) bool {
	ts, _ := raw["transforms"].([]any)
	for _, t := range ts {
		if m, ok := t.(map[string]any); ok && m["action"] == "combine" {
			return true
		}
	}
	return false
}

// explainChanges compares an item before and after the processor. For
// transform, each change names the statement that made it, statements whose
// condition was false report what they would have changed (skipped), and the
// replay is checked against the processor's real output.
func (n *Node) explainChanges(ctx context.Context, before, after pmetric.Metrics) []lineage.Event {
	changes := diff(before, after)
	if n.stmts == nil {
		for i := range changes {
			changes[i].Reason = "changed by the " + n.typ + " processor"
		}
		return changes
	}
	effects, replayed := n.stmts.explain(ctx, before)
	key := func(e lineage.Event) string {
		level, _ := e.Detail["level"].(string)
		return level + "|" + e.Attribute
	}
	changed := map[string]bool{}
	for i := range changes {
		changes[i].Reason = "changed by the " + n.typ + " processor"
		k := key(changes[i])
		changed[k] = true
		for _, ef := range effects { // the last statement to touch it wins
			for _, c := range ef.changes {
				if key(c) == k {
					changes[i].Reason = "set by " + ef.stmt.context + " statement `" + ef.stmt.text + "`"
					changes[i].Detail = withStatement(changes[i].Detail, ef.stmt)
				}
			}
		}
	}
	out := changes
	for _, ef := range effects {
		for _, w := range ef.would {
			if changed[key(w)] {
				continue
			}
			d := withStatement(w.Detail, ef.stmt)
			d["condition"] = false
			d["where"] = whereClause(ef.stmt.text)
			if w.Action == lineage.Set {
				d["would_set"] = w.Value
			} else {
				d["would"] = string(w.Action)
			}
			out = append(out, lineage.Event{
				Attribute: w.Attribute, Action: lineage.Skipped, Detail: d,
				Reason: ef.stmt.context + " statement `" + ef.stmt.text + "` didn't run: its condition is false for this signal",
			})
		}
		if ef.err != nil {
			out = append(out, lineage.Event{Action: lineage.Unverified, Detail: withStatement(nil, ef.stmt),
				Reason: "replaying " + ef.stmt.context + " statement `" + ef.stmt.text + "` failed: " + ef.err.Error()})
		}
	}
	if len(n.stmts.incomplete) == 0 {
		if mismatch := diff(replayed, after); len(mismatch) > 0 {
			out = append(out, lineage.Event{Action: lineage.Unverified,
				Reason: "the statement-by-statement replay doesn't match the transform processor's output, so statement details may be wrong"})
		}
	}
	return out
}

func withStatement(d map[string]any, s statement) map[string]any {
	out := map[string]any{}
	for k, v := range d {
		out[k] = v
	}
	out["statement"] = s.text
	out["context"] = s.context
	return out
}
