// Spike: validate Oracle support through the pure-Go go-ora driver.
//
// Covers: connection without Instant Client, DDL and PL/SQL execution,
// metadata queries for the object tree and the table info panel, source code
// of views and procedures, value types (NUMBER, DATE, TIMESTAMP, CLOB, BLOB,
// NULL), 50-row preview syntax, paged streaming and query cancellation.
//
// Expects the gvenzl/oracle-free container (see ORACLE_URL default below).
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"dibi/spikes/pager"

	go_ora "github.com/sijms/go-ora/v2"
)

func timed(label string, f func() error) {
	t := time.Now()
	if err := f(); err != nil {
		log.Fatalf("%s: %v", label, err)
	}
	fmt.Printf("%-45s %v\n", label, time.Since(t).Round(time.Millisecond))
}

func must(db *sql.DB, stmts ...string) error {
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("%w\n  in: %s", err, s)
		}
	}
	return nil
}

// printRows prints every row of a query, one line per row.
func printRows(db *sql.DB, title, query string, args ...any) {
	rows, err := db.Query(query, args...)
	if err != nil {
		log.Fatalf("%s: %v", title, err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	fmt.Printf("\n== %s\n   %s\n", title, strings.Join(cols, " | "))
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			log.Fatalf("%s: %v", title, err)
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = fmt.Sprintf("%v", v)
		}
		fmt.Printf("   %s\n", strings.Join(parts, " | "))
	}
	if err := rows.Err(); err != nil {
		log.Fatalf("%s: %v", title, err)
	}
}

