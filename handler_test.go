package sqlexecutor

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/xuri/excelize/v2"
	"go.uber.org/zap"
)

var testQueries = map[string]string{
	"users.sql": `-- all users, optionally filtered
SELECT id, name, score, active, created_at, data
FROM users
WHERE (:min_score IS NULL OR score >= :min_score)
ORDER BY id`,
	"users/by_id.sql": "SELECT id, name FROM users WHERE id = :id",
	"users/create.sql": `INSERT INTO users (name, score, active, created_at)
VALUES (:name, :score, 1, '2024-03-01T00:00:00Z')
RETURNING id, name`,
	"empty.sql":        "SELECT id, name FROM users WHERE 0",
	"markup.sql":       `SELECT '<script>alert(1)</script>' AS "<b>col</b>", NULL AS missing`,
	"broken.sql":       "SELECT * FROM no_such_table",
	"unterminated.sql": "SELECT 'oops",
	"slow.sql":         "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c) SELECT count(*) FROM c",
	"json_param.sql":   "SELECT value FROM json_each(:ids) ORDER BY value",
	".hidden/x.sql":    "SELECT 1",
	"dir.sql/x.sql":    "SELECT 1",
	"notes.txt":        "not a query",
}

func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// newSQLiteHandler provisions a handler over a fresh SQLite database
// seeded with a small users table.
func newSQLiteHandler(t *testing.T, configure func(*SQLExecutor)) *SQLExecutor {
	t.Helper()
	dir := t.TempDir()
	queries := filepath.Join(dir, "queries")
	writeTree(t, queries, testQueries)

	dbPath := filepath.Join(dir, "test.db")
	seed, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = seed.Exec(`
		CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			score REAL,
			active BOOLEAN,
			created_at DATETIME,
			data BLOB
		);
		INSERT INTO users (name, score, active, created_at, data) VALUES
			('Ada', 9.5, 1, '2024-01-02T03:04:05Z', x'00ff'),
			('Grace, "Amazing"', 7, 0, '2024-02-03T04:05:06Z', NULL);`)
	seed.Close()
	if err != nil {
		t.Fatal(err)
	}

	h := &SQLExecutor{Root: queries, Driver: "sqlite", DSN: "file:" + dbPath}
	if configure != nil {
		configure(h)
	}
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	t.Cleanup(cancel)
	if err := h.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	h.logger = zap.NewNop() // expected query errors would otherwise flood test output
	if err := h.Validate(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Cleanup() })
	return h
}

type response struct {
	status    int
	header    http.Header
	body      string
	nextCalls int
}

func serve(t *testing.T, h *SQLExecutor, method, target, body string, headers map[string]string) response {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	var res response
	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		res.nextCalls++
		w.WriteHeader(http.StatusNotFound)
		return nil
	})
	if err := h.ServeHTTP(w, r, next); err != nil {
		t.Fatalf("ServeHTTP returned error: %v", err)
	}
	res.status, res.header, res.body = w.Code, w.Header(), w.Body.String()
	return res
}

func TestHandlerJSON(t *testing.T) {
	h := newSQLiteHandler(t, nil)
	res := serve(t, h, "POST", "/users", `{"min_score": null}`, nil)
	if res.status != 200 {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
	if ct := res.header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("content type %q", ct)
	}
	want := `[{"id":1,"name":"Ada","score":9.5,"active":true,"created_at":"2024-01-02T03:04:05Z","data":"AP8="},` +
		`{"id":2,"name":"Grace, \"Amazing\"","score":7,"active":false,"created_at":"2024-02-03T04:05:06Z","data":null}]` + "\n"
	if res.body != want {
		t.Errorf("body\n got: %s\nwant: %s", res.body, want)
	}
}

