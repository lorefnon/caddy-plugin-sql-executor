package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"

	"gopkg.in/yaml.v3"
)

// The subset of OpenAPI 3.1 that sql-openapi writes.

type document struct {
	OpenAPI    string               `json:"openapi" yaml:"openapi"`
	Info       info                 `json:"info" yaml:"info"`
	Servers    []server             `json:"servers,omitempty" yaml:"servers,omitempty"`
	Paths      map[string]*pathItem `json:"paths" yaml:"paths"`
	Components components           `json:"components" yaml:"components"`
}

type info struct {
	Title       string `json:"title" yaml:"title"`
	Version     string `json:"version" yaml:"version"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

type server struct {
	URL         string `json:"url" yaml:"url"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

// pathItem holds a path's operations, one per HTTP method OpenAPI 3.1 has
// a field for.
type pathItem struct {
	Get     *operation `json:"get,omitempty" yaml:"get,omitempty"`
	Put     *operation `json:"put,omitempty" yaml:"put,omitempty"`
	Post    *operation `json:"post,omitempty" yaml:"post,omitempty"`
	Delete  *operation `json:"delete,omitempty" yaml:"delete,omitempty"`
	Options *operation `json:"options,omitempty" yaml:"options,omitempty"`
	Head    *operation `json:"head,omitempty" yaml:"head,omitempty"`
	Patch   *operation `json:"patch,omitempty" yaml:"patch,omitempty"`
	Trace   *operation `json:"trace,omitempty" yaml:"trace,omitempty"`
}

// set stores op as the operation for an upper case HTTP method. It reports
// false if OpenAPI 3.1 has no operation for the method.
func (p *pathItem) set(method string, op *operation) bool {
	slot := map[string]**operation{
		"GET": &p.Get, "PUT": &p.Put, "POST": &p.Post, "DELETE": &p.Delete,
		"OPTIONS": &p.Options, "HEAD": &p.Head, "PATCH": &p.Patch, "TRACE": &p.Trace,
	}[method]
	if slot == nil {
		return false
	}
	*slot = op
	return true
}

type operation struct {
	OperationID string               `json:"operationId" yaml:"operationId"`
	Description string               `json:"description,omitempty" yaml:"description,omitempty"`
	Parameters  []parameter          `json:"parameters,omitempty" yaml:"parameters,omitempty"`
	RequestBody *requestBody         `json:"requestBody,omitempty" yaml:"requestBody,omitempty"`
	Responses   map[string]*response `json:"responses" yaml:"responses"`
}

type parameter struct {
	Name        string  `json:"name" yaml:"name"`
	In          string  `json:"in" yaml:"in"`
	Description string  `json:"description,omitempty" yaml:"description,omitempty"`
	Required    bool    `json:"required,omitempty" yaml:"required,omitempty"`
	Schema      *Schema `json:"schema" yaml:"schema"`
}

type requestBody struct {
	Required bool                 `json:"required,omitempty" yaml:"required,omitempty"`
	Content  map[string]mediaType `json:"content" yaml:"content"`
}

type mediaType struct {
	Schema *Schema `json:"schema" yaml:"schema"`
}

type response struct {
	Ref         string               `json:"$ref,omitempty" yaml:"$ref,omitempty"`
	Description string               `json:"description,omitempty" yaml:"description,omitempty"`
	Content     map[string]mediaType `json:"content,omitempty" yaml:"content,omitempty"`
}

type components struct {
	Schemas   map[string]*Schema   `json:"schemas,omitempty" yaml:"schemas,omitempty"`
	Responses map[string]*response `json:"responses,omitempty" yaml:"responses,omitempty"`
}

// Schema is a JSON Schema. A nil *Schema means the type is unknown, and is
// written as {}.
type Schema struct {
	Ref             string     `json:"$ref,omitempty" yaml:"$ref,omitempty"`
	Type            any        `json:"type,omitempty" yaml:"type,omitempty"` // string or []string
	Format          string     `json:"format,omitempty" yaml:"format,omitempty"`
	ContentEncoding string     `json:"contentEncoding,omitempty" yaml:"contentEncoding,omitempty"`
	Description     string     `json:"description,omitempty" yaml:"description,omitempty"`
	Enum            []any      `json:"enum,omitempty" yaml:"enum,omitempty"`
	Items           *Schema    `json:"items,omitempty" yaml:"items,omitempty"`
	Properties      Properties `json:"properties,omitempty" yaml:"properties,omitempty"`
	Required        []string   `json:"required,omitempty" yaml:"required,omitempty"`
	AnyOf           []*Schema  `json:"anyOf,omitempty" yaml:"anyOf,omitempty"`
}

// Properties are an object's properties, kept in order.
type Properties []Property

type Property struct {
	Name   string
	Schema *Schema
}

func (ps Properties) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, p := range ps {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := json.Marshal(p.Name)
		if err != nil {
			return nil, err
		}
		v, err := json.Marshal(orEmpty(p.Schema))
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func (ps Properties) MarshalYAML() (any, error) {
	n := &yaml.Node{Kind: yaml.MappingNode}
	for _, p := range ps {
		var v yaml.Node
		if err := v.Encode(orEmpty(p.Schema)); err != nil {
			return nil, err
		}
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: p.Name}, &v)
	}
	return n, nil
}

