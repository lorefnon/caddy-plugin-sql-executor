package sqlexecutor

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/nikolalohinski/gonja/v2"
	controlstructures "github.com/nikolalohinski/gonja/v2/builtins/control_structures"
	"github.com/nikolalohinski/gonja/v2/exec"
	"github.com/nikolalohinski/gonja/v2/loaders"
	"github.com/nikolalohinski/gonja/v2/nodes"
	"github.com/nikolalohinski/gonja/v2/tokens"
)

// jinjaExt marks query files that are Jinja templates rendering to SQL,
// e.g. reports/sales.sql.j2.
const jinjaExt = ".j2"

// Names the plugin defines in every Jinja render context. They shadow
// request parameters of the same name.
const (
	jinjaBindFunc   = "__sqlx_bind"
	jinjaIdentFunc  = "ident"
	jinjaHeaderFunc = "header"
)

// jinjaTemplate is a parsed Jinja query template.
//
// Like Handlebars templates, Jinja templates are injection-safe by
// construction: after parsing, every {{ expression }} in the syntax tree is
// wrapped in a call that emits a bound parameter placeholder, except
// {{ ident(...) }}, whose output is a quoted identifier. The traversal
// fails closed: tags it does not know how to inspect (include, extends,
// import, macro, call, filter, with, block, trans, and {% set %} blocks)
// are rejected rather than left unwrapped.
type jinjaTemplate struct {
	tpl *exec.Template
}

func parseJinjaTemplate(src string) (*jinjaTemplate, error) {
	cfg := gonja.DefaultConfig.Inherit()
	cfg.AutoEscape = false
	cfg.StrictUndefined = false
	tpl, err := exec.NewTemplate(jinjaRootName, cfg, &sourceOnlyLoader{src: src}, gonja.DefaultEnvironment)
	if err != nil {
		return nil, err
	}
	root := tpl.Root()
	if len(root.Macros) > 0 {
		return nil, errors.New("macros are not supported in SQL templates")
	}
	w := &jinjaWalker{}
	for _, n := range root.Nodes {
		w.node(n)
	}
	if w.err != nil {
		return nil, w.err
	}
	return &jinjaTemplate{tpl: tpl}, nil
}

func (t *jinjaTemplate) render(params map[string]any, header http.Header) (string, map[string]any, error) {
	bound := map[string]any{}
	var inputErr *inputError
	fail := func(msg string) error {
		if inputErr == nil {
			inputErr = &inputError{msg}
		}
		return inputErr
	}

	data := make(map[string]any, len(params)+3)
	for k, v := range params {
		data[k] = v
	}
	data[jinjaBindFunc] = func(va *exec.VarArgs) (any, error) {
		if len(va.Args) != 1 {
			return nil, errors.New("bind expects one value")
		}
		// Convert gonja's own list and dict types (from template literals
		// or filters) into plain Go values the database driver accepts.
		v := va.Args[0].ToGoSimpleType(false)
		if err, ok := v.(error); ok {
			return nil, fmt.Errorf("cannot bind value: %w", err)
		}
		name := boundParamPrefix + strconv.Itoa(len(bound)+1)
		bound[name] = v
		return ":" + name, nil
	}
	data[jinjaIdentFunc] = func(va *exec.VarArgs) (*exec.Value, error) {
		if len(va.Args) != 1 {
			return nil, fail("ident expects one value, plus an optional allow list")
		}
		var allow []string
		if a, ok := va.KwArgs["allow"]; ok {
			var err error
			if allow, err = allowList(a); err != nil {
				return nil, err
			}
		}
		quoted, err := quoteIdent(va.Args[0].ToGoSimpleType(false), allow)
		if err != nil {
			return nil, fail(err.Error())
		}
		return exec.AsSafeValue(quoted), nil
	}
	data[jinjaHeaderFunc] = func(va *exec.VarArgs) (any, error) {
		if len(va.Args) != 1 || !va.Args[0].IsString() {
			return nil, errors.New("header expects a header name")
		}
		vs := header.Values(va.Args[0].String())
		if len(vs) == 0 {
			return nil, nil
		}
		return strings.Join(vs, ", "), nil
	}

	out, err := t.tpl.ExecuteToString(exec.NewContext(data))
	if inputErr != nil {
		return "", nil, inputErr
	}
	if err != nil {
		return "", nil, err
	}
	return out, bound, nil
}

