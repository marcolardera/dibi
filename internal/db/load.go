package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"dibi/internal/config"

	duckdbdrv "github.com/duckdb/duckdb-go/v2"
	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/stdlib"
	mssql "github.com/microsoft/go-mssqldb"
	go_ora "github.com/sijms/go-ora/v2"
)

// Creating a table and loading rows into it, for the import. Each database
// gets its fastest way in:
//
//   - Postgres: COPY FROM STDIN, in binary format, through pgx.
//   - SQL Server: a bulk copy (INSERT BULK) through go-mssqldb.
//   - DuckDB: the appender.
//   - MySQL/MariaDB: LOAD DATA LOCAL INFILE, streamed from memory. Servers
//     where local_infile is off (the default since MySQL 8) refuse it, and
//     multi-row INSERTs are used instead.
//   - Oracle: INSERTs with array binding.
//   - SQLite: prepared INSERTs; there is no bulk path, and in a single
//     transaction they are fast.
//
// The load is all or nothing: on error or cancellation no table is left.
// Where DDL is transactional (Postgres, SQL Server, DuckDB, SQLite) the
// table is created in the load transaction; elsewhere it is dropped.

// ColumnKind is the type of a column of a new table.
type ColumnKind int

const (
	ColText ColumnKind = iota
	ColBool
	ColInt    // fits in 32 bits
	ColBigInt // fits in 64 bits
	ColDecimal
	ColFloat
	ColDate
	ColTimestamp
)

// MaxDecimalPrecision is the largest precision of a ColDecimal column (the
// limit of SQL Server, Oracle and DuckDB).
const MaxDecimalPrecision = 38

// MaxNameBytes is the longest table or column name accepted by every
// database (Postgres truncates names to 63 bytes).
const MaxNameBytes = 63

// NewColumn describes a column of a table to create.
type NewColumn struct {
	Name string
	Kind ColumnKind

	Precision, Scale int // ColDecimal
	FracDigits       int // ColTimestamp: most fractional second digits seen

	// ColText: length of the longest value in characters, bytes (UTF-8)
	// and UTF-16 code units.
	MaxChars, MaxBytes, MaxUTF16 int
}

// RowFunc returns the next row to load, or io.EOF after the last one. The
// values match the column kinds: nil (NULL), string (ColText, and
// ColDecimal as a plain decimal like "-12.50"), bool, int64, float64, and
// time.Time in UTC, holding the wall-clock time (ColDate, ColTimestamp).
// The slice may be reused by the next call.
type RowFunc func() ([]any, error)

// loader is implemented by the dialects that can create and load tables.
type loader interface {
	// columnTypes returns the SQL type of each column.
	columnTypes(cols []NewColumn) []string
	// load creates the table and loads the rows on a dedicated connection.
	// It sets t.created if the table survives a failed load, to be dropped.
	load(ctx context.Context, conn *sql.Conn, t *newTable, next RowFunc, progress func(int64)) (int64, error)
}

// newTable is a table being created by an import.
type newTable struct {
	database, schema, name string // location; unused levels are ""
	qualified              string // quoted, qualified name
	columns                []NewColumn
	names                  []string // column names, unquoted
	quoted                 []string // column names, quoted
	ddl                    string
	created                bool // the table must be dropped if the load fails
	dirty                  bool // the session state of the connection was changed
}

// progressEvery is how often (in rows) a load reports its progress.
const progressEvery = 5000

// CanImport reports whether a table can be imported into a node: a schema,
// a database where there are no schemas (MySQL, SQLite), or their Tables
// group, which locates the same schema or database.
func (s *Source) CanImport(n Node) bool {
	if _, ok := s.d.(loader); !ok {
		return false
	}
	if _, ok := s.d.(fileDialect); ok { // views of read-only files
		return false
	}
	if n.Kind == KindGroup && n.Group == KindTable {
		return true
	}
	if s.d.hasSchemas() {
		return n.Kind == KindSchema
	}
	return n.Kind == KindDatabase
}

