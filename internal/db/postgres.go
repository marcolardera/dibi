package db

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"dibi/internal/config"
	"dibi/internal/sqlsplit"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
	"github.com/jackc/pgx/v5/stdlib"
)

type postgres struct{}

func (postgres) open(c config.Connection, database string) (*sql.DB, error) {
	if database == "" {
		database = c.Database
	}
	if database == "" {
		database = "postgres"
	}
	cfg, err := pgx.ParseConfig("")
	if err != nil {
		return nil, err
	}
	cfg.Host = c.Host
	cfg.Port = uint16(c.Port)
	cfg.User = c.User
	cfg.Password = c.Password
	cfg.Database = database
	cfg.RuntimeParams["application_name"] = "dibi"
	// On cancellation, ask the server to cancel the query instead of closing
	// the connection, so that the session state of an editor tab survives
	// Stop. The deadline only kicks in if the server does not respond.
	cfg.BuildContextWatcherHandler = func(c *pgconn.PgConn) ctxwatch.Handler {
		return &pgconn.CancelRequestContextWatcherHandler{Conn: c, DeadlineDelay: 10 * time.Second}
	}
	return stdlib.OpenDB(*cfg), nil
}

func (postgres) splitDialect() sqlsplit.Dialect { return sqlsplit.Postgres }
func (postgres) hasDatabases() bool             { return true }
func (postgres) hasSchemas() bool               { return true }
func (postgres) separatePools() bool            { return true }

func (postgres) useDatabase(context.Context, *sql.Conn, string) error { return nil }

func (postgres) groups() []Kind {
	return []Kind{KindTable, KindView, KindMatView, KindFunction, KindProcedure, KindSequence}
}

func (postgres) databases(ctx context.Context, db *sql.DB) ([]string, error) {
	return queryStrings(ctx, db,
		`SELECT datname FROM pg_database WHERE datallowconn AND NOT datistemplate ORDER BY datname`)
}

func (postgres) schemas(ctx context.Context, db *sql.DB, _ string) ([]string, error) {
	return queryStrings(ctx, db, `
		SELECT nspname FROM pg_namespace
		WHERE nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast')
		  AND nspname NOT LIKE 'pg_temp_%' AND nspname NOT LIKE 'pg_toast_temp_%'
		ORDER BY nspname`)
}

func (postgres) objects(ctx context.Context, db *sql.DB, g Node) ([]Node, error) {
	relkinds := map[Kind]string{KindTable: "'r','p','f'", KindView: "'v'", KindMatView: "'m'", KindSequence: "'S'"}
	if rk, ok := relkinds[g.Group]; ok {
		names, err := queryStrings(ctx, db, `
			SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1 AND c.relkind IN (`+rk+`) AND NOT c.relispartition
			ORDER BY c.relname`, g.Schema)
		return namedObjects(g.Group, g, names), err
	}
	prokind := "f"
	if g.Group == KindProcedure {
		prokind = "p"
	}
	rows, err := queryRows(ctx, db, `
		SELECT p.oid::text, p.proname, pg_get_function_identity_arguments(p.oid)
		FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = $1 AND p.prokind = $2
		ORDER BY p.proname, 3`, g.Schema, prokind)
	var out []Node
	for _, r := range rows {
		out = append(out, Node{Kind: g.Group, Name: r[1] + "(" + r[2] + ")", Object: r[1], ID: r[0],
			Database: g.Database, Schema: g.Schema})
	}
	return out, err
}

func (postgres) tableChildren(ctx context.Context, db *sql.DB, t Node) ([]Node, error) {
	var out []Node
	idx, err := queryStrings(ctx, db,
		`SELECT indexname FROM pg_indexes WHERE schemaname = $1 AND tablename = $2 ORDER BY indexname`,
		t.Schema, t.Object)
	if err != nil {
		return nil, err
	}
	for _, name := range idx {
		out = append(out, Node{Kind: KindIndex, Name: name, Object: name, Database: t.Database, Schema: t.Schema, Table: t.Object})
	}
	trg, err := queryStrings(ctx, db, `
		SELECT tg.tgname FROM pg_trigger tg
		JOIN pg_class c ON c.oid = tg.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2 AND NOT tg.tgisinternal
		ORDER BY tg.tgname`, t.Schema, t.Object)
	for _, name := range trg {
		out = append(out, Node{Kind: KindTrigger, Name: name, Object: name, Database: t.Database, Schema: t.Schema, Table: t.Object})
	}
	return out, err
}

