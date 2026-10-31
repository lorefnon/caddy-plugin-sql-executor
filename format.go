package sqlexecutor

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
)

// format is an output encoding for query results.
type format string

const (
	formatJSON format = "json"
	formatCSV  format = "csv"
	formatYAML format = "yaml"
	formatXLSX format = "xlsx"
	formatHTML format = "html"
)

// allFormats lists every format in the order used for messages and as the
// default when no formats are configured.
var allFormats = []format{formatJSON, formatCSV, formatYAML, formatXLSX, formatHTML}

// formatSet is a set of enabled formats. A nil set enables every format.
type formatSet map[format]bool

func (s formatSet) has(f format) bool { return s == nil || s[f] }

// mediaTypes returns the primary media type of each enabled format.
func (s formatSet) mediaTypes() []string {
	var out []string
	for _, f := range allFormats {
		if s.has(f) {
			out = append(out, f.mediaType())
		}
	}
	return out
}

const xlsxMediaType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"

// mediaType returns the format's media type without parameters.
func (f format) mediaType() string {
	switch f {
	case formatCSV:
		return "text/csv"
	case formatYAML:
		return "application/yaml"
	case formatXLSX:
		return xlsxMediaType
	case formatHTML:
		return "text/html"
	default:
		return "application/json"
	}
}

// contentType returns the Content-Type header value for the format.
func (f format) contentType() string {
	if f == formatXLSX {
		return xlsxMediaType
	}
	return f.mediaType() + "; charset=utf-8"
}

var formatsByExt = map[string]format{
	".json": formatJSON,
	".csv":  formatCSV,
	".yaml": formatYAML,
	".yml":  formatYAML,
	".xlsx": formatXLSX,
	".html": formatHTML,
	".htm":  formatHTML,
}

var formatsByMediaType = map[string]format{
	"application/json":   formatJSON,
	"text/json":          formatJSON,
	"text/csv":           formatCSV,
	"application/csv":    formatCSV,
	"application/yaml":   formatYAML,
	"application/x-yaml": formatYAML,
	"text/yaml":          formatYAML,
	"text/x-yaml":        formatYAML,
	xlsxMediaType:        formatXLSX,
	"text/html":          formatHTML,
}

func parseFormat(s string) (format, bool) {
	f, ok := formatsByExt["."+strings.ToLower(strings.TrimPrefix(s, "."))]
	return f, ok
}

// parseFormats turns configured format names into a set. It returns the
// first name listed too, which becomes the default when json is excluded.
// An empty list enables every format.
func parseFormats(names []string) (set formatSet, first format, err error) {
	if len(names) == 0 {
		return nil, formatJSON, nil
	}
	set = formatSet{}
	for i, n := range names {
		f, ok := parseFormat(n)
		if !ok {
			return nil, "", fmt.Errorf("unknown format %q: use json, csv, yaml, xlsx or html", n)
		}
		if i == 0 {
			first = f
		}
		set[f] = true
	}
	return set, first, nil
}

// resolveRoute maps a request path to the SQL file that backs it and the
// format implied by the path's extension, if any. For example
// "/reports/sales.csv" maps to "reports/sales.sql" with CSV output.
func resolveRoute(urlPath string) (file string, f format, ok bool) {
	route, f, ok := routeOf(urlPath)
	if !ok {
		return "", "", false
	}
	return route + ".sql", f, true
}

// routeOf is like resolveRoute, but returns the route without the .sql
// suffix: "reports/sales" for "/reports/sales.csv".
func routeOf(urlPath string) (route string, f format, ok bool) {
	p := path.Clean("/" + urlPath)
	if p == "/" || strings.HasSuffix(urlPath, "/") {
		return "", "", false
	}
	p = strings.TrimPrefix(p, "/")
	if ext := path.Ext(p); ext != "" {
		if ef, known := formatsByExt[strings.ToLower(ext)]; known {
			f = ef
			p = strings.TrimSuffix(p, ext)
		}
	}
	if p == "" {
		return "", "", false
	}
	return p, f, true
}

// negotiate picks a format from an Accept header, considering only the
// formats in allowed. It returns ok=false when the header only lists media
// types that cannot be produced, and an empty format when any type is
// acceptable.
func negotiate(accept string, allowed formatSet) (f format, ok bool) {
	accept = strings.TrimSpace(accept)
	if accept == "" {
		return "", true
	}
	type candidate struct {
		f        format
		wildcard bool
		q        float64
		order    int
	}
	var cands []candidate
	for i, part := range strings.Split(accept, ",") {
		fields := strings.Split(part, ";")
		mt := strings.ToLower(strings.TrimSpace(fields[0]))
		q := 1.0
		for _, param := range fields[1:] {
			k, v, _ := strings.Cut(strings.TrimSpace(param), "=")
			if strings.EqualFold(strings.TrimSpace(k), "q") {
				if parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
					q = parsed
				}
			}
		}
		if q <= 0 {
			continue
		}
		switch {
		case mt == "*/*" || mt == "application/*" || mt == "text/*":
			// text/* is satisfied by CSV, or HTML when CSV is off;
			// application/* and */* by the default.
			c := candidate{wildcard: true, q: q, order: i}
			if mt == "text/*" {
				switch {
				case allowed.has(formatCSV):
					c = candidate{f: formatCSV, q: q, order: i}
				case allowed.has(formatHTML):
					c = candidate{f: formatHTML, q: q, order: i}
				default:
					continue
				}
			}
			cands = append(cands, c)
		default:
			if mf, known := formatsByMediaType[mt]; known && allowed.has(mf) {
				cands = append(cands, candidate{f: mf, q: q, order: i})
			}
		}
	}
	if len(cands) == 0 {
		return "", false
	}
	sort.SliceStable(cands, func(a, b int) bool {
		if cands[a].q != cands[b].q {
			return cands[a].q > cands[b].q
		}
		// Prefer concrete types over wildcards at equal quality.
		if cands[a].wildcard != cands[b].wildcard {
			return !cands[a].wildcard
		}
		return cands[a].order < cands[b].order
	})
	return cands[0].f, true
}
