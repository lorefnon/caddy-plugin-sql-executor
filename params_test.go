package sqlexecutor

import (
	"errors"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestCompile(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		style      placeholderStyle
		wantSQL    string
		wantParams []string
	}{
		{
			name:       "dollar style reuses index for repeated names",
			in:         "SELECT * FROM t WHERE a = :a AND b = :b OR a2 = :a",
			style:      styleDollar,
			wantSQL:    "SELECT * FROM t WHERE a = $1 AND b = $2 OR a2 = $1",
			wantParams: []string{"a", "b"},
		},
		{
			name:       "question style repeats arguments",
			in:         "SELECT * FROM t WHERE a = :a AND b = :b OR a2 = :a",
			style:      styleQuestion,
			wantSQL:    "SELECT * FROM t WHERE a = ? AND b = ? OR a2 = ?",
			wantParams: []string{"a", "b", "a"},
		},
		{
			name:       "casts are not parameters",
			in:         "SELECT :id::int, now()::date",
			style:      styleDollar,
			wantSQL:    "SELECT $1::int, now()::date",
			wantParams: []string{"id"},
		},
		{
			name:       "string literals are untouched",
			in:         "SELECT ':nope', 'it''s :nope', :yes",
			style:      styleDollar,
			wantSQL:    "SELECT ':nope', 'it''s :nope', $1",
			wantParams: []string{"yes"},
		},
		{
			name:       "E-strings honour backslash escapes",
			in:         `SELECT E'\' :nope', :yes`,
			style:      styleDollar,
			wantSQL:    `SELECT E'\' :nope', $1`,
			wantParams: []string{"yes"},
		},
		{
			name:       "quoted identifiers are untouched",
			in:         "SELECT \"col:x\", `col:y` FROM t WHERE z = :z",
			style:      styleQuestion,
			wantSQL:    "SELECT \"col:x\", `col:y` FROM t WHERE z = ?",
			wantParams: []string{"z"},
		},
		{
			name:       "comments are untouched",
			in:         "-- filter by :nope\nSELECT /* :nope /* nested :nope */ */ :yes",
			style:      styleDollar,
			wantSQL:    "-- filter by :nope\nSELECT /* :nope /* nested :nope */ */ $1",
			wantParams: []string{"yes"},
		},
		{
			name:       "dollar-quoted strings are untouched",
			in:         "SELECT $$ :nope $$, $tag$ :nope $$ $tag$, :yes",
			style:      styleDollar,
			wantSQL:    "SELECT $$ :nope $$, $tag$ :nope $$ $tag$, $1",
			wantParams: []string{"yes"},
		},
		{
			name:       "names may contain digits and underscores",
			in:         "SELECT :user_id2, :_x",
			style:      styleDollar,
			wantSQL:    "SELECT $1, $2",
			wantParams: []string{"user_id2", "_x"},
		},
		{
			name:    "colon followed by a digit is not a parameter",
			in:      "SELECT arr[1:2], '10:30'",
			style:   styleDollar,
			wantSQL: "SELECT arr[1:2], '10:30'",
		},
		{
			name:    "no parameters",
			in:      "SELECT 1",
			style:   styleQuestion,
			wantSQL: "SELECT 1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := compile(tt.in, tt.style)
			if err != nil {
				t.Fatal(err)
			}
			if got.SQL != tt.wantSQL {
				t.Errorf("SQL\n got: %s\nwant: %s", got.SQL, tt.wantSQL)
			}
			if !reflect.DeepEqual(got.Params, tt.wantParams) {
				t.Errorf("params got %v, want %v", got.Params, tt.wantParams)
			}
		})
	}
}

func TestCompileErrors(t *testing.T) {
	for _, in := range []string{
		"SELECT 'unterminated",
		`SELECT "unterminated`,
		"SELECT /* unterminated",
		"SELECT /* /* nested */",
	} {
		if _, err := compile(in, styleDollar); err == nil {
			t.Errorf("compile(%q): expected error", in)
		}
	}
}

func TestBind(t *testing.T) {
	stmt, _ := compile("SELECT :a, :b, :a", styleQuestion)
	args, err := stmt.bind(map[string]any{"a": int64(1), "b": nil, "extra": "ignored"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []any{int64(1), nil, int64(1)}; !reflect.DeepEqual(args, want) {
		t.Errorf("got %v, want %v", args, want)
	}

	_, err = stmt.bind(map[string]any{})
	if err == nil || err.Error() != "missing parameter(s): a, b" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestReadParams(t *testing.T) {
	t.Run("json body overrides query string", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/q?a=from-query&c=3", strings.NewReader(
			`{"a":"from-body","i":42,"f":1.5,"big":1e400,"t":true,"n":null,"arr":[1,2],"obj":{"k":"v"}}`))
		r.Header.Set("Content-Type", "application/json")
		_, err := readParams(r, 1<<20)
		if err == nil || !strings.Contains(err.Error(), "big") {
			t.Fatalf("expected out-of-range number error, got %v", err)
		}

		r = httptest.NewRequest("POST", "/q?a=from-query&c=3", strings.NewReader(
			`{"a":"from-body","i":42,"f":1.5,"t":true,"n":null,"arr":[1, 2],"obj":{"k":"v"}}`))
		got, err := readParams(r, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]any{
			"a": "from-body", "c": "3", "i": int64(42), "f": 1.5, "t": true, "n": nil,
			"arr": "[1, 2]", "obj": `{"k":"v"}`,
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %#v\nwant %#v", got, want)
		}
	})

	t.Run("form body", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/q", strings.NewReader("a=1&b=x+y"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		got, err := readParams(r, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if want := map[string]any{"a": "1", "b": "x y"}; !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("JSON sent with curl's default form content type", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/q", strings.NewReader(` {"a": 1}`))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		got, err := readParams(r, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if want := map[string]any{"a": int64(1)}; !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("empty body", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/q?a=1", strings.NewReader("  \n"))
		got, err := readParams(r, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if want := map[string]any{"a": "1"}; !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("body too large", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/q", strings.NewReader(`{"a":"0123456789"}`))
		if _, err := readParams(r, 10); !errors.Is(err, errBodyTooLarge) {
			t.Errorf("expected errBodyTooLarge, got %v", err)
		}
	})

	t.Run("non-object JSON", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/q", strings.NewReader(`[1,2]`))
		if _, err := readParams(r, 1<<20); err == nil {
			t.Error("expected error")
		}
	})

	t.Run("unsupported content type", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/q", strings.NewReader(`<a/>`))
		r.Header.Set("Content-Type", "application/xml")
		if _, err := readParams(r, 1<<20); err == nil {
			t.Error("expected error")
		}
	})
}
