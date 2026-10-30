package main

import (
	"database/sql"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

// schemaDatabase creates a throwaway database from a file of DDL, so that
// statements can be described without access to the real database. It
// returns the database's DSN and a function that removes it. pgVersion
// picks the PostgreSQL major version, by default the latest.
func schemaDatabase(driver, schemaFile, pgVersion string) (dsn string, cleanup func(), err error) {
	ddl, err := os.ReadFile(schemaFile)
	if err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp("", "sql-openapi-")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { os.RemoveAll(dir) }
	defer func() {
		if err != nil {
			cleanup()
		}
	}()

	var driverName string
	switch strings.ToLower(driver) {
	case "sqlite", "sqlite3":
		driverName, dsn = "sqlite", "file:"+filepath.Join(dir, "schema.db")
	case "postgres", "postgresql", "pgx":
		// Runs a real PostgreSQL server, downloaded on first use and cached
		// in ~/.embedded-postgres-go.
		port, err := freePort()
		if err != nil {
			return "", nil, err
		}
		cfg := embeddedpostgres.DefaultConfig()
		if pgVersion != "" {
			v, ok := postgresVersions[pgVersion]
			if !ok {
				return "", nil, fmt.Errorf("unsupported postgres_version %q", pgVersion)
			}
			cfg = cfg.Version(v)
		}
		cfg = cfg.
			Port(port).
			RuntimePath(filepath.Join(dir, "runtime")).
			DataPath(filepath.Join(dir, "data")).
			Logger(io.Discard)
		pg := embeddedpostgres.NewDatabase(cfg)
		if err := pg.Start(); err != nil {
			return "", nil, fmt.Errorf("starting embedded postgres: %w", err)
		}
		removeDir := cleanup
		cleanup = func() {
			pg.Stop()
			removeDir()
		}
		driverName, dsn = "pgx", cfg.GetConnectionURL()+"?sslmode=disable"
	default:
		return "", nil, fmt.Errorf("unsupported driver %q: use postgres or sqlite", driver)
	}

	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return "", nil, err
	}
	defer db.Close()
	if _, err = db.Exec(string(ddl)); err != nil {
		return "", nil, fmt.Errorf("loading %s: %w", schemaFile, err)
	}
	return dsn, cleanup, nil
}

var postgresVersions = map[string]embeddedpostgres.PostgresVersion{
	"12": embeddedpostgres.V12, "13": embeddedpostgres.V13, "14": embeddedpostgres.V14, "15": embeddedpostgres.V15,
	"16": embeddedpostgres.V16, "17": embeddedpostgres.V17, "18": embeddedpostgres.V18,
}

func freePort() (uint32, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return uint32(l.Addr().(*net.TCPAddr).Port), nil
}
