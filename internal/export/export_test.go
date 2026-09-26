package export

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/xuri/excelize/v2"
	_ "modernc.org/sqlite"
)

func testDB(t *testing.T) *sql.DB {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`CREATE TABLE t(id INTEGER, name TEXT, amount REAL, data BLOB);
		INSERT INTO t VALUES (1, 'a,b', 1.5, X'DEAD'), (2, 'say "hi"', NULL, NULL), (3, 'multi
line', 3, NULL)`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestCSV(t *testing.T) {
	db := testDB(t)
	path := filepath.Join(t.TempDir(), "out.csv")
	n, err := Run(context.Background(), db, "SELECT * FROM t ORDER BY id",
		Options{Format: CSV, Path: path, Header: true, Separator: ';', NullText: "NULL"}, nil)
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	data, _ := os.ReadFile(path)
	want := "id;name;amount;data\n1;a,b;1.5;0xDEAD\n2;\"say \"\"hi\"\"\";NULL;NULL\n3;\"multi\nline\";3;NULL\n"
	if string(data) != want {
		t.Errorf("got:\n%s\nwant:\n%s", data, want)
	}
}

func TestXLSX(t *testing.T) {
	db := testDB(t)
	path := filepath.Join(t.TempDir(), "out.xlsx")
	n, err := Run(context.Background(), db, "SELECT id, name, amount FROM t ORDER BY id",
		Options{Format: XLSX, Path: path, Header: true, SheetName: "Result"}, nil)
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	f, err := excelize.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := f.GetRows("Result")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 || rows[0][1] != "name" || rows[1][1] != "a,b" || rows[1][2] != "1.5" {
		t.Fatalf("unexpected rows %q", rows)
	}
	if typ, _ := f.GetCellType("Result", "C2"); typ != excelize.CellTypeNumber && typ != excelize.CellTypeUnset {
		t.Errorf("amount must be a number cell, got %v", typ)
	}
}

func TestFailedExportLeavesNoFile(t *testing.T) {
	db := testDB(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "out.csv")
	if _, err := Run(context.Background(), db, "SELECT * FROM missing", Options{Format: CSV, Path: path}, nil); err == nil {
		t.Fatal("expected an error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	Run(ctx, db, "SELECT * FROM t", Options{Format: CSV, Path: path}, nil)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("leftover files: %v", entries)
	}
}
