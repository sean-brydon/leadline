package runner

import (
	"bytes"
	"fmt"
	"strings"
)

// Stream says where a line was printed.
type Stream uint8

const (
	// Terminal is a pseudo-terminal, where stdout and stderr are one stream.
	Terminal Stream = iota
	Stdout
	Stderr
)

func (s Stream) String() string {
	switch s {
	case Stdout:
		return "stdout"
	case Stderr:
		return "stderr"
	}
	return "tty"
}

const (
	// A line longer than this is cut. Minified bundles and base64 blobs get
	// printed by accident; none of it is worth a megabyte per record.
	maxLine = 16 << 10
	// What an unfinished line may grow to before it is dealt with anyway.
	maxPending = 64 << 10
)

// The ways a program repaints the line it is on: a spinner or a progress bar
// writes these again and again, and only the last state is a line.
var lineResets = [][]byte{
	[]byte("\r"),
	[]byte("\x1b[2K"),
	[]byte("\x1b[G"),
	[]byte("\x1b[1G"),
	[]byte("\x1b[0G"),
}

// Entering and leaving the alternate screen: an editor, a pager or a
// full-screen dashboard. What is drawn there is a picture, not lines.
var (
	altEnter = [][]byte{[]byte("\x1b[?1049h"), []byte("\x1b[?1047h"), []byte("\x1b[?47h")}
	altLeave = [][]byte{[]byte("\x1b[?1049l"), []byte("\x1b[?1047l"), []byte("\x1b[?47l")}
)

// assembler turns one stream's chunks into lines of plain text.
type assembler struct {
	buf []byte
	alt bool
	// emit gets each finished line. sent is true for a line the Node provider
	// has already sent (see Mark); its text is not needed and is empty.
	emit func(text string, sent bool)
}

func (a *assembler) Write(p []byte) {
	a.buf = append(a.buf, p...)
	for {
		if a.alt {
			i, n := firstOf(a.buf, altLeave)
			if i < 0 {
				// Keep a tail, in case the sequence was cut by the read.
				if len(a.buf) > 16 {
					a.buf = append(a.buf[:0], a.buf[len(a.buf)-16:]...)
				}
				return
			}
			a.alt = false
			a.buf = a.buf[i+n:]
			continue
		}
		enter, n := firstOf(a.buf, altEnter)
		nl := bytes.IndexByte(a.buf, '\n')
		if enter >= 0 && (nl < 0 || enter < nl) {
			// A line the screen switch cut off was never finished.
			a.alt = true
			a.buf = a.buf[enter+n:]
			continue
		}
		if nl < 0 {
			break
		}
		a.line(a.buf[:nl])
		a.buf = a.buf[nl+1:]
	}
	if len(a.buf) > maxPending {
		// No newline in sight: a progress bar that never ends its line, or
		// one very long line. Keep what the line currently shows.
		a.buf = append(a.buf[:0], current(a.buf)...)
		if len(a.buf) > maxPending {
			a.line(a.buf)
			a.buf = a.buf[:0]
		}
	}
	if len(a.buf) == 0 && cap(a.buf) > maxPending {
		a.buf = nil
	}
}

// Close hands over a last line that had no newline.
func (a *assembler) Close() {
	if !a.alt && len(a.buf) > 0 {
		a.line(a.buf)
	}
	a.buf = nil
}

func (a *assembler) line(raw []byte) {
	if bytes.Contains(raw, mark) {
		a.emit("", true)
		return
	}
	text := strings.TrimRight(StripANSI(current(raw)), " \t")
	if len(text) > maxLine {
		text = fmt.Sprintf("%s… (%d bytes)", strings.ToValidUTF8(text[:maxLine], ""), len(text))
	}
	a.emit(text, false)
}

// current returns the part of a raw line that is still showing: what came
// after the last time the program went back to the start of the line.
func current(raw []byte) []byte {
	raw = bytes.TrimRight(raw, "\r")
	start := 0
	for _, reset := range lineResets {
		if i := bytes.LastIndex(raw, reset); i >= 0 && i+len(reset) > start {
			start = i + len(reset)
		}
	}
	return raw[start:]
}

func firstOf(b []byte, seqs [][]byte) (index, length int) {
	index = -1
	for _, seq := range seqs {
		if i := bytes.Index(b, seq); i >= 0 && (index < 0 || i < index) {
			index, length = i, len(seq)
		}
	}
	return index, length
}
