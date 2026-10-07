package ui

import (
	"fmt"
	"sort"
	"strings"

	"dibi/internal/db"
	"dibi/internal/pager"
	"dibi/internal/value"

	qt "github.com/mappu/miqt/qt6"
)

const (
	maxColumnWidth = 400
	maxTooltip     = 4000
)

// resultModel exposes one page of rows to a QTableView.
type resultModel struct {
	*qt.QAbstractTableModel
	cols   []value.Column
	rows   [][]any
	offset int // absolute index of the first row, for row numbers
}

func newResultModel() *resultModel {
	m := &resultModel{QAbstractTableModel: qt.NewQAbstractTableModel()}
	right := int(qt.AlignRight | qt.AlignVCenter)

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
		r, c := idx.Row(), idx.Column()
		if !idx.IsValid() || r >= len(m.rows) || c >= len(m.cols) {
			return qt.NewQVariant()
		}
		v := m.rows[r][c]
		col := m.cols[c]
		switch qt.ItemDataRole(role) {
		case qt.DisplayRole:
			text, _ := col.Display(v)
			return qt.NewQVariant14(text)
		case qt.ForegroundRole:
			if v == nil {
				return color(th.muted).ToQVariant()
			}
		case qt.TextAlignmentRole:
			if col.Numeric() {
				return qt.NewQVariant4(right)
			}
		case qt.ToolTipRole:
			// Show the full value when the cell is truncated.
			if v != nil {
				full := col.Text(v)
				if disp, _ := col.Display(v); disp != full && !strings.HasPrefix(disp, "<binary") {
					return qt.NewQVariant14(firstRunes(full, maxTooltip))
				}
			}
		}
		return qt.NewQVariant()
	})
	m.OnHeaderData(func(super func(int, qt.Orientation, int) *qt.QVariant, section int, o qt.Orientation, role int) *qt.QVariant {
		if qt.ItemDataRole(role) == qt.DisplayRole {
			if o == qt.Horizontal && section < len(m.cols) {
				return qt.NewQVariant14(m.cols[section].Name)
			}
			if o == qt.Vertical {
				return qt.NewQVariant14(fmt.Sprint(m.offset + section + 1))
			}
		}
		if qt.ItemDataRole(role) == qt.ToolTipRole && o == qt.Horizontal && section < len(m.cols) {
			return qt.NewQVariant14(m.cols[section].Name + " (" + m.cols[section].Type + ")")
		}
		return super(section, o, role)
	})
	return m
}

func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func (m *resultModel) set(cols []value.Column, rows [][]any, offset int) {
	m.BeginResetModel()
	m.cols, m.rows, m.offset = cols, rows, offset
	m.EndResetModel()
}

// resultView is a grid showing a result set, one page at a time.
type resultView struct {
	app   *App
	table *qt.QTableView
	model *resultModel
	cols  []value.Column

	pager   *pager.Pager
	page    int
	partial bool // the cursor was closed after the first page
	busy    bool // a page is being fetched

	// For export: the query is re-run on a fresh connection of the source.
	src         *db.Source
	database    string
	exportQuery string
	exportNote  string
}

func newResultView(app *App) *resultView {
	v := &resultView{app: app, model: newResultModel(), table: qt.NewQTableView2()}
	t := v.table
	t.SetModel(v.model.QAbstractItemModel)
	t.SetAlternatingRowColors(true)
	t.SetWordWrap(false)
	t.SetHorizontalScrollMode(qt.QAbstractItemView__ScrollPerPixel)
	t.SetEditTriggers(qt.QAbstractItemView__NoEditTriggers)
	t.SetSelectionBehavior(qt.QAbstractItemView__SelectItems)
	t.SetSelectionMode(qt.QAbstractItemView__ExtendedSelection)
	t.VerticalHeader().SetDefaultSectionSize(t.FontMetrics().Height() + 6)
	t.VerticalHeader().SetSectionResizeMode(qt.QHeaderView__Fixed)
	t.SetContextMenuPolicy(qt.ActionsContextMenu)

	copyAct := qt.NewQAction5("Copy", t.QObject)
	copyAct.SetShortcut(qt.NewQKeySequence6(qt.QKeySequence__Copy))
	copyAct.SetShortcutContext(qt.WidgetWithChildrenShortcut)
	copyAct.OnTriggered(func() { v.copySelection(false) })
	t.AddAction(copyAct)
	copyHdr := qt.NewQAction5("Copy with Column Names", t.QObject)
	copyHdr.OnTriggered(func() { v.copySelection(true) })
	t.AddAction(copyHdr)
	selectAll := qt.NewQAction5("Select All", t.QObject)
	selectAll.SetShortcut(qt.NewQKeySequence6(qt.QKeySequence__SelectAll))
	selectAll.SetShortcutContext(qt.WidgetWithChildrenShortcut)
	selectAll.OnTriggered(func() { t.SelectAll() })
	t.AddAction(selectAll)
	return v
}

