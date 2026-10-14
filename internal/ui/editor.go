package ui

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"

	qt "github.com/mappu/miqt/qt6"
)

// codeEditor is the SQL editor: a plain text editor with syntax
// highlighting, a line number gutter and a find/replace bar.
type codeEditor struct {
	*qt.QPlainTextEdit
	highlighter *sqlHighlighter
	gutter      *qt.QWidget
	find        *findBar
}

func newCodeEditor() *codeEditor {
	e := &codeEditor{QPlainTextEdit: qt.NewQPlainTextEdit2()}
	e.SetFont(monoFont())
	e.SetLineWrapMode(qt.QPlainTextEdit__NoWrap)
	e.SetTabStopDistance(float64(4 * e.FontMetrics().HorizontalAdvance(" ")))
	e.highlighter = newSQLHighlighter(e.QPlainTextEdit)

	e.gutter = qt.NewQWidget(e.QWidget)
	e.gutter.OnPaintEvent(func(_ func(*qt.QPaintEvent), ev *qt.QPaintEvent) { e.paintGutter(ev) })
	e.OnBlockCountChanged(func(int) { e.updateGutterWidth() })
	e.OnUpdateRequest(func(rect *qt.QRect, dy int) {
		if dy != 0 {
			e.gutter.Scroll(0, dy)
		} else {
			e.gutter.Update()
		}
	})
	e.OnCursorPositionChanged(e.gutter.Update) // the current line number is emphasized
	e.updateGutterWidth()

	e.find = newFindBar(e)
	e.OnResizeEvent(func(super func(*qt.QResizeEvent), ev *qt.QResizeEvent) {
		super(ev)
		cr := e.ContentsRect()
		e.gutter.SetGeometry(cr.Left(), cr.Top(), e.gutterWidth(), cr.Height())
		e.find.place()
	})
	e.OnKeyPressEvent(e.keyPress)
	e.OnContextMenuEvent(func(_ func(*qt.QContextMenuEvent), ev *qt.QContextMenuEvent) {
		menu := e.CreateStandardContextMenuWithPosition(ev.Pos())
		menu.AddSeparator()
		act := menu.QWidget.AddActionWithText("Toggle Comment")
		act.SetShortcut(qt.NewQKeySequence2("Ctrl+K")) // shown as a hint
		act.OnTriggered(e.toggleComment)
		menu.ExecWithPos(ev.GlobalPos())
		menu.DeleteLater()
	})
	return e
}

// toggleComment comments the lines of the selection, or the current line,
// with "--", or uncomments them if they all are, in one undoable edit.
func (e *codeEditor) toggleComment() {
	e.editLines(toggleComments, func(col int, before, after string) int {
		// Keep the cursor on the same text, unless it was in the indentation.
		indent := utf16Len(before[:len(before)-len(strings.TrimLeft(before, " \t"))])
		if col > indent {
			col = max(col+utf16Len(after)-utf16Len(before), indent)
		}
		return col
	})
}

// shiftSelection indents the lines of the selection, or the current line,
// by one level, or outdents them, in one undoable edit.
func (e *codeEditor) shiftSelection(out bool) {
	e.editLines(func(lines []string) []string { return shiftLines(lines, out) }, func(col int, before, after string) int {
		return max(col+utf16Len(after)-utf16Len(before), 0) // the cursor moves with the text
	})
}

