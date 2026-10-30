package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const pgSchema = `
DROP TABLE IF EXISTS openapi_things;
DROP TYPE IF EXISTS openapi_mood;
DROP DOMAIN IF EXISTS openapi_email;
CREATE TYPE openapi_mood AS ENUM ('sad', 'ok', 'happy');
CREATE DOMAIN openapi_email AS text;
CREATE TABLE openapi_things (
	id serial PRIMARY KEY,
	small smallint NOT NULL,
	big bigint,
	f4 real,
	f8 double precision,
	amount numeric(10, 2),
	flag boolean,
	label varchar(20) NOT NULL,
	code char(3),
	day date,
	at timestamp,
	atz timestamptz,
	doc jsonb,
	meta json,
	data bytea,
	uid uuid,
	feeling openapi_mood,
	contact openapi_email,
	tags text[],
	t time,
	span interval
);
INSERT INTO openapi_things (small, big, f4, f8, amount, flag, label, code, day, at, atz, doc, meta, data, uid, feeling, contact, tags, t, span) VALUES
	(1, 2, 1.5, 2.5, 3.25, true, 'a', 'abc', '2024-01-02', '2024-01-02 03:04:05', '2024-01-02 03:04:05+02',
	 '{"k": [1, 2]}', '[1]', '\x00ff', 'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11', 'happy', 'a@example.com', '{x,y}', '10:30', '1 day'),
	(2, NULL, NULL, NULL, NULL, NULL, 'b', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL);
`

// postgresDSN returns a PostgreSQL database loaded with ddl: the one
// named by SQL_EXECUTOR_TEST_POSTGRES_DSN, or, with
// SQL_OPENAPI_TEST_EMBEDDED_POSTGRES=1, a throwaway embedded server of the
// given major version (default: the latest), which is downloaded on first
// use. The latter also tests -schema.
func postgresDSN(t *testing.T, ddl, version string) string {
	t.Helper()
	schema := filepath.Join(t.TempDir(), "schema.sql")
	writeFile(t, schema, ddl)
	if dsn := os.Getenv("SQL_EXECUTOR_TEST_POSTGRES_DSN"); dsn != "" {
		if version != "" {
			t.Skip("only runs against an embedded server")
		}
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(ddl); err != nil {
			t.Fatal(err)
		}
		return dsn
	}
	if os.Getenv("SQL_OPENAPI_TEST_EMBEDDED_POSTGRES") == "" {
		t.Skip("set SQL_EXECUTOR_TEST_POSTGRES_DSN, or SQL_OPENAPI_TEST_EMBEDDED_POSTGRES=1")
	}
	dsn, cleanup, err := schemaDatabase("postgres", schema, version)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	return dsn
}

func TestPostgres(t *testing.T) {
	dsn := postgresDSN(t, pgSchema, "")
	dir := t.TempDir()
	root := filepath.Join(dir, "queries")
	writeFile(t, filepath.Join(root, "all.sql"), "SELECT * FROM openapi_things ORDER BY id")
	writeFile(t, filepath.Join(root, "filter.sql"), `
SELECT id, amount * 2 AS doubled, count(*) OVER () AS total
FROM openapi_things
WHERE (:min::int IS NULL OR small >= :min)
  AND (:mood::openapi_mood IS NULL OR feeling = :mood)
  AND (:since::date IS NULL OR day >= :since)
ORDER BY id`)
	writeFile(t, filepath.Join(root, "labels.sql.j2"), `
SELECT id, label FROM openapi_things
{% if labels %}WHERE label IN ({% for l in labels %}{{ l }}{% if not loop.last %}, {% endif %}{% endfor %}){% endif %}
ORDER BY id`)
	config := filepath.Join(dir, "config.yaml")
	writeFile(t, config, `
root: queries
driver: postgres
queries:
  labels.sql.j2:
    samples:
      - { labels: [a, b] }
`)
	args := []string{"-config", config, "-dsn", dsn}
	roundTrip(t, root, "postgres", dsn, false, args, []request{
		{route: "all"},
		{route: "filter", body: map[string]any{"min": nil, "mood": nil, "since": nil}},
		{route: "filter", body: map[string]any{"min": 1, "mood": "happy", "since": "2024-01-01"}},
		{route: "labels", body: map[string]any{"labels": []any{"a", "b"}}},
	})

	spec := specFor(t, args...)
	assertProperties(t, spec.Components.Schemas["AllRow"], map[string]string{
		"id":      `{"type":"integer","format":"int32"}`,
		"small":   `{"type":"integer","format":"int32"}`,
		"big":     `{"type":["integer","null"],"format":"int64"}`,
		"f4":      `{"type":["number","null"],"format":"double"}`,
		"amount":  `{"type":["string","null"],"format":"decimal"}`,
		"flag":    `{"type":["boolean","null"]}`,
		"label":   `{"type":"string"}`,
		"code":    `{"type":["string","null"]}`,
		"day":     `{"type":["string","null"],"format":"date"}`,
		"at":      `{"type":["string","null"],"format":"date-time"}`,
		"atz":     `{"type":["string","null"],"format":"date-time"}`,
		"doc":     `{}`,
		"meta":    `{}`,
		"data":    `{"type":["string","null"],"contentEncoding":"base64"}`,
		"uid":     `{"type":["string","null"],"format":"uuid"}`,
		"feeling": `{"type":["string","null"],"enum":["sad","ok","happy",null]}`,
		"contact": `{"type":["string","null"]}`,
		"tags":    `{"type":["string","null"]}`,
		"t":       `{"type":["string","null"]}`,
		"span":    `{"type":["string","null"]}`,
	})
	assertProperties(t, spec.Components.Schemas["FilterRow"], map[string]string{
		"doubled": `{"type":["string","null"],"format":"decimal"}`,
		"total":   `{"type":"integer","format":"int64"}`,
	})
	assertProperties(t, spec.Paths["/filter"].Post.RequestBody.Content["application/json"].Schema, map[string]string{
		"min":   `{"type":["integer","null"]}`,
		"mood":  `{"type":["string","null"],"enum":["sad","ok","happy",null]}`,
		"since": `{"type":["string","null"],"format":"date"}`,
	})
}

