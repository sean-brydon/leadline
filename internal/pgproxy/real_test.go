package pgproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

// The tests in this file run against a real Postgres, which the usual run
// doesn't have: they are skipped unless LEADLINE_TEST_POSTGRES holds its
// URL (postgres://user:password@127.0.0.1:5432/db). They are how to check
// the proxy against a new server version or a protocol corner the fake
// server only imitates.

func realRig(t testing.TB) (direct string, proxied string, p *Proxy, sink *memSink, rec *Recorder) {
	t.Helper()
	direct = os.Getenv("LEADLINE_TEST_POSTGRES")
	if direct == "" {
		t.Skip("LEADLINE_TEST_POSTGRES is not set")
	}
	cfg, err := pgconn.ParseConfig(direct)
	if err != nil {
		t.Fatal(err)
	}
	sink = &memSink{}
	rec = newRecorder(sink, nil, time.Hour)
	upstream := net.JoinHostPort(cfg.Host, fmt.Sprint(cfg.Port))
	p, err = Listen(Route{Listen: "127.0.0.1:0", Upstream: upstream}, rec)
	if err != nil {
		t.Fatal(err)
	}
	go p.Serve()
	t.Cleanup(func() {
		p.Close()
		rec.Close()
	})
	proxied = strings.Replace(direct, upstream, p.Addr().String(), 1)
	if proxied == direct {
		t.Fatalf("can't point %q at the proxy", direct)
	}
	if !strings.Contains(proxied, "sslmode=") {
		sep := "?"
		if strings.Contains(proxied, "?") {
			sep = "&"
		}
		proxied += sep + "sslmode=disable"
	}
	return direct, proxied, p, sink, rec
}