// editLines replaces the lines of the selection, or the current line, with
// transform(lines), in one undoable edit. A selection ending at the start
// of a line leaves it out. A selection afterwards covers the edited lines;
// otherwise cursorCol maps the cursor's UTF-16 column in the first line,
// before, to its column in the edited one, after.
func (e *codeEditor) editLines(transform func([]string) []string, cursorCol func(col int, before, after string) int) {
	cur := e.TextCursor()
	doc := e.Document()
	first, last := doc.FindBlock(cur.SelectionStart()), doc.FindBlock(cur.SelectionEnd())
	if cur.HasSelection() && last.BlockNumber() > first.BlockNumber() && cur.SelectionEnd() == last.Position() {
		last = last.Previous() // a selection ending at the start of a line leaves it out
	}
	var lines []string
	for b := first; b.IsValid() && b.BlockNumber() <= last.BlockNumber(); b = b.Next() {
		lines = append(lines, b.Text())
	}
	edited := transform(lines)
	if strings.Join(edited, "\n") == strings.Join(lines, "\n") {
		return
	}
	hadSelection, col := cur.HasSelection(), cur.PositionInBlock()
	start := first.Position()
	cur.SetPosition(start)
	cur.SetPosition2(last.Position()+last.Length()-1, qt.QTextCursor__KeepAnchor)
	cur.InsertText(strings.Join(edited, "\n"))
	if hadSelection {
		cur.SetPosition2(start, qt.QTextCursor__KeepAnchor)
	} else {
		cur.SetPosition(start + min(cursorCol(col, lines[0], edited[0]), utf16Len(edited[0])))
	}
	e.SetTextCursor(cur)
}

// toggleComments removes the "--" (and one space after it) from lines if
// every non-blank one starts with it; otherwise it adds "-- " to every
// non-blank line, at the smallest indentation so the lines stay aligned.
// Lines that already are comments get a second "--", so toggling twice
// gives back the original text.
func toggleComments(lines []string) []string {
	indent, commented := -1, true
	for _, l := range lines {
		trimmed := strings.TrimLeft(l, " \t")
		if trimmed == "" {
			continue
		}
		if n := len(l) - len(trimmed); indent < 0 || n < indent {
			indent = n
		}
		commented = commented && strings.HasPrefix(trimmed, "--")
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		trimmed := strings.TrimLeft(l, " \t")
		switch {
		case trimmed == "" || indent < 0:
			out[i] = l
		case commented:
			ws := l[:len(l)-len(trimmed)]
			trimmed = strings.TrimPrefix(trimmed, "--")
			out[i] = ws + strings.TrimPrefix(trimmed, " ")
		default:
			out[i] = l[:indent] + "-- " + l[indent:]
		}
	}
	return out
}

// keyPress closes brackets and quotes as they are typed: an opening
// character also inserts its closing one, typing a closing character over
// an equal one moves past it, Backspace between an empty pair deletes
// both, and an opening character typed over a selection wraps it.
// It also indents: Enter keeps the indentation (see newlineAt), a closing
// bracket typed in the indentation outdents, Tab inserts spaces or indents
// the selected lines, Shift+Tab outdents them, and Backspace in the
// indentation deletes back to the previous tab stop.
// Ctrl+K toggles comments; it is handled here because the text edit
// claims it (delete to end of line) before any shortcut sees it.
func (e *codeEditor) keyPress(super func(*qt.QKeyEvent), ev *qt.QKeyEvent) {
	key := qt.Key(ev.Key())
	if key == qt.Key_K && ev.Modifiers() == qt.ControlModifier {
		e.toggleComment()
		return
	}
	cur := e.TextCursor()
	if mods := ev.Modifiers() &^ (qt.ShiftModifier | qt.KeypadModifier); mods != 0 && !altGrText(mods, ev.Text()) {
		super(ev)
		return
	}
	switch key {
	case qt.Key_Return, qt.Key_Enter: // Shift+Enter too, which would insert a line separator
		e.newline(cur)
		return
	case qt.Key_Tab:
		if ev.Modifiers()&qt.ShiftModifier != 0 { // usually Backtab
			e.shiftSelection(true)
			return
		}
		doc := e.Document()
		if doc.FindBlock(cur.SelectionStart()).BlockNumber() != doc.FindBlock(cur.SelectionEnd()).BlockNumber() {
			e.shiftSelection(false)
			return
		}
		cur.BeginEditBlock()
		cur.RemoveSelectedText()
		line, col := utf16.Encode([]rune(cur.Block().Text())), cur.PositionInBlock()
		cur.InsertText(strings.Repeat(" ", tabSpaces(line, col)))
		cur.EndEditBlock()
		e.SetTextCursor(cur)
		return
	case qt.Key_Backtab:
		e.shiftSelection(true)
		return
	}
	if cur.HasSelection() {
		if text := []rune(ev.Text()); len(text) == 1 && closers[text[0]] != 0 {
			e.wrapSelection(cur, text[0])
			return
		}
		super(ev)
		return
	}
	line, col := utf16.Encode([]rune(cur.Block().Text())), cur.PositionInBlock()
	if key == qt.Key_Backspace {
		if emptyPair(line, col) {
			cur.MovePosition(qt.QTextCursor__PreviousCharacter)
			cur.MovePosition3(qt.QTextCursor__NextCharacter, qt.QTextCursor__KeepAnchor, 2)
			cur.RemoveSelectedText()
			return
		}
		if n := indentBackspace(line, col); n > 0 {
			cur.MovePosition3(qt.QTextCursor__PreviousCharacter, qt.QTextCursor__KeepAnchor, n)
			cur.RemoveSelectedText()
			return
		}
		super(ev)
		return
	}
	text := []rune(ev.Text())
	if len(text) != 1 {
		super(ev)
		return
	}
	switch autoPair(line, col, text[0]) {
	case pairSkip:
		cur.MovePosition(qt.QTextCursor__NextCharacter)
		e.SetTextCursor(cur)
	case pairInsert:
		cur.InsertText(string(text[0]) + string(closers[text[0]]))
		cur.MovePosition(qt.QTextCursor__PreviousCharacter)
		e.SetTextCursor(cur)
	default:
		if indent, ok := closerIndent(line, col, text[0]); ok {
			cur.MovePosition2(qt.QTextCursor__StartOfBlock, qt.QTextCursor__KeepAnchor)
			cur.InsertText(indent + string(text[0]))
			e.SetTextCursor(cur)
			return
		}
		super(ev)
	}
}

