package sqlexecutor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// placeholderStyle controls how named parameters are rewritten for a driver.
type placeholderStyle int

const (
	// styleDollar rewrites to $1, $2, ... and reuses the same index for
	// repeated names (PostgreSQL).
	styleDollar placeholderStyle = iota
	// styleQuestion rewrites every occurrence to ? and repeats the
	// argument for each occurrence (SQLite).
	styleQuestion
)

// statement is a SQL query whose :name placeholders have been rewritten
// into driver-specific positional placeholders.
type statement struct {
	SQL string
	// Params lists, in positional order, the parameter name each
	// placeholder is bound to.
	Params []string
	// Headers maps parameter names to the request headers that supply
	// them, as declared by "-- @header :name = Header-Name" comments.
	Headers map[string]string
	// Directives holds the text of each -- comment that starts with @,
	// trimmed, in order. Only @header is acted on here; the others are
	// for tools that read the query, such as cmd/sql-openapi.
	Directives []string
}

var (
	headerDirectiveStart = regexp.MustCompile(`^@header(\s|$)`)
	headerDirective      = regexp.MustCompile("^@header\\s+:([A-Za-z_][A-Za-z0-9_]*)\\s*=\\s*([!#$%&'*+.^_`|~0-9A-Za-z-]+)$")
)

// parseHeaderDirective recognises "@header :name = Header-Name" in the text
// of a -- comment. Comments not starting with @header are ignored.
func parseHeaderDirective(comment string) (name, header string, ok bool, err error) {
	text := strings.TrimSpace(comment)
	if !headerDirectiveStart.MatchString(text) {
		return "", "", false, nil
	}
	m := headerDirective.FindStringSubmatch(text)
	if m == nil {
		return "", "", false, fmt.Errorf("malformed directive %q: expected -- @header :name = Header-Name", text)
	}
	return m[1], m[2], true, nil
}

// compile scans query for :name placeholders, skipping string literals,
// quoted identifiers, comments, dollar-quoted strings and :: casts, and
// rewrites them for the given placeholder style.
func compile(query string, style placeholderStyle) (*statement, error) {
	var (
		out     strings.Builder
		params  []string
		index   = map[string]int{}
		headers map[string]string
		notes   []string
	)
	out.Grow(len(query))

	n := len(query)
	for i := 0; i < n; {
		c := query[i]
		switch {
		case c == '\'':
			// E'...' strings allow backslash escapes in PostgreSQL.
			backslash := i > 0 && (query[i-1] == 'e' || query[i-1] == 'E') && (i < 2 || !isIdentChar(query[i-2]))
			end, err := skipQuoted(query, i, '\'', backslash)
			if err != nil {
				return nil, err
			}
			out.WriteString(query[i:end])
			i = end
		case c == '"' || c == '`':
			end, err := skipQuoted(query, i, c, false)
			if err != nil {
				return nil, err
			}
			out.WriteString(query[i:end])
			i = end
		case c == '-' && i+1 < n && query[i+1] == '-':
			end := strings.IndexByte(query[i:], '\n')
			if end < 0 {
				end = n
			} else {
				end += i
			}
			if text := strings.TrimSpace(query[i+2 : end]); strings.HasPrefix(text, "@") {
				notes = append(notes, text)
			}
			name, header, ok, err := parseHeaderDirective(query[i+2 : end])
			if err != nil {
				return nil, err
			}
			if ok {
				if prev, dup := headers[name]; dup {
					return nil, fmt.Errorf(":%s is mapped to both %s and %s", name, prev, header)
				}
				if headers == nil {
					headers = map[string]string{}
				}
				headers[name] = header
			}
			out.WriteString(query[i:end])
			i = end
		case c == '/' && i+1 < n && query[i+1] == '*':
			end, err := skipBlockComment(query, i)
			if err != nil {
				return nil, err
			}
			out.WriteString(query[i:end])
			i = end
		case c == '$' && (i == 0 || !isIdentChar(query[i-1])):
			if end, ok := skipDollarQuoted(query, i); ok {
				out.WriteString(query[i:end])
				i = end
				continue
			}
			out.WriteByte(c)
			i++
		case c == ':' && i+1 < n && query[i+1] == ':':
			out.WriteString("::")
			i += 2
		case c == ':' && i+1 < n && isIdentStart(query[i+1]):
			j := i + 1
			for j < n && isIdentChar(query[j]) {
				j++
			}
			name := query[i+1 : j]
			switch style {
			case styleDollar:
				pos, seen := index[name]
				if !seen {
					params = append(params, name)
					pos = len(params)
					index[name] = pos
				}
				out.WriteString("$" + strconv.Itoa(pos))
			case styleQuestion:
				params = append(params, name)
				out.WriteByte('?')
			}
			i = j
		default:
			out.WriteByte(c)
			i++
		}
	}
	for name, header := range headers {
		if !slices.Contains(params, name) {
			return nil, fmt.Errorf("@header maps %s to :%s, but the query does not use :%s", header, name, name)
		}
	}
	return &statement{SQL: out.String(), Params: params, Headers: headers, Directives: notes}, nil
}

