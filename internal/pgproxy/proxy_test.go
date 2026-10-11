package pgproxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/sean-brydon/leadline/internal/store"
)

type memSink struct {
	mu       sync.Mutex
	spans    []store.Span
	batches  int
	notified atomic.Int32
}

func (s *memSink) AddSpans(spans []store.Span) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.spans = append(s.spans, spans...)
	s.batches++
	return nil
}

func (s *memSink) take() []store.Span {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.spans
	s.spans = nil
	return out
}

// rig is a fake Postgres with a proxy in front of it.
type rig struct {
	t     *testing.T
	fake  *fakePG
	proxy *Proxy
	rec   *Recorder
	sink  *memSink
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, fake: newFakePG(t), sink: &memSink{}}
	// The recorder's own timer is out of the way here, so a test sees
	// exactly what its Flush wrote.
	r.rec = newRecorder(r.sink, func() { r.sink.notified.Add(1) }, time.Hour)
	p, err := Listen(Route{Listen: "127.0.0.1:0", Upstream: r.fake.addr()}, r.rec)
	if err != nil {
		t.Fatal(err)
	}
	r.proxy = p
	go p.Serve()
	t.Cleanup(func() {
		p.Close()
		r.rec.Close()
	})
	return r
}

func (r *rig) url(extra string) string {
	u := fmt.Sprintf("postgres://acme:%s@%s/acmedb?application_name=acme-api", fakePassword, r.proxy.Addr())
	if extra == "" {
		extra = "sslmode=disable"
	}
	return u + "&" + extra
}

func (r *rig) connect(mode pgx.QueryExecMode) *pgx.Conn {
	r.t.Helper()
	cfg, err := pgx.ParseConfig(r.url(""))
	if err != nil {
		r.t.Fatal(err)
	}
	cfg.DefaultQueryExecMode = mode
	conn, err := pgx.ConnectConfig(ctx(r.t), cfg)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// spans flushes the recorder and returns what it wrote since last time. By
// the time a client has its answer the proxy has already handed the
// statement to the recorder, so no waiting is needed.
func (r *rig) spans() []store.Span {
	r.rec.Flush()
	return r.sink.take()
}

// tracked fails the test if the proxy gave up reading any connection.
func (r *rig) tracked() {
	r.t.Helper()
	if n := r.proxy.fallbacks.Load(); n != 0 {
		r.t.Errorf("the proxy fell back to a plain relay on %d connections", n)
	}
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return c
}

func attrsOf(t *testing.T, sp store.Span) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(sp.Attrs), &m); err != nil {
		t.Fatalf("attrs %q: %v", sp.Attrs, err)
	}
	return m
}

func statements(spans []store.Span) []string {
	var out []string
	for _, sp := range spans {
		if sp.DBSystem != "" {
			out = append(out, sp.DBStatement)
		}
	}
	return out
}

func wantStatements(t *testing.T, spans []store.Span, want ...string) {
	t.Helper()
	got := statements(spans)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("statements:\n got %q\nwant %q", got, want)
	}
}

func TestSimpleQuery(t *testing.T) {
	r := newRig(t)
	conn := r.connect(pgx.QueryExecModeSimpleProtocol)
	before := time.Now().UnixNano()
	var v string
	if err := conn.QueryRow(ctx(t), "select 1 from acme_orders where id = 42").Scan(&v); err != nil {
		t.Fatal(err)
	}
	spans := r.spans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1: %+v", len(spans), spans)
	}
	sp := spans[0]
	want := store.Span{
		Service: "acme-api", Name: "SELECT", Kind: "client", Status: "ok",
		DBSystem: "postgresql", DBStatement: "select 1 from acme_orders where id = 42",
		DBFingerprint: "select ? from acme_orders where id = ?",
		Source:        store.SourcePostgresWire, DBName: "acmedb",
	}
	got := sp
	if got.DBServer == "" || !strings.HasPrefix(got.DBServer, "127.0.0.1:") {
		t.Errorf("the span does not say which server it ran against: %q", got.DBServer)
	}
	got.TraceID, got.SpanID, got.StartNs, got.DurationNs, got.Attrs, got.DBServer = "", "", 0, 0, "", ""
	if got != want {
		t.Errorf("span:\n got %+v\nwant %+v", got, want)
	}
	if len(sp.TraceID) != 32 || len(sp.SpanID) != 16 || sp.ParentID != "" {
		t.Errorf("ids: trace %q span %q parent %q", sp.TraceID, sp.SpanID, sp.ParentID)
	}
	if sp.StartNs < before || sp.DurationNs <= 0 || sp.StartNs+sp.DurationNs > time.Now().UnixNano() {
		t.Errorf("timing: start %d duration %d", sp.StartNs, sp.DurationNs)
	}
	a := attrsOf(t, sp)
	if a["db.namespace"] != "acmedb" || a["db.user"] != "acme" || a["db.rows"] != float64(1) {
		t.Errorf("attrs: %v", a)
	}
	if r.sink.notified.Load() != 1 {
		t.Errorf("notify ran %d times, want 1", r.sink.notified.Load())
	}
	r.tracked()
}