// altGrText reports whether a key pressed with the modifiers mods (other
// than Shift and Keypad) types text, which is the case when they include
// Alt and the key gives one printable character: Windows reports AltGr as
// Ctrl+Alt and macOS reports Option as Alt, and on many layouts, such as
// the Italian one, they type brackets, braces and other characters.
func altGrText(mods qt.KeyboardModifier, text string) bool {
	r := []rune(text)
	return mods&qt.AltModifier != 0 && len(r) == 1 && unicode.IsPrint(r[0])
}

// newline replaces the selection, if any, with a line break indented as
// newlineAt decides, in one undoable edit.
func (e *codeEditor) newline(cur *qt.QTextCursor) {
	cur.BeginEditBlock()
	cur.RemoveSelectedText()
	ed := newlineAt(utf16.Encode([]rune(cur.Block().Text())), cur.PositionInBlock())
	start := cur.Position() - ed.removeBefore
	cur.SetPosition(cur.Position() + ed.removeAfter)
	cur.SetPosition2(start, qt.QTextCursor__KeepAnchor)
	cur.InsertText(ed.before + ed.after)
	cur.EndEditBlock()
	cur.SetPosition(start + utf16Len(ed.before))
	e.SetTextCursor(cur)
}

// wrapSelection puts the selection between open and its closing
// character, in one undoable edit, and keeps the wrapped text selected.
func (e *codeEditor) wrapSelection(cur *qt.QTextCursor, open rune) {
	start, end := cur.SelectionStart(), cur.SelectionEnd()
	cur.BeginEditBlock()
	cur.SetPosition(end) // the end first, so start stays valid
	cur.InsertText(string(closers[open]))
	cur.SetPosition(start)
	cur.InsertText(string(open))
	cur.EndEditBlock()
	cur.SetPosition(start + 1)
	cur.SetPosition2(end+1, qt.QTextCursor__KeepAnchor)
	e.SetTextCursor(cur)
}

// closers maps each auto-closed character to its closing one.
var closers = map[rune]rune{'(': ')', '[': ']', '{': '}', '\'': '\'', '"': '"'}

type pairAction int

const (
	pairNone   pairAction = iota // type the character as usual
	pairSkip                     // move past the equal closing character
	pairInsert                   // insert the character and its closing one
)