// headerParams sets the parameters declared by @header directives from the
// request headers, overriding any request parameters of the same name.
func (s *statement) headerParams(params map[string]any, header http.Header) error {
	for name, h := range s.Headers {
		vs := header.Values(h)
		if len(vs) == 0 {
			return fmt.Errorf("missing header %s (for :%s)", h, name)
		}
		params[name] = strings.Join(vs, ", ")
	}
	return nil
}

// bind resolves the statement's parameters against the supplied values.
func (s *statement) bind(values map[string]any) ([]any, error) {
	args := make([]any, len(s.Params))
	var missing []string
	for i, name := range s.Params {
		v, ok := values[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		jv, err := jsonIfStructured(v)
		if err != nil {
			return nil, fmt.Errorf("parameter %q: %w", name, err)
		}
		args[i] = jv
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing parameter(s): %s", strings.Join(dedupe(missing), ", "))
	}
	return args, nil
}

func dedupe(names []string) []string {
	seen := make(map[string]bool, len(names))
	out := names[:0]
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

func skipQuoted(s string, start int, quote byte, backslash bool) (int, error) {
	for i := start + 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if backslash {
				i++
			}
		case quote:
			// A doubled quote is an escaped quote.
			if i+1 < len(s) && s[i+1] == quote {
				i++
				continue
			}
			return i + 1, nil
		}
	}
	return 0, fmt.Errorf("unterminated %c-quoted literal starting at offset %d", quote, start)
}

// skipBlockComment handles nested /* */ comments as PostgreSQL does.
func skipBlockComment(s string, start int) (int, error) {
	depth := 0
	for i := start; i+1 < len(s); i++ {
		switch {
		case s[i] == '/' && s[i+1] == '*':
			depth++
			i++
		case s[i] == '*' && s[i+1] == '/':
			depth--
			i++
			if depth == 0 {
				return i + 1, nil
			}
		}
	}
	return 0, fmt.Errorf("unterminated block comment starting at offset %d", start)
}

// skipDollarQuoted recognises $tag$ ... $tag$ strings. It reports false if
// the text at start is not a dollar-quote opener (e.g. a $1 placeholder).
func skipDollarQuoted(s string, start int) (int, bool) {
	j := start + 1
	for j < len(s) && s[j] != '$' {
		if !isIdentChar(s[j]) || (j == start+1 && !isIdentStart(s[j])) {
			return 0, false
		}
		j++
	}
	if j >= len(s) {
		return 0, false
	}
	tag := s[start : j+1]
	end := strings.Index(s[j+1:], tag)
	if end < 0 {
		return 0, false
	}
	return j + 1 + end + len(tag), true
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isIdentChar(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

// readParams collects parameters from the query string and the request
// body. Body values take precedence over query string values.
func readParams(r *http.Request, maxBody int64) (map[string]any, error) {
	params := map[string]any{}
	for k, v := range r.URL.Query() {
		params[k] = v[len(v)-1]
	}

	if r.Body == nil || r.Body == http.NoBody {
		return params, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("reading request body: %w", err)
	}
	if int64(len(body)) > maxBody {
		return nil, errBodyTooLarge
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return params, nil
	}

	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	// curl -d sends JSON labelled as a form unless told otherwise. A real
	// form body never starts with "{", so treat it as the JSON it is.
	if mediaType == "application/x-www-form-urlencoded" && strings.HasPrefix(trimmed, "{") {
		mediaType = "application/json"
	}
	switch mediaType {
	case "application/x-www-form-urlencoded":
		form, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, fmt.Errorf("invalid form body: %w", err)
		}
		for k, v := range form {
			params[k] = v[len(v)-1]
		}
	case "", "application/json", "text/json":
		dec := json.NewDecoder(strings.NewReader(trimmed))
		dec.UseNumber()
		var obj map[string]any
		if err := dec.Decode(&obj); err != nil || obj == nil {
			if err == nil {
				err = errors.New("got null")
			}
			return nil, fmt.Errorf("request body must be a JSON object: %w", err)
		}
		if dec.More() {
			return nil, errors.New("request body must be a single JSON object")
		}
		for k, v := range obj {
			nv, err := normalizeJSON(v)
			if err != nil {
				return nil, fmt.Errorf("parameter %q: %w", k, err)
			}
			params[k] = nv
		}
	default:
		return nil, fmt.Errorf("unsupported request content type %q", mediaType)
	}
	return params, nil
}

// normalizeJSON converts decoded JSON into values database drivers and
// templates handle well: numbers become int64 or float64, recursively.
// Arrays and objects stay structured so templates can iterate them; they
// are bound to queries as JSON text (see jsonIfStructured).
func normalizeJSON(v any) (any, error) {
	switch v := v.(type) {
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return i, nil
		}
		f, err := strconv.ParseFloat(v.String(), 64)
		if err != nil {
			return nil, fmt.Errorf("invalid number %s", v)
		}
		return f, nil
	case []any:
		for i, e := range v {
			ne, err := normalizeJSON(e)
			if err != nil {
				return nil, err
			}
			v[i] = ne
		}
		return v, nil
	case map[string]any:
		for k, e := range v {
			ne, err := normalizeJSON(e)
			if err != nil {
				return nil, err
			}
			v[k] = ne
		}
		return v, nil
	default:
		return v, nil
	}
}
