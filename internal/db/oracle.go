package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dibi/internal/config"
	"dibi/internal/sqlsplit"

	go_ora "github.com/sijms/go-ora/v2"
)

// oracle: there is no database level; a schema is a user. Database holds
// the service name in the connection settings.
type oracle struct{}

func (oracle) open(c config.Connection, _ string) (*sql.DB, error) {
	url := go_ora.BuildUrl(c.Host, c.Port, c.Database, c.User, c.Password, nil)
	return sql.Open("oracle", url)
}

func (oracle) splitDialect() sqlsplit.Dialect { return sqlsplit.Oracle }
func (oracle) hasDatabases() bool             { return false }
func (oracle) hasSchemas() bool               { return true }
func (oracle) separatePools() bool            { return false }

func (oracle) useDatabase(context.Context, *sql.Conn, string) error { return nil }

func (oracle) groups() []Kind {
	return []Kind{KindTable, KindView, KindMatView, KindFunction, KindProcedure, KindPackage, KindSequence}
}

func (oracle) databases(context.Context, *sql.DB) ([]string, error) { return nil, nil }

// schemas lists the non Oracle-maintained users, with the current user first.
func (oracle) schemas(ctx context.Context, db *sql.DB, _ string) ([]string, error) {
	return queryStrings(ctx, db, `
		SELECT username FROM all_users
		WHERE oracle_maintained = 'N' OR username = USER
		ORDER BY CASE WHEN username = USER THEN 0 ELSE 1 END, username`)
}

func (oracle) objects(ctx context.Context, db *sql.DB, g Node) ([]Node, error) {
	var q string
	switch g.Group {
	case KindTable:
		q = `SELECT table_name FROM all_tables t
		     WHERE owner = :1 AND nested = 'NO' AND secondary = 'N' AND dropped = 'NO'
		       AND (iot_type IS NULL OR iot_type = 'IOT')
		       AND NOT EXISTS (SELECT 1 FROM all_mviews m WHERE m.owner = t.owner AND m.mview_name = t.table_name)
		     ORDER BY table_name`
	case KindView:
		q = `SELECT view_name FROM all_views WHERE owner = :1 ORDER BY view_name`
	case KindMatView:
		q = `SELECT mview_name FROM all_mviews WHERE owner = :1 ORDER BY mview_name`
	case KindSequence:
		q = `SELECT sequence_name FROM all_sequences WHERE sequence_owner = :1 ORDER BY sequence_name`
	case KindFunction, KindProcedure, KindPackage:
		t := map[Kind]string{KindFunction: "FUNCTION", KindProcedure: "PROCEDURE", KindPackage: "PACKAGE"}[g.Group]
		q = `SELECT object_name FROM all_objects WHERE owner = :1 AND object_type = '` + t + `' ORDER BY object_name`
	default:
		return nil, nil
	}
	names, err := queryStrings(ctx, db, q, g.Schema)
	return namedObjects(g.Group, g, names), err
}

func (oracle) tableChildren(ctx context.Context, db *sql.DB, t Node) ([]Node, error) {
	var out []Node
	// LOB indexes are system-generated and not interesting.
	idx, err := queryStrings(ctx, db, `
		SELECT index_name FROM all_indexes
		WHERE table_owner = :1 AND table_name = :2 AND index_type <> 'LOB'
		ORDER BY index_name`, t.Schema, t.Object)
	if err != nil {
		return nil, err
	}
	for _, name := range idx {
		out = append(out, Node{Kind: KindIndex, Name: name, Object: name, Schema: t.Schema, Table: t.Object})
	}
	trg, err := queryStrings(ctx, db, `
		SELECT trigger_name FROM all_triggers
		WHERE table_owner = :1 AND table_name = :2 ORDER BY trigger_name`, t.Schema, t.Object)
	for _, name := range trg {
		out = append(out, Node{Kind: KindTrigger, Name: name, Object: name, Schema: t.Schema, Table: t.Object})
	}
	return out, err
}