// ImportTarget names the schema or database a table imported into n goes
// to, for messages: "schema public", "database shop".
func (s *Source) ImportTarget(n Node) string {
	if s.d.hasSchemas() {
		return "schema " + n.Schema
	}
	return "database " + n.Database
}

// plainIdent matches the names that need no quoting in any database.
var plainIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// StoredName returns the name under which a table or column is stored when
// the name is typed unquoted in a statement: Postgres folds plain names to
// lower case, Oracle to upper case. Other names are kept as they are.
func (s *Source) StoredName(name string) string {
	if !plainIdent.MatchString(name) {
		return name
	}
	switch s.Conn.Type {
	case config.Postgres:
		return strings.ToLower(name)
	case config.Oracle:
		return strings.ToUpper(name)
	}
	return name
}

// quoteIdent always quotes an identifier.
func quoteIdent(t config.Type, s string) string {
	switch t {
	case config.MySQL, config.MariaDB:
		return quoteBacktick(s)
	case config.MSSQL:
		return quoteBracket(s)
	}
	return quoteDouble(s)
}

// CreateTable creates a table named table (a stored name) in the schema or
// database n, with the given columns, and loads the rows returned by next
// into it. progress, if not nil, is called periodically with the number of
// rows loaded. It returns the number of rows loaded.
func (s *Source) CreateTable(ctx context.Context, n Node, table string, cols []NewColumn, next RowFunc, progress func(int64)) (_ int64, err error) {
	l, ok := s.d.(loader)
	if !ok || !s.CanImport(n) {
		return 0, errors.New("tables cannot be created here")
	}
	if len(cols) == 0 {
		return 0, errors.New("the table has no columns")
	}
	if progress == nil {
		progress = func(int64) {}
	}
	q := func(name string) string { return quoteIdent(s.Conn.Type, name) }
	t := &newTable{database: n.Database, schema: n.Schema, name: table, columns: cols}
	switch s.Conn.Type {
	case config.DuckDB:
		t.qualified = q(n.Database) + "." + q(n.Schema) + "." + q(table)
	case config.MySQL, config.MariaDB, config.SQLite:
		t.qualified = q(n.Database) + "." + q(table)
	default:
		t.qualified = q(n.Schema) + "." + q(table)
	}
	types := l.columnTypes(cols)
	defs := make([]string, len(cols))
	for i, c := range cols {
		t.names = append(t.names, c.Name)
		t.quoted = append(t.quoted, q(c.Name))
		defs[i] = q(c.Name) + " " + types[i]
	}
	t.ddl = "CREATE TABLE " + t.qualified + " (\n\t" + strings.Join(defs, ",\n\t") + "\n)"

	pool, err := s.Pool(n.Database)
	if err != nil {
		return 0, err
	}
	conn, err := pool.Conn(ctx)
	if err != nil {
		return 0, err
	}
	var count int64
	defer func() {
		// A connection left in a broken state or with changed settings is
		// not returned to the pool, unless it holds an in-memory database.
		if (err != nil || t.dirty) && !s.Conn.InMemory() {
			conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		conn.Close()
	}()

	if rows, err := conn.QueryContext(ctx, "SELECT * FROM "+t.qualified+" WHERE 1 = 0"); err == nil {
		rows.Close()
		return 0, fmt.Errorf("%s already exists", s.QualifiedName(Node{Kind: KindTable,
			Database: n.Database, Schema: n.Schema, Object: table}))
	}

	count, err = l.load(ctx, conn, t, next, progress)
	if err != nil && t.created {
		// With a fresh context: ctx may be the reason of the failure.
		dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, derr := pool.ExecContext(dctx, "DROP TABLE "+t.qualified); derr != nil {
			err = fmt.Errorf("%w\n\nThe table %s could not be dropped: %v", err, t.qualified, derr)
		}
	}
	if err != nil {
		return 0, err
	}
	return count, nil
}

// insertSQL returns an INSERT of rows rows into t, with placeholders
// built by ph from the parameter number (1-based).
func insertSQL(t *newTable, rows int, ph func(int) string) string {
	var b strings.Builder
	b.WriteString("INSERT INTO " + t.qualified + " (" + strings.Join(t.quoted, ", ") + ") VALUES ")
	p := 1
	for r := range rows {
		if r > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('(')
		for i := range t.columns {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(ph(p))
			p++
		}
		b.WriteByte(')')
	}
	return b.String()
}

func questionMark(int) string { return "?" }

// timestampText formats a timestamp for databases that take text.
func timestampText(t time.Time) string {
	return t.Format("2006-01-02 15:04:05.999999999")
}

// decimalDigits returns the digits of a decimal string scaled to scale
// fractional digits, as an integer: "-1.5" with scale 2 gives -150.
func decimalDigits(s string, scale int) (*big.Int, error) {
	intPart, frac, _ := strings.Cut(s, ".")
	if len(frac) > scale {
		return nil, fmt.Errorf("%s has more than %d decimals", s, scale)
	}
	v, ok := new(big.Int).SetString(intPart+frac+strings.Repeat("0", scale-len(frac)), 10)
	if !ok {
		return nil, fmt.Errorf("invalid decimal %q", s)
	}
	return v, nil
}

// ---------------------------------------------------------------- Postgres

func (postgres) columnTypes(cols []NewColumn) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		switch c.Kind {
		case ColBool:
			out[i] = "boolean"
		case ColInt:
			out[i] = "integer"
		case ColBigInt:
			out[i] = "bigint"
		case ColDecimal:
			out[i] = fmt.Sprintf("numeric(%d,%d)", c.Precision, c.Scale)
		case ColFloat:
			out[i] = "double precision"
		case ColDate:
			out[i] = "date"
		case ColTimestamp:
			out[i] = "timestamp"
		default:
			out[i] = "text"
		}
	}
	return out
}

