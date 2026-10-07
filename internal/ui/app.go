package ui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"dibi/internal/config"
	"dibi/internal/db"
	"dibi/internal/export"

	qt "github.com/mappu/miqt/qt6"
)

// App is the main window and the application state.
type App struct {
	store   *config.Store
	conns   []config.Connection
	sources map[string]*db.Source

	win      *qt.QMainWindow
	tree     *objectTree
	tabs     *qt.QTabWidget
	sqlTabs  map[uintptr]*sqlTab
	infoTabs map[uintptr]*infoTab

	status    *qt.QLabel
	pageLabel *qt.QLabel
	navFirst  *qt.QToolButton
	navPrev   *qt.QToolButton
	navNext   *qt.QToolButton
	navLast   *qt.QToolButton
	exportBtn *qt.QPushButton

	runAct, stopAct, saveAct *qt.QAction
	themeAct                 *qt.QAction

	lastConn, lastDB string
	theme            string
	scripts          int
}

// Run creates the application window and runs the event loop.
func Run(store *config.Store) int {
	qt.NewQApplication(os.Args)
	qt.QCoreApplication_SetApplicationName("Dibi")
	qt.QGuiApplication_SetDesktopFileName("dibi")

	a := &App{store: store, sources: map[string]*db.Source{},
		sqlTabs: map[uintptr]*sqlTab{}, infoTabs: map[uintptr]*infoTab{}}
	st := store.LoadState()
	a.lastConn, a.lastDB = st.LastConnection, st.LastDatabase
	a.theme = themeByName(st.Theme).name
	applyTheme(themeByName(a.theme))
	a.build()
	a.loadConnections()
	if len(a.conns) == 0 {
		a.setStatus("Welcome to Dibi. Create a connection to get started.")
	}
	a.win.Show()
	if autopilot != nil {
		go autopilot(a)
	}
	return qt.QApplication_Exec()
}

// autopilot, when set (only in builds with the "autopilot" tag), drives the
// UI for automated end-to-end checks.
var autopilot func(*App)

func (a *App) build() {
	a.win = qt.NewQMainWindow2()
	a.win.SetWindowTitle("Dibi")
	a.win.Resize(1280, 820)
	a.win.SetWindowIcon(icon("server-database", qt.QStyle__SP_DriveNetIcon))

	a.buildToolbar()

	a.tree = newObjectTree(a)
	a.tabs = qt.NewQTabWidget2()
	a.tabs.SetTabsClosable(true)
	a.tabs.SetMovable(true)
	a.tabs.SetDocumentMode(true)
	a.tabs.OnTabCloseRequested(a.closeTab)
	a.tabs.OnCurrentChanged(func(int) {
		if t := a.currentSQL(); t != nil {
			a.setLastUsed(t.conn, t.database)
		} else if it := a.currentInfo(); it != nil {
			a.setLastUsed(it.conn, it.node.Database)
		}
		a.refreshChrome()
	})

	split := qt.NewQSplitter3(qt.Horizontal)
	split.AddWidget(a.tree.panel)
	split.AddWidget(a.tabs.QWidget)
	split.SetStretchFactor(0, 0)
	split.SetStretchFactor(1, 1)
	split.SetSizes([]int{280, 1000})
	a.win.SetCentralWidget(split.QWidget)

	a.buildStatusBar()

	a.win.OnCloseEvent(func(super func(*qt.QCloseEvent), ev *qt.QCloseEvent) {
		if !a.confirmQuit() {
			ev.Ignore()
			return
		}
		for _, s := range a.sources {
			s.Close()
		}
		super(ev)
	})
	a.refreshChrome()
}

