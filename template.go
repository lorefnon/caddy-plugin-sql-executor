package sqlexecutor

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/mailgun/raymond/v2"
	"github.com/mailgun/raymond/v2/ast"
	"github.com/mailgun/raymond/v2/parser"
)

// templateExt marks query files that are Handlebars templates rendering
// to SQL, e.g. reports/sales.sql.hbs.
const templateExt = ".hbs"

// boundParamPrefix names the placeholders generated for {{value}}
// expressions; request parameters with this prefix are discarded.
const boundParamPrefix = "__hbs_"

// sqlHelpers are the helpers whose output may appear directly in SQL.
var sqlHelpers = map[string]bool{"bind": true, "ident": true}

// inputError reports a template failure caused by request data rather
// than by the template itself.
type inputError struct{ msg string }

func (e *inputError) Error() string { return e.msg }

// sqlTemplate is a parsed query template.
//
// Templates are made injection-safe by construction: every plain
// {{expression}} is rewritten to {{bind expression}}, which emits a bound
// parameter placeholder rather than the value itself. Raw output
// ({{{x}}} / {{& x}}), partials and helpers other than bind and ident are
// rejected when the template is parsed.
type sqlTemplate struct {
	tpl *raymond.Template
	// headers lists the names referenced as @headers.<name>.
	headers []string
}

func parseSQLTemplate(src string) (*sqlTemplate, error) {
	rewritten, headers, err := rewriteTemplate(src)
	if err != nil {
		return nil, err
	}
	tpl, err := raymond.Parse(rewritten)
	if err != nil {
		return nil, err
	}
	return &sqlTemplate{tpl: tpl, headers: headers}, nil
}

// absentHeader stands in for referenced headers the request lacks. It is
// non-nil, so raymond does not fall back from @headers.x to the context
// path headers.x (which request parameters control); it is falsy in
// {{#if}}, and binds as NULL.
type absentHeader struct{}

var absent = (*absentHeader)(nil)

// render evaluates the template against params, returning SQL with :name
// placeholders and the values for the placeholders it generated. Request
// headers are exposed as the @headers data variable, keyed by lower-case
// name, so request parameters can never masquerade as headers.
func (t *sqlTemplate) render(params map[string]any, header http.Header) (string, map[string]any, error) {
	bound := map[string]any{}
	tpl := t.tpl.Clone()
	tpl.RegisterHelper("bind", func(v any) raymond.SafeString {
		if v == absent {
			v = nil
		}
		name := boundParamPrefix + strconv.Itoa(len(bound)+1)
		bound[name] = v
		return raymond.SafeString(":" + name)
	})
	tpl.RegisterHelper("ident", identHelper)

	data := raymond.NewDataFrame()
	data.Set("headers", headerData(header, t.headers))
	out, err := tpl.ExecWith(params, data)
	if err != nil {
		return "", nil, err
	}
	return out, bound, nil
}

// headerData flattens request headers for templates: names are
// lower-cased and repeated headers are joined with ", " as in HTTP. Every
// referenced name is also present as written (matched case-insensitively),
// holding absent when the request does not carry it.
func headerData(header http.Header, referenced []string) map[string]any {
	out := make(map[string]any, len(header)+len(referenced))
	for k, vs := range header {
		out[strings.ToLower(k)] = strings.Join(vs, ", ")
	}
	for _, name := range referenced {
		if vs := header.Values(name); len(vs) > 0 {
			out[name] = strings.Join(vs, ", ")
		} else {
			out[name] = absent
		}
	}
	return out
}

