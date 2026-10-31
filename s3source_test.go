package sqlexecutor

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"html"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

// fakeS3 is a minimal path-style S3 endpoint for a single bucket, with
// HeadBucket and GetObject (including If-None-Match revalidation).
type fakeS3 struct {
	bucket string

	mu       sync.Mutex
	objects  map[string]string
	gets     map[string]int // full downloads
	notMods  map[string]int // 304 responses
	misses   map[string]int // 404 responses
	failAll  bool
	getDelay time.Duration
	lists    map[string]int // ListObjectsV2 requests by prefix
}

func newFakeS3(bucket string, objects map[string]string) *fakeS3 {
	return &fakeS3{bucket: bucket, objects: objects,
		gets: map[string]int{}, notMods: map[string]int{}, misses: map[string]int{}, lists: map[string]int{}}
}

func etagOf(body string) string {
	sum := md5.Sum([]byte(body))
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func (f *fakeS3) set(key, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if body == "" {
		delete(f.objects, key)
	} else {
		f.objects[key] = body
	}
}

func (f *fakeS3) setFailing(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failAll = fail
}

// requests reports how many requests of each kind a key has received.
func (f *fakeS3) requests(key string) (gets, notMods, misses int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets[key], f.notMods[key], f.misses[key]
}

func (f *fakeS3) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, m := range []map[string]int{f.gets, f.notMods, f.misses} {
		for _, c := range m {
			n += c
		}
	}
	return n
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	delay := f.getDelay
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAll {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket != f.bucket {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if key == "" && r.Method == http.MethodHead {
		return // HeadBucket
	}
	if key == "" && r.URL.Query().Get("list-type") == "2" {
		f.list(w, r.URL.Query().Get("prefix"), r.URL.Query().Get("delimiter"))
		return
	}
	body, ok := f.objects[key]
	if !ok {
		f.misses[key]++
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`<Error><Code>NoSuchKey</Code></Error>`))
		return
	}
	etag := etagOf(body)
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		f.notMods[key]++
		w.WriteHeader(http.StatusNotModified)
		return
	}
	f.gets[key]++
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Write([]byte(body))
}

