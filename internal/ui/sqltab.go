package ui

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"dibi/internal/db"
	"dibi/internal/pager"
	"dibi/internal/sqlsplit"

	"github.com/jackc/pgx/v5/pgconn"
	qt "github.com/mappu/miqt/qt6"
	mssql "github.com/microsoft/go-mssqldb"
)

// sqlTab is a SQL editor with its results. It owns a database session,
// opened on first run and kept until the tab is closed or its connection
// changes.
type sqlTab struct {
	app    *App
	widget *qt.QWidget

	connCombo *qt.QComboBox
	dbLabel   *qt.QLabel
	dbCombo   *qt.QComboBox
	autoBox   *qt.QCheckBox
	editor    *codeEditor
	results   *qt.QTabWidget
	messages  *qt.QPlainTextEdit
	views     []*resultView

	number   int    // for the "Script N" title
	name     string // saved query name, "" if never saved
	conn     string // connection name, "" if none
	database string // database for the session ("" = default)

	autocommit bool // wanted for the session
	running    bool
	switching  bool // turning autocommit on in the background
	cancel     context.CancelFunc

	// Accessed from the run goroutine.
	mu      sync.Mutex
	session *db.Session
	sessSrc *db.Source
	active  *pager.Pager // pager currently fetching its first page
}

func newSQLTab(app *App, number int, conn, database string) *sqlTab {
	t := &sqlTab{app: app, number: number, widget: qt.NewQWidget2(), autocommit: true}
	layout := qt.NewQVBoxLayout(t.widget)
	layout.SetContentsMargins(4, 4, 4, 0)

	// Connection bar.
	t.connCombo = qt.NewQComboBox2()
	t.connCombo.SetMinimumContentsLength(18)
	t.connCombo.SetSizeAdjustPolicy(qt.QComboBox__AdjustToContents)
	t.dbLabel = qt.NewQLabel3("Database:")
	t.dbCombo = qt.NewQComboBox2()
	t.dbCombo.SetMinimumContentsLength(14)
	t.dbCombo.SetSizeAdjustPolicy(qt.QComboBox__AdjustToContents)
	t.autoBox = qt.NewQCheckBox3("Autocommit")
	t.autoBox.SetChecked(true)
	t.autoBox.SetToolTip("When off, statements run in a transaction that lasts until\n" +
		"you run COMMIT or ROLLBACK. Turning it back on commits it.")
	bar := hbox(qt.NewQLabel3("Connection:").QWidget, t.connCombo.QWidget, t.dbLabel.QWidget, t.dbCombo.QWidget,
		t.autoBox.QWidget)
	bar.AddStretch()
	layout.AddLayout(bar.QLayout)

	// Editor and results.
	split := qt.NewQSplitter3(qt.Vertical)
	t.editor = newCodeEditor()
	t.editor.OnModificationChanged(func(bool) { app.updateTabTitle(t) })

	t.results = qt.NewQTabWidget2()
	t.results.SetDocumentMode(true)
	t.results.OnCurrentChanged(func(int) { app.refreshChrome() })
	t.messages = qt.NewQPlainTextEdit2()
	t.messages.SetReadOnly(true)
	t.messages.SetFont(monoFont())
	t.results.AddTab(t.messages.QWidget, "Messages")

	split.AddWidget(t.editor.QWidget)
	split.AddWidget(t.results.QWidget)
	split.SetStretchFactor(0, 2)
	split.SetStretchFactor(1, 3)
	layout.AddWidget(split.QWidget)

	t.reloadConnections(conn)
	t.setConnection(conn, database)
	t.connCombo.OnActivated(func(int) {
		name := t.connCombo.CurrentData().ToString()
		if name != t.conn {
			t.setConnection(name, "")
			app.setLastUsed(name, t.database)
		}
	})
	t.dbCombo.OnActivated(func(int) {
		t.database = t.dbCombo.CurrentText()
		app.setLastUsed(t.conn, t.database)
	})
	t.autoBox.OnToggled(t.setAutocommit)
	return t
}

