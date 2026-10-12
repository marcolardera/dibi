package importer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"dibi/internal/config"
	"dibi/internal/db"

	"github.com/xuri/excelize/v2"
)

func TestColumnInference(t *testing.T) {
	tests := []struct {
		vals []string
		want db.NewColumn
	}{
		{[]string{"1", "-2", ""}, db.NewColumn{Kind: db.ColInt}},
		{[]string{"1", "3000000000"}, db.NewColumn{Kind: db.ColBigInt}},
		{[]string{"1", "-12.50", "0.125"}, db.NewColumn{Kind: db.ColDecimal, Precision: 5, Scale: 3}},
		{[]string{"12345678901234567890"}, db.NewColumn{Kind: db.ColDecimal, Precision: 20}},
		{[]string{"1.5", "2e3"}, db.NewColumn{Kind: db.ColFloat}},
		{[]string{"TRUE", "false"}, db.NewColumn{Kind: db.ColBool}},
		{[]string{"2024-01-02", "1999-12-31"}, db.NewColumn{Kind: db.ColDate}},
		{[]string{"2024-01-02", "2024-01-02T10:11", "2024-01-02 10:11:12.123"}, db.NewColumn{Kind: db.ColTimestamp, FracDigits: 3}},
		{[]string{"007", "1"}, db.NewColumn{Kind: db.ColText, MaxChars: 3, MaxBytes: 3, MaxUTF16: 3}},
		{[]string{"1", "true"}, db.NewColumn{Kind: db.ColText, MaxChars: 4, MaxBytes: 4, MaxUTF16: 4}},
		{[]string{"2024-02-30"}, db.NewColumn{Kind: db.ColText, MaxChars: 10, MaxBytes: 10, MaxUTF16: 10}},
		{[]string{"città 😀"}, db.NewColumn{Kind: db.ColText, MaxChars: 7, MaxBytes: 11, MaxUTF16: 8}},
		{[]string{"", ""}, db.NewColumn{Kind: db.ColText}},
		{[]string{"1" + strings.Repeat("0", 38) + ".5"}, db.NewColumn{Kind: db.ColText, MaxChars: 41, MaxBytes: 41, MaxUTF16: 41}},
	}
	for _, tt := range tests {
		var s colStats
		for _, v := range tt.vals {
			s.add(v)
		}
		if got := s.column(); got != tt.want {
			t.Errorf("%q: got %+v, want %+v", tt.vals, got, tt.want)
		}
	}
}

func TestFormatDateKind(t *testing.T) {
	tests := map[string]dateKind{
		"yyyy-mm-dd":          dateOnly,
		"dd/mm/yyyy":          dateOnly,
		"yyyy-mm-dd hh:mm:ss": dateTime,
		"[h]:mm:ss":           timeOnly,
		"hh:mm AM/PM":         timeOnly,
		"0.00":                notDate,
		"#,##0;[Red]-#,##0":   notDate,
		`"day "0`:             notDate,
		"General":             notDate,
		"[$€-407] #,##0.00":   notDate,
		"0.00E+00":            notDate,
	}
	for code, want := range tests {
		if got := formatDateKind(code); got != want {
			t.Errorf("%q: got %d, want %d", code, got, want)
		}
	}
}

func TestNumberText(t *testing.T) {
	tests := []struct {
		raw  string
		kind dateKind
		want string
	}{
		{"12.5", notDate, "12.5"},
		{"0.30000000000000004", notDate, "0.3"},
		{"45292", dateOnly, "2024-01-01"},
		{"45292.5", dateTime, "2024-01-01 12:00:00"},
		{"0.75", timeOnly, "18:00:00"},
	}
	for _, tt := range tests {
		if got := numberText(tt.raw, tt.kind, false); got != tt.want {
			t.Errorf("numberText(%q, %d) = %q, want %q", tt.raw, tt.kind, got, tt.want)
		}
	}
}

