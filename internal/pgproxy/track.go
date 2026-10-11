package pgproxy

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"sync"
)

const (
	// maxSQL is how much of a statement's text the proxy keeps. A bulk
	// INSERT can be megabytes; its first 16 KB say what it is.
	maxSQL  = 16 << 10
	maxName = 256
	// maxBind is how much of a Bind message's parameters are kept for
	// display.
	maxBind = 4 << 10
	// maxBatch bounds the statements held while waiting to learn whether
	// they are in a transaction. See settle.
	maxBatch = 256
)

type kind uint8

// What the client has sent that the server has yet to answer.
const (
	kParse kind = iota + 1
	kBind
	kDescribe
	kExec
	kClose
	kSync
	kQuery // a simple query: any number of statements, then ReadyForQuery
	kFunc  // a fast-path function call
)

// stmt is one statement's text and what was read out of it. A prepared
// statement has one stmt however often it runs.
type stmt struct {
	sql  string
	name string // the first keyword
	cut  bool   // sql is only the start of what was sent
	// From a sqlcommenter comment, when there is one.
	traceID, parentID string
	// Parameter types, from Parse or the server's ParameterDescription.
	oids []uint32

	fp string // the fingerprint; the recorder fills and reads it
}

func newStmt(sql string, cut bool) *stmt {
	st := &stmt{sql: sql, cut: cut, name: keyword(sql)}
	st.traceID, st.parentID, _ = traceparent(sql)
	return st
}

type portal struct {
	st   *stmt
	bind []byte // the Bind message from its format codes on, or nil
}

type item struct {
	kind  kind
	st    *stmt
	parts []*stmt // kQuery with several statements
	done  int     // kQuery: statements answered so far
	bind  []byte
	start int64 // when the proxy read the message, unix ns
	mark  int64 // kQuery: when the last statement was answered
	rows  int64 // DataRows seen
	// The server asked for COPY data and the client hasn't finished
	// sending it.
	copyIn bool
}

// current is the statement the server is running for this item.
func (it *item) current() *stmt {
	if it.kind == kQuery && it.done < len(it.parts) {
		return it.parts[it.done]
	}
	return it.st
}

// queue is the FIFO of unanswered messages. Items are held by value so that
// a statement costs no allocation here.
type queue struct {
	items []item
	head  int
}

func (q *queue) push(it item) { q.items = append(q.items, it) }

func (q *queue) front() *item {
	if q.head == len(q.items) {
		return nil
	}
	return &q.items[q.head]
}

func (q *queue) pop() {
	q.items[q.head] = item{}
	q.head++
	switch {
	case q.head == len(q.items):
		q.items, q.head = q.items[:0], 0
	case q.head >= 256 && q.head*2 >= len(q.items):
		// A client that pipelines without pause never empties the queue;
		// move the tail down so the slice doesn't grow for ever.
		n := copy(q.items, q.items[q.head:])
		clear(q.items[n:])
		q.items, q.head = q.items[:n], 0
	}
}

// dropSyncsAfterFront removes the Syncs queued right behind the front item.
func (q *queue) dropSyncsAfterFront() {
	i := q.head + 1
	j := i
	for j < len(q.items) && q.items[j].kind == kSync {
		j++
	}
	if j > i {
		n := copy(q.items[i:], q.items[j:])
		clear(q.items[i+n:])
		q.items = q.items[:i+n]
	}
}

// meta is what a connection's spans have in common. It is replaced, never
// changed, so events can share it.
type meta struct {
	service  string
	database string
	user     string
	upstream string
	// named is the service the proxy was opened for (Route.Service), and
	// app what the connection called itself when it opened.
	named string
	app   string
}

func (m *meta) withApp(app string) *meta {
	next := *m
	next.app = app
	next.service = serviceName(app, m.named, m.database)
	return &next
}

// serviceName is who a connection's statements are filed under: what the
// connection calls itself, else what the proxy was opened for, else the
// database.
func serviceName(app, named, database string) string {
	switch {
	case app != "":
		return app
	case named != "":
		return named
	case database != "":
		return database
	}
	return "postgresql"
}

