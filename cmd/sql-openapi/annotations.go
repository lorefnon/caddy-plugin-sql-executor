package main

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// annotations are what a query file declares about its endpoint with
// comments such as:
//
//	-- @description Orders placed since a date.
//	-- @column total = [number, "null"]
//	-- @param :since = {type: string, format: date}
//
// Schemas are YAML: a type name, a list of them, or a schema.
type annotations struct {
	// description holds the lines of @description, if any.
	description []string
	columns     map[string]*Schema
	params      map[string]*Schema
}

var (
	annotationStart  = regexp.MustCompile(`^@(header|description|column|param)(\s|$)`)
	annotationKind   = regexp.MustCompile(`^@(description|column|param)(\s|$)`)
	columnAnnotation = regexp.MustCompile(`^@column\s+(?:"([^"]+)"|([^\s"=]+))\s*=\s*(\S.*)$`)
	paramAnnotation  = regexp.MustCompile(`^@param\s+:([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(\S.*)$`)
)

// isDirective reports whether the text of a -- comment is a directive,
// rather than prose for the description.
func isDirective(comment string) bool {
	return annotationStart.MatchString(comment)
}

// parseAnnotations reads the @description, @column and @param directives
// among a compiled query's directives.
func parseAnnotations(directives []string) (*annotations, error) {
	a := &annotations{columns: map[string]*Schema{}, params: map[string]*Schema{}}
	for _, d := range directives {
		var (
			kind, name, value string
			into              map[string]*Schema
		)
		k := annotationKind.FindStringSubmatch(d)
		if k == nil {
			continue
		}
		switch k[1] {
		case "description":
			// One space separates the directive from its text; any more is
			// kept, e.g. to indent Markdown.
			text := strings.TrimPrefix(d, "@description")
			text = strings.TrimPrefix(text, " ")
			a.description = append(a.description, text)
			continue
		case "column":
			m := columnAnnotation.FindStringSubmatch(d)
			if m == nil {
				return nil, fmt.Errorf("malformed directive %q: expected -- @column name = schema", d)
			}
			kind, name, value, into = "column", m[1]+m[2], m[3], a.columns
		case "param":
			m := paramAnnotation.FindStringSubmatch(d)
			if m == nil {
				return nil, fmt.Errorf("malformed directive %q: expected -- @param :name = schema", d)
			}
			kind, name, value, into = "param", ":"+m[1], m[2], a.params
		}
		s, err := parseSchema(value)
		if err != nil {
			return nil, fmt.Errorf("@%s %s: %w", kind, name, err)
		}
		key := strings.TrimPrefix(name, ":")
		if _, dup := into[key]; dup {
			return nil, fmt.Errorf("@%s %s is declared twice", kind, name)
		}
		into[key] = s
	}
	return a, nil
}

// parseSchema reads a schema written as YAML: a type name such as integer,
// a list of them such as [integer, "null"], or a schema such as
// {type: string, format: date}.
func parseSchema(text string) (*Schema, error) {
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(text), &n); err != nil {
		return nil, err
	}
	if len(n.Content) == 0 {
		return nil, fmt.Errorf("missing schema")
	}
	s := &Schema{}
	switch n := n.Content[0]; n.Kind {
	case yaml.ScalarNode, yaml.SequenceNode:
		if err := n.Decode(&s.Type); err != nil {
			return nil, err
		}
	case yaml.MappingNode:
		dec := yaml.NewDecoder(strings.NewReader(text))
		dec.KnownFields(true)
		if err := dec.Decode(s); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("expected a type, a list of types or a schema")
	}
	if err := checkTypes(s); err != nil {
		return nil, err
	}
	return s, nil
}

var jsonTypes = map[string]bool{
	"string": true, "number": true, "integer": true, "boolean": true,
	"array": true, "object": true, "null": true,
}

// checkTypes rejects type names JSON Schema doesn't have, such as int.
func checkTypes(s *Schema) error {
	if s == nil {
		return nil
	}
	var ts []any
	switch t := s.Type.(type) {
	case nil:
	case string:
		ts = []any{t}
	case []any:
		ts = t
	default:
		return fmt.Errorf("type must be a name or a list of names")
	}
	for _, t := range ts {
		name, ok := t.(string)
		if !ok || !jsonTypes[name] {
			return fmt.Errorf("unknown type %v: use string, number, integer, boolean, array, object or null", t)
		}
	}
	for _, sub := range append([]*Schema{s.Items}, s.AnyOf...) {
		if err := checkTypes(sub); err != nil {
			return err
		}
	}
	for _, p := range s.Properties {
		if err := checkTypes(p.Schema); err != nil {
			return err
		}
	}
	return nil
}

// override applies a declared schema to an inferred one. The keys it sets
// replace the inferred ones, and if it says anything about the type, none
// of the inferred type is kept. So {description: ...} documents a column
// without changing its type, and integer makes it a non-null integer.
func override(inferred, declared *Schema) *Schema {
	if declared == nil {
		return inferred
	}
	d := declared
	if d.Ref == "" && d.Type == nil && d.Format == "" && d.ContentEncoding == "" && d.Enum == nil &&
		d.Items == nil && d.Properties == nil && d.Required == nil && d.AnyOf == nil {
		out := *orEmpty(inferred)
		out.Description = declared.Description
		return &out
	}
	out := *declared
	if out.Description == "" && inferred != nil {
		out.Description = inferred.Description
	}
	return &out
}
