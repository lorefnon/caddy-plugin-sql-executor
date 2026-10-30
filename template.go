package sqlexecutor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// boundParamPrefix names the placeholders generated for {{ value }}
// expressions in templates; request parameters with this prefix are
// discarded.
const boundParamPrefix = "__tpl_"

// isTemplate reports whether a query file name is a template.
func isTemplate(name string) bool {
	return strings.HasSuffix(name, jinjaExt)
}

// inputError reports a template failure caused by request data rather
// than by the template itself.
type inputError struct{ msg string }

func (e *inputError) Error() string { return e.msg }

// quoteIdent renders a value as a double-quoted SQL identifier, quoting
// each dot-separated part. With an allow list the value must be one of the
// listed names, which is the recommended way to accept identifiers such as
// sort columns from requests.
func quoteIdent(v any, allow []string) (string, error) {
	name, ok := v.(string)
	if !ok || name == "" {
		return "", fmt.Errorf("ident: expected a non-empty string, got %v", v)
	}
	if len(allow) > 0 && !slices.Contains(allow, name) {
		return "", fmt.Errorf("ident: %q is not one of %s", name, strings.Join(allow, ","))
	}
	if strings.ContainsRune(name, 0) {
		return "", errors.New("ident: identifier contains a NUL byte")
	}
	parts := strings.Split(name, ".")
	for i, p := range parts {
		parts[i] = `"` + strings.ReplaceAll(p, `"`, `""`) + `"`
	}
	return strings.Join(parts, "."), nil
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
// survive compilation, which happens when {{ value }} is written inside a
// quoted SQL string ('%{{ q }}%') and would silently match the literal text.
func checkBoundPlaceholders(stmt *statement, bound map[string]any) error {
	used := make(map[string]bool, len(stmt.Params))
	for _, p := range stmt.Params {
		used[p] = true
	}
	for name := range bound {
		if !used[name] {
			return errors.New("a template value appears inside a quoted SQL string or comment, " +
				"where it cannot be bound; build the string in SQL instead, e.g. '%' || {{ q }} || '%'")
		}
	}
	return nil
}

// templateCache keeps parsed templates keyed by file name, re-parsing
// whenever the source text changes, or that of a template it extends.
type templateCache struct {
	// source serves the templates that Jinja templates load; nil unless
	// jinja_includes is enabled.
	source source

	mu      sync.Mutex
	entries map[string]templateCacheEntry
}

type templateCacheEntry struct {
	src  string
	deps map[string]string // templates read while parsing, with their source
	tpl  *jinjaTemplate
	err  error
}

func newTemplateCache(src source) *templateCache {
	return &templateCache{source: src, entries: map[string]templateCacheEntry{}}
}

func (c *templateCache) get(ctx context.Context, name, src string) (*jinjaTemplate, error) {
	c.mu.Lock()
	e, ok := c.entries[name]
	c.mu.Unlock()
	if ok && e.src == src && c.current(ctx, e.deps) {
		return e.tpl, e.err
	}

	e = templateCacheEntry{src: src}
	var loader *templateCache
	if c.source != nil {
		loader = c
	}
	e.tpl, e.deps, e.err = parseJinjaTemplate(ctx, name, src, loader)
	// A failure may come from a template that is missing or unreadable for
	// now, so with loading enabled, failures are not cached.
	if e.err == nil || c.source == nil {
		c.mu.Lock()
		c.entries[name] = e
		c.mu.Unlock()
	}
	return e.tpl, e.err
}

// current reports whether the templates read while parsing are unchanged.
func (c *templateCache) current(ctx context.Context, deps map[string]string) bool {
	for name, want := range deps {
		if got, err := c.source.Get(ctx, name); err != nil || got != want {
			return false
		}
	}
	return true
}

// read returns the source of a template that another one loads.
func (c *templateCache) read(ctx context.Context, name string) (string, error) {
	if !strings.HasSuffix(name, jinjaExt) || !validQueryFile(name) {
		return "", fmt.Errorf("cannot load template %q: names are relative to the query root and end in %s", name, jinjaExt)
	}
	src, err := c.source.Get(ctx, name)
	if err != nil {
		return "", fmt.Errorf("loading template %q: %w", name, err)
	}
	return src, nil
}

// jinja returns the checked Jinja template that another one loads.
func (c *templateCache) jinja(ctx context.Context, name string) (*jinjaTemplate, error) {
	src, err := c.read(ctx, name)
	if err != nil {
		return nil, err
	}
	tpl, err := c.get(ctx, name, src)
	if err != nil {
		return nil, fmt.Errorf("in %s: %w", name, err)
	}
	return tpl, nil
}
