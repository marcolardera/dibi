// Package importer creates a table from a CSV or XLSX file.
//
// The file is read twice. The first pass checks it and infers the type of
// each column; nothing is written if it finds a problem. The second pass
// streams the rows to the database, which loads them with its bulk path
// (see db.Source.CreateTable).
package importer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"dibi/internal/db"
)

// Format is an import file format.
type Format string

const (
	CSV  Format = "csv"
	XLSX Format = "xlsx"
)

// Options configures an import.
type Options struct {
	Format Format
	Path   string
	Header bool   // the first row holds the column names
	Table  string // name of the table to create

	// CSV only.
	Separator rune   // ',' by default
	NullText  string // values read as NULL ("" = empty values)

	// XLSX only.
	SheetName string // "" = the first sheet
}

// maxErrors is the number of problems reported before giving up.
const maxErrors = 100

// FileErrors lists the problems found in the file. Nothing was imported.
type FileErrors struct {
	List      []string
	Truncated bool // there were more than maxErrors
}

func (e *FileErrors) Error() string {
	msg := strings.Join(e.List, "\n")
	if e.Truncated {
		msg += "\n…"
	}
	return msg
}

func (e *FileErrors) add(where, msg string) {
	if len(e.List) == maxErrors {
		e.Truncated = true
		return
	}
	if where != "" {
		msg = where + ": " + msg
	}
	e.List = append(e.List, msg)
}

// Result describes a completed import.
type Result struct {
	Table   string // qualified name
	Rows    int64
	Columns []db.NewColumn
}

// Phases reported to the progress function.
const (
	Reading   = "Reading"
	Importing = "Importing"
)

// Run imports the file into a new table of the schema (or database) n.
// progress, if not nil, is called periodically with the phase and the
// number of rows read or imported so far. Problems in the file are
// reported as a *FileErrors.
func Run(ctx context.Context, src *db.Source, n db.Node, opts Options, progress func(phase string, rows int64)) (Result, error) {
	if progress == nil {
		progress = func(string, int64) {}
	}
	table := src.StoredName(strings.TrimSpace(opts.Table))
	switch {
	case table == "":
		return Result{}, errors.New("no table name given")
	case len(table) > db.MaxNameBytes:
		return Result{}, fmt.Errorf("the table name is longer than %d bytes", db.MaxNameBytes)
	}
	r, err := open(opts)
	if err != nil {
		return Result{}, err
	}
	defer r.close()

	cols, err := scan(ctx, r, src, opts, func(n int64) { progress(Reading, n) })
	if err != nil {
		return Result{}, err
	}
	if err := r.rewind(); err != nil {
		return Result{}, err
	}
	if opts.Header {
		if _, _, err := r.next(); err != nil && err != io.EOF {
			return Result{}, err
		}
	}
	row := make([]any, len(cols))
	next := func() ([]any, error) {
		vals, nulls, err := r.next()
		for err == nil && len(vals) == 0 { // empty rows are not imported
			vals, nulls, err = r.next()
		}
		if err != nil {
			return nil, err // io.EOF included, at every call after the last row
		}
		for i := range cols {
			if i >= len(vals) {
				row[i] = nil
				continue
			}
			if row[i], err = convert(vals[i], nulls[i], &cols[i]); err != nil {
				return nil, fmt.Errorf("%s, column %s: %v (has the file changed?)", r.where(), cols[i].Name, err)
			}
		}
		return row, nil
	}
	rows, err := src.CreateTable(ctx, n, table, cols, next, func(n int64) { progress(Importing, n) })
	if err != nil {
		return Result{}, err
	}
	return Result{Rows: rows, Columns: cols,
		Table: src.QualifiedName(db.Node{Kind: db.KindTable, Database: n.Database, Schema: n.Schema, Object: table})}, nil
}