// list answers ListObjectsV2 with the keys under prefix, grouping those
// below the delimiter into common prefixes. The caller holds f.mu.
func (f *fakeS3) list(w http.ResponseWriter, prefix, delimiter string) {
	f.lists[prefix]++
	var keys, prefixes []string
	seen := map[string]bool{}
	for key := range f.objects {
		rest, ok := strings.CutPrefix(key, prefix)
		if !ok {
			continue
		}
		if i := strings.Index(rest, delimiter); delimiter != "" && i >= 0 {
			if p := prefix + rest[:i+len(delimiter)]; !seen[p] {
				seen[p] = true
				prefixes = append(prefixes, p)
			}
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	sort.Strings(prefixes)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><IsTruncated>false</IsTruncated>`)
	for _, k := range keys {
		b.WriteString("<Contents><Key>" + html.EscapeString(k) + "</Key></Contents>")
	}
	for _, p := range prefixes {
		b.WriteString("<CommonPrefixes><Prefix>" + html.EscapeString(p) + "</Prefix></CommonPrefixes>")
	}
	b.WriteString("</ListBucketResult>")
	w.Header().Set("Content-Type", "application/xml")
	w.Write([]byte(b.String()))
}

// listRequests reports how many listings a prefix has received.
func (f *fakeS3) listRequests(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lists[prefix]
}

// testClock is a manually advanced clock for TTL tests.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newS3Handler provisions a handler over a fake bucket through the real AWS
// SDK, with a controllable clock.
func newS3Handler(t *testing.T, fake *fakeS3, root string, cfg S3Config) (*SQLExecutor, *testClock) {
	t.Helper()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	cfg.Region, cfg.Endpoint, cfg.UsePathStyle = "us-east-1", srv.URL, true
	cfg.AccessKeyID, cfg.SecretAccessKey = "test", "test"
	h := &SQLExecutor{
		Root:   root,
		Driver: "sqlite",
		DSN:    "file:" + filepath.Join(t.TempDir(), "s3.db"),
		S3:     &cfg,
	}
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	t.Cleanup(cancel)
	if err := h.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Cleanup() })
	h.logger = zap.NewNop()
	src := h.source.(*s3Source)
	src.logger = zap.NewNop()
	clock := &testClock{t: time.Unix(1_700_000_000, 0)}
	src.now = clock.now
	return h, clock
}

func TestParseS3URL(t *testing.T) {
	tests := []struct{ in, bucket, prefix string }{
		{"s3://bucket", "bucket", ""},
		{"s3://bucket/", "bucket", ""},
		{"s3://bucket/queries", "bucket", "queries/"},
		{"s3://bucket/a/b/", "bucket", "a/b/"},
	}
	for _, tt := range tests {
		b, p, err := parseS3URL(tt.in)
		if err != nil || b != tt.bucket || p != tt.prefix {
			t.Errorf("parseS3URL(%q) = %q, %q, %v", tt.in, b, p, err)
		}
	}
	for _, bad := range []string{"s3://", "http://bucket/x", "s3:///x"} {
		if _, _, err := parseS3URL(bad); err == nil {
			t.Errorf("parseS3URL(%q): expected error", bad)
		}
	}
}

func TestS3SourceServesQueries(t *testing.T) {
	fake := newFakeS3("qbucket", map[string]string{
		"queries/one.sql":          "SELECT 1 AS n",
		"queries/nested/three.sql": "SELECT :x AS n",
		"queries/jtpl.sql.j2":      "SELECT {{ x * 2 }} AS n",
		"queries/.hidden/x.sql":    "SELECT 'hidden'",
		"other/outside.sql":        "SELECT 'outside prefix'",
	})
	h, _ := newS3Handler(t, fake, "s3://qbucket/queries", S3Config{})

	if n := fake.total(); n != 0 {
		t.Errorf("provisioning fetched %d objects; loading should be lazy", n)
	}

	for target, want := range map[string]string{
		"/one":              `[{"n":1}]` + "\n",
		"/nested/three.csv": "n\n4\n",
		"/jtpl":             `[{"n":8}]` + "\n",
	} {
		if res := serve(t, h, "POST", target, `{"x": 4}`, nil); res.status != 200 || res.body != want {
			t.Errorf("%s: %d %q, want %q", target, res.status, res.body, want)
		}
	}
	for _, miss := range []string{"/readme", "/.hidden/x", "/outside", "/../other/outside"} {
		if res := serve(t, h, "POST", miss, "", nil); res.nextCalls != 1 {
			t.Errorf("%s should not resolve: %d %s", miss, res.status, res.body)
		}
	}
	// Hidden names are rejected before any S3 request is made.
	if gets, _, misses := fake.requests("queries/.hidden/x.sql"); gets+misses != 0 {
		t.Errorf("hidden key was requested from S3")
	}
}

func TestS3SourceCachingAndTTL(t *testing.T) {
	fake := newFakeS3("b", map[string]string{"q/a.sql": "SELECT 1 AS n"})
	h, clock := newS3Handler(t, fake, "s3://b/q", S3Config{CacheTTL: caddy.Duration(time.Minute)})
	src := h.source.(*s3Source)
	ctx := context.Background()

	get := func() (string, error) { return src.Get(ctx, "a.sql") }

	// First use downloads; later uses within the TTL are served from memory.
	for i := 0; i < 3; i++ {
		if sql, err := get(); err != nil || sql != "SELECT 1 AS n" {
			t.Fatalf("get: %q %v", sql, err)
		}
	}
	if gets, notMods, _ := fake.requests("q/a.sql"); gets != 1 || notMods != 0 {
		t.Errorf("within TTL: gets=%d notMods=%d, want 1, 0", gets, notMods)
	}

	// After the TTL an unchanged file is revalidated, not downloaded.
	clock.advance(61 * time.Second)
	get()
	get()
	if gets, notMods, _ := fake.requests("q/a.sql"); gets != 1 || notMods != 1 {
		t.Errorf("after TTL, unchanged: gets=%d notMods=%d, want 1, 1", gets, notMods)
	}

	// Changes become visible after the TTL.
	fake.set("q/a.sql", "SELECT 2 AS n")
	if sql, _ := get(); sql != "SELECT 1 AS n" {
		t.Errorf("change visible before TTL: %q", sql)
	}
	clock.advance(61 * time.Second)
	if sql, _ := get(); sql != "SELECT 2 AS n" {
		t.Errorf("change not visible after TTL: %q", sql)
	}

	// If S3 fails while revalidating, the cached copy keeps being served.
	fake.setFailing(true)
	clock.advance(61 * time.Second)
	if sql, err := get(); err != nil || sql != "SELECT 2 AS n" {
		t.Errorf("during outage: %q %v", sql, err)
	}
	fake.setFailing(false)

	// Deletions become visible after the TTL.
	fake.set("q/a.sql", "")
	clock.advance(61 * time.Second)
	if _, err := get(); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("deleted file: %v", err)
	}
}

func TestS3SourceCachesMisses(t *testing.T) {
	fake := newFakeS3("b", map[string]string{})
	h, clock := newS3Handler(t, fake, "s3://b", S3Config{CacheTTL: caddy.Duration(time.Minute)})

	for i := 0; i < 5; i++ {
		if res := serve(t, h, "POST", "/nope", "", nil); res.nextCalls != 1 {
			t.Fatalf("expected fall-through, got %d %s", res.status, res.body)
		}
	}
	// One lookup per candidate file name, then cached.
	for _, key := range []string{"nope.sql", "nope.sql.j2"} {
		if _, _, misses := fake.requests(key); misses != 1 {
			t.Errorf("%s: %d misses, want 1", key, misses)
		}
	}

	// A file created later is found once the cached miss expires.
	fake.set("nope.sql", "SELECT 'found' AS s")
	clock.advance(61 * time.Second)
	if res := serve(t, h, "POST", "/nope", "", nil); res.body != `[{"s":"found"}]`+"\n" {
		t.Errorf("after TTL: %d %s", res.status, res.body)
	}
}

func TestS3SourceSizeLimit(t *testing.T) {
	big := "SELECT '" + strings.Repeat("x", 400<<10) + "' AS s" // ~400 KiB
	fake := newFakeS3("b", map[string]string{
		"a.sql": big, "b.sql": big, "c.sql": big,
		"huge.sql": "SELECT '" + strings.Repeat("y", 1200<<10) + "' AS s", // > 1 MiB
	})
	h, _ := newS3Handler(t, fake, "s3://b", S3Config{CacheSizeMB: 1})
	src := h.source.(*s3Source)
	ctx := context.Background()

	// Two ~400 KiB files fit in 1 MiB; a third evicts the least recently used.
	src.Get(ctx, "a.sql")
	src.Get(ctx, "b.sql")
	src.Get(ctx, "a.sql") // a is now more recent than b
	src.Get(ctx, "c.sql") // evicts b
	if src.bytes > src.maxBytes {
		t.Errorf("cache holds %d bytes, limit %d", src.bytes, src.maxBytes)
	}
	src.Get(ctx, "a.sql")
	src.Get(ctx, "b.sql")
	if gets, _, _ := fake.requests("a.sql"); gets != 1 {
		t.Errorf("a.sql downloaded %d times, want 1 (should have stayed cached)", gets)
	}
	if gets, _, _ := fake.requests("b.sql"); gets != 2 {
		t.Errorf("b.sql downloaded %d times, want 2 (should have been evicted)", gets)
	}

	// A file larger than the whole cache is served but never cached.
	for i := 0; i < 2; i++ {
		if sql, err := src.Get(ctx, "huge.sql"); err != nil || len(sql) < 1200<<10 {
			t.Fatalf("huge.sql: %d bytes, %v", len(sql), err)
		}
	}
	if gets, _, _ := fake.requests("huge.sql"); gets != 2 {
		t.Errorf("huge.sql downloaded %d times, want 2", gets)
	}
	if src.bytes > src.maxBytes {
		t.Errorf("cache holds %d bytes, limit %d", src.bytes, src.maxBytes)
	}
}

func TestS3SourceSharesConcurrentFetches(t *testing.T) {
	fake := newFakeS3("b", map[string]string{"slow.sql": "SELECT 1"})
	h, _ := newS3Handler(t, fake, "s3://b", S3Config{})
	fake.mu.Lock()
	fake.getDelay = 100 * time.Millisecond
	fake.mu.Unlock()
	src := h.source.(*s3Source)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if sql, err := src.Get(context.Background(), "slow.sql"); err != nil || sql != "SELECT 1" {
				t.Errorf("get: %q %v", sql, err)
			}
		}()
	}
	wg.Wait()
	if gets, _, _ := fake.requests("slow.sql"); gets != 1 {
		t.Errorf("%d downloads for 20 concurrent requests, want 1", gets)
	}
}

func TestS3SourceUnavailable(t *testing.T) {
	fake := newFakeS3("b", map[string]string{"a.sql": "SELECT 1"})
	h, _ := newS3Handler(t, fake, "s3://b", S3Config{})
	fake.setFailing(true)

	// With nothing cached, the request fails (Caddy responds 500) rather
	// than falling through to the next handler.
	r := httptest.NewRequest("POST", "/a", nil)
	err := h.ServeHTTP(httptest.NewRecorder(), r, caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
		t.Error("fell through to next handler")
		return nil
	}))
	var herr caddyhttp.HandlerError
	if !errors.As(err, &herr) || herr.StatusCode != 500 {
		t.Errorf("uncached during outage: %v", err)
	}
	fake.setFailing(false)
	if res := serve(t, h, "POST", "/a", "", nil); res.status != 200 {
		t.Errorf("after recovery: %d %s", res.status, res.body)
	}
}

// Queries load lazily, so S3 being down at startup is only logged.
func TestS3ProvisionWhileUnavailable(t *testing.T) {
	fake := newFakeS3("b", map[string]string{})
	fake.failAll = true
	newS3Handler(t, fake, "s3://b", S3Config{}) // fails the test on error
}

func TestS3SourceDynamicRoutes(t *testing.T) {
	fake := newFakeS3("b", map[string]string{
		"q/users/[id].sql":      "SELECT :id AS id",
		"q/users/me.sql":        "SELECT 'me' AS id",
		"q/docs/[...slug].sql":  "SELECT :slug AS slug",
		"q/items/[id]/GET.sql":  "SELECT :id AS id",
		"q/items/[id]/":         "x", // folder placeholder, not an entry
		"other/users/[id].sql":  "SELECT 'wrong prefix'",
		"q/users/.hidden/x.sql": "SELECT 1",
	})
	h, clock := newS3Handler(t, fake, "s3://b/q", S3Config{CacheTTL: caddy.Duration(time.Minute)})

	res := serve(t, h, "POST", "/users/42", "", nil)
	if res.status != 200 || res.body != `[{"id":"42"}]`+"\n" {
		t.Errorf("dynamic file: status %d body %q", res.status, res.body)
	}
	res = serve(t, h, "POST", "/docs/a/b", "", nil)
	if res.status != 200 || res.body != `[{"slug":"[\"a\",\"b\"]"}]`+"\n" {
		t.Errorf("catch-all: status %d body %q", res.status, res.body)
	}
	if res := serve(t, h, "POST", "/nothing/here", "", nil); res.nextCalls != 1 {
		t.Errorf("unknown route: status %d", res.status)
	}

	// Listings are cached: repeated requests don't list again.
	listed := fake.listRequests("q/users/")
	for range 3 {
		serve(t, h, "POST", "/users/43", "", nil)
	}
	if got := fake.listRequests("q/users/"); got != listed {
		t.Errorf("listings of q/users/: %d, want %d", got, listed)
	}

	// ...until the TTL passes, and then a new file shows up.
	fake.set("q/users/[name]/orders.sql", "SELECT :name AS name")
	clock.advance(2 * time.Minute)
	res = serve(t, h, "POST", "/users/zed/orders", "", nil)
	if res.status != 200 || res.body != `[{"name":"zed"}]`+"\n" {
		t.Errorf("new file after TTL: status %d body %q", res.status, res.body)
	}

	// A failing S3 doesn't take down routes that were listed before.
	fake.setFailing(true)
	clock.advance(2 * time.Minute)
	res = serve(t, h, "POST", "/users/42", "", nil)
	if res.status != 200 || res.body != `[{"id":"42"}]`+"\n" {
		t.Errorf("stale listing during outage: status %d body %q", res.status, res.body)
	}
}

func TestS3SourceVerbNamedFiles(t *testing.T) {
	fake := newFakeS3("b", map[string]string{
		"items/GET.sql":         "SELECT 'list' AS op",
		"items/[id]/DELETE.sql": "SELECT 'delete' AS op, :id AS id",
	})
	h, _ := newS3Handler(t, fake, "s3://b/", S3Config{})
	h.VerbNamedFiles = true

	res := serve(t, h, "DELETE", "/items/9", "", nil)
	if res.status != 200 || res.body != `[{"op":"delete","id":"9"}]`+"\n" {
		t.Errorf("DELETE: status %d body %q", res.status, res.body)
	}
	res = serve(t, h, "GET", "/items", "", nil)
	if res.status != 200 || res.body != `[{"op":"list"}]`+"\n" {
		t.Errorf("GET: status %d body %q", res.status, res.body)
	}
	if res := serve(t, h, "GET", "/items/9", "", nil); res.nextCalls != 1 {
		t.Errorf("GET without a file: status %d", res.status)
	}
}
