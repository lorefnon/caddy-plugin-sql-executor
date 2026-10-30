package main

import (
	"context"
	"database/sql"
	"slices"
	"sort"
	"strings"

	rq "github.com/rqlite/sql"
)

// sqliteAnalyzer parses a statement and resolves its columns against the
// schema, to work out what SQLite doesn't report: the types of
// expressions and parameters, and which result columns can be null. It
// only claims what it understands; anything else is left as unknown.
type sqliteAnalyzer struct {
	ctx    context.Context
	conn   *sql.Conn
	tables map[string][]sqliteColumn // by lower-case schema.table
	// outerJoins is false when the query plan has no outer joins, i.e.
	// SQLite turned any it had into inner joins.
	outerJoins bool

	binds  map[int]int // placeholder index by offset in the statement
	params []*Schema
	err    error
}

type sqliteColumn struct {
	name, decltype string
	notNull        bool
}

// sqliteValue is what is known about an expression.
type sqliteValue struct {
	schema  *Schema // nil if unknown
	notNull bool
	// known is false when the analyzer can't tell anything, e.g. for a
	// column of a subquery.
	known bool
}

type sqliteAnalysis struct {
	columns []sqliteValue // nil if the result columns couldn't be resolved
	params  []*Schema     // nil if the placeholders couldn't be numbered
	// mayAddNulls is set when the statement can produce nulls from NOT NULL
	// columns in ways not tracked per column: aggregates without GROUP BY
	// and compound selects. (Outer joins show in the query plan.)
	mayAddNulls bool
}

// sqliteSource is a table or subquery in a FROM clause.
type sqliteSource struct {
	name     string         // lower-case alias or table name
	columns  []sqliteColumn // nil if unknown (subquery, CTE, table function)
	nullable bool           // on the nullable side of an outer join
}

type sqliteScope struct {
	sources []*sqliteSource
	parent  *sqliteScope
	ctes    map[string]bool
	// usingJoin is set if sources are joined with USING or NATURAL, which
	// changes what * expands to.
	usingJoin bool
	// aggregate is set for a select with aggregates but no GROUP BY, which
	// returns a row (of nulls, for plain columns) even for no input rows.
	aggregate bool
	grouped   bool
}

var unknown = sqliteValue{}

func (a *sqliteAnalyzer) analyze(query string, nparams int) (*sqliteAnalysis, error) {
	stmt, err := rq.NewParser(strings.NewReader(query)).ParseStatement()
	if err != nil {
		// Not something the parser understands; rely on SQLite alone.
		return nil, nil
	}
	res := &sqliteAnalysis{}
	var offsets []int
	inspect(stmt, func(n rq.Node) bool {
		switch n := n.(type) {
		case *rq.BindExpr:
			offsets = append(offsets, n.NamePos.Offset)
		case *rq.SelectStatement:
			if n.Compound != nil || (len(n.GroupByExprs) == 0 && hasAggregate(n)) {
				res.mayAddNulls = true
			}
		}
		return true
	})
	if len(offsets) == nparams {
		sort.Ints(offsets)
		a.binds = map[int]int{}
		for i, off := range offsets {
			a.binds[off] = i
		}
		a.params = make([]*Schema, nparams)
	}

	var cols []sqliteValue
	ok := false
	switch s := stmt.(type) {
	case *rq.SelectStatement:
		cols, ok = a.selectColumns(s, nil)
	case *rq.InsertStatement:
		cols, ok = a.insert(s)
	case *rq.UpdateStatement:
		cols, ok = a.update(s)
	case *rq.DeleteStatement:
		cols, ok = a.delete(s)
	}
	if a.err != nil {
		return nil, a.err
	}
	if ok {
		res.columns = cols
	}
	res.params = a.params
	return res, nil
}

// inspect calls f for each node of the tree, descending while f returns true.
func inspect(n rq.Node, f func(rq.Node) bool) {
	rq.Walk(inspector(f), n)
}

type inspector func(rq.Node) bool

func (f inspector) Visit(n rq.Node) (rq.Visitor, rq.Node, error) {
	if f(n) {
		return f, n, nil
	}
	return nil, n, nil
}

func (f inspector) VisitEnd(n rq.Node) (rq.Node, error) { return n, nil }

