// Command snapshot turns what capture.sh copied off the kind cluster into a
// snapshot otelcov can replay.
//
// It reads, in -dir:
//
//	tap/<node>.jsonl       each agent's raw input (metrics/tap)
//	detected/<node>.jsonl  the same input after resourcedetection/capture (metrics/detected)
//	pods.yaml              the pods that ran
//
// and writes, in -dir:
//
//	recording.otlp.jsonl   every agent's tap, node by node
//	snapshot.meta.yaml     recorded_at, pods, and what resourcedetection detected per node
//	scenarios/*.yaml       the -templates scenarios, with eval_time set to the last recorded timestamp
//
// Detected values are the attributes metrics/detected has and metrics/tap
// doesn't (or has with another value), matched by the sender's k8s.pod.ip.
// The capture processor runs with override on, so every detected value shows;
// it must otherwise match the agent's resourcedetection processor, which is
// what the snapshot records as the config.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"sigs.k8s.io/yaml"

	"github.com/Anefu/otelcov/internal/collector"
	"github.com/Anefu/otelcov/internal/nodes/resourcedetection"
	"github.com/Anefu/otelcov/internal/scenario"
)

func main() {
	var o options
	flag.StringVar(&o.dir, "dir", "captured", "capture directory")
	flag.StringVar(&o.agentConfig, "agent-config", "", "the deployed agent collector config")
	flag.StringVar(&o.collector, "collector", "agent", "the agent's name in the otelcov topology")
	flag.StringVar(&o.processor, "processor", "resourcedetection", "the agent's resourcedetection processor")
	flag.StringVar(&o.capture, "capture-processor", "resourcedetection/capture", "the processor metrics/detected runs")
	flag.StringVar(&o.templates, "templates", "", "directory of scenario templates (eval_time: ${EVAL_TIME})")
	flag.Parse()
	if o.agentConfig == "" {
		fmt.Fprintln(os.Stderr, "usage: snapshot -dir captured -agent-config agent.yaml [-templates dir]")
		os.Exit(2)
	}
	if err := run(o, time.Now().UTC()); err != nil {
		fmt.Fprintln(os.Stderr, "snapshot:", err)
		os.Exit(1)
	}
}

type options struct {
	dir, agentConfig, collector, processor, capture, templates string
}

func run(o options, now time.Time) error {
	detectCfg, err := detectionConfig(o)
	if err != nil {
		return err
	}
	nodes, err := nodeNames(filepath.Join(o.dir, "tap"))
	if err != nil {
		return err
	}
	var (
		recording bytes.Buffer
		last      time.Time
		values    = resourcedetection.Values{}
	)
	for _, node := range nodes {
		raw, err := readRequests(filepath.Join(o.dir, "tap", node+".jsonl"))
		if err != nil {
			return err
		}
		detected, err := readRequests(filepath.Join(o.dir, "detected", node+".jsonl"))
		if err != nil {
			return err
		}
		for _, r := range raw {
			recording.Write(r.line)
			recording.WriteByte('\n')
			if t := lastTimestamp(r.md); t.After(last) {
				last = t
			}
		}
		v, err := detectedValues(raw, detected)
		if err != nil {
			return fmt.Errorf("node %s: %w", node, err)
		}
		if len(v) > 0 {
			values[node] = v
		}
		fmt.Printf("%s: %d export requests, detected %v\n", node, len(raw), keys(v))
	}
	if last.IsZero() {
		return errors.New("the recording has no data points; wait for the apps to export, then capture again")
	}
	if err := os.WriteFile(filepath.Join(o.dir, "recording.otlp.jsonl"), recording.Bytes(), 0o644); err != nil {
		return err
	}

	snap := scenario.Snapshot{
		RecordedAt: now.Format(time.RFC3339),
		Pods:       "pods.yaml",
		ResourceDetection: map[string]map[string]resourcedetection.Recorded{
			o.collector: {o.processor: {Config: detectCfg, Values: values}},
		},
	}
	b, err := yaml.Marshal(snap)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(o.dir, "snapshot.meta.yaml"), b, 0o644); err != nil {
		return err
	}

	// The lookback window ends at eval_time inclusive; round up so the last
	// sample is inside it.
	evalTime := last.Truncate(time.Second)
	if evalTime.Before(last) {
		evalTime = evalTime.Add(time.Second)
	}
	fmt.Printf("eval_time: %s\n", evalTime.Format(time.RFC3339))
	if o.templates == "" {
		return nil
	}
	return writeScenarios(o, evalTime)
}

// detectionConfig returns the agent's resourcedetection config, after
// checking that the capture processor detects the same way.
func detectionConfig(o options) (map[string]any, error) {
	cfg, err := collector.Load(o.agentConfig)
	if err != nil {
		return nil, err
	}
	rd, ok := cfg.Processors[o.processor]
	if !ok {
		return nil, fmt.Errorf("%s has no processor %s", o.agentConfig, o.processor)
	}
	capture, ok := cfg.Processors[o.capture]
	if !ok {
		return nil, fmt.Errorf("%s has no processor %s", o.agentConfig, o.capture)
	}
	if !reflect.DeepEqual(without(rd, "override"), without(capture, "override")) {
		return nil, fmt.Errorf("%s and %s must have the same settings apart from override, or the recorded values aren't what %s detects", o.processor, o.capture, o.processor)
	}
	if rd == nil {
		rd = map[string]any{}
	}
	return rd, nil
}

