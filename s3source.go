package sqlexecutor

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
)

// maxQueryFileSize bounds how much of a single S3 object is loaded.
const maxQueryFileSize = 4 << 20

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
	// How often the bucket is re-listed to pick up added, changed or
	// removed queries. Default: 1m.
	RefreshInterval caddy.Duration `json:"refresh_interval,omitempty"`
}

// s3API is the subset of the S3 client used here, so tests can fake it.
type s3API interface {
	s3.ListObjectsV2APIClient
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

type s3Entry struct {
	etag string
	sql  string
}

// s3Source keeps an in-memory index of every .sql object under the prefix
// and refreshes it periodically. Serving from memory keeps lookups cheap
// and means unknown routes never cause S3 requests.
type s3Source struct {
	client s3API
	bucket string
	prefix string
	logger *zap.Logger

	mu      sync.RWMutex
	entries map[string]s3Entry
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

func newS3Source(client s3API, bucket, prefix string, logger *zap.Logger) *s3Source {
	return &s3Source{client: client, bucket: bucket, prefix: prefix, logger: logger, entries: map[string]s3Entry{}}
}

func (s *s3Source) Get(_ context.Context, name string) (string, error) {
	if !validRouteName(name) {
		return "", fs.ErrNotExist
	}
	s.mu.RLock()
	e, ok := s.entries[name]
	s.mu.RUnlock()
	if !ok {
		return "", fs.ErrNotExist
	}
	return e.sql, nil
}

// refresh re-lists the bucket, downloading only objects whose ETag changed.
// On error the previous index is kept intact.
func (s *s3Source) refresh(ctx context.Context) error {
	s.mu.RLock()
	old := s.entries
	s.mu.RUnlock()

	next := make(map[string]s3Entry, len(old))
	pager := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(s.prefix),
	})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("listing s3://%s/%s: %w", s.bucket, s.prefix, err)
		}
		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			name := strings.TrimPrefix(key, s.prefix)
			if !validRouteName(name) {
				continue
			}
			etag := aws.ToString(obj.ETag)
			if prev, ok := old[name]; ok && etag != "" && prev.etag == etag {
				next[name] = prev
				continue
			}
			body, err := s.fetch(ctx, key)
			if err != nil {
				return err
			}
			next[name] = s3Entry{etag: etag, sql: body}
		}
	}

	s.mu.Lock()
	s.entries = next
	s.mu.Unlock()
	return nil
}

func (s *s3Source) fetch(ctx context.Context, key string) (string, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return "", fmt.Errorf("fetching s3://%s/%s: %w", s.bucket, key, err)
	}
	defer out.Body.Close()
	b, err := io.ReadAll(io.LimitReader(out.Body, maxQueryFileSize+1))
	if err != nil {
		return "", fmt.Errorf("reading s3://%s/%s: %w", s.bucket, key, err)
	}
	if len(b) > maxQueryFileSize {
		return "", fmt.Errorf("s3://%s/%s exceeds %d bytes", s.bucket, key, maxQueryFileSize)
	}
	return string(b), nil
}

// watch refreshes the index every interval until ctx is cancelled.
func (s *s3Source) watch(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.refresh(ctx); err != nil && ctx.Err() == nil {
				s.logger.Error("refreshing S3 queries; keeping previous set", zap.Error(err))
			}
		}
	}
}