// title returns the tab title.
func (t *sqlTab) title() string {
	title := t.name
	if title == "" {
		title = fmt.Sprintf("Script %d", t.number)
	}
	if t.editor.Document().IsModified() {
		title += " •"
	}
	return title
}

// reloadConnections refills the connection selector, keeping the selection.
func (t *sqlTab) reloadConnections(selected string) {
	t.connCombo.Clear()
	t.connCombo.AddItem3("(no connection)", qt.NewQVariant14(""))
	for _, c := range t.app.conns {
		t.connCombo.AddItem4(connIcon(c.Type), c.Name, qt.NewQVariant14(c.Name))
	}
	idx := t.connCombo.FindData(qt.NewQVariant14(selected))
	t.connCombo.SetCurrentIndex(max(idx, 0))
}

// setConnection binds the tab to a connection and database. The session,
// if any, is replaced on the next run.
func (t *sqlTab) setConnection(conn, database string) {
	if t.app.source(conn) == nil {
		conn, database = "", ""
	}
	t.conn, t.database = conn, database
	t.connCombo.SetCurrentIndex(max(t.connCombo.FindData(qt.NewQVariant14(conn)), 0))
	t.dbCombo.Clear()
	src := t.app.source(conn)
	t.autoBox.SetVisible(src != nil && src.ManualCommit())
	switchable := src != nil && src.SwitchableDatabases()
	t.dbLabel.SetVisible(switchable)
	t.dbCombo.SetVisible(switchable)
	if !switchable {
		t.database = ""
		return
	}
	if database != "" {
		t.dbCombo.AddItem(database)
	}
	// Load the list of databases in the background.
	type result struct {
		names []string
		def   string
	}
	async(func() result {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		names, _ := src.Databases(ctx)
		def := database
		if def == "" {
			def = src.DefaultDatabase(ctx)
		}
		return result{names, def}
	}, func(r result) {
		if t.conn != conn { // changed meanwhile
			return
		}
		if t.database == "" {
			t.database = r.def
		}
		t.dbCombo.Clear()
		names := r.names
		if t.database != "" && !contains(names, t.database) {
			names = append([]string{t.database}, names...)
		}
		t.dbCombo.AddItems(names)
		t.dbCombo.SetCurrentIndex(max(t.dbCombo.FindText(t.database), 0))
	})
}

