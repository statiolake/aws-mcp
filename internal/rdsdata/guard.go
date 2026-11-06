// Package rdsdata runs read-only SQL through the RDS Data API.
package rdsdata

import (
	"fmt"
	"strings"
)

// ReadStatements are the statement keywords rds_query accepts.
var ReadStatements = []string{"SELECT", "WITH", "SHOW", "EXPLAIN", "DESCRIBE", "DESC"}

// forbidden keywords may not appear anywhere outside literals, even inside a
// read statement: data-modifying CTEs (WITH x AS (DELETE ...)), EXPLAIN
// ANALYZE of DML, SELECT ... INTO, SELECT ... FOR UPDATE. Statement-only
// keywords (BEGIN, SET, CALL, ...) are already excluded by the first-keyword
// and single-statement rules, so they are not listed here to avoid refusing
// columns named "start" or "comment".
var forbidden = map[string]bool{
	"INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true, "UPSERT": true,
	"INTO": true, "OUTFILE": true, "DUMPFILE": true,
	"CREATE": true, "DROP": true, "ALTER": true, "TRUNCATE": true, "GRANT": true, "REVOKE": true,
}

// CheckReadOnly returns an error unless sql is a single read statement. It is
// a pure, conservative lexical check: string literals, quoted identifiers and
// comments are skipped (both with and without backslash escapes, so neither
// PostgreSQL's nor MySQL's string rules can hide a second statement), then
// the statement must start with a read keyword, contain no write keyword and
// have no ';' except at the end.
func CheckReadOnly(sql string) error {
	for _, backslash := range []bool{false, true} {
		if err := check(sql, backslash); err != nil {
			return err
		}
	}
	return nil
}

func check(sql string, backslashEscapes bool) error {
	words, semis, err := lex(sql, backslashEscapes)
	if err != nil {
		return err
	}
	if len(words) == 0 {
		return fmt.Errorf("empty statement")
	}
	if len(semis) > 1 || (len(semis) == 1 && semis[0] != len(words)) {
		return fmt.Errorf("refused: only a single statement is allowed (found ';' between statements)")
	}
	first := words[0]
	ok := false
	for _, k := range ReadStatements {
		if first == k {
			ok = true
		}
	}
	if !ok {
		return fmt.Errorf("refused: only read statements (%s) are allowed, got %s", strings.Join(ReadStatements, ", "), first)
	}
	for _, w := range words {
		if forbidden[w] {
			return fmt.Errorf("refused: keyword %s is not allowed in a read-only query", w)
		}
	}
	return nil
}

// lex returns the upper-cased bare words of sql outside literals and
// comments, and for each ';' the number of words before it.
func lex(s string, backslashEscapes bool) (words []string, semis []int, err error) {
	i := 0
	n := len(s)
	for i < n {
		c := s[i]
		switch {
		case c == '-' && i+1 < n && s[i+1] == '-':
			for i < n && s[i] != '\n' {
				i++
			}
		case c == '#':
			// MySQL line comment; in PostgreSQL '#' is an operator. Treating
			// it as a comment could hide text, so refuse it instead.
			return nil, nil, fmt.Errorf("refused: '#' is not allowed (ambiguous between MySQL comments and PostgreSQL operators)")
		case c == '/' && i+1 < n && s[i+1] == '*':
			depth := 0
			for i < n {
				if i+1 < n && s[i] == '/' && s[i+1] == '*' {
					depth++
					i += 2
					continue
				}
				if i+1 < n && s[i] == '*' && s[i+1] == '/' {
					depth--
					i += 2
					if depth == 0 {
						break
					}
					continue
				}
				i++
			}
			if depth != 0 {
				return nil, nil, fmt.Errorf("unterminated comment")
			}
		case c == '\'' || c == '"' || c == '`':
			j, ok := skipQuoted(s, i, c, backslashEscapes && c == '\'')
			if !ok {
				return nil, nil, fmt.Errorf("unterminated quoted string")
			}
			i = j
		case c == '$':
			// PostgreSQL dollar quoting: $tag$ ... $tag$ (tag may be empty).
			j := i + 1
			for j < n && (isWordChar(s[j]) && !(s[j] >= '0' && s[j] <= '9' && j == i+1)) {
				j++
			}
			if j < n && s[j] == '$' {
				tag := s[i : j+1]
				end := strings.Index(s[j+1:], tag)
				if end < 0 {
					return nil, nil, fmt.Errorf("unterminated dollar-quoted string")
				}
				i = j + 1 + end + len(tag)
			} else {
				i = j // positional parameter like $1
			}
		case c == ';':
			semis = append(semis, len(words))
			i++
		case isWordChar(c):
			j := i
			for j < n && isWordChar(s[j]) {
				j++
			}
			w := strings.ToUpper(s[i:j])
			if !(w[0] >= '0' && w[0] <= '9') {
				words = append(words, w)
			}
			i = j
		default:
			i++
		}
	}
	return words, semis, nil
}

func skipQuoted(s string, i int, q byte, backslash bool) (int, bool) {
	i++
	for i < len(s) {
		c := s[i]
		if backslash && c == '\\' {
			i += 2
			continue
		}
		if c == q {
			if i+1 < len(s) && s[i+1] == q { // doubled quote
				i += 2
				continue
			}
			return i + 1, true
		}
		i++
	}
	return i, false
}

func isWordChar(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c >= 0x80
}
