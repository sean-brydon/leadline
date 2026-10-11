package runner

import "bytes"

// Mark is what @leadline/node writes before the newline of every console
// line it has itself sent to the daemon, when it runs under `leadline run`.
// It is an APC string, which a terminal shows as nothing. The runner takes
// it out of what the terminal gets and skips the lines that carry it, so a
// line is in Leadline once, as the provider's record with its trace id.
//
// packages/node/src/run.cts has the same bytes.
const Mark = "\x1b_leadline:sent\x1b\\"

var mark = []byte(Mark)

// markFilter removes marks from a stream of chunks. A mark cut in two by a
// read is held until the rest arrives; a terminal shows the start of an APC
// string as nothing either, so holding it changes nothing on screen.
type markFilter struct {
	held []byte
}

// Strip returns p without marks. The result may share p's memory and is
// only good until the next call.
func (f *markFilter) Strip(p []byte) []byte {
	if len(f.held) == 0 && bytes.IndexByte(p, esc) < 0 {
		return p
	}
	data := p
	if len(f.held) > 0 {
		data = append(f.held, p...)
		f.held = nil
	}
	var out []byte
	for {
		i := bytes.Index(data, mark)
		if i < 0 {
			break
		}
		out = append(out, data[:i]...)
		data = data[i+len(mark):]
	}
	// The longest tail that could still become a mark stays behind.
	keep := len(data)
	from := max(0, len(data)-(len(mark)-1))
	for i := from; i < len(data); i++ {
		if data[i] == esc && bytes.HasPrefix(mark, data[i:]) {
			keep = i
			break
		}
	}
	if keep < len(data) {
		f.held = append([]byte(nil), data[keep:]...)
		data = data[:keep]
	}
	if out == nil {
		return data
	}
	return append(out, data...)
}

// Flush returns what was held back, at the end of the stream.
func (f *markFilter) Flush() []byte {
	held := f.held
	f.held = nil
	return held
}
