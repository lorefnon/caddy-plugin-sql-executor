package sqlexecutor

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// newIncludeCache returns a template cache with jinja_includes enabled,
// serving files from a temporary query root.
func newIncludeCache(t *testing.T, files map[string]string) (*templateCache, string) {
	t.Helper()
	dir := t.TempDir()
	writeTree(t, dir, files)
	src, err := newDirSource(dir)
	if err != nil {
		t.Fatal(err)
	}
	return newTemplateCache(src), dir
}

func renderFile(t *testing.T, c *templateCache, name string, params map[string]any) (string, map[string]any, error) {
	t.Helper()
	tpl, err := c.jinja(t.Context(), name)
	if err != nil {
		return "", nil, err
	}
	return tpl.render(t.Context(), params, nil)
}

func TestJinjaIncludes(t *testing.T) {
	c, _ := newIncludeCache(t, map[string]string{
		"partials/where.j2": `WHERE name = {{ name }}{% set leaked = 1 %}`,
		"partials/limit.j2": ` LIMIT {{ n }}`,
		"macros.j2": `{% macro between(col, lo, hi) %}{{ ident(col) }} BETWEEN {{ lo }} AND {{ hi }}{% endmacro %}
{{ "top-level output of an imported template is not rendered" }}`,
		"base.j2": `SELECT id FROM users WHERE {% block where %}active = {{ true }}{% endblock %} ORDER BY id`,

		"include.sql.j2": `SELECT * FROM users {% include "partials/where.j2" %}{% if leaked %} LEAK{% endif %}` +
			`{% include "partials/" ~ kind ~ ".j2" ignore missing %}`,
		"macro.sql.j2": `{% macro eq(col, v) %}{{ ident(col) }} = {{ v }}{% endmacro -%}
SELECT 1 WHERE {{ eq("name", name) }} AND {{ eq("id", id) | upper }}`,
		"import.sql.j2": `{% import "macros.j2" as m %}{% from "macros.j2" import between as b -%}
SELECT 1 WHERE {{ m.between("score", lo, hi) }} OR {{ b("id", 1, 2) }}`,
		"extends.sql.j2": `{% extends "base.j2" %}{% block where %}{{ super() }} AND name = {{ name }}{% endblock %}`,
		"safe.sql.j2":    `SELECT {{ name | safe }}`,
		// Imports and assignments outside blocks are visible inside them.
		"toplevel.sql.j2": `{% extends "base.j2" %}{% import "macros.j2" as m %}{% set col = "score" %}
ignored text {{ ignored }}
{% block where %}{{ m.between(col, 1, 2) }}{% endblock %}`,
	})
	injection := "x' OR '1'='1"

	tests := []struct {
		name      string
		params    map[string]any
		wantSQL   string
		wantBound map[string]any
	}{{
		// Included templates see the includer's variables but not the
		// other way round, and their output is bound like any other.
		name: "include.sql.j2", params: map[string]any{"name": injection, "kind": "limit", "n": int64(5)},
		wantSQL:   `SELECT * FROM users WHERE name = :__tpl_1 LIMIT :__tpl_2`,
		wantBound: map[string]any{"__tpl_1": injection, "__tpl_2": 5},
	}, {
		name: "include.sql.j2", params: map[string]any{"name": "a", "kind": "missing"},
		wantSQL:   `SELECT * FROM users WHERE name = :__tpl_1`,
		wantBound: map[string]any{"__tpl_1": "a"},
	}, {
		// Macro output is SQL, but a filtered macro result is just a string,
		// so it is bound.
		name: "macro.sql.j2", params: map[string]any{"name": injection, "id": "7"},
		wantSQL:   `SELECT 1 WHERE "name" = :__tpl_1 AND :__tpl_3`,
		wantBound: map[string]any{"__tpl_1": injection, "__tpl_2": "7", "__tpl_3": `"ID" = :__TPL_2`},
	}, {
		name: "import.sql.j2", params: map[string]any{"lo": int64(1), "hi": injection},
		wantSQL:   `SELECT 1 WHERE "score" BETWEEN :__tpl_1 AND :__tpl_2 OR "id" BETWEEN :__tpl_3 AND :__tpl_4`,
		wantBound: map[string]any{"__tpl_1": 1, "__tpl_2": injection, "__tpl_3": 1, "__tpl_4": 2},
	}, {
		name: "extends.sql.j2", params: map[string]any{"name": injection},
		wantSQL:   `SELECT id FROM users WHERE active = :__tpl_1 AND name = :__tpl_2 ORDER BY id`,
		wantBound: map[string]any{"__tpl_1": true, "__tpl_2": injection},
	}, {
		// Only macro output counts as SQL, not values marked safe.
		name: "safe.sql.j2", params: map[string]any{"name": injection},
		wantSQL:   `SELECT :__tpl_1`,
		wantBound: map[string]any{"__tpl_1": injection},
	}, {
		name:      "toplevel.sql.j2",
		wantSQL:   `SELECT id FROM users WHERE "score" BETWEEN :__tpl_1 AND :__tpl_2 ORDER BY id`,
		wantBound: map[string]any{"__tpl_1": 1, "__tpl_2": 2},
	}}
	for _, tt := range tests {
		sql, bound, err := renderFile(t, c, tt.name, tt.params)
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if sql != tt.wantSQL || !reflect.DeepEqual(bound, tt.wantBound) {
			t.Errorf("%s:\n got %q %#v\nwant %q %#v", tt.name, sql, bound, tt.wantSQL, tt.wantBound)
		}
	}
}

