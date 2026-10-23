// Package resourcedetection models the collector's resourcedetection
// processor with values recorded in the snapshot.
//
// The real processor asks the cloud, the host or the Kubernetes API, which a
// replay can't do. Instead, each processor's detected attributes are captured
// when the snapshot is recorded (decision 40), per Kubernetes node for
// daemonset collectors or under "*" for the rest, together with the
// processor's config at that time. If the config under test has different
// detector settings, the recorded values may not hold: every item is
// unverified until a new snapshot is recorded.
package resourcedetection

import (
	"context"
	"fmt"
	"reflect"
	"sort"

	corev1 "k8s.io/api/core/v1"

	"github.com/Anefu/otelcov/internal/graph"
	"github.com/Anefu/otelcov/internal/kube"
	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/unit"
)

// Values are recorded detections: Kubernetes node name (or "*") to attributes.
type Values map[string]map[string]string

// Recorded is one processor's detections, as stored in snapshot metadata.
type Recorded struct {
	// Config is the processor's config when the values were recorded.
	Config map[string]any `json:"config"`
	Values Values         `json:"values"`
}

// Node applies recorded detections.
type Node struct {
	name     string
	override bool
	rec      *Recorded
	// stale says why recorded values may not hold, or is "".
	stale string
	pods  *kube.Pods
}

// New builds the node. raw is the processor's config under test; rec is what
// the snapshot recorded for this processor (nil if nothing). snapshotPods map
// a signal to the node its collector instance ran on.
func New(name string, raw map[string]any, rec *Recorded, snapshotPods []corev1.Pod) (*Node, error) {
	override, err := overrideOf(raw)
	if err != nil {
		return nil, err
	}
	n := &Node{name: name, override: override, rec: rec, pods: kube.NewPods(snapshotPods)}
	if rec != nil && !reflect.DeepEqual(withoutOverride(raw), withoutOverride(rec.Config)) {
		n.stale = "the detector settings differ from the ones the snapshot was recorded with, so the recorded values may not hold; record a new snapshot to check"
	}
	return n, nil
}

func overrideOf(raw map[string]any) (bool, error) {
	v, ok := raw["override"]
	if !ok {
		return true, nil // the processor's default
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("override must be true or false")
	}
	return b, nil
}

// withoutOverride drops the one setting the node models itself.
func withoutOverride(raw map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range raw {
		if k != "override" {
			out[k] = v
		}
	}
	return out
}

func (n *Node) Name() string { return n.name }

func (n *Node) ProcessMetrics(_ context.Context, in []unit.Metric) (graph.Output, error) {
	out := graph.Output{Items: in}
	for _, it := range in {
		attrs := unit.Resource(it.Data).Attributes()
		emit := func(e lineage.Event) {
			e.Node, e.Signal = n.name, it.ID
			out.Events = append(out.Events, e)
		}
		node := ""
		if ip, ok := attrs.Get("k8s.pod.ip"); ok {
			if p, ok := n.pods.ByIP(ip.AsString()); ok {
				node = p.Spec.NodeName
			}
		}
		var detected map[string]string
		ok := false
		if n.rec != nil {
			if detected, ok = n.rec.Values[node]; !ok {
				detected, ok = n.rec.Values["*"]
			}
		}
		if !ok {
			emit(lineage.Event{Action: lineage.Unverified,
				Reason: fmt.Sprintf("the snapshot has no recorded resource detection for %s on node %q", n.name, node)})
			continue
		}
		if n.stale != "" {
			emit(lineage.Event{Action: lineage.Unverified, Reason: n.stale})
		}
		keys := make([]string, 0, len(detected))
		for k := range detected {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := detected[k]
			if cur, exists := attrs.Get(k); exists && !n.override {
				emit(lineage.Event{Attribute: k, Action: lineage.Kept, Value: cur.AsString(), Reason: "already on the resource, and override is off"})
				continue
			}
			attrs.PutStr(k, v)
			emit(lineage.Event{Attribute: k, Action: lineage.Set, Value: v, Reason: "detected by resourcedetection (recorded in the snapshot)"})
		}
	}
	return out, nil
}