// identHelper renders a value as a double-quoted SQL identifier, quoting
// each dot-separated part. With allow="a,b,c" the value must be one of
// the listed names, which is the recommended way to accept identifiers
// such as sort columns from requests.
func identHelper(v any, options *raymond.Options) raymond.SafeString {
	name, ok := v.(string)
	if !ok || name == "" {
		panic(&inputError{fmt.Sprintf("ident: expected a non-empty string, got %v", v)})
	}
	if allow := options.HashStr("allow"); allow != "" {
		permitted := false
		for _, a := range strings.Split(allow, ",") {
			if strings.TrimSpace(a) == name {
				permitted = true
				break
			}
		}
		if !permitted {
			panic(&inputError{fmt.Sprintf("ident: %q is not one of %s", name, allow)})
		}
	}
	if strings.ContainsRune(name, 0) {
		panic(&inputError{"ident: identifier contains a NUL byte"})
	}
	parts := strings.Split(name, ".")
	for i, p := range parts {
		parts[i] = `"` + strings.ReplaceAll(p, `"`, `""`) + `"`
	}
	return raymond.SafeString(strings.Join(parts, "."))
}

// rewriteTemplate validates a template and inserts "bind " into each plain
// {{expression}}, using statement offsets reported by the parser. It also
// returns the header names referenced via @headers.<name>.
func rewriteTemplate(src string) (string, []string, error) {
	program, err := parser.Parse(src)
	if err != nil {
		return "", nil, err
	}
	w := &templateWalker{src: src, headers: map[string]bool{}}
	program.Accept(w)
	if w.err != nil {
		return "", nil, w.err
	}
	headers := make([]string, 0, len(w.headers))
	for name := range w.headers {
		headers = append(headers, name)
	}

	var b strings.Builder
	last := 0
	for _, pos := range w.binds {
		b.WriteString(src[last:pos])
		b.WriteString("bind ")
		last = pos
	}
	b.WriteString(src[last:])
	return b.String(), headers, nil
}

// templateWalker visits every node, recording where "bind " must be
// inserted and the first disallowed construct.
type templateWalker struct {
	src     string
	binds   []int
	headers map[string]bool
	err     error
}

func (w *templateWalker) fail(line int, format string, args ...any) {
	if w.err == nil {
		w.err = fmt.Errorf("line %d: "+format, append([]any{line}, args...)...)
	}
}

func (w *templateWalker) VisitProgram(n *ast.Program) any {
	for _, s := range n.Body {
		s.Accept(w)
	}
	return nil
}

func (w *templateWalker) VisitMustache(n *ast.MustacheStatement) any {
	expr := n.Expression
	if n.Unescaped {
		w.fail(n.Line, "%s: raw output is not allowed in SQL templates; use double braces to bind the value as a parameter",
			w.snippet(n.Pos))
		return nil
	}
	if len(expr.Params) == 0 && expr.Hash == nil {
		// Plain value: insert "bind " after "{{" and an optional "~".
		pos := n.Pos
		if !strings.HasPrefix(w.src[pos:], "{{") {
			w.fail(n.Line, "unexpected template structure near offset %d", pos)
			return nil
		}
		pos += 2
		if strings.HasPrefix(w.src[pos:], "~") {
			pos++
		}
		w.binds = append(w.binds, pos)
		expr.Accept(w)
		return nil
	}
	if name := helperName(expr); !sqlHelpers[name] {
		w.fail(n.Line, "%s: only bind and ident may produce output in SQL templates; wrap other helpers as {{bind (helper ...)}}",
			w.snippet(n.Pos))
		return nil
	}
	expr.Accept(w)
	return nil
}

func (w *templateWalker) VisitBlock(n *ast.BlockStatement) any {
	n.Expression.Accept(w)
	if n.Program != nil {
		n.Program.Accept(w)
	}
	if n.Inverse != nil {
		n.Inverse.Accept(w)
	}
	return nil
}

func (w *templateWalker) VisitPartial(n *ast.PartialStatement) any {
	w.fail(n.Line, "partials are not supported in SQL templates")
	return nil
}

func (w *templateWalker) VisitContent(*ast.ContentStatement) any { return nil }
func (w *templateWalker) VisitComment(*ast.CommentStatement) any { return nil }

