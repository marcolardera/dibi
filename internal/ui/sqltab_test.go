package ui

import "testing"

func TestMSSQLErrorPosition(t *testing.T) {
	batch := "SELECT 1\nSELECT * FORM t\nSELECT 3"
	for _, c := range []struct {
		line int
		msg  string
		want int
	}{
		{2, "Incorrect syntax near 'FORM'.", 18},
		{2, "Invalid object name 't'.", 9},
		{3, "Incorrect syntax near 'missing'.", 25},
		{0, "no line", -1},
		{9, "past the end", -1},
	} {
		if got := mssqlErrorPosition(batch, c.line, c.msg); got != c.want {
			t.Errorf("line %d %q: got %d, want %d", c.line, c.msg, got, c.want)
		}
	}
}
