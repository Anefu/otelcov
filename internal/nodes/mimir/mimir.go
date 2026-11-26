// Package mimir models how Grafana Mimir's OTLP endpoint turns OTLP metrics
// into Prometheus series.
//
// The conversion is Prometheus's own OTLP translator (prometheusremotewrite),
// which Mimir's distributor is based on, configured from Mimir's per-tenant
// limits. Outputs are Prometheus-shaped series items (see unit.NewSeries).
package mimir

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/otlptranslator"
	promconfig "github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/exemplar"
	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	prw "github.com/prometheus/prometheus/storage/remote/otlptranslator/prometheusremotewrite"
	"sigs.k8s.io/yaml"

	"github.com/Anefu/otelcov/internal/collector"
	"github.com/Anefu/otelcov/internal/graph"
	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/unit"
)

// Prefix starts backend node names in lineage: "backend/<backend name>".
const Prefix = "backend/"

// Type is the topology backend type this node models.
const Type = "mimir-otlp"

// The Mimir versions the model is tested for (decision 72): the conformance
// suite (TestConformance) runs real Mimir at both ends of the range, and any
// version in between counts as tested. CI runs it at 3.0.0 and 3.2.6
// (.github/workflows/mimir-conformance.yml).
const (
	TestedMin = "3.0"
	TestedMax = "3.2"
)

// note says what the model is.
const note = "Prometheus's OTLP translator configured from Mimir's limits"

// Fidelity is the model's for a deployed Mimir version (decisions 71, 72):
// Prometheus's OTLP translator, configured from Mimir's limits, is otelcov's
// own model of Mimir's OTLP ingestion. It passes conformance for the versions
// the differential suite covers; for others, or without a version, answers
// that depend on it are modelled.
func Fidelity(version string) lineage.Stage {
	st := lineage.Stage{Fidelity: lineage.Reimplementation, Note: note}
	v, ok := minor(version)
	lo, _ := minor(TestedMin)
	hi, _ := minor(TestedMax)
	switch {
	case version == "":
		st.Note += "; the topology doesn't say which Mimir version runs, and the model is tested for " + TestedMin + "–" + TestedMax
	case !ok:
		st.Note += fmt.Sprintf("; version %q isn't a version like 3.2.3", version)
	case less(v, lo) || less(hi, v):
		st.Note += "; tested for Mimir " + TestedMin + "–" + TestedMax + ", not " + version
	default:
		st.Conformance = true
	}
	return st
}

// minor parses the major and minor version of "3.2", "3.2.3" or "v3.2.3".
func minor(v string) ([2]int, bool) {
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) < 2 || len(parts) > 3 {
		return [2]int{}, false
	}
	var out [2]int
	for i := range out {
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 {
			return [2]int{}, false
		}
		out[i] = n
	}
	return out, true
}

func less(a, b [2]int) bool { return a[0] < b[0] || a[0] == b[0] && a[1] < b[1] }