func TestHandlerFormats(t *testing.T) {
	h := newSQLiteHandler(t, nil)

	t.Run("csv via extension", func(t *testing.T) {
		res := serve(t, h, "POST", "/users.csv", `{"min_score": 8}`, nil)
		want := "id,name,score,active,created_at,data\n1,Ada,9.5,true,2024-01-02T03:04:05Z,AP8=\n"
		if res.status != 200 || res.body != want {
			t.Errorf("status %d body\n%s\nwant\n%s", res.status, res.body, want)
		}
		if ct := res.header.Get("Content-Type"); ct != "text/csv; charset=utf-8" {
			t.Errorf("content type %q", ct)
		}
	})

	t.Run("csv quoting", func(t *testing.T) {
		res := serve(t, h, "POST", "/users/by_id.csv", `{"id": 2}`, nil)
		want := "id,name\n2,\"Grace, \"\"Amazing\"\"\"\n"
		if res.body != want {
			t.Errorf("body\n%s\nwant\n%s", res.body, want)
		}
	})

	t.Run("yaml via Accept", func(t *testing.T) {
		res := serve(t, h, "POST", "/users", `{"min_score": null}`, map[string]string{"Accept": "application/yaml"})
		want := `- id: 1
  name: Ada
  score: 9.5
  active: true
  created_at: "2024-01-02T03:04:05Z"
  data: AP8=
- id: 2
  name: Grace, "Amazing"
  score: 7
  active: false
  created_at: "2024-02-03T04:05:06Z"
  data: null
`
		if res.status != 200 || res.body != want {
			t.Errorf("status %d body\n%s\nwant\n%s", res.status, res.body, want)
		}
		if ct := res.header.Get("Content-Type"); ct != "application/yaml; charset=utf-8" {
			t.Errorf("content type %q", ct)
		}
	})

	t.Run("xlsx", func(t *testing.T) {
		res := serve(t, h, "POST", "/users.xlsx", `{"min_score": null}`, nil)
		if res.status != 200 {
			t.Fatalf("status %d: %s", res.status, res.body)
		}
		if cd := res.header.Get("Content-Disposition"); cd != `attachment; filename=users.xlsx` {
			t.Errorf("content disposition %q", cd)
		}
		f, err := excelize.OpenReader(strings.NewReader(res.body))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if sheets := f.GetSheetList(); len(sheets) != 1 || sheets[0] != "users" {
			t.Fatalf("sheets %v", sheets)
		}
		rows, err := f.GetRows("users")
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 3 || strings.Join(rows[0], ",") != "id,name,score,active,created_at,data" {
			t.Fatalf("rows %v", rows)
		}
		if rows[1][0] != "1" || rows[1][1] != "Ada" || rows[1][2] != "9.5" || rows[1][3] != "TRUE" {
			t.Errorf("row 1 %v", rows[1])
		}
		// Timestamps are stored as real Excel dates.
		typ, err := f.GetCellType("users", "E2")
		if err != nil {
			t.Fatal(err)
		}
		if raw, _ := f.GetCellValue("users", "E2", excelize.Options{RawCellValue: true}); typ != excelize.CellTypeUnset || !strings.HasPrefix(raw, "45293.") {
			t.Errorf("E2 type %v raw %q, want an Excel serial date", typ, raw)
		}
	})

	t.Run("default format", func(t *testing.T) {
		h := newSQLiteHandler(t, func(h *SQLExecutor) { h.DefaultFormat = "csv" })
		res := serve(t, h, "POST", "/empty", "", map[string]string{"Accept": "*/*"})
		if res.body != "id,name\n" {
			t.Errorf("body %q", res.body)
		}
	})
}

func TestHandlerEmptyResults(t *testing.T) {
	h := newSQLiteHandler(t, nil)
	for target, want := range map[string]string{
		"/empty":      "[]\n",
		"/empty.json": "[]\n",
		"/empty.csv":  "id,name\n",
		"/empty.yaml": "[]\n",
	} {
		res := serve(t, h, "POST", target, "", nil)
		if res.status != 200 || res.body != want {
			t.Errorf("%s: status %d body %q, want %q", target, res.status, res.body, want)
		}
	}
}

