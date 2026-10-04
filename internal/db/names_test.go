package db

import (
	"testing"

	"dibi/internal/config"
)

func TestQualifiedName(t *testing.T) {
	cases := []struct {
		typ  config.Type
		node Node
		want string
	}{
		{config.Postgres, Node{Kind: KindTable, Schema: "public", Object: "orders"}, "public.orders"},
		{config.Postgres, Node{Kind: KindView, Schema: "Sales", Object: "order"}, `"Sales"."order"`},
		{config.Postgres, Node{Kind: KindTable, Schema: "s", Object: "my table"}, `s."my table"`},
		{config.Postgres, Node{Kind: KindMatView, Schema: "s", Object: `a"b`}, `s."a""b"`},
		{config.Oracle, Node{Kind: KindTable, Schema: "DIBI", Object: "ORDERS"}, "DIBI.ORDERS"},
		{config.Oracle, Node{Kind: KindTable, Schema: "DIBI", Object: "MixedCase"}, `DIBI."MixedCase"`},
		{config.Oracle, Node{Kind: KindTable, Schema: "DIBI", Object: "LEVEL"}, `DIBI."LEVEL"`},
		{config.MySQL, Node{Kind: KindTable, Database: "shop", Object: "Orders"}, "shop.Orders"},
		{config.MariaDB, Node{Kind: KindTable, Database: "my-db", Object: "select"}, "`my-db`.`select`"},
		{config.MySQL, Node{Kind: KindTable, Database: "shop", Object: "123"}, "shop.`123`"},
		{config.SQLite, Node{Kind: KindTable, Database: "main", Object: "orders"}, "orders"},
		{config.SQLite, Node{Kind: KindTable, Database: "aux", Object: "Orders"}, "aux.Orders"},
		{config.DuckDB, Node{Kind: KindView, Database: "test", Schema: "main", Object: "v 1"}, `main."v 1"`},
		{config.MSSQL, Node{Kind: KindTable, Database: "shop", Schema: "dbo", Object: "Orders"}, "dbo.Orders"},
		{config.MSSQL, Node{Kind: KindView, Schema: "sales", Object: "order details"}, "sales.[order details]"},
		{config.MSSQL, Node{Kind: KindTable, Schema: "dbo", Object: "user"}, "dbo.[user]"},
		{config.MSSQL, Node{Kind: KindTable, Schema: "dbo", Object: "a]b"}, "dbo.[a]]b]"},
		{config.MSSQL, Node{Kind: KindTable, Schema: "dbo", Object: "t#1"}, "dbo.t#1"},
		{config.CSV, Node{Kind: KindView, Database: "memory", Schema: "main", Object: "sales_2026"}, "sales_2026"},
		{config.Postgres, Node{Kind: KindFunction, Schema: "s", Object: "f"}, ""},
	}
	for _, c := range cases {
		src, _ := NewSource(config.Connection{Name: "x", Type: c.typ})
		if got := src.QualifiedName(c.node); got != c.want {
			t.Errorf("%s %+v: got %s, want %s", c.typ, c.node, got, c.want)
		}
	}
}

func TestNodeContains(t *testing.T) {
	orders := Node{Kind: KindTable, Name: "orders", Object: "orders", Database: "shop", Schema: "public"}
	idx := Node{Kind: KindIndex, Object: "orders_pk", Database: "shop", Schema: "public", Table: "orders"}
	view := Node{Kind: KindView, Object: "v", Database: "shop", Schema: "public"}
	other := Node{Kind: KindTable, Object: "orders", Database: "shop", Schema: "audit"}
	cases := []struct {
		parent, child Node
		want          bool
	}{
		{Node{Kind: KindConnection}, other, true},
		{Node{Kind: KindDatabase, Database: "shop"}, orders, true},
		{Node{Kind: KindDatabase, Database: "crm"}, orders, false},
		{Node{Kind: KindSchema, Database: "shop", Schema: "public"}, orders, true},
		{Node{Kind: KindSchema, Database: "shop", Schema: "public"}, other, false},
		{Node{Kind: KindGroup, Group: KindTable, Database: "shop", Schema: "public"}, orders, true},
		{Node{Kind: KindGroup, Group: KindTable, Database: "shop", Schema: "public"}, idx, true},
		{Node{Kind: KindGroup, Group: KindTable, Database: "shop", Schema: "public"}, view, false},
		{Node{Kind: KindGroup, Group: KindView, Database: "shop", Schema: "public"}, view, true},
		{orders, orders, true},
		{orders, idx, true},
		{orders, other, false},
		{orders, view, false},
		{view, view, true},
	}
	for i, c := range cases {
		if got := c.parent.Contains(c.child); got != c.want {
			t.Errorf("case %d: %v contains %v = %v, want %v", i, c.parent, c.child, got, c.want)
		}
	}
}
