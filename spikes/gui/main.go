// Spike: validate the Dibi GUI shell with miqt/Qt 6.
//
// Covers: grouped large-icon toolbar, object tree, tabbed SQL editor with a
// QSyntaxHighlighter, a custom QAbstractTableModel showing one 1000-row page
// streamed from DuckDB, status-bar pagination, Run/Stop with Ctrl+Enter,
// background queries marshalled back to the UI thread, and Ctrl+C copy.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dibi/spikes/pager"

	_ "github.com/duckdb/duckdb-go/v2"
	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"
)

const maxCellChars = 200

// ---------------------------------------------------------------- SQL highlighter

var sqlKeywords = []string{
	"select", "from", "where", "and", "or", "not", "in", "is", "null", "as", "on",
	"join", "left", "right", "inner", "outer", "full", "cross", "group", "by", "order",
	"having", "limit", "offset", "union", "all", "distinct", "insert", "into", "values",
	"update", "set", "delete", "create", "table", "view", "index", "drop", "alter",
	"primary", "key", "foreign", "references", "case", "when", "then", "else", "end",
	"with", "exists", "between", "like", "asc", "desc", "begin", "commit", "rollback",
}

type highlightRule struct {
	re     *qt.QRegularExpression
	format *qt.QTextCharFormat
}

func charFormat(color string, bold, italic bool) *qt.QTextCharFormat {
	f := qt.NewQTextCharFormat()
	f.SetForeground(qt.NewQBrush3(qt.NewQColor6(color)))
	if bold {
		f.SetFontWeight(int(qt.QFont__Bold))
	}
	f.SetFontItalic(italic)
	return f
}

func newSQLHighlighter(doc *qt.QTextDocument) *qt.QSyntaxHighlighter {
	h := qt.NewQSyntaxHighlighter2(doc)
	ci := qt.QRegularExpression__CaseInsensitiveOption
	rules := []highlightRule{
		{qt.NewQRegularExpression4(`\b(`+strings.Join(sqlKeywords, "|")+`)\b`, ci), charFormat("#0033b3", true, false)},
		{qt.NewQRegularExpression2(`\b\d+(\.\d+)?\b`), charFormat("#1750eb", false, false)},
		{qt.NewQRegularExpression2(`'([^']|'')*'`), charFormat("#067d17", false, false)},
		{qt.NewQRegularExpression2(`--[^\n]*`), charFormat("#8c8c8c", false, true)},
	}
	comment := charFormat("#8c8c8c", false, true)
	h.OnHighlightBlock(func(text string) {
		for _, r := range rules {
			it := r.re.GlobalMatch(text)
			for it.HasNext() {
				m := it.Next()
				h.SetFormat(int(m.CapturedStart()), int(m.CapturedLength()), r.format)
			}
		}
		// Multi-line /* */ comments: block state 1 means "inside a comment".
		// Note: offsets are UTF-16 code units, as Qt expects.
		h.SetCurrentBlockState(0)
		start := 0
		if h.PreviousBlockState() != 1 {
			start = strings.Index(text, "/*")
		}
		for start >= 0 {
			end := strings.Index(text[start:], "*/")
			if end < 0 {
				h.SetCurrentBlockState(1)
				h.SetFormat(start, len(text)-start, comment)
				break
			}
			h.SetFormat(start, end+2, comment)
			next := strings.Index(text[start+end+2:], "/*")
			if next < 0 {
				break
			}
			start = start + end + 2 + next
		}
	})
	return h
}

// ---------------------------------------------------------------- result model

type resultModel struct {
	*qt.QAbstractTableModel
	cols []string
	rows [][]any
}

func formatValue(v any) (text string, isNull bool) {
	switch x := v.(type) {
	case nil:
		return "NULL", true
	case []byte:
		return fmt.Sprintf("<binary %s>", humanSize(len(x))), false
	case time.Time:
		if x.Hour() == 0 && x.Minute() == 0 && x.Second() == 0 && x.Nanosecond() == 0 {
			return x.Format("2006-01-02"), false
		}
		return x.Format("2006-01-02 15:04:05.999999999"), false
	case fmt.Stringer:
		return x.String(), false
	default:
		return fmt.Sprint(x), false
	}
}

func humanSize(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/1024/1024)
	}
}

func truncate(s string) string {
	r := []rune(s)
	if len(r) <= maxCellChars {
		return s
	}
	return string(r[:maxCellChars]) + "…"
}

