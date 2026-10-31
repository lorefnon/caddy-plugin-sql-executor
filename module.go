// Package sqlexecutor is a Caddy HTTP handler that exposes every .sql file
// in a directory or S3 bucket as a POST endpoint, or as an endpoint per HTTP
// method with verb_named_files. Each request runs the file's query against a configured PostgreSQL or SQLite database and
// returns the result as JSON, CSV, YAML, XLSX or HTML.
package sqlexecutor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver
	"go.uber.org/zap"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

func init() {
	caddy.RegisterModule(SQLExecutor{})
}

const (
	defaultMaxBodySize = 1 << 20
)

var errBodyTooLarge = errors.New("request body too large")

// SQLExecutor runs the SQL file matching the request path when a POST
// request arrives. A request for /reports/sales runs reports/sales.sql
// under the configured root. The response format is chosen from the path
// extension (/reports/sales.csv), then the Accept header, then
// default_format.
//
// Queries use :name placeholders, bound from the JSON or form-encoded
// request body and the query string (body wins). Files ending in
// .sql.j2 are Jinja templates rendered against the same parameters before
// execution; see jinja.go for how they are kept injection-safe.
type SQLExecutor struct {
	// Directory containing .sql files, or s3://bucket/prefix.
	Root string `json:"root,omitempty"`
	// Extra settings used when Root is an s3:// location.
	S3 *S3Config `json:"s3,omitempty"`
	// Database driver: "postgres" or "sqlite".
	Driver string `json:"driver,omitempty"`
	// Data source name, e.g. postgres://user:pass@host/db or
	// file:data.db?_pragma=busy_timeout(5000). Placeholders such as
	// {env.DATABASE_URL} are expanded.
	DSN string `json:"dsn,omitempty"`
	// Maximum query duration. Default: no limit beyond the request's own.
	Timeout caddy.Duration `json:"timeout,omitempty"`
	// Maximum request body size in bytes. Default: 1 MiB.
	MaxBodySize int64 `json:"max_body_size,omitempty"`
	// Formats clients may request: any of json, csv, yaml, xlsx and html.
	// Default: all of them.
	Formats []string `json:"formats,omitempty"`
	// Format used when neither extension nor Accept header picks one:
	// json (default), csv, yaml, xlsx or html. Must be one of Formats.
	DefaultFormat string `json:"default_format,omitempty"`
	// Connection pool limits; zero means database/sql defaults.
	MaxOpenConns int `json:"max_open_conns,omitempty"`
	MaxIdleConns int `json:"max_idle_conns,omitempty"`
	// Include database error messages in responses. Off by default,
	// since they can reveal schema details.
	VerboseErrors bool `json:"verbose_errors,omitempty"`
	// Let Jinja templates use include, import, from, extends, block and
	// macro. Templates are loaded from the query root by names relative to
	// it, ending in .j2.
	JinjaIncludes bool `json:"jinja_includes,omitempty"`
	// Route by HTTP method: DELETE /foo/bar runs foo/bar/DELETE.sql, and
	// every method is allowed. Off by default, in which case only POST is
	// handled and POST /foo/bar runs foo/bar.sql.
	VerbNamedFiles bool `json:"verb_named_files,omitempty"`

	db            *sql.DB
	style         placeholderStyle
	source        source
	templates     *templateCache
	defaultFormat format
	formats       formatSet
	logger        *zap.Logger
}

// CaddyModule returns the Caddy module information.
func (SQLExecutor) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.sql_executor",
		New: func() caddy.Module { return new(SQLExecutor) },
	}
}

