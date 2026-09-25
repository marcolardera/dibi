// Spike: stream a large result set page by page from an open cursor,
// using DuckDB (CSV/Parquet) and SQLite through database/sql, and check
// that a running query can be cancelled via context.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"dibi/spikes/pager"

	_ "github.com/duckdb/duckdb-go/v2"
	_ "modernc.org/sqlite"
)

func timed(label string, f func() error) {
	t := time.Now()
	if err := f(); err != nil {
		log.Fatalf("%s: %v", label, err)
	}
	fmt.Printf("%-45s %v\n", label, time.Since(t).Round(time.Millisecond))
}

func main() {
	dir, err := os.MkdirTemp("", "dibi-spike")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	csvPath := filepath.Join(dir, "data.csv")
	parquetPath := filepath.Join(dir, "data.parquet")
	ctx := context.Background()

	duck, err := sql.Open("duckdb", "")
	if err != nil {
		log.Fatal(err)
	}
	defer duck.Close()

	gen := `SELECT i AS id, 'name_' || i AS name,
	               CASE WHEN i % 7 = 0 THEN NULL ELSE i * 1.5 END AS amount,
	               TIMESTAMP '2026-01-01' + INTERVAL (i) SECOND AS created,
	               'x'::BLOB AS payload
	        FROM range(1000000) t(i)`
	timed("duckdb: write 1M rows to CSV", func() error {
		_, err := duck.Exec(fmt.Sprintf("COPY (%s) TO '%s' (HEADER)", gen, csvPath))
		return err
	})
	timed("duckdb: write 1M rows to Parquet", func() error {
		_, err := duck.Exec(fmt.Sprintf("COPY (%s) TO '%s' (FORMAT parquet)", gen, parquetPath))
		return err
	})

	for _, src := range []struct{ label, query string }{
		{"csv", fmt.Sprintf("SELECT * FROM read_csv_auto('%s')", csvPath)},
		{"parquet", fmt.Sprintf("SELECT * FROM read_parquet('%s')", parquetPath)},
	} {
		var p *pager.Pager
		timed("duckdb "+src.label+": open cursor + first page", func() error {
			var err error
			if p, err = pager.New(ctx, duck, src.query); err != nil {
				return err
			}
			_, err = p.Page(0)
			return err
		})
		timed("duckdb "+src.label+": jump to page 10", func() error {
			_, err := p.Page(9)
			return err
		})
		var lastIdx int
		timed("duckdb "+src.label+": last page (fetch all)", func() error {
			var err error
			lastIdx, err = p.Last()
			return err
		})
		if src.label == "parquet" {
			fmt.Printf("  columns:")
			for _, c := range p.Cols {
				fmt.Printf(" %s:%s", c.Name(), c.DatabaseTypeName())
			}
			first, _ := p.Page(0)
			fmt.Printf("\n  pages=%d  row[0]=%#v\n", lastIdx+1, first[0])
			fmt.Printf("  row[7]=%#v (NULL amount)\n", first[7])
		}
	}

	// Cancellation: a deliberately slow cross join, cancelled after 300ms.
	timed("duckdb: cancel slow query after 300ms", func() error {
		cctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel()
		var n int64
		err := duck.QueryRowContext(cctx,
			"SELECT count(*) FROM range(100000000) a, range(1000) b WHERE a.range % 997 = b.range").Scan(&n)
		if errors.Is(err, context.DeadlineExceeded) || (err != nil && cctx.Err() != nil) {
			fmt.Printf("  cancelled ok: %v\n", err)
			return nil
		}
		return fmt.Errorf("expected cancellation, got n=%d err=%v", n, err)
	})

	// SQLite via pure-Go driver, same pager.
	lite, err := sql.Open("sqlite", filepath.Join(dir, "test.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer lite.Close()
	timed("sqlite: create + insert 1M rows", func() error {
		_, err := lite.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, name TEXT, amount REAL);
			WITH RECURSIVE c(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM c WHERE i < 999999)
			INSERT INTO t SELECT i, 'name_' || i, CASE WHEN i % 7 = 0 THEN NULL ELSE i * 1.5 END FROM c`)
		return err
	})
	var p *pager.Pager
	timed("sqlite: open cursor + first page", func() error {
		var err error
		if p, err = pager.New(ctx, lite, "SELECT * FROM t"); err != nil {
			return err
		}
		_, err = p.Page(0)
		return err
	})
	timed("sqlite: last page (fetch all)", func() error {
		_, err := p.Last()
		return err
	})
}
