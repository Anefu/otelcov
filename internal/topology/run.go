package topology

import (
	"context"
	"fmt"

	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/Anefu/otelcov/internal/collector"
	"github.com/Anefu/otelcov/internal/graph"
	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/pluginhost"
	"github.com/Anefu/otelcov/internal/unit"
)

// CopyNode is the lineage node of copies made where data fans out.
const CopyNode = "topology"

// Inputs are what a run needs besides the topology.
type Inputs struct {
	// Plugins model the collectors' processors (decision 67): each
	// collector's distribution and version picks one.
	Plugins *pluginhost.Host
}

// Runner moves items through the collectors, following joins.
type Runner struct {
	f       *File
	v       *Validation
	in      Inputs
	log     *lineage.Log
	ids     *lineage.IDs
	configs map[string]*collector.Config
	nodes   map[string][]graph.MetricsNode // per collector/pipeline
	// plugins models each collector's processors; noPlugin says why a
	// collector has none.
	plugins  map[string]*pluginhost.Plugin
	noPlugin map[string]string
	// Delivered collects, per backend, the items that reach it.
	Delivered map[string][]unit.Metric
}

// NewRunner prepares a run: it picks the plugin for each collector's
// distribution and version and starts it with the feature gates of the
// collectors it models (Validate rejects conflicting settings). A collector
// without a plugin gets a problem, and its processors are unverified.
func NewRunner(ctx context.Context, f *File, v *Validation, in Inputs, log *lineage.Log) (*Runner, error) {
	r := &Runner{f: f, v: v, in: in, log: log, ids: lineage.NewIDs("c"),
		configs: map[string]*collector.Config{}, nodes: map[string][]graph.MetricsNode{}, Delivered: map[string][]unit.Metric{},
		plugins: map[string]*pluginhost.Plugin{}, noPlugin: map[string]string{}}
	var order []*pluginhost.Plugin
	gates := map[*pluginhost.Plugin][]string{}
	for _, name := range Names(f.Collectors) {
		cfg, err := f.LoadCollector(name)
		if err != nil {
			return nil, err
		}
		r.configs[name] = cfg
		c := f.Collectors[name]
		var why string
		switch {
		case c.Distribution == "" || c.Version == "":
			why = "the topology doesn't say which distribution and version collector " + name + " runs (`distribution`, `version`)"
		case in.Plugins == nil:
			why = "no stage plugins are available"
		default:
			p, w, err := in.Plugins.Resolve(c.Distribution, c.Version)
			if err != nil {
				return nil, err
			}
			if p != nil {
				if _, ok := gates[p]; !ok {
					order = append(order, p)
				}
				gates[p] = append(gates[p], c.FeatureGates...)
				r.plugins[name] = p
				continue
			}
			why = w
		}
		r.noPlugin[name] = why
		v.Problems = append(v.Problems, Problem{Severity: Unverified, Where: name,
			Message: why + ", so its processors are unverified"})
	}
	for _, p := range order {
		if err := in.Plugins.Start(ctx, p, gates[p]); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Group is recorded items that one instance of a tapped collector received:
// one recording.
type Group struct {
	Collector string
	// Node is the instance's Kubernetes node (a daemonset agent's); "" when
	// unknown.
	Node  string
	Items []unit.Metric
}

// Tapped returns the collectors that have a tap, in name order.
func (f *File) Tapped() []string {
	var out []string
	for _, name := range Names(f.Collectors) {
		if f.Collectors[name].Tap != "" {
			out = append(out, name)
		}
	}
	return out
}

// Run feeds each group of recorded items into its collector, as the instance
// on its node, and follows them to the backends. Collectors after the first
// (e.g. a gateway) run as one shared instance.
func (r *Runner) Run(ctx context.Context, groups []Group) error {
	defer r.shutdown(ctx)
	for _, g := range groups {
		c, ok := r.f.Collectors[g.Collector]
		if !ok || c.Tap == "" {
			return fmt.Errorf("a recording is for collector %q, which has no tap in the topology", g.Collector)
		}
		receivers := map[string]bool{}
		for _, rc := range r.configs[g.Collector].Service.Pipelines[c.Tap].Receivers {
			receivers[rc] = true
		}
		if err := r.flow(ctx, g.Collector, receivers, g.Items, 0, g.Node); err != nil {
			return err
		}
	}
	return nil
}

// flow runs items through the pipelines of one collector instance. node is
// the instance's node, for a tapped collector's instance; "" otherwise.
func (r *Runner) flow(ctx context.Context, coll string, receivers map[string]bool, items []unit.Metric, depth int, node string) error {
	if depth > 16 {
		return fmt.Errorf("joins form a loop through %s", coll)
	}
	if len(items) == 0 {
		return nil
	}
	cfg := r.configs[coll]
	c := r.f.Collectors[coll]
	var pipelines []string
	for _, pid := range Names(cfg.Service.Pipelines) {
		if c.Recording(pid) || !IsMetrics(pid) {
			continue
		}
		for _, rc := range cfg.Service.Pipelines[pid].Receivers {
			if receivers[rc] {
				pipelines = append(pipelines, pid)
				break
			}
		}
	}
	if len(pipelines) == 0 {
		return r.record(coll, eachEvent(items, lineage.Event{Node: coll, Action: lineage.Dropped,
			Reason: "no metrics pipeline of " + coll + " uses the receiver it arrives on"}))
	}

	for pi, pid := range pipelines {
		in := items
		if pi > 0 {
			var err error
			if in, err = r.copies(items, "the receiver also feeds "+coll+" pipeline "+pid); err != nil {
				return err
			}
		}
		nodes, err := r.pipelineNodes(ctx, coll, pid, node)
		if err != nil {
			return err
		}
		out, err := graph.Process(ctx, nodes, in, r.log)
		if err != nil {
			return err
		}
		for ei, exp := range cfg.Service.Pipelines[pid].Exporters {
			x := out
			if ei > 0 {
				if x, err = r.copies(out, coll+" pipeline "+pid+" also exports to "+exp); err != nil {
					return err
				}
			}
			if err := r.export(ctx, coll, exp, x, depth); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Runner) export(ctx context.Context, coll, exp string, items []unit.Metric, depth int) error {
	key := coll + "/exporters/" + exp
	node := exporterName(coll, exp)
	for _, e := range r.f.External {
		if e == key {
			return r.record(node, eachEvent(items, lineage.Event{Node: node, Action: lineage.Exported,
				Reason: "leaves the modeled system: the exporter is listed as external in the topology"}))
		}
	}
	for i, j := range r.f.Joins {
		if j.From != key {
			continue
		}
		if reason, bad := r.v.BadJoins[i]; bad {
			if err := r.record(node, eachEvent(items, lineage.Event{Node: node, Action: lineage.Unverified,
				Reason: "the join " + j.From + " -> " + j.To + " can't be trusted: " + reason})); err != nil {
				return err
			}
		}
		to, err := r.f.ParseEndpoint(j.To)
		if err != nil {
			return err
		}
		if to.Backend != "" {
			r.Delivered[to.Backend] = append(r.Delivered[to.Backend], items...)
			return nil
		}
		return r.flow(ctx, to.Collector, map[string]bool{to.ID: true}, items, depth+1, "")
	}
	return fmt.Errorf("%s is neither joined nor external", key)
}

// pipelineNodes builds (once per collector instance) the processor nodes of
// a pipeline.
func (r *Runner) pipelineNodes(ctx context.Context, coll, pid, node string) ([]graph.MetricsNode, error) {
	key := coll + "/" + pid + "@" + node
	if n, ok := r.nodes[key]; ok {
		return n, nil
	}
	cfg := r.configs[coll]
	var nodes []graph.MetricsNode
	for _, id := range cfg.Service.Pipelines[pid].Processors {
		name := processorName(cfg, r.f.Collectors[coll], coll, pid, id)
		n, err := r.processorNode(ctx, coll, name, id, cfg.Processors[id], node)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	r.nodes[key] = nodes
	return nodes, nil
}

// processorName is a processor's node name in lineage:
// "<collector>/<processor id>", or "<collector>/<pipeline>/<processor id>" when
// several modeled pipelines of the collector use the processor.
func processorName(cfg *collector.Config, c Collector, coll, pid, id string) string {
	uses := 0
	for p, pl := range cfg.Service.Pipelines {
		if !IsMetrics(p) || c.Recording(p) {
			continue
		}
		for _, x := range pl.Processors {
			if x == id {
				uses++
			}
		}
	}
	if uses > 1 {
		return coll + "/" + pid + "/" + id
	}
	return coll + "/" + id
}

// exporterName is an exporter's node name in lineage.
func exporterName(coll, exp string) string { return coll + "/" + exp }

func (r *Runner) processorNode(ctx context.Context, coll, name, id string, raw map[string]any, node string) (graph.MetricsNode, error) {
	p := r.plugins[coll]
	if p == nil {
		return graph.Unsupported{NodeName: name, Reason: r.noPlugin[coll]}, nil
	}
	return p.Stage(ctx, "processor", id, name, raw, pluginhost.Instance{Collector: coll, Node: node, Env: r.f.Collectors[coll].Env}, r.ids)
}

// copies duplicates items for another branch, with new IDs.
func (r *Runner) copies(items []unit.Metric, reason string) ([]unit.Metric, error) {
	out := make([]unit.Metric, len(items))
	var events []lineage.Event
	var signals []lineage.Signal
	for i, it := range items {
		id := r.ids.Next()
		data := pmetric.NewMetrics()
		it.Data.CopyTo(data)
		from := []lineage.SignalID{it.ID}
		out[i] = unit.Metric{ID: id, From: from, Data: data}
		signals = append(signals, lineage.Signal{ID: id, Node: CopyNode, Kind: "otlp_metric",
			Name: unit.FirstMetric(data).Name(), From: from, Resource: unit.Resource(data).Attributes().AsRaw()})
		events = append(events, lineage.Event{Node: CopyNode, Signal: id, Action: lineage.Derived, From: from, Reason: reason})
	}
	ids := make([]lineage.SignalID, len(out))
	for i, it := range out {
		ids[i] = it.ID
	}
	return out, graph.Record(r.log, CopyNode, events, signals, ids)
}

func (r *Runner) record(node string, events []lineage.Event) error {
	return graph.Record(r.log, node, events, nil, nil)
}

func (r *Runner) shutdown(ctx context.Context) {
	for _, ns := range r.nodes {
		graph.Shutdown(ctx, ns)
	}
}

func eachEvent(items []unit.Metric, e lineage.Event) []lineage.Event {
	out := make([]lineage.Event, len(items))
	for i, it := range items {
		out[i] = e
		out[i].Signal = it.ID
	}
	return out
}
