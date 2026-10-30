package main

import "testing"

func TestPGPlan(t *testing.T) {
	// Every table column is NOT NULL.
	att := func(schema, table, column string) (bool, error) { return true, nil }
	cases := []struct {
		name string
		plan string
		want []string // n(ullable), N(ot null) or ? (unknown), per output
	}{
		{
			name: "left join, written as a right join with the sides swapped",
			plan: `{"Node Type": "Hash Join", "Join Type": "Right",
				"Output": ["p.name", "c.label", "COALESCE(c.label, ''::text)", "(p.id + 1)", "(c.id + 1)", "upper(c.label)", "count(*)", "$1", "(p.id / c.id)"],
				"Plans": [
					{"Node Type": "Seq Scan", "Parent Relationship": "Outer", "Schema": "public", "Relation Name": "c", "Alias": "c"},
					{"Node Type": "Hash", "Parent Relationship": "Inner", "Plans": [
						{"Node Type": "Seq Scan", "Parent Relationship": "Outer", "Schema": "public", "Relation Name": "p", "Alias": "p"}]}]}`,
			want: []string{"N", "n", "N", "N", "n", "n", "N", "n", "n"},
		},
		{
			name: "single table, unqualified",
			plan: `{"Node Type": "Seq Scan", "Schema": "public", "Relation Name": "p", "Alias": "p", "Output": ["name", "CASE WHEN (id > 1) THEN name ELSE 'x'::text END", "CASE WHEN (id > 1) THEN name END"]}`,
			want: []string{"N", "N", "n"},
		},
		{
			name: "aggregates without GROUP BY",
			plan: `{"Node Type": "Aggregate", "Strategy": "Plain", "Output": ["max(name)", "count(name)", "array_agg(name)"], "Plans": [
				{"Node Type": "Seq Scan", "Parent Relationship": "Outer", "Schema": "public", "Relation Name": "p", "Alias": "p"}]}`,
			want: []string{"n", "N", "n"},
		},
		{
			name: "grouping sets",
			plan: `{"Node Type": "Aggregate", "Strategy": "Mixed", "Grouping Sets": [{"Group Keys": [["category"]]}], "Output": ["category", "max(name)"], "Plans": [
				{"Node Type": "Seq Scan", "Parent Relationship": "Outer", "Schema": "public", "Relation Name": "p", "Alias": "p"}]}`,
			want: []string{"n", "n"},
		},
		{
			name: "UNION ALL, by member",
			plan: `{"Node Type": "Append", "Plans": [
				{"Node Type": "Seq Scan", "Parent Relationship": "Member", "Schema": "public", "Relation Name": "p", "Alias": "p", "Output": ["p.name", "p.name"]},
				{"Node Type": "Result", "Parent Relationship": "Member", "Output": ["'x'::text", "NULL::text"]}]}`,
			want: []string{"N", "n"},
		},
		{
			name: "UNION: outputs above the Append name only one member",
			plan: `{"Node Type": "Aggregate", "Strategy": "Hashed", "Output": ["p.name"], "Plans": [
				{"Node Type": "Append", "Parent Relationship": "Outer", "Plans": [
					{"Node Type": "Seq Scan", "Parent Relationship": "Member", "Schema": "public", "Relation Name": "p", "Alias": "p", "Output": ["p.name"]},
					{"Node Type": "Seq Scan", "Parent Relationship": "Member", "Schema": "public", "Relation Name": "c", "Alias": "c", "Output": ["c.label"]}]}]}`,
			want: []string{"?"},
		},
		{
			name: "subqueries and CTEs can't be followed",
			plan: `{"Node Type": "CTE Scan", "Alias": "x", "Output": ["x.name", "(SubPlan 1)", "upper(x.name)"]}`,
			want: []string{"?", "?", "n"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := parsePGPlan([]byte(`[{"Plan": ` + c.plan + `}]`))
			if err != nil {
				t.Fatal(err)
			}
			for i, want := range c.want {
				notNull, known, err := p.notNull(i, att)
				if err != nil {
					t.Fatal(err)
				}
				got := "?"
				if known && notNull {
					got = "N"
				} else if known {
					got = "n"
				}
				if got != want {
					t.Errorf("output %d: got %s, want %s", i, got, want)
				}
			}
		})
	}
}