func (a *App) buildToolbar() {
	tb := qt.NewQToolBar2("Main")
	tb.SetMovable(false)
	tb.SetFloatable(false)
	tb.SetToolButtonStyle(qt.ToolButtonTextUnderIcon)
	tb.SetIconSize(qt.NewQSize2(32, 32))
	tb.SetContextMenuPolicy(qt.PreventContextMenu)
	a.win.AddToolBarWithToolbar(tb)

	add := func(ic *qt.QIcon, text, tip, shortcut string, f func()) *qt.QAction {
		act := qt.NewQAction3(ic, text)
		act.SetToolTip(tip)
		if shortcut != "" {
			act.SetShortcut(qt.NewQKeySequence2(shortcut))
			act.SetShortcutContext(qt.ApplicationShortcut)
			act.SetToolTip(tip + " (" + qt.NewQKeySequence2(shortcut).ToStringWithFormat(qt.QKeySequence__NativeText) + ")")
		}
		act.OnTriggered(f)
		tb.QWidget.AddAction(act)
		return act
	}

	// Connection group.
	add(icon("network-server", qt.QStyle__SP_DriveNetIcon), "New\nConnection", "Create a new connection", "", a.newConnection)
	tb.AddSeparator()
	// Script group.
	add(icon("document-new", qt.QStyle__SP_FileIcon), "New\nScript", "New SQL script", "Ctrl+N", func() { a.newScript("", "") })
	add(icon("document-open", qt.QStyle__SP_DialogOpenButton), "Open\nScript", "Open a saved query", "Ctrl+O", a.openScript)
	a.saveAct = add(icon("document-save", qt.QStyle__SP_DialogSaveButton), "Save\nScript", "Save the query", "Ctrl+S", func() { a.saveScript() })
	tb.AddSeparator()
	// Execute group.
	a.runAct = add(icon("media-playback-start", qt.QStyle__SP_MediaPlay), "Run", "Run the selection, or the whole script", "Ctrl+Return", a.run)
	a.stopAct = add(icon("media-playback-stop", qt.QStyle__SP_MediaStop), "Stop", "Stop the running query", "", a.stop)

	// Ctrl+Enter on the keypad runs too.
	sc := qt.NewQShortcut2(qt.NewQKeySequence2("Ctrl+Enter"), a.win.QObject)
	sc.SetContext(qt.ApplicationShortcut)
	sc.OnActivated(a.run)
	// Ctrl+W closes the current tab.
	cw := qt.NewQShortcut2(qt.NewQKeySequence6(qt.QKeySequence__Close), a.win.QObject)
	cw.SetContext(qt.ApplicationShortcut)
	cw.OnActivated(func() {
		if i := a.tabs.CurrentIndex(); i >= 0 {
			a.closeTab(i)
		}
	})

	// The theme switch, at the right end.
	spacer := qt.NewQWidget2()
	spacer.SetSizePolicy2(qt.QSizePolicy__Expanding, qt.QSizePolicy__Preferred)
	tb.AddWidget(spacer)
	a.themeAct = add(themeIcon(false), "", "", "", func() {
		if a.theme == themeDark {
			a.setTheme(themeLight)
		} else {
			a.setTheme(themeDark)
		}
	})
	a.updateThemeAction()
}

// setTheme switches the application theme and remembers it.
func (a *App) setTheme(name string) {
	a.theme = name
	applyTheme(themeByName(name))
	a.updateThemeAction()
	a.saveState()
}

// updateThemeAction makes the theme switch offer the other theme.
func (a *App) updateThemeAction() {
	dark := a.theme == themeDark
	a.themeAct.SetIcon(themeIcon(dark))
	if dark {
		a.themeAct.SetText("Light\nTheme")
		a.themeAct.SetToolTip("Switch to the light theme")
	} else {
		a.themeAct.SetText("Dark\nTheme")
		a.themeAct.SetToolTip("Switch to the dark theme")
	}
}

