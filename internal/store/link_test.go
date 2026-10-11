package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"

	"github.com/sean-brydon/leadline/internal/store"
)

// These tests are about what a span counts as: a statement seen two ways
// is one query, BEGIN and COMMIT are none, and a request is counted the
// same way by every view.

var linkEpoch = time.Now().Add(-time.Minute).UnixNano()

func ms(n float64) int64 { return int64(n * 1e6) }

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

const customerSQL = "SELECT name FROM customers WHERE id = $1"

// request is one request of an app that is seen two ways: its own spans
// (a server span and a pg span for each statement) and the Postgres proxy's
// span for each statement, timed inside the app's.
type request struct {
	trace      string
	app, wire  []store.Span
	statements int
}

// orders makes a request that runs one query for the orders and then one
// per order: an N+1 of n. linked says the app put a traceparent comment on
// each statement, so the proxy's spans name the app's trace themselves.
func orders(i, n int, linked bool) request {
	r := request{trace: fmt.Sprintf("%032x", i+1), statements: n + 1}
	root := fmt.Sprintf("%016x", i*1000+1)
	at := linkEpoch + int64(i)*ms(200)
	sqls := []string{"SELECT id, customer_id FROM orders ORDER BY id LIMIT 25"}
	for k := 0; k < n; k++ {
		sqls = append(sqls, customerSQL)
	}
	cur := at + ms(1)
	for k, q := range sqls {
		appID := fmt.Sprintf("%016x", i*1000+10+k)
		// The app waits 2ms for each; 1.2ms of it is the database's.
		r.app = append(r.app, store.Span{TraceID: r.trace, SpanID: appID, ParentID: root, Service: "acme-orders",
			Name: "pg.query:SELECT acme", Kind: "client", StartNs: cur, DurationNs: ms(2), Status: "unset",
			DBSystem: "postgresql", DBStatement: q, DBFingerprint: strings.ReplaceAll(q, "$1", "?"), DBName: "acme", Attrs: "{}"})
		w := store.Span{TraceID: fmt.Sprintf("%032x", 0xfff000+i*100+k), SpanID: fmt.Sprintf("%016x", i*1000+500+k),
			Service: "acme", Name: "SELECT", Kind: "client", StartNs: cur + ms(0.4), DurationNs: ms(1.2), Status: "ok",
			DBSystem: "postgresql", DBStatement: q, DBFingerprint: strings.ReplaceAll(q, "$1", "?"), DBName: "acme",
			Attrs: `{"db.rows":1,"leadline.source":"pgproxy"}`, Source: store.SourcePostgresWire}
		if linked {
			w.TraceID, w.ParentID, w.Linked = r.trace, appID, true
		}
		r.wire = append(r.wire, w)
		cur += ms(2.5)
	}
	r.app = append(r.app, store.Span{TraceID: r.trace, SpanID: root, Service: "acme-orders", Name: "GET /orders",
		Kind: "server", StartNs: at, DurationNs: cur - at + ms(1), Status: "unset", HTTPRoute: "/orders", Attrs: "{}"})
	return r
}

func add(t *testing.T, st *store.Store, spans ...[]store.Span) {
	t.Helper()
	for _, batch := range spans {
		if err := st.AddSpans(batch); err != nil {
			t.Fatal(err)
		}
	}
	st.Flush()
}

func customerQuery(t *testing.T, st *store.Store) store.QueryRow {
	t.Helper()
	rows, err := st.Queries(context.Background(), store.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if strings.Contains(r.Fingerprint, "FROM customers") {
			return r
		}
	}
	t.Fatalf("no customers query among %+v", rows)
	return store.QueryRow{}
}

func window() store.Window {
	return store.Window{SinceMs: float64(linkEpoch)/1e6 - 1000, UntilMs: float64(linkEpoch)/1e6 + 60_000}
}