func newResultModel() *resultModel {
	m := &resultModel{QAbstractTableModel: qt.NewQAbstractTableModel()}
	gray := qt.NewQColor6("#9a9a9a")
	m.OnRowCount(func(parent *qt.QModelIndex) int {
		if parent.IsValid() {
			return 0
		}
		return len(m.rows)
	})
	m.OnColumnCount(func(parent *qt.QModelIndex) int {
		if parent.IsValid() {
			return 0
		}
		return len(m.cols)
	})
	m.OnData(func(idx *qt.QModelIndex, role int) *qt.QVariant {
		if !idx.IsValid() || idx.Row() >= len(m.rows) {
			return qt.NewQVariant()
		}
		v := m.rows[idx.Row()][idx.Column()]
		switch qt.ItemDataRole(role) {
		case qt.DisplayRole:
			text, _ := formatValue(v)
			return qt.NewQVariant14(truncate(text))
		case qt.ForegroundRole:
			if v == nil {
				return gray.ToQVariant()
			}
		}
		return qt.NewQVariant()
	})
	m.OnHeaderData(func(super func(int, qt.Orientation, int) *qt.QVariant, section int, o qt.Orientation, role int) *qt.QVariant {
		if o == qt.Horizontal && qt.ItemDataRole(role) == qt.DisplayRole && section < len(m.cols) {
			return qt.NewQVariant14(m.cols[section])
		}
		return super(section, o, role)
	})
	return m
}

func (m *resultModel) set(cols []string, rows [][]any) {
	m.BeginResetModel()
	m.cols, m.rows = cols, rows
	m.EndResetModel()
}

// fullText returns the untruncated value, used by copy.
func (m *resultModel) fullText(row, col int) string {
	text, _ := formatValue(m.rows[row][col])
	return text
}

// ---------------------------------------------------------------- SQL tab

type sqlTab struct {
	widget *qt.QWidget
	editor *qt.QPlainTextEdit
	table  *qt.QTableView
	model  *resultModel
	errors *qt.QPlainTextEdit
	stack  *qt.QStackedWidget
	pager  *pager.Pager
	page   int
	// cancel cancels the context of the current cursor; it lives as long as
	// the pager, because later page fetches still use that context.
	cancel  context.CancelFunc
	running bool
}

func newSQLTab(initial string) *sqlTab {
	t := &sqlTab{widget: qt.NewQWidget2()}
	layout := qt.NewQVBoxLayout(t.widget)
	layout.SetContentsMargins(0, 0, 0, 0)

	split := qt.NewQSplitter3(qt.Vertical)
	t.editor = qt.NewQPlainTextEdit3(initial)
	mono := qt.QFontDatabase_SystemFont(qt.QFontDatabase__FixedFont)
	t.editor.SetFont(mono)
	newSQLHighlighter(t.editor.Document())

	t.model = newResultModel()
	t.table = qt.NewQTableView2()
	t.table.SetModel(t.model.QAbstractItemModel)
	t.table.SetAlternatingRowColors(true)
	t.table.SetHorizontalScrollMode(qt.QAbstractItemView__ScrollPerPixel)
	t.table.VerticalHeader().SetDefaultSectionSize(22)
	t.table.SetContextMenuPolicy(qt.ActionsContextMenu)

	copyAct := qt.NewQAction5("Copy", t.table.QObject)
	copyAct.SetShortcut(qt.NewQKeySequence6(qt.QKeySequence__Copy))
	copyAct.SetShortcutContext(qt.WidgetWithChildrenShortcut)
	copyAct.OnTriggered(t.copySelection)
	t.table.AddAction(copyAct)

	t.errors = qt.NewQPlainTextEdit2()
	t.errors.SetReadOnly(true)
	t.errors.SetStyleSheet("color: #b00020;")

	t.stack = qt.NewQStackedWidget2()
	t.stack.AddWidget(t.table.QWidget)
	t.stack.AddWidget(t.errors.QWidget)

	split.AddWidget(t.editor.QWidget)
	split.AddWidget(t.stack.QWidget)
	split.SetSizes([]int{200, 400})
	layout.AddWidget(split.QWidget)
	return t
}

// closeCursor releases the current cursor, cancelling it if still running.
func (t *sqlTab) closeCursor() {
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}
	if t.pager != nil {
		t.pager.Close()
		t.pager = nil
	}
}

// sqlToRun returns the selection if any, otherwise the whole editor content.
func (t *sqlTab) sqlToRun() string {
	if sel := t.editor.TextCursor().SelectedText(); sel != "" {
		// Qt uses U+2029 as paragraph separator in selections.
		return strings.ReplaceAll(sel, " ", "\n")
	}
	return t.editor.ToPlainText()
}

