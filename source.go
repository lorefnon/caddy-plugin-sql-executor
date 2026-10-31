package sqlexecutor

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
)

// source provides SQL text for route names such as "reports/sales.sql".
// Get returns an error wrapping fs.ErrNotExist when there is no such file.
type source interface {
	Get(ctx context.Context, name string) (string, error)
	// List returns the visible entries of a directory ("" is the root),
	// sorted by name. It returns an error wrapping fs.ErrNotExist when
	// there is no such directory.
	List(ctx context.Context, dir string) ([]dirEntry, error)
}

// dirEntry is a file or subdirectory found by source.List.
type dirEntry struct {
	Name  string
	IsDir bool
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
	if !validQueryFile(name) {
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

func (d *dirSource) List(_ context.Context, dir string) ([]dirEntry, error) {
	if dir == "" {
		dir = "."
	}
	if !fs.ValidPath(dir) {
		return nil, fs.ErrNotExist
	}
	des, err := fs.ReadDir(d.fsys, dir)
	if err != nil {
		return nil, err
	}
	entries := make([]dirEntry, 0, len(des))
	for _, de := range des {
		if strings.HasPrefix(de.Name(), ".") {
			continue
		}
		isDir := de.IsDir()
		if de.Type()&fs.ModeSymlink != 0 {
			if info, err := fs.Stat(d.fsys, path.Join(dir, de.Name())); err == nil {
				isDir = info.IsDir()
			}
		}
		entries = append(entries, dirEntry{Name: de.Name(), IsDir: isDir})
	}
	return entries, nil
}

// validQueryFile accepts the names of query files and of Jinja templates
// they load, rejecting names that could escape the query root or refer to
// hidden files. Only names ending in .sql or .sql.j2 can back a route, so
// a template such as partials/filters.j2 is not one.
func validQueryFile(name string) bool {
	if !fs.ValidPath(name) || !(strings.HasSuffix(name, ".sql") || strings.HasSuffix(name, jinjaExt)) {
		return false
	}
	for _, seg := range strings.Split(name, "/") {
		if strings.HasPrefix(seg, ".") {
			return false
		}
	}
	return true
}
