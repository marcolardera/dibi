package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
		info.setSelect(qualified, limitSyntax)
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
			info.EditScript = orReplace(info.Source)
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

// fileDialect exposes CSV or Parquet files as DuckDB views in an in-memory
// database. The path is either a single file or a folder, whose files
// (not those of subfolders) each become a view named after the file. The
// view's comment holds the path of its file.
type fileDialect struct{ duckdb }

func (fileDialect) open(c config.Connection, _ string) (*sql.DB, error) {
	fi, err := os.Stat(c.Path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, err
	}
	// All connections of this pool share the same in-memory database.
	_, err = syncFileViews(context.Background(), db, c)
	if err != nil && !fi.IsDir() {
		// A folder stays usable when some of its files cannot be read; the
		// tree reports them.
		db.Close()
		return nil, err
	}
	return db, nil
}

func (fileDialect) hasDatabases() bool { return false }
func (fileDialect) hasSchemas() bool   { return false }

func (fileDialect) useDatabase(context.Context, *sql.Conn, string) error { return nil }

// root rescans the files, so that refreshing the connection picks up files
// added or removed since, and returns their views.
func (fileDialect) root(ctx context.Context, db *sql.DB, c config.Connection) ([]Node, error) {
	return syncFileViews(ctx, db, c)
}

// syncFileViews makes the views match the files of the connection: it
// creates the missing ones and drops those whose file is gone. It returns
// the views, and an error listing the files that could not be read.
func syncFileViews(ctx context.Context, db *sql.DB, c config.Connection) ([]Node, error) {
	files, err := dataFiles(c)
	if err != nil {
		return nil, err
	}
	rows, err := queryRows(ctx, db, `
		SELECT view_name, COALESCE(comment, '') FROM duckdb_views()
		WHERE database_name = 'memory' AND schema_name = 'main' AND NOT internal`)
	if err != nil {
		return nil, err
	}
	// Names are compared in lower case, as DuckDB does. Views are dropped
	// before creating others, which may take a name differing only in case.
	names := fileViewNames(files)
	wanted := map[string]string{} // lower-case view name → file
	for i, f := range files {
		wanted[strings.ToLower(names[i])] = f
	}
	kept := map[string]bool{}
	var errs []error
	for _, r := range rows {
		if lower := strings.ToLower(r[0]); wanted[lower] == r[1] {
			kept[lower] = true
		} else if _, err := db.ExecContext(ctx, "DROP VIEW IF EXISTS "+quoteDouble(r[0])); err != nil {
			errs = append(errs, err)
		}
	}
	var out []Node
	for i, f := range files {
		name := names[i]
		if !kept[strings.ToLower(name)] {
			quoted, path := quoteDouble(name), quoteSingle(f)
			_, err := db.ExecContext(ctx, fmt.Sprintf("CREATE VIEW %s AS SELECT * FROM %s", quoted, fileReader(c, path)))
			if err == nil {
				_, err = db.ExecContext(ctx, fmt.Sprintf("COMMENT ON VIEW %s IS %s", quoted, path))
			}
			if err != nil {
				db.ExecContext(ctx, "DROP VIEW IF EXISTS "+quoted)
				errs = append(errs, fmt.Errorf("%s: %w", filepath.Base(f), err))
				continue
			}
		}
		out = append(out, Node{Kind: KindView, Name: name, Object: name, Database: "memory", Schema: "main"})
	}
	slices.SortFunc(out, func(a, b Node) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	return out, errors.Join(errs...)
}

// fileReader returns the table function call reading a file, given as a
// quoted literal.
func fileReader(c config.Connection, path string) string {
	if c.Type == config.Parquet {
		return "read_parquet(" + path + ")"
	}
	args := path
	if c.Separator != "" {
		args += ", delim = " + quoteSingle(c.Separator)
	}
	if c.Quote != "" {
		args += ", quote = " + quoteSingle(c.Quote)
	}
	return "read_csv_auto(" + args + ")"
}

// dataFiles returns the path of a file connection, or the files of its
// folder with an extension of the connection type, sorted by name.
func dataFiles(c config.Connection) ([]string, error) {
	fi, err := os.Stat(c.Path)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		if c.Type == config.CSV && !hasDataExt(c.Type, c.Path) {
			return nil, fmt.Errorf("%s is not a .csv file", filepath.Base(c.Path))
		}
		return []string{c.Path}, nil
	}
	entries, err := os.ReadDir(c.Path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || !hasDataExt(c.Type, name) {
			continue
		}
		path := filepath.Join(c.Path, name)
		if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() { // follows symlinks
			continue
		}
		out = append(out, path)
	}
	return out, nil
}

// DataExtension returns the extension of the files read by a CSV or
// Parquet connection.
func DataExtension(t config.Type) string {
	if t == config.Parquet {
		return ".parquet"
	}
	return ".csv"
}

func hasDataExt(t config.Type, name string) bool {
	return strings.EqualFold(filepath.Ext(name), DataExtension(t))
}

// fileViewNames returns the view name of each file. DuckDB names are case
// insensitive, so names differing only in case get a numeric suffix.
func fileViewNames(files []string) []string {
	taken := map[string]bool{}
	out := make([]string, len(files))
	for i, f := range files {
		base := FileTableName(f)
		name := base
		for n := 2; taken[strings.ToLower(name)]; n++ {
			name = fmt.Sprintf("%s_%d", base, n)
		}
		taken[strings.ToLower(name)] = true
		out[i] = name
	}
	return out
}

func (f fileDialect) info(ctx context.Context, db *sql.DB, n Node) (*Info, error) {
	info, err := f.duckdb.info(ctx, db, n)
	if err != nil {
		return nil, err
	}
	info.Title = "File " + n.Name
	info.Source, info.EditScript = "", ""
	info.setSelect(quoteDouble(n.Object), limitSyntax)
	path, err := queryString(ctx, db, `
		SELECT COALESCE(comment, '') FROM duckdb_views() WHERE database_name = ? AND schema_name = ? AND view_name = ?`,
		n.Database, n.Schema, n.Object)
	if path != "" {
		info.Properties = append(info.Properties, [2]string{"File", path})
	}
	return info, err
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
