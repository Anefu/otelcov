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
	proc, sink, err := build(ctx, id, raw)
	if err != nil {
		return nil, err
	}
	n := &Node{name: name, typ: typ, proc: proc, sink: sink, byMetric: byMetric[typ], ids: ids}
	if typ == "metricstransform" && usesCombine(raw) {
		n.uncertain = "metricstransform's combine merges different metrics, which needs a batch unit; not supported yet"
	}
	switch typ {
	case "transform":
		n.stmts = newTransformExplainer(ctx, id, raw)
	case "filter":
		n.filter = newFilterExplainer(raw)
	}
	return n, nil
}

// build creates and starts the real processor for component ID id (e.g.
// "transform/fix") with its raw collector config. Its output goes to the
// returned sink.
func build(ctx context.Context, id string, raw map[string]any) (processor.Metrics, *consumertest.MetricsSink, error) {
	f, ok := factories[collector.Type(id)]
	if !ok {
		return nil, nil, fmt.Errorf("processor type %s doesn't run as real code yet", collector.Type(id))
	}
	cfg := f.CreateDefaultConfig()
	if raw != nil {
		if err := confmap.NewFromStringMap(raw).Unmarshal(cfg); err != nil {
			return nil, nil, fmt.Errorf("%s config: %w", id, err)
		}
	}
	if err := xconfmap.Validate(cfg); err != nil {
		return nil, nil, fmt.Errorf("%s config: %w", id, err)
	}
	set := processortest.NewNopSettings(f.Type())
	if _, n, found := strings.Cut(id, "/"); found {
		set.ID = component.NewIDWithName(f.Type(), n)
	}
	sink := new(consumertest.MetricsSink)
	proc, err := f.CreateMetrics(ctx, set, cfg, sink)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", id, err)
	}
	if err := proc.Start(ctx, componenttest.NewNopHost()); err != nil {
		return nil, nil, fmt.Errorf("%s: start: %w", id, err)
	}
	return proc, sink, nil
}

// runOnce feeds md to a processor and returns its output, one item per data
// point.
func runOnce(ctx context.Context, proc processor.Metrics, sink *consumertest.MetricsSink, md pmetric.Metrics) ([]pmetric.Metrics, error) {
	in := pmetric.NewMetrics()
	md.CopyTo(in)
	sink.Reset()
	if err := proc.ConsumeMetrics(ctx, in); err != nil {
		return nil, err
	}
	var out []pmetric.Metrics
	for _, m := range sink.AllMetrics() {
		out = append(out, unit.SplitMetrics(m)...)
	}
	return out, nil
}

func (n *Node) Name() string { return n.name }

