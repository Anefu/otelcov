// Package pluginwire converts otelcov's units and lineage to and from the
// stage plugin protocol (pkg/plugin/pluginv1). Both sides use it: the core
// and the plugins built in this repository.
//
// Units cross as OTLP protobuf. Lineage values, details and resources cross
// as JSON, decoded with numbers kept as written (json.Number), so a report
// says exactly what it would say without the process boundary.
package pluginwire

import (
	"bytes"
	"encoding/json"
	"fmt"

	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/unit"
	"github.com/Anefu/otelcov/pkg/plugin/pluginv1"
)

var (
	marshaler   pmetric.ProtoMarshaler
	unmarshaler pmetric.ProtoUnmarshaler
)

// Units converts units to the protocol.
func Units(in []unit.Metric) ([]*pluginv1.Unit, error) {
	out := make([]*pluginv1.Unit, len(in))
	for i, u := range in {
		b, err := marshaler.MarshalMetrics(u.Data)
		if err != nil {
			return nil, fmt.Errorf("unit %s: %w", u.ID, err)
		}
		out[i] = &pluginv1.Unit{Id: string(u.ID), From: ids(u.From), Otlp: b}
	}
	return out, nil
}

// FromUnits converts units from the protocol.
func FromUnits(in []*pluginv1.Unit) ([]unit.Metric, error) {
	out := make([]unit.Metric, len(in))
	for i, u := range in {
		md, err := unmarshaler.UnmarshalMetrics(u.GetOtlp())
		if err != nil {
			return nil, fmt.Errorf("unit %s: %w", u.GetId(), err)
		}
		out[i] = unit.Metric{ID: lineage.SignalID(u.GetId()), From: signalIDs(u.GetFrom()), Data: md}
	}
	return out, nil
}

// Size is roughly how many bytes a unit takes in a message.
func Size(u *pluginv1.Unit) int { return len(u.GetOtlp()) + len(u.GetId()) + 16 }

// Events converts lineage events to the protocol. Event IDs aren't sent: the
// core assigns them when it logs the events.
func Events(in []lineage.Event) ([]*pluginv1.Event, error) {
	out := make([]*pluginv1.Event, len(in))
	for i, e := range in {
		value, err := encode(e.Value, e.Value == nil)
		if err != nil {
			return nil, fmt.Errorf("event %s/%s: value: %w", e.Node, e.Signal, err)
		}
		detail, err := encode(e.Detail, len(e.Detail) == 0)
		if err != nil {
			return nil, fmt.Errorf("event %s/%s: detail: %w", e.Node, e.Signal, err)
		}
		out[i] = &pluginv1.Event{
			Node: e.Node, Signal: string(e.Signal), Attribute: e.Attribute, Action: string(e.Action),
			Value: value, Simulated: e.Simulated, Reason: e.Reason, From: ids(e.From), Detail: detail,
		}
	}
	return out, nil
}

// FromEvents converts events from the protocol.
func FromEvents(in []*pluginv1.Event) ([]lineage.Event, error) {
	out := make([]lineage.Event, len(in))
	for i, e := range in {
		ev := lineage.Event{
			Node: e.GetNode(), Signal: lineage.SignalID(e.GetSignal()), Attribute: e.GetAttribute(),
			Action: lineage.Action(e.GetAction()), Simulated: e.GetSimulated(), Reason: e.GetReason(), From: signalIDs(e.GetFrom()),
		}
		if err := decode(e.GetValue(), &ev.Value); err != nil {
			return nil, fmt.Errorf("event %s/%s: value: %w", ev.Node, ev.Signal, err)
		}
		if err := decode(e.GetDetail(), &ev.Detail); err != nil {
			return nil, fmt.Errorf("event %s/%s: detail: %w", ev.Node, ev.Signal, err)
		}
		out[i] = ev
	}
	return out, nil
}

// Signals converts signals to the protocol.
func Signals(in []lineage.Signal) ([]*pluginv1.Signal, error) {
	out := make([]*pluginv1.Signal, len(in))
	for i, s := range in {
		value, err := encode(s.Value, s.Value == nil)
		if err != nil {
			return nil, fmt.Errorf("signal %s: value: %w", s.ID, err)
		}
		res, err := encode(s.Resource, len(s.Resource) == 0)
		if err != nil {
			return nil, fmt.Errorf("signal %s: resource: %w", s.ID, err)
		}
		out[i] = &pluginv1.Signal{
			Id: string(s.ID), Node: s.Node, Kind: s.Kind, Name: s.Name, Origin: s.Origin,
			From: ids(s.From), Resource: res, Labels: s.Labels, Value: value,
		}
	}
	return out, nil
}

// FromSignals converts signals from the protocol.
func FromSignals(in []*pluginv1.Signal) ([]lineage.Signal, error) {
	out := make([]lineage.Signal, len(in))
	for i, s := range in {
		sig := lineage.Signal{
			ID: lineage.SignalID(s.GetId()), Node: s.GetNode(), Kind: s.GetKind(), Name: s.GetName(),
			Origin: s.GetOrigin(), From: signalIDs(s.GetFrom()), Labels: s.GetLabels(),
		}
		if len(sig.Labels) == 0 {
			sig.Labels = nil
		}
		if err := decode(s.GetValue(), &sig.Value); err != nil {
			return nil, fmt.Errorf("signal %s: value: %w", sig.ID, err)
		}
		if err := decode(s.GetResource(), &sig.Resource); err != nil {
			return nil, fmt.Errorf("signal %s: resource: %w", sig.ID, err)
		}
		out[i] = sig
	}
	return out, nil
}

// encode writes v as JSON, or nothing when empty.
func encode(v any, empty bool) ([]byte, error) {
	if empty {
		return nil, nil
	}
	return json.Marshal(v)
}

// decode reads JSON into v, keeping numbers as written; nothing leaves v
// alone.
func decode[T any](b []byte, v *T) error {
	if len(b) == 0 {
		return nil
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	return d.Decode(v)
}

func ids(in []lineage.SignalID) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	for i, id := range in {
		out[i] = string(id)
	}
	return out
}

func signalIDs(in []string) []lineage.SignalID {
	if len(in) == 0 {
		return nil
	}
	out := make([]lineage.SignalID, len(in))
	for i, id := range in {
		out[i] = lineage.SignalID(id)
	}
	return out
}
