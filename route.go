package sqlexecutor

import (
	"context"
	"errors"
	"io/fs"
	"path"
	"regexp"
	"strings"
)

// This file resolves request paths to query files when route segments are
// dynamic, using Next.js's naming:
//
//	[id]          matches one segment, bound to :id
//	[...path]     matches one or more segments, bound to :path as a list
//	[[...path]]   matches zero or more segments, bound to :path as a list
//
// A dynamic name can be a directory (users/[id]/orders.sql) or a file
// (users/[id].sql). At each level the most specific entry wins: a static
// name, then [x], then [...x], then [[...x]]. If the most specific entry
// leads nowhere, the next one is tried.

const (
	paramIdent = `([A-Za-z_][A-Za-z0-9_]*)`
)

var (
	paramSegment       = regexp.MustCompile(`^\[` + paramIdent + `\]$`)
	catchAllSegment    = regexp.MustCompile(`^\[\.\.\.` + paramIdent + `\]$`)
	optCatchAllSegment = regexp.MustCompile(`^\[\[\.\.\.` + paramIdent + `\]\]$`)
)

// SegmentKind is the kind of a route segment.
type SegmentKind int

const (
	// StaticSegment is an ordinary name, matched literally.
	StaticSegment SegmentKind = iota
	// ParamSegment is [name], matching one segment.
	ParamSegment
	// CatchAllSegment is [...name], matching one or more segments.
	CatchAllSegment
	// OptionalCatchAllSegment is [[...name]], matching zero or more.
	OptionalCatchAllSegment
)

// parseSegment classifies a file or directory name, without its .sql or
// .sql.j2 suffix. Names that are not valid dynamic segments, such as
// [foo-bar], are static.
func parseSegment(name string) (SegmentKind, string) {
	for _, c := range []struct {
		re   *regexp.Regexp
		kind SegmentKind
	}{{paramSegment, ParamSegment}, {catchAllSegment, CatchAllSegment}, {optCatchAllSegment, OptionalCatchAllSegment}} {
		if m := c.re.FindStringSubmatch(name); m != nil {
			return c.kind, m[1]
		}
	}
	return StaticSegment, ""
}

// routeMatch is a query file found for a request path, with the values
// captured by its dynamic segments.
type routeMatch struct {
	// File is the query file's name relative to the root, ending in .sql.
	// The .sql.j2 variant is used if only that exists.
	File   string
	Params map[string]any
}

func (m *routeMatch) capture(name string, value any) {
	if m.Params == nil {
		m.Params = map[string]any{}
	}
	m.Params[name] = value
}

// dirIndex holds a directory's entries by role.
type dirIndex struct {
	// stems are the names of the files, without .sql or .sql.j2.
	stems map[string]bool
	dirs  map[string]bool
	// names of dynamic entries by kind, in listing order.
	dynFiles [4][]string
	dynDirs  [4][]string
}

func indexDir(entries []dirEntry) *dirIndex {
	idx := &dirIndex{stems: map[string]bool{}, dirs: map[string]bool{}}
	for _, e := range entries {
		if e.IsDir {
			idx.dirs[e.Name] = true
			if kind, _ := parseSegment(e.Name); kind != StaticSegment {
				idx.dynDirs[kind] = append(idx.dynDirs[kind], e.Name)
			}
			continue
		}
		stem, ok := strings.CutSuffix(e.Name, ".sql")
		if !ok {
			if stem, ok = strings.CutSuffix(e.Name, jinjaExt); !ok {
				continue
			}
			if stem, ok = strings.CutSuffix(stem, ".sql"); !ok {
				continue
			}
		}
		if idx.stems[stem] {
			continue
		}
		idx.stems[stem] = true
		if kind, _ := parseSegment(stem); kind != StaticSegment {
			idx.dynFiles[kind] = append(idx.dynFiles[kind], stem)
		}
	}
	return idx
}

func anySlice(segs []string) []any {
	out := make([]any, len(segs))
	for i, s := range segs {
		out[i] = s
	}
	return out
}

// match finds the query file for the path segments under dir, which is ""
// for the root. In normal mode the last segment names a file. With verb
// set, every segment names a directory, and the file is verb + ".sql" in
// the last one. It returns a nil match when there is none.
func (h *SQLExecutor) match(ctx context.Context, dir string, segs []string, verb string) (*routeMatch, error) {
	entries, err := h.source.List(ctx, dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	idx := indexDir(entries)
	join := func(name string) string { return path.Join(dir, name) }
	leaf := func(d string) string { return path.Join(d, verb+".sql") }

	if len(segs) == 0 {
		if verb != "" {
			if idx.stems[verb] {
				return &routeMatch{File: leaf(dir)}, nil
			}
			// [[...x]] also matches no segments at all.
			for _, name := range idx.dynDirs[OptionalCatchAllSegment] {
				if sub, _ := h.source.List(ctx, join(name)); indexDir(sub).stems[verb] {
					_, param := parseSegment(name)
					m := &routeMatch{File: leaf(join(name))}
					m.capture(param, []any{})
					return m, nil
				}
			}
			return nil, nil
		}
		for _, name := range idx.dynFiles[OptionalCatchAllSegment] {
			_, param := parseSegment(name)
			m := &routeMatch{File: join(name) + ".sql"}
			m.capture(param, []any{})
			return m, nil
		}
		return nil, nil
	}

	seg, rest := segs[0], segs[1:]
	segKind, _ := parseSegment(seg)

	// A request for "[id]" is not a request for the file called that.
	if segKind == StaticSegment {
		if verb == "" && len(rest) == 0 && idx.stems[seg] {
			return &routeMatch{File: join(seg) + ".sql"}, nil
		}
		if idx.dirs[seg] {
			if m, err := h.match(ctx, join(seg), rest, verb); m != nil || err != nil {
				return m, err
			}
		}
	}

	if verb == "" && len(rest) == 0 {
		for _, name := range idx.dynFiles[ParamSegment] {
			_, param := parseSegment(name)
			m := &routeMatch{File: join(name) + ".sql"}
			m.capture(param, seg)
			return m, nil
		}
	}
	for _, name := range idx.dynDirs[ParamSegment] {
		m, err := h.match(ctx, join(name), rest, verb)
		if err != nil {
			return nil, err
		}
		if m != nil {
			_, param := parseSegment(name)
			m.capture(param, seg)
			return m, nil
		}
	}

	for _, kind := range []SegmentKind{CatchAllSegment, OptionalCatchAllSegment} {
		if verb == "" {
			for _, name := range idx.dynFiles[kind] {
				_, param := parseSegment(name)
				m := &routeMatch{File: join(name) + ".sql"}
				m.capture(param, anySlice(segs))
				return m, nil
			}
			continue
		}
		for _, name := range idx.dynDirs[kind] {
			sub, err := h.source.List(ctx, join(name))
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
			if indexDir(sub).stems[verb] {
				_, param := parseSegment(name)
				m := &routeMatch{File: leaf(join(name))}
				m.capture(param, anySlice(segs))
				return m, nil
			}
		}
	}
	return nil, nil
}