func TestRealPostgres(t *testing.T) {
	_, url, p, sink, rec := realRig(t)
	taken := func() []string {
		rec.Flush()
		return statements(sink.take())
	}
	modes := map[string]pgx.QueryExecMode{
		"cache statement": pgx.QueryExecModeCacheStatement,
		"cache describe":  pgx.QueryExecModeCacheDescribe,
		"describe exec":   pgx.QueryExecModeDescribeExec,
		"exec":            pgx.QueryExecModeExec,
		"simple":          pgx.QueryExecModeSimpleProtocol,
	}
	for name, mode := range modes {
		t.Run(name, func(t *testing.T) {
			cfg, err := pgx.ParseConfig(url)
			if err != nil {
				t.Fatal(err)
			}
			cfg.DefaultQueryExecMode = mode
			conn, err := pgx.ConnectConfig(ctx(t), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close(context.Background())
			c := ctx(t)
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			count := func(want int, what string) {
				t.Helper()
				if got := taken(); len(got) != want {
					t.Errorf("%s: %d statements recorded, want %d: %q", what, len(got), want, got)
				}
				if n := p.fallbacks.Load(); n != 0 {
					t.Fatalf("%s: the proxy lost its place", what)
				}
			}

			_, err = conn.Exec(c, "create temp table acme_items (id int primary key, name text, qty int)")
			must(err)
			count(1, "create")

			for i := 1; i <= 5; i++ {
				_, err = conn.Exec(c, "insert into acme_items values ($1, $2, $3)", i, fmt.Sprintf("item %d", i), i*10)
				must(err)
			}
			count(5, "inserts")

			var name string
			var qty int
			must(conn.QueryRow(c, "select name, qty from acme_items where id = $1", 3).Scan(&name, &qty))
			if name != "item 3" || qty != 30 {
				t.Errorf("got %q, %d", name, qty)
			}
			count(1, "select")

			// A null and an empty result.
			rows, err := conn.Query(c, "select id from acme_items where name = $1", nil)
			must(err)
			rows.Close()
			must(rows.Err())
			count(1, "null parameter")

			// An error at run time, then the connection still in step.
			if _, err = conn.Exec(c, "select 1 / (qty - qty) from acme_items where id = $1", 1); err == nil {
				t.Error("division by zero succeeded")
			}
			// One at parse time.
			if _, err = conn.Exec(c, "select nothing_like_this from acme_items where id = $1", 1); err == nil {
				t.Error("unknown column succeeded")
			}
			// And one in a parameter's value, which fails at Bind when
			// the value goes as text.
			if _, err = conn.Exec(c, "select $1::int", "not a number"); err == nil {
				t.Error("bad integer succeeded")
			}
			rec.Flush()
			failed := sink.take()
			for _, sp := range failed {
				if sp.Status != "error" || sp.StatusMessage == "" {
					t.Errorf("a failed statement was recorded as %q: %s", sp.Status, sp.DBStatement)
				}
			}
			if len(failed) != 3 {
				t.Errorf("%d failed statements recorded, want 3: %q", len(failed), statements(failed))
			}
			must(conn.QueryRow(c, "select count(*) from acme_items").Scan(&qty))
			count(1, "after errors")

			// A transaction.
			tx, err := conn.Begin(c)
			must(err)
			_, err = tx.Exec(c, "update acme_items set qty = qty + 1 where id = $1", 1)
			must(err)
			_, err = tx.Exec(c, "savepoint s")
			must(err)
			if _, err = tx.Exec(c, "insert into acme_items values ($1, $2, $3)", 1, "twice", 0); err == nil {
				t.Error("duplicate key succeeded")
			}
			_, err = tx.Exec(c, "rollback to savepoint s")
			must(err)
			must(tx.Commit(c))
			rec.Flush()
			spans := sink.take()
			traces := map[string]int{}
			roots := 0
			for _, sp := range spans {
				traces[sp.TraceID]++
				if sp.Name == "transaction" {
					roots++
				}
			}
			// begin, update, savepoint, insert, rollback to, commit, and
			// the transaction's own span.
			if len(traces) != 1 || len(spans) != 7 || roots != 1 {
				t.Errorf("transaction: %d spans in %d traces, %d roots: %q", len(spans), len(traces), roots, statements(spans))
			}

			// A pipelined batch, then one with an error in the middle.
			batch := &pgx.Batch{}
			for i := 1; i <= 4; i++ {
				batch.Queue("select name from acme_items where id = $1", i)
			}
			batch.Queue("update acme_items set qty = $1 where id = $2", 99, 2)
			must(conn.SendBatch(c, batch).Close())
			count(5, "batch")

			batch = &pgx.Batch{}
			batch.Queue("select name from acme_items where id = $1", 1)
			batch.Queue("select 1 / (qty - qty) from acme_items where id = $1", 1)
			batch.Queue("select name from acme_items where id = $1", 2)
			if err := conn.SendBatch(c, batch).Close(); err == nil {
				t.Error("the batch didn't fail")
			}
			rec.Flush()
			spans = sink.take()
			if got := statements(spans); len(got) != 2 || spans[len(spans)-1].Status != "error" {
				t.Errorf("failed batch: %q", got)
			}
			if n := p.fallbacks.Load(); n != 0 {
				t.Fatal("failed batch: the proxy lost its place")
			}

			// Several statements in one simple query, with an empty one.
			results, err := conn.PgConn().Exec(c, "select 1; ; select 'a;b', $$c;d$$; update acme_items set qty = 1 where id = 5").ReadAll()
			must(err)
			if len(results) != 3 {
				t.Errorf("%d results", len(results))
			}
			if got := taken(); len(got) != 3 || got[1] != "select 'a;b', $$c;d$$" {
				t.Errorf("multi-statement query: %q", got)
			}
			_, err = conn.PgConn().Exec(c, "").ReadAll()
			must(err)
			count(0, "empty query")

			// COPY in both directions; pgx sends rows in binary.
			n, err := conn.CopyFrom(c, pgx.Identifier{"acme_items"}, []string{"id", "name", "qty"},
				pgx.CopyFromSlice(2000, func(i int) ([]any, error) {
					return []any{1000 + i, strings.Repeat("n", 200), i}, nil
				}))
			must(err)
			if n != 2000 {
				t.Errorf("copied %d rows", n)
			}
			rec.Flush()
			spans = sink.take()
			if last := spans[len(spans)-1]; last.Name != "COPY" || !strings.Contains(last.Attrs, `"db.rows":2000`) {
				t.Errorf("copy in: %+v", last)
			}
			var out strings.Builder
			_, err = conn.PgConn().CopyTo(c, &out, "copy acme_items to stdout")
			must(err)
			if got := strings.Count(out.String(), "\n"); got != 2005 {
				t.Errorf("copied out %d rows", got)
			}
			count(1, "copy out")

			// A result far larger than the proxy's buffers.
			rows, err = conn.Query(c, "select g, repeat('x', 500) from generate_series(1, $1::int) g", 100000)
			must(err)
			got := 0
			for rows.Next() {
				got++
			}
			must(rows.Err())
			if got != 100000 {
				t.Errorf("%d rows", got)
			}
			rec.Flush()
			if spans = sink.take(); len(spans) != 1 || !strings.Contains(spans[0].Attrs, `"db.rows":100000`) {
				t.Errorf("large result: %+v", spans)
			}

			// A notification arrives when it likes.
			_, err = conn.Exec(c, "listen acme_events")
			must(err)
			_, err = conn.Exec(c, "select pg_notify('acme_events', 'hello')")
			must(err)
			note, err := conn.WaitForNotification(c)
			must(err)
			if note.Payload != "hello" {
				t.Errorf("notification %+v", note)
			}
			count(2, "notify")

			// A statement cancelled from another connection.
			go func() {
				time.Sleep(150 * time.Millisecond)
				conn.PgConn().CancelRequest(context.Background())
			}()
			_, err = conn.Exec(c, "select pg_sleep(20)")
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
				t.Errorf("cancelled sleep: %v", err)
			}
			rec.Flush()
			if spans = sink.take(); len(spans) != 1 || spans[0].Status != "error" || spans[0].DurationNs > int64(10*time.Second) {
				t.Errorf("cancelled statement: %+v", spans)
			}
			must(conn.QueryRow(c, "select 1").Scan(&qty))
			count(1, "after cancel")
		})
	}
}

