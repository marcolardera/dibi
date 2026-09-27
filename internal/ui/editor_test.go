package ui

import (
	"reflect"
	"testing"
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
		{"aaaa", "aa", false, [][2]int{{0, 2}, {2, 4}}}, // no overlaps
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
