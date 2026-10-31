package main

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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
//
// With a method other than POST, the body is sent in the query string and
// checked against the operation's query parameters instead. route is the
// spec's path without its leading slash; url is the path requested (default
// "/" + route), and pathParams are the values it captures, checked against
// the operation's path parameters.
type request struct {
	method     string
	route      string
	url        string
	pathParams map[string]any
	body       map[string]any
	headers    map[string]string
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
	h := &sqlexecutor.SQLExecutor{Root: root, Driver: driver, DSN: dsn, JinjaIncludes: jinjaIncludes, VerboseErrors: true}
	roundTripHandler(t, h, args, requests)
}

// roundTripHandler is roundTrip with a handler configured by the caller.
func roundTripHandler(t *testing.T, h *sqlexecutor.SQLExecutor, args []string, requests []request) {
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

	h.VerboseErrors = true
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	t.Cleanup(cancel)
	if err := h.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Cleanup() })

	for _, r := range requests {
		method := cmp.Or(r.method, http.MethodPost)
		path := "/" + r.route
		item := spec.Paths[path]
		if item == nil {
			t.Errorf("%s: not in the spec", path)
			continue
		}
		op := operationFor(item, method)
		if op == nil {
			t.Errorf("%s %s: not in the spec", method, path)
			continue
		}
		ptr := "/paths/" + strings.ReplaceAll(strings.ReplaceAll(path, "~", "~0"), "/", "~1") + "/" + strings.ToLower(method)
		body := r.body
		if body == nil {
			body = map[string]any{}
		}
		raw, _ := json.Marshal(body)
		target := cmp.Or(r.url, path)
		var reqBody io.Reader = bytes.NewReader(raw)
		if bodyless(method) {
			if op.RequestBody != nil {
				t.Errorf("%s %s: a request body is described for a method without one", method, path)
			}
			q := url.Values{}
			for k, v := range body {
				q.Set(k, fmt.Sprint(v))
				if !slices.ContainsFunc(op.Parameters, func(p parameter) bool { return p.In == "query" && p.Name == k }) {
					t.Errorf("%s %s: no query parameter %s", method, path, k)
				}
			}
			for _, p := range op.Parameters {
				if _, ok := body[p.Name]; p.In == "query" && p.Required && !ok {
					t.Errorf("%s %s: required query parameter %s was not sent", method, path, p.Name)
				}
			}
			target += "?" + q.Encode()
			reqBody = nil
		} else if op.RequestBody != nil {
			inst, _ := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if err := compile(ptr + "/requestBody/content/application~1json/schema").Validate(inst); err != nil {
				t.Errorf("%s %s: request %s does not match the spec: %v", method, path, raw, err)
			}
		}
		for name, v := range r.pathParams {
			i := slices.IndexFunc(op.Parameters, func(p parameter) bool { return p.In == "path" && p.Name == name })
			if i < 0 {
				t.Errorf("%s %s: no path parameter %s", method, path, name)
				continue
			}
			encoded, _ := json.Marshal(v)
			inst, _ := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
			if err := compile(fmt.Sprintf("%s/parameters/%d/schema", ptr, i)).Validate(inst); err != nil {
				t.Errorf("%s %s: path parameter %s=%v does not match the spec: %v", method, path, name, v, err)
			}
		}
		for _, p := range op.Parameters {
			if _, ok := r.pathParams[p.Name]; p.In == "path" && !ok {
				t.Errorf("%s %s: no value given for path parameter %s", method, path, p.Name)
			}
		}

		req := httptest.NewRequest(method, target, reqBody)
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
			t.Errorf("%s %s: status %d: %s", target, raw, w.Code, w.Body)
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

// operationFor returns the operation a path item has for a method.
func operationFor(item *pathItem, method string) *operation {
	return map[string]*operation{
		"GET": item.Get, "PUT": item.Put, "POST": item.Post, "DELETE": item.Delete,
		"OPTIONS": item.Options, "HEAD": item.Head, "PATCH": item.Patch, "TRACE": item.Trace,
	}[method]
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

func TestDynamicRoutes(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "schema.sql"), `
		CREATE TABLE u (id INTEGER PRIMARY KEY, name TEXT NOT NULL);
		CREATE TABLE o (id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL, item TEXT NOT NULL);
		INSERT INTO u VALUES (1, 'Ada'), (2, 'Grace');
		INSERT INTO o VALUES (7, 1, 'tea');`)
	root := filepath.Join(dir, "q")
	writeFile(t, filepath.Join(root, "users", "[id].sql"), "-- One user.\nSELECT id, name FROM u WHERE id = :id")
	writeFile(t, filepath.Join(root, "users", "me.sql"), "SELECT id, name FROM u WHERE id = 1")
	writeFile(t, filepath.Join(root, "users", "[id]", "orders", "[order].sql"),
		"SELECT id, item, :ignored AS ignored FROM o WHERE user_id = :id AND id = :order")
	writeFile(t, filepath.Join(root, "users", "[id]", "orders.sql"), "SELECT id, item FROM o WHERE user_id = :id")
	writeFile(t, filepath.Join(root, "docs", "[...slug].sql"), "SELECT :slug AS slug")
	writeFile(t, filepath.Join(root, "shop", "[[...cat]].sql"), "SELECT :cat AS cat")
	writeFile(t, filepath.Join(root, "tpl", "[name].sql.j2"), "SELECT {{ name }} AS name, {{ n }} AS n")
	writeFile(t, filepath.Join(dir, "config.yaml"), `
root: q
driver: sqlite
schema: schema.sql
queries:
  tpl/[name]:
    samples:
      - { name: x, n: 3 }
`)
	args := []string{"-config", filepath.Join(dir, "config.yaml")}

	spec := specFor(t, args...)
	var paths []string
	for p := range spec.Paths {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	want := []string{"/docs/{slug}", "/shop", "/shop/{cat}", "/tpl/{name}", "/users/me", "/users/{id}", "/users/{id}/orders", "/users/{id}/orders/{order}"}
	if !slices.Equal(paths, want) {
		t.Errorf("paths = %v\nwant    %v", paths, want)
	}

	op := spec.Paths["/users/{id}"].Post
	if op.OperationID != "users_id" || op.Description != "One user." || op.RequestBody != nil {
		t.Errorf("users/{id}: %+v", op)
	}
	if len(op.Parameters) != 1 {
		t.Fatalf("parameters = %+v", op.Parameters)
	}
	p := op.Parameters[0]
	if p.Name != "id" || p.In != "path" || !p.Required {
		t.Errorf("parameter = %+v", p)
	}
	assertJSON(t, p.Schema, `{"type":"integer","format":"int64"}`)

	// Path parameters aren't in the body, but the others are.
	op = spec.Paths["/users/{id}/orders/{order}"].Post
	assertJSON(t, op.RequestBody.Content["application/json"].Schema,
		`{"type":"object","properties":{"ignored":{}},"required":["ignored"]}`)
	if len(op.Parameters) != 2 || op.Parameters[0].Name != "id" || op.Parameters[1].Name != "order" {
		t.Errorf("parameters = %+v", op.Parameters)
	}

	// Catch-alls are one parameter holding the rest of the path.
	op = spec.Paths["/docs/{slug}"].Post
	if len(op.Parameters) != 1 || op.Parameters[0].In != "path" || !strings.Contains(op.Parameters[0].Description, "JSON array") {
		t.Errorf("catch-all parameters = %+v", op.Parameters)
	}
	assertJSON(t, op.Parameters[0].Schema, `{"type":"string"}`)

	// An optional catch-all is served with and without its parameter.
	with, without := spec.Paths["/shop/{cat}"].Post, spec.Paths["/shop"].Post
	if len(with.Parameters) != 1 || len(without.Parameters) != 0 || with.OperationID == without.OperationID {
		t.Errorf("optional catch-all: with %+v, without %+v", with, without)
	}

	roundTrip(t, root, "sqlite", sqliteFromSchema(t, filepath.Join(dir, "schema.sql")), false, args, []request{
		{route: "users/{id}", url: "/users/1", pathParams: map[string]any{"id": 1}},
		{route: "users/me"},
		{route: "users/{id}/orders", url: "/users/1/orders", pathParams: map[string]any{"id": 1}},
		{route: "users/{id}/orders/{order}", url: "/users/1/orders/7", pathParams: map[string]any{"id": 1, "order": 7}, body: map[string]any{"ignored": nil}},
		{route: "docs/{slug}", url: "/docs/a/b", pathParams: map[string]any{"slug": "a/b"}},
		{route: "shop", url: "/shop"},
		{route: "shop/{cat}", url: "/shop/x/y", pathParams: map[string]any{"cat": "x/y"}},
		{route: "tpl/{name}", url: "/tpl/x", pathParams: map[string]any{"name": "x"}, body: map[string]any{"n": 3}},
	})
}

func TestVerbNamedFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "schema.sql"), `
		CREATE TABLE p (id INTEGER PRIMARY KEY, name TEXT NOT NULL, price REAL);
		INSERT INTO p VALUES (1, 'tea', 2.5), (2, 'coffee', 3);`)
	root := filepath.Join(dir, "q")
	writeFile(t, filepath.Join(root, "products", "GET.sql"), "SELECT id, name FROM p WHERE (:name IS NULL OR name = :name)")
	writeFile(t, filepath.Join(root, "products", "POST.sql"), "INSERT INTO p (name, price) VALUES (:name, :price) RETURNING id, name")
	writeFile(t, filepath.Join(root, "products", "[id]", "GET.sql"), "SELECT id, name, price FROM p WHERE id = :id")
	writeFile(t, filepath.Join(root, "products", "[id]", "PATCH.sql"), "UPDATE p SET price = :price WHERE id = :id RETURNING id, price")
	writeFile(t, filepath.Join(root, "products", "[id]", "DELETE.sql"), "DELETE FROM p WHERE id = :id AND :confirm RETURNING id")
	writeFile(t, filepath.Join(root, "plain.sql"), "SELECT 1")
	writeFile(t, filepath.Join(dir, "config.yaml"), `
root: q
driver: sqlite
schema: schema.sql
verb_named_files: true
`)
	args := []string{"-config", filepath.Join(dir, "config.yaml")}

	spec := specFor(t, args...)
	if spec.Paths["/plain"] != nil || spec.Paths["/products/GET"] != nil || len(spec.Paths) != 2 {
		t.Errorf("paths = %v", spec.Paths)
	}

	collection := spec.Paths["/products"]
	if collection.Get == nil || collection.Post == nil || collection.Delete != nil {
		t.Fatalf("/products = %+v", collection)
	}
	if collection.Get.OperationID != "products_get" || collection.Post.OperationID != "products_post" {
		t.Errorf("ids = %q, %q", collection.Get.OperationID, collection.Post.OperationID)
	}
	// GET sends parameters in the query string, POST in a body.
	if collection.Get.RequestBody != nil || len(collection.Get.Parameters) != 1 {
		t.Fatalf("GET = %+v", collection.Get)
	}
	q := collection.Get.Parameters[0]
	if q.Name != "name" || q.In != "query" || !q.Required {
		t.Errorf("query parameter = %+v", q)
	}
	assertJSON(t, q.Schema, `{"type":"string"}`)
	if collection.Post.RequestBody == nil || len(collection.Post.Parameters) != 0 {
		t.Errorf("POST = %+v", collection.Post)
	}

	item := spec.Paths["/products/{id}"]
	if item.Get == nil || item.Patch == nil || item.Delete == nil {
		t.Fatalf("/products/{id} = %+v", item)
	}
	if item.Delete.RequestBody != nil || len(item.Delete.Parameters) != 2 || item.Delete.Parameters[0].In != "path" || item.Delete.Parameters[1].Name != "confirm" {
		t.Errorf("DELETE = %+v", item.Delete.Parameters)
	}
	if item.Patch.RequestBody == nil {
		t.Errorf("PATCH has no body")
	}
	for name, schema := range map[string]*Schema{"ProductsGetRow": nil, "ProductsIdGetRow": nil, "ProductsIdPatchRow": nil} {
		if _, ok := spec.Components.Schemas[name]; !ok {
			t.Errorf("no schema %s in %v (want %v)", name, spec.Components.Schemas, schema)
		}
	}

	h := &sqlexecutor.SQLExecutor{Root: root, Driver: "sqlite", DSN: sqliteFromSchema(t, filepath.Join(dir, "schema.sql")), VerbNamedFiles: true}
	roundTripHandler(t, h, args, []request{
		{method: "GET", route: "products", body: map[string]any{"name": "tea"}},
		{method: "POST", route: "products", body: map[string]any{"name": "mate", "price": 4}},
		{method: "GET", route: "products/{id}", url: "/products/1", pathParams: map[string]any{"id": 1}},
		{method: "PATCH", route: "products/{id}", url: "/products/2", pathParams: map[string]any{"id": 2}, body: map[string]any{"price": 3.5}},
		{method: "DELETE", route: "products/{id}", url: "/products/1", pathParams: map[string]any{"id": 1}, body: map[string]any{"confirm": 1}},
	})
}

