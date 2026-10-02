// Package db opens connections to the supported databases, runs sessions and
// reads the metadata shown in the object tree and in the info panel.
package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"time"

	"dibi/internal/config"
	"dibi/internal/sqlsplit"
)

// Kind is the kind of a node in the object tree.
type Kind int

const (
	KindConnection Kind = iota
	KindDatabase
	KindSchema
	KindGroup
	KindTable
	KindView
	KindMatView
	KindFunction
	KindProcedure
	KindPackage
	KindSequence
	KindIndex
	KindTrigger
)

// groupLabel is the label of a group node containing objects of a kind.
func groupLabel(k Kind) string {
	switch k {
	case KindTable:
		return "Tables"
	case KindView:
		return "Views"
	case KindMatView:
		return "Materialized Views"
	case KindFunction:
		return "Functions"
	case KindProcedure:
		return "Procedures"
	case KindPackage:
		return "Packages"
	case KindSequence:
		return "Sequences"
	}
	return "Objects"
}

// KindLabel returns a singular, human-readable name of an object kind.
func KindLabel(k Kind) string {
	switch k {
	case KindConnection:
		return "Connection"
	case KindDatabase:
		return "Database"
	case KindSchema:
		return "Schema"
	case KindTable:
		return "Table"
	case KindView:
		return "View"
	case KindMatView:
		return "Materialized view"
	case KindFunction:
		return "Function"
	case KindProcedure:
		return "Procedure"
	case KindPackage:
		return "Package"
	case KindSequence:
		return "Sequence"
	case KindIndex:
		return "Index"
	case KindTrigger:
		return "Trigger"
	}
	return "Object"
}

// Node is a node of the object tree. Database, Schema and Table locate the
// node; which of them are used depends on the database type.
type Node struct {
	Kind     Kind
	Name     string // display name
	Object   string // object name as stored in the catalog
	Database string
	Schema   string
	Table    string // parent table, for indexes and triggers
	Group    Kind   // for KindGroup: kind of the children
	ID       string // dialect-specific identifier (e.g. Postgres function OID)
}

// HasChildren reports whether the node can be expanded.
func (n Node) HasChildren() bool {
	switch n.Kind {
	case KindConnection, KindDatabase, KindSchema, KindGroup, KindTable:
		return true
	}
	return false
}

// HasInfo reports whether clicking the node opens an information panel.
func (n Node) HasInfo() bool {
	switch n.Kind {
	case KindConnection, KindDatabase, KindSchema, KindGroup:
		return false
	}
	return true
}

// Key returns a string uniquely identifying the node within its connection.
func (n Node) Key() string {
	return fmt.Sprintf("%d|%s|%s|%s|%s|%d|%s", n.Kind, n.Database, n.Schema, n.Table, n.Object, n.Group, n.ID)
}

// Column describes a table or view column.
type Column struct {
	Name       string
	Type       string
	Nullable   bool
	Default    string
	PrimaryKey bool
	Comment    string
}

// Constraint describes a table constraint.
type Constraint struct {
	Name       string
	Type       string // PRIMARY KEY, FOREIGN KEY, UNIQUE, CHECK, EXCLUDE
	Columns    string
	Definition string // e.g. referenced table and columns, or check expression
}

// Index describes a table index.
type Index struct {
	Name    string
	Unique  bool
	Columns string
}

// Info is the content of the information panel of an object.
type Info struct {
	Title       string
	Columns     []Column
	Constraints []Constraint
	Indexes     []Index
	Properties  [][2]string
	Source      string
	Preview     string // SQL returning the first PreviewRows rows, or ""
	SelectAll   string // SQL returning all rows (used by export), or ""
}

// Row-limiting syntaxes for the data preview.
const (
	limitSyntax      = "LIMIT %d"
	fetchFirstSyntax = "FETCH FIRST %d ROWS ONLY"
)

// setSelect sets the preview and select-all queries for a table or view.
func (i *Info) setSelect(qualified, limit string) {
	i.SelectAll = "SELECT * FROM " + qualified
	i.Preview = i.SelectAll + " " + fmt.Sprintf(limit, PreviewRows)
}

// setSelectTop is setSelect for SQL Server, which has no LIMIT clause
// (and OFFSET … FETCH requires an ORDER BY).
func (i *Info) setSelectTop(qualified string) {
	i.SelectAll = "SELECT * FROM " + qualified
	i.Preview = fmt.Sprintf("SELECT TOP %d * FROM %s", PreviewRows, qualified)
}

// PreviewRows is the number of rows shown in the data preview.
const PreviewRows = 50

// dialect implements the database-specific parts.
type dialect interface {
	open(c config.Connection, database string) (*sql.DB, error)
	splitDialect() sqlsplit.Dialect

	// Tree levels: hasDatabases/hasSchemas tell which levels exist.
	hasDatabases() bool
	hasSchemas() bool
	databases(ctx context.Context, db *sql.DB) ([]string, error)
	schemas(ctx context.Context, db *sql.DB, database string) ([]string, error)
	groups() []Kind
	objects(ctx context.Context, db *sql.DB, group Node) ([]Node, error)
	tableChildren(ctx context.Context, db *sql.DB, table Node) ([]Node, error)
	info(ctx context.Context, db *sql.DB, n Node) (*Info, error)

	// useDatabase switches a session to a database (no-op if not applicable).
	useDatabase(ctx context.Context, conn *sql.Conn, database string) error
	// separatePools reports whether each database needs its own pool (Postgres).
	separatePools() bool
}

