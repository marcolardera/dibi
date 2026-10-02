package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConnectionsRoundTrip(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pg := Connection{Name: "prod/pg", Type: Postgres, Host: "db", Port: 5432, Database: "app", User: "u", Password: "p"}
	if err := s.SaveConnection(pg, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveConnection(pg, ""); err == nil {
		t.Fatal("expected duplicate name error")
	}
	lite := Connection{Name: "local", Type: SQLite, Path: "/tmp/x.db"}
	if err := s.SaveConnection(lite, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Root, "connections", "prod_pg.yaml")); err != nil {
		t.Fatalf("unsafe characters must be replaced in file names: %v", err)
	}

	conns, err := s.Connections()
	if err != nil {
		t.Fatal(err)
	}
	if len(conns) != 2 || conns[0].Name != "local" || conns[1] != pg {
		t.Fatalf("unexpected connections: %+v", conns)
	}

	// Rename.
	pg2 := pg
	pg2.Name = "staging"
	if err := s.SaveConnection(pg2, pg.Name); err != nil {
		t.Fatal(err)
	}
	conns, _ = s.Connections()
	if len(conns) != 2 || conns[1].Name != "staging" {
		t.Fatalf("rename failed: %+v", conns)
	}
	// Renaming onto an existing name must fail.
	pg2.Name = "local"
	if err := s.SaveConnection(pg2, "staging"); err == nil {
		t.Fatal("expected error renaming onto existing connection")
	}

	if err := s.DeleteConnection("staging"); err != nil {
		t.Fatal(err)
	}
	conns, _ = s.Connections()
	if len(conns) != 1 {
		t.Fatalf("delete failed: %+v", conns)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		c  Connection
		ok bool
	}{
		{Connection{Name: "a", Type: SQLite, Path: "x"}, true},
		{Connection{Name: "a", Type: SQLite}, false},
		{Connection{Name: "", Type: SQLite, Path: "x"}, false},
		{Connection{Name: "a", Type: Postgres, Host: "h", Port: 5432}, true},
		{Connection{Name: "a", Type: Postgres, Host: "h"}, false},
		{Connection{Name: "a", Type: Oracle, Host: "h", Port: 1521}, false},
		{Connection{Name: "a", Type: MSSQL, Host: `h\sqlexpress`, Port: 1433}, true},
		{Connection{Name: "a", Type: "nope", Host: "h", Port: 1}, false},
	}
	for _, c := range cases {
		if err := c.c.Validate(); (err == nil) != c.ok {
			t.Errorf("%+v: got err=%v, want ok=%v", c.c, err, c.ok)
		}
	}
}

func TestQueriesAndState(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	name, err := s.SaveQuery("daily report", "SELECT 1;")
	if err != nil || name != "daily report" {
		t.Fatal(name, err)
	}
	s.SaveQuery("another", "SELECT 2;")
	names, _ := s.Queries()
	if len(names) != 2 || names[0] != "another" {
		t.Fatalf("unexpected queries: %v", names)
	}
	if text, _ := s.LoadQuery("daily report"); text != "SELECT 1;" {
		t.Fatalf("unexpected text %q", text)
	}
	if !s.QueryExists("another") || s.QueryExists("missing") {
		t.Fatal("QueryExists is wrong")
	}

	if st := s.LoadState(); st != (State{}) {
		t.Fatalf("expected empty state, got %+v", st)
	}
	s.SaveState(State{LastConnection: "pg", LastDatabase: "app"})
	if st := s.LoadState(); st.LastConnection != "pg" || st.LastDatabase != "app" {
		t.Fatalf("unexpected state %+v", st)
	}
}

func TestSSLMode(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// SQL Server connections saved before SSLMode existed.
	legacy := map[string]string{
		"a": "name: a\ntype: sqlserver\nhost: h\nport: 1433\nencrypt: true\ntrust_server_certificate: true\n",
		"b": "name: b\ntype: sqlserver\nhost: h\nport: 1433\nencrypt: true\n",
		"c": "name: c\ntype: sqlserver\nhost: h\nport: 1433\n",
	}
	for name, data := range legacy {
		os.WriteFile(filepath.Join(s.Root, "connections", name+".yaml"), []byte(data), 0o600)
	}
	conns, err := s.Connections()
	if err != nil {
		t.Fatal(err)
	}
	want := []SSLMode{SSLRequire, SSLVerifyFull, SSLPrefer}
	for i, c := range conns {
		if c.TLSMode() != want[i] {
			t.Errorf("%s: mode %q, want %q", c.Name, c.TLSMode(), want[i])
		}
	}

	if m := (&Connection{Type: Oracle}).TLSMode(); m != SSLDisable {
		t.Errorf("Oracle default mode %q, want disable", m)
	}
	ora := Connection{Name: "o", Type: Oracle, Host: "h", Port: 1522, Database: "svc", SSLMode: SSLPrefer}
	if err := ora.Validate(); err == nil {
		t.Error("Oracle must reject prefer")
	}
	pg := Connection{Name: "p", Type: Postgres, Host: "h", Port: 5432, SSLMode: SSLRequire, SSLCert: "c.pem"}
	if err := pg.Validate(); err == nil {
		t.Error("a client certificate without a key must be rejected")
	}
}

func TestCSVOptionsValidate(t *testing.T) {
	c := Connection{Name: "c", Type: CSV, Path: "/x.csv", Separator: ";", Quote: "\""}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Separator = ";;"
	if err := c.Validate(); err == nil {
		t.Fatal("expected an error for a multi-character separator")
	}
}
