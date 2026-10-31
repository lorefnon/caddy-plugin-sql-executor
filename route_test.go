package sqlexecutor

import (
	"testing"
)

var dynamicQueries = map[string]string{
	"users/[id].sql":                "SELECT 'user' AS kind, :id AS id",
	"users/me.sql":                  "SELECT 'me' AS kind",
	"users/[id]/orders.sql":         "SELECT 'orders' AS kind, :id AS id",
	"users/[id]/orders/[order].sql": "SELECT 'order' AS kind, :id AS id, :order AS \"order\"",
	"users/[id]/[...rest].sql":      "SELECT 'rest' AS kind, :id AS id, :rest AS rest",
	"docs/[...slug].sql":            "SELECT 'docs' AS kind, :slug AS slug",
	"shop/[[...cat]].sql":           "SELECT 'shop' AS kind, :cat AS cat",
	"tpl/[name].sql.j2":             "SELECT {{ name }} AS name",
	"fmt-only/[x]/static.sql":       "SELECT 'static' AS kind",
	"fmt-only/[x]/[y].sql":          "SELECT 'dynamic' AS kind, :x AS x, :y AS y",
	"bad/[foo-bar].sql":             "SELECT 'literal' AS kind",
	"[tenant]/report.sql":           "SELECT 'report' AS kind, :tenant AS tenant",
	"hdr/[who].sql":                 "-- @header :who = X-Who\nSELECT :who AS who",
}

func TestDynamicRoutes(t *testing.T) {
	h := newSQLiteHandler(t, func(h *SQLExecutor) { writeTree(t, h.Root, dynamicQueries) })

	tests := []struct {
		target string
		body   string
		want   string // "" means the request falls through to the next handler
	}{
		{"/users/42", "", `[{"kind":"user","id":"42"}]`},
		{"/users/42.csv", "", "kind,id\nuser,42\n"},
		{"/users/me", "", `[{"kind":"me"}]`},
		{"/users/42/orders", "", `[{"kind":"orders","id":"42"}]`},
		{"/users/42/orders/7", "", `[{"kind":"order","id":"42","order":"7"}]`},
		{"/users/42/other/deep", "", `[{"kind":"rest","id":"42","rest":"[\"other\",\"deep\"]"}]`},
		{"/docs/a", "", `[{"kind":"docs","slug":"[\"a\"]"}]`},
		{"/docs/a/b/c", "", `[{"kind":"docs","slug":"[\"a\",\"b\",\"c\"]"}]`},
		{"/docs", "", ""}, // [...slug] needs at least one segment
		{"/shop", "", `[{"kind":"shop","cat":"[]"}]`},
		{"/shop/x/y", "", `[{"kind":"shop","cat":"[\"x\",\"y\"]"}]`},
		{"/tpl/hello", "", `[{"name":"hello"}]`},
		// A static match that dead-ends falls back to the dynamic sibling.
		{"/fmt-only/a/static", "", `[{"kind":"static"}]`},
		{"/fmt-only/a/b", "", `[{"kind":"dynamic","x":"a","y":"b"}]`},
		// A top-level dynamic directory.
		{"/acme/report", "", `[{"kind":"report","tenant":"acme"}]`},
		// Names that aren't valid parameter names are literal.
		{"/bad/[foo-bar]", "", `[{"kind":"literal"}]`},
		{"/bad/anything", "", ""},
		// A request can't spell out a dynamic file name to skip capturing.
		{"/users/[id]/orders", "", `[{"kind":"orders","id":"[id]"}]`},
		// The path wins over the body and query string.
		{"/users/42?id=1", `{"id": 2}`, `[{"kind":"user","id":"42"}]`},
	}
	for _, tt := range tests {
		res := serve(t, h, "POST", tt.target, tt.body, nil)
		if tt.want == "" {
			if res.nextCalls != 1 {
				t.Errorf("%s: expected fall-through, got status %d body %q", tt.target, res.status, res.body)
			}
			continue
		}
		if res.status != 200 || res.body != tt.want+"\n" && res.body != tt.want {
			t.Errorf("%s: status %d, body %q, want %q", tt.target, res.status, res.body, tt.want)
		}
	}

	// Header directives still win over the path.
	res := serve(t, h, "POST", "/hdr/path", "", map[string]string{"X-Who": "header"})
	if res.status != 200 || res.body != `[{"who":"header"}]`+"\n" {
		t.Errorf("header over path: status %d body %q", res.status, res.body)
	}

	// Dynamic routes are POST-only too.
	res = serve(t, h, "GET", "/users/42", "", nil)
	if res.status != 405 {
		t.Errorf("GET on dynamic route: status %d", res.status)
	}
}