func (t *sqlTab) copySelection() {
	idxs := t.table.SelectionModel().SelectedIndexes()
	if len(idxs) == 0 {
		return
	}
	minR, maxR, minC, maxC := idxs[0].Row(), idxs[0].Row(), idxs[0].Column(), idxs[0].Column()
	sel := map[[2]int]bool{}
	for _, i := range idxs {
		r, c := i.Row(), i.Column()
		sel[[2]int{r, c}] = true
		minR, maxR, minC, maxC = min(minR, r), max(maxR, r), min(minC, c), max(maxC, c)
	}
	var b strings.Builder
	for r := minR; r <= maxR; r++ {
		for c := minC; c <= maxC; c++ {
			if c > minC {
				b.WriteByte('\t')
			}
			if sel[[2]int{r, c}] {
				b.WriteString(t.model.fullText(r, c))
			}
		}
		if r < maxR {
			b.WriteByte('\n')
		}
	}
	qt.QGuiApplication_Clipboard().SetText(b.String())
}

// ---------------------------------------------------------------- main window

type app struct {
	win     *qt.QMainWindow
	tabs    *qt.QTabWidget
	status  *qt.QLabel
	pageLbl *qt.QLabel
	runAct  *qt.QAction
	stopAct *qt.QAction
	db      *sql.DB
	sqlTabs map[uintptr]*sqlTab
	nScript int
}

func (a *app) current() *sqlTab {
	w := a.tabs.CurrentWidget()
	if w == nil {
		return nil
	}
	return a.sqlTabs[uintptr(w.UnsafePointer())]
}

func (a *app) newScript(text string) {
	t := newSQLTab(text)
	a.nScript++
	a.sqlTabs[uintptr(t.widget.UnsafePointer())] = t
	a.tabs.SetCurrentIndex(a.tabs.AddTab(t.widget, fmt.Sprintf("Script %d", a.nScript)))
	t.editor.SetFocus()
}

func (a *app) updateActions() {
	t := a.current()
	a.runAct.SetEnabled(t != nil && !t.running)
	a.stopAct.SetEnabled(t != nil && t.running)
	a.updatePageLabel()
}

func (a *app) updatePageLabel() {
	t := a.current()
	if t == nil || t.pager == nil {
		a.pageLbl.SetText("")
		return
	}
	total := "?"
	if t.pager.Done() {
		total = fmt.Sprint(max(t.pager.Fetched(), 1))
	}
	a.pageLbl.SetText(fmt.Sprintf("Page %d of %s", t.page+1, total))
}

func (a *app) run() {
	t := a.current()
	if t == nil || t.running {
		return
	}
	query := strings.TrimSpace(t.sqlToRun())
	if query == "" {
		return
	}
	t.closeCursor()
	ctx, cancel := context.WithCancel(context.Background())
	t.cancel, t.running = cancel, true
	a.updateActions()
	a.status.SetText("Running…")
	start := time.Now()

	go func() {
		p, err := pager.New(ctx, a.db, query)
		var rows [][]any
		if err == nil {
			rows, err = p.Page(0)
		}
		elapsed := time.Since(start)
		mainthread.Wait(func() {
			t.running = false
			if err != nil {
				if p != nil {
					p.Close()
				}
				cancel()
				t.cancel = nil
				t.errors.SetPlainText(err.Error())
				t.stack.SetCurrentIndex(1)
				a.status.SetText(fmt.Sprintf("Query failed after %.3f s.", elapsed.Seconds()))
			} else {
				t.pager, t.page = p, 0
				cols := make([]string, len(p.Cols))
				for i, c := range p.Cols {
					cols[i] = c.Name()
				}
				t.model.set(cols, rows)
				t.stack.SetCurrentIndex(0)
				a.status.SetText(fmt.Sprintf("Query completed in %.3f s.", elapsed.Seconds()))
			}
			a.updateActions()
		})
	}()
}

func (a *app) stop() {
	if t := a.current(); t != nil && t.running {
		t.cancel()
		a.status.SetText("Cancelling…")
	}
}

