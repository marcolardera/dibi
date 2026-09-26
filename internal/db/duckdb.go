package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"dibi/internal/config"
	"dibi/internal/sqlsplit"

	_ "github.com/duckdb/duckdb-go/v2"
)

type duckdb struct{}

func (duckdb) open(c config.Connection, _ string) (*sql.DB, error) {
	// Refuse to silently create a new empty database on a mistyped path.
	if _, err := os.Stat(c.Path); err != nil {
		return nil, err
	}
	return sql.Open("duckdb", c.Path)
}

func (duckdb) splitDialect() sqlsplit.Dialect { return sqlsplit.Generic }
func (duckdb) hasDatabases() bool             { return true }
func (duckdb) hasSchemas() bool               { return true }
func (duckdb) separatePools() bool            { return false }

func (duckdb) useDatabase(ctx context.Context, conn *sql.Conn, database string) error {
	_, err := conn.ExecContext(ctx, "USE "+quoteDouble(database))
	return err
}

func (duckdb) groups() []Kind { return []Kind{KindTable, KindView} }

func (duckdb) databases(ctx context.Context, db *sql.DB) ([]string, error) {
	return queryStrings(ctx, db, `
		SELECT database_name FROM duckdb_databases() WHERE NOT internal ORDER BY database_name`)
}

func (duckdb) schemas(ctx context.Context, db *sql.DB, database string) ([]string, error) {
	return queryStrings(ctx, db, `
		SELECT schema_name FROM duckdb_schemas()
		WHERE database_name = ? AND schema_name NOT IN ('information_schema', 'pg_catalog')
		ORDER BY schema_name`, database)
}

func (duckdb) objects(ctx context.Context, db *sql.DB, g Node) ([]Node, error) {
	fn := "duckdb_tables()"
	col := "table_name"
	if g.Group == KindView {
		fn, col = "duckdb_views()", "view_name"
	}
	names, err := queryStrings(ctx, db, `
		SELECT `+col+` FROM `+fn+`
		WHERE database_name = ? AND schema_name = ? AND NOT internal
		ORDER BY 1`, g.Database, g.Schema)
	return namedObjects(g.Group, g, names), err
}

func (duckdb) tableChildren(ctx context.Context, db *sql.DB, t Node) ([]Node, error) {
	names, err := queryStrings(ctx, db, `
		SELECT index_name FROM duckdb_indexes()
		WHERE database_name = ? AND schema_name = ? AND table_name = ?
		ORDER BY index_name`, t.Database, t.Schema, t.Object)
	var out []Node
	for _, name := range names {
		out = append(out, Node{Kind: KindIndex, Name: name, Object: name, Database: t.Database, Schema: t.Schema, Table: t.Object})
	}
	return out, err
}

func (d duckdb) info(ctx context.Context, db *sql.DB, n Node) (*Info, error) {
	qualified := quoteDouble(n.Database) + "." + quoteDouble(n.Schema) + "." + quoteDouble(n.Object)
	info := &Info{Title: objectTitle(n, n.Database+"."+n.Schema+"."+n.Name)}
	var err error
	switch n.Kind {
	case KindTable, KindView:
		if info.Columns, err = d.columns(ctx, db, n); err != nil {
			return nil, err
		}
		info.Preview = fmt.Sprintf("SELECT * FROM %s LIMIT %d", qualified, PreviewRows)
		if n.Kind == KindTable {
			if info.Constraints, err = d.constraints(ctx, db, n); err != nil {
				return nil, err
			}
			markPrimaryKey(info)
			info.Indexes, err = d.indexes(ctx, db, n)
		} else {
			info.Source, err = queryString(ctx, db, `
				SELECT sql FROM duckdb_views() WHERE database_name = ? AND schema_name = ? AND view_name = ?`,
				n.Database, n.Schema, n.Object)
		}
	case KindIndex:
		info.Title = objectTitle(n, n.Database+"."+n.Schema+"."+n.Name+" on "+n.Table)
		info.Source, err = queryString(ctx, db, `
			SELECT sql FROM duckdb_indexes() WHERE database_name = ? AND schema_name = ? AND index_name = ?`,
			n.Database, n.Schema, n.Object)
	}
	return info, err
}