// hasAggregate reports whether a select computes aggregates.
func hasAggregate(s *rq.SelectStatement) bool {
	found := false
	visit := func(n rq.Node) bool {
		switch n := n.(type) {
		case *rq.SelectStatement, rq.SelectExpr, *rq.Exists:
			return false // subqueries aggregate on their own
		case *rq.Call:
			if n.Over == nil && isAggregate(n) {
				found = true
			}
		}
		return !found
	}
	for _, c := range s.Columns {
		if c.Expr != nil {
			inspect(c.Expr, visit)
		}
	}
	if s.HavingExpr != nil {
		inspect(s.HavingExpr, visit)
	}
	return found
}

func isAggregate(c *rq.Call) bool {
	switch strings.ToLower(c.Name.Name) {
	case "count", "sum", "total", "avg", "group_concat", "string_agg":
		return true
	case "min", "max":
		return len(c.Args) == 1
	}
	return false
}

func (a *sqliteAnalyzer) selectColumns(s *rq.SelectStatement, parent *sqliteScope) ([]sqliteValue, bool) {
	sc := &sqliteScope{parent: parent, ctes: map[string]bool{}}
	if s.WithClause != nil {
		for _, cte := range s.WithClause.CTEs {
			sc.ctes[strings.ToLower(cte.TableName.Name)] = true
		}
		for _, cte := range s.WithClause.CTEs {
			a.selectColumns(cte.Select, sc)
		}
	}
	if len(s.ValueLists) > 0 {
		for _, l := range s.ValueLists {
			for _, e := range l.Exprs {
				a.eval(e, sc)
			}
		}
		return nil, false
	}
	ons := a.addSources(s.Source, sc, false)
	sc.grouped = len(s.GroupByExprs) > 0
	sc.aggregate = !sc.grouped && hasAggregate(s)
	for _, on := range ons {
		a.eval(on, sc)
	}
	a.eval(s.WhereExpr, sc)
	for _, g := range s.GroupByExprs {
		a.eval(g, sc)
	}
	a.eval(s.HavingExpr, sc)
	cols, ok := a.resultColumns(s.Columns, sc)
	for _, t := range s.OrderingTerms {
		a.eval(t.X, sc)
	}
	a.limit(s.LimitExpr, sc)
	a.limit(s.OffsetExpr, sc)
	if s.Compound != nil {
		a.selectColumns(s.Compound, parent)
		return nil, false
	}
	return cols, ok
}

func (a *sqliteAnalyzer) limit(e rq.Expr, sc *sqliteScope) {
	if e != nil {
		a.eval(e, sc)
		a.bind(e, typed("integer", ""))
	}
}

// addSources adds the tables of a FROM clause to the scope, returning the
// join constraints to evaluate once all are in scope.
func (a *sqliteAnalyzer) addSources(src rq.Source, sc *sqliteScope, nullable bool) []rq.Expr {
	switch s := src.(type) {
	case *rq.QualifiedTableName:
		name := s.Name.Name
		alias := name
		if s.Alias != nil {
			alias = s.Alias.Name
		}
		item := &sqliteSource{name: strings.ToLower(alias), nullable: nullable}
		if s.Schema != nil || !sc.isCTE(name) {
			item.columns = a.tableColumns(s.Schema, name)
		}
		sc.sources = append(sc.sources, item)
	case *rq.QualifiedTableFunctionName:
		for _, arg := range s.Args {
			a.eval(arg, sc)
		}
		alias := s.Name.Name
		if s.Alias != nil {
			alias = s.Alias.Name
		}
		sc.sources = append(sc.sources, &sqliteSource{name: strings.ToLower(alias), nullable: nullable})
	case *rq.ParenSource:
		if sel, ok := s.X.(*rq.SelectStatement); ok {
			a.selectColumns(sel, sc.parent)
			name := ""
			if s.Alias != nil {
				name = strings.ToLower(s.Alias.Name)
			}
			sc.sources = append(sc.sources, &sqliteSource{name: name, nullable: nullable})
			return nil
		}
		return a.addSources(s.X, sc, nullable)
	case *rq.SelectStatement:
		a.selectColumns(s, sc.parent)
		sc.sources = append(sc.sources, &sqliteSource{nullable: nullable})
	case *rq.JoinClause:
		left, right := nullable, nullable
		if op := s.Operator; op != nil && a.outerJoins {
			left = left || op.Right.IsValid() || op.Full.IsValid()
			right = right || op.Left.IsValid() || op.Full.IsValid()
		}
		if op := s.Operator; op != nil && op.Natural.IsValid() {
			sc.usingJoin = true
		}
		ons := a.addSources(s.X, sc, left)
		ons = append(ons, a.addSources(s.Y, sc, right)...)
		switch c := s.Constraint.(type) {
		case *rq.OnConstraint:
			ons = append(ons, c.X)
		case *rq.UsingConstraint:
			sc.usingJoin = true
		}
		return ons
	}
	return nil
}

