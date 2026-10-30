package main

import (
	"encoding/json"
	"errors"
	"strings"

	pgq "github.com/pganalyze/pg_query_go/v6"
	pgparser "github.com/wasilibs/go-pgquery"
)

// pgPlanNode is a node of EXPLAIN (VERBOSE, FORMAT JSON) output.
type pgPlanNode struct {
	NodeType     string          `json:"Node Type"`
	JoinType     string          `json:"Join Type"`
	Relationship string          `json:"Parent Relationship"`
	Alias        string          `json:"Alias"`
	Relation     string          `json:"Relation Name"`
	Schema       string          `json:"Schema"`
	Strategy     string          `json:"Strategy"`
	GroupingSets json.RawMessage `json:"Grouping Sets"`
	Output       []string        `json:"Output"`
	Plans        []*pgPlanNode   `json:"Plans"`
}

// pgPlan is what a generic query plan says about where result columns
// come from. With VERBOSE, the plan's top node lists the expression
// computing each result column, with views and subqueries flattened and
// columns qualified by the alias of the table scan that reads them
// (unqualified when the plan reads just one table).
type pgPlan struct {
	// outputs holds the result column expressions: those of the top node,
	// or, for UNION ALL and partitioned tables, of each member of the
	// top-level Append.
	outputs [][]string
	scans   map[string]*pgScan // table scans by alias
	// single is the alias of the only table the main plan reads, if so.
	single string

	outerJoin    bool // has outer joins
	groupingSets bool // has ROLLUP, CUBE or GROUPING SETS
	setOp        bool // combines rows of different sources below the top
	plainAgg     bool // aggregates without GROUP BY, which can see no rows
}

type pgScan struct {
	schema, table string
	nullable      bool // on the nullable side of an outer join
	ambiguous     bool // the alias names more than one table
}

func parsePGPlan(js []byte) (*pgPlan, error) {
	var out []struct {
		Plan *pgPlanNode `json:"Plan"`
	}
	if err := json.Unmarshal(js, &out); err != nil {
		return nil, err
	}
	if len(out) != 1 || out[0].Plan == nil {
		return nil, errors.New("unexpected plan")
	}
	root := out[0].Plan
	p := &pgPlan{scans: map[string]*pgScan{}}
	memberwise := root.Output == nil && isSetNode(root)
	if memberwise {
		for _, m := range root.Plans {
			if m.Relationship == "Member" {
				p.outputs = append(p.outputs, m.Output)
			}
		}
	} else if root.Output != nil {
		p.outputs = [][]string{root.Output}
	}
	main := map[string]bool{}
	p.walk(root, false, false, memberwise, main)
	if len(main) == 1 {
		for alias := range main {
			if s := p.scans[alias]; s != nil && !s.ambiguous {
				p.single = alias
			}
		}
	}
	return p, nil
}

func isSetNode(n *pgPlanNode) bool {
	switch n.NodeType {
	case "Append", "Merge Append", "SetOp", "Recursive Union":
		return true
	}
	return false
}

// walk records the plan's table scans and the features that can make
// columns null. Subplans (InitPlan, SubPlan) only feed expressions, so
// their tables are not part of the main plan.
func (p *pgPlan) walk(n *pgPlanNode, nullable, sub, top bool, main map[string]bool) {
	if n.Relationship == "InitPlan" || n.Relationship == "SubPlan" {
		sub, nullable = true, false
	}
	if n.Alias != "" {
		if !sub {
			main[n.Alias] = true
		}
		if n.Relation != "" {
			s := p.scans[n.Alias]
			if s == nil {
				s = &pgScan{schema: n.Schema, table: n.Relation}
				p.scans[n.Alias] = s
			} else if s.schema != n.Schema || s.table != n.Relation {
				s.ambiguous = true
			}
			s.nullable = s.nullable || nullable
		}
	}
	switch n.JoinType {
	case "Left", "Right", "Full":
		p.outerJoin = true
	}
	if len(n.GroupingSets) > 0 && string(n.GroupingSets) != "null" {
		p.groupingSets = true
	}
	if !sub {
		if n.NodeType == "Aggregate" && n.Strategy == "Plain" {
			p.plainAgg = true
		}
		if isSetNode(n) && !top {
			p.setOp = true
		}
	}
	for _, c := range n.Plans {
		cn := nullable
		switch {
		case n.JoinType == "Left" && c.Relationship == "Inner",
			n.JoinType == "Right" && c.Relationship == "Outer",
			n.JoinType == "Full":
			cn = true
		}
		p.walk(c, cn, sub, false, main)
	}
}

// attNotNull reports whether a table column is declared NOT NULL.
type attNotNull func(schema, table, column string) (bool, error)