func (p postgres) info(ctx context.Context, db *sql.DB, n Node) (*Info, error) {
	qualified := quoteDouble(n.Schema) + "." + quoteDouble(n.Object)
	info := &Info{Title: objectTitle(n, n.Schema+"."+n.Name)}
	var err error
	switch n.Kind {
	case KindTable, KindView, KindMatView:
		if info.Columns, err = p.columns(ctx, db, n); err != nil {
			return nil, err
		}
		info.setSelect(qualified, limitSyntax)
		switch n.Kind {
		case KindTable:
			if info.Constraints, err = p.constraints(ctx, db, n); err != nil {
				return nil, err
			}
			markPrimaryKey(info)
			info.Indexes, err = p.indexes(ctx, db, n)
		case KindView:
			var def string
			def, err = queryString(ctx, db, `SELECT pg_get_viewdef(to_regclass($1), true)`, qualified)
			info.Source = "CREATE OR REPLACE VIEW " + qualified + " AS\n" + def
		case KindMatView:
			var def string
			def, err = queryString(ctx, db, `SELECT pg_get_viewdef(to_regclass($1), true)`, qualified)
			info.Source = "CREATE MATERIALIZED VIEW " + qualified + " AS\n" + def
		}
	case KindFunction, KindProcedure:
		info.Source, err = queryString(ctx, db, `SELECT pg_get_functiondef($1::oid)`, n.ID)
	case KindSequence:
		var rows [][]string
		rows, err = queryRows(ctx, db, `
			SELECT data_type::text, start_value::text, min_value::text, max_value::text,
			       increment_by::text, cycle::text, cache_size::text, COALESCE(last_value::text, '')
			FROM pg_sequences WHERE schemaname = $1 AND sequencename = $2`, n.Schema, n.Object)
		if len(rows) == 1 {
			r := rows[0]
			info.Properties = [][2]string{{"Data type", r[0]}, {"Start value", r[1]}, {"Minimum", r[2]},
				{"Maximum", r[3]}, {"Increment", r[4]}, {"Cycle", r[5]}, {"Cache", r[6]}, {"Last value", r[7]}}
		}
	case KindIndex:
		info.Title = objectTitle(n, n.Schema+"."+n.Name+" on "+n.Table)
		info.Source, err = queryString(ctx, db,
			`SELECT indexdef FROM pg_indexes WHERE schemaname = $1 AND indexname = $2`, n.Schema, n.Object)
	case KindTrigger:
		info.Title = objectTitle(n, n.Name+" on "+n.Schema+"."+n.Table)
		info.Source, err = queryString(ctx, db, `
			SELECT pg_get_triggerdef(tg.oid, true) FROM pg_trigger tg
			JOIN pg_class c ON c.oid = tg.tgrelid JOIN pg_namespace ns ON ns.oid = c.relnamespace
			WHERE ns.nspname = $1 AND c.relname = $2 AND tg.tgname = $3`, n.Schema, n.Table, n.Object)
	}
	return info, err
}

func (postgres) columns(ctx context.Context, db *sql.DB, n Node) ([]Column, error) {
	rows, err := queryRows(ctx, db, `
		SELECT a.attname, format_type(a.atttypid, a.atttypmod), (NOT a.attnotnull)::text,
		       COALESCE(pg_get_expr(d.adbin, d.adrelid), ''), COALESCE(col_description(c.oid, a.attnum), '')
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace ns ON ns.oid = c.relnamespace
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE ns.nspname = $1 AND c.relname = $2 AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attnum`, n.Schema, n.Object)
	var out []Column
	for _, r := range rows {
		out = append(out, Column{Name: r[0], Type: r[1], Nullable: truthy(r[2]), Default: r[3], Comment: r[4]})
	}
	return out, err
}

func (postgres) constraints(ctx context.Context, db *sql.DB, n Node) ([]Constraint, error) {
	rows, err := queryRows(ctx, db, `
		SELECT con.conname, con.contype::text,
		       COALESCE((SELECT string_agg(a.attname, ', ' ORDER BY array_position(con.conkey, a.attnum))
		                 FROM pg_attribute a WHERE a.attrelid = con.conrelid AND a.attnum = ANY(con.conkey)), ''),
		       pg_get_constraintdef(con.oid, true)
		FROM pg_constraint con
		JOIN pg_class c ON c.oid = con.conrelid
		JOIN pg_namespace ns ON ns.oid = c.relnamespace
		WHERE ns.nspname = $1 AND c.relname = $2 AND con.contype IN ('p', 'f', 'u', 'c', 'x')
		ORDER BY position(con.contype::text in 'pufcx'), con.conname`, n.Schema, n.Object)
	types := map[string]string{"p": "PRIMARY KEY", "f": "FOREIGN KEY", "u": "UNIQUE", "c": "CHECK", "x": "EXCLUDE"}
	var out []Constraint
	for _, r := range rows {
		def := r[3]
		if r[1] == "p" || r[1] == "u" {
			def = "" // same as the column list
		}
		if r[1] == "f" {
			// Keep only the REFERENCES … part; the columns are listed separately.
			if i := strings.Index(def, "REFERENCES"); i >= 0 {
				def = def[i:]
			}
		}
		out = append(out, Constraint{Name: r[0], Type: types[r[1]], Columns: r[2], Definition: def})
	}
	return out, err
}

func (postgres) indexes(ctx context.Context, db *sql.DB, n Node) ([]Index, error) {
	rows, err := queryRows(ctx, db, `
		SELECT i.relname, ix.indisunique::text,
		       (SELECT string_agg(pg_get_indexdef(ix.indexrelid, k, true), ', ' ORDER BY k)
		        FROM generate_series(1, ix.indnkeyatts) k)
		FROM pg_index ix
		JOIN pg_class i ON i.oid = ix.indexrelid
		JOIN pg_class c ON c.oid = ix.indrelid
		JOIN pg_namespace ns ON ns.oid = c.relnamespace
		WHERE ns.nspname = $1 AND c.relname = $2
		ORDER BY i.relname`, n.Schema, n.Object)
	var out []Index
	for _, r := range rows {
		out = append(out, Index{Name: r[0], Unique: truthy(r[1]), Columns: r[2]})
	}
	return out, err
}