func (sc *sqliteScope) isCTE(name string) bool {
	for s := sc; s != nil; s = s.parent {
		if s.ctes[strings.ToLower(name)] {
			return true
		}
	}
	return false
}

func (sc *sqliteScope) source(name string) *sqliteSource {
	name = strings.ToLower(name)
	for _, s := range sc.sources {
		if s.name == name {
			return s
		}
	}
	return nil
}

// resultColumns resolves the columns of a SELECT or RETURNING clause. ok is
// false if a * can't be expanded.
func (a *sqliteAnalyzer) resultColumns(cols []*rq.ResultColumn, sc *sqliteScope) ([]sqliteValue, bool) {
	var out []sqliteValue
	ok := true
	expand := func(src *sqliteSource) {
		if src == nil || src.columns == nil {
			ok = false
			return
		}
		for _, c := range src.columns {
			out = append(out, columnValue(src, c, sc))
		}
	}
	for _, c := range cols {
		if c.Star.IsValid() {
			if sc.usingJoin {
				ok = false
			}
			for _, src := range sc.sources {
				expand(src)
			}
			continue
		}
		if ref, isRef := c.Expr.(*rq.QualifiedRef); isRef && ref.Star.IsValid() {
			expand(sc.source(ref.Table.Name))
			continue
		}
		out = append(out, a.eval(c.Expr, sc))
	}
	return out, ok
}

func columnValue(src *sqliteSource, c sqliteColumn, sc *sqliteScope) sqliteValue {
	return sqliteValue{
		schema:  sqliteColumnSchema(c.decltype),
		notNull: c.notNull && !src.nullable && !sc.aggregate,
		known:   true,
	}
}

// ref resolves a column reference, looking in enclosing scopes for
// correlated subqueries.
func (a *sqliteAnalyzer) ref(sc *sqliteScope, table, column string) sqliteValue {
	column = strings.ToLower(column)
	for s := sc; s != nil; s = s.parent {
		if table != "" {
			src := s.source(table)
			if src == nil {
				continue
			}
			if c, ok := findColumn(src.columns, column); ok {
				return columnValue(src, c, s)
			}
			return unknown
		}
		var (
			match  *sqliteSource
			col    sqliteColumn
			count  int
			opaque bool
		)
		for _, src := range s.sources {
			if src.columns == nil {
				opaque = true
			} else if c, ok := findColumn(src.columns, column); ok {
				match, col = src, c
				count++
			}
		}
		switch {
		case opaque || count > 1:
			return unknown
		case count == 1:
			return columnValue(match, col, s)
		}
	}
	return unknown
}

func findColumn(cols []sqliteColumn, name string) (sqliteColumn, bool) {
	for _, c := range cols {
		if strings.ToLower(c.name) == name {
			return c, true
		}
	}
	return sqliteColumn{}, false
}

// tableColumns returns a table's columns, or nil if there is no such table.
func (a *sqliteAnalyzer) tableColumns(schema *rq.Ident, table string) []sqliteColumn {
	db := "main"
	if schema != nil {
		db = schema.Name
	}
	key := strings.ToLower(db + "." + table)
	if cols, ok := a.tables[key]; ok {
		return cols
	}
	rows, err := a.conn.QueryContext(a.ctx, `
		SELECT name, type, "notnull" OR (pk = 1 AND upper(type) = 'INTEGER'
			AND (SELECT count(*) FROM pragma_table_info(?1, ?2) WHERE pk > 0) = 1)
		FROM pragma_table_info(?1, ?2) ORDER BY cid`, table, db)
	if err != nil {
		a.fail(err)
		return nil
	}
	defer rows.Close()
	var cols []sqliteColumn
	for rows.Next() {
		var c sqliteColumn
		if err := rows.Scan(&c.name, &c.decltype, &c.notNull); err != nil {
			a.fail(err)
			return nil
		}
		cols = append(cols, c)
	}
	a.fail(rows.Err())
	a.tables[key] = cols
	return cols
}

