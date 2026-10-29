package sqlexecutor

import "testing"

func TestResolveRoute(t *testing.T) {
	tests := []struct {
		path     string
		wantFile string
		wantFmt  format
		wantOK   bool
	}{
		{"/users", "users.sql", "", true},
		{"/reports/sales", "reports/sales.sql", "", true},
		{"/reports/sales.csv", "reports/sales.sql", formatCSV, true},
		{"/reports/sales.JSON", "reports/sales.sql", formatJSON, true},
		{"/reports/sales.yml", "reports/sales.sql", formatYAML, true},
		{"/reports/sales.yaml", "reports/sales.sql", formatYAML, true},
		{"/reports/sales.xlsx", "reports/sales.sql", formatXLSX, true},
		{"/v1.2/users", "v1.2/users.sql", "", true},
		{"/users.sql", "users.sql.sql", "", true},
		{"/a/../b", "b.sql", "", true},
		{"/../../etc/passwd", "etc/passwd.sql", "", true},
		{"/", "", "", false},
		{"", "", "", false},
		{"/reports/", "", "", false},
		{"/.csv", "", "", false},
	}
	for _, tt := range tests {
		file, f, ok := resolveRoute(tt.path)
		if file != tt.wantFile || f != tt.wantFmt || ok != tt.wantOK {
			t.Errorf("resolveRoute(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.path, file, f, ok, tt.wantFile, tt.wantFmt, tt.wantOK)
		}
	}
}

func TestNegotiate(t *testing.T) {
	tests := []struct {
		accept string
		want   format
		wantOK bool
	}{
		{"", "", true},
		{"*/*", "", true},
		{"application/json", formatJSON, true},
		{"text/csv", formatCSV, true},
		{"application/x-yaml", formatYAML, true},
		{"text/yaml", formatYAML, true},
		{xlsxMediaType, formatXLSX, true},
		{"text/html, text/csv;q=0.5", formatCSV, true},
		{"text/csv;q=0.4, application/yaml;q=0.9", formatYAML, true},
		{"*/*, text/csv", formatCSV, true},
		{"*/*;q=0.1, text/csv;q=0.0", "", true},
		{"text/*", formatCSV, true},
		{"TEXT/CSV", formatCSV, true},
		{"text/html", "", false},
		{"application/json;q=0", "", false},
	}
	for _, tt := range tests {
		got, ok := negotiate(tt.accept)
		if got != tt.want || ok != tt.wantOK {
			t.Errorf("negotiate(%q) = (%q, %v), want (%q, %v)", tt.accept, got, ok, tt.want, tt.wantOK)
		}
	}
}

func TestParseFormat(t *testing.T) {
	for in, want := range map[string]format{"json": formatJSON, ".csv": formatCSV, "YML": formatYAML, "xlsx": formatXLSX} {
		if got, ok := parseFormat(in); !ok || got != want {
			t.Errorf("parseFormat(%q) = %q, %v", in, got, ok)
		}
	}
	if _, ok := parseFormat("xml"); ok {
		t.Error("parseFormat(xml) should fail")
	}
}
