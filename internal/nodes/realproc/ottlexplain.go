package realproc

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottldatapoint"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlmetric"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlresource"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlscope"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/ottlfuncs"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/Anefu/otelcov/internal/lineage"
	"github.com/Anefu/otelcov/internal/unit"
)

// The statement explainer replays a transform's statements one at a time with
// the real OTTL library, to say which statement changed what, and which
// statements didn't run because their condition was false (and what they
// would have changed). The real processor's output stays the truth; the
// replay is checked against it.

// runFn executes a statement on a single-data-point item. With editorOnly, the
// where clause is dropped (to see what the statement would change). It
// reports whether the condition held.
type runFn func(ctx context.Context, md pmetric.Metrics, editorOnly bool) (bool, error)

type statement struct {
	context string
	text    string
	run     runFn
}

// effect is what one statement did to one item.
type effect struct {
	stmt    statement
	met     bool
	err     error
	changes []lineage.Event // what it changed
	would   []lineage.Event // condition false: what it would have changed
}

type stmtExplainer struct {
	stmts []statement
	// incomplete lists why some statements can't be replayed; the replay is
	// then not checked against the processor's output.
	incomplete []string
}

// newTransformExplainer parses a transform processor's metric_statements.
func newTransformExplainer(raw map[string]any) *stmtExplainer {
	x := &stmtExplainer{}
	groups, _ := raw["metric_statements"].([]any)
	for _, g := range groups {
		m, ok := g.(map[string]any)
		if !ok {
			x.incomplete = append(x.incomplete, "statements without a context block (context inference) aren't replayed yet")
			continue
		}
		ctxName, _ := m["context"].(string)
		if c, ok := m["conditions"].([]any); ok && len(c) > 0 {
			x.incomplete = append(x.incomplete, "statement groups with conditions aren't replayed yet")
			continue
		}
		stmts, _ := m["statements"].([]any)
		for _, s := range stmts {
			text, _ := s.(string)
			st, err := compile(ctxName, text)
			if err != nil {
				x.incomplete = append(x.incomplete, fmt.Sprintf("statement %q: %v", text, err))
				continue
			}
			x.stmts = append(x.stmts, st)
		}
	}
	return x
}

// explain replays the statements on a copy of one item. It returns each
// statement's effect and the replayed result.
func (x *stmtExplainer) explain(ctx context.Context, item pmetric.Metrics) ([]effect, pmetric.Metrics) {
	cur := pmetric.NewMetrics()
	item.CopyTo(cur)
	var out []effect
	for _, s := range x.stmts {
		before := pmetric.NewMetrics()
		cur.CopyTo(before)
		met, err := s.run(ctx, cur, false)
		e := effect{stmt: s, met: met, err: err}
		if err == nil {
			if met {
				e.changes = diff(before, cur)
			} else {
				scratch := pmetric.NewMetrics()
				cur.CopyTo(scratch)
				if _, werr := s.run(ctx, scratch, true); werr == nil {
					e.would = diff(cur, scratch)
				}
			}
		}
		out = append(out, e)
	}
	return out, cur
}

func compile(ctxName, text string) (statement, error) {
	editor := editorPart(text)
	set := componenttest.NewNopTelemetrySettings()
	switch ctxName {
	case "resource":
		p, err := ottlresource.NewParser(ottlfuncs.StandardFuncs[ottlresource.TransformContext](), set)
		if err != nil {
			return statement{}, err
		}
		full, ed, err := parseBoth(p, text, editor)
		if err != nil {
			return statement{}, err
		}
		return statement{ctxName, text, func(ctx context.Context, md pmetric.Metrics, editorOnly bool) (bool, error) {
			rm := md.ResourceMetrics().At(0)
			_, met, err := pick(full, ed, editorOnly).Execute(ctx, ottlresource.NewTransformContext(rm.Resource(), rm))
			return met, err
		}}, nil
	case "scope":
		p, err := ottlscope.NewParser(ottlfuncs.StandardFuncs[ottlscope.TransformContext](), set)
		if err != nil {
			return statement{}, err
		}
		full, ed, err := parseBoth(p, text, editor)
		if err != nil {
			return statement{}, err
		}
		return statement{ctxName, text, func(ctx context.Context, md pmetric.Metrics, editorOnly bool) (bool, error) {
			rm := md.ResourceMetrics().At(0)
			sm := rm.ScopeMetrics().At(0)
			_, met, err := pick(full, ed, editorOnly).Execute(ctx, ottlscope.NewTransformContext(sm.Scope(), rm.Resource(), sm))
			return met, err
		}}, nil
	case "metric":
		p, err := ottlmetric.NewParser(ottlfuncs.StandardFuncs[ottlmetric.TransformContext](), set)
		if err != nil {
			return statement{}, err
		}
		full, ed, err := parseBoth(p, text, editor)
		if err != nil {
			return statement{}, err
		}
		return statement{ctxName, text, func(ctx context.Context, md pmetric.Metrics, editorOnly bool) (bool, error) {
			rm := md.ResourceMetrics().At(0)
			sm := rm.ScopeMetrics().At(0)
			m := sm.Metrics().At(0)
			_, met, err := pick(full, ed, editorOnly).Execute(ctx, ottlmetric.NewTransformContext(m, sm.Metrics(), sm.Scope(), rm.Resource(), sm, rm))
			return met, err
		}}, nil
	case "datapoint":
		p, err := ottldatapoint.NewParser(ottlfuncs.StandardFuncs[ottldatapoint.TransformContext](), set)
		if err != nil {
			return statement{}, err
		}
		full, ed, err := parseBoth(p, text, editor)
		if err != nil {
			return statement{}, err
		}
		return statement{ctxName, text, func(ctx context.Context, md pmetric.Metrics, editorOnly bool) (bool, error) {
			rm := md.ResourceMetrics().At(0)
			sm := rm.ScopeMetrics().At(0)
			m := sm.Metrics().At(0)
			_, met, err := pick(full, ed, editorOnly).Execute(ctx, ottldatapoint.NewTransformContext(dataPoint(m), m, sm.Metrics(), sm.Scope(), rm.Resource(), sm, rm))
			return met, err
		}}, nil
	}
	return statement{}, fmt.Errorf("context %q isn't replayed yet", ctxName)
}

func parseBoth[K any](p ottl.Parser[K], text, editor string) (*ottl.Statement[K], *ottl.Statement[K], error) {
	full, err := p.ParseStatement(text)
	if err != nil {
		return nil, nil, err
	}
	ed, err := p.ParseStatement(editor)
	if err != nil {
		ed = nil // the counterfactual just isn't available
	}
	return full, ed, nil
}

func pick[K any](full, editor *ottl.Statement[K], editorOnly bool) *ottl.Statement[K] {
	if editorOnly && editor != nil {
		return editor
	}
	return full
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