func (duckdb) columns(ctx context.Context, db *sql.DB, n Node) ([]Column, error) {
	rows, err := queryRows(ctx, db, `
		SELECT column_name, data_type, is_nullable::VARCHAR, COALESCE(column_default, ''), COALESCE(comment, '')
		FROM duckdb_columns()
		WHERE database_name = ? AND schema_name = ? AND table_name = ?
		ORDER BY column_index`, n.Database, n.Schema, n.Object)
	var out []Column
	for _, r := range rows {
		out = append(out, Column{Name: r[0], Type: r[1], Nullable: truthy(r[2]), Default: r[3], Comment: r[4]})
	}
	return out, err
}

func (duckdb) constraints(ctx context.Context, db *sql.DB, n Node) ([]Constraint, error) {
	rows, err := queryRows(ctx, db, `
		SELECT COALESCE(constraint_name, ''), constraint_type,
		       array_to_string(constraint_column_names, ', '), COALESCE(constraint_text, ''),
		       COALESCE(referenced_table, ''), COALESCE(array_to_string(referenced_column_names, ', '), '')
		FROM duckdb_constraints()
		WHERE database_name = ? AND schema_name = ? AND table_name = ? AND constraint_type <> 'NOT NULL'
		ORDER BY CASE constraint_type WHEN 'PRIMARY KEY' THEN 0 WHEN 'UNIQUE' THEN 1
		                              WHEN 'FOREIGN KEY' THEN 2 ELSE 3 END, constraint_index`,
		n.Database, n.Schema, n.Object)
	var out []Constraint
	for _, r := range rows {
		c := Constraint{Name: r[0], Type: r[1], Columns: r[2]}
		switch r[1] {
		case "FOREIGN KEY":
			c.Definition = fmt.Sprintf("REFERENCES %s (%s)", r[4], r[5])
		case "CHECK":
			c.Definition = r[3]
		}
		out = append(out, c)
	}
	return out, err
}

func (duckdb) indexes(ctx context.Context, db *sql.DB, n Node) ([]Index, error) {
	rows, err := queryRows(ctx, db, `
		SELECT index_name, is_unique::VARCHAR, COALESCE(expressions, '')
		FROM duckdb_indexes()
		WHERE database_name = ? AND schema_name = ? AND table_name = ?
		ORDER BY index_name`, n.Database, n.Schema, n.Object)
	var out []Index
	for _, r := range rows {
		cols := strings.Trim(r[2], "[]")
		out = append(out, Index{Name: r[0], Unique: truthy(r[1]), Columns: cols})
	}
	return out, err
}

// fileDialect exposes a single CSV or Parquet file as a DuckDB view in an
// in-memory database. The view is named after the file.
type fileDialect struct{ duckdb }

func (fileDialect) open(c config.Connection, _ string) (*sql.DB, error) {
	if _, err := os.Stat(c.Path); err != nil {
		return nil, err
	}
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, err
	}
	reader := "read_csv_auto"
	if c.Type == config.Parquet {
		reader = "read_parquet"
	}
	path := "'" + strings.ReplaceAll(c.Path, "'", "''") + "'"
	// All connections of this pool share the same in-memory database.
	_, err = db.Exec(fmt.Sprintf("CREATE VIEW %s AS SELECT * FROM %s(%s)", quoteDouble(FileTableName(c.Path)), reader, path))
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func (fileDialect) hasDatabases() bool { return false }
func (fileDialect) hasSchemas() bool   { return false }

func (fileDialect) useDatabase(context.Context, *sql.Conn, string) error { return nil }

// root returns the single table node of a file connection.
func (fileDialect) root(c config.Connection) []Node {
	name := FileTableName(c.Path)
	return []Node{{Kind: KindView, Name: name, Object: name, Database: "memory", Schema: "main"}}
}

func (f fileDialect) info(ctx context.Context, db *sql.DB, n Node) (*Info, error) {
	info, err := f.duckdb.info(ctx, db, n)
	if err != nil {
		return nil, err
	}
	info.Title = "File " + n.Name
	info.Source = ""
	info.Preview = fmt.Sprintf("SELECT * FROM %s LIMIT %d", quoteDouble(n.Object), PreviewRows)
	return info, nil
}

// FileTableName returns the name under which a CSV/Parquet file is queried:
// the file name without extension, with unusual characters replaced by "_".
func FileTableName(path string) string {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	var b strings.Builder
	for _, r := range base {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	name := b.String()
	if name == "" || unicode.IsDigit([]rune(name)[0]) {
		name = "t_" + name
	}
	return name
}