func (ps *Properties) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: properties must be a mapping", n.Line)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		var s Schema
		if err := n.Content[i+1].Decode(&s); err != nil {
			return err
		}
		*ps = append(*ps, Property{Name: n.Content[i].Value, Schema: &s})
	}
	return nil
}

func (ps Properties) get(name string) (*Schema, bool) {
	for _, p := range ps {
		if p.Name == name {
			return p.Schema, true
		}
	}
	return nil, false
}

func orEmpty(s *Schema) *Schema {
	if s == nil {
		return &Schema{}
	}
	return s
}

func typed(t, format string) *Schema { return &Schema{Type: t, Format: format} }

// types returns a schema's type keyword as a list.
func (s *Schema) types() []string {
	switch t := s.Type.(type) {
	case string:
		return []string{t}
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			out = append(out, fmt.Sprint(e))
		}
		return out
	}
	return nil
}

func (s *Schema) setTypes(ts []string) {
	if len(ts) == 1 {
		s.Type = ts[0]
	} else {
		s.Type = ts
	}
}

// nullable returns s, allowing null as well.
func nullable(s *Schema) *Schema {
	if s == nil {
		return nil
	}
	ts := s.types()
	if len(ts) == 0 || slices.Contains(ts, "null") {
		return s
	}
	c := *s
	c.setTypes(append(slices.Clone(ts), "null"))
	if c.Enum != nil {
		c.Enum = append(slices.Clone(c.Enum), nil)
	}
	return &c
}

func equalSchema(a, b *Schema) bool {
	ja, _ := json.Marshal(orEmpty(a))
	jb, _ := json.Marshal(orEmpty(b))
	return bytes.Equal(ja, jb)
}

// union returns a schema accepting what either a or b accepts. A nil
// schema already accepts anything.
func union(a, b *Schema) *Schema {
	if a == nil || b == nil {
		return nil
	}
	if equalSchema(a, b) {
		return a
	}
	ta, tb := a.types(), b.types()
	plain := func(s *Schema) bool { return s.Ref == "" && s.Enum == nil && s.AnyOf == nil }
	if len(ta) > 0 && len(tb) > 0 && plain(a) && plain(b) {
		baseA, baseB := withoutNull(ta), withoutNull(tb)
		switch {
		case len(baseA) == 1 && len(baseB) == 1 && baseA[0] == baseB[0] && a.Format == b.Format:
			// Same type: merge what is inside, e.g. array items.
			c := *a
			c.setTypes(mergeTypes(ta, tb))
			if a.Items != nil || b.Items != nil {
				c.Items = unionItems(a.Items, b.Items)
			}
			c.Properties, c.Required = unionProperties(a, b)
			return &c
		case a.Items == nil && b.Items == nil && a.Properties == nil && b.Properties == nil:
			// Different scalar types: list them all.
			c := &Schema{}
			if a.Format == b.Format {
				c.Format = a.Format
			}
			c.setTypes(mergeTypes(ta, tb))
			return c
		}
	}
	var alts []*Schema
	for _, s := range []*Schema{a, b} {
		if len(s.AnyOf) > 0 && len(s.types()) == 0 {
			alts = append(alts, s.AnyOf...)
		} else {
			alts = append(alts, s)
		}
	}
	out := &Schema{}
	for _, s := range alts {
		if !slices.ContainsFunc(out.AnyOf, func(t *Schema) bool { return equalSchema(s, t) }) {
			out.AnyOf = append(out.AnyOf, s)
		}
	}
	return out
}

// unionItems merges array item schemas, where nil means no items seen.
func unionItems(a, b *Schema) *Schema {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	return union(a, b)
}

func unionProperties(a, b *Schema) (Properties, []string) {
	if a.Properties == nil && b.Properties == nil {
		return nil, nil
	}
	var ps Properties
	for _, p := range a.Properties {
		s := p.Schema
		if bs, ok := b.Properties.get(p.Name); ok {
			s = union(s, bs)
		}
		ps = append(ps, Property{p.Name, s})
	}
	for _, p := range b.Properties {
		if _, ok := a.Properties.get(p.Name); !ok {
			ps = append(ps, p)
		}
	}
	var req []string
	for _, r := range a.Required {
		if slices.Contains(b.Required, r) {
			req = append(req, r)
		}
	}
	return ps, req
}

func withoutNull(ts []string) []string {
	return slices.DeleteFunc(slices.Clone(ts), func(t string) bool { return t == "null" })
}

// mergeTypes lists the types in either list; number subsumes integer.
func mergeTypes(a, b []string) []string {
	var out []string
	for _, t := range append(slices.Clone(a), b...) {
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	if slices.Contains(out, "number") {
		out = slices.DeleteFunc(out, func(t string) bool { return t == "integer" })
	}
	return out
}
