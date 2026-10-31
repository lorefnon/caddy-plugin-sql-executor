package main

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"

	sqlexecutor "github.com/lorefnon/caddy-plugin-sql-executor"
)

const xlsxMediaType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"

// generator builds the OpenAPI document for the files under a query root.
type generator struct {
	cfg      *config
	compiler *sqlexecutor.Compiler
	desc     describer
	used     map[string]bool
	ids      map[string]bool
}

// fileError is a failure to describe one query file.
type fileError struct {
	File string
	Err  error
}

func (e *fileError) Error() string { return e.File + ": " + e.Err.Error() }

// generate returns the document, plus an error for each file that could
// not be described. Those files are left out of the document.
func (g *generator) generate(ctx context.Context) (*document, []error, error) {
	files, err := sqlexecutor.ListQueryFiles(g.cfg.Root, g.cfg.VerbNamedFiles)
	if err != nil {
		return nil, nil, err
	}
	doc := &document{
		OpenAPI: "3.1.0",
		Info:    g.cfg.Info,
		Servers: g.cfg.Servers,
		Paths:   map[string]*pathItem{},
		Components: components{
			Schemas:   map[string]*Schema{"Error": errorSchema()},
			Responses: errorResponses(),
		},
	}
	if doc.Info.Title == "" {
		doc.Info.Title = "SQL API"
	}
	if doc.Info.Version == "" {
		doc.Info.Version = "1.0.0"
	}

	var errs []error
	g.used = map[string]bool{}
	g.ids = map[string]bool{}
	for _, f := range files {
		if err := g.add(ctx, doc, f); err != nil {
			errs = append(errs, &fileError{f.File, err})
		}
	}
	for _, key := range g.cfg.unused(g.used) {
		errs = append(errs, fmt.Errorf("queries: %s matches no query file", key))
	}
	return doc, errs, nil
}

// add describes a query file and adds its operations to doc.
func (g *generator) add(ctx context.Context, doc *document, f sqlexecutor.QueryFile) error {
	segs := f.Segments()
	for i, s := range segs {
		if (s.Kind == sqlexecutor.CatchAllSegment || s.Kind == sqlexecutor.OptionalCatchAllSegment) && i < len(segs)-1 {
			return fmt.Errorf("the handler never serves this route: [...%s] must be its last segment", s.Name)
		}
	}
	if !(&pathItem{}).set(f.Method, nil) {
		return fmt.Errorf("OpenAPI 3.1 has no operation for the %s method", f.Method)
	}

	id := g.operationID(f)
	op, row, err := g.operation(ctx, f, id)
	if err != nil {
		return err
	}
	doc.Components.Schemas[rowSchemaName(id)] = row

	g.pathItem(doc, "/"+openAPIPath(segs)).set(f.Method, op)
	// [[...x]] also matches the path without it, where x is an empty list.
	if last := segs[len(segs)-1]; last.Kind == sqlexecutor.OptionalCatchAllSegment {
		base := *op
		base.OperationID = g.uniqueID(id + "_none")
		base.Parameters = slices.DeleteFunc(slices.Clone(op.Parameters), func(p parameter) bool {
			return p.In == "path" && p.Name == last.Name
		})
		g.pathItem(doc, "/"+openAPIPath(segs[:len(segs)-1])).set(f.Method, &base)
	}
	return nil
}

func (g *generator) pathItem(doc *document, path string) *pathItem {
	item := doc.Paths[path]
	if item == nil {
		item = &pathItem{}
		doc.Paths[path] = item
	}
	return item
}

// openAPIPath writes route segments as an OpenAPI path: users/[id] becomes
// users/{id}.
func openAPIPath(segs []sqlexecutor.Segment) string {
	parts := make([]string, len(segs))
	for i, s := range segs {
		parts[i] = s.Name
		if s.Kind != sqlexecutor.StaticSegment {
			parts[i] = "{" + s.Name + "}"
		}
	}
	return strings.Join(parts, "/")
}

// operationID names a file's operation after its route, and after its
// method when routing is by method: users/[id] becomes users_id, and
// DELETE on it users_id_delete.
func (g *generator) operationID(f sqlexecutor.QueryFile) string {
	segs := f.Segments()
	names := make([]string, len(segs))
	for i, s := range segs {
		names[i] = s.Name
	}
	id := operationID(strings.Join(names, "/"))
	if g.cfg.VerbNamedFiles {
		id += "_" + strings.ToLower(f.Method)
	}
	return g.uniqueID(id)
}

