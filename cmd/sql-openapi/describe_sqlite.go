package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"modernc.org/sqlite"
)

// sqliteDescriber prepares statements without stepping them. SQLite knows
// the declared types of columns read directly from tables, but not the
// types of expressions or parameters, nor whether columns can be null;
// those are worked out by parsing the statement (see sqlite_analyze.go).
type sqliteDescriber struct {
	db *sql.DB
}

func (d *sqliteDescriber) Close() error { return d.db.Close() }

func (d *sqliteDescriber) Describe(ctx context.Context, query string, nparams int) (*description, error) {
	conn, err := d.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var info []sqlite.ColumnInfo
	err = conn.Raw(func(dc any) error {
		ci, ok := dc.(interface {
			ColumnInfo(query string) ([]sqlite.ColumnInfo, error)
		})
		if !ok {
			return errors.New("the sqlite driver does not support ColumnInfo")
		}
		info, err = ci.ColumnInfo(query)
		return err
	})
	if err != nil {
		return nil, err
	}

	desc := &description{}
	outerJoins, planned := sqliteOuterJoins(ctx, conn, query, nparams)
	// Without a plan, any outer join in the SQL is taken at its word.
	a := &sqliteAnalyzer{ctx: ctx, conn: conn, tables: map[string][]sqliteColumn{}, outerJoins: outerJoins || !planned}
	analysis, err := a.analyze(query, nparams)
	if err != nil {
		return nil, err
	}
	addsNulls := outerJoins || !planned
	var resolved []sqliteValue
	if analysis != nil {
		desc.Params = analysis.params
		addsNulls = addsNulls || analysis.mayAddNulls
		if len(analysis.columns) == len(info) {
			resolved = analysis.columns
		}
	}

	for i, c := range info {
		col := field{Name: c.Name, Schema: sqliteColumnSchema(c.DeclType), Nullable: true}
		switch {
		case resolved != nil && resolved[i].known:
			if col.Schema == nil {
				col.Schema = resolved[i].schema
			}
			col.Nullable = !resolved[i].notNull
		case c.TableName != "" && !addsNulls:
			// Read from a table: non-null if the table column is.
			notNull, err := a.originNotNull(c)
			if err != nil {
				return nil, err
			}
			col.Nullable = !notNull
		}
		desc.Columns = append(desc.Columns, col)
	}
	return desc, nil
}

// sqliteOuterJoins reports whether the query plan has outer joins. SQLite
// marks them LEFT-JOIN and RIGHT-JOIN in EXPLAIN QUERY PLAN; outer joins
// it turns into inner joins (as WHERE conditions on the nullable side
// allow) are not marked. planned is false if there's no plan.
func sqliteOuterJoins(ctx context.Context, conn *sql.Conn, query string, nparams int) (outer, planned bool) {
	rows, err := conn.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, make([]any, nparams)...)
	if err != nil {
		return false, false
	}
	defer rows.Close()
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			return false, false
		}
		if strings.Contains(detail, "LEFT-JOIN") || strings.Contains(detail, "RIGHT-JOIN") {
			outer = true
		}
	}
	return outer, rows.Err() == nil
}

// originNotNull reports whether the table column a result column is read
// from is declared NOT NULL, or is its table's INTEGER PRIMARY KEY.
func (a *sqliteAnalyzer) originNotNull(c sqlite.ColumnInfo) (bool, error) {
	var notNull bool
	err := a.conn.QueryRowContext(a.ctx, `
		SELECT "notnull" OR (pk = 1 AND upper(type) = 'INTEGER'
			AND (SELECT count(*) FROM pragma_table_info(?1, ?2) WHERE pk > 0) = 1)
		FROM pragma_table_info(?1, ?2) WHERE name = ?3`,
		c.TableName, c.DatabaseName, c.OriginName).Scan(&notNull)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return notNull, err
}