var verbQueries = map[string]string{
	"items/GET.sql":             "SELECT 'list' AS op",
	"items/POST.sql":            "SELECT 'create' AS op",
	"items/[id]/GET.sql":        "SELECT 'get' AS op, :id AS id",
	"items/[id]/DELETE.sql":     "SELECT 'delete' AS op, :id AS id",
	"items/[id]/PATCH.sql.j2":   "SELECT 'patch' AS op, {{ id }} AS id",
	"files/[...path]/GET.sql":   "SELECT 'file' AS op, :path AS path",
	"tree/[[...path]]/GET.sql":  "SELECT 'tree' AS op, :path AS path",
	"plain.sql":                 "SELECT 'not routed in verb mode' AS op",
	"items/[id]/orders/GET.sql": "SELECT 'orders' AS op, :id AS id",
	"items/special/GET.sql":     "SELECT 'special' AS op",
}

func TestVerbNamedFiles(t *testing.T) {
	h := newSQLiteHandler(t, func(h *SQLExecutor) {
		h.VerbNamedFiles = true
		writeTree(t, h.Root, verbQueries)
	})

	tests := []struct {
		method, target, body, want string
	}{
		{"GET", "/items", "", `[{"op":"list"}]`},
		{"POST", "/items", "", `[{"op":"create"}]`},
		{"GET", "/items.csv", "", "op\nlist\n"},
		{"GET", "/items/7", "", `[{"op":"get","id":"7"}]`},
		{"DELETE", "/items/7", "", `[{"op":"delete","id":"7"}]`},
		{"PATCH", "/items/7", "", `[{"op":"patch","id":"7"}]`},
		{"GET", "/items/7/orders", "", `[{"op":"orders","id":"7"}]`},
		{"GET", "/items/special", "", `[{"op":"special"}]`},
		{"GET", "/files/a/b", "", `[{"op":"file","path":"[\"a\",\"b\"]"}]`},
		{"GET", "/tree", "", `[{"op":"tree","path":"[]"}]`},
		{"GET", "/tree/a/b", "", `[{"op":"tree","path":"[\"a\",\"b\"]"}]`},
		// A method without a file for it isn't handled.
		{"PUT", "/items", "", ""},
		{"DELETE", "/items", "", ""},
		{"GET", "/files", "", ""},
		{"GET", "/plain", "", ""},
		{"POST", "/plain", "", ""},
		{"GET", "/nothing", "", ""},
		{"GET", "/", "", ""},
		// A file can't be asked for by its own name.
		{"GET", "/items/GET", "", `[{"op":"get","id":"GET"}]`},
		{"GET", "/items/7/GET", "", ""},
	}
	for _, tt := range tests {
		res := serve(t, h, tt.method, tt.target, tt.body, nil)
		if tt.want == "" {
			if res.nextCalls != 1 {
				t.Errorf("%s %s: expected fall-through, got status %d body %q", tt.method, tt.target, res.status, res.body)
			}
			continue
		}
		if res.status != 200 || res.body != tt.want+"\n" && res.body != tt.want {
			t.Errorf("%s %s: status %d, body %q, want %q", tt.method, tt.target, res.status, res.body, tt.want)
		}
	}

	// Bodies on non-POST methods are read like any other.
	res := serve(t, h, "DELETE", "/items/7", `{"x": 1}`, nil)
	if res.status != 200 {
		t.Errorf("DELETE with body: status %d body %q", res.status, res.body)
	}
}

func TestVerbNamedFilesOffIgnoresVerbFiles(t *testing.T) {
	h := newSQLiteHandler(t, func(h *SQLExecutor) { writeTree(t, h.Root, verbQueries) })
	for _, target := range []string{"/items/7"} {
		if res := serve(t, h, "DELETE", target, "", nil); res.nextCalls != 1 {
			t.Errorf("DELETE %s with verb_named_files off: status %d", target, res.status)
		}
	}
}

func TestValidVerb(t *testing.T) {
	for m, want := range map[string]bool{"GET": true, "M-SEARCH": true, "": false, "get": false, ".": false, "A/B": false, "G.T": false} {
		if got := validVerb(m); got != want {
			t.Errorf("validVerb(%q) = %v, want %v", m, got, want)
		}
	}
}