// txn is an open transaction: BEGIN to COMMIT or ROLLBACK.
type txn struct {
	traceID, spanID, parentID string
	start                     int64
	statements                int
	errMsg                    string
	failed                    bool
}

// tracker follows the conversation on one connection. Both relay
// directions feed it, under mu: the client side queues what was asked, the
// server side matches each answer to the oldest question. The protocol
// answers strictly in order, which is what makes pipelining traceable.
type tracker struct {
	mu  sync.Mutex
	off bool // gave up; the connection is a plain relay from here
	rec *Recorder

	meta *meta
	now  int64 // when the chunk being read arrived, unix ns

	q     queue
	ready bool // the server has sent its first ReadyForQuery
	// After an error in the extended protocol the server throws away
	// everything up to the next Sync. skip is set when that Sync hasn't
	// been sent yet.
	skip bool

	unnamed       *stmt
	stmts         map[string]*stmt
	unnamedPortal portal
	portals       map[string]portal

	// Finished statements wait here for the next ReadyForQuery, which says
	// whether they ran inside a transaction.
	batch   []event
	tx      *txn
	outcome string // "commit" or "rollback" when the last statement was one
}

type desync struct {
	got  byte
	head kind
}

func (d *desync) Error() string {
	return fmt.Sprintf("server sent %q while waiting on message kind %d", d.got, d.head)
}

func (t *tracker) unexpected(typ byte) error {
	d := &desync{got: typ}
	if it := t.q.front(); it != nil {
		d.head = it.kind
	}
	return d
}

func (t *tracker) push(it item) {
	if t.skip {
		if it.kind != kSync {
			return
		}
		t.skip = false
	}
	t.q.push(it)
}

// ---- what the client sends ----

type frontTap struct{ t *tracker }

func (frontTap) keep(typ byte) int {
	switch typ {
	case 'Q':
		return maxSQL + 1
	case 'P':
		return maxName + maxSQL + 1024
	case 'B':
		return 2*maxName + maxBind
	case 'E', 'C', 'D':
		return maxName + 8
	case 'S', 'H', 'X', 'F', 'd', 'c', 'f':
		return 0
	case 'p':
		// Passwords and SASL exchanges: never read.
		return 0
	}
	return -1
}

func (f frontTap) message(typ byte, body []byte, size int) error {
	t := f.t
	switch typ {
	case 'Q':
		sql, _, cut := sqlText(body)
		st := newStmt(sql, cut)
		it := item{kind: kQuery, st: st, start: t.now}
		for _, part := range splitStatements(sql) {
			it.parts = append(it.parts, newStmt(part, false))
		}
		t.push(it)

	case 'P':
		name, rest, cut := cstring(body)
		if cut {
			return errFraming
		}
		sql, rest, cut := sqlText(rest)
		st := newStmt(sql, cut)
		if !cut && len(rest) >= 2 {
			n := int(binary.BigEndian.Uint16(rest))
			if rest = rest[2:]; n > 0 && len(rest) >= 4*n {
				st.oids = oidList(rest, n)
			}
		}
		if name == "" {
			t.unnamed = st
		} else {
			t.remember(name, st)
		}
		t.push(item{kind: kParse, st: st, start: t.now})

	case 'B':
		portalName, rest, cut := cstring(body)
		if cut {
			return errFraming
		}
		stmtName, rest, cut := cstring(rest)
		if cut {
			return errFraming
		}
		p := portal{st: t.statement(stmtName)}
		if hasParams(rest) {
			p.bind = bytes.Clone(rest)
		}
		if portalName == "" {
			t.unnamedPortal = p
		} else {
			if t.portals == nil {
				t.portals = make(map[string]portal)
			}
			t.portals[portalName] = p
		}
		t.push(item{kind: kBind, st: p.st, bind: p.bind, start: t.now})

	case 'E':
		name, _, cut := cstring(body)
		if cut {
			return errFraming
		}
		p := t.unnamedPortal
		if name != "" {
			p = t.portals[name]
		}
		if p.st == nil {
			// A cursor declared in SQL can be fetched from this way.
			p.st = &stmt{sql: "FETCH ALL FROM " + name, name: "FETCH"}
		}
		t.push(item{kind: kExec, st: p.st, bind: p.bind, start: t.now})

	case 'D':
		it := item{kind: kDescribe}
		if len(body) > 0 && body[0] == 'S' {
			name, _, _ := cstring(body[1:])
			if name == "" {
				it.st = t.unnamed
			} else {
				it.st = t.stmts[name]
			}
		}
		t.push(it)

	case 'C':
		if len(body) > 0 {
			name, _, _ := cstring(body[1:])
			switch {
			case name == "":
			case body[0] == 'S':
				delete(t.stmts, name)
			default:
				delete(t.portals, name)
			}
		}
		t.push(item{kind: kClose})

	case 'S':
		// While the server reads COPY data it ignores Sync, so one sent
		// then has no ReadyForQuery coming.
		if it := t.q.front(); it != nil && it.copyIn {
			return nil
		}
		t.push(item{kind: kSync})

	case 'F':
		t.push(item{kind: kFunc})

	case 'c', 'f':
		if it := t.q.front(); it != nil {
			it.copyIn = false
		}
	}
	return nil
}