// pgCopySource adapts a RowFunc to pgx.CopyFromSource.
type pgCopySource struct {
	ctx      context.Context
	cols     []NewColumn
	next     RowFunc
	progress func(int64)
	row      []any
	n        int64
	err      error
}

func (s *pgCopySource) Next() bool {
	if s.err = s.ctx.Err(); s.err != nil {
		return false
	}
	row, err := s.next()
	if err == io.EOF {
		return false
	}
	if err != nil {
		s.err = err
		return false
	}
	for i, v := range row {
		if str, ok := v.(string); ok && s.cols[i].Kind == ColDecimal {
			var num pgtype.Numeric
			if err := num.Scan(str); err != nil {
				s.err = err
				return false
			}
			row[i] = num
		}
	}
	s.row = row
	if s.n++; s.n%progressEvery == 0 {
		s.progress(s.n)
	}
	return true
}

func (s *pgCopySource) Values() ([]any, error) { return s.row, nil }
func (s *pgCopySource) Err() error             { return s.err }

func (postgres) load(ctx context.Context, conn *sql.Conn, t *newTable, next RowFunc, progress func(int64)) (int64, error) {
	var n int64
	err := conn.Raw(func(dc any) error {
		pc := dc.(*stdlib.Conn).Conn()
		tx, err := pc.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(context.Background())
		if _, err := tx.Exec(ctx, t.ddl); err != nil {
			return err
		}
		src := &pgCopySource{ctx: ctx, cols: t.columns, next: next, progress: progress}
		if n, err = tx.CopyFrom(ctx, pgx.Identifier{t.schema, t.name}, t.names, src); err != nil {
			return err
		}
		if err := src.Err(); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
	return n, err
}

// ---------------------------------------------------------------- SQL Server

func (mssqlDialect) columnTypes(cols []NewColumn) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		switch c.Kind {
		case ColBool:
			out[i] = "bit"
		case ColInt:
			out[i] = "int"
		case ColBigInt:
			out[i] = "bigint"
		case ColDecimal:
			out[i] = fmt.Sprintf("decimal(%d,%d)", c.Precision, c.Scale)
		case ColFloat:
			out[i] = "float"
		case ColDate:
			out[i] = "date"
		case ColTimestamp:
			out[i] = "datetime2"
		default:
			if c.MaxUTF16 <= 4000 {
				out[i] = fmt.Sprintf("nvarchar(%d)", max(c.MaxUTF16, 1))
			} else {
				out[i] = "nvarchar(max)"
			}
		}
	}
	return out
}

