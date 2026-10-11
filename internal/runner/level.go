package runner

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
)

// The OTLP severity numbers Leadline's levels start at.
const (
	sevTrace int32 = 1
	sevDebug int32 = 5
	sevInfo  int32 = 9
	sevWarn  int32 = 13
	sevError int32 = 17
	sevFatal int32 = 21
)

func severityText(n int32) string {
	switch {
	case n >= sevFatal:
		return "FATAL"
	case n >= sevError:
		return "ERROR"
	case n >= sevWarn:
		return "WARN"
	case n >= sevInfo:
		return "INFO"
	case n >= sevDebug:
		return "DEBUG"
	}
	return "TRACE"
}

var levelWords = map[string]int32{
	"trace": sevTrace, "verbose": sevTrace, "silly": sevTrace,
	"debug": sevDebug, "dbg": sevDebug,
	"info": sevInfo, "information": sevInfo, "notice": sevInfo, "log": sevInfo,
	"warn": sevWarn, "warning": sevWarn, "wrn": sevWarn,
	"error": sevError, "err": sevError, "severe": sevError,
	"fatal": sevFatal, "critical": sevFatal, "crit": sevFatal, "panic": sevFatal, "emerg": sevFatal, "alert": sevFatal,
}

// Entry is one captured event, ready to become a log record.
type Entry struct {
	Body     string
	Severity int32
	// Fields of a JSON log line other than the level and the message.
	Attrs map[string]any
	// Set when a JSON log line carried them, as OpenTelemetry's log
	// correlation writes them.
	TraceID, SpanID []byte
}

const (
	words = `trace|debug|info|notice|warn|warning|error|err|fatal|critical|panic`
	stamp = `[\[(]?\d[\d\-:.,/TZ+ ]{6,34}[\])]?`
)

var (
	// level=warn, lvl=error, severity="INFO": logfmt, and Go's slog.
	logfmtLevel = regexp.MustCompile(`(?i)(?:^|[\s,;])(?:level|lvl|severity|loglevel)=["']?([a-z]+)`)
	// An upper-case level word near the start: "2026-01-02 ERROR ...",
	// "[WARN]", "WARNING:root:...", "I, [...] INFO -- :".
	upperLevel = regexp.MustCompile(`(?:^|[^A-Za-z0-9_])(TRACE|DEBUG|INFO|NOTICE|WARN|WARNING|ERROR|ERR|SEVERE|FATAL|CRITICAL|CRIT|PANIC)(?:[^A-Za-z0-9_]|$)`)
	// A level word that opens the line, after an optional timestamp, set off
	// by brackets or punctuation: "Error: ...", "[warn] ...", "warn  - ...",
	// "12:00:01 [info] ...".
	leadLevel = regexp.MustCompile(`(?i)^\s*(?:` + stamp + `\s+)?(?:[\[(<](` + words + `)[\])>]|(` + words + `)(?::|\s+-\s|\s{2,}|\t))`)
	// The same word in lower case with only a space after it, as yarn and
	// eslint print it ("error Command failed"). Capitalised it is more often
	// the start of a sentence.
	lowerLevel = regexp.MustCompile(`^\s*(?:` + stamp + `\s+)?(` + words + `)\s`)
	// An exception's own first line, in most languages.
	exceptionLine = regexp.MustCompile(`^\s*(?:Uncaught |Unhandled |Exception in thread .* )?[\w.$]*(?:Error|Exception)(?::| \[|$)`)
	stackFrame    = regexp.MustCompile(`(?m)^\s+(?:at .+|File ".+", line \d+.*)$`)
	nodeWarning   = regexp.MustCompile(`^\(node:\d+\) (?:\[\w+\] )?\w*Warning:`)
	weakError     = regexp.MustCompile(`(?i)\b(?:error|errors|failed|failure|exception|fatal|panic|cannot|can't|unable to|refused|denied|not found|no such)\b`)
	weakWarn      = regexp.MustCompile(`(?i)\b(?:warn|warning|deprecated|deprecation)\b`)
)

// Marks dev tools print in place of a level word.
var symbolLevels = []struct {
	prefix string
	level  int32
}{
	{"⨯", sevError}, {"✖", sevError}, {"✘", sevError}, {"✗", sevError}, {"×", sevError},
	{"⚠", sevWarn},
	{"npm ERR!", sevError}, {"npm error", sevError},
	{"npm WARN", sevWarn}, {"npm warn", sevWarn},
}

// Classify settles the level of a captured event and, for a JSON log line,
// takes it apart. Where it was printed is a hint and not a verdict: plenty
// of programs log everything to stderr, so stderr alone makes nothing an
// error, but it tips a line that only mentions a failure.
func Classify(lines []string, stream Stream) Entry {
	body := strings.Join(lines, "\n")
	if len(lines) == 1 {
		if e, ok := classifyJSON(lines[0]); ok {
			return e
		}
	}
	return Entry{Body: body, Severity: textLevel(lines, body, stream)}
}