func TestMultiStatementSimpleQuery(t *testing.T) {
	r := newRig(t)
	conn := r.connect(pgx.QueryExecModeSimpleProtocol)
	sql := "select 1; insert into acme_orders values (2, 'a;b'); /* nothing */ ; update acme_orders set n = 3"
	results, err := conn.PgConn().Exec(ctx(t), sql).ReadAll()
	if err != nil || len(results) != 3 {
		t.Fatalf("results %d, err %v", len(results), err)
	}
	spans := r.spans()
	wantStatements(t, spans, "select 1", "insert into acme_orders values (2, 'a;b')", "update acme_orders set n = 3")
	for i, name := range []string{"SELECT", "INSERT", "UPDATE"} {
		if spans[i].Name != name {
			t.Errorf("span %d is %q, want %q", i, spans[i].Name, name)
		}
	}
	if rows := attrsOf(t, spans[2])["db.rows"]; rows != float64(3) {
		t.Errorf("update rows = %v, want 3", rows)
	}
	// The answers to a short script usually arrive together, and then the
	// proxy can't time its statements apart: each carries the time of all
	// of them, and says so. Otherwise a statement runs from the answer
	// before it to its own.
	for i := 1; i < len(spans); i++ {
		prev, sp := spans[i-1], spans[i]
		if sp.StartNs+sp.DurationNs == prev.StartNs+prev.DurationNs {
			if sp.StartNs != prev.StartNs || attrsOf(t, sp)["leadline.shared_timing"] != true ||
				attrsOf(t, prev)["leadline.shared_timing"] != true {
				t.Errorf("statements %d and %d were answered together but don't share their time", i-1, i)
			}
		} else if sp.StartNs != prev.StartNs+prev.DurationNs {
			t.Errorf("statement %d doesn't start where %d ends", i, i-1)
		}
	}

	// One that fails part way: the first statement ran, the second is the
	// error, the third never ran.
	_, err = conn.PgConn().Exec(ctx(t), "select 1; select boom; select 3").ReadAll()
	if err == nil {
		t.Fatal("no error")
	}
	spans = r.spans()
	wantStatements(t, spans, "select 1", "select boom")
	if spans[1].Status != "error" || !strings.Contains(spans[1].StatusMessage, "boom") {
		t.Errorf("failed statement: %+v", spans[1])
	}
	r.tracked()
}

func TestPreparedStatementRunSeveralTimes(t *testing.T) {
	r := newRig(t)
	conn := r.connect(pgx.QueryExecModeCacheStatement)
	const sql = "select $1::int from acme_users"
	for i := 0; i < 4; i++ {
		var v string
		if err := conn.QueryRow(ctx(t), sql, 40+i).Scan(&v); err != nil {
			t.Fatal(err)
		}
		if v != fmt.Sprint(40+i) {
			t.Fatalf("run %d returned %q", i, v)
		}
	}
	spans := r.spans()
	// Preparing is one round trip and running is four more; only the
	// runs are statements.
	wantStatements(t, spans, sql, sql, sql, sql)
	for i, sp := range spans {
		if sp.DBFingerprint != "select ?::int from acme_users" || sp.Name != "SELECT" {
			t.Errorf("run %d: fingerprint %q name %q", i, sp.DBFingerprint, sp.Name)
		}
		// pgx sends an int in binary once it knows the type; the proxy
		// learned the type from the same ParameterDescription.
		params, _ := attrsOf(t, sp)["db.query.parameters"].([]any)
		if len(params) != 1 || params[0] != float64(40+i) {
			t.Errorf("run %d: parameters %v", i, params)
		}
	}

	// A text parameter, and a named statement prepared by hand.
	if _, err := conn.Prepare(ctx(t), "acme_by_name", "select $1 from acme_users where name = $1"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ada", "grace"} {
		if _, err := conn.Exec(ctx(t), "acme_by_name", name); err != nil {
			t.Fatal(err)
		}
	}
	spans = r.spans()
	wantStatements(t, spans, "select $1 from acme_users where name = $1", "select $1 from acme_users where name = $1")
	if params := attrsOf(t, spans[1])["db.query.parameters"]; fmt.Sprint(params) != "[grace]" {
		t.Errorf("parameters %v", params)
	}
	r.tracked()
}