func without(m map[string]any, key string) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		if k != key {
			out[k] = v
		}
	}
	return out
}

func nodeNames(dir string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no recordings in %s", dir)
	}
	var nodes []string
	for _, f := range files {
		nodes = append(nodes, strings.TrimSuffix(filepath.Base(f), ".jsonl"))
	}
	sort.Strings(nodes)
	return nodes, nil
}

type request struct {
	line []byte
	md   pmetric.Metrics
}

// readRequests reads one OTLP/JSON export request per line, as the file
// exporter writes them. The last line may be cut off if the file was being
// written during the copy; it's skipped. A bad line elsewhere is an error.
func readRequests(path string) ([]request, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var (
		out     []request
		pending error
		u       pmetric.JSONUnmarshaler
	)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for n := 1; sc.Scan(); n++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if pending != nil {
			return nil, pending
		}
		md, err := u.UnmarshalMetrics(line)
		if err != nil {
			pending = fmt.Errorf("%s:%d: %w", path, n, err)
			continue
		}
		out = append(out, request{line: append([]byte(nil), line...), md: md})
	}
	if pending != nil {
		fmt.Fprintf(os.Stderr, "skipping a partly written last line: %v\n", pending)
	}
	return out, sc.Err()
}

// detectedValues compares each resource after resourcedetection/capture with
// the same sender's raw resource.
func detectedValues(raw, detected []request) (map[string]string, error) {
	rawByIP := map[string]map[string]string{}
	for _, r := range raw {
		for _, res := range resources(r.md) {
			if ip := res["k8s.pod.ip"]; ip != "" {
				if _, ok := rawByIP[ip]; !ok {
					rawByIP[ip] = res
				}
			}
		}
	}
	out := map[string]string{}
	for _, d := range detected {
		for _, res := range resources(d.md) {
			before, ok := rawByIP[res["k8s.pod.ip"]]
			if !ok {
				continue // not in the tap copy (yet)
			}
			for k, v := range res {
				if before[k] == v {
					continue
				}
				if prev, seen := out[k]; seen && prev != v {
					return nil, fmt.Errorf("%s was detected as both %q and %q", k, prev, v)
				}
				out[k] = v
			}
		}
	}
	return out, nil
}

func resources(md pmetric.Metrics) []map[string]string {
	var out []map[string]string
	for i := 0; i < md.ResourceMetrics().Len(); i++ {
		attrs := map[string]string{}
		md.ResourceMetrics().At(i).Resource().Attributes().Range(func(k string, v pcommon.Value) bool {
			attrs[k] = v.AsString()
			return true
		})
		out = append(out, attrs)
	}
	return out
}

func lastTimestamp(md pmetric.Metrics) time.Time {
	var last pcommon.Timestamp
	note := func(ts pcommon.Timestamp) {
		if ts > last {
			last = ts
		}
	}
	for i := 0; i < md.ResourceMetrics().Len(); i++ {
		sms := md.ResourceMetrics().At(i).ScopeMetrics()
		for j := 0; j < sms.Len(); j++ {
			ms := sms.At(j).Metrics()
			for k := 0; k < ms.Len(); k++ {
				m := ms.At(k)
				switch m.Type() {
				case pmetric.MetricTypeGauge:
					for p := 0; p < m.Gauge().DataPoints().Len(); p++ {
						note(m.Gauge().DataPoints().At(p).Timestamp())
					}
				case pmetric.MetricTypeSum:
					for p := 0; p < m.Sum().DataPoints().Len(); p++ {
						note(m.Sum().DataPoints().At(p).Timestamp())
					}
				case pmetric.MetricTypeHistogram:
					for p := 0; p < m.Histogram().DataPoints().Len(); p++ {
						note(m.Histogram().DataPoints().At(p).Timestamp())
					}
				case pmetric.MetricTypeExponentialHistogram:
					for p := 0; p < m.ExponentialHistogram().DataPoints().Len(); p++ {
						note(m.ExponentialHistogram().DataPoints().At(p).Timestamp())
					}
				case pmetric.MetricTypeSummary:
					for p := 0; p < m.Summary().DataPoints().Len(); p++ {
						note(m.Summary().DataPoints().At(p).Timestamp())
					}
				}
			}
		}
	}
	if last == 0 {
		return time.Time{}
	}
	return last.AsTime().UTC()
}

func writeScenarios(o options, evalTime time.Time) error {
	templates, err := filepath.Glob(filepath.Join(o.templates, "*.yaml"))
	if err != nil {
		return err
	}
	dir := filepath.Join(o.dir, "scenarios")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, t := range templates {
		b, err := os.ReadFile(t)
		if err != nil {
			return err
		}
		if !bytes.Contains(b, []byte("${EVAL_TIME}")) {
			return fmt.Errorf("%s has no ${EVAL_TIME} placeholder", t)
		}
		b = bytes.ReplaceAll(b, []byte("${EVAL_TIME}"), []byte(evalTime.Format(time.RFC3339)))
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(t)), b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func keys(m map[string]string) []string {
	var out []string
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}
