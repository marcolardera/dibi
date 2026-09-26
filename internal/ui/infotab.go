package ui

import (
	"context"
	"time"

	"dibi/internal/db"
	"dibi/internal/pager"

	qt "github.com/mappu/miqt/qt6"
)

// infoTab shows the details of a database object: columns, constraints,
// indexes, source code and a data preview.
type infoTab struct {
	app    *App
	widget *qt.QWidget
	conn   string
	node   db.Node
	src    *db.Source

	title *qt.QLabel
	inner *qt.QTabWidget
	data  *resultView
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
	layout.AddWidget(t.inner.QWidget)
	t.load()
	return t
}

func (t *infoTab) load() {
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
		t.inner.AddTab(t.data.widget(), "Data")
		t.loadPreview(info.Preview)
	}
	if t.inner.Count() == 0 {
		t.inner.AddTab(qt.NewQLabel3("No details available for this object.").QWidget, "Details")
	}
	t.app.refreshChrome()
}

func (t *infoTab) loadPreview(query string) {
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
		if r.err != nil {
			idx := t.inner.IndexOf(t.data.widget())
			msg := qt.NewQPlainTextEdit2()
			msg.SetReadOnly(true)
			msg.AppendHtml(errorHTML("Cannot load the data preview: ", r.err.Error()))
			t.inner.RemoveTab(idx)
			t.inner.InsertTab(idx, msg.QWidget, "Data")
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
	tw.SetAlternatingRowColors(true)
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