func TestPipelinedBatch(t *testing.T) {
	r := newRig(t)
	conn := r.connect(pgx.QueryExecModeCacheStatement)
	batch := &pgx.Batch{}
	batch.Queue("select $1 from acme_a", "one")
	batch.Queue("insert into acme_b values ($1)", "two")
	batch.Queue("select $1 from acme_c", "three")
	batch.Queue("select $1 from acme_a", "four")
	if err := conn.SendBatch(ctx(t), batch).Close(); err != nil {
		t.Fatal(err)
	}
	spans := r.spans()
	wantStatements(t, spans,
		"select $1 from acme_a", "insert into acme_b values ($1)", "select $1 from acme_c", "select $1 from acme_a")
	for i, want := range []string{"one", "two", "three", "four"} {
		if params := attrsOf(t, spans[i])["db.query.parameters"]; fmt.Sprint(params) != "["+want+"]" {
			t.Errorf("statement %d: parameters %v, want %s", i, params, want)
		}
	}
	// Every statement was sent before the first answer came back, so each
	// must have been running, by the proxy's clock, when the one before
	// it finished.
	for i := 1; i < len(spans); i++ {
		if spans[i].StartNs > spans[i-1].StartNs+spans[i-1].DurationNs {
			t.Errorf("statement %d wasn't pipelined behind %d", i, i-1)
		}
	}
	r.tracked()
}

func TestErrorMidPipeline(t *testing.T) {
	r := newRig(t)
	conn := r.connect(pgx.QueryExecModeCacheStatement)
	// Prepare first, so the batch itself is one pipeline of
	// Bind/Execute pairs behind a single Sync.
	for _, sql := range []string{"select 1 from acme_a", "select boom from acme_b", "select 3 from acme_c"} {
		if _, err := conn.Prepare(ctx(t), sql, sql); err != nil {
			t.Fatal(err)
		}
	}
	r.spans()

	batch := &pgx.Batch{}
	batch.Queue("select 1 from acme_a")
	batch.Queue("select boom from acme_b")
	batch.Queue("select 3 from acme_c")
	if err := conn.SendBatch(ctx(t), batch).Close(); err == nil {
		t.Fatal("the batch didn't fail")
	}
	spans := r.spans()
	// The server threw away what followed the error, so the third
	// statement never ran and has no span.
	wantStatements(t, spans, "select 1 from acme_a", "select boom from acme_b")
	if spans[0].Status != "ok" {
		t.Errorf("first statement: %+v", spans[0])
	}
	if spans[1].Status != "error" || spans[1].StatusMessage != `column "boom" does not exist` {
		t.Errorf("failed statement: %+v", spans[1])
	}
	if code := attrsOf(t, spans[1])["db.response.status_code"]; code != "42703" {
		t.Errorf("sqlstate %v", code)
	}

	// The connection is still in step afterwards.
	if _, err := conn.Exec(ctx(t), "select 3 from acme_c"); err != nil {
		t.Fatal(err)
	}
	wantStatements(t, r.spans(), "select 3 from acme_c")

	// A statement that doesn't parse is reported too, though it never ran.
	if _, err := conn.Exec(ctx(t), "select syntax_error from acme_a where id = $1", "1"); err == nil {
		t.Fatal("no error")
	}
	spans = r.spans()
	wantStatements(t, spans, "select syntax_error from acme_a where id = $1")
	if a := attrsOf(t, spans[0]); spans[0].Status != "error" || a["db.failed_at"] != "parse" {
		t.Errorf("parse failure: %+v", spans[0])
	}
	if _, err := conn.Exec(ctx(t), "select 1 from acme_a"); err != nil {
		t.Fatal(err)
	}
	wantStatements(t, r.spans(), "select 1 from acme_a")
	r.tracked()
}

