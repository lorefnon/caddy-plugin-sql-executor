package sqlexecutor

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
)

// fakeS3 is a minimal path-style S3 endpoint supporting ListObjectsV2
// (with pagination) and GetObject for a single bucket.
type fakeS3 struct {
	bucket   string
	pageSize int

	mu      sync.Mutex
	objects map[string]string
	gets    map[string]int
	failAll bool
}

func newFakeS3(bucket string, objects map[string]string) *fakeS3 {
	return &fakeS3{bucket: bucket, pageSize: 2, objects: objects, gets: map[string]int{}}
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

func (f *fakeS3) getCount(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets[key]
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAll {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket != f.bucket || r.Method != http.MethodGet {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if key == "" {
		f.list(w, r)
		return
	}
	body, ok := f.objects[key]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`<Error><Code>NoSuchKey</Code></Error>`))
		return
	}
	f.gets[key]++
	w.Header().Set("ETag", etagOf(body))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Write([]byte(body))
}

func (f *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix := q.Get("prefix")
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	start := 0
	if tok := q.Get("continuation-token"); tok != "" {
		start, _ = strconv.Atoi(tok)
	}
	end := min(start+f.pageSize, len(keys))

	type content struct {
		Key  string
		ETag string
		Size int
	}
	res := struct {
		XMLName               xml.Name `xml:"ListBucketResult"`
		Name                  string
		Prefix                string
		KeyCount              int
		IsTruncated           bool
		NextContinuationToken string `xml:",omitempty"`
		Contents              []content
	}{Name: f.bucket, Prefix: prefix, KeyCount: end - start, IsTruncated: end < len(keys)}
	if res.IsTruncated {
		res.NextContinuationToken = strconv.Itoa(end)
	}
	for _, k := range keys[start:end] {
		res.Contents = append(res.Contents, content{Key: k, ETag: etagOf(f.objects[k]), Size: len(f.objects[k])})
	}
	w.Header().Set("Content-Type", "application/xml")
	xml.NewEncoder(w).Encode(res)
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

func TestS3Source(t *testing.T) {
	fake := newFakeS3("qbucket", map[string]string{
		"queries/one.sql":          "SELECT 1 AS n",
		"queries/nested/two.sql":   "SELECT 2 AS n",
		"queries/readme.md":        "ignored",
		"queries/.hidden/x.sql":    "SELECT 'hidden'",
		"other/outside.sql":        "SELECT 'outside prefix'",
		"queries/nested/three.sql": "SELECT :x AS n",
	})
	srv := httptest.NewServer(fake)
	defer srv.Close()

	dir := t.TempDir()
	h := &SQLExecutor{
		Root:   "s3://qbucket/queries",
		Driver: "sqlite",
		DSN:    "file:" + filepath.Join(dir, "s3.db"),
		S3: &S3Config{
			Region:          "us-east-1",
			Endpoint:        srv.URL,
			UsePathStyle:    true,
			AccessKeyID:     "test",
			SecretAccessKey: "test",
		},
	}
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()
	if err := h.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	defer h.Cleanup()
	h.logger = zap.NewNop()

	res := serve(t, h, "POST", "/one", "", nil)
	if res.status != 200 || res.body != `[{"n":1}]`+"\n" {
		t.Errorf("/one: %d %s", res.status, res.body)
	}
	res = serve(t, h, "POST", "/nested/three.csv", `{"x": 3}`, nil)
	if res.status != 200 || res.body != "n\n3\n" {
		t.Errorf("/nested/three.csv: %d %q", res.status, res.body)
	}
	for _, miss := range []string{"/readme", "/.hidden/x", "/outside", "/../other/outside"} {
		if res := serve(t, h, "POST", miss, "", nil); res.nextCalls != 1 {
			t.Errorf("%s should not resolve: %d %s", miss, res.status, res.body)
		}
	}

	src := h.source.(*s3Source)

	// Unchanged objects are not downloaded again; changed and new ones are,
	// and deleted ones disappear.
	fake.set("queries/nested/two.sql", "SELECT 22 AS n")
	fake.set("queries/new.sql", "SELECT 'new' AS n")
	fake.set("queries/nested/three.sql", "")
	if err := src.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := fake.getCount("queries/one.sql"); n != 1 {
		t.Errorf("one.sql fetched %d times, want 1", n)
	}
	if n := fake.getCount("queries/nested/two.sql"); n != 2 {
		t.Errorf("two.sql fetched %d times, want 2", n)
	}
	if got, _ := src.Get(context.Background(), "nested/two.sql"); got != "SELECT 22 AS n" {
		t.Errorf("two.sql = %q", got)
	}
	if got, _ := src.Get(context.Background(), "new.sql"); got != "SELECT 'new' AS n" {
		t.Errorf("new.sql = %q", got)
	}
	if _, err := src.Get(context.Background(), "nested/three.sql"); err != fs.ErrNotExist {
		t.Errorf("deleted query still served: %v", err)
	}

	// A failed refresh keeps serving the last good set.
	fake.mu.Lock()
	fake.failAll = true
	fake.mu.Unlock()
	if err := src.refresh(context.Background()); err == nil {
		t.Error("expected refresh error")
	}
	if got, _ := src.Get(context.Background(), "one.sql"); got != "SELECT 1 AS n" {
		t.Errorf("after failed refresh one.sql = %q", got)
	}
}

func TestS3ProvisionFailsWhenUnreachable(t *testing.T) {
	fake := newFakeS3("b", map[string]string{})
	fake.failAll = true
	srv := httptest.NewServer(fake)
	defer srv.Close()

	h := &SQLExecutor{
		Root:   "s3://b",
		Driver: "sqlite",
		DSN:    "file:" + filepath.Join(t.TempDir(), "x.db"),
		S3:     &S3Config{Region: "us-east-1", Endpoint: srv.URL, UsePathStyle: true, AccessKeyID: "a", SecretAccessKey: "b"},
	}
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()
	if err := h.Provision(ctx); err == nil {
		h.Cleanup()
		t.Fatal("expected provisioning error")
	}
}
