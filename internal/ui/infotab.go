package ui

import (
	"context"
	"strings"
	"time"

	"dibi/internal/db"
	"dibi/internal/pager"

	qt "github.com/mappu/miqt/qt6"
)

// previewTab is the title of the data preview sub-tab.
const previewTab = "Data preview"

// infoTab shows the details of a database object: columns, constraints,
// indexes, source code and a data preview.
type infoTab struct {
	app    *App
	widget *qt.QWidget
	conn   string
	node   db.Node
	src    *db.Source

	title   *qt.QLabel
	inner   *qt.QTabWidget
	data    *resultView
	refresh *qt.QToolButton
	edit    *qt.QToolButton
	script  string // script opened by the edit button

	gen     int    // incremented by reload and dispose: older loads are dropped
	current string // sub-tab to show again after a reload
}

func newInfoTab(app *App, conn string, src *db.Source, node db.Node) *infoTab {
	t := &infoTab{app: app, conn: conn, src: src, node: node, widget: qt.NewQWidget2()}
	layout := qt.NewQVBoxLayout(t.widget)
	layout.SetContentsMargins(6, 6, 6, 0)
	t.title = qt.NewQLabel3("Loading…")
	f := t.title.Font()
	f.SetBold(true)
	f.SetPointSizeF(f.PointSizeF() * 1.15)
	t.title.SetFont(f)
	t.title.SetTextInteractionFlags(qt.TextSelectableByMouse)
	layout.AddWidget(t.title.QWidget)
	t.inner = qt.NewQTabWidget2()
	t.inner.SetDocumentMode(true)
	t.inner.OnCurrentChanged(func(int) { app.refreshChrome() })
	t.refresh = qt.NewQToolButton2()
	t.refresh.SetIcon(icon("view-refresh", qt.QStyle__SP_BrowserReload))
	t.refresh.SetText("Refresh")
	t.refresh.SetToolTip("Reload the details and the data preview")
	t.refresh.SetToolButtonStyle(qt.ToolButtonTextBesideIcon)
	t.refresh.SetAutoRaise(true)
	t.refresh.OnClicked(t.reload)
	t.edit = qt.NewQToolButton2()
	t.edit.SetIcon(icon("document-edit", qt.QStyle__SP_FileDialogContentsView))
	t.edit.SetText("Edit " + strings.ToLower(db.KindLabel(node.Kind)))
	t.edit.SetToolTip("Open the definition in a new SQL script, ready to be changed and run")
	t.edit.SetToolButtonStyle(qt.ToolButtonTextBesideIcon)
	t.edit.SetAutoRaise(true)
	t.edit.SetVisible(false)
	t.edit.OnClicked(t.editScript)
	corner := qt.NewQWidget2()
	corner.SetLayout(hbox(t.edit.QWidget, t.refresh.QWidget).QLayout)
	t.inner.SetCornerWidget2(corner, qt.TopRightCorner)
	layout.AddWidget(t.inner.QWidget)
	t.load()
	return t
}

// reload loads the details and the data preview again, keeping the
// visible sub-tab.
func (t *infoTab) reload() {
	t.gen++
	if i := t.inner.CurrentIndex(); i >= 0 {
		t.current = t.inner.TabText(i)
	}
	if t.data != nil {
		t.data.close()
		t.data = nil
	}
	for t.inner.Count() > 0 {
		w := t.inner.Widget(0)
		t.inner.RemoveTab(0)
		w.DeleteLater()
	}
	t.edit.SetVisible(false)
	t.load()
	t.app.refreshChrome()
}

// editScript opens the script redefining the object in a new SQL editor,
// bound to the connection and database of the object.
func (t *infoTab) editScript() {
	t.app.setLastUsed(t.conn, t.node.Database)
	t.app.newScript(t.script, "")
}

// dispose drops the loads in progress, before the tab is closed.
func (t *infoTab) dispose() {
	t.gen++
	if t.data != nil {
		t.data.close()
	}
}

func (t *infoTab) load() {
	gen := t.gen
	type result struct {
		info *db.Info
		err  error
	}
	async(func() result {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		info, err := t.src.Info(ctx, t.node)
		return result{info, err}
	}, func(r result) {
		if t.gen != gen {
			return
		}
		if r.err != nil {
			t.title.SetText(db.KindLabel(t.node.Kind) + " " + t.node.Name)
			msg := qt.NewQPlainTextEdit2()
			msg.SetReadOnly(true)
			msg.AppendHtml(errorHTML("Cannot load information: ", r.err.Error()))
			t.inner.AddTab(msg.QWidget, "Error")
			t.app.setStatus("Cannot load " + t.node.Name + ".")
			return
		}
		t.build(r.info)
	})
}

