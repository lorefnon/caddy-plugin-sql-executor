package main

import (
	"strings"
)

// Column schemas describe the values the handler writes in JSON output. They
// follow pgx's stdlib driver (which returns bool, bytea, date, float, int,
// json and timestamp columns as Go values and everything else as text) and
// the handler's encode.go.

// pgType is a row of pg_type.
type pgType struct {
	Name     string // typname
	Kind     string // typtype: b(ase), d(omain), e(num), ...
	Category string // typcategory
	Base     uint32 // typbasetype, for domains
	Labels   []string
}

func pgColumnSchema(t *pgType) *Schema {
	switch t.Name {
	case "int2", "int4", "oid", "xid", "cid":
		return typed("integer", "int32")
	case "int8":
		return typed("integer", "int64")
	case "float4", "float8":
		return typed("number", "double")
	case "numeric":
		// Exact decimals are returned as strings.
		return typed("string", "decimal")
	case "bool":
		return typed("boolean", "")
	case "date":
		return typed("string", "date")
	case "timestamp", "timestamptz":
		return typed("string", "date-time")
	case "json", "jsonb":
		return nil
	case "bytea":
		return &Schema{Type: "string", ContentEncoding: "base64"}
	case "uuid":
		return typed("string", "uuid")
	}
	return pgTextSchema(t)
}

// pgParamSchema describes the JSON values a request can send for a
// parameter. Objects and arrays are bound as JSON text.
func pgParamSchema(t *pgType) *Schema {
	switch t.Name {
	case "int2", "int4", "int8", "oid":
		return typed("integer", "")
	case "float4", "float8", "numeric":
		return typed("number", "")
	case "bool":
		return typed("boolean", "")
	case "json", "jsonb":
		return nil
	case "date":
		return typed("string", "date")
	case "timestamp", "timestamptz":
		return typed("string", "date-time")
	case "uuid":
		return typed("string", "uuid")
	}
	return pgTextSchema(t)
}

func pgTextSchema(t *pgType) *Schema {
	s := typed("string", "")
	if t.Kind == "e" {
		for _, l := range t.Labels {
			s.Enum = append(s.Enum, l)
		}
	}
	return s
}

// sqliteColumnSchema maps a declared column type. SQLite reports no type
// for expressions, so their schema is unknown.
func sqliteColumnSchema(decltype string) *Schema {
	t := strings.ToUpper(strings.TrimSpace(decltype))
	// Types the driver or the handler treat specially (see kindFor).
	switch t {
	case "":
		return nil
	case "JSON", "JSONB":
		return nil
	case "BOOL", "BOOLEAN":
		return typed("boolean", "")
	case "DATE":
		return typed("string", "date")
	case "DATETIME", "TIMESTAMP":
		return typed("string", "date-time")
	}
	// Otherwise, SQLite's type affinity rules.
	switch {
	case strings.Contains(t, "INT"):
		return typed("integer", "int64")
	case strings.Contains(t, "CHAR"), strings.Contains(t, "CLOB"), strings.Contains(t, "TEXT"):
		return typed("string", "")
	case strings.Contains(t, "BLOB"):
		return &Schema{Type: "string", ContentEncoding: "base64"}
	case strings.Contains(t, "REAL"), strings.Contains(t, "FLOA"), strings.Contains(t, "DOUB"):
		return typed("number", "double")
	}
	return typed("number", "")
}

// valueSchema describes a sample parameter value.
func valueSchema(v any) *Schema {
	switch v := v.(type) {
	case nil:
		return typed("null", "")
	case bool:
		return typed("boolean", "")
	case int, int64:
		return typed("integer", "")
	case float64:
		return typed("number", "")
	case string:
		return typed("string", "")
	case []any:
		s := typed("array", "")
		for _, e := range v {
			s.Items = unionItems(s.Items, valueSchema(e))
		}
		return s
	case map[string]any:
		s := typed("object", "")
		for _, k := range sortedKeys(v) {
			s.Properties = append(s.Properties, Property{k, valueSchema(v[k])})
		}
		return s
	}
	return nil
}