func TestColumnNames(t *testing.T) {
	src, _ := db.NewSource(config.Connection{Type: config.Postgres})
	long := strings.Repeat("x", 70)
	got := columnNames([]string{"Id", "", "id", " Name ", long, long}, 7, src)
	want := []string{"id", "column_2", "id_2", "name", long[:63], long[:61] + "_2", "column_7"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFileErrors(t *testing.T) {
	src := memorySource(t, config.SQLite)
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.csv")
	os.WriteFile(path, []byte("a,b\n1,2\n3\n4,5,6\n\"x,7\n"), 0o644)
	_, err := Run(context.Background(), src, mainNode(src), Options{Format: CSV, Path: path, Header: true, Table: "t"}, nil)
	var fe *FileErrors
	if !errors.As(err, &fe) || len(fe.List) != 3 {
		t.Fatalf("got %v, want 3 file errors", err)
	}
	for i, want := range []string{"line 3: 1 values instead of 2", "line 4: 3 values instead of 2", "line 5:"} {
		if !strings.HasPrefix(fe.List[i], want) {
			t.Errorf("error %d = %q, want %q…", i, fe.List[i], want)
		}
	}
	if tableExists(t, src, "t") {
		t.Error("nothing must be created when the file has errors")
	}
}

// testCSV exercises every type, quoting, NULLs and special characters.
const testCSV = "id;name;amount;ratio;active;day;stamp;code;note\n" +
	"1;Ada;12.50;1.5e3;true;2024-01-02;2024-01-02 10:11:12.5;007;\"multi\nline\"\n" +
	"2;\"Tab\there\";-3;NULL;FALSE;NULL;2024-01-03T00:00;010;\"back\\slash \"\"q\"\"\"\n" +
	"3;città 😀;NULL;2E-2;NULL;1999-12-31;NULL;NULL;\n"

var wantCSV = [][]string{
	{"1", "Ada", "12.5", "1500", "1", "2024-01-02", "2024-01-02 10:11:12.5", "007", "multi\nline"},
	{"2", "Tab\there", "-3", "NULL", "0", "NULL", "2024-01-03", "010", `back\slash "q"`},
	{"3", "città 😀", "NULL", "0.02", "NULL", "1999-12-31", "NULL", "NULL", ""},
}

var wantKinds = []db.ColumnKind{db.ColInt, db.ColText, db.ColDecimal, db.ColFloat, db.ColBool,
	db.ColDate, db.ColTimestamp, db.ColText, db.ColText}

func TestImport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.csv")
	os.WriteFile(path, []byte("\xEF\xBB\xBF"+testCSV), 0o644)
	forEachTarget(t, func(t *testing.T, src *db.Source, n db.Node) {
		ctx := context.Background()
		opts := Options{Format: CSV, Path: path, Header: true, Separator: ';', NullText: "NULL", Table: "Dibi_Import_Test"}
		res, err := Run(ctx, src, n, opts, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { dropTable(src, n, res.Table) })
		if res.Rows != 3 {
			t.Errorf("rows = %d, want 3", res.Rows)
		}
		for i, c := range res.Columns {
			if c.Kind != wantKinds[i] {
				t.Errorf("column %s: kind %d, want %d", c.Name, c.Kind, wantKinds[i])
			}
		}
		if want := src.StoredName("id"); res.Columns[0].Name != want {
			t.Errorf("first column named %q, want %q", res.Columns[0].Name, want)
		}
		got := selectAll(t, src, n, res.Table, res.Columns)
		for r, row := range wantCSV {
			for c, want := range row {
				if src.Conn.Type == config.Oracle && want == "" {
					want = "NULL" // Oracle stores empty strings as NULL
				}
				if got[r][c] != want {
					t.Errorf("row %d, column %s: got %q, want %q", r+1, res.Columns[c].Name, got[r][c], want)
				}
			}
		}

		// The table now exists: a second import must fail without touching it.
		if _, err := Run(ctx, src, n, opts, nil); err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Errorf("second import: got %v, want an \"already exists\" error", err)
		}
		if len(selectAll(t, src, n, res.Table, res.Columns)) != 3 {
			t.Error("the existing table was changed")
		}
	})
}

