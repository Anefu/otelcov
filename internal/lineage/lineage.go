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
type Log struct {
	events  []Event
	signals map[SignalID]Signal
	order   []SignalID
}

// NewLog returns an empty log.
func NewLog() *Log {
	return &Log{signals: map[SignalID]Signal{}}
}

// Record appends events, numbering them e1, e2, ... in the order received.
// It returns the assigned IDs.
func (l *Log) Record(events ...Event) []string {
	ids := make([]string, len(events))
	for i, e := range events {
		e.ID = "e" + strconv.Itoa(len(l.events)+1)
		l.events = append(l.events, e)
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