// Provision opens the database and query source.
func (h *SQLExecutor) Provision(ctx caddy.Context) error {
	h.logger = ctx.Logger()
	repl := caddy.NewReplacer()

	if h.MaxBodySize <= 0 {
		h.MaxBodySize = defaultMaxBodySize
	}
	formats, first, err := parseFormats(h.Formats)
	if err != nil {
		return err
	}
	h.formats = formats
	h.defaultFormat = formatJSON
	if !formats.has(formatJSON) {
		h.defaultFormat = first
	}
	if h.DefaultFormat != "" {
		f, ok := parseFormat(h.DefaultFormat)
		if !ok {
			return fmt.Errorf("unknown default_format %q", h.DefaultFormat)
		}
		if !formats.has(f) {
			return fmt.Errorf("default_format %q is not among the enabled formats", h.DefaultFormat)
		}
		h.defaultFormat = f
	}

	driverName, style, err := parseDriver(h.Driver)
	if err != nil {
		return err
	}
	h.style = style
	dsn := repl.ReplaceKnown(h.DSN, "")
	if dsn == "" {
		return errors.New("dsn is required")
	}

	if err := h.provisionSource(ctx, repl); err != nil {
		return err
	}
	h.templates = newTemplateCache(nil)
	if h.JinjaIncludes {
		h.templates = newTemplateCache(h.source)
	}

	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	if h.MaxOpenConns > 0 {
		db.SetMaxOpenConns(h.MaxOpenConns)
	}
	if h.MaxIdleConns > 0 {
		db.SetMaxIdleConns(h.MaxIdleConns)
	}
	h.db = db

	// Connectivity problems are logged rather than fatal, so Caddy can
	// start while the database is temporarily unavailable.
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		h.logger.Warn("database is not reachable yet", zap.String("driver", h.Driver), zap.Error(err))
	}
	return nil
}

func (h *SQLExecutor) provisionSource(ctx caddy.Context, repl *caddy.Replacer) error {
	root := repl.ReplaceKnown(h.Root, "")
	if root == "" {
		return errors.New("root is required")
	}
	if !strings.HasPrefix(root, "s3://") {
		src, err := newDirSource(root)
		if err != nil {
			return fmt.Errorf("query root: %w", err)
		}
		h.source = src
		return nil
	}

	bucket, prefix, err := parseS3URL(root)
	if err != nil {
		return err
	}
	cfg := S3Config{}
	if h.S3 != nil {
		cfg = *h.S3
	}
	cfg.Region = repl.ReplaceKnown(cfg.Region, "")
	cfg.Endpoint = repl.ReplaceKnown(cfg.Endpoint, "")
	cfg.AccessKeyID = repl.ReplaceKnown(cfg.AccessKeyID, "")
	cfg.SecretAccessKey = repl.ReplaceKnown(cfg.SecretAccessKey, "")
	cfg.SessionToken = repl.ReplaceKnown(cfg.SessionToken, "")
	client, err := newS3Client(ctx, &cfg)
	if err != nil {
		return err
	}

	src := newS3Source(client, bucket, prefix, &cfg, h.logger)
	// Queries load lazily, so S3 being unreachable now is not fatal.
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := src.check(checkCtx); err != nil {
		h.logger.Warn("S3 bucket is not reachable yet", zap.String("bucket", bucket), zap.Error(err))
	}
	h.source = src
	return nil
}

// parseDriver maps a configured driver name to the database/sql driver
// that implements it and the placeholder style its queries use.
func parseDriver(driver string) (string, placeholderStyle, error) {
	switch strings.ToLower(driver) {
	case "postgres", "postgresql", "pgx":
		return "pgx", styleDollar, nil
	case "sqlite", "sqlite3":
		return "sqlite", styleQuestion, nil
	}
	return "", 0, fmt.Errorf("unsupported driver %q: use postgres or sqlite", driver)
}

// Validate checks the configuration.
func (h *SQLExecutor) Validate() error {
	if h.db == nil || h.source == nil {
		return errors.New("sql_executor is not provisioned")
	}
	return nil
}

// Cleanup closes the database pool.
func (h *SQLExecutor) Cleanup() error {
	if h.db != nil {
		return h.db.Close()
	}
	return nil
}

