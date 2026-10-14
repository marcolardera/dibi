package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dibi/internal/config"
	"dibi/internal/pager"
	"dibi/internal/sqlsplit"
)

// fixture describes a test database: how to connect, the setup script and
// the names expected in the tree.
type fixture struct {
	conn   config.Connection
	setup  string
	path   []string // names from the root down to the group containing the tables
	upper  bool     // Oracle stores unquoted names in upper case
	routes map[Kind]string
}

func (f fixture) name(s string) string {
	if f.upper {
		return strings.ToUpper(s)
	}
	return s
}

const commonTables = `
CREATE TABLE customers (
	id    INTEGER PRIMARY KEY,
	name  VARCHAR(100) NOT NULL,
	email VARCHAR(200) UNIQUE
);
CREATE TABLE orders (
	id          INTEGER PRIMARY KEY,
	customer_id INTEGER NOT NULL,
	amount      DECIMAL(12,2) CHECK (amount >= 0),
	FOREIGN KEY (customer_id) REFERENCES customers(id)
);
CREATE INDEX orders_customer_ix ON orders(customer_id);
CREATE VIEW v_orders AS SELECT c.name, o.amount FROM orders o JOIN customers c ON c.id = o.customer_id;
INSERT INTO customers VALUES (1, 'Ada', 'ada@example.com');
INSERT INTO customers VALUES (2, 'Linus', NULL);
INSERT INTO orders VALUES (10, 1, 12.5);
`

func serverFixtures(t *testing.T) []fixture {
	if os.Getenv("DIBI_TEST_SERVERS") == "" {
		return nil
	}
	return []fixture{
		{
			conn: config.Connection{Name: "pg", Type: config.Postgres, Host: "localhost", Port: 5433,
				User: "dibi", Password: "dibi", Database: "dibi"},
			setup: `DROP SCHEMA IF EXISTS dibi_test CASCADE;
CREATE SCHEMA dibi_test;
SET search_path TO dibi_test;` + commonTables + `
CREATE SEQUENCE order_seq START 100;
CREATE FUNCTION add_one(x integer) RETURNS integer AS $$ BEGIN RETURN x + 1; END; $$ LANGUAGE plpgsql;
CREATE PROCEDURE touch() LANGUAGE sql AS $$ SELECT 1; $$;
CREATE FUNCTION trg_fn() RETURNS trigger AS $$ BEGIN RETURN NEW; END; $$ LANGUAGE plpgsql;
CREATE TRIGGER orders_trg BEFORE INSERT ON orders FOR EACH ROW EXECUTE FUNCTION trg_fn();`,
			path:   []string{"dibi", "dibi_test", "Tables"},
			routes: map[Kind]string{KindFunction: "add_one(x integer)", KindProcedure: "touch()", KindSequence: "order_seq"},
		},
		mysqlFixture("mariadb", config.MariaDB, 3307),
		mysqlFixture("mysql", config.MySQL, 3308),
		{
			conn: config.Connection{Name: "oracle", Type: config.Oracle, Host: "localhost", Port: 1521,
				User: "dibi", Password: "dibi", Database: "FREEPDB1"},
			setup: `BEGIN
  FOR o IN (SELECT object_name, object_type FROM user_objects
            WHERE object_type IN ('TABLE','VIEW','SEQUENCE','PROCEDURE','FUNCTION','PACKAGE')) LOOP
    EXECUTE IMMEDIATE 'DROP ' || o.object_type || ' "' || o.object_name || '"' ||
      CASE WHEN o.object_type = 'TABLE' THEN ' CASCADE CONSTRAINTS PURGE' END;
  END LOOP;
END;
/
` + strings.NewReplacer("VARCHAR(", "VARCHAR2(").Replace(commonTables) + `
CREATE SEQUENCE order_seq START WITH 100;
CREATE FUNCTION add_one(x NUMBER) RETURN NUMBER AS
BEGIN
  RETURN x + 1;
END;
CREATE PROCEDURE touch AS
  v NUMBER;
BEGIN
  v := 1;
END;
CREATE PACKAGE pkg AS
  PROCEDURE p;
END pkg;
/
CREATE PACKAGE BODY pkg AS
  PROCEDURE p IS BEGIN NULL; END;
END pkg;
/
CREATE TRIGGER orders_trg BEFORE INSERT ON orders FOR EACH ROW
BEGIN
  :NEW.amount := NVL(:NEW.amount, 0);
END;`,
			path:   []string{"DIBI", "Tables"},
			upper:  true,
			routes: map[Kind]string{KindFunction: "ADD_ONE", KindProcedure: "TOUCH", KindPackage: "PKG", KindSequence: "ORDER_SEQ"},
		},
		{
			conn: config.Connection{Name: "mssql", Type: config.MSSQL, Host: "localhost", Port: 1433,
				User: "sa", Password: "Dibi_pass1"},
			setup: `IF DB_ID('dibi_test') IS NOT NULL BEGIN
  ALTER DATABASE dibi_test SET SINGLE_USER WITH ROLLBACK IMMEDIATE;
  DROP DATABASE dibi_test;
END
GO
CREATE DATABASE dibi_test
GO
USE dibi_test
GO
` + strings.NewReplacer("CREATE VIEW", "GO\nCREATE VIEW", "INSERT INTO customers VALUES (1", "GO\nINSERT INTO customers VALUES (1").Replace(commonTables) + `
GO
CREATE SEQUENCE order_seq START WITH 100
GO
CREATE FUNCTION add_one(@x INT) RETURNS INT AS BEGIN RETURN @x + 1 END
GO
CREATE PROCEDURE touch AS
BEGIN
  DECLARE @v INT;
  SET @v = 1;
  IF @v = 1 SELECT 1;
END
GO
CREATE TRIGGER orders_trg ON orders AFTER INSERT AS
BEGIN
  SET NOCOUNT ON;
END`,
			path:   []string{"dibi_test", "dbo", "Tables"},
			routes: map[Kind]string{KindFunction: "add_one", KindProcedure: "touch", KindSequence: "order_seq"},
		},
	}
}