func TestJinjaIncludeErrors(t *testing.T) {
	c, _ := newIncludeCache(t, map[string]string{
		"secret.sql":       "SELECT 'not a template'",
		".hidden.j2":       "hidden",
		"unsafe.j2":        `{% filter upper %}{{ x }}{% endfilter %}`,
		"cycle_a.j2":       `{% extends "cycle_b.j2" %}`,
		"cycle_b.j2":       `{% extends "cycle_a.j2" %}`,
		"self.sql.j2":      `x{% include "self.sql.j2" %}`,
		"recurse.sql.j2":   `{% macro r() %}{{ r() }}{% endmacro %}{{ r() }}`,
		"dynamic.sql.j2":   `{% include name %}`,
		"missing.sql.j2":   `{% include "nope.j2" %}`,
		"unsafe.sql.j2":    `{% include "unsafe.j2" %}`,
		"cycle.sql.j2":     `{% extends "cycle_a.j2" %}`,
		"plain.j2":         "no macros here",
		"no_macro.sql.j2":  `{% from "plain.j2" import m %}`,
		"call.sql.j2":      `{% macro m() %}{{ caller() }}{% endmacro %}{% call m() %}x{% endcall %}`,
		"no_parent.sql.j2": `{% extends "nope.j2" %}`,
	})
	tests := []struct {
		name   string
		params map[string]any
		want   string
	}{
		{"dynamic.sql.j2", map[string]any{"name": "../etc/passwd.j2"}, `cannot load template "../etc/passwd.j2"`},
		{"dynamic.sql.j2", map[string]any{"name": "secret.sql"}, `cannot load template "secret.sql"`},
		{"dynamic.sql.j2", map[string]any{"name": ".hidden.j2"}, `cannot load template ".hidden.j2"`},
		{"dynamic.sql.j2", map[string]any{"name": int64(1)}, "template name must be a string"},
		{"missing.sql.j2", nil, `loading template "nope.j2"`},
		{"unsafe.sql.j2", nil, "{% filter %} is not supported"},
		{"cycle.sql.j2", nil, "extends cycle"},
		{"self.sql.j2", nil, "nest more than 64"},
		{"recurse.sql.j2", nil, "nest more than 64"},
		{"no_macro.sql.j2", nil, `does not define a macro named "m"`},
		{"call.sql.j2", nil, "{% call %} is not supported"},
		{"no_parent.sql.j2", nil, `loading template "nope.j2"`},
	}
	for _, tt := range tests {
		_, _, err := renderFile(t, c, tt.name, tt.params)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s %v: got %v, want error containing %q", tt.name, tt.params, err, tt.want)
		}
	}
}

