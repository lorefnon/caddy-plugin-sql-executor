package sqlexecutor

import (
	"context"
	"io/fs"
	"net/http"
	"slices"
	"strings"
)

// This file exposes how the handler maps files to routes and compiles
// queries, for tools that describe the API, such as cmd/sql-openapi.

// QueryFile is a file under the query root that backs a route.
type QueryFile struct {
	// Route is the request path without the leading slash, e.g.
	// "products/by_id".
	Route string
	// File is the file's path relative to the root, e.g.
	// "products/by_id.sql".
	File string
	// Template reports whether File is a Jinja template (.sql.j2).
	Template bool
}

// ListQueryFiles returns the routes served from a local query root, sorted
// by route. Where both name.sql and name.sql.j2 exist, the handler serves
// name.sql, and so only it is listed.
func ListQueryFiles(root string) ([]QueryFile, error) {
	src, err := newDirSource(root)
	if err != nil {
		return nil, err
	}
	routes := map[string]QueryFile{}
	err = fs.WalkDir(src.fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if name != "." && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || !validQueryFile(name) {
			return nil
		}
		f := QueryFile{File: name, Template: isTemplate(name)}
		var ok bool
		if f.Route, ok = strings.CutSuffix(name, ".sql"); !ok {
			if f.Route, ok = strings.CutSuffix(name, ".sql"+jinjaExt); !ok {
				// A template loaded by others, e.g. partials/filters.j2.
				return nil
			}
		}
		if prev, dup := routes[f.Route]; !dup || prev.Template {
			routes[f.Route] = f
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	files := make([]QueryFile, 0, len(routes))
	for _, f := range routes {
		files = append(files, f)
	}
	slices.SortFunc(files, func(a, b QueryFile) int { return strings.Compare(a.Route, b.Route) })
	return files, nil
}

// Compiler compiles the query files under a local root as the handler
// would, without binding or running them.
type Compiler struct {
	source    *dirSource
	templates *templateCache
	style     placeholderStyle
}

// NewCompiler returns a Compiler for the files under root, compiled for
// driver ("postgres" or "sqlite"). jinjaIncludes corresponds to the
// handler's jinja_includes option.
func NewCompiler(root, driver string, jinjaIncludes bool) (*Compiler, error) {
	_, style, err := parseDriver(driver)
	if err != nil {
		return nil, err
	}
	src, err := newDirSource(root)
	if err != nil {
		return nil, err
	}
	c := &Compiler{source: src, templates: newTemplateCache(nil), style: style}
	if jinjaIncludes {
		c.templates = newTemplateCache(src)
	}
	return c, nil
}

// CompiledQuery is a query file compiled to SQL for its driver.
type CompiledQuery struct {
	// SQL uses the driver's positional placeholders ($1 or ?).
	SQL string
	// Params names the parameter bound to each placeholder, in order.
	// Names for which IsGenerated reports true hold template values.
	Params []string
	// Headers maps parameters to the request headers declared to supply
	// them with -- @header directives.
	Headers map[string]string
	// HeadersRead lists the headers a template read with header(...),
	// in the order first read.
	HeadersRead []string
	// Directives holds the text of each -- comment starting with @, such
	// as "@column total = integer", in order. The handler ignores all but
	// @header.
	Directives []string
}

// IsGenerated reports whether a parameter name was generated for a
// {{ value }} expression in a template rather than written as :name.
func IsGenerated(param string) bool {
	return strings.HasPrefix(param, boundParamPrefix)
}

// Compile compiles the query file named by file (relative to the root).
// A template is first rendered with params and header, as if they came
// from a request.
func (c *Compiler) Compile(ctx context.Context, file string, params map[string]any, header http.Header) (*CompiledQuery, error) {
	query, err := c.source.Get(ctx, file)
	if err != nil {
		return nil, err
	}
	if header == nil {
		header = http.Header{}
	}
	var read []string
	onHeader := func(name string) {
		name = http.CanonicalHeaderKey(name)
		if !slices.Contains(read, name) {
			read = append(read, name)
		}
	}
	stmt, _, err := compileQuery(ctx, c.templates, c.style, file, query, params, header, onHeader)
	if err != nil {
		return nil, err
	}
	return &CompiledQuery{SQL: stmt.SQL, Params: stmt.Params, Headers: stmt.Headers, HeadersRead: read, Directives: stmt.Directives}, nil
}

// Source returns the text of the query file named by file.
func (c *Compiler) Source(ctx context.Context, file string) (string, error) {
	return c.source.Get(ctx, file)
}