func TestHandlerParams(t *testing.T) {
	h := newSQLiteHandler(t, nil)

	res := serve(t, h, "POST", "/users/by_id", "", nil)
	if res.status != 400 || !strings.Contains(res.body, "missing parameter(s): id") {
		t.Errorf("missing param: status %d body %s", res.status, res.body)
	}

	res = serve(t, h, "POST", "/users/by_id?id=1", "", nil)
	if res.body != `[{"id":1,"name":"Ada"}]`+"\n" {
		t.Errorf("query string param: %s", res.body)
	}

	res = serve(t, h, "POST", "/users/by_id", "id=2", map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if res.body != `[{"id":2,"name":"Grace, \"Amazing\""}]`+"\n" {
		t.Errorf("form param: %s", res.body)
	}

	res = serve(t, h, "POST", "/json_param", `{"ids": [3, 1, 2]}`, nil)
	if res.body != `[{"value":1},{"value":2},{"value":3}]`+"\n" {
		t.Errorf("array param: %s", res.body)
	}

	res = serve(t, h, "POST", "/users/by_id", `{"id":`, nil)
	if res.status != 400 {
		t.Errorf("malformed JSON: status %d", res.status)
	}
}

func TestHandlerWrites(t *testing.T) {
	h := newSQLiteHandler(t, nil)
	res := serve(t, h, "POST", "/users/create", `{"name": "Linus", "score": 5}`, nil)
	if res.status != 200 || res.body != `[{"id":3,"name":"Linus"}]`+"\n" {
		t.Fatalf("status %d body %s", res.status, res.body)
	}
	res = serve(t, h, "POST", "/users/by_id", `{"id": 3}`, nil)
	if res.body != `[{"id":3,"name":"Linus"}]`+"\n" {
		t.Errorf("inserted row not visible: %s", res.body)
	}
}

func TestHandlerRouting(t *testing.T) {
	h := newSQLiteHandler(t, nil)

	for _, target := range []string{"/missing", "/", "/users/", "/.hidden/x", "/notes", "/notes.txt", "/users.sql", "/dir"} {
		res := serve(t, h, "POST", target, "", nil)
		if res.nextCalls != 1 || res.status != 404 {
			t.Errorf("%s: expected fall-through to next handler, got status %d next=%d", target, res.status, res.nextCalls)
		}
	}

	res := serve(t, h, "GET", "/users", "", nil)
	if res.status != 405 || res.header.Get("Allow") != "POST" || res.nextCalls != 0 {
		t.Errorf("GET: status %d allow %q", res.status, res.header.Get("Allow"))
	}

	res = serve(t, h, "POST", "/users", `{"min_score":0}`, map[string]string{"Accept": "image/png"})
	if res.status != 406 {
		t.Errorf("unsupported Accept: status %d", res.status)
	}

	// An explicit extension wins over the Accept header.
	res = serve(t, h, "POST", "/empty.csv", "", map[string]string{"Accept": "text/html"})
	if res.status != 200 || res.body != "id,name\n" {
		t.Errorf("extension over Accept: status %d body %q", res.status, res.body)
	}
}

func TestHandlerErrors(t *testing.T) {
	h := newSQLiteHandler(t, func(h *SQLExecutor) {
		h.Timeout = caddy.Duration(200 * time.Millisecond)
		h.MaxBodySize = 64
	})

	res := serve(t, h, "POST", "/broken", "", nil)
	if res.status != 500 || res.body != `{"error":"query failed"}`+"\n" {
		t.Errorf("broken query: status %d body %s", res.status, res.body)
	}

	res = serve(t, h, "POST", "/unterminated", "", nil)
	if res.status != 500 {
		t.Errorf("unterminated literal: status %d body %s", res.status, res.body)
	}

	start := time.Now()
	res = serve(t, h, "POST", "/slow", "", nil)
	if res.status != 504 {
		t.Errorf("slow query: status %d body %s", res.status, res.body)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("timeout not enforced, took %v", elapsed)
	}

	res = serve(t, h, "POST", "/users/by_id", `{"id": 1, "padding": "`+strings.Repeat("x", 100)+`"}`, nil)
	if res.status != 413 {
		t.Errorf("large body: status %d", res.status)
	}

	verbose := newSQLiteHandler(t, func(h *SQLExecutor) { h.VerboseErrors = true })
	res = serve(t, verbose, "POST", "/broken", "", nil)
	var body map[string]string
	if err := json.Unmarshal([]byte(res.body), &body); err != nil || !strings.Contains(body["error"], "no_such_table") {
		t.Errorf("verbose error: %s", res.body)
	}
}

func TestHandlerPicksUpFileChanges(t *testing.T) {
	h := newSQLiteHandler(t, nil)
	if err := os.WriteFile(filepath.Join(h.Root, "late.sql"), []byte("SELECT 42 AS answer"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := serve(t, h, "POST", "/late", "", nil)
	if res.body != `[{"answer":42}]`+"\n" {
		t.Errorf("new file not served: %d %s", res.status, res.body)
	}
}

func TestProvisionErrors(t *testing.T) {
	dir := t.TempDir()
	tests := map[string]SQLExecutor{
		"missing root":     {Driver: "sqlite", DSN: "file::memory:"},
		"missing dir":      {Root: filepath.Join(dir, "nope"), Driver: "sqlite", DSN: "file::memory:"},
		"bad driver":       {Root: dir, Driver: "oracle", DSN: "x"},
		"missing dsn":      {Root: dir, Driver: "sqlite"},
		"bad format":       {Root: dir, Driver: "sqlite", DSN: "file::memory:", DefaultFormat: "xml"},
		"bad formats":      {Root: dir, Driver: "sqlite", DSN: "file::memory:", Formats: []string{"json", "xml"}},
		"disabled default": {Root: dir, Driver: "sqlite", DSN: "file::memory:", Formats: []string{"csv"}, DefaultFormat: "json"},
		"bad s3 url":       {Root: "s3:///prefix", Driver: "sqlite", DSN: "file::memory:"},
	}
	for name, h := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
			defer cancel()
			if err := h.Provision(ctx); err == nil {
				h.Cleanup()
				t.Error("expected error")
			}
		})
	}
}

