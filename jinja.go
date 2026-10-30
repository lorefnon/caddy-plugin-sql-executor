package sqlexecutor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/nikolalohinski/gonja/v2"
	controlstructures "github.com/nikolalohinski/gonja/v2/builtins/control_structures"
	"github.com/nikolalohinski/gonja/v2/config"
	"github.com/nikolalohinski/gonja/v2/exec"
	"github.com/nikolalohinski/gonja/v2/loaders"
	"github.com/nikolalohinski/gonja/v2/nodes"
	"github.com/nikolalohinski/gonja/v2/parser"
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
	jinjaRunVar     = "__sqlx_run"
)

// maxJinjaDepth bounds nested includes and macro calls, so a recursive
// template fails instead of exhausting the stack.
const maxJinjaDepth = 64

// maxJinjaExtends bounds the templates read while parsing one template,
// which also stops extends cycles.
const maxJinjaExtends = 32

// jinjaTemplate is a parsed Jinja query template.
//
// Templates are injection-safe by construction: after parsing, every
// {{ expression }} in the syntax tree is wrapped in a call that emits a
// bound parameter placeholder, except {{ ident(...) }}, whose output is a
// quoted identifier, and the output of macros and super(), which is SQL
// rendered from other checked template text. The traversal fails closed: tags it does not know how to inspect
// (filter, with, call, trans and {% set %} blocks) are rejected rather
// than left unwrapped.
//
// Unless jinja_includes is enabled, templates also cannot use include,
// import, from, extends, block or macro. When it is, other templates are
// loaded from the query source and checked the same way before use.
type jinjaTemplate struct {
	tpl    *exec.Template
	cfg    *config.Config
	loader *jinjaLoader
	cache  *templateCache // nil unless jinja_includes is enabled
}

// parseJinjaTemplate parses the template src read from the file name.
// With a non-nil cache, the template may load others through it; the
// templates it extends are returned with their source text so the cache
// can tell when they change.
func parseJinjaTemplate(ctx context.Context, name, src string, cache *templateCache) (*jinjaTemplate, map[string]string, error) {
	cfg := gonja.DefaultConfig.Inherit()
	cfg.AutoEscape = false
	cfg.StrictUndefined = false
	loader := &jinjaLoader{ctx: ctx, cache: cache, name: name, src: src, deps: map[string]string{}}
	tpl, err := exec.NewTemplate(name, cfg, loader, jinjaEnvironment)
	loader.closed = true
	if err != nil {
		return nil, nil, err
	}
	w := &jinjaWalker{includes: cache != nil}
	// Rendering starts from the last template in the extends chain, and
	// blocks may come from any of them.
	for root := tpl.Root(); root != nil; root = root.Parent {
		for _, n := range root.Nodes {
			w.node(n)
		}
	}
	if w.err != nil {
		return nil, nil, w.err
	}
	return &jinjaTemplate{tpl: tpl, cfg: cfg, loader: loader, cache: cache}, loader.deps, nil
}

func (t *jinjaTemplate) render(ctx context.Context, params map[string]any, header http.Header) (string, map[string]any, error) {
	return t.renderWith(ctx, params, header, nil)
}

