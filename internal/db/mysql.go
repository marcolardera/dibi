package db

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"dibi/internal/config"
	"dibi/internal/sqlsplit"

	"github.com/go-sql-driver/mysql"
)

// mysqlDialect serves both MySQL and MariaDB. A "database" is a schema.
type mysqlDialect struct{}

func (mysqlDialect) open(c config.Connection, _ string) (*sql.DB, error) {
	cfg := mysql.NewConfig()
	cfg.User = c.User
	cfg.Passwd = c.Password
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	cfg.DBName = c.Database
	cfg.ParseTime = true
	tlsCfg, err := tlsConfig(c, c.Host)
	if err != nil {
		return nil, err
	}
	cfg.TLS = tlsCfg
	cfg.AllowFallbackToPlaintext = c.TLSMode() == config.SSLPrefer
	conn, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(conn), nil
}

func (mysqlDialect) splitDialect() sqlsplit.Dialect { return sqlsplit.MySQL }
func (mysqlDialect) hasDatabases() bool             { return true }
func (mysqlDialect) hasSchemas() bool               { return false }
func (mysqlDialect) separatePools() bool            { return false }

func quoteBacktick(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

func (mysqlDialect) useDatabase(ctx context.Context, conn *sql.Conn, database string) error {
	_, err := conn.ExecContext(ctx, "USE "+quoteBacktick(database))
	return err
}

// sessionInterrupt returns a function killing the running query of conn
// from another connection. The driver would otherwise close the connection
// on cancellation, losing the session state.
func (mysqlDialect) sessionInterrupt(ctx context.Context, pool *sql.DB, conn *sql.Conn) (func(), error) {
	var id int64
	if err := conn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&id); err != nil {
		return nil, err
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		pool.ExecContext(ctx, fmt.Sprintf("KILL QUERY %d", id))
	}, nil
}

func (mysqlDialect) groups() []Kind {
	return []Kind{KindTable, KindView, KindFunction, KindProcedure}
}

func (mysqlDialect) databases(ctx context.Context, db *sql.DB) ([]string, error) {
	return queryStrings(ctx, db, `
		SELECT schema_name FROM information_schema.schemata
		WHERE schema_name NOT IN ('information_schema', 'performance_schema', 'mysql', 'sys')
		ORDER BY schema_name`)
}

func (mysqlDialect) schemas(context.Context, *sql.DB, string) ([]string, error) { return nil, nil }

func (mysqlDialect) objects(ctx context.Context, db *sql.DB, g Node) ([]Node, error) {
	var names []string
	var err error
	switch g.Group {
	case KindTable, KindView:
		tt := "BASE TABLE"
		if g.Group == KindView {
			tt = "VIEW"
		}
		names, err = queryStrings(ctx, db, `
			SELECT table_name FROM information_schema.tables
			WHERE table_schema = ? AND table_type = ? ORDER BY table_name`, g.Database, tt)
	case KindFunction, KindProcedure:
		rt := "FUNCTION"
		if g.Group == KindProcedure {
			rt = "PROCEDURE"
		}
		names, err = queryStrings(ctx, db, `
			SELECT routine_name FROM information_schema.routines
			WHERE routine_schema = ? AND routine_type = ? ORDER BY routine_name`, g.Database, rt)
	}
	return namedObjects(g.Group, g, names), err
}

func (mysqlDialect) tableChildren(ctx context.Context, db *sql.DB, t Node) ([]Node, error) {
	var out []Node
	idx, err := queryStrings(ctx, db, `
		SELECT DISTINCT index_name FROM information_schema.statistics
		WHERE table_schema = ? AND table_name = ? ORDER BY index_name`, t.Database, t.Object)
	if err != nil {
		return nil, err
	}
	for _, name := range idx {
		out = append(out, Node{Kind: KindIndex, Name: name, Object: name, Database: t.Database, Table: t.Object})
	}
	trg, err := queryStrings(ctx, db, `
		SELECT trigger_name FROM information_schema.triggers
		WHERE event_object_schema = ? AND event_object_table = ? ORDER BY trigger_name`, t.Database, t.Object)
	for _, name := range trg {
		out = append(out, Node{Kind: KindTrigger, Name: name, Object: name, Database: t.Database, Table: t.Object})
	}
	return out, err
}

