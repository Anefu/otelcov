// Package lineage records where signals come from and what happens to them
// on their way through the graph.
//
// Nodes never assign event IDs themselves. Like a Writer monad, each node
// returns its outputs plus the events it wants to log, and the runner appends
// them to a Log, which numbers them.
package lineage

import (
	"fmt"
	"strconv"
	"strings"
)

// SignalID identifies one signal (an item, a series, an alert) in a run.
type SignalID string

// Action says what a node did to a signal or one of its attributes.
type Action string

const (
	// Attributed: sdk-env worked out which pod and container sent the signal.
	Attributed Action = "attributed"
	// Associated: k8sattributes linked the signal to a pod.
	Associated Action = "associated"
	// Set: the attribute was added or changed.
	Set Action = "set"
	// Kept: the attribute was already present and left alone.
	Kept Action = "kept"
	// Removed: the attribute was deleted.
	Removed Action = "removed"
	// Skipped: the node could have set or used the attribute but didn't.
	// Recording non-events is what lets a diagnosis reach a root cause.
	Skipped Action = "skipped"
	// Derived: the node produced a new signal from its inputs.
	Derived Action = "derived"
	// Dropped: the input contributed to no output.
	Dropped Action = "dropped"
	// Routed: an alert reached a receiver.
	Routed Action = "routed"
	// Exported: the signal left the modeled system through an external
	// exporter.
	Exported Action = "exported"
	// Unverified: the node can't tell what really happens to this signal.
	// Everything downstream inherits the uncertainty.
	Unverified Action = "unverified"
)

// Event is one structured lineage record.
type Event struct {
	ID        string         `json:"id"`
	Node      string         `json:"node"`
	Signal    SignalID       `json:"signal"`
	Attribute string         `json:"attribute,omitempty"`
	Action    Action         `json:"action"`
	Value     any            `json:"value,omitempty"`
	Simulated bool           `json:"simulated,omitempty"`
	Reason    string         `json:"reason,omitempty"`
	From      []SignalID     `json:"from,omitempty"`
	Detail    map[string]any `json:"detail,omitempty"`
}

