// Command sql-openapi writes an OpenAPI 3.1 description of the routes the
// sql_executor handler serves from a query directory.
//
// Request and response types are inferred by asking a database to describe
// each statement, without running it: either a live database (-dsn) or a
// throwaway one created from a file of DDL (-schema). Jinja templates are
// rendered with the sample parameters given in the config file first.
//
//	sql-openapi -config sql-openapi.yaml -out openapi.yaml
//	sql-openapi -root queries -driver postgres -dsn "$DATABASE_URL"
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	sqlexecutor "github.com/lorefnon/caddy-plugin-sql-executor"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	if err != nil {
		if !errors.Is(err, errReported) {
			fmt.Fprintln(os.Stderr, "sql-openapi:", err)
		}
		os.Exit(1)
	}
}

// errReported means the errors were already written to stderr.
var errReported = errors.New("errors reported")

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fl := flag.NewFlagSet("sql-openapi", flag.ContinueOnError)
	fl.SetOutput(stderr)
	configPath := fl.String("config", "", "configuration `file` (YAML)")
	root := fl.String("root", "", "query `directory`")
	driver := fl.String("driver", "", "database driver: postgres or sqlite")
	dsn := fl.String("dsn", "", "describe statements against this database")
	schema := fl.String("schema", "", "describe statements against a throwaway database created from this DDL `file`")
	jinjaIncludes := fl.Bool("jinja-includes", false, "allow include/import/extends/macro in templates, as the handler's jinja_includes")
	verbNamedFiles := fl.Bool("verb-named-files", false, "route by HTTP method, as the handler's verb_named_files: DELETE /a/b is a/b/DELETE.sql")
	out := fl.String("out", "", "write the spec to this `file` (.json or .yaml); default: YAML on stdout")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if fl.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fl.Args(), " "))
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	fl.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "root":
			cfg.Root = *root
		case "driver":
			cfg.Driver = *driver
		case "dsn":
			cfg.DSN = *dsn
		case "schema":
			cfg.Schema = *schema
		case "jinja-includes":
			cfg.JinjaIncludes = *jinjaIncludes
		case "verb-named-files":
			cfg.VerbNamedFiles = *verbNamedFiles
		}
	})
	cfg.DSN = os.ExpandEnv(cfg.DSN)
	switch {
	case cfg.Root == "":
		return errors.New("no query root: set root in the config or pass -root")
	case cfg.Driver == "":
		return errors.New("no driver: set driver in the config or pass -driver")
	case cfg.DSN == "" && cfg.Schema == "":
		return errors.New("no database: set dsn or schema in the config, or pass -dsn or -schema")
	}

	compiler, err := sqlexecutor.NewCompiler(cfg.Root, cfg.Driver, cfg.JinjaIncludes)
	if err != nil {
		return err
	}
	dbDSN := cfg.DSN
	if dbDSN == "" {
		var cleanup func()
		if dbDSN, cleanup, err = schemaDatabase(cfg.Driver, cfg.Schema, cfg.PostgresVersion); err != nil {
			return err
		}
		defer cleanup()
	}
	desc, err := openDescriber(ctx, cfg.Driver, dbDSN)
	if err != nil {
		return fmt.Errorf("connecting to the database: %w", err)
	}
	defer desc.Close()

	g := &generator{cfg: cfg, compiler: compiler, desc: desc}
	doc, fileErrs, err := g.generate(ctx)
	if err != nil {
		return err
	}
	b, err := encode(doc, *out)
	if err != nil {
		return err
	}
	if *out == "" {
		_, err = stdout.Write(b)
	} else {
		err = os.WriteFile(*out, b, 0o644)
	}
	if err != nil {
		return err
	}
	for _, e := range fileErrs {
		fmt.Fprintln(stderr, "sql-openapi:", e)
	}
	if len(fileErrs) > 0 {
		return errReported
	}
	return nil
}

func encode(doc *document, out string) ([]byte, error) {
	if strings.EqualFold(filepath.Ext(out), ".json") {
		b, err := json.MarshalIndent(doc, "", "  ")
		return append(b, '\n'), err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	err := enc.Close()
	return buf.Bytes(), err
}