// uniqueID returns id, or id with a number appended if it is taken, since
// two routes such as users/[id] and users/id would get the same one.
func (g *generator) uniqueID(id string) string {
	for n, candidate := 2, id; ; n++ {
		if !g.ids[candidate] {
			g.ids[candidate] = true
			return candidate
		}
		candidate = fmt.Sprintf("%s_%d", id, n)
	}
}

// variant is one way of running a query file: the file itself, or a
// template rendered with one sample.
type variant struct {
	sample map[string]any
	query  *sqlexecutor.CompiledQuery
	desc   *description
	notes  *annotations
}

func (g *generator) operation(ctx context.Context, f sqlexecutor.QueryFile, id string) (*operation, *Schema, error) {
	qc := g.cfg.query(f.File, f.Route, g.used)
	src, err := g.compiler.Source(ctx, f.File)
	if err != nil {
		return nil, nil, err
	}

	samples := qc.Samples
	if !f.Template {
		samples = []map[string]any{nil}
	}
	header := http.Header{}
	for k, v := range qc.Headers {
		header.Set(k, v)
	}
	var variants []variant
	for i, sample := range samples {
		v, err := g.variant(ctx, f, sample, header)
		if err != nil {
			if f.Template {
				return nil, nil, fmt.Errorf("sample %d: %w", i+1, err)
			}
			return nil, nil, err
		}
		variants = append(variants, v)
	}

	desc, err := operationDescription(src, f.Template, variants)
	if err != nil {
		return nil, nil, err
	}
	op := &operation{
		OperationID: id,
		Description: desc,
		Responses: map[string]*response{
			"200": {
				Description: "The rows the query returned.",
				Content:     resultContent(&Schema{Ref: "#/components/schemas/" + rowSchemaName(id)}),
			},
		},
	}
	for _, code := range []string{"400", "406", "413", "500", "504"} {
		op.Responses[code] = &response{Ref: "#/components/responses/" + errorResponseNames[code]}
	}

	body, headers, err := requestSchema(f, qc, variants)
	if err != nil {
		return nil, nil, err
	}
	row, err := rowSchema(qc, variants)
	if err != nil {
		return nil, nil, err
	}
	body, pathParams := splitPathParams(f, body)
	op.Parameters = pathParams
	if bodyless(f.Method) {
		// The handler reads the query string for every method. Methods
		// whose requests carry no body get their parameters there.
		for _, p := range body.Properties {
			op.Parameters = append(op.Parameters, parameter{
				Name: p.Name, In: "query", Required: slices.Contains(body.Required, p.Name), Schema: textSchema(p.Schema),
			})
		}
	} else if len(body.Properties) > 0 || (f.Template && len(variants) == 0) {
		op.RequestBody = &requestBody{
			Required: len(body.Required) > 0,
			Content: map[string]mediaType{
				"application/json":                  {Schema: body},
				"application/x-www-form-urlencoded": {Schema: body},
			},
		}
	}
	op.Parameters = append(op.Parameters, headers...)
	return op, row, nil
}

// bodyless reports whether OpenAPI advises against a request body for
// the method.
func bodyless(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodDelete ||
		method == http.MethodOptions || method == http.MethodTrace
}

// splitPathParams moves the parameters captured from the path, such as id
// for users/[id], out of the request body schema and into path parameters.
// The path supplies these, so they are never in the body, and are always
// required.
func splitPathParams(f sqlexecutor.QueryFile, body *Schema) (*Schema, []parameter) {
	var params []parameter
	rest := *body
	rest.Properties = slices.Clone(body.Properties)
	rest.Required = slices.Clone(body.Required)
	for _, seg := range f.Segments() {
		if seg.Kind == sqlexecutor.StaticSegment {
			continue
		}
		p := parameter{Name: seg.Name, In: "path", Required: true}
		if seg.Kind == sqlexecutor.ParamSegment {
			s, _ := body.Properties.get(seg.Name)
			p.Schema = textSchema(s)
		} else {
			p.Schema = typed("string", "")
			p.Description = "Path segments separated by /. The query receives them as a JSON array."
		}
		params = append(params, p)
		rest.Properties = slices.DeleteFunc(rest.Properties, func(pr Property) bool { return pr.Name == seg.Name })
		rest.Required = slices.DeleteFunc(rest.Required, func(n string) bool { return n == seg.Name })
	}
	return &rest, params
}

