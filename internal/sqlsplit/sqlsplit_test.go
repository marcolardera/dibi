package sqlsplit

import (
	"reflect"
	"testing"
)

func texts(stmts []Statement) []string {
	out := make([]string, len(stmts))
	for i, s := range stmts {
		out[i] = s.Text
	}
	return out
}

func TestSplit(t *testing.T) {
	cases := []struct {
		name string
		d    Dialect
		src  string
		want []string
	}{
		{"single without delimiter", Generic, "SELECT 1", []string{"SELECT 1"}},
		{"two statements", Generic, "SELECT 1;\nSELECT 2;", []string{"SELECT 1", "SELECT 2"}},
		{"empty and comment-only statements dropped", Generic, " ;; -- nothing\n/* x */;", nil},
		{"leading comment is skipped", Generic, "-- hello\nSELECT 1; -- trailing", []string{"SELECT 1"}},
		{"semicolon in string", Generic, "SELECT 'a;b'; SELECT 2", []string{"SELECT 'a;b'", "SELECT 2"}},
		{"doubled quote", Generic, "SELECT 'it''s;'; SELECT 2", []string{"SELECT 'it''s;'", "SELECT 2"}},
		{"semicolon in identifier", Generic, `SELECT 1 AS "a;b"; SELECT 2`, []string{`SELECT 1 AS "a;b"`, "SELECT 2"}},
		{"semicolon in comments", Generic, "SELECT 1 -- a;b\n, 2 /* c;d */; SELECT 3",
			[]string{"SELECT 1 -- a;b\n, 2 /* c;d */", "SELECT 3"}},
		{"unterminated string runs to end", Generic, "SELECT 'abc; SELECT 2", []string{"SELECT 'abc; SELECT 2"}},
		{"sqlite trigger", Generic,
			"CREATE TRIGGER t AFTER INSERT ON a BEGIN\n  UPDATE b SET n = n + 1;\n  DELETE FROM c;\nEND;\nSELECT 1;",
			[]string{"CREATE TRIGGER t AFTER INSERT ON a BEGIN\n  UPDATE b SET n = n + 1;\n  DELETE FROM c;\nEND", "SELECT 1"}},
		{"sqlite trigger with case", Generic,
			"CREATE TEMP TRIGGER t AFTER INSERT ON a BEGIN SELECT CASE WHEN 1 THEN 2 END; END; SELECT 1",
			[]string{"CREATE TEMP TRIGGER t AFTER INSERT ON a BEGIN SELECT CASE WHEN 1 THEN 2 END; END", "SELECT 1"}},
		{"begin transaction is plain", Generic, "BEGIN; INSERT INTO a VALUES (1); COMMIT;",
			[]string{"BEGIN", "INSERT INTO a VALUES (1)", "COMMIT"}},

		{"postgres dollar quote", Postgres,
			"CREATE FUNCTION f() RETURNS int AS $$ BEGIN RETURN 1; END; $$ LANGUAGE plpgsql; SELECT f();",
			[]string{"CREATE FUNCTION f() RETURNS int AS $$ BEGIN RETURN 1; END; $$ LANGUAGE plpgsql", "SELECT f()"}},
		{"postgres tagged dollar quote", Postgres,
			"DO $body$ BEGIN RAISE NOTICE 'x;$$'; END $body$; SELECT 1",
			[]string{"DO $body$ BEGIN RAISE NOTICE 'x;$$'; END $body$", "SELECT 1"}},
		{"postgres positional params are not dollar quotes", Postgres,
			"SELECT $1; SELECT 2", []string{"SELECT $1", "SELECT 2"}},
		{"postgres E string with backslash", Postgres,
			`SELECT E'a\';b'; SELECT 2`, []string{`SELECT E'a\';b'`, "SELECT 2"}},
		{"postgres standard string: backslash is literal", Postgres,
			`SELECT 'a\'; SELECT 2`, []string{`SELECT 'a\'`, "SELECT 2"}},

		{"mysql backslash escape", MySQL, `SELECT 'a\';b'; SELECT 2`, []string{`SELECT 'a\';b'`, "SELECT 2"}},
		{"mysql double-quoted string", MySQL, `SELECT "a;\"b"; SELECT 2`, []string{`SELECT "a;\"b"`, "SELECT 2"}},
		{"mysql backticks and hash comment", MySQL, "SELECT `a;b` # c;d\n; SELECT 2",
			[]string{"SELECT `a;b` # c;d", "SELECT 2"}},
		{"mysql procedure without delimiter", MySQL,
			"CREATE PROCEDURE p()\nBEGIN\n  DECLARE x INT;\n  IF x > 0 THEN SET x = 1; END IF;\n  lbl: LOOP LEAVE lbl; END LOOP;\n  SELECT CASE WHEN x THEN 1 END;\nEND;\nCALL p();",
			[]string{"CREATE PROCEDURE p()\nBEGIN\n  DECLARE x INT;\n  IF x > 0 THEN SET x = 1; END IF;\n  lbl: LOOP LEAVE lbl; END LOOP;\n  SELECT CASE WHEN x THEN 1 END;\nEND", "CALL p()"}},
		{"mysql definer routine with single statement body", MySQL,
			"CREATE DEFINER=`root`@`%` FUNCTION f() RETURNS INT RETURN 1; SELECT f()",
			[]string{"CREATE DEFINER=`root`@`%` FUNCTION f() RETURNS INT RETURN 1", "SELECT f()"}},
		{"mysql delimiter command", MySQL,
			"DELIMITER //\nCREATE PROCEDURE p() BEGIN SELECT 1; END//\nDELIMITER ;\nSELECT 2;",
			[]string{"CREATE PROCEDURE p() BEGIN SELECT 1; END", "SELECT 2"}},

		{"oracle anonymous block", Oracle,
			"BEGIN\n  FOR i IN 1..3 LOOP\n    INSERT INTO t VALUES (i);\n  END LOOP;\nEND;\nSELECT 1 FROM dual;",
			[]string{"BEGIN\n  FOR i IN 1..3 LOOP\n    INSERT INTO t VALUES (i);\n  END LOOP;\nEND;", "SELECT 1 FROM dual"}},
		{"oracle declare block", Oracle,
			"DECLARE\n  x NUMBER := 1;\nBEGIN\n  IF x = 1 THEN NULL; END IF;\n  CASE x WHEN 1 THEN NULL; END CASE;\nEND;\nSELECT 1 FROM dual",
			[]string{"DECLARE\n  x NUMBER := 1;\nBEGIN\n  IF x = 1 THEN NULL; END IF;\n  CASE x WHEN 1 THEN NULL; END CASE;\nEND;", "SELECT 1 FROM dual"}},
		{"oracle procedure with declarations", Oracle,
			"CREATE OR REPLACE PROCEDURE p AS\n  v NUMBER;\nBEGIN\n  v := 1;\nEND p;\nSELECT 1 FROM dual",
			[]string{"CREATE OR REPLACE PROCEDURE p AS\n  v NUMBER;\nBEGIN\n  v := 1;\nEND p;", "SELECT 1 FROM dual"}},
		{"oracle slash terminator", Oracle,
			"CREATE PACKAGE pk AS\n  PROCEDURE a;\nEND pk;\n/\nSELECT 1 FROM dual\n/\nSELECT 2 FROM dual",
			[]string{"CREATE PACKAGE pk AS\n  PROCEDURE a;\nEND pk;", "SELECT 1 FROM dual", "SELECT 2 FROM dual"}},
		{"oracle q-quote", Oracle, "SELECT q'[a;']b]' FROM dual; SELECT 2 FROM dual",
			[]string{"SELECT q'[a;']b]' FROM dual", "SELECT 2 FROM dual"}},
		{"oracle division at line start is not a terminator", Oracle, "SELECT 4\n/ 2 FROM dual",
			[]string{"SELECT 4\n/ 2 FROM dual"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := texts(Split(c.src, c.d))
			if len(got) == 0 && len(c.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("\n got: %q\nwant: %q", got, c.want)
			}
		})
	}
}

func TestSplitOffsets(t *testing.T) {
	src := "-- c\n  SELECT 1;\n\nSELECT 2"
	for _, s := range Split(src, Generic) {
		if src[s.Start:s.Start+len(s.Text)] != s.Text {
			t.Errorf("offset %d does not point to %q", s.Start, s.Text)
		}
	}
}

func TestReturnsRows(t *testing.T) {
	for stmt, want := range map[string]bool{
		"SELECT 1":                      true,
		"  -- c\n/* x */ select 1":      true,
		"WITH a AS (SELECT 1) SELECT *": true,
		"(SELECT 1) UNION (SELECT 2)":   true,
		"show tables":                   true,
		"PRAGMA table_info(x)":          true,
		"INSERT INTO a VALUES (1)":      false,
		"UPDATE a SET b = 1":            false,
		"CREATE TABLE a (b int)":        false,
		"BEGIN NULL; END;":              false,
		"":                              false,
	} {
		if got := ReturnsRows(stmt); got != want {
			t.Errorf("ReturnsRows(%q) = %v, want %v", stmt, got, want)
		}
	}
}
