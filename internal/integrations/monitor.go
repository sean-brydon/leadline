package integrations

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/sean-brydon/leadline/internal/ingest"
	"github.com/sean-brydon/leadline/internal/store"
)

// Live capture for Redis. MONITOR makes the server send a line for every
// command any client runs. That is the only way to see an app's commands
// without changing the app, and it is not free: the server writes each
// command out a second time, which on a busy one can cost half its
// throughput. So Leadline only does it to a Redis on this machine, only
// when asked, and keeps no more than a fixed number of commands a second.

const (
	// MonitorMaxPerSec is the most commands recorded in any one second.
	// The rest are counted and dropped.
	MonitorMaxPerSec = 1000

	monitorFlush = 200 * time.Millisecond

	// maxOwnAddrs bounds how many addresses of Leadline's own connections a
	// capture remembers. Leadline has one or two open at a time; past this
	// the old ones are long closed.
	maxOwnAddrs = 256

	// MonitorNote is shown wherever live capture is offered or running.
	MonitorNote = "Live capture uses Redis's MONITOR, which makes the server send Leadline every command it runs. " +
		"That can cost a busy server up to half its throughput, so it is only offered for a Redis on this machine, and is off until you turn it on. " +
		"MONITOR reports when a command arrived, not how long it took: captured Redis commands show how often and when, and their durations are always 0. " +
		"For timings see this connection's stats (commands and slow log). " +
		"At most 1000 commands a second are recorded; the rest are counted as dropped."
)

// MonitorView is a live capture as the app sees it.
type MonitorView struct {
	// State is "connecting", "streaming" or "error".
	State string `json:"state"`
	Error string `json:"error,omitempty"`
	// Seen is every command the server reported, Recorded those kept and
	// Dropped those over the rate cap.
	Seen           int64   `json:"seen"`
	Recorded       int64   `json:"recorded"`
	Dropped        int64   `json:"dropped"`
	MaxPerSec      int     `json:"max_per_sec"`
	LastActivityMs float64 `json:"last_activity_ms"`
	Note           string  `json:"note"`
	// WindowRecorded is the commands recorded since the time Sources was
	// asked about (its since_ms; the last 15 minutes elsewhere), to the
	// minute. Recorded counts from when the capture began.
	WindowRecorded int64 `json:"window_recorded"`
}

type monitor struct {
	m       *Manager
	t       *target
	id      string // the connection's
	service string
	recent  tally
	limit   limiter
	// own reports whether an address is one of Leadline's own connections
	// to this Redis, whose commands are not the app's.
	own func(addr string) bool
	// onlyDB, when set, is the one database number whose commands are
	// recorded: a capture for one worktree of a server others share.
	onlyDB string

	seen, recorded, dropped atomic.Int64
	lastNs                  atomic.Int64

	mu    sync.Mutex
	state string
	err   string
	conn  *respConn

	once sync.Once
	stop chan struct{}
	done chan struct{}
}

func (m *Manager) startMonitor(t *target, id, service string, own func(string) bool) *monitor {
	return m.startMonitorOf(t, id, service, "", own)
}

