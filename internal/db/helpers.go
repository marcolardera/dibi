package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// queryer is implemented by *sql.DB and *sql.Conn.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// queryRows runs a query and returns every row as strings. NULL becomes "".
func queryRows(ctx context.Context, q queryer, query string, args ...any) ([][]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out [][]string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make([]string, len(cols))
		for i, v := range vals {
			row[i] = str(v)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// queryStrings returns the first column of every row.
func queryStrings(ctx context.Context, q queryer, query string, args ...any) ([]string, error) {
	rows, err := queryRows(ctx, q, query, args...)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r[0]
	}
	return out, nil
}

// queryString returns the first column of the first row, or "" if none.
func queryString(ctx context.Context, q queryer, query string, args ...any) (string, error) {
	rows, err := queryRows(ctx, q, query, args...)
	if err != nil || len(rows) == 0 {
		return "", err
	}
	return rows[0][0], nil
}

func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(x)
	case string:
		return x
	default:
		return fmt.Sprint(x)
	}
}

func truthy(s string) bool {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "1", "T", "TRUE", "Y", "YES":
		return true
	}
	return false
}

// quoteDouble quotes an identifier with double quotes (ANSI).
func quoteDouble(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// namedObjects builds leaf nodes of a kind from names.
func namedObjects(k Kind, parent Node, names []string) []Node {
	out := make([]Node, len(names))
	for i, n := range names {
		out[i] = Node{Kind: k, Name: n, Object: n, Database: parent.Database, Schema: parent.Schema}
	}
	return out
}

// markPrimaryKey sets PrimaryKey on the columns listed in the PK constraint.
func markPrimaryKey(info *Info) {
	for _, c := range info.Constraints {
		if c.Type != "PRIMARY KEY" {
			continue
		}
		for _, name := range strings.Split(c.Columns, ",") {
			name = strings.TrimSpace(name)
			for i := range info.Columns {
				if info.Columns[i].Name == name {
					info.Columns[i].PrimaryKey = true
				}
			}
		}
	}
}

// indexDefinition builds a readable CREATE INDEX statement.
func indexDefinition(name, table string, unique bool, columns string) string {
	u := ""
	if unique {
		u = "UNIQUE "
	}
	return fmt.Sprintf("CREATE %sINDEX %s ON %s (%s)", u, name, table, columns)
}

// objectTitle builds the title of an info panel.
func objectTitle(n Node, qualified string) string {
	return KindLabel(n.Kind) + " " + qualified
}