func textLevel(lines []string, body string, stream Stream) int32 {
	head := lines[0]
	if len(head) > 200 {
		head = head[:200]
	}
	trimmed := strings.TrimSpace(head)

	if m := logfmtLevel.FindStringSubmatch(head); m != nil {
		if n, ok := levelWords[strings.ToLower(m[1])]; ok {
			return n
		}
	}
	for _, s := range symbolLevels {
		if strings.HasPrefix(trimmed, s.prefix) {
			return s.level
		}
	}
	switch {
	case strings.HasPrefix(trimmed, "panic:"), strings.HasPrefix(trimmed, "fatal error:"):
		return sevFatal
	case strings.HasPrefix(trimmed, "Traceback (most recent call last):"),
		strings.HasPrefix(trimmed, "goroutine ") && strings.Contains(trimmed, " ["):
		return sevError
	case nodeWarning.MatchString(trimmed):
		return sevWarn
	}
	if m := leadLevel.FindStringSubmatch(head); m != nil {
		return levelWords[strings.ToLower(m[1]+m[2])]
	}
	if m := lowerLevel.FindStringSubmatch(head); m != nil {
		return levelWords[m[1]]
	}
	// Only near the start: further in, an upper-case word is the message's.
	near := head
	if len(near) > 80 {
		near = near[:80]
	}
	if m := upperLevel.FindStringSubmatch(near); m != nil {
		return levelWords[strings.ToLower(m[1])]
	}
	if exceptionLine.MatchString(head) {
		return sevError
	}
	// An event that carries a stack trace is an error wherever its first
	// line leaves that open, and so is a traceback behind a plain message.
	if len(lines) > 1 && (stackFrame.MatchString(body) || strings.Contains(body, "\nTraceback (most recent call last):")) {
		return sevError
	}
	if stream == Stderr {
		switch {
		case weakError.MatchString(head):
			return sevError
		case weakWarn.MatchString(head):
			return sevWarn
		}
	}
	return sevInfo
}

var (
	jsonLevelKeys = []string{"level", "severity", "lvl", "levelname", "log.level", "loglevel", "@l"}
	jsonBodyKeys  = []string{"msg", "message", "event", "@m"}
	jsonTraceKeys = []string{"trace_id", "traceId", "trace.id", "otelTraceID"}
	jsonSpanKeys  = []string{"span_id", "spanId", "span.id", "otelSpanID"}
)

const (
	maxJSONAttrs     = 40
	maxJSONAttrValue = 4 << 10
)

// classifyJSON reads a structured log line: one JSON object on one line, as
// pino, bunyan, zap, slog, structlog and the rest write them.
func classifyJSON(line string) (Entry, bool) {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return Entry{}, false
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil || dec.More() {
		return Entry{}, false
	}

	e := Entry{Body: trimmed, Severity: sevInfo}
	used := map[string]bool{}
	for _, key := range jsonLevelKeys {
		if n, ok := jsonLevel(obj[key]); ok {
			e.Severity = n
			used[key] = true
			break
		}
	}
	for _, key := range jsonBodyKeys {
		if s, ok := obj[key].(string); ok && strings.TrimSpace(s) != "" {
			e.Body = s
			used[key] = true
			break
		}
	}
	if !anyUsed(used, jsonBodyKeys) {
		// No message to lift out: the line is the body, whole, and repeating
		// its fields as attributes would only double it.
		e.TraceID, e.SpanID = jsonID(obj, jsonTraceKeys, 16, nil), jsonID(obj, jsonSpanKeys, 8, nil)
		return e, true
	}
	e.TraceID = jsonID(obj, jsonTraceKeys, 16, used)
	e.SpanID = jsonID(obj, jsonSpanKeys, 8, used)

	e.Attrs = map[string]any{}
	for key, value := range obj {
		if used[key] || len(e.Attrs) >= maxJSONAttrs {
			continue
		}
		switch v := value.(type) {
		case nil:
		case string:
			e.Attrs[key] = clip(v)
		case bool:
			e.Attrs[key] = v
		case json.Number:
			if n, err := v.Int64(); err == nil {
				e.Attrs[key] = n
			} else if f, err := v.Float64(); err == nil {
				e.Attrs[key] = f
			}
		default:
			if b, err := json.Marshal(v); err == nil {
				e.Attrs[key] = clip(string(b))
			}
		}
	}
	return e, true
}

func jsonLevel(v any) (int32, bool) {
	switch x := v.(type) {
	case string:
		n, ok := levelWords[strings.ToLower(strings.TrimSpace(x))]
		return n, ok
	case json.Number:
		// pino and bunyan: 10 trace, 20 debug, 30 info, 40 warn, 50 error,
		// 60 fatal. Smaller numbers are someone else's scale; leave them.
		n, err := x.Int64()
		if err != nil || n < 10 {
			return 0, false
		}
		switch {
		case n < 20:
			return sevTrace, true
		case n < 30:
			return sevDebug, true
		case n < 40:
			return sevInfo, true
		case n < 50:
			return sevWarn, true
		case n < 60:
			return sevError, true
		}
		return sevFatal, true
	}
	return 0, false
}

// jsonID reads a trace or span id of the given size in bytes. An id of
// zeros is OpenTelemetry's "none".
func jsonID(obj map[string]any, keys []string, size int, used map[string]bool) []byte {
	for _, key := range keys {
		s, ok := obj[key].(string)
		if !ok || len(s) != size*2 {
			continue
		}
		id, err := hex.DecodeString(s)
		if err != nil || bytes.Equal(id, make([]byte, size)) {
			continue
		}
		if used != nil {
			used[key] = true
		}
		return id
	}
	return nil
}

func anyUsed(used map[string]bool, keys []string) bool {
	for _, key := range keys {
		if used[key] {
			return true
		}
	}
	return false
}

func clip(s string) string {
	if len(s) > maxJSONAttrValue {
		return strings.ToValidUTF8(s[:maxJSONAttrValue], "") + "…"
	}
	return s
}
