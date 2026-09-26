package db

import (
	"context"
	"fmt"
	"os"
	"testing"

	"dibi/internal/config"
)

func TestProbeTypes(t *testing.T) {
	if os.Getenv("DIBI_PROBE") == "" {
		t.Skip()
	}
	qs := map[config.Type]string{
		config.Postgres: `SELECT 1::int, 2::bigint, 1.5::numeric(10,2), 1.5::float8, 'txt'::text, 'c'::char(3), true, now(), now()::timestamp, current_date, '10:11:12'::time, '\xdead'::bytea, gen_random_uuid(), '{"a":1}'::jsonb, ARRAY[1,2], interval '1 day', NULL::text, '1.2.3.4'::inet`,
		config.MySQL:    `SELECT 1, CAST(2 AS SIGNED), CAST(1.5 AS DECIMAL(10,2)), 1.5e0, 'txt', CAST('c' AS CHAR(3)), true, NOW(), CURRENT_DATE, CAST('10:11:12' AS TIME), X'DEAD', UUID(), JSON_OBJECT('a',1), NULL, CAST('2020-01-01' AS DATETIME)`,
		config.MariaDB:  `SELECT 1, CAST(2 AS SIGNED), CAST(1.5 AS DECIMAL(10,2)), 1.5e0, 'txt', true, NOW(), CURRENT_DATE, X'DEAD', NULL`,
		config.Oracle:   `SELECT 1, CAST(1.5 AS NUMBER(10,2)), CAST(1.5 AS BINARY_DOUBLE), 'txt', CAST('c' AS CHAR(3)), SYSDATE, SYSTIMESTAMP, CAST(SYSTIMESTAMP AS TIMESTAMP), HEXTORAW('DEAD'), TO_CLOB('clob'), NULL, INTERVAL '1' DAY FROM dual`,
		config.MSSQL:    `SELECT CAST(1 AS TINYINT), CAST(1.5 AS DECIMAL(10,2)), CAST(1.5 AS MONEY), CAST(1.5 AS SMALLMONEY), CAST(1.5 AS REAL), CAST(1 AS BIT), N'txt', CAST('2026-03-01' AS DATE), CAST('10:11:12.5' AS TIME(3)), SYSDATETIME(), CAST(SYSDATETIME() AS DATETIME), SYSDATETIMEOFFSET(), NEWID(), 0xDEAD, CAST(0x0000000000000001 AS ROWVERSION), CAST('<a/>' AS XML), geography::Point(1, 2, 4326), NULL`,
		config.SQLite:   `SELECT 1, 1.5, 'txt', X'DEAD', NULL, CURRENT_TIMESTAMP`,
		config.DuckDB:   `SELECT 1::INT, 2::BIGINT, 1.5::DECIMAL(10,2), 1.5::DOUBLE, 'txt', true, now(), now()::TIMESTAMP, current_date, '10:11:12'::TIME, '\xDE\xAD'::BLOB, uuid(), [1,2], {'a': 1}, MAP {'k': 1}, INTERVAL 1 DAY, NULL, 170141183460469231731687303715884105727::HUGEINT, '{"a":1}'::JSON`,
	}
	fixtures := append(fileFixtures(t), serverFixtures(t)...)
	for _, f := range fixtures {
		src, _ := NewSource(f.conn)
		pool, err := src.Pool("")
		if err != nil {
			t.Fatal(err)
		}
		rows, err := pool.QueryContext(context.Background(), qs[f.conn.Type])
		if err != nil {
			t.Errorf("%s: %v", f.conn.Name, err)
			continue
		}
		cts, _ := rows.ColumnTypes()
		rows.Next()
		vals := make([]any, len(cts))
		ptrs := make([]any, len(cts))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Errorf("%s: %v", f.conn.Name, err)
		}
		fmt.Printf("== %s\n", f.conn.Name)
		for i, c := range cts {
			fmt.Printf("   %-22s %-28T %v\n", c.DatabaseTypeName(), vals[i], vals[i])
		}
		rows.Close()
		src.Close()
	}
}