func (w *templateWalker) VisitExpression(n *ast.Expression) any {
	n.Path.Accept(w)
	for _, p := range n.Params {
		p.Accept(w)
	}
	if n.Hash != nil {
		n.Hash.Accept(w)
	}
	return nil
}

func (w *templateWalker) VisitSubExpression(n *ast.SubExpression) any {
	return n.Expression.Accept(w)
}

// VisitPath records @headers.<name> references. Other shapes are rejected
// because raymond would resolve them against request parameters whenever
// the data lookup comes back empty.
func (w *templateWalker) VisitPath(n *ast.PathExpression) any {
	if !n.Data || len(n.Parts) == 0 || n.Parts[0] != "headers" {
		return nil
	}
	switch {
	case n.Depth != 0:
		w.fail(n.Line, "@%s: refer to headers as @headers.<name>, without ../", n.Original)
	case len(n.Parts) > 2:
		w.fail(n.Line, "@%s: header values have no fields", n.Original)
	case len(n.Parts) == 2:
		w.headers[n.Parts[1]] = true
	}
	return nil
}
func (w *templateWalker) VisitString(*ast.StringLiteral) any   { return nil }
func (w *templateWalker) VisitBoolean(*ast.BooleanLiteral) any { return nil }
func (w *templateWalker) VisitNumber(*ast.NumberLiteral) any   { return nil }
func (w *templateWalker) VisitHashPair(n *ast.HashPair) any    { return n.Val.Accept(w) }
func (w *templateWalker) VisitHash(n *ast.Hash) any {
	for _, p := range n.Pairs {
		p.Accept(w)
	}
	return nil
}

func helperName(expr *ast.Expression) string {
	if p, ok := expr.Path.(*ast.PathExpression); ok {
		return p.Original
	}
	return ""
}

// snippet returns the source text of the mustache starting at pos.
func (w *templateWalker) snippet(pos int) string {
	rest := w.src[pos:]
	end := strings.Index(rest, "}}")
	if end < 0 {
		return rest
	}
	end += 2
	if strings.HasPrefix(rest[end:], "}") {
		end++
	}
	return rest[:end]
}

// mergeBoundParams adds the template's generated parameters to the
// request parameters. Request values can never fill generated slots.
func mergeBoundParams(params, bound map[string]any) map[string]any {
	merged := make(map[string]any, len(params)+len(bound))
	for k, v := range params {
		if !strings.HasPrefix(k, boundParamPrefix) {
			merged[k] = v
		}
	}
	for k, v := range bound {
		merged[k] = v
	}
	return merged
}

// jsonIfStructured passes arrays and objects to the database as JSON text.
func jsonIfStructured(v any) (any, error) {
	switch v.(type) {
	case []any, map[string]any:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		return string(b), nil
	}
	return v, nil
}

// checkBoundPlaceholders reports generated placeholders that did not
// survive compilation, which happens when {{value}} is written inside a
// quoted SQL string ('%{{q}}%') and would silently match the literal text.
func checkBoundPlaceholders(stmt *statement, bound map[string]any) error {
	used := make(map[string]bool, len(stmt.Params))
	for _, p := range stmt.Params {
		used[p] = true
	}
	for name := range bound {
		if !used[name] {
			return errors.New("a {{value}} expression appears inside a quoted SQL string or comment, " +
				"where it cannot be bound; build the string in SQL instead, e.g. '%' || {{q}} || '%'")
		}
	}
	return nil
}

// templateCache keeps parsed templates keyed by route name, re-parsing
// whenever the source text changes.
type templateCache struct {
	mu      sync.Mutex
	entries map[string]templateCacheEntry
}

type templateCacheEntry struct {
	src string
	tpl *sqlTemplate
	err error
}

func (c *templateCache) get(name, src string) (*sqlTemplate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[name]; ok && e.src == src {
		return e.tpl, e.err
	}
	tpl, err := parseSQLTemplate(src)
	c.entries[name] = templateCacheEntry{src: src, tpl: tpl, err: err}
	return tpl, err
}
