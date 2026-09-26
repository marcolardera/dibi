// Package sqlsplit splits a script into individual SQL statements.
//
// It understands the quoting and comment rules of each supported dialect,
// so that semicolons inside strings, identifiers, comments or procedural
// blocks do not end a statement:
//
//   - Postgres: dollar-quoted bodies ($$ … $$, $tag$ … $tag$), E'…' strings.
//   - MySQL/MariaDB: backslash escapes, `backticks`, # comments, the client
//     DELIMITER command, and BEGIN … END bodies of routines and triggers.
//   - Oracle: q'[…]' strings, PL/SQL blocks (BEGIN/DECLARE, CREATE
//     PROCEDURE/FUNCTION/TRIGGER) and the SQL*Plus "/" line terminator,
//     which is required for CREATE PACKAGE and CREATE TYPE.
//   - SQLite/DuckDB (Generic): CREATE TRIGGER … BEGIN … END bodies.
package sqlsplit

import (
	"strings"
	"unicode/utf8"
)

// Dialect selects the lexical rules used for splitting.
type Dialect int

const (
	Generic Dialect = iota
	Postgres
	MySQL
	Oracle
)

// Statement is one statement of a script.
type Statement struct {
	Text  string // statement text, trimmed, without the trailing delimiter
	Start int    // byte offset of Text in the original script
}

// blockMode tells how a statement containing procedural code ends.
type blockMode int

const (
	plain     blockMode = iota // ends at the delimiter
	depth                      // ends at the delimiter once BEGIN … END blocks are closed
	slashOnly                  // ends only at a "/" line (Oracle packages and types)
)

type splitter struct {
	src   string
	d     Dialect
	delim string
	out   []Statement

	// current statement
	start       int // -1 when no token has been seen yet
	words       []string
	depth       int
	seenBegin   bool
	pendingEnd  bool
	lastWordEnd int
}

// Split splits a script into statements. Empty statements (only whitespace
// or comments) are dropped.
func Split(src string, d Dialect) []Statement {
	s := &splitter{src: src, d: d, delim: ";", start: -1}
	s.run()
	return s.out
}

func (s *splitter) reset() {
	s.start = -1
	s.words = s.words[:0]
	s.depth = 0
	s.seenBegin = false
	s.pendingEnd = false
}

// emit ends the current statement at byte offset end (exclusive).
func (s *splitter) emit(end int) {
	if s.start >= 0 {
		text := strings.TrimRightFunc(s.src[s.start:end], isSpace)
		if text != "" {
			s.out = append(s.out, Statement{Text: text, Start: s.start})
		}
	}
	s.reset()
}

func (s *splitter) mark(i int) {
	if s.start < 0 {
		s.start = i
	}
}

func (s *splitter) run() {
	src := s.src
	i := 0
	lineStart := true
	for i < len(src) {
		c := src[i]

		if lineStart {
			// Oracle: a line containing only "/" ends the statement.
			if s.d == Oracle {
				if end, ok := slashLine(src, i); ok {
					s.emit(i)
					i = end
					continue
				}
			}
			// MySQL: DELIMITER command, only between statements.
			if s.d == MySQL && s.start < 0 {
				if newDelim, end, ok := delimiterCommand(src, i); ok {
					s.delim = newDelim
					i = end
					continue
				}
			}
		}

		switch {
		case c == '\n':
			lineStart = true
			i++
			continue
		case isSpace(rune(c)):
			i++
			continue
		}
		lineStart = false

		switch {
		case c == '-' && strings.HasPrefix(src[i:], "--"):
			i = skipLine(src, i)
			continue
		case c == '#' && s.d == MySQL:
			i = skipLine(src, i)
			continue
		case c == '/' && strings.HasPrefix(src[i:], "/*"):
			if end := strings.Index(src[i+2:], "*/"); end >= 0 {
				i += 2 + end + 2
			} else {
				i = len(src)
			}
			continue
		}

		// Delimiter.
		if strings.HasPrefix(src[i:], s.delim) && s.delim != "" {
			if s.endsHere() {
				if s.d == Oracle && s.mode() != plain {
					// PL/SQL blocks keep their final ';' (END;), plain SQL must not.
					s.emit(i + len(s.delim))
				} else {
					s.emit(i)
				}
				i += len(s.delim)
				continue
			}
			s.mark(i)
			i += len(s.delim)
			continue
		}

		s.mark(i)
		switch {
		case c == '\'':
			i = skipQuoted(src, i, '\'', s.backslashEscapes(i))
		case c == '"':
			i = skipQuoted(src, i, '"', s.d == MySQL)
		case c == '`':
			i = skipQuoted(src, i, '`', false)
		case (c == 'q' || c == 'Q') && s.d == Oracle && i+2 < len(src) && src[i+1] == '\'' && !identBefore(src, i):
			i = skipOracleQ(src, i)
		case c == '$' && s.d == Postgres && !identBefore(src, i):
			if end, ok := skipDollar(src, i); ok {
				i = end
			} else {
				i++
			}
		case isIdentStart(c):
			j := i + 1
			for j < len(src) && isIdentChar(src[j]) {
				j++
			}
			s.word(strings.ToUpper(src[i:j]))
			i = j
		default:
			_, size := utf8.DecodeRuneInString(src[i:])
			i += size
		}
	}
	s.emit(len(src))
}