func TestProvisionExpandsPlaceholders(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SQLX_TEST_ROOT", dir)
	t.Setenv("SQLX_TEST_DSN", "file:"+filepath.Join(dir, "x.db"))
	h := &SQLExecutor{Root: "{env.SQLX_TEST_ROOT}", Driver: "sqlite", DSN: "{env.SQLX_TEST_DSN}"}
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()
	if err := h.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	defer h.Cleanup()
	if err := h.db.Ping(); err != nil {
		t.Fatal(err)
	}
}

func TestHandlerHTML(t *testing.T) {
	h := newSQLiteHandler(t, nil)

	for name, req := range map[string]struct{ target, accept string }{
		"extension": {"/markup.html", ""},
		"accept":    {"/markup", "text/html,application/xhtml+xml,*/*;q=0.8"},
	} {
		res := serve(t, h, "POST", req.target, "", map[string]string{"Accept": req.accept})
		if res.status != 200 || res.header.Get("Content-Type") != "text/html; charset=utf-8" {
			t.Fatalf("%s: status %d type %q", name, res.status, res.header.Get("Content-Type"))
		}
		if csp := res.header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
			t.Errorf("%s: CSP %q", name, csp)
		}
		for _, want := range []string{
			"<details>", "<summary>", "</details>",
			"<th>&lt;b&gt;col&lt;/b&gt;</th>",
			"<td>&lt;script&gt;alert(1)&lt;/script&gt;</td>",
			`<td class="null"></td>`,
			"SELECT &#39;&lt;script&gt;alert(1)&lt;/script&gt;&#39; AS",
		} {
			if !strings.Contains(res.body, want) {
				t.Errorf("%s: body lacks %q:\n%s", name, want, res.body)
			}
		}
		// The query sits in a closed accordion, before the table.
		if strings.Contains(res.body, "<details open") || strings.Index(res.body, "<details>") > strings.Index(res.body, "<table>") {
			t.Errorf("%s: query should be a collapsed accordion above the table", name)
		}
		// Nothing but the (escaped) data may mention a script.
		if strings.Contains(strings.ToLower(res.body), "<script") {
			t.Errorf("%s: page must not contain scripts:\n%s", name, res.body)
		}
	}

	res := serve(t, h, "POST", "/users.html", `{"min_score":0}`, nil)
	if res.status != 200 || !strings.Contains(res.body, "Grace, &#34;Amazing&#34;") || !strings.Contains(res.body, "<td>AP8=</td>") {
		t.Errorf("users: status %d body %s", res.status, res.body)
	}

	res = serve(t, h, "POST", "/empty.html", "", nil)
	if res.status != 200 || !strings.Contains(res.body, "<th>id</th><th>name</th>") || strings.Contains(res.body, "<td") {
		t.Errorf("empty: status %d body %s", res.status, res.body)
	}

	// JSON stays the default when nothing picks a format.
	res = serve(t, h, "POST", "/empty", "", nil)
	if res.body != "[]\n" {
		t.Errorf("default body %q", res.body)
	}
}

func TestHandlerRestrictedFormats(t *testing.T) {
	h := newSQLiteHandler(t, func(h *SQLExecutor) { h.Formats = []string{"csv", "yaml"} })

	if res := serve(t, h, "POST", "/empty", "", nil); res.status != 200 || res.body != "id,name\n" {
		t.Errorf("default should fall back to the first format: status %d body %q", res.status, res.body)
	}
	if res := serve(t, h, "POST", "/empty.yaml", "", nil); res.status != 200 {
		t.Errorf("yaml: status %d", res.status)
	}
	for _, target := range []string{"/empty.json", "/empty.html", "/empty.xlsx"} {
		res := serve(t, h, "POST", target, "", nil)
		if res.status != 406 || !strings.Contains(res.body, "text/csv, application/yaml") || res.nextCalls != 0 {
			t.Errorf("%s: status %d body %s", target, res.status, res.body)
		}
	}
	for _, accept := range []string{"application/json", "text/html"} {
		res := serve(t, h, "POST", "/empty", "", map[string]string{"Accept": accept})
		if res.status != 406 {
			t.Errorf("Accept %s: status %d", accept, res.status)
		}
	}
	// A disabled format does not hide queries that do not exist.
	if res := serve(t, h, "POST", "/missing.json", "", nil); res.nextCalls != 1 {
		t.Errorf("missing query should fall through, status %d", res.status)
	}
}