func (mssqlDialect) load(ctx context.Context, conn *sql.Conn, t *newTable, next RowFunc, progress func(int64)) (int64, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, t.ddl); err != nil {
		return 0, err
	}
	stmt, err := tx.PrepareContext(ctx, mssql.CopyIn(quoteBracket(t.schema)+"."+quoteBracket(t.name),
		mssql.BulkOptions{KeepNulls: true, Tablock: true}, t.names...))
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	var n int64
	for {
		row, err := next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
		if _, err := stmt.ExecContext(ctx, row...); err != nil {
			return 0, err
		}
		if n++; n%progressEvery == 0 {
			progress(n)
		}
	}
	// Without arguments, Exec sends the buffered rows and ends the copy.
	if _, err := stmt.ExecContext(ctx); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// ---------------------------------------------------------------- DuckDB

func (duckdb) columnTypes(cols []NewColumn) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		switch c.Kind {
		case ColBool:
			out[i] = "BOOLEAN"
		case ColInt:
			out[i] = "INTEGER"
		case ColBigInt:
			out[i] = "BIGINT"
		case ColDecimal:
			out[i] = fmt.Sprintf("DECIMAL(%d,%d)", c.Precision, c.Scale)
		case ColFloat:
			out[i] = "DOUBLE"
		case ColDate:
			out[i] = "DATE"
		case ColTimestamp:
			out[i] = "TIMESTAMP"
		default:
			out[i] = "VARCHAR"
		}
	}
	return out
}

func (duckdb) load(ctx context.Context, conn *sql.Conn, t *newTable, next RowFunc, progress func(int64)) (int64, error) {
	// The appender writes through the connection, so it takes part in a
	// transaction begun on it.
	if _, err := conn.ExecContext(ctx, "BEGIN TRANSACTION"); err != nil {
		return 0, err
	}
	done := false
	defer func() {
		if !done {
			conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if _, err := conn.ExecContext(ctx, t.ddl); err != nil {
		return 0, err
	}
	var n int64
	err := conn.Raw(func(dc any) error {
		a, err := duckdbdrv.NewAppender(dc.(driver.Conn), t.database, t.schema, t.name)
		if err != nil {
			return err
		}
		defer a.Close()
		vals := make([]driver.Value, len(t.columns))
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			row, err := next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			for i, v := range row {
				vals[i] = v
				if str, ok := v.(string); ok && t.columns[i].Kind == ColDecimal {
					c := t.columns[i]
					digits, err := decimalDigits(str, c.Scale)
					if err != nil {
						return err
					}
					vals[i] = duckdbdrv.Decimal{Width: uint8(c.Precision), Scale: uint8(c.Scale), Value: digits}
				}
			}
			if err := a.AppendRow(vals...); err != nil {
				return err
			}
			if n++; n%progressEvery == 0 {
				progress(n)
			}
		}
		return a.Close()
	})
	if err != nil {
		return 0, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return 0, err
	}
	done = true
	return n, nil
}

// ---------------------------------------------------------------- SQLite

func (sqlite) columnTypes(cols []NewColumn) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		switch c.Kind {
		case ColBool:
			out[i] = "BOOLEAN"
		case ColInt, ColBigInt:
			out[i] = "INTEGER"
		case ColDecimal:
			out[i] = fmt.Sprintf("DECIMAL(%d,%d)", c.Precision, c.Scale)
		case ColFloat:
			out[i] = "REAL"
		case ColDate:
			out[i] = "DATE"
		case ColTimestamp:
			out[i] = "TIMESTAMP"
		default:
			out[i] = "TEXT"
		}
	}
	return out
}