func (a *App) buildStatusBar() {
	sb := a.win.StatusBar()
	a.status = qt.NewQLabel3("Ready.")
	a.status.SetTextInteractionFlags(qt.TextSelectableByMouse)
	sb.AddWidget2(a.status.QWidget, 1)

	nav := func(ic *qt.QIcon, tip string, f func()) *qt.QToolButton {
		b := qt.NewQToolButton2()
		b.SetIcon(ic)
		b.SetToolTip(tip)
		b.SetAutoRaise(true)
		b.OnClicked(f)
		sb.AddPermanentWidget(b.QWidget)
		return b
	}
	a.navFirst = nav(icon("go-first", qt.QStyle__SP_MediaSkipBackward), "First page", func() { a.goPage(0) })
	a.navPrev = nav(icon("go-previous", qt.QStyle__SP_MediaSeekBackward), "Previous page", func() { a.goPage(-2) })
	a.pageLabel = qt.NewQLabel2()
	a.pageLabel.SetMinimumWidth(170)
	a.pageLabel.SetAlignment(qt.AlignCenter)
	sb.AddPermanentWidget(a.pageLabel.QWidget)
	a.navNext = nav(icon("go-next", qt.QStyle__SP_MediaSeekForward), "Next page", func() { a.goPage(-3) })
	a.navLast = nav(icon("go-last", qt.QStyle__SP_MediaSkipForward), "Last page (fetches all rows)", func() { a.goPage(-1) })
	a.exportBtn = qt.NewQPushButton4(icon("document-export", qt.QStyle__SP_DialogSaveButton), "Export…")
	a.exportBtn.SetToolTip("Export the result to CSV or XLSX")
	a.exportBtn.OnClicked(a.export)
	sb.AddPermanentWidget(a.exportBtn.QWidget)
}

func (a *App) setStatus(msg string) { a.status.SetText(msg) }

// ---------------------------------------------------------------- connections

func (a *App) loadConnections() {
	conns, err := a.store.Connections()
	if err != nil {
		a.setStatus("Some connections could not be read: " + err.Error())
	}
	a.conns = conns
	a.tree.reload()
	for _, t := range a.sqlTabs {
		t.reloadConnections(t.conn)
		if a.source(t.conn) == nil {
			t.setConnection("", "")
		}
	}
	if a.source(a.lastConn) == nil {
		a.lastConn, a.lastDB = "", ""
	}
}

// source returns the data source of a connection (created on first use).
func (a *App) source(name string) *db.Source {
	if name == "" {
		return nil
	}
	if s, ok := a.sources[name]; ok {
		return s
	}
	for _, c := range a.conns {
		if c.Name == name {
			s, err := db.NewSource(c)
			if err != nil {
				return nil
			}
			a.sources[name] = s
			return s
		}
	}
	return nil
}

// dropSource closes the pools of a connection and forgets it.
func (a *App) dropSource(name string) {
	if s, ok := a.sources[name]; ok {
		delete(a.sources, name)
		go s.Close()
	}
}

func (a *App) newConnection() {
	c, ok := connectionDialog(a.win.QWidget, a.store, nil)
	if !ok {
		return
	}
	a.loadConnections()
	a.setLastUsed(c.Name, "")
	a.setStatus(fmt.Sprintf("Connection %q created.", c.Name))
	if item := a.tree.connectionItem(c.Name); item != nil {
		a.tree.widget.SetCurrentItem(item)
	}
}

func (a *App) connection(name string) *config.Connection {
	for i := range a.conns {
		if a.conns[i].Name == name {
			return &a.conns[i]
		}
	}
	return nil
}

func (a *App) editConnection(name string) {
	old := a.connection(name)
	if old == nil {
		return
	}
	existing := *old
	c, ok := connectionDialog(a.win.QWidget, a.store, &existing)
	if !ok {
		return
	}
	a.closeInfoTabs(name)
	a.dropSource(name)
	a.loadConnections()
	// Scripts bound to the renamed connection follow it.
	for _, t := range a.sqlTabs {
		if t.conn == name {
			t.reloadConnections(c.Name)
			t.setConnection(c.Name, t.database)
		}
	}
	if a.lastConn == name {
		a.setLastUsed(c.Name, a.lastDB)
	}
	a.setStatus(fmt.Sprintf("Connection %q saved.", c.Name))
}