func assertProperties(t *testing.T, s *Schema, want map[string]string) {
	t.Helper()
	if s == nil {
		t.Fatal("no schema")
	}
	for name, w := range want {
		p, ok := s.Properties.get(name)
		if !ok {
			t.Errorf("no property %s", name)
			continue
		}
		if got, _ := json.Marshal(orEmpty(p)); string(got) != w {
			t.Errorf("%s: got %s, want %s", name, got, w)
		}
	}
}

const pgNullabilitySchema = `
DROP VIEW IF EXISTS openapi_v;
DROP TABLE IF EXISTS openapi_c, openapi_p, openapi_pt;
CREATE TABLE openapi_p (id int PRIMARY KEY, name text NOT NULL, category text NOT NULL, price numeric(10, 2) NOT NULL, parent_id int, note text);
CREATE TABLE openapi_c (id int PRIMARY KEY, label text NOT NULL, p_id int NOT NULL);
CREATE TABLE openapi_pt (id int NOT NULL, region text NOT NULL) PARTITION BY LIST (region);
CREATE TABLE openapi_pt_a PARTITION OF openapi_pt FOR VALUES IN ('a');
CREATE TABLE openapi_pt_b PARTITION OF openapi_pt FOR VALUES IN ('b');
CREATE VIEW openapi_v AS SELECT p.name, c.label FROM openapi_p p LEFT JOIN openapi_c c ON c.p_id = p.id;
INSERT INTO openapi_p VALUES (1, 'a', 'x', 1.5, NULL, NULL), (2, 'b', 'x', 2.5, 1, 'n'), (3, 'c', 'y', 3, 2, NULL);
INSERT INTO openapi_c VALUES (1, 'l1', 1), (2, 'l2', 1);
INSERT INTO openapi_pt VALUES (1, 'a'), (2, 'b');
`

const (
	pgText     = `{"type":"string"}`
	pgTextNull = `{"type":["string","null"]}`
	pgInt      = `{"type":"integer","format":"int32"}`
	pgIntNull  = `{"type":["integer","null"],"format":"int32"}`
	pgBigint   = `{"type":"integer","format":"int64"}`
)

