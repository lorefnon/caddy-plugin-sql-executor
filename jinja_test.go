package sqlexecutor

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func renderJinja(t *testing.T, src string, params map[string]any, header http.Header) (string, map[string]any, error) {
	t.Helper()
	tpl, _, err := parseJinjaTemplate(t.Context(), "test.sql.j2", src, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return tpl.render(t.Context(), params, header)
}

func TestJinjaRender(t *testing.T) {
	header := http.Header{}
	header.Set("X-User-Id", "7")
	sql, bound, err := renderJinja(t, `SELECT * FROM t WHERE 1=1
{%- if name %} AND name = {{ name }}{% endif %}
{%- if ids %} AND id IN ({% for id in ids %}{{ id }}{% if not loop.last %}, {% endif %}{% endfor %}){% endif %}
{%- set lim = limit | default(10) %}
 AND owner = {{ header("X-User-Id") }} AND gone = {{ missing }} AND pick = {{ a if flag else b }} AND up = {{ name | upper }}
ORDER BY {{ ident(sort, allow=["id", "name"]) }} LIMIT {{ lim }}`,
		map[string]any{
			"name": "O'Brien <b>", "ids": []any{int64(1), int64(2)}, "sort": "name",
			"flag": false, "a": "A", "b": int64(5),
			// Reserved names are shadowed by the plugin's own functions.
			"header": "spoofed", "ident": "spoofed", jinjaBindFunc: "spoofed",
		}, header)
	if err != nil {
		t.Fatal(err)
	}
	wantSQL := `SELECT * FROM t WHERE 1=1 AND name = :__tpl_1 AND id IN (:__tpl_2, :__tpl_3)
 AND owner = :__tpl_4 AND gone = :__tpl_5 AND pick = :__tpl_6 AND up = :__tpl_7
ORDER BY "name" LIMIT :__tpl_8`
	if sql != wantSQL {
		t.Errorf("sql\n got: %s\nwant: %s", sql, wantSQL)
	}
	wantBound := map[string]any{
		"__tpl_1": "O'Brien <b>", // verbatim: no quoting, no HTML escaping
		"__tpl_2": 1, "__tpl_3": 2,
		"__tpl_4": "7",
		"__tpl_5": nil,
		"__tpl_6": 5,
		"__tpl_7": "O'BRIEN <B>",
		"__tpl_8": 10,
	}
	if !reflect.DeepEqual(bound, wantBound) {
		t.Errorf("bound %#v\nwant  %#v", bound, wantBound)
	}
}

// Every output position must be bound, however deeply it is nested.
func TestJinjaWrapsNestedOutputs(t *testing.T) {
	sql, bound, err := renderJinja(t, `{% for x in xs %}{% if x > 1 %}{{ x }}{% elif x == 1 %}{{ x }}{% else %}{{ x }}{% endif %}{% else %}{{ none_left }}{% endfor %}`+
		`{% for y in empty %}{% else %}{{ "empty" }}{% endfor %}`+
		`{% autoescape true %}{{ "<x>" }}{% endautoescape %}{% raw %}{{ literal }}{% endraw %}{{ [1, 2] }}{{ {"k": "v"} }}`,
		map[string]any{"xs": []any{int64(0), int64(1), int64(2)}, "empty": []any{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := ":__tpl_1:__tpl_2:__tpl_3:__tpl_4:__tpl_5{{ literal }}:__tpl_6:__tpl_7"; sql != want {
		t.Errorf("sql\n got: %s\nwant: %s", sql, want)
	}
	want := map[string]any{
		"__tpl_1": 0, "__tpl_2": 1, "__tpl_3": 2, "__tpl_4": "empty", "__tpl_5": "<x>",
		"__tpl_6": []any{1, 2}, "__tpl_7": map[string]any{"k": "v"},
	}
	if !reflect.DeepEqual(bound, want) {
		t.Errorf("bound %#v\nwant  %#v", bound, want)
	}
}

func TestJinjaAllowedStatements(t *testing.T) {
	sql, bound, err := renderJinja(t, `{% set xs = [] %}{% do xs.append(9) %}`+
		`{% for x in [1, 2, 3, 4] %}{% if x == 2 %}{% continue %}{% endif %}{% if x == 4 %}{% break %}{% endif %}{{ x }},{% endfor %}{{ xs }}`,
		nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sql != ":__tpl_1,:__tpl_2,:__tpl_3" || !reflect.DeepEqual(bound, map[string]any{"__tpl_1": 1, "__tpl_2": 3, "__tpl_3": []any{9}}) {
		t.Errorf("got %q %#v", sql, bound)
	}
}

func TestJinjaIdent(t *testing.T) {
	ok := map[string]string{
		`{{ ident(v) }}`:                          `"public"."users"`,
		`{{ ident(v, allow=["public.users"]) }}`:  `"public"."users"`,
		`{{ ident(v, allow="x, public.users") }}`: `"public"."users"`,
	}
	for src, want := range ok {
		got, bound, err := renderJinja(t, src, map[string]any{"v": "public.users"}, nil)
		if err != nil || got != want || len(bound) != 0 {
			t.Errorf("%s = %q %v %v, want %q", src, got, bound, err, want)
		}
	}

	// Client input that fails validation is the client's error (400).
	clientErrs := map[string]string{
		`{{ ident(v, allow=["a", "b"]) }}`: `"pw" is not one of a,b`,
		`{{ ident(v, allow="a") }}`:        `"pw" is not one of a`,
		`{{ ident(n) }}`:                   "expected a non-empty string",
		`{{ ident(missing) }}`:             "expected a non-empty string",
	}
	for src, wantMsg := range clientErrs {
		_, _, err := renderJinja(t, src, map[string]any{"v": "pw", "n": int64(3)}, nil)
		var inputErr *inputError
		if !errors.As(err, &inputErr) || !strings.Contains(err.Error(), wantMsg) {
			t.Errorf("%s: got %v, want input error containing %q", src, err, wantMsg)
		}
	}

	// A malformed allow list is the template author's error (500), and
	// never means "no restriction".
	authorErrs := map[string]string{
		`{{ ident(v, allow=3) }}`:   "allow must be a list",
		`{{ ident(v, allow=[1]) }}`: "entries must be strings",
		`{{ ident(v, allow=[]) }}`:  "allow list is empty",
	}
	for src, wantMsg := range authorErrs {
		_, _, err := renderJinja(t, src, map[string]any{"v": "a"}, nil)
		var inputErr *inputError
		if err == nil || errors.As(err, &inputErr) || !strings.Contains(err.Error(), wantMsg) {
			t.Errorf("%s: got %v, want non-input error containing %q", src, err, wantMsg)
		}
	}
}

func TestJinjaRejects(t *testing.T) {
	tests := map[string]string{
		`{% include "/etc/passwd" %}`:                    "{% include %} is not supported in SQL templates unless jinja_includes",
		`{% extends "base.sql" %}`:                       "loading other templates",
		`{% import "x" as y %}`:                          "{% import %} is not supported",
		`{% from "x" import y %}`:                        "{% from %} is not supported",
		`{% macro m() %}x{% endmacro %}`:                 "{% macro %} is not supported in SQL templates unless jinja_includes is enabled",
		"SELECT 1\n{% with a = 1 %}{{ a }}{% endwith %}": "line 2: {% with %} is not supported",
		`{% filter upper %}{{ a }}{% endfilter %}`:       "{% filter %} is not supported",
		`{% set x %}{{ a }}{% endset %}`:                 "{% set %} blocks are not supported",
		`{% block b %}{{ a }}{% endblock %}`:             "{% block %} is not supported",
		`{% call m() %}x{% endcall %}`:                   "{% call %} is not supported",
		`{% if a %}`:                                     "",
	}
	for src, wantMsg := range tests {
		_, _, err := parseJinjaTemplate(t.Context(), "test.sql.j2", src, nil)
		if err == nil || !strings.Contains(err.Error(), wantMsg) {
			t.Errorf("parse(%q) = %v, want error containing %q", src, err, wantMsg)
		}
	}
}

func TestJinjaHeaders(t *testing.T) {
	header := http.Header{}
	header.Add("X-Tenant", "a")
	header.Add("X-Tenant", "b")
	sql, bound, err := renderJinja(t, `{{ header("x-tenant") }}|{% if header("X-Missing") %}SET{% endif %}|{{ header("X-Missing") }}`,
		map[string]any{"headers": map[string]any{"x-missing": "spoofed"}}, header)
	if err != nil {
		t.Fatal(err)
	}
	if sql != ":__tpl_1||:__tpl_2" || !reflect.DeepEqual(bound, map[string]any{"__tpl_1": "a, b", "__tpl_2": nil}) {
		t.Errorf("got %q %#v", sql, bound)
	}
}

var jinjaQueries = map[string]string{
	"jsearch.sql.j2": `{# Optional filters, an IN list and a client-chosen sort column. #}
SELECT id, name, score
FROM users
WHERE 1 = 1
{% if name %}  AND name = {{ name }}
{% endif %}
{% if ids %}  AND id IN ({% for id in ids %}{{ id }}{% if not loop.last %}, {% endif %}{% endfor %})
{% endif %}
ORDER BY {{ ident(sort, allow=["id", "name", "score"]) }} {{ "DESC" | sqlkw if desc else "" }}`,
	"jsort.sql.j2": `SELECT id FROM users ORDER BY {{ ident(sort, allow=["id", "score"]) }} {% if desc %}DESC{% endif %}`,
	"jbulk.sql.j2": `INSERT INTO users (name, score) VALUES
{% for u in users %}({{ u.name }}, {{ u.score }}){% if not loop.last %}, {% endif %}{% endfor %}
RETURNING id, name, score`,
	"jme.sql.j2": `-- @header :uid = X-User-Id
SELECT id, name FROM users WHERE id = :uid{% if name %} AND name = {{ name }}{% endif %}`,
	"jmine.sql.j2":    `SELECT id, name FROM users WHERE id = {{ header("X-User-Id") }}`,
	"jquoted.sql.j2":  `SELECT id FROM users WHERE name LIKE '%{{ q }}%'`,
	"jconcat.sql.j2":  `SELECT id FROM users WHERE name LIKE '%' || {{ q }} || '%' ORDER BY id`,
	"jinclude.sql.j2": `{% include "users.sql" %}`,
	"pick.sql":        `SELECT 'plain' AS source`,
	"pick.sql.j2":     `SELECT 'jinja' AS source`,
	"jslots.sql.j2":   `SELECT {{ a }} AS a`,
	"jmixed.sql.j2":   `SELECT id, name FROM users WHERE id = :id{% if name %} AND name = {{ name }}{% endif %}`,
}

func TestJinjaHandler(t *testing.T) {
	h := newSQLiteHandler(t, func(h *SQLExecutor) { h.VerboseErrors = true })
	writeTree(t, h.Root, jinjaQueries)
	ada := `[{"id":1,"name":"Ada"}]` + "\n"

	t.Run("filters, IN lists and sorting", func(t *testing.T) {
		res := serve(t, h, "POST", "/jsort", `{"sort": "score", "desc": true}`, nil)
		if res.status != 200 || res.body != `[{"id":1},{"id":2}]`+"\n" {
			t.Errorf("sort: %d %s", res.status, res.body)
		}
		res = serve(t, h, "POST", "/jsort.csv?sort=id", "", nil)
		if res.status != 200 || res.body != "id\n1\n2\n" {
			t.Errorf("csv: %d %q", res.status, res.body)
		}
		res = serve(t, h, "POST", "/jme", `{"name": "Ada"}`, map[string]string{"X-User-Id": "1"})
		if res.status != 200 || res.body != ada {
			t.Errorf("directive + value: %d %s", res.status, res.body)
		}
	})

	t.Run("injection attempts", func(t *testing.T) {
		res := serve(t, h, "POST", "/jme", `{"name": "x' OR '1'='1"}`, map[string]string{"X-User-Id": "1"})
		if res.status != 200 || res.body != "[]\n" {
			t.Errorf("value injection: %d %s", res.status, res.body)
		}
		res = serve(t, h, "POST", "/jsort", `{"sort": "id; DROP TABLE users"}`, nil)
		if res.status != 400 || !strings.Contains(res.body, "is not one of id,score") {
			t.Errorf("ident injection: %d %s", res.status, res.body)
		}
		res = serve(t, h, "POST", "/jmine", `{"header": "1"}`, map[string]string{"X-User-Id": "1 OR 1=1"})
		if res.status != 200 || res.body != "[]\n" {
			t.Errorf("header injection: %d %s", res.status, res.body)
		}
		res = serve(t, h, "POST", "/users/by_id", `{"id": 1}`, nil)
		if res.body != ada {
			t.Errorf("table damaged: %d %s", res.status, res.body)
		}
	})

	t.Run("headers", func(t *testing.T) {
		res := serve(t, h, "POST", "/jmine", "", map[string]string{"X-User-Id": "1"})
		if res.status != 200 || res.body != ada {
			t.Errorf("header(): %d %s", res.status, res.body)
		}
		res = serve(t, h, "POST", "/jme", `{"uid": 2}`, nil)
		if res.status != 400 || !strings.Contains(res.body, "missing header X-User-Id") {
			t.Errorf("directive without header: %d %s", res.status, res.body)
		}
	})

	t.Run("bulk insert", func(t *testing.T) {
		res := serve(t, h, "POST", "/jbulk", `{"users": [{"name": "Linus", "score": 5}, {"name": "Ken", "score": 6.5}]}`, nil)
		want := `[{"id":3,"name":"Linus","score":5},{"id":4,"name":"Ken","score":6.5}]` + "\n"
		if res.status != 200 || res.body != want {
			t.Errorf("%d\n got: %s\nwant: %s", res.status, res.body, want)
		}
	})

	t.Run("template mistakes are server errors", func(t *testing.T) {
		res := serve(t, h, "POST", "/jquoted", `{"q": "a"}`, nil)
		if res.status != 500 || !strings.Contains(res.body, "inside a quoted SQL string") {
			t.Errorf("quoted: %d %s", res.status, res.body)
		}
		res = serve(t, h, "POST", "/jinclude", "", nil)
		if res.status != 500 || !strings.Contains(res.body, "{% include %} is not supported") {
			t.Errorf("include: %d %s", res.status, res.body)
		}
		res = serve(t, h, "POST", "/jsearch", `{"sort": "id", "desc": true}`, nil)
		if res.status != 500 {
			t.Errorf("unknown filter: %d %s", res.status, res.body)
		}
		res = serve(t, h, "POST", "/jconcat", `{"q": "rac"}`, nil)
		if res.status != 200 || res.body != `[{"id":2}]`+"\n" {
			t.Errorf("concat: %d %s", res.status, res.body)
		}
	})

	t.Run("request data cannot fill generated placeholders", func(t *testing.T) {
		res := serve(t, h, "POST", "/jslots", `{"a": 1, "__tpl_1": "hijacked"}`, nil)
		if res.body != `[{"a":1}]`+"\n" {
			t.Errorf("slot override: %d %s", res.status, res.body)
		}
	})

	t.Run(":name placeholders still work and are required", func(t *testing.T) {
		res := serve(t, h, "POST", "/jmixed", `{"id": 1, "name": "Ada"}`, nil)
		if res.body != ada {
			t.Errorf("got %d %s", res.status, res.body)
		}
		res = serve(t, h, "POST", "/jmixed", `{"name": "Ada"}`, nil)
		if res.status != 400 || !strings.Contains(res.body, "missing parameter(s): id") {
			t.Errorf("missing :id: %d %s", res.status, res.body)
		}
	})

	t.Run("lookup order is .sql, .sql.j2", func(t *testing.T) {
		if res := serve(t, h, "POST", "/pick", "", nil); res.body != `[{"source":"plain"}]`+"\n" {
			t.Errorf("pick: %s", res.body)
		}
		for _, target := range []string{"/jsort.sql", "/jsort.sql.j2", "/jsort.j2"} {
			if res := serve(t, h, "POST", target, `{"sort":"id"}`, nil); res.nextCalls != 1 {
				t.Errorf("%s: expected fall-through, got %d %s", target, res.status, res.body)
			}
		}
	})

	t.Run("edits are picked up", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(h.Root, "jsort.sql.j2"), []byte("SELECT {{ 40 + 2 }} AS answer"), 0o644); err != nil {
			t.Fatal(err)
		}
		if res := serve(t, h, "POST", "/jsort", "", nil); res.body != `[{"answer":42}]`+"\n" {
			t.Errorf("got %d %s", res.status, res.body)
		}
	})
}

func TestJinjaErrorsHiddenWithoutVerbose(t *testing.T) {
	h := newSQLiteHandler(t, nil)
	writeTree(t, h.Root, jinjaQueries)
	res := serve(t, h, "POST", "/jquoted", `{"q": "a"}`, nil)
	if res.status != 500 || res.body != `{"error":"query failed"}`+"\n" {
		t.Errorf("got %d %s", res.status, res.body)
	}
	// Input errors are always reported, since they are the client's to fix.
	res = serve(t, h, "POST", "/jsort", `{"sort": "nope"}`, nil)
	if res.status != 400 || !strings.Contains(res.body, "not one of") {
		t.Errorf("got %d %s", res.status, res.body)
	}
}