func (a *sqliteAnalyzer) fail(err error) {
	if err != nil && a.err == nil {
		a.err = err
	}
}

// bind records the type of a placeholder: e, if it is one, takes values
// of type s.
func (a *sqliteAnalyzer) bind(e rq.Expr, s *Schema) {
	for {
		p, ok := e.(*rq.ParenExpr)
		if !ok {
			break
		}
		e = p.X
	}
	b, ok := e.(*rq.BindExpr)
	if !ok || s == nil || a.binds == nil {
		return
	}
	i, ok := a.binds[b.NamePos.Offset]
	if !ok {
		return
	}
	c := *s
	if ts := c.types(); len(ts) > 0 {
		if ts = withoutNull(ts); len(ts) == 0 {
			return // compared with NULL: says nothing
		}
		c.setTypes(ts)
		if c.Enum != nil {
			c.Enum = slices.DeleteFunc(slices.Clone(c.Enum), func(v any) bool { return v == nil })
		}
	}
	a.params[i] = unionItems(a.params[i], &c)
}

func (a *sqliteAnalyzer) insert(s *rq.InsertStatement) ([]sqliteValue, bool) {
	sc := &sqliteScope{ctes: map[string]bool{}}
	if s.WithClause != nil {
		for _, cte := range s.WithClause.CTEs {
			sc.ctes[strings.ToLower(cte.TableName.Name)] = true
			a.selectColumns(cte.Select, sc)
		}
	}
	cols := a.tableColumns(s.Schema, s.Table.Name)
	// The columns values are given for, in order.
	targets := cols
	if len(s.Columns) > 0 {
		targets = nil
		for _, id := range s.Columns {
			c, _ := findColumn(cols, strings.ToLower(id.Name))
			targets = append(targets, c)
		}
	}
	empty := &sqliteScope{parent: sc}
	for _, l := range s.ValueLists {
		for j, e := range l.Exprs {
			a.eval(e, empty)
			if j < len(targets) {
				a.bind(e, sqliteColumnSchema(targets[j].decltype))
			}
		}
	}
	if s.Select != nil {
		a.selectColumns(s.Select, sc)
		if s.Select.Compound == nil {
			for j, rc := range s.Select.Columns {
				if j < len(targets) && rc.Expr != nil {
					a.bind(rc.Expr, sqliteColumnSchema(targets[j].decltype))
				}
			}
		}
	}

	name := s.Table.Name
	if s.Alias != nil {
		name = s.Alias.Name
	}
	target := &sqliteSource{name: strings.ToLower(name), columns: cols}
	for _, u := range s.UpsertClauses {
		usc := &sqliteScope{parent: sc, sources: []*sqliteSource{target, {name: "excluded", columns: cols}}}
		a.eval(u.WhereExpr, usc)
		a.assign(u.Assignments, cols, usc)
		a.eval(u.UpdateWhereExpr, usc)
	}
	return a.returning(s.ReturningClause, &sqliteScope{parent: sc, sources: []*sqliteSource{target}})
}

func (a *sqliteAnalyzer) update(s *rq.UpdateStatement) ([]sqliteValue, bool) {
	sc := &sqliteScope{ctes: map[string]bool{}}
	if s.WithClause != nil {
		for _, cte := range s.WithClause.CTEs {
			sc.ctes[strings.ToLower(cte.TableName.Name)] = true
			a.selectColumns(cte.Select, sc)
		}
	}
	cols, tsc := a.targetScope(s.Table, sc)
	ons := a.addSources(s.Source, tsc, false)
	for _, on := range ons {
		a.eval(on, tsc)
	}
	a.assign(s.Assignments, cols, tsc)
	a.eval(s.WhereExpr, tsc)
	return a.returning(s.ReturningClause, tsc)
}

func (a *sqliteAnalyzer) delete(s *rq.DeleteStatement) ([]sqliteValue, bool) {
	sc := &sqliteScope{ctes: map[string]bool{}}
	if s.WithClause != nil {
		for _, cte := range s.WithClause.CTEs {
			sc.ctes[strings.ToLower(cte.TableName.Name)] = true
			a.selectColumns(cte.Select, sc)
		}
	}
	_, tsc := a.targetScope(s.Table, sc)
	a.eval(s.WhereExpr, tsc)
	for _, t := range s.OrderingTerms {
		a.eval(t.X, tsc)
	}
	a.limit(s.LimitExpr, tsc)
	a.limit(s.OffsetExpr, tsc)
	return a.returning(s.ReturningClause, tsc)
}

