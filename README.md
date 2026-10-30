# caddy-plugin-sql-executor

A [Caddy](https://caddyserver.com) HTTP handler that turns a folder of `.sql` files into an API.

Point it at a local directory or an S3 bucket. Each `.sql` file becomes a route, and so does each [Jinja](#jinja-templates-sqlj2) (`.sql.j2`) or [Handlebars](#handlebars-templates-sqlhbs) (`.sql.hbs`) template. A `POST` to that route runs the query against PostgreSQL or SQLite and returns the rows as **JSON**, **CSV**, **YAML** or **XLSX**.

```
queries/
├── products.sql            → POST /products
├── search.sql.j2          → POST /search   (rendered with Jinja first)
├── report.sql.hbs         → POST /report   (rendered with Handlebars first)
└── reports/
    └── monthly_sales.sql   → POST /reports/monthly_sales
                              POST /reports/monthly_sales.csv
                              POST /reports/monthly_sales.xlsx
```
## Why?

If you are building a service whose primary purpose is to run some database queries and forward the results to client (think dashboards, data visualizations, reports etc.) the API layer is mostly boilerplate. This plugin enables you to skip the service layer completely. Bring your SQL scripts, point caddy to them, and you are done. 

Focus on data modeling and writing performant queries - caddy takes care of routing.

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
		cache_size_mb     32
		cache_ttl         1m
		# For MinIO, R2 and other S3-compatible stores:
		# endpoint          http://minio:9000
		# use_path_style
		# access_key_id     {env.S3_KEY}
		# secret_access_key {env.S3_SECRET}
		# session_token     {env.S3_TOKEN}
	}
}
```

Query files are fetched **on first use** and kept in an in-memory cache:

| Option | Default | Description |
| --- | --- | --- |
| `cache_size_mb` | `32` | Memory for cached files, in MiB. When it's full, the least recently used entries are evicted. A single file larger than the whole cache is still served, but isn't cached. |
| `cache_ttl` | `1m` | How long a cached entry is used before S3 is checked again. |

How the cache behaves:

- **Expired entries are revalidated.** After `cache_ttl`, the next request asks S3 whether the file changed (a conditional `GET` using the ETag). An unchanged file isn't downloaded again. Changes and deletions are therefore picked up within `cache_ttl`.
- **Missing files are cached too.** A route with no file costs up to three `GET`s (`.sql`, `.sql.hbs`, `.sql.j2`) and is then remembered for `cache_ttl`, like any other entry. A file added later appears once the cached miss expires.
- **Concurrent requests share one fetch.** Many simultaneous requests for an uncached file result in a single S3 request.
- **S3 outages degrade gracefully.** If S3 fails while revalidating, the cached copy keeps being served and is checked again after another `cache_ttl`. A request for a file that isn't cached fails with `500`.
- **Startup doesn't depend on S3.** Nothing is loaded when Caddy starts, so an unreachable bucket is logged as a warning rather than stopping Caddy.

The credentials need `s3:GetObject` and `s3:ListBucket` on the bucket. Without `s3:ListBucket`, S3 answers requests for missing files with `403 Access Denied` instead of `404`. The plugin can't tell that apart from a real error, so unknown routes would fail instead of falling through to the next handler.

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
  "s3": { "region": "eu-west-1", "cache_size_mb": 32, "cache_ttl": "1m" }
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
- **Templates.** Files are tried in the order `name.sql`, `name.sql.hbs`, `name.sql.j2`, and the first one that exists is used.
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

Statements that write data work too. For a bulk insert, use a template with `{{#each}}` over an array of objects. Add `RETURNING` to get rows back; without it the response is an empty result.

## Jinja templates (`.sql.j2`)

Use a template when the shape of the SQL depends on the request, for example optional filters, `IN` lists of any length, bulk inserts or a client-chosen sort column. Jinja templates are rendered with [gonja](https://github.com/NikolaLohinski/gonja), a Go port of Jinja2, against the request parameters (body and query string). The resulting SQL then runs like any other query. If you've used dbt, the syntax is the same.

```jinja
{# products/search.sql.j2 #}
SELECT id, name, category, price
FROM products
WHERE 1 = 1
{% if categories %}
  AND category IN ({% for c in categories %}{{ c }}{% if not loop.last %}, {% endif %}{% endfor %})
{% endif %}
{% if max_price %}
  AND price <= {{ max_price }}
{% endif %}
ORDER BY {{ ident(sort | default("id"), allow=["id", "name", "price"]) }} {% if desc %}DESC{% else %}ASC{% endif %}
```

**`{{ ... }}` never puts the value into the SQL text.** Each output expression becomes a bound parameter, keeping its type. This includes expressions with filters or inline `if`s, such as `{{ name | upper }}` and `{{ a if flag else b }}`. The example above, with two categories, runs as `... IN ($1, $2) AND price <= $3 ORDER BY "price" DESC`. Lists and dicts are bound as JSON text.

What you can use:

| Syntax | Result |
| --- | --- |
| `{{ expr }}` | Bound parameter. Undefined values bind `NULL`. |
| `{% if %}`/`{% elif %}`/`{% else %}`, `{% for %}` (with `loop.*` and `{% else %}`), `{% set x = ... %}`, `{% raw %}`, `{% do %}`, `{% break %}`/`{% continue %}`, filters and tests | Standard Jinja for structure. |
| `{{ ident(col, allow=["a", "b"]) }}` | A double-quoted identifier. `allow` may also be a comma-separated string. If the value isn't allowed, the response is `400`. |
| `{{ header("X-User-Id") }}` | The request header's value, bound as a parameter, or `None` if it's missing. The name is case-insensitive. |
| `:name` | Still works, and is still **required**. |
| `{# comment #}` | Comment. |

`ident` and `header` are reserved: request parameters with those names are hidden from templates.

Form and query-string values are always strings. Send numbers in a JSON body if the template does arithmetic: with `?x=4`, `{{ x * 2 }}` is string repetition (`"44"`).

These are rejected when the template is loaded:

- `{% include %}`, `{% extends %}`, `{% import %}` and `{% from %}`. A template can never load another file.
- `{% macro %}`, `{% call %}`, `{% filter %}`, `{% with %}`, `{% block %}` and `{% set %}...{% endset %}` blocks. The plugin can't guarantee that every value inside these ends up bound.
- `{{ value }}` inside a quoted SQL string, as with Handlebars below.

The request then gets `500`, and the reason is logged (and included in the response with `verbose_errors`). A malformed `allow` list on `ident` is also a template error, never "no restriction".

## Handlebars templates (`.sql.hbs`)

Handlebars templates work the same way as Jinja ones; only the syntax differs. They're rendered with [Handlebars](https://handlebarsjs.com) (via [raymond](https://github.com/mailgun/raymond), which is no longer maintained) against the request parameters (body and query string). The resulting SQL then runs like any other query.

```handlebars
{{!-- products/search.sql.hbs --}}
SELECT id, name, category, price
FROM products
WHERE 1 = 1
{{#if categories}}
  AND category IN ({{#each categories}}{{this}}{{#unless @last}}, {{/unless}}{{/each}})
{{/if}}
{{#if max_price}}
  AND price <= {{max_price}}
{{/if}}
ORDER BY {{ident sort allow="id,name,price"}} {{#if desc}}DESC{{else}}ASC{{/if}}
```

```sh
curl -X POST localhost:8080/api/products/search \
  -d '{"categories": ["coffee", "equipment"], "max_price": 30, "sort": "price", "desc": true}'
```

**`{{value}}` never puts the value into the SQL text.** Each `{{value}}` becomes a bound parameter, the same as a `:name` placeholder. The example above, with two categories, runs as:

```sql
... AND category IN ($1, $2) AND price <= $3 ORDER BY "price" DESC
```

The values keep their JSON types: numbers stay numbers and booleans stay booleans. Arrays and objects are bound as JSON text. Input like `"x) OR (1=1"` is only ever compared as data.

What you can use in a template:

| Syntax | Result |
| --- | --- |
| `{{name}}`, `{{user.email}}`, `{{this}}`, `{{@index}}` | Bound parameter. A missing value binds `NULL`. |
| `{{#if}}`, `{{#unless}}`, `{{#each}}`, `{{#with}}`, `{{#equal a "b"}}`, `{{else}}` | Standard Handlebars blocks for structure. |
| `{{ident col allow="a,b,c"}}` | A double-quoted identifier (`"col"`; `schema.table` becomes `"schema"."table"`). If the value isn't in `allow`, the response is `400`. |
| `{{bind (lookup obj key)}}` | Explicit binding, for the output of a helper. |
| `:name` | Still works, and is still **required**: a missing value gets `400`. |
| `{{!-- comment --}}` | Comment. Use the `--` form if the comment contains `}}`. |

These are rejected when the template is loaded. The request then gets `500`, and the reason is logged (and included in the response with `verbose_errors`):

- `{{{raw}}}` and `{{& raw}}`, which would paste text straight into the SQL.
- Output from any helper other than `bind` and `ident`, for example `{{lookup a b}}`. Wrap it as `{{bind (lookup a b)}}`.
- Partials.
- `{{value}}` inside a quoted SQL string, e.g. `'%{{q}}%'`. The placeholder would just be literal text there. Write `'%' || {{q}} || '%'` instead.

Notes:

- **Identifiers come from the client, so always pass `allow` to `ident`.** Quoting stops injection, but without `allow` a client could still sort by any column, including ones you'd rather not expose.
- **Guard `IN` lists with `{{#if list}}`.** An empty list would render `IN ()`, which is a syntax error.
- **Request parameters can't fill the placeholders templates generate.** These are named `__tpl_1`, `__tpl_2` and so on, and request parameters with that prefix are dropped.
- **Parsed templates are cached and re-parsed when the file changes.**

## Request headers

Queries can read request headers in two ways. Header values are always bound as parameters, like any other value.

### `@header` directives (all query files)

A `--` comment of the form `@header :name = Header-Name` fills the `:name` placeholder from that header:

```sql
-- @header :user_id = X-User-Id
SELECT id, total FROM orders WHERE user_id = :user_id
```

- **The header always wins.** A `user_id` in the body or query string is ignored, so a client can't override the value.
- **The header is required.** If it's missing, the response is `400` with `missing header X-User-Id (for :user_id)`. A value in the body does not take its place.
- **Repeated headers are joined** with `", "`.
- **Mistakes are reported as errors.** A malformed directive, two mappings for the same placeholder, or a mapping whose placeholder the query never uses all return `500`.
- **Comments that merely start with `@header` are left alone.** Directives are only read from `--` comments, never from string literals or `/* */` comments. So `-- @headers are set by nginx` is just a comment.

### Headers in templates

In Jinja templates, use `{{ header("X-Tenant-Id") }}` as described above.

In a `.sql.hbs` template, headers are available as the `@headers` data variable. Header names are case-insensitive:

```handlebars
SELECT id, name FROM projects
WHERE tenant_id = {{@headers.x-tenant-id}}
{{#if @headers.x-include-archived}}{{else}}  AND NOT archived{{/if}}
```

A missing header is falsy in `{{#if}}` and binds `NULL`. The request body and query string can't supply `@headers` values: a body field named `headers` is ordinary data. Use `@headers.<name>` directly; forms like `@../headers.x` or `@headers.x.y` are rejected.

### Trusting headers

Clients can send any header they like. This plugin passes header values through as-is; whether a value can be trusted is decided by the Caddy config in front of it. If a header carries identity, make sure Caddy sets it and the client can't:

```caddyfile
handle_path /api/* {
	# copy_headers replaces any client-supplied X-User-Id
	# with the auth service's value, or removes it.
	forward_auth auth-service:9000 {
		uri /verify
		copy_headers X-User-Id
	}
	sql_executor { ... }
}
```

To remove a header the client must not provide, use `request_header -X-User-Id`.

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
