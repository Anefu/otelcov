package realproc

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottldatapoint"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlmetric"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/ottlfuncs"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/processor"

	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/unit"
)

// The statement explainer says which transform statement changed what, and
// which statements didn't run because their condition was false, using the
// real transform processor throughout. For statement i it runs a processor
// with the statements up to i (the "prefix"): the difference from the prefix
// before is what statement i did. A statement with a condition that changed
// nothing runs once more on the data before it, without its condition and its
// group's conditions: the difference is what it would have changed. That
// covers every statement the processor accepts, statement groups with
// conditions, statements without a context block and transform-only
// functions alike.

type statement struct {
	context string // the group's context; "" when the processor infers it
	text    string
	// where is the statement's condition: its group's conditions (ORed)
	// and its where clause. "" when it has none.
	where      string
	prefix     processor.Metrics
	prefixSink *consumertest.MetricsSink
	// free runs the statement without its condition; nil when it has none.
	free     processor.Metrics
	freeSink *consumertest.MetricsSink
}

// describe names the statement in reasons.
func (s *statement) describe() string {
	if s.context == "" {
		return "statement `" + s.text + "`"
	}
	return s.context + " statement `" + s.text + "`"
}

// effect is what one statement did to one item.
type effect struct {
	stmt    *statement
	changes []lineage.Event // what it changed
	would   []lineage.Event // condition false: what it would have changed
}

type stmtExplainer struct {
	stmts []*statement
	// incomplete is set when the prefix processors couldn't be built; the
	// changes then aren't attributed to statements.
	incomplete string
}

// newTransformExplainer builds the prefix and condition-free processors for a
// transform processor's metric_statements.
func newTransformExplainer(ctx context.Context, id string, raw map[string]any) *stmtExplainer {
	x := &stmtExplainer{}
	groups, _ := raw["metric_statements"].([]any)
	with := func(stmts []any) map[string]any {
		cfg := map[string]any{}
		for k, v := range raw {
			cfg[k] = v
		}
		cfg["metric_statements"] = stmts
		return cfg
	}
	var done []any // groups before the current one, complete
	for _, g := range groups {
		var texts []string
		var conds []string
		group, isGroup := g.(map[string]any)
		ctxName := ""
		if isGroup {
			ctxName, _ = group["context"].(string)
			for _, c := range asList(group["conditions"]) {
				conds = append(conds, c)
			}
			texts = asList(group["statements"])
		} else if text, ok := g.(string); ok {
			texts = []string{text} // basic config: the context is inferred
		}
		for i, text := range texts {
			s := &statement{context: ctxName, text: text, where: combine(conds, whereClause(text))}
			var current any = text
			if isGroup {
				part := copyMap(group)
				part["statements"] = toAny(texts[:i+1])
				current = part
			}
			var err error
			if s.prefix, s.prefixSink, err = build(ctx, id, with(append(append([]any(nil), done...), current))); err != nil {
				x.incomplete = fmt.Sprintf("statement %q: %v", text, err)
				return x
			}
			if s.where != "" {
				var free any = editorPart(text)
				if isGroup {
					part := copyMap(group)
					delete(part, "conditions")
					part["statements"] = []any{editorPart(text)}
					free = part
				}
				// Without its condition a statement may not build (e.g. it
				// needs the condition's context); then nothing is said about it.
				s.free, s.freeSink, _ = build(ctx, id, with([]any{free}))
			}
			x.stmts = append(x.stmts, s)
		}
		done = append(done, g)
	}
	return x
}

// explain runs the statements on a copy of one item. It returns each
// statement's effect and the result of all statements, or false if the
// processor didn't return exactly one item at some step (it split, merged or
// dropped the data), in which case nothing is attributed.
func (x *stmtExplainer) explain(ctx context.Context, item pmetric.Metrics) ([]effect, pmetric.Metrics, bool) {
	if x.incomplete != "" {
		return nil, pmetric.Metrics{}, false
	}
	cur := pmetric.NewMetrics()
	item.CopyTo(cur)
	var out []effect
	for _, s := range x.stmts {
		next, err := runOnce(ctx, s.prefix, s.prefixSink, item)
		if err != nil || len(next) != 1 {
			return nil, pmetric.Metrics{}, false
		}
		e := effect{stmt: s, changes: diff(cur, next[0])}
		if len(e.changes) == 0 && s.free != nil {
			if alt, err := runOnce(ctx, s.free, s.freeSink, cur); err == nil && len(alt) == 1 {
				e.would = diff(cur, alt[0])
			}
		}
		out = append(out, e)
		cur = next[0]
	}
	return out, cur, true
}

