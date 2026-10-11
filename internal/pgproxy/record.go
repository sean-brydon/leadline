package pgproxy

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/sean-brydon/leadline/internal/ingest"
	"github.com/sean-brydon/leadline/internal/store"
)

// Sink is where spans go. *store.Store is one.
type Sink interface {
	AddSpans([]store.Span) error
}

const (
	flushEvery = 200 * time.Millisecond
	// maxPending bounds memory if the store stalls. Past it new statements
	// are counted and dropped: losing a row of telemetry beats slowing
	// the app's database.
	maxPending = 100_000
	maxParams  = 32
	maxParam   = 200
)

// event is a finished statement as the relay saw it. Turning it into a
// span (the fingerprint, the JSON) is left to the recorder's goroutine, so
// the relay does as little as it can between two reads.
type event struct {
	st   *stmt
	meta *meta
	bind []byte
	oids []uint32

	start, end int64
	rows       int64 // -1 when the server gave no count; statements, for a transaction
	errMsg     string
	errCode    string
	phase      string // "parse" or "bind" when the statement failed before running
	suspended  bool
	shared     bool // answered together with its neighbours; see tracker.part

	traceID, spanID, parentID string

	// linked says traceID and parentID are the app's own, read from a
	// sqlcommenter comment, and not ids the proxy made up.
	linked bool

	root    bool   // a transaction's span rather than a statement's
	outcome string // root: "commit", "rollback" or unknown
}

// Recorder collects statements from every proxied connection and writes
// them to the sink in batches.
type Recorder struct {
	sink   Sink
	notify func()

	mu      sync.Mutex
	pending []event
	dropped int

	// flushMu serialises flushes; fps and each stmt.fp belong to it.
	flushMu sync.Mutex
	spare   []event
	fps     map[string]string

	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// NewRecorder starts a recorder that flushes to sink every 200 ms and calls
// notify (which may be nil) after each write. Close it to flush what is
// left.
func NewRecorder(sink Sink, notify func()) *Recorder {
	return newRecorder(sink, notify, flushEvery)
}

func newRecorder(sink Sink, notify func(), every time.Duration) *Recorder {
	r := &Recorder{
		sink:   sink,
		notify: notify,
		fps:    make(map[string]string),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go r.loop(every)
	return r
}

func (r *Recorder) loop(every time.Duration) {
	defer close(r.done)
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			r.Flush()
		case <-r.stop:
			r.Flush()
			return
		}
	}
}

// Close stops the recorder after a last flush.
func (r *Recorder) Close() {
	r.once.Do(func() { close(r.stop) })
	<-r.done
}

func (r *Recorder) add(evs []event) {
	r.mu.Lock()
	if room := maxPending - len(r.pending); len(evs) > room {
		r.dropped += len(evs) - room
		evs = evs[:room]
	}
	r.pending = append(r.pending, evs...)
	r.mu.Unlock()
}

// Flush writes what has been collected so far.
func (r *Recorder) Flush() {
	r.flushMu.Lock()
	defer r.flushMu.Unlock()

	r.mu.Lock()
	evs, dropped := r.pending, r.dropped
	r.pending, r.dropped = r.spare[:0], 0
	r.mu.Unlock()

	if dropped > 0 {
		slog.Warn("pg proxy: statements arrived faster than they could be stored; some were not recorded", "dropped", dropped)
	}
	if len(evs) == 0 {
		r.spare = evs
		return
	}
	spans := make([]store.Span, len(evs))
	for i := range evs {
		spans[i] = r.span(&evs[i])
	}
	clear(evs)
	r.spare = evs[:0]

	if err := r.sink.AddSpans(spans); err != nil {
		slog.Warn("pg proxy: could not store queries", "err", err, "spans", len(spans))
		return
	}
	if r.notify != nil {
		r.notify()
	}
}

