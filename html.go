package sqlexecutor

import (
	"bufio"
	"html"
	"strings"
)

// htmlStyle is the page's only styling. The page contains no scripts.
const htmlStyle = `body{font:14px/1.4 system-ui,sans-serif;margin:1.5rem;color:#1f2328;background:#fff}
h1{font-size:1.25rem;margin:0 0 1rem}
details{margin:0 0 1rem}
summary{cursor:pointer;color:#57606a}
pre{background:#f6f8fa;padding:.75rem;overflow:auto;border-radius:6px}
table{border-collapse:collapse}
th,td{border:1px solid #d0d7de;padding:.3rem .6rem;text-align:left;vertical-align:top}
th{background:#f6f8fa}
tbody tr:nth-child(even){background:#fafbfc}
td.null{background:repeating-linear-gradient(45deg,#fff,#fff 4px,#f6f8fa 4px,#f6f8fa 8px)}
@media (prefers-color-scheme:dark){
body{color:#e6edf3;background:#0d1117}
summary{color:#8b949e}
pre,th{background:#161b22}
th,td{border-color:#30363d}
tbody tr:nth-child(even){background:#11161d}
td.null{background:repeating-linear-gradient(45deg,#0d1117,#0d1117 4px,#161b22 4px,#161b22 8px)}
}`

// htmlEncoder writes the result as a standalone HTML page: the executed SQL
// in a collapsed <details> element, then a table. Everything is escaped and
// no script is emitted. Rows stream as they arrive.
type htmlEncoder struct {
	w     *bufio.Writer
	title string
	query string
	cols  []column
}

func (e *htmlEncoder) Begin(cols []column) error {
	e.cols = cols
	title := html.EscapeString(e.title)
	e.w.WriteString("<!doctype html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n")
	e.w.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n")
	e.w.WriteString("<title>" + title + "</title>\n<style>\n" + htmlStyle + "\n</style>\n</head>\n<body>\n")
	e.w.WriteString("<h1>" + title + "</h1>\n")
	e.w.WriteString("<details>\n<summary>SQL query</summary>\n<pre><code>")
	e.w.WriteString(html.EscapeString(strings.TrimSpace(e.query)))
	e.w.WriteString("</code></pre>\n</details>\n")
	e.w.WriteString("<table>\n<thead>\n<tr>")
	for _, c := range cols {
		e.w.WriteString("<th>" + html.EscapeString(c.Name) + "</th>")
	}
	_, err := e.w.WriteString("</tr>\n</thead>\n<tbody>\n")
	return err
}

func (e *htmlEncoder) Row(vals []any) error {
	e.w.WriteString("<tr>")
	for i, v := range vals {
		if v == nil {
			e.w.WriteString(`<td class="null"></td>`)
			continue
		}
		e.w.WriteString("<td>" + html.EscapeString(textValue(e.cols[i], v)) + "</td>")
	}
	_, err := e.w.WriteString("</tr>\n")
	return err
}

func (e *htmlEncoder) End() error {
	if _, err := e.w.WriteString("</tbody>\n</table>\n</body>\n</html>\n"); err != nil {
		return err
	}
	return e.w.Flush()
}

// htmlTitle derives a page title from a query file name: "reports/sales.sql"
// becomes "reports/sales".
func htmlTitle(name string) string {
	name = strings.TrimSuffix(name, jinjaExt)
	return strings.TrimSuffix(name, ".sql")
}
