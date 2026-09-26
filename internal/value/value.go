// Package value formats database values for display, copy and export.
//
// Drivers return different Go types for the same SQL type (MySQL returns
// most values as []byte, Oracle returns NUMBER as a string, DuckDB returns
// UUID as 16 bytes, SQL Server UNIQUEIDENTIFIER as 16 mixed-endian bytes…), so formatting uses both the Go value and the database
// type name of the column.
package value

import (
	"encoding/hex"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/duckdb/duckdb-go/v2"
)

// MaxDisplayChars is the length after which text is truncated in the grid.
const MaxDisplayChars = 200

// Column holds what is needed to format the values of one result column.
type Column struct {
	Name     string
	Type     string // database type name, as reported by the driver
	binary   bool
	numeric  bool
	uuid     bool
	guid     bool // SQL Server UNIQUEIDENTIFIER
	dateOnly bool // DATE (Oracle DATE may carry a time: shown if not midnight)
	timeOnly bool
	zone     bool // show the UTC offset
}

// NewColumn classifies a column by its database type name.
func NewColumn(name, dbType string) Column {
	t := strings.ToUpper(dbType)
	c := Column{Name: name, Type: dbType}
	switch {
	case strings.Contains(t, "BLOB") || strings.Contains(t, "BINARY") || t == "BYTEA" ||
		strings.Contains(t, "RAW") || t == "GEOMETRY" || t == "GEOGRAPHY" || t == "IMAGE" || t == "HIERARCHYID":
		c.binary = !strings.Contains(t, "DOUBLE") // Oracle BINARY_DOUBLE is a number
	case t == "UUID":
		c.uuid = true
	case t == "UNIQUEIDENTIFIER":
		c.guid = true
	}
	switch {
	case isNumericType(t):
		c.numeric = true
	case t == "DATE":
		c.dateOnly = true
	case strings.HasPrefix(t, "TIME") && !strings.HasPrefix(t, "TIMESTAMP"):
		c.timeOnly = true
	}
	c.zone = strings.Contains(t, "TZ") || strings.Contains(t, "TIME ZONE") || t == "DATETIMEOFFSET"
	return c
}

func isNumericType(t string) bool {
	for _, p := range []string{"INT", "NUMERIC", "DECIMAL", "NUMBER", "FLOAT", "DOUBLE", "REAL",
		"SERIAL", "IBDOUBLE", "IBFLOAT", "MONEY", "HUGEINT", "UHUGEINT"} {
		if strings.Contains(t, p) && !strings.Contains(t, "INTERVAL") && !strings.Contains(t, "POINT") {
			return true
		}
	}
	return false
}

// Numeric reports whether values should be right-aligned.
func (c Column) Numeric() bool { return c.numeric }

// Display returns the text shown in a grid cell: single-line, truncated,
// with "NULL" for null values (isNull is then true) and a placeholder for
// binary data.
func (c Column) Display(v any) (text string, isNull bool) {
	if v == nil {
		return "NULL", true
	}
	if b, ok := v.([]byte); ok && c.binary {
		return "<binary " + humanSize(len(b)) + ">", false
	}
	return truncate(singleLine(c.Text(v))), false
}

// Text returns the full value as text, for copy and CSV export. NULL is "".
// Binary values are rendered as hexadecimal (0x…).
func (c Column) Text(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case []byte:
		switch {
		case c.binary:
			return "0x" + strings.ToUpper(hex.EncodeToString(x))
		case c.uuid && len(x) == 16:
			return formatUUID(x)
		case c.guid && len(x) == 16:
			return formatGUID(x)
		}
		return string(x)
	case string:
		return x
	case time.Time:
		return c.formatTime(x)
	case float64:
		return formatFloat(x, 64)
	case float32:
		return formatFloat(float64(x), 32)
	case bool:
		return strconv.FormatBool(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case *big.Int:
		return x.String()
	case duckdb.Interval:
		return formatInterval(x)
	case fmt.Stringer:
		return x.String()
	}
	return formatComposite(v)
}