func TestTextSchema(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{}`, `{"type":"string"}`},
		{`{"type":"integer"}`, `{"type":"integer"}`},
		{`{"type":["integer","null"]}`, `{"type":"integer"}`},
		{`{"type":["string","null"],"enum":["a",null]}`, `{"type":"string","enum":["a"]}`},
		{`{"type":["array","null"],"items":{"type":"integer"}}`, `{"type":"string","description":"JSON text."}`},
		{`{"anyOf":[{"type":"string"},{"type":"integer"}]}`, `{"type":"string"}`},
	}
	for _, c := range cases {
		assertJSON(t, textSchema(mustSchema(t, c.in)), c.want)
	}
}

func TestRouteErrors(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "schema.sql"), "CREATE TABLE p (id INTEGER);")
	root := filepath.Join(dir, "q")
	writeFile(t, filepath.Join(root, "ok", "GET.sql"), "SELECT id FROM p")
	writeFile(t, filepath.Join(root, "ok", "PROPFIND.sql"), "SELECT id FROM p")
	writeFile(t, filepath.Join(root, "files", "[...x]", "y", "GET.sql"), "SELECT id FROM p")
	writeFile(t, filepath.Join(root, "opt", "[[...x]]", "y", "GET.sql"), "SELECT id FROM p")
	var out, errOut bytes.Buffer
	err := run(t.Context(), []string{"-root", root, "-driver", "sqlite", "-schema", filepath.Join(dir, "schema.sql"), "-verb-named-files"}, &out, &errOut)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{
		"ok/PROPFIND.sql: OpenAPI 3.1 has no operation for the PROPFIND method",
		"files/[...x]/y/GET.sql: the handler never serves this route: [...x] must be its last segment",
		"opt/[[...x]]/y/GET.sql: the handler never serves this route",
	} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut.String())
		}
	}
	var spec document
	if err := yaml.Unmarshal(out.Bytes(), &spec); err != nil {
		t.Fatal(err)
	}
	if len(spec.Paths) != 1 || spec.Paths["/ok"] == nil || spec.Paths["/ok"].Get == nil {
		t.Errorf("paths = %v", spec.Paths)
	}
}