// setAutocommit records the autocommit mode for the session. Turning it on
// commits the open transaction right away; otherwise the mode is applied
// on the next run.
func (t *sqlTab) setAutocommit(on bool) {
	t.autocommit = on
	t.mu.Lock()
	sess := t.session
	t.mu.Unlock()
	if !on || sess == nil || sess.Autocommit() || t.busy() {
		return
	}
	// The session must have no open cursor: stop paging the results that
	// were not read to the end.
	for _, v := range t.views {
		if v.pager != nil && !v.pager.Done() {
			v.close()
		}
	}
	t.switching = true
	t.app.setStatus("Committing…")
	t.app.refreshChrome()
	async(func() error {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		return sess.SetAutocommit(ctx, true)
	}, func(err error) {
		t.switching = false
		if err != nil {
			appendError(t.messages, "Cannot turn autocommit on: ", err.Error())
			t.results.SetCurrentWidget(t.messages.QWidget)
			t.app.setStatus("Commit failed.")
			t.autocommit = false
			t.autoBox.BlockSignals(true)
			t.autoBox.SetChecked(false)
			t.autoBox.BlockSignals(false)
		} else {
			t.messages.AppendPlainText("Autocommit on: the open transaction was committed.")
			t.app.setStatus("Committed.")
		}
		t.app.refreshChrome()
	})
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// sqlToRun returns the selected text, or the whole script, and the UTF-16
// offset of that text in the editor.
func (t *sqlTab) sqlToRun() (string, int) {
	cur := t.editor.TextCursor()
	if cur.HasSelection() {
		// Qt uses U+2029 as the paragraph separator in selections.
		return strings.ReplaceAll(cur.SelectedText(), " ", "\n"), cur.SelectionStart()
	}
	return t.editor.ToPlainText(), 0
}

// currentResult returns the result grid shown in the results area, if any.
func (t *sqlTab) currentResult() *resultView {
	w := t.results.CurrentWidget()
	if w == nil {
		return nil
	}
	for _, v := range t.views {
		if v.widget().UnsafePointer() == w.UnsafePointer() {
			return v
		}
	}
	return nil
}

func (t *sqlTab) clearResults() {
	for _, v := range t.views {
		v.close()
		t.results.RemoveTab(t.results.IndexOf(v.widget()))
		v.widget().DeleteLater()
	}
	t.views = nil
	t.messages.Clear()
}

// run executes the selection (or the whole script), statement by statement.
func (t *sqlTab) run() {
	if t.busy() {
		return
	}
	src := t.app.source(t.conn)
	if src == nil {
		t.app.setStatus("Choose a connection for this script first.")
		t.connCombo.SetFocus()
		t.connCombo.ShowPopup()
		return
	}
	text, base := t.sqlToRun()
	stmts := sqlsplit.Split(text, src.SplitDialect())
	if len(stmts) == 0 {
		t.app.setStatus("Nothing to run.")
		return
	}

	t.clearResults()
	t.results.SetCurrentWidget(t.messages.QWidget)
	ctx, cancel := context.WithCancel(context.Background())
	t.running, t.cancel = true, cancel
	t.app.setLastUsed(t.conn, t.database)
	t.app.setStatus("Running…")
	t.app.refreshChrome()

	database, autocommit := t.database, t.autocommit
	go t.execute(ctx, src, database, autocommit, text, base, stmts)
}

// execute runs in a goroutine. UI updates go through onUIWait so that
// results appear in order while the script is running.
func (t *sqlTab) execute(ctx context.Context, src *db.Source, database string, autocommit bool,
	text string, base int, stmts []sqlsplit.Statement) {
	start := time.Now()
	ok := 0
	var failed error
	defer func() {
		elapsed := time.Since(start)
		cancelled := ctx.Err() != nil // before the cleanup below cancels ctx
		onUIWait(func() {
			t.running = false
			if t.cancel != nil {
				t.cancel()
				t.cancel = nil
			}
			switch {
			case failed != nil && cancelled:
				t.app.setStatus(fmt.Sprintf("Query cancelled after %.3f s.", elapsed.Seconds()))
			case failed != nil:
				t.app.setStatus(fmt.Sprintf("Query failed after %.3f s.", elapsed.Seconds()))
			case len(stmts) == 1:
				t.app.setStatus(fmt.Sprintf("Query completed in %.3f s.", elapsed.Seconds()))
			default:
				t.app.setStatus(fmt.Sprintf("%d statements completed in %.3f s.", ok, elapsed.Seconds()))
			}
			t.app.refreshChrome()
		})
	}()

	sess, err := t.ensureSession(ctx, src, database)
	if err != nil {
		failed = err
		onUIWait(func() { appendError(t.messages, "Cannot connect: ", err.Error()) })
		return
	}
	if src.ManualCommit() {
		if err := sess.SetAutocommit(ctx, autocommit); err != nil {
			failed = err
			onUIWait(func() { appendError(t.messages, "Cannot set autocommit: ", err.Error()) })
			return
		}
	}

	for i, st := range stmts {
		label := ""
		if len(stmts) > 1 {
			label = fmt.Sprintf("[%d] ", i+1)
		}
		t0 := time.Now()
		last := i == len(stmts)-1
		if sqlsplit.ReturnsRows(st.Text, src.SplitDialect()) {
			failed = t.runQuery(ctx, sess, src, database, st, label, last, t0)
		} else {
			failed = t.runExec(ctx, sess, st, label, t0)
		}
		if failed != nil {
			stmt, errMsg := st, failed.Error()
			if ctx.Err() != nil {
				errMsg = "cancelled (" + errMsg + ")"
			}
			onUIWait(func() {
				appendError(t.messages, label+"Error: ", errMsg)
				t.messages.AppendPlainText("    " + firstLine(stmt.Text, 120))
				t.results.SetCurrentWidget(t.messages.QWidget)
				t.showErrorPosition(failed, text, base, stmt)
			})
			if !sess.Alive(context.Background()) {
				t.dropSession()
			}
			return
		}
		ok++
	}
}

// runQuery opens a cursor and shows the first page of each of its result
// sets in a new result tab. A result set is only reachable once the
// previous one has been fully read, so the sets after a partial one are not
// shown. Except for the last statement, the cursor is closed after the
// first page (the session can only have one open cursor).
func (t *sqlTab) runQuery(ctx context.Context, sess *db.Session, src *db.Source, database string,
	st sqlsplit.Statement, label string, last bool, t0 time.Time) error {
	p, err := pager.New(ctx, sess, st.Text)
	if err != nil {
		return err
	}
	for set := 1; p != nil; set++ {
		t.mu.Lock()
		t.active = p
		t.mu.Unlock()
		rows, err := p.Page(0)
		t.mu.Lock()
		t.active = nil
		t.mu.Unlock()
		if err != nil {
			p.Close()
			return err
		}
		var next *pager.Pager
		if p.Done() {
			if next, err = p.NextResult(); err != nil {
				p.Close()
				return err
			}
		}
		partial := false
		if !last {
			partial = !p.Done()
			p.Close() // no-op once the cursor is handed to next
		}
		elapsed := time.Since(t0)

		if len(p.Cols) == 0 { // e.g. a data-modifying WITH statement
			p.Close()
			if set == 1 && next == nil {
				onUIWait(func() {
					t.messages.AppendPlainText(fmt.Sprintf("%sStatement executed (%.3f s).", label, elapsed.Seconds()))
				})
			}
			p = next
			continue
		}
		setLabel := label
		if set > 1 || next != nil {
			setLabel = fmt.Sprintf("%sResult set %d: ", label, set)
		}
		exportQuery := ""
		if set == 1 { // re-running the statement only reaches the first result set
			exportQuery = st.Text
		}
		cur := p
		onUIWait(func() {
			v := newResultView(t.app)
			v.src, v.database, v.exportQuery = src, database, exportQuery
			v.show(cur, rows, partial)
			t.views = append(t.views, v)
			title := fmt.Sprintf("Result %d", len(t.views))
			idx := t.results.InsertTab(len(t.views)-1, v.widget(), title)
			t.results.SetTabToolTip(idx, firstLine(st.Text, 200))
			if set == 1 {
				t.results.SetCurrentIndex(idx)
			}

			more := ""
			switch {
			case partial:
				more = fmt.Sprintf(" (first %d shown; run the statement alone to page through all rows)", pager.PageSize)
			case !cur.Done():
				more = "+"
			}
			t.messages.AppendPlainText(fmt.Sprintf("%s%d row(s)%s fetched in %.3f s.", setLabel, len(rows), more, elapsed.Seconds()))
		})
		p = next
	}
	return nil
}

func (t *sqlTab) runExec(ctx context.Context, sess *db.Session, st sqlsplit.Statement, label string, t0 time.Time) error {
	res, err := sess.ExecContext(ctx, st.Text)
	if err != nil {
		return err
	}
	msg := "Statement executed"
	if n, err := res.RowsAffected(); err == nil && n >= 0 {
		msg = fmt.Sprintf("%d row(s) affected", n)
	}
	elapsed := time.Since(t0)
	onUIWait(func() {
		t.messages.AppendPlainText(fmt.Sprintf("%s%s (%.3f s).", label, msg, elapsed.Seconds()))
	})
	return nil
}

// ensureSession returns the tab session, opening a new one if there is
// none, if it is bound to another source or database, or if it died.
func (t *sqlTab) ensureSession(ctx context.Context, src *db.Source, database string) (*db.Session, error) {
	t.mu.Lock()
	sess, sessSrc := t.session, t.sessSrc
	t.mu.Unlock()
	if sess != nil && sessSrc == src && sess.Database == database && sess.Alive(ctx) {
		return sess, nil
	}
	t.dropSession()
	sess, err := src.NewSession(ctx, database)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.session, t.sessSrc = sess, src
	t.mu.Unlock()
	return sess, nil
}

func (t *sqlTab) dropSession() {
	t.mu.Lock()
	sess := t.session
	t.session, t.sessSrc = nil, nil
	t.mu.Unlock()
	if sess != nil {
		sess.Close()
	}
}

// stop cancels the running statement or page fetch.
func (t *sqlTab) stop() {
	if t.running && t.cancel != nil {
		t.cancel()
		t.mu.Lock()
		if t.active != nil {
			t.active.Cancel()
		}
		t.mu.Unlock()
		t.app.setStatus("Cancelling…")
	}
	for _, v := range t.views {
		v.stop()
	}
}

func (t *sqlTab) busyPaging() bool {
	for _, v := range t.views {
		if v.busy {
			return true
		}
	}
	return false
}

func (t *sqlTab) busy() bool { return t.running || t.switching || t.busyPaging() }

// dispose releases everything; called when the tab is closed.
func (t *sqlTab) dispose() {
	t.stop()
	for _, v := range t.views {
		v.close()
	}
	go t.dropSession()
}

var (
	oraclePosition = regexp.MustCompile(`error occur at position: (\d+)`)
	mssqlNear      = regexp.MustCompile(`near '([^']+)'`)
)

// showErrorPosition moves the editor cursor to the position of the error
// in the failed statement, when the database reports one.
func (t *sqlTab) showErrorPosition(err error, text string, base int, st sqlsplit.Statement) {
	pos := -1 // 0-based character offset in the statement
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Position > 0 {
		pos = int(pgErr.Position) - 1
	} else if m := oraclePosition.FindStringSubmatch(err.Error()); m != nil {
		pos, _ = strconv.Atoi(m[1])
	} else if msErr := (mssql.Error{}); errors.As(err, &msErr) && msErr.ProcName == "" {
		pos = mssqlErrorPosition(st.Text, int(msErr.LineNo), msErr.Message)
	}
	if pos < 0 {
		return
	}
	stmtRunes := []rune(st.Text)
	if pos > len(stmtRunes) {
		return
	}
	offset := base + utf16Len(text[:st.Start]) + utf16Len(string(stmtRunes[:pos]))
	cur := t.editor.TextCursor()
	cur.SetPosition(offset)
	t.editor.SetTextCursor(cur)
}

// mssqlErrorPosition returns the character offset in a batch of a SQL
// Server error at the given 1-based line: at the token named by "near 'x'"
// when it is on that line, else at the start of the line; -1 if unknown.
func mssqlErrorPosition(batch string, line int, msg string) int {
	if line < 1 {
		return -1
	}
	lines := strings.SplitAfter(batch, "\n")
	if line > len(lines) {
		return -1
	}
	start := 0
	for _, l := range lines[:line-1] {
		start += utf8.RuneCountInString(l)
	}
	if m := mssqlNear.FindStringSubmatch(msg); m != nil {
		if i := strings.Index(strings.ToLower(lines[line-1]), strings.ToLower(m[1])); i >= 0 {
			return start + utf8.RuneCountInString(lines[line-1][:i])
		}
	}
	return start
}