func (a *App) deleteConnection(name string) {
	if qt.QMessageBox_Question(a.win.QWidget, "Delete Connection",
		fmt.Sprintf("Delete the connection %q?", name)) != qt.QMessageBox__Yes {
		return
	}
	if err := a.store.DeleteConnection(name); err != nil {
		qt.QMessageBox_Warning(a.win.QWidget, "Delete Connection", err.Error())
		return
	}
	a.closeInfoTabs(name)
	a.dropSource(name)
	a.loadConnections()
	a.setStatus(fmt.Sprintf("Connection %q deleted.", name))
}

// disconnect closes every connection to a source; it reconnects on demand.
func (a *App) disconnect(name string) {
	for _, t := range a.sqlTabs {
		if t.conn == name && !t.busy() {
			for _, v := range t.views {
				v.close()
			}
			go t.dropSession()
		}
	}
	a.dropSource(name)
	a.tree.collapseConnection(name)
	a.setStatus(fmt.Sprintf("Disconnected from %q.", name))
}

// setLastUsed records the connection (and database) the user worked with
// last; new scripts default to it.
func (a *App) setLastUsed(conn, database string) {
	if conn == "" || (conn == a.lastConn && database == a.lastDB) {
		return
	}
	if conn != a.lastConn && database == "" {
		a.lastDB = ""
	}
	a.lastConn = conn
	if database != "" {
		a.lastDB = database
	}
	a.saveState()
}

func (a *App) saveState() {
	a.store.SaveState(config.State{LastConnection: a.lastConn, LastDatabase: a.lastDB, Theme: a.theme})
}

// ---------------------------------------------------------------- tabs

func (a *App) currentSQL() *sqlTab {
	if w := a.tabs.CurrentWidget(); w != nil {
		return a.sqlTabs[key(w.UnsafePointer())]
	}
	return nil
}

func (a *App) currentInfo() *infoTab {
	if w := a.tabs.CurrentWidget(); w != nil {
		return a.infoTabs[key(w.UnsafePointer())]
	}
	return nil
}

// currentResult returns the result grid of the current tab, if any.
func (a *App) currentResult() *resultView {
	if t := a.currentSQL(); t != nil {
		return t.currentResult()
	}
	if t := a.currentInfo(); t != nil {
		return t.currentResult()
	}
	return nil
}

// newScript opens a SQL editor bound to the last used connection.
func (a *App) newScript(text, name string) *sqlTab {
	a.scripts++
	database := ""
	if a.lastConn != "" {
		database = a.lastDB
	}
	t := newSQLTab(a, a.scripts, a.lastConn, database)
	t.name = name
	t.editor.SetPlainText(text)
	t.editor.Document().SetModifiedWithBool(false)
	a.sqlTabs[key(t.widget.UnsafePointer())] = t
	idx := a.tabs.AddTab2(t.widget, icon("text-x-sql", qt.QStyle__SP_FileIcon), t.title())
	a.tabs.SetCurrentIndex(idx)
	t.editor.SetFocus()
	if t.conn == "" && len(a.conns) > 0 {
		a.setStatus("Choose a connection for this script.")
	}
	return t
}

func (a *App) updateTabTitle(t *sqlTab) {
	if idx := a.tabs.IndexOf(t.widget); idx >= 0 {
		a.tabs.SetTabText(idx, t.title())
	}
}

func (a *App) openScript() {
	name, ok := openQueryDialog(a.win.QWidget, a.store)
	if !ok {
		return
	}
	text, err := a.store.LoadQuery(name)
	if err != nil {
		qt.QMessageBox_Warning(a.win.QWidget, "Open Script", err.Error())
		return
	}
	a.newScript(text, name)
	a.setStatus(fmt.Sprintf("Opened %q.", name))
}

// saveScript saves the current script under its name, asking for one the
// first time. It returns false if the script was not saved.
func (a *App) saveScript() bool {
	t := a.currentSQL()
	if t == nil {
		return false
	}
	return a.saveTab(t)
}