// allowList reads ident's allow argument, given as ["a", "b"] or "a,b".
// Anything else is an error, so a mistyped allow list can never mean "no
// restriction".
func allowList(v *exec.Value) ([]string, error) {
	var out []string
	switch g := v.ToGoSimpleType(false).(type) {
	case string:
		for _, s := range strings.Split(g, ",") {
			out = append(out, strings.TrimSpace(s))
		}
	case []any:
		for _, e := range g {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("ident: allow list entries must be strings, got %v", e)
			}
			out = append(out, s)
		}
	default:
		return nil, fmt.Errorf("ident: allow must be a list of names or a comma-separated string, got %v", g)
	}
	if len(out) == 0 {
		return nil, errors.New("ident: allow list is empty")
	}
	return out, nil
}

// jinjaWalker wraps every output expression in a bind call, recording the
// first construct it cannot vouch for.
type jinjaWalker struct {
	err error
}

func (w *jinjaWalker) fail(tok *tokens.Token, format string, args ...any) {
	if w.err != nil {
		return
	}
	if tok != nil {
		format = "line %d: " + format
		args = append([]any{tok.Line}, args...)
	}
	w.err = fmt.Errorf(format, args...)
}

func (w *jinjaWalker) node(n nodes.Node) {
	switch n := n.(type) {
	case *nodes.Data, *nodes.Comment:
	case *nodes.Output:
		n.Expression = w.wrap(n.Expression)
		if n.Alternative != nil {
			n.Alternative = w.wrap(n.Alternative)
		}
	case *nodes.Wrapper:
		w.wrapper(n)
	case *nodes.ControlStructureBlock:
		w.control(n.Location, n.Name, n.ControlStructure)
	default:
		w.fail(nil, "unsupported template construct %T", n)
	}
}

func (w *jinjaWalker) wrapper(n *nodes.Wrapper) {
	if n == nil {
		return
	}
	for _, c := range n.Nodes {
		w.node(c)
	}
}

func (w *jinjaWalker) control(tok *tokens.Token, name string, cs nodes.ControlStructure) {
	switch cs := cs.(type) {
	case *controlstructures.IfControlStructure:
		for _, wr := range cs.Wrappers {
			w.wrapper(wr)
		}
	case *controlstructures.ForControlStructure:
		w.wrapper(cs.BodyWrapper)
		w.wrapper(cs.EmptyWrapper)
	case *controlstructures.AutoescapeControlStructure:
		w.wrapper(cs.Wrapper)
	case *controlstructures.RawControlStructure, *controlstructures.DoControlStructure,
		*controlstructures.BreakControlStructure, *controlstructures.ContinueControlStructure:
	case *controlstructures.SetControlStructure:
		// {% set x = ... %} produces no output; {% set x %}...{% endset %}
		// captures rendered text that could later be output unbound. The
		// body is unexported, so it is inspected by reflection and treated
		// as present if the field cannot be found.
		body := reflect.ValueOf(cs).Elem().FieldByName("body")
		if !body.IsValid() || body.Kind() != reflect.Pointer || !body.IsNil() {
			w.fail(tok, "{%% set %%} blocks are not supported in SQL templates; use {%% set name = value %%}")
		}
	default:
		w.fail(tok, "{%% %s %%} is not supported in SQL templates", name)
	}
}

// wrap turns expr into __sqlx_bind(expr), leaving ident(...) calls alone.
func (w *jinjaWalker) wrap(expr nodes.Expression) nodes.Expression {
	if call, ok := expr.(*nodes.Call); ok {
		if name, ok := call.Func.(*nodes.Name); ok && (name.Name.Val == jinjaIdentFunc || name.Name.Val == jinjaBindFunc) {
			return expr
		}
	}
	pos := expr.Position()
	tok := &tokens.Token{Type: tokens.Name, Val: jinjaBindFunc}
	if pos != nil {
		tok.Pos, tok.Line, tok.Col = pos.Pos, pos.Line, pos.Col
	}
	return &nodes.Call{
		Location: tok,
		Func:     &nodes.Name{Name: tok},
		Args:     []nodes.Expression{expr},
		Kwargs:   map[string]nodes.Expression{},
	}
}

const jinjaRootName = "query"

// sourceOnlyLoader serves the template being parsed and nothing else, so
// include/extends/import cannot read files.
type sourceOnlyLoader struct{ src string }

var _ loaders.Loader = (*sourceOnlyLoader)(nil)

func (l *sourceOnlyLoader) Read(path string) (io.Reader, error) {
	if path != jinjaRootName {
		return nil, fmt.Errorf("loading other templates (%q) is not supported in SQL templates", path)
	}
	return strings.NewReader(l.src), nil
}

func (l *sourceOnlyLoader) Resolve(path string) (string, error) {
	if path != jinjaRootName {
		return "", fmt.Errorf("loading other templates (%q) is not supported in SQL templates", path)
	}
	return path, nil
}

func (l *sourceOnlyLoader) Inherit(string) (loaders.Loader, error) {
	return nil, errors.New("loading other templates is not supported in SQL templates")
}