// textSchema describes a value sent as text, in the path or query string:
// it can't be null or structured. Where it is, the value is described as
// the string the handler is given. Unknown types are strings.
func textSchema(s *Schema) *Schema {
	if s == nil {
		return typed("string", "")
	}
	ts := s.types()
	if len(ts) == 0 {
		return typed("string", "")
	}
	if slices.Contains(ts, "array") || slices.Contains(ts, "object") {
		return &Schema{Type: "string", Description: "JSON text."}
	}
	if !slices.Contains(ts, "null") {
		return s
	}
	c := *s
	c.setTypes(withoutNull(ts))
	c.Enum = slices.DeleteFunc(slices.Clone(s.Enum), func(v any) bool { return v == nil })
	if len(c.Enum) == 0 {
		c.Enum = nil
	}
	return &c
}

// variant compiles and describes a query file, rendered with sample if it
// is a template.
func (g *generator) variant(ctx context.Context, f sqlexecutor.QueryFile, sample map[string]any, header http.Header) (variant, error) {
	q, err := g.compiler.Compile(ctx, f.File, sample, header)
	if err != nil {
		return variant{}, err
	}
	notes, err := parseAnnotations(q.Directives)
	if err != nil {
		return variant{}, err
	}
	d, err := g.desc.Describe(ctx, q.SQL, len(q.Params))
	if err != nil {
		return variant{}, err
	}
	return variant{sample, q, d, notes}, nil
}

// requestSchema describes the request body and headers. The parameters of
// a plain query are its :name placeholders, all required. Those of a
// template are the names its samples use, typed by their sample values,
// plus any :name placeholders the rendered SQL contains. @param
// annotations, then the config, refine the inferred types.
func requestSchema(f sqlexecutor.QueryFile, qc *queryConfig, variants []variant) (*Schema, []parameter, error) {
	body := typed("object", "")
	params := map[string]*Schema{}
	seen := map[string]bool{}
	var names, required []string
	add := func(name string, s *Schema) {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
			params[name] = s
		} else {
			params[name] = union(params[name], s)
		}
	}

	declared := map[string]*Schema{}
	var headers []parameter
	headerSeen := map[string]bool{}
	addHeader := func(p parameter) {
		key := http.CanonicalHeaderKey(p.Name)
		if !headerSeen[key] {
			headerSeen[key] = true
			headers = append(headers, p)
		}
	}

	for _, v := range variants {
		if f.Template {
			for _, k := range sortedKeys(v.sample) {
				add(k, valueSchema(v.sample[k]))
			}
		}
		// A name used more than once (SQLite has a placeholder for each use)
		// takes the types found for any of its uses.
		var placeholders []string
		types := map[string]*Schema{}
		for i, name := range v.query.Params {
			if sqlexecutor.IsGenerated(name) {
				continue
			}
			if !slices.Contains(placeholders, name) {
				placeholders = append(placeholders, name)
			}
			if i < len(v.desc.Params) {
				types[name] = unionItems(types[name], v.desc.Params[i])
			}
		}
		for _, name := range placeholders {
			if h, ok := v.query.Headers[name]; ok {
				addHeader(parameter{
					Name: h, In: "header", Required: true, Schema: typed("string", ""),
					Description: fmt.Sprintf("Supplies :%s.", name),
				})
				continue
			}
			// Placeholders must always be sent; null is allowed.
			add(name, nullable(types[name]))
			if !slices.Contains(required, name) {
				required = append(required, name)
			}
		}
		for _, h := range v.query.HeadersRead {
			addHeader(parameter{Name: h, In: "header", Schema: typed("string", "")})
		}
		for _, name := range sortedKeys(v.notes.params) {
			s := v.notes.params[name]
			_, sampled := v.sample[name]
			switch {
			case v.query.Headers[name] != "":
				return nil, nil, fmt.Errorf("@param :%s: the value comes from the %s header", name, v.query.Headers[name])
			case !slices.Contains(placeholders, name) && !(f.Template && sampled):
				return nil, nil, fmt.Errorf("@param :%s: the query has no parameter :%s", name, name)
			}
			if prev, ok := declared[name]; ok && !equalSchema(prev, s) {
				return nil, nil, fmt.Errorf("@param :%s differs between samples", name)
			}
			declared[name] = s
		}
	}
	for _, name := range names {
		s := override(override(params[name], declared[name]), qc.Params[name])
		body.Properties = append(body.Properties, Property{name, s})
	}
	body.Required = required
	return body, headers, nil
}