// targetScope is the scope of an UPDATE or DELETE: its table.
func (a *sqliteAnalyzer) targetScope(t *rq.QualifiedTableName, parent *sqliteScope) ([]sqliteColumn, *sqliteScope) {
	cols := a.tableColumns(t.Schema, t.Name.Name)
	name := t.Name.Name
	if t.Alias != nil {
		name = t.Alias.Name
	}
	return cols, &sqliteScope{parent: parent, sources: []*sqliteSource{{name: strings.ToLower(name), columns: cols}}}
}

func (a *sqliteAnalyzer) assign(as []*rq.Assignment, cols []sqliteColumn, sc *sqliteScope) {
	for _, as := range as {
		a.eval(as.Expr, sc)
		if len(as.Columns) == 1 {
			if c, ok := findColumn(cols, strings.ToLower(as.Columns[0].Name)); ok {
				a.bind(as.Expr, sqliteColumnSchema(c.decltype))
			}
		}
	}
}

func (a *sqliteAnalyzer) returning(r *rq.ReturningClause, sc *sqliteScope) ([]sqliteValue, bool) {
	if r == nil {
		return nil, true
	}
	return a.resultColumns(r.Columns, sc)
}

// eval works out an expression's type and nullability, recording the
// types of the placeholders it uses along the way.
func (a *sqliteAnalyzer) eval(e rq.Expr, sc *sqliteScope) sqliteValue {
	switch e := e.(type) {
	case nil:
		return unknown
	case *rq.ParenExpr:
		return a.eval(e.X, sc)
	case *rq.NumberLit:
		if strings.ContainsAny(e.Value, ".eE") && !strings.HasPrefix(strings.ToLower(e.Value), "0x") {
			return sqliteValue{typed("number", ""), true, true}
		}
		return sqliteValue{typed("integer", ""), true, true}
	case *rq.StringLit:
		return sqliteValue{typed("string", ""), true, true}
	case *rq.BlobLit:
		return sqliteValue{&Schema{Type: "string", ContentEncoding: "base64"}, true, true}
	case *rq.BoolLit:
		return sqliteValue{typed("integer", ""), true, true}
	case *rq.NullLit:
		return sqliteValue{typed("null", ""), false, true}
	case *rq.BindExpr:
		return sqliteValue{known: true}
	case *rq.Ident:
		return a.ref(sc, "", e.Name)
	case *rq.QualifiedRef:
		if e.Star.IsValid() || e.Column == nil {
			return unknown
		}
		return a.ref(sc, e.Table.Name, e.Column.Name)
	case *rq.UnaryExpr:
		x := a.eval(e.X, sc)
		switch e.Op {
		case rq.NOT, rq.BITNOT:
			return sqliteValue{typed("integer", ""), x.notNull, true}
		case rq.PLUS, rq.MINUS:
			return sqliteValue{numeric(x.schema, x.schema), x.notNull, true}
		}
		return unknown
	case *rq.BinaryExpr:
		return a.binary(e, sc)
	case *rq.Null:
		a.eval(e.X, sc)
		return sqliteValue{typed("integer", ""), true, true}
	case *rq.CastExpr:
		x := a.eval(e.X, sc)
		s := castSchema(e.Type)
		a.bind(e.X, s)
		return sqliteValue{s, x.notNull, true}
	case *rq.CaseExpr:
		a.eval(e.Operand, sc)
		v := sqliteValue{notNull: e.ElseExpr != nil, known: true}
		var results []sqliteValue
		for _, b := range e.Blocks {
			a.eval(b.Condition, sc)
			results = append(results, a.eval(b.Body, sc))
		}
		if e.ElseExpr != nil {
			results = append(results, a.eval(e.ElseExpr, sc))
		}
		for i, r := range results {
			if i == 0 {
				v.schema = r.schema
			} else {
				v.schema = union(v.schema, r.schema)
			}
			v.notNull = v.notNull && r.notNull
		}
		return v
	case *rq.Exists:
		a.selectColumns(e.Select, sc)
		return sqliteValue{typed("integer", ""), true, true}
	case rq.SelectExpr:
		cols, ok := a.selectColumns(e.SelectStatement, sc)
		v := sqliteValue{known: true}
		if ok && len(cols) == 1 {
			v.schema = cols[0].schema
		}
		return v
	case *rq.Call:
		return a.call(e, sc)
	case *rq.ExprList:
		for _, x := range e.Exprs {
			a.eval(x, sc)
		}
		return unknown
	case *rq.Range:
		a.eval(e.X, sc)
		a.eval(e.Y, sc)
		return unknown
	}
	return unknown
}