func (r *Recorder) span(ev *event) store.Span {
	m := ev.meta
	sp := store.Span{
		TraceID:    ev.traceID,
		SpanID:     ev.spanID,
		ParentID:   ev.parentID,
		Service:    text(m.service),
		StartNs:    ev.start,
		DurationNs: max(ev.end-ev.start, 0),
		Status:     "ok",
		// Read off the wire: the store pairs it with the app's own span
		// for the same statement, when there is one.
		Source: store.SourcePostgresWire,
		DBName: text(m.database),
		Linked: ev.linked,
	}
	if !ev.root {
		sp.DBServer = store.DBServer(m.upstream, 0)
	}
	attrs := map[string]any{
		"db.namespace":    text(m.database),
		"db.user":         text(m.user),
		"server.address":  m.upstream,
		"leadline.source": "pgproxy",
	}
	if ev.errMsg != "" {
		sp.Status, sp.StatusMessage = "error", text(ev.errMsg)
		if ev.errCode != "" {
			attrs["db.response.status_code"] = ev.errCode
		}
	}
	if ev.root {
		// Not a database call itself, so it stays out of the query list:
		// it is the frame its statements hang in.
		sp.Name, sp.Kind = "transaction", "internal"
		attrs["db.transaction.statements"] = ev.rows
		if ev.outcome != "" {
			attrs["db.transaction.outcome"] = ev.outcome
		}
		sp.Attrs = toJSON(attrs)
		return sp
	}

	st := ev.st
	sp.Name, sp.Kind = st.name, "client"
	sp.DBSystem = "postgresql"
	sp.DBStatement = text(redact(st.sql))
	sp.DBFingerprint = r.fingerprint(st)
	if ev.rows >= 0 {
		attrs["db.rows"] = ev.rows
	}
	if ev.bind != nil {
		attrs["db.query.parameters"] = params(ev.bind, ev.oids)
	}
	if ev.phase != "" {
		attrs["db.failed_at"] = ev.phase
	}
	if ev.suspended {
		attrs["db.portal.suspended"] = true
	}
	if ev.shared {
		attrs["leadline.shared_timing"] = true
	}
	if st.cut {
		sp.DBStatement += " …"
		attrs["db.query.truncated"] = true
	}
	sp.Attrs = toJSON(attrs)
	return sp
}

// fingerprint is ingest.Fingerprint with a memory: it is a handful of
// regular expressions, and an app sends the same few statements over and
// over.
func (r *Recorder) fingerprint(st *stmt) string {
	if st.fp != "" {
		return st.fp
	}
	fp, ok := r.fps[st.sql]
	if !ok {
		// psql sends the semicolon along; a driver doesn't.
		sql := strings.TrimRight(withoutTraceComment(redact(st.sql)), "; \t\r\n")
		if st.cut {
			sql = closeQuote(sql)
		}
		fp = text(ingest.Fingerprint(text(sql)))
		if len(r.fps) >= 4096 {
			clear(r.fps)
		}
		r.fps[st.sql] = fp
	}
	st.fp = fp
	return fp
}

// text makes a string safe to store: DuckDB refuses a VARCHAR that isn't
// UTF-8, and a client may speak LATIN1 or have had a string cut mid-rune.
func text(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "�")
}

func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// Ids are random, as OpenTelemetry's are: 16 bytes for a trace, 8 for a
// span, in hex.
func newTraceID() string {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], rand.Uint64())
	binary.BigEndian.PutUint64(b[8:], rand.Uint64()|1)
	return hex.EncodeToString(b[:])
}

func newSpanID() string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], rand.Uint64()|1)
	return hex.EncodeToString(b[:])
}

// ---- parameters ----

