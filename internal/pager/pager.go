// Package pager streams a result set one page at a time from an open cursor.
//
// The query is never rewritten with LIMIT/OFFSET: rows are read from the
// cursor on demand and the pages already fetched are kept in memory.
package pager

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
)

// PageSize is the number of rows in a page.
const PageSize = 1000

// Queryer is implemented by *sql.DB, *sql.Conn and db.Session.
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// interrupter is implemented by queryers that stop a running query
// server-side (db.Session for MySQL/MariaDB).
type interrupter interface {
	Interrupt()
}

// Pager keeps a cursor open and fetches one page at a time on demand.
// Its methods are safe to call from any goroutine.
//
// A query may return several result sets (SQL Server batches, procedure
// calls): each one is read by its own pager, see NextResult.
type Pager struct {
	Cols []*sql.ColumnType

	cancel    context.CancelFunc
	interrupt func()
	finished  atomic.Bool // the result set is exhausted or closed
	handedOff atomic.Bool // the cursor now belongs to the next result's pager

	mu    sync.Mutex
	rows  *sql.Rows
	pages [][][]any
	done  bool
	next  bool // another result set follows; the cursor is kept open for it
}

// New runs query and returns a pager positioned before the first row.
//
// Cancelling ctx interrupts the query while New runs. Afterwards the cursor
// is independent of ctx (it must outlive the call that created it: pages
// are fetched later); use Cancel or Close to stop it.
func New(ctx context.Context, q Queryer, query string) (*Pager, error) {
	cctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, cancel)
	rows, err := q.QueryContext(cctx, query)
	if !stop() && err == nil {
		// ctx was cancelled while the query started.
		err = ctx.Err()
		rows.Close()
	}
	if err != nil {
		cancel()
		return nil, err
	}
	cols, err := rows.ColumnTypes()
	if err != nil {
		rows.Close()
		cancel()
		return nil, err
	}
	p := &Pager{rows: rows, Cols: cols, cancel: cancel}
	if in, ok := q.(interrupter); ok {
		p.interrupt = in.Interrupt
	}
	return p, nil
}

// Page returns page n (0-based), fetching from the cursor as needed. It
// returns nil rows if the result has fewer pages.
func (p *Pager) Page(n int) ([][]any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
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

// Last fetches all remaining rows and returns the index of the last page
// (0 for an empty result).
func (p *Pager) Last() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for !p.done {
		if err := p.fetchNext(); err != nil {
			return 0, err
		}
	}
	return max(len(p.pages)-1, 0), nil
}

func (p *Pager) fetchNext() error {
	var page [][]any
	for len(page) < PageSize {
		if !p.rows.Next() {
			p.setDone()
			if err := p.rows.Err(); err != nil {
				p.rows.Close()
				return err
			}
			if p.next = p.rows.NextResultSet(); !p.next {
				p.rows.Close()
			}
			break
		}
		vals := make([]any, len(p.Cols))
		ptrs := make([]any, len(p.Cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := p.rows.Scan(ptrs...); err != nil {
			p.setDone()
			p.rows.Close()
			return err
		}
		page = append(page, vals)
	}
	if len(page) > 0 {
		p.pages = append(p.pages, page)
	}
	return nil
}

// NextResult returns a pager on the next result set of the query, or nil
// if the current one has not been fully read or is the last one. The cursor
// then belongs to the returned pager: closing p no longer affects it.
func (p *Pager) NextResult() (*Pager, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.next {
		return nil, nil
	}
	p.next = false
	p.handedOff.Store(true)
	n := &Pager{rows: p.rows, cancel: p.cancel, interrupt: p.interrupt}
	cols, err := p.rows.ColumnTypes()
	if err != nil {
		n.Close()
		return nil, err
	}
	n.Cols = cols
	return n, nil
}

// Done reports whether the cursor has been fully read.
func (p *Pager) Done() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done
}

// Pages returns the number of pages fetched so far.
func (p *Pager) Pages() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pages)
}

func (p *Pager) setDone() {
	p.done = true
	p.finished.Store(true)
}

// Cancel interrupts a fetch in progress. It may be called from any goroutine
// and does not wait for the fetch to return.
func (p *Pager) Cancel() {
	if p.handedOff.Load() {
		return
	}
	p.stopServer()
	p.cancel()
}

// stopServer interrupts the query server-side while the cursor is open, so
// that closing it does not have to read the remaining rows.
func (p *Pager) stopServer() {
	if p.interrupt != nil && !p.finished.Load() {
		p.interrupt()
	}
}

// Close cancels the cursor and releases it. It waits for any fetch in
// progress to return.
func (p *Pager) Close() {
	if p.handedOff.Load() {
		return
	}
	p.stopServer()
	p.cancel()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rows.Close()
	p.setDone()
}