func (a *sqliteAnalyzer) binary(e *rq.BinaryExpr, sc *sqliteScope) sqliteValue {
	x := a.eval(e.X, sc)
	boolean := func(notNull bool) sqliteValue { return sqliteValue{typed("integer", ""), notNull, true} }
	switch e.Op {
	case rq.IN, rq.NOTIN:
		list, ok := e.Y.(*rq.ExprList)
		if !ok {
			a.eval(e.Y, sc)
			return boolean(false)
		}
		notNull := x.notNull
		for _, item := range list.Exprs {
			v := a.eval(item, sc)
			a.bind(item, x.schema)
			a.bind(e.X, v.schema)
			notNull = notNull && v.notNull
		}
		return boolean(notNull)
	case rq.BETWEEN, rq.NOTBETWEEN:
		r, ok := e.Y.(*rq.Range)
		if !ok {
			a.eval(e.Y, sc)
			return boolean(false)
		}
		lo, hi := a.eval(r.X, sc), a.eval(r.Y, sc)
		a.bind(r.X, x.schema)
		a.bind(r.Y, x.schema)
		a.bind(e.X, union(lo.schema, hi.schema))
		return boolean(x.notNull && lo.notNull && hi.notNull)
	}
	y := a.eval(e.Y, sc)
	both := x.notNull && y.notNull
	switch e.Op {
	case rq.JSON_EXTRACT_JSON, rq.JSON_EXTRACT_SQL:
		return sqliteValue{known: true}
	}
	a.bind(e.X, y.schema)
	a.bind(e.Y, x.schema)
	switch e.Op {
	case rq.PLUS, rq.MINUS, rq.STAR:
		return sqliteValue{numeric(x.schema, y.schema), both, true}
	case rq.SLASH, rq.REM:
		// Division by zero is NULL in SQLite.
		return sqliteValue{numeric(x.schema, y.schema), false, true}
	case rq.CONCAT:
		return sqliteValue{typed("string", ""), both, true}
	case rq.IS, rq.ISNOT, rq.ISDISTINCT, rq.ISNOTDISTINCT:
		return boolean(true)
	case rq.EQ, rq.NE, rq.LT, rq.LE, rq.GT, rq.GE, rq.AND, rq.OR,
		rq.LIKE, rq.NOTLIKE, rq.GLOB, rq.NOTGLOB, rq.MATCH, rq.NOTMATCH, rq.REGEXP, rq.NOTREGEXP,
		rq.BITAND, rq.BITOR, rq.LSHIFT, rq.RSHIFT:
		return boolean(both)
	}
	return unknown
}

// numeric is the type of arithmetic on values of types x and y.
func numeric(x, y *Schema) *Schema {
	kind := func(s *Schema) string {
		if s == nil {
			return ""
		}
		ts := withoutNull(s.types())
		if len(ts) != 1 {
			return ""
		}
		switch ts[0] {
		case "integer", "boolean": // booleans are stored as 0 and 1
			return "integer"
		case "number":
			return "number"
		}
		return ""
	}
	kx, ky := kind(x), kind(y)
	switch {
	case kx == "" || ky == "":
		return nil
	case kx == "integer" && ky == "integer":
		return typed("integer", "")
	}
	return typed("number", "")
}

// castSchema is the type of CAST(x AS t), by t's type affinity.
func castSchema(t *rq.Type) *Schema {
	if t == nil || t.Name == nil {
		return nil
	}
	name := strings.ToUpper(t.Name.Name)
	switch {
	case strings.Contains(name, "INT"):
		return typed("integer", "")
	case strings.Contains(name, "CHAR"), strings.Contains(name, "CLOB"), strings.Contains(name, "TEXT"):
		return typed("string", "")
	case strings.Contains(name, "BLOB"):
		return &Schema{Type: "string", ContentEncoding: "base64"}
	case strings.Contains(name, "REAL"), strings.Contains(name, "FLOA"), strings.Contains(name, "DOUB"):
		return typed("number", "")
	}
	return typed("number", "")
}

