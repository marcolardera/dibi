// Spike: validate Microsoft SQL Server support through the pure-Go
// go-mssqldb driver.
//
// Covers: TLS options (self-signed certificate), one pool per database,
// DDL and T-SQL execution, metadata queries for the object tree and the
// info panel, module source from sys.sql_modules, value types (notably
// UNIQUEIDENTIFIER byte order and DATETIMEOFFSET), the TOP 50 preview
// syntax, multiple result sets, paged streaming, query cancellation (the
// session must survive it) and session state across statements.
//
// Expects the SQL Server 2022 container (see README):
//
//	docker run -d --name dibi-mssql -p 1433:1433 -e ACCEPT_EULA=Y \
//	  -e MSSQL_SA_PASSWORD=Dibi_pass1 mcr.microsoft.com/mssql/server:2022-latest
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"time"

	"dibi/spikes/pager"

	mssql "github.com/microsoft/go-mssqldb"
)

func timed(label string, f func() error) {
	t := time.Now()
	if err := f(); err != nil {
		log.Fatalf("%s: %v", label, err)
	}
	fmt.Printf("%-45s %v\n", label, time.Since(t).Round(time.Millisecond))
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func must(db execer, stmts ...string) error {
	for _, s := range stmts {
		if _, err := db.ExecContext(context.Background(), s); err != nil {
			return fmt.Errorf("%w\n  in: %s", err, s)
		}
	}
	return nil
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// printRows prints every row of every result set of a query.
func printRows(db queryer, title, query string, args ...any) {
	rows, err := db.QueryContext(context.Background(), query, args...)
	if err != nil {
		log.Fatalf("%s: %v", title, err)
	}
	defer rows.Close()
	fmt.Printf("\n== %s\n", title)
	for set := 1; ; set++ {
		cols, _ := rows.Columns()
		fmt.Printf("   [set %d] %s\n", set, strings.Join(cols, " | "))
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
				s := fmt.Sprintf("%v", v)
				if b, ok := v.([]byte); ok {
					s = string(b)
				}
				parts[i] = strings.Join(strings.Fields(s), " ")
				if len(parts[i]) > 120 {
					parts[i] = parts[i][:120] + "…"
				}
			}
			fmt.Printf("   %s\n", strings.Join(parts, " | "))
		}
		if !rows.NextResultSet() {
			break
		}
	}
	if err := rows.Err(); err != nil {
		log.Fatalf("%s: %v", title, err)
	}
}

func dsn(database string, params map[string]string) string {
	q := url.Values{}
	q.Set("app name", "dibi")
	if database != "" {
		q.Set("database", database)
	}
	for k, v := range params {
		q.Set(k, v)
	}
	u := &url.URL{Scheme: "sqlserver", User: url.UserPassword("sa", "Dibi_pass1"),
		Host: "localhost:1433", RawQuery: q.Encode()}
	return u.String()
}

func open(database string, params map[string]string) *sql.DB {
	c, err := mssql.NewConnector(dsn(database, params))
	if err != nil {
		log.Fatal(err)
	}
	return sql.OpenDB(c)
}