func TestTransaction(t *testing.T) {
	r := newRig(t)
	conn := r.connect(pgx.QueryExecModeCacheStatement)
	if _, err := conn.Exec(ctx(t), "select 1 from acme_before"); err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx(t), "insert into acme_orders values ($1)", "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx(t), "update acme_orders set n = 1"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx(t), "select 1 from acme_after"); err != nil {
		t.Fatal(err)
	}

	spans := r.spans()
	wantStatements(t, spans, "select 1 from acme_before", "begin", "insert into acme_orders values ($1)",
		"update acme_orders set n = 1", "commit", "select 1 from acme_after")
	var root *store.Span
	byStatement := map[string]store.Span{}
	for i, sp := range spans {
		if sp.Name == "transaction" {
			root = &spans[i]
		}
		byStatement[sp.DBStatement] = sp
	}
	if root == nil {
		t.Fatal("no transaction span")
	}
	if root.ParentID != "" || root.DBSystem != "" || root.Kind != "internal" || root.Status != "ok" {
		t.Errorf("transaction span: %+v", *root)
	}
	if a := attrsOf(t, *root); a["db.transaction.statements"] != float64(4) || a["db.transaction.outcome"] != "commit" {
		t.Errorf("transaction attrs: %v", a)
	}
	begin, commit := byStatement["begin"], byStatement["commit"]
	for _, sql := range []string{"begin", "insert into acme_orders values ($1)", "update acme_orders set n = 1", "commit"} {
		sp := byStatement[sql]
		if sp.TraceID != root.TraceID || sp.ParentID != root.SpanID {
			t.Errorf("%q is not under the transaction: trace %s parent %s", sql, sp.TraceID, sp.ParentID)
		}
	}
	if root.StartNs != begin.StartNs || root.StartNs+root.DurationNs < commit.StartNs+commit.DurationNs {
		t.Errorf("transaction doesn't span its statements: %+v", *root)
	}
	for _, sql := range []string{"select 1 from acme_before", "select 1 from acme_after"} {
		if sp := byStatement[sql]; sp.TraceID == root.TraceID || sp.ParentID != "" {
			t.Errorf("%q is outside the transaction but shares its trace", sql)
		}
	}

	// A transaction that fails and is rolled back.
	tx, err = conn.Begin(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx(t), "select boom from acme_orders"); err == nil {
		t.Fatal("no error")
	}
	tx.Rollback(ctx(t))
	spans = r.spans()
	root = nil
	for i, sp := range spans {
		if sp.Name == "transaction" {
			root = &spans[i]
		}
	}
	if root == nil || root.Status != "error" || !strings.Contains(root.StatusMessage, "boom") {
		t.Fatalf("failed transaction span: %+v", root)
	}
	if a := attrsOf(t, *root); a["db.transaction.outcome"] != "rollback" {
		t.Errorf("outcome: %v", a)
	}
	r.tracked()
}

func TestTransactionLeftOpen(t *testing.T) {
	r := newRig(t)
	conn := r.connect(pgx.QueryExecModeSimpleProtocol)
	if _, err := conn.Exec(ctx(t), "begin"); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx(t))
	// The span is written when the proxy sees the connection end.
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, sp := range r.spans() {
			if sp.Name == "transaction" {
				if sp.Status != "error" || !strings.Contains(sp.StatusMessage, "connection closed") {
					t.Errorf("transaction span: %+v", sp)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no transaction span for a connection that closed mid-transaction")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestLargeResultStreams(t *testing.T) {
	r := newRig(t)
	r.fake.gate = make(chan struct{})
	conn := r.connect(pgx.QueryExecModeSimpleProtocol)

	// The server sends 100 rows and then waits. If the proxy were holding
	// the result until it was whole, none would reach the client.
	rows, err := conn.Query(ctx(t), "select gated big 20000")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for n < 100 && rows.Next() {
		n++
	}
	if n != 100 {
		t.Fatalf("got %d rows before the server finished, want 100: %v", n, rows.Err())
	}
	close(r.fake.gate)
	for rows.Next() {
		if len(rows.RawValues()[0]) != 1024 {
			t.Fatal("a row was damaged on the way through")
		}
		n++
	}
	if err := rows.Err(); err != nil || n != 20000 {
		t.Fatalf("rows %d, err %v", n, err)
	}
	spans := r.spans()
	wantStatements(t, spans, "select gated big 20000")
	if got := attrsOf(t, spans[0])["db.rows"]; got != float64(20000) {
		t.Errorf("rows = %v", got)
	}
	r.tracked()
}

func TestSSLRequest(t *testing.T) {
	r := newRig(t)

	// By hand: both kinds of encryption request are refused with "N", and
	// the connection stays open for the plaintext startup.
	raw, err := net.Dial("tcp", r.proxy.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(10 * time.Second))
	for _, code := range []uint32{gssEncRequest, sslRequest} {
		req := binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, 8), code)
		if _, err := raw.Write(req); err != nil {
			t.Fatal(err)
		}
		var answer [1]byte
		if _, err := io.ReadFull(raw, answer[:]); err != nil || answer[0] != 'N' {
			t.Fatalf("request %d: answer %q, err %v", code, answer, err)
		}
	}

	// A real client that would like TLS settles for plaintext.
	conn, err := pgx.Connect(ctx(t), r.url("sslmode=prefer"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx(t), "select 1"); err != nil {
		t.Fatal(err)
	}
	wantStatements(t, r.spans(), "select 1")

	// One that insists on TLS can't connect; that is the documented limit.
	if c, err := pgx.Connect(ctx(t), r.url("sslmode=require")); err == nil {
		c.Close(context.Background())
		t.Error("sslmode=require connected through a plaintext proxy")
	}

	r.fake.mu.Lock()
	defer r.fake.mu.Unlock()
	if r.fake.sawTLS {
		t.Error("an encryption request reached the server")
	}
}

func TestTraceparentComment(t *testing.T) {
	r := newRig(t)
	conn := r.connect(pgx.QueryExecModeCacheStatement)
	const (
		trace = "4bf92f3577b34da6a3ce929d0e0e4736"
		span  = "00f067aa0ba902b7"
		sql   = "select 1 from acme_users where id = $1 /*route='%2Fusers',traceparent='00-" + trace + "-" + span + "-01'*/"
	)
	if _, err := conn.Exec(ctx(t), sql, "7"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx(t), "select 1 from acme_users where id = $1", "7"); err != nil {
		t.Fatal(err)
	}
	spans := r.spans()
	if len(spans) != 2 {
		t.Fatalf("got %d spans", len(spans))
	}
	if spans[0].TraceID != trace || spans[0].ParentID != span || len(spans[0].SpanID) != 16 {
		t.Errorf("trace %q parent %q span %q", spans[0].TraceID, spans[0].ParentID, spans[0].SpanID)
	}
	if spans[0].DBStatement != sql {
		t.Errorf("statement %q", spans[0].DBStatement)
	}
	// With or without the comment it is the same query.
	if spans[0].DBFingerprint != spans[1].DBFingerprint {
		t.Errorf("fingerprints differ: %q and %q", spans[0].DBFingerprint, spans[1].DBFingerprint)
	}
	if spans[1].TraceID == trace || spans[1].ParentID != "" {
		t.Errorf("a statement with no comment joined the trace: %+v", spans[1])
	}

	// Inside a transaction the comment still wins for its own statement.
	tx, err := conn.Begin(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx(t), sql, "7"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx(t)); err != nil {
		t.Fatal(err)
	}
	for _, sp := range r.spans() {
		if sp.DBStatement == sql && (sp.TraceID != trace || sp.ParentID != span) {
			t.Errorf("in a transaction: trace %q parent %q", sp.TraceID, sp.ParentID)
		}
	}
	r.tracked()
}

// rawUpstream is a server that isn't Postgres: it records what it is sent
// and answers with whatever the test tells it to.
func rawUpstream(t *testing.T, answer func(c net.Conn)) (addr string, received func() string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var mu sync.Mutex
	var got bytes.Buffer
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				answer(c)
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					mu.Lock()
					got.Write(buf[:n])
					mu.Unlock()
					if err != nil {
						return
					}
					c.Write([]byte(upper(string(buf[:n]))))
				}
			}()
		}
	}()
	return ln.Addr().String(), func() string {
		mu.Lock()
		defer mu.Unlock()
		return got.String()
	}
}

