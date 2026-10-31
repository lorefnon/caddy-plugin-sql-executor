package sqlexecutor

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
)

const (
	// maxQueryFileSize bounds how much of a single S3 object is loaded.
	maxQueryFileSize = 4 << 20

	defaultCacheSizeMB = 32
	defaultCacheTTL    = time.Minute

	// cacheEntryOverhead approximates per-entry bookkeeping, so cached
	// misses, which hold no SQL, still count against the size limit.
	cacheEntryOverhead = 128

	// fetchTimeout bounds a single S3 request. Fetches are shared between
	// concurrent requests, so they don't use any one request's context.
	fetchTimeout = 30 * time.Second
)

// S3Config holds options for loading queries from an S3 (or S3-compatible)
// bucket. The bucket and key prefix come from the s3://bucket/prefix root.
type S3Config struct {
	// AWS region. Falls back to the standard AWS environment/config.
	Region string `json:"region,omitempty"`
	// Custom endpoint for S3-compatible stores (MinIO, R2, ...).
	Endpoint string `json:"endpoint,omitempty"`
	// Use path-style addressing (required by most S3-compatible stores).
	UsePathStyle bool `json:"use_path_style,omitempty"`
	// Static credentials. When empty the default AWS credential chain
	// (environment, shared config, instance role, ...) is used.
	AccessKeyID     string `json:"access_key_id,omitempty"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`
	SessionToken    string `json:"session_token,omitempty"`
	// Maximum memory, in MiB, for cached query files (including cached
	// misses). Least recently used entries are evicted first. Default: 32.
	CacheSizeMB int `json:"cache_size_mb,omitempty"`
	// How long a cached file (or miss) is used before S3 is checked
	// again. Unchanged files are revalidated with a conditional request
	// rather than downloaded again. Default: 1m.
	CacheTTL caddy.Duration `json:"cache_ttl,omitempty"`
}

// s3API is the subset of the S3 client used here, so tests can fake it.
type s3API interface {
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	HeadBucket(ctx context.Context, in *s3.HeadBucketInput, opts ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

// s3Listing is a cached directory listing. Like s3Entry it is replaced,
// never modified.
type s3Listing struct {
	entries   []dirEntry
	fetchedAt time.Time
}

// s3Entry is a cached query file, or a cached miss. Entries are never
// modified once stored; updates replace them.
type s3Entry struct {
	name      string
	sql       string
	etag      string
	missing   bool
	fetchedAt time.Time
	size      int64
}

func (e *s3Entry) result() (string, error) {
	if e.missing {
		return "", fs.ErrNotExist
	}
	return e.sql, nil
}

// s3Source loads query files from S3 on first use and keeps them in a
// size-bounded LRU cache. Entries older than the TTL are revalidated with
// a conditional GET; misses are cached too, so unknown routes don't cause
// an S3 request each time. Concurrent loads of the same file share one
// request, and if S3 fails during revalidation the stale entry is served.
type s3Source struct {
	client   s3API
	bucket   string
	prefix   string
	ttl      time.Duration
	maxBytes int64
	logger   *zap.Logger
	now      func() time.Time
	group    singleflight.Group

	mu    sync.Mutex
	lru   *list.List // of *s3Entry, most recently used first
	items map[string]*list.Element
	bytes int64
	// Directory listings, used to resolve dynamic route segments. There is
	// one per directory in the bucket that a request has reached, so they
	// are not counted against maxBytes.
	listings map[string]*s3Listing
}

// parseS3URL splits s3://bucket/some/prefix into bucket and "some/prefix/".
func parseS3URL(raw string) (bucket, prefix string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", err
	}
	if u.Scheme != "s3" || u.Host == "" {
		return "", "", fmt.Errorf("invalid S3 location %q: expected s3://bucket[/prefix]", raw)
	}
	prefix = strings.Trim(u.Path, "/")
	if prefix != "" {
		prefix += "/"
	}
	return u.Host, prefix, nil
}

func newS3Client(ctx context.Context, cfg *S3Config) (*s3.Client, error) {
	var opts []func(*awsconfig.LoadOptions) error
	if cfg.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.Region))
	}
	if cfg.AccessKeyID != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, cfg.SessionToken)))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}
	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.UsePathStyle
		// Objects uploaded without checksums would otherwise log a
		// debug line to stdout on every fetch.
		o.DisableLogOutputChecksumValidationSkipped = true
	}), nil
}

func newS3Source(client s3API, bucket, prefix string, cfg *S3Config, logger *zap.Logger) *s3Source {
	sizeMB := cfg.CacheSizeMB
	if sizeMB <= 0 {
		sizeMB = defaultCacheSizeMB
	}
	ttl := time.Duration(cfg.CacheTTL)
	if ttl <= 0 {
		ttl = defaultCacheTTL
	}
	return &s3Source{
		client:   client,
		bucket:   bucket,
		prefix:   prefix,
		ttl:      ttl,
		maxBytes: int64(sizeMB) << 20,
		logger:   logger,
		now:      time.Now,
		lru:      list.New(),
		items:    map[string]*list.Element{},
		listings: map[string]*s3Listing{},
	}
}

// check verifies that the bucket is reachable.
func (s *s3Source) check(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)})
	return err
}

func (s *s3Source) Get(_ context.Context, name string) (string, error) {
	if !validQueryFile(name) {
		return "", fs.ErrNotExist
	}
	prev := s.cached(name)
	if prev != nil && s.now().Sub(prev.fetchedAt) < s.ttl {
		return prev.result()
	}
	v, err, _ := s.group.Do(name, func() (any, error) { return s.load(name, prev) })
	if err != nil {
		return "", err
	}
	return v.(*s3Entry).result()
}