// reader reads the rows of a file as text. Each value comes with whether it
// is NULL.
type reader interface {
	// next returns the next row; empty rows are returned as no values. It
	// returns io.EOF after the last row.
	next() (vals []string, nulls []bool, err error)
	// where locates the last row read, for messages.
	where() string
	// ragged reports whether rows may have fewer values than columns (the
	// missing ones are NULL), or more where there is no header.
	ragged() bool
	rewind() error
	close() error
}

func open(opts Options) (reader, error) {
	switch opts.Format {
	case CSV:
		return openCSV(opts)
	case XLSX:
		return openXLSX(opts)
	}
	return nil, fmt.Errorf("unknown import format %q", opts.Format)
}

// scan reads the whole file, checking it and inferring the columns.
func scan(ctx context.Context, r reader, src *db.Source, opts Options, progress func(int64)) ([]db.NewColumn, error) {
	errs := &FileErrors{}
	var header []string
	var stats []colStats
	width := -1 // number of columns, once known
	var n int64
	for first := true; ; first = false {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		vals, nulls, err := r.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			errs.add("", err.Error())
			if _, ok := err.(*fatalError); ok || len(errs.List) == maxErrors {
				break
			}
			continue
		}
		if first && opts.Header {
			header = slices.Clone(vals) // the reader reuses vals
			if r.ragged() {
				header = trimTrailingEmpty(header)
			}
			width = len(header)
			if width == 0 {
				errs.add(r.where(), "the header row is empty")
				break
			}
			continue
		}
		if len(vals) == 0 {
			continue
		}
		n++
		if n%progressEvery == 0 {
			progress(n)
		}
		switch {
		case width < 0 && !r.ragged():
			width = len(vals)
		case r.ragged() && header == nil:
			width = max(width, len(vals))
		case len(vals) > width && r.ragged():
			for i := width; i < len(vals); i++ {
				if !nulls[i] {
					errs.add(r.where(), fmt.Sprintf("value %q in column %d, which has no header", vals[i], i+1))
					break
				}
			}
			vals = vals[:width]
		case len(vals) != width && !r.ragged():
			errs.add(r.where(), fmt.Sprintf("%d values instead of %d", len(vals), width))
			continue
		}
		for len(stats) < width {
			stats = append(stats, colStats{})
		}
		for i, v := range vals {
			if nulls[i] {
				continue
			}
			if !utf8.ValidString(v) {
				errs.add(r.where(), fmt.Sprintf("column %d is not valid UTF-8 text", i+1))
				continue
			}
			stats[i].add(v)
		}
	}
	if len(errs.List) > 0 {
		return nil, errs
	}
	if width <= 0 {
		return nil, errors.New("the file holds no data")
	}
	for len(stats) < width {
		stats = append(stats, colStats{})
	}
	names := columnNames(header, width, src)
	cols := make([]db.NewColumn, width)
	for i := range cols {
		cols[i] = stats[i].column()
		cols[i].Name = names[i]
	}
	return cols, nil
}

const progressEvery = 5000

// fatalError is a reading error after which the file cannot be read further.
type fatalError struct{ err error }

func (e *fatalError) Error() string { return e.err.Error() }
func (e *fatalError) Unwrap() error { return e.err }

func trimTrailingEmpty(vals []string) []string {
	for len(vals) > 0 && strings.TrimSpace(vals[len(vals)-1]) == "" {
		vals = vals[:len(vals)-1]
	}
	return vals
}

// columnNames returns the names of the columns: the header ones, as the
// database would store them, or column_N. Missing names become column_N,
// long ones are shortened and duplicates get a numeric suffix.
func columnNames(header []string, width int, src *db.Source) []string {
	out := make([]string, width)
	taken := map[string]bool{}
	for i := range out {
		name := ""
		if i < len(header) {
			name = src.StoredName(truncate(strings.TrimSpace(header[i]), db.MaxNameBytes))
		}
		if name == "" {
			name = src.StoredName(fmt.Sprintf("column_%d", i+1))
		}
		base := name
		for k := 2; taken[strings.ToLower(name)]; k++ {
			suffix := fmt.Sprintf("_%d", k)
			name = truncate(base, db.MaxNameBytes-len(suffix)) + suffix
		}
		taken[strings.ToLower(name)] = true
		out[i] = name
	}
	return out
}