func newDialect(t config.Type) (dialect, error) {
	switch t {
	case config.Postgres:
		return postgres{}, nil
	case config.MySQL, config.MariaDB:
		return mysqlDialect{}, nil
	case config.Oracle:
		return oracle{}, nil
	case config.MSSQL:
		return mssqlDialect{}, nil
	case config.SQLite:
		return sqlite{}, nil
	case config.DuckDB:
		return duckdb{}, nil
	case config.CSV, config.Parquet:
		return fileDialect{}, nil
	}
	return nil, fmt.Errorf("unsupported connection type %q", t)
}

// Source is an open (lazily connected) data source for one saved connection.
// It is safe for concurrent use.
type Source struct {
	Conn config.Connection
	d    dialect

	mu    sync.Mutex
	pools map[string]*sql.DB // by database name ("" = default)
}

// NewSource prepares a source; no connection is made until needed.
func NewSource(c config.Connection) (*Source, error) {
	d, err := newDialect(c.Type)
	if err != nil {
		return nil, err
	}
	return &Source{Conn: c, d: d, pools: map[string]*sql.DB{}}, nil
}

// SplitDialect returns the lexical rules for splitting scripts.
func (s *Source) SplitDialect() sqlsplit.Dialect { return s.d.splitDialect() }

// HasDatabases reports whether the source has a database level
// (sessions can then be bound to a database).
func (s *Source) HasDatabases() bool { return s.d.hasDatabases() }

// SwitchableDatabases reports whether a session can be bound to one of
// several databases (Postgres, MySQL/MariaDB, SQL Server, DuckDB).
func (s *Source) SwitchableDatabases() bool {
	return s.d.hasDatabases() && s.Conn.Type != config.SQLite
}

// Databases lists the databases a session can be bound to, or nil.
func (s *Source) Databases(ctx context.Context) ([]string, error) {
	if !s.SwitchableDatabases() {
		return nil, nil
	}
	pool, err := s.Pool("")
	if err != nil {
		return nil, err
	}
	return s.d.databases(ctx, pool)
}

// DefaultDatabase returns the database a new session uses when none is chosen.
func (s *Source) DefaultDatabase(ctx context.Context) string {
	if !s.SwitchableDatabases() {
		return ""
	}
	if s.Conn.Database != "" {
		return s.Conn.Database
	}
	pool, err := s.Pool("")
	if err != nil {
		return ""
	}
	q := "SELECT current_database()"
	switch s.Conn.Type {
	case config.MySQL, config.MariaDB:
		q = "SELECT COALESCE(DATABASE(), '')"
	case config.MSSQL:
		q = "SELECT DB_NAME()"
	}
	name, _ := queryString(ctx, pool, q)
	return name
}

// Pool returns the connection pool for a database ("" = the default one).
func (s *Source) Pool(database string) (*sql.DB, error) {
	if !s.d.separatePools() || database == s.Conn.Database {
		database = ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.pools[database]; ok {
		return p, nil
	}
	p, err := s.d.open(s.Conn, database)
	if err != nil {
		return nil, err
	}
	p.SetMaxIdleConns(2)
	p.SetConnMaxIdleTime(5 * time.Minute)
	s.pools[database] = p
	return p, nil
}

// Ping checks that the source is reachable.
func (s *Source) Ping(ctx context.Context) error {
	p, err := s.Pool("")
	if err != nil {
		return err
	}
	return p.PingContext(ctx)
}

// Close closes every pool of the source.
func (s *Source) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, p := range s.pools {
		p.Close()
		delete(s.pools, k)
	}
}

// Root returns the root node of the source.
func (s *Source) Root() Node {
	return Node{Kind: KindConnection, Name: s.Conn.Name}
}

// Children loads the children of a node.
func (s *Source) Children(ctx context.Context, n Node) ([]Node, error) {
	pool, err := s.Pool(n.Database)
	if err != nil {
		return nil, err
	}
	switch n.Kind {
	case KindConnection:
		if fd, ok := s.d.(fileDialect); ok {
			return fd.root(ctx, pool, s.Conn)
		}
		if s.d.hasDatabases() {
			names, err := s.d.databases(ctx, pool)
			return nodes(names, func(name string) Node {
				return Node{Kind: KindDatabase, Name: name, Object: name, Database: name}
			}), err
		}
		return s.schemaNodes(ctx, pool, "")
	case KindDatabase:
		if s.d.hasSchemas() {
			return s.schemaNodes(ctx, pool, n.Database)
		}
		return s.groupNodes(n), nil
	case KindSchema:
		return s.groupNodes(n), nil
	case KindGroup:
		return s.d.objects(ctx, pool, n)
	case KindTable:
		return s.d.tableChildren(ctx, pool, n)
	}
	return nil, nil
}

