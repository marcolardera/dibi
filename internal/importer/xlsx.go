package importer

import (
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
)

// xlsxReader reads a worksheet. Its values are read up front, as text:
// numbers in their raw form rather than as displayed, dates (numbers with
// a date format) as YYYY-MM-DD[ hh:mm:ss[.fff]] and booleans as
// true/false. Empty cells are NULL.
type xlsxReader struct {
	rows  [][]string
	row   int // index of the next row
	nulls []bool
}

func openXLSX(opts Options) (*xlsxReader, error) {
	f, err := excelize.OpenFile(opts.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sheets := f.GetSheetList()
	sheet := strings.TrimSpace(opts.SheetName)
	if sheet == "" {
		if len(sheets) == 0 {
			return nil, fmt.Errorf("%s has no worksheet", opts.Path)
		}
		sheet = sheets[0]
	} else if idx, _ := f.GetSheetIndex(sheet); idx < 0 {
		return nil, fmt.Errorf("there is no sheet %q; the sheets are: %s", sheet, strings.Join(sheets, ", "))
	}
	rows, err := f.GetRows(sheet, excelize.Options{RawCellValue: true})
	if err != nil {
		return nil, err
	}
	date1904 := false
	if props, err := f.GetWorkbookProps(); err == nil && props.Date1904 != nil {
		date1904 = *props.Date1904
	}
	styles := map[int]dateKind{}
	for r, row := range rows {
		for c, v := range row {
			if v == "" {
				continue
			}
			if cls, _, _ := classify(v); cls != clsInt && cls != clsDecimal && cls != clsFloat {
				continue
			}
			// A number: it may be a boolean, a date, or text that looks like one.
			cell, _ := excelize.CoordinatesToCellName(c+1, r+1)
			typ, err := f.GetCellType(sheet, cell)
			if err != nil {
				return nil, err
			}
			switch typ {
			case excelize.CellTypeBool:
				row[c] = strconv.FormatBool(v == "1")
				continue
			case excelize.CellTypeUnset, excelize.CellTypeNumber:
			default:
				continue
			}
			id, err := f.GetCellStyle(sheet, cell)
			if err != nil {
				return nil, err
			}
			kind, ok := styles[id]
			if !ok {
				kind = styleDateKind(f, id)
				styles[id] = kind
			}
			row[c] = numberText(v, kind, date1904)
		}
	}
	return &xlsxReader{rows: rows}, nil
}

// dateKind tells whether a number format shows dates or times.
type dateKind int

const (
	notDate dateKind = iota
	dateOnly
	dateTime
	timeOnly
)

func styleDateKind(f *excelize.File, id int) dateKind {
	style, err := f.GetStyle(id)
	if err != nil || style == nil {
		return notDate
	}
	if style.CustomNumFmt != nil {
		return formatDateKind(*style.CustomNumFmt)
	}
	switch n := style.NumFmt; {
	case n >= 14 && n <= 17, n >= 27 && n <= 31, n >= 34 && n <= 36, n >= 50 && n <= 58:
		return dateOnly
	case n == 22:
		return dateTime
	case n >= 18 && n <= 21, n >= 32 && n <= 33, n >= 45 && n <= 47:
		return timeOnly
	}
	return notDate
}

// formatDateKind inspects a custom number format code: dates use the
// y/m/d codes, times h/m/s (m is a month next to y or d).
func formatDateKind(code string) dateKind {
	code, _, _ = strings.Cut(code, ";") // the format of positive numbers
	var date, tm bool
	for i := 0; i < len(code); i++ {
		switch c := code[i]; c {
		case '"': // literal text
			if j := strings.IndexByte(code[i+1:], '"'); j >= 0 {
				i += j + 1
			} else {
				i = len(code)
			}
		case '\\', '_', '*': // escaped character, padding
			i++
		case '[': // color, locale, condition; [h], [mm], [ss] are elapsed times
			j := strings.IndexByte(code[i:], ']')
			if j < 0 {
				return notDate
			}
			if s := strings.ToLower(code[i+1 : i+j]); strings.Trim(s, "hms") == "" && s != "" {
				tm = true
			}
			i += j
		case 'y', 'Y', 'd', 'D':
			date = true
		case 'h', 'H', 's', 'S':
			tm = true
		case 'G', 'g':
			if strings.HasPrefix(strings.ToLower(code[i:]), "general") {
				i += len("general") - 1
			}
		}
	}
	switch {
	case date && tm:
		return dateTime
	case date:
		return dateOnly
	case tm:
		return timeOnly
	}
	return notDate
}

// numberText returns the text of a numeric cell: a date or time if its
// format shows one, otherwise the number, rounded to the 15 significant
// digits Excel keeps.
func numberText(raw string, kind dateKind, date1904 bool) string {
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return raw
	}
	if kind != notDate && f >= 0 {
		t, err := excelize.ExcelDateToTime(f, date1904)
		if err == nil {
			t = t.Round(time.Millisecond)
			switch {
			case kind == timeOnly && f < 1:
				return t.Format("15:04:05.999")
			case kind == dateOnly && t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 && t.Nanosecond() == 0:
				return t.Format(time.DateOnly)
			default:
				return t.Format("2006-01-02 15:04:05.999")
			}
		}
	}
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return raw
	}
	if _, ok := plainNumber(raw); ok && significantDigits(raw) <= 15 {
		return raw
	}
	return strconv.FormatFloat(f, 'g', 15, 64)
}

// significantDigits counts the digits of a plain number, without leading
// zeros.
func significantDigits(s string) int {
	s = strings.TrimLeft(strings.Replace(strings.TrimPrefix(s, "-"), ".", "", 1), "0")
	return len(s)
}

func (x *xlsxReader) next() ([]string, []bool, error) {
	if x.row >= len(x.rows) {
		return nil, nil, io.EOF
	}
	vals := x.rows[x.row]
	x.row++
	x.nulls = x.nulls[:0]
	empty := true
	for _, v := range vals {
		x.nulls = append(x.nulls, v == "")
		if v != "" {
			empty = false
		}
	}
	if empty {
		return nil, nil, nil
	}
	return vals, x.nulls, nil
}

func (x *xlsxReader) where() string { return fmt.Sprintf("row %d", x.row) }
func (x *xlsxReader) ragged() bool  { return true }
func (x *xlsxReader) rewind() error { x.row = 0; return nil }
func (x *xlsxReader) close() error  { return nil }
