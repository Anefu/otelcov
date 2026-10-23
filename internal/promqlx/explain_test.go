package promqlx

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

var t0 = time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)

// testSeries has two samples per series, a minute apart, so range functions
// produce output.
func testSeries() []Series {
	mk := func(v float64, kv ...string) Series {
		return Series{Labels: labels.FromStrings(kv...), Samples: []Sample{{t0.Add(-time.Minute), v}, {t0, v * 2}}}
	}
	return []Series{
		mk(10, "__name__", "http_requests_total", "job", "api", "instance", "a:80", "code", "200"),
		mk(1, "__name__", "http_requests_total", "job", "api", "instance", "a:80", "code", "500"),
		mk(20, "__name__", "http_requests_total", "job", "api", "instance", "b:80", "code", "200"),
		mk(5, "__name__", "http_requests_total", "job", "web", "instance", "c:80", "code", "200"),
		mk(1, "__name__", "up", "job", "api", "instance", "a:80"),
		mk(1, "__name__", "up", "job", "api", "instance", "b:80"),
		mk(0, "__name__", "up", "job", "web", "instance", "c:80"),
		mk(1, "__name__", "node_info", "instance", "a:80", "zone", "eu-1"),
		mk(1, "__name__", "node_info", "instance", "b:80", "zone", "eu-2"),
		mk(3, "__name__", "latency_bucket", "job", "api", "le", "0.1"),
		mk(7, "__name__", "latency_bucket", "job", "api", "le", "+Inf"),
	}
}

// TestExplainerAgreesWithEngine checks the explainer's possible outputs
// against the real engine's outputs. Without value filters they must be equal;
// with them, every engine output must be a possible output.
func TestExplainerAgreesWithEngine(t *testing.T) {
	series := testSeries()
	lsets := make([]labels.Labels, len(series))
	for i, s := range series {
		lsets[i] = s.Labels
	}
	eng := NewEngine()
	for _, q := range []string{
		`http_requests_total`,
		`http_requests_total{code="500"}`,
		`sum(http_requests_total)`,
		`sum by (job) (http_requests_total)`,
		`sum without (code) (http_requests_total)`,
		`max by (job, instance) (rate(http_requests_total[5m]))`,
		`increase(http_requests_total{job="api"}[5m])`,
		`-http_requests_total`,
		`http_requests_total * 2`,
		`abs(http_requests_total)`,
		`sum by (job) (rate(http_requests_total[5m])) / on(job) group_left sum by (job) (up)`,
		`sum by (instance) (up) * on(instance) group_left(zone) node_info`,
		`up and on(instance) node_info`,
		`up unless on(instance) node_info`,
		`up or node_info`,
		`sum by (instance) (up) + on(instance) sum by (instance) (node_info)`,
		`label_replace(up, "host", "$1", "instance", "(.*):.*")`,
		`label_join(up, "id", "-", "job", "instance")`,
		`histogram_quantile(0.9, sum by (job, le) (rate(latency_bucket[5m])))`,
		`count(up)`,
		`last_over_time(up[5m])`,
		`up == 1`,
		`topk(1, http_requests_total)`,
		`sum(rate(http_requests_total[5m])) > 1000`,
		`vector(1)`,
	} {
		t.Run(q, func(t *testing.T) {
			expr, err := parser.ParseExpr(q)
			if err != nil {
				t.Fatal(err)
			}
			vec, err := eng.Instant(context.Background(), q, series, t0)
			if err != nil {
				t.Fatal(err)
			}
			ex, err := Explain(expr, lsets)
			if err != nil {
				t.Fatal(err)
			}
			engine := map[string]bool{}
			for _, s := range vec {
				engine[key(s.Metric)] = true
			}
			possible := map[string]bool{}
			for _, g := range ex.Groups {
				possible[key(g.Labels)] = true
			}
			for k := range engine {
				if !possible[k] {
					t.Errorf("engine output %s isn't a possible output; explainer has %v", k, keys(possible))
				}
			}
			if !FiltersByValue(expr) {
				for k := range possible {
					if !engine[k] {
						t.Errorf("possible output %s missing from engine output %v", k, keys(engine))
					}
				}
			}
		})
	}
}

func TestExplainerParentsAndDrops(t *testing.T) {
	series := testSeries()
	lsets := make([]labels.Labels, len(series))
	for i, s := range series {
		lsets[i] = s.Labels
	}
	ex, err := Explain(mustParse(t, `sum by (job) (http_requests_total{code="200"})`), lsets)
	if err != nil {
		t.Fatal(err)
	}
	parents := map[string][]int{}
	for _, g := range ex.Groups {
		parents[g.Labels.Get("job")] = g.Parents
	}
	if got := parents["api"]; len(got) != 2 || got[0] != 0 || got[1] != 2 {
		t.Errorf("api parents = %v, want [0 2]", got)
	}
	if len(ex.Drops) != 1 || ex.Drops[0].Input != 1 || ex.Drops[0].Label != "code" ||
		!strings.Contains(ex.Drops[0].Reason, `doesn't match code="500"`) {
		t.Errorf("drops = %+v", ex.Drops)
	}

	ex, err = Explain(mustParse(t, `up and on(instance) node_info`), lsets)
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Drops) != 1 || ex.Drops[0].Input != 6 || ex.Drops[0].Clause != "and" {
		t.Errorf("want up{instance=c:80} dropped by and: %+v", ex.Drops)
	}

	var unsup *UnsupportedError
	if _, err := Explain(mustParse(t, `count_values("v", up)`), lsets); !errors.As(err, &unsup) {
		t.Errorf("count_values should be unsupported, got %v", err)
	}
}

func mustParse(t *testing.T, q string) parser.Expr {
	t.Helper()
	e, err := parser.ParseExpr(q)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func key(ls labels.Labels) string {
	return labels.NewBuilder(ls).Del(labels.MetricName).Labels().String()
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestStructural(t *testing.T) {
	for in, want := range map[string]string{
		`x > 12e9`:                         `x > bool 12000000000`,
		`sum by (job) (rate(x[5m])) < 0.5`: `sum by (job) (rate(x[5m])) < bool 0.5`,
		`a < on (job) b`:                   `a < bool on (job) b`,
		`topk(3, x) > 1`:                   `x > bool 1`,
		`x and y > 2`:                      `x and y > bool 2`,
		`max by (a) (x)`:                   `max by (a) (x)`,
	} {
		e, err := Structural(mustParse(t, in))
		if err != nil {
			t.Fatal(err)
		}
		if e.String() != want {
			t.Errorf("Structural(%s) = %s, want %s", in, e, want)
		}
	}
	// The input isn't modified.
	orig := mustParse(t, `x > 1`)
	if _, err := Structural(orig); err != nil || orig.String() != `x > 1` {
		t.Errorf("input modified: %s", orig)
	}

	// Rewritten expressions keep every series that could pass, per the engine.
	series := testSeries()
	e, _ := Structural(mustParse(t, `up == 1`))
	vec, err := NewEngine().Instant(context.Background(), e.String(), series, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(vec) != 3 {
		t.Errorf("want all 3 up series (one is 0), got %d", len(vec))
	}
}
