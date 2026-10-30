package main

import (
	"path/filepath"
	"testing"
)

const nullabilitySchema = `
CREATE TABLE p (id INTEGER PRIMARY KEY, name TEXT NOT NULL, category TEXT NOT NULL, price REAL NOT NULL, parent_id INTEGER, note TEXT);
CREATE TABLE c (id INTEGER PRIMARY KEY, label TEXT NOT NULL, p_id INTEGER NOT NULL);
INSERT INTO p VALUES (1, 'a', 'x', 1.5, NULL, NULL), (2, 'b', 'x', 2.5, 1, 'n'), (3, 'c', 'y', 3.0, 2, NULL);
INSERT INTO c VALUES (1, 'l1', 1), (2, 'l2', 1);
`

// nullabilityCase is a query, a request to run it with, and the schemas
// expected for some of its result columns and parameters.
type nullabilityCase struct {
	sql     string
	body    map[string]any
	columns map[string]string
	params  map[string]string
}

var sqliteCases = map[string]nullabilityCase{
	"left": {
		sql:     "SELECT p.name, c.label FROM p LEFT JOIN c ON c.p_id = p.id",
		columns: map[string]string{"name": `{"type":"string"}`, "label": `{"type":["string","null"]}`},
	},
	"reduced": {
		// The WHERE condition turns the outer join into an inner join.
		sql:     "SELECT p.name, c.label FROM p LEFT JOIN c ON c.p_id = p.id WHERE c.label = 'l1'",
		columns: map[string]string{"name": `{"type":"string"}`, "label": `{"type":"string"}`},
	},
	"self": {
		sql:     "SELECT a.name, b.name AS parent FROM p a LEFT JOIN p b ON b.id = a.parent_id",
		columns: map[string]string{"name": `{"type":"string"}`, "parent": `{"type":["string","null"]}`},
	},
	"right": {
		sql:     "SELECT p.name, c.label FROM c RIGHT JOIN p ON c.p_id = p.id",
		columns: map[string]string{"name": `{"type":"string"}`, "label": `{"type":["string","null"]}`},
	},
	"star": {
		sql: "SELECT * FROM p LEFT JOIN c ON c.p_id = p.id",
		columns: map[string]string{
			"name":  `{"type":"string"}`,
			"label": `{"type":["string","null"]}`,
			"id":    `{"type":["integer","null"],"format":"int64"}`, // p.id and c.id
		},
	},
	"aggregate": {
		sql: "SELECT count(*) AS n, coalesce(max(c.label), '-') AS m, sum(p.price) AS total, p.name FROM p LEFT JOIN c ON c.p_id = p.id",
		columns: map[string]string{
			"n":     `{"type":"integer"}`,
			"m":     `{"type":"string"}`,
			"total": `{"type":["number","null"]}`,
			// Without GROUP BY, an empty table still gives a row.
			"name": `{"type":["string","null"]}`,
		},
	},
	"grouped": {
		sql: "SELECT p.category, count(*) AS n, max(p.name) AS top, avg(p.price) AS avg, sum(p.id) AS ids, group_concat(p.name) AS names FROM p GROUP BY p.category",
		columns: map[string]string{
			"category": `{"type":"string"}`,
			"n":        `{"type":"integer"}`,
			"top":      `{"type":"string"}`,
			"avg":      `{"type":"number"}`,
			"ids":      `{"type":"integer"}`,
			"names":    `{"type":"string"}`,
		},
	},
	"expressions": {
		sql: `SELECT p.id + 1 AS next, p.price * 2 AS twice, p.price / 2 AS half, upper(p.name) AS up, p.name || '!' AS bang,
			CASE WHEN p.price > 2 THEN 'hi' ELSE 'lo' END AS band, CASE WHEN p.price > 2 THEN 'hi' END AS maybe,
			CAST(p.id AS TEXT) AS sid, p.note IS NULL AS no_note, date('now') AS today,
			coalesce(p.note, p.name) AS note_or_name, length(p.note) AS note_len, NULL AS none
			FROM p`,
		columns: map[string]string{
			"next":         `{"type":"integer"}`,
			"twice":        `{"type":"number"}`,
			"half":         `{"type":["number","null"]}`, // x / 0 is NULL
			"up":           `{"type":"string"}`,
			"bang":         `{"type":"string"}`,
			"band":         `{"type":"string"}`,
			"maybe":        `{"type":["string","null"]}`,
			"sid":          `{"type":"string"}`,
			"no_note":      `{"type":"integer"}`,
			"today":        `{"type":["string","null"],"format":"date"}`,
			"note_or_name": `{"type":"string"}`,
			"note_len":     `{"type":["integer","null"]}`,
			"none":         `{"type":"null"}`,
		},
	},
	"subquery": {
		sql:     "SELECT x.n FROM (SELECT name AS n FROM p) x",
		columns: map[string]string{"n": `{"type":"string"}`},
	},
	"union": {
		sql:     "SELECT name FROM p UNION ALL SELECT NULL",
		columns: map[string]string{"name": `{"type":["string","null"]}`},
	},
	"window": {
		sql:     "SELECT p.name, row_number() OVER (ORDER BY p.id) AS rn, lag(p.name) OVER (ORDER BY p.id) AS prev FROM p",
		columns: map[string]string{"rn": `{"type":"integer"}`, "prev": `{"type":["string","null"]}`},
	},
	"correlated": {
		sql:     "SELECT p.name, (SELECT count(*) FROM c WHERE c.p_id = p.id) AS kids, EXISTS (SELECT 1 FROM c WHERE c.p_id = p.id) AS has_kids FROM p",
		columns: map[string]string{"kids": `{"type":["integer","null"]}`, "has_kids": `{"type":"integer"}`},
	},
	"params": {
		sql: `SELECT p.id FROM p
			WHERE p.id >= :id AND p.name LIKE :pattern AND p.price BETWEEN :lo AND :hi
			  AND p.category IN (:c1, :c2) AND coalesce(:note, p.note, '') IS NOT NULL
			LIMIT :n`,
		body: map[string]any{"id": 1, "pattern": "%", "lo": 0, "hi": 10, "c1": "x", "c2": "y", "note": nil, "n": 5},
		params: map[string]string{
			"id":      `{"type":["integer","null"],"format":"int64"}`,
			"pattern": `{"type":["string","null"]}`,
			"lo":      `{"type":["number","null"],"format":"double"}`,
			"hi":      `{"type":["number","null"],"format":"double"}`,
			"c1":      `{"type":["string","null"]}`,
			"note":    `{"type":["string","null"]}`,
			"n":       `{"type":["integer","null"]}`,
		},
	},
	"insert": {
		sql:     "INSERT INTO p (name, category, price) VALUES (:name, :category, :price) RETURNING id, name, note",
		body:    map[string]any{"name": "z", "category": "x", "price": 1},
		params:  map[string]string{"name": `{"type":["string","null"]}`, "price": `{"type":["number","null"],"format":"double"}`},
		columns: map[string]string{"id": `{"type":"integer","format":"int64"}`, "name": `{"type":"string"}`, "note": `{"type":["string","null"]}`},
	},
	"update": {
		sql:     "UPDATE p SET note = :note WHERE id = :id RETURNING id, note",
		body:    map[string]any{"note": "hi", "id": 1},
		params:  map[string]string{"note": `{"type":["string","null"]}`, "id": `{"type":["integer","null"],"format":"int64"}`},
		columns: map[string]string{"id": `{"type":"integer","format":"int64"}`, "note": `{"type":["string","null"]}`},
	},
}

