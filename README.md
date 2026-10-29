# caddy-plugin-sql-executor

A [Caddy](https://caddyserver.com) HTTP handler that turns a folder of `.sql` files into an API.

Point it at a local directory or an S3 bucket. Each `.sql` file becomes a route. A `POST` to that route runs the query against PostgreSQL or SQLite and returns the rows as **JSON**, **CSV**, **YAML** or **XLSX**.

```
queries/
├── products.sql            → POST /products
└── reports/
    └── monthly_sales.sql   → POST /reports/monthly_sales
                              POST /reports/monthly_sales.csv
                              POST /reports/monthly_sales.xlsx
```

## Building

With [xcaddy](https://github.com/caddyserver/xcaddy):

```sh
xcaddy build --with github.com/lorefnon/caddy-plugin-sql-executor
```

Or from this repository, which includes a Caddy entrypoint with the standard modules:

```sh
go build -o caddy ./cmd/caddy
```

Requires Go 1.26 or newer. Both database drivers are pure Go ([pgx](https://github.com/jackc/pgx) and [modernc.org/sqlite](https://gitlab.com/cznic/sqlite)), so no cgo is needed.

## Quick start

```sh
sqlite3 examples/app.db < examples/seed.sql
go run ./cmd/caddy run --config examples/Caddyfile

curl -X POST localhost:8080/api/products -d '{"category": "coffee"}'
curl -X POST localhost:8080/api/products.csv -d '{"category": null}'
curl -X POST localhost:8080/api/category_summary -H 'Accept: application/yaml'
curl -X POST localhost:8080/api/products.xlsx -d '{"category": null}' -o products.xlsx
```

## Caddyfile

```caddyfile
example.com {
	handle_path /api/* {
		sql_executor {
			root           ./queries          # or s3://bucket/prefix
			driver         postgres           # postgres | sqlite
			dsn            {env.DATABASE_URL}
			timeout        30s
			max_body_size  1MiB
			default_format json               # json | csv | yaml | xlsx
			max_open_conns 10
			max_idle_conns 5
			# verbose_errors                  # include DB error text in responses
		}
	}
}
```

| Option | Default | Description |
| --- | --- | --- |
| `root` | (required) | A directory of `.sql` files, or `s3://bucket/optional/prefix`. |
| `driver` | (required) | `postgres` or `sqlite`. |
| `dsn` | (required) | Connection string, for example `postgres://user:pass@host:5432/db?sslmode=require` or `file:app.db?_pragma=busy_timeout(5000)`. |
| `timeout` | none | Maximum duration of a query. A query that runs longer is cancelled and the request gets `504`. |
| `max_body_size` | `1MiB` | Largest request body accepted. A larger body gets `413`. |
| `default_format` | `json` | Format used when neither the extension nor `Accept` picks one. |
| `max_open_conns`, `max_idle_conns` | `database/sql` defaults | Connection pool limits. |
| `verbose_errors` | off | Return database error messages to clients. When off, clients get `{"error":"query failed"}` and the details go to Caddy's log. |

`root`, `dsn` and the S3 credentials expand Caddy placeholders such as `{env.DATABASE_URL}` and `{file./run/secrets/db_dsn}`.

Requests that don't match a query file go to the next handler. So you can put `sql_executor` next to `file_server`, `reverse_proxy` and so on. The directive runs before `file_server` in Caddy's default order.

### Loading queries from S3

```caddyfile
sql_executor {
	root   s3://my-bucket/queries
	driver postgres
	dsn    {env.DATABASE_URL}
	s3 {
		region            eu-west-1
		refresh_interval  1m
		# For MinIO, R2 and other S3-compatible stores:
		# endpoint          http://minio:9000
		# use_path_style
		# access_key_id     {env.S3_KEY}
		# secret_access_key {env.S3_SECRET}
		# session_token     {env.S3_TOKEN}
	}
}
```

At startup the plugin lists every `.sql` object under the prefix and loads it into memory. If S3 can't be reached at startup, Caddy refuses to start. After that, the bucket is listed again every `refresh_interval`. Only objects whose ETag changed are downloaded, and deleted objects stop being served. If a refresh fails, the error is logged and the last good set of queries keeps being served. Lookups are served from memory, so requests for unknown routes never call S3.

Without static keys, the standard AWS credential chain is used: environment variables, shared config, then the instance or task role.

With a local directory, files are read on every request. Edits take effect immediately, with no reload.

### JSON config

The module ID is `http.handlers.sql_executor`:

```json
{
  "handler": "sql_executor",
  "root": "s3://my-bucket/queries",
  "driver": "postgres",
  "dsn": "{env.DATABASE_URL}",
  "timeout": "30s",
  "s3": { "region": "eu-west-1", "refresh_interval": "1m" }
}
```

## Routing

The request path, after any `handle_path` prefix stripping, maps to a file under `root`:

| Request | File | Format |
| --- | --- | --- |
| `POST /products` | `products.sql` | from `Accept`, else `default_format` |
| `POST /reports/sales.csv` | `reports/sales.sql` | CSV |
| `POST /reports/sales.json` | `reports/sales.sql` | JSON |
| `POST /reports/sales.yaml` (or `.yml`) | `reports/sales.sql` | YAML |
| `POST /reports/sales.xlsx` | `reports/sales.sql` | XLSX |

- **Format priority.** The path extension wins. Next comes the `Accept` header, which honours q-values and accepts `application/json`, `text/csv`, `application/yaml`, `application/x-yaml`, `text/yaml` and `application/vnd.openxmlformats-officedocument.spreadsheetml.sheet`. Last comes `default_format`. If `Accept` lists only types the plugin can't produce, the response is `406`.
- **Methods.** Only `POST` runs a query. Other methods on an existing route get `405` with `Allow: POST`.
- **Path safety.** Paths are cleaned before lookup, so `..` can't escape `root`. Hidden files and directories (names starting with `.`) are never served.

## Writing queries

Each file holds a **single statement**. Use `:name` placeholders for parameters:

```sql
-- reports/sales.sql
SELECT region, sum(amount) AS total
FROM sales
WHERE sold_at >= :from AND sold_at < :to
  AND (:region::text IS NULL OR region = :region)
GROUP BY region
ORDER BY total DESC
```

Placeholders are rewritten to the driver's positional form: `$1` for PostgreSQL, `?` for SQLite. Values are always bound as parameters and never put into the SQL text. The rewriter skips string literals (including `E'...'`), quoted identifiers, `--` and nested `/* */` comments, dollar-quoted strings and `::` casts. So `:id::int` and `'10:30'` work as you'd expect.

### Supplying parameters

Parameters come from the request body and the query string. If a name appears in both, the body wins.

```sh
# JSON body (curl's default form content type is fine too)
curl -X POST localhost:8080/api/reports/sales.csv \
  -H 'Content-Type: application/json' \
  -d '{"from": "2024-01-01", "to": "2024-02-01", "region": null}'

# Form body
curl -X POST localhost:8080/api/reports/sales -d from=2024-01-01 -d to=2024-02-01 -d region=emea

# Query string
curl -X POST 'localhost:8080/api/products/by_id?id=3'
```

- JSON numbers become integers or floats. `true` and `false` become booleans, and `null` becomes SQL `NULL`.
- JSON arrays and objects are passed as JSON text. Use them with `:ids::jsonb` / `jsonb_array_elements_text(...)` on PostgreSQL, or `json_each(:ids)` on SQLite.
- Form and query-string values are strings. PostgreSQL converts them to the parameter's type, so `?id=3` works against an `int` column.
- Every placeholder in the file must be supplied, or the response is `400` with `missing parameter(s): ...`. To make a filter optional, send `null` explicitly and write the SQL to allow for it, as `region` does in the example above. On PostgreSQL, cast optional parameters (`:region::text`) so the server can infer their type.

Statements that write data work too. Add `RETURNING` to get rows back; without it the response is an empty result.

## Output formats

| | JSON | CSV | YAML | XLSX |
| --- | --- | --- | --- | --- |
| Shape | array of objects | header row + rows | sequence of mappings | one sheet, bold frozen header row |
| Column order | preserved | preserved | preserved | preserved |
| `NULL` | `null` | empty field | `null` | empty cell |
| Timestamps | RFC 3339, UTC | RFC 3339, UTC | RFC 3339, UTC | Excel date-time |
| `DATE` columns | `"2024-05-10"` | `2024-05-10` | `"2024-05-10"` | Excel date |
| `json`/`jsonb` | embedded as JSON | JSON text | native YAML | JSON text |
| `numeric`/`decimal` | string (exact) | as-is | string (exact) | number |
| `bytea`/`BLOB` | base64 | base64 | base64 | base64 |

SQLite columns declared `BOOLEAN` are returned as `true`/`false`.

Results are streamed: rows go to the client as they are read from the database, so large exports aren't held in memory. XLSX is the exception, because the file has to be assembled before it can be sent. The first row is fetched before any response is sent, so query errors still produce a proper error status. If the database fails partway through the stream, the connection is aborted. The client then sees a broken response rather than a truncated one that looks complete.

## Error responses

Errors are returned as JSON: `{"error": "..."}`.

| Status | When |
| --- | --- |
| `400` | Missing parameters, malformed JSON or form body, or an unsupported request content type |
| `405` | The query file exists but the method isn't `POST` |
| `406` | No acceptable output format |
| `413` | Body larger than `max_body_size` |
| `500` | The query failed (details only with `verbose_errors`) |
| `504` | `timeout` exceeded |

## Security notes

- **The plugin does no authentication or authorization.** Anyone who can reach a route can run that query. Protect routes with Caddy's `basic_auth`, `forward_auth`, mTLS or a `remote_ip` matcher.
- **Give the database user only the access the queries need.** For read-only APIs, use a read-only role or `file:app.db?mode=ro` for SQLite.
- **Anyone who can write to the query directory or bucket can run SQL as that database user.** Lock down write access to `root`.
- **Keep `verbose_errors` off in production**, since database errors can reveal schema details.

## Development

```sh
go test ./...                     # unit, SQLite, S3 (fake server) and end-to-end Caddy tests
go test -short ./...              # skip the end-to-end test, which binds :9080 and :2999

# PostgreSQL integration tests run when a DSN is provided:
SQL_EXECUTOR_TEST_POSTGRES_DSN='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable' go test ./...
```
