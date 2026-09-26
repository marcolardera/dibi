package pager

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func openDB(t *testing.T, n int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`CREATE TABLE t(id INTEGER);
		WITH RECURSIVE c(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM c WHERE i < ?)
		INSERT INTO t SELECT i FROM c`, n-1)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestPaging(t *testing.T) {
	db := openDB(t, 2500)
	p, err := New(context.Background(), db, "SELECT id FROM t ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	page, err := p.Page(1)
	if err != nil || len(page) != PageSize || page[0][0].(int64) != 1000 {
		t.Fatalf("page 1: len=%d err=%v", len(page), err)
	}
	if p.Done() || p.Pages() != 2 {
		t.Fatalf("expected 2 pages fetched and cursor open, got %d done=%v", p.Pages(), p.Done())
	}
	last, err := p.Last()
	if err != nil || last != 2 {
		t.Fatalf("last=%d err=%v", last, err)
	}
	page, _ = p.Page(2)
	if len(page) != 500 || page[499][0].(int64) != 2499 {
		t.Fatalf("last page has %d rows", len(page))
	}
	if page, _ := p.Page(3); page != nil {
		t.Fatal("expected nil past the end")
	}
	// Cached pages are still available after the cursor is exhausted.
	if page, _ := p.Page(0); len(page) != PageSize || page[0][0].(int64) != 0 {
		t.Fatal("page 0 not cached")
	}
}

func TestEmptyResult(t *testing.T) {
	db := openDB(t, 1)
	p, err := New(context.Background(), db, "SELECT id FROM t WHERE id < 0")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if page, err := p.Page(0); page != nil || err != nil {
		t.Fatalf("page=%v err=%v", page, err)
	}
	if last, _ := p.Last(); last != 0 || !p.Done() {
		t.Fatalf("last=%d done=%v", last, p.Done())
	}
	if len(p.Cols) != 1 || p.Cols[0].Name() != "id" {
		t.Fatal("columns must be known even for an empty result")
	}
}

func TestCloseReleasesConnection(t *testing.T) {
	db := openDB(t, 5000) // single connection pool
	p, err := New(context.Background(), db, "SELECT id FROM t")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Page(0); err != nil {
		t.Fatal(err)
	}
	p.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil || n != 5000 {
		t.Fatalf("connection not released: n=%d err=%v", n, err)
	}
}