func (s *Source) schemaNodes(ctx context.Context, pool *sql.DB, database string) ([]Node, error) {
	names, err := s.d.schemas(ctx, pool, database)
	return nodes(names, func(name string) Node {
		return Node{Kind: KindSchema, Name: name, Object: name, Database: database, Schema: name}
	}), err
}

func (s *Source) groupNodes(parent Node) []Node {
	var out []Node
	for _, k := range s.d.groups() {
		out = append(out, Node{Kind: KindGroup, Name: groupLabel(k), Group: k,
			Database: parent.Database, Schema: parent.Schema})
	}
	return out
}

func nodes(names []string, f func(string) Node) []Node {
	out := make([]Node, len(names))
	for i, n := range names {
		out[i] = f(n)
	}
	return out
}

// Info loads the information panel content of a node.
func (s *Source) Info(ctx context.Context, n Node) (*Info, error) {
	if !n.HasInfo() {
		return nil, errors.New("no information available for this node")
	}
	pool, err := s.Pool(n.Database)
	if err != nil {
		return nil, err
	}
	return s.d.info(ctx, pool, n)
}

// Session is a dedicated database connection, used by one SQL editor tab so
// that session state (transactions, temp tables, settings) is preserved.
//
// Use its QueryContext and ExecContext rather than the raw connection: for
// MySQL/MariaDB, cancelling the context would make the driver close the
// connection, so queries are instead interrupted server-side (KILL QUERY).
type Session struct {
	Database string
	conn     *sql.Conn

	// interrupt, if set, stops the running query server-side; the context
	// passed to the driver is then never cancelled.
	interrupt func()
	// plainCancel: the context passed to the driver is cancelled with the
	// caller's one but never reaches its deadline (see plainCanceller).
	plainCancel bool
}

// interrupter is implemented by dialects that cancel queries server-side.
type interrupter interface {
	sessionInterrupt(ctx context.Context, pool *sql.DB, conn *sql.Conn) (func(), error)
}

// plainCanceller is implemented by dialects whose driver drops the
// connection when a context deadline expires, but not when the context is
// cancelled (SQL Server). Sessions then turn deadlines into cancellations,
// so that a timeout does not lose the session state.
type plainCanceller interface {
	plainCancel() bool
}

// NewSession opens a dedicated connection, bound to database if not empty.
func (s *Source) NewSession(ctx context.Context, database string) (*Session, error) {
	pool, err := s.Pool(database)
	if err != nil {
		return nil, err
	}
	conn, err := pool.Conn(ctx)
	if err != nil {
		return nil, err
	}
	ss := &Session{Database: database, conn: conn}
	if database != "" {
		if err := s.d.useDatabase(ctx, conn, database); err != nil {
			ss.Close()
			return nil, err
		}
	}
	if pc, ok := s.d.(plainCanceller); ok {
		ss.plainCancel = pc.plainCancel()
	}
	if in, ok := s.d.(interrupter); ok {
		if ss.interrupt, err = in.sessionInterrupt(ctx, pool, conn); err != nil {
			ss.Close()
			return nil, err
		}
	}
	return ss, nil
}

// driverContext returns the context to hand to the driver, and a function
// to call when the driver call returns.
func (ss *Session) driverContext(ctx context.Context) (context.Context, func() bool) {
	if ss.plainCancel {
		// The driver context must live as long as ctx: rows returned by a
		// query still use it, so the AfterFunc is not stopped on return.
		dctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		context.AfterFunc(ctx, cancel)
		return dctx, func() bool { return true }
	}
	if ss.interrupt == nil {
		return ctx, func() bool { return true }
	}
	return context.WithoutCancel(ctx), context.AfterFunc(ctx, ss.interrupt)
}

// QueryContext runs a query on the session. Cancelling ctx interrupts it
// while it starts; use Interrupt to stop reading a result later.
func (ss *Session) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	dctx, done := ss.driverContext(ctx)
	defer done()
	return ss.conn.QueryContext(dctx, query, args...)
}

// ExecContext executes a statement on the session. Cancelling ctx interrupts it.
func (ss *Session) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	dctx, done := ss.driverContext(ctx)
	defer done()
	return ss.conn.ExecContext(dctx, query, args...)
}

// Interrupt stops the query currently running on the session server-side,
// where cancelling its context is not enough (MySQL/MariaDB). It is a no-op
// for other databases. Call it only while a query is running.
func (ss *Session) Interrupt() {
	if ss.interrupt != nil {
		ss.interrupt()
	}
}

// Alive reports whether the connection still works (it may have been closed
// by the server, or by a cancelled query).
func (ss *Session) Alive(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return ss.conn.PingContext(ctx) == nil
}

// Close releases the connection.
func (ss *Session) Close() error {
	// Returning ErrBadConn from Raw discards the connection instead of
	// putting it back in the pool with leftover session state.
	ss.conn.Raw(func(any) error { return driver.ErrBadConn })
	return ss.conn.Close()
}