// truncate shortens s to at most n bytes, on a character boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// ---------------------------------------------------------------- inference

// class is the most specific form a value has.
type class int

const (
	clsText class = iota
	clsEmpty
	clsBool
	clsInt     // fits in int64
	clsDecimal // plain notation; includes integers too large for int64
	clsFloat   // exponent notation
	clsDate
	clsTimestamp
)

// group of a class: values of different groups make a text column.
func (c class) group() int {
	switch c {
	case clsBool:
		return 1
	case clsInt, clsDecimal, clsFloat:
		return 2
	case clsDate, clsTimestamp:
		return 3
	}
	return 0
}

// numberInfo describes a number in plain notation.
type numberInfo struct {
	intDigits, scale int // digits before (without leading zeros) and after the point
}

// classify returns the class of a value (trimmed of spaces), and for
// numbers their digits, for timestamps their fractional second digits.
func classify(s string) (class, numberInfo, int) {
	s = strings.TrimSpace(s)
	if s == "" {
		return clsEmpty, numberInfo{}, 0
	}
	if strings.EqualFold(s, "true") || strings.EqualFold(s, "false") {
		return clsBool, numberInfo{}, 0
	}
	if ni, ok := plainNumber(s); ok {
		if ni.scale == 0 {
			if _, err := strconv.ParseInt(s, 10, 64); err == nil {
				return clsInt, ni, 0
			}
		}
		return clsDecimal, ni, 0
	}
	if isFloat(s) {
		return clsFloat, numberInfo{}, 0
	}
	if len(s) == 10 {
		if _, err := time.Parse(time.DateOnly, s); err == nil {
			return clsDate, numberInfo{}, 0
		}
	}
	if _, frac, ok := parseTimestamp(s); ok {
		return clsTimestamp, numberInfo{}, frac
	}
	return clsText, numberInfo{}, 0
}

// plainNumber recognizes -?(0|[1-9][0-9]*)(\.[0-9]+)?. Integers with
// leading zeros (codes, like "007") are not numbers.
func plainNumber(s string) (numberInfo, bool) {
	i := 0
	if s[0] == '-' {
		i++
	}
	start := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	intLen := i - start
	if intLen == 0 || intLen > 1 && s[start] == '0' {
		return numberInfo{}, false
	}
	ni := numberInfo{intDigits: intLen}
	if intLen == 1 && s[start] == '0' {
		ni.intDigits = 0
	}
	if i == len(s) {
		return ni, true
	}
	if s[i] != '.' {
		return numberInfo{}, false
	}
	i++
	fracStart := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i != len(s) || i == fracStart {
		return numberInfo{}, false
	}
	ni.scale = i - fracStart
	return ni, true
}

// isFloat recognizes numbers in exponent notation, like 1.5e-3.
func isFloat(s string) bool {
	mant, exp, ok := strings.Cut(strings.ToLower(s), "e")
	if !ok || mant == "" || exp == "" {
		return false
	}
	if _, ok := plainNumber(mant); !ok {
		return false
	}
	if exp[0] == '+' || exp[0] == '-' {
		exp = exp[1:]
	}
	if exp == "" || strings.Trim(exp, "0123456789") != "" {
		return false
	}
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}

// parseTimestamp parses "YYYY-MM-DD hh:mm[:ss[.fff]]" (or with a T),
// without time zone. It returns the time in UTC and the number of
// fractional second digits.
func parseTimestamp(s string) (time.Time, int, bool) {
	if len(s) < 16 || (s[10] != ' ' && s[10] != 'T') {
		return time.Time{}, 0, false
	}
	b := []byte(s)
	b[10] = ' '
	s = string(b)
	frac := 0
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		frac = len(s) - dot - 1
	}
	layout := "2006-01-02 15:04"
	switch {
	case len(s) == 16:
	case len(s) == 19:
		layout += ":05"
	case len(s) > 20 && s[19] == '.' && frac <= 9:
		layout += ":05." + strings.Repeat("0", frac)
	default:
		return time.Time{}, 0, false
	}
	t, err := time.Parse(layout, s)
	return t, frac, err == nil
}