// statement finds a prepared statement by name.
func (t *tracker) statement(name string) *stmt {
	st := t.unnamed
	if name != "" {
		st = t.stmts[name]
	}
	if st == nil {
		// Prepared before the proxy was watching, or with SQL's PREPARE.
		st = &stmt{sql: "EXECUTE " + name, name: "EXECUTE"}
		if name != "" {
			t.remember(name, st)
		}
	}
	return st
}

// maxStmts bounds the prepared statements remembered for one connection.
const maxStmts = 4096

func (t *tracker) remember(name string, st *stmt) {
	if t.stmts == nil {
		t.stmts = make(map[string]*stmt)
	}
	if len(t.stmts) >= maxStmts {
		// A client that prepares without end and never closes. Let some
		// go; if one is run again it shows as "EXECUTE name".
		n := 0
		for old := range t.stmts {
			delete(t.stmts, old)
			if n++; n == maxStmts/4 {
				break
			}
		}
	}
	t.stmts[name] = st
}

// forget follows DEALLOCATE and DISCARD ALL: some drivers drop their
// prepared statements with SQL rather than with a Close message.
func (t *tracker) forget(sql string) {
	sql = strings.TrimRight(sql, "; \t\r\n")
	if strings.HasSuffix(sql, `"`) {
		// A quoted name is taken as written.
		if open := strings.LastIndexByte(sql[:len(sql)-1], '"'); open >= 0 {
			delete(t.stmts, sql[open+1:len(sql)-1])
		}
		return
	}
	name := sql[strings.LastIndexAny(sql, " \t\r\n")+1:]
	switch {
	case name == sql:
	case strings.EqualFold(name, "all"):
		clear(t.stmts)
	case len(sql) >= 10 && strings.EqualFold(sql[:10], "deallocate"):
		// An unquoted one folds to lower case.
		delete(t.stmts, strings.ToLower(name))
	}
}

// ---- what the server answers ----

type backTap struct{ t *tracker }

func (backTap) keep(typ byte) int {
	switch typ {
	case 'D', 'd', 'T', 'N', 'A', 'R', 'K', 'V', 'v':
		return 0
	case '1', '2', '3', 'n', 's', 'I', 'c', 'G', 'H', 'W':
		return 0
	case 'C':
		return 96
	case 'Z':
		return 1
	case 'E':
		return 4096
	case 'S':
		return 256
	case 't':
		return 2 + 4*256
	}
	return -1
}