// TestRealPostgresByHand covers what pgx has no call for: a portal fetched
// a few rows at a time, COPY FROM STDIN through the extended protocol, and
// an error that only surfaces at Sync.
func TestRealPostgresByHand(t *testing.T) {
	_, url, p, sink, rec := realRig(t)
	conn, err := pgx.Connect(ctx(t), url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	fe := conn.PgConn().Frontend()
	flush := func() {
		t.Helper()
		if err := fe.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	// until reads messages up to the named one and returns their names.
	until := func(last string) []string {
		t.Helper()
		var seen []string
		for {
			msg, err := fe.Receive()
			if err != nil {
				t.Fatalf("after %v: %v", seen, err)
			}
			name := strings.TrimPrefix(fmt.Sprintf("%T", msg), "*pgproto3.")
			if e, ok := msg.(*pgproto3.ErrorResponse); ok {
				name += "(" + e.Code + ")"
			}
			if name == "DataRow" || name == "ParameterStatus" || name == "NoticeResponse" {
				continue
			}
			seen = append(seen, name)
			if name == last {
				return seen
			}
		}
	}
	taken := func() []string {
		t.Helper()
		rec.Flush()
		if n := p.fallbacks.Load(); n != 0 {
			t.Fatal("the proxy lost its place")
		}
		var out []string
		for _, sp := range sink.take() {
			out = append(out, fmt.Sprintf("%s|%s|%s", sp.Name, sp.Status, sp.Attrs))
		}
		return out
	}

	// A portal read three rows at a time.
	fe.SendParse(&pgproto3.Parse{Query: "select generate_series(1, 7)"})
	fe.SendBind(&pgproto3.Bind{})
	for i := 0; i < 3; i++ {
		fe.SendExecute(&pgproto3.Execute{MaxRows: 3})
	}
	fe.SendSync(&pgproto3.Sync{})
	flush()
	got := strings.Join(until("ReadyForQuery"), " ")
	if got != "ParseComplete BindComplete PortalSuspended PortalSuspended CommandComplete ReadyForQuery" {
		t.Fatalf("portal: %s", got)
	}
	spans := taken()
	if len(spans) != 3 || !strings.Contains(spans[0], `"db.portal.suspended":true`) || !strings.Contains(spans[0], `"db.rows":3`) {
		t.Errorf("portal fetches: %q", spans)
	}

	// COPY FROM STDIN by Parse/Bind/Execute. The server swallows the Sync
	// that follows the Execute and wants another after the data.
	if _, err := conn.Exec(ctx(t), "create temp table acme_copy (id int, name text)"); err != nil {
		t.Fatal(err)
	}
	taken()
	fe.SendParse(&pgproto3.Parse{Query: "copy acme_copy from stdin"})
	fe.SendBind(&pgproto3.Bind{})
	fe.SendExecute(&pgproto3.Execute{})
	fe.SendSync(&pgproto3.Sync{})
	flush()
	until("CopyInResponse")
	fe.Send(&pgproto3.CopyData{Data: []byte("1\tada\n2\tgrace\n")})
	fe.Send(&pgproto3.CopyDone{})
	fe.SendSync(&pgproto3.Sync{})
	flush()
	if got := strings.Join(until("ReadyForQuery"), " "); got != "CommandComplete ReadyForQuery" {
		t.Fatalf("copy: %s", got)
	}
	if spans = taken(); len(spans) != 1 || !strings.HasPrefix(spans[0], "COPY|ok|") || !strings.Contains(spans[0], `"db.rows":2`) {
		t.Errorf("extended copy: %q", spans)
	}

	// The same, with data the server rejects part way.
	fe.SendParse(&pgproto3.Parse{Query: "copy acme_copy from stdin"})
	fe.SendBind(&pgproto3.Bind{})
	fe.SendExecute(&pgproto3.Execute{})
	fe.SendSync(&pgproto3.Sync{})
	flush()
	until("CopyInResponse")
	fe.Send(&pgproto3.CopyData{Data: []byte("not a number\tada\n")})
	fe.Send(&pgproto3.CopyDone{})
	fe.SendSync(&pgproto3.Sync{})
	flush()
	if got := strings.Join(until("ReadyForQuery"), " "); got != "ErrorResponse(22P02) ReadyForQuery" {
		t.Fatalf("failed copy: %s", got)
	}
	if spans = taken(); len(spans) != 1 || !strings.HasPrefix(spans[0], "COPY|error|") {
		t.Errorf("failed extended copy: %q", spans)
	}

	// A deferred constraint fails when the implicit transaction commits,
	// which is at Sync: after the statement itself has been answered.
	if _, err := conn.Exec(ctx(t), "create temp table acme_once (id int unique deferrable initially deferred)"); err != nil {
		t.Fatal(err)
	}
	taken()
	fe.SendParse(&pgproto3.Parse{Query: "insert into acme_once values (1), (1)"})
	fe.SendBind(&pgproto3.Bind{})
	fe.SendExecute(&pgproto3.Execute{})
	fe.SendSync(&pgproto3.Sync{})
	flush()
	if got := strings.Join(until("ReadyForQuery"), " "); got != "ParseComplete BindComplete CommandComplete ErrorResponse(23505) ReadyForQuery" {
		t.Fatalf("deferred constraint: %s", got)
	}
	taken()

	// And the connection is in step after all that.
	var one int
	if err := conn.QueryRow(ctx(t), "select 1").Scan(&one); err != nil {
		t.Fatal(err)
	}
	if spans = taken(); len(spans) != 1 {
		t.Errorf("afterwards: %q", spans)
	}
}

// BenchmarkRealPostgres measures what the proxy adds: the same work sent
// straight to the server and through the proxy.
func BenchmarkRealPostgres(b *testing.B) {
	direct, proxied, _, _, rec := realRig(b)
	// Record as the daemon does, on the recorder's own clock.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		tick := time.NewTicker(flushEvery)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				rec.Flush()
			case <-stop:
				return
			}
		}
	}()
	// A relay that only copies bytes is the floor for anything in the
	// path: what the proxy costs beyond it is the price of reading.
	cfg, err := pgconn.ParseConfig(direct)
	if err != nil {
		b.Fatal(err)
	}
	upstream := net.JoinHostPort(cfg.Host, fmt.Sprint(cfg.Port))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s, err := net.Dial("tcp", upstream)
			if err != nil {
				c.Close()
				continue
			}
			go func() { io.Copy(s, c); s.Close() }()
			go func() { io.Copy(c, s); c.Close() }()
		}
	}()
	relayed := strings.Replace(direct, upstream, ln.Addr().String(), 1)

	for _, target := range []struct{ name, url string }{{"direct", direct}, {"relay", relayed}, {"proxy", proxied}} {
		conn, err := pgx.Connect(context.Background(), target.url)
		if err != nil {
			b.Fatal(err)
		}
		b.Run("point-select/"+target.name, func(b *testing.B) {
			var v int
			for b.Loop() {
				if err := conn.QueryRow(context.Background(), "select $1::int", 7).Scan(&v); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run("100k-rows/"+target.name, func(b *testing.B) {
			for b.Loop() {
				rows, err := conn.Query(context.Background(), "select g, repeat('x', 100) from generate_series(1, 100000) g")
				if err != nil {
					b.Fatal(err)
				}
				for rows.Next() {
				}
				if err := rows.Err(); err != nil {
					b.Fatal(err)
				}
			}
		})
		conn.Close(context.Background())
	}
}
