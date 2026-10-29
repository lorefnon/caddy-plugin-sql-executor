package sqlexecutor

import (
	"bufio"
	"database/sql"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xuri/excelize/v2"
	"gopkg.in/yaml.v3"
)

// columnKind refines how a column's raw driver values are rendered.
type columnKind int

const (
	kindGeneric columnKind = iota
	kindJSON               // json/jsonb: embed as structured data
	kindBinary             // bytea/blob: base64 in text formats
	kindNumeric            // numeric/decimal: drivers return strings
	kindBool               // boolean: SQLite returns 0/1 integers
	kindDate               // date: rendered without a time component
)

const dateLayout = "2006-01-02"

type column struct {
	Name string
	Kind columnKind
}

func columnsFromTypes(types []*sql.ColumnType) []column {
	cols := make([]column, len(types))
	for i, t := range types {
		cols[i] = column{Name: t.Name(), Kind: kindFor(t.DatabaseTypeName())}
	}
	return cols
}

func kindFor(dbType string) columnKind {
	switch strings.ToUpper(dbType) {
	case "JSON", "JSONB":
		return kindJSON
	case "BYTEA", "BLOB":
		return kindBinary
	case "NUMERIC", "DECIMAL":
		return kindNumeric
	case "BOOL", "BOOLEAN":
		return kindBool
	case "DATE":
		return kindDate
	}
	return kindGeneric
}

// normalizeRow converts driver values in place where the declared column
// type says more than the driver's representation does. Times are moved
// to UTC so output does not depend on the server's time zone.
func normalizeRow(cols []column, vals []any) {
	for i, c := range cols {
		switch v := vals[i].(type) {
		case int64:
			if c.Kind == kindBool {
				vals[i] = v != 0
			}
		case time.Time:
			vals[i] = v.UTC()
		}
	}
}

func formatTime(col column, t time.Time) string {
	if col.Kind == kindDate {
		return t.Format(dateLayout)
	}
	return t.Format(time.RFC3339Nano)
}

// encoder streams a result set in a particular format.
type encoder interface {
	Begin(cols []column) error
	Row(vals []any) error
	End() error
}

func newEncoder(f format, w io.Writer, sheetName string) encoder {
	switch f {
	case formatCSV:
		return &csvEncoder{w: csv.NewWriter(w)}
	case formatYAML:
		return &yamlEncoder{w: w}
	case formatXLSX:
		return &xlsxEncoder{w: w, sheet: sheetName}
	default:
		return &jsonEncoder{w: bufio.NewWriter(w)}
	}
}

// textValue renders a value for formats without native types (CSV).
func textValue(col column, v any) string {
	switch v := v.(type) {
	case nil:
		return ""
	case []byte:
		if col.Kind == kindBinary || (col.Kind == kindGeneric && !utf8.Valid(v)) {
			return base64.StdEncoding.EncodeToString(v)
		}
		return string(v)
	case string:
		return v
	case time.Time:
		return formatTime(col, v)
	case bool:
		return strconv.FormatBool(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32)
	default:
		return fmt.Sprint(v)
	}
}

// structuredValue renders a value for JSON and YAML output.
func structuredValue(col column, v any) any {
	switch v := v.(type) {
	case []byte:
		if col.Kind == kindJSON && json.Valid(v) {
			return json.RawMessage(v)
		}
		return textValue(col, v)
	case string:
		if col.Kind == kindJSON && json.Valid([]byte(v)) {
			return json.RawMessage(v)
		}
		return v
	case float64:
		// NaN and ±Inf have no JSON representation.
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return textValue(col, v)
		}
		return v
	case float32:
		return structuredValue(col, float64(v))
	case time.Time:
		return formatTime(col, v)
	default:
		return v
	}
}

type jsonEncoder struct {
	w     *bufio.Writer
	cols  []column
	keys  [][]byte
	count int
}

func (e *jsonEncoder) Begin(cols []column) error {
	e.cols = cols
	e.keys = make([][]byte, len(cols))
	for i, c := range cols {
		k, err := json.Marshal(c.Name)
		if err != nil {
			return err
		}
		e.keys[i] = k
	}
	_, err := e.w.WriteString("[")
	return err
}

// Row writes an object whose keys follow the result's column order.
func (e *jsonEncoder) Row(vals []any) error {
	if e.count > 0 {
		e.w.WriteByte(',')
	}
	e.count++
	e.w.WriteByte('{')
	for i, v := range vals {
		if i > 0 {
			e.w.WriteByte(',')
		}
		e.w.Write(e.keys[i])
		e.w.WriteByte(':')
		b, err := json.Marshal(structuredValue(e.cols[i], v))
		if err != nil {
			return fmt.Errorf("encoding column %q: %w", e.cols[i].Name, err)
		}
		e.w.Write(b)
	}
	return e.w.WriteByte('}')
}

func (e *jsonEncoder) End() error {
	if _, err := e.w.WriteString("]\n"); err != nil {
		return err
	}
	return e.w.Flush()
}

type csvEncoder struct {
	w    *csv.Writer
	cols []column
	buf  []string
}