func (b backTap) message(typ byte, body []byte, size int) error {
	t := b.t
	it := t.q.front()
	switch typ {
	case 'D':
		if it != nil {
			it.rows++
		}

	case 'C':
		if it == nil {
			return t.unexpected(typ)
		}
		t.outcome = outcomeOf(body)
		st := it.current()
		switch it.kind {
		case kExec:
			t.emit(event{st: st, bind: it.bind, start: it.start, end: t.now, rows: tagRows(body)})
			t.q.pop()
		case kQuery:
			t.part(it, event{st: st, rows: tagRows(body)})
		default:
			return t.unexpected(typ)
		}
		if st.name == "DEALLOCATE" || st.name == "DISCARD" {
			t.forget(st.sql)
		}

	case 'Z':
		if len(body) < 1 {
			return errFraming
		}
		switch {
		case it != nil && (it.kind == kSync || it.kind == kQuery || it.kind == kFunc):
			t.q.pop()
		case it == nil && !t.ready:
			// The one that ends startup.
		default:
			return t.unexpected(typ)
		}
		t.ready = true
		t.settle(body[0])

	case '1':
		return t.answered(typ, kParse)
	case '2':
		return t.answered(typ, kBind)
	case '3':
		return t.answered(typ, kClose)

	case 't':
		if it == nil || it.kind != kDescribe {
			return t.unexpected(typ)
		}
		if it.st != nil && it.st.oids == nil && size == len(body) && len(body) >= 2 {
			if n := int(binary.BigEndian.Uint16(body)); n > 0 && len(body) >= 2+4*n {
				it.st.oids = oidList(body[2:], n)
			}
		}

	case 'T', 'n':
		switch {
		case it != nil && it.kind == kDescribe:
			t.q.pop()
		case it != nil && it.kind == kQuery && typ == 'T':
		default:
			return t.unexpected(typ)
		}

	case 'I':
		// An empty statement: nothing ran, so there is nothing to show.
		switch {
		case it != nil && it.kind == kExec:
			t.q.pop()
		case it != nil && it.kind == kQuery:
		default:
			return t.unexpected(typ)
		}

	case 's':
		// The client asked for a limited number of rows and the portal has
		// more. Each fetch is a round trip of its own, so it gets a span.
		if it == nil || it.kind != kExec {
			return t.unexpected(typ)
		}
		t.emit(event{st: it.st, bind: it.bind, start: it.start, end: t.now, rows: it.rows, suspended: true})
		t.q.pop()

	case 'G':
		if it == nil || (it.kind != kExec && it.kind != kQuery) {
			return t.unexpected(typ)
		}
		it.copyIn = true
		// The Sync that followed the Execute was read as COPY data would
		// be, and ignored.
		t.q.dropSyncsAfterFront()

	case 'H':
		if it == nil || (it.kind != kExec && it.kind != kQuery) {
			return t.unexpected(typ)
		}

	case 'W':
		// Streaming replication: from here on it is a different protocol.
		return errReplication

	case 'E':
		t.failed(parseError(body))

	case 'S':
		name, rest, cut := cstring(body)
		if !cut && name == "application_name" {
			app, _, _ := cstring(rest)
			if svc := serviceName(app, t.meta.named, t.meta.database); svc != t.meta.service {
				t.meta = t.meta.withApp(app)
			}
		}
	}
	return nil
}

var errReplication = fmt.Errorf("replication stream")

// answered pops the front item when it is the kind the server just
// acknowledged.
func (t *tracker) answered(typ byte, want kind) error {
	if it := t.q.front(); it == nil || it.kind != want {
		return t.unexpected(typ)
	}
	t.q.pop()
	return nil
}

type pgError struct {
	severity, code, message string
}

// failed handles an ErrorResponse.
func (t *tracker) failed(e pgError) {
	it := t.q.front()
	if e.severity == "FATAL" || e.severity == "PANIC" {
		// The server is hanging up. Whatever was running failed with it.
		if it != nil && (it.kind == kExec || it.kind == kQuery) {
			t.emit(event{st: it.current(), bind: it.bind, start: it.start, end: t.now, rows: -1, errMsg: e.message, errCode: e.code})
		}
		t.q = queue{}
		return
	}
	if it == nil {
		return
	}
	switch it.kind {
	case kQuery:
		// The rest of a simple query is abandoned; ReadyForQuery follows.
		t.part(it, event{st: it.current(), rows: -1, errMsg: e.message, errCode: e.code})
		it.done = len(it.parts)
		return
	case kFunc, kSync:
		// A Sync fails when the commit it implies does (a deferred
		// constraint, say). No one statement is to blame.
		return
	case kParse, kBind, kExec:
		ev := event{st: it.st, bind: it.bind, start: it.start, end: t.now, rows: -1, errMsg: e.message, errCode: e.code}
		switch it.kind {
		case kParse:
			ev.phase = "parse"
		case kBind:
			ev.phase = "bind"
		}
		t.emit(ev)
	}
	// The server now discards everything up to the next Sync: none of it
	// will be answered.
	t.q.pop()
	for {
		next := t.q.front()
		if next == nil {
			t.skip = true
			return
		}
		if next.kind == kSync {
			return
		}
		t.q.pop()
	}
}

