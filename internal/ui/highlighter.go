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

// sqlHighlighter highlights SQL syntax and the matches of the find bar.
type sqlHighlighter struct {
	*qt.QSyntaxHighlighter
	search    string // text to mark, "" for none
	matchCase bool
}

// setSearch changes the text marked as a search match.
func (h *sqlHighlighter) setSearch(text string, matchCase bool) {
	if text == h.search && matchCase == h.matchCase {
		return
	}
	h.search, h.matchCase = text, matchCase
	h.Rehighlight()
}

// newSQLHighlighter highlights keywords, numbers, strings and comments.
// Rules are applied in order, so later ones (strings, comments) win over
// keywords found inside them.
func newSQLHighlighter(doc *qt.QTextDocument) *sqlHighlighter {
	h := &sqlHighlighter{QSyntaxHighlighter: qt.NewQSyntaxHighlighter2(doc)}
	matchBg := qt.NewQBrush3(qt.NewQColor6("#ffe58a"))
	ci := qt.QRegularExpression__CaseInsensitiveOption
	rules := []highlightRule{
		{qt.NewQRegularExpression4(`\b(`+strings.Join(sqlKeywords, "|")+`)\b`, ci), charFormat("#0033b3", true, false)},
		{qt.NewQRegularExpression2(`\b\d+(\.\d+)?([eE][+-]?\d+)?\b`), charFormat("#1750eb", false, false)},
		{qt.NewQRegularExpression2(`"([^"]|"")*"`), charFormat("#871094", false, false)},
		{qt.NewQRegularExpression2(`'([^'\\]|''|\\.)*'`), charFormat("#067d17", false, false)},
		{qt.NewQRegularExpression2(`--[^\n]*`), charFormat("#8c8c8c", false, true)},
	}
	comment := charFormat("#8c8c8c", false, true)
	open := qt.NewQRegularExpression2(`/\*`)
	closeRe := qt.NewQRegularExpression2(`\*/`)

	h.OnHighlightBlock(func(text string) {
		for _, r := range rules {
			it := r.re.GlobalMatch(text)
			for it.HasNext() {
				m := it.Next()
				h.SetFormat(int(m.CapturedStart()), int(m.CapturedLength()), r.format)
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
				f.SetBackground(matchBg)
				h.SetFormat(i, 1, &f)
			}
		}
	})
	return h
}