// notNull reports whether result column i is never null. known is false
// when the plan doesn't say, e.g. for a column read from a CTE.
func (p *pgPlan) notNull(i int, att attNotNull) (notNull, known bool, err error) {
	if len(p.outputs) == 0 {
		return false, false, nil
	}
	notNull = true
	for _, out := range p.outputs {
		if i >= len(out) {
			return false, false, nil
		}
		n, err := parsePGExpr(out[i])
		if err != nil {
			return false, false, nil
		}
		e := &pgEval{plan: p, att: att}
		var v, k bool
		if ref := n.GetColumnRef(); ref != nil {
			// A bare column the plan can't place is left to the caller.
			v, k = e.columnRef(ref)
		} else {
			v, k = e.notNull(n), true
		}
		if e.err != nil {
			return false, false, e.err
		}
		if !k {
			return false, false, nil
		}
		notNull = notNull && v
	}
	return notNull, true, nil
}

// parsePGExpr parses an expression as EXPLAIN prints it. Before
// PostgreSQL 16, window definitions are printed as "OVER (?)".
func parsePGExpr(expr string) (*pgq.Node, error) {
	expr = strings.ReplaceAll(expr, " OVER (?)", " OVER ()")
	res, err := pgparser.Parse("SELECT " + expr)
	if err != nil {
		return nil, err
	}
	if len(res.Stmts) != 1 {
		return nil, errors.New("not an expression")
	}
	sel := res.Stmts[0].Stmt.GetSelectStmt()
	if sel == nil || len(sel.TargetList) != 1 || sel.TargetList[0].GetResTarget() == nil {
		return nil, errors.New("not an expression")
	}
	return sel.TargetList[0].GetResTarget().Val, nil
}

// pgEval decides whether expressions are never null. Anything it doesn't
// recognise may be null.
type pgEval struct {
	plan *pgPlan
	att  attNotNull
	err  error
}

func (e *pgEval) columnRef(ref *pgq.ColumnRef) (notNull, known bool) {
	var names []string
	for _, f := range ref.Fields {
		s := f.GetString_()
		if s == nil {
			return false, false
		}
		names = append(names, s.Sval)
	}
	var alias, column string
	switch len(names) {
	case 1:
		alias, column = e.plan.single, names[0]
	case 2:
		alias, column = names[0], names[1]
	default:
		return false, false
	}
	s := e.plan.scans[alias]
	if s == nil || s.ambiguous || e.plan.setOp {
		return false, false
	}
	if s.nullable || e.plan.groupingSets {
		return false, true
	}
	v, err := e.att(s.schema, s.table, column)
	if err != nil && e.err == nil {
		e.err = err
	}
	return v, true
}

func (e *pgEval) notNull(n *pgq.Node) bool {
	if n == nil {
		return false
	}
	switch {
	case n.GetColumnRef() != nil:
		v, known := e.columnRef(n.GetColumnRef())
		return known && v
	case n.GetAConst() != nil:
		return !n.GetAConst().GetIsnull()
	case n.GetTypeCast() != nil:
		return e.notNull(n.GetTypeCast().Arg)
	case n.GetCollateClause() != nil:
		return e.notNull(n.GetCollateClause().Arg)
	case n.GetNullTest() != nil, n.GetBooleanTest() != nil, n.GetAArrayExpr() != nil, n.GetRowExpr() != nil:
		return true
	case n.GetBoolExpr() != nil:
		return e.all(n.GetBoolExpr().Args)
	case n.GetCoalesceExpr() != nil:
		return e.any(n.GetCoalesceExpr().Args)
	case n.GetMinMaxExpr() != nil:
		// GREATEST and LEAST ignore nulls.
		return e.any(n.GetMinMaxExpr().Args)
	case n.GetCaseExpr() != nil:
		c := n.GetCaseExpr()
		if !e.notNull(c.GetDefresult()) {
			return false
		}
		for _, w := range c.Args {
			if !e.notNull(w.GetCaseWhen().GetResult()) {
				return false
			}
		}
		return true
	case n.GetAExpr() != nil:
		return e.aExpr(n.GetAExpr())
	case n.GetFuncCall() != nil:
		return e.funcCall(n.GetFuncCall())
	case n.GetSqlvalueFunction() != nil:
		return n.GetSqlvalueFunction().GetOp() != pgq.SQLValueFunctionOp_SVFOP_CURRENT_SCHEMA
	case n.GetSubLink() != nil:
		return n.GetSubLink().GetSubLinkType() == pgq.SubLinkType_EXISTS_SUBLINK
	case n.GetList() != nil:
		return e.all(n.GetList().Items)
	}
	return false
}

func (e *pgEval) all(ns []*pgq.Node) bool {
	for _, n := range ns {
		if !e.notNull(n) {
			return false
		}
	}
	return true
}