// word records a keyword or identifier and tracks BEGIN … END nesting.
func (s *splitter) word(w string) {
	if len(s.words) < 12 {
		s.words = append(s.words, w)
	}
	if s.pendingEnd {
		s.pendingEnd = false
		switch w {
		case "IF", "LOOP", "WHILE", "REPEAT":
			// END IF / END LOOP / …: these openers are not counted.
			return
		case "CASE":
			// END CASE closes a CASE statement; CASE is not a new opener.
			s.depth--
			return
		}
		s.depth--
	}
	switch w {
	case "BEGIN":
		s.depth++
		s.seenBegin = true
	case "CASE":
		s.depth++
	case "END":
		s.pendingEnd = true
	}
}

// endsHere reports whether a delimiter at the current position ends the statement.
func (s *splitter) endsHere() bool {
	if s.pendingEnd {
		s.pendingEnd = false
		s.depth--
	}
	if s.delim != ";" {
		return true // MySQL custom delimiter: always a hard terminator
	}
	switch s.mode() {
	case slashOnly:
		return false
	case depth:
		if s.depth > 0 {
			return false
		}
		// Oracle routines may declare variables (ending in ';') before BEGIN.
		if s.d == Oracle && !s.seenBegin {
			return false
		}
		return true
	}
	return true
}

func (s *splitter) mode() blockMode {
	if len(s.words) == 0 {
		return plain
	}
	first := s.words[0]
	if s.d == Oracle && (first == "BEGIN" || first == "DECLARE") {
		return depth
	}
	if first != "CREATE" {
		return plain
	}
	switch objectKind(s.words[1:]) {
	case "PROCEDURE", "FUNCTION":
		if s.d == Oracle || s.d == MySQL {
			return depth
		}
	case "TRIGGER":
		if s.d != Postgres {
			return depth
		}
	case "EVENT":
		if s.d == MySQL {
			return depth
		}
	case "PACKAGE", "TYPE":
		if s.d == Oracle {
			return slashOnly
		}
	}
	return plain
}

// objectKind finds the object type of a CREATE statement, skipping
// modifiers like OR REPLACE, TEMP, DEFINER=…, EDITIONABLE.
func objectKind(words []string) string {
	for _, w := range words {
		switch w {
		case "TABLE", "VIEW", "INDEX", "SEQUENCE", "SCHEMA", "DATABASE", "USER", "ROLE",
			"PROCEDURE", "FUNCTION", "TRIGGER", "EVENT", "PACKAGE", "TYPE", "MACRO", "SYNONYM":
			return w
		}
	}
	return ""
}

// backslashEscapes reports whether a string literal starting at i uses
// backslash escapes: always in MySQL, and for E'…' strings in Postgres.
func (s *splitter) backslashEscapes(i int) bool {
	if s.d == MySQL {
		return true
	}
	if s.d == Postgres && i > 0 && (s.src[i-1] == 'E' || s.src[i-1] == 'e') && !identBefore(s.src, i-1) {
		return true
	}
	return false
}