// goPage moves to a page; n < 0 means last page. Fetching happens off the UI thread.
func (a *app) goPage(n int) {
	t := a.current()
	if t == nil || t.pager == nil || t.running {
		return
	}
	if n < 0 && t.page == 0 && !t.pager.Done() {
		a.status.SetText("Fetching all rows…")
	}
	p := t.pager
	t.running = true
	a.updateActions()
	go func() {
		var err error
		if n < 0 {
			n, err = p.Last()
		}
		var rows [][]any
		if err == nil {
			rows, err = p.Page(max(n, 0))
		}
		mainthread.Wait(func() {
			t.running = false
			a.updateActions()
			if err != nil {
				a.status.SetText(err.Error())
				t.closeCursor()
				a.updatePageLabel()
				return
			}
			if rows == nil { // past the end
				a.updatePageLabel()
				return
			}
			t.page = max(n, 0)
			t.model.BeginResetModel()
			t.model.rows = rows
			t.model.EndResetModel()
			t.table.ScrollToTop()
			a.updatePageLabel()
		})
	}()
}

func stdIcon(p qt.QStyle__StandardPixmap) *qt.QIcon {
	return qt.QApplication_Style().StandardIcon(p, nil, nil)
}

func main() {
	demo, db := prepareDemoData()
	defer os.RemoveAll(filepath.Dir(demo))
	defer db.Close()

	qt.NewQApplication(os.Args)
	a := &app{db: db, sqlTabs: map[uintptr]*sqlTab{}}
	a.win = qt.NewQMainWindow2()
	a.win.SetWindowTitle("Dibi (spike)")
	a.win.Resize(1200, 800)

	// Toolbar: grouped, large icons, text below.
	tb := qt.NewQToolBar3()
	tb.SetMovable(false)
	tb.SetToolButtonStyle(qt.ToolButtonTextUnderIcon)
	tb.SetIconSize(qt.NewQSize2(32, 32))
	a.win.AddToolBarWithToolbar(tb)
	addAct := func(icon qt.QStyle__StandardPixmap, text string, f func()) *qt.QAction {
		act := qt.NewQAction3(stdIcon(icon), text)
		act.OnTriggered(f)
		tb.QWidget.AddAction(act)
		return act
	}
	addAct(qt.QStyle__SP_DriveNetIcon, "New\nConnection", func() {
		qt.QMessageBox_Information(a.win.QWidget, "New Connection", "Not part of the spike.")
	})
	tb.AddSeparator()
	addAct(qt.QStyle__SP_FileIcon, "New\nScript", func() { a.newScript("") })
	addAct(qt.QStyle__SP_DialogOpenButton, "Open\nScript", func() {})
	addAct(qt.QStyle__SP_DialogSaveButton, "Save\nScript", func() {})
	tb.AddSeparator()
	a.runAct = addAct(qt.QStyle__SP_MediaPlay, "Run", a.run)
	a.stopAct = addAct(qt.QStyle__SP_MediaStop, "Stop", a.stop)
	a.runAct.SetShortcut(qt.NewQKeySequence2("Ctrl+Return")) // Ctrl maps to Cmd on macOS
	a.runAct.SetShortcutContext(qt.ApplicationShortcut)

	// Left tree.
	tree := qt.NewQTreeWidget2()
	tree.SetHeaderHidden(true)
	conn := qt.NewQTreeWidgetItem4(tree, []string{"demo (DuckDB)"})
	schema := qt.NewQTreeWidgetItem7(conn, []string{"main"})
	tables := qt.NewQTreeWidgetItem7(schema, []string{"Tables"})
	qt.NewQTreeWidgetItem7(tables, []string{"events"})
	tree.ExpandAll()
	tree.OnItemClicked(func(item *qt.QTreeWidgetItem, _ int) {
		if item.Text(0) == "events" {
			a.newScript("SELECT * FROM events;")
			a.run()
		}
	})

	a.tabs = qt.NewQTabWidget2()
	a.tabs.SetTabsClosable(true)
	a.tabs.SetDocumentMode(true)
	a.tabs.OnCurrentChanged(func(int) { a.updateActions() })
	a.tabs.OnTabCloseRequested(func(i int) {
		w := a.tabs.Widget(i)
		key := uintptr(w.UnsafePointer())
		if t := a.sqlTabs[key]; t != nil {
			t.closeCursor()
			delete(a.sqlTabs, key)
		}
		a.tabs.RemoveTab(i)
		w.DeleteLater()
	})

	split := qt.NewQSplitter3(qt.Horizontal)
	split.AddWidget(tree.QWidget)
	split.AddWidget(a.tabs.QWidget)
	split.SetSizes([]int{250, 950})
	a.win.SetCentralWidget(split.QWidget)

	// Status bar: message left, pagination + export right.
	sb := a.win.StatusBar()
	a.status = qt.NewQLabel3("Ready.")
	sb.AddWidget(a.status.QWidget)
	navBtn := func(icon qt.QStyle__StandardPixmap, tip string, f func()) {
		b := qt.NewQToolButton2()
		b.SetIcon(stdIcon(icon))
		b.SetToolTip(tip)
		b.SetAutoRaise(true)
		b.OnClicked(f)
		sb.AddPermanentWidget(b.QWidget)
	}
	navBtn(qt.QStyle__SP_MediaSkipBackward, "First page", func() { a.goPage(0) })
	navBtn(qt.QStyle__SP_MediaSeekBackward, "Previous page", func() {
		if t := a.current(); t != nil && t.page > 0 {
			a.goPage(t.page - 1)
		}
	})
	a.pageLbl = qt.NewQLabel2()
	sb.AddPermanentWidget(a.pageLbl.QWidget)
	navBtn(qt.QStyle__SP_MediaSeekForward, "Next page", func() {
		if t := a.current(); t != nil {
			a.goPage(t.page + 1)
		}
	})
	navBtn(qt.QStyle__SP_MediaSkipForward, "Last page", func() { a.goPage(-1) })
	export := qt.NewQPushButton3("Export…")
	sb.AddPermanentWidget(export.QWidget)

	a.newScript("-- Ctrl+Enter runs the selection, or the whole script\n" +
		"SELECT id, name, amount, created, payload, note\n" +
		"FROM events\n" +
		"WHERE id < 5000 /* multi-line\n   comment */\n" +
		"ORDER BY id;")
	a.updateActions()
	a.win.Show()
	if dir := os.Getenv("DIBI_SPIKE_SHOT"); dir != "" {
		go a.autopilot(dir)
	}
	qt.QApplication_Exec()
}

