package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	sqlexecutor "github.com/lorefnon/caddy-plugin-sql-executor"
)

var update = flag.Bool("update", false, "rewrite golden files")

func generateSpec(t *testing.T, args ...string) []byte {
	t.Helper()
	var out, errOut bytes.Buffer
	if err := run(t.Context(), args, &out, &errOut); err != nil {
		t.Fatalf("run: %v\n%s", err, errOut.String())
	}
	return out.Bytes()
}

func TestExamplesGolden(t *testing.T) {
	got := generateSpec(t, "-config", "../../examples/sql-openapi.yaml")
	golden := filepath.Join("testdata", "examples.openapi.yaml")
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("spec differs from %s; run go test -update to accept\n%s", golden, got)
	}
}

// request is a call to make against the handler. Its body is checked
// against the spec's request schema, and the response rows against the
// row schema.
type request struct {
	route   string
	body    map[string]any
	headers map[string]string
}

func TestExamplesRoundTrip(t *testing.T) {
	cfg, err := loadConfig("../../examples/sql-openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	requests := []request{
		{route: "category_summary"},
		{route: "products", body: map[string]any{"category": nil}},
		{route: "products", body: map[string]any{"category": "coffee"}},
		{route: "products/by_id", body: map[string]any{"id": 1}},
		{route: "products/create", body: map[string]any{"name": "Tea", "category": "tea", "price": 3.5}},
		{route: "products/mine", headers: map[string]string{"X-User-Id": "1"}},
	}
	for file, q := range cfg.Queries {
		for _, s := range q.Samples {
			requests = append(requests, request{route: strings.TrimSuffix(file, ".sql.j2"), body: s, headers: q.Headers})
		}
	}
	roundTrip(t, cfg.Root, "sqlite", sqliteFromSchema(t, cfg.Schema), cfg.JinjaIncludes, []string{"-config", "../../examples/sql-openapi.yaml"}, requests)
}

// TestSQLiteTypes checks the schemas of every kind of SQLite column
// against what the handler actually returns.
func TestSQLiteTypes(t *testing.T) {
	dir := t.TempDir()
	schema := filepath.Join(dir, "schema.sql")
	writeFile(t, schema, `
		CREATE TABLE t (
			id INTEGER PRIMARY KEY,
			n INT NOT NULL,
			r REAL,
			d DOUBLE PRECISION,
			num NUMERIC,
			dec DECIMAL(10, 2),
			s VARCHAR(20) NOT NULL,
			c TEXT,
			b BOOLEAN,
			day DATE,
			at DATETIME,
			ts TIMESTAMP,
			j JSON,
			blob BLOB,
			untyped
		);
		INSERT INTO t VALUES
			(1, 2, 1.5, 2.5, 3, 4.25, 'x', 'y', 1, '2024-01-02', '2024-01-02T03:04:05Z', '2024-01-02 03:04:05', '{"a":[1]}', x'00ff', 'z'),
			(2, 3, NULL, NULL, NULL, NULL, 'x', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL);`)
	root := filepath.Join(dir, "queries")
	writeFile(t, filepath.Join(root, "all.sql"), "SELECT * FROM t ORDER BY id")
	writeFile(t, filepath.Join(root, "exprs.sql"), "SELECT count(*) AS n, max(r) AS m, :x AS x FROM t")
	writeFile(t, filepath.Join(dir, "config.yaml"), `
root: queries
driver: sqlite
schema: schema.sql
queries:
  exprs.sql:
    params: { x: { type: string } }
    columns: { n: { type: integer } }
`)
	roundTrip(t, root, "sqlite", sqliteFromSchema(t, schema), false, []string{"-config", filepath.Join(dir, "config.yaml")}, []request{
		{route: "all"},
		{route: "exprs", body: map[string]any{"x": "hi"}},
	})

	spec := specFor(t, "-config", filepath.Join(dir, "config.yaml"))
	assertProperties(t, spec.Components.Schemas["AllRow"], map[string]string{
		"id":      `{"type":"integer","format":"int64"}`,
		"n":       `{"type":"integer","format":"int64"}`,
		"r":       `{"type":["number","null"],"format":"double"}`,
		"s":       `{"type":"string"}`,
		"b":       `{"type":["boolean","null"]}`,
		"day":     `{"type":["string","null"],"format":"date"}`,
		"at":      `{"type":["string","null"],"format":"date-time"}`,
		"j":       `{}`,
		"blob":    `{"type":["string","null"],"contentEncoding":"base64"}`,
		"untyped": `{}`,
	})
}

