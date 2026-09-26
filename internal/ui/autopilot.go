//go:build autopilot

// Autopilot: drives the real UI for end-to-end checks and screenshots.
// Build with -tags autopilot and set DIBI_SHOTS to an output directory.
// It is never part of release builds.
package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dibi/internal/export"

	qt "github.com/mappu/miqt/qt6"
)

func init() { autopilot = runAutopilot }

type pilot struct {
	a     *App
	dir   string
	fails int
}

func (p *pilot) log(format string, args ...any) { fmt.Printf(format+"\n", args...) }

func (p *pilot) check(ok bool, format string, args ...any) {
	status := "ok  "
	if !ok {
		status = "FAIL"
		p.fails++
	}
	fmt.Printf("  [%s] %s\n", status, fmt.Sprintf(format, args...))
}

// ui runs f on the UI thread and returns its result.
func ui[T any](f func() T) T {
	var r T
	onUIWait(func() { r = f() })
	return r
}

func (p *pilot) wait(what string, cond func() bool) bool {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if ui(cond) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	p.check(false, "timeout waiting for %s", what)
	return false
}

func (p *pilot) shot(name string) {
	time.Sleep(400 * time.Millisecond)
	onUIWait(func() {
		w := qt.QApplication_ActiveModalWidget()
		if w == nil {
			w = p.a.win.QWidget
		}
		w.Grab().Save(filepath.Join(p.dir, name+".png"))
	})
}

func (p *pilot) status() string { return ui(func() string { return p.a.status.Text() }) }
func (p *pilot) page() string   { return ui(func() string { return p.a.pageLabel.Text() }) }

// item finds a tree item by path, expanding each level.
func (p *pilot) item(path ...string) *qt.QTreeWidgetItem {
	var cur *qt.QTreeWidgetItem
	for _, name := range path {
		parent := cur
		if parent != nil {
			onUIWait(func() { parent.SetExpanded(true) })
			if !p.wait("children of "+name, func() bool { e := p.a.tree.entry(parent); return e != nil && e.loaded }) {
				return nil
			}
		}
		cur = ui(func() *qt.QTreeWidgetItem {
			var n int
			var child func(i int) *qt.QTreeWidgetItem
			if parent == nil {
				n, child = p.a.tree.widget.TopLevelItemCount(), p.a.tree.widget.TopLevelItem
			} else {
				n, child = parent.ChildCount(), parent.Child
			}
			for i := 0; i < n; i++ {
				if c := child(i); c.Text(0) == name {
					return c
				}
			}
			return nil
		})
		if cur == nil {
			p.check(false, "tree item %q not found (path %v)", name, path)
			return nil
		}
	}
	return cur
}

// openInfo clicks a tree item and waits for its info panel.
func (p *pilot) openInfo(path ...string) *infoTab {
	it := p.item(path...)
	if it == nil {
		return nil
	}
	onUIWait(func() {
		p.a.tree.widget.SetCurrentItem(it)
		p.a.tree.activate(it)
	})
	var t *infoTab
	p.wait("info panel", func() bool { t = p.a.currentInfo(); return t != nil && t.inner.Count() > 0 })
	return t
}

func (p *pilot) selectInner(t *infoTab, label string) bool {
	return ui(func() bool {
		for i := 0; i < t.inner.Count(); i++ {
			if t.inner.TabText(i) == label {
				t.inner.SetCurrentIndex(i)
				return true
			}
		}
		return false
	})
}

func (p *pilot) innerTabs(t *infoTab) []string {
	return ui(func() []string {
		var out []string
		for i := 0; i < t.inner.Count(); i++ {
			out = append(out, t.inner.TabText(i))
		}
		return out
	})
}

// script opens a new script with text and runs it, waiting for completion.
func (p *pilot) runScript(text string) *sqlTab {
	t := ui(func() *sqlTab {
		t := p.a.newScript(text, "")
		return t
	})
	p.wait("connection list", func() bool { return !t.dbCombo.IsVisible() || t.dbCombo.Count() > 0 })
	onUIWait(func() { p.a.run() })
	p.wait("query end", func() bool { return !t.running })
	return t
}

func (p *pilot) messages(t *sqlTab) string {
	return ui(func() string { return t.messages.ToPlainText() })
}