func (sqlite) load(ctx context.Context, conn *sql.Conn, t *newTable, next RowFunc, progress func(int64)) (int64, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, t.ddl); err != nil {
		return 0, err
	}
	stmt, err := tx.PrepareContext(ctx, insertSQL(t, 1, questionMark))
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	var n int64
	for {
		row, err := next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
		// Dates are stored as ISO text, which SQLite's date functions read.
		for i, v := range row {
			if tm, ok := v.(time.Time); ok {
				if t.columns[i].Kind == ColDate {
					row[i] = tm.Format(time.DateOnly)
				} else {
					row[i] = timestampText(tm)
				}
			}
		}
		if _, err := stmt.ExecContext(ctx, row...); err != nil {
			return 0, err
		}
		if n++; n%progressEvery == 0 {
			progress(n)
		}
	}
	return n, tx.Commit()
}

// ---------------------------------------------------------------- MySQL

func (mysqlDialect) columnTypes(cols []NewColumn) []string {
	// A row holds at most 65,535 bytes, VARCHAR columns counting for their
	// largest size (4 bytes per character in utf8mb4); TEXT columns for a
	// few bytes only.
	budget := 65535 - 1000
	out := make([]string, len(cols))
	for i, c := range cols {
		switch c.Kind {
		case ColBool:
			out[i] = "BOOLEAN"
		case ColInt:
			out[i] = "INT"
		case ColBigInt:
			out[i] = "BIGINT"
		case ColDecimal:
			out[i] = fmt.Sprintf("DECIMAL(%d,%d)", c.Precision, c.Scale)
		case ColFloat:
			out[i] = "DOUBLE"
		case ColDate:
			out[i] = "DATE"
		case ColTimestamp:
			out[i] = "DATETIME"
			if c.FracDigits > 0 {
				out[i] = fmt.Sprintf("DATETIME(%d)", min(c.FracDigits, 6))
			}
		default:
			size := max(c.MaxChars, 1)*4 + 2
			switch {
			case c.MaxChars <= 255 && size <= budget:
				out[i] = fmt.Sprintf("VARCHAR(%d)", max(c.MaxChars, 1))
				budget -= size
			case c.MaxBytes <= 65535:
				out[i] = "TEXT"
			case c.MaxBytes <= 16777215:
				out[i] = "MEDIUMTEXT"
			default:
				out[i] = "LONGTEXT"
			}
		}
	}
	return out
}

// loadSeq numbers the readers registered for LOAD DATA.
var loadSeq atomic.Int64

// errLocalInfile reports that the server refuses LOAD DATA LOCAL INFILE.
var errLocalInfile = errors.New("LOAD DATA LOCAL INFILE is disabled")

func (mysqlDialect) load(ctx context.Context, conn *sql.Conn, t *newTable, next RowFunc, progress func(int64)) (int64, error) {
	if _, err := conn.ExecContext(ctx, t.ddl); err != nil {
		return 0, err
	}
	t.created = true // DDL commits: the table is dropped on failure
	n, err := mysqlLoadData(ctx, conn, t, next, progress)
	if errors.Is(err, errLocalInfile) {
		return mysqlInsert(ctx, conn, t, next, progress)
	}
	return n, err
}