func (a *App) saveTab(t *sqlTab) bool {
	name := t.name
	if name == "" {
		ok := false
		name = strings.TrimSpace(qt.QInputDialog_GetText4(a.win.QWidget, "Save Script", "Query name:",
			qt.QLineEdit__Normal, "", &ok))
		if !ok || name == "" {
			return false
		}
		if a.store.QueryExists(name) && qt.QMessageBox_Question(a.win.QWidget, "Save Script",
			fmt.Sprintf("A query named %q already exists. Replace it?", name)) != qt.QMessageBox__Yes {
			return false
		}
	}
	saved, err := a.store.SaveQuery(name, t.editor.ToPlainText())
	if err != nil {
		qt.QMessageBox_Warning(a.win.QWidget, "Save Script", err.Error())
		return false
	}
	t.name = saved
	t.editor.Document().SetModifiedWithBool(false)
	a.updateTabTitle(t)
	a.setStatus(fmt.Sprintf("Saved %q.", saved))
	return true
}

// openInfo shows the information panel of an object, reusing its tab if open.
func (a *App) openInfo(conn string, node db.Node) {
	for _, t := range a.infoTabs {
		if t.conn == conn && t.node.Key() == node.Key() {
			a.tabs.SetCurrentWidget(t.widget)
			return
		}
	}
	src := a.source(conn)
	if src == nil {
		return
	}
	t := newInfoTab(a, conn, src, node)
	a.infoTabs[key(t.widget.UnsafePointer())] = t
	idx := a.tabs.AddTab2(t.widget, nodeIcon(node.Kind), node.Name)
	a.tabs.SetTabToolTip(idx, db.KindLabel(node.Kind)+" "+node.Name+" · "+conn)
	a.tabs.SetCurrentIndex(idx)
}

// reloadInfoTabs reloads the info tabs of a connection showing node or an
// object below it.
func (a *App) reloadInfoTabs(conn string, node db.Node) {
	for _, t := range a.infoTabs {
		if t.conn == conn && node.Contains(t.node) {
			t.reload()
		}
	}
}

func (a *App) closeInfoTabs(conn string) {
	for i := a.tabs.Count() - 1; i >= 0; i-- {
		if t := a.infoTabs[key(a.tabs.Widget(i).UnsafePointer())]; t != nil && t.conn == conn {
			a.closeTab(i)
		}
	}
}

func (a *App) closeTab(i int) {
	w := a.tabs.Widget(i)
	if w == nil {
		return
	}
	k := key(w.UnsafePointer())
	if t := a.sqlTabs[k]; t != nil {
		if t.editor.Document().IsModified() && strings.TrimSpace(t.editor.ToPlainText()) != "" {
			a.tabs.SetCurrentIndex(i)
			switch qt.QMessageBox_Question6(a.win.QWidget, "Close Script",
				fmt.Sprintf("Save changes to %q before closing?", strings.TrimSuffix(t.title(), " •")),
				qt.QMessageBox__Save|qt.QMessageBox__Discard|qt.QMessageBox__Cancel, qt.QMessageBox__Save) {
			case qt.QMessageBox__Save:
				if !a.saveTab(t) {
					return
				}
			case qt.QMessageBox__Discard:
			default:
				return
			}
		}
		t.dispose()
		delete(a.sqlTabs, k)
	}
	if t := a.infoTabs[k]; t != nil {
		t.dispose()
		delete(a.infoTabs, k)
	}
	a.tabs.RemoveTab(a.tabs.IndexOf(w))
	w.DeleteLater()
	a.refreshChrome()
}

func (a *App) confirmQuit() bool {
	var unsaved []string
	for _, t := range a.sqlTabs {
		if t.editor.Document().IsModified() && strings.TrimSpace(t.editor.ToPlainText()) != "" {
			unsaved = append(unsaved, strings.TrimSuffix(t.title(), " •"))
		}
	}
	if len(unsaved) == 0 {
		return true
	}
	msg := fmt.Sprintf("%d script(s) have unsaved changes:\n\n%s\n\nQuit anyway?", len(unsaved), strings.Join(unsaved, "\n"))
	return qt.QMessageBox_Question6(a.win.QWidget, "Quit Dibi", msg,
		qt.QMessageBox__Yes|qt.QMessageBox__No, qt.QMessageBox__No) == qt.QMessageBox__Yes
}

// ---------------------------------------------------------------- actions