func runAutopilot(a *App) {
	p := &pilot{a: a, dir: os.Getenv("DIBI_SHOTS")}
	if p.dir == "" {
		p.dir = "."
	}
	time.Sleep(time.Second)
	defer func() {
		p.log("\nautopilot finished: %d failure(s)", p.fails)
		onUIWait(func() {
			for _, t := range a.sqlTabs {
				t.editor.Document().SetModifiedWithBool(false)
			}
			a.win.Close()
		})
	}()

	p.log("== start")
	p.shot("01-start")
	p.check(ui(func() int { return a.tree.widget.TopLevelItemCount() }) == len(a.conns), "all connections in the tree")

	p.log("== PostgreSQL: info panel of a table")
	t := p.openInfo("pg", "dibi", "dibi_test", "Tables", "orders")
	if t != nil {
		tabs := p.innerTabs(t)
		p.check(strings.Join(tabs, ",") == "Columns,Constraints,Indexes,"+previewTab, "info sub-tabs: %v", tabs)
		p.shot("02-pg-columns")
		p.selectInner(t, "Constraints")
		p.shot("03-pg-constraints")
		p.selectInner(t, previewTab)
		p.wait("preview", func() bool { return t.data != nil && t.data.pager != nil })
		p.check(p.page() != "", "preview page label: %q", p.page())
		p.shot("04-pg-data")
	}
	p.check(ui(func() bool { return a.lastConn == "pg" && a.lastDB == "dibi" }), "last used = pg/dibi (got %s/%s)", a.lastConn, a.lastDB)

	p.log("== Ctrl+C on a tree item copies the table reference")
	for _, c := range []struct {
		path []string
		want string
	}{
		{[]string{"pg", "dibi", "dibi_test", "Tables", "orders"}, "dibi_test.orders"},
		{[]string{"mysql", "dibi_test", "Views", "v_orders"}, "dibi_test.v_orders"},
		{[]string{"oracle", "DIBI", "Tables", "CUSTOMERS"}, "DIBI.CUSTOMERS"},
		{[]string{"mssql", "dibi_test", "dbo", "Tables", "orders"}, "dbo.orders"},
		{[]string{"csv", "sales_2026"}, "sales_2026"},
	} {
		it := p.item(c.path...)
		if it == nil {
			continue
		}
		onUIWait(func() {
			qt.QGuiApplication_Clipboard().SetText("")
			a.tree.widget.SetFocus()
			a.tree.widget.SetCurrentItem(it)
			// Send a real Ctrl+C key press to the tree.
			ev := qt.NewQKeyEvent(qt.QEvent__KeyPress, int(qt.Key_C), qt.ControlModifier)
			qt.QCoreApplication_SendEvent(a.tree.widget.QObject, ev.QEvent)
		})
		got := ui(func() string { return qt.QGuiApplication_Clipboard().Text() })
		p.check(got == c.want, "%s → %q", strings.Join(c.path, "/"), got)
	}
	// Browsing the tree changed the last used connection; go back to pg.
	p.openInfo("pg", "dibi", "dibi_test", "Tables", "orders")

	p.log("== New script defaults to the last used connection")
	st := p.runScript("SELECT id, name, email FROM dibi_test.customers ORDER BY id;\n" +
		"SELECT n, 'row ' || n AS label, CASE WHEN n % 7 = 0 THEN NULL ELSE n * 1.5 END AS amount,\n" +
		"       now() AS ts, repeat('long text ', 40) AS long_text\nFROM generate_series(1, 2500) n;")
	p.check(ui(func() bool { return st.conn == "pg" && st.database == "dibi" }), "script bound to pg/dibi (got %s/%s)", st.conn, st.database)
	p.check(ui(func() int { return len(st.views) }) == 2, "two result tabs")
	p.log("  messages:\n%s", indent(p.messages(st)))
	p.log("  status: %s | page: %s", p.status(), p.page())
	p.check(strings.HasPrefix(p.page(), "Page 1 of ?"), "page label before paging: %q", p.page())
	p.shot("05-script-results")
	onUIWait(func() { a.goPage(-3) })
	p.wait("page 2", func() bool { v := a.currentResult(); return v != nil && v.page == 1 && !v.busy })
	p.check(strings.HasPrefix(p.page(), "Page 2 of ?  ·  rows 1001–2000"), "page 2: %q", p.page())
	onUIWait(func() { a.goPage(-1) })
	p.wait("last page", func() bool { v := a.currentResult(); return v != nil && v.page == 2 && !v.busy })
	p.check(p.page() == "Page 3 of 3  ·  rows 2001–2500", "last page: %q", p.page())
	p.shot("06-last-page")

	// Copy the full value of a truncated cell.
	onUIWait(func() {
		v := a.currentResult()
		v.table.SetCurrentIndex(v.model.Index(0, 4, qt.NewQModelIndex()))
		v.copySelection(false)
	})
	clip := ui(func() string { return qt.QGuiApplication_Clipboard().Text() })
	p.check(len(clip) == 400 && !strings.Contains(clip, "…"), "copied full long text (%d chars)", len(clip))

	p.log("== Export")
	dir := p.dir
	for _, f := range []export.Format{export.CSV, export.XLSX} {
		path := filepath.Join(dir, "export."+string(f))
		done := make(chan int, 1)
		onUIWait(func() {
			a.runExport(a.currentResult(), export.Options{Format: f, Path: path, Header: true, Separator: ','},
				func(n int, err error) {
					if err != nil {
						p.log("  export error: %v", err)
					}
					done <- n
				})
		})
		n := <-done
		info, err := os.Stat(path)
		p.check(n == 2500 && err == nil && info.Size() > 0, "%s export: %d rows, %v", f, n, err)
	}

	p.log("== Error with position")
	et := p.runScript("SELECT 1;\nSELECT id, nme FROM dibi_test.customers;")
	msg := p.messages(et)
	p.check(strings.Contains(msg, "column \"nme\" does not exist"), "error shown in messages")
	p.check(strings.HasPrefix(p.status(), "Query failed"), "status: %s", p.status())
	cursor := ui(func() int { return et.editor.TextCursor().Position() })
	p.check(cursor == len("SELECT 1;\nSELECT id, "), "cursor moved to the error (pos %d)", cursor)
	p.shot("07-error")

	p.log("== Stop")
	onUIWait(func() {
		t := a.newScript("SELECT pg_sleep(30);", "")
		_ = t
		a.run()
	})
	time.Sleep(700 * time.Millisecond)
	p.check(ui(func() bool { return a.stopAct.IsEnabled() }), "Stop enabled while running")
	onUIWait(func() { a.stop() })
	p.wait("cancel", func() bool { t := a.currentSQL(); return t != nil && !t.running })
	p.check(strings.Contains(p.status(), "cancelled"), "status: %s", p.status())
	// The session survives: run again in the same tab.
	onUIWait(func() {
		a.currentSQL().editor.SetPlainText("SELECT 42 AS answer")
		a.run()
	})
	p.wait("rerun", func() bool { t := a.currentSQL(); return !t.running })
	p.check(strings.Contains(p.status(), "completed"), "rerun after stop: %s", p.status())
	p.shot("08-after-stop")

	p.log("== Other databases")
	for _, c := range []struct {
		name, sub string
		path      []string
	}{
		{"mysql", "Source", []string{"mysql", "dibi_test", "Procedures", "touch"}},
		{"mariadb", "Constraints", []string{"mariadb", "dibi_test", "Tables", "orders"}},
		{"oracle", "Source", []string{"oracle", "DIBI", "Packages", "PKG"}},
		{"oracle", "Indexes", []string{"oracle", "DIBI", "Tables", "ORDERS"}},
		{"sqlite", "Source", []string{"sqlite", "main", "Tables", "orders", "orders_trg"}},
		{"duckdb", previewTab, []string{"duckdb", "test", "main", "Tables", "orders"}},
		{"csv", previewTab, []string{"csv", "sales_2026"}},
		{"parquet", "Columns", []string{"parquet", "events"}},
	} {
		t := p.openInfo(c.path...)
		if t == nil {
			continue
		}
		ok := p.selectInner(t, c.sub)
		if c.sub == previewTab {
			p.wait("preview", func() bool { return t.data != nil && t.data.pager != nil })
		}
		p.check(ok, "%s: %s tab (%v) · %s", c.name, c.sub, p.innerTabs(t), ui(func() string { return t.title.Text() }))
		p.shot("09-" + c.name + "-" + strings.ToLower(c.sub))
	}

	p.log("== Script on the last used connection (parquet)")
	pt := p.runScript("SELECT count(*) AS n, min(id), max(id) FROM events")
	p.check(ui(func() string { return pt.conn }) == "parquet", "bound to parquet")
	p.check(strings.Contains(p.status(), "completed"), "status: %s", p.status())

	p.log("== MySQL: DELIMITER script, Stop keeps the session")
	onUIWait(func() {
		a.setLastUsed("mysql", "dibi_test")
	})
	mt := p.runScript("DROP PROCEDURE IF EXISTS p2;\nDELIMITER //\nCREATE PROCEDURE p2() BEGIN SELECT 1; SELECT 2; END//\nDELIMITER ;\nCREATE TEMPORARY TABLE tmp_x (a INT);\nINSERT INTO tmp_x VALUES (1), (2);\nSELECT * FROM tmp_x;")
	p.log("  messages:\n%s", indent(p.messages(mt)))
	p.check(ui(func() int { return len(mt.views) }) == 1, "one result tab")
	onUIWait(func() {
		mt.editor.SetPlainText("SELECT COUNT(*) FROM information_schema.columns a, information_schema.columns b, information_schema.columns c")
		a.run()
	})
	time.Sleep(700 * time.Millisecond)
	onUIWait(func() { a.stop() })
	p.wait("mysql cancel", func() bool { return !mt.running })
	onUIWait(func() {
		mt.editor.SetPlainText("SELECT COUNT(*) AS n FROM tmp_x")
		a.run()
	})
	p.wait("mysql rerun", func() bool { return !mt.running })
	p.check(strings.Contains(p.messages(mt), "1 row(s) fetched"), "temp table still there after Stop: %s", strings.TrimSpace(p.messages(mt)))

	p.log("== SQL Server")
	if t := p.openInfo("mssql", "dibi_test", "dbo", "Tables", "orders"); t != nil {
		tabs := p.innerTabs(t)
		p.check(strings.Join(tabs, ",") == "Columns,Constraints,Indexes,"+previewTab, "info sub-tabs: %v", tabs)
		p.shot("12-mssql-columns")
		p.selectInner(t, "Constraints")
		p.shot("13-mssql-constraints")
		p.selectInner(t, previewTab)
		p.wait("preview", func() bool { return t.data != nil && t.data.pager != nil })
		p.check(p.page() != "", "preview page label: %q", p.page())
	}
	if t := p.openInfo("mssql", "dibi_test", "dbo", "Procedures", "touch"); t != nil {
		p.selectInner(t, "Source")
		p.shot("14-mssql-procedure")
	}
	onUIWait(func() { a.setLastUsed("mssql", "dibi_test") })
	sst := p.runScript("DECLARE @n INT = 2;\nSELECT TOP (@n) id, name, email, NEWID() AS guid, SYSDATETIMEOFFSET() AS ts\nFROM customers ORDER BY id;\n" +
		"SELECT @n AS n\nGO\nUPDATE orders SET amount = amount\nGO\nEXEC touch")
	p.log("  messages:\n%s", indent(p.messages(sst)))
	p.check(ui(func() bool { return sst.conn == "mssql" && sst.database == "dibi_test" }), "script bound to mssql/dibi_test (got %s/%s)", sst.conn, sst.database)
	p.check(ui(func() int { return len(sst.views) }) == 3, "three result tabs (two result sets, then EXEC)")
	p.check(strings.Contains(p.messages(sst), "Result set 2: 1 row(s)"), "second result set reported")
	p.check(strings.Contains(p.messages(sst), "[2] 1 row(s) affected"), "rows affected by the UPDATE batch")
	onUIWait(func() { sst.results.SetCurrentIndex(0) })
	p.shot("15-mssql-script")

	mst := p.runScript("SELECT 1\nGO\nSELECT * FORM customers")
	p.check(strings.Contains(p.messages(mst), "Incorrect syntax near 'FORM'"), "error shown in messages")
	cursor = ui(func() int { return mst.editor.TextCursor().Position() })
	p.check(cursor == len("SELECT 1\nGO\nSELECT * "), "cursor moved to the error (pos %d)", cursor)
	p.shot("16-mssql-error")

	p.log("== SQL Server: Stop keeps the session")
	kt := p.runScript("CREATE TABLE #tmp_x (a INT); INSERT INTO #tmp_x VALUES (1), (2)")
	onUIWait(func() {
		kt.editor.SetPlainText("WAITFOR DELAY '00:00:30'")
		a.run()
	})
	time.Sleep(700 * time.Millisecond)
	onUIWait(func() { a.stop() })
	p.wait("mssql cancel", func() bool { return !kt.running })
	p.check(strings.Contains(p.status(), "cancelled"), "status: %s", p.status())
	onUIWait(func() {
		kt.editor.SetPlainText("SELECT COUNT(*) AS n FROM #tmp_x")
		a.run()
	})
	p.wait("mssql rerun", func() bool { return !kt.running })
	p.check(strings.Contains(p.messages(kt), "1 row(s) fetched"), "temp table still there after Stop: %s", strings.TrimSpace(p.messages(kt)))

	p.log("== Dialogs")
	go func() {
		time.Sleep(800 * time.Millisecond)
		p.shot("10-new-connection")
		onUIWait(func() {
			if w := qt.QApplication_ActiveModalWidget(); w != nil {
				w.Close()
			}
		})
	}()
	onUIWait(a.newConnection)
	go func() {
		time.Sleep(800 * time.Millisecond)
		p.shot("10-edit-mssql-connection")
		onUIWait(func() {
			if w := qt.QApplication_ActiveModalWidget(); w != nil {
				w.Close()
			}
		})
	}()
	onUIWait(func() { a.editConnection("mssql") })
	onUIWait(func() { a.setLastUsed("pg", "dibi") })
	p.runScript("SELECT * FROM dibi_test.orders")
	go func() {
		time.Sleep(800 * time.Millisecond)
		p.shot("11-export-dialog")
		onUIWait(func() {
			if w := qt.QApplication_ActiveModalWidget(); w != nil {
				w.Close()
			}
		})
	}()
	onUIWait(a.export)
}

func indent(s string) string {
	s = strings.TrimRight(s, "\n")
	return "    " + strings.ReplaceAll(s, "\n", "\n    ")
}
