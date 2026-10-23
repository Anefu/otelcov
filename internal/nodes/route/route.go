// Package route routes alerts with Alertmanager's own routing code.
package route

import (
	"fmt"
	"reflect"
	"strings"

	amconfig "github.com/prometheus/alertmanager/config"
	"github.com/prometheus/alertmanager/dispatch"
	amlabels "github.com/prometheus/alertmanager/pkg/labels"
	"github.com/prometheus/common/model"

	"github.com/Anefu/otelcov/internal/alerting"
	"github.com/Anefu/otelcov/internal/lineage"
)

// Name is the node name used in lineage.
const Name = "alertmanager/route"

// Node routes alerts to receivers.
type Node struct {
	cfg  *amconfig.Config
	root *dispatch.Route
}

// Load reads an Alertmanager config with Alertmanager's own loader.
func Load(path string) (*Node, error) {
	cfg, err := amconfig.LoadFile(path)
	if err != nil {
		return nil, err
	}
	return &Node{cfg: cfg, root: dispatch.NewRoute(cfg.Route, nil)}, nil
}

// Process routes each alert and returns the lineage.
func (n *Node) Process(alerts []alerting.Alert) []lineage.Event {
	var events []lineage.Event
	for _, a := range alerts {
		emit := func(e lineage.Event) {
			e.Node, e.Signal = Name, a.ID
			events = append(events, e)
		}
		for _, r := range n.root.Match(a.LabelSet()) {
			path := n.path(r)
			receiver := r.RouteOpts.Receiver
			if !n.hasIntegrations(receiver) {
				emit(lineage.Event{Action: lineage.Dropped, Reason: fmt.Sprintf("receiver %s has no integrations, so notifications go nowhere", receiver),
					Detail: map[string]any{"receiver": receiver, "route": path}})
				continue
			}
			emit(lineage.Event{Action: lineage.Routed, Detail: map[string]any{"receiver": receiver, "route": path}})
			if ivs := append(append([]string(nil), r.RouteOpts.MuteTimeIntervals...), r.RouteOpts.ActiveTimeIntervals...); len(ivs) > 0 {
				emit(lineage.Event{Action: lineage.Unverified,
					Reason: "the route only notifies outside mute or inside active time intervals (" + strings.Join(ivs, ", ") + ")"})
			}
		}
		for _, ir := range n.cfg.InhibitRules {
			if inhibitTargets(ir, a.LabelSet()) {
				emit(lineage.Event{Action: lineage.Unverified,
					Reason: "an inhibit rule can suppress it while another alert matching " + amlabels.Matchers(ir.SourceMatchers).String() + " fires; inhibition isn't modeled yet"})
				break
			}
		}
	}
	return events
}

// path describes the routes from the root to r.
func (n *Node) path(target *dispatch.Route) []any {
	var walk func(r *dispatch.Route, acc []*dispatch.Route) []*dispatch.Route
	walk = func(r *dispatch.Route, acc []*dispatch.Route) []*dispatch.Route {
		acc = append(acc, r)
		if r == target {
			return acc
		}
		for _, c := range r.Routes {
			if found := walk(c, acc); found != nil {
				return found
			}
		}
		return nil
	}
	var out []any
	for _, r := range walk(n.root, nil) {
		ms := []any{}
		for _, m := range r.Matchers {
			ms = append(ms, m.String())
		}
		out = append(out, map[string]any{"receiver": r.RouteOpts.Receiver, "matchers": ms})
	}
	return out
}

// hasIntegrations reports whether a receiver sends anything anywhere.
func (n *Node) hasIntegrations(name string) bool {
	for _, rc := range n.cfg.Receivers {
		if rc.Name != name {
			continue
		}
		v := reflect.ValueOf(rc)
		for i := 0; i < v.NumField(); i++ {
			f := v.Field(i)
			if strings.HasSuffix(v.Type().Field(i).Name, "Configs") && f.Kind() == reflect.Slice && f.Len() > 0 {
				return true
			}
		}
		return false
	}
	return false
}

// inhibitTargets reports whether an inhibit rule's target side matches lset,
// including the deprecated target_match and target_match_re forms.
func inhibitTargets(ir amconfig.InhibitRule, lset model.LabelSet) bool {
	if !amlabels.Matchers(ir.TargetMatchers).Matches(lset) {
		return false
	}
	for k, v := range ir.TargetMatch {
		if string(lset[model.LabelName(k)]) != v {
			return false
		}
	}
	for k, re := range ir.TargetMatchRE {
		if !re.MatchString(string(lset[model.LabelName(k)])) {
			return false
		}
	}
	return true
}

// Receivers returns the receivers' names, in config order.
func (n *Node) Receivers() []string {
	var out []string
	for _, rc := range n.cfg.Receivers {
		out = append(out, rc.Name)
	}
	return out
}