// mysqlLoadData streams the rows with LOAD DATA LOCAL INFILE, in its
// default text format. It returns errLocalInfile if the server refuses it
// (before any row is read).
func mysqlLoadData(ctx context.Context, conn *sql.Conn, t *newTable, next RowFunc, progress func(int64)) (int64, error) {
	r := &loadDataReader{ctx: ctx, cols: t.columns, next: next, progress: progress}
	name := fmt.Sprintf("dibi-import-%d", loadSeq.Add(1))
	mysql.RegisterReaderHandler(name, func() io.Reader {
		r.started = true
		return r
	})
	defer mysql.DeregisterReaderHandler(name)
	_, err := conn.ExecContext(ctx, "LOAD DATA LOCAL INFILE "+quoteSingle("Reader::"+name)+
		" INTO TABLE "+t.qualified+" CHARACTER SET utf8mb4 ("+strings.Join(t.quoted, ", ")+")")
	if err != nil {
		var me *mysql.MySQLError
		if !r.started && errors.As(err, &me) {
			switch me.Number {
			case 1148, // ER_NOT_ALLOWED_COMMAND
				2068, // CR_LOAD_DATA_LOCAL_INFILE_REJECTED
				3948, // ER_CLIENT_LOCAL_FILES_DISABLED
				3950, // ER_LOAD_DATA_INFILE_FAILED_IN_SECURE_FILE_PRIV
				4166: // MariaDB: ER_LOAD_INFILE_CAPABILITY_DISABLED
				return 0, errLocalInfile
			}
		}
		return 0, err
	}
	// LOCAL turns data errors into warnings, skipping or truncating values:
	// any warning fails the import.
	warnings, err := queryRows(ctx, conn, "SHOW WARNINGS LIMIT 20")
	if err != nil {
		return 0, err
	}
	var msgs []string
	for _, w := range warnings {
		if len(w) >= 3 && !strings.EqualFold(w[0], "Note") {
			msgs = append(msgs, w[2])
		}
	}
	if len(msgs) > 0 {
		return 0, errors.New(strings.Join(msgs, "\n"))
	}
	return r.n, nil
}

// loadDataReader encodes rows in the default format of LOAD DATA: fields
// separated by tabs, lines by newlines, backslash escapes, \N for NULL.
type loadDataReader struct {
	ctx      context.Context
	cols     []NewColumn
	next     RowFunc
	progress func(int64)
	started  bool
	buf      []byte
	pos      int
	n        int64
	eof      bool
}

func (r *loadDataReader) Read(p []byte) (int, error) {
	for r.pos == len(r.buf) {
		if r.eof {
			return 0, io.EOF
		}
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		r.buf, r.pos = r.buf[:0], 0
		// Encode rows until the buffer is worth a write.
		for len(r.buf) < 64*1024 {
			row, err := r.next()
			if err == io.EOF {
				r.eof = true
				break
			}
			if err != nil {
				return 0, err
			}
			r.buf = r.appendRow(r.buf, row)
			if r.n++; r.n%progressEvery == 0 {
				r.progress(r.n)
			}
		}
	}
	n := copy(p, r.buf[r.pos:])
	r.pos += n
	return n, nil
}

func (r *loadDataReader) appendRow(b []byte, row []any) []byte {
	for i, v := range row {
		if i > 0 {
			b = append(b, '\t')
		}
		switch x := v.(type) {
		case nil:
			b = append(b, `\N`...)
		case bool:
			if x {
				b = append(b, '1')
			} else {
				b = append(b, '0')
			}
		case int64:
			b = strconv.AppendInt(b, x, 10)
		case float64:
			b = strconv.AppendFloat(b, x, 'g', -1, 64)
		case time.Time:
			if r.cols[i].Kind == ColDate {
				b = x.AppendFormat(b, time.DateOnly)
			} else {
				b = x.AppendFormat(b, "2006-01-02 15:04:05.999999")
			}
		case string:
			for j := 0; j < len(x); j++ {
				switch c := x[j]; c {
				case '\\':
					b = append(b, `\\`...)
				case '\t':
					b = append(b, `\t`...)
				case '\n':
					b = append(b, `\n`...)
				case '\r':
					b = append(b, `\r`...)
				case 0:
					b = append(b, `\0`...)
				default:
					b = append(b, c)
				}
			}
		default:
			b = append(b, fmt.Sprint(x)...)
		}
	}
	return append(b, '\n')
}

