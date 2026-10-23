package topology

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/Anefu/otelcov/internal/collector"
)

// Severity of a validation problem.
type Severity string

const (
	// Fatal problems stop the run: the topology doesn't describe the configs.
	Fatal Severity = "fatal"
	// Unverified problems make every path through the affected part unverified.
	Unverified Severity = "unverified"
	// Warning problems are reported; results are still trusted.
	Warning Severity = "warning"
)

// Problem is one validation finding.
type Problem struct {
	Severity Severity `json:"severity"`
	Where    string   `json:"where"`
	Message  string   `json:"message"`
}

// Validation is the result of Validate.
type Validation struct {
	Problems []Problem
	// BadJoins maps a join index to why data crossing it is unverified.
	BadJoins map[int]string
}

// Fatal returns the fatal problems.
func (v *Validation) Fatal() []Problem {
	var out []Problem
	for _, p := range v.Problems {
		if p.Severity == Fatal {
			out = append(out, p)
		}
	}
	return out
}

// Validate checks the topology against the configs it points at: files exist
// and parse, joins connect existing and used components with matching
// protocols and ports, every exporter is joined or external, and recorded
// hashes still match.
func Validate(f *File) *Validation {
	v := &Validation{BadJoins: map[int]string{}}
	add := func(s Severity, where, format string, args ...any) {
		v.Problems = append(v.Problems, Problem{Severity: s, Where: where, Message: fmt.Sprintf(format, args...)})
	}
	checkHash := func(where, path, want string) {
		if want == "" {
			return
		}
		got, err := FileHash(path)
		if err == nil && got != want {
			add(Warning, where, "%s changed since the topology was written; rerun discovery to confirm the topology still holds", f.Rel(path))
		}
	}

	configs := map[string]*collector.Config{}
	taps := 0
	gates := map[string]string{} // gate -> "collector:on|off"
	for _, name := range Names(f.Collectors) {
		c := f.Collectors[name]
		cfg, err := f.LoadCollector(name)
		if err != nil {
			add(Fatal, name, "can't load config: %v", err)
			continue
		}
		configs[name] = cfg
		checkHash(name, c.Config, c.SHA256)
		if c.Tap != "" {
			taps++
			tap, ok := cfg.Service.Pipelines[c.Tap]
			if !ok {
				add(Fatal, name, "tap pipeline %q doesn't exist", c.Tap)
			} else {
				tapped := map[string]bool{}
				for _, r := range tap.Receivers {
					tapped[r] = true
				}
				for _, pid := range Names(cfg.Service.Pipelines) {
					if pid == c.Tap || !IsMetrics(pid) {
						continue
					}
					for _, r := range cfg.Service.Pipelines[pid].Receivers {
						if !tapped[r] {
							add(Warning, name, "receiver %s feeds pipeline %s but isn't tapped, so its data isn't in recordings", r, pid)
						}
					}
				}
			}
		}
		for _, g := range c.FeatureGates {
			id, state := strings.TrimPrefix(g, "-"), "on"
			if strings.HasPrefix(g, "-") {
				state = "off"
			}
			if prev, ok := gates[id]; ok && !strings.HasSuffix(prev, ":"+state) {
				add(Fatal, name, "feature gate %s is %s here but %s; one otelcov build can only apply one setting", id, state, prev)
			}
			gates[id] = name + ":" + state
		}
	}
	if taps == 0 {
		add(Fatal, "collectors", "no collector has a tap pipeline, so there's no recorded input")
	}

	for _, name := range Names(f.Backends) {
		b := f.Backends[name]
		if b.Type != "mimir-otlp" {
			add(Fatal, name, "backend type %q isn't supported yet", b.Type)
		}
		if _, err := os.Stat(b.Config); err != nil {
			add(Fatal, name, "config: %v", err)
		}
		checkHash(name, b.Config, b.SHA256)
	}
	for _, name := range Names(f.Consumers) {
		c := f.Consumers[name]
		switch c.Type {
		case "prometheus-rules":
			if _, ok := f.Backends[c.Reads]; !ok {
				add(Fatal, name, "reads %q, which isn't a backend", c.Reads)
			}
			for _, p := range c.Paths {
				if _, err := os.Stat(p); err != nil {
					add(Fatal, name, "%v", err)
				}
			}
		case "alertmanager":
			if r, ok := f.Consumers[c.Reads]; !ok || r.Type != "prometheus-rules" {
				add(Fatal, name, "reads %q, which isn't a prometheus-rules consumer", c.Reads)
			}
			if _, err := os.Stat(c.Config); err != nil {
				add(Fatal, name, "config: %v", err)
			}
			checkHash(name, c.Config, c.SHA256)
		default:
			add(Fatal, name, "consumer type %q isn't supported yet", c.Type)
		}
	}

	// Joins.
	joined := map[string]bool{}
	for i, j := range f.Joins {
		where := j.From + " -> " + j.To
		from, err := f.ParseEndpoint(j.From)
		if err != nil || from.Kind != "exporters" {
			add(Fatal, where, "from must be <collector>/exporters/<id>")
			continue
		}
		to, err := f.ParseEndpoint(j.To)
		if err != nil {
			add(Fatal, where, "%v", err)
			continue
		}
		cfg, ok := configs[from.Collector]
		if !ok {
			add(Fatal, where, "no collector %q", from.Collector)
			continue
		}
		exp, ok := cfg.Exporters[from.ID]
		if !ok || !usedBy(cfg, from.ID, "exporters", f.Collectors[from.Collector].Tap) {
			add(Fatal, where, "exporter %s isn't defined or isn't used by any pipeline of %s", from.ID, from.Collector)
			continue
		}
		joined[j.From] = true
		if e, _ := exp["endpoint"].(string); collector.Unresolved(e) {
			add(Warning, where, "endpoint %q has an unresolved env reference, so ports can't be compared; add its value under env for %s", e, from.Collector)
		}
		if reason := checkJoin(f, configs, from, exp, to); reason != "" {
			v.BadJoins[i] = reason
			add(Unverified, where, "%s", reason)
		}
	}

	// Strict: every exporter in use is joined or external.
	external := map[string]bool{}
	for _, e := range f.External {
		external[e] = true
	}
	for _, name := range Names(configs) {
		cfg := configs[name]
		tap := f.Collectors[name].Tap
		for _, pid := range Names(cfg.Service.Pipelines) {
			if pid == tap || !IsMetrics(pid) {
				continue
			}
			for _, e := range cfg.Service.Pipelines[pid].Exporters {
				key := name + "/exporters/" + e
				if !joined[key] && !external[key] {
					add(Fatal, key, "exporter isn't joined to anything and isn't listed as external; rerun discovery or add it to the topology")
				}
			}
		}
	}
	return v
}

