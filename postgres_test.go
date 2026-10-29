package sqlexecutor

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/xuri/excelize/v2"
	"go.uber.org/zap"
)

// Postgres tests run only when SQL_EXECUTOR_TEST_POSTGRES_DSN points at a
// database the test may create and drop tables in, e.g.
//
//	SQL_EXECUTOR_TEST_POSTGRES_DSN=postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable go test ./...
func newPostgresHandler(t *testing.T, queries map[string]string, configure func(*SQLExecutor)) *SQLExecutor {
	t.Helper()
	dsn := os.Getenv("SQL_EXECUTOR_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SQL_EXECUTOR_TEST_POSTGRES_DSN not set")
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`
		DROP TABLE IF EXISTS sqlx_orders;
		CREATE TABLE sqlx_orders (
			id serial PRIMARY KEY,
			customer text NOT NULL,
			qty int NOT NULL,
			total numeric(10,2),
			paid boolean,
			placed_at timestamptz,
			ship_date date,
			meta jsonb,
			blob bytea,
			tags text[],
			ref uuid
		);
		INSERT INTO sqlx_orders (customer, qty, total, paid, placed_at, ship_date, meta, blob, tags, ref) VALUES
			('acme', 3, 19.99, true, '2024-05-06T07:08:09Z', '2024-05-10', '{"rush": true, "notes": ["fragile"]}', '\x00ff', '{a,b}', '6f1c3b2a-0d4e-4a8f-9b3c-2e1f0a9d8c7b'),
			('globex', 10, 1234.50, false, NULL, NULL, NULL, NULL, NULL, NULL);`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db, _ := sql.Open("pgx", dsn)
		db.Exec("DROP TABLE IF EXISTS sqlx_orders")
		db.Close()
	})

	dir := t.TempDir()
	writeTree(t, dir, queries)
	h := &SQLExecutor{Root: dir, Driver: "postgres", DSN: dsn}
	if configure != nil {
		configure(h)
	}
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	t.Cleanup(cancel)
	if err := h.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	h.logger = zap.NewNop()
	t.Cleanup(func() { h.Cleanup() })
	return h
}