func (e *pgEval) any(ns []*pgq.Node) bool {
	for _, n := range ns {
		if e.notNull(n) {
			return true
		}
	}
	return false
}

// strictOperators return a non-null result for non-null operands.
var strictOperators = setOf("+", "-", "*", "/", "%", "^", "||", "=", "<>", "!=", "<", ">", "<=", ">=",
	"~~", "!~~", "~~*", "!~~*", "~", "!~", "~*", "!~*", "@", "|/", "&", "|", "#", "<<", ">>", "@>", "<@", "&&")

func (e *pgEval) aExpr(a *pgq.A_Expr) bool {
	switch a.Kind {
	case pgq.A_Expr_Kind_AEXPR_DISTINCT, pgq.A_Expr_Kind_AEXPR_NOT_DISTINCT:
		return true
	case pgq.A_Expr_Kind_AEXPR_OP:
		if len(a.Name) != 1 || a.Name[0].GetString_() == nil || !strictOperators[a.Name[0].GetString_().Sval] {
			return false
		}
		return (a.Lexpr == nil || e.notNull(a.Lexpr)) && e.notNull(a.Rexpr)
	case pgq.A_Expr_Kind_AEXPR_IN, pgq.A_Expr_Kind_AEXPR_LIKE, pgq.A_Expr_Kind_AEXPR_ILIKE,
		pgq.A_Expr_Kind_AEXPR_BETWEEN, pgq.A_Expr_Kind_AEXPR_NOT_BETWEEN,
		pgq.A_Expr_Kind_AEXPR_BETWEEN_SYM, pgq.A_Expr_Kind_AEXPR_NOT_BETWEEN_SYM:
		return e.notNull(a.Lexpr) && e.notNull(a.Rexpr)
	}
	return false
}

var (
	// Window functions that are never null.
	pgWindowNotNull = setOf("row_number", "rank", "dense_rank", "percent_rank", "cume_dist", "ntile", "count")
	// Aggregates that are null over no rows. Those in pgCollectionAggs
	// are otherwise never null; the others are when their arguments aren't.
	pgAggregates = setOf("sum", "min", "max", "avg", "bool_and", "bool_or", "every", "bit_and", "bit_or", "bit_xor",
		"string_agg", "array_agg", "json_agg", "jsonb_agg", "json_object_agg", "jsonb_object_agg", "xmlagg")
	pgCollectionAggs = setOf("array_agg", "json_agg", "jsonb_agg", "json_object_agg", "jsonb_object_agg")
	// Functions that are never null.
	pgFuncNotNull = setOf("now", "clock_timestamp", "statement_timestamp", "transaction_timestamp", "timeofday",
		"random", "gen_random_uuid", "concat", "json_build_object", "jsonb_build_object", "json_build_array",
		"jsonb_build_array", "pg_backend_pid", "version")
	// Functions that are never null when their arguments aren't.
	pgFuncStrict = setOf("upper", "lower", "length", "char_length", "character_length", "octet_length", "bit_length",
		"btrim", "ltrim", "rtrim", "substr", "replace", "translate", "lpad", "rpad", "left", "right", "repeat",
		"reverse", "initcap", "md5", "ascii", "chr", "strpos", "split_part", "starts_with", "concat_ws", "format",
		"quote_ident", "quote_literal", "to_hex", "regexp_replace", "string_to_array", "cardinality",
		"abs", "ceil", "ceiling", "floor", "round", "trunc", "sign", "sqrt", "cbrt", "exp", "ln", "log", "log10",
		"power", "pow", "mod", "div", "degrees", "radians",
		"date_trunc", "date_part", "extract", "to_char", "to_number", "to_date", "to_timestamp", "age", "timezone",
		"justify_days", "justify_hours", "justify_interval", "make_date", "make_time", "make_timestamp",
		"make_timestamptz", "make_interval", "to_json", "to_jsonb", "row_to_json")
)

func (e *pgEval) funcCall(f *pgq.FuncCall) bool {
	name := ""
	if n := len(f.Funcname); n > 0 && f.Funcname[n-1].GetString_() != nil {
		name = strings.ToLower(f.Funcname[n-1].GetString_().Sval)
	}
	switch {
	case f.Over != nil:
		return pgWindowNotNull[name]
	case f.AggStar || name == "count":
		return true
	case pgAggregates[name]:
		// Each group has rows; without GROUP BY, or with a FILTER, there
		// may be none.
		if e.plan.plainAgg || e.plan.groupingSets || f.AggFilter != nil {
			return false
		}
		return pgCollectionAggs[name] || e.all(f.Args)
	case pgFuncNotNull[name]:
		return true
	case pgFuncStrict[name]:
		return e.all(f.Args)
	}
	return false
}

func setOf(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}