func TestTemplateSamples(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "schema.sql"), "CREATE TABLE p (id INTEGER PRIMARY KEY, name TEXT NOT NULL, price REAL);")
	writeFile(t, filepath.Join(dir, "q", "pick.sql.j2"), `{# Pick columns. #}
SELECT id{% if wide %}, name, price{% endif %} FROM p
WHERE {{ header("x-tenant") }} IS NULL OR 1 = 1
{% if ids %}AND id IN ({% for i in ids %}{{ i }}{% if not loop.last %},{% endif %}{% endfor %}){% endif %}`)
	writeFile(t, filepath.Join(dir, "q", "none.sql.j2"), "SELECT 1")
	writeFile(t, filepath.Join(dir, "config.yaml"), `
root: q
driver: sqlite
schema: schema.sql
queries:
  pick:
    samples:
      - { ids: [1, 2] }
      - { wide: true, ids: [] }
`)
	spec := specFor(t, "-config", filepath.Join(dir, "config.yaml"))

	op := spec.Paths["/pick"].Post
	if op.Description != "Pick columns." {
		t.Errorf("description = %q", op.Description)
	}
	if len(op.Parameters) != 1 || op.Parameters[0].Name != "X-Tenant" || op.Parameters[0].Required {
		t.Errorf("parameters = %+v", op.Parameters)
	}
	assertJSON(t, op.RequestBody.Content["application/json"].Schema,
		`{"type":"object","properties":{"ids":{"type":"array","items":{"type":"integer"}},"wide":{"type":"boolean"}}}`)
	assertJSON(t, spec.Components.Schemas["PickRow"],
		`{"anyOf":[{"type":"object","properties":{"id":{"type":"integer","format":"int64"}},"required":["id"]},`+
			`{"type":"object","properties":{"id":{"type":"integer","format":"int64"},"name":{"type":"string"},"price":{"type":["number","null"],"format":"double"}},"required":["id","name","price"]}]}`)

	// Without samples, nothing is known.
	none := spec.Paths["/none"].Post
	assertJSON(t, none.RequestBody.Content["application/json"].Schema, `{"type":"object"}`)
	assertJSON(t, spec.Components.Schemas["NoneRow"], `{"type":"object"}`)
}

func TestFileErrors(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "schema.sql"), "CREATE TABLE p (id INTEGER);")
	writeFile(t, filepath.Join(dir, "q", "good.sql"), "SELECT id FROM p")
	writeFile(t, filepath.Join(dir, "q", "bad.sql"), "SELECT nope FROM p")
	var out, errOut bytes.Buffer
	err := run(t.Context(), []string{"-root", filepath.Join(dir, "q"), "-driver", "sqlite", "-schema", filepath.Join(dir, "schema.sql")}, &out, &errOut)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(errOut.String(), "bad.sql: ") {
		t.Errorf("stderr = %q", errOut.String())
	}
	var doc document
	if err := yaml.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Paths["/good"] == nil || doc.Paths["/bad"] != nil {
		t.Errorf("paths = %v", doc.Paths)
	}
}

func TestUnion(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{`{"type":"integer"}`, `{"type":"number"}`, `{"type":"number"}`},
		{`{"type":"string"}`, `{"type":"null"}`, `{"type":["string","null"]}`},
		{`{"type":"array","items":{"type":"integer"}}`, `{"type":"array"}`, `{"type":"array","items":{"type":"integer"}}`},
		{`{"type":"array","items":{"type":"integer"}}`, `{"type":"array","items":{"type":"string"}}`, `{"type":"array","items":{"type":["integer","string"]}}`},
		{`{"type":"string"}`, `{"type":"array"}`, `{"type":["string","array"]}`},
		{`{"type":"string"}`, `{"type":"array","items":{"type":"integer"}}`, `{"anyOf":[{"type":"string"},{"type":"array","items":{"type":"integer"}}]}`},
		{`{}`, `{"type":"string"}`, `{}`},
	}
	for _, c := range cases {
		a, b := mustSchema(t, c.a), mustSchema(t, c.b)
		assertJSON(t, union(a, b), c.want)
	}
}