// List returns the entries of a directory under the prefix. Listings are
// cached for the TTL; if S3 fails while refreshing one, the stale listing
// is served.
func (s *s3Source) List(_ context.Context, dir string) ([]dirEntry, error) {
	s.mu.Lock()
	prev := s.listings[dir]
	s.mu.Unlock()
	if prev != nil && s.now().Sub(prev.fetchedAt) < s.ttl {
		return prev.entries, nil
	}
	v, err, _ := s.group.Do("list:"+dir, func() (any, error) { return s.loadListing(dir, prev) })
	if err != nil {
		return nil, err
	}
	return v.(*s3Listing).entries, nil
}

func (s *s3Source) loadListing(dir string, prev *s3Listing) (*s3Listing, error) {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()

	prefix := s.prefix
	if dir != "" {
		prefix += dir + "/"
	}
	in := &s3.ListObjectsV2Input{
		Bucket:    aws.String(s.bucket),
		Prefix:    aws.String(prefix),
		Delimiter: aws.String("/"),
	}
	var entries []dirEntry
	for {
		out, err := s.client.ListObjectsV2(ctx, in)
		if err != nil {
			if prev != nil {
				s.logger.Warn("listing S3 directory failed; using cached listing",
					zap.String("prefix", prefix), zap.Error(err))
				l := &s3Listing{entries: prev.entries, fetchedAt: s.now()}
				s.storeListing(dir, l)
				return l, nil
			}
			return nil, fmt.Errorf("listing s3://%s/%s: %w", s.bucket, prefix, err)
		}
		for _, p := range out.CommonPrefixes {
			if name := strings.TrimSuffix(strings.TrimPrefix(aws.ToString(p.Prefix), prefix), "/"); name != "" && !strings.HasPrefix(name, ".") {
				entries = append(entries, dirEntry{Name: name, IsDir: true})
			}
		}
		for _, o := range out.Contents {
			// Skip "folder" placeholder objects (keys ending in /).
			if name := strings.TrimPrefix(aws.ToString(o.Key), prefix); name != "" && !strings.HasSuffix(name, "/") && !strings.HasPrefix(name, ".") {
				entries = append(entries, dirEntry{Name: name})
			}
		}
		if !aws.ToBool(out.IsTruncated) || out.NextContinuationToken == nil {
			break
		}
		in.ContinuationToken = out.NextContinuationToken
	}
	slices.SortFunc(entries, func(a, b dirEntry) int { return strings.Compare(a.Name, b.Name) })

	l := &s3Listing{entries: entries, fetchedAt: s.now()}
	s.storeListing(dir, l)
	return l, nil
}

func (s *s3Source) storeListing(dir string, l *s3Listing) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listings[dir] = l
}

func (s *s3Source) cached(name string) *s3Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	el, ok := s.items[name]
	if !ok {
		return nil
	}
	s.lru.MoveToFront(el)
	return el.Value.(*s3Entry)
}

// load fetches name from S3, revalidating prev when there is one, and
// stores the result.
func (s *s3Source) load(name string, prev *s3Entry) (*s3Entry, error) {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()

	key := s.prefix + name
	in := &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}
	if prev != nil && !prev.missing && prev.etag != "" {
		in.IfNoneMatch = aws.String(prev.etag)
	}
	out, err := s.client.GetObject(ctx, in)

	var entry *s3Entry
	switch status := httpStatus(err); {
	case err == nil:
		defer out.Body.Close()
		b, err := io.ReadAll(io.LimitReader(out.Body, maxQueryFileSize+1))
		if err != nil {
			return nil, fmt.Errorf("reading s3://%s/%s: %w", s.bucket, key, err)
		}
		if len(b) > maxQueryFileSize {
			return nil, fmt.Errorf("s3://%s/%s exceeds %d bytes", s.bucket, key, maxQueryFileSize)
		}
		entry = &s3Entry{name: name, sql: string(b), etag: aws.ToString(out.ETag)}
	case status == 304 && prev != nil:
		e := *prev
		entry = &e
	case status == 404:
		entry = &s3Entry{name: name, missing: true}
	case prev != nil:
		// Keep serving what we had; try again after another TTL.
		s.logger.Warn("revalidating S3 query failed; serving cached copy",
			zap.String("key", key), zap.Error(err))
		e := *prev
		entry = &e
	default:
		return nil, fmt.Errorf("fetching s3://%s/%s: %w", s.bucket, key, err)
	}
	entry.fetchedAt = s.now()
	s.store(entry)
	return entry, nil
}

// store adds or replaces an entry, evicting least recently used entries
// to stay within maxBytes. Entries larger than the whole cache are not
// stored.
func (s *s3Source) store(e *s3Entry) {
	e.size = int64(len(e.name)+len(e.sql)+len(e.etag)) + cacheEntryOverhead
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.items[e.name]; ok {
		s.remove(el)
	}
	if e.size > s.maxBytes {
		return
	}
	s.items[e.name] = s.lru.PushFront(e)
	s.bytes += e.size
	for s.bytes > s.maxBytes {
		s.remove(s.lru.Back())
	}
}

func (s *s3Source) remove(el *list.Element) {
	e := s.lru.Remove(el).(*s3Entry)
	delete(s.items, e.name)
	s.bytes -= e.size
}

// httpStatus extracts the HTTP status code from an AWS SDK error, or 0.
func httpStatus(err error) int {
	var re interface{ HTTPStatusCode() int }
	if errors.As(err, &re) {
		return re.HTTPStatusCode()
	}
	return 0
}