func (t *infoTab) build(info *db.Info) {
	t.title.SetText(info.Title + "   ·   " + t.conn)
	t.script = info.EditScript
	t.edit.SetVisible(t.script != "")
	if len(info.Columns) > 0 {
		rows := make([][]string, len(info.Columns))
		for i, c := range info.Columns {
			pk, null := "", "NOT NULL"
			if c.PrimaryKey {
				pk = "PK"
			}
			if c.Nullable {
				null = ""
			}
			rows[i] = []string{c.Name, c.Type, null, pk, c.Default, c.Comment}
		}
		t.inner.AddTab(stringTable([]string{"Name", "Type", "Null", "Key", "Default", "Comment"}, rows).QWidget,
			"Columns")
	}
	if len(info.Constraints) > 0 {
		rows := make([][]string, len(info.Constraints))
		for i, c := range info.Constraints {
			rows[i] = []string{c.Name, c.Type, c.Columns, c.Definition}
		}
		t.inner.AddTab(stringTable([]string{"Name", "Type", "Columns", "Definition"}, rows).QWidget, "Constraints")
	}
	if len(info.Indexes) > 0 {
		rows := make([][]string, len(info.Indexes))
		for i, x := range info.Indexes {
			unique := ""
			if x.Unique {
				unique = "UNIQUE"
			}
			rows[i] = []string{x.Name, unique, x.Columns}
		}
		t.inner.AddTab(stringTable([]string{"Name", "Unique", "Columns"}, rows).QWidget, "Indexes")
	}
	if len(info.Properties) > 0 {
		rows := make([][]string, len(info.Properties))
		for i, p := range info.Properties {
			rows[i] = []string{p[0], p[1]}
		}
		t.inner.AddTab(stringTable([]string{"Property", "Value"}, rows).QWidget, "Properties")
	}
	if info.Source != "" {
		src := qt.NewQPlainTextEdit3(info.Source)
		src.SetReadOnly(true)
		src.SetFont(monoFont())
		src.SetLineWrapMode(qt.QPlainTextEdit__NoWrap)
		src.SetTextInteractionFlags(qt.TextSelectableByMouse | qt.TextSelectableByKeyboard)
		newSQLHighlighter(src.Document())
		t.inner.AddTab(src.QWidget, "Source")
	}
	if info.Preview != "" {
		t.data = newResultView(t.app)
		t.data.src, t.data.database = t.src, t.node.Database
		t.data.exportQuery = info.SelectAll
		t.data.exportNote = "All rows of " + t.node.Name + " are exported, not only the preview."
		t.inner.AddTab(t.data.widget(), previewTab)
		t.loadPreview(info.Preview)
	}
	if t.inner.Count() == 0 {
		t.inner.AddTab(qt.NewQLabel3("No details available for this object.").QWidget, "Details")
	}
	for i := 0; i < t.inner.Count(); i++ {
		if t.inner.TabText(i) == t.current {
			t.inner.SetCurrentIndex(i)
		}
	}
	t.app.refreshChrome()
}

func (t *infoTab) loadPreview(query string) {
	gen := t.gen
	type result struct {
		p    *pager.Pager
		rows [][]any
		err  error
	}
	async(func() result {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		pool, err := t.src.Pool(t.node.Database)
		if err != nil {
			return result{err: err}
		}
		p, err := pager.New(ctx, pool, query)
		if err != nil {
			return result{err: err}
		}
		rows, err := p.Page(0)
		p.Close()
		return result{p, rows, err}
	}, func(r result) {
		if t.gen != gen {
			return
		}
		if r.err != nil {
			idx := t.inner.IndexOf(t.data.widget())
			msg := qt.NewQPlainTextEdit2()
			msg.SetReadOnly(true)
			msg.AppendHtml(errorHTML("Cannot load the data preview: ", r.err.Error()))
			t.inner.RemoveTab(idx)
			t.inner.InsertTab(idx, msg.QWidget, previewTab)
			t.data = nil
			return
		}
		t.data.show(r.p, r.rows, true)
		t.app.refreshChrome()
	})
}

// currentResult returns the data grid if it is the visible sub-tab.
func (t *infoTab) currentResult() *resultView {
	if t.data == nil || t.inner.CurrentWidget() == nil ||
		t.inner.CurrentWidget().UnsafePointer() != t.data.widget().UnsafePointer() {
		return nil
	}
	return t.data
}

// stringTable builds a read-only table of strings.
func stringTable(headers []string, rows [][]string) *qt.QTableWidget {
	tw := qt.NewQTableWidget3(len(rows), len(headers))
	tw.SetHorizontalHeaderLabels(headers)
	tw.SetEditTriggers(qt.QAbstractItemView__NoEditTriggers)
	stripeRows(tw.QAbstractItemView)
	tw.SetWordWrap(false)
	tw.VerticalHeader().SetVisible(false)
	tw.VerticalHeader().SetDefaultSectionSize(tw.FontMetrics().Height() + 6)
	tw.HorizontalHeader().SetStretchLastSection(true)
	for r, row := range rows {
		for c, s := range row {
			item := qt.NewQTableWidgetItem2(s)
			if len(s) > 60 {
				item.SetToolTip(s)
			}
			tw.SetItem(r, c, item)
		}
	}
	tw.ResizeColumnsToContents()
	for c := range headers {
		if tw.HorizontalHeader().SectionSize(c) > maxColumnWidth {
			tw.HorizontalHeader().ResizeSection(c, maxColumnWidth)
		}
	}
	return tw
}
