package ui

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"

	qt "github.com/mappu/miqt/qt6"
)

func TestFindAll(t *testing.T) {
	tests := []struct {
		text, term string
		matchCase  bool
		want       [][2]int
	}{
		{"select * from t", "", false, nil},
		{"Select SELECT select", "select", false, [][2]int{{0, 6}, {7, 13}, {14, 20}}},
		{"Select SELECT select", "select", true, [][2]int{{14, 20}}},
		{"aaaa", "aa", false, [][2]int{{0, 2}, {2, 4}}},              // no overlaps
		{"😀 città CITTÀ", "città", false, [][2]int{{3, 8}, {9, 14}}}, // the emoji is 2 UTF-16 units
		{"a\nb a", "a", false, [][2]int{{0, 1}, {4, 5}}},
		{"abc", "abcd", false, nil},
	}
	for _, tt := range tests {
		if got := findAll(tt.text, tt.term, tt.matchCase); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("findAll(%q, %q, %v) = %v, want %v", tt.text, tt.term, tt.matchCase, got, tt.want)
		}
	}
}

func TestAutoPair(t *testing.T) {
	tests := []struct {
		line string // | marks the cursor
		ch   rune
		want pairAction
	}{
		{"|", '(', pairInsert},
		{"count|", '(', pairInsert},
		{"f(|)", '[', pairInsert},
		{"a |, b", '{', pairInsert},
		{"|abc", '(', pairNone}, // before a word
		{"f(|)", ')', pairSkip}, // over the closing one
		{"x[1|]", ']', pairSkip},
		{"x|", ')', pairNone},
		{"where a = |", '\'', pairInsert},
		{"'abc|'", '\'', pairSkip},
		{"N|", '\'', pairNone}, // after a word
		{"'it|", '\'', pairNone},
		{"''|", '\'', pairNone}, // after a quote
		{"select |", '"', pairInsert},
		{"\"col|\"", '"', pairSkip},
		{"select |", 'a', pairNone},
		{"😀 |", '(', pairInsert}, // the emoji is 2 UTF-16 units
	}
	for _, tt := range tests {
		before, after, _ := strings.Cut(tt.line, "|")
		line := utf16.Encode([]rune(before + after))
		col := len(utf16.Encode([]rune(before)))
		if got := autoPair(line, col, tt.ch); got != tt.want {
			t.Errorf("autoPair(%q, %q) = %v, want %v", tt.line, tt.ch, got, tt.want)
		}
	}
}

func TestAltGrText(t *testing.T) {
	ctrlAlt := qt.ControlModifier | qt.AltModifier
	tests := []struct {
		mods qt.KeyboardModifier
		text string
		want bool
	}{
		{ctrlAlt, "[", true}, // AltGr on Windows
		{ctrlAlt, "{", true},
		{qt.AltModifier, "[", true}, // Option on macOS
		{qt.ControlModifier, "[", false},
		{qt.ControlModifier, "\x1b", false},
		{ctrlAlt, "", false},
		{ctrlAlt, "\b", false},
	}
	for _, tt := range tests {
		if got := altGrText(tt.mods, tt.text); got != tt.want {
			t.Errorf("altGrText(%v, %q) = %v, want %v", tt.mods, tt.text, got, tt.want)
		}
	}
}

func TestEmptyPair(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{"|", false},
		{"(|)", true},
		{"f[|]", true},
		{"'|'", true},
		{"\"|\"", true},
		{"(|]", false},
		{"(|", false},
		{"|()", false},
		{"()|", false},
		{"(a|)", false},
	}
	for _, tt := range tests {
		before, after, _ := strings.Cut(tt.line, "|")
		line := utf16.Encode([]rune(before + after))
		if got := emptyPair(line, len(utf16.Encode([]rune(before)))); got != tt.want {
			t.Errorf("emptyPair(%q) = %v, want %v", tt.line, got, tt.want)
		}
	}
}