// Shutdown stops the processor, and those the statement explainer runs.
func (n *Node) Shutdown(ctx context.Context) error {
	if n.stmts != nil {
		n.stmts.shutdown(ctx)
	}
	return n.proc.Shutdown(ctx)
}

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
	as, uncertain := assign(before, outs)
	claimed := make([]bool, len(g))
	for oi, o := range outs {
		a := as[oi]
		if a.input >= 0 && !a.copy {
			parent := a.input
			claimed[parent] = true
			out.Items = append(out.Items, unit.Metric{ID: g[parent].ID, From: g[parent].From, Data: o})
			for _, e := range n.explainChanges(ctx, before[parent], o) {
				emit(g[parent].ID, e)
			}
			if a.uncertain != "" {
				emit(g[parent].ID, lineage.Event{Action: lineage.Unverified, Reason: a.uncertain})
			}
			if n.uncertain != "" {
				emit(g[parent].ID, lineage.Event{Action: lineage.Unverified, Reason: n.uncertain})
			}
			continue
		}
		// A new signal: a copy of one input, or an output no single input
		// explains (then every input of the unit is a parent).
		var from []lineage.SignalID
		reason := "created by the " + n.typ + " processor"
		if a.input >= 0 {
			from = []lineage.SignalID{g[a.input].ID}
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
	for i, it := range g {
		if !claimed[i] && !hasChild(out.Signals, it.ID) {
			if why, ok := uncertain[i]; ok {
				emit(it.ID, lineage.Event{Action: lineage.Unverified, Reason: why + "; this input may have been dropped"})
				continue
			}
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

// assignment says which input an output came from.
type assignment struct {
	input int  // index into the unit's inputs; -1: no single input explains it
	copy  bool // a further output from an input that's already matched
	// uncertain is set when several inputs fit and the match is by order.
	uncertain string
}

// assign matches a unit's outputs to its inputs. With one input, every
// output comes from it. In a metric group, outputs are matched by timestamps
// and value, and among those, by point attributes:
//
//   - an input with the same timestamps, value and attributes is a match
//     (inputs identical in all three are interchangeable, so ties go in order);
//   - if the processor changed the attributes, the only remaining input with
//     the same timestamps and value is a match;
//   - if several remain, the outputs are matched in order, and the match is
//     uncertain unless the outputs are identical, one per input. Inputs left
//     over from an uncertain match may or may not have been dropped.
//
// The second return value lists those uncertain leftover inputs, with why.
func assign(before, outs []pmetric.Metrics) ([]assignment, map[int]string) {
	as := make([]assignment, len(outs))
	uncertain := map[int]string{}
	if len(before) == 1 {
		for i := range outs {
			as[i] = assignment{input: 0, copy: i > 0}
		}
		return as, uncertain
	}
	inKey, inAttrs := make([]string, len(before)), make([]string, len(before))
	for i, md := range before {
		inKey[i], inAttrs[i] = pointKey(md), attrsKey(md)
	}
	outKey, outAttrs := make([]string, len(outs)), make([]string, len(outs))
	var keys []string
	byKey := map[string][]int{}
	for i, md := range outs {
		outKey[i], outAttrs[i] = pointKey(md), attrsKey(md)
		if _, ok := byKey[outKey[i]]; !ok {
			keys = append(keys, outKey[i])
		}
		byKey[outKey[i]] = append(byKey[outKey[i]], i)
	}
	claimed := make([]bool, len(before))
	for _, k := range keys {
		var ins []int
		for i := range before {
			if inKey[i] == k {
				ins = append(ins, i)
			}
		}
		if len(ins) == 0 {
			for _, o := range byKey[k] {
				as[o] = assignment{input: -1}
			}
			continue
		}
		var rest []int
		for _, o := range byKey[k] {
			found := -1
			for _, i := range ins {
				if !claimed[i] && inAttrs[i] == outAttrs[o] {
					found = i
					break
				}
			}
			if found < 0 {
				rest = append(rest, o)
				continue
			}
			claimed[found] = true
			as[o] = assignment{input: found}
		}
		var left []int
		for _, i := range ins {
			if !claimed[i] {
				left = append(left, i)
			}
		}
		switch {
		case len(rest) == 0:
		case len(left) == 0:
			for _, o := range rest {
				as[o] = assignment{input: ins[0], copy: true}
			}
		case len(left) == 1:
			claimed[left[0]] = true
			as[rest[0]] = assignment{input: left[0]}
			for _, o := range rest[1:] {
				as[o] = assignment{input: left[0], copy: true}
			}
		default:
			identical := len(rest) == len(left)
			for _, o := range rest[1:] {
				identical = identical && outAttrs[o] == outAttrs[rest[0]]
			}
			reason := ""
			if !identical {
				reason = fmt.Sprintf("%d inputs have the same timestamps and value, and the processor changed their point attributes, so which input each output came from is uncertain", len(left))
			}
			for j, o := range rest {
				if j < len(left) {
					claimed[left[j]] = true
					as[o] = assignment{input: left[j], uncertain: reason}
				} else {
					as[o] = assignment{input: left[0], copy: true}
				}
			}
			if reason != "" {
				for _, i := range left {
					if !claimed[i] {
						uncertain[i] = reason
					}
				}
			}
		}
	}
	return as, uncertain
}

// attrsKey identifies an item's data point attributes.
func attrsKey(md pmetric.Metrics) string {
	b, _ := json.Marshal(pointAttrs(unit.FirstMetric(md)).AsRaw())
	return string(b)
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
// transform, each change names the statement that made it, a statement that
// changed nothing but would have without its condition reports what it would
// have changed (skipped), and the explanation is checked against the
// processor's real output for this item.
func (n *Node) explainChanges(ctx context.Context, before, after pmetric.Metrics) []lineage.Event {
	changes := diff(before, after)
	for i := range changes {
		changes[i].Reason = "changed by the " + n.typ + " processor"
	}
	if n.stmts == nil {
		return changes
	}
	effects, final, ok := n.stmts.explain(ctx, before)
	if !ok {
		return changes
	}
	key := func(e lineage.Event) string {
		level, _ := e.Detail["level"].(string)
		return level + "|" + e.Attribute
	}
	changed := map[string]bool{}
	for i := range changes {
		k := key(changes[i])
		changed[k] = true
		for _, ef := range effects { // the last statement to touch it wins
			for _, c := range ef.changes {
				if key(c) == k {
					changes[i].Reason = "set by " + ef.stmt.describe()
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
			d["where"] = ef.stmt.where
			if w.Action == lineage.Set {
				d["would_set"] = w.Value
			} else {
				d["would"] = string(w.Action)
			}
			out = append(out, lineage.Event{
				Attribute: w.Attribute, Action: lineage.Skipped, Detail: d,
				Reason: ef.stmt.describe() + " didn't run: its condition is false for this signal",
			})
		}
	}
	if mismatch := diff(final, after); len(mismatch) > 0 {
		out = append(out, lineage.Event{Action: lineage.Unverified,
			Reason: "the transform processor changed this data point differently when it ran on the whole metric, so statement details may be wrong"})
	}
	return out
}

func withStatement(d map[string]any, s *statement) map[string]any {
	out := map[string]any{}
	for k, v := range d {
		out[k] = v
	}
	out["statement"] = s.text
	if s.context != "" {
		out["context"] = s.context
	}
	return out
}