// checkJoin compares an exporter with what it's joined to. It returns why
// the join can't be trusted, or "".
func checkJoin(f *File, configs map[string]*collector.Config, from Endpoint, exp map[string]any, to Endpoint) string {
	typ := collector.Type(from.ID)
	endpoint, _ := exp["endpoint"].(string)
	if to.Backend != "" {
		if f.Backends[to.Backend].Type == "mimir-otlp" && typ != "otlphttp" {
			return fmt.Sprintf("Mimir's OTLP endpoint takes OTLP over HTTP, but %s is a %s exporter", from.ID, typ)
		}
		return ""
	}
	cfg, ok := configs[to.Collector]
	if !ok {
		return "no collector " + to.Collector
	}
	rcv, ok := cfg.Receivers[to.ID]
	if !ok || !usedBy(cfg, to.ID, "receivers", "") {
		return fmt.Sprintf("receiver %s isn't defined or isn't used by any pipeline of %s", to.ID, to.Collector)
	}
	if collector.Type(to.ID) != "otlp" {
		return fmt.Sprintf("only otlp receivers are modeled; %s is a %s receiver", to.ID, collector.Type(to.ID))
	}
	proto, defPort := "grpc", "4317"
	switch typ {
	case "otlp":
	case "otlphttp":
		proto, defPort = "http", "4318"
	default:
		return fmt.Sprintf("only otlp and otlphttp exporters are modeled; %s is a %s exporter", from.ID, typ)
	}
	protocols, _ := rcv["protocols"].(map[string]any)
	pc, has := protocols[proto]
	if !has {
		return fmt.Sprintf("%s sends OTLP over %s, but receiver %s/%s doesn't enable %s", from.ID, proto, to.Collector, to.ID, proto)
	}
	rEndpoint := "localhost:" + defPort // the receiver's default since collector 0.104
	if m, ok := pc.(map[string]any); ok {
		if e, ok := m["endpoint"].(string); ok && e != "" {
			rEndpoint = e
		}
	}
	if collector.Unresolved(rEndpoint) || collector.Unresolved(endpoint) {
		return "" // reported as a warning by the caller's env check; can't compare
	}
	rHost, rPort, err := net.SplitHostPort(rEndpoint)
	if err != nil {
		return fmt.Sprintf("can't parse receiver endpoint %q", rEndpoint)
	}
	if rHost == "localhost" || rHost == "127.0.0.1" || rHost == "::1" {
		return fmt.Sprintf("receiver %s/%s listens on %s only, so other pods can't reach it", to.Collector, to.ID, rEndpoint)
	}
	ePort, err := exporterPort(typ, endpoint)
	if err != nil {
		return err.Error()
	}
	if ePort != "" && ePort != rPort {
		return fmt.Sprintf("%s sends to port %s, but receiver %s/%s listens on port %s", from.ID, ePort, to.Collector, to.ID, rPort)
	}
	return ""
}

func exporterPort(typ, endpoint string) (string, error) {
	if endpoint == "" {
		return "", fmt.Errorf("exporter has no endpoint")
	}
	if typ == "otlphttp" || strings.Contains(endpoint, "://") {
		e := strings.TrimPrefix(endpoint, "dns:///")
		if !strings.Contains(e, "://") {
			e = "http://" + e
		}
		u, err := url.Parse(e)
		if err != nil {
			return "", fmt.Errorf("can't parse exporter endpoint %q", endpoint)
		}
		if p := u.Port(); p != "" {
			return p, nil
		}
		if typ == "otlp" {
			return "", nil
		}
		if u.Scheme == "https" {
			return "443", nil
		}
		return "80", nil
	}
	_, p, err := net.SplitHostPort(endpoint)
	if err != nil {
		return "", fmt.Errorf("can't parse exporter endpoint %q", endpoint)
	}
	return p, nil
}

// usedBy reports whether a component is used by a metrics pipeline other than
// skip.
func usedBy(cfg *collector.Config, id, kind, skip string) bool {
	for pid, p := range cfg.Service.Pipelines {
		if pid == skip || !IsMetrics(pid) {
			continue
		}
		list := p.Receivers
		if kind == "exporters" {
			list = p.Exporters
		}
		for _, x := range list {
			if x == id {
				return true
			}
		}
	}
	return false
}

// IsMetrics reports whether a pipeline ID is a metrics pipeline.
func IsMetrics(pipelineID string) bool {
	return pipelineID == "metrics" || strings.HasPrefix(pipelineID, "metrics/")
}