func TestPostgres(t *testing.T) {
	h := newPostgresHandler(t, map[string]string{
		"orders.sql": `SELECT id, customer, qty, total, paid, placed_at, ship_date, meta, blob, tags, ref
FROM sqlx_orders
WHERE qty >= :min_qty
  AND (:customer::text IS NULL OR customer = :customer)
ORDER BY id`,
		"orders/create.sql": `INSERT INTO sqlx_orders (customer, qty, total, meta)
VALUES (:customer, :qty, :total, :meta::jsonb)
RETURNING id, customer, qty, total, meta`,
		"orders/by_ids.sql": "SELECT id FROM sqlx_orders WHERE id = ANY(SELECT jsonb_array_elements_text(:ids::jsonb)::int) ORDER BY id",
		"dollar.sql":        "SELECT $q$ :not_a_param $q$ AS s, :real::int AS n",
		"slow.sql":          "SELECT pg_sleep(5)",
		"broken.sql":        "SELEC 1",
		"orders/search.sql.hbs": `SELECT id, customer FROM sqlx_orders
WHERE true
{{#if ids}} AND id IN ({{#each ids}}{{this}}{{#unless @last}}, {{/unless}}{{/each}}){{/if}}
{{#if paid}} AND paid = {{paid}}{{/if}}
{{#if meta}} AND meta @> {{meta}}::jsonb{{/if}}
ORDER BY {{ident sort allow="id,customer"}} DESC`,
	}, func(h *SQLExecutor) { h.Timeout = caddy.Duration(300 * time.Millisecond) })

	t.Run("json types", func(t *testing.T) {
		res := serve(t, h, "POST", "/orders", `{"min_qty": 1, "customer": null}`, nil)
		want := `[{"id":1,"customer":"acme","qty":3,"total":"19.99","paid":true,"placed_at":"2024-05-06T07:08:09Z",` +
			`"ship_date":"2024-05-10","meta":{"rush":true,"notes":["fragile"]},"blob":"AP8=","tags":"{a,b}",` +
			`"ref":"6f1c3b2a-0d4e-4a8f-9b3c-2e1f0a9d8c7b"},` +
			`{"id":2,"customer":"globex","qty":10,"total":"1234.50","paid":false,"placed_at":null,"ship_date":null,` +
			`"meta":null,"blob":null,"tags":null,"ref":null}]` + "\n"
		if res.status != 200 || res.body != want {
			t.Errorf("status %d\n got: %s\nwant: %s", res.status, res.body, want)
		}
	})

	t.Run("string params from query string are coerced", func(t *testing.T) {
		res := serve(t, h, "POST", "/orders.csv?min_qty=5&customer=globex", "", nil)
		want := "id,customer,qty,total,paid,placed_at,ship_date,meta,blob,tags,ref\n2,globex,10,1234.50,false,,,,,,\n"
		if res.status != 200 || res.body != want {
			t.Errorf("status %d\n got: %q\nwant: %q", res.status, res.body, want)
		}
	})

	t.Run("yaml embeds json columns", func(t *testing.T) {
		res := serve(t, h, "POST", "/orders.yaml", `{"min_qty": 1, "customer": "acme"}`, nil)
		for _, frag := range []string{"meta:\n    notes:\n        - fragile\n    rush: true\n", "total: \"19.99\"\n", "paid: true\n"} {
			if !strings.Contains(res.body, frag) {
				t.Errorf("yaml missing %q:\n%s", frag, res.body)
			}
		}
	})

	t.Run("xlsx numeric columns are numbers", func(t *testing.T) {
		res := serve(t, h, "POST", "/orders.xlsx", `{"min_qty": 1, "customer": null}`, nil)
		f, err := excelize.OpenReader(strings.NewReader(res.body))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if v, _ := f.GetCellValue("orders", "D3"); v != "1234.5" {
			t.Errorf("D3 = %q", v)
		}
		if typ, _ := f.GetCellType("orders", "D3"); typ != excelize.CellTypeUnset {
			t.Errorf("D3 type = %v, want numeric", typ)
		}
	})

	t.Run("insert with json and float params", func(t *testing.T) {
		res := serve(t, h, "POST", "/orders/create", `{"customer": "initech", "qty": 2, "total": 5.5, "meta": {"via": "api"}}`, nil)
		want := `[{"id":3,"customer":"initech","qty":2,"total":"5.50","meta":{"via":"api"}}]` + "\n"
		if res.status != 200 || res.body != want {
			t.Errorf("status %d\n got: %s\nwant: %s", res.status, res.body, want)
		}
	})

	t.Run("array param", func(t *testing.T) {
		res := serve(t, h, "POST", "/orders/by_ids", `{"ids": [2, 1]}`, nil)
		if res.body != `[{"id":1},{"id":2}]`+"\n" {
			t.Errorf("got %s", res.body)
		}
	})

	t.Run("dollar quotes and casts", func(t *testing.T) {
		res := serve(t, h, "POST", "/dollar", `{"real": "7"}`, nil)
		if res.body != `[{"s":" :not_a_param ","n":7}]`+"\n" {
			t.Errorf("got %d %s", res.status, res.body)
		}
	})

	t.Run("template", func(t *testing.T) {
		res := serve(t, h, "POST", "/orders/search", `{"ids": [1, 2], "sort": "id"}`, nil)
		if res.status != 200 || res.body != `[{"id":2,"customer":"globex"},{"id":1,"customer":"acme"}]`+"\n" {
			t.Errorf("IN list: %d %s", res.status, res.body)
		}
		res = serve(t, h, "POST", "/orders/search", `{"paid": true, "meta": {"rush": true}, "sort": "customer"}`, nil)
		if res.status != 200 || res.body != `[{"id":1,"customer":"acme"}]`+"\n" {
			t.Errorf("typed bool and jsonb object: %d %s", res.status, res.body)
		}
	})

	t.Run("errors", func(t *testing.T) {
		if res := serve(t, h, "POST", "/slow", "", nil); res.status != 504 {
			t.Errorf("slow: %d %s", res.status, res.body)
		}
		if res := serve(t, h, "POST", "/broken", "", nil); res.status != 500 {
			t.Errorf("broken: %d %s", res.status, res.body)
		}
		// The pool must still work after a cancelled query.
		if res := serve(t, h, "POST", "/dollar", `{"real": 1}`, nil); res.status != 200 {
			t.Errorf("after timeout: %d %s", res.status, res.body)
		}
	})
}