func (a *sqliteAnalyzer) call(e *rq.Call, sc *sqliteScope) sqliteValue {
	name := strings.ToLower(e.Name.Name)
	var args []sqliteValue
	allNotNull := true
	for _, arg := range e.Args {
		v := a.eval(arg, sc)
		args = append(args, v)
		allNotNull = allNotNull && v.notNull
	}
	if e.Filter != nil {
		a.eval(e.Filter.X, sc)
	}
	arg := func(i int) sqliteValue {
		if i < len(args) {
			return args[i]
		}
		return unknown
	}
	v := func(s *Schema, notNull bool) sqliteValue { return sqliteValue{s, notNull, true} }

	if e.Over != nil {
		switch name {
		case "row_number", "rank", "dense_rank", "ntile", "count":
			return v(typed("integer", ""), true)
		case "percent_rank", "cume_dist":
			return v(typed("number", ""), true)
		case "min", "max", "first_value", "last_value", "nth_value", "lag", "lead":
			return v(arg(0).schema, false)
		}
		return sqliteValue{known: true}
	}

	// Aggregates are null over no rows: without GROUP BY, or with FILTER.
	grouped := sc.grouped && e.Filter == nil
	switch name {
	case "count":
		return v(typed("integer", ""), true)
	case "total":
		return v(typed("number", ""), true)
	case "sum":
		s := typed("number", "")
		if n := numeric(arg(0).schema, arg(0).schema); n != nil && n.Type == "integer" {
			s = n
		}
		return v(s, grouped && arg(0).notNull)
	case "avg":
		return v(typed("number", ""), grouped && arg(0).notNull)
	case "group_concat", "string_agg":
		return v(typed("string", ""), grouped && arg(0).notNull)
	case "min", "max":
		if len(args) == 1 {
			return v(arg(0).schema, grouped && arg(0).notNull)
		}
		a.bindEach(e.Args, args)
		return v(unionAll(args), allNotNull)
	case "coalesce", "ifnull":
		a.bindEach(e.Args, args)
		notNull := false
		for _, x := range args {
			notNull = notNull || x.notNull
		}
		return v(unionAll(args), notNull)
	case "nullif":
		a.bindEach(e.Args, args)
		return v(arg(0).schema, false)
	case "iif":
		if len(args) == 3 {
			return v(unionAll(args[1:]), args[1].notNull && args[2].notNull)
		}
	case "length", "instr", "unicode", "octet_length":
		return v(typed("integer", ""), allNotNull)
	case "upper", "lower", "trim", "ltrim", "rtrim", "substr", "substring", "replace", "hex", "char":
		return v(typed("string", ""), allNotNull)
	case "quote", "typeof", "sqlite_version":
		return v(typed("string", ""), true)
	case "abs":
		return v(numeric(arg(0).schema, arg(0).schema), allNotNull)
	case "round":
		return v(typed("number", ""), allNotNull)
	case "random", "changes", "total_changes", "last_insert_rowid":
		return v(typed("integer", ""), true)
	case "randomblob", "zeroblob":
		return v(&Schema{Type: "string", ContentEncoding: "base64"}, allNotNull)
	case "like", "glob":
		return v(typed("integer", ""), allNotNull)
	// Date functions return NULL for input they can't parse.
	case "date":
		return v(typed("string", "date"), false)
	case "time", "datetime", "strftime":
		return v(typed("string", ""), false)
	case "julianday":
		return v(typed("number", ""), false)
	case "unixepoch":
		return v(typed("integer", ""), false)
	}
	return sqliteValue{known: true}
}

// bindEach types the placeholders among a function's arguments, like
// coalesce(:x, col), by the other arguments.
func (a *sqliteAnalyzer) bindEach(exprs []rq.Expr, vals []sqliteValue) {
	for i, e := range exprs {
		var others []sqliteValue
		for j, v := range vals {
			if j != i {
				others = append(others, v)
			}
		}
		a.bind(e, unionAll(others))
	}
}

// unionAll is the union of the values' types, or nil if any is unknown.
func unionAll(vals []sqliteValue) *Schema {
	var s *Schema
	for i, v := range vals {
		if i == 0 {
			s = v.schema
		} else {
			s = union(s, v.schema)
		}
	}
	return s
}