// mysqlInsert loads the rows with multi-row INSERTs in a transaction.
func mysqlInsert(ctx context.Context, conn *sql.Conn, t *newTable, next RowFunc, progress func(int64)) (int64, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	perBatch := max(1, min(1000, 65535/len(t.columns))) // 65,535 placeholders at most
	full, err := tx.PrepareContext(ctx, insertSQL(t, perBatch, questionMark))
	if err != nil {
		return 0, err
	}
	defer full.Close()
	args := make([]any, 0, perBatch*len(t.columns))
	var n int64
	for {
		row, err := next()
		if err != nil && err != io.EOF {
			return 0, err
		}
		if err == nil {
			args = append(args, row...)
			n++
		}
		rows := len(args) / len(t.columns)
		switch {
		case rows == perBatch:
			_, err = full.ExecContext(ctx, args...)
		case err == io.EOF && rows > 0:
			_, err = tx.ExecContext(ctx, insertSQL(t, rows, questionMark), args...)
		case err == io.EOF:
			return n, tx.Commit()
		default:
			continue
		}
		if err != nil {
			return 0, err
		}
		args = args[:0]
		progress(n)
	}
}

// ---------------------------------------------------------------- Oracle

func (oracle) columnTypes(cols []NewColumn) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		switch c.Kind {
		case ColBool:
			out[i] = "NUMBER(1)"
		case ColInt:
			out[i] = "NUMBER(10)"
		case ColBigInt:
			out[i] = "NUMBER(19)"
		case ColDecimal:
			out[i] = fmt.Sprintf("NUMBER(%d,%d)", c.Precision, c.Scale)
		case ColFloat:
			out[i] = "BINARY_DOUBLE"
		case ColDate:
			out[i] = "DATE"
		case ColTimestamp:
			out[i] = "TIMESTAMP"
		default:
			if c.MaxBytes <= 4000 {
				out[i] = fmt.Sprintf("VARCHAR2(%d CHAR)", max(c.MaxChars, 1))
			} else {
				out[i] = "CLOB"
			}
		}
	}
	return out
}

// oracleBatch is the number of rows sent per array-bound INSERT.
const oracleBatch = 1000

func (oracle) load(ctx context.Context, conn *sql.Conn, t *newTable, next RowFunc, progress func(int64)) (int64, error) {
	// Decimals are sent as text: make sure the session reads "." as the
	// decimal separator.
	t.dirty = true
	if _, err := conn.ExecContext(ctx, "ALTER SESSION SET NLS_NUMERIC_CHARACTERS = '.,'"); err != nil {
		return 0, err
	}
	if _, err := conn.ExecContext(ctx, t.ddl); err != nil {
		return 0, err
	}
	t.created = true // DDL commits: the table is dropped on failure
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	query := insertSQL(t, 1, func(i int) string { return ":" + strconv.Itoa(i) })
	// One array per column: each holds the values of the batch.
	arrays := make([][]any, len(t.columns))
	for i := range arrays {
		arrays[i] = make([]any, 0, oracleBatch)
	}
	args := make([]any, len(t.columns))
	var n int64
	flush := func() error {
		if len(arrays[0]) == 0 {
			return nil
		}
		for i := range arrays {
			args[i] = arrays[i]
		}
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return err
		}
		for i := range arrays {
			arrays[i] = arrays[i][:0]
		}
		progress(n)
		return nil
	}
	for {
		row, err := next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
		for i, v := range row {
			switch x := v.(type) {
			case bool:
				v = int64(0)
				if x {
					v = int64(1)
				}
			case time.Time:
				if t.columns[i].Kind == ColTimestamp {
					v = go_ora.TimeStamp(x) // keeps the fractional seconds
				}
			case string:
				if t.columns[i].Kind == ColText && t.columns[i].MaxBytes > 4000 {
					v = go_ora.Clob{String: x, Valid: true}
				}
			}
			arrays[i] = append(arrays[i], v)
		}
		n++
		if len(arrays[0]) == oracleBatch {
			if err := flush(); err != nil {
				return 0, err
			}
		}
	}
	if err := flush(); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}