// Limits are the Mimir per-tenant limits otelcov models, for one tenant,
// with what Load found about the others (decision 74).
type Limits struct {
	// PromoteOTelResourceAttributes is a comma-separated list of resource
	// attributes that become labels.
	PromoteOTelResourceAttributes string `json:"promote_otel_resource_attributes"`
	// OTelMetricSuffixesEnabled adds unit and type suffixes to metric names.
	OTelMetricSuffixesEnabled bool `json:"otel_metric_suffixes_enabled"`
	// OTelKeepIdentifyingResourceAttributes keeps service.* attributes on target_info.
	OTelKeepIdentifyingResourceAttributes bool `json:"otel_keep_identifying_resource_attributes"`
	// OTelLabelNameUnderscoreSanitization prefixes "key" to label names
	// that start with a single underscore. Mimir's default is true.
	OTelLabelNameUnderscoreSanitization *bool `json:"otel_label_name_underscore_sanitization"`
	// OTelLabelNamePreserveMultipleUnderscores keeps consecutive
	// underscores in label names. Mimir's default is true.
	OTelLabelNamePreserveMultipleUnderscores *bool `json:"otel_label_name_preserve_multiple_underscores"`

	// Label limits: Mimir refuses series with more labels, or longer names
	// or values, than these (defaults 30, 80 for info series, 1024, 2048).
	// With the "truncate" or "drop" strategy, Mimir hashes long values
	// instead, which otelcov doesn't model.
	MaxLabelNamesPerSeries            *int   `json:"max_label_names_per_series"`
	MaxLabelNamesPerInfoSeries        *int   `json:"max_label_names_per_info_series"`
	MaxLabelNameLength                *int   `json:"max_label_name_length"`
	MaxLabelValueLength               *int   `json:"max_label_value_length"`
	LabelValueLengthOverLimitStrategy string `json:"label_value_length_over_limit_strategy"`

	// Tenant is the tenant these limits are for, "" when unknown.
	Tenant string `json:"-"`
	// Unverified says why answers through the backend are unverified:
	// limits otelcov doesn't model, set to other than Mimir's default;
	// limits it doesn't know; runtime config it can't apply.
	Unverified []string `json:"-"`
	// Assumed lists the load, timing and size limits set explicitly, as
	// name=value; answers assume the tenant stays within them anyway.
	Assumed []string `json:"-"`
}

// Source says where a Mimir backend's limits come from (decision 74).
type Source struct {
	// Config is Mimir's configuration file: its limits section holds every
	// tenant's defaults.
	Config string
	// RuntimeConfig is the runtime configuration file Mimir reads
	// (runtime_config.file), as rendered; "" when the topology doesn't give
	// it.
	RuntimeConfig string
	// Tenant is the tenant the stack writes as; "" means Mimir's no-auth
	// tenant when multitenancy is off, and unknown otherwise.
	Tenant string
}

// identifying resource attributes become job and instance, not target_info labels.
var identifying = map[string]bool{"service.name": true, "service.namespace": true, "service.instance.id": true}

// Node is the Mimir OTLP backend node.
type Node struct {
	name     string
	limits   Limits
	promote  []string
	settings prw.Settings
}

// Load reads a Mimir backend's limits for the tenant the stack writes as:
// the config's limits section, with the tenant's overrides from the runtime
// config over it, as Mimir applies them. Every limit is classified
// (limits.go): modelled ones are read, ignored ones skipped, assumed ones
// listed in Assumed, and ones otelcov doesn't model or know, unless set to
// Mimir's default, make answers unverified (Unverified). Other sections of
// the config (storage, rings, the ruler) don't affect what's stored and are
// ignored.
func Load(src Source) (Limits, error) {
	data, err := os.ReadFile(src.Config)
	if err != nil {
		return Limits{}, err
	}
	var f struct {
		Limits        map[string]any `json:"limits"`
		Multitenancy  *bool          `json:"multitenancy_enabled"`
		NoAuthTenant  string         `json:"no_auth_tenant"`
		RuntimeConfig struct {
			File string `json:"file"`
		} `json:"runtime_config"`
	}
	if err := yaml.Unmarshal(data, &f); err != nil {
		return Limits{}, fmt.Errorf("%s: %w", src.Config, err)
	}
	var l Limits
	raw := map[string]any{}
	for k, v := range f.Limits {
		raw[k] = v
	}

	// Without multitenancy, Mimir stores everything as its no-auth tenant.
	l.Tenant = src.Tenant
	if f.Multitenancy != nil && !*f.Multitenancy {
		l.Tenant = "anonymous"
		if f.NoAuthTenant != "" {
			l.Tenant = f.NoAuthTenant
		}
	}
	switch {
	case src.RuntimeConfig != "":
		if err := l.applyRuntime(src.RuntimeConfig, raw); err != nil {
			return Limits{}, err
		}
	case f.RuntimeConfig.File != "":
		l.Unverified = append(l.Unverified, "Mimir's runtime config ("+f.RuntimeConfig.File+") can override limits per tenant, and the topology doesn't give it (runtime_config)")
	}

	modelled := map[string]any{}
	for _, k := range sortedKeys(raw) {
		v := raw[k]
		u, unmodelled := unmodelledLimits[k]
		switch {
		case modelledLimits[k]:
			modelled[k] = v
		case ignoredLimits[k]:
		case assumedLimits[k]:
			l.Assumed = append(l.Assumed, k+"="+text(v))
		case unmodelled && isDefault(v, u.def):
		case unmodelled:
			l.Unverified = append(l.Unverified, "Mimir limit "+k+" is "+text(v)+", which otelcov doesn't model: it "+u.what)
		default:
			l.Unverified = append(l.Unverified, "Mimir limit "+k+" isn't one otelcov knows (it classified Mimir "+TestedMin+"–"+TestedMax+"'s)")
		}
	}
	if err := collector.Decode(modelled, &l); err != nil {
		return Limits{}, fmt.Errorf("%s: limits: %w", src.Config, err)
	}
	return l, nil
}

