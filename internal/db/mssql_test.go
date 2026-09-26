package db

import "testing"

func TestMSSQLType(t *testing.T) {
	for _, c := range []struct{ typ, maxLength, precision, scale, want string }{
		{"nvarchar", "200", "0", "0", "nvarchar(100)"},
		{"nvarchar", "-1", "0", "0", "nvarchar(max)"},
		{"varbinary", "-1", "0", "0", "varbinary(max)"},
		{"char", "3", "0", "0", "char(3)"},
		{"decimal", "9", "12", "2", "decimal(12,2)"},
		{"datetime2", "8", "27", "7", "datetime2(7)"},
		{"float", "8", "53", "0", "float"},
		{"float", "4", "24", "0", "float(24)"},
		{"int", "4", "10", "0", "int"},
	} {
		if got := mssqlType(c.typ, c.maxLength, c.precision, c.scale); got != c.want {
			t.Errorf("mssqlType(%s, %s, %s, %s) = %q, want %q", c.typ, c.maxLength, c.precision, c.scale, got, c.want)
		}
	}
}

func TestStripParens(t *testing.T) {
	for in, want := range map[string]string{
		"((0))":          "0",
		"(newid())":      "newid()",
		"('a)')":         "'a)'",
		"((1)+(2))":      "(1)+(2)",
		"(1)+(2)":        "(1)+(2)",
		"(getdate())":    "getdate()",
		"":               "",
		"(N'it''s (x)')": "N'it''s (x)'",
	} {
		if got := stripParens(in); got != want {
			t.Errorf("stripParens(%q) = %q, want %q", in, got, want)
		}
	}
}
