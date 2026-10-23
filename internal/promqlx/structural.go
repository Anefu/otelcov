package promqlx

import "github.com/prometheus/prometheus/promql/parser"

// Structural rewrites expr so that which outputs exist no longer depends on
// values:
//
//   - comparisons become bool comparisons (x > 12e9 becomes x > bool 12e9), so
//     every series that could pass the comparison is kept, with value 0 or 1;
//   - topk, bottomk, limitk and limit_ratio are replaced by their input, since
//     any series could be picked.
//
// Coverage asks whether a series could reach an alert, not whether it does at
// the recorded values. Output values of the rewritten expression are not the
// real values. The input expression is not modified.
func Structural(expr parser.Expr) (parser.Expr, error) {
	cp, err := parser.ParseExpr(expr.String()) // deep copy
	if err != nil {
		return nil, err
	}
	return rewrite(cp), nil
}

func rewrite(e parser.Expr) parser.Expr {
	switch n := e.(type) {
	case *parser.ParenExpr:
		n.Expr = rewrite(n.Expr)
	case *parser.StepInvariantExpr:
		n.Expr = rewrite(n.Expr)
	case *parser.SubqueryExpr:
		n.Expr = rewrite(n.Expr)
	case *parser.UnaryExpr:
		n.Expr = rewrite(n.Expr)
	case *parser.Call:
		for i := range n.Args {
			n.Args[i] = rewrite(n.Args[i])
		}
	case *parser.AggregateExpr:
		n.Expr = rewrite(n.Expr)
		switch n.Op {
		case parser.TOPK, parser.BOTTOMK, parser.LIMITK, parser.LIMIT_RATIO:
			return n.Expr
		}
	case *parser.BinaryExpr:
		n.LHS, n.RHS = rewrite(n.LHS), rewrite(n.RHS)
		if n.Op.IsComparisonOperator() {
			n.ReturnBool = true
		}
	}
	return e
}
