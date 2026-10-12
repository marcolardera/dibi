package importer

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"io"
	"os"
)

// csvReader reads a CSV file. Values equal to the NULL text are NULL.
type csvReader struct {
	opts  Options
	f     *os.File
	r     *csv.Reader
	line  int
	nulls []bool
}

func openCSV(opts Options) (*csvReader, error) {
	f, err := os.Open(opts.Path)
	if err != nil {
		return nil, err
	}
	c := &csvReader{opts: opts, f: f}
	if err := c.rewind(); err != nil {
		f.Close()
		return nil, err
	}
	return c, nil
}

func (c *csvReader) rewind() error {
	if _, err := c.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	br := bufio.NewReaderSize(c.f, 256*1024)
	if bom, _ := br.Peek(3); string(bom) == "\xEF\xBB\xBF" {
		br.Discard(3)
	}
	c.r = csv.NewReader(br)
	c.r.Comma = c.opts.Separator
	if c.r.Comma == 0 {
		c.r.Comma = ','
	}
	c.r.FieldsPerRecord = -1 // checked by the caller, with a better message
	c.r.ReuseRecord = true
	c.line = 0
	return nil
}

func (c *csvReader) next() ([]string, []bool, error) {
	rec, err := c.r.Read()
	if err == io.EOF {
		return nil, nil, err
	}
	if err != nil {
		if pe, ok := err.(*csv.ParseError); ok {
			c.line = pe.StartLine
			return nil, nil, fmt.Errorf("line %d: %v", pe.StartLine, pe.Err)
		}
		return nil, nil, &fatalError{err}
	}
	c.line, _ = c.r.FieldPos(0)
	c.nulls = c.nulls[:0]
	for _, v := range rec {
		c.nulls = append(c.nulls, v == c.opts.NullText)
	}
	return rec, c.nulls, nil
}

func (c *csvReader) where() string { return fmt.Sprintf("line %d", c.line) }
func (c *csvReader) ragged() bool  { return false }
func (c *csvReader) close() error  { return c.f.Close() }
