package sqlexecutor

import (
	"strconv"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/dustin/go-humanize"
)

func init() {
	httpcaddyfile.RegisterHandlerDirective("sql_executor", parseCaddyfile)
	httpcaddyfile.RegisterDirectiveOrder("sql_executor", httpcaddyfile.Before, "file_server")
}

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	m := new(SQLExecutor)
	err := m.UnmarshalCaddyfile(h.Dispenser)
	return m, err
}

// UnmarshalCaddyfile sets up the handler from Caddyfile tokens. Syntax:
//
//	sql_executor [<matcher>] {
//	    root           <directory | s3://bucket/prefix>
//	    driver         postgres | sqlite
//	    dsn            <data source name>
//	    timeout        <duration>
//	    max_body_size  <size>
//	    formats        json | csv | yaml | xlsx | html ...
//	    default_format json | csv | yaml | xlsx | html
//	    max_open_conns <n>
//	    max_idle_conns <n>
//	    verbose_errors
//	    jinja_includes
//	    verb_named_files
//	    s3 {
//	        region            <region>
//	        endpoint          <url>
//	        use_path_style
//	        access_key_id     <id>
//	        secret_access_key <secret>
//	        session_token     <token>
//	        cache_size_mb     <MiB>
//	        cache_ttl         <duration>
//	    }
//	}
func (h *SQLExecutor) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // directive name
	// No positional root: an absolute path would be parsed as a matcher.
	if d.NextArg() {
		return d.ArgErr()
	}

	for d.NextBlock(0) {
		switch d.Val() {
		case "root":
			if err := singleArg(d, &h.Root); err != nil {
				return err
			}
		case "driver":
			if err := singleArg(d, &h.Driver); err != nil {
				return err
			}
		case "dsn":
			if err := singleArg(d, &h.DSN); err != nil {
				return err
			}
		case "formats":
			h.Formats = d.RemainingArgs()
			if len(h.Formats) == 0 {
				return d.ArgErr()
			}
		case "default_format":
			if err := singleArg(d, &h.DefaultFormat); err != nil {
				return err
			}
		case "timeout":
			if err := durationArg(d, &h.Timeout); err != nil {
				return err
			}
		case "max_body_size":
			var raw string
			if err := singleArg(d, &raw); err != nil {
				return err
			}
			size, err := humanize.ParseBytes(raw)
			if err != nil {
				return d.Errf("invalid max_body_size %q: %v", raw, err)
			}
			h.MaxBodySize = int64(size)
		case "max_open_conns":
			if err := intArg(d, &h.MaxOpenConns); err != nil {
				return err
			}
		case "max_idle_conns":
			if err := intArg(d, &h.MaxIdleConns); err != nil {
				return err
			}
		case "verbose_errors":
			if d.NextArg() {
				return d.ArgErr()
			}
			h.VerboseErrors = true
		case "jinja_includes":
			if d.NextArg() {
				return d.ArgErr()
			}
			h.JinjaIncludes = true
		case "verb_named_files":
			if d.NextArg() {
				return d.ArgErr()
			}
			h.VerbNamedFiles = true
		case "s3":
			if d.NextArg() {
				return d.ArgErr()
			}
			if h.S3 == nil {
				h.S3 = new(S3Config)
			}
			if err := h.S3.unmarshalCaddyfile(d); err != nil {
				return err
			}
		default:
			return d.Errf("unrecognized sql_executor option %q", d.Val())
		}
	}
	return nil
}

func (c *S3Config) unmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		switch d.Val() {
		case "region":
			if err := singleArg(d, &c.Region); err != nil {
				return err
			}
		case "endpoint":
			if err := singleArg(d, &c.Endpoint); err != nil {
				return err
			}
		case "use_path_style":
			if d.NextArg() {
				return d.ArgErr()
			}
			c.UsePathStyle = true
		case "access_key_id":
			if err := singleArg(d, &c.AccessKeyID); err != nil {
				return err
			}
		case "secret_access_key":
			if err := singleArg(d, &c.SecretAccessKey); err != nil {
				return err
			}
		case "session_token":
			if err := singleArg(d, &c.SessionToken); err != nil {
				return err
			}
		case "cache_size_mb":
			if err := intArg(d, &c.CacheSizeMB); err != nil {
				return err
			}
		case "cache_ttl":
			if err := durationArg(d, &c.CacheTTL); err != nil {
				return err
			}
		default:
			return d.Errf("unrecognized s3 option %q", d.Val())
		}
	}
	return nil
}

func singleArg(d *caddyfile.Dispenser, dst *string) error {
	if !d.NextArg() {
		return d.ArgErr()
	}
	*dst = d.Val()
	if d.NextArg() {
		return d.ArgErr()
	}
	return nil
}

func durationArg(d *caddyfile.Dispenser, dst *caddy.Duration) error {
	var raw string
	if err := singleArg(d, &raw); err != nil {
		return err
	}
	dur, err := caddy.ParseDuration(raw)
	if err != nil {
		return d.Errf("invalid duration %q: %v", raw, err)
	}
	*dst = caddy.Duration(dur)
	return nil
}

func intArg(d *caddyfile.Dispenser, dst *int) error {
	var raw string
	if err := singleArg(d, &raw); err != nil {
		return err
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return d.Errf("invalid number %q", raw)
	}
	*dst = n
	return nil
}

// Interface guard
var _ caddyfile.Unmarshaler = (*SQLExecutor)(nil)