// TestFailedLoad checks that a load failing midway leaves no table behind.
func TestFailedLoad(t *testing.T) {
	forEachTarget(t, func(t *testing.T, src *db.Source, n db.Node) {
		cols := []db.NewColumn{{Name: "a", Kind: db.ColBigInt}, {Name: "b", Kind: db.ColText, MaxChars: 5, MaxBytes: 5, MaxUTF16: 5}}
		boom := errors.New("boom")
		i := 0
		next := func() ([]any, error) {
			if i++; i > 2500 {
				return nil, boom
			}
			return []any{int64(i), "x"}, nil
		}
		table := src.StoredName("dibi_failed_test")
		_, err := src.CreateTable(context.Background(), n, table, cols, next, nil)
		if !errors.Is(err, boom) && (err == nil || !strings.Contains(err.Error(), "boom")) {
			t.Fatalf("got %v, want boom", err)
		}
		qualified := src.QualifiedName(db.Node{Kind: db.KindTable, Database: n.Database, Schema: n.Schema, Object: table})
		if exists(src, n, qualified) {
			dropTable(src, n, qualified)
			t.Fatal("the table was left behind")
		}
	})
}

// TestManyRows loads enough rows to fill several batches and buffers.
func TestManyRows(t *testing.T) {
	var b strings.Builder
	b.WriteString("n,label,price,at\n")
	const rows = 12345
	for i := 1; i <= rows; i++ {
		fmt.Fprintf(&b, "%d,row %d with some text,%d.%02d,2024-01-01 00:00:%02d\n", i, i, i, i%100, i%60)
	}
	path := filepath.Join(t.TempDir(), "many.csv")
	os.WriteFile(path, []byte(b.String()), 0o644)
	forEachTarget(t, func(t *testing.T, src *db.Source, n db.Node) {
		var phases []string
		res, err := Run(context.Background(), src, n, Options{Format: CSV, Path: path, Header: true, Table: "dibi_many_test"},
			func(phase string, n int64) { phases = append(phases, fmt.Sprintf("%s %d", phase, n)) })
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { dropTable(src, n, res.Table) })
		if res.Rows != rows {
			t.Errorf("rows = %d, want %d", res.Rows, rows)
		}
		if len(phases) == 0 || !strings.HasPrefix(phases[0], Reading) || !strings.HasPrefix(phases[len(phases)-1], Importing) {
			t.Errorf("progress: %v", phases)
		}
		var count, sum any
		pool, _ := src.Pool(n.Database)
		if err := pool.QueryRow("SELECT COUNT(*), SUM("+quote(src, res.Columns[0].Name)+") FROM "+res.Table).Scan(&count, &sum); err != nil {
			t.Fatal(err)
		}
		c, s := normalize(count, db.ColBigInt), normalize(sum, db.ColBigInt)
		if c != strconv.Itoa(rows) || s != strconv.Itoa(rows*(rows+1)/2) {
			t.Errorf("count, sum = %s, %s", c, s)
		}
	})
}

