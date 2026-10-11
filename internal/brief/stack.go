package brief

import (
	"regexp"
	"strconv"
	"strings"
)

// Frame is one line of a stack trace, read into its parts.
type Frame struct {
	Function string `json:"function,omitempty"`
	File     string `json:"file"`
	Line     int    `json:"line,omitempty"`
	// App says the frame is in the application's own code, as against a
	// library's, the framework's or the runtime's. It is a reading of the
	// path, and a frame it can't place counts as the application's.
	App bool `json:"app"`
}

// How a frame is written, language by language. Each has the file and the
// line; the function is where the language puts it.
var (
	// at handle (/srv/app/src/checkout.ts:48:13)    at async /srv/app/x.js:3:1
	// The line is taken apart by hand (jsFrame): a path may hold brackets.
	reJSAt  = regexp.MustCompile(`^\s*at (?:async )?(.+)$`)
	reJSLoc = regexp.MustCompile(`^(.+?):(\d+)(?::\d+)?$`)
	// File "/srv/app/views.py", line 31, in checkout
	rePyFrame = regexp.MustCompile(`^\s*File "(.+?)", line (\d+)(?:, in (.+))?$`)
	// \t/srv/app/checkout.go:48 +0x1d4, under a line naming the function
	reGoFrame = regexp.MustCompile(`^\s+(\S+\.go):(\d+)(?: \+0x[0-9a-f]+)?$`)
	// at com.acme.Checkout.charge(Checkout.java:48)
	reJavaFrame = regexp.MustCompile(`^\s*at ([\w.$<>]+)\(([\w$]+\.(?:java|kt|scala|groovy)):(\d+)\)$`)
	// /srv/app/checkout.rb:48:in `charge'    from /srv/app/x.rb:3:in 'y'
	reRubyFrame = regexp.MustCompile("^\\s*(?:from )?(\\S+\\.rb):(\\d+):in [`'](.+?)'$")
	// #0 /srv/app/Checkout.php(48): App\Checkout->charge()
	rePHPFrame = regexp.MustCompile(`^\s*#\d+ (\S+\.php)\((\d+)\): (.+)$`)
)

// The parts of a path that mean the code is someone else's.
var libraryPaths = []string{
	"node_modules/", "node:", "internal/process/", "internal/modules/", "<anonymous>", "webpack-internal:", "/.pnpm/", "/next/dist/",
	"site-packages/", "dist-packages/", "/lib/python", "<frozen ", "/usr/lib/", "/usr/local/lib/",
	"/go/src/", "/libexec/src/", "/pkg/mod/", "/vendor/", "/gems/", "/.cargo/", "/rustc/", "/.m2/",
}

// And the packages that are, where a frame has no path to read (Java).
var libraryPackages = []string{"java.", "javax.", "jakarta.", "jdk.", "sun.", "com.sun.", "kotlin.", "kotlinx.", "scala.", "org.springframework.", "org.apache.", "org.hibernate.", "io.netty.", "reactor."}

func isLibrary(file, function string) bool {
	for _, p := range libraryPaths {
		if strings.Contains(file, p) {
			return true
		}
	}
	if !strings.ContainsAny(file, "/\\") {
		for _, p := range libraryPackages {
			if strings.HasPrefix(function, p) {
				return true
			}
		}
	}
	return false
}

// ParseStack reads a stack trace as text. head is what stands before the
// first frame (the exception's own line, as a rule); a text with no frame
// in it comes back whole as head.
func ParseStack(stack string) (head string, frames []Frame) {
	lines := strings.Split(strings.ReplaceAll(stack, "\r\n", "\n"), "\n")
	var before []string
	for i, l := range lines {
		f, ok := parseFrame(l, lines, i)
		if !ok {
			if len(frames) == 0 {
				before = append(before, l)
			}
			continue
		}
		f.App = !isLibrary(f.File, f.Function)
		frames = append(frames, f)
	}
	if len(frames) > 0 {
		// A Go frame's function line is not part of the head, nor is
		// Python's "Traceback (most recent call last):".
		kept := before[:0]
		for i, l := range before {
			t := strings.TrimSpace(l)
			if t == "" || strings.HasPrefix(t, "Traceback (most recent call last)") || strings.HasPrefix(t, "goroutine ") {
				continue
			}
			if i == len(before)-1 && reGoFrame.MatchString(lines[len(before)]) {
				continue
			}
			kept = append(kept, t)
		}
		before = kept
	}
	return strings.TrimSpace(strings.Join(before, "\n")), frames
}

