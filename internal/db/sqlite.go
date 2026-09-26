package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"

	"dibi/internal/config"
	"dibi/internal/sqlsplit"

	_ "modernc.org/sqlite"
)

// sqlite: databases are "main" plus attached ones; there is no schema level.
type sqlite struct{}

func (sqlite) open(c config.Connection, _ string) (*sql.DB, error) {
	// Refuse to silently create a new empty database on a mistyped path.
	if _, err := os.Stat(c.Path); err != nil {
		return nil, err
	}
	dsn := "file:" + (&url.URL{Path: c.Path}).EscapedPath() +
		"?mode=rw&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	return sql.Open("sqlite", dsn)
}

func (sqlite) splitDialect() sqlsplit.Dialect { return sqlsplit.Generic }
func (sqlite) hasDatabases() bool             { return true }
func (sqlite) hasSchemas() bool               { return false }
func (sqlite) separatePools() bool            { return false }

// useDatabase is a no-op: SQLite has no "current database"; unqualified
// names resolve to main (or temp) first.
func (sqlite) useDatabase(context.Context, *sql.Conn, string) error { return nil }

func (sqlite) groups() []Kind { return []Kind{KindTable, KindView} }

func (sqlite) databases(ctx context.Context, db *sql.DB) ([]string, error) {
	return queryStrings(ctx, db, `SELECT name FROM pragma_database_list WHERE name <> 'temp' ORDER BY seq`)
}

func (sqlite) schemas(context.Context, *sql.DB, string) ([]string, error) { return nil, nil }

func (sqlite) objects(ctx context.Context, db *sql.DB, g Node) ([]Node, error) {
	typ := "table"
	if g.Group == KindView {
		typ = "view"
	}
	names, err := queryStrings(ctx, db, fmt.Sprintf(`
		SELECT name FROM %s.sqlite_master
		WHERE type = ? AND name NOT LIKE 'sqlite\_%%' ESCAPE '\'
		ORDER BY name`, quoteDouble(g.Database)), typ)
	return namedObjects(g.Group, g, names), err
}

func (sqlite) tableChildren(ctx context.Context, db *sql.DB, t Node) ([]Node, error) {
	rows, err := queryRows(ctx, db, fmt.Sprintf(`
		SELECT type, name FROM %s.sqlite_master
		WHERE type IN ('index', 'trigger') AND tbl_name = ?
		ORDER BY type, name`, quoteDouble(t.Database)), t.Object)
	var out []Node
	for _, r := range rows {
		k := KindIndex
		if r[0] == "trigger" {
			k = KindTrigger
		}
		out = append(out, Node{Kind: k, Name: r[1], Object: r[1], Database: t.Database, Table: t.Object})
	}
	return out, err
}

func (s sqlite) info(ctx context.Context, db *sql.DB, n Node) (*Info, error) {
	qualified := quoteDouble(n.Database) + "." + quoteDouble(n.Object)
	info := &Info{Title: objectTitle(n, n.Database+"."+n.Name)}
	var err error
	switch n.Kind {
	case KindTable, KindView:
		if info.Columns, err = s.columns(ctx, db, n); err != nil {
			return nil, err
		}
		info.Preview = fmt.Sprintf("SELECT * FROM %s LIMIT %d", qualified, PreviewRows)
		if n.Kind == KindTable {
			if info.Constraints, err = s.constraints(ctx, db, n, info.Columns); err != nil {
				return nil, err
			}
			info.Indexes, err = s.indexes(ctx, db, n.Database, n.Object)
		} else {
			info.Source, err = s.sql(ctx, db, n.Database, n.Object)
		}
	case KindTrigger:
		info.Title = objectTitle(n, n.Database+"."+n.Name+" on "+n.Table)
		info.Source, err = s.sql(ctx, db, n.Database, n.Object)
	case KindIndex:
		info.Title = objectTitle(n, n.Database+"."+n.Name+" on "+n.Table)
		info.Source, err = s.sql(ctx, db, n.Database, n.Object)
		if err == nil && info.Source == "" {
			// Automatic indexes (PRIMARY KEY / UNIQUE) have no SQL text.
			var idx []Index
			idx, err = s.indexes(ctx, db, n.Database, n.Table)
			for _, i := range idx {
				if i.Name == n.Object {
					info.Source = "-- automatic index\n" + indexDefinition(quoteDouble(i.Name), quoteDouble(n.Table), i.Unique, i.Columns)
				}
			}
		}
	}
	return info, err
}

