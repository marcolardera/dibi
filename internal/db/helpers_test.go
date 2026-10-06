package db

import "testing"

func TestEditScripts(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{orReplace("CREATE ALGORITHM=UNDEFINED VIEW `v` AS select 1"), "CREATE OR REPLACE ALGORITHM=UNDEFINED VIEW `v` AS select 1"},
		{orReplace("create temp view v as select 1;"), "CREATE OR REPLACE temp view v as select 1;"},
		{orReplace("CREATED"), ""},
		{alterView("-- note\n/* x */ create\n  view dbo.v as select 1"), "-- note\n/* x */ ALTER VIEW dbo.v as select 1"},
		{alterView("CREATE OR ALTER VIEW v AS SELECT 1"), "ALTER VIEW v AS SELECT 1"},
		{alterView("CREATE VIEWS"), ""},
		{alterView("-- The definition is not available."), ""},
		{sqliteEditView("CREATE VIEW v as select 1", "aux", `"aux"."v"`), "DROP VIEW IF EXISTS \"aux\".\"v\";\nCREATE VIEW \"aux\".v as select 1"},
		{sqliteEditView(`CREATE VIEW IF NOT EXISTS "v" AS SELECT 1`, "main", `"main"."v"`), "DROP VIEW IF EXISTS \"main\".\"v\";\nCREATE VIEW \"main\".\"v\" AS SELECT 1"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}
