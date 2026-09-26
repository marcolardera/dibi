package value

import (
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/duckdb/duckdb-go/v2"
)

func TestDisplay(t *testing.T) {
	cest := time.FixedZone("CEST", 2*3600)
	ts := time.Date(2026, 9, 26, 17, 52, 40, 44553000, cest)
	midnight := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		typ  string
		v    any
		want string
		null bool
	}{
		{"TEXT", nil, "NULL", true},
		{"TEXT", "", "", false},
		{"VARCHAR", []byte("txt"), "txt", false},   // MySQL text as bytes
		{"DECIMAL", []byte("1.50"), "1.50", false}, // MySQL decimal as bytes
		{"JSONB", []byte(`{"a": 1}`), `{"a": 1}`, false},
		{"BLOB", []byte{0xde, 0xad, 0xbe}, "<binary 3 B>", false},
		{"BYTEA", make([]byte, 2048), "<binary 2.0 KB>", false},
		{"LongRaw", []byte{1}, "<binary 1 B>", false},
		{"UUID", []byte{0x63, 0x44, 0xd0, 0xf3, 0x1d, 0xbb, 0x43, 0xa4, 0x87, 0x8b, 0x06, 0x66, 0xce, 0x1c, 0xb0, 0xb1},
			"6344d0f3-1dbb-43a4-878b-0666ce1cb0b1", false},
		{"DATE", midnight, "2026-09-26", false},
		{"DATE", ts, "2026-09-26 17:52:40.044553", false}, // Oracle DATE with time
		{"TIMESTAMP", ts, "2026-09-26 17:52:40.044553", false},
		{"TIMESTAMPTZ", ts, "2026-09-26 17:52:40.044553+02:00", false},
		{"TimeStampTZ_DTY", ts.UTC(), "2026-09-26 15:52:40.044553Z", false},
		{"DATETIME", time.Date(2020, 1, 1, 10, 0, 0, 0, time.UTC), "2020-01-01 10:00:00", false},
		{"TIME", time.Date(1, 1, 1, 10, 11, 12, 0, time.UTC), "10:11:12", false},
		{"DOUBLE", 1.5, "1.5", false},
		{"DOUBLE", 1e25, "1e+25", false},
		{"FLOAT8", 123456789.0, "123456789", false},
		{"BOOL", true, "true", false},
		{"HUGEINT", new(big.Int).Lsh(big.NewInt(1), 100), "1267650600228229401496703205376", false},
		{"INTEGER[]", []any{int32(1), int32(2)}, "[1, 2]", false},
		{"STRUCT", map[string]any{"b": "x", "a": int32(1)}, "{'a': 1, 'b': 'x'}", false},
		{"TEXT", "line1\nline2\tx", "line1 line2 x", false},
		{"INTERVAL", duckdb.Interval{Days: 1}, "1 day", false},
		{"INTERVAL", duckdb.Interval{Months: 14, Days: 3, Micros: 3723500000}, "1 year 2 mons 3 days 01:02:03.5", false},
		{"INTERVAL", duckdb.Interval{}, "00:00:00", false},
	}
	for _, c := range cases {
		got, null := NewColumn("c", c.typ).Display(c.v)
		if got != c.want || null != c.null {
			t.Errorf("Display(%s, %#v) = %q,%v; want %q,%v", c.typ, c.v, got, null, c.want, c.null)
		}
	}
}

func TestTruncateAndFullText(t *testing.T) {
	long := strings.Repeat("é", MaxDisplayChars+10)
	col := NewColumn("c", "TEXT")
	d, _ := col.Display(long)
	if []rune(d)[MaxDisplayChars] != '…' || len([]rune(d)) != MaxDisplayChars+1 {
		t.Errorf("unexpected truncation: %d runes", len([]rune(d)))
	}
	if col.Text(long) != long {
		t.Error("Text must not truncate")
	}
	if got := NewColumn("b", "BLOB").Text([]byte{0xde, 0xad}); got != "0xDEAD" {
		t.Errorf("binary text = %q", got)
	}
	if got := col.Text(nil); got != "" {
		t.Errorf("NULL text = %q", got)
	}
}

func TestNumeric(t *testing.T) {
	for typ, want := range map[string]bool{
		"INT4": true, "NUMBER": true, "DECIMAL(10,2)": true, "IBDouble": true, "BIGINT": true,
		"VARCHAR": false, "INTERVAL": false, "POINT": false, "DATE": false, "BINARY_DOUBLE": true,
	} {
		if got := NewColumn("c", typ).Numeric(); got != want {
			t.Errorf("Numeric(%s) = %v", typ, got)
		}
	}
	if NewColumn("c", "BINARY_DOUBLE").binary {
		t.Error("BINARY_DOUBLE is not binary data")
	}
	if f, ok := NewColumn("c", "NUMBER").Number("1.25"); !ok || f != 1.25 {
		t.Errorf("Number(NUMBER '1.25') = %v %v", f, ok)
	}
	if _, ok := NewColumn("c", "VARCHAR").Number("1.25"); ok {
		t.Error("strings in text columns are not numbers")
	}
	if f, ok := NewColumn("c", "DECIMAL").Number([]byte("3.5")); !ok || f != 3.5 {
		t.Errorf("Number(MySQL decimal bytes) = %v %v", f, ok)
	}
}