func (o oracle) info(ctx context.Context, db *sql.DB, n Node) (*Info, error) {
	qualified := quoteDouble(n.Schema) + "." + quoteDouble(n.Object)
	info := &Info{Title: objectTitle(n, n.Schema+"."+n.Name)}
	var err error
	switch n.Kind {
	case KindTable, KindView, KindMatView:
		if info.Columns, err = o.columns(ctx, db, n); err != nil {
			return nil, err
		}
		info.setSelect(qualified, fetchFirstSyntax)
		switch n.Kind {
		case KindTable:
			if info.Constraints, err = o.constraints(ctx, db, n); err != nil {
				return nil, err
			}
			markPrimaryKey(info)
			info.Indexes, err = o.indexes(ctx, db, n.Schema, n.Object)
		case KindView:
			var text string
			text, err = queryString(ctx, db, `SELECT text FROM all_views WHERE owner = :1 AND view_name = :2`, n.Schema, n.Object)
			info.Source = "CREATE OR REPLACE VIEW " + qualified + " AS\n" + strings.TrimSpace(text)
		case KindMatView:
			var text string
			text, err = queryString(ctx, db, `SELECT query FROM all_mviews WHERE owner = :1 AND mview_name = :2`, n.Schema, n.Object)
			info.Source = "CREATE MATERIALIZED VIEW " + qualified + " AS\n" + strings.TrimSpace(text)
		}
	case KindFunction, KindProcedure, KindPackage:
		info.Source, err = o.source(ctx, db, n)
	case KindSequence:
		var rows [][]string
		rows, err = queryRows(ctx, db, `
			SELECT min_value, max_value, increment_by, cycle_flag, cache_size, last_number
			FROM all_sequences WHERE sequence_owner = :1 AND sequence_name = :2`, n.Schema, n.Object)
		if len(rows) == 1 {
			r := rows[0]
			info.Properties = [][2]string{{"Minimum", r[0]}, {"Maximum", r[1]}, {"Increment", r[2]},
				{"Cycle", r[3]}, {"Cache", r[4]}, {"Last number", r[5]}}
		}
	case KindIndex:
		info.Title = objectTitle(n, n.Schema+"."+n.Name+" on "+n.Table)
		var idx []Index
		idx, err = o.indexes(ctx, db, n.Schema, n.Table)
		for _, i := range idx {
			if i.Name == n.Object {
				info.Source = indexDefinition(quoteDouble(n.Schema)+"."+quoteDouble(i.Name),
					quoteDouble(n.Schema)+"."+quoteDouble(n.Table), i.Unique, i.Columns)
			}
		}
	case KindTrigger:
		info.Title = objectTitle(n, n.Schema+"."+n.Name+" on "+n.Table)
		var rows [][]string
		rows, err = queryRows(ctx, db, `
			SELECT description, trigger_body FROM all_triggers WHERE owner = :1 AND trigger_name = :2`,
			n.Schema, n.Object)
		if len(rows) == 1 {
			info.Source = "CREATE OR REPLACE TRIGGER " + strings.TrimSpace(rows[0][0]) + "\n" + strings.TrimSpace(rows[0][1])
		}
	}
	return info, err
}

// source rebuilds CREATE statements from ALL_SOURCE; a package includes
// its body, separated by the "/" terminator.
func (oracle) source(ctx context.Context, db *sql.DB, n Node) (string, error) {
	types := []string{map[Kind]string{KindFunction: "FUNCTION", KindProcedure: "PROCEDURE", KindPackage: "PACKAGE"}[n.Kind]}
	if n.Kind == KindPackage {
		types = append(types, "PACKAGE BODY")
	}
	var parts []string
	for _, t := range types {
		lines, err := queryStrings(ctx, db, `
			SELECT text FROM all_source WHERE owner = :1 AND name = :2 AND type = :3 ORDER BY line`,
			n.Schema, n.Object, t)
		if err != nil {
			return "", err
		}
		if len(lines) == 0 {
			continue
		}
		parts = append(parts, "CREATE OR REPLACE "+strings.TrimRight(strings.Join(lines, ""), "\n"))
	}
	return strings.Join(parts, "\n/\n\n"), nil
}

func (oracle) columns(ctx context.Context, db *sql.DB, n Node) ([]Column, error) {
	rows, err := queryRows(ctx, db, `
		SELECT c.column_name, c.data_type, c.data_length, c.data_precision, c.data_scale,
		       c.char_length, c.char_used, c.nullable, c.data_default, cm.comments
		FROM all_tab_columns c
		LEFT JOIN all_col_comments cm
		  ON cm.owner = c.owner AND cm.table_name = c.table_name AND cm.column_name = c.column_name
		WHERE c.owner = :1 AND c.table_name = :2
		ORDER BY c.column_id`, n.Schema, n.Object)
	var out []Column
	for _, r := range rows {
		out = append(out, Column{
			Name:     r[0],
			Type:     oracleType(r[1], r[2], r[3], r[4], r[5], r[6]),
			Nullable: r[7] == "Y",
			Default:  strings.TrimSpace(r[8]),
			Comment:  r[9],
		})
	}
	return out, err
}

// oracleType formats a column type with its length/precision.
func oracleType(typ, length, precision, scale, charLength, charUsed string) string {
	switch typ {
	case "VARCHAR2", "NVARCHAR2", "CHAR", "NCHAR":
		unit := ""
		if charUsed == "C" && (typ == "VARCHAR2" || typ == "CHAR") {
			unit = " CHAR"
		}
		return fmt.Sprintf("%s(%s%s)", typ, charLength, unit)
	case "RAW":
		return fmt.Sprintf("RAW(%s)", length)
	case "NUMBER":
		switch {
		case precision == "" && scale == "":
			return "NUMBER"
		case precision == "" && scale == "0":
			return "INTEGER"
		case scale == "" || scale == "0":
			return fmt.Sprintf("NUMBER(%s)", precision)
		default:
			return fmt.Sprintf("NUMBER(%s,%s)", precision, scale)
		}
	case "FLOAT":
		if precision != "" {
			return fmt.Sprintf("FLOAT(%s)", precision)
		}
	}
	return typ
}