// applyRuntime puts the tenant's overrides from a runtime config over raw.
func (l *Limits) applyRuntime(path string, raw map[string]any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var rt map[string]any
	if err := yaml.Unmarshal(data, &rt); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for _, k := range sortedKeys(rt) {
		switch k {
		case "overrides", "multi_kv_config":
		case "ingester_limits", "distributor_limits":
			// Instance limits: load, like the assumed per-tenant ones.
			if rt[k] != nil {
				l.Assumed = append(l.Assumed, k+"="+text(rt[k]))
			}
		default:
			l.Unverified = append(l.Unverified, "Mimir's runtime config has a section "+k+", which otelcov doesn't know")
		}
	}
	overrides, _ := rt["overrides"].(map[string]any)
	if l.Tenant == "" {
		if len(overrides) > 0 {
			l.Unverified = append(l.Unverified, "Mimir's runtime config overrides limits for tenants "+strings.Join(sortedKeys(overrides), ", ")+
				", and the topology doesn't say which tenant the stack writes as (tenant)")
		}
		return nil
	}
	own, _ := overrides[l.Tenant].(map[string]any)
	for k, v := range own {
		raw[k] = v
	}
	return nil
}

// isDefault reports whether a limit's value is Mimir's default; an empty list
// or map is the default nil.
func isDefault(v, def any) bool {
	if def == nil {
		switch v := v.(type) {
		case nil:
			return true
		case []any:
			return len(v) == 0
		case map[string]any:
			return len(v) == 0
		case string:
			return v == ""
		}
		return false
	}
	return reflect.DeepEqual(v, def)
}

