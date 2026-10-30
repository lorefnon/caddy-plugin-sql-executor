package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// field is a result column.
type field struct {
	Name     string
	Schema   *Schema // nil when the type is unknown
	Nullable bool
}

// description is what the database knows about a statement, found without
// running it.
type description struct {
	// Params has the schema of each placeholder, in order. It is nil, or
	// has nil entries, where the type isn't known.
	Params  []*Schema
	Columns []field
}

type describer interface {
	// Describe describes query, which has nparams placeholders.
	Describe(ctx context.Context, query string, nparams int) (*description, error)
	Close() error
}

func openDescriber(ctx context.Context, driver, dsn string) (describer, error) {
	switch strings.ToLower(driver) {
	case "postgres", "postgresql", "pgx":
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			return nil, err
		}
		return newPGDescriber(conn), nil
	case "sqlite", "sqlite3":
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			return nil, err
		}
		if err := db.PingContext(ctx); err != nil {
			db.Close()
			return nil, err
		}
		return &sqliteDescriber{db: db}, nil
	}
	return nil, fmt.Errorf("unsupported driver %q: use postgres or sqlite", driver)
}
