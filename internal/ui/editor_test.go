package ui

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"
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
