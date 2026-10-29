package sqlexecutor

import (
	"reflect"
	"strings"
	"testing"
)

func TestCompileHeaderDirectives(t *testing.T) {
	stmt, err := compile(`-- @header :uid = X-User-Id
--@header :tenant=X-Tenant
-- @headers are set by the proxy (ordinary comment)
/* @header :ignored = In-Block-Comment */
SELECT '-- @header :nope = Inside-String', :uid, :tenant, :other`, styleDollar)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"uid": "X-User-Id", "tenant": "X-Tenant"}
	if !reflect.DeepEqual(stmt.Headers, want) {
		t.Errorf("headers %v, want %v", stmt.Headers, want)
	}
	// Directives stay in the SQL as ordinary comments.
	if !strings.HasPrefix(stmt.SQL, "-- @header :uid = X-User-Id\n") || !strings.Contains(stmt.SQL, "$1, $2, $3") {
		t.Errorf("sql %q", stmt.SQL)
	}

	for in, wantErr := range map[string]string{
		"-- @header uid = X-User-Id\nSELECT :uid":               "malformed directive",
		"-- @header :uid X-User-Id\nSELECT :uid":                "malformed directive",
		"-- @header :uid = X User\nSELECT :uid":                 "malformed directive",
		"-- @header\nSELECT 1":                                  "malformed directive",
		"-- @header :uid = A\n-- @header :uid = B\nSELECT :uid": "mapped to both A and B",
		"-- @header :uid = X-User-Id\nSELECT :user_id":          "does not use :uid",
		"-- @header :uid = X-User-Id\nSELECT ':uid'":            "does not use :uid",
	} {
		if _, err := compile(in, styleQuestion); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("compile(%q) error %v, want %q", in, err, wantErr)
		}
	}
}

var directiveQueries = map[string]string{
	"me.sql": `-- The caller's identity comes from the auth proxy, never the body.
-- @header :uid = X-User-Id
SELECT id, name FROM users WHERE id = :uid`,
	"me_named.sql.hbs": `-- @header :uid = X-User-Id
SELECT id, name FROM users WHERE id = :uid{{#if name}} AND name = {{name}}{{/if}}`,
	"typo.sql": "-- @header :uid = X-User-Id\nSELECT id FROM users WHERE id = :user_id",
}

func TestHeaderDirectiveHandler(t *testing.T) {
	h := newSQLiteHandler(t, func(h *SQLExecutor) { h.VerboseErrors = true })
	writeTree(t, h.Root, directiveQueries)
	ada := `[{"id":1,"name":"Ada"}]` + "\n"

	res := serve(t, h, "POST", "/me", "", map[string]string{"X-User-Id": "1"})
	if res.status != 200 || res.body != ada {
		t.Errorf("header: %d %s", res.status, res.body)
	}

	// The header wins over body and query string values of the same name.
	res = serve(t, h, "POST", "/me?uid=2", `{"uid": 2}`, map[string]string{"X-User-Id": "1"})
	if res.status != 200 || res.body != ada {
		t.Errorf("override: %d %s", res.status, res.body)
	}

	// Without the header the request is rejected, even if the body has uid.
	res = serve(t, h, "POST", "/me", `{"uid": 1}`, nil)
	if res.status != 400 || !strings.Contains(res.body, "missing header X-User-Id (for :uid)") {
		t.Errorf("missing header: %d %s", res.status, res.body)
	}

	// Header values are bound, not spliced into SQL.
	res = serve(t, h, "POST", "/me", "", map[string]string{"X-User-Id": "1 OR 1=1"})
	if res.status != 200 || res.body != "[]\n" {
		t.Errorf("injection: %d %s", res.status, res.body)
	}

	// Directives work in templates too.
	res = serve(t, h, "POST", "/me_named", `{"name": "Ada", "uid": 2}`, map[string]string{"X-User-Id": "1"})
	if res.status != 200 || res.body != ada {
		t.Errorf("template: %d %s", res.status, res.body)
	}

	// A mapping to a placeholder the query doesn't use is a file bug.
	res = serve(t, h, "POST", "/typo", `{"user_id": 1}`, map[string]string{"X-User-Id": "1"})
	if res.status != 500 || !strings.Contains(res.body, "does not use :uid") {
		t.Errorf("typo: %d %s", res.status, res.body)
	}
}