func TestJinjaIncludesPickUpEdits(t *testing.T) {
	c, dir := newIncludeCache(t, map[string]string{
		"base.j2":        `SELECT {% block cols %}1{% endblock %} AS a`,
		"part.j2":        `FROM t`,
		"child.sql.j2":   `{% extends "base.j2" %}{% block cols %}2{% endblock %} {% include "part.j2" %}`,
		"orphan.sql.j2":  `{% extends "later.j2" %}`,
		"partial.sql.j2": `{% include "part.j2" %}`,
	})
	write := func(name, src string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	render := func(name string) string {
		t.Helper()
		sql, _, err := renderFile(t, c, name, nil)
		if err != nil {
			return "error: " + err.Error()
		}
		return sql
	}

	if got := render("child.sql.j2"); got != "SELECT 2 AS a" {
		t.Fatalf("got %q", got)
	}
	write("base.j2", `SELECT {% block cols %}1{% endblock %} AS b {% block from %}{% endblock %}`)
	write("child.sql.j2", `{% extends "base.j2" %}{% block cols %}2{% endblock %}{% block from %}{% include "part.j2" %}{% endblock %}`)
	if got := render("child.sql.j2"); got != "SELECT 2 AS b FROM t" {
		t.Errorf("after editing the parent: %q", got)
	}
	write("base.j2", `SELECT {% block cols %}1{% endblock %} AS c {% block from %}{% endblock %}`)
	if got := render("child.sql.j2"); got != "SELECT 2 AS c FROM t" {
		t.Errorf("after editing only the parent: %q", got)
	}
	write("part.j2", `FROM u`)
	if got := render("partial.sql.j2"); got != "FROM u" {
		t.Errorf("after editing an include: %q", got)
	}

	if got := render("orphan.sql.j2"); !strings.Contains(got, "error") {
		t.Fatalf("missing parent: %q", got)
	}
	write("later.j2", `SELECT 3`)
	if got := render("orphan.sql.j2"); got != "SELECT 3" {
		t.Errorf("after adding the parent: %q", got)
	}
}

func TestJinjaIncludesHandler(t *testing.T) {
	h := newSQLiteHandler(t, func(h *SQLExecutor) {
		h.VerboseErrors = true
		h.JinjaIncludes = true
	})
	writeTree(t, h.Root, map[string]string{
		"partials/filters.j2": `{% if name %} AND name = {{ name }}{% endif %}`,
		"layouts/list.j2":     `SELECT id, name FROM users WHERE 1 = 1{% block filters %}{% endblock %} ORDER BY {% block order %}id{% endblock %}`,
		"people.sql.j2": `{% extends "layouts/list.j2" %}
{%- block filters %}{% include "partials/filters.j2" %}{% endblock %}
{%- block order %}{{ ident(sort, allow=["id", "name"]) }} DESC{% endblock %}`,
	})

	res := serve(t, h, "POST", "/people", `{"sort": "name"}`, nil)
	if res.status != 200 || res.body != `[{"id":2,"name":"Grace, \"Amazing\""},{"id":1,"name":"Ada"}]`+"\n" {
		t.Errorf("all: %d %s", res.status, res.body)
	}
	res = serve(t, h, "POST", "/people", `{"sort": "id", "name": "Ada"}`, nil)
	if res.status != 200 || res.body != `[{"id":1,"name":"Ada"}]`+"\n" {
		t.Errorf("filtered: %d %s", res.status, res.body)
	}
	res = serve(t, h, "POST", "/people", `{"sort": "id", "name": "x' OR '1'='1"}`, nil)
	if res.status != 200 || res.body != "[]\n" {
		t.Errorf("injection: %d %s", res.status, res.body)
	}
	// Templates without .sql in their name are not routes.
	for _, target := range []string{"/partials/filters", "/partials/filters.j2", "/layouts/list"} {
		if res := serve(t, h, "POST", target, "", nil); res.nextCalls != 1 {
			t.Errorf("%s: expected fall-through, got %d %s", target, res.status, res.body)
		}
	}
}