// autoPair decides what typing ch does at UTF-16 column col of line.
// Pairs are inserted only before blanks, closing brackets and separators,
// so typing before a word does not leave a stray closing character; quotes
// are not paired after a word or a quote either, as in N'x' or a doubled
// quote inside a string.
func autoPair(line []uint16, col int, ch rune) pairAction {
	var prev, next rune
	if col > 0 {
		prev = rune(line[col-1])
	}
	if col < len(line) {
		next = rune(line[col])
	}
	if ch == next && strings.ContainsRune(")]}'\"", ch) {
		return pairSkip
	}
	closing, ok := closers[ch]
	if !ok || next != 0 && !unicode.IsSpace(next) && !strings.ContainsRune(")]},;", next) {
		return pairNone
	}
	if closing == ch && (prev == ch || prev == '_' || unicode.IsLetter(prev) || unicode.IsDigit(prev)) {
		return pairNone
	}
	return pairInsert
}

// emptyPair reports whether UTF-16 column col of line sits between an
// opening character and its closing one.
func emptyPair(line []uint16, col int) bool {
	if col == 0 || col >= len(line) {
		return false
	}
	closing, ok := closers[rune(line[col-1])]
	return ok && closing == rune(line[col])
}

// indentUnit is one level of indentation.
const indentUnit = "    "

// newlineEdit is what Enter does: the removeBefore and removeAfter UTF-16
// units around the cursor are replaced with before+after, and the cursor
// goes between the two.
type newlineEdit struct {
	removeBefore, removeAfter int
	before, after             string
}

// newlineAt decides what Enter does at UTF-16 column col of line. The new
// line keeps the indentation of the current one, one level deeper after a
// bracket left open and one level shallower when it starts with a closing
// bracket. Between an opening bracket and its closing one, the closing one
// goes on a line of its own below the indented cursor. Blanks around the
// cursor are dropped, so no line ends with them, except in the
// indentation: there Enter moves the line down as it is, leaving an empty
// line.
func newlineAt(line []uint16, col int) newlineEdit {
	head, tail := string(utf16.Decode(line[:col])), string(utf16.Decode(line[col:]))
	if strings.TrimLeft(head, " \t") == "" {
		return newlineEdit{removeBefore: col, before: "\n" + head}
	}
	// Blanks are ASCII, so their bytes are UTF-16 units too.
	trimmedHead, trimmedTail := strings.TrimRight(head, " \t"), strings.TrimLeft(tail, " \t")
	ed := newlineEdit{removeBefore: len(head) - len(trimmedHead), removeAfter: len(tail) - len(trimmedTail)}
	indent := head[:len(head)-len(strings.TrimLeft(head, " \t"))]
	open := unclosedBracket(trimmedHead)
	closing := trimmedTail != "" && strings.ContainsRune(")]}", rune(trimmedTail[0]))
	switch {
	case open && closing && closers[rune(trimmedHead[len(trimmedHead)-1])] == rune(trimmedTail[0]):
		ed.before, ed.after = "\n"+indent+indentUnit, "\n"+indent
	case open && !closing:
		ed.before = "\n" + indent + indentUnit
	case closing && !open:
		ed.before = "\n" + outdent(indent)
	default:
		ed.before = "\n" + indent
	}
	return ed
}

// unclosedBracket reports whether s has an opening bracket that it does
// not close, ignoring the brackets in strings, quoted identifiers and
// comments.
func unclosedBracket(s string) bool {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth = max(depth-1, 0)
		case '\'', '"', '`':
			end := strings.IndexByte(s[i+1:], c)
			if end < 0 {
				return depth > 0
			}
			i += end + 1
		case '-':
			if strings.HasPrefix(s[i:], "--") {
				return depth > 0
			}
		case '/':
			if strings.HasPrefix(s[i:], "/*") {
				end := strings.Index(s[i+2:], "*/")
				if end < 0 {
					return depth > 0
				}
				i += end + 3
			}
		}
	}
	return depth > 0
}

// outdent removes one level from the end of the indentation indent: a
// tab, or the spaces back to the previous tab stop.
func outdent(indent string) string {
	if strings.HasSuffix(indent, "\t") {
		return indent[:len(indent)-1]
	}
	spaces := len(indent) - len(strings.TrimRight(indent, " "))
	n := spaces % len(indentUnit)
	if n == 0 {
		n = min(spaces, len(indentUnit))
	}
	return indent[:len(indent)-n]
}