// startMonitorOf is startMonitor for one database number of the server
// ("" for all of them).
func (m *Manager) startMonitorOf(t *target, id, service, onlyDB string, own func(string) bool) *monitor {
	mon := &monitor{
		m: m, t: t, id: id, service: service, own: own, onlyDB: onlyDB,
		limit: limiter{max: m.monitorMax},
		state: "connecting",
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go mon.run()
	return mon
}

func (mon *monitor) view(since time.Time) *MonitorView {
	mon.mu.Lock()
	state, errText := mon.state, mon.err
	mon.mu.Unlock()
	v := &MonitorView{
		State: state, Error: errText,
		Seen: mon.seen.Load(), Recorded: mon.recorded.Load(), Dropped: mon.dropped.Load(),
		MaxPerSec: mon.limit.max, Note: MonitorNote,
		WindowRecorded: mon.recent.since(time.Now(), since),
	}
	if ns := mon.lastNs.Load(); ns > 0 {
		v.LastActivityMs = float64(ns) / 1e6
	}
	return v
}

func (mon *monitor) set(state, errText string) {
	mon.mu.Lock()
	mon.state, mon.err = state, errText
	mon.mu.Unlock()
}

// halt ends the capture at once; wait returns when what it held has been
// written.
func (mon *monitor) halt() {
	mon.once.Do(func() { close(mon.stop) })
	mon.mu.Lock()
	if mon.conn != nil {
		mon.conn.close()
	}
	mon.mu.Unlock()
}

func (mon *monitor) wait() { <-mon.done }

func (mon *monitor) stopped() bool {
	select {
	case <-mon.stop:
		return true
	default:
		return false
	}
}

// run keeps a MONITOR stream open until close, reconnecting when Redis
// goes away: a dev Redis is restarted often.
func (mon *monitor) run() {
	defer close(mon.done)
	mon.m.capturing(systemRedis, true)
	defer mon.m.capturing(systemRedis, false)
	delay := time.Second
	for !mon.stopped() {
		began := time.Now()
		err := mon.stream()
		if mon.stopped() {
			return
		}
		mon.set("error", explain(err, mon.t).Message)
		if time.Since(began) > 10*time.Second {
			delay = time.Second
		}
		select {
		case <-mon.stop:
			return
		case <-time.After(delay):
		}
		delay = min(2*delay, 15*time.Second)
	}
}

func (mon *monitor) stream() error {
	mon.set("connecting", "")
	ctx, cancel := context.WithTimeout(context.Background(), redisDial)
	c, err := dialRedis(ctx, mon.t)
	cancel()
	if err != nil {
		return err
	}
	mon.mu.Lock()
	mon.conn = c
	mon.mu.Unlock()
	defer c.close()
	if mon.stopped() {
		return nil
	}
	if _, err := c.do("MONITOR"); err != nil {
		return err
	}
	mon.set("streaming", "")

	// Reading and writing are apart so that a store that stalls never
	// leaves the server's output piling up behind an unread socket.
	var (
		pmu     sync.Mutex
		pending []store.Span
	)
	flush := func() {
		pmu.Lock()
		spans := pending
		pending = nil
		pmu.Unlock()
		if len(spans) > 0 {
			mon.m.record(sourceRedisMonitor, spans)
			mon.m.wake()
		}
	}
	quit := make(chan struct{})
	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		tick := time.NewTicker(monitorFlush)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				flush()
			case <-quit:
				flush()
				return
			}
		}
	}()
	defer func() {
		close(quit)
		<-flushed
	}()

	// The addresses of Leadline's own connections to this server, learned
	// from the stream itself.
	mine := map[string]struct{}{}
	for {
		// No deadline: a quiet Redis sends nothing, and halt ends the
		// read by closing the connection.
		c.c.SetDeadline(time.Time{})
		v, err := c.read()
		if err != nil {
			return err
		}
		line, ok := v.(string)
		if !ok {
			continue
		}
		cmd, ok := parseMonitorLine(line)
		if !ok {
			continue
		}
		// A connection of Leadline's own says so first (a stats connection,
		// this or another capture's): nothing it sends is the app's. This
		// is before the database is looked at, because it says so before
		// it selects one.
		if isOwnName(cmd.args) {
			if len(mine) >= maxOwnAddrs {
				clear(mine)
			}
			mine[cmd.client] = struct{}{}
			continue
		}
		if _, own := mine[cmd.client]; own || mon.own(cmd.client) || credential(cmd.args[0]) {
			continue
		}
		if mon.onlyDB != "" && cmd.db != mon.onlyDB {
			continue
		}
		mon.seen.Add(1)
		now := time.Now()
		mon.lastNs.Store(now.UnixNano())
		if !mon.limit.allow(now.Unix()) {
			mon.dropped.Add(1)
			continue
		}
		mon.recorded.Add(1)
		mon.recent.add(now, 1)
		sp := mon.span(cmd)
		pmu.Lock()
		pending = append(pending, sp)
		pmu.Unlock()
	}
}

// credential reports whether a command is a client logging in. Redis blanks
// its arguments, so there is nothing to see, and one arrives with every new
// connection.
func credential(name string) bool {
	return strings.EqualFold(name, "AUTH") || strings.EqualFold(name, "HELLO")
}

// limiter lets at most max events through in any one clock second. It is
// a hard cap, not an average: a burst can't borrow from a quiet minute.
type limiter struct {
	max    int
	second int64
	n      int
}

func (l *limiter) allow(unix int64) bool {
	if unix != l.second {
		l.second, l.n = unix, 0
	}
	if l.n >= l.max {
		return false
	}
	l.n++
	return true
}

type monitorCmd struct {
	ns     int64
	db     string
	client string
	args   []string
}

// parseMonitorLine reads one line of MONITOR's output:
//
//	1339518083.107412 [0 127.0.0.1:60866] "set" "user:42" "a \"b\""
func parseMonitorLine(line string) (monitorCmd, bool) {
	var cmd monitorCmd
	stamp, rest, ok := strings.Cut(line, " [")
	if !ok {
		return cmd, false
	}
	sec, frac, _ := strings.Cut(stamp, ".")
	s, err := strconv.ParseInt(sec, 10, 64)
	if err != nil {
		return cmd, false
	}
	frac = (frac + "000000")[:6]
	us, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return cmd, false
	}
	cmd.ns = s*1e9 + us*1e3

	source, rest, ok := strings.Cut(rest, "] ")
	if !ok {
		return cmd, false
	}
	cmd.db, cmd.client, _ = strings.Cut(source, " ")

	for len(rest) > 0 {
		if rest[0] == ' ' {
			rest = rest[1:]
			continue
		}
		if rest[0] != '"' {
			return cmd, false
		}
		arg, after, ok := unquoteRedis(rest[1:])
		if !ok {
			return cmd, false
		}
		cmd.args = append(cmd.args, arg)
		rest = after
	}
	return cmd, len(cmd.args) > 0
}