var pgCases = map[string]nullabilityCase{
	"left": {
		sql:     "SELECT p.name, c.label FROM openapi_p p LEFT JOIN openapi_c c ON c.p_id = p.id",
		columns: map[string]string{"name": pgText, "label": pgTextNull},
	},
	"reduced": {
		sql:     "SELECT p.name, c.label FROM openapi_p p LEFT JOIN openapi_c c ON c.p_id = p.id WHERE c.label = 'l1'",
		columns: map[string]string{"name": pgText, "label": pgText},
	},
	"self": {
		sql:     "SELECT a.name, b.name AS parent FROM openapi_p a LEFT JOIN openapi_p b ON b.id = a.parent_id",
		columns: map[string]string{"name": pgText, "parent": pgTextNull},
	},
	"right": {
		sql:     "SELECT p.name, c.label FROM openapi_c c RIGHT JOIN openapi_p p ON c.p_id = p.id",
		columns: map[string]string{"name": pgText, "label": pgTextNull},
	},
	"full": {
		sql:     "SELECT p.name, c.label FROM openapi_p p FULL JOIN openapi_c c ON c.p_id = p.id",
		columns: map[string]string{"name": pgTextNull, "label": pgTextNull},
	},
	"star": {
		sql:     "SELECT * FROM openapi_p p LEFT JOIN openapi_c c ON c.p_id = p.id",
		columns: map[string]string{"name": pgText, "label": pgTextNull, "id": pgIntNull},
	},
	"view": {
		sql:     "SELECT * FROM openapi_v",
		columns: map[string]string{"name": pgText, "label": pgTextNull},
	},
	"lateral": {
		sql:     "SELECT p.name, l.label FROM openapi_p p LEFT JOIN LATERAL (SELECT label FROM openapi_c c WHERE c.p_id = p.id LIMIT 1) l ON true",
		columns: map[string]string{"name": pgText, "label": pgTextNull},
	},
	"aggregate": {
		sql: "SELECT count(*) AS n, coalesce(max(c.label), '-') AS m, sum(p.price) AS total FROM openapi_p p LEFT JOIN openapi_c c ON c.p_id = p.id",
		columns: map[string]string{
			"n":     pgBigint,
			"m":     pgText,
			"total": `{"type":["string","null"],"format":"decimal"}`,
		},
	},
	"grouped": {
		sql: "SELECT category, count(*) AS n, max(name) AS top, sum(id) AS ids, array_agg(note) AS notes FROM openapi_p GROUP BY category ORDER BY 2",
		columns: map[string]string{
			"category": pgText,
			"n":        pgBigint,
			"top":      pgText,
			"ids":      pgBigint,
			"notes":    pgText,
		},
	},
	"rollup": {
		sql:     "SELECT category, count(*) AS n FROM openapi_p GROUP BY ROLLUP (category)",
		columns: map[string]string{"category": pgTextNull, "n": pgBigint},
	},
	"expressions": {
		sql: `SELECT p.id + 1 AS next, upper(p.name) AS up, p.name || '!' AS bang,
			CASE WHEN p.price > 2 THEN 'hi' ELSE 'lo' END AS band, CASE WHEN p.price > 2 THEN 'hi' END AS maybe,
			p.note IS NULL AS no_note, coalesce(p.note, p.name) AS note_or_name, length(p.note) AS note_len,
			now() AS at, greatest(p.parent_id, p.id) AS g, nullif(p.name, 'a') AS nn
			FROM openapi_p p`,
		columns: map[string]string{
			"next":         pgInt,
			"up":           pgText,
			"bang":         pgText,
			"band":         pgText,
			"maybe":        pgTextNull,
			"no_note":      `{"type":"boolean"}`,
			"note_or_name": pgText,
			"note_len":     pgIntNull,
			"at":           `{"type":"string","format":"date-time"}`,
			"g":            pgInt,
			"nn":           pgTextNull,
		},
	},
	"union": {
		sql:     "SELECT name FROM openapi_p UNION ALL SELECT NULL::text",
		columns: map[string]string{"name": pgTextNull},
	},
	"union_distinct": {
		sql:     "SELECT name FROM openapi_p UNION SELECT label FROM openapi_c",
		columns: map[string]string{"name": pgTextNull},
	},
	"partitioned": {
		sql:     "SELECT id, region FROM openapi_pt",
		columns: map[string]string{"id": pgInt, "region": pgText},
	},
	"cte": {
		sql:     "WITH x AS MATERIALIZED (SELECT p.name, c.label FROM openapi_p p LEFT JOIN openapi_c c ON c.p_id = p.id) SELECT name, label FROM x",
		columns: map[string]string{"label": pgTextNull},
	},
	"window": {
		sql:     "SELECT name, row_number() OVER (ORDER BY id) AS rn, lag(name) OVER (ORDER BY id) AS prev FROM openapi_p",
		columns: map[string]string{"name": pgText, "rn": pgBigint, "prev": pgTextNull},
	},
	"correlated": {
		sql:     "SELECT p.name, (SELECT count(*) FROM openapi_c c WHERE c.p_id = p.id) AS kids FROM openapi_p p",
		columns: map[string]string{"name": pgText, "kids": `{"type":["integer","null"],"format":"int64"}`},
	},
	"params": {
		sql:  "SELECT p.id FROM openapi_p p WHERE p.id >= :id AND p.name LIKE :pattern AND p.category IN (:c1, :c2) LIMIT :n",
		body: map[string]any{"id": 1, "pattern": "%", "c1": "x", "c2": "y", "n": 5},
		params: map[string]string{
			"id":      `{"type":["integer","null"]}`,
			"pattern": pgTextNull,
			"c1":      pgTextNull,
			"n":       `{"type":["integer","null"]}`,
		},
	},
	"insert": {
		sql:     "INSERT INTO openapi_p (id, name, category, price) VALUES (:id, :name, :category, :price) RETURNING id, name, note, upper(name) AS up",
		body:    map[string]any{"id": 10, "name": "z", "category": "x", "price": 1},
		columns: map[string]string{"id": pgInt, "name": pgText, "note": pgTextNull, "up": pgText},
	},
	"update": {
		sql:     "UPDATE openapi_p SET note = :note WHERE id = :id RETURNING id, note",
		body:    map[string]any{"note": "hi", "id": 1},
		columns: map[string]string{"id": pgInt, "note": pgTextNull},
	},
}

func TestPostgresNullability(t *testing.T) {
	for _, version := range []string{"", "15"} {
		name := "latest"
		if version != "" {
			name = version
		}
		t.Run(name, func(t *testing.T) {
			testNullability(t, "postgres", postgresDSN(t, pgNullabilitySchema, version), pgCases)
		})
	}
}