// Signal describes a signal when it is first created.
type Signal struct {
	ID       SignalID          `json:"-"`
	Node     string            `json:"node"`
	Kind     string            `json:"kind"`
	Name     string            `json:"name,omitempty"`
	Origin   string            `json:"origin,omitempty"`
	From     []SignalID        `json:"from,omitempty"`
	Resource map[string]any    `json:"resource,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
	Value    any               `json:"value,omitempty"`
}

// Log collects the events and signals of one run, in order.
//
// Every event, new signal and stage crossing gets the next number in one
// sequence, so a report can tell what happened to a signal before another
// signal was made from it (see Born and Branched).
type Log struct {
	events  []Event
	signals map[SignalID]Signal
	order   []SignalID

	seq      int
	eventSeq []int
	born     map[SignalID]int
	stages   map[string]Stage
	crossed  map[SignalID][]crossing
}

type crossing struct {
	node string
	seq  int
}

// NewLog returns an empty log.
func NewLog() *Log {
	return &Log{signals: map[SignalID]Signal{}, born: map[SignalID]int{},
		stages: map[string]Stage{}, crossed: map[SignalID][]crossing{}}
}

// Record appends events, numbering them e1, e2, ... in the order received.
// It returns the assigned IDs.
func (l *Log) Record(events ...Event) []string {
	ids := make([]string, len(events))
	for i, e := range events {
		e.ID = "e" + strconv.Itoa(len(l.events)+1)
		l.events = append(l.events, e)
		l.seq++
		l.eventSeq = append(l.eventSeq, l.seq)
		ids[i] = e.ID
	}
	return ids
}

// AddSignal registers a newly created signal. IDs must be unique.
func (l *Log) AddSignal(s Signal) error {
	if s.ID == "" {
		return fmt.Errorf("signal from %s has no ID", s.Node)
	}
	if _, dup := l.signals[s.ID]; dup {
		return fmt.Errorf("signal %s registered twice (second time by %s)", s.ID, s.Node)
	}
	l.signals[s.ID] = s
	l.order = append(l.order, s.ID)
	l.seq++
	l.born[s.ID] = l.seq
	return nil
}

// HasSignal reports whether a signal was registered.
func (l *Log) HasSignal(id SignalID) bool {
	_, ok := l.signals[id]
	return ok
}

// Events returns all events in order.
func (l *Log) Events() []Event {
	return append([]Event(nil), l.events...)
}

// Signals returns all signals in registration order.
func (l *Log) Signals() []Signal {
	out := make([]Signal, len(l.order))
	for i, id := range l.order {
		out[i] = l.signals[id]
	}
	return out
}

// Fidelity is how closely a stage's model follows the real component
// (decision 67).
type Fidelity string

const (
	// Real: the component's own code.
	Real Fidelity = "real"
	// Reimplementation: otelcov's own model of the component. It counts as
	// real when its conformance tests pass at the deployed version.
	Reimplementation Fidelity = "reimplementation"
	// Approximation: a model known to leave out some of what the component
	// does, such as batch passing data through.
	Approximation Fidelity = "approximation"
	// Unmodelled: data passes through, unverified.
	Unmodelled Fidelity = "unmodelled"
)

// Stage is a stage's fidelity.
type Stage struct {
	Fidelity Fidelity `json:"fidelity"`
	// Conformance: a reimplementation's conformance tests pass at the
	// deployed version.
	Conformance bool   `json:"conformance,omitempty"`
	Note        string `json:"note,omitempty"`
}

// Modelled reports whether answers that depend on the stage rest on
// otelcov's model rather than on the component's own behavior: an
// approximation, or a reimplementation without conformance. Real stages,
// tested reimplementations and unmodelled stages (which make answers
// unverified instead) aren't.
func (s Stage) Modelled() bool {
	return s.Fidelity == Approximation || s.Fidelity == Reimplementation && !s.Conformance
}

// SetStage records a stage's fidelity. A stage that isn't set counts as
// real. Set it before the stage processes anything, so its crossings are
// kept.
func (l *Log) SetStage(node string, s Stage) { l.stages[node] = s }

// Stage returns a stage's fidelity, real when it wasn't set.
func (l *Log) Stage(node string) Stage {
	if s, ok := l.stages[node]; ok {
		return s
	}
	return Stage{Fidelity: Real}
}

// Cross records that signals entered a stage. Only modelled stages are kept:
// they leave no events when they change nothing (batch), yet answers depend
// on them.
func (l *Log) Cross(node string, ids []SignalID) {
	if !l.Stage(node).Modelled() {
		return
	}
	l.seq++
	for _, id := range ids {
		l.crossed[id] = append(l.crossed[id], crossing{node: node, seq: l.seq})
	}
}

// Crossed returns the modelled stages a signal entered before before (a
// point from Born; 0 for all), in order.
func (l *Log) Crossed(id SignalID, before int) []string {
	var out []string
	for _, c := range l.crossed[id] {
		if before == 0 || c.seq < before {
			out = append(out, c.node)
		}
	}
	return out
}

// Mark is the current point in the log's sequence.
func (l *Log) Mark() int { return l.seq }

// Born is the point at which a signal was registered, or at which Branched
// says it branched off its parents: what happened to a parent after it
// didn't happen to this signal's data.
func (l *Log) Born(id SignalID) int { return l.born[id] }

// Branched says that copies branched off their parents at a point from Mark,
// though they were made later: a fan-out runs its first branch to the end
// before copying the data for the others.
func (l *Log) Branched(ids []SignalID, at int) {
	for _, id := range ids {
		l.born[id] = at + 1
	}
}

// EventSeq is the point at which an event (by ID) was recorded, 0 for an
// unknown ID.
func (l *Log) EventSeq(id string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "e"))
	if err != nil || n < 1 || n > len(l.eventSeq) {
		return 0
	}
	return l.eventSeq[n-1]
}

// IDs hands out new signal IDs with a prefix: c1, c2, ...
type IDs struct {
	prefix string
	n      int
}

// NewIDs returns an allocator for IDs with the given prefix.
func NewIDs(prefix string) *IDs { return &IDs{prefix: prefix} }

// Next returns the next ID.
func (i *IDs) Next() SignalID {
	i.n++
	return SignalID(i.prefix + strconv.Itoa(i.n))
}

// NextNumber is the number the next ID gets.
func (i *IDs) NextNumber() int64 { return int64(i.n) + 1 }

// SetNextNumber makes the next ID the given number: a plugin numbers new
// items from the number the core hands it, so IDs stay one sequence.
func (i *IDs) SetNextNumber(n int64) { i.n = int(n) - 1 }
