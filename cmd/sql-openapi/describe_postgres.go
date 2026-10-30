package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// pgDescriber prepares statements as unnamed prepared statements, which
// PostgreSQL describes without executing them, and reads their generic
// query plans to find out which columns can be null.
type pgDescriber struct {
	conn  *pgx.Conn
	major int // server major version
	types map[uint32]*pgType
	// NOT NULL flags of table columns, by table OID and attribute number,
	// and by schema, table and column name.
	notNullByOID  map[[2]uint32]bool
	notNullByName map[[3]string]bool
}

func newPGDescriber(conn *pgx.Conn) *pgDescriber {
	var major int
	if v := strings.FieldsFunc(conn.PgConn().ParameterStatus("server_version"), func(r rune) bool {
		return r < '0' || r > '9'
	}); len(v) > 0 {
		major, _ = strconv.Atoi(v[0])
	}
	return &pgDescriber{
		conn:          conn,
		major:         major,
		types:         map[uint32]*pgType{},
		notNullByOID:  map[[2]uint32]bool{},
		notNullByName: map[[3]string]bool{},
	}
}

func (d *pgDescriber) Close() error { return d.conn.Close(context.Background()) }

func (d *pgDescriber) Describe(ctx context.Context, query string, _ int) (*description, error) {
	sd, err := d.conn.PgConn().Prepare(ctx, "", query, nil)
	if err != nil {
		return nil, err
	}
	desc := &description{Params: []*Schema{}}
	for _, oid := range sd.ParamOIDs {
		t, err := d.lookup(ctx, oid)
		if err != nil {
			return nil, err
		}
		desc.Params = append(desc.Params, pgParamSchema(t))
	}
	var plan *pgPlan
	if len(sd.Fields) > 0 {
		plan = d.plan(ctx, query, len(sd.ParamOIDs))
	}
	for i, f := range sd.Fields {
		t, err := d.lookup(ctx, f.DataTypeOID)
		if err != nil {
			return nil, err
		}
		notNull, err := d.columnNotNull(ctx, query, plan, i, f)
		if err != nil {
			return nil, err
		}
		desc.Columns = append(desc.Columns, field{Name: f.Name, Schema: pgColumnSchema(t), Nullable: !notNull})
	}
	return desc, nil
}

// mayAddNulls matches SQL that can produce nulls from NOT NULL columns,
// for when no query plan is available.
var mayAddNulls = regexp.MustCompile(`(?i)\b(left|right|full)\s+(outer\s+)?join\b|\b(rollup|cube|grouping\s+sets)\b`)

// columnNotNull reports whether result column i can never be null. It
// evaluates the column's expression in the query plan; failing that, a
// column read straight from a NOT NULL table column is non-null, unless
// the query has outer joins or grouping sets.
func (d *pgDescriber) columnNotNull(ctx context.Context, query string, plan *pgPlan, i int, f pgconn.FieldDescription) (bool, error) {
	if plan != nil {
		notNull, known, err := plan.notNull(i, func(schema, table, column string) (bool, error) {
			return d.notNullByNames(ctx, schema, table, column)
		})
		if err != nil || known {
			return notNull, err
		}
		if plan.outerJoin || plan.groupingSets {
			return false, nil
		}
	} else if mayAddNulls.MatchString(query) {
		return false, nil
	}
	return d.tableColumnNotNull(ctx, f)
}