// ServeHTTP implements caddyhttp.MiddlewareHandler. Requests that do not
// match a query file are passed to the next handler.
func (h *SQLExecutor) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	route, f, ok := routeOf(r.URL.Path)
	if !ok {
		return next.ServeHTTP(w, r)
	}
	verb := ""
	if h.VerbNamedFiles {
		if verb = r.Method; !validVerb(verb) {
			return next.ServeHTTP(w, r)
		}
	}
	name, query, pathParams, err := h.find(r.Context(), route, verb)
	if errors.Is(err, fs.ErrNotExist) {
		return next.ServeHTTP(w, r)
	}
	if err != nil {
		return caddyhttp.Error(http.StatusInternalServerError, err)
	}

	if !h.VerbNamedFiles && r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		return writeError(w, http.StatusMethodNotAllowed, "only POST is allowed")
	}

	supported := "supported types: " + strings.Join(h.formats.mediaTypes(), ", ")
	if f != "" && !h.formats.has(f) {
		return writeError(w, http.StatusNotAcceptable, "format not enabled; "+supported)
	}
	if f == "" {
		negotiated, acceptable := negotiate(r.Header.Get("Accept"), h.formats)
		if !acceptable {
			return writeError(w, http.StatusNotAcceptable, supported)
		}
		f = negotiated
	}
	if f == "" {
		f = h.defaultFormat
	}

	params, err := readParams(r, h.MaxBodySize)
	if errors.Is(err, errBodyTooLarge) {
		return writeError(w, http.StatusRequestEntityTooLarge, err.Error())
	}
	if err != nil {
		return writeError(w, http.StatusBadRequest, err.Error())
	}
	// Values from the path can't be overridden by the client.
	for k, v := range pathParams {
		params[k] = v
	}

	stmt, args, err := h.prepare(r.Context(), name, query, params, r.Header)
	var badReq *badRequestError
	if errors.As(err, &badReq) {
		return writeError(w, http.StatusBadRequest, badReq.Error())
	}
	if err != nil {
		h.logger.Error("invalid query file", zap.String("file", name), zap.Error(err))
		return writeError(w, http.StatusInternalServerError, h.dbErrorMessage(err))
	}

	ctx := r.Context()
	if h.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(h.Timeout))
		defer cancel()
	}

	return h.execute(ctx, w, name, f, stmt.SQL, args)
}

// badRequestError marks failures caused by the request rather than the
// query file.
type badRequestError struct{ error }

// validVerb reports whether method can name a query file, such as GET for
// GET.sql.
func validVerb(method string) bool {
	if method == "" {
		return false
	}
	for _, c := range method {
		if (c < 'A' || c > 'Z') && c != '-' {
			return false
		}
	}
	return true
}

// find returns the file backing a route, and the values captured from its
// dynamic segments. Files named exactly like the route win over dynamic
// ones. With verb set, the route is a directory holding verb.sql.
// It returns an error wrapping fs.ErrNotExist if there is none.
func (h *SQLExecutor) find(ctx context.Context, route, verb string) (name, query string, params map[string]any, err error) {
	segs := strings.Split(route, "/")
	// A request that spells out a dynamic name, such as /users/[id], is a
	// request with "[id]" as the value, not one for the file of that name.
	if !slices.ContainsFunc(segs, func(seg string) bool { kind, _ := parseSegment(seg); return kind != StaticSegment }) {
		static := route + ".sql"
		if verb != "" {
			static = route + "/" + verb + ".sql"
		}
		name, query, err = h.lookup(ctx, static)
		if !errors.Is(err, fs.ErrNotExist) {
			return name, query, nil, err
		}
	}

	m, err := h.match(ctx, "", segs, verb)
	if err != nil {
		return "", "", nil, err
	}
	if m == nil {
		return "", "", nil, fs.ErrNotExist
	}
	name, query, err = h.lookup(ctx, m.File)
	return name, query, m.Params, err
}

// lookup finds the file backing a route, trying name.sql, then the
// template name.sql.j2. It returns the name of the file found.
func (h *SQLExecutor) lookup(ctx context.Context, name string) (string, string, error) {
	var err error
	for _, ext := range []string{"", jinjaExt} {
		var query string
		query, err = h.source.Get(ctx, name+ext)
		if !errors.Is(err, fs.ErrNotExist) {
			return name + ext, query, err
		}
	}
	return name, "", err
}

// prepare turns a query file and request parameters into SQL with driver
// placeholders plus its arguments, rendering templates (which may also
// read request headers) first.
func (h *SQLExecutor) prepare(ctx context.Context, name, query string, params map[string]any, header http.Header) (*statement, []any, error) {
	stmt, params, err := compileQuery(ctx, h.templates, h.style, name, query, params, header, nil)
	if err != nil {
		return nil, nil, err
	}
	if err := stmt.headerParams(params, header); err != nil {
		return nil, nil, &badRequestError{err}
	}
	args, err := stmt.bind(params)
	if err != nil {
		return nil, nil, &badRequestError{err}
	}
	return stmt, args, nil
}

