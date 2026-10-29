package sqlexecutor

import (
	"bytes"
	"database/sql"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/caddyserver/caddy/v2/caddytest"
	_ "github.com/caddyserver/caddy/v2/modules/standard"
)

// TestEndToEnd boots a real Caddy server from a Caddyfile and exercises
// the handler over HTTP, including prefix stripping and fall-through.
func TestEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a Caddy server")
	}
	dir := t.TempDir()
	queries := filepath.Join(dir, "queries")
	writeTree(t, queries, map[string]string{
		"items.sql":     "SELECT id, label FROM items WHERE id >= :min ORDER BY id",
		"items/add.sql": "INSERT INTO items (label) VALUES (:label) RETURNING id, label",
	})
	dbPath := filepath.Join(dir, "e2e.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE items (id INTEGER PRIMARY KEY, label TEXT);
		INSERT INTO items (label) VALUES ('alpha'), ('beta');`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	tester := caddytest.NewTester(t)
	tester.InitServer(fmt.Sprintf(`
	{
		skip_install_trust
		admin localhost:2999
		http_port 9080
		https_port 9443
		grace_period 1ns
	}
	http://localhost:9080 {
		handle_path /api/* {
			sql_executor {
				root %q
				driver sqlite
				dsn %q
			}
		}
		respond "fallback" 404
	}`, queries, "file:"+dbPath+"?_pragma=busy_timeout(5000)"), "caddyfile")

	jsonHeader := []string{"Content-Type: application/json"}
	tester.AssertPostResponseBody("http://localhost:9080/api/items", jsonHeader,
		bytes.NewBufferString(`{"min": 2}`), 200, `[{"id":2,"label":"beta"}]`+"\n")
	tester.AssertPostResponseBody("http://localhost:9080/api/items/add.csv", jsonHeader,
		bytes.NewBufferString(`{"label": "gamma"}`), 200, "id,label\n3,gamma\n")

	req, _ := http.NewRequest("POST", "http://localhost:9080/api/items?min=0", nil)
	req.Header.Set("Accept", "text/yaml")
	tester.AssertResponse(req, 200, "- id: 1\n  label: alpha\n- id: 2\n  label: beta\n- id: 3\n  label: gamma\n")

	tester.AssertGetResponse("http://localhost:9080/api/items", 405, `{"error":"only POST is allowed"}`+"\n")
	tester.AssertPostResponseBody("http://localhost:9080/api/nope", nil, &bytes.Buffer{}, 404, "fallback")
}
