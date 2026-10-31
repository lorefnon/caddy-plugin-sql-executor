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
	got, err := ListQueryFiles(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []QueryFile{
		{Route: "a", File: "a.sql", Method: "POST"},
		{Route: "b", File: "b.sql.j2", Template: true, Method: "POST"},
		{Route: "dir.sql/inner", File: "dir.sql/inner.sql", Method: "POST"},
		{Route: "reports/c", File: "reports/c.sql", Method: "POST"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestListQueryFilesVerbNamed(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"GET.sql":                 "SELECT 0",
		"plain.sql":               "SELECT 1",
		"items/GET.sql":           "SELECT 2",
		"items/GET.sql.j2":        "SELECT 3",
		"items/POST.sql.j2":       "SELECT 4",
		"items/[id]/DELETE.sql":   "SELECT 5",
		"items/[id]/notaverb.sql": "SELECT 6",
		"files/[...p]/GET.sql":    "SELECT 7",
	})
	got, err := ListQueryFiles(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []QueryFile{
		{Route: "files/[...p]", File: "files/[...p]/GET.sql", Method: "GET"},
		{Route: "items", File: "items/GET.sql", Method: "GET"},
		{Route: "items", File: "items/POST.sql.j2", Template: true, Method: "POST"},
		{Route: "items/[id]", File: "items/[id]/DELETE.sql", Method: "DELETE"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}

func TestQueryFileSegments(t *testing.T) {
	got := QueryFile{Route: "a/[id]/[...rest]"}.Segments()
	want := []Segment{{StaticSegment, "a"}, {ParamSegment, "id"}, {CatchAllSegment, "rest"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	got = QueryFile{Route: "[[...x]]"}.Segments()
	if want := []Segment{{OptionalCatchAllSegment, "x"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
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
