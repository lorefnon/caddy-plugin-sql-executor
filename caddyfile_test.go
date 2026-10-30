package sqlexecutor

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
)

func TestUnmarshalCaddyfile(t *testing.T) {
	d := caddyfile.NewTestDispenser(`
	sql_executor {
		root s3://my-bucket/queries
		driver postgres
		dsn {env.DATABASE_URL}
		timeout 15s
		max_body_size 2MiB
		default_format csv
		max_open_conns 20
		max_idle_conns 5
		verbose_errors
		s3 {
			region eu-west-1
			endpoint http://localhost:9000
			use_path_style
			access_key_id minio
			secret_access_key secret
			session_token tok
			cache_size_mb 128
			cache_ttl 30s
		}
	}`)
	var got SQLExecutor
	if err := got.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	want := SQLExecutor{
		Root:          "s3://my-bucket/queries",
		Driver:        "postgres",
		DSN:           "{env.DATABASE_URL}",
		Timeout:       caddy.Duration(15 * time.Second),
		MaxBodySize:   2 << 20,
		DefaultFormat: "csv",
		MaxOpenConns:  20,
		MaxIdleConns:  5,
		VerboseErrors: true,
		S3: &S3Config{
			Region:          "eu-west-1",
			Endpoint:        "http://localhost:9000",
			UsePathStyle:    true,
			AccessKeyID:     "minio",
			SecretAccessKey: "secret",
			SessionToken:    "tok",
			CacheSizeMB:     128,
			CacheTTL:        caddy.Duration(30 * time.Second),
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

func TestUnmarshalCaddyfileErrors(t *testing.T) {
	for _, input := range []string{
		"sql_executor ./queries",
		"sql_executor {\nbogus x\n}",
		"sql_executor {\ndriver\n}",
		"sql_executor {\ndriver a b\n}",
		"sql_executor {\ntimeout forever\n}",
		"sql_executor {\nmax_body_size lots\n}",
		"sql_executor {\nmax_open_conns -1\n}",
		"sql_executor {\nverbose_errors yes\n}",
		"sql_executor {\ns3 x {\n}\n}",
		"sql_executor {\ns3 {\nbogus\n}\n}",
		"sql_executor {\ns3 {\nuse_path_style no\n}\n}",
		"sql_executor {\ns3 {\ncache_ttl\n}\n}",
		"sql_executor {\ns3 {\ncache_size_mb lots\n}\n}",
	} {
		var h SQLExecutor
		if err := h.UnmarshalCaddyfile(caddyfile.NewTestDispenser(input)); err == nil {
			t.Errorf("%s: expected error", input)
		}
	}
}

// The directive is ordered before file_server, so a site can serve static
// files and SQL routes side by side without a route block.
func TestCaddyfileAdapt(t *testing.T) {
	adapter := caddyfile.Adapter{ServerType: httpcaddyfile.ServerType{}}
	out, _, err := adapter.Adapt([]byte(`
	:8080 {
		file_server
		handle_path /api/* {
			sql_executor {
				root ./queries
				driver sqlite
				dsn file:app.db
			}
		}
		sql_executor {
			root ./other
			driver sqlite
			dsn file:other.db
		}
	}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	js := string(out)
	sqlIdx := strings.Index(js, `"handler":"sql_executor","root":"./other"`)
	fileIdx := strings.Index(js, `"handler":"file_server"`)
	if sqlIdx < 0 || fileIdx < 0 || sqlIdx > fileIdx {
		t.Errorf("expected sql_executor ordered before file_server:\n%s", js)
	}
	if !strings.Contains(js, `"root":"./queries"`) {
		t.Errorf("nested handler missing:\n%s", js)
	}
}