func (sqlite) sql(ctx context.Context, db *sql.DB, database, name string) (string, error) {
	return queryString(ctx, db, fmt.Sprintf(`SELECT sql FROM %s.sqlite_master WHERE name = ?`, quoteDouble(database)), name)
}

func (sqlite) columns(ctx context.Context, db *sql.DB, n Node) ([]Column, error) {
	rows, err := queryRows(ctx, db, `
		SELECT name, type, "notnull", dflt_value, pk FROM pragma_table_xinfo(?, ?)
		WHERE hidden <> 1 ORDER BY cid`, n.Object, n.Database)
	var out []Column
	pkCols := 0
	for _, r := range rows {
		if r[4] != "0" {
			pkCols++
		}
		out = append(out, Column{Name: r[0], Type: r[1], Nullable: r[2] == "0", Default: r[3], PrimaryKey: r[4] != "0"})
	}
	// A single INTEGER PRIMARY KEY is the rowid and can never be NULL.
	for i := range out {
		if out[i].PrimaryKey && pkCols == 1 && strings.EqualFold(out[i].Type, "INTEGER") {
			out[i].Nullable = false
		}
	}
	return out, err
}

func (sqlite) constraints(ctx context.Context, db *sql.DB, n Node, cols []Column) ([]Constraint, error) {
	var out []Constraint
	pk, err := queryStrings(ctx, db,
		`SELECT name FROM pragma_table_info(?, ?) WHERE pk > 0 ORDER BY pk`, n.Object, n.Database)
	if err != nil {
		return nil, err
	}
	if len(pk) > 0 {
		out = append(out, Constraint{Name: "", Type: "PRIMARY KEY", Columns: strings.Join(pk, ", ")})
	}
	uniq, err := queryRows(ctx, db, `
		SELECT il.name, (SELECT group_concat(ii.name, ', ') FROM pragma_index_info(il.name, ?) ii)
		FROM pragma_index_list(?, ?) il WHERE il.origin = 'u' ORDER BY il.name`, n.Database, n.Object, n.Database)
	if err != nil {
		return nil, err
	}
	for _, r := range uniq {
		out = append(out, Constraint{Name: r[0], Type: "UNIQUE", Columns: r[1]})
	}
	fks, err := queryRows(ctx, db, `
		SELECT id, "table", "from", COALESCE("to", ''), on_update, on_delete
		FROM pragma_foreign_key_list(?, ?) ORDER BY id, seq`, n.Object, n.Database)
	if err != nil {
		return nil, err
	}
	var from, to []string
	for i, r := range fks {
		from = append(from, r[2])
		to = append(to, r[3])
		if i == len(fks)-1 || fks[i+1][0] != r[0] {
			def := fmt.Sprintf("REFERENCES %s (%s)", r[1], strings.Join(to, ", "))
			if r[5] != "NO ACTION" {
				def += " ON DELETE " + r[5]
			}
			if r[4] != "NO ACTION" {
				def += " ON UPDATE " + r[4]
			}
			out = append(out, Constraint{Type: "FOREIGN KEY", Columns: strings.Join(from, ", "), Definition: def})
			from, to = nil, nil
		}
	}
	return out, nil
}

func (sqlite) indexes(ctx context.Context, db *sql.DB, database, table string) ([]Index, error) {
	rows, err := queryRows(ctx, db, `
		SELECT il.name, il."unique",
		       (SELECT group_concat(COALESCE(ii.name, '(expression)'), ', ') FROM pragma_index_info(il.name, ?) ii)
		FROM pragma_index_list(?, ?) il ORDER BY il.name`, database, table, database)
	var out []Index
	for _, r := range rows {
		out = append(out, Index{Name: r[0], Unique: r[1] == "1", Columns: r[2]})
	}
	return out, err
}