func TestXLSX(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.xlsx")
	f := excelize.NewFile()
	f.NewSheet("Data")
	dateStyle, _ := f.NewStyle(&excelize.Style{NumFmt: 14})
	stampFmt := "yyyy-mm-dd hh:mm:ss"
	stampStyle, _ := f.NewStyle(&excelize.Style{CustomNumFmt: &stampFmt})
	rows := [][]any{
		{"id", "name", "price", "day", "stamp", "ok", "code"},
		{1, "Ada", 12.5, time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC), time.Date(2024, 1, 2, 10, 11, 12, 0, time.UTC), true, "007"},
		{2, nil, 0.1 + 0.2, nil, nil, false},
		{},
		{3, "Bob", nil, time.Date(1999, 12, 31, 0, 0, 0, 0, time.UTC)},
	}
	for r, row := range rows {
		for c, v := range row {
			cell, _ := excelize.CoordinatesToCellName(c+1, r+1)
			if v != nil {
				f.SetCellValue("Data", cell, v)
			}
			if c == 3 {
				f.SetCellStyle("Data", cell, cell, dateStyle)
			}
			if c == 4 {
				f.SetCellStyle("Data", cell, cell, stampStyle)
			}
		}
	}
	if err := f.SaveAs(path); err != nil {
		t.Fatal(err)
	}
	src := memorySource(t, config.SQLite)
	n := mainNode(src)
	if _, err := Run(context.Background(), src, n, Options{Format: XLSX, Path: path, Header: true, Table: "x", SheetName: "Nope"}, nil); err == nil ||
		!strings.Contains(err.Error(), "Sheet1, Data") {
		t.Errorf("missing sheet: got %v", err)
	}
	res, err := Run(context.Background(), src, n, Options{Format: XLSX, Path: path, Header: true, Table: "x", SheetName: "Data"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	kinds := []db.ColumnKind{db.ColInt, db.ColText, db.ColDecimal, db.ColDate, db.ColTimestamp, db.ColBool, db.ColText}
	for i, c := range res.Columns {
		if c.Kind != kinds[i] {
			t.Errorf("column %s: kind %d, want %d", c.Name, c.Kind, kinds[i])
		}
	}
	got := selectAll(t, src, n, res.Table, res.Columns)
	want := [][]string{
		{"1", "Ada", "12.5", "2024-01-02", "2024-01-02 10:11:12", "1", "007"},
		{"2", "NULL", "0.3", "NULL", "NULL", "0", "NULL"},
		{"3", "Bob", "NULL", "1999-12-31", "NULL", "NULL", "NULL"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}

	// Without a header, the columns are numbered and the first sheet is used.
	res, err = Run(context.Background(), src, n, Options{Format: XLSX, Path: path, Table: "y"}, nil)
	if err == nil || !strings.Contains(err.Error(), "no data") {
		t.Errorf("empty first sheet: got %v, %v", res, err)
	}
}

// ---------------------------------------------------------------- helpers

func memorySource(t *testing.T, typ config.Type) *db.Source {
	src, err := db.NewSource(config.Connection{Name: "mem", Type: typ, Path: config.MemoryPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(src.Close)
	return src
}

func mainNode(src *db.Source) db.Node {
	if src.Conn.Type == config.DuckDB {
		return db.Node{Kind: db.KindSchema, Database: "memory", Schema: "main"}
	}
	return db.Node{Kind: db.KindDatabase, Database: "main"}
}

// forEachTarget runs f on in-memory SQLite and DuckDB databases, and on
// the Docker servers when DIBI_TEST_SERVERS is set (see the db tests).
func forEachTarget(t *testing.T, f func(t *testing.T, src *db.Source, n db.Node)) {
	for _, typ := range []config.Type{config.SQLite, config.DuckDB} {
		t.Run(string(typ), func(t *testing.T) {
			src := memorySource(t, typ)
			f(t, src, mainNode(src))
		})
	}
	if os.Getenv("DIBI_TEST_SERVERS") == "" {
		return
	}
	servers := []struct {
		conn config.Connection
		node db.Node
	}{
		{config.Connection{Type: config.Postgres, Host: "localhost", Port: 5433, User: "dibi", Password: "dibi", Database: "dibi"},
			db.Node{Kind: db.KindSchema, Database: "dibi", Schema: "public"}},
		{config.Connection{Type: config.MariaDB, Host: "localhost", Port: 3307, User: "root", Password: "dibi", Database: "dibi"},
			db.Node{Kind: db.KindDatabase, Database: "dibi"}},
		{config.Connection{Type: config.MySQL, Host: "localhost", Port: 3308, User: "root", Password: "dibi", Database: "dibi"},
			db.Node{Kind: db.KindDatabase, Database: "dibi"}},
		{config.Connection{Type: config.Oracle, Host: "localhost", Port: 1521, User: "dibi", Password: "dibi", Database: "FREEPDB1"},
			db.Node{Kind: db.KindSchema, Schema: "DIBI"}},
		{config.Connection{Type: config.MSSQL, Host: "localhost", Port: 1433, User: "sa", Password: "Dibi_pass1", Database: "tempdb"},
			db.Node{Kind: db.KindSchema, Database: "tempdb", Schema: "dbo"}},
	}
	for _, s := range servers {
		t.Run(string(s.conn.Type), func(t *testing.T) {
			s.conn.Name = string(s.conn.Type)
			src, err := db.NewSource(s.conn)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(src.Close)
			f(t, src, s.node)
		})
	}
}

func quote(src *db.Source, name string) string {
	switch src.Conn.Type {
	case config.MySQL, config.MariaDB:
		return "`" + name + "`"
	case config.MSSQL:
		return "[" + name + "]"
	}
	return `"` + name + `"`
}

func exists(src *db.Source, n db.Node, qualified string) bool {
	pool, err := src.Pool(n.Database)
	if err != nil {
		return false
	}
	rows, err := pool.Query("SELECT * FROM " + qualified + " WHERE 1 = 0")
	if err != nil {
		return false
	}
	rows.Close()
	return true
}

func tableExists(t *testing.T, src *db.Source, name string) bool {
	return exists(src, mainNode(src), name)
}

func dropTable(src *db.Source, n db.Node, qualified string) {
	if pool, err := src.Pool(n.Database); err == nil {
		pool.Exec("DROP TABLE " + qualified)
	}
}

// selectAll returns the rows of a table ordered by its first column, as
// normalized text: NULL, 1/0 for booleans, dates without a zero time,
// decimals without trailing zeros.
func selectAll(t *testing.T, src *db.Source, n db.Node, qualified string, cols []db.NewColumn) [][]string {
	t.Helper()
	pool, err := src.Pool(n.Database)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query("SELECT * FROM " + qualified + " ORDER BY 1")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out [][]string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		row := make([]string, len(cols))
		for i, v := range vals {
			row[i] = normalize(v, cols[i].Kind)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func normalize(v any, kind db.ColumnKind) string {
	var s string
	switch x := v.(type) {
	case nil:
		return "NULL"
	case bool:
		s = "0"
		if x {
			s = "1"
		}
		return s
	case time.Time:
		if x.Hour() == 0 && x.Minute() == 0 && x.Second() == 0 && x.Nanosecond() == 0 {
			return x.Format(time.DateOnly)
		}
		return x.Format("2006-01-02 15:04:05.999999")
	case []byte:
		s = string(x)
	case float64:
		s = strconv.FormatFloat(x, 'f', -1, 64)
	case fmt.Stringer:
		s = x.String()
	default:
		s = fmt.Sprint(x)
	}
	switch kind {
	case db.ColDecimal, db.ColFloat, db.ColInt, db.ColBigInt:
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return strconv.FormatFloat(f, 'f', -1, 64)
		}
	case db.ColDate, db.ColTimestamp: // SQLite returns its text
		if t, err := time.Parse("2006-01-02 15:04:05.999999999", s); err == nil {
			return normalize(t, kind)
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return normalize(t, kind)
		}
	}
	return s
}

// TestImportIntoTablesGroup imports through the Tables group of a schema
// (or database), which locates the same place.
func TestImportIntoTablesGroup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "g.csv")
	os.WriteFile(path, []byte("a,b\n1,x\n"), 0o644)
	for _, typ := range []config.Type{config.SQLite, config.DuckDB} {
		t.Run(string(typ), func(t *testing.T) {
			src := memorySource(t, typ)
			parent := mainNode(src)
			group := db.Node{Kind: db.KindGroup, Group: db.KindTable, Database: parent.Database, Schema: parent.Schema}
			views := group
			views.Group = db.KindView
			if !src.CanImport(parent) || !src.CanImport(group) || src.CanImport(views) {
				t.Errorf("CanImport: parent %v, tables %v, views %v; want true, true, false",
					src.CanImport(parent), src.CanImport(group), src.CanImport(views))
			}
			if got, want := src.ImportTarget(group), src.ImportTarget(parent); got != want {
				t.Errorf("target of the group %q, of the parent %q", got, want)
			}
			res, err := Run(context.Background(), src, group, Options{Format: CSV, Path: path, Header: true, Table: "g"}, nil)
			if err != nil || res.Rows != 1 {
				t.Fatalf("got %+v, %v", res, err)
			}
			if !exists(src, parent, res.Table) {
				t.Errorf("%s not created in %s", res.Table, src.ImportTarget(parent))
			}
		})
	}
}
