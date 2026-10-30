package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"gopkg.in/yaml.v3"
)

// config is the sql-openapi configuration file. Every setting except
// queries can also be given as a flag.
type config struct {
	Info    info     `yaml:"info"`
	Servers []server `yaml:"servers"`

	// Root is the query directory. Relative paths in the file are
	// relative to the file.
	Root string `yaml:"root"`
	// Driver is postgres or sqlite.
	Driver string `yaml:"driver"`
	// DSN of a database to describe statements against. $VAR and ${VAR}
	// are expanded.
	DSN string `yaml:"dsn"`
	// Schema is a file of DDL loaded into a throwaway database when no DSN
	// is given.
	Schema string `yaml:"schema"`
	// PostgresVersion is the major version of the PostgreSQL server run for
	// Schema, e.g. "16". Default: the latest.
	PostgresVersion string `yaml:"postgres_version"`
	// JinjaIncludes matches the handler's jinja_includes option.
	JinjaIncludes bool `yaml:"jinja_includes"`

	// Queries holds per-file settings, keyed by file (products/search.sql.j2)
	// or by route (products/search).
	Queries map[string]*queryConfig `yaml:"queries"`
}

type queryConfig struct {
	// Samples are request parameters a template is rendered with. Each is
	// rendered and described, and the results are combined. A template
	// without samples gets unknown request and response schemas.
	Samples []map[string]any `yaml:"samples"`
	// Headers are request headers set while rendering samples.
	Headers map[string]string `yaml:"headers"`
	// Params and Columns refine the inferred schemas of request
	// parameters and result columns, as -- @param and -- @column
	// annotations do, and are applied after them.
	Params  map[string]*Schema `yaml:"params"`
	Columns map[string]*Schema `yaml:"columns"`
}

func loadConfig(path string) (*config, error) {
	cfg := &config{}
	if path == "" {
		return cfg, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	dir := filepath.Dir(path)
	for _, p := range []*string{&cfg.Root, &cfg.Schema} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(dir, *p)
		}
	}
	for _, q := range cfg.Queries {
		if q == nil {
			continue
		}
		for i, s := range q.Samples {
			q.Samples[i] = normalizeSample(s).(map[string]any)
		}
	}
	return cfg, nil
}

// query returns the settings for a file, if any, and marks them used.
func (c *config) query(file, route string, used map[string]bool) *queryConfig {
	for _, key := range []string{file, route} {
		if q, ok := c.Queries[key]; ok {
			used[key] = true
			if q == nil {
				q = &queryConfig{}
			}
			return q
		}
	}
	return &queryConfig{}
}

// unused lists configured queries that matched no file, which are most
// likely typos.
func (c *config) unused(used map[string]bool) []string {
	var out []string
	for key := range c.Queries {
		if !used[key] {
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return out
}

// normalizeSample gives sample values the types request parameters have:
// int64 rather than int, and nil-safe maps.
func normalizeSample(v any) any {
	switch v := v.(type) {
	case int:
		return int64(v)
	case []any:
		for i, e := range v {
			v[i] = normalizeSample(e)
		}
		return v
	case map[string]any:
		if v == nil {
			return map[string]any{}
		}
		for k, e := range v {
			v[k] = normalizeSample(e)
		}
		return v
	}
	return v
}