func (oracle) constraints(ctx context.Context, db *sql.DB, n Node) ([]Constraint, error) {
	cons, err := queryRows(ctx, db, `
		SELECT constraint_name, constraint_type, search_condition, r_owner, r_constraint_name, generated
		FROM all_constraints
		WHERE owner = :1 AND table_name = :2 AND constraint_type IN ('P', 'U', 'R', 'C')`, n.Schema, n.Object)
	if err != nil {
		return nil, err
	}
	cols, err := consColumns(ctx, db, `owner = :1 AND table_name = :2`, n.Schema, n.Object)
	if err != nil {
		return nil, err
	}
	types := map[string]string{"P": "PRIMARY KEY", "U": "UNIQUE", "R": "FOREIGN KEY", "C": "CHECK"}
	order := map[string]int{"P": 0, "U": 1, "R": 2, "C": 3}
	var out []Constraint
	var sortKeys []int
	for _, r := range cons {
		name, typ, cond := r[0], r[1], r[2]
		// NOT NULL is shown in the columns list; skip the generated checks.
		if typ == "C" && r[5] == "GENERATED NAME" && strings.HasSuffix(strings.TrimSpace(cond), "IS NOT NULL") {
			continue
		}
		c := Constraint{Name: name, Type: types[typ], Columns: strings.Join(cols[name], ", ")}
		switch typ {
		case "C":
			c.Definition = "CHECK (" + cond + ")"
		case "R":
			ref, err := queryRows(ctx, db, `
				SELECT table_name FROM all_constraints WHERE owner = :1 AND constraint_name = :2`, r[3], r[4])
			if err != nil {
				return nil, err
			}
			refCols, err := consColumns(ctx, db, `owner = :1 AND constraint_name = :2`, r[3], r[4])
			if err != nil {
				return nil, err
			}
			if len(ref) == 1 {
				c.Definition = fmt.Sprintf("REFERENCES %s.%s (%s)", r[3], ref[0][0], strings.Join(refCols[r[4]], ", "))
			}
		}
		out = append(out, c)
		sortKeys = append(sortKeys, order[typ])
	}
	sortBy(out, sortKeys)
	return out, nil
}

// consColumns returns constraint columns by constraint name.
func consColumns(ctx context.Context, db *sql.DB, where string, args ...any) (map[string][]string, error) {
	rows, err := queryRows(ctx, db, `
		SELECT constraint_name, column_name FROM all_cons_columns
		WHERE `+where+` ORDER BY constraint_name, position`, args...)
	out := map[string][]string{}
	for _, r := range rows {
		out[r[0]] = append(out[r[0]], r[1])
	}
	return out, err
}

func (oracle) indexes(ctx context.Context, db *sql.DB, owner, table string) ([]Index, error) {
	idx, err := queryRows(ctx, db, `
		SELECT index_name, uniqueness FROM all_indexes
		WHERE table_owner = :1 AND table_name = :2 AND index_type <> 'LOB'
		ORDER BY index_name`, owner, table)
	if err != nil {
		return nil, err
	}
	cols, err := queryRows(ctx, db, `
		SELECT index_name, column_position, column_name, descend FROM all_ind_columns
		WHERE table_owner = :1 AND table_name = :2
		ORDER BY index_name, column_position`, owner, table)
	if err != nil {
		return nil, err
	}
	// Function-based indexes (and some types, e.g. TIMESTAMP WITH TIME ZONE)
	// use hidden SYS_NC…$ columns; the real expression is in ALL_IND_EXPRESSIONS.
	exprRows, err := queryRows(ctx, db, `
		SELECT index_name, column_position, column_expression FROM all_ind_expressions
		WHERE table_owner = :1 AND table_name = :2`, owner, table)
	if err != nil {
		return nil, err
	}
	exprs := map[string]string{}
	for _, r := range exprRows {
		exprs[r[0]+"|"+r[1]] = r[2]
	}
	byIndex := map[string][]string{}
	for _, r := range cols {
		col := r[2]
		if e, ok := exprs[r[0]+"|"+r[1]]; ok {
			col = e
		}
		if r[3] == "DESC" {
			col += " DESC"
		}
		byIndex[r[0]] = append(byIndex[r[0]], col)
	}
	var out []Index
	for _, r := range idx {
		out = append(out, Index{Name: r[0], Unique: r[1] == "UNIQUE", Columns: strings.Join(byIndex[r[0]], ", ")})
	}
	return out, nil
}

// sortBy stably sorts constraints by the given keys.
func sortBy(cs []Constraint, keys []int) {
	for i := 1; i < len(cs); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			cs[j], cs[j-1] = cs[j-1], cs[j]
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
}
