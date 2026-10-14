package db

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"dibi/internal/config"
	"dibi/internal/sqlsplit"

	mssql "github.com/microsoft/go-mssqldb"
	"github.com/microsoft/go-mssqldb/msdsn"
)

// mssqlDialect serves SQL Server. Catalog views only describe the current
// database, so each database gets its own pool, like Postgres.
type mssqlDialect struct{}

func (mssqlDialect) open(c config.Connection, database string) (*sql.DB, error) {
	if database == "" {
		database = c.Database
	}
	q := url.Values{}
	q.Set("app name", "dibi")
	if database != "" {
		q.Set("database", database)
	}
	switch c.TLSMode() {
	case config.SSLDisable:
		q.Set("encrypt", "disable")
	case config.SSLPrefer:
		// Without encrypt, only the login packet is encrypted, unless the
		// server forces encryption.
	default:
		q.Set("encrypt", "true")
	}
	u := &url.URL{Scheme: "sqlserver", RawQuery: q.Encode()}
	// host\instance: the port is found through the SQL Server Browser.
	host, instance, named := strings.Cut(c.Host, `\`)
	if named {
		u.Host, u.Path = host, instance
	} else {
		u.Host = net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	}
	// Without a user, the driver uses integrated authentication.
	if c.User != "" {
		u.User = url.UserPassword(c.User, c.Password)
	}
	cfg, err := msdsn.Parse(u.String())
	if err != nil {
		return nil, err
	}
	if cfg.Encryption != msdsn.EncryptionDisabled {
		tlsCfg, err := tlsConfig(c, host)
		if err != nil {
			return nil, err
		}
		// SQL Server expects one TDS packet per TLS record.
		tlsCfg.DynamicRecordSizingDisabled = true
		cfg.TLSConfig = tlsCfg
	}
	return sql.OpenDB(mssql.NewConnectorConfig(cfg)), nil
}

func (mssqlDialect) splitDialect() sqlsplit.Dialect { return sqlsplit.MSSQL }
func (mssqlDialect) hasDatabases() bool             { return true }
func (mssqlDialect) hasSchemas() bool               { return true }
func (mssqlDialect) separatePools() bool            { return true }

func (mssqlDialect) useDatabase(context.Context, *sql.Conn, string) error { return nil }

// plainCancel: the driver drops the connection when a context deadline
// expires (context.DeadlineExceeded is a net.Error for it), but keeps it
// when the context is cancelled.
func (mssqlDialect) plainCancel() bool { return true }

func (mssqlDialect) groups() []Kind {
	return []Kind{KindTable, KindView, KindFunction, KindProcedure, KindSequence}
}

// databases lists the accessible online databases, system databases last.
func (mssqlDialect) databases(ctx context.Context, db *sql.DB) ([]string, error) {
	return queryStrings(ctx, db, `
		SELECT name FROM sys.databases
		WHERE HAS_DBACCESS(name) = 1 AND state_desc = 'ONLINE'
		ORDER BY CASE WHEN database_id <= 4 THEN 1 ELSE 0 END, name`)
}

// schemas lists the schemas, without the system ones and those owned by
// the fixed database roles (db_owner, db_datareader, …).
func (mssqlDialect) schemas(ctx context.Context, db *sql.DB, _ string) ([]string, error) {
	return queryStrings(ctx, db, `
		SELECT name FROM sys.schemas
		WHERE schema_id < 16384 AND name NOT IN ('sys', 'INFORMATION_SCHEMA', 'guest')
		ORDER BY name`)
}

func (mssqlDialect) objects(ctx context.Context, db *sql.DB, g Node) ([]Node, error) {
	var q string
	switch g.Group {
	case KindTable:
		q = `SELECT name FROM sys.tables WHERE schema_id = SCHEMA_ID(@p1) AND is_ms_shipped = 0 ORDER BY name`
	case KindView:
		q = `SELECT name FROM sys.views WHERE schema_id = SCHEMA_ID(@p1) AND is_ms_shipped = 0 ORDER BY name`
	case KindFunction:
		q = `SELECT name FROM sys.objects WHERE schema_id = SCHEMA_ID(@p1) AND is_ms_shipped = 0
		       AND type IN ('FN', 'IF', 'TF', 'FS', 'FT') ORDER BY name`
	case KindProcedure:
		q = `SELECT name FROM sys.objects WHERE schema_id = SCHEMA_ID(@p1) AND is_ms_shipped = 0
		       AND type IN ('P', 'PC') ORDER BY name`
	case KindSequence:
		q = `SELECT name FROM sys.sequences WHERE schema_id = SCHEMA_ID(@p1) ORDER BY name`
	default:
		return nil, nil
	}
	names, err := queryStrings(ctx, db, q, g.Schema)
	return namedObjects(g.Group, g, names), err
}

// objectID finds an object from its schema (@p1) and name (@p2).
const objectID = `OBJECT_ID(QUOTENAME(@p1) + '.' + QUOTENAME(@p2))`

func (mssqlDialect) tableChildren(ctx context.Context, db *sql.DB, t Node) ([]Node, error) {
	var out []Node
	idx, err := queryStrings(ctx, db, `
		SELECT name FROM sys.indexes
		WHERE object_id = `+objectID+` AND type > 0 AND is_hypothetical = 0
		ORDER BY name`, t.Schema, t.Object)
	if err != nil {
		return nil, err
	}
	for _, name := range idx {
		out = append(out, Node{Kind: KindIndex, Name: name, Object: name, Database: t.Database, Schema: t.Schema, Table: t.Object})
	}
	trg, err := queryStrings(ctx, db, `
		SELECT name FROM sys.triggers WHERE parent_id = `+objectID+` ORDER BY name`, t.Schema, t.Object)
	for _, name := range trg {
		out = append(out, Node{Kind: KindTrigger, Name: name, Object: name, Database: t.Database, Schema: t.Schema, Table: t.Object})
	}
	return out, err
}

func quoteBracket(s string) string {
	return "[" + strings.ReplaceAll(s, "]", "]]") + "]"
}

func (m mssqlDialect) info(ctx context.Context, db *sql.DB, n Node) (*Info, error) {
	qualified := quoteBracket(n.Schema) + "." + quoteBracket(n.Object)
	info := &Info{Title: objectTitle(n, n.Schema+"."+n.Name)}
	var err error
	switch n.Kind {
	case KindTable, KindView:
		if info.Columns, err = m.columns(ctx, db, n); err != nil {
			return nil, err
		}
		info.setSelectTop(qualified)
		if n.Kind == KindView {
			info.Source, err = m.module(ctx, db, n.Schema, n.Object)
			info.EditScript = alterView(info.Source)
			break
		}
		if info.Constraints, err = m.constraints(ctx, db, n); err != nil {
			return nil, err
		}
		markPrimaryKey(info)
		var idx []mssqlIndex
		idx, err = m.indexes(ctx, db, n.Schema, n.Object)
		for _, i := range idx {
			info.Indexes = append(info.Indexes, Index{Name: i.name, Unique: i.unique, Columns: i.columns()})
		}
	case KindFunction, KindProcedure:
		info.Source, err = m.module(ctx, db, n.Schema, n.Object)
	case KindTrigger:
		info.Title = objectTitle(n, n.Name+" on "+n.Schema+"."+n.Table)
		info.Source, err = m.module(ctx, db, n.Schema, n.Object)
	case KindIndex:
		info.Title = objectTitle(n, n.Name+" on "+n.Schema+"."+n.Table)
		var idx []mssqlIndex
		idx, err = m.indexes(ctx, db, n.Schema, n.Table)
		for _, i := range idx {
			if i.name == n.Object {
				info.Source = i.definition(quoteBracket(n.Schema) + "." + quoteBracket(n.Table))
			}
		}
	case KindSequence:
		var rows [][]string
		rows, err = queryRows(ctx, db, `
			SELECT TYPE_NAME(user_type_id), CAST(start_value AS nvarchar(40)), CAST(minimum_value AS nvarchar(40)),
			       CAST(maximum_value AS nvarchar(40)), CAST(increment AS nvarchar(40)), is_cycling,
			       CASE WHEN is_cached = 0 THEN 'no' ELSE COALESCE(CAST(cache_size AS nvarchar(40)), 'default') END,
			       CAST(current_value AS nvarchar(40))
			FROM sys.sequences WHERE object_id = `+objectID, n.Schema, n.Object)
		if len(rows) == 1 {
			r := rows[0]
			info.Properties = [][2]string{{"Data type", r[0]}, {"Start value", r[1]}, {"Minimum", r[2]},
				{"Maximum", r[3]}, {"Increment", r[4]}, {"Cycle", r[5]}, {"Cache", r[6]}, {"Current value", r[7]}}
		}
	}
	return info, err
}

// module returns the source of a view, routine or trigger: the original
// CREATE statement.
// alterView turns the definition of a view into an ALTER VIEW statement,
// which keeps its permissions (CREATE OR ALTER needs SQL Server 2016 SP1),
// or returns "".
func alterView(def string) string {
	for _, kw := range []string{"CREATE OR ALTER VIEW", "CREATE VIEW"} {
		if before, after, ok := cutLeading(def, kw); ok {
			return before + "ALTER VIEW" + after
		}
	}
	return ""
}

func (mssqlDialect) module(ctx context.Context, db *sql.DB, schema, name string) (string, error) {
	def, err := queryString(ctx, db, `SELECT definition FROM sys.sql_modules WHERE object_id = `+objectID, schema, name)
	if err == nil && def == "" {
		def = "-- The definition is not available (encrypted or CLR object, or no VIEW DEFINITION permission)."
	}
	return strings.TrimSpace(def), err
}

func (mssqlDialect) columns(ctx context.Context, db *sql.DB, n Node) ([]Column, error) {
	rows, err := queryRows(ctx, db, `
		SELECT c.name, t.name, c.max_length, c.precision, c.scale, c.is_nullable, t.is_user_defined,
		       COALESCE(dc.definition, ''), COALESCE(cc.definition, ''),
		       CASE WHEN ic.column_id IS NULL THEN ''
		            ELSE CONCAT('IDENTITY(', CAST(ic.seed_value AS nvarchar(40)), ',', CAST(ic.increment_value AS nvarchar(40)), ')') END,
		       COALESCE(CAST(ep.value AS nvarchar(4000)), '')
		FROM sys.columns c
		JOIN sys.types t ON t.user_type_id = c.user_type_id
		LEFT JOIN sys.default_constraints dc ON dc.object_id = c.default_object_id
		LEFT JOIN sys.computed_columns cc ON cc.object_id = c.object_id AND cc.column_id = c.column_id
		LEFT JOIN sys.identity_columns ic ON ic.object_id = c.object_id AND ic.column_id = c.column_id
		LEFT JOIN sys.extended_properties ep
		  ON ep.class = 1 AND ep.major_id = c.object_id AND ep.minor_id = c.column_id AND ep.name = 'MS_Description'
		WHERE c.object_id = `+objectID+`
		ORDER BY c.column_id`, n.Schema, n.Object)
	var out []Column
	for _, r := range rows {
		typ := r[1]
		if !truthy(r[6]) {
			typ = mssqlType(r[1], r[2], r[3], r[4])
		}
		def := stripParens(r[7])
		switch {
		case r[8] != "":
			def = "AS " + r[8]
		case r[9] != "":
			def = r[9]
		}
		out = append(out, Column{Name: r[0], Type: typ, Nullable: truthy(r[5]), Default: def, Comment: r[10]})
	}
	return out, err
}

// mssqlType formats a column type with its length/precision. maxLength is
// in bytes (-1 for MAX); n(var)char use two bytes per character.
func mssqlType(typ, maxLength, precision, scale string) string {
	switch typ {
	case "varchar", "char", "varbinary", "binary", "nvarchar", "nchar":
		if maxLength == "-1" {
			return typ + "(max)"
		}
		if typ == "nvarchar" || typ == "nchar" {
			if n, err := strconv.Atoi(maxLength); err == nil {
				maxLength = strconv.Itoa(n / 2)
			}
		}
		return typ + "(" + maxLength + ")"
	case "decimal", "numeric":
		return fmt.Sprintf("%s(%s,%s)", typ, precision, scale)
	case "datetime2", "time", "datetimeoffset":
		return fmt.Sprintf("%s(%s)", typ, scale)
	case "float":
		if precision != "53" {
			return fmt.Sprintf("float(%s)", precision)
		}
	}
	return typ
}

// stripParens removes the parentheses SQL Server wraps default values in:
// ((0)) becomes 0.
func stripParens(s string) string {
	for len(s) >= 2 && s[0] == '(' && s[len(s)-1] == ')' && closingParen(s) == len(s)-1 {
		s = s[1 : len(s)-1]
	}
	return s
}

// closingParen returns the index of the parenthesis closing s[0], ignoring
// those inside string literals, or -1.
func closingParen(s string) int {
	depth := 0
	inString := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\'':
			inString = !inString
		case inString:
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func (mssqlDialect) constraints(ctx context.Context, db *sql.DB, n Node) ([]Constraint, error) {
	var out []Constraint
	// Adds a column to the constraint named name, which is the last one
	// appended when rows are ordered by constraint.
	add := func(c Constraint, col string) {
		if len(out) > 0 && out[len(out)-1].Name == c.Name {
			out[len(out)-1].Columns += ", " + col
			return
		}
		c.Columns = col
		out = append(out, c)
	}
	quote := identQuoter(config.MSSQL)

	keys, err := queryRows(ctx, db, `
		SELECT kc.name, kc.type, col.name
		FROM sys.key_constraints kc
		JOIN sys.index_columns ic ON ic.object_id = kc.parent_object_id AND ic.index_id = kc.unique_index_id
		JOIN sys.columns col ON col.object_id = ic.object_id AND col.column_id = ic.column_id
		WHERE kc.parent_object_id = `+objectID+` AND ic.is_included_column = 0
		ORDER BY CASE kc.type WHEN 'PK' THEN 0 ELSE 1 END, kc.name, ic.key_ordinal`, n.Schema, n.Object)
	if err != nil {
		return nil, err
	}
	for _, r := range keys {
		typ := "UNIQUE"
		if strings.TrimSpace(r[1]) == "PK" {
			typ = "PRIMARY KEY"
		}
		add(Constraint{Name: r[0], Type: typ}, r[2])
	}

	fks, err := queryRows(ctx, db, `
		SELECT fk.name, pc.name, OBJECT_SCHEMA_NAME(fk.referenced_object_id), OBJECT_NAME(fk.referenced_object_id),
		       rc.name, fk.delete_referential_action_desc, fk.update_referential_action_desc
		FROM sys.foreign_keys fk
		JOIN sys.foreign_key_columns fkc ON fkc.constraint_object_id = fk.object_id
		JOIN sys.columns pc ON pc.object_id = fkc.parent_object_id AND pc.column_id = fkc.parent_column_id
		JOIN sys.columns rc ON rc.object_id = fkc.referenced_object_id AND rc.column_id = fkc.referenced_column_id
		WHERE fk.parent_object_id = `+objectID+`
		ORDER BY fk.name, fkc.constraint_column_id`, n.Schema, n.Object)
	if err != nil {
		return nil, err
	}
	var refCols []string
	for i, r := range fks {
		add(Constraint{Name: r[0], Type: "FOREIGN KEY"}, r[1])
		refCols = append(refCols, quote(r[4]))
		if i == len(fks)-1 || fks[i+1][0] != r[0] {
			def := fmt.Sprintf("REFERENCES %s.%s (%s)", quote(r[2]), quote(r[3]), strings.Join(refCols, ", "))
			for _, a := range []struct{ on, action string }{{"DELETE", r[5]}, {"UPDATE", r[6]}} {
				if a.action != "NO_ACTION" {
					def += " ON " + a.on + " " + strings.ReplaceAll(a.action, "_", " ")
				}
			}
			out[len(out)-1].Definition = def
			refCols = nil
		}
	}

	checks, err := queryRows(ctx, db, `
		SELECT name, COALESCE(COL_NAME(parent_object_id, parent_column_id), ''), definition
		FROM sys.check_constraints WHERE parent_object_id = `+objectID+`
		ORDER BY name`, n.Schema, n.Object)
	for _, r := range checks {
		out = append(out, Constraint{Name: r[0], Type: "CHECK", Columns: r[1], Definition: "CHECK " + r[2]})
	}
	return out, err
}

// mssqlIndex describes an index with what is needed to rebuild its
// definition.
type mssqlIndex struct {
	name, typ, filter string // typ: CLUSTERED, NONCLUSTERED, XML, … COLUMNSTORE
	unique            bool
	keys, included    []string
}

// columns lists the key columns, then the included ones.
func (i mssqlIndex) columns() string {
	if len(i.keys) == 0 { // columnstore: every column is "included"
		return strings.Join(i.included, ", ")
	}
	s := strings.Join(i.keys, ", ")
	if len(i.included) > 0 {
		s += " INCLUDE (" + strings.Join(i.included, ", ") + ")"
	}
	return s
}

func (i mssqlIndex) definition(table string) string {
	u := ""
	if i.unique {
		u = "UNIQUE "
	}
	def := fmt.Sprintf("CREATE %s%s INDEX %s ON %s", u, i.typ, quoteBracket(i.name), table)
	if i.typ != "CLUSTERED COLUMNSTORE" {
		def += " (" + i.columns() + ")"
		if len(i.keys) == 0 {
			def = strings.Replace(def, " INCLUDE (", " (", 1)
		}
	}
	if i.filter != "" {
		def += " WHERE " + i.filter
	}
	return def
}

func (mssqlDialect) indexes(ctx context.Context, db *sql.DB, schema, table string) ([]mssqlIndex, error) {
	rows, err := queryRows(ctx, db, `
		SELECT i.name, i.type_desc, i.is_unique, COALESCE(i.filter_definition, ''),
		       c.name, ic.is_descending_key, ic.is_included_column, ic.key_ordinal
		FROM sys.indexes i
		JOIN sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id
		JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
		WHERE i.object_id = `+objectID+` AND i.type > 0 AND i.is_hypothetical = 0
		ORDER BY i.name, ic.key_ordinal, ic.index_column_id`, schema, table)
	quote := identQuoter(config.MSSQL)
	var out []mssqlIndex
	for _, r := range rows {
		if len(out) == 0 || out[len(out)-1].name != r[0] {
			out = append(out, mssqlIndex{name: r[0], typ: r[1], unique: truthy(r[2]), filter: r[3]})
		}
		idx := &out[len(out)-1]
		col := quote(r[4])
		switch {
		case truthy(r[6]) || r[7] == "0":
			idx.included = append(idx.included, col)
		case truthy(r[5]):
			idx.keys = append(idx.keys, col+" DESC")
		default:
			idx.keys = append(idx.keys, col)
		}
	}
	return out, err
}

// setAutocommit uses implicit transactions, which turning off does not
// commit by itself.
func (mssqlDialect) setAutocommit(ctx context.Context, conn *sql.Conn, on bool) error {
	q := "SET IMPLICIT_TRANSACTIONS ON"
	if on {
		q = "WHILE @@TRANCOUNT > 0 COMMIT; SET IMPLICIT_TRANSACTIONS OFF"
	}
	_, err := conn.ExecContext(ctx, q)
	return err
}