// Number returns the value as a float64 when it is numeric, for XLSX export.
func (c Column) Number(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case int32:
		return float64(x), true
	case int16:
		return float64(x), true
	case int8:
		return float64(x), true
	case uint64:
		return float64(x), true
	case uint32:
		return float64(x), true
	case float64:
		return x, true
	case float32:
		return float64(x), true
	}
	if !c.numeric {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(c.Text(v)), 64)
	return f, err == nil
}

func (c Column) formatTime(t time.Time) string {
	midnight := t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 && t.Nanosecond() == 0
	if c.dateOnly && midnight {
		return t.Format("2006-01-02")
	}
	layout := "15:04:05"
	if t.Nanosecond() != 0 {
		layout += ".999999999"
	}
	if !c.timeOnly {
		layout = "2006-01-02 " + layout
	}
	if c.zone {
		layout += "Z07:00"
	}
	return t.Format(layout)
}

func formatFloat(f float64, bits int) string {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return strconv.FormatFloat(f, 'g', -1, bits)
	}
	if a := math.Abs(f); a != 0 && (a >= 1e21 || a < 1e-6) {
		return strconv.FormatFloat(f, 'g', -1, bits)
	}
	return strconv.FormatFloat(f, 'f', -1, bits)
}

// formatInterval renders a DuckDB interval like Postgres does
// (e.g. "1 year 2 mons 3 days 04:05:06").
func formatInterval(iv duckdb.Interval) string {
	var parts []string
	plural := func(n int64, unit string) {
		if n == 0 {
			return
		}
		if n != 1 && n != -1 {
			unit += "s"
		}
		parts = append(parts, fmt.Sprintf("%d %s", n, unit))
	}
	plural(int64(iv.Months/12), "year")
	plural(int64(iv.Months%12), "mon")
	plural(int64(iv.Days), "day")
	if iv.Micros != 0 || len(parts) == 0 {
		d := time.Duration(iv.Micros) * time.Microsecond
		sign := ""
		if d < 0 {
			sign, d = "-", -d
		}
		clock := fmt.Sprintf("%s%02d:%02d:%02d", sign, int64(d.Hours()), int64(d.Minutes())%60, int64(d.Seconds())%60)
		if frac := d % time.Second; frac != 0 {
			clock += strings.TrimRight(fmt.Sprintf(".%06d", frac/time.Microsecond), "0")
		}
		parts = append(parts, clock)
	}
	return strings.Join(parts, " ")
}

func formatUUID(b []byte) string {
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// formatGUID renders a SQL Server UNIQUEIDENTIFIER, whose first three
// groups are stored little-endian, in upper case like SQL Server does.
func formatGUID(b []byte) string {
	g := []byte{b[3], b[2], b[1], b[0], b[5], b[4], b[7], b[6]}
	g = append(g, b[8:]...)
	return strings.ToUpper(formatUUID(g))
}

// formatComposite renders lists and maps (DuckDB LIST/STRUCT/MAP) in a
// compact, SQL-like form.
func formatComposite(v any) string {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		parts := make([]string, rv.Len())
		for i := range parts {
			parts[i] = elem(rv.Index(i).Interface())
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case reflect.Map:
		keys := rv.MapKeys()
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = elem(k.Interface()) + ": " + elem(rv.MapIndex(k).Interface())
		}
		sort.Strings(parts)
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

func elem(v any) string {
	if v == nil {
		return "NULL"
	}
	if s, ok := v.(string); ok {
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return Column{}.Text(v)
}

func singleLine(s string) string {
	if !strings.ContainsAny(s, "\r\n\t") {
		return s
	}
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ").Replace(s)
}

func truncate(s string) string {
	if len(s) <= MaxDisplayChars {
		return s
	}
	r := []rune(s)
	if len(r) <= MaxDisplayChars {
		return s
	}
	return string(r[:MaxDisplayChars]) + "…"
}

func humanSize(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/1024/1024)
	}
}