// autopilot drives the UI and saves screenshots, for unattended checks.
func (a *app) autopilot(dir string) {
	shot := func(name string) {
		time.Sleep(700 * time.Millisecond)
		mainthread.Wait(func() {
			a.win.Grab().Save(filepath.Join(dir, name+".png"))
			fmt.Printf("%s: status=%q page=%q\n", name, a.status.Text(), a.pageLbl.Text())
		})
	}
	shot("1-start")
	mainthread.Wait(a.run)
	shot("2-run")
	mainthread.Wait(func() { a.goPage(1) })
	shot("3-page2")
	mainthread.Wait(func() {
		a.newScript("SELECT * FROM events;")
		a.run()
	})
	shot("4-full-table")
	mainthread.Wait(func() { a.goPage(-1) })
	shot("5-last-page")
	mainthread.Wait(func() {
		t := a.current()
		t.table.SelectAll()
		t.copySelection()
		fmt.Printf("clipboard bytes after copying last page: %d\n", len(qt.QGuiApplication_Clipboard().Text()))
		a.newScript("SELECT * FROM missing_table;")
		a.run()
	})
	shot("6-error")
	mainthread.Wait(func() {
		a.newScript("SELECT count(*) FROM range(100000000) a, range(1000) b WHERE a.range % 997 = b.range;")
		a.run()
	})
	time.Sleep(300 * time.Millisecond)
	mainthread.Wait(a.stop)
	shot("7-stopped")
	mainthread.Wait(func() { qt.QCoreApplication_Quit() })
}

// prepareDemoData creates a 1M-row Parquet file and a DuckDB view over it.
func prepareDemoData() (string, *sql.DB) {
	dir, err := os.MkdirTemp("", "dibi-gui-spike")
	if err != nil {
		log.Fatal(err)
	}
	path := filepath.Join(dir, "events.parquet")
	db, err := sql.Open("duckdb", "")
	if err != nil {
		log.Fatal(err)
	}
	db.SetMaxOpenConns(4)
	_, err = db.Exec(fmt.Sprintf(`COPY (
		SELECT i AS id, 'name_' || i AS name,
		       CASE WHEN i %% 7 = 0 THEN NULL ELSE i * 1.5 END AS amount,
		       TIMESTAMP '2026-01-01' + INTERVAL (i) SECOND AS created,
		       'xyz'::BLOB AS payload,
		       CASE WHEN i %% 100 = 0 THEN repeat('long text ', 50) ELSE '' END AS note
		FROM range(1000000) t(i)) TO '%s' (FORMAT parquet)`, path))
	if err != nil {
		log.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf("CREATE VIEW events AS SELECT * FROM read_parquet('%s')", path)); err != nil {
		log.Fatal(err)
	}
	return path, db
}