// skipQuoted skips a quoted string or identifier starting at i. A doubled
// quote character is an escaped quote.
func skipQuoted(src string, i int, q byte, backslash bool) int {
	i++
	for i < len(src) {
		switch src[i] {
		case '\\':
			if backslash {
				i += 2
				continue
			}
		case q:
			if i+1 < len(src) && src[i+1] == q {
				i += 2
				continue
			}
			return i + 1
		}
		i++
	}
	return len(src)
}

// skipOracleQ skips an Oracle alternative-quoting literal q'X…X'.
func skipOracleQ(src string, i int) int {
	open := src[i+2]
	closer := open
	switch open {
	case '[':
		closer = ']'
	case '{':
		closer = '}'
	case '(':
		closer = ')'
	case '<':
		closer = '>'
	}
	end := strings.Index(src[i+3:], string(closer)+"'")
	if end < 0 {
		return len(src)
	}
	return i + 3 + end + 2
}

// skipDollar skips a Postgres dollar-quoted string ($$…$$ or $tag$…$tag$).
func skipDollar(src string, i int) (int, bool) {
	j := i + 1
	for j < len(src) && src[j] != '$' {
		if !(isIdentStart(src[j]) || (j > i+1 && src[j] >= '0' && src[j] <= '9')) {
			return 0, false
		}
		j++
	}
	if j >= len(src) {
		return 0, false
	}
	tag := src[i : j+1]
	end := strings.Index(src[j+1:], tag)
	if end < 0 {
		return len(src), true
	}
	return j + 1 + end + len(tag), true
}

func skipLine(src string, i int) int {
	if end := strings.IndexByte(src[i:], '\n'); end >= 0 {
		return i + end
	}
	return len(src)
}

// slashLine reports whether the line starting at i contains only "/".
func slashLine(src string, i int) (int, bool) {
	end := skipLine(src, i)
	if strings.TrimSpace(src[i:end]) != "/" {
		return 0, false
	}
	return end, true
}

// delimiterCommand parses a MySQL client "DELIMITER xx" line starting at i.
func delimiterCommand(src string, i int) (string, int, bool) {
	j := i
	for j < len(src) && (src[j] == ' ' || src[j] == '\t') {
		j++
	}
	const kw = "DELIMITER"
	if len(src)-j < len(kw)+2 || !strings.EqualFold(src[j:j+len(kw)], kw) || !isSpace(rune(src[j+len(kw)])) {
		return "", 0, false
	}
	end := skipLine(src, j)
	d := strings.TrimSpace(src[j+len(kw) : end])
	if d == "" {
		return "", 0, false
	}
	return d, end, true
}

func isSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' || r == '\v'
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isIdentChar(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9') || c == '$' || c == '#'
}

func identBefore(src string, i int) bool {
	return i > 0 && isIdentChar(src[i-1])
}

// ReturnsRows reports whether a statement is expected to produce a result
// set and should therefore be run as a query rather than executed.
func ReturnsRows(stmt string) bool {
	w := firstWord(stmt)
	switch w {
	case "SELECT", "WITH", "VALUES", "TABLE", "SHOW", "DESCRIBE", "DESC", "EXPLAIN",
		"PRAGMA", "FROM", "SUMMARIZE", "PIVOT", "UNPIVOT", "(":
		return true
	}
	return false
}

// firstWord returns the first keyword of a statement, upper-cased, skipping
// leading comments.
func firstWord(stmt string) string {
	i := 0
	for i < len(stmt) {
		switch {
		case isSpace(rune(stmt[i])):
			i++
		case strings.HasPrefix(stmt[i:], "--") || stmt[i] == '#':
			i = skipLine(stmt, i)
		case strings.HasPrefix(stmt[i:], "/*"):
			end := strings.Index(stmt[i+2:], "*/")
			if end < 0 {
				return ""
			}
			i += 2 + end + 2
		case stmt[i] == '(':
			return "("
		default:
			j := i
			for j < len(stmt) && isIdentChar(stmt[j]) {
				j++
			}
			return strings.ToUpper(stmt[i:j])
		}
	}
	return ""
}
