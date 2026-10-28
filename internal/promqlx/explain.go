package promqlx

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

// The explainer follows label flow through a PromQL expression. It never
// decides which outputs exist (the engine does that); it says which inputs
// each possible output comes from, and why inputs drop out.

// Group is one possible output of an expression and the inputs it comes from.
type Group struct {
	Labels  labels.Labels
	Parents []int // indexes into the explained inputs, ascending
}

// Drop says why an input contributes to no output.
type Drop struct {
	Input  int
	Label  string // label the clause needed, if any
	Clause string // "selector", "and", "unless", "on(...)" ...
	// Matcher is the selector matcher that failed, for selector drops.
	Matcher string
	Reason  string
	// Name is set when the series has another name for the selector's
	// metric (Label is then __name__).
	Name *NameMiss
}

// NameMiss is a series whose name is the selector's metric name written
// differently: the same words apart from translation suffixes and
// separators. Suffixes are written as in names, without the leading
// underscore ("bytes", "bytes_total"), "" for none.
type NameMiss struct {
	Want, Got             string
	WantSuffix, GotSuffix string
}

// Explanation is the result of Explain.
type Explanation struct {
	Groups []Group
	Drops  []Drop
}

// UnsupportedError reports an expression construct the explainer can't follow.
type UnsupportedError struct{ Construct string }

func (e *UnsupportedError) Error() string {
	return "the explainer doesn't support " + e.Construct + " yet"
}

func unsupported(format string, args ...any) error {
	return &UnsupportedError{Construct: fmt.Sprintf(format, args...)}
}

// item is a series flowing through the expression.
type item struct {
	lset    labels.Labels
	parents []int
}

type flow struct {
	scalar bool
	items  []item
}

type explainer struct {
	inputs  []labels.Labels
	drops   []Drop
	dropped map[string]bool
}

// Explain follows expr over inputs.
func Explain(expr parser.Expr, inputs []labels.Labels) (*Explanation, error) {
	x := &explainer{inputs: inputs, dropped: map[string]bool{}}
	f, err := x.walk(expr)
	if err != nil {
		return nil, err
	}
	out := &Explanation{Drops: x.drops}
	if f.scalar {
		out.Groups = []Group{{Labels: labels.EmptyLabels()}}
		return out, nil
	}
	out.Groups = mergeGroups(f.items)
	sort.SliceStable(out.Drops, func(i, j int) bool { return out.Drops[i].Input < out.Drops[j].Input })
	return out, nil
}

func (x *explainer) drop(d Drop) {
	k := fmt.Sprintf("%d|%s|%s|%s", d.Input, d.Clause, d.Matcher, d.Label)
	if !x.dropped[k] {
		x.dropped[k] = true
		x.drops = append(x.drops, d)
	}
}

func (x *explainer) walk(e parser.Expr) (flow, error) {
	switch n := e.(type) {
	case *parser.ParenExpr:
		return x.walk(n.Expr)
	case *parser.StepInvariantExpr:
		return x.walk(n.Expr)
	case *parser.SubqueryExpr:
		return x.walk(n.Expr)
	case *parser.MatrixSelector:
		return x.walk(n.VectorSelector)
	case *parser.NumberLiteral, *parser.StringLiteral:
		return flow{scalar: true}, nil
	case *parser.VectorSelector:
		return x.selector(n), nil
	case *parser.UnaryExpr:
		f, err := x.walk(n.Expr)
		if err != nil || f.scalar {
			return f, err
		}
		return withoutName(f), nil
	case *parser.AggregateExpr:
		return x.aggregate(n)
	case *parser.Call:
		return x.call(n)
	case *parser.BinaryExpr:
		return x.binary(n)
	}
	return flow{}, unsupported("%T", e)
}