func (v *resultView) widget() *qt.QWidget { return v.table.QWidget }

// show displays the first page of a pager. If partial is true the cursor
// has already been closed and only this page is available.
func (v *resultView) show(p *pager.Pager, first [][]any, partial bool) {
	v.pager, v.page, v.partial = p, 0, partial
	v.cols = make([]value.Column, len(p.Cols))
	for i, c := range p.Cols {
		v.cols[i] = value.NewColumn(c.Name(), c.DatabaseTypeName())
	}
	v.model.set(v.cols, first, 0)
	v.fitColumns()
}

func (v *resultView) fitColumns() {
	v.table.ResizeColumnsToContents()
	h := v.table.HorizontalHeader()
	for i := range v.cols {
		if h.SectionSize(i) > maxColumnWidth {
			h.ResizeSection(i, maxColumnWidth)
		}
	}
}

// close releases the cursor (in the background: for some drivers closing
// a partially read cursor has to talk to the server).
func (v *resultView) close() {
	if p := v.pager; p != nil {
		v.pager = nil
		go p.Close()
	}
}

// pageInfo describes the pagination state for the status bar.
type pageInfo struct {
	page, pages  int // pages is 0 if unknown
	first, last  int // row range shown (1-based), 0 if empty
	canPrev      bool
	canNext      bool
	busy, exists bool
}

func (v *resultView) info() pageInfo {
	if v.pager == nil {
		return pageInfo{}
	}
	pi := pageInfo{exists: true, page: v.page + 1, busy: v.busy}
	if v.partial {
		pi.pages = 1
	} else if v.pager.Done() {
		pi.pages = max(v.pager.Pages(), 1)
	}
	if n := len(v.model.rows); n > 0 {
		pi.first = v.model.offset + 1
		pi.last = v.model.offset + n
	}
	pi.canPrev = v.page > 0 && !v.busy
	pi.canNext = !v.busy && (pi.pages == 0 || v.page+1 < pi.pages)
	return pi
}

// goPage shows page n (0-based); n < 0 means the last page. Rows are
// fetched in the background.
func (v *resultView) goPage(n int) {
	if v.pager == nil || v.busy || (v.partial && n != 0) {
		return
	}
	p := v.pager
	v.busy = true
	if n < 0 && !p.Done() {
		v.app.setStatus("Fetching all rows…")
	}
	v.app.refreshChrome()
	type result struct {
		n    int
		rows [][]any
		err  error
	}
	async(func() result {
		var err error
		if n < 0 {
			if n, err = p.Last(); err != nil {
				return result{err: err}
			}
		}
		rows, err := p.Page(n)
		return result{n, rows, err}
	}, func(r result) {
		v.busy = false
		if v.pager != p { // closed or replaced meanwhile
			return
		}
		switch {
		case r.err != nil:
			v.app.setStatus("Fetch failed: " + r.err.Error())
		case r.rows != nil || r.n == 0:
			v.page = r.n
			v.model.set(v.cols, r.rows, r.n*pager.PageSize)
			v.table.ScrollToTop()
			v.app.setStatus(fmt.Sprintf("Page %d.", r.n+1))
		}
		v.app.refreshChrome()
	})
}

// stop interrupts a page fetch in progress.
func (v *resultView) stop() {
	if v.busy && v.pager != nil {
		v.pager.Cancel()
	}
}

// copySelection copies the selected cells as tab-separated text with the
// full (untruncated) values.
func (v *resultView) copySelection(withHeader bool) {
	idxs := v.table.SelectionModel().SelectedIndexes()
	if len(idxs) == 0 {
		return
	}
	rowSet, colSet := map[int]bool{}, map[int]bool{}
	sel := map[[2]int]bool{}
	for _, i := range idxs {
		r, c := i.Row(), i.Column()
		rowSet[r], colSet[c] = true, true
		sel[[2]int{r, c}] = true
	}
	rows, cols := sortedKeys(rowSet), sortedKeys(colSet)
	var b strings.Builder
	if withHeader {
		for i, c := range cols {
			if i > 0 {
				b.WriteByte('\t')
			}
			b.WriteString(v.cols[c].Name)
		}
		b.WriteByte('\n')
	}
	for ri, r := range rows {
		for i, c := range cols {
			if i > 0 {
				b.WriteByte('\t')
			}
			if sel[[2]int{r, c}] {
				b.WriteString(v.cols[c].Text(v.model.rows[r][c]))
			}
		}
		if ri < len(rows)-1 {
			b.WriteByte('\n')
		}
	}
	qt.QGuiApplication_Clipboard().SetText(b.String())
	v.app.setStatus(fmt.Sprintf("Copied %d cell(s).", len(idxs)))
}

func sortedKeys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}
