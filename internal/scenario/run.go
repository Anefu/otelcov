package scenario

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/Anefu/otelcov/internal/alerting"
	"github.com/Anefu/otelcov/internal/graph"
	"github.com/Anefu/otelcov/internal/kube"
	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/nodes/alerts"
	"github.com/Anefu/otelcov/internal/nodes/mimir"
	"github.com/Anefu/otelcov/internal/nodes/route"
	"github.com/Anefu/otelcov/internal/nodes/rules"
	"github.com/Anefu/otelcov/internal/nodes/sdkenv"
	"github.com/Anefu/otelcov/internal/nodes/source"
	"github.com/Anefu/otelcov/internal/report"
	"github.com/Anefu/otelcov/internal/requirements"
	"github.com/Anefu/otelcov/internal/rulefile"
	"github.com/Anefu/otelcov/internal/topology"
	"github.com/Anefu/otelcov/internal/unit"
)

// Result is the outcome of a run.
type Result struct {
	Log *lineage.Log
	// Series per backend, after recording rules.
	Series map[string][]unit.Metric
	// Alerts the alerting rules could produce.
	Alerts []alerting.Alert
	// Problems found while validating the topology (none fatal).
	Problems []topology.Problem

	pods []corev1.Pod
}

// Run executes the scenario: replay, sdk-env, the collectors as the topology
// connects them, each backend, recording and alerting rules, then
// Alertmanager routing.
func Run(ctx context.Context, s *Scenario) (*Result, error) {
	snap, err := LoadSnapshot(s.Data.Snapshot)
	if err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	snapPods, err := kube.LoadPods(snap.Pods)
	if err != nil {
		return nil, fmt.Errorf("snapshot pods: %w", err)
	}
	pods, err := kube.LoadPods(s.Config.Pods)
	if err != nil {
		return nil, fmt.Errorf("pods: %w", err)
	}
	topo, err := topology.Load(s.Config.Topology)
	if err != nil {
		return nil, err
	}
	val := topology.Validate(topo)
	if fatal := val.Fatal(); len(fatal) > 0 {
		var msgs []string
		for _, p := range fatal {
			msgs = append(msgs, p.Where+": "+p.Message)
		}
		return nil, fmt.Errorf("topology doesn't match the configs:\n  %s", strings.Join(msgs, "\n  "))
	}
	at, err := time.Parse(time.RFC3339, s.EvalTime)
	if err != nil {
		return nil, fmt.Errorf("eval_time: %w", err)
	}

	log := lineage.NewLog()
	items, err := graph.ReadSource(ctx, source.New(s.Data.Recording, s.Rel(s.Data.Recording)), log)
	if err != nil {
		return nil, err
	}
	if items, err = graph.Process(ctx, []graph.MetricsNode{sdkenv.New(snapPods, pods)}, items, log); err != nil {
		return nil, err
	}

	runner, err := topology.NewRunner(topo, val, topology.Inputs{Pods: pods, SnapshotPods: snapPods, ResourceDetection: snap.ResourceDetection}, log)
	if err != nil {
		return nil, err
	}
	if err := runner.Run(ctx, items); err != nil {
		return nil, err
	}

	res := &Result{Log: log, Series: map[string][]unit.Metric{}, Problems: val.Problems, pods: pods}
	for _, name := range topology.Names(topo.Backends) {
		limits, err := mimir.Load(topo.Backends[name].Config)
		if err != nil {
			return nil, fmt.Errorf("backend %s: %w", name, err)
		}
		series, err := graph.Process(ctx, []graph.MetricsNode{mimir.New(name, limits)}, runner.Delivered[name], log)
		if err != nil {
			return nil, err
		}
		res.Series[name] = series
	}

	// Consumers: rules read a backend; Alertmanager reads rules.
	ruleIDs, alertIDs := lineage.NewIDs("r"), lineage.NewIDs("a")
	alertsBy := map[string][]alerting.Alert{}
	for _, name := range topology.Names(topo.Consumers) {
		c := topo.Consumers[name]
		if c.Type != "prometheus-rules" {
			continue
		}
		set, err := rulefile.Load(c.Paths)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		rn, err := rules.New(set.Recording, at, ruleIDs)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		series, err := graph.Process(ctx, []graph.MetricsNode{rn}, res.Series[c.Reads], log)
		if err != nil {
			return nil, err
		}
		res.Series[c.Reads] = series
		ao, err := alerts.New(set.Alerting, at, alertIDs).Process(ctx, series)
		if err != nil {
			return nil, err
		}
		ids := make([]lineage.SignalID, len(ao.Alerts))
		for i, a := range ao.Alerts {
			ids[i] = a.ID
		}
		if err := graph.Record(log, name, ao.Events, ao.Signals, ids); err != nil {
			return nil, err
		}
		alertsBy[name] = ao.Alerts
		res.Alerts = append(res.Alerts, ao.Alerts...)
	}
	for _, name := range topology.Names(topo.Consumers) {
		c := topo.Consumers[name]
		if c.Type != "alertmanager" {
			continue
		}
		router, err := route.Load(c.Config)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if err := graph.Record(log, route.Name, router.Process(alertsBy[c.Reads]), nil, nil); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// Report runs the scenario and answers its requirements.
func Report(ctx context.Context, s *Scenario) (*report.Report, error) {
	res, err := Run(ctx, s)
	if err != nil {
		return nil, err
	}
	reqs, err := requirements.Load(s.Requirements)
	if err != nil {
		return nil, fmt.Errorf("requirements: %w", err)
	}
	rep := report.Build(s.Name, s.EvalTime, res.Log, reqs, res.pods)
	rep.Topology = res.Problems
	return rep, nil
}