// part records one statement of a simple query. The server answers them
// in order, so a statement ran between the answer before it and its own.
// But the server usually sends the answers to a short script in one go,
// and then all the proxy can know is the time they took together: each
// gets that whole stretch, marked as shared, rather than the first getting
// all of it and the rest none.
func (t *tracker) part(it *item, ev event) {
	if it.done > 0 {
		if t.now == it.mark {
			ev.shared = true
			if n := len(t.batch); n > 0 {
				t.batch[n-1].shared = true
			}
		} else {
			it.start = it.mark
		}
	}
	ev.start, ev.end = it.start, t.now
	it.mark = t.now
	it.done++
	it.rows = 0
	t.emit(ev)
}

func (t *tracker) emit(ev event) {
	ev.meta = t.meta
	if ev.bind != nil {
		ev.oids = ev.st.oids
	}
	t.batch = append(t.batch, ev)
	if len(t.batch) >= maxBatch {
		// A script of thousands of statements in one query: don't hold
		// them all for the ReadyForQuery at its end.
		t.flush()
	}
}

// settle runs at each ReadyForQuery, which reports whether the session is
// in a transaction. Statements of one transaction share a trace under a
// "transaction" span; any other statement is a trace of its own. Going by
// the server's word rather than by spotting BEGIN and COMMIT in the SQL
// keeps this right for savepoints, failed transactions and ORMs' variants.
func (t *tracker) settle(status byte) {
	inTx := status == 'T' || status == 'E'
	if inTx && t.tx == nil {
		t.tx = t.begin()
	}
	t.flush()
	if !inTx {
		if t.tx != nil {
			t.end(t.outcome)
		}
		// Portals don't outlive their transaction.
		if len(t.portals) > 0 {
			clear(t.portals)
		}
	}
}

func (t *tracker) begin() *txn {
	tx := &txn{start: t.now, spanID: newSpanID()}
	if len(t.batch) > 0 {
		tx.start = t.batch[0].start
	}
	// If the statement that opened the transaction came with the app's
	// trace context, the whole transaction belongs to that trace.
	for i := range t.batch {
		if st := t.batch[i].st; st != nil && st.traceID != "" {
			tx.traceID, tx.parentID = st.traceID, st.parentID
			break
		}
	}
	if tx.traceID == "" {
		tx.traceID = newTraceID()
	}
	return tx
}

// flush gives the waiting statements their place in a trace and hands them
// to the recorder.
func (t *tracker) flush() {
	if len(t.batch) == 0 {
		return
	}
	for i := range t.batch {
		ev := &t.batch[i]
		ev.spanID = newSpanID()
		if t.tx != nil {
			t.tx.statements++
			if ev.errMsg != "" && !t.tx.failed {
				t.tx.failed, t.tx.errMsg = true, ev.errMsg
			}
		}
		switch {
		case ev.st.traceID != "":
			ev.traceID, ev.parentID, ev.linked = ev.st.traceID, ev.st.parentID, true
		case t.tx != nil:
			ev.traceID, ev.parentID, ev.linked = t.tx.traceID, t.tx.spanID, t.tx.parentID != ""
		default:
			ev.traceID = newTraceID()
		}
	}
	t.rec.add(t.batch)
	clear(t.batch)
	t.batch = t.batch[:0]
}

