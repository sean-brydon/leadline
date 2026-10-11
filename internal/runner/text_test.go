package runner

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestStripANSI(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain", "listening on :3000", "listening on :3000"},
		{"colour", "\x1b[32m✓\x1b[39m Ready in \x1b[1m1.2s\x1b[0m", "✓ Ready in 1.2s"},
		{"256 and true colour", "\x1b[38;5;196mred\x1b[0m \x1b[38;2;1;2;3mrgb\x1b[m", "red rgb"},
		{"cursor moves", "\x1b[2K\x1b[1Gdone\x1b[?25h", "done"},
		{"hyperlink", "see \x1b]8;;https://acme.example\x1b\\the docs\x1b]8;;\x1b\\ now", "see the docs now"},
		{"window title, BEL ended", "\x1b]0;acme dev\x07ready", "ready"},
		{"APC, as the provider's mark", "sent" + Mark, "sent"},
		{"charset and keypad", "\x1b(B\x1b=ok\x1b>", "ok"},
		{"tab kept, other controls dropped", "a\tb\x07c\x00", "a\tbc"},
		{"backspace takes a character back", "^D\b\bready, na\u00efve\b\b\bive", "ready, naive"},
		{"backspace at the start", "\b\bok", "ok"},
		{"cut short", "half \x1b[38;5", "half "},
		{"lone escape at the end", "tail\x1b", "tail"},
		{"not UTF-8", "caf\xe9", "caf\uFFFD"},
		{"multibyte kept", "naïve — 日本語 🚀", "naïve — 日本語 🚀"},
	} {
		if got := StripANSI([]byte(tc.in)); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestMarkFilterAtEverySplit(t *testing.T) {
	text := "one" + Mark + "\ntwo\x1b[0m" + Mark + "\n\x1b_other\x1b\\three\n"
	want := "one\ntwo\x1b[0m\n\x1b_other\x1b\\three\n"
	// However the reads fall, the terminal gets the same bytes.
	for i := 0; i <= len(text); i++ {
		for j := i; j <= len(text); j++ {
			var f markFilter
			var got []byte
			for _, part := range []string{text[:i], text[i:j], text[j:]} {
				got = append(got, f.Strip([]byte(part))...)
			}
			got = append(got, f.Flush()...)
			if string(got) != want {
				t.Fatalf("split at %d,%d: got %q", i, j, got)
			}
		}
	}
}

func TestMarkFilterHoldsNothingItNeedNot(t *testing.T) {
	var f markFilter
	in := []byte("plain \x1b[31mred\x1b[0m text\n")
	if got := f.Strip(in); string(got) != string(in) || len(f.held) != 0 {
		t.Fatalf("got %q, held %q", got, f.held)
	}
	// The start of something that turns out not to be a mark comes out with
	// the next read, whole.
	got := string(f.Strip([]byte("x\x1b_lead")))
	got += string(f.Strip([]byte("er\x1b\\y")))
	if got != "x\x1b_leader\x1b\\y" {
		t.Fatalf("got %q", got)
	}
}

// collect runs chunks through an assembler and returns its lines; a line
// the provider sent comes back as "<sent>".
func collect(chunks ...string) []string {
	var out []string
	a := assembler{emit: func(text string, sent bool) {
		if sent {
			text = "<sent>"
		}
		out = append(out, text)
	}}
	for _, c := range chunks {
		a.Write([]byte(c))
	}
	a.Close()
	return out
}

func TestAssembler(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks []string
		want   []string
	}{
		{"lines across reads", []string{"hel", "lo\nwor", "ld\n"}, []string{"hello", "world"}},
		{"a terminal's CRLF", []string{"one\r\ntwo\r\n"}, []string{"one", "two"}},
		{"last line without a newline", []string{"one\ntwo"}, []string{"one", "two"}},
		{"colour stripped", []string{"\x1b[31merror\x1b[0m: no\n"}, []string{"error: no"}},
		{"blank lines kept for the grouper", []string{"a\n\nb\n"}, []string{"a", "", "b"}},
		{"a progress bar is its last state", []string{"10%\r50%\r", "100%\r\n"}, []string{"100%"}},
		{"a spinner that erases the line", []string{"\x1b[2K\x1b[1G⠋ building", "\x1b[2K\x1b[1G⠙ building", "\x1b[2K\x1b[1G✓ built\n"}, []string{"✓ built"}},
		{"a marked line is the provider's", []string{"mine\n", "theirs" + Mark + "\n", "mine again\n"}, []string{"mine", "<sent>", "mine again"}},
		{"a mark cut by a read", []string{"theirs\x1b_lead", "line:sent\x1b\\\r\nmine\n"}, []string{"<sent>", "mine"}},
		{"full-screen programs are not lines", []string{"before\n\x1b[?1049h\x1b[2J\x1b[1;1Hmenu\nmore", " drawing\n\x1b[?10", "49lafter\n"}, []string{"before", "after"}},
	} {
		if got := collect(tc.chunks...); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestAssemblerBoundsWhatItHolds(t *testing.T) {
	// A progress bar that never ends its line must not grow without limit.
	var lines []string
	a := assembler{emit: func(text string, _ bool) { lines = append(lines, text) }}
	for i := 0; i < 5000; i++ {
		a.Write([]byte("\rdownloading " + strings.Repeat("#", i%50)))
		if len(a.buf) > maxPending+100 {
			t.Fatalf("holding %d bytes", len(a.buf))
		}
	}
	a.Write([]byte("\rdone\n"))
	if !reflect.DeepEqual(lines, []string{"done"}) {
		t.Fatalf("got %q", lines)
	}

	// One enormous line is cut, and says how long it was.
	lines = nil
	a.Write([]byte(strings.Repeat("x", 3*maxLine) + "\n"))
	if len(lines) != 1 || len(lines[0]) > maxLine+40 || !strings.HasSuffix(lines[0], "bytes)") {
		t.Fatalf("got %d lines, first %d bytes", len(lines), len(lines[0]))
	}
}

// group runs lines through a grouper and returns each record's lines joined.
func group(text string) []string {
	var out []string
	g := grouper{emit: func(_ time.Time, lines []string) { out = append(out, strings.Join(lines, "\n")) }}
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		g.Line(time.Time{}, line)
	}
	g.Flush()
	return out
}

func TestGrouper(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want []string
	}{
		{
			"a JavaScript error and its stack",
			"listening\nTypeError: x is not a function\n    at handler (/app/server.js:10:3)\n    at next (/app/node_modules/express/lib/router.js:1:1)\nGET /boom 500\n",
			[]string{"listening", "TypeError: x is not a function\n    at handler (/app/server.js:10:3)\n    at next (/app/node_modules/express/lib/router.js:1:1)", "GET /boom 500"},
		},
		{
			"a Java stack with its cause",
			"java.lang.IllegalStateException: no\n\tat acme.Orders.list(Orders.java:12)\nCaused by: java.sql.SQLException: gone\n\tat acme.Db.query(Db.java:4)\n\t... 12 more\nnext\n",
			[]string{"java.lang.IllegalStateException: no\n\tat acme.Orders.list(Orders.java:12)\nCaused by: java.sql.SQLException: gone\n\tat acme.Db.query(Db.java:4)\n\t... 12 more", "next"},
		},
		{
			"a Python traceback ends with the exception",
			"Traceback (most recent call last):\n  File \"app.py\", line 9, in orders\n    rows = db.query()\nValueError: bad id\n127.0.0.1 - - \"GET /orders\" 500\n",
			[]string{"Traceback (most recent call last):\n  File \"app.py\", line 9, in orders\n    rows = db.query()\nValueError: bad id", "127.0.0.1 - - \"GET /orders\" 500"},
		},
		{
			"logging.exception: the message, then the traceback",
			"ERROR:acme:could not list orders\nTraceback (most recent call last):\n  File \"app.py\", line 9, in orders\nKeyError: 'id'\nINFO:acme:next\n",
			[]string{"ERROR:acme:could not list orders\nTraceback (most recent call last):\n  File \"app.py\", line 9, in orders\nKeyError: 'id'", "INFO:acme:next"},
		},
		{
			"a Go panic with its goroutines",
			"2026/01/02 10:00:00 serving\npanic: runtime error: index out of range [3] with length 3\n\ngoroutine 1 [running]:\nmain.list(...)\n\t/app/main.go:12 +0x1d\nmain.main()\n\t/app/main.go:7 +0x18\nexit status 2\n",
			[]string{"2026/01/02 10:00:00 serving", "panic: runtime error: index out of range [3] with length 3\n\ngoroutine 1 [running]:\nmain.list(...)\n\t/app/main.go:12 +0x1d\nmain.main()\n\t/app/main.go:7 +0x18\nexit status 2"},
		},
		{
			"an object printed over several lines",
			"user {\n  id: 3,\n  tags: [\n    'a'\n  ]\n}\nnext\n",
			[]string{"user {\n  id: 3,\n  tags: [\n    'a'\n  ]\n}", "next"},
		},
		{
			"a banner indented alike stays separate lines",
			"   ▲ Next.js 16.0.0\n   - Local:        http://localhost:3000\n\n ✓ Starting...\n ✓ Ready in 900ms\n GET / 200 in 40ms\n GET /api 200 in 9ms\n",
			[]string{"   ▲ Next.js 16.0.0", "   - Local:        http://localhost:3000", " ✓ Starting...", " ✓ Ready in 900ms", " GET / 200 in 40ms", " GET /api 200 in 9ms"},
		},
		{
			"Node's uncaught error: the source line, then the error",
			"/app/server.js:10\n    throw new Error(\"boom\");\n    ^\n\nError: boom\n    at Object.<anonymous> (/app/server.js:10:11)\n\nNode.js v22.1.0\n",
			[]string{"/app/server.js:10\n    throw new Error(\"boom\");\n    ^\n\nError: boom\n    at Object.<anonymous> (/app/server.js:10:11)", "Node.js v22.1.0"},
		},
		{
			"a blank line ends a group that nothing continues",
			"Error: one\n    at a (a.js:1:1)\n\nError: two\n    at b (b.js:1:1)\n",
			[]string{"Error: one\n    at a (a.js:1:1)", "Error: two\n    at b (b.js:1:1)"},
		},
	} {
		if got := group(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

func TestGrouperBoundsAGroup(t *testing.T) {
	var sizes []int
	g := grouper{emit: func(_ time.Time, lines []string) { sizes = append(sizes, len(lines)) }}
	g.Line(time.Time{}, "Error: deep")
	for i := 0; i < 450; i++ {
		g.Line(time.Time{}, "    at frame (x.js:1:1)")
	}
	g.Flush()
	if len(sizes) < 3 || sizes[0] != maxGroupLines {
		t.Fatalf("got groups of %v lines", sizes)
	}
}

func TestClassifyLevels(t *testing.T) {
	for _, tc := range []struct {
		line   string
		stream Stream
		want   int32
	}{
		{"listening on http://127.0.0.1:3000", Stdout, sevInfo},
		{"ERROR could not connect", Stdout, sevError},
		{"WARN  slow query", Stdout, sevWarn},
		{"[warn] deprecated option", Stdout, sevWarn},
		{"2026-01-02T10:00:00.123Z ERROR db: gone", Stdout, sevError},
		{"2026-01-02 10:00:00,123 - acme - WARNING - careful", Stdout, sevWarn},
		{"10:00:01 [debug] cache miss", Stdout, sevDebug},
		{"error: Cannot find module 'x'", Stdout, sevError},
		{"error Command failed with exit code 1.", Stdout, sevError},
		{"warning package.json: No license field", Stdout, sevWarn},
		{"warn  - ./pages/index.js", Stdout, sevWarn},
		{"WARNING:werkzeug: * Debugger is active!", Stderr, sevWarn},
		{"INFO:     127.0.0.1:5000 - \"GET / HTTP/1.1\" 200 OK", Stderr, sevInfo},
		{"time=2026-01-02T10:00:00Z level=WARN msg=\"slow\" took=3s", Stderr, sevWarn},
		{"ts=1 level=info msg=\"error budget fine\"", Stdout, sevInfo},
		{"I, [2026-01-02T10:00:00 #1]  INFO -- : Started GET", Stdout, sevInfo},
		{"TypeError: x is not a function", Stdout, sevError},
		{"Error [ERR_MODULE_NOT_FOUND]: Cannot find package", Stderr, sevError},
		{"java.lang.NullPointerException: no", Stdout, sevError},
		{"Traceback (most recent call last):", Stderr, sevError},
		{"panic: runtime error: nil map", Stderr, sevFatal},
		{"fatal error: all goroutines are asleep - deadlock!", Stderr, sevFatal},
		{" ⨯ Failed to compile", Terminal, sevError},
		{" ⚠ Port 3000 is in use", Terminal, sevWarn},
		{"npm ERR! code ELIFECYCLE", Stderr, sevError},
		{"npm warn deprecated inflight@1.0.6", Stderr, sevWarn},
		{"(node:123) ExperimentalWarning: CommonJS module loading ES Module", Stderr, sevWarn},
		{"(node:123) [DEP0040] DeprecationWarning: The punycode module", Stderr, sevWarn},
		// stderr is a hint, not a verdict: Go's and Python's loggers print
		// everything there.
		{"2026/01/02 10:00:00 serving on :8080", Stderr, sevInfo},
		{"compiled successfully", Stderr, sevInfo},
		// ...and it tips a line that only mentions a failure.
		{"connection to database failed", Stderr, sevError},
		{"connection to database failed", Stdout, sevInfo},
		{"compiled with 0 errors", Stdout, sevInfo},
		{"this API is deprecated", Stderr, sevWarn},
		// A capitalised word that starts a sentence is not a level.
		{"Error handling is set up", Stdout, sevInfo},
		{"Info about the build follows", Stdout, sevInfo},
	} {
		if got := Classify([]string{tc.line}, tc.stream).Severity; got != tc.want {
			t.Errorf("%q on %s: got %s, want %s", tc.line, tc.stream, severityText(got), severityText(tc.want))
		}
	}
}

func TestClassifyStackTraceIsAnError(t *testing.T) {
	e := Classify([]string{"request failed: GET /boom", "    at handler (/app/server.js:10:3)"}, Stdout)
	if e.Severity != sevError {
		t.Errorf("a record with a stack: got %s", severityText(e.Severity))
	}
	if e.Body != "request failed: GET /boom\n    at handler (/app/server.js:10:3)" {
		t.Errorf("body: %q", e.Body)
	}
	e = Classify([]string{"user {", "  id: 3", "}"}, Stdout)
	if e.Severity != sevInfo {
		t.Errorf("a printed object: got %s", severityText(e.Severity))
	}
}

func TestClassifyJSON(t *testing.T) {
	e := Classify([]string{`{"level":50,"time":1767348000000,"pid":12,"msg":"order failed","order_id":42,"ok":false,"err":{"type":"Error"},"trace_id":"0af7651916cd43dd8448eb211c80319c","span_id":"b7ad6b7169203331"}`}, Stdout)
	if e.Severity != sevError || e.Body != "order failed" {
		t.Fatalf("got %s %q", severityText(e.Severity), e.Body)
	}
	if got := e.Attrs["order_id"]; got != int64(42) {
		t.Errorf("order_id: %#v", got)
	}
	if got := e.Attrs["ok"]; got != false {
		t.Errorf("ok: %#v", got)
	}
	if got := e.Attrs["err"]; got != `{"type":"Error"}` {
		t.Errorf("err: %#v", got)
	}
	for _, gone := range []string{"level", "msg", "trace_id", "span_id"} {
		if _, ok := e.Attrs[gone]; ok {
			t.Errorf("%s is both a field of the record and an attribute", gone)
		}
	}
	if len(e.TraceID) != 16 || len(e.SpanID) != 8 {
		t.Errorf("ids: %x %x", e.TraceID, e.SpanID)
	}

	for _, tc := range []struct {
		line string
		want int32
		body string
	}{
		{`{"level":"warn","message":"slow"}`, sevWarn, "slow"},
		{`{"severity":"ERROR","message":"gone"}`, sevError, "gone"},
		{`{"level":"info","event":"started","logger":"acme"}`, sevInfo, "started"},
		{`{"level":30,"msg":"pino info"}`, sevInfo, "pino info"},
		{`{"level":60,"msg":"pino fatal"}`, sevFatal, "pino fatal"},
		{`{"level":"debug"}`, sevDebug, `{"level":"debug"}`},
		{`{"a":1}`, sevInfo, `{"a":1}`},
		{`{"trace_id":"00000000000000000000000000000000","msg":"none"}`, sevInfo, "none"},
	} {
		e := Classify([]string{tc.line}, Stdout)
		if e.Severity != tc.want || e.Body != tc.body {
			t.Errorf("%s: got %s %q", tc.line, severityText(e.Severity), e.Body)
		}
		if strings.Contains(tc.line, "0000000000000000") && e.TraceID != nil {
			t.Errorf("an id of zeros is no id: %x", e.TraceID)
		}
	}
	// Looks like JSON, is not: a line like any other.
	if e := Classify([]string{`{ not json }`}, Stderr); e.Body != `{ not json }` || e.Attrs != nil {
		t.Errorf("got %#v", e)
	}
}