// The heart of it: an N+1 of 25, seen by the app and by the proxy, is 25,
// in whichever order and however split the two arrive, and the query that
// is counted has the app's trace and the wire's timing.
func TestAStatementSeenTwiceIsOneQuery(t *testing.T) {
	ctx := context.Background()
	for name, feed := range map[string]func(*store.Store, request){
		"the proxy first": func(st *store.Store, r request) { add(t, st, r.wire); add(t, st, r.app) },
		"the app first":   func(st *store.Store, r request) { add(t, st, r.app); add(t, st, r.wire) },
		"together":        func(st *store.Store, r request) { add(t, st, r.wire, r.app) },
		"in pieces": func(st *store.Store, r request) {
			add(t, st, r.wire[:9])
			add(t, st, r.app[:14])
			add(t, st, r.wire[9:])
			add(t, st, r.app[14:])
		},
	} {
		for _, linked := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, traceparent %v", name, linked), func(t *testing.T) {
				st := openStore(t)
				st.Capturing("postgresql", true)
				r := orders(1, 25, linked)
				feed(st, r)

				q := customerQuery(t, st)
				if q.Calls != 25 || q.MaxPerTrace != 25 || q.Traces != 1 {
					t.Errorf("the N+1: %d calls, at most %d in one of %d traces; want 25, 25, 1", q.Calls, q.MaxPerTrace, q.Traces)
				}
				// The wire's timing, not the app's 2ms.
				if !near(q.AvgMs, 1.2) || !near(q.TotalMs, 25*1.2) {
					t.Errorf("timing: avg %v total %v, want the wire's 1.2ms each", q.AvgMs, q.TotalMs)
				}

				eps, err := st.Endpoints(ctx, window(), store.EndpointQuery{})
				if err != nil {
					t.Fatal(err)
				}
				if len(eps.Rows) != 1 || eps.Rows[0].Name != "GET /orders" || eps.Rows[0].Requests != 1 ||
					eps.Rows[0].Queries != 26 || !near(eps.Rows[0].DBMs, 26*1.2) {
					t.Errorf("endpoints = %+v, want one request with 26 queries and 31.2ms in the database", eps.Rows)
				}
				series, err := st.SeriesBetween(ctx, window().SinceMs, window().UntilMs, 1, "")
				if err != nil {
					t.Fatal(err)
				}
				if series.Requests[0] != 1 || series.Queries[0] != 26 || !near(series.DBMs[0], 26*1.2) {
					t.Errorf("overview: %v requests, %v queries, %vms; want 1, 26, 31.2", series.Requests[0], series.Queries[0], series.DBMs[0])
				}

				// One trace, with each wire span under the app span that
				// sent it, and both to be seen.
				traces, err := st.Traces(ctx, store.Filter{})
				if err != nil {
					t.Fatal(err)
				}
				if len(traces) != 1 || traces[0].TraceID != r.trace || traces[0].Spans != 53 || traces[0].Queries != 26 {
					t.Fatalf("traces = %+v, want the one with 53 spans and 26 queries", traces)
				}
				spans, err := st.TraceSpans(ctx, r.trace)
				if err != nil {
					t.Fatal(err)
				}
				roles := map[string]int{}
				parents := map[string]string{}
				for _, sp := range spans {
					roles[sp.DBRole]++
					parents[sp.SpanID] = sp.ParentID
					if sp.Service != "acme-orders" {
						t.Errorf("span %s is filed under %q", sp.Name, sp.Service)
					}
				}
				if roles["query"] != 26 || roles["duplicate"] != 26 {
					t.Errorf("roles = %v, want 26 queries and 26 second sightings", roles)
				}
				for k, w := range r.wire {
					if parents[w.SpanID] != r.app[k].SpanID {
						t.Errorf("wire span %d is under %q, want the app's span %q", k, parents[w.SpanID], r.app[k].SpanID)
					}
				}
			})
		}
	}
}

