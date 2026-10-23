package topology

import (
	"strings"

	"github.com/Anefu/otelcov/internal/collector"
	"github.com/Anefu/otelcov/internal/flowgraph"
	"github.com/Anefu/otelcov/internal/nodes/mimir"
	"github.com/Anefu/otelcov/internal/nodes/realproc"
	"github.com/Anefu/otelcov/internal/nodes/sdkenv"
	"github.com/Anefu/otelcov/internal/nodes/source"
)

// Flow returns the graph of the sources and collectors: each modeled pipeline's
// processors and exporters, in the order data goes through them, following
// joins from the tapped collectors. Node IDs match the runner's lineage node
// names. Backends that exporters are joined to are included; what reads them
// is added by the caller.
func Flow(f *File, v *Validation) (flowgraph.Graph, error) {
	var g flowgraph.Graph
	g.AddNode(flowgraph.Node{ID: source.Name, Group: "source", Label: "Tap recording", Kind: flowgraph.Source})
	g.AddNode(flowgraph.Node{ID: sdkenv.Name, Group: "source", Label: "SDK environment", Kind: flowgraph.Source,
		Note: "OTEL_RESOURCE_ATTRIBUTES and OTEL_SERVICE_NAME from the pods under test"})
	g.AddEdge(flowgraph.Edge{From: source.Name, To: sdkenv.Name})

	configs := map[string]*collector.Config{}
	for _, name := range Names(f.Collectors) {
		cfg, err := f.LoadCollector(name)
		if err != nil {
			return g, err
		}
		configs[name] = cfg
	}
	external := map[string]bool{}
	for _, e := range f.External {
		external[e] = true
	}
	done := map[string]bool{}
	var backends []flowgraph.Node // added after the collectors, so groups stay together

	var walk func(from, note, coll string, receivers map[string]bool, depth int)
	walk = func(from, note, coll string, receivers map[string]bool, depth int) {
		cfg, c := configs[coll], f.Collectors[coll]
		if cfg == nil || depth > 16 {
			return
		}
		for _, pid := range Names(cfg.Service.Pipelines) {
			if c.Recording(pid) || !IsMetrics(pid) || !usesAny(cfg.Service.Pipelines[pid].Receivers, receivers) {
				continue
			}
			p := cfg.Service.Pipelines[pid]
			prev, edgeNote := from, note
			for _, id := range p.Processors {
				name := processorName(cfg, c, coll, pid, id)
				g.AddNode(flowgraph.Node{ID: name, Group: coll, Label: strings.TrimPrefix(name, coll+"/"),
					Kind: flowgraph.Processor, Note: processorNote(collector.Type(id))})
				g.AddEdge(flowgraph.Edge{From: prev, To: name, Note: edgeNote})
				prev, edgeNote = name, ""
			}
			key := coll + "/" + pid
			for _, exp := range p.Exporters {
				name := exporterName(coll, exp)
				n := flowgraph.Node{ID: name, Group: coll, Label: exp, Kind: flowgraph.Exporter}
				if external[coll+"/exporters/"+exp] {
					n.Note = "external: data leaves the modeled system"
				}
				g.AddNode(n)
				g.AddEdge(flowgraph.Edge{From: prev, To: name, Note: edgeNote})
				if done[key] {
					continue
				}
				for i, j := range f.Joins {
					if j.From != coll+"/exporters/"+exp {
						continue
					}
					to, err := f.ParseEndpoint(j.To)
					if err != nil {
						continue
					}
					if to.Backend != "" {
						id := BackendNode(to.Backend)
						backends = append(backends, BackendFlowNode(to.Backend, f.Backends[to.Backend].Type))
						g.AddEdge(flowgraph.Edge{From: name, To: id, Note: v.BadJoins[i]})
						continue
					}
					walk(name, v.BadJoins[i], to.Collector, map[string]bool{to.ID: true}, depth+1)
				}
			}
			done[key] = true
		}
	}
	for _, name := range Names(f.Collectors) {
		c := f.Collectors[name]
		if c.Tap == "" {
			continue
		}
		receivers := map[string]bool{}
		for _, r := range configs[name].Service.Pipelines[c.Tap].Receivers {
			receivers[r] = true
		}
		walk(sdkenv.Name, "", name, receivers, 0)
	}
	for _, b := range backends {
		g.AddNode(b)
	}
	return g, nil
}

// BackendNode is a backend's node name in lineage.
func BackendNode(backend string) string { return mimir.Prefix + backend }

// BackendFlowNode is a backend's flow graph node. Its type is a note, unless
// the backend is named after it.
func BackendFlowNode(backend, typ string) flowgraph.Node {
	n := flowgraph.Node{ID: BackendNode(backend), Group: backend, Label: backend, Kind: flowgraph.Backend}
	if typ != backend {
		n.Note = typ
	}
	return n
}

func processorNote(typ string) string {
	switch {
	case typ == "batch", typ == "memory_limiter":
		return "passes data through"
	case typ == "k8sattributes", typ == "resourcedetection", realproc.Supported(typ):
		return ""
	default:
		return "not modeled: results after it are unverified"
	}
}

func usesAny(list []string, set map[string]bool) bool {
	for _, x := range list {
		if set[x] {
			return true
		}
	}
	return false
}