// compileQuery renders a template query (other queries are used as they
// are) and compiles the result. It returns the statement and the request
// parameters merged with the values the template bound. onHeader, if not
// nil, is called with each header name the template reads.
func compileQuery(ctx context.Context, templates *templateCache, style placeholderStyle, name, query string, params map[string]any, header http.Header, onHeader func(string)) (*statement, map[string]any, error) {
	var bound map[string]any
	if isTemplate(name) {
		tpl, err := templates.get(ctx, name, query)
		if err != nil {
			return nil, nil, fmt.Errorf("parsing template: %w", err)
		}
		var inputErr *inputError
		query, bound, err = tpl.renderWith(ctx, params, header, onHeader)
		if errors.As(err, &inputErr) {
			return nil, nil, &badRequestError{inputErr}
		}
		if err != nil {
			return nil, nil, fmt.Errorf("rendering template: %w", err)
		}
		params = mergeBoundParams(params, bound)
	}

	stmt, err := compile(query, style)
	if err != nil {
		return nil, nil, err
	}
	if err := checkBoundPlaceholders(stmt, bound); err != nil {
		return nil, nil, err
	}
	return stmt, params, nil
}

func (h *SQLExecutor) execute(ctx context.Context, w http.ResponseWriter, name string, f format, query string, args []any) error {
	// Argument values are left out since they carry request data.
	h.logger.Debug("executing query",
		zap.String("file", name), zap.String("sql", query), zap.Int("args", len(args)))
	rows, err := h.db.QueryContext(ctx, query, args...)
	if err != nil {
		return h.queryFailed(w, name, err)
	}
	defer rows.Close()

	types, err := rows.ColumnTypes()
	if err != nil {
		return h.queryFailed(w, name, err)
	}
	cols := columnsFromTypes(types)
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}

	// Fetch the first row before committing to a 200, so errors that
	// surface on the first fetch still produce a proper error response.
	hasRow := rows.Next()
	if !hasRow {
		if err := rows.Err(); err != nil {
			return h.queryFailed(w, name, err)
		}
	} else if err := rows.Scan(ptrs...); err != nil {
		return h.queryFailed(w, name, err)
	} else {
		normalizeRow(cols, vals)
	}

	hdr := w.Header()
	hdr.Set("Content-Type", f.contentType())
	if f == formatHTML {
		// The page needs no scripts; forbid them as a second line of
		// defence against anything the escaping might miss.
		hdr.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
		hdr.Set("X-Content-Type-Options", "nosniff")
	}
	if f == formatXLSX {
		hdr.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": sanitizeSheetName(name) + ".xlsx"}))
	}
	w.WriteHeader(http.StatusOK)

	enc := newEncoder(f, w, name, query)
	err = func() error {
		if err := enc.Begin(cols); err != nil {
			return err
		}
		for hasRow {
			if err := enc.Row(vals); err != nil {
				return err
			}
			if hasRow = rows.Next(); hasRow {
				if err := rows.Scan(ptrs...); err != nil {
					return err
				}
				normalizeRow(cols, vals)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return enc.End()
	}()
	if err != nil {
		// The status line is already sent; abort the connection so the
		// client sees a broken response rather than a truncated but
		// valid-looking one.
		h.logger.Error("streaming query result failed", zap.String("file", name), zap.Error(err))
		panic(http.ErrAbortHandler)
	}
	return nil
}

func (h *SQLExecutor) queryFailed(w http.ResponseWriter, name string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return writeError(w, http.StatusGatewayTimeout, "query timed out")
	}
	if errors.Is(err, context.Canceled) {
		// Client went away; nothing useful to send.
		return nil
	}
	h.logger.Error("query failed", zap.String("file", name), zap.Error(err))
	return writeError(w, http.StatusInternalServerError, h.dbErrorMessage(err))
}

func (h *SQLExecutor) dbErrorMessage(err error) string {
	if h.VerboseErrors {
		return err.Error()
	}
	return "query failed"
}

func writeError(w http.ResponseWriter, status int, msg string) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// Interface guards
var (
	_ caddy.Provisioner           = (*SQLExecutor)(nil)
	_ caddy.Validator             = (*SQLExecutor)(nil)
	_ caddy.CleanerUpper          = (*SQLExecutor)(nil)
	_ caddyhttp.MiddlewareHandler = (*SQLExecutor)(nil)
)