// selector keeps inputs that match every matcher, and records near misses:
// series with the selector's metric name (or, without one, failing a single
// matcher) that a matcher rejects, and series whose name is a variant of the
// selector's metric name (see nameVariant).
func (x *explainer) selector(vs *parser.VectorSelector) flow {
	var nameM *labels.Matcher
	for _, m := range vs.LabelMatchers {
		if m.Name == labels.MetricName {
			nameM = m
		}
	}
	var f flow
	for i, ls := range x.inputs {
		var failed []*labels.Matcher
		for _, m := range vs.LabelMatchers {
			if !m.Matches(ls.Get(m.Name)) {
				failed = append(failed, m)
			}
		}
		if len(failed) == 0 {
			f.items = append(f.items, item{lset: ls, parents: []int{i}})
			continue
		}
		name := ls.Get(labels.MetricName)
		if nameM != nil && nameM.Type == labels.MatchEqual && !nameM.Matches(name) {
			// The name is the first thing to fix; other matchers aren't
			// checked against a series the selector doesn't name.
			if miss, ok := nameVariant(nameM.Value, name); ok {
				x.drop(Drop{Input: i, Label: labels.MetricName, Clause: "selector", Matcher: nameM.String(), Name: miss,
					Reason: fmt.Sprintf("the selector wants metric %q; this series is named %q, the same name %s", miss.Want, miss.Got, miss.difference())})
			}
			continue
		}
		nearMiss := (nameM != nil && nameM.Matches(name)) || (nameM == nil && len(failed) == 1)
		if !nearMiss {
			continue
		}
		for _, m := range failed {
			reason := fmt.Sprintf("the selector matcher %s doesn't match %s=%q", m, m.Name, ls.Get(m.Name))
			if !ls.Has(m.Name) {
				reason = fmt.Sprintf("the selector matcher %s needs this label; the series has none", m)
			}
			x.drop(Drop{Input: i, Label: m.Name, Clause: "selector", Matcher: m.String(), Reason: reason})
		}
	}
	return f
}

// Words Prometheus's OTLP translation appends to metric names (the
// otlptranslator package's unit maps): unit suffixes, "per <unit>", "total"
// for counters, "ratio" for unit 1 gauges. Units outside the maps are
// appended as written and can't be recognized.
var (
	unitWords = map[string]bool{
		"days": true, "hours": true, "minutes": true, "seconds": true, "milliseconds": true, "microseconds": true, "nanoseconds": true,
		"bytes": true, "kibibytes": true, "mebibytes": true, "gibibytes": true, "tibibytes": true,
		"kilobytes": true, "megabytes": true, "gigabytes": true, "terabytes": true,
		"meters": true, "volts": true, "amperes": true, "joules": true, "watts": true, "grams": true,
		"celsius": true, "hertz": true, "percent": true,
	}
	perWords = map[string]bool{"second": true, "minute": true, "hour": true, "day": true, "week": true, "month": true, "year": true}
	// Series a histogram or summary becomes; they must agree.
	seriesKinds = map[string]bool{"bucket": true, "count": true, "sum": true}
)