// colStats accumulates what the first pass learns about a column.
type colStats struct {
	seen   [clsTimestamp + 1]bool
	groups int // bit set of the groups seen

	minInt, maxInt   int64
	intDigits, scale int
	fracDigits       int

	maxChars, maxBytes, maxUTF16 int
}

func (s *colStats) add(v string) {
	cls, ni, frac := classify(v)
	if cls == clsInt {
		n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if !s.seen[clsInt] {
			s.minInt, s.maxInt = n, n
		}
		s.minInt, s.maxInt = min(s.minInt, n), max(s.maxInt, n)
	}
	s.seen[cls] = true
	if cls != clsEmpty {
		s.groups |= 1 << cls.group()
	}
	s.intDigits = max(s.intDigits, ni.intDigits)
	s.scale = max(s.scale, ni.scale)
	s.fracDigits = max(s.fracDigits, frac)
	chars, utf16 := 0, 0
	for _, r := range v {
		chars++
		utf16++
		if r > 0xFFFF {
			utf16++
		}
	}
	s.maxChars = max(s.maxChars, chars)
	s.maxBytes = max(s.maxBytes, len(v))
	s.maxUTF16 = max(s.maxUTF16, utf16)
}

// column returns the column type that holds every value seen.
func (s *colStats) column() db.NewColumn {
	text := db.NewColumn{Kind: db.ColText, MaxChars: s.maxChars, MaxBytes: s.maxBytes, MaxUTF16: s.maxUTF16}
	switch s.groups {
	case 1 << 1:
		return db.NewColumn{Kind: db.ColBool}
	case 1 << 2:
		switch {
		case s.seen[clsFloat]:
			return db.NewColumn{Kind: db.ColFloat}
		case s.seen[clsDecimal]:
			p := max(s.intDigits+s.scale, 1)
			if p > db.MaxDecimalPrecision {
				return text
			}
			return db.NewColumn{Kind: db.ColDecimal, Precision: p, Scale: s.scale}
		case s.minInt >= -1<<31 && s.maxInt < 1<<31:
			return db.NewColumn{Kind: db.ColInt}
		default:
			return db.NewColumn{Kind: db.ColBigInt}
		}
	case 1 << 3:
		if s.seen[clsTimestamp] {
			return db.NewColumn{Kind: db.ColTimestamp, FracDigits: s.fracDigits}
		}
		return db.NewColumn{Kind: db.ColDate}
	}
	return text
}

// convert turns a value into the Go value loaded into a column.
func convert(v string, null bool, c *db.NewColumn) (any, error) {
	if null {
		return nil, nil
	}
	if c.Kind == db.ColText {
		return v, nil
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	switch c.Kind {
	case db.ColBool:
		switch {
		case strings.EqualFold(v, "true"):
			return true, nil
		case strings.EqualFold(v, "false"):
			return false, nil
		}
	case db.ColInt, db.ColBigInt:
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n, nil
		}
	case db.ColDecimal:
		if _, ok := plainNumber(v); ok {
			return v, nil
		}
	case db.ColFloat:
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f, nil
		}
	case db.ColDate:
		if t, err := time.Parse(time.DateOnly, v); err == nil {
			return t, nil
		}
	case db.ColTimestamp:
		if len(v) == 10 {
			if t, err := time.Parse(time.DateOnly, v); err == nil {
				return t, nil
			}
		}
		if t, _, ok := parseTimestamp(v); ok {
			return t, nil
		}
	}
	return nil, fmt.Errorf("unexpected value %q", v)
}