// What only one of them saw is counted once, and stays where it was.
func TestAStatementOnlyOneSourceSaw(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	st.Capturing("postgresql", true)
	seen := orders(1, 5, false)
	appOnly := orders(2, 5, false)  // through another database, say
	wireOnly := orders(3, 5, false) // an app with no telemetry
	add(t, st, seen.wire, wireOnly.wire)
	add(t, st, seen.app, appOnly.app)

	q := customerQuery(t, st)
	if q.Calls != 15 || q.MaxPerTrace != 5 {
		t.Errorf("%d calls, at most %d a trace; want 15 and 5", q.Calls, q.MaxPerTrace)
	}
	// The proxy's own statements keep the service the proxy gave them and
	// are no request.
	rows, err := st.Queries(ctx, store.Filter{Service: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	var calls int64
	for _, r := range rows {
		calls += r.Calls
	}
	if calls != 6 {
		t.Errorf("the uninstrumented app has %d queries, want its 6", calls)
	}
	eps, err := st.Endpoints(ctx, window(), store.EndpointQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(eps.Rows) != 1 || eps.Rows[0].Requests != 2 || eps.Rows[0].Queries != 6 {
		t.Errorf("endpoints = %+v, want GET /orders twice with 6 queries each", eps.Rows)
	}
}

// With no capture running, nothing is held back or paired: the app's spans
// are the queries.
func TestNoCaptureNoPairing(t *testing.T) {
	st := openStore(t)
	r := orders(1, 25, false)
	add(t, st, r.app)
	if q := customerQuery(t, st); q.Calls != 25 || !near(q.AvgMs, 2) {
		t.Errorf("%d calls averaging %vms, want the app's 25 at 2ms", q.Calls, q.AvgMs)
	}
}

// A transaction the proxy framed goes with its statements into the app's
// trace: the transaction's own span and the statements the app never
// reported. BEGIN and COMMIT are in the trace and in database time, and
// are not queries.
func TestATransactionSeenTwice(t *testing.T) {
	ctx := context.Background()
	for _, proxyFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("proxy first %v", proxyFirst), func(t *testing.T) {
			st := openStore(t)
			st.Capturing("postgresql", true)
			const trace, root, txTrace, frame = "000000000000000000000000000000aa", "00000000000000a0", "000000000000000000000000000000ff", "00000000000000f0"
			at := linkEpoch
			var app, wire []store.Span
			for k, q := range []string{"BEGIN", "INSERT INTO orders (customer_id) VALUES ($1)", "SET LOCAL lock_timeout = 100", "COMMIT"} {
				start := at + ms(1) + int64(k)*ms(3)
				fp := strings.NewReplacer("$1", "?", "100", "?").Replace(q)
				// The app's instrumentation does not report the SET.
				if k != 2 {
					app = append(app, store.Span{TraceID: trace, SpanID: fmt.Sprintf("%016x", 0xa1+k), ParentID: root, Service: "acme-orders",
						Name: "pg.query", Kind: "client", StartNs: start, DurationNs: ms(2), Status: "unset",
						DBSystem: "postgresql", DBStatement: q, DBFingerprint: fp, Attrs: "{}"})
				}
				wire = append(wire, store.Span{TraceID: txTrace, SpanID: fmt.Sprintf("%016x", 0xf1+k), ParentID: frame, Service: "acme",
					Name: strings.Fields(q)[0], Kind: "client", StartNs: start + ms(0.5), DurationNs: ms(1), Status: "ok",
					DBSystem: "postgresql", DBStatement: q, DBFingerprint: fp, Attrs: `{"leadline.source":"pgproxy"}`, Source: store.SourcePostgresWire})
			}
			app = append(app, store.Span{TraceID: trace, SpanID: root, Service: "acme-orders", Name: "POST /orders", Kind: "server",
				StartNs: at, DurationNs: ms(15), Status: "unset", Attrs: "{}"})
			wire = append(wire, store.Span{TraceID: txTrace, SpanID: frame, Service: "acme", Name: "transaction", Kind: "internal",
				StartNs: at + ms(1.5), DurationNs: ms(10), Status: "ok", Attrs: `{"leadline.source":"pgproxy"}`, Source: store.SourcePostgresWire})
			if proxyFirst {
				add(t, st, wire)
				add(t, st, app)
			} else {
				add(t, st, app)
				add(t, st, wire)
			}

			traces, err := st.Traces(ctx, store.Filter{})
			if err != nil {
				t.Fatal(err)
			}
			if len(traces) != 1 || traces[0].TraceID != trace || traces[0].Spans != 9 {
				t.Fatalf("traces = %+v, want the app's one with all 9 spans", traces)
			}
			// One query (the INSERT); BEGIN, SET and COMMIT take database
			// time, 1ms each on the wire, and are not queries.
			if traces[0].Queries != 1 || !near(traces[0].DBMs, 4) {
				t.Errorf("the trace has %d queries and %vms in the database, want 1 and 4", traces[0].Queries, traces[0].DBMs)
			}
			spans, err := st.TraceSpans(ctx, trace)
			if err != nil {
				t.Fatal(err)
			}
			for _, sp := range spans {
				switch {
				case sp.Name == "transaction":
					if sp.ParentID != root || sp.Entry {
						t.Errorf("the transaction's span: parent %q entry %v; want it under the request and no request itself", sp.ParentID, sp.Entry)
					}
				case sp.Name == "SET":
					if sp.DBRole != "control" || sp.ParentID != frame {
						t.Errorf("the SET the app did not report: %+v", sp)
					}
				}
			}
			queries, err := st.Queries(ctx, store.Filter{})
			if err != nil {
				t.Fatal(err)
			}
			if len(queries) != 1 || !strings.HasPrefix(queries[0].Fingerprint, "INSERT") || queries[0].Calls != 1 {
				t.Errorf("queries = %+v, want the INSERT once", queries)
			}
			control, err := st.Queries(ctx, store.Filter{Control: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(control) != 3 {
				t.Errorf("control statements = %+v, want BEGIN, SET and COMMIT", control)
			}
		})
	}
}

// Redis's MONITOR says when a command ran and not for how long, so there
// the app's span is the one that counts and the capture's is under it.
func TestRedisSeenTwice(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	st.Capturing("redis", true)
	const trace, root = "000000000000000000000000000000bb", "00000000000000b0"
	at := linkEpoch
	var app, wire []store.Span
	for k := 0; k < 10; k++ {
		start := at + ms(1) + int64(k)*ms(2)
		app = append(app, store.Span{TraceID: trace, SpanID: fmt.Sprintf("%016x", 0xb1+k), ParentID: root, Service: "acme-web",
			Name: "get", Kind: "client", StartNs: start, DurationNs: ms(1), Status: "unset",
			DBSystem: "redis", DBStatement: fmt.Sprintf("get session:%d", k), DBFingerprint: "GET session:?", Attrs: "{}"})
		wire = append(wire, store.Span{TraceID: fmt.Sprintf("%032x", 0xcc00+k), SpanID: fmt.Sprintf("%016x", 0xc1+k), Service: "Cache",
			Name: "GET", Kind: "client", StartNs: start + ms(0.3), Status: "ok",
			DBSystem: "redis", DBStatement: fmt.Sprintf("GET session:%d", k), DBFingerprint: "GET session:?",
			Attrs: `{"leadline.source":"redis-monitor"}`, Source: store.SourceRedisMonitor})
	}
	// What a client says to keep its connection: seen, and no query.
	wire = append(wire, store.Span{TraceID: fmt.Sprintf("%032x", 0xcc99), SpanID: "00000000000000c9", Service: "Cache",
		Name: "PING", Kind: "client", StartNs: at + ms(30), Status: "ok", DBSystem: "redis", DBStatement: "PING", DBFingerprint: "PING",
		Attrs: `{"leadline.source":"redis-monitor"}`, Source: store.SourceRedisMonitor})
	app = append(app, store.Span{TraceID: trace, SpanID: root, Service: "acme-web", Name: "GET /", Kind: "server",
		StartNs: at, DurationNs: ms(25), Status: "unset", Attrs: "{}"})
	add(t, st, wire)
	add(t, st, app)

	rows, err := st.Queries(ctx, store.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Calls != 10 || !near(rows[0].AvgMs, 1) || rows[0].MaxPerTrace != 10 {
		t.Errorf("queries = %+v, want GET session:? 10 times at the app's 1ms", rows)
	}
	if paired := st.Paired()[store.Pairing{Service: "acme-web", Source: store.SourceRedisMonitor}]; paired != 10 {
		t.Errorf("paired = %v, want 10 for acme-web", st.Paired())
	}
}

// A query builder's span around the driver's span for the same statement
// is not a second query.
func TestAWrappedQueryIsOne(t *testing.T) {
	st := openStore(t)
	const trace, root = "000000000000000000000000000000dd", "00000000000000d0"
	at := linkEpoch
	q := store.Span{TraceID: trace, Service: "acme-web", Kind: "client", Status: "unset", DBSystem: "postgresql",
		DBStatement: "select * from users where id = ?", DBFingerprint: "select * from users where id = ?", Attrs: "{}"}
	outer, inner := q, q
	outer.SpanID, outer.ParentID, outer.Name, outer.StartNs, outer.DurationNs = "00000000000000d1", root, "knex.query", at+ms(1), ms(5)
	inner.SpanID, inner.ParentID, inner.Name, inner.StartNs, inner.DurationNs = "00000000000000d2", outer.SpanID, "pg.query", at+ms(2), ms(3)
	rootSpan := store.Span{TraceID: trace, SpanID: root, Service: "acme-web", Name: "GET /users/:id", Kind: "server",
		StartNs: at, DurationNs: ms(8), Status: "unset", Attrs: "{}"}
	// The driver's span ends first, and may be exported a batch earlier.
	add(t, st, []store.Span{inner})
	add(t, st, []store.Span{outer, rootSpan})

	rows, err := st.Queries(context.Background(), store.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Calls != 1 || !near(rows[0].AvgMs, 3) {
		t.Errorf("queries = %+v, want the driver's one call of 3ms", rows)
	}
}

// One definition of a request, used by the overview and by the endpoints:
// they agree with a service filter and without one.
func TestRequestsMeanOneThing(t *testing.T) {
	ctx := context.Background()
	f := load(t, 400)
	// Two more kinds of span that are no request of their own: the page's
	// span for a fetch, and a server span inside a server span of the same
	// service. The inner one arrives a batch before its parent.
	at := int64(f.window.SinceMs*1e6) + ms(5000)
	const trace = "000000000000000000000000000000ee"
	browser := store.Span{TraceID: trace, SpanID: "00000000000000e1", Service: "acme-web", Name: "fetch GET /api/orders", Kind: "client",
		StartNs: at, DurationNs: ms(40), Status: "unset", Attrs: `{"leadline.source":"browser"}`, Source: store.SourceBrowser}
	outer := store.Span{TraceID: trace, SpanID: "00000000000000e2", ParentID: browser.SpanID, Service: "acme-web", Name: "GET /api/orders", Kind: "server",
		StartNs: at + ms(2), DurationNs: ms(30), Status: "unset", Attrs: "{}"}
	inner := store.Span{TraceID: trace, SpanID: "00000000000000e3", ParentID: outer.SpanID, Service: "acme-web", Name: "GET /api/orders/route", Kind: "server",
		StartNs: at + ms(3), DurationNs: ms(28), Status: "unset", Attrs: "{}"}
	// A server span in another service, under the first: a request there.
	other := store.Span{TraceID: trace, SpanID: "00000000000000e4", ParentID: inner.SpanID, Service: "acme-payments", Name: "POST /charge", Kind: "server",
		StartNs: at + ms(5), DurationNs: ms(10), Status: "unset", Attrs: "{}"}
	add(t, f.st, []store.Span{inner, other})
	add(t, f.st, []store.Span{outer, browser})

	for _, service := range []string{"", "acme-api", "acme-payments", "acme-worker", "acme-web"} {
		w := f.window
		w.Service = service
		series, err := f.st.SeriesBetween(ctx, w.SinceMs, w.UntilMs, 30, service)
		if err != nil {
			t.Fatal(err)
		}
		eps, err := f.st.Endpoints(ctx, w, store.EndpointQuery{Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		heat, err := f.st.RequestHeatmap(ctx, w, 30)
		if err != nil {
			t.Fatal(err)
		}
		var overview, listed, queries, perEndpoint float64
		var heated int64
		for i := range series.Requests {
			overview += series.Requests[i]
			queries += series.Queries[i]
		}
		for _, e := range eps.Rows {
			listed += float64(e.Requests)
			perEndpoint += e.Queries * float64(e.Requests)
		}
		for _, n := range heat.N {
			heated += n
		}
		if overview == 0 || overview != listed || int64(overview) != heated {
			t.Errorf("service %q: the overview counts %v requests, the endpoints %v, the heatmap %d", service, overview, listed, heated)
		}
		// Every query of these apps runs inside a request of the same
		// service, so the two counts of queries agree as well.
		if service != "acme-web" && !near(queries, perEndpoint) {
			t.Errorf("service %q: the overview counts %v queries, the endpoints %v", service, queries, perEndpoint)
		}
		if service == "acme-web" && (overview != 1 || len(eps.Rows) != 1 || eps.Rows[0].Name != "GET /api/orders") {
			t.Errorf("acme-web: %v requests in %+v; want the one server span, not the browser's or the nested one", overview, eps.Rows)
		}
	}

	// The browser's span and the nested one are still in the trace.
	spans, err := f.st.TraceSpans(ctx, trace)
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string]bool{}
	for _, sp := range spans {
		entries[sp.Name] = sp.Entry
	}
	if len(spans) != 4 || entries["fetch GET /api/orders"] || entries["GET /api/orders/route"] || !entries["GET /api/orders"] || !entries["POST /charge"] {
		t.Errorf("entries = %v", entries)
	}
}

// An app that sends only metrics, or only logs, is an app.
func TestSummaryCountsEverySignal(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	now := time.Now().UnixNano()
	if err := st.AddMetrics([]store.MetricPoint{{Name: "queue.depth", Type: store.MetricGauge, Service: "acme-metrics-only",
		Series: 1, Attrs: "{}", TsNs: now, Value: 3}}); err != nil {
		t.Fatal(err)
	}
	sum, err := st.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.MetricPoints != 1 || sum.Spans != 0 || len(sum.Services) != 1 || sum.Services[0] != "acme-metrics-only" {
		t.Errorf("summary = %+v, want the service that sent one metric point", sum)
	}
	if err := st.AddLogs([]store.Log{{TsNs: now, Service: "acme-logs-only", Severity: "info", SeverityNum: 9, Body: "hello", Attrs: "{}"}}); err != nil {
		t.Fatal(err)
	}
	if sum, err = st.Summary(ctx); err != nil || len(sum.Services) != 2 || sum.Logs != 1 {
		t.Errorf("summary = %+v, %v; want both services", sum, err)
	}
}

// A database file from before these columns is brought up to date when it
// is opened: the columns are added and its spans classified.
func TestOpenBringsAnOlderFileUpToDate(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "leadline.duckdb")
	db, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE spans (
			trace_id VARCHAR, span_id VARCHAR, parent_id VARCHAR, service VARCHAR, name VARCHAR, kind VARCHAR,
			start_ns BIGINT, duration_ns BIGINT, status VARCHAR, status_message VARCHAR,
			db_system VARCHAR, db_statement VARCHAR, db_fingerprint VARCHAR,
			http_method VARCHAR, http_route VARCHAR, http_status INTEGER, attrs VARCHAR)`,
		`CREATE TABLE logs (ts_ns BIGINT, service VARCHAR, severity VARCHAR, severity_num INTEGER,
			body VARCHAR, trace_id VARCHAR, span_id VARCHAR, attrs VARCHAR)`,
		fmt.Sprintf(`INSERT INTO spans VALUES
			('t1', 's1', '', 'acme-api', 'GET /api/orders', 'server', %[1]d, 9000000, 'unset', '', '', '', '', 'GET', '/api/orders', 200, '{}'),
			('t1', 's2', 's1', 'acme-api', 'pg.query', 'client', %[1]d + 1000, 1000000, 'unset', '', 'postgresql', 'BEGIN', 'BEGIN', '', '', 0, '{}'),
			('t1', 's3', 's1', 'acme-api', 'pg.query', 'client', %[1]d + 2000, 2000000, 'unset', '', 'postgresql', 'SELECT 1', 'SELECT ?', '', '', 0, '{}'),
			('t1', 's4', 's1', 'acme-api', 'set', 'client', %[1]d + 3000, 1000000, 'unset', '', 'redis', 'SET a 1', 'SET a ?', '', '', 0, '{}'),
			('t2', 's5', '', 'acme-web', 'fetch GET /api/orders', 'client', %[1]d, 9000000, 'unset', '', '', '', '', '', '', 0, '{"leadline.source":"browser"}'),
			('t3', 's6', '', 'acme', 'SELECT', 'client', %[1]d, 1000000, 'ok', '', 'postgresql', 'SELECT 2', 'SELECT ?', '', '', 0, '{"leadline.source":"pgproxy"}')`, linkEpoch),
		fmt.Sprintf(`INSERT INTO logs VALUES (%d, 'acme-api', 'info', 9, 'listed orders', 't1', 's1', '{}')`, linkEpoch),
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	eps, err := st.Endpoints(ctx, window(), store.EndpointQuery{})
	if err != nil {
		t.Fatal(err)
	}
	// SELECT 1 and Redis's SET are queries; BEGIN is not, though its
	// millisecond is database time.
	if len(eps.Rows) != 1 || eps.Rows[0].Name != "GET /api/orders" || eps.Rows[0].Queries != 2 || !near(eps.Rows[0].DBMs, 4) {
		t.Errorf("endpoints = %+v, want the one request with 2 queries and 4ms", eps.Rows)
	}
	logs, err := st.Logs(ctx, store.Filter{})
	if err != nil || len(logs) != 1 {
		t.Errorf("logs = %+v, %v", logs, err)
	}
	// And it takes new rows, which an appender only does when the columns
	// are the ones it expects.
	r := orders(9, 3, false)
	add(t, st, r.app)
	if err := st.AddLogs([]store.Log{{TsNs: linkEpoch, Service: "acme-web", Severity: "error", SeverityNum: 17, Body: "boom",
		Attrs: "{}", Kind: "browser.error", UserID: "u_42"}}); err != nil {
		t.Fatal(err)
	}
	if q := customerQuery(t, st); q.Calls != 3 {
		t.Errorf("after the upgrade: %+v", q)
	}
	if logs, err = st.Logs(ctx, store.Filter{Kind: "browser.error", User: "u_42"}); err != nil || len(logs) != 1 || logs[0].Kind != "browser.error" {
		t.Errorf("logs by kind and user = %+v, %v", logs, err)
	}
}

// Traces and logs can be asked for by the user they were for.
func TestFilterByUser(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	a, b := orders(1, 2, false), orders(2, 2, false)
	a.app[len(a.app)-1].UserID = "u_42"
	add(t, st, a.app, b.app)
	if err := st.AddLogs([]store.Log{
		{TsNs: linkEpoch, Service: "acme-orders", Severity: "info", SeverityNum: 9, Body: "for 42", Attrs: "{}", UserID: "u_42"},
		{TsNs: linkEpoch + 1, Service: "acme-orders", Severity: "info", SeverityNum: 9, Body: "for nobody", Attrs: "{}"},
		{TsNs: linkEpoch + 2, Service: "acme-orders", Severity: "warn", SeverityNum: 13, Body: "slow", Attrs: "{}", Kind: "browser.fetch_slow"},
	}); err != nil {
		t.Fatal(err)
	}
	traces, err := st.Traces(ctx, store.Filter{User: "u_42"})
	if err != nil || len(traces) != 1 || traces[0].TraceID != a.trace || traces[0].UserID != "u_42" {
		t.Errorf("traces for u_42 = %+v, %v", traces, err)
	}
	if all, _ := st.Traces(ctx, store.Filter{}); len(all) != 2 {
		t.Errorf("all traces = %+v", all)
	}
	logs, err := st.Logs(ctx, store.Filter{User: "u_42"})
	if err != nil || len(logs) != 1 || logs[0].Body != "for 42" || logs[0].UserID != "u_42" {
		t.Errorf("logs for u_42 = %+v, %v", logs, err)
	}
	if logs, err = st.Logs(ctx, store.Filter{Kind: "browser.fetch_slow"}); err != nil || len(logs) != 1 || logs[0].Body != "slow" {
		t.Errorf("logs of a kind = %+v, %v", logs, err)
	}
}

// A framework's route list is kept, the newest per service, and says which
// routes nothing asked for.
func TestRoutes(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	const body = `{"version":1,"framework":"next","next":"16.4.0","root":"/work/acme","routes":[
		{"route":"/orders","kind":"page","router":"app","file":"app/orders/page.tsx","methods":["GET"],"layouts":["app/layout.tsx"]},
		{"route":"/products/[id]","kind":"page","router":"app","file":"app/products/[id]/page.tsx","dynamic":true},
		{"route":"/api/health","kind":"api","router":"pages","file":"pages/api/health.ts"},
		{"route":"/","kind":"layout","router":"app","file":"app/layout.tsx"}]}`
	if err := st.SetRoutes(ctx, "acme-orders", linkEpoch, body); err != nil {
		t.Fatal(err)
	}
	// An older list, and one that is not JSON, change nothing.
	st.SetRoutes(ctx, "acme-orders", linkEpoch-1, `{"framework":"next","routes":[]}`)
	st.SetRoutes(ctx, "acme-orders", linkEpoch+1, `not json`)
	add(t, st, orders(1, 1, false).app)

	lists, err := st.Routes(ctx, window())
	if err != nil || len(lists) != 1 || len(lists[0].Routes) != 4 || lists[0].Version != "16.4.0" {
		t.Fatalf("routes = %+v, %v", lists, err)
	}
	if r := lists[0].Routes[0]; r.Route != "/orders" || r.Requests != 1 || r.LastMs == 0 {
		t.Errorf("/orders = %+v, want its one request", r)
	}
	eps, err := st.Endpoints(ctx, window(), store.EndpointQuery{})
	if err != nil {
		t.Fatal(err)
	}
	// The layout is no route a request asks for by name.
	if len(eps.NeverHit) != 2 || eps.NeverHit[0].Route != "/api/health" || eps.NeverHit[1].Route != "/products/[id]" {
		t.Errorf("never hit = %+v, want /api/health and /products/[id]", eps.NeverHit)
	}
	// A newer list replaces it.
	if err := st.SetRoutes(ctx, "acme-orders", linkEpoch+2, `{"framework":"next","routes":[{"route":"/orders","kind":"page"}]}`); err != nil {
		t.Fatal(err)
	}
	if lists, _ = st.Routes(ctx, window()); len(lists) != 1 || len(lists[0].Routes) != 1 {
		t.Errorf("after a newer list: %+v", lists)
	}
}

// Everything can be narrowed to one database: its queries, the traces and
// requests that sent it one, and the logs written during those.
func TestFilterByDatabase(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	// Three requests: one asks the main Postgres, one a second Postgres on
	// another port, one Redis.
	type call struct{ system, server, statement string }
	var logs []store.Log
	for i, c := range []call{
		{"postgresql", "localhost:5432", "SELECT 1"},
		{"postgresql", "127.0.0.1:5433", "SELECT 2"},
		{"redis", "127.0.0.1:6379", "GET a"},
	} {
		trace, root := fmt.Sprintf("%032x", 0xd0+i), fmt.Sprintf("%016x", 0xd0+i)
		at := linkEpoch + int64(i)*ms(100)
		add(t, st, []store.Span{
			{TraceID: trace, SpanID: fmt.Sprintf("%016x", 0xe0+i), ParentID: root, Service: "acme-web", Name: "db", Kind: "client",
				StartNs: at + ms(1), DurationNs: ms(2), Status: "unset", DBSystem: c.system, DBStatement: c.statement,
				DBFingerprint: c.statement, DBServer: store.DBServer(c.server, 0), Attrs: "{}"},
			{TraceID: trace, SpanID: root, Service: "acme-web", Name: fmt.Sprintf("GET /r%d", i), Kind: "server",
				StartNs: at, DurationNs: ms(5), Status: "unset", Attrs: "{}"},
		})
		logs = append(logs, store.Log{TsNs: at + ms(3), Service: "acme-web", Severity: "info", SeverityNum: 9,
			Body: fmt.Sprintf("handled r%d", i), TraceID: trace, Attrs: "{}"})
	}
	logs = append(logs, store.Log{TsNs: linkEpoch, Service: "acme-web", Severity: "info", SeverityNum: 9, Body: "started", Attrs: "{}"})
	if err := st.AddLogs(logs); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		system, server string
		want           []string // the requests kept
	}{
		{"postgresql", "", []string{"GET /r0", "GET /r1"}},
		{"postgresql", "127.0.0.1:5432", []string{"GET /r0"}}, // localhost, spelled the other way
		{"", "127.0.0.1:5433", []string{"GET /r1"}},
		{"redis", "", []string{"GET /r2"}},
		{"mysql", "", nil},
	} {
		name := c.system + " " + c.server
		traces, err := st.Traces(ctx, store.Filter{DBSystem: c.system, DBServer: c.server})
		if err != nil {
			t.Fatal(err)
		}
		w := window()
		w.DBSystem, w.DBServer = c.system, c.server
		eps, err := st.Endpoints(ctx, w, store.EndpointQuery{Spark: 4})
		if err != nil {
			t.Fatal(err)
		}
		queries, err := st.Queries(ctx, store.Filter{DBSystem: c.system, DBServer: c.server})
		if err != nil {
			t.Fatal(err)
		}
		got, err := st.Logs(ctx, store.Filter{DBSystem: c.system, DBServer: c.server})
		if err != nil {
			t.Fatal(err)
		}
		var traced, listed []string
		for _, tr := range traces {
			traced = append(traced, tr.Name)
		}
		for _, e := range eps.Rows {
			listed = append(listed, e.Name)
			var spark float64
			for _, v := range e.Spark {
				spark += v
			}
			if e.Requests != 1 || spark != 1 {
				t.Errorf("%s: %s has %d requests and a sparkline of %v", name, e.Name, e.Requests, spark)
			}
		}
		sort.Strings(traced)
		sort.Strings(listed)
		if !slices.Equal(traced, c.want) || !slices.Equal(listed, c.want) {
			t.Errorf("%s: traces %v, endpoints %v; want %v", name, traced, listed, c.want)
		}
		if len(queries) != len(c.want) || len(got) != len(c.want) {
			t.Errorf("%s: %d queries and %d logs, want %d of each", name, len(queries), len(got), len(c.want))
		}
	}
	// An endpoint's own figures follow the filter too.
	w := window()
	w.DBSystem = "redis"
	if d, err := st.EndpointDetail(ctx, w, store.Endpoint{Service: "acme-web", Name: "GET /r0"}, 10); err != nil || d.Stats != nil {
		t.Errorf("GET /r0 never touched Redis, and has stats %+v (%v)", d.Stats, err)
	}
	if d, err := st.EndpointDetail(ctx, w, store.Endpoint{Service: "acme-web", Name: "GET /r2"}, 10); err != nil || d.Stats == nil || d.Stats.Requests != 1 {
		t.Errorf("GET /r2 with the Redis filter: %+v (%v)", d.Stats, err)
	}
}