// stem splits a metric name into its words, apart from the translation
// suffix, and the series kind (bucket, count, sum) if any. Any character
// other than a letter or digit separates words, so an OpenTelemetry name
// (process.memory.usage) and its Prometheus form (process_memory_usage)
// have the same words.
func stem(name string) (base []string, kind string, suffix []string) {
	words := strings.FieldsFunc(name, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	i := len(words)
	if i > 1 && seriesKinds[words[i-1]] {
		kind = words[i-1]
		i--
	}
	end := i
	if i > 1 && words[i-1] == "total" {
		i--
	}
	if i > 1 && words[i-1] == "ratio" {
		i--
	}
	if i > 2 && words[i-2] == "per" && perWords[words[i-1]] {
		i -= 2
	}
	if i > 1 && unitWords[words[i-1]] {
		i--
	}
	return words[:i], kind, words[i:end]
}

// nameVariant reports whether got is the metric name want written
// differently: the same words and series kind, with other translation
// suffixes or other separators. That is how one OpenTelemetry metric is
// named under different translation settings (suffixes on or off, UTF-8
// names) or with another unit.
func nameVariant(want, got string) (*NameMiss, bool) {
	wb, wk, ws := stem(want)
	gb, gk, gs := stem(got)
	if want == got || wk != gk || len(wb) == 0 || strings.Join(wb, " ") != strings.Join(gb, " ") {
		return nil, false
	}
	return &NameMiss{Want: want, Got: got, WantSuffix: strings.Join(ws, "_"), GotSuffix: strings.Join(gs, "_")}, true
}

// difference says how the names differ, for reasons: "without the `_bytes`
// suffix".
func (m *NameMiss) difference() string {
	switch {
	case m.WantSuffix == m.GotSuffix:
		return "with other separators"
	case m.GotSuffix == "":
		return fmt.Sprintf("without the %q suffix", "_"+m.WantSuffix)
	case m.WantSuffix == "":
		return fmt.Sprintf("with a %q suffix", "_"+m.GotSuffix)
	}
	return fmt.Sprintf("with the suffix %q instead of %q", "_"+m.GotSuffix, "_"+m.WantSuffix)
}

func (x *explainer) aggregate(a *parser.AggregateExpr) (flow, error) {
	f, err := x.walk(a.Expr)
	if err != nil {
		return f, err
	}
	switch a.Op {
	case parser.TOPK, parser.BOTTOMK, parser.LIMITK, parser.LIMIT_RATIO:
		// Outputs are a value-dependent subset of the inputs, labels unchanged.
		return f, nil
	case parser.COUNT_VALUES:
		return flow{}, unsupported("count_values")
	}
	if !a.Op.IsAggregator() {
		return flow{}, unsupported("aggregation %s", a.Op)
	}
	grouped := make([]item, 0, len(f.items))
	for _, it := range f.items {
		b := labels.NewBuilder(it.lset)
		if a.Without {
			b.Del(append([]string{labels.MetricName}, a.Grouping...)...)
		} else {
			b.Keep(a.Grouping...)
		}
		grouped = append(grouped, item{lset: b.Labels(), parents: it.parents})
	}
	return flow{items: merge(grouped)}, nil
}

// Functions that keep labels and drop the metric name.
var dropsName = map[string]bool{
	"abs": true, "ceil": true, "floor": true, "round": true, "exp": true, "sqrt": true,
	"ln": true, "log2": true, "log10": true, "sgn": true, "deg": true, "rad": true,
	"clamp": true, "clamp_min": true, "clamp_max": true, "timestamp": true,
	"rate": true, "irate": true, "increase": true, "delta": true, "idelta": true,
	"deriv": true, "predict_linear": true, "changes": true, "resets": true,
	"avg_over_time": true, "min_over_time": true, "max_over_time": true, "sum_over_time": true,
	"count_over_time": true, "stddev_over_time": true, "stdvar_over_time": true,
	"quantile_over_time": true, "mad_over_time": true, "present_over_time": true,
	"day_of_month": true, "day_of_week": true, "day_of_year": true, "days_in_month": true,
	"hour": true, "minute": true, "month": true, "year": true,
	"histogram_count": true, "histogram_sum": true, "histogram_avg": true,
}

// Functions that keep labels including the metric name.
var keepsName = map[string]bool{
	"last_over_time": true, "sort": true, "sort_desc": true, "sort_by_label": true, "sort_by_label_desc": true,
}

func (x *explainer) call(c *parser.Call) (flow, error) {
	name := c.Func.Name
	if c.Func.ReturnType == parser.ValueTypeScalar || name == "vector" {
		if name == "vector" {
			return flow{items: []item{{lset: labels.EmptyLabels()}}}, nil
		}
		return flow{scalar: true}, nil
	}
	// The one vector or matrix argument.
	var arg parser.Expr
	for _, a := range c.Args {
		if t := a.Type(); t == parser.ValueTypeVector || t == parser.ValueTypeMatrix {
			if arg != nil {
				return flow{}, unsupported("function %s with several vector arguments", name)
			}
			arg = a
		}
	}
	if arg == nil {
		return flow{}, unsupported("function %s", name)
	}
	f, err := x.walk(arg)
	if err != nil {
		return f, err
	}
	switch {
	case keepsName[name]:
		return f, nil
	case dropsName[name]:
		return withoutName(f), nil
	case name == "histogram_quantile" || name == "histogram_fraction":
		out := make([]item, len(f.items))
		for i, it := range f.items {
			out[i] = item{lset: labels.NewBuilder(it.lset).Del(labels.MetricName, labels.BucketLabel).Labels(), parents: it.parents}
		}
		return flow{items: merge(out)}, nil
	case name == "label_replace":
		return labelReplace(f, c.Args)
	case name == "label_join":
		return labelJoin(f, c.Args)
	}
	return flow{}, unsupported("function %s", name)
}

func labelReplace(f flow, args parser.Expressions) (flow, error) {
	str := func(i int) (string, bool) {
		s, ok := unwrap(args[i]).(*parser.StringLiteral)
		if !ok {
			return "", false
		}
		return s.Val, true
	}
	dst, ok1 := str(1)
	repl, ok2 := str(2)
	src, ok3 := str(3)
	re, ok4 := str(4)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return flow{}, unsupported("label_replace with non-literal arguments")
	}
	rx, err := regexp.Compile("^(?s:" + re + ")$")
	if err != nil {
		return flow{}, err
	}
	out := make([]item, len(f.items))
	for i, it := range f.items {
		out[i] = it
		v := it.lset.Get(src)
		idx := rx.FindStringSubmatchIndex(v)
		if idx == nil {
			continue
		}
		res := string(rx.ExpandString(nil, repl, v, idx))
		b := labels.NewBuilder(it.lset)
		if res == "" {
			b.Del(dst)
		} else {
			b.Set(dst, res)
		}
		out[i].lset = b.Labels()
	}
	return flow{items: merge(out)}, nil
}

