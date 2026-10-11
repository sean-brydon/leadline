package runner

import "unicode/utf8"

const esc = 0x1b

// StripANSI returns what a terminal would have shown as text: no colours,
// cursor moves, window titles or hyperlinks, and no control characters but
// the tab. A backspace takes back the character before it, as it does on
// screen (a terminal echoes Ctrl-D as "^D" and two of them). Bytes that are
// not UTF-8 come out as U+FFFD.
func StripANSI(b []byte) string {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == esc:
			i = skipEscape(b, i)
		case c == '\t':
			out = append(out, c)
			i++
		case c == '\b':
			_, size := utf8.DecodeLastRune(out)
			out = out[:len(out)-size]
			i++
		case c < 0x20 || c == 0x7f:
			i++
		case c < utf8.RuneSelf:
			out = append(out, c)
			i++
		default:
			r, size := utf8.DecodeRune(b[i:])
			// C1 controls, which a few programs still send as two bytes.
			if r >= 0x80 && r < 0xa0 && size > 1 {
				i += size
				continue
			}
			out = utf8.AppendRune(out, r)
			i += size
		}
	}
	return string(out)
}

// skipEscape returns the index after the escape sequence that starts at
// b[i]. A sequence cut short by the end of b is skipped to the end.
func skipEscape(b []byte, i int) int {
	if i+1 >= len(b) {
		return len(b)
	}
	switch b[i+1] {
	case '[': // CSI: parameters and intermediates, then one final byte.
		for j := i + 2; j < len(b); j++ {
			if b[j] >= 0x40 && b[j] <= 0x7e {
				return j + 1
			}
			if b[j] < 0x20 || b[j] > 0x7e {
				// Not a sequence after all; drop the introducer alone.
				return j
			}
		}
		return len(b)
	case ']', 'P', '_', '^', 'X': // OSC, DCS, APC, PM, SOS: a string up to ST or BEL.
		for j := i + 2; j < len(b); j++ {
			if b[j] == 0x07 {
				return j + 1
			}
			if b[j] == esc && j+1 < len(b) && b[j+1] == '\\' {
				return j + 2
			}
		}
		return len(b)
	case '(', ')', '*', '+', '-', '.', '/', '#', '%', ' ': // one more byte follows.
		if i+2 < len(b) {
			return i + 3
		}
		return len(b)
	}
	return i + 2
}