func (a *App) run() {
	if t := a.currentSQL(); t != nil {
		t.run()
	}
}

func (a *App) stop() {
	if t := a.currentSQL(); t != nil {
		t.stop()
	} else if v := a.currentResult(); v != nil {
		v.stop()
	}
}

// goPage navigates the current result: n >= 0 is a page, -1 the last page,
// -2 the previous and -3 the next one.
func (a *App) goPage(n int) {
	v := a.currentResult()
	if v == nil {
		return
	}
	switch n {
	case -2:
		n = v.page - 1
	case -3:
		n = v.page + 1
	}
	if n < -1 {
		return
	}
	v.goPage(n)
}

// refreshChrome updates the toolbar and status bar for the current tab.
func (a *App) refreshChrome() {
	if a.runAct == nil || a.pageLabel == nil {
		return // still building the window
	}
	t := a.currentSQL()
	a.runAct.SetEnabled(t != nil && !t.busy())
	a.saveAct.SetEnabled(t != nil)
	busy := t != nil && t.busy()
	v := a.currentResult()
	if v != nil && v.busy {
		busy = true
	}
	a.stopAct.SetEnabled(busy)

	pi := pageInfo{}
	if v != nil {
		pi = v.info()
	}
	a.navFirst.SetEnabled(pi.canPrev)
	a.navPrev.SetEnabled(pi.canPrev)
	a.navNext.SetEnabled(pi.canNext)
	a.navLast.SetEnabled(pi.canNext)
	a.exportBtn.SetEnabled(v != nil && v.exportQuery != "" && !pi.busy)
	switch {
	case !pi.exists:
		a.pageLabel.SetText("")
	case pi.first == 0:
		a.pageLabel.SetText("No rows")
	default:
		pages := "?"
		if pi.pages > 0 {
			pages = fmt.Sprint(pi.pages)
		}
		a.pageLabel.SetText(fmt.Sprintf("Page %d of %s  ·  rows %d–%d", pi.page, pages, pi.first, pi.last))
	}
}

// export re-runs the query of the current result and writes it to a file.
func (a *App) export() {
	v := a.currentResult()
	if v == nil || v.exportQuery == "" || v.src == nil {
		return
	}
	opts, ok := exportDialog(a.win.QWidget, v.exportNote)
	if !ok {
		return
	}
	a.runExport(v, opts, nil)
}

// runExport exports the query of a result in the background, with a
// cancellable progress dialog. done, if set, is called when it finishes.
func (a *App) runExport(v *resultView, opts export.Options, done func(n int, err error)) {
	src, database, query := v.src, v.database, v.exportQuery
	ctx, cancel := context.WithCancel(context.Background())
	progress := qt.NewQProgressDialog5("Exporting…", "Cancel", 0, 0, a.win.QWidget)
	progress.SetWindowTitle("Export")
	progress.SetWindowModality(qt.WindowModal)
	progress.SetMinimumDuration(300)
	progress.SetAutoClose(false)
	progress.SetAutoReset(false)
	progress.OnCanceled(cancel)
	start := time.Now()
	type result struct {
		n   int
		err error
	}
	async(func() result {
		pool, err := src.Pool(database)
		if err != nil {
			return result{err: err}
		}
		n, err := export.Run(ctx, pool, query, opts, func(n int) {
			onUI(func() { progress.SetLabelText(fmt.Sprintf("Exporting… %d rows written", n)) })
		})
		return result{n, err}
	}, func(r result) {
		cancel()
		progress.Hide()
		progress.DeleteLater()
		switch {
		case r.err != nil && ctx.Err() != nil:
			a.setStatus("Export cancelled.")
		case r.err != nil:
			a.setStatus("Export failed.")
			qt.QMessageBox_Warning(a.win.QWidget, "Export", "Export failed:\n\n"+r.err.Error())
		default:
			a.setStatus(fmt.Sprintf("Exported %d rows to %s in %.1f s.", r.n, opts.Path, time.Since(start).Seconds()))
		}
		if done != nil {
			done(r.n, r.err)
		}
	})
}