// shiftLines indents lines by one level, or outdents them (see outdent).
// Blank lines are left alone.
func shiftLines(lines []string, out bool) []string {
	res := make([]string, len(lines))
	for i, l := range lines {
		body := strings.TrimLeft(l, " \t")
		indent := l[:len(l)-len(body)]
		switch {
		case body == "":
			res[i] = l
		case out:
			res[i] = outdent(indent) + body
		default:
			res[i] = indent + indentUnit + body
		}
	}
	return res
}

// tabSpaces returns how many spaces Tab inserts at UTF-16 column col of
// line: as many as reach the next tab stop.
func tabSpaces(line []uint16, col int) int {
	width := 0
	for _, c := range line[:col] {
		if c == '\t' {
			width += len(indentUnit) - width%len(indentUnit)
		} else {
			width++
		}
	}
	return len(indentUnit) - width%len(indentUnit)
}

// indentBackspace returns how many spaces Backspace deletes at UTF-16
// column col of line when only spaces precede it: back to the previous tab
// stop. It returns 0 elsewhere, where Backspace deletes a character.
func indentBackspace(line []uint16, col int) int {
	if col == 0 || strings.Trim(string(utf16.Decode(line[:col])), " ") != "" {
		return 0
	}
	if n := col % len(indentUnit); n > 0 {
		return n
	}
	return len(indentUnit)
}

// closerIndent returns the indentation that goes before a closing bracket
// ch typed at UTF-16 column col of line, if only blanks precede it: one
// level less, which lines it up with the line of the opening bracket.
func closerIndent(line []uint16, col int, ch rune) (string, bool) {
	head := string(utf16.Decode(line[:col]))
	if !strings.ContainsRune(")]}", ch) || head == "" || strings.TrimLeft(head, " \t") != "" {
		return "", false
	}
	return outdent(head), true
}

// gutterWidth fits the largest line number, with at least 3 digits.
func (e *codeEditor) gutterWidth() int {
	digits := max(len(strconv.Itoa(e.BlockCount())), 3)
	return 12 + digits*e.FontMetrics().HorizontalAdvance("9")
}

func (e *codeEditor) updateGutterWidth() {
	e.SetViewportMargins(e.gutterWidth(), 0, 0, 0)
	cr := e.ContentsRect()
	e.gutter.SetGeometry(cr.Left(), cr.Top(), e.gutterWidth(), cr.Height())
}

func (e *codeEditor) paintGutter(ev *qt.QPaintEvent) {
	p := qt.NewQPainter2(e.gutter.QPaintDevice)
	defer p.End()
	area := ev.Rect()
	p.FillRect5(area.Left(), area.Top(), area.Width(), area.Height(), color(th.gutter))
	p.SetFont(e.Font())
	width, lineHeight := e.gutter.Width()-6, e.FontMetrics().Height()
	current := e.TextCursor().BlockNumber()
	normal, emphasis := color(th.lineNumber), color(th.currentLine)

	block := e.FirstVisibleBlock()
	offset := e.ContentOffset()
	geo := e.BlockBoundingGeometry(&block)
	top := geo.TranslatedWithQPointF(&offset).Top()
	for block.IsValid() && int(top) <= area.Bottom() {
		rect := e.BlockBoundingRect(&block)
		height := rect.Height()
		if block.IsVisible() && int(top+height) >= area.Top() {
			if block.BlockNumber() == current {
				p.SetPen(emphasis)
			} else {
				p.SetPen(normal)
			}
			p.DrawText7(0, int(top), width, lineHeight, int(qt.AlignRight|qt.AlignVCenter), strconv.Itoa(block.BlockNumber()+1))
		}
		top += height
		block = *block.Next()
	}
}

