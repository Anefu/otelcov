// Package alerting defines otelcov's alert type, the one exception to pdata
// throughout: alerts have state over time and a fixed schema, and Alertmanager
// works on plain label sets.
package alerting

import (
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"

	"github.com/Anefu/otelcov/internal/lineage"
)

// Alert is one alert instance an alerting rule could produce.
type Alert struct {
	ID   lineage.SignalID
	From []lineage.SignalID
	// Labels include alertname, the series labels and the rule's labels.
	Labels labels.Labels
	// Annotations as written in the rule; templates aren't expanded.
	Annotations map[string]string
	// For is the rule's pending period. At a single instant it's treated as
	// met.
	For time.Duration
}

// Name returns the alertname.
func (a Alert) Name() string { return a.Labels.Get(labels.AlertName) }

// LabelSet converts the labels for Alertmanager.
func (a Alert) LabelSet() model.LabelSet {
	ls := model.LabelSet{}
	a.Labels.Range(func(l labels.Label) {
		ls[model.LabelName(l.Name)] = model.LabelValue(l.Value)
	})
	return ls
}

// Output is what an alert stage returns: alerts plus lineage.
type Output struct {
	Alerts  []Alert
	Events  []lineage.Event
	Signals []lineage.Signal
}