// upper is what rawUpstream answers with: the same bytes, a-z in capitals.
func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 32
		}
	}
	return string(b)
}

func proxyTo(t *testing.T, upstream string) (*Proxy, *memSink, *Recorder) {
	t.Helper()
	sink := &memSink{}
	rec := newRecorder(sink, nil, time.Hour)
	p, err := Listen(Route{Listen: "127.0.0.1:0", Upstream: upstream}, rec)
	if err != nil {
		t.Fatal(err)
	}
	go p.Serve()
	t.Cleanup(func() {
		p.Close()
		rec.Close()
	})
	return p, sink, rec
}

func exchange(t *testing.T, c net.Conn, send, want string) {
	t.Helper()
	if _, err := c.Write([]byte(send)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != want {
		t.Fatalf("sent %q: got %q (err %v), want %q", send, got, err, want)
	}
}

func TestFallbackOnGarbage(t *testing.T) {
	t.Run("from the first byte", func(t *testing.T) {
		upstream, received := rawUpstream(t, func(net.Conn) {})
		p, _, _ := proxyTo(t, upstream)
		c, err := net.Dial("tcp", p.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(10 * time.Second))
		// Not Postgres at all; the upstream gets it untouched and its
		// answers come back.
		exchange(t, c, "get / http/1.1\r\n\r\n", "GET / HTTP/1.1\r\n\r\n")
		exchange(t, c, "more of the same", "MORE OF THE SAME")
		// A plausible startup length with an unknown code behind it.
		c2, err := net.Dial("tcp", p.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c2.Close()
		c2.SetDeadline(time.Now().Add(10 * time.Second))
		exchange(t, c2, "\x00\x00\x00\x10abcdefghijkl", "\x00\x00\x00\x10ABCDEFGHIJKL")
		if !strings.Contains(received(), "get / http/1.1\r\n\r\nmore of the same") {
			t.Errorf("upstream received %q", received())
		}
		if n := p.fallbacks.Load(); n != 2 {
			t.Errorf("fallbacks = %d, want 2", n)
		}
	})

	t.Run("after a good start", func(t *testing.T) {
		// The upstream accepts the startup like Postgres would, then
		// turns into an echo: everything after is nonsense as far as the
		// protocol goes, in both directions.
		ok := []byte{'R', 0, 0, 0, 8, 0, 0, 0, 0, 'Z', 0, 0, 0, 5, 'I'}
		upstream, received := rawUpstream(t, func(c net.Conn) {
			var n [4]byte
			io.ReadFull(c, n[:])
			io.CopyN(io.Discard, c, int64(binary.BigEndian.Uint32(n[:]))-4)
			c.Write(ok)
		})
		p, sink, rec := proxyTo(t, upstream)
		c, err := net.Dial("tcp", p.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(10 * time.Second))

		startup := binary.BigEndian.AppendUint32(nil, 196608)
		startup = append(startup, "user\x00acme\x00database\x00acmedb\x00\x00"...)
		startup = append(binary.BigEndian.AppendUint32(nil, uint32(len(startup)+4)), startup...)
		exchange(t, c, string(startup), string(ok))

		// A message type that doesn't exist, with a length that would
		// swallow the stream if the proxy waited for it.
		exchange(t, c, "~\x7f\xff\xff\xffnot a message", "~\x7f\xff\xff\xffNOT A MESSAGE")
		// And a well-formed query after it: the proxy is no longer
		// reading, so it goes through like any other bytes.
		query := "Q\x00\x00\x00\x0dselect 1\x00"
		exchange(t, c, query, upper(query))
		// A megabyte in one go still arrives whole.
		big := strings.Repeat("abcdefgh", 1<<17)
		exchange(t, c, big, upper(big))

		if !strings.HasSuffix(received(), big) {
			t.Error("upstream didn't receive the bytes as sent")
		}
		if n := p.fallbacks.Load(); n != 1 {
			t.Errorf("fallbacks = %d, want 1", n)
		}
		rec.Flush()
		if spans := sink.take(); len(spans) != 0 {
			t.Errorf("spans from a connection that was only relayed: %+v", spans)
		}
	})
}

func TestCopy(t *testing.T) {
	r := newRig(t)
	conn := r.connect(pgx.QueryExecModeCacheStatement)

	tag, err := conn.PgConn().CopyFrom(ctx(t), strings.NewReader("1\tada\n2\tgrace\n3\tjoan\n"), "copy acme_users from stdin")
	if err != nil || tag.RowsAffected() != 3 {
		t.Fatalf("copy in: %v, %v", tag, err)
	}
	var out bytes.Buffer
	tag, err = conn.PgConn().CopyTo(ctx(t), &out, "copy acme_users to stdout")
	if err != nil || tag.RowsAffected() != 3 || out.String() != "row 0\nrow 1\nrow 2\n" {
		t.Fatalf("copy out: %v, %v, %q", tag, err, out.String())
	}
	if _, err := conn.Exec(ctx(t), "select 1 from acme_users"); err != nil {
		t.Fatal(err)
	}
	spans := r.spans()
	wantStatements(t, spans, "copy acme_users from stdin", "copy acme_users to stdout", "select 1 from acme_users")
	if spans[0].Name != "COPY" || attrsOf(t, spans[0])["db.rows"] != float64(3) {
		t.Errorf("copy span: %+v", spans[0])
	}
	r.fake.mu.Lock()
	copied := strings.Join(r.fake.copied, "")
	r.fake.mu.Unlock()
	if copied != "1\tada\n2\tgrace\n3\tjoan\n" {
		t.Errorf("server received %q", copied)
	}
	r.tracked()
}

// TestCopyInExtendedProtocol drives COPY FROM STDIN through Parse, Bind and
// Execute by hand. The Sync that follows the Execute is swallowed by the
// server's copy mode, so the proxy must not wait for its ReadyForQuery.
func TestCopyInExtendedProtocol(t *testing.T) {
	r := newRig(t)
	conn := r.connect(pgx.QueryExecModeCacheStatement)
	pg := conn.PgConn()
	fe := pg.Frontend()
	fe.SendParse(&pgproto3.Parse{Query: "copy acme_users from stdin"})
	fe.SendBind(&pgproto3.Bind{})
	fe.SendExecute(&pgproto3.Execute{})
	fe.SendSync(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	receive := func(want string) {
		t.Helper()
		msg, err := fe.Receive()
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%T", msg); got != "*pgproto3."+want {
			t.Fatalf("got %s, want %s (%+v)", got, want, msg)
		}
	}
	receive("ParseComplete")
	receive("BindComplete")
	receive("CopyInResponse")
	fe.Send(&pgproto3.CopyData{Data: []byte("1\tada\n")})
	fe.Send(&pgproto3.CopyDone{})
	fe.SendSync(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	receive("CommandComplete")
	receive("ReadyForQuery")

	if _, err := conn.Exec(ctx(t), "select 1 from acme_users"); err != nil {
		t.Fatal(err)
	}
	wantStatements(t, r.spans(), "copy acme_users from stdin", "select 1 from acme_users")
	r.tracked()
}

func TestCancelRequestPassesThrough(t *testing.T) {
	r := newRig(t)
	conn := r.connect(pgx.QueryExecModeSimpleProtocol)
	if err := conn.PgConn().CancelRequest(ctx(t)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.fake.mu.Lock()
		cancels := append([]uint32(nil), r.fake.cancels...)
		r.fake.mu.Unlock()
		if len(cancels) == 1 && cancels[0] == 4242 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server saw cancel requests %v, want [4242]", cancels)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPasswordsAreNotRecorded(t *testing.T) {
	r := newRig(t)
	conn := r.connect(pgx.QueryExecModeSimpleProtocol)
	if _, err := conn.Exec(ctx(t), "alter role acme with password 'hunter2-acme'"); err != nil {
		t.Fatal(err)
	}
	spans := r.spans()
	wantStatements(t, spans, "alter role acme with password '***'")
	if spans[0].DBFingerprint != "alter role acme with password ?" {
		t.Errorf("fingerprint %q", spans[0].DBFingerprint)
	}
	r.fake.mu.Lock()
	authenticated := r.fake.password == fakePassword
	r.fake.mu.Unlock()
	if !authenticated {
		t.Fatal("the password didn't reach the server")
	}
	for _, sp := range spans {
		if all := fmt.Sprintf("%+v", sp); strings.Contains(all, fakePassword) || strings.Contains(all, "hunter2") {
			t.Errorf("a password was recorded: %s", all)
		}
	}
}

func TestUpstreamDown(t *testing.T) {
	// A port nothing listens on.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()
	p, _, _ := proxyTo(t, dead)

	_, err = pgx.Connect(ctx(t), fmt.Sprintf("postgres://acme@%s/acmedb?sslmode=disable", p.Addr()))
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) {
		t.Fatalf("want a postgres error, got %v", err)
	}
	if pgErr.Code != "08001" || !strings.Contains(pgErr.Message, "could not reach Postgres at "+dead) {
		t.Errorf("error: %+v", pgErr)
	}
}

func TestServiceFollowsApplicationName(t *testing.T) {
	r := newRig(t)
	cfg, err := pgx.ParseConfig(fmt.Sprintf("postgres://acme:%s@%s/acmedb?sslmode=disable", fakePassword, r.proxy.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	conn, err := pgx.ConnectConfig(ctx(t), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx(t), "select 1"); err != nil {
		t.Fatal(err)
	}
	// No application_name: the database's name stands in.
	if spans := r.spans(); len(spans) != 1 || spans[0].Service != "acmedb" {
		t.Fatalf("spans %+v", spans)
	}
}

func TestRecorderBatches(t *testing.T) {
	sink := &memSink{}
	rec := newRecorder(sink, func() { sink.notified.Add(1) }, time.Hour)
	st := newStmt("select 1", false)
	m := &meta{service: "acme", database: "acmedb"}
	for i := 0; i < 50; i++ {
		rec.add([]event{{st: st, meta: m, traceID: newTraceID(), spanID: newSpanID(), start: 1, end: 2, rows: -1}})
	}
	sink.mu.Lock()
	early := len(sink.spans)
	sink.mu.Unlock()
	if early != 0 {
		t.Fatalf("%d spans were written before a flush", early)
	}
	// Closing writes what is waiting, in one go.
	rec.Close()
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.spans) != 50 || sink.batches != 1 || sink.notified.Load() != 1 {
		t.Errorf("spans %d in %d batches, notified %d", len(sink.spans), sink.batches, sink.notified.Load())
	}
}

func TestRecorderFlushesOnItsOwn(t *testing.T) {
	sink := &memSink{}
	rec := newRecorder(sink, nil, 5*time.Millisecond)
	defer rec.Close()
	rec.add([]event{{st: newStmt("select 1", false), meta: &meta{}, start: 1, end: 2}})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sink.mu.Lock()
		n := len(sink.spans)
		sink.mu.Unlock()
		if n == 1 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the recorder never flushed")
}

func TestManyConnectionsAtOnce(t *testing.T) {
	r := newRig(t)
	const clients, each = 8, 40
	var wg sync.WaitGroup
	errs := make(chan error, clients)
	for c := 0; c < clients; c++ {
		conn := r.connect(pgx.QueryExecModeCacheStatement)
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				var v string
				want := fmt.Sprintf("c%d-%d", c, i)
				if err := conn.QueryRow(ctx(t), "select $1 from acme_load", want).Scan(&v); err != nil || v != want {
					errs <- fmt.Errorf("client %d: got %q, err %v", c, v, err)
					return
				}
			}
		}(c)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if spans := r.spans(); len(spans) != clients*each {
		t.Errorf("got %d spans, want %d", len(spans), clients*each)
	}
	r.tracked()
}

// Messages far larger than the proxy keeps still arrive whole, and what is
// recorded of them is bounded.
func TestLargeStatementsAndParameters(t *testing.T) {
	r := newRig(t)
	conn := r.connect(pgx.QueryExecModeCacheStatement)

	// A megabyte parameter, echoed back as a megabyte value.
	big := strings.Repeat("0123456789abcdef", 1<<16)
	var back string
	if err := conn.QueryRow(ctx(t), "select $1 from acme_blobs where k = $2", big, "after").Scan(&back); err != nil {
		t.Fatal(err)
	}
	if back != big+",after" {
		t.Fatalf("the value came back changed: %d bytes", len(back))
	}
	spans := r.spans()
	wantStatements(t, spans, "select $1 from acme_blobs where k = $2")
	params, _ := attrsOf(t, spans[0])["db.query.parameters"].([]any)
	if first, _ := params[0].(string); len(params) != 1 || len(first) > maxParam+8 || !strings.HasSuffix(first, "…") {
		t.Errorf("parameters of a huge Bind: %.300v", params)
	}

	// A megabyte of SQL, by both protocols.
	sql := "insert into acme_blobs values ('x" + strings.Repeat("é", 1<<19) + "', $1)"
	if _, err := conn.Exec(ctx(t), sql, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(ctx(t), strings.Replace(sql, "$1", "'x'", 1)).ReadAll(); err != nil {
		t.Fatal(err)
	}
	spans = r.spans()
	if len(spans) != 2 {
		t.Fatalf("%d spans", len(spans))
	}
	for _, sp := range spans {
		if len(sp.DBStatement) > maxSQL+16 || !strings.HasSuffix(sp.DBStatement, " …") || !utf8.ValidString(sp.DBStatement) {
			t.Errorf("statement kept: %d bytes, ends %q", len(sp.DBStatement), sp.DBStatement[len(sp.DBStatement)-8:])
		}
		// Cut off inside a literal, it still groups as the same insert.
		if sp.DBFingerprint != "insert into acme_blobs values (?" || attrsOf(t, sp)["db.query.truncated"] != true {
			t.Errorf("fingerprint %.80q attrs %s", sp.DBFingerprint, sp.Attrs)
		}
	}
	r.tracked()
}

// What the daemon does: start, proxy, and on the way out write what the
// timer hadn't got to yet.
func TestGroupWritesWhatIsLeftOnClose(t *testing.T) {
	fake := newFakePG(t)
	sink := &memSink{}
	g := Start([]Route{{Listen: "127.0.0.1:0", Upstream: fake.addr()}}, sink, func() { sink.notified.Add(1) })
	if len(g.proxies) != 1 {
		t.Fatal("the proxy didn't start")
	}
	url := fmt.Sprintf("postgres://acme:%s@%s/acmedb?sslmode=disable", fakePassword, g.proxies[0].Addr())
	conn, err := pgx.Connect(ctx(t), url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx(t), "begin"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx(t), "insert into acme_orders values (1)"); err != nil {
		t.Fatal(err)
	}
	// Close with the connection open and its transaction unfinished.
	g.Close()
	spans := sink.take()
	wantStatements(t, spans, "begin", "insert into acme_orders values (1)")
	if last := spans[len(spans)-1]; last.Name != "transaction" || last.Status != "error" {
		t.Errorf("the open transaction: %+v", last)
	}
	if sink.notified.Load() == 0 {
		t.Error("notify never ran")
	}
	if _, err := conn.Exec(ctx(t), "select 1"); err == nil {
		t.Error("the connection outlived the proxy")
	}

	// Nothing to proxy is fine too, and a port that is taken is skipped.
	Start(nil, sink, nil).Close()
	taken := Start([]Route{{Listen: fake.addr(), Upstream: fake.addr()}}, sink, nil)
	if len(taken.proxies) != 0 {
		t.Error("listened on a port that was in use")
	}
	taken.Close()
}

func TestUnixSocketUpstream(t *testing.T) {
	dir, err := os.MkdirTemp("", "pg")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, ".s.PGSQL.5432")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Skip("no unix sockets here:", err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(c, c)
	}()
	routes, err := ParseRoutes("127.0.0.1:0=" + path)
	if err != nil {
		t.Fatal(err)
	}
	p, _, _ := proxyTo(t, routes[0].Upstream)
	c, err := net.Dial("tcp", p.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	exchange(t, c, "anything at all", "anything at all")
}