func TestToggleComments(t *testing.T) {
	tests := []struct{ in, want []string }{
		{[]string{"select 1"}, []string{"-- select 1"}},
		{[]string{"-- select 1"}, []string{"select 1"}},
		{[]string{"--select 1"}, []string{"select 1"}},
		{[]string{"--  select 1"}, []string{" select 1"}}, // one space only
		{[]string{""}, []string{""}},
		{[]string{"  "}, []string{"  "}},
		{ // aligned at the smallest indentation, blank lines untouched
			[]string{"select a,", "", "    b", "  from t"},
			[]string{"-- select a,", "", "--     b", "--   from t"},
		},
		{
			[]string{"  where a = 1", "    and b = 2"},
			[]string{"  -- where a = 1", "  --   and b = 2"},
		},
		{
			[]string{"  -- where a = 1", "", "  --   and b = 2"},
			[]string{"  where a = 1", "", "    and b = 2"},
		},
		{ // mixed: comment all, so toggling back restores the comment
			[]string{"-- some filter", "where a in (1,2,3)"},
			[]string{"-- -- some filter", "-- where a in (1,2,3)"},
		},
		{
			[]string{"-- -- some filter", "-- where a in (1,2,3)"},
			[]string{"-- some filter", "where a in (1,2,3)"},
		},
		{[]string{"\tselect 1"}, []string{"\t-- select 1"}},
	}
	for _, tt := range tests {
		if got := toggleComments(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("toggleComments(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// cursorLine splits a line with a | marking the cursor into its UTF-16
// units and the cursor's column.
func cursorLine(s string) ([]uint16, int) {
	before, after, _ := strings.Cut(s, "|")
	return utf16.Encode([]rune(before + after)), len(utf16.Encode([]rune(before)))
}

func TestNewlineAt(t *testing.T) {
	tests := []struct{ line, want string }{ // | marks the cursor
		{"|", "\n|"},
		{"select a|", "select a\n|"},
		{"    and b = 1|", "    and b = 1\n    |"},
		{"\tand b = 1|", "\tand b = 1\n\t|"},
		{"select a, |b", "select a,\n|b"}, // blanks around the cursor are dropped
		{"  select a  |  from t", "  select a\n  |from t"},
		{"select count(|", "select count(\n    |"},
		{"  insert into t (a, b) values (|", "  insert into t (a, b) values (\n      |"},
		{"select count(|)", "select count(\n    |\n)"},
		{"  where a in ( | )", "  where a in (\n      |\n  )"},
		{"select f(a|)", "select f(a\n|)"},
		{"    b|)", "    b\n|)"},
		{"      b|)", "      b\n    |)"},
		{"x = ')' |", "x = ')'\n|"},
		{"x = '(' |", "x = '('\n|"}, // brackets in strings, identifiers and comments do not count
		{"\"a(\" |", "\"a(\"\n|"},
		{"a -- (|", "a -- (\n|"},
		{"a /* ( */ b|", "a /* ( */ b\n|"},
		{"f(x) |", "f(x)\n|"},
		{"f(g(x) |", "f(g(x)\n    |"},
		{"    |select 1", "\n    |select 1"}, // in the indentation the line moves down
		{"  |  select 1", "\n  |  select 1"},
		{"    |", "\n    |"},
		{"😀 (|)", "😀 (\n    |\n)"}, // the emoji is 2 UTF-16 units
	}
	for _, tt := range tests {
		line, col := cursorLine(tt.line)
		ed := newlineAt(line, col)
		before, after := line[:col-ed.removeBefore], line[col+ed.removeAfter:]
		got := string(utf16.Decode(before)) + ed.before + "|" + ed.after + string(utf16.Decode(after))
		if got != tt.want {
			t.Errorf("newlineAt(%q) gives %q, want %q", tt.line, got, tt.want)
		}
	}
}

func TestOutdent(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"  ", ""},
		{"    ", ""},
		{"      ", "    "}, // back to the previous tab stop
		{"        ", "    "},
		{"\t", ""},
		{"\t\t", "\t"},
		{"\t  ", "\t"},
	}
	for _, tt := range tests {
		if got := outdent(tt.in); got != tt.want {
			t.Errorf("outdent(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestShiftLines(t *testing.T) {
	tests := []struct {
		in   []string
		out  bool
		want []string
	}{
		{[]string{"select a", "", "  from t"}, false, []string{"    select a", "", "      from t"}},
		{[]string{"\tselect 1"}, false, []string{"\t    select 1"}},
		{[]string{"    select a", "  ", "      from t"}, true, []string{"select a", "  ", "    from t"}},
		{[]string{"select a", "  from t", "\twhere"}, true, []string{"select a", "from t", "where"}},
	}
	for _, tt := range tests {
		if got := shiftLines(tt.in, tt.out); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("shiftLines(%q, %v) = %q, want %q", tt.in, tt.out, got, tt.want)
		}
	}
}

func TestTabSpaces(t *testing.T) {
	tests := []struct {
		line string
		want int
	}{
		{"|", 4},
		{"ab|", 2},
		{"abcd|", 4},
		{"\t|", 4},
		{"\ta|", 3},
		{"a\t|b", 4},
	}
	for _, tt := range tests {
		line, col := cursorLine(tt.line)
		if got := tabSpaces(line, col); got != tt.want {
			t.Errorf("tabSpaces(%q) = %d, want %d", tt.line, got, tt.want)
		}
	}
}

func TestIndentBackspace(t *testing.T) {
	tests := []struct {
		line string
		want int
	}{
		{"|", 0},
		{"    |", 4},
		{"      |select", 2},
		{"        |", 4},
		{" |", 1},
		{"\t|", 0}, // a tab is one character
		{"  \t|", 0},
		{"ab  |", 0}, // not in the indentation
	}
	for _, tt := range tests {
		line, col := cursorLine(tt.line)
		if got := indentBackspace(line, col); got != tt.want {
			t.Errorf("indentBackspace(%q) = %d, want %d", tt.line, got, tt.want)
		}
	}
}

func TestCloserIndent(t *testing.T) {
	tests := []struct {
		line   string
		ch     rune
		want   string
		wantOK bool
	}{
		{"        |", ')', "    ", true},
		{"    |", ']', "", true},
		{"\t\t|", '}', "\t", true},
		{"|", ')', "", false},
		{"    a|", ')', "", false},
		{"    |", 'a', "", false},
	}
	for _, tt := range tests {
		line, col := cursorLine(tt.line)
		if got, ok := closerIndent(line, col, tt.ch); got != tt.want || ok != tt.wantOK {
			t.Errorf("closerIndent(%q, %q) = %q, %v, want %q, %v", tt.line, tt.ch, got, ok, tt.want, tt.wantOK)
		}
	}
}