// plan returns the statement's generic plan, or nil if it isn't available.
// EXPLAIN without ANALYZE does not run the statement.
func (d *pgDescriber) plan(ctx context.Context, query string, nparams int) *pgPlan {
	var (
		out []byte
		err error
	)
	switch {
	case d.major >= 16:
		out, err = d.explain(ctx, "EXPLAIN (GENERIC_PLAN, VERBOSE, COSTS OFF, FORMAT JSON) "+query)
	case d.major >= 12:
		// Before GENERIC_PLAN, a prepared statement can be forced to use a
		// generic plan and explained with any arguments.
		pc := d.conn.PgConn()
		if _, err = pc.Exec(ctx, "SET plan_cache_mode = force_generic_plan; PREPARE sqlopenapi_explain AS "+query).ReadAll(); err != nil {
			return nil
		}
		args := strings.TrimSuffix(strings.Repeat("NULL, ", nparams), ", ")
		if nparams > 0 {
			args = "(" + args + ")"
		}
		out, err = d.explain(ctx, "EXPLAIN (VERBOSE, COSTS OFF, FORMAT JSON) EXECUTE sqlopenapi_explain"+args)
		pc.Exec(ctx, "DEALLOCATE sqlopenapi_explain; RESET plan_cache_mode").ReadAll()
	default:
		return nil
	}
	if err != nil {
		return nil
	}
	plan, err := parsePGPlan(out)
	if err != nil {
		return nil
	}
	return plan
}

func (d *pgDescriber) explain(ctx context.Context, sql string) ([]byte, error) {
	res, err := d.conn.PgConn().Exec(ctx, sql).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(res) != 1 || len(res[0].Rows) != 1 || len(res[0].Rows[0]) != 1 {
		return nil, errors.New("unexpected EXPLAIN output")
	}
	return res[0].Rows[0][0], nil
}

// lookup reads a type from pg_type, resolving domains to their base type.
func (d *pgDescriber) lookup(ctx context.Context, oid uint32) (*pgType, error) {
	if t, ok := d.types[oid]; ok {
		return t, nil
	}
	t := &pgType{}
	err := d.conn.QueryRow(ctx, `
		SELECT t.typname::text, t.typtype::text, t.typcategory::text, t.typbasetype,
		       array(SELECT e.enumlabel::text FROM pg_enum e WHERE e.enumtypid = t.oid ORDER BY e.enumsortorder)
		FROM pg_type t WHERE t.oid = $1`, oid).Scan(&t.Name, &t.Kind, &t.Category, &t.Base, &t.Labels)
	if errors.Is(err, pgx.ErrNoRows) {
		// Not a catalogued type (e.g. unknown, for an untyped literal).
		t = &pgType{Name: "unknown"}
	} else if err != nil {
		return nil, fmt.Errorf("looking up type %d: %w", oid, err)
	}
	if t.Kind == "d" && t.Base != 0 {
		if t, err = d.lookup(ctx, t.Base); err != nil {
			return nil, err
		}
	}
	d.types[oid] = t
	return t, nil
}

// tableColumnNotNull reports whether a result column comes straight from a
// table column declared NOT NULL.
func (d *pgDescriber) tableColumnNotNull(ctx context.Context, f pgconn.FieldDescription) (bool, error) {
	if f.TableOID == 0 || f.TableAttributeNumber == 0 {
		return false, nil
	}
	key := [2]uint32{f.TableOID, uint32(f.TableAttributeNumber)}
	if v, ok := d.notNullByOID[key]; ok {
		return v, nil
	}
	var v bool
	err := d.conn.QueryRow(ctx, `SELECT attnotnull FROM pg_attribute WHERE attrelid = $1 AND attnum = $2`,
		f.TableOID, f.TableAttributeNumber).Scan(&v)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	d.notNullByOID[key] = v
	return v, nil
}

// notNullByNames reports whether a table column is declared NOT NULL.
func (d *pgDescriber) notNullByNames(ctx context.Context, schema, table, column string) (bool, error) {
	key := [3]string{schema, table, column}
	if v, ok := d.notNullByName[key]; ok {
		return v, nil
	}
	var v bool
	err := d.conn.QueryRow(ctx, `
		SELECT a.attnotnull
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2 AND a.attname = $3 AND NOT a.attisdropped`,
		schema, table, column).Scan(&v)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	d.notNullByName[key] = v
	return v, nil
}
