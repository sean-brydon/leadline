package pgproxy

import (
	"regexp"
	"strings"
)

// skipBlank returns the index of the first byte of sql from i on that is
// neither white space nor part of a comment.
func skipBlank(sql string, i int) int {
	for i < len(sql) {
		switch c := sql[i]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			i++
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			nl := strings.IndexByte(sql[i:], '\n')
			if nl < 0 {
				return len(sql)
			}
			i += nl + 1
		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			i = skipBlockComment(sql, i)
		default:
			return i
		}
	}
	return i
}

// skipBlockComment steps over the comment that opens at i. Postgres block
// comments nest.
func skipBlockComment(sql string, i int) int {
	depth := 0
	for i < len(sql) {
		switch {
		case strings.HasPrefix(sql[i:], "/*"):
			depth++
			i += 2
		case strings.HasPrefix(sql[i:], "*/"):
			depth--
			i += 2
			if depth == 0 {
				return i
			}
		default:
			i++
		}
	}
	return i
}

// keyword is a statement's first word in capitals (SELECT, INSERT, WITH):
// the name its span goes by.
func keyword(sql string) string {
	i := skipBlank(sql, 0)
	for i < len(sql) && sql[i] == '(' {
		i = skipBlank(sql, i+1)
	}
	j := i
	for j < len(sql) && isLetter(sql[j]) {
		j++
	}
	if j == i {
		return "QUERY"
	}
	return strings.ToUpper(sql[i:j])
}

func isLetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
}

func isIdent(c byte) bool {
	return isLetter(c) || c >= '0' && c <= '9' || c == '$' || c >= 0x80
}

// splitStatements cuts the text of a simple query at its top-level
// semicolons, the way the server will, and drops the empty pieces (the
// server answers nothing for those). It returns nil when there is only one
// statement.
func splitStatements(sql string) []string {
	if semi := strings.IndexByte(sql, ';'); semi < 0 || skipBlank(sql, semi+1) == len(sql) {
		return nil
	}
	// A SQL-standard function body (BEGIN ATOMIC ... END) has semicolons of
	// its own. Telling those apart takes a real parser, so leave such a
	// script in one piece.
	if containsFold(sql, "atomic") {
		return nil
	}
	var parts []string
	add := func(piece string) {
		if skipBlank(piece, 0) < len(piece) {
			parts = append(parts, strings.TrimSpace(piece))
		}
	}
	start, i := 0, 0
	for i < len(sql) {
		switch c := sql[i]; {
		case c == ';':
			add(sql[start:i])
			i++
			start = i
		case c == '\'':
			// E'...' strings take backslash escapes; plain ones don't.
			escapes := i > 0 && (sql[i-1] == 'E' || sql[i-1] == 'e') && (i < 2 || !isIdent(sql[i-2]))
			i = skipQuoted(sql, i, '\'', escapes)
		case c == '"':
			i = skipQuoted(sql, i, '"', false)
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			i = skipBlank(sql, i)
		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			i = skipBlockComment(sql, i)
		case c == '$' && (i == 0 || !isIdent(sql[i-1])):
			i = skipDollarQuoted(sql, i)
		default:
			i++
		}
	}
	add(sql[start:])
	if len(parts) < 2 {
		return nil
	}
	return parts
}

// skipQuoted steps over the quoted run that opens at i. A doubled quote is
// a quote inside it.
func skipQuoted(sql string, i int, quote byte, escapes bool) int {
	i++
	for i < len(sql) {
		switch {
		case escapes && sql[i] == '\\':
			i += 2
		case sql[i] == quote:
			if i+1 < len(sql) && sql[i+1] == quote {
				i += 2
				continue
			}
			return i + 1
		default:
			i++
		}
	}
	return len(sql)
}

// skipDollarQuoted steps over $tag$ ... $tag$ if one opens at i, and over
// the dollar sign alone if not ($1 is a parameter).
func skipDollarQuoted(sql string, i int) int {
	j := i + 1
	for j < len(sql) && (isLetter(sql[j]) || j > i+1 && sql[j] >= '0' && sql[j] <= '9') {
		j++
	}
	if j >= len(sql) || sql[j] != '$' {
		return i + 1
	}
	tag := sql[i : j+1]
	end := strings.Index(sql[j+1:], tag)
	if end < 0 {
		return len(sql)
	}
	return j + 1 + end + len(tag)
}

const traceKey = "traceparent='"

// traceparent finds a sqlcommenter comment's W3C trace context,
// /*traceparent='00-<trace>-<span>-01'*/, which an instrumented app adds so
// the database's view of a query can be tied to the request that made it.
func traceparent(sql string) (traceID, spanID string, ok bool) {
	i := strings.Index(sql, traceKey)
	if i < 0 {
		return "", "", false
	}
	v := sql[i+len(traceKey):]
	// version(2) - trace(32) - span(16) - flags(2), then the closing quote.
	if len(v) < 56 || v[2] != '-' || v[35] != '-' || v[52] != '-' || v[55] != '\'' {
		return "", "", false
	}
	traceID, spanID = v[3:35], v[36:52]
	if !isHex(traceID) || !isHex(spanID) || allZero(traceID) || allZero(spanID) {
		return "", "", false
	}
	return traceID, spanID, true
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func allZero(s string) bool {
	return strings.Trim(s, "0") == ""
}

// withoutTraceComment drops the comment that carries the trace context, so
// a statement groups with the same statement sent without one.
func withoutTraceComment(sql string) string {
	i := strings.Index(sql, traceKey)
	if i < 0 {
		return sql
	}
	open := strings.LastIndex(sql[:i], "/*")
	end := strings.Index(sql[i:], "*/")
	if open < 0 || end < 0 {
		return sql
	}
	return strings.TrimSpace(sql[:open] + sql[i+end+2:])
}

var rePassword = regexp.MustCompile(`(?i)(\bpassword\s*(?:=\s*)?)(?:E'(?:[^'\\]|''|\\.)*'|'(?:[^']|'')*')`)

// redact hides the password in ALTER ROLE ... PASSWORD '...' and its
// relatives. It is the one place SQL text carries a credential by design.
func redact(sql string) string {
	if !containsFold(sql, "password") {
		return sql
	}
	return rePassword.ReplaceAllString(sql, "${1}'***'")
}

// containsFold reports whether s contains word, which must be lower case,
// in any letter case. It allocates nothing.
func containsFold(s, word string) bool {
	for i := 0; i+len(word) <= len(s); i++ {
		if s[i]|0x20 == word[0] && strings.EqualFold(s[i:i+len(word)], word) {
			return true
		}
	}
	return false
}

// closeQuote ends a string literal that a cut-off statement left open, so
// the fingerprint still sees it as one literal.
func closeQuote(sql string) string {
	if strings.Count(sql, "'")%2 == 1 {
		return sql + "'"
	}
	return sql
}