func (e *csvEncoder) Begin(cols []column) error {
	e.cols = cols
	e.buf = make([]string, len(cols))
	for i, c := range cols {
		e.buf[i] = c.Name
	}
	return e.w.Write(e.buf)
}

func (e *csvEncoder) Row(vals []any) error {
	for i, v := range vals {
		e.buf[i] = textValue(e.cols[i], v)
	}
	return e.w.Write(e.buf)
}

func (e *csvEncoder) End() error {
	e.w.Flush()
	return e.w.Error()
}

// yamlEncoder writes a YAML sequence of mappings, one document in total.
// Rows are emitted as they arrive so large results are not buffered.
type yamlEncoder struct {
	w     io.Writer
	cols  []column
	count int
}

func (e *yamlEncoder) Begin(cols []column) error {
	e.cols = cols
	return nil
}

func (e *yamlEncoder) Row(vals []any) error {
	m := &yaml.Node{Kind: yaml.MappingNode}
	for i, v := range vals {
		val, err := yamlValue(e.cols[i], v)
		if err != nil {
			return fmt.Errorf("encoding column %q: %w", e.cols[i].Name, err)
		}
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: e.cols[i].Name}, val)
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{m}}
	out, err := yaml.Marshal(seq)
	if err != nil {
		return err
	}
	e.count++
	_, err = e.w.Write(out)
	return err
}

func (e *yamlEncoder) End() error {
	if e.count == 0 {
		_, err := io.WriteString(e.w, "[]\n")
		return err
	}
	return nil
}

func yamlValue(col column, v any) (*yaml.Node, error) {
	sv := structuredValue(col, v)
	if raw, ok := sv.(json.RawMessage); ok {
		// JSON is a subset of YAML; decode it so it renders as native YAML.
		var decoded any
		if err := yaml.Unmarshal(raw, &decoded); err != nil {
			return nil, err
		}
		sv = decoded
	}
	n := &yaml.Node{}
	if err := n.Encode(sv); err != nil {
		return nil, err
	}
	return n, nil
}

type xlsxEncoder struct {
	w         io.Writer
	sheet     string
	file      *excelize.File
	sw        *excelize.StreamWriter
	cols      []column
	row       int
	timeStyle int
	dateStyle int
	buf       []any
}

func (e *xlsxEncoder) Begin(cols []column) error {
	e.cols = cols
	e.file = excelize.NewFile()
	name := sanitizeSheetName(e.sheet)
	if err := e.file.SetSheetName("Sheet1", name); err != nil {
		return err
	}
	sw, err := e.file.NewStreamWriter(name)
	if err != nil {
		return err
	}
	e.sw = sw
	if e.timeStyle, err = e.file.NewStyle(&excelize.Style{NumFmt: 22}); err != nil {
		return err
	}
	if e.dateStyle, err = e.file.NewStyle(&excelize.Style{NumFmt: 14}); err != nil {
		return err
	}
	header, err := e.file.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	if err != nil {
		return err
	}
	e.buf = make([]any, len(cols))
	for i, c := range cols {
		e.buf[i] = excelize.Cell{StyleID: header, Value: c.Name}
	}
	e.row = 1
	if err := sw.SetPanes(&excelize.Panes{Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft"}); err != nil {
		return err
	}
	return e.writeRow()
}

func (e *xlsxEncoder) Row(vals []any) error {
	for i, v := range vals {
		e.buf[i] = e.cellValue(e.cols[i], v)
	}
	return e.writeRow()
}

func (e *xlsxEncoder) writeRow() error {
	cell, err := excelize.CoordinatesToCellName(1, e.row)
	if err != nil {
		return err
	}
	e.row++
	return e.sw.SetRow(cell, e.buf)
}

func (e *xlsxEncoder) cellValue(col column, v any) any {
	switch v := v.(type) {
	case nil:
		return nil
	case time.Time:
		if col.Kind == kindDate {
			return excelize.Cell{StyleID: e.dateStyle, Value: v}
		}
		return excelize.Cell{StyleID: e.timeStyle, Value: v}
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return textValue(col, v)
		}
		return v
	case string:
		if col.Kind == kindNumeric {
			if f, err := strconv.ParseFloat(v, 64); err == nil && !math.IsInf(f, 0) && !math.IsNaN(f) {
				return f
			}
		}
		return v
	case []byte:
		return textValue(col, v)
	default:
		return v
	}
}

func (e *xlsxEncoder) End() error {
	defer e.file.Close()
	if err := e.sw.Flush(); err != nil {
		return err
	}
	_, err := e.file.WriteTo(e.w)
	return err
}

// sanitizeSheetName derives a valid worksheet name (max 31 characters, no
// []:*?/\ characters) from a route name.
func sanitizeSheetName(name string) string {
	name = strings.TrimSuffix(name, ".sql")
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`[]:*?/\`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, "'")
	if r := []rune(name); len(r) > 31 {
		name = string(r[:31])
	}
	if name == "" {
		return "Sheet1"
	}
	return name
}