func labelJoin(f flow, args parser.Expressions) (flow, error) {
	var strs []string
	for _, a := range args[1:] {
		s, ok := unwrap(a).(*parser.StringLiteral)
		if !ok {
			return flow{}, unsupported("label_join with non-literal arguments")
		}
		strs = append(strs, s.Val)
	}
	dst, sep, srcs := strs[0], strs[1], strs[2:]
	out := make([]item, len(f.items))
	for i, it := range f.items {
		vals := make([]string, len(srcs))
		for j, s := range srcs {
			vals[j] = it.lset.Get(s)
		}
		b := labels.NewBuilder(it.lset)
		if v := strings.Join(vals, sep); v == "" {
			b.Del(dst)
		} else {
			b.Set(dst, v)
		}
		out[i] = item{lset: b.Labels(), parents: it.parents}
	}
	return flow{items: merge(out)}, nil
}

func (x *explainer) binary(b *parser.BinaryExpr) (flow, error) {
	l, err := x.walk(b.LHS)
	if err != nil {
		return l, err
	}
	r, err := x.walk(b.RHS)
	if err != nil {
		return r, err
	}
	keepName := b.Op.IsComparisonOperator() && !b.ReturnBool
	switch {
	case l.scalar && r.scalar:
		return flow{scalar: true}, nil
	case r.scalar:
		if keepName {
			return l, nil
		}
		return withoutName(l), nil
	case l.scalar:
		if keepName {
			return r, nil
		}
		return withoutName(r), nil
	}

	vm := b.VectorMatching
	if vm == nil {
		vm = &parser.VectorMatching{Card: parser.CardOneToOne}
	}
	sig := func(ls labels.Labels) string {
		bl := labels.NewBuilder(ls)
		if vm.On {
			bl.Keep(vm.MatchingLabels...)
		} else {
			bl.Del(append([]string{labels.MetricName}, vm.MatchingLabels...)...)
		}
		return bl.Labels().String()
	}
	clause := "on(" + strings.Join(vm.MatchingLabels, ", ") + ")"
	if !vm.On {
		clause = "ignoring(" + strings.Join(vm.MatchingLabels, ", ") + ")"
	}
	rightBy := map[string][]item{}
	for _, it := range r.items {
		rightBy[sig(it.lset)] = append(rightBy[sig(it.lset)], it)
	}
	leftBy := map[string][]item{}
	for _, it := range l.items {
		leftBy[sig(it.lset)] = append(leftBy[sig(it.lset)], it)
	}
	unmatched := func(it item, side, op string) {
		for _, p := range it.parents {
			x.drop(Drop{Input: p, Clause: op, Reason: fmt.Sprintf("no series on the %s side of %s matches it %s", side, op, clause)})
		}
	}

	switch b.Op {
	case parser.LAND:
		var out []item
		for _, it := range l.items {
			m := rightBy[sig(it.lset)]
			if len(m) == 0 {
				unmatched(it, "right", "and")
				continue
			}
			out = append(out, item{lset: it.lset, parents: union(it.parents, parentsOf(m))})
		}
		return flow{items: out}, nil
	case parser.LUNLESS:
		var out []item
		for _, it := range l.items {
			if m := rightBy[sig(it.lset)]; len(m) > 0 {
				for _, p := range it.parents {
					x.drop(Drop{Input: p, Clause: "unless", Reason: "a series on the right side of unless matches it " + clause})
				}
				continue
			}
			out = append(out, it)
		}
		return flow{items: out}, nil
	case parser.LOR:
		out := append([]item(nil), l.items...)
		for _, it := range r.items {
			if len(leftBy[sig(it.lset)]) == 0 {
				out = append(out, it)
			}
		}
		return flow{items: out}, nil
	}

	// Arithmetic and comparison between vectors.
	// For each series on the "many" side, find its match on the "one" side.
	many, oneBy := l, rightBy
	if vm.Card == parser.CardOneToMany {
		many, oneBy = r, leftBy
	}
	var out []item
	for _, it := range many.items {
		m := oneBy[sig(it.lset)]
		if len(m) == 0 {
			side := "right"
			if vm.Card == parser.CardOneToMany {
				side = "left"
			}
			unmatched(it, side, b.Op.String())
			continue
		}
		for _, o := range m {
			bl := labels.NewBuilder(it.lset)
			if !keepName {
				bl.Del(labels.MetricName)
			}
			switch vm.Card {
			case parser.CardOneToOne:
				if vm.On {
					bl.Keep(vm.MatchingLabels...)
				} else {
					bl.Del(vm.MatchingLabels...)
				}
			case parser.CardManyToOne, parser.CardOneToMany:
				for _, inc := range vm.Include {
					if v := o.lset.Get(inc); v != "" {
						bl.Set(inc, v)
					} else {
						bl.Del(inc)
					}
				}
			default:
				return flow{}, unsupported("many-to-many matching")
			}
			out = append(out, item{lset: bl.Labels(), parents: union(it.parents, o.parents)})
		}
	}
	return flow{items: merge(out)}, nil
}