// findBar is the find/replace box shown over the top-right corner of the
// editor. Matching is plain text (no regular expressions), optionally case
// sensitive, and wraps around the document.
type findBar struct {
	e          *codeEditor
	frame      *qt.QFrame
	toggle     *qt.QToolButton
	findEdit   *qt.QLineEdit
	count      *qt.QLabel
	matchCase  *qt.QToolButton
	replaceRow *qt.QWidget
	replace    *qt.QLineEdit
	escape     *qt.QShortcut

	matches  [][2]int // UTF-16 [start, end) positions in the document
	replaced int      // matches replaced by the last Replace All, shown until the next change
	busy     bool     // replacing: ignore document changes
}

func newFindBar(e *codeEditor) *findBar {
	b := &findBar{e: e, frame: qt.NewQFrame(e.QWidget)}
	onThemeChange(b.frame.QWidget, b.showCount)
	b.frame.SetFrameShape(qt.QFrame__StyledPanel)
	b.frame.SetAutoFillBackground(true)
	b.frame.SetCursor(qt.NewQCursor2(qt.ArrowCursor))

	toolButton := func(ic *qt.QIcon, text, tip string) *qt.QToolButton {
		btn := qt.NewQToolButton2()
		if ic != nil {
			btn.SetIcon(ic)
		}
		btn.SetText(text)
		btn.SetToolTip(tip)
		btn.SetAutoRaise(true)
		return btn
	}
	b.toggle = toolButton(nil, "", "Toggle Replace")
	b.toggle.SetArrowType(qt.RightArrow)
	b.findEdit = qt.NewQLineEdit2()
	b.findEdit.SetPlaceholderText("Find")
	b.findEdit.SetMinimumWidth(220)
	b.count = qt.NewQLabel2()
	b.count.SetMinimumWidth(b.count.FontMetrics().HorizontalAdvance("999 of 999"))
	prev := toolButton(icon("go-up", qt.QStyle__SP_ArrowUp), "", "Previous match (Shift+Enter, Shift+F3)")
	next := toolButton(icon("go-down", qt.QStyle__SP_ArrowDown), "", "Next match (Enter, F3)")
	b.matchCase = toolButton(nil, "Aa", "Match case")
	b.matchCase.SetCheckable(true)
	closeBtn := toolButton(icon("window-close", qt.QStyle__SP_TitleBarCloseButton), "", "Close (Esc)")

	b.replace = qt.NewQLineEdit2()
	b.replace.SetPlaceholderText("Replace")
	replaceBtn := qt.NewQPushButton3("Replace")
	replaceAllBtn := qt.NewQPushButton3("Replace All")

	grid := qt.NewQGridLayout2()
	grid.SetContentsMargins(4, 4, 4, 4)
	grid.SetHorizontalSpacing(2)
	grid.SetVerticalSpacing(4)
	grid.AddWidget2(b.toggle.QWidget, 0, 0)
	grid.AddWidget2(b.findEdit.QWidget, 0, 1)
	findTools := qt.NewQWidget2()
	findTools.SetLayout(hbox(b.count.QWidget, prev.QWidget, next.QWidget, b.matchCase.QWidget, closeBtn.QWidget).QLayout)
	grid.AddWidget2(findTools, 0, 2)
	grid.AddWidget2(b.replace.QWidget, 1, 1)
	b.replaceRow = qt.NewQWidget2()
	b.replaceRow.SetLayout(hbox(replaceBtn.QWidget, replaceAllBtn.QWidget).QLayout)
	grid.AddWidget2(b.replaceRow, 1, 2)
	b.frame.SetLayout(grid.QLayout)
	b.showReplace(false)
	b.frame.Hide()

	b.toggle.OnClicked(func() { b.showReplace(!b.replace.IsVisible()) })
	b.findEdit.OnTextChanged(func(string) { b.update(true) })
	onEnter(b.findEdit, func(shift bool) { b.step(!shift) })
	b.matchCase.OnToggled(func(bool) { b.update(true) })
	prev.OnClicked(func() { b.step(false) })
	next.OnClicked(func() { b.step(true) })
	closeBtn.OnClicked(b.close)
	onEnter(b.replace, func(bool) { b.replaceOne() })
	// Keys the fields do not use (QLineEdit ignores Enter, Up, Down, …)
	// propagate to the parent, which is the editor: keep them in the bar,
	// or Enter would replace the selected match with a newline.
	b.frame.OnKeyPressEvent(func(_ func(*qt.QKeyEvent), ev *qt.QKeyEvent) { ev.Accept() })
	replaceBtn.OnClicked(b.replaceOne)
	replaceAllBtn.OnClicked(b.replaceAll)
	e.Document().OnContentsChanged(func() {
		if b.frame.IsVisible() && !b.busy {
			b.replaced = 0
			b.update(false)
		}
	})

	// Shortcuts work in the editor and in the bar (a child of the editor).
	shortcut := func(seq *qt.QKeySequence, f func()) *qt.QShortcut {
		sc := qt.NewQShortcut2(seq, e.QObject)
		sc.SetContext(qt.WidgetWithChildrenShortcut)
		sc.OnActivated(f)
		return sc
	}
	shortcut(qt.NewQKeySequence6(qt.QKeySequence__Find), func() { b.open(false) })
	shortcut(qt.NewQKeySequence6(qt.QKeySequence__Replace), func() { b.open(true) })
	shortcut(qt.NewQKeySequence6(qt.QKeySequence__FindNext), func() { b.stepOrOpen(true) })
	shortcut(qt.NewQKeySequence6(qt.QKeySequence__FindPrevious), func() { b.stepOrOpen(false) })
	b.escape = shortcut(qt.NewQKeySequence2("Esc"), b.close)
	b.escape.SetEnabled(false)
	return b
}

