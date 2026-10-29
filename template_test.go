package sqlexecutor

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRewriteTemplate(t *testing.T) {
	tests := []struct{ in, want string }{
		{"WHERE a = {{a}}", "WHERE a = {{bind a}}"},
		{"WHERE a = {{ a.b }}", "WHERE a = {{bind  a.b }}"},
		{"WHERE a = {{~a~}}", "WHERE a = {{~bind a~}}"},
		{"{{#each ids}}{{this}}, {{@index}}{{/each}}", "{{#each ids}}{{bind this}}, {{bind @index}}{{/each}}"},
		{"{{#if a}}x = {{a}}{{else}}x IS NULL{{/if}}", "{{#if a}}x = {{bind a}}{{else}}x IS NULL{{/if}}"},
		{"ORDER BY {{ident sort allow=\"a,b\"}}", "ORDER BY {{ident sort allow=\"a,b\"}}"},
		{"{{bind (lookup m k)}}", "{{bind (lookup m k)}}"},
		{"{{!-- a comment with {{x}} --}}SELECT {{y}}", "{{!-- a comment with {{x}} --}}SELECT {{bind y}}"},
		{"SELECT 'é', {{x}}", "SELECT 'é', {{bind x}}"},
		{"SELECT :plain, {{x}}", "SELECT :plain, {{bind x}}"},
	}
	for _, tt := range tests {
		got, err := rewriteTemplate(tt.in)
		if err != nil {
			t.Errorf("rewriteTemplate(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("rewriteTemplate(%q)\n got: %q\nwant: %q", tt.in, got, tt.want)
		}
	}
}

func TestRewriteTemplateRejects(t *testing.T) {
	tests := map[string]string{
		"SELECT {{{a}}}":                      "raw output is not allowed",
		"SELECT {{& a}}":                      "raw output is not allowed",
		"SELECT 1\n{{lookup a b}}":            "line 2: {{lookup a b}}: only bind and ident",
		"SELECT {{log a}}":                    "only bind and ident",
		"{{#if a}}{{{a}}}{{/if}}":             "raw output is not allowed",
		"{{#each a}}{{else}}{{{b}}}{{/each}}": "raw output is not allowed",
		"SELECT {{> part}}":                   "partials are not supported",
		"SELECT {{#if a}}":                    "", // syntax error
	}
	for in, wantMsg := range tests {
		_, err := rewriteTemplate(in)
		if err == nil {
			t.Errorf("rewriteTemplate(%q): expected error", in)
			continue
		}
		if !strings.Contains(err.Error(), wantMsg) {
			t.Errorf("rewriteTemplate(%q) error %q, want it to contain %q", in, err, wantMsg)
		}
	}
}

func TestRenderTemplate(t *testing.T) {
	tpl, err := parseSQLTemplate(`SELECT * FROM t WHERE 1=1
{{#if name}} AND name = {{name}}{{/if}}
{{#if ids}} AND id IN ({{#each ids}}{{this}}{{#unless @last}}, {{/unless}}{{/each}}){{/if}}
{{#if active}} AND active{{/if}}
ORDER BY {{ident sort allow="name, created_at"}} LIMIT {{limit}}`)
	if err != nil {
		t.Fatal(err)
	}

	sql, bound, err := tpl.render(map[string]any{
		"name":   "O'Brien <b>",
		"ids":    []any{int64(3), int64(5)},
		"active": false,
		"sort":   "created_at",
		"limit":  int64(10),
	})
	if err != nil {
		t.Fatal(err)
	}
	wantSQL := `SELECT * FROM t WHERE 1=1
 AND name = :__hbs_1
 AND id IN (:__hbs_2, :__hbs_3)

ORDER BY "created_at" LIMIT :__hbs_4`
	if sql != wantSQL {
		t.Errorf("sql\n got: %s\nwant: %s", sql, wantSQL)
	}
	wantBound := map[string]any{
		"__hbs_1": "O'Brien <b>", // bound verbatim: no HTML escaping, no quoting
		"__hbs_2": int64(3),
		"__hbs_3": int64(5),
		"__hbs_4": int64(10),
	}
	if !reflect.DeepEqual(bound, wantBound) {
		t.Errorf("bound %#v\nwant %#v", bound, wantBound)
	}

	// Renders are independent: counters restart and values don't leak.
	sql, bound, err = tpl.render(map[string]any{"sort": "name", "limit": int64(1)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(sql, `ORDER BY "name" LIMIT :__hbs_1`) || len(bound) != 1 {
		t.Errorf("second render: %s %v", sql, bound)
	}

	_, _, err = tpl.render(map[string]any{"sort": "password", "limit": int64(1)})
	var inputErr *inputError
	if !errors.As(err, &inputErr) || !strings.Contains(err.Error(), `"password" is not one of name, created_at`) {
		t.Errorf("disallowed ident: %v", err)
	}
}

func TestRenderTemplateMissingValueBindsNull(t *testing.T) {
	tpl, err := parseSQLTemplate("SELECT {{missing}}, {{obj.missing}}")
	if err != nil {
		t.Fatal(err)
	}
	sql, bound, err := tpl.render(map[string]any{"obj": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if sql != "SELECT :__hbs_1, :__hbs_2" || bound["__hbs_1"] != nil || bound["__hbs_2"] != nil || len(bound) != 2 {
		t.Errorf("got %q %v", sql, bound)
	}
}

func TestIdentHelper(t *testing.T) {
	tests := map[string]string{
		"col":            `"col"`,
		"public.users":   `"public"."users"`,
		`we"ird`:         `"we""ird"`,
		`x"; DROP TABLE`: `"x""; DROP TABLE"`,
	}
	for in, want := range tests {
		tpl, err := parseSQLTemplate("{{ident v}}")
		if err != nil {
			t.Fatal(err)
		}
		got, _, err := tpl.render(map[string]any{"v": in})
		if err != nil || got != want {
			t.Errorf("ident(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []any{nil, "", int64(3), []any{"a"}} {
		tpl, _ := parseSQLTemplate("{{ident v}}")
		_, _, err := tpl.render(map[string]any{"v": bad})
		var inputErr *inputError
		if !errors.As(err, &inputErr) {
			t.Errorf("ident(%#v): expected input error, got %v", bad, err)
		}
	}
}

var templateQueries = map[string]string{
	"search.sql.hbs": `{{! Optional filters, an IN list and a client-chosen sort column. }}
SELECT id, name, score
FROM users
WHERE 1 = 1
{{#if name}}  AND name = {{name}}
{{/if}}
{{#if ids}}  AND id IN ({{#each ids}}{{this}}{{#unless @last}}, {{/unless}}{{/each}})
{{/if}}
ORDER BY {{ident sort allow="id,name,score"}} {{#if desc}}DESC{{else}}ASC{{/if}}`,
	"bulk_insert.sql.hbs": `INSERT INTO users (name, score) VALUES
{{#each users}}({{name}}, {{score}}){{#unless @last}}, {{/unless}}{{/each}}
RETURNING id, name, score`,
	"mixed.sql.hbs":  "SELECT id, name FROM users WHERE id = :id{{#if name}} AND name = {{name}}{{/if}}",
	"quoted.sql.hbs": "SELECT id FROM users WHERE name LIKE '%{{q}}%'",
	"raw.sql.hbs":    "SELECT {{{col}}} FROM users",
	"concat.sql.hbs": "SELECT id FROM users WHERE name LIKE '%' || {{q}} || '%' ORDER BY id",
	"shadow.sql":     "SELECT 'plain' AS source",
	"shadow.sql.hbs": "SELECT 'template' AS source",
	"slots.sql.hbs":  "SELECT {{a}} AS a",
}

func newTemplateHandler(t *testing.T, configure func(*SQLExecutor)) *SQLExecutor {
	t.Helper()
	h := newSQLiteHandler(t, configure)
	writeTree(t, h.Root, templateQueries)
	return h
}

func TestTemplateHandler(t *testing.T) {
	h := newTemplateHandler(t, func(h *SQLExecutor) { h.VerboseErrors = true })

	t.Run("optional filters and sorting", func(t *testing.T) {
		res := serve(t, h, "POST", "/search", `{"sort": "score", "desc": true}`, nil)
		want := `[{"id":1,"name":"Ada","score":9.5},{"id":2,"name":"Grace, \"Amazing\"","score":7}]` + "\n"
		if res.status != 200 || res.body != want {
			t.Errorf("status %d\n got: %s\nwant: %s", res.status, res.body, want)
		}

		res = serve(t, h, "POST", "/search.csv", `{"sort": "id", "ids": [2, 99]}`, nil)
		if res.status != 200 || res.body != "id,name,score\n2,\"Grace, \"\"Amazing\"\"\",7\n" {
			t.Errorf("IN list: status %d body %q", res.status, res.body)
		}

		res = serve(t, h, "POST", "/search?sort=name&name=Ada", "", nil)
		if res.status != 200 || res.body != `[{"id":1,"name":"Ada","score":9.5}]`+"\n" {
			t.Errorf("query string params: status %d body %s", res.status, res.body)
		}
	})

	t.Run("injection attempts", func(t *testing.T) {
		// Values are bound, so SQL in them is just data.
		res := serve(t, h, "POST", "/search", `{"sort": "id", "name": "x' OR '1'='1"}`, nil)
		if res.status != 200 || res.body != "[]\n" {
			t.Errorf("value injection: status %d body %s", res.status, res.body)
		}
		// Identifiers must be on the allow list.
		res = serve(t, h, "POST", "/search", `{"sort": "id; DROP TABLE users"}`, nil)
		if res.status != 400 || !strings.Contains(res.body, "is not one of id,name,score") {
			t.Errorf("ident injection: status %d body %s", res.status, res.body)
		}
		// Request data cannot fill generated placeholders.
		res = serve(t, h, "POST", "/slots", `{"a": 1, "__hbs_1": "hijacked"}`, nil)
		if res.body != `[{"a":1}]`+"\n" {
			t.Errorf("slot override: %s", res.body)
		}
		// Table must be intact.
		res = serve(t, h, "POST", "/users/by_id", `{"id": 1}`, nil)
		if res.status != 200 || res.body != `[{"id":1,"name":"Ada"}]`+"\n" {
			t.Errorf("after injection attempts: %d %s", res.status, res.body)
		}
	})

	t.Run("bulk insert from array of objects", func(t *testing.T) {
		res := serve(t, h, "POST", "/bulk_insert", `{"users": [{"name": "Linus", "score": 5}, {"name": "Ken", "score": 6.5}]}`, nil)
		want := `[{"id":3,"name":"Linus","score":5},{"id":4,"name":"Ken","score":6.5}]` + "\n"
		if res.status != 200 || res.body != want {
			t.Errorf("status %d\n got: %s\nwant: %s", res.status, res.body, want)
		}
	})

	t.Run(":name placeholders still work and are required", func(t *testing.T) {
		res := serve(t, h, "POST", "/mixed", `{"id": 1, "name": "Ada"}`, nil)
		if res.body != `[{"id":1,"name":"Ada"}]`+"\n" {
			t.Errorf("got %d %s", res.status, res.body)
		}
		res = serve(t, h, "POST", "/mixed", `{"name": "Ada"}`, nil)
		if res.status != 400 || !strings.Contains(res.body, "missing parameter(s): id") {
			t.Errorf("missing :id: %d %s", res.status, res.body)
		}
	})

	t.Run("concatenation instead of quoting", func(t *testing.T) {
		res := serve(t, h, "POST", "/concat", `{"q": "rac"}`, nil)
		if res.body != `[{"id":2}]`+"\n" {
			t.Errorf("got %d %s", res.status, res.body)
		}
	})

	t.Run("template mistakes are server errors", func(t *testing.T) {
		res := serve(t, h, "POST", "/quoted", `{"q": "a"}`, nil)
		if res.status != 500 || !strings.Contains(res.body, "inside a quoted SQL string") {
			t.Errorf("quoted: %d %s", res.status, res.body)
		}
		res = serve(t, h, "POST", "/raw", `{"col": "name"}`, nil)
		if res.status != 500 || !strings.Contains(res.body, "raw output is not allowed") {
			t.Errorf("raw: %d %s", res.status, res.body)
		}
	})

	t.Run("plain .sql wins over .sql.hbs", func(t *testing.T) {
		res := serve(t, h, "POST", "/shadow", "", nil)
		if res.body != `[{"source":"plain"}]`+"\n" {
			t.Errorf("got %s", res.body)
		}
	})

	t.Run("template file is not addressable by its own name", func(t *testing.T) {
		for _, target := range []string{"/search.sql", "/search.sql.hbs", "/search.hbs"} {
			if res := serve(t, h, "POST", target, `{"sort":"id"}`, nil); res.nextCalls != 1 {
				t.Errorf("%s: expected fall-through, got %d %s", target, res.status, res.body)
			}
		}
	})

	t.Run("edits are picked up", func(t *testing.T) {
		p := filepath.Join(h.Root, "slots.sql.hbs")
		if err := os.WriteFile(p, []byte("SELECT {{a}} AS a, {{b}} AS b"), 0o644); err != nil {
			t.Fatal(err)
		}
		res := serve(t, h, "POST", "/slots", `{"a": 1, "b": "two"}`, nil)
		if res.body != `[{"a":1,"b":"two"}]`+"\n" {
			t.Errorf("got %s", res.body)
		}
	})
}

func TestTemplateErrorsHiddenWithoutVerbose(t *testing.T) {
	h := newTemplateHandler(t, nil)
	res := serve(t, h, "POST", "/raw", `{"col": "name"}`, nil)
	if res.status != 500 || res.body != `{"error":"query failed"}`+"\n" {
		t.Errorf("got %d %s", res.status, res.body)
	}
	// Input errors are always reported, since they are the client's to fix.
	res = serve(t, h, "POST", "/search", `{"sort": "nope"}`, nil)
	if res.status != 400 || !strings.Contains(res.body, "not one of") {
		t.Errorf("got %d %s", res.status, res.body)
	}
}
