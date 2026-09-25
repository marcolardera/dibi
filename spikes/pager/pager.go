// Package pager streams a result set one page at a time from an open cursor.
package pager

import (
	"context"
	"database/sql"
)

const PageSize = 1000

// Pager keeps a cursor open and fetches one page at a time on demand.
type Pager struct {
	rows  *sql.Rows
	Cols  []*sql.ColumnType
	pages [][][]any
	done  bool
}

func New(ctx context.Context, db *sql.DB, query string) (*Pager, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	cols, err := rows.ColumnTypes()
	if err != nil {
		rows.Close()
		return nil, err
	}
	return &Pager{rows: rows, Cols: cols}, nil
}

// Page returns page n (0-based), fetching from the cursor as needed.
func (p *Pager) Page(n int) ([][]any, error) {
	for len(p.pages) <= n && !p.done {
		if err := p.fetchNext(); err != nil {
			return nil, err
		}
	}
	if n >= len(p.pages) {
		return nil, nil
	}
	return p.pages[n], nil
}

func (p *Pager) fetchNext() error {
	var page [][]any
	for len(page) < PageSize {
		if !p.rows.Next() {
			p.done = true
			p.rows.Close()
			if err := p.rows.Err(); err != nil {
				return err
			}
			break
		}
		vals := make([]any, len(p.Cols))
		ptrs := make([]any, len(p.Cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := p.rows.Scan(ptrs...); err != nil {
			return err
		}
		page = append(page, vals)
	}
	if len(page) > 0 {
		p.pages = append(p.pages, page)
	}
	return nil
}

// Last fetches everything and returns the index of the last page.
func (p *Pager) Last() (int, error) {
	for !p.done {
		if err := p.fetchNext(); err != nil {
			return 0, err
		}
	}
	return len(p.pages) - 1, nil
}

// Done reports whether the cursor has been fully read.
func (p *Pager) Done() bool { return p.done }

// Fetched returns the number of pages fetched so far.
func (p *Pager) Fetched() int { return len(p.pages) }

// Close releases the cursor.
func (p *Pager) Close() error { return p.rows.Close() }