func (x *stmtExplainer) shutdown(ctx context.Context) {
	for _, s := range x.stmts {
		_ = s.prefix.Shutdown(ctx)
		if s.free != nil {
			_ = s.free.Shutdown(ctx)
		}
	}
}

// combine describes a statement's condition: its group's conditions, which
// the processor ORs, and its where clause.
func combine(conds []string, where string) string {
	group := strings.Join(conds, " or ")
	switch {
	case group == "":
		return where
	case where == "":
		return group
	case len(conds) > 1:
		return "(" + group + ") and " + where
	default:
		return group + " and " + where
	}
}

func asList(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, x := range l {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// editorPart returns a statement without its where clause, splitting on the
// first " where " outside quotes.
func editorPart(s string) string {
	ed, _ := splitWhere(s)
	return ed
}

// whereClause returns a statement's condition, or "".
func whereClause(s string) string {
	_, cond := splitWhere(s)
	return cond
}

func splitWhere(s string) (editor, cond string) {
	inQuote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inQuote != 0:
			if c == '\\' {
				i++
			} else if c == inQuote {
				inQuote = 0
			}
		case c == '"' || c == '\'':
			inQuote = c
		case strings.HasPrefix(s[i:], " where "):
			return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+len(" where "):])
		}
	}
	return s, ""
}

// dataPoint returns an item's single data point, as OTTL expects it.
func dataPoint(m pmetric.Metric) any {
	switch m.Type() {
	case pmetric.MetricTypeGauge:
		return m.Gauge().DataPoints().At(0)
	case pmetric.MetricTypeSum:
		return m.Sum().DataPoints().At(0)
	case pmetric.MetricTypeHistogram:
		return m.Histogram().DataPoints().At(0)
	case pmetric.MetricTypeExponentialHistogram:
		return m.ExponentialHistogram().DataPoints().At(0)
	case pmetric.MetricTypeSummary:
		return m.Summary().DataPoints().At(0)
	}
	return nil
}

// filterExplainer finds which filter condition drops an item.
type filterExplainer struct {
	metric    []condition[ottlmetric.TransformContext]
	datapoint []condition[ottldatapoint.TransformContext]
}

type condition[K any] struct {
	text string
	c    *ottl.Condition[K]
}

func newFilterExplainer(raw map[string]any) *filterExplainer {
	x := &filterExplainer{}
	metrics, _ := raw["metrics"].(map[string]any)
	set := componenttest.NewNopTelemetrySettings()
	if conds, _ := metrics["metric"].([]any); len(conds) > 0 {
		if p, err := ottlmetric.NewParser(ottlfuncs.StandardConverters[ottlmetric.TransformContext](), set); err == nil {
			for _, c := range conds {
				text, _ := c.(string)
				if parsed, err := p.ParseCondition(text); err == nil {
					x.metric = append(x.metric, condition[ottlmetric.TransformContext]{text, parsed})
				}
			}
		}
	}
	if conds, _ := metrics["datapoint"].([]any); len(conds) > 0 {
		if p, err := ottldatapoint.NewParser(ottlfuncs.StandardConverters[ottldatapoint.TransformContext](), set); err == nil {
			for _, c := range conds {
				text, _ := c.(string)
				if parsed, err := p.ParseCondition(text); err == nil {
					x.datapoint = append(x.datapoint, condition[ottldatapoint.TransformContext]{text, parsed})
				}
			}
		}
	}
	return x
}

// why returns the first condition that matches the item, or "".
func (x *filterExplainer) why(ctx context.Context, md pmetric.Metrics) string {
	rm := md.ResourceMetrics().At(0)
	sm := rm.ScopeMetrics().At(0)
	m := unit.FirstMetric(md)
	for _, c := range x.metric {
		if ok, err := c.c.Eval(ctx, ottlmetric.NewTransformContext(m, sm.Metrics(), sm.Scope(), rm.Resource(), sm, rm)); err == nil && ok {
			return "metric condition `" + c.text + "`"
		}
	}
	for _, c := range x.datapoint {
		if ok, err := c.c.Eval(ctx, ottldatapoint.NewTransformContext(dataPoint(m), m, sm.Metrics(), sm.Scope(), rm.Resource(), sm, rm)); err == nil && ok {
			return "data point condition `" + c.text + "`"
		}
	}
	return ""
}