// renderWith is render, calling onHeader (if not nil) with the name of
// each header the template reads.
func (t *jinjaTemplate) renderWith(ctx context.Context, params map[string]any, header http.Header, onHeader func(string)) (string, map[string]any, error) {
	bound := map[string]any{}
	var inputErr *inputError
	fail := func(msg string) error {
		if inputErr == nil {
			inputErr = &inputError{msg}
		}
		return inputErr
	}

	data := make(map[string]any, len(params)+4)
	for k, v := range params {
		data[k] = v
	}
	data[jinjaRunVar] = &jinjaRun{ctx: ctx, cache: t.cache}
	data[jinjaBindFunc] = func(va *exec.VarArgs) (any, error) {
		if len(va.Args) != 1 {
			return nil, errors.New("bind expects one value")
		}
		// Macro output is SQL rendered from checked template text.
		if f, ok := va.Args[0].Interface().(sqlFragment); ok {
			return string(f), nil
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
		if onHeader != nil {
			onHeader(va.Args[0].String())
		}
		vs := header.Values(va.Args[0].String())
		if len(vs) == 0 {
			return nil, nil
		}
		return strings.Join(vs, ", "), nil
	}

	out, err := t.execute(data)
	if inputErr != nil {
		return "", nil, inputErr
	}
	if err != nil {
		return "", nil, err
	}
	return out, bound, nil
}

// execute renders the template. Like Jinja, and unlike gonja, it first
// runs the imports, macro definitions and assignments outside blocks in
// each template that extends another, so that its blocks can use them.
func (t *jinjaTemplate) execute(data map[string]any) (string, error) {
	var out strings.Builder
	env := *jinjaEnvironment
	env.Context = jinjaEnvironment.Context.Inherit().Update(exec.NewContext(data))
	r := exec.NewRenderer(&env, &out, t.cfg, t.loader, t.tpl)
	for root := t.tpl.Root(); root.Parent != nil; root = root.Parent {
		for _, n := range root.Nodes {
			b, ok := n.(*nodes.ControlStructureBlock)
			if !ok {
				continue
			}
			switch b.ControlStructure.(type) {
			case *jinjaImport, *jinjaMacro, *controlstructures.SetControlStructure:
				if _, err := r.Visit(b); err != nil {
					return "", err
				}
			}
		}
	}
	if err := r.Execute(); err != nil {
		return "", err
	}
	return out.String(), nil
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
	includes bool // whether jinja_includes is enabled
	err      error
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
	case *jinjaMacro, *jinjaBlock, *jinjaInclude, *jinjaImport, *controlstructures.ExtendsControlStructure:
		if !w.includes {
			w.fail(tok, "{%% %s %%} is not supported in SQL templates unless jinja_includes is enabled", name)
			return
		}
		switch cs := cs.(type) {
		case *jinjaMacro:
			w.wrapper(cs.Wrapper)
		case *jinjaBlock:
			w.wrapper(cs.body)
		}
		// Included and imported templates are checked when they are loaded,
		// and extended ones by parseJinjaTemplate.
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

// sqlFragment is SQL produced by rendering checked template text: the
// output of a macro or of super(). Bind emits it as is. Request data never
// has this type, and filters or string operations on a fragment produce
// plain strings, which are bound as usual.
type sqlFragment string

// jinjaEnvironment is gonja's default environment with the tags that
// render other templates replaced by versions that only produce
// sqlFragments from checked templates.
var jinjaEnvironment = func() *exec.Environment {
	cs := exec.NewControlStructureSet(map[string]parser.ControlStructureParser{}).
		Update(gonja.DefaultEnvironment.ControlStructures)
	macro, _ := cs.Get("macro")
	from, _ := cs.Get("from")
	for name, p := range map[string]parser.ControlStructureParser{
		"macro":   wrapMacroParser(macro),
		"from":    wrapFromParser(from),
		"import":  importParser,
		"include": includeParser,
		"block":   blockParser,
	} {
		if err := cs.Replace(name, p); err != nil {
			panic(err)
		}
	}
	env := *gonja.DefaultEnvironment
	env.ControlStructures = cs
	return &env
}()

// jinjaRun is the state of one render, shared with the tags that load
// templates or call macros.
type jinjaRun struct {
	ctx   context.Context
	cache *templateCache
	depth int
}

func runOf(r *exec.Renderer) (*jinjaRun, error) {
	v, _ := r.Environment.Context.Get(jinjaRunVar)
	run, ok := v.(*jinjaRun)
	if !ok || run.cache == nil {
		return nil, errors.New("loading other templates requires jinja_includes")
	}
	return run, nil
}

func (run *jinjaRun) enter() error {
	if run.depth >= maxJinjaDepth {
		return fmt.Errorf("templates nest more than %d includes or macro calls deep; check for recursion", maxJinjaDepth)
	}
	run.depth++
	return nil
}

func (run *jinjaRun) leave() { run.depth-- }

// load evaluates a template name and returns that template, checked.
func (run *jinjaRun) load(r *exec.Renderer, expr nodes.Expression) (*jinjaTemplate, error) {
	v := r.Eval(expr)
	if v.IsError() {
		return nil, v
	}
	if !v.IsString() {
		return nil, fmt.Errorf("template name must be a string, got %s", v)
	}
	return run.cache.jinja(run.ctx, v.String())
}

// macro turns a macro definition into a function returning an sqlFragment.
func (run *jinjaRun) macro(node *nodes.Macro, r *exec.Renderer) (exec.Macro, error) {
	fn, err := exec.MacroNodeToFunc(node, r)
	if err != nil {
		return nil, err
	}
	return func(va *exec.VarArgs) *exec.Value {
		if err := run.enter(); err != nil {
			return exec.AsValue(err)
		}
		defer run.leave()
		out := fn(va)
		if out.IsError() {
			return out
		}
		return exec.AsValue(sqlFragment(out.String()))
	}, nil
}

// jinjaMacro is {% macro %}, defining a macro that returns an sqlFragment.
type jinjaMacro struct {
	*controlstructures.MacroControlStructure
}

func wrapMacroParser(p parser.ControlStructureParser) parser.ControlStructureParser {
	return func(doc *parser.Parser, args *parser.Parser) (nodes.ControlStructure, error) {
		cs, err := p(doc, args)
		if err != nil {
			return nil, err
		}
		return &jinjaMacro{cs.(*controlstructures.MacroControlStructure)}, nil
	}
}

func (m *jinjaMacro) Execute(r *exec.Renderer, _ *nodes.ControlStructureBlock) error {
	run, err := runOf(r)
	if err != nil {
		return err
	}
	fn, err := run.macro(m.Macro, r)
	if err != nil {
		return err
	}
	r.Environment.Context.Set(m.Name, fn)
	return nil
}

// jinjaInclude is {% include name [ignore missing] [with context] %}.
// Included templates see the including template's variables.
type jinjaInclude struct {
	location      *tokens.Token
	name          nodes.Expression
	ignoreMissing bool
}

func (c *jinjaInclude) Position() *tokens.Token { return c.location }
func (c *jinjaInclude) String() string {
	return fmt.Sprintf("Include(%s Line=%d)", c.name, c.location.Line)
}

func includeParser(p *parser.Parser, args *parser.Parser) (nodes.ControlStructure, error) {
	c := &jinjaInclude{location: p.Current()}
	var err error
	if c.name, err = args.ParseExpression(); err != nil {
		return nil, err
	}
	if args.MatchName("ignore") != nil {
		if args.MatchName("missing") == nil {
			return nil, args.Error("expected 'missing' after 'ignore'", nil)
		}
		c.ignoreMissing = true
	}
	if args.MatchName("with") != nil && args.MatchName("context") == nil {
		return nil, args.Error("expected 'context' after 'with'", nil)
	}
	if !args.End() {
		return nil, args.Error("malformed include tag; included templates always see the including template's variables", nil)
	}
	return c, nil
}

func (c *jinjaInclude) Execute(r *exec.Renderer, _ *nodes.ControlStructureBlock) error {
	run, err := runOf(r)
	if err != nil {
		return err
	}
	tpl, err := run.load(r, c.name)
	if c.ignoreMissing && errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := run.enter(); err != nil {
		return err
	}
	defer run.leave()
	// The included template gets its own scope, so its {% set %}s do not
	// leak into the including template.
	sub := r.Inherit()
	return exec.NewRenderer(sub.Environment, r.Output, r.Config, r.Loader, tpl.tpl).Execute()
}

// jinjaImport is {% import name as alias %} or
// {% from name import macro [as alias], ... %}.
type jinjaImport struct {
	location *tokens.Token
	name     nodes.Expression
	as       string            // import ... as as
	macros   map[string]string // from ... import: alias -> macro name
}

func (c *jinjaImport) Position() *tokens.Token { return c.location }
func (c *jinjaImport) String() string {
	return fmt.Sprintf("Import(%s Line=%d)", c.name, c.location.Line)
}

func importParser(p *parser.Parser, args *parser.Parser) (nodes.ControlStructure, error) {
	c := &jinjaImport{location: p.Current()}
	var err error
	if c.name, err = args.ParseExpression(); err != nil {
		return nil, err
	}
	if args.MatchName("as") == nil {
		return nil, args.Error(`expected "as"`, args.Current())
	}
	alias := args.Match(tokens.Name)
	if alias == nil {
		return nil, args.Error("expected a name after 'as'", args.Current())
	}
	c.as = alias.Val
	if args.MatchName("with") != nil && args.MatchName("context") == nil {
		return nil, args.Error("expected 'context' after 'with'", nil)
	}
	if !args.End() {
		return nil, args.Error("malformed import tag", nil)
	}
	return c, nil
}

func wrapFromParser(p parser.ControlStructureParser) parser.ControlStructureParser {
	return func(doc *parser.Parser, args *parser.Parser) (nodes.ControlStructure, error) {
		cs, err := p(doc, args)
		if err != nil {
			return nil, err
		}
		from := cs.(*controlstructures.FromImportControlStructure)
		return &jinjaImport{location: from.Position(), name: from.FilenameExpression, macros: from.As}, nil
	}
}

func (c *jinjaImport) Execute(r *exec.Renderer, _ *nodes.ControlStructureBlock) error {
	run, err := runOf(r)
	if err != nil {
		return err
	}
	tpl, err := run.load(r, c.name)
	if err != nil {
		return err
	}
	defined := tpl.tpl.Macros()
	if c.as != "" {
		macros := make(map[string]exec.Macro, len(defined))
		for name, node := range defined {
			if macros[name], err = run.macro(node, r); err != nil {
				return err
			}
		}
		r.Environment.Context.Set(c.as, macros)
		return nil
	}
	for alias, name := range c.macros {
		node, ok := defined[name]
		if !ok {
			return fmt.Errorf("%s does not define a macro named %q", tpl.tpl.Root().Identifier, name)
		}
		fn, err := run.macro(node, r)
		if err != nil {
			return err
		}
		r.Environment.Context.Set(alias, fn)
	}
	return nil
}

// jinjaBlock is {% block name %}...{% endblock %}. Inside it, super()
// returns the parent template's version of the block as an sqlFragment.
type jinjaBlock struct {
	location *tokens.Token
	name     string
	body     *nodes.Wrapper
}

func (b *jinjaBlock) Position() *tokens.Token { return b.location }
func (b *jinjaBlock) String() string {
	return fmt.Sprintf("Block(%s Line=%d)", b.name, b.location.Line)
}

func blockParser(p *parser.Parser, args *parser.Parser) (nodes.ControlStructure, error) {
	b := &jinjaBlock{location: p.Current()}
	name := args.Match(tokens.Name)
	if name == nil || !args.End() {
		return nil, args.Error("block takes exactly one argument, its name", nil)
	}
	b.name = name.Val
	body, endArgs, err := p.WrapUntil("endblock")
	if err != nil {
		return nil, err
	}
	if !endArgs.End() {
		if end := endArgs.Match(tokens.Name); end == nil || end.Val != b.name || !endArgs.End() {
			return nil, endArgs.Error(fmt.Sprintf("endblock may only name its block, %q", b.name), nil)
		}
	}
	if p.Template.Blocks.Exists(b.name) {
		return nil, args.Error(fmt.Sprintf("block %q is already defined", b.name), nil)
	}
	p.Template.Blocks.Register(b.name, body)
	b.body = body
	return b, nil
}

func (b *jinjaBlock) Execute(r *exec.Renderer, _ *nodes.ControlStructureBlock) error {
	// The most derived version of the block comes first.
	return renderBlock(r, r.RootNode.GetBlocks(b.name))
}

func renderBlock(r *exec.Renderer, versions []*nodes.Wrapper) error {
	sub := r.Inherit()
	parents := versions[1:]
	sub.Environment.Context.Set("super", exec.Macro(func(*exec.VarArgs) *exec.Value {
		if len(parents) == 0 {
			return exec.AsValue(sqlFragment(""))
		}
		var out strings.Builder
		parent := sub.Inherit()
		parent.Output = &out
		if err := renderBlock(parent, parents); err != nil {
			return exec.AsValue(err)
		}
		return exec.AsValue(sqlFragment(out.String()))
	}))
	return sub.ExecuteWrapper(versions[0])
}

// jinjaLoader serves the template being parsed and, with jinja_includes
// enabled, the templates it extends, recording the source of each so the
// template cache can tell when they change. Included and imported
// templates are loaded at render time instead, through jinjaRun.
type jinjaLoader struct {
	ctx    context.Context
	cache  *templateCache // nil unless jinja_includes is enabled
	name   string
	src    string
	deps   map[string]string
	reads  int
	closed bool
}

var _ loaders.Loader = (*jinjaLoader)(nil)

func (l *jinjaLoader) Read(name string) (io.Reader, error) {
	if l.closed {
		return nil, errors.New("templates can only be loaded while parsing")
	}
	if l.reads++; l.reads > maxJinjaExtends {
		return nil, fmt.Errorf("more than %d templates extended; check for an extends cycle", maxJinjaExtends)
	}
	if name == l.name && l.reads == 1 {
		return strings.NewReader(l.src), nil
	}
	if l.cache == nil {
		return nil, fmt.Errorf("loading other templates (%q) is not supported in SQL templates unless jinja_includes is enabled", name)
	}
	src, err := l.cache.read(l.ctx, name)
	if err != nil {
		return nil, err
	}
	l.deps[name] = src
	return strings.NewReader(src), nil
}

func (l *jinjaLoader) Resolve(name string) (string, error) { return name, nil }

func (l *jinjaLoader) Inherit(string) (loaders.Loader, error) { return l, nil }