func TestSQLiteNullability(t *testing.T) {
	testNullability(t, "sqlite", sqliteFromSchemaText(t, nullabilitySchema), sqliteCases)
}

// testNullability generates a spec for the cases, checks the expected
// schemas, and runs each case against the handler to check that what it
// returns matches.
func testNullability(t *testing.T, driver, dsn string, cases map[string]nullabilityCase) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "queries")
	// Writes go last, so that the rows the other queries see are known.
	var requests, writes []request
	for name, c := range cases {
		writeFile(t, filepath.Join(root, name+".sql"), c.sql)
		if name == "insert" || name == "update" {
			writes = append(writes, request{route: name, body: c.body})
		} else {
			requests = append(requests, request{route: name, body: c.body})
		}
	}
	requests = append(requests, writes...)
	args := []string{"-root", root, "-driver", driver, "-dsn", dsn}
	spec := specFor(t, args...)
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			op := spec.Paths["/"+name]
			if op == nil {
				t.Fatal("not in the spec")
			}
			if len(c.columns) > 0 {
				assertProperties(t, spec.Components.Schemas[rowSchemaName(operationID(name))], c.columns)
			}
			if len(c.params) > 0 {
				assertProperties(t, op.Post.RequestBody.Content["application/json"].Schema, c.params)
			}
		})
	}
	roundTrip(t, root, driver, dsn, false, args, requests)
}

func sqliteFromSchemaText(t *testing.T, ddl string) string {
	t.Helper()
	schema := filepath.Join(t.TempDir(), "schema.sql")
	writeFile(t, schema, ddl)
	return sqliteFromSchema(t, schema)
}
