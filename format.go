package sqlexecutor

import (
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
)

const xlsxMediaType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"

// contentType returns the Content-Type header value for the format.
func (f format) contentType() string {
	switch f {
	case formatCSV:
		return "text/csv; charset=utf-8"
	case formatYAML:
		return "application/yaml; charset=utf-8"
	case formatXLSX:
		return xlsxMediaType
	default:
		return "application/json; charset=utf-8"
	}
}

var formatsByExt = map[string]format{
	".json": formatJSON,
	".csv":  formatCSV,
	".yaml": formatYAML,
	".yml":  formatYAML,
	".xlsx": formatXLSX,
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
}

func parseFormat(s string) (format, bool) {
	f, ok := formatsByExt["."+strings.ToLower(strings.TrimPrefix(s, "."))]
	return f, ok
}

// resolveRoute maps a request path to the SQL file that backs it and the
// format implied by the path's extension, if any. For example
// "/reports/sales.csv" maps to "reports/sales.sql" with CSV output.
func resolveRoute(urlPath string) (file string, f format, ok bool) {
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
	return p + ".sql", f, true
}

// negotiate picks a format from an Accept header. It returns ok=false when
// the header only lists media types that cannot be produced, and an empty
// format when any type is acceptable.
func negotiate(accept string) (f format, ok bool) {
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
			// text/* is satisfied by CSV; application/* and */* by the default.
			c := candidate{wildcard: true, q: q, order: i}
			if mt == "text/*" {
				c = candidate{f: formatCSV, q: q, order: i}
			}
			cands = append(cands, c)
		default:
			if mf, known := formatsByMediaType[mt]; known {
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