// unquoteRedis reads a string up to its closing quote, undoing the escapes
// Redis writes: \" \\ \n \r \t \a \b and \xHH.
func unquoteRedis(s string) (arg, rest string, ok bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"':
			return b.String(), s[i+1:], true
		case '\\':
			i++
			if i >= len(s) {
				return "", "", false
			}
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'a':
				b.WriteByte('\a')
			case 'b':
				b.WriteByte('\b')
			case 'x':
				if i+2 >= len(s) {
					return "", "", false
				}
				n, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
				if err != nil {
					return "", "", false
				}
				b.WriteByte(byte(n))
				i += 2
			default:
				b.WriteByte(s[i])
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", "", false
}

func (mon *monitor) span(cmd monitorCmd) store.Span {
	name, _ := redisCommandName(cmd.args)
	attrs, _ := json.Marshal(map[string]any{
		"db.namespace":    cmd.db,
		"server.address":  mon.t.addr(),
		"client.address":  cmd.client,
		"leadline.source": "redis-monitor",
		// MONITOR says when, never how long.
		"leadline.no_duration": true,
	})
	return store.Span{
		TraceID:       newTraceID(),
		SpanID:        newSpanID(),
		Service:       mon.service,
		Name:          name,
		Kind:          "client",
		StartNs:       cmd.ns,
		Status:        "ok",
		DBSystem:      "redis",
		DBStatement:   redisStatement(cmd.args),
		DBFingerprint: redisFingerprint(cmd.args),
		Attrs:         string(attrs),
		Source:        store.SourceRedisMonitor,
		Capture:       mon.id,
		DBServer:      store.DBServer(mon.t.addr(), 0),
	}
}

// redisCommandName is the command in capitals, and how many arguments it
// took up.
func redisCommandName(args []string) (string, int) { return ingest.RedisCommand(args) }

const (
	maxRedisArg       = 64
	maxRedisArgs      = 16
	maxRedisStatement = 512
)

// redisStatement writes a command the way a person would type it, with
// long values cut and credentials left out.
func redisStatement(args []string) string {
	if len(args) == 0 {
		return ""
	}
	name, used := redisCommandName(args)
	rest := args[used:]
	hide := map[int]bool{}
	switch name {
	case "AUTH":
		return "AUTH ?"
	case "HELLO", "MIGRATE":
		for i, a := range rest {
			switch strings.ToUpper(a) {
			case "AUTH":
				hide[i+1] = true
				if name == "HELLO" {
					hide[i+2] = true
				}
			case "AUTH2":
				hide[i+1], hide[i+2] = true, true
			}
		}
	case "ACL SETUSER":
		for i := 1; i < len(rest); i++ {
			hide[i] = true
		}
	case "CONFIG SET":
		for i := 0; i+1 < len(rest); i += 2 {
			if k := strings.ToLower(rest[i]); strings.Contains(k, "pass") || strings.Contains(k, "auth") {
				hide[i+1] = true
			}
		}
	}
	var b strings.Builder
	b.WriteString(name)
	for i, a := range rest {
		if i == maxRedisArgs {
			b.WriteString(" … " + strconv.Itoa(len(rest)-i) + " more")
			break
		}
		b.WriteByte(' ')
		switch {
		case hide[i]:
			b.WriteByte('?')
		case a == "":
			b.WriteString(`""`)
		case strings.ContainsAny(a, " \t\r\n"):
			// Quoted, or a value of several words reads as several
			// arguments.
			b.WriteString(strconv.Quote(clipText(printable(a), maxRedisArg)))
		default:
			b.WriteString(clipText(printable(a), maxRedisArg))
		}
		if b.Len() > maxRedisStatement {
			break
		}
	}
	return clipText(b.String(), maxRedisStatement)
}

// redisFingerprint groups commands by what they do to which kind of key;
// see ingest.RedisFingerprint, which the app's own Redis spans go through
// too, so a command seen both ways has one fingerprint.
func redisFingerprint(args []string) string { return ingest.RedisFingerprint(args) }

// printable makes an argument safe to show and store: valid UTF-8, on one
// line.
func printable(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	if strings.ContainsAny(s, "\r\n\t") {
		s = strings.Join(strings.Fields(s), " ")
	}
	return s
}

// clipText cuts s to at most n bytes, on a rune boundary, and says so.
func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
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
