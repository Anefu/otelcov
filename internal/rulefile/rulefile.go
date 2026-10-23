// Package rulefile loads Prometheus-format rule files with Prometheus's own
// parser.
package rulefile

import (
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/rulefmt"
	"github.com/prometheus/prometheus/promql/parser"
)

// Recording is a recording rule.
type Recording struct {
	Record string
	Expr   parser.Expr
	Labels map[string]string
	Group  string
	File   string
}

// Alerting is an alerting rule.
type Alerting struct {
	Alert       string
	Expr        parser.Expr
	For         time.Duration
	Labels      map[string]string
	Annotations map[string]string
	Group       string
	File        string
}

// Rules are the rules of a set of files, in file order.
type Rules struct {
	Recording []Recording
	Alerting  []Alerting
}

// Load parses rule files. Unknown fields are an error.
func Load(paths []string) (*Rules, error) {
	out := &Rules{}
	for _, path := range paths {
		groups, errs := rulefmt.ParseFile(path, false)
		if len(errs) > 0 {
			return nil, fmt.Errorf("%s: %w", path, errors.Join(errs...))
		}
		for _, g := range groups.Groups {
			for _, r := range g.Rules {
				expr, err := parser.ParseExpr(r.Expr)
				if err != nil {
					return nil, fmt.Errorf("%s: group %s: %w", path, g.Name, err)
				}
				if r.Record != "" {
					out.Recording = append(out.Recording, Recording{
						Record: r.Record, Expr: expr, Labels: r.Labels, Group: g.Name, File: path,
					})
					continue
				}
				out.Alerting = append(out.Alerting, Alerting{
					Alert: r.Alert, Expr: expr, For: time.Duration(r.For), Labels: r.Labels,
					Annotations: r.Annotations, Group: g.Name, File: path,
				})
			}
		}
	}
	return out, nil
}

// SelectedNames returns the metric names expr selects with equality or
// regex matchers on __name__, as matchers.
func SelectedNames(expr parser.Expr) []*labels.Matcher {
	var out []*labels.Matcher
	parser.Inspect(expr, func(n parser.Node, _ []parser.Node) error {
		if vs, ok := n.(*parser.VectorSelector); ok {
			for _, m := range vs.LabelMatchers {
				if m.Name == labels.MetricName {
					out = append(out, m)
				}
			}
		}
		return nil
	})
	return out
}

// OrderRecording sorts recording rules so every rule comes after the rules
// whose outputs it reads. Independent rules keep file order. A cycle is an
// error.
func OrderRecording(rules []Recording) ([]Recording, error) {
	deps := make([][]int, len(rules))
	for i, r := range rules {
		for _, m := range SelectedNames(r.Expr) {
			for j, other := range rules {
				if j != i && m.Matches(other.Record) {
					deps[i] = append(deps[i], j)
				}
			}
		}
	}
	const (
		unvisited = iota
		visiting
		done
	)
	state := make([]int, len(rules))
	var out []Recording
	var visit func(i int) error
	visit = func(i int) error {
		switch state[i] {
		case done:
			return nil
		case visiting:
			return fmt.Errorf("recording rules depend on each other in a cycle through %s", rules[i].Record)
		}
		state[i] = visiting
		for _, d := range deps[i] {
			if err := visit(d); err != nil {
				return err
			}
		}
		state[i] = done
		out = append(out, rules[i])
		return nil
	}
	for i := range rules {
		if err := visit(i); err != nil {
			return nil, err
		}
	}
	return out, nil
}