// end closes the open transaction and records its span.
func (t *tracker) end(outcome string) {
	tx := t.tx
	t.tx = nil
	t.rec.add([]event{{
		root:     true,
		meta:     t.meta,
		traceID:  tx.traceID,
		spanID:   tx.spanID,
		parentID: tx.parentID,
		linked:   tx.parentID != "",
		start:    tx.start,
		end:      t.now,
		rows:     int64(tx.statements),
		errMsg:   tx.errMsg,
		outcome:  outcome,
	}})
}

// stop ends tracking: the connection closed, or the tracker met something
// it doesn't understand. What it knows so far is recorded.
func (t *tracker) stop(closed bool) {
	if t.off {
		return
	}
	t.off = true
	if closed {
		if it := t.q.front(); it != nil && (it.kind == kExec || it.kind == kQuery) && it.current() != nil {
			t.emit(event{st: it.current(), bind: it.bind, start: it.start, end: t.now, rows: -1,
				errMsg: "connection closed before the server answered"})
		}
	}
	t.flush()
	if t.tx != nil {
		if t.tx.errMsg == "" {
			t.tx.errMsg = "connection closed inside the transaction"
		}
		t.end("")
	}
	t.q = queue{}
	t.stmts, t.portals, t.unnamed, t.unnamedPortal = nil, nil, nil, portal{}
}

// ---- reading message bodies ----

// cstring reads a NUL-terminated string. cut says the terminator wasn't
// there: the proxy kept only the start of the message.
func cstring(b []byte) (s string, rest []byte, cut bool) {
	i := bytes.IndexByte(b, 0)
	if i < 0 {
		return string(b), nil, true
	}
	return string(b[:i]), b[i+1:], false
}

// sqlText reads a statement's text, keeping at most maxSQL of it.
func sqlText(b []byte) (sql string, rest []byte, cut bool) {
	i := bytes.IndexByte(b, 0)
	if i >= 0 && i <= maxSQL {
		return string(b[:i]), b[i+1:], false
	}
	return string(b[:min(len(b), maxSQL)]), nil, true
}

func oidList(b []byte, n int) []uint32 {
	oids := make([]uint32, n)
	for i := range oids {
		oids[i] = binary.BigEndian.Uint32(b[4*i:])
	}
	return oids
}

// hasParams looks past a Bind's format codes for its parameter count.
func hasParams(b []byte) bool {
	if len(b) < 2 {
		return false
	}
	skip := 2 + 2*int(binary.BigEndian.Uint16(b))
	return len(b) >= skip+2 && binary.BigEndian.Uint16(b[skip:]) > 0
}

// tagRows reads the row count off a CommandComplete tag: "SELECT 3",
// "UPDATE 12", "INSERT 0 1". Tags like "BEGIN" have none, which is -1.
func tagRows(tag []byte) int64 {
	if i := bytes.IndexByte(tag, 0); i >= 0 {
		tag = tag[:i]
	}
	i := len(tag)
	for i > 0 && tag[i-1] >= '0' && tag[i-1] <= '9' {
		i--
	}
	if i == len(tag) || i == 0 || tag[i-1] != ' ' || len(tag)-i > 18 {
		return -1
	}
	var n int64
	for _, c := range tag[i:] {
		n = n*10 + int64(c-'0')
	}
	return n
}

func outcomeOf(tag []byte) string {
	switch {
	case bytes.HasPrefix(tag, []byte("COMMIT")):
		return "commit"
	case bytes.HasPrefix(tag, []byte("ROLLBACK")):
		return "rollback"
	}
	return ""
}

// parseError reads the fields of an ErrorResponse that are worth showing.
func parseError(body []byte) (e pgError) {
	localized := ""
	for len(body) > 1 {
		field := body[0]
		value, rest, _ := cstring(body[1:])
		body = rest
		switch field {
		case 'V': // the severity in English, whatever lc_messages says
			e.severity = value
		case 'S':
			localized = value
		case 'C':
			e.code = value
		case 'M':
			e.message = value
		}
	}
	if e.severity == "" {
		e.severity = localized
	}
	if e.message == "" {
		e.message = "error " + e.code
	}
	return e
}