func parseFrame(l string, lines []string, i int) (Frame, bool) {
	num := func(s string) int { n, _ := strconv.Atoi(s); return n }
	if m := reJavaFrame.FindStringSubmatch(l); m != nil {
		return Frame{Function: m[1], File: m[2], Line: num(m[3])}, true
	}
	if f, ok := jsFrame(l); ok {
		return f, true
	}
	if m := rePyFrame.FindStringSubmatch(l); m != nil {
		return Frame{File: m[1], Line: num(m[2]), Function: m[3]}, true
	}
	if m := reRubyFrame.FindStringSubmatch(l); m != nil {
		return Frame{File: m[1], Line: num(m[2]), Function: m[3]}, true
	}
	if m := rePHPFrame.FindStringSubmatch(l); m != nil {
		return Frame{File: m[1], Line: num(m[2]), Function: m[3]}, true
	}
	if m := reGoFrame.FindStringSubmatch(l); m != nil {
		fn := ""
		if i > 0 {
			fn = strings.TrimSpace(lines[i-1])
			if j := strings.LastIndexByte(fn, '('); j > 0 {
				fn = fn[:j]
			}
		}
		return Frame{Function: fn, File: m[1], Line: num(m[2])}, true
	}
	return Frame{}, false
}

// jsFrame reads a JavaScript frame: "at fn (file:line:col)" or "at
// file:line:col".
func jsFrame(l string) (Frame, bool) {
	m := reJSAt.FindStringSubmatch(l)
	if m == nil {
		return Frame{}, false
	}
	fn, loc := "", m[1]
	if i := strings.Index(loc, " ("); i > 0 && strings.HasSuffix(loc, ")") {
		fn, loc = loc[:i], loc[i+2:len(loc)-1]
	}
	at := reJSLoc.FindStringSubmatch(loc)
	if at == nil {
		return Frame{}, false
	}
	line, _ := strconv.Atoi(at[2])
	return Frame{Function: fn, File: at[1], Line: line}, true
}

// frameText writes a frame on one line, the same for every language.
func frameText(f Frame) string {
	at := f.File
	if f.Line > 0 {
		at += ":" + strconv.Itoa(f.Line)
	}
	if f.Function == "" {
		return clipName(at)
	}
	return clipName(f.Function + " (" + at + ")")
}

// codeFromAttrs reads the attributes that name a file: OpenTelemetry's
// code.* (both spellings of the convention) and Next.js's "next.file".
func codeFromAttrs(attrs map[string]any, what string) []CodeRef {
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := attrs[k].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	num := func(keys ...string) int {
		for _, k := range keys {
			switch v := attrs[k].(type) {
			case float64:
				return int(v)
			case string:
				if n, err := strconv.Atoi(v); err == nil {
					return n
				}
			}
		}
		return 0
	}
	var out []CodeRef
	if file := str("code.file.path", "code.filepath"); file != "" {
		out = append(out, CodeRef{File: file, Line: num("code.line.number", "code.lineno"),
			Function: str("code.function.name", "code.function"), From: "`code.filepath` on " + what})
	}
	if file := str("next.file"); file != "" {
		out = append(out, CodeRef{File: file, From: "`next.file` on " + what})
	}
	return out
}

// codeFromFrames is the application's own first frame, as a place to start.
func codeFromFrames(frames []Frame) []CodeRef {
	for _, f := range frames {
		if f.App {
			return []CodeRef{{File: f.File, Line: f.Line, Function: f.Function, From: "the first of the application's own frames in the stack trace"}}
		}
	}
	return nil
}