func unwrap(e parser.Expr) parser.Expr {
	for {
		switch n := e.(type) {
		case *parser.ParenExpr:
			e = n.Expr
		case *parser.StepInvariantExpr:
			e = n.Expr
		default:
			return e
		}
	}
}

func withoutName(f flow) flow {
	out := make([]item, len(f.items))
	for i, it := range f.items {
		out[i] = item{lset: labels.NewBuilder(it.lset).Del(labels.MetricName).Labels(), parents: it.parents}
	}
	return flow{items: merge(out)}
}

// merge combines items with identical labels, keeping first-seen order.
func merge(items []item) []item {
	var out []item
	idx := map[string]int{}
	for _, it := range items {
		k := it.lset.String()
		if i, ok := idx[k]; ok {
			out[i].parents = union(out[i].parents, it.parents)
			continue
		}
		idx[k] = len(out)
		out = append(out, item{lset: it.lset, parents: append([]int(nil), it.parents...)})
	}
	return out
}

func mergeGroups(items []item) []Group {
	m := merge(items)
	out := make([]Group, len(m))
	for i, it := range m {
		out[i] = Group{Labels: it.lset, Parents: it.parents}
	}
	return out
}

func parentsOf(items []item) []int {
	var out []int
	for _, it := range items {
		out = append(out, it.parents...)
	}
	return out
}

// union returns the sorted, deduplicated union of a and b.
func union(a, b []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, v := range append(append([]int(nil), a...), b...) {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Ints(out)
	return out
}

// Selectors returns the vector selectors in expr.
func Selectors(expr parser.Expr) []*parser.VectorSelector {
	var out []*parser.VectorSelector
	parser.Inspect(expr, func(n parser.Node, _ []parser.Node) error {
		if vs, ok := n.(*parser.VectorSelector); ok {
			out = append(out, vs)
		}
		return nil
	})
	return out
}

// UsesRange reports whether expr has range vectors (rate() and friends).
func UsesRange(expr parser.Expr) bool {
	found := false
	parser.Inspect(expr, func(n parser.Node, _ []parser.Node) error {
		switch n.(type) {
		case *parser.MatrixSelector, *parser.SubqueryExpr:
			found = true
		}
		return nil
	})
	return found
}

// FiltersByValue reports whether expr drops series depending on their values:
// comparisons without bool, or topk-like aggregations.
func FiltersByValue(expr parser.Expr) bool {
	found := false
	parser.Inspect(expr, func(n parser.Node, _ []parser.Node) error {
		switch e := n.(type) {
		case *parser.BinaryExpr:
			if e.Op.IsComparisonOperator() && !e.ReturnBool {
				found = true
			}
		case *parser.AggregateExpr:
			switch e.Op {
			case parser.TOPK, parser.BOTTOMK, parser.LIMITK, parser.LIMIT_RATIO:
				found = true
			}
		}
		return nil
	})
	return found
}
