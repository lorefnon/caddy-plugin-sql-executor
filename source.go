package sqlexecutor

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// source provides SQL text for route names such as "reports/sales.sql".
// Get returns an error wrapping fs.ErrNotExist when there is no such file.
type source interface {
	Get(ctx context.Context, name string) (string, error)
}

// dirSource reads queries from a local directory on every request, so
// edits take effect immediately without reloading Caddy.
type dirSource struct {
	fsys fs.FS
}

func newDirSource(dir string) (*dirSource, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	return &dirSource{fsys: os.DirFS(dir)}, nil
}

func (d *dirSource) Get(_ context.Context, name string) (string, error) {
	if !validRouteName(name) {
		return "", fs.ErrNotExist
	}
	b, err := fs.ReadFile(d.fsys, name)
	if err != nil {
		// A directory named like a route is a miss, not a failure.
		if info, statErr := fs.Stat(d.fsys, name); statErr == nil && info.IsDir() {
			return "", fs.ErrNotExist
		}
		return "", err
	}
	return string(b), nil
}

// validRouteName rejects names that could escape the query root or refer
// to hidden files.
func validRouteName(name string) bool {
	if !fs.ValidPath(name) || !strings.HasSuffix(name, ".sql") {
		return false
	}
	for _, seg := range strings.Split(name, "/") {
		if strings.HasPrefix(seg, ".") {
			return false
		}
	}
	return true
}
