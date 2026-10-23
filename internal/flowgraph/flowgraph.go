// Package flowgraph describes how a scenario's components connect: sources,
// each collector's processors and exporters, backends, rules, alerts, routing
// and receivers. Node IDs are the node names lineage events use, so a report's
// events can be placed on the graph.
package flowgraph

// Node kinds.
const (
	Source    = "source"
	Processor = "processor"
	Exporter  = "exporter"
	Backend   = "backend"
	Rule      = "rule"
	Alert     = "alert"
	Route     = "route"
	Receiver  = "receiver"
)

// Graph is a scenario's flow, nodes in flow order.
type Graph struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// Node is one component.
type Node struct {
	// ID is the lineage node name, e.g. "gateway/transform/team".
	ID string `json:"id"`
	// Group is the part of the stack it belongs to: "source", a collector
	// name, a backend name, "rules", "alerting" or "receivers".
	Group string `json:"group"`
	Label string `json:"label"`
	Kind  string `json:"kind"`
	// Note says something the events won't, e.g. "passes data through".
	Note string `json:"note,omitempty"`
}

// Edge says data flows from one node to another. Note marks an edge that
// can't be trusted, e.g. a join validation flagged.
type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Note string `json:"note,omitempty"`
}

// AddNode adds a node once.
func (g *Graph) AddNode(n Node) {
	for _, have := range g.Nodes {
		if have.ID == n.ID {
			return
		}
	}
	g.Nodes = append(g.Nodes, n)
}

// AddEdge adds an edge once.
func (g *Graph) AddEdge(e Edge) {
	for _, have := range g.Edges {
		if have.From == e.From && have.To == e.To {
			return
		}
	}
	g.Edges = append(g.Edges, e)
}

// Has reports whether a node exists.
func (g *Graph) Has(id string) bool {
	for _, n := range g.Nodes {
		if n.ID == id {
			return true
		}
	}
	return false
}
