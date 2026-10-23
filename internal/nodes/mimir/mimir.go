// Package mimir models how Grafana Mimir's OTLP endpoint turns OTLP metrics
// into Prometheus series.
//
// The conversion is Prometheus's own OTLP translator (prometheusremotewrite),
// which Mimir's distributor is based on, configured from Mimir's per-tenant
// limits. Outputs are Prometheus-shaped series items (see unit.NewSeries).
package mimir

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/otlptranslator"
	promconfig "github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/prompb"
	prw "github.com/prometheus/prometheus/storage/remote/otlptranslator/prometheusremotewrite"
	"sigs.k8s.io/yaml"

	"github.com/Anefu/otelcov/internal/collector"
	"github.com/Anefu/otelcov/internal/graph"
	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/unit"
)

// Name is the node name used in lineage.
const Name = "backend/mimir-otlp"

// Limits are the Mimir per-tenant limits that shape OTLP ingestion.
type Limits struct {
	// PromoteOTelResourceAttributes is a comma-separated list of resource
	// attributes that become labels.
	PromoteOTelResourceAttributes string `json:"promote_otel_resource_attributes"`
	// OTelMetricSuffixesEnabled adds unit and type suffixes to metric names.
	OTelMetricSuffixesEnabled bool `json:"otel_metric_suffixes_enabled"`
	// OTelKeepIdentifyingResourceAttributes keeps service.* attributes on target_info.
	OTelKeepIdentifyingResourceAttributes bool `json:"otel_keep_identifying_resource_attributes"`
}

// identifying resource attributes become job and instance, not target_info labels.
var identifying = map[string]bool{"service.name": true, "service.namespace": true, "service.instance.id": true}

// Node is the Mimir OTLP backend node.
type Node struct {
	limits   Limits
	promote  []string
	settings prw.Settings
}

// Load reads Mimir limits from a file with a top-level "limits" key.
func Load(path string) (Limits, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Limits{}, err
	}
	var f struct {
		Limits map[string]any `json:"limits"`
	}
	if err := yaml.UnmarshalStrict(data, &f); err != nil {
		return Limits{}, fmt.Errorf("%s: %w", path, err)
	}
	var l Limits
	if err := collector.Decode(f.Limits, &l); err != nil {
		return Limits{}, fmt.Errorf("%s: unsupported Mimir limits: %w", path, err)
	}
	return l, nil
}

// New builds the node from Mimir limits.
func New(l Limits) *Node {
	var promote []string
	for _, a := range strings.Split(l.PromoteOTelResourceAttributes, ",") {
		if a = strings.TrimSpace(a); a != "" {
			promote = append(promote, a)
		}
	}
	return &Node{
		limits:  l,
		promote: promote,
		settings: prw.Settings{
			AddMetricSuffixes:                 l.OTelMetricSuffixesEnabled,
			PromoteResourceAttributes:         prw.NewPromoteResourceAttributes(promconfig.OTLPConfig{PromoteResourceAttributes: promote}),
			KeepIdentifyingResourceAttributes: l.OTelKeepIdentifyingResourceAttributes,
			// Mimir validates names with the legacy scheme by default and
			// rejects delta temporality unless native delta ingestion is on.
			AllowUTF8:             false,
			AllowDeltaTemporality: false,
		},
	}
}

func (n *Node) Name() string { return Name }

// converted is one output series before IDs are assigned.
type converted struct {
	lset   labels.Labels
	ts     time.Time
	value  float64
	from   []lineage.SignalID
	target bool // target_info
	// for data series: the input it came from
	input unit.Metric
}

func (n *Node) ProcessMetrics(ctx context.Context, in []unit.Metric) (graph.Output, error) {
	var (
		out     graph.Output
		data    []*converted
		targets []*converted
		byKey   = map[string]*converted{}
	)
	for _, it := range in {
		c := prw.NewPrometheusConverter()
		_, err := c.FromMetrics(ctx, it.Data, n.settings)
		series := c.TimeSeries()
		if err != nil {
			// The metric is rejected. target_info for its resource may still
			// be written, so the remaining series are kept.
			out.Events = append(out.Events, lineage.Event{
				Node: Name, Signal: it.ID, Action: lineage.Dropped,
				Reason: "Mimir's OTLP translation rejects it: " + err.Error(),
			})
		}
		sort.Slice(series, func(i, j int) bool { return seriesKey(series[i]) < seriesKey(series[j]) })
		for _, ts := range series {
			key := seriesKey(ts)
			if prev, dup := byKey[key]; dup {
				// The same series from several inputs, such as target_info
				// for items of one resource: one series, several parents.
				prev.from = append(prev.from, it.ID)
				continue
			}
			cv := &converted{lset: toLabels(ts.Labels), from: []lineage.SignalID{it.ID}, input: it}
			if len(ts.Samples) > 0 {
				s := ts.Samples[len(ts.Samples)-1]
				cv.ts, cv.value = time.UnixMilli(s.Timestamp), s.Value
			}
			byKey[key] = cv
			if cv.lset.Get(labels.MetricName) == "target_info" {
				cv.target = true
				targets = append(targets, cv)
			} else {
				data = append(data, cv)
			}
		}
	}

	// Data series first, then target_info, each in input order.
	next := 0
	for _, cv := range append(data, targets...) {
		next++
		id := lineage.SignalID("p" + strconv.Itoa(next))
		out.Items = append(out.Items, unit.Metric{ID: id, From: cv.from, Data: unit.NewSeries(cv.lset, cv.ts, cv.value)})
		out.Signals = append(out.Signals, lineage.Signal{
			ID: id, Node: Name, Kind: "series", From: cv.from, Labels: cv.lset.Map(), Value: cv.value,
		})
		if cv.target {
			out.Events = append(out.Events, lineage.Event{
				Node: Name, Signal: id, Action: lineage.Derived, From: cv.from,
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
	namer := otlptranslator.LabelNamer{UTF8Allowed: n.settings.AllowUTF8}

	promoted := map[string]any{}
	promoteSet := map[string]bool{}
	for _, a := range n.promote {
		promoteSet[a] = true
		if _, ok := res.Get(a); ok {
			promoted[a] = namer.Build(a)
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
	events := []lineage.Event{{Node: Name, Signal: id, Action: lineage.Derived, From: cv.from, Detail: detail}}
	for _, a := range n.promote {
		if _, ok := res.Get(a); !ok {
			events = append(events, lineage.Event{
				Node: Name, Signal: id, Attribute: a, Action: lineage.Skipped,
				Reason: "listed in promote_otel_resource_attributes, but absent on the input resource",
			})
		}
	}
	return events
}

func toLabels(ls []prompb.Label) labels.Labels {
	b := labels.NewScratchBuilder(len(ls))
	for _, l := range ls {
		b.Add(l.Name, l.Value)
	}
	b.Sort()
	return b.Labels()
}

func seriesKey(ts prompb.TimeSeries) string { return toLabels(ts.Labels).String() }