// text formats a limit's value for messages.
func text(v any) string {
	if s, ok := v.(string); ok {
		return strconv.Quote(s)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Assumption is what answers through a Mimir backend assume, for reports.
func (l Limits) Assumption() string {
	who := "the tenant"
	if l.Tenant != "" {
		who = "tenant " + strconv.Quote(l.Tenant)
	}
	msg := "answers assume " + who + " stays within Mimir's rate, series, sample-time, query and rule limits, which a replay can't check"
	if len(l.Assumed) > 0 {
		msg += " (set here: " + strings.Join(l.Assumed, ", ") + ")"
	}
	return msg
}

// New builds the node from Mimir limits. backend is the backend's name in the
// topology; the node is named "backend/<backend>".
func New(backend string, l Limits) *Node {
	var promote []string
	for _, a := range strings.Split(l.PromoteOTelResourceAttributes, ",") {
		if a = strings.TrimSpace(a); a != "" {
			promote = append(promote, a)
		}
	}
	return &Node{
		name:    Prefix + backend,
		limits:  l,
		promote: promote,
		settings: prw.Settings{
			AddMetricSuffixes:                 l.OTelMetricSuffixesEnabled,
			PromoteResourceAttributes:         prw.NewPromoteResourceAttributes(promconfig.OTLPConfig{PromoteResourceAttributes: promote}),
			KeepIdentifyingResourceAttributes: l.OTelKeepIdentifyingResourceAttributes,
			// Mimir validates names with the legacy scheme by default and
			// rejects delta temporality unless native delta ingestion is on.
			AllowUTF8:                            false,
			AllowDeltaTemporality:                false,
			LabelNameUnderscoreSanitization:      orTrue(l.OTelLabelNameUnderscoreSanitization),
			LabelNamePreserveMultipleUnderscores: orTrue(l.OTelLabelNamePreserveMultipleUnderscores),
		},
	}
}

func orTrue(b *bool) bool { return b == nil || *b }

// LabelName is the label Mimir makes of a resource attribute with its
// default limits, or "" if it can't make one.
func LabelName(attr string) string {
	name, err := (&otlptranslator.LabelNamer{UnderscoreLabelSanitization: true, PreserveMultipleUnderscores: true}).Build(attr)
	if err != nil {
		return ""
	}
	return name
}

// appender collects what the translator writes, one entry per series in the
// order first written.
type appender struct {
	series []*appended
	byKey  map[string]*appended
}

type appended struct {
	lset    labels.Labels
	samples []unit.Sample
}

func (a *appender) get(ls labels.Labels) *appended {
	key := ls.String()
	s, ok := a.byKey[key]
	if !ok {
		s = &appended{lset: ls.Copy()}
		a.byKey[key] = s
		a.series = append(a.series, s)
	}
	return s
}

func (a *appender) AppendSample(ls labels.Labels, _ prw.Metadata, _, t int64, v float64, _ []exemplar.Exemplar) error {
	s := a.get(ls)
	s.samples = append(s.samples, unit.Sample{T: time.UnixMilli(t), V: v})
	return nil
}

// AppendHistogram keeps a native histogram sample, as Mimir stores
// exponential histograms (decision 73).
func (a *appender) AppendHistogram(ls labels.Labels, _ prw.Metadata, _, t int64, h *histogram.Histogram, _ []exemplar.Exemplar) error {
	s := a.get(ls)
	s.samples = append(s.samples, unit.Sample{T: time.UnixMilli(t), H: h.ToFloat(nil)})
	return nil
}

func (n *Node) Name() string { return n.name }

// converted is one output series before IDs are assigned.
type converted struct {
	lset    labels.Labels
	samples []unit.Sample
	from    []lineage.SignalID
	target  bool // target_info
	// for data series: the first input it came from
	input unit.Metric
}

// add merges a sample, ignoring duplicates at the same timestamp (the backend
// keeps the first).
func (c *converted) add(s unit.Sample) {
	for _, have := range c.samples {
		if have.T.Equal(s.T) {
			return
		}
	}
	c.samples = append(c.samples, s)
}

func (n *Node) ProcessMetrics(ctx context.Context, in []unit.Metric) (graph.Output, error) {
	var (
		out     graph.Output
		data    []*converted
		targets []*converted
		byKey   = map[string]*converted{}
	)
	for _, it := range in {
		if len(n.limits.Unverified) > 0 {
			out.Events = append(out.Events, lineage.Event{
				Node: n.name, Signal: it.ID, Action: lineage.Unverified, Reason: strings.Join(n.limits.Unverified, "; "),
			})
		}
		app := &appender{byKey: map[string]*appended{}}
		_, err := prw.NewPrometheusConverter(app).FromMetrics(ctx, it.Data, n.settings)
		series := app.series
		if err != nil {
			// The metric is rejected. target_info for its resource may still
			// be written, so the remaining series are kept.
			out.Events = append(out.Events, lineage.Event{
				Node: n.name, Signal: it.ID, Action: lineage.Dropped,
				Reason: "Mimir's OTLP translation rejects it: " + err.Error(),
			})
		}
		sort.Slice(series, func(i, j int) bool { return series[i].lset.String() < series[j].lset.String() })
		for _, ts := range series {
			key := ts.lset.String()
			cv, dup := byKey[key]
			if dup {
				// The same series from several inputs: later samples of one
				// series, or target_info for items of one resource. One
				// series, several parents.
				if !containsID(cv.from, it.ID) {
					cv.from = append(cv.from, it.ID)
				}
			} else {
				cv = &converted{lset: ts.lset, from: []lineage.SignalID{it.ID}, input: it}
				byKey[key] = cv
			}
			for _, smp := range ts.samples {
				cv.add(smp)
			}
			if dup {
				continue
			}
			if cv.lset.Get(labels.MetricName) == "target_info" {
				cv.target = true
				targets = append(targets, cv)
			} else {
				data = append(data, cv)
			}
		}
	}

	// Data series first, then target_info, each in input order. Mimir
	// refuses series over its label limits.
	next := 0
	for _, cv := range append(data, targets...) {
		refused, unverified := n.checkLabels(cv.lset)
		for _, from := range cv.from {
			if refused != "" {
				out.Events = append(out.Events, lineage.Event{Node: n.name, Signal: from, Action: lineage.Dropped, Reason: refused})
			}
			if unverified != "" {
				out.Events = append(out.Events, lineage.Event{Node: n.name, Signal: from, Action: lineage.Unverified, Reason: unverified})
			}
		}
		if refused != "" {
			continue
		}
		next++
		id := lineage.SignalID("p" + strconv.Itoa(next))
		data := unit.NewSeries(cv.lset, cv.samples...)
		out.Items = append(out.Items, unit.Metric{ID: id, From: cv.from, Data: data})
		out.Signals = append(out.Signals, lineage.Signal{
			ID: id, Node: n.name, Kind: "series", From: cv.from, Labels: cv.lset.Map(), Value: latest(cv.samples),
		})
		if cv.target {
			out.Events = append(out.Events, lineage.Event{
				Node: n.name, Signal: id, Action: lineage.Derived, From: cv.from,
				Reason: "target_info carries the resource attributes that aren't promoted to labels",
			})
			continue
		}
		out.Events = append(out.Events, n.dataEvents(id, cv)...)
	}
	return out, nil
}

// dataEvents explains a data series: where its name and labels came from, and
// which promoted attributes were missing.
func (n *Node) dataEvents(id lineage.SignalID, cv *converted) []lineage.Event {
	res := unit.Resource(cv.input.Data).Attributes()
	namer := otlptranslator.LabelNamer{UTF8Allowed: n.settings.AllowUTF8,
		UnderscoreLabelSanitization: n.settings.LabelNameUnderscoreSanitization, PreserveMultipleUnderscores: n.settings.LabelNamePreserveMultipleUnderscores}

	promoted := map[string]any{}
	promoteSet := map[string]bool{}
	for _, a := range n.promote {
		promoteSet[a] = true
		if _, ok := res.Get(a); ok {
			promoted[a], _ = namer.Build(a)
		}
	}
	targetOnly := []any{}
	for k := range res.All() {
		if !promoteSet[k] && !identifying[k] {
			targetOnly = append(targetOnly, k)
		}
	}
	sort.Slice(targetOnly, func(i, j int) bool { return targetOnly[i].(string) < targetOnly[j].(string) })

	detail := map[string]any{
		"metric":           map[string]any{"from": unit.FirstMetric(cv.input.Data).Name(), "to": cv.lset.Get(labels.MetricName)},
		"promoted":         promoted,
		"target_info_only": targetOnly,
	}
	if cv.lset.Has("job") {
		detail["job_from"] = "service.name"
		if _, ok := res.Get("service.namespace"); ok {
			detail["job_from"] = "service.namespace/service.name"
		}
	}
	if cv.lset.Has("instance") {
		detail["instance_from"] = "service.instance.id"
	}
	events := []lineage.Event{{Node: n.name, Signal: id, Action: lineage.Derived, From: cv.from, Detail: detail}}
	for _, a := range n.promote {
		if _, ok := res.Get(a); !ok {
			events = append(events, lineage.Event{
				Node: n.name, Signal: id, Attribute: a, Action: lineage.Skipped,
				Reason: "listed in promote_otel_resource_attributes, but absent on the input resource",
			})
		}
	}
	return events
}

// Mimir's defaults for the label limits.
const (
	defaultMaxLabelNames        = 30
	defaultMaxLabelNamesForInfo = 80
	defaultMaxLabelNameLength   = 1024
	defaultMaxLabelValueLength  = 2048
)

func orDefault(v *int, def int) int {
	if v == nil {
		return def
	}
	return *v
}

// checkLabels applies Mimir's label limits to a series: why Mimir refuses
// it, or why what it stores is unverified (a long value it hashes).
func (n *Node) checkLabels(lset labels.Labels) (refused, unverified string) {
	name := lset.Get(labels.MetricName)
	max, limit := orDefault(n.limits.MaxLabelNamesPerSeries, defaultMaxLabelNames), "max_label_names_per_series"
	if info := orDefault(n.limits.MaxLabelNamesPerInfoSeries, defaultMaxLabelNamesForInfo); isInfo(name) && info > max {
		max, limit = info, "max_label_names_per_info_series"
	}
	if c := labelCount(lset); c > max {
		return fmt.Sprintf("Mimir refuses series %s: it has %d labels, more than %s (%d)", name, c, limit, max), ""
	}
	maxName := orDefault(n.limits.MaxLabelNameLength, defaultMaxLabelNameLength)
	maxValue := orDefault(n.limits.MaxLabelValueLength, defaultMaxLabelValueLength)
	lset.Range(func(l labels.Label) {
		switch {
		case refused != "":
		case labelLength(l.Name) > maxName:
			refused = fmt.Sprintf("Mimir refuses series %s: label name %s is longer than max_label_name_length (%d)", name, l.Name, maxName)
		case labelLength(l.Value) <= maxValue:
		case n.limits.LabelValueLengthOverLimitStrategy == "" || n.limits.LabelValueLengthOverLimitStrategy == "error":
			refused = fmt.Sprintf("Mimir refuses series %s: label %s's value is longer than max_label_value_length (%d)", name, l.Name, maxValue)
		default:
			unverified = fmt.Sprintf("Mimir replaces label %s's value on series %s with a hash, since it's longer than max_label_value_length (%d) (label_value_length_over_limit_strategy %s); otelcov doesn't model the hash",
				l.Name, name, maxValue, n.limits.LabelValueLengthOverLimitStrategy)
		}
	})
	if refused != "" {
		unverified = ""
	}
	return refused, unverified
}

// How Mimir applies the label limits, as the conformance suite's
// label-limits cases show at 3.0 and 3.2: an info series is one whose name
// ends in _info, __name__ counts as a label, and lengths are in bytes.

func isInfo(name string) bool           { return strings.HasSuffix(name, "_info") }
func labelCount(lset labels.Labels) int { return lset.Len() }
func labelLength(s string) int          { return len(s) }

// latest is a series' latest value for lineage: a float, or a native
// histogram in Prometheus's text form.
func latest(samples []unit.Sample) any {
	if len(samples) == 0 {
		return 0.0
	}
	last := samples[0]
	for _, s := range samples[1:] {
		if s.T.After(last.T) {
			last = s
		}
	}
	if last.H != nil {
		return last.H.String()
	}
	return last.V
}

func containsID(ids []lineage.SignalID, id lineage.SignalID) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}