// params renders a Bind message's parameter values for display. bind
// starts at the format codes and may be cut short.
func params(bind []byte, oids []uint32) []any {
	b := reader{b: bind}
	nf := int(b.uint16())
	formats := b.take(2 * nf)
	np := int(b.uint16())
	if b.short {
		return nil
	}
	out := make([]any, 0, min(np, maxParams+1))
	for i := 0; i < np; i++ {
		if i == maxParams {
			out = append(out, fmt.Sprintf("… %d more", np-i))
			break
		}
		n := int32(b.uint32())
		if b.short {
			out = append(out, "…")
			break
		}
		if n < 0 {
			out = append(out, nil)
			continue
		}
		v := b.take(int(n))
		binaryFormat := false
		switch {
		case nf == 1:
			binaryFormat = binary.BigEndian.Uint16(formats) == 1
		case i < nf:
			binaryFormat = binary.BigEndian.Uint16(formats[2*i:]) == 1
		}
		var oid uint32
		if i < len(oids) {
			oid = oids[i]
		}
		switch {
		case !binaryFormat:
			out = append(out, clip(string(v), b.short))
		case b.short:
			out = append(out, fmt.Sprintf("(binary, %d bytes)", n))
		default:
			out = append(out, binaryParam(v, oid))
		}
		if b.short {
			break
		}
	}
	return out
}

func clip(s string, cut bool) string {
	s = text(s)
	if len(s) > maxParam {
		s, cut = strings.ToValidUTF8(s[:maxParam], ""), true
	}
	if cut {
		s += "…"
	}
	return s
}

// pgEpoch is what Postgres counts dates and timestamps from.
var pgEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// binaryParam decodes the binary forms a driver is likely to send. Without
// the type (the client need not declare it) the bytes can't be read.
func binaryParam(v []byte, oid uint32) any {
	switch {
	case oid == 16 && len(v) == 1: // bool
		return v[0] != 0
	case oid == 21 && len(v) == 2: // int2
		return int16(binary.BigEndian.Uint16(v))
	case (oid == 23 || oid == 26) && len(v) == 4: // int4, oid
		if oid == 26 {
			return binary.BigEndian.Uint32(v)
		}
		return int32(binary.BigEndian.Uint32(v))
	case oid == 20 && len(v) == 8: // int8
		return int64(binary.BigEndian.Uint64(v))
	case oid == 700 && len(v) == 4: // float4
		return finite(float64(math.Float32frombits(binary.BigEndian.Uint32(v))))
	case oid == 701 && len(v) == 8: // float8
		return finite(math.Float64frombits(binary.BigEndian.Uint64(v)))
	case oid == 25 || oid == 1043 || oid == 1042 || oid == 19 || oid == 114: // text, varchar, bpchar, name, json
		return clip(string(v), false)
	case oid == 3802 && len(v) >= 1: // jsonb: a version byte, then the text
		return clip(string(v[1:]), false)
	case oid == 2950 && len(v) == 16: // uuid
		h := hex.EncodeToString(v)
		return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
	case (oid == 1114 || oid == 1184) && len(v) == 8: // timestamp, timestamptz: microseconds
		us := int64(binary.BigEndian.Uint64(v))
		if us == math.MaxInt64 || us == math.MinInt64 {
			return "infinity"
		}
		return pgEpoch.Add(time.Duration(us) * time.Microsecond).Format("2006-01-02T15:04:05.999999Z")
	case oid == 1082 && len(v) == 4: // date: days
		return pgEpoch.AddDate(0, 0, int(int32(binary.BigEndian.Uint32(v)))).Format("2006-01-02")
	}
	return fmt.Sprintf("(binary, %d bytes)", len(v))
}

// finite keeps NaN and the infinities, which JSON has no number for.
func finite(f float64) any {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Sprint(f)
	}
	return f
}

// reader walks a message body that may have been cut short.
type reader struct {
	b     []byte
	short bool
}

func (r *reader) take(n int) []byte {
	if n < 0 || n > len(r.b) {
		r.short = true
		n = len(r.b)
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}

func (r *reader) uint16() uint16 {
	if v := r.take(2); len(v) == 2 {
		return binary.BigEndian.Uint16(v)
	}
	return 0
}

func (r *reader) uint32() uint32 {
	if v := r.take(4); len(v) == 4 {
		return binary.BigEndian.Uint32(v)
	}
	return 0
}