func (m mysqlDialect) info(ctx context.Context, db *sql.DB, n Node) (*Info, error) {
	qualified := quoteBacktick(n.Database) + "." + quoteBacktick(n.Object)
	info := &Info{Title: objectTitle(n, n.Database+"."+n.Name)}
	var err error
	switch n.Kind {
	case KindTable, KindView:
		if info.Columns, err = m.columns(ctx, db, n); err != nil {
			return nil, err
		}
		info.setSelect(qualified, limitSyntax)
		if n.Kind == KindTable {
			if info.Constraints, err = m.constraints(ctx, db, n); err != nil {
				return nil, err
			}
			markPrimaryKey(info)
			info.Indexes, err = m.indexes(ctx, db, n)
		} else {
			info.Source, err = showCreate(ctx, db, "SHOW CREATE VIEW "+qualified, 1)
			info.EditScript = orReplace(info.Source)
		}
	case KindProcedure:
		info.Source, err = showCreate(ctx, db, "SHOW CREATE PROCEDURE "+qualified, 2)
	case KindFunction:
		info.Source, err = showCreate(ctx, db, "SHOW CREATE FUNCTION "+qualified, 2)
	case KindTrigger:
		info.Title = objectTitle(n, n.Database+"."+n.Name+" on "+n.Table)
		info.Source, err = showCreate(ctx, db, "SHOW CREATE TRIGGER "+qualified, 2)
	case KindIndex:
		info.Title = objectTitle(n, n.Name+" on "+n.Database+"."+n.Table)
		var idx []Index
		idx, err = m.indexes(ctx, db, Node{Database: n.Database, Object: n.Table})
		for _, i := range idx {
			if i.Name == n.Object {
				info.Source = indexDefinition(quoteBacktick(i.Name), quoteBacktick(n.Database)+"."+quoteBacktick(n.Table), i.Unique, i.Columns)
			}
		}
	}
	return info, err
}

// showCreate runs a SHOW CREATE statement and returns column col.
func showCreate(ctx context.Context, db *sql.DB, stmt string, col int) (string, error) {
	rows, err := queryRows(ctx, db, stmt)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 || len(rows[0]) <= col {
		return "", nil
	}
	if rows[0][col] == "" {
		return "", fmt.Errorf("source not available (insufficient privileges?)")
	}
	return rows[0][col], nil
}

func (mysqlDialect) columns(ctx context.Context, db *sql.DB, n Node) ([]Column, error) {
	rows, err := queryRows(ctx, db, `
		SELECT column_name, column_type, is_nullable, column_default, column_comment, extra
		FROM information_schema.columns
		WHERE table_schema = ? AND table_name = ? ORDER BY ordinal_position`, n.Database, n.Object)
	var out []Column
	for _, r := range rows {
		def := r[3]
		if def == "NULL" { // MariaDB reports a NULL default as the string NULL
			def = ""
		}
		if r[5] != "" {
			def = strings.TrimSpace(def + " " + r[5]) // e.g. auto_increment, on update …
		}
		out = append(out, Column{Name: r[0], Type: r[1], Nullable: r[2] == "YES", Default: def, Comment: r[4]})
	}
	return out, err
}

func (mysqlDialect) constraints(ctx context.Context, db *sql.DB, n Node) ([]Constraint, error) {
	rows, err := queryRows(ctx, db, `
		SELECT tc.constraint_name, tc.constraint_type,
		       COALESCE(GROUP_CONCAT(kcu.column_name ORDER BY kcu.ordinal_position SEPARATOR ', '), ''),
		       COALESCE(MAX(kcu.referenced_table_schema), ''),
		       COALESCE(MAX(kcu.referenced_table_name), ''),
		       COALESCE(GROUP_CONCAT(kcu.referenced_column_name ORDER BY kcu.ordinal_position SEPARATOR ', '), ''),
		       COALESCE(MAX(cc.check_clause), '')
		FROM information_schema.table_constraints tc
		LEFT JOIN information_schema.key_column_usage kcu
		  ON kcu.constraint_schema = tc.constraint_schema AND kcu.constraint_name = tc.constraint_name
		 AND kcu.table_name = tc.table_name
		LEFT JOIN information_schema.check_constraints cc
		  ON cc.constraint_schema = tc.constraint_schema AND cc.constraint_name = tc.constraint_name
		WHERE tc.table_schema = ? AND tc.table_name = ?
		GROUP BY tc.constraint_name, tc.constraint_type
		ORDER BY FIELD(tc.constraint_type, 'PRIMARY KEY', 'UNIQUE', 'FOREIGN KEY', 'CHECK'), tc.constraint_name`,
		n.Database, n.Object)
	var out []Constraint
	for _, r := range rows {
		c := Constraint{Name: r[0], Type: r[1], Columns: r[2]}
		switch r[1] {
		case "FOREIGN KEY":
			c.Definition = fmt.Sprintf("REFERENCES %s.%s (%s)", r[3], r[4], r[5])
		case "CHECK":
			c.Definition = "CHECK (" + r[6] + ")"
		}
		out = append(out, c)
	}
	return out, err
}

func (mysqlDialect) indexes(ctx context.Context, db *sql.DB, n Node) ([]Index, error) {
	rows, err := queryRows(ctx, db, `
		SELECT index_name, MIN(non_unique),
		       GROUP_CONCAT(COALESCE(column_name, '(expression)') ORDER BY seq_in_index SEPARATOR ', ')
		FROM information_schema.statistics
		WHERE table_schema = ? AND table_name = ?
		GROUP BY index_name ORDER BY index_name`, n.Database, n.Object)
	var out []Index
	for _, r := range rows {
		out = append(out, Index{Name: r[0], Unique: r[1] == "0", Columns: r[2]})
	}
	return out, err
}

// setAutocommit: turning autocommit on commits the open transaction.
func (mysqlDialect) setAutocommit(ctx context.Context, conn *sql.Conn, on bool) error {
	q := "SET autocommit = 0"
	if on {
		q = "SET autocommit = 1"
	}
	_, err := conn.ExecContext(ctx, q)
	return err
}