func mysqlFixture(name string, typ config.Type, port int) fixture {
	return fixture{
		conn: config.Connection{Name: name, Type: typ, Host: "localhost", Port: port,
			User: "root", Password: "dibi", Database: "dibi"},
		setup: `DROP DATABASE IF EXISTS dibi_test;
CREATE DATABASE dibi_test;
USE dibi_test;` + commonTables + `
CREATE FUNCTION add_one(x INT) RETURNS INT DETERMINISTIC RETURN x + 1;
CREATE PROCEDURE touch()
BEGIN
  DECLARE v INT;
  SET v = 1;
  IF v = 1 THEN SELECT 1; END IF;
END;
CREATE TRIGGER orders_trg BEFORE INSERT ON orders FOR EACH ROW SET NEW.amount = COALESCE(NEW.amount, 0);`,
		path:   []string{"dibi_test", "Tables"},
		routes: map[Kind]string{KindFunction: "add_one", KindProcedure: "touch"},
	}
}

func fileFixtures(t *testing.T) []fixture {
	dir := t.TempDir()
	lite := filepath.Join(dir, "test.db")
	duck := filepath.Join(dir, "test.duckdb")
	// Create the database files (the driver refuses to create them on open).
	for _, x := range []struct{ driver, path string }{{"sqlite", lite}, {"duckdb", duck}} {
		db, err := sql.Open(x.driver, x.path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("CREATE TABLE _init(x INTEGER); DROP TABLE _init;"); err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
	return []fixture{
		{
			conn: config.Connection{Name: "sqlite", Type: config.SQLite, Path: lite},
			setup: commonTables + `
CREATE TRIGGER orders_trg AFTER INSERT ON orders BEGIN
  UPDATE customers SET name = name WHERE id = NEW.customer_id;
END;`,
			path: []string{"main", "Tables"},
		},
		{
			conn:  config.Connection{Name: "duckdb", Type: config.DuckDB, Path: duck},
			setup: commonTables,
			path:  []string{"test", "main", "Tables"},
		},
	}
}

func TestSources(t *testing.T) {
	fixtures := append(fileFixtures(t), serverFixtures(t)...)
	for _, f := range fixtures {
		t.Run(f.conn.Name, func(t *testing.T) { testSource(t, f) })
	}
}

func testSource(t *testing.T, f fixture) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	src, err := NewSource(f.conn)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if err := src.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	// Run the setup script through the splitter, on a session.
	sess, err := src.NewSession(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range sqlsplit.Split(f.setup, src.SplitDialect()) {
		if _, err := sess.ExecContext(ctx, st.Text); err != nil {
			t.Fatalf("setup: %v\n  in: %s", err, st.Text)
		}
	}
	sess.Close()

	// Walk down to the tables group.
	node := src.Root()
	for _, name := range f.path {
		node = child(t, ctx, src, node, name)
	}
	tablesGroup := node
	table := child(t, ctx, src, tablesGroup, f.name("orders"))

	info, err := src.Info(ctx, table)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: %d columns, constraints %+v, indexes %+v", info.Title, len(info.Columns), info.Constraints, info.Indexes)
	if len(info.Columns) != 3 {
		t.Fatalf("expected 3 columns, got %+v", info.Columns)
	}
	id, cust := info.Columns[0], info.Columns[1]
	if !id.PrimaryKey || id.Nullable || cust.Nullable || cust.PrimaryKey || !info.Columns[2].Nullable {
		t.Errorf("unexpected column flags: %+v", info.Columns)
	}
	want := map[string]bool{"PRIMARY KEY": false, "FOREIGN KEY": false, "CHECK": false}
	if f.conn.Type == config.SQLite {
		delete(want, "CHECK") // not exposed by SQLite pragmas
	}
	for _, c := range info.Constraints {
		want[c.Type] = true
		if c.Type == "FOREIGN KEY" && (!strings.Contains(strings.ToLower(c.Definition), "customers") ||
			!strings.EqualFold(c.Columns, "customer_id")) {
			t.Errorf("unexpected foreign key %+v", c)
		}
	}
	for k, ok := range want {
		if !ok {
			t.Errorf("missing %s constraint in %+v", k, info.Constraints)
		}
	}
	foundIdx := false
	for _, i := range info.Indexes {
		if strings.EqualFold(i.Name, "orders_customer_ix") && strings.EqualFold(i.Columns, "customer_id") && !i.Unique {
			foundIdx = true
		}
	}
	if !foundIdx {
		t.Errorf("index orders_customer_ix not found in %+v", info.Indexes)
	}

	// Preview.
	pool, _ := src.Pool(table.Database)
	var n int
	rows, err := pool.QueryContext(ctx, info.Preview)
	if err != nil {
		t.Fatalf("preview %q: %v", info.Preview, err)
	}
	for rows.Next() {
		n++
	}
	rows.Close()
	if n != 1 {
		t.Errorf("preview returned %d rows", n)
	}

	// Unique constraint on customers.
	cinfo, err := src.Info(ctx, child(t, ctx, src, tablesGroup, f.name("customers")))
	if err != nil {
		t.Fatal(err)
	}
	hasUnique := false
	for _, c := range cinfo.Constraints {
		if c.Type == "UNIQUE" && strings.EqualFold(c.Columns, "email") {
			hasUnique = true
		}
	}
	if !hasUnique {
		t.Errorf("missing UNIQUE(email) in %+v", cinfo.Constraints)
	}

	// Table children: index and trigger nodes with a definition.
	kids, err := src.Children(ctx, table)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range kids {
		ki, err := src.Info(ctx, k)
		if err != nil {
			t.Fatalf("info of %s: %v", k.Name, err)
		}
		if ki.Source == "" {
			t.Errorf("%s %s has no definition", KindLabel(k.Kind), k.Name)
		}
		if strings.EqualFold(k.Name, "orders_trg") {
			t.Logf("trigger source: %q", ki.Source)
		}
	}
	if f.conn.Type != config.DuckDB && !hasNode(kids, KindTrigger, f.name("orders_trg")) {
		t.Errorf("trigger not found among %+v", kids)
	}
	if !hasNode(kids, KindIndex, f.name("orders_customer_ix")) {
		t.Errorf("index not found among %+v", kids)
	}

	// View with source.
	groupsParent := parentOfGroups(t, ctx, src, f)
	views := child(t, ctx, src, groupsParent, "Views")
	vinfo, err := src.Info(ctx, child(t, ctx, src, views, f.name("v_orders")))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(vinfo.Source), "join") || len(vinfo.Columns) != 2 {
		t.Errorf("unexpected view info: %q %+v", vinfo.Source, vinfo.Columns)
	}
	testEditView(t, ctx, src, f, child(t, ctx, src, views, f.name("v_orders")), vinfo.EditScript)

	// Routines, packages and sequences.
	for kind, name := range f.routes {
		g := child(t, ctx, src, groupsParent, groupLabel(kind))
		ri, err := src.Info(ctx, child(t, ctx, src, g, name))
		if err != nil {
			t.Fatalf("%s %s: %v", KindLabel(kind), name, err)
		}
		if kind == KindSequence {
			if len(ri.Properties) == 0 {
				t.Errorf("sequence %s has no properties", name)
			}
			continue
		}
		if !strings.Contains(strings.ToUpper(ri.Source), "CREATE") {
			t.Errorf("%s %s: unexpected source %q", KindLabel(kind), name, ri.Source)
		}
		if kind == KindPackage && !strings.Contains(ri.Source, "PACKAGE BODY") {
			t.Errorf("package source misses the body: %q", ri.Source)
		}
	}

	testSession(t, ctx, src, f)
}

// testEditView runs the edit script of a view, changed to add a column, on
// a session bound to the database of the view, like the editor does.
func testEditView(t *testing.T, ctx context.Context, src *Source, f fixture, view Node, script string) {
	t.Logf("edit script: %q", script)
	i := strings.LastIndex(strings.ToLower(script), "o.amount")
	if i < 0 {
		t.Fatalf("unexpected edit script %q", script)
	}
	script = script[:i] + "o.amount, o.id" + script[i+len("o.amount"):]
	sess, err := src.NewSession(ctx, view.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	for _, st := range sqlsplit.Split(script, src.SplitDialect()) {
		if _, err := sess.ExecContext(ctx, st.Text); err != nil {
			t.Fatalf("edit script: %v\n  in: %s", err, st.Text)
		}
	}
	info, err := src.Info(ctx, view)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Columns) != 3 {
		t.Errorf("edited view has columns %+v", info.Columns)
	}
}

// testSession checks sessions keep state and survive cancellation.
func testSession(t *testing.T, ctx context.Context, src *Source, f fixture) {
	database := ""
	if src.HasDatabases() {
		database = f.path[0]
	}
	sess, err := src.NewSession(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	// Unqualified name must resolve thanks to the session database.
	q := "SELECT count(*) FROM orders"
	switch f.conn.Type {
	case config.Postgres:
		q = "SELECT count(*) FROM dibi_test.orders"
	}
	var n int
	if err := queryRowScan(ctx, sess, q, &n); err != nil || n != 1 {
		t.Fatalf("session query: n=%d err=%v", n, err)
	}

	if f.conn.Type == config.MSSQL {
		testResultSets(t, ctx, sess)
	}
	testAutocommit(t, ctx, sess, strings.Replace(q, "orders", "customers", 1))

	// Cancel a slow query; the session must be usable (or reported dead).
	slow := map[config.Type]string{
		config.Postgres: "SELECT pg_sleep(10)",
		config.MySQL:    "SELECT COUNT(*) FROM information_schema.columns a, information_schema.columns b, information_schema.columns c",
		config.MariaDB:  "SELECT SLEEP(10)",
		config.Oracle:   "SELECT COUNT(*) FROM all_objects a, all_objects b WHERE a.object_id + b.object_id < 0",
		config.SQLite:   "WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c) SELECT count(*) FROM c",
		config.DuckDB:   "SELECT count(*) FROM range(100000000) a, range(1000) b WHERE a.range % 997 = b.range",
		config.MSSQL:    "WAITFOR DELAY '00:00:10'",
	}[f.conn.Type]
	cctx, ccancel := context.WithTimeout(ctx, 500*time.Millisecond)
	start := time.Now()
	var dummy any
	err = queryRowScan(cctx, sess, slow, &dummy)
	ccancel()
	t.Logf("cancel after %v: %v; alive=%v", time.Since(start).Round(time.Millisecond), err, sess.Alive(ctx))
	if err == nil {
		t.Error("slow query was not cancelled")
	}
	if f.conn.Type != config.SQLite && f.conn.Type != config.DuckDB && !sess.Alive(ctx) {
		t.Error("the session must survive a cancelled query")
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("cancellation took %v", time.Since(start))
	}
}

// testAutocommit checks that with autocommit off changes last until
// ROLLBACK, and that turning it back on commits them.
func testAutocommit(t *testing.T, ctx context.Context, sess *Session, count string) {
	table := strings.TrimPrefix(count, "SELECT count(*) FROM ")
	exec := func(q string) {
		t.Helper()
		if _, err := sess.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	check := func(what string, want int) {
		t.Helper()
		var n int
		if err := queryRowScan(ctx, sess, count, &n); err != nil || n != want {
			t.Errorf("%s: %d customers, want %d (err %v)", what, n, want, err)
		}
	}
	insert := "INSERT INTO " + table + " (id, name, email) VALUES (3, 'Grace', 'grace@example.com')"

	if err := sess.SetAutocommit(ctx, false); err != nil {
		t.Fatal(err)
	}
	exec(insert)
	check("before rollback", 3)
	exec("ROLLBACK")
	check("after rollback", 2)

	exec(insert)
	if err := sess.SetAutocommit(ctx, true); err != nil {
		t.Fatal(err)
	}
	sess.ExecContext(ctx, "ROLLBACK") // nothing to roll back; may fail
	check("after turning autocommit on", 3)
	exec("DELETE FROM " + table + " WHERE id = 3")
}

// testResultSets reads every result set of a batch through pagers; a
// statement without rows between them does not produce a result set.
func testResultSets(t *testing.T, ctx context.Context, sess *Session) {
	p, err := pager.New(ctx, sess, "DECLARE @x INT = 2\nSELECT 1 AS a\nUPDATE orders SET amount = amount\nSELECT @x AS b, 3 AS c")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for p != nil {
		rows, err := p.Page(0)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, c := range p.Cols {
			names = append(names, c.Name())
		}
		got = append(got, fmt.Sprintf("%s=%v", strings.Join(names, ","), rows))
		next, err := p.NextResult()
		if err != nil {
			t.Fatal(err)
		}
		p.Close() // must not close the cursor handed to next
		p = next
	}
	if want := []string{"a=[[1]]", "b,c=[[2 3]]"}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("result sets: got %v, want %v", got, want)
	}
}

func parentOfGroups(t *testing.T, ctx context.Context, src *Source, f fixture) Node {
	node := src.Root()
	for _, name := range f.path[:len(f.path)-1] {
		node = child(t, ctx, src, node, name)
	}
	return node
}

func child(t *testing.T, ctx context.Context, src *Source, parent Node, name string) Node {
	t.Helper()
	kids, err := src.Children(ctx, parent)
	if err != nil {
		t.Fatalf("children of %q: %v", parent.Name, err)
	}
	for _, k := range kids {
		if k.Name == name {
			return k
		}
	}
	var names []string
	for _, k := range kids {
		names = append(names, k.Name)
	}
	t.Fatalf("%q not found under %q; children: %v", name, parent.Name, names)
	return Node{}
}

func hasNode(nodes []Node, k Kind, name string) bool {
	for _, n := range nodes {
		if n.Kind == k && n.Name == name {
			return true
		}
	}
	return false
}

func TestFileSources(t *testing.T) {
	dir := t.TempDir()
	csvPath := filepath.Join(dir, "sales 2026.csv")
	os.WriteFile(csvPath, []byte("id,region,amount\n1,north,10.5\n2,south,\n3,east,7\n"), 0o600)
	pqPath := filepath.Join(dir, "events.parquet")
	db, _ := sql.Open("duckdb", "")
	if _, err := db.Exec(fmt.Sprintf("COPY (SELECT range AS id FROM range(120)) TO '%s' (FORMAT parquet)", pqPath)); err != nil {
		t.Fatal(err)
	}
	db.Close()

	for _, c := range []struct {
		conn  config.Connection
		table string
		cols  int
		rows  int
	}{
		{config.Connection{Name: "csv", Type: config.CSV, Path: csvPath}, "sales_2026", 3, 3},
		{config.Connection{Name: "pq", Type: config.Parquet, Path: pqPath}, "events", 1, PreviewRows},
	} {
		t.Run(c.conn.Name, func(t *testing.T) {
			ctx := context.Background()
			src, err := NewSource(c.conn)
			if err != nil {
				t.Fatal(err)
			}
			defer src.Close()
			kids, err := src.Children(ctx, src.Root())
			if err != nil || len(kids) != 1 || kids[0].Name != c.table {
				t.Fatalf("children=%+v err=%v", kids, err)
			}
			info, err := src.Info(ctx, kids[0])
			if err != nil || len(info.Columns) != c.cols {
				t.Fatalf("info=%+v err=%v", info, err)
			}
			// A session sees the view too.
			sess, err := src.NewSession(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			defer sess.Close()
			rows, err := sess.QueryContext(ctx, info.Preview)
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for rows.Next() {
				n++
			}
			rows.Close()
			if n != c.rows {
				t.Errorf("preview returned %d rows, want %d", n, c.rows)
			}
		})
	}
}

func TestFolderSource(t *testing.T) {
	dir := t.TempDir()
	write := func(name, data string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("orders.csv", "id,customer_id,amount\n1,1,10\n2,2,20\n3,1,5\n")
	write("customers.csv", "id,name\n1,Ada\n2,Bob\n")
	write("Orders.CSV", "id,customer_id,amount\n4,2,7\n") // same name as orders.csv but for case
	write("notes.md", "not data")
	write("other.tsv", "a\tb\n1\t2\n")
	write("other.txt", "a,b\n1,2\n")
	write(".hidden.csv", "a\n1\n")
	os.Mkdir(filepath.Join(dir, "sub"), 0o700)
	write("sub/nested.csv", "a\n1\n")

	ctx := context.Background()
	src, err := NewSource(config.Connection{Name: "folder", Type: config.CSV, Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	names := func() []string {
		kids, err := src.Children(ctx, src.Root())
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, k := range kids {
			out = append(out, k.Name)
		}
		return out
	}
	if got, want := fmt.Sprint(names()), "[customers Orders orders_2]"; got != want {
		t.Fatalf("views = %s, want %s", got, want)
	}

	sess, err := src.NewSession(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	var total float64
	if err := queryRowScan(ctx, sess, `
		SELECT sum(o.amount) FROM (SELECT * FROM orders_2 UNION ALL SELECT * FROM Orders) o
		JOIN customers c ON c.id = o.customer_id WHERE c.name = 'Bob'`, &total); err != nil {
		t.Fatal(err)
	}
	if total != 27 {
		t.Errorf("total = %v, want 27", total)
	}

	info, err := src.Info(ctx, Node{Kind: KindView, Name: "customers", Object: "customers", Database: "memory", Schema: "main"})
	if err != nil || len(info.Properties) != 1 || info.Properties[0][1] != filepath.Join(dir, "customers.csv") {
		t.Fatalf("info=%+v err=%v", info, err)
	}

	// A refresh picks up new and removed files.
	os.Remove(filepath.Join(dir, "Orders.CSV"))
	write("products.csv", "id,name\n1,pen\n")
	if got, want := fmt.Sprint(names()), "[customers orders products]"; got != want {
		t.Errorf("views after refresh = %s, want %s", got, want)
	}
	if err := queryRowScan(ctx, sess, "SELECT sum(amount) FROM orders", &total); err != nil || total != 35 {
		t.Errorf("total = %v, err = %v", total, err)
	}
}

func TestFolderWithUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	db, _ := sql.Open("duckdb", "")
	if _, err := db.Exec(fmt.Sprintf("COPY (SELECT 1 AS id) TO '%s' (FORMAT parquet)", filepath.Join(dir, "good.parquet"))); err != nil {
		t.Fatal(err)
	}
	db.Close()
	os.WriteFile(filepath.Join(dir, "bad.parquet"), []byte("not parquet"), 0o600)

	src, _ := NewSource(config.Connection{Name: "pq", Type: config.Parquet, Path: dir})
	defer src.Close()
	// The folder still opens, and the tree gets the readable files and an
	// error naming the other.
	kids, err := src.Children(context.Background(), src.Root())
	if err == nil || !strings.Contains(err.Error(), "bad.parquet") {
		t.Errorf("expected an error naming bad.parquet, got %v", err)
	}
	if len(kids) != 1 || kids[0].Name != "good" {
		t.Errorf("children = %+v", kids)
	}
}

func TestCSVOptions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.csv")
	os.WriteFile(path, []byte("name;note\n|Rossi; Mario|;x\n"), 0o600)
	src, _ := NewSource(config.Connection{Name: "c", Type: config.CSV, Path: path, Separator: ";", Quote: "|"})
	defer src.Close()
	ctx := context.Background()
	sess, err := src.NewSession(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	var name string
	if err := queryRowScan(ctx, sess, "SELECT name FROM a", &name); err != nil || name != "Rossi; Mario" {
		t.Errorf("name = %q, err = %v", name, err)
	}
}

func TestOnlyCSVFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.tsv")
	os.WriteFile(path, []byte("a\tb\n1\t2\n"), 0o600)
	src, _ := NewSource(config.Connection{Name: "c", Type: config.CSV, Path: path})
	defer src.Close()
	if err := src.Ping(context.Background()); err == nil || !strings.Contains(err.Error(), "not a .csv file") {
		t.Errorf("expected a .csv error, got %v", err)
	}
}

func TestFileTableName(t *testing.T) {
	for in, want := range map[string]string{
		"/x/sales 2026.csv": "sales_2026",
		"/x/2026.parquet":   "t_2026",
	} {
		if got := FileTableName(in); got != want {
			t.Errorf("FileTableName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMissingFileIsNotCreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	src, _ := NewSource(config.Connection{Name: "x", Type: config.SQLite, Path: path})
	if err := src.Ping(context.Background()); err == nil {
		t.Fatal("expected an error for a missing file")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("the file must not be created")
	}
}

func TestInMemory(t *testing.T) {
	ctx := context.Background()
	for _, typ := range []config.Type{config.SQLite, config.DuckDB} {
		t.Run(string(typ), func(t *testing.T) {
			open := func() *Source {
				src, err := NewSource(config.Connection{Name: "mem", Type: typ, Path: config.MemoryPath})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(src.Close)
				return src
			}
			src := open()
			s1, err := src.NewSession(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			defer s1.Close()
			if _, err := s1.ExecContext(ctx, "CREATE TABLE t (a INTEGER)"); err != nil {
				t.Fatal(err)
			}
			if _, err := s1.ExecContext(ctx, "INSERT INTO t VALUES (1), (2)"); err != nil {
				t.Fatal(err)
			}
			// Every session of the source sees the same database.
			s2, err := src.NewSession(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			defer s2.Close()
			var n int
			if err := queryRowScan(ctx, s2, "SELECT count(*) FROM t", &n); err != nil || n != 2 {
				t.Fatalf("count = %d, %v; want 2", n, err)
			}
			// Another source gets its own database.
			s3, err := open().NewSession(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			defer s3.Close()
			if err := queryRowScan(ctx, s3, "SELECT count(*) FROM t", &n); err == nil {
				t.Fatal("another source must not see the table")
			}
			if _, err := os.Stat(config.MemoryPath); err == nil {
				t.Fatal("no file must be created")
			}
		})
	}
}

func queryRowScan(ctx context.Context, sess *Session, q string, dest ...any) error {
	rows, err := sess.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	return rows.Scan(dest...)
}
