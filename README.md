# caddy-plugin-sql-executor

A [Caddy](https://caddyserver.com) HTTP handler that turns a folder of `.sql` files into an API.

Point it at a local directory or an S3 bucket. Each `.sql` file becomes a route, and so does each [Jinja](#jinja-templates-sqlj2) (`.sql.j2`) template. A `POST` to that route runs the query against PostgreSQL or SQLite and returns the rows as **JSON**, **CSV**, **YAML**, **XLSX** or **HTML**.

```
queries/
├── products.sql            → POST /products
├── search.sql.j2           → POST /search   (rendered with Jinja first)
├── partials/
│   └── filters.j2          → not a route; included by templates
└── reports/
    └── monthly_sales.sql   → POST /reports/monthly_sales
                              POST /reports/monthly_sales.csv
                              POST /reports/monthly_sales.xlsx
```
## Why?

If you are building a service whose primary purpose is to run some database queries and forward the results to client (think dashboards, data visualizations, reports etc.) the API layer is mostly boilerplate. This plugin enables you to skip the service layer completely. Bring your SQL scripts, point caddy to them, and you are done. 

Focus on data modeling and writing performant queries - caddy wires up the routes from your directory structure.

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
			# formats       json csv html      # restrict the formats clients can ask for (default: all)
			default_format json               # json | csv | yaml | xlsx | html
			max_open_conns 10
			max_idle_conns 5
			# verbose_errors                  # include DB error text in responses
			# jinja_includes                  # allow include/import/extends/macro in .sql.j2
			# verb_named_files                # route by HTTP method: DELETE /a/b runs a/b/DELETE.sql
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
| `formats` | all | Formats clients may request, from `json`, `csv`, `yaml`, `xlsx` and `html`. Anything else gets `406`. |
| `default_format` | `json` | Format used when neither the extension nor `Accept` picks one. Must be one of `formats`. When `formats` leaves out `json`, the default is the first format listed. |
| `max_open_conns`, `max_idle_conns` | `database/sql` defaults | Connection pool limits. |
| `jinja_includes` | off | Allow `include`, `import`, `extends` and `macro` in templates. See [Includes, macros and template inheritance](#includes-macros-and-template-inheritance). |
| `verb_named_files` | off | Route by HTTP method, so `DELETE /foo/bar` runs `foo/bar/DELETE.sql`, and allow every method. See [Verb-named files](#verb-named-files). |
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
- **Missing files are cached too.** A route with no file costs up to two `GET`s (`.sql`, `.sql.j2`) and is then remembered for `cache_ttl`, like any other entry. A file added later appears once the cached miss expires.
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

There are two modes of operation. 

### Purely path based routing (default)

In this mode, if we make a `POST` call (only verb allowed) to `/reports/sales`, `/reports/sales.sql` gets executed. It is convenient if you already have a directory of sql files and want to expose them to web.

The request path, after any `handle_path` prefix stripping, maps to a file under `root`:

| Request | File | Format |
| --- | --- | --- |
| `POST /products` | `products.sql` | from `Accept`, else `default_format` |
| `POST /reports/sales.csv` | `reports/sales.sql` | CSV |
| `POST /reports/sales.json` | `reports/sales.sql` | JSON |
| `POST /reports/sales.yaml` (or `.yml`) | `reports/sales.sql` | YAML |
| `POST /reports/sales.xlsx` | `reports/sales.sql` | XLSX |
| `POST /reports/sales.html` | `reports/sales.sql` | HTML table |

- **Format priority.** The path extension wins. Next comes the `Accept` header, which honours q-values and accepts `application/json`, `text/csv`, `application/yaml`, `application/x-yaml`, `text/yaml`, `text/html` and `application/vnd.openxmlformats-officedocument.spreadsheetml.sheet`. Last comes `default_format`. If `Accept` lists only types the plugin can't produce (or that `formats` has turned off), the response is `406`. Browsers ask for `text/html` first, so they get the HTML table unless `html` is excluded from `formats`.
- **Restricting formats.** `formats csv json` turns off the rest. A path extension for a disabled format, such as `/sales.xlsx`, also gets `406`.
- **Methods.** Only `POST` runs a query. Other methods on an existing route get `405` with `Allow: POST`. With [`verb_named_files`](#verb-named-files), every method is allowed instead.
- **Templates.** `name.sql` is tried before `name.sql.j2`, and the first one that exists is used. Only files ending in `.sql` or `.sql.j2` are routes, so templates that other templates load (for example `partials/filters.j2`) are not.
- **Path safety.** Paths are cleaned before lookup, so `..` can't escape `root`. Hidden files and directories (names starting with `.`) are never served.

### Path + Verb based routing

With `verb_named_files` set to true, every HTTP method is allowed and a route is a **directory** holding one file per method, named for the method in upper case:

```
queries/
└── items/
    ├── GET.sql           → GET    /items
    ├── POST.sql          → POST   /items
    └── [id]/
        ├── GET.sql       → GET    /items/42
        ├── PATCH.sql.j2  → PATCH  /items/42
        └── DELETE.sql    → DELETE /items/42
```

- **Dynamic segments work as above**, with the directory holding the files named `[id]`, `[...path]` and so on.
- **Plain files aren't routes in this mode.** `items.sql` is ignored, and `POST /items` runs `items/POST.sql`.
- **A method with no file is not handled.** The request goes to the next handler, so you get whatever it does for unknown routes (`404` by default) rather than `405`.
- **Methods aren't mapped.** `HEAD` doesn't use `GET.sql`, and `OPTIONS` isn't answered. Add `HEAD.sql` or handle them in Caddy.
- **Nothing is enforced about what a method does.** A `GET.sql` that writes data will still do so. Put `SELECT`s in `GET.sql` and writes in the others.
- **Parameters, formats and templates work as usual.** A request body is read whatever the method, so `DELETE` can carry one. `/items/42.csv` runs `items/[id]/GET.sql` with CSV output.
- **The root isn't routable.** `GET /` doesn't run a root-level `GET.sql`.

### Dynamic segments

Names in the style of [Next.js dynamic routes](https://nextjs.org/docs/app/api-reference/file-conventions/dynamic-routes) capture parts of the path. They work for both directories and files:

| Name | Matches | Example |
| --- | --- | --- |
| `[id]` | One segment | `users/[id].sql` matches `/users/42` |
| `[...path]` | One or more segments | `docs/[...path].sql` matches `/docs/a` and `/docs/a/b`, not `/docs` |
| `[[...path]]` | Zero or more segments | `shop/[[...path]].sql` matches `/shop`, `/shop/a` and `/shop/a/b` |

```
queries/
├── users/
│   ├── me.sql                 → POST /users/me
│   ├── [id].sql               → POST /users/42
│   └── [id]/
│       ├── orders.sql         → POST /users/42/orders
│       └── orders/[order].sql → POST /users/42/orders/7
└── docs/[...path].sql         → POST /docs/a/b/c
```

The captured value is bound to the `:name` placeholder of the same name (and is available to [templates](#jinja-templates-sqlj2) under that name):

```sql
-- users/[id]/orders/[order].sql
SELECT * FROM orders WHERE user_id = :id AND id = :order
```

- **Catch-all values are lists**, bound as JSON text like any other array: `/docs/a/b` gives `:path = '["a","b"]'`. An optional catch-all that matches nothing gives `[]`.
- **Other values are strings.** PostgreSQL converts them to the parameter's type, as it does for form values.
- **The path wins.** A body or query-string value with the same name is ignored. A [`@header`](#header-directives-all-query-files) directive for the same name still takes precedence.
- **The most specific match wins**, level by level: a static name, then `[x]`, then `[...x]`, then `[[...x]]`. If the most specific entry leads nowhere (`users/me/` has no `orders.sql`), the next one is tried. Avoid siblings of the same kind, such as `[id].sql` next to `[name].sql`. The first by name is used.
- **The extension is still a format.** `/users/42.csv` captures `42` and returns CSV, so a value can't end in `.csv`, `.json`, `.yaml`, `.yml`, `.xlsx`, `.html` or `.htm`.
- **Names must be identifiers** (`[A-Za-z_][A-Za-z0-9_]*`). A name such as `[foo-bar]` is an ordinary file name.
- **Mind the reserved names.** `ident` and `header` are hidden from templates, so don't use them.
- **S3 needs `s3:ListBucket`**, as it already does for missing files. Matching dynamic segments lists directories, and listings are cached for `cache_ttl` like files.


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

Statements that write data work too. For a bulk insert, use a template with `{% for %}` over an array of objects. Add `RETURNING` to get rows back; without it the response is an empty result.

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

```sh
curl -X POST localhost:8080/api/products/search \
  -d '{"categories": ["coffee", "equipment"], "max_price": 30, "sort": "price", "desc": true}'
```

**`{{ ... }}` never puts the value into the SQL text.** Each output expression becomes a bound parameter, the same as a `:name` placeholder. This includes expressions with filters or inline `if`s, such as `{{ name | upper }}` and `{{ a if flag else b }}`. The example above, with two categories, runs as:

```sql
... AND category IN ($1, $2) AND price <= $3 ORDER BY "price" DESC
```

The values keep their JSON types: numbers stay numbers and booleans stay booleans. Lists and dicts are bound as JSON text. Input like `"x) OR (1=1"` is only ever compared as data.

What you can use:

| Syntax | Result |
| --- | --- |
| `{{ expr }}` | Bound parameter. Undefined values bind `NULL`. |
| `{% if %}`/`{% elif %}`/`{% else %}`, `{% for %}` (with `loop.*` and `{% else %}`), `{% set x = ... %}`, `{% raw %}`, `{% do %}`, `{% break %}`/`{% continue %}`, filters and tests | Standard Jinja for structure. |
| `{{ ident(col, allow=["a", "b"]) }}` | A double-quoted identifier (`"col"`; `schema.table` becomes `"schema"."table"`). `allow` may also be a comma-separated string. If the value isn't allowed, the response is `400`. |
| `{{ header("X-User-Id") }}` | The request header's value, bound as a parameter, or `None` if it's missing. The name is case-insensitive. |
| `{% include %}`, `{% import %}`, `{% from %}`, `{% extends %}`/`{% block %}`, `{% macro %}` | Only with [`jinja_includes`](#includes-macros-and-template-inheritance). |
| `:name` | Still works, and is still **required**: a missing value gets `400`. |
| `{# comment #}` | Comment. |

`ident` and `header` are reserved: request parameters with those names are hidden from templates.

Form and query-string values are always strings. Send numbers in a JSON body if the template does arithmetic: with `?x=4`, `{{ x * 2 }}` is string repetition (`"44"`).

These are rejected when the template is loaded:

- `{% call %}`, `{% filter %}`, `{% with %}`, `{% trans %}` and `{% set %}...{% endset %}` blocks. The plugin can't guarantee that every value inside these ends up bound.
- `{% include %}`, `{% import %}`, `{% from %}`, `{% extends %}`, `{% block %}` and `{% macro %}`, unless `jinja_includes` is on.
- `{{ value }}` inside a quoted SQL string, e.g. `'%{{ q }}%'`. The placeholder would just be literal text there. Write `'%' || {{ q }} || '%'` instead.

The request then gets `500`, and the reason is logged (and included in the response with `verbose_errors`). A malformed `allow` list on `ident` is also a template error, never "no restriction".

Notes:

- **Identifiers come from the client, so always pass `allow` to `ident`.** Quoting stops injection, but without `allow` a client could still sort by any column, including ones you'd rather not expose.
- **Guard `IN` lists with `{% if list %}`.** An empty list would render `IN ()`, which is a syntax error.
- **Request parameters can't fill the placeholders templates generate.** These are named `__tpl_1`, `__tpl_2` and so on, and request parameters with that prefix are dropped.
- **Parsed templates are cached and re-parsed when the file changes**, or when a template it extends changes.

### Includes, macros and template inheritance

With `jinja_includes` in the `sql_executor` block (`"jinja_includes": true` in JSON), templates can share SQL:

```jinja
{# layouts/list.j2 #}
SELECT id, name, price FROM products
WHERE 1 = 1{% block filters %}{% endblock %}
ORDER BY {% block order %}id{% endblock %}
```

```jinja
{# macros/filters.j2 #}
{% macro in_list(col, values) %}{{ ident(col) }} IN ({% for v in values %}{{ v }}{% if not loop.last %}, {% endif %}{% endfor %}){% endmacro %}
```

```jinja
{# products/catalog.sql.j2 #}
{% extends "layouts/list.j2" %}
{% import "macros/filters.j2" as f %}
{% block filters %}
  {%- if categories %} AND {{ f.in_list("category", categories) }}{% endif %}
  {%- include "partials/price.j2" ignore missing %}
{% endblock %}
{% block order %}{{ super() }}, price DESC{% endblock %}
```

- **Names are relative to `root`** (for S3, to the bucket prefix) and must end in `.j2`. Hidden files and `..` are rejected. Name shared templates without `.sql` (e.g. `partials/price.j2`) so they don't also become routes.
- **Every loaded template is checked the same way.** Its `{{ ... }}` output is bound, and the tags rejected above are rejected there too.
- **Macro output and `{{ super() }}` are SQL**, built from the macro's (or parent block's) template text, with its values bound. Anything else is still bound: `{{ name | safe }}`, a macro's output after a filter (`{{ f.x() | trim }}`) or after `~` concatenation. Use `{%- -%}` to control whitespace instead of string filters.
- **`include` passes the template's variables along**, including request parameters. Variables the included template sets don't leak back. `ignore missing` is supported; `without context` is not.
- **Imports, macros and `{% set %}` outside blocks in a child template are run first**, so blocks can use them. Other text outside blocks is ignored, as in Jinja.
- **`self.<block>()` and `{% call %}`/`caller()` aren't supported.**
- **Recursion is limited.** More than 64 nested includes or macro calls, or 32 templates in an `extends` chain, is an error.
- **Edits are picked up.** Included and imported templates are loaded when they're used. Templates in an `extends` chain are checked against their source on each request (for S3, against the cache).

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

Use `{{ header("X-Tenant-Id") }}`. Header names are case-insensitive:

```jinja
SELECT id, name FROM projects
WHERE tenant_id = {{ header("X-Tenant-Id") }}
{% if not header("X-Include-Archived") %}  AND NOT archived{% endif %}
```

A missing header is `None`: falsy in `{% if %}`, and bound as `NULL`. The request body and query string can't supply header values, since `header` is reserved.

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

| | JSON | CSV | YAML | XLSX | HTML |
| --- | --- | --- | --- | --- | --- |
| Shape | array of objects | header row + rows | sequence of mappings | one sheet, bold frozen header row | `<table>` with a header row |
| Column order | preserved | preserved | preserved | preserved | preserved |
| `NULL` | `null` | empty field | `null` | empty cell | empty cell, class `null` |
| Timestamps | RFC 3339, UTC | RFC 3339, UTC | RFC 3339, UTC | Excel date-time | RFC 3339, UTC |
| `DATE` columns | `"2024-05-10"` | `2024-05-10` | `"2024-05-10"` | Excel date | `2024-05-10` |
| `json`/`jsonb` | embedded as JSON | JSON text | native YAML | JSON text | JSON text |
| `numeric`/`decimal` | string (exact) | as-is | string (exact) | number | as-is |
| `bytea`/`BLOB` | base64 | base64 | base64 | base64 | base64 |

The HTML format is a standalone page with no JavaScript. The SQL that ran sits above the table in a collapsed `<details>` element, with `$1`/`?` placeholders and without argument values, so request data isn't echoed back. Every value is escaped, and the response carries a `Content-Security-Policy` that forbids scripts.

SQLite columns declared `BOOLEAN` are returned as `true`/`false`.

Results are streamed: rows go to the client as they are read from the database, so large exports aren't held in memory. XLSX is the exception, because the file has to be assembled before it can be sent. The first row is fetched before any response is sent, so query errors still produce a proper error status. If the database fails partway through the stream, the connection is aborted. The client then sees a broken response rather than a truncated one that looks complete.

## Error responses

Errors are returned as JSON: `{"error": "..."}`.

| Status | When |
| --- | --- |
| `400` | Missing parameters, malformed JSON or form body, or an unsupported request content type |
| `405` | The query file exists but the method isn't `POST` (not used with `verb_named_files`) |
| `406` | No acceptable output format, or the requested one is excluded by `formats` |
| `413` | Body larger than `max_body_size` |
| `500` | The query failed (details only with `verbose_errors`) |
| `504` | `timeout` exceeded |

## Logging

Each query's SQL, with templates already rendered and placeholders in driver form, is logged at `DEBUG` level along with its file name and argument count. Argument values aren't logged, since they carry request data. Caddy logs at `INFO` by default. To see the queries, turn on the `debug` global option, or set the level for this handler's logger alone:

```caddyfile
{
	log sql {
		level DEBUG
		include http.handlers.sql_executor
	}
}
```

## Generating an OpenAPI spec

`cmd/sql-openapi` writes an OpenAPI 3.1 description of the routes under a query root. It gets request and response types by asking a database to **describe** each statement. Nothing is executed.

```sh
go run ./cmd/sql-openapi -config examples/sql-openapi.yaml -out openapi.yaml
go run ./cmd/sql-openapi -root queries -driver postgres -dsn "$DATABASE_URL"
go run ./cmd/sql-openapi -root queries -driver postgres -schema schema.sql
```

- **`-dsn`** describes statements against a live database.
- **`-schema`** loads a file of DDL into a throwaway database first, so no running database is needed. For SQLite that's a temporary file. For PostgreSQL it's an embedded server, downloaded on first use and cached in `~/.embedded-postgres-go`.
- Flags override the config file. `$VAR` and `${VAR}` are expanded in the DSN.

Set `verb_named_files: true` in the config (or pass `-verb-named-files`) if the handler uses [`verb_named_files`](#verb-named-files). Without it, each file is a `POST` operation. With it, each `METHOD.sql` file is an operation for that method on its directory's path.

- **Paths.** [Dynamic segments](#dynamic-segments) become OpenAPI path templates, and each captured value a required `path` parameter: `users/[id].sql` is `/users/{id}`. A catch-all is a single string parameter holding the rest of the path, and `[[...x]]` is described twice, as `/shop` and `/shop/{x}`.
- **Path parameter types** are inferred from how the query uses the placeholder, as for any other parameter. An `id` compared with an integer column is an integer. A value the query doesn't use, or whose type is unknown, is a string.
- **Request body** (JSON and form) for `POST`, `PUT` and `PATCH`: one property per `:name` placeholder that the path doesn't supply, all required and nullable. `@header` placeholders become required header parameters instead.
- **Query parameters** for `GET`, `HEAD`, `DELETE`, `OPTIONS` and `TRACE`, whose requests shouldn't carry a body. The handler reads the query string for every method. Values there are text, so they can't be null, and arrays and objects are described as JSON text.
- **Operation IDs** come from the route: `users/[id]` is `users_id`. With verb-named files the method is added, as in `users_id_delete`.
- **Files that can't be described are reported** as errors, like any other file: a method OpenAPI 3.1 has no operation for (such as `PROPFIND`), or a catch-all that isn't the last segment, which the handler never serves.
- **Response**: an array of rows for JSON and YAML, plus CSV and XLSX bodies. There's a row schema per route under `components/schemas`, and shared error responses.
- **Description**: the file's `-- @description` lines, if any. Otherwise the leading `--` comment lines (other than directives), or a template's opening `{# #}` comment.

How types are inferred depends on the database:

| | PostgreSQL | SQLite |
| --- | --- | --- |
| Column types | From the database, for all columns | Declared type for table columns; expressions are typed by parsing the SQL (`count(*)` is an integer, `upper(x)` a string, and so on) |
| Parameter types | From the database | From how each placeholder is used: compared with a column, inserted into one, assigned with `SET`, in `LIMIT`, and so on |
| Enums | Enum values become an `enum` | n/a |

**Nullability** is worked out per column, from the query plan and the SQL:

- **Table columns** are non-null when declared `NOT NULL`, unless an outer join (`LEFT`, `RIGHT` or `FULL`) puts their table on the nullable side. Each join is tracked by alias, so a self-join is handled. A `LEFT JOIN` that the planner turns into an inner join (because a `WHERE` condition rejects nulls) adds no nulls.
- **Expressions** are non-null when they're sure to be, for example `count(*)`, `coalesce(x, 'default')`, arithmetic and string functions over non-null values, a `CASE` whose branches and `ELSE` are all non-null, and `max(x)` over a non-null column with `GROUP BY`.
- **Everything else** is nullable. That includes aggregates without `GROUP BY` (which return a row even for no input), columns under `ROLLUP`, `CUBE` or `GROUPING SETS`, division in SQLite (`x / 0` is `NULL`), date functions in SQLite (invalid input gives `NULL`), and scalar subqueries.

On PostgreSQL, this reads the statement's generic plan: `EXPLAIN (GENERIC_PLAN, VERBOSE)` on PostgreSQL 16 or later, or a prepared statement forced to a generic plan on versions 12–15. Expressions in the plan are parsed with PostgreSQL's own parser ([libpg_query](https://github.com/pganalyze/libpg_query), run as WebAssembly, so no cgo is needed). On SQLite, the SQL is parsed with [rqlite/sql](https://github.com/rqlite/sql) and checked against `EXPLAIN QUERY PLAN`. Neither runs the statement.

Anything the analysis can't follow, such as columns of a materialized CTE, falls back to the safe answer: nullable if the query has outer joins or grouping sets.

Types match what the handler actually returns. For example, `numeric` is a string with `format: decimal`, `bytea` is base64, `json`/`jsonb` is any JSON value, and arrays, `time` and `interval` are text.

### Annotating endpoints, columns and parameters

To document an endpoint, or when inference falls short, add `--` comments next to the query, much like `@header`:

```sql
-- @description Sales per category.
-- @description
-- @description Prices are in **cents**.
-- @column avg_price = {description: "Average price, rounded to cents."}
-- @column total = [number, "null"]
-- @column "Unit Price" = {type: string, format: decimal}
-- @param :since = {type: string, format: date}
SELECT ...
```

- **`@description`** sets the operation's description, taking the place of the leading comment. Each line adds a line of Markdown; a bare `-- @description` adds a blank one. Text after the first space is kept as written, so it can be indented.
- **The schema after `=` is YAML**: a type name (`integer`), a list of them (`[integer, "null"]`), or a schema using `type`, `format`, `contentEncoding`, `description`, `enum`, `items`, `properties`, `required`, `anyOf` or `$ref`. Values with a comma need quotes.
- **Keys you give replace the inferred ones.** A schema with only a `description` keeps the inferred type and nullability. Anything about the type replaces the whole inferred type, nullability included: `integer` means a non-null integer.
- **Columns** are named as they appear in the result; quote names that contain spaces. **Parameters** are `:name` placeholders or, in templates, request properties.
- **In templates**, annotations are read from the rendered SQL, so they can sit inside `{% if %}` along with the columns they describe. A template with no samples has no rendered SQL, so its annotations aren't seen. Every sample must render the same `@description`.
- **Mistakes are errors:** an annotation naming no column or parameter, an unknown type such as `int`, an unsupported keyword, or a `@param` for a placeholder that `@header` fills.

The handler ignores these comments. Schemas in the config file's `columns` and `params` are applied on top, the same way.

### Config file

```yaml
info: { title: Products API, version: 1.0.0 }
servers: [{ url: https://example.com/api }]
root: queries            # relative to this file
driver: sqlite
schema: schema.sql       # or dsn: ${DATABASE_URL}
# postgres_version: "16" # PostgreSQL major version for schema (default: latest)
jinja_includes: true
queries:                 # keyed by file or by route
  products/search.sql.j2:
    samples:             # template parameters to render with
      - {}
      - { categories: [coffee, tea], max_price: 20.5, sort: price, desc: true }
    headers: { X-Tenant-Id: "1" }   # headers set while rendering
  category_summary.sql:
    columns:             # like -- @column, applied after annotations
      avg_price: { type: number }
  products.sql:
    params:              # like -- @param
      category: { type: [string, "null"] }
```

**Templates** produce different SQL for different inputs, so they're described from **samples**:

- Each sample is rendered as if it were a request, then described.
- Request properties are the union of the sample keys, typed by the sample values (e.g. `categories: [coffee]` becomes an array of strings). None are required.
- If the samples produce different columns, the row schema is an `anyOf` of the shapes.
- `header("...")` calls become optional header parameters.
- A template with no samples gets an open object for the request body and for rows.

If a file can't be described (a SQL error, or a sample that fails to render), the error is printed and the file is left out of the spec. The command then exits with status 1. The same happens when a `queries` entry matches no file.

## Security notes

- **The plugin does no authentication or authorization.** Anyone who can reach a route can run that query. Protect routes with Caddy's `basic_auth`, a plugin like `caddy-security` or a solution compatible with `forward_auth` (like `tinyauth`).
- **Give the database user only the access the queries need.** For read-only APIs, use a read-only role or `file:app.db?mode=ro` for SQLite.
- **Anyone who can write to the query directory or bucket can run SQL as that database user.** Lock down write access to `root`.
- **Keep `verbose_errors` off in production**, since database errors can reveal schema details.

## Development

```sh
go test ./...                     # unit, SQLite, S3 (fake server) and end-to-end Caddy tests
go test -short ./...              # skip the end-to-end test, which binds :9080 and :2999

# PostgreSQL integration tests run when a DSN is provided:
SQL_EXECUTOR_TEST_POSTGRES_DSN='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable' go test ./...

# Or run the sql-openapi PostgreSQL tests against an embedded server (downloaded on first use):
SQL_OPENAPI_TEST_EMBEDDED_POSTGRES=1 go test ./cmd/sql-openapi

# Regenerate the sql-openapi golden file after an intended change:
go test ./cmd/sql-openapi -update
```