func main() {
	ctx := context.Background()

	// ---- TLS: SQL Server 2022 in Docker has a self-signed certificate.
	fmt.Println("== TLS options")
	for _, p := range []map[string]string{
		nil,
		{"encrypt": "true"},
		{"encrypt": "true", "trustservercertificate": "true"},
		{"encrypt": "disable"},
		{"encrypt": "strict", "trustservercertificate": "true"},
	} {
		db := open("", p)
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		var enc string
		err := db.QueryRowContext(cctx,
			`SELECT encrypt_option FROM sys.dm_exec_connections WHERE session_id = @@SPID`).Scan(&enc)
		cancel()
		db.Close()
		res := "encrypt_option=" + enc
		if err != nil {
			res = "ERROR: " + firstLine(err.Error())
		}
		fmt.Printf("   %-50s %s\n", fmt.Sprint(p), res)
	}

	master := open("", nil)
	defer master.Close()
	timed("connect + ping", func() error { return master.PingContext(ctx) })
	var version string
	master.QueryRow("SELECT @@VERSION").Scan(&version)
	fmt.Println("  ", firstLine(version))
	printRows(master, "session defaults (driver login settings)",
		`SELECT DB_NAME() AS db, SESSIONPROPERTY('QUOTED_IDENTIFIER') AS quoted_ident,
		        SESSIONPROPERTY('ANSI_NULLS') AS ansi_nulls, SESSIONPROPERTY('ARITHABORT') AS arithabort,
		        @@TEXTSIZE AS text_size, @@LANGUAGE AS lang, @@DATEFIRST AS datefirst`)

	timed("recreate database dibi_spike", func() error {
		return must(master,
			`IF DB_ID('dibi_spike') IS NOT NULL BEGIN
			   ALTER DATABASE dibi_spike SET SINGLE_USER WITH ROLLBACK IMMEDIATE;
			   DROP DATABASE dibi_spike;
			 END`,
			`CREATE DATABASE dibi_spike`)
	})

	// One pool per database, like Postgres: the database is in the DSN.
	db := open("dibi_spike", nil)
	defer db.Close()

	timed("create schema objects", func() error {
		return must(db,
			`CREATE SCHEMA sales`,
			`CREATE TABLE sales.customers (
				id       INT IDENTITY(1,1) CONSTRAINT pk_customers PRIMARY KEY,
				name     NVARCHAR(100) NOT NULL,
				email    VARCHAR(200) CONSTRAINT uq_customers_email UNIQUE,
				notes    NVARCHAR(MAX),
				avatar   VARBINARY(MAX),
				guid     UNIQUEIDENTIFIER NOT NULL CONSTRAINT df_customers_guid DEFAULT NEWID(),
				created  DATETIME2(3) NOT NULL CONSTRAINT df_customers_created DEFAULT SYSUTCDATETIME(),
				CONSTRAINT ck_customers_name CHECK (LEN(name) > 0))`,
			`EXEC sp_addextendedproperty 'MS_Description', N'Customer display name',
				'SCHEMA', 'sales', 'TABLE', 'customers', 'COLUMN', 'name'`,
			`CREATE TABLE sales.orders (
				id          INT NOT NULL PRIMARY KEY,
				customer_id INT NOT NULL REFERENCES sales.customers(id),
				amount      DECIMAL(12,2),
				placed_at   DATETIMEOFFSET(3),
				[order]     NVARCHAR(10) NULL)`,
			`CREATE INDEX orders_customer_ix ON sales.orders(customer_id, placed_at DESC) INCLUDE (amount)`,
			`CREATE UNIQUE INDEX orders_filtered_ix ON sales.orders([order]) WHERE [order] IS NOT NULL`,
			`CREATE SEQUENCE sales.order_seq START WITH 100 INCREMENT BY 5`,
			`CREATE VIEW sales.v_orders AS
				SELECT c.name, o.amount, o.placed_at
				FROM sales.orders o JOIN sales.customers c ON c.id = o.customer_id`,
			`CREATE PROCEDURE sales.add_customer @name NVARCHAR(100), @email VARCHAR(200) = NULL AS
			BEGIN
				SET NOCOUNT ON;
				INSERT INTO sales.customers(name, email) VALUES (@name, @email);
				SELECT SCOPE_IDENTITY() AS new_id;
				SELECT COUNT(*) AS total FROM sales.customers;
			END`,
			`CREATE FUNCTION sales.add_one(@x INT) RETURNS INT AS BEGIN RETURN @x + 1 END`,
			`CREATE FUNCTION sales.big_orders(@min DECIMAL(12,2)) RETURNS TABLE AS
				RETURN SELECT id, amount FROM sales.orders WHERE amount >= @min`,
			`CREATE TRIGGER sales.orders_trg ON sales.orders AFTER INSERT AS
			BEGIN
				SET NOCOUNT ON;
			END`,
		)
	})

	timed("insert data (100k orders, set-based)", func() error {
		return must(db,
			`INSERT INTO sales.customers(name, email, notes, avatar, created)
			 VALUES (N'Ada', NULL, NULL, NULL, '2026-03-01T10:11:12.345'),
			        (N'Linus ☃', 'l@x.org', REPLICATE(CAST(N'x' AS NVARCHAR(MAX)), 5000), 0xDEADBEEF, DEFAULT)`,
			`WITH n AS (SELECT TOP (100000) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) AS i
			            FROM sys.all_objects a CROSS JOIN sys.all_objects b)
			 INSERT INTO sales.orders(id, customer_id, amount, placed_at)
			 SELECT i, i % 2 + 1, CASE WHEN i % 7 = 0 THEN NULL ELSE i * 1.5 END,
			        DATEADD(SECOND, i, CAST('2026-01-01T00:00:00+01:00' AS DATETIMEOFFSET(3)))
			 FROM n`)
	})

	// ---- Metadata for the object tree
	printRows(master, "tree: databases",
		`SELECT name FROM sys.databases WHERE HAS_DBACCESS(name) = 1 AND state_desc = 'ONLINE' ORDER BY name`)
	printRows(db, "tree: schemas (non-system)",
		`SELECT s.name FROM sys.schemas s
		 WHERE s.schema_id < 16384 AND s.name NOT IN ('sys', 'INFORMATION_SCHEMA', 'guest')
		 ORDER BY s.name`)
	printRows(db, "tree: objects by type in schema sales",
		`SELECT o.type, o.type_desc, o.name FROM sys.objects o
		 WHERE o.schema_id = SCHEMA_ID(@p1) AND o.parent_object_id = 0 AND o.is_ms_shipped = 0
		 ORDER BY o.type, o.name`, "sales")
	printRows(db, "tree: indexes and triggers of sales.orders",
		`SELECT 'index' AS kind, name FROM sys.indexes WHERE object_id = OBJECT_ID(@p1) AND type > 0
		 UNION ALL
		 SELECT 'trigger', name FROM sys.triggers WHERE parent_id = OBJECT_ID(@p1)
		 ORDER BY 1, 2`, "sales.orders")

	// ---- Metadata for the info panel
	printRows(db, "info: columns of sales.customers",
		`SELECT c.name, t.name AS type, c.max_length, c.precision, c.scale, c.is_nullable, c.is_identity,
		        dc.definition AS default_def, CAST(ep.value AS NVARCHAR(4000)) AS comment
		 FROM sys.columns c
		 JOIN sys.types t ON t.user_type_id = c.user_type_id
		 LEFT JOIN sys.default_constraints dc ON dc.object_id = c.default_object_id
		 LEFT JOIN sys.extended_properties ep
		   ON ep.class = 1 AND ep.major_id = c.object_id AND ep.minor_id = c.column_id AND ep.name = 'MS_Description'
		 WHERE c.object_id = OBJECT_ID(@p1)
		 ORDER BY c.column_id`, "sales.customers")
	printRows(db, "info: key/unique constraints of sales.customers",
		`SELECT kc.name, kc.type_desc,
		        STRING_AGG(col.name, ', ') WITHIN GROUP (ORDER BY ic.key_ordinal) AS cols
		 FROM sys.key_constraints kc
		 JOIN sys.index_columns ic ON ic.object_id = kc.parent_object_id AND ic.index_id = kc.unique_index_id
		 JOIN sys.columns col ON col.object_id = ic.object_id AND col.column_id = ic.column_id
		 WHERE kc.parent_object_id = OBJECT_ID(@p1)
		 GROUP BY kc.name, kc.type_desc`, "sales.customers")
	printRows(db, "info: check constraints of sales.customers",
		`SELECT name, definition FROM sys.check_constraints WHERE parent_object_id = OBJECT_ID(@p1)`, "sales.customers")
	printRows(db, "info: foreign keys of sales.orders",
		`SELECT fk.name,
		        STRING_AGG(pc.name, ', ') WITHIN GROUP (ORDER BY fkc.constraint_column_id) AS cols,
		        OBJECT_SCHEMA_NAME(fk.referenced_object_id) + '.' + OBJECT_NAME(fk.referenced_object_id) AS ref_table,
		        STRING_AGG(rc.name, ', ') WITHIN GROUP (ORDER BY fkc.constraint_column_id) AS ref_cols,
		        fk.delete_referential_action_desc
		 FROM sys.foreign_keys fk
		 JOIN sys.foreign_key_columns fkc ON fkc.constraint_object_id = fk.object_id
		 JOIN sys.columns pc ON pc.object_id = fkc.parent_object_id AND pc.column_id = fkc.parent_column_id
		 JOIN sys.columns rc ON rc.object_id = fkc.referenced_object_id AND rc.column_id = fkc.referenced_column_id
		 WHERE fk.parent_object_id = OBJECT_ID(@p1)
		 GROUP BY fk.name, fk.referenced_object_id, fk.delete_referential_action_desc`, "sales.orders")
	printRows(db, "info: indexes of sales.orders",
		`SELECT i.name, i.type_desc, i.is_unique, i.is_primary_key, i.filter_definition,
		        STRING_AGG(CASE WHEN ic.is_included_column = 0 THEN c.name + CASE WHEN ic.is_descending_key = 1 THEN ' DESC' ELSE '' END END, ', ')
		          WITHIN GROUP (ORDER BY ic.key_ordinal) AS key_cols,
		        STRING_AGG(CASE WHEN ic.is_included_column = 1 THEN c.name END, ', ') AS included
		 FROM sys.indexes i
		 JOIN sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id
		 JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
		 WHERE i.object_id = OBJECT_ID(@p1) AND i.type > 0
		 GROUP BY i.name, i.type_desc, i.is_unique, i.is_primary_key, i.filter_definition
		 ORDER BY i.name`, "sales.orders")
	printRows(db, "info: sequence sales.order_seq",
		`SELECT TYPE_NAME(user_type_id), start_value, minimum_value, maximum_value, increment, is_cycling,
		        cache_size, current_value
		 FROM sys.sequences WHERE object_id = OBJECT_ID(@p1)`, "sales.order_seq")
	printRows(db, "source: sys.sql_modules (view, procedure, functions, trigger)",
		`SELECT OBJECT_NAME(m.object_id), o.type, m.definition
		 FROM sys.sql_modules m JOIN sys.objects o ON o.object_id = m.object_id
		 WHERE o.schema_id = SCHEMA_ID('sales') ORDER BY 1`)

	// ---- Preview syntax: no LIMIT; OFFSET/FETCH needs ORDER BY.
	printRows(db, "preview: TOP 50",
		`SELECT TOP 50 id, name, email, LEFT(notes, 10) AS notes_head, LEN(notes) AS notes_len FROM sales.customers`)
	_, err := db.Exec(`SELECT * FROM sales.customers OFFSET 0 ROWS FETCH NEXT 50 ROWS ONLY`)
	fmt.Printf("   OFFSET/FETCH without ORDER BY: %v\n", err)

	// ---- Value types scanned into any
	func() {
		rows, err := db.Query(`SELECT TOP 1
			CAST(1 AS TINYINT) AS tinyint_, CAST(2 AS SMALLINT) AS smallint_, 3 AS int_, CAST(4 AS BIGINT) AS bigint_,
			CAST(1 AS BIT) AS bit_, CAST(12.34 AS DECIMAL(10,2)) AS decimal_, CAST(12.34 AS MONEY) AS money_,
			CAST(1.5 AS FLOAT) AS float_, CAST(1.5 AS REAL) AS real_,
			CAST('txt' AS VARCHAR(10)) AS varchar_, N'nvärchar ☃' AS nvarchar_, CAST('c' AS CHAR(3)) AS char_,
			CAST('2026-03-01' AS DATE) AS date_, CAST('10:11:12.1234567' AS TIME) AS time_,
			CAST('2026-03-01T10:11:12.123' AS DATETIME) AS datetime_,
			CAST('2026-03-01T10:11:12.1234567' AS DATETIME2) AS datetime2_,
			CAST('2026-03-01T10:11:00' AS SMALLDATETIME) AS smalldatetime_,
			CAST('2026-03-01T10:11:12.123+05:30' AS DATETIMEOFFSET(3)) AS dto_,
			CAST('6F9619FF-8B86-D011-B42D-00C04FC964FF' AS UNIQUEIDENTIFIER) AS guid_,
			0xDEAD AS varbinary_, CAST(0xBEEF AS BINARY(4)) AS binary_,
			CAST('<a b="1"/>' AS XML) AS xml_, CAST(42 AS SQL_VARIANT) AS variant_,
			geography::Point(45.07, 7.68, 4326) AS geography_, hierarchyid::Parse('/1/2/') AS hierarchyid_,
			CAST(NULL AS INT) AS null_,
			(SELECT TOP 1 CAST(avatar AS IMAGE) FROM sales.customers WHERE avatar IS NOT NULL) AS image_,
			CAST('text' AS TEXT) AS text_, CAST(N'ntext' AS NTEXT) AS ntext_,
			JSON_QUERY('{"a":1}') AS json_`)
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
			if len(s) > 40 {
				s = s[:40] + "…"
			}
			fmt.Printf("   %-15s %-18s %-12T %s\n", c.Name(), c.DatabaseTypeName(), vals[i], s)
		}
		var g mssql.UniqueIdentifier
		if err := g.Scan(vals[18]); err == nil {
			fmt.Printf("   UNIQUEIDENTIFIER via mssql.UniqueIdentifier: %s (expected 6F9619FF-8B86-D011-B42D-00C04FC964FF)\n", g)
		}
	}()

	// ---- Batches, multiple result sets and procedure results
	printRows(db, "batch without semicolons: SELECT 1 SELECT 2", `SELECT 1 AS a SELECT 2 AS b`)
	printRows(db, "EXEC procedure returning two result sets",
		`EXEC sales.add_customer @name = N'Grace', @email = 'g@x.org'`)
	printRows(db, "Query() of an UPDATE (no result set)",
		`UPDATE sales.customers SET email = email WHERE id = 1`)
	func() {
		res, err := db.Exec(`UPDATE sales.orders SET amount = amount WHERE id <= 10; UPDATE sales.orders SET amount = amount WHERE id <= 3`)
		n, _ := res.RowsAffected()
		fmt.Printf("\n== Exec of two UPDATEs in one batch: rows affected = %d, err = %v\n", n, err)
	}()
	// GO is a client-side batch separator; "SELECT 1 GO" would parse GO as an alias.
	_, err = db.Exec("PRINT 1\nGO\nPRINT 2")
	fmt.Printf("   batch containing GO: %v\n", err)
	_, err = db.Exec("SELECT 1 AS a\nCREATE VIEW sales.v2 AS SELECT 1 AS x")
	fmt.Printf("   CREATE VIEW not first in batch: %v\n", err)

	// ---- Session: temp table and explicit transaction across statements
	conn, err := db.Conn(ctx)
	if err != nil {
		log.Fatal(err)
	}
	timed("session: temp table + BEGIN TRAN across calls", func() error {
		return must(conn,
			`CREATE TABLE #scratch (x INT)`,
			`BEGIN TRAN`,
			`INSERT INTO #scratch VALUES (1)`,
			`INSERT INTO sales.customers(name, email) VALUES (N'rolled back', 'rb@x.org')`)
	})
	printRows(conn, "session: @@TRANCOUNT and #scratch", `SELECT @@TRANCOUNT AS trancount, (SELECT COUNT(*) FROM #scratch) AS scratch`)
	timed("session: ROLLBACK", func() error { return must(conn, `ROLLBACK`) })
	printRows(conn, "session: after ROLLBACK",
		`SELECT @@TRANCOUNT AS trancount, (SELECT COUNT(*) FROM sales.customers WHERE name = N'rolled back') AS leftover`)

	// ---- Streaming pages
	var p *pager.Pager
	timed("orders: open cursor + first page", func() error {
		var err error
		if p, err = pager.New(ctx, db, "SELECT * FROM sales.orders ORDER BY id"); err != nil {
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

	// Closing a large result after the first page (the grid's partial results).
	timed("close cursor after 1 page of a 10M-row result", func() error {
		rows, err := conn.QueryContext(ctx,
			`SELECT a.id, b.id FROM sales.orders a CROSS JOIN (SELECT TOP 100 id FROM sales.orders) b`)
		if err != nil {
			return err
		}
		for i := 0; i < 1000 && rows.Next(); i++ {
		}
		return rows.Close()
	})

	// ---- Cancellation on a session: the connection and its state must survive.
	// Use a plain cancel, as the Stop button does: the driver marks the
	// connection bad on any net.Error, and context.DeadlineExceeded is one
	// (see the deadline check below).
	timed("cancel WAITFOR after 500ms (session)", func() error {
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		time.AfterFunc(500*time.Millisecond, cancel)
		_, err := conn.ExecContext(cctx, `WAITFOR DELAY '00:00:10'`)
		if err != nil && (errors.Is(err, context.DeadlineExceeded) || cctx.Err() != nil) {
			fmt.Printf("   cancelled ok: %v\n", err)
			return nil
		}
		return fmt.Errorf("expected cancellation, got err=%v", err)
	})
	timed("cancel CPU-bound query after 500ms (session)", func() error {
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		time.AfterFunc(500*time.Millisecond, cancel)
		var n int64
		err := conn.QueryRowContext(cctx,
			`SELECT COUNT_BIG(*) FROM sales.orders a, sales.orders b WHERE (CAST(a.id AS BIGINT) + b.id) % 997 = 0`).Scan(&n)
		if err != nil && (errors.Is(err, context.DeadlineExceeded) || cctx.Err() != nil) {
			fmt.Printf("   cancelled ok: %v\n", err)
			return nil
		}
		return fmt.Errorf("expected cancellation, got n=%d err=%v", n, err)
	})
	timed("session still usable after cancel", func() error { return conn.PingContext(ctx) })
	printRows(conn, "session state after cancel (#scratch must exist)",
		`SELECT @@SPID AS spid, (SELECT COUNT(*) FROM #scratch) AS scratch`)
	conn.Close()

	// A deadline (rather than a cancel) makes the driver drop the connection.
	func() {
		c, err := db.Conn(ctx)
		if err != nil {
			log.Fatal(err)
		}
		defer c.Close()
		cctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		_, err = c.ExecContext(cctx, `WAITFOR DELAY '00:00:10'`)
		fmt.Printf("\n== deadline instead of cancel: %v; next query: %v\n", err, c.PingContext(ctx))
	}()

	// ---- Error message format shown to the user
	_, err = db.Exec("SELECT * FROM missing_table")
	fmt.Printf("\nerror message: %v\n", err)
	_, err = db.Exec("SELECT 1 +")
	fmt.Printf("syntax error:  %v\n", err)
	var me mssql.Error
	if errors.As(err, &me) {
		fmt.Printf("   mssql.Error: number=%d line=%d proc=%q\n", me.Number, me.LineNo, me.ProcName)
	}

	if os.Getenv("KEEP") == "" {
		db.Close()
		must(master, `ALTER DATABASE dibi_spike SET SINGLE_USER WITH ROLLBACK IMMEDIATE`, `DROP DATABASE dibi_spike`)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
