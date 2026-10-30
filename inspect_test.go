package sqlexecutor

import (
	"net/http"
	"reflect"
	"testing"
)

func TestListQueryFiles(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"a.sql":             "SELECT 1",
		"a.sql.j2":          "SELECT 2",
		"b.sql.j2":          "SELECT 3",
		"reports/c.sql":     "SELECT 4",
		"partials/p.j2":     "AND 1 = 1",
		".hidden/x.sql":     "SELECT 5",
		"reports/.y.sql":    "SELECT 6",
		"notes.txt":         "not a query",
		"dir.sql/inner.sql": "SELECT 7",
	})
	got, err := ListQueryFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []QueryFile{
		{Route: "a", File: "a.sql"},
		{Route: "b", File: "b.sql.j2", Template: true},
		{Route: "dir.sql/inner", File: "dir.sql/inner.sql"},
		{Route: "reports/c", File: "reports/c.sql"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestCompiler(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"mine.sql": "-- @header :user = X-User\nSELECT * FROM t WHERE owner = :user AND id = :id",
		"t.sql.j2": `SELECT * FROM t WHERE tenant = {{ header("x-tenant") }} AND id = :id{% if name %} AND name = {{ name }}{% endif %}`,
	})
	c, err := NewCompiler(dir, "postgres", false)
	if err != nil {
		t.Fatal(err)
	}

	q, err := c.Compile(t.Context(), "mine.sql", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if q.SQL != "-- @header :user = X-User\nSELECT * FROM t WHERE owner = $1 AND id = $2" ||
		!reflect.DeepEqual(q.Params, []string{"user", "id"}) ||
		!reflect.DeepEqual(q.Headers, map[string]string{"user": "X-User"}) {
		t.Errorf("got %+v", q)
	}

	q, err = c.Compile(t.Context(), "t.sql.j2", map[string]any{"name": "x"}, http.Header{"X-Tenant": {"1"}})
	if err != nil {
		t.Fatal(err)
	}
	if q.SQL != "SELECT * FROM t WHERE tenant = $1 AND id = $2 AND name = $3" ||
		!reflect.DeepEqual(q.HeadersRead, []string{"X-Tenant"}) ||
		len(q.Params) != 3 || !IsGenerated(q.Params[0]) || q.Params[1] != "id" || !IsGenerated(q.Params[2]) {
		t.Errorf("got %+v", q)
	}
}
