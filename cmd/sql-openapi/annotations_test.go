package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnnotations(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "schema.sql"), "CREATE TABLE p (id INTEGER PRIMARY KEY, category TEXT NOT NULL, price REAL NOT NULL, note TEXT);")
	writeFile(t, filepath.Join(dir, "q", "summary.sql"), `-- Totals per category.
-- @column total = {description: Sum of prices.}
-- @column avg = [number, "null"]
-- @column "the note" = {type: string, description: Any note.}
-- @param :min = {type: number, description: Lower bound.}
-- @headers are set elsewhere (not a directive)
SELECT category, sum(price) AS total, avg(price) AS avg, max(note) AS "the note", count(*) AS n
FROM p WHERE price >= :min GROUP BY category`)
	writeFile(t, filepath.Join(dir, "q", "pick.sql.j2"), `-- @param :cats = {type: array, items: {type: string}}
{% if wide %}-- @column note = string
{% endif %}SELECT id{% if wide %}, note{% endif %} FROM p
{% if cats %}WHERE category IN ({% for c in cats %}{{ c }}{% if not loop.last %},{% endif %}{% endfor %}){% endif %}`)
	writeFile(t, filepath.Join(dir, "q", "described.sql"), `-- Not this.
-- @description Prices by category.
-- @description
-- @description   - indented
SELECT id FROM p`)
	writeFile(t, filepath.Join(dir, "config.yaml"), `
root: q
driver: sqlite
schema: schema.sql
queries:
  summary.sql:
    columns:
      avg: { description: From the config. }
    params:
      min: { type: integer }
  pick:
    samples:
      - { cats: [a] }
      - { wide: true, cats: [] }
`)
	spec := specFor(t, "-config", filepath.Join(dir, "config.yaml"))

	op := spec.Paths["/summary"].Post
	if op.Description != "Totals per category.\n@headers are set elsewhere (not a directive)" {
		t.Errorf("description = %q", op.Description)
	}
	if got := spec.Paths["/described"].Post.Description; got != "Prices by category.\n\n  - indented" {
		t.Errorf("description = %q", got)
	}
	assertProperties(t, spec.Components.Schemas["SummaryRow"], map[string]string{
		// A description alone keeps the inferred type.
		"total": `{"type":"number","description":"Sum of prices."}`,
		// The config's description is added to the annotation.
		"avg":      `{"type":["number","null"],"description":"From the config."}`,
		"the note": `{"type":"string","description":"Any note."}`,
		"n":        `{"type":"integer"}`,
	})
	// The config's type replaces the annotation's, keeping its description.
	assertProperties(t, op.RequestBody.Content["application/json"].Schema, map[string]string{
		"min": `{"type":"integer","description":"Lower bound."}`,
	})

	assertProperties(t, spec.Paths["/pick"].Post.RequestBody.Content["application/json"].Schema, map[string]string{
		"cats": `{"type":"array","items":{"type":"string"}}`,
	})
	// Annotations are read from the rendered SQL, so they can be conditional.
	assertJSON(t, spec.Components.Schemas["PickRow"],
		`{"anyOf":[{"type":"object","properties":{"id":{"type":"integer","format":"int64"}},"required":["id"]},`+
			`{"type":"object","properties":{"id":{"type":"integer","format":"int64"},"note":{"type":"string"}},"required":["id","note"]}]}`)
}

func TestDescriptionDiffersBetweenSamples(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "schema.sql"), "CREATE TABLE p (id INTEGER);")
	writeFile(t, filepath.Join(dir, "q", "t.sql.j2"), "{% if wide %}-- @description Wide{% else %}-- @description Narrow{% endif %}\nSELECT id FROM p")
	writeFile(t, filepath.Join(dir, "config.yaml"), `
root: q
driver: sqlite
schema: schema.sql
queries:
  t: { samples: [{}, { wide: true }] }
`)
	var out, errOut bytes.Buffer
	if err := run(t.Context(), []string{"-config", filepath.Join(dir, "config.yaml")}, &out, &errOut); err == nil ||
		!strings.Contains(errOut.String(), "@description differs between samples") {
		t.Errorf("stderr %q", errOut.String())
	}
}

func TestAnnotationErrors(t *testing.T) {
	for sql, want := range map[string]string{
		"-- @column nope = string\nSELECT id FROM p":                                      "@column nope: the query has no column nope",
		"-- @param :nope = string\nSELECT id FROM p WHERE id = :id":                       "@param :nope: the query has no parameter :nope",
		"-- @param :id = int\nSELECT id FROM p WHERE id = :id":                            "unknown type int",
		"-- @column id = {type: integer, minimum: 1}\nSELECT id FROM p":                   "field minimum not found",
		"-- @column id\nSELECT id FROM p":                                                 "expected -- @column name = schema",
		"-- @param id = integer\nSELECT id FROM p WHERE id = :id":                         "expected -- @param :name = schema",
		"-- @column id = integer\n-- @column id = string\nSELECT id FROM p":               "@column id is declared twice",
		"-- @header :id = X-Id\n-- @param :id = integer\nSELECT id FROM p WHERE id = :id": "the value comes from the X-Id header",
	} {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "schema.sql"), "CREATE TABLE p (id INTEGER);")
		writeFile(t, filepath.Join(dir, "q", "bad.sql"), sql)
		var out, errOut bytes.Buffer
		err := run(t.Context(), []string{"-root", filepath.Join(dir, "q"), "-driver", "sqlite", "-schema", filepath.Join(dir, "schema.sql")}, &out, &errOut)
		if err == nil || !strings.Contains(errOut.String(), want) {
			t.Errorf("%q: stderr %q, want %q", sql, errOut.String(), want)
		}
	}
}