// onEnter handles Enter in a line edit, reporting whether Shift is held.
func onEnter(edit *qt.QLineEdit, f func(shift bool)) {
	edit.OnKeyPressEvent(func(super func(*qt.QKeyEvent), ev *qt.QKeyEvent) {
		if k := qt.Key(ev.Key()); k == qt.Key_Return || k == qt.Key_Enter {
			ev.Accept()
			f(ev.Modifiers()&qt.ShiftModifier != 0)
			return
		}
		super(ev)
	})
}

func (b *findBar) showReplace(on bool) {
	b.replace.SetVisible(on)
	b.replaceRow.SetVisible(on)
	if on {
		b.toggle.SetArrowType(qt.DownArrow)
	} else {
		b.toggle.SetArrowType(qt.RightArrow)
	}
	b.place()
}

// place moves the bar to the top-right corner of the text area.
func (b *findBar) place() {
	b.frame.AdjustSize()
	vp := b.e.Viewport().Geometry()
	b.frame.Move(max(vp.X(), vp.X()+vp.Width()-b.frame.Width()-4), vp.Y()+2)
}

// open shows the bar, searching for the selected text if it fits on a line.
func (b *findBar) open(withReplace bool) {
	cur := b.e.TextCursor()
	if sel := cur.SelectedText(); sel != "" && !strings.ContainsRune(sel, ' ') {
		b.findEdit.SetText(sel) // the selection is a match, so it stays selected
	}
	b.showReplace(withReplace)
	b.frame.Show()
	b.frame.Raise()
	b.escape.SetEnabled(true)
	b.update(false)
	if withReplace && b.findEdit.Text() != "" {
		b.replace.SetFocus()
		b.replace.SelectAll()
	} else {
		b.findEdit.SetFocus()
		b.findEdit.SelectAll()
	}
}

func (b *findBar) close() {
	b.frame.Hide()
	b.escape.SetEnabled(false)
	b.e.highlighter.setSearch("", false)
	b.e.SetFocus()
}

// update recomputes the matches. With jump, it also selects the first
// match at or after the start of the current selection (find as you type).
func (b *findBar) update(jump bool) {
	text, matchCase := b.findEdit.Text(), b.matchCase.IsChecked()
	b.e.highlighter.setSearch(text, matchCase)
	b.matches = findAll(b.e.ToPlainText(), text, matchCase)
	if jump && len(b.matches) > 0 {
		b.selectMatch(b.nextFrom(b.e.TextCursor().SelectionStart()))
	}
	b.showCount()
}

