package ui

import (
	"strings"

	qt "github.com/mappu/miqt/qt6"
)

// sqlKeywords are highlighted in the editor (ANSI SQL plus common
// procedural and T-SQL keywords).
var sqlKeywords = []string{
	"add", "all", "alter", "and", "any", "as", "asc", "begin", "between", "by", "call", "cascade",
	"case", "cast", "check", "column", "commit", "constraint", "create", "cross", "current_date",
	"current_time", "current_timestamp", "database", "declare", "default", "delete", "desc", "distinct",
	"drop", "else", "end", "except", "exec", "execute", "exists", "false", "fetch", "first", "for",
	"foreign", "from", "full", "function", "grant", "group", "having", "if", "in", "index", "inner",
	"insert", "intersect", "into", "is", "join", "key", "left", "like", "limit", "loop", "merge",
	"natural", "not", "null", "nulls", "offset", "on", "only", "or", "order", "outer", "over",
	"partition", "primary", "procedure", "references", "replace", "return", "returns", "revoke",
	"right", "rollback", "row", "rows", "savepoint", "schema", "select", "sequence", "set", "table",
	"then", "to", "transaction", "trigger", "true", "truncate", "union", "unique", "update", "using",
	"values", "view", "when", "where", "while", "window", "with",
	// T-SQL
	"catch", "clustered", "go", "identity", "nocount", "nonclustered", "output", "print", "raiserror",
	"throw", "top", "tran", "try", "waitfor",
}

// highlightRule formats the matches of re (format points into the
// highlighter's formats, which change with the theme).
type highlightRule struct {
	re     *qt.QRegularExpression
	format **qt.QTextCharFormat
}

// highlightFormats are the text formats of a theme.
type highlightFormats struct {
	keyword, number, quoted, str, comment *qt.QTextCharFormat
	match                                 *qt.QBrush
}

func newHighlightFormats(t *theme) highlightFormats {
	return highlightFormats{
		keyword: charFormat(t.keyword, true, false),
		number:  charFormat(t.number, false, false),
		quoted:  charFormat(t.quoted, false, false),
		str:     charFormat(t.str, false, false),
		comment: charFormat(t.comment, false, true),
		match:   qt.NewQBrush3(color(t.match)),
	}
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

// sqlHighlighter highlights SQL syntax and the matches of the find bar.
type sqlHighlighter struct {
	*qt.QSyntaxHighlighter
	search    string // text to mark, "" for none
	matchCase bool
	formats   highlightFormats
}

// setSearch changes the text marked as a search match.
func (h *sqlHighlighter) setSearch(text string, matchCase bool) {
	if text == h.search && matchCase == h.matchCase {
		return
	}
	h.search, h.matchCase = text, matchCase
	h.Rehighlight()
}

// newSQLHighlighter highlights keywords, numbers, strings and comments in
// the editor, following the theme.
// Rules are applied in order, so later ones (strings, comments) win over
// keywords found inside them.
func newSQLHighlighter(edit *qt.QPlainTextEdit) *sqlHighlighter {
	h := &sqlHighlighter{QSyntaxHighlighter: qt.NewQSyntaxHighlighter2(edit.Document()), formats: newHighlightFormats(th)}
	onThemeChange(edit.QWidget, func() {
		h.formats = newHighlightFormats(th)
		h.Rehighlight()
	})
	ci := qt.QRegularExpression__CaseInsensitiveOption
	rules := []highlightRule{
		{qt.NewQRegularExpression4(`\b(`+strings.Join(sqlKeywords, "|")+`)\b`, ci), &h.formats.keyword},
		{qt.NewQRegularExpression2(`\b\d+(\.\d+)?([eE][+-]?\d+)?\b`), &h.formats.number},
		{qt.NewQRegularExpression2(`"([^"]|"")*"`), &h.formats.quoted},
		{qt.NewQRegularExpression2(`'([^'\\]|''|\\.)*'`), &h.formats.str},
		{qt.NewQRegularExpression2(`--[^\n]*`), &h.formats.comment},
	}
	open := qt.NewQRegularExpression2(`/\*`)
	closeRe := qt.NewQRegularExpression2(`\*/`)

	h.OnHighlightBlock(func(text string) {
		comment := h.formats.comment
		for _, r := range rules {
			it := r.re.GlobalMatch(text)
			for it.HasNext() {
				m := it.Next()
				h.SetFormat(int(m.CapturedStart()), int(m.CapturedLength()), *r.format)
			}
		}
		// Multi-line /* */ comments; block state 1 means "inside a comment".
		// Positions come from Qt regular expressions, so they are already
		// in UTF-16 units as SetFormat expects.
		h.SetCurrentBlockState(0)
		textLen := utf16Len(text)
		start := 0
		if h.PreviousBlockState() != 1 {
			start = int(open.Match(text).CapturedStart())
		}
		for start >= 0 {
			m := closeRe.Match2(text, int64(start))
			end := int(m.CapturedStart())
			if end < 0 {
				h.SetCurrentBlockState(1)
				h.SetFormat(start, textLen-start, comment)
				break
			}
			h.SetFormat(start, end+2-start, comment)
			start = int(open.Match2(text, int64(end+2)).CapturedStart())
		}
		// Search matches get a background, keeping their syntax colors.
		for _, m := range findAll(text, h.search, h.matchCase) {
			for i := m[0]; i < m[1]; i++ {
				f := h.Format(i)
				f.SetBackground(h.formats.match)
				h.SetFormat(i, 1, &f)
			}
		}
	})
	return h
}