// rowSchema describes a result row. Templates whose samples produce
// different columns get a union of the row shapes. @column annotations,
// then the config, refine the inferred types.
func rowSchema(qc *queryConfig, variants []variant) (*Schema, error) {
	var rows []*Schema
	for _, v := range variants {
		for _, name := range sortedKeys(v.notes.columns) {
			if !slices.ContainsFunc(v.desc.Columns, func(c field) bool { return c.Name == name }) {
				return nil, fmt.Errorf("@column %s: the query has no column %s", name, name)
			}
		}
		row := typed("object", "")
		for _, c := range v.desc.Columns {
			s := c.Schema
			if c.Nullable {
				s = nullable(s)
			}
			s = override(override(s, v.notes.columns[c.Name]), qc.Columns[c.Name])
			// Columns with the same name are all written to the object, and
			// which one a JSON parser keeps varies, so either may be seen.
			if i := slices.IndexFunc(row.Properties, func(p Property) bool { return p.Name == c.Name }); i >= 0 {
				row.Properties[i].Schema = union(row.Properties[i].Schema, s)
				continue
			}
			row.Properties = append(row.Properties, Property{c.Name, s})
			row.Required = append(row.Required, c.Name)
		}
		if !slices.ContainsFunc(rows, func(r *Schema) bool { return equalSchema(r, row) }) {
			rows = append(rows, row)
		}
	}
	switch len(rows) {
	case 0:
		return typed("object", ""), nil
	case 1:
		return rows[0], nil
	}
	return &Schema{AnyOf: rows}, nil
}

func resultContent(row *Schema) map[string]mediaType {
	rows := &Schema{Type: "array", Items: row}
	return map[string]mediaType{
		"application/json": {Schema: rows},
		"application/yaml": {Schema: rows},
		"text/csv":         {Schema: typed("string", "")},
		xlsxMediaType:      {Schema: typed("string", "binary")},
	}
}

func errorSchema() *Schema {
	return &Schema{
		Type:       "object",
		Properties: Properties{{"error", typed("string", "")}},
		Required:   []string{"error"},
	}
}

var errorResponseNames = map[string]string{
	"400": "BadRequest",
	"406": "NotAcceptable",
	"413": "PayloadTooLarge",
	"500": "QueryFailed",
	"504": "Timeout",
}

func errorResponses() map[string]*response {
	descriptions := map[string]string{
		"400": "Missing parameters, a malformed body, or an unsupported content type.",
		"406": "None of the accepted media types can be produced.",
		"413": "The request body is too large.",
		"500": "The query failed.",
		"504": "The query timed out.",
	}
	out := map[string]*response{}
	for code, name := range errorResponseNames {
		out[name] = &response{
			Description: descriptions[code],
			Content:     map[string]mediaType{"application/json": {Schema: &Schema{Ref: "#/components/schemas/Error"}}},
		}
	}
	return out
}

// operationID turns a route such as products/by_id into products_by_id.
func operationID(route string) string {
	return strings.Map(func(r rune) rune {
		if r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, route)
}

// rowSchemaName turns products_by_id into ProductsByIdRow.
func rowSchemaName(id string) string {
	var b strings.Builder
	for _, part := range strings.Split(id, "_") {
		if part != "" {
			b.WriteString(strings.ToUpper(part[:1]) + part[1:])
		}
	}
	return b.String() + "Row"
}

// operationDescription returns an operation's description: its
// @description lines, or else the file's leading comment.
func operationDescription(src string, template bool, variants []variant) (string, error) {
	var lines []string
	for i, v := range variants {
		if i > 0 && !slices.Equal(v.notes.description, lines) {
			return "", fmt.Errorf("@description differs between samples")
		}
		lines = v.notes.description
	}
	if len(lines) == 0 {
		return leadingComment(src, template), nil
	}
	return strings.TrimSpace(strings.Join(lines, "\n")), nil
}

// leadingComment returns the comment a query file starts with: its leading
// -- lines (other than @header directives), or a template's opening {# #}.
func leadingComment(src string, template bool) string {
	src = strings.TrimSpace(src)
	if template {
		rest, ok := strings.CutPrefix(src, "{#")
		if !ok {
			return ""
		}
		text, _, ok := strings.Cut(rest, "#}")
		if !ok {
			return ""
		}
		return dedent(text)
	}
	var lines []string
	for _, line := range strings.Split(src, "\n") {
		text, ok := strings.CutPrefix(strings.TrimSpace(line), "--")
		if !ok {
			break
		}
		text = strings.TrimSpace(text)
		if isDirective(text) {
			continue
		}
		lines = append(lines, text)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func dedent(text string) string {
	lines := strings.Split(strings.Trim(text, "\n"), "\n")
	indent := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, " \t"))
		if indent < 0 || n < indent {
			indent = n
		}
	}
	for i, l := range lines {
		if len(l) >= indent && indent > 0 {
			lines[i] = l[indent:]
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
