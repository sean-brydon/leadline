package runner

import (
	"regexp"
	"strings"
	"time"
)

const (
	maxGroupLines = 200
	maxGroupBytes = 64 << 10
)

type groupKind uint8

const (
	plainGroup groupKind = iota
	// A Python traceback: indented frames, then one line at the margin that
	// names the exception and ends it.
	pythonGroup
	// A Go panic or fatal error: goroutine headers, function lines at the
	// margin, file lines behind a tab, and blank lines between goroutines.
	goGroup
)

var (
	pythonHead = regexp.MustCompile(`^\s*Traceback \(most recent call last\):`)
	goHead     = regexp.MustCompile(`^(panic: |fatal error: |goroutine \d+ \[)`)
	goFrame    = regexp.MustCompile(`^(goroutine \d+ \[|created by |\[signal |panic: |\t|\.\.\.|[\w./*\-\[\]{}·]+\(.*\)$|.+\(0x[0-9a-f]+.*\)$|exit status \d+$)`)
	caretLine  = regexp.MustCompile(`^\s*\^+\s*$`)
	closerLine = regexp.MustCompile(`^\s*[\]})]+[,;]?$`)
	// Lines at the margin that still belong to the stack trace above them.
	traceTail = regexp.MustCompile(`^(Caused by:|Suppressed:|\.\.\. \d+ more|\s*\[cause\]:)`)
)

// grouper joins the lines of one event, such as an error and its stack
// trace or an object printed over several lines, into one record. A line
// belongs to the one before it when it is indented further than the line
// that started the group; that keeps a banner whose every line is indented
// alike as separate lines.
//
// A group cannot be known to be finished until the next line arrives, so
// the owner calls Flush when the stream has been quiet for a moment.
type grouper struct {
	emit func(at time.Time, lines []string)

	at     time.Time
	lines  []string
	bytes  int
	kind   groupKind
	indent int
	// Blank lines seen since the last line of the group. They join it only
	// if what follows does.
	blanks int
	// The group's last line was a row of carets under a source line, as Node
	// prints before an uncaught error: the error itself follows a blank line.
	caret bool
}

// Line takes the next line of the stream.
func (g *grouper) Line(at time.Time, text string) {
	if strings.TrimSpace(text) == "" {
		if len(g.lines) > 0 {
			g.blanks++
		}
		return
	}
	if len(g.lines) > 0 && g.continues(text) {
		for ; g.blanks > 0; g.blanks-- {
			g.add("")
		}
		g.add(text)
		g.caret = caretLine.MatchString(text)
		if g.kind == pythonGroup && indentOf(text) <= g.indent && !pythonHead.MatchString(text) {
			// The exception's own line: the traceback is complete.
			g.Flush()
		} else if len(g.lines) >= maxGroupLines || g.bytes >= maxGroupBytes {
			g.Flush()
		}
		return
	}
	g.Flush()
	g.at, g.indent = at, indentOf(text)
	g.kind = plainGroup
	switch {
	case pythonHead.MatchString(text):
		g.kind = pythonGroup
	case goHead.MatchString(text):
		g.kind = goGroup
	}
	g.add(text)
}

// Flush hands over the group in progress, if there is one.
func (g *grouper) Flush() {
	if len(g.lines) > 0 {
		g.emit(g.at, g.lines)
	}
	g.lines, g.bytes, g.blanks, g.caret, g.kind = nil, 0, 0, false, plainGroup
}

func (g *grouper) add(text string) {
	g.lines = append(g.lines, text)
	g.bytes += len(text) + 1
}

func (g *grouper) continues(text string) bool {
	switch g.kind {
	case pythonGroup:
		// Everything up to and including the first line back at the margin.
		return true
	case goGroup:
		return goFrame.MatchString(text)
	}
	if g.caret {
		return true
	}
	if indentOf(text) > g.indent {
		return true
	}
	if len(g.lines) == 1 && g.blanks == 0 && pythonHead.MatchString(text) {
		// logging.exception prints its message, then the traceback.
		g.kind = pythonGroup
		g.indent = indentOf(text)
		return true
	}
	if len(g.lines) > 1 && g.blanks == 0 {
		if indentOf(text) == g.indent && closerLine.MatchString(text) {
			return true
		}
		return traceTail.MatchString(text)
	}
	return false
}

// indentOf counts leading white space, a tab as one step like a space: the
// comparison is only ever "further in than the first line".
func indentOf(s string) int {
	return len(s) - len(strings.TrimLeft(s, " \t"))
}