func TestLeadingComment(t *testing.T) {
	sql := "-- Line one.\n-- @header :u = X-User\n--   Line two.\nSELECT 1 -- not this"
	if got := leadingComment(sql, false); got != "Line one.\nLine two." {
		t.Errorf("got %q", got)
	}
	tpl := "{#\n  First.\n    Indented.\n#}\nSELECT 1"
	if got := leadingComment(tpl, true); got != "First.\n  Indented." {
		t.Errorf("got %q", got)
	}
}

// roundTrip generates a spec by running the command with args, runs each request against a
// handler serving root from dsn, and validates request bodies and response
// rows against the spec.
func roundTrip(t *testing.T, root, driver, dsn string, jinjaIncludes bool, args []string, requests []request) {
	t.Helper()
	spec := specFor(t, args...)
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	if err := c.AddResource("spec.json", doc); err != nil {
		t.Fatal(err)
	}
	compile := func(ptr string) *jsonschema.Schema {
		s, err := c.Compile("spec.json#" + ptr)
		if err != nil {
			t.Fatalf("compiling %s: %v", ptr, err)
		}
		return s
	}

	h := &sqlexecutor.SQLExecutor{Root: root, Driver: driver, DSN: dsn, JinjaIncludes: jinjaIncludes, VerboseErrors: true}
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	t.Cleanup(cancel)
	if err := h.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Cleanup() })

	for _, r := range requests {
		path := "/" + r.route
		item := spec.Paths[path]
		if item == nil {
			t.Errorf("%s: not in the spec", path)
			continue
		}
		ptr := "/paths/" + strings.ReplaceAll(strings.ReplaceAll(path, "~", "~0"), "/", "~1") + "/post"
		body := r.body
		if body == nil {
			body = map[string]any{}
		}
		raw, _ := json.Marshal(body)
		if item.Post.RequestBody != nil {
			inst, _ := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if err := compile(ptr + "/requestBody/content/application~1json/schema").Validate(inst); err != nil {
				t.Errorf("%s: request %s does not match the spec: %v", path, raw, err)
			}
		}

		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		for k, v := range r.headers {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
			w.WriteHeader(http.StatusNotFound)
			return nil
		})
		if err := h.ServeHTTP(w, req, next); err != nil {
			t.Fatal(err)
		}
		if w.Code != http.StatusOK {
			t.Errorf("%s %s: status %d: %s", path, raw, w.Code, w.Body)
			continue
		}
		rows, err := jsonschema.UnmarshalJSON(w.Body)
		if err != nil {
			t.Fatal(err)
		}
		if n, _ := rows.([]any); len(n) == 0 {
			t.Errorf("%s %s: no rows, so nothing was checked", path, raw)
		}
		if err := compile(ptr + "/responses/200/content/application~1json/schema").Validate(rows); err != nil {
			t.Errorf("%s %s: response does not match the spec: %v", path, raw, err)
		}
	}
}

func specFor(t *testing.T, args ...string) *document {
	t.Helper()
	var doc document
	if err := yaml.Unmarshal(generateSpec(t, args...), &doc); err != nil {
		t.Fatal(err)
	}
	return &doc
}

// sqliteFromSchema creates a SQLite database file from a DDL file.
func sqliteFromSchema(t *testing.T, schema string) string {
	t.Helper()
	ddl, err := os.ReadFile(schema)
	if err != nil {
		t.Fatal(err)
	}
	dsn := "file:" + filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}
	return dsn
}

func writeFile(t *testing.T, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustSchema(t *testing.T, s string) *Schema {
	t.Helper()
	if s == "{}" {
		return nil
	}
	var out Schema
	if err := yaml.Unmarshal([]byte(s), &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

func assertJSON(t *testing.T, s *Schema, want string) {
	t.Helper()
	got, err := json.Marshal(orEmpty(s))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}