func main() {
	url := os.Getenv("ORACLE_URL")
	if url == "" {
		url = go_ora.BuildUrl("localhost", 1521, "FREEPDB1", "dibi", "dibi", nil)
	}
	db, err := sql.Open("oracle", url)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	timed("connect + ping", func() error { return db.PingContext(ctx) })
	var banner string
	db.QueryRow("SELECT banner_full FROM v$version").Scan(&banner)
	fmt.Println("  ", strings.ReplaceAll(banner, "\n", " "))

	// Clean up from previous runs; errors are expected the first time.
	for _, s := range []string{
		"DROP VIEW v_orders", "DROP TABLE orders", "DROP TABLE customers", "DROP PROCEDURE add_customer",
	} {
		db.Exec(s)
	}

	timed("create schema objects", func() error {
		return must(db,
			`CREATE TABLE customers (
				id      NUMBER(10) PRIMARY KEY,
				name    VARCHAR2(100) NOT NULL,
				email   VARCHAR2(200) UNIQUE,
				notes   CLOB,
				avatar  BLOB,
				created DATE DEFAULT SYSDATE NOT NULL)`,
			`CREATE TABLE orders (
				id          NUMBER(10) PRIMARY KEY,
				customer_id NUMBER(10) NOT NULL REFERENCES customers(id),
				amount      NUMBER(12,2),
				placed_at   TIMESTAMP(6) WITH TIME ZONE)`,
			`CREATE INDEX orders_customer_ix ON orders(customer_id, placed_at)`,
			`CREATE VIEW v_orders AS
				SELECT c.name, o.amount, o.placed_at
				FROM orders o JOIN customers c ON c.id = o.customer_id`,
			`CREATE PROCEDURE add_customer(p_id NUMBER, p_name VARCHAR2) AS
			BEGIN
				INSERT INTO customers(id, name) VALUES (p_id, p_name);
			END;`,
		)
	})

	timed("insert data (PL/SQL block, 100k orders)", func() error {
		return must(db,
			`BEGIN
				add_customer(1, 'Ada');
				INSERT INTO customers(id, name, email, notes, avatar, created)
				VALUES (2, 'Linus', 'l@x.org', RPAD('clob ', 5000, 'x'), HEXTORAW('DEADBEEF'), DATE '2026-03-01');
				FOR i IN 1..100000 LOOP
					INSERT INTO orders VALUES (i, MOD(i, 2) + 1,
						CASE WHEN MOD(i, 7) = 0 THEN NULL ELSE i * 1.5 END,
						TIMESTAMP '2026-01-01 00:00:00 +01:00' + NUMTODSINTERVAL(i, 'SECOND'));
				END LOOP;
				COMMIT;
			END;`)
	})

	// ---- Metadata for the object tree and info panel
	printRows(db, "tree: objects in current schema",
		`SELECT object_type, object_name, status FROM user_objects
		 WHERE object_type IN ('TABLE','VIEW','PROCEDURE','FUNCTION','PACKAGE','SEQUENCE','INDEX')
		 ORDER BY object_type, object_name`)
	printRows(db, "info: columns of ORDERS",
		`SELECT column_name, data_type, data_length, data_precision, data_scale, nullable, data_default
		 FROM all_tab_columns WHERE owner = USER AND table_name = :1 ORDER BY column_id`, "ORDERS")
	printRows(db, "info: constraints of ORDERS",
		`SELECT c.constraint_name, c.constraint_type, cc.column_name, r.table_name AS ref_table
		 FROM all_constraints c
		 JOIN all_cons_columns cc ON cc.owner = c.owner AND cc.constraint_name = c.constraint_name
		 LEFT JOIN all_constraints r ON r.owner = c.r_owner AND r.constraint_name = c.r_constraint_name
		 WHERE c.owner = USER AND c.table_name = :1 ORDER BY c.constraint_name, cc.position`, "ORDERS")
	printRows(db, "info: indexes of ORDERS",
		`SELECT i.index_name, i.uniqueness, ic.column_name
		 FROM all_indexes i JOIN all_ind_columns ic ON ic.index_owner = i.owner AND ic.index_name = i.index_name
		 WHERE i.owner = USER AND i.table_name = :1 ORDER BY i.index_name, ic.column_position`, "ORDERS")
	// ALL_VIEWS.TEXT is a LONG column: check the driver can read it.
	printRows(db, "source: view V_ORDERS (LONG column)",
		`SELECT text FROM all_views WHERE owner = USER AND view_name = :1`, "V_ORDERS")
	printRows(db, "source: procedure ADD_CUSTOMER",
		`SELECT line, RTRIM(text, CHR(10)) FROM all_source WHERE owner = USER AND name = :1 AND type = 'PROCEDURE' ORDER BY line`, "ADD_CUSTOMER")

	// ---- Value types and the 50-row preview syntax
	printRows(db, "preview: CUSTOMERS (FETCH FIRST 50 ROWS ONLY)",
		`SELECT id, name, email, SUBSTR(notes, 1, 20) AS notes_head, DBMS_LOB.GETLENGTH(notes) AS notes_len, avatar, created
		 FROM customers ORDER BY id FETCH FIRST 50 ROWS ONLY`)
	func() {
		rows, err := db.Query("SELECT * FROM customers WHERE id = 2")
		if err != nil {
			log.Fatal(err)
		}
		defer rows.Close()
		cts, _ := rows.ColumnTypes()
		rows.Next()
		vals := make([]any, len(cts))
		ptrs := make([]any, len(cts))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			log.Fatal(err)
		}
		fmt.Println("\n== Go types scanned into any")
		for i, c := range cts {
			s := fmt.Sprintf("%v", vals[i])
			if len(s) > 30 {
				s = s[:30] + "…"
			}
			fmt.Printf("   %-8s %-10s %-12T %s\n", c.Name(), c.DatabaseTypeName(), vals[i], s)
		}
	}()

	// ---- Streaming pages
	var p *pager.Pager
	timed("orders: open cursor + first page", func() error {
		var err error
		if p, err = pager.New(ctx, db, "SELECT * FROM orders ORDER BY id"); err != nil {
			return err
		}
		first, err := p.Page(0)
		if err == nil {
			fmt.Printf("   row[6] = %v  (amount NULL)\n", first[6])
		}
		return err
	})
	timed("orders: last page (fetch 100k)", func() error {
		n, err := p.Last()
		fmt.Printf("   pages = %d\n", n+1)
		return err
	})

	// ---- Cancellation of a long-running query
	timed("cancel slow query after 500ms", func() error {
		cctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		var n int64
		err := db.QueryRowContext(cctx,
			`SELECT COUNT(*) FROM orders a, orders b WHERE MOD(a.id + b.id, 997) = 0`).Scan(&n)
		if err != nil && (errors.Is(err, context.DeadlineExceeded) || cctx.Err() != nil) {
			fmt.Printf("   cancelled ok: %v\n", err)
			return nil
		}
		return fmt.Errorf("expected cancellation, got n=%d err=%v", n, err)
	})
	timed("connection still usable after cancel", func() error {
		var one int
		return db.QueryRow("SELECT 1 FROM dual").Scan(&one)
	})

	// ---- Error message format shown to the user
	_, err = db.Exec("SELECT * FROM missing_table")
	fmt.Printf("\nerror message: %v\n", err)
}