func (b *findBar) showCount() {
	setTextColor(b.count, "")
	switch {
	case b.replaced > 0:
		b.count.SetText(fmt.Sprintf("%d replaced", b.replaced))
	case b.findEdit.Text() == "":
		b.count.SetText("")
	case len(b.matches) == 0:
		setTextColor(b.count, th.errText)
		b.count.SetText("No results")
	default:
		if i := b.current(); i >= 0 {
			b.count.SetText(fmt.Sprintf("%d of %d", i+1, len(b.matches)))
		} else {
			b.count.SetText(fmt.Sprintf("%d found", len(b.matches)))
		}
	}
	b.place()
}

// current returns the index of the match that is selected, or -1.
func (b *findBar) current() int {
	cur := b.e.TextCursor()
	for i, m := range b.matches {
		if m[0] == cur.SelectionStart() && m[1] == cur.SelectionEnd() {
			return i
		}
	}
	return -1
}

// nextFrom returns the first match starting at or after pos, wrapping.
func (b *findBar) nextFrom(pos int) int {
	for i, m := range b.matches {
		if m[0] >= pos {
			return i
		}
	}
	return 0
}

func (b *findBar) selectMatch(i int) {
	cur := b.e.TextCursor()
	cur.SetPosition(b.matches[i][0])
	cur.SetPosition2(b.matches[i][1], qt.QTextCursor__KeepAnchor)
	b.e.SetTextCursor(cur)
	b.e.CenterCursor()
}

// step selects the next or previous match, wrapping around.
func (b *findBar) step(forward bool) {
	if len(b.matches) == 0 {
		return
	}
	cur := b.e.TextCursor()
	i := b.nextFrom(cur.SelectionEnd())
	if !forward {
		i = len(b.matches) - 1
		for j := len(b.matches) - 1; j >= 0; j-- {
			if b.matches[j][0] < cur.SelectionStart() {
				i = j
				break
			}
		}
	}
	b.selectMatch(i)
	b.showCount()
}

// stepOrOpen serves F3/Shift+F3, which open the bar when nothing is searched.
func (b *findBar) stepOrOpen(forward bool) {
	if b.findEdit.Text() == "" {
		b.open(false)
		return
	}
	if !b.frame.IsVisible() {
		b.frame.Show()
		b.escape.SetEnabled(true)
		b.update(false)
	}
	b.step(forward)
}

// replaceOne replaces the selected match, if any, and selects the next one.
func (b *findBar) replaceOne() {
	if i := b.current(); i >= 0 {
		b.e.TextCursor().InsertText(b.replace.Text())
	}
	b.step(true)
}

// replaceAll replaces every match in one undoable edit.
func (b *findBar) replaceAll() {
	if len(b.matches) == 0 {
		return
	}
	n := len(b.matches)
	cur := b.e.TextCursor()
	b.busy = true
	cur.BeginEditBlock()
	for i := n - 1; i >= 0; i-- { // backwards, so earlier positions stay valid
		cur.SetPosition(b.matches[i][0])
		cur.SetPosition2(b.matches[i][1], qt.QTextCursor__KeepAnchor)
		cur.InsertText(b.replace.Text())
	}
	cur.EndEditBlock()
	b.busy = false
	b.e.SetTextCursor(cur)
	b.update(false)
	b.replaced = n
	b.showCount()
}

// findAll returns the non-overlapping occurrences of term in text as
// [start, end) positions in UTF-16 code units, the unit of Qt positions.
func findAll(text, term string, matchCase bool) [][2]int {
	if term == "" {
		return nil
	}
	hay, needle := []rune(text), []rune(term)
	same := func(a, b rune) bool {
		return a == b || !matchCase && (unicode.ToLower(a) == unicode.ToLower(b) || unicode.ToUpper(a) == unicode.ToUpper(b))
	}
	var out [][2]int
	pos := 0 // UTF-16 position of hay[i]
	for i := 0; i+len(needle) <= len(hay); {
		j := 0
		for j < len(needle) && same(hay[i+j], needle[j]) {
			j++
		}
		if j == len(needle) {
			start := pos
			for _, r := range hay[i : i+j] {
				pos += utf16.RuneLen(r)
			}
			out = append(out, [2]int{start, pos})
			i += j
			continue
		}
		pos += utf16.RuneLen(hay[i])
		i++
	}
	return out
}
