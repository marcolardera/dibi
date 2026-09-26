// Package export writes the result of a query to a CSV or XLSX file.
//
// The query is re-run and its rows are streamed to the file, so the whole
// result is exported, not just the pages shown in the grid.
package export

import (
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"dibi/internal/value"

	"github.com/xuri/excelize/v2"
)

// Format is an export file format.
type Format string

const (
	CSV  Format = "csv"
	XLSX Format = "xlsx"
)

// MaxXLSXRows is the number of data rows that fit in a worksheet (the
// Excel limit of 1,048,576 rows, minus the header).
const MaxXLSXRows = 1048575

// Options configures an export.
type Options struct {
	Format Format
	Path   string
	Header bool // write column names as the first row

	// CSV only.
	Separator rune   // ',' by default
	NullText  string // text written for NULL values
	CRLF      bool   // Windows line endings

	// XLSX only.
	SheetName string // "Sheet1" by default
}

// Queryer is implemented by *sql.DB, *sql.Conn and db.Session.
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Run executes query and writes every row to opts.Path. progress, if not
// nil, is called periodically with the number of rows written. The file is
// written atomically: on error or cancellation no partial file is left.
func Run(ctx context.Context, q Queryer, query string, opts Options, progress func(int)) (int, error) {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	cts, err := rows.ColumnTypes()
	if err != nil {
		return 0, err
	}
	cols := make([]value.Column, len(cts))
	for i, c := range cts {
		cols[i] = value.NewColumn(c.Name(), c.DatabaseTypeName())
	}

	tmp, err := os.CreateTemp(filepath.Dir(opts.Path), "."+filepath.Base(opts.Path)+".*.tmp")
	if err != nil {
		return 0, err
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmpPath)
		}
	}()

	var n int
	switch opts.Format {
	case CSV:
		n, err = writeCSV(tmp, rows, cols, opts, progress)
	case XLSX:
		n, err = writeXLSX(tmp, rows, cols, opts, progress)
	default:
		err = fmt.Errorf("unknown export format %q", opts.Format)
	}
	if err == nil {
		err = rows.Err()
	}
	if err == nil {
		err = tmp.Close()
	}
	if err != nil {
		return n, err
	}
	if err := os.Rename(tmpPath, opts.Path); err != nil {
		return n, err
	}
	ok = true
	return n, nil
}

// scanner reads rows into a reusable buffer.
type scanner struct {
	vals []any
	ptrs []any
}

func newScanner(n int) *scanner {
	s := &scanner{vals: make([]any, n), ptrs: make([]any, n)}
	for i := range s.vals {
		s.ptrs[i] = &s.vals[i]
	}
	return s
}

func (s *scanner) scan(rows *sql.Rows) error { return rows.Scan(s.ptrs...) }

const progressEvery = 5000

func writeCSV(f *os.File, rows *sql.Rows, cols []value.Column, opts Options, progress func(int)) (int, error) {
	w := csv.NewWriter(f)
	if opts.Separator != 0 {
		w.Comma = opts.Separator
	}
	w.UseCRLF = opts.CRLF
	record := make([]string, len(cols))
	if opts.Header {
		for i, c := range cols {
			record[i] = c.Name
		}
		if err := w.Write(record); err != nil {
			return 0, err
		}
	}
	s := newScanner(len(cols))
	n := 0
	for rows.Next() {
		if err := s.scan(rows); err != nil {
			return n, err
		}
		for i, c := range cols {
			if s.vals[i] == nil {
				record[i] = opts.NullText
			} else {
				record[i] = c.Text(s.vals[i])
			}
		}
		if err := w.Write(record); err != nil {
			return n, err
		}
		n++
		if progress != nil && n%progressEvery == 0 {
			progress(n)
		}
	}
	w.Flush()
	return n, w.Error()
}

func writeXLSX(f *os.File, rows *sql.Rows, cols []value.Column, opts Options, progress func(int)) (int, error) {
	x := excelize.NewFile()
	defer x.Close()
	sheet := opts.SheetName
	if sheet == "" {
		sheet = "Sheet1"
	}
	if sheet != "Sheet1" {
		if err := x.SetSheetName("Sheet1", sheet); err != nil {
			return 0, err
		}
	}
	sw, err := x.NewStreamWriter(sheet)
	if err != nil {
		return 0, err
	}
	bold, err := x.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	if err != nil {
		return 0, err
	}
	dateStyle, err := x.NewStyle(&excelize.Style{NumFmt: 14}) // m/d/yyyy, localised by Excel
	if err != nil {
		return 0, err
	}
	stampFmt := "yyyy-mm-dd hh:mm:ss"
	stampStyle, err := x.NewStyle(&excelize.Style{CustomNumFmt: &stampFmt})
	if err != nil {
		return 0, err
	}

	rowNum := 1
	if opts.Header {
		// Keep the header visible while scrolling (must precede SetRow).
		if err := sw.SetPanes(&excelize.Panes{Freeze: true, YSplit: 1, TopLeftCell: "A2", ActivePane: "bottomLeft"}); err != nil {
			return 0, err
		}
		header := make([]any, len(cols))
		for i, c := range cols {
			header[i] = excelize.Cell{StyleID: bold, Value: c.Name}
		}
		if err := sw.SetRow("A1", header); err != nil {
			return 0, err
		}
		rowNum++
	}

	s := newScanner(len(cols))
	cells := make([]any, len(cols))
	n := 0
	for rows.Next() {
		if n >= MaxXLSXRows {
			return n, errors.New("the result has more rows than an XLSX worksheet can hold (1,048,575); export to CSV instead")
		}
		if err := s.scan(rows); err != nil {
			return n, err
		}
		for i, c := range cols {
			cells[i] = xlsxCell(c, s.vals[i], dateStyle, stampStyle)
		}
		cell, _ := excelize.CoordinatesToCellName(1, rowNum)
		if err := sw.SetRow(cell, cells); err != nil {
			return n, err
		}
		rowNum++
		n++
		if progress != nil && n%progressEvery == 0 {
			progress(n)
		}
	}
	if err := sw.Flush(); err != nil {
		return n, err
	}
	_, err = x.WriteTo(f)
	return n, err
}

// xlsxCell converts a value to a typed cell: numbers, booleans and dates
// keep their type so that they can be used in formulas.
func xlsxCell(c value.Column, v any, dateStyle, stampStyle int) any {
	switch x := v.(type) {
	case nil:
		return nil
	case bool:
		return x
	case time.Time:
		style := stampStyle
		if x.Hour() == 0 && x.Minute() == 0 && x.Second() == 0 && x.Nanosecond() == 0 {
			style = dateStyle
		}
		if x.Year() < 1900 { // Excel cannot represent it: keep the text
			return c.Text(v)
		}
		// Excel has no time zones: write the wall-clock time.
		return excelize.Cell{StyleID: style, Value: time.Date(x.Year(), x.Month(), x.Day(),
			x.Hour(), x.Minute(), x.Second(), x.Nanosecond(), time.UTC)}
	}
	if f, ok := c.Number(v); ok {
		return f
	}
	return c.Text(v)
}
