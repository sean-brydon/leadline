package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/sean-brydon/leadline/internal/api"
	"github.com/sean-brydon/leadline/internal/demo"
	"github.com/sean-brydon/leadline/internal/ingest"
	"github.com/sean-brydon/leadline/internal/store"
)

// runBench measures the store at volume: it fills a database file with demo
// data through the store, times every question the app and the MCP tools
// ask of it, and then pushes OTLP through the real HTTP handlers while
// reading. It never talks to a running daemon and never touches ~/.leadline.
func runBench(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	dbPath := fs.String("db", "", "database file to fill and measure (required); one that already holds data is reused as it is")
	spans := fs.Int("spans", 5_000_000, "about how many spans to fill")
	logs := fs.Int("logs", 2_000_000, "about how many log records to fill")
	hours := fs.Float64("hours", 24, "spread the data over this many hours")
	runs := fs.Int("runs", 7, "how many times to run each query")
	ingestFor := fs.Duration("ingest", 8*time.Second, "how long to push OTLP over HTTP in each ingest test; 0 skips them")
	clients := fs.Int("clients", 4, "concurrent OTLP exporters in the ingest tests")
	churn := fs.Int("churn", 0, "then prune to the newest 12 hours and add this many more hours, an hour at a time, pruning as it goes, to watch the file's size")
	seed := fs.Uint64("seed", 1, "seed for the made-up data")
	threads := fs.Int("threads", 0, "cores one query may take; 0 is the daemon's default")
	memory := fs.Int("memory", 0, "DuckDB's memory cap in MB; 0 is the daemon's default, -1 is none")
	sql := fs.String("sql", "", "run these statements instead (separated by semicolons; EXPLAIN ANALYZE ..., say), -runs times each, and print what comes back")
	fs.Parse(args)
	if *dbPath == "" {
		return fmt.Errorf("bench needs -db, a file of its own (it can grow to gigabytes)")
	}

	opened := time.Now()
	st, err := benchOpen(*dbPath, *threads, *memory)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	fmt.Printf("opened %s in %s\n", *dbPath, since(opened))

	if *sql != "" {
		return printSQL(ctx, st, *sql, *runs)
	}

	sum, err := st.Summary(ctx)
	if err != nil {
		return err
	}
	if sum.Spans == 0 {
		over := time.Duration(*hours * float64(time.Hour))
		began := time.Now()
		nSpans, nLogs, err := benchFill(st, *spans, *logs, began.Add(-over), began, *seed, true)
		if err != nil {
			return err
		}
		took := time.Since(began)
		fmt.Printf("\rfilled %d spans and %d logs over %s in %s (%.0f rows/s)\n",
			nSpans, nLogs, over, took.Round(time.Millisecond), float64(nSpans+nLogs)/took.Seconds())
		cp := time.Now()
		if _, err := st.DB().Exec(`CHECKPOINT`); err != nil {
			return err
		}
		fmt.Printf("checkpoint took %s\n", since(cp))
		if sum, err = st.Summary(ctx); err != nil {
			return err
		}
	}
	fmt.Printf("holds %d spans, %d logs, %d traces; file %s\n\n", sum.Spans, sum.Logs, sum.Traces, fileSize(*dbPath))

	// The data's own end stands in for "now", so a file filled yesterday
	// measures the same windows today.
	var endNs int64
	if err := st.DB().QueryRowContext(ctx, `SELECT max(start_ns) FROM spans`).Scan(&endNs); err != nil {
		return err
	}
	if err := benchQueries(ctx, st, endNs, *runs); err != nil {
		return err
	}
	if err := benchPaging(ctx, st, endNs); err != nil {
		return err
	}
	if *ingestFor > 0 {
		// First what a busy dev machine sends, then as much as will go in.
		for _, load := range []struct{ perRequest, tracesPerSecond int }{{1, 300}, {1, 0}, {75, 0}} {
			if endNs, err = benchIngest(ctx, st, endNs, load.perRequest, load.tracesPerSecond, *clients, *ingestFor, *seed); err != nil {
				return err
			}
		}
		fmt.Printf("file after ingest %s\n\n", fileSize(*dbPath))
	}
	if *churn > 0 {
		if err := benchRetention(ctx, st, *dbPath, endNs, *churn, *seed); err != nil {
			return err
		}
	}
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) == nil {
		// Darwin reports bytes, Linux kilobytes.
		fmt.Printf("peak memory of this process: %d MB (on macOS; divide by 1024 again on Linux)\n", ru.Maxrss>>20)
	}
	return nil
}

// printSQL runs statements separated by semicolons on one connection's
// database and says how long each took: the way to try a query shape or a
// setting, or to read an EXPLAIN ANALYZE, on a filled file.
func printSQL(ctx context.Context, st *store.Store, sql string, runs int) error {
	for _, q := range strings.Split(sql, ";") {
		if q = strings.TrimSpace(q); q == "" {
			continue
		}
		var took []time.Duration
		var n int
		for i := 0; i < runs; i++ {
			began := time.Now()
			rows, err := st.DB().QueryContext(ctx, q)
			if err != nil {
				return fmt.Errorf("%s: %w", q, err)
			}
			cols, _ := rows.Columns()
			n = 0
			for rows.Next() {
				vals := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					rows.Close()
					return err
				}
				// The first rows are enough to see what came back.
				if n++; i == 0 && n <= 40 {
					parts := make([]string, len(vals))
					for i, v := range vals {
						parts[i] = fmt.Sprint(v)
					}
					fmt.Println(strings.Join(parts, "\t"))
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			took = append(took, time.Since(began))
		}
		sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
		fmt.Printf("-- %d rows; min %s, median %s, max %s: %.90s\n\n", n, ms(took[0]), ms(took[len(took)/2]), ms(took[len(took)-1]),
			strings.Join(strings.Fields(q), " "))
	}
	return nil
}

// spansPerTrace is what demo.Generator makes on average; it only sizes the
// fill, so being a little off just means a little more or less data.
const spansPerTrace = 6.7

type fillBatch struct {
	spans []store.Span
	logs  []store.Log
}

// benchFill writes demo traces between start and end in time order, as a
// running app would send them: batch after batch, each covering the next
// slice of time.
func benchFill(st *store.Store, wantSpans, wantLogs int, start, end time.Time, seed uint64, progress bool) (nSpans, nLogs int, err error) {
	const tracesPerBatch = 1000
	batches := int(float64(wantSpans)/(spansPerTrace*tracesPerBatch)) + 1
	// The demo writes about one log a trace. Real apps are chattier, so the
	// rest is plain log lines spread over the same slice.
	extraLogs := (wantLogs - batches*tracesPerBatch) / batches
	if extraLogs < 0 {
		extraLogs = 0
	}
	slice := end.Sub(start) / time.Duration(batches)

	// Making the data costs more than storing it, so several workers make
	// batches and the writer takes them in order.
	type job struct {
		i   int
		out chan fillBatch
	}
	work := make(chan job)
	ordered := make(chan job, 16)
	for w := 0; w < 8; w++ {
		go func() {
			for j := range work {
				from := start.Add(time.Duration(j.i) * slice)
				gen := demo.New(seed + uint64(j.i)*7919 + uint64(start.UnixNano()))
				traces, logs := gen.Batch(tracesPerBatch, from, from.Add(slice))
				b := fillBatch{spans: ingest.Spans(traces, false), logs: ingest.Logs(logs, false)}
				b.logs = append(b.logs, chatter(seed+uint64(j.i), extraLogs, from, slice, b.spans)...)
				j.out <- b
			}
		}()
	}
	go func() {
		for i := 0; i < batches; i++ {
			j := job{i: i, out: make(chan fillBatch, 1)}
			ordered <- j
			work <- j
		}
		close(work)
		close(ordered)
	}()

	for j := range ordered {
		b := <-j.out
		if err != nil {
			continue // drain the workers
		}
		if err = st.AddSpans(b.spans); err == nil {
			err = st.AddLogs(b.logs)
		}
		nSpans += len(b.spans)
		nLogs += len(b.logs)
		if progress && (j.i+1)%100 == 0 {
			fmt.Printf("\rfilling: %d spans, %d logs", nSpans, nLogs)
		}
	}
	return nSpans, nLogs, err
}

var chatterLines = []struct {
	service string
	sev     int32
	text    string
	body    string
}{
	{"acme-api", 5, "debug", "cache hit products:category:%d"},
	{"acme-api", 9, "info", "request finished in %dms"},
	{"acme-api", 9, "info", "session refreshed for user %d"},
	{"acme-worker", 9, "info", "queue depth %d"},
	{"acme-worker", 5, "debug", "heartbeat %d"},
	{"acme-payments", 9, "info", "ledger balanced, %d entries"},
	{"acme-api", 13, "warn", "rate limit at %d%% for anonymous clients"},
	{"acme-payments", 17, "error", "gateway timeout after %dms, retrying"},
}

// chatter makes n plain log lines inside one slice of time. One in three
// carries the id of a trace from the same slice, as a request's logs would.
func chatter(seed uint64, n int, from time.Time, slice time.Duration, spans []store.Span) []store.Log {
	r := rand.New(rand.NewPCG(seed, 0x5eed))
	out := make([]store.Log, 0, n)
	for i := 0; i < n; i++ {
		// Mostly debug and info, a few warnings, now and then an error.
		line := chatterLines[r.IntN(6)]
		if p := r.Float64(); p < 0.01 {
			line = chatterLines[7]
		} else if p < 0.04 {
			line = chatterLines[6]
		}
		l := store.Log{
			TsNs:        from.Add(time.Duration(r.Int64N(int64(slice)))).UnixNano(),
			Service:     line.service,
			Severity:    line.text,
			SeverityNum: line.sev,
			Body:        fmt.Sprintf(line.body, r.IntN(5000)),
			Attrs:       "{}",
		}
		if len(spans) > 0 && r.IntN(3) == 0 {
			sp := spans[r.IntN(len(spans))]
			l.TraceID, l.SpanID = sp.TraceID, sp.SpanID
		}
		out = append(out, l)
	}
	return out
}

type timing struct {
	name, window string
	rows         int
	took         []time.Duration
}

func (t timing) String() string {
	s := append([]time.Duration(nil), t.took...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return fmt.Sprintf("%-28s %-6s %8d %10s %10s %10s", t.name, t.window, t.rows,
		ms(s[0]), ms(s[len(s)/2]), ms(s[len(s)-1]))
}

const timingHeader = "query                        window     rows        min     median        max"

func ms(d time.Duration) string { return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000) }

func since(t time.Time) string { return time.Since(t).Round(time.Millisecond).String() }

func timeIt(name, window string, runs int, fn func() (int, error)) (timing, error) {
	t := timing{name: name, window: window}
	for i := 0; i < runs; i++ {
		began := time.Now()
		n, err := fn()
		if err != nil {
			return t, fmt.Errorf("%s %s: %w", name, window, err)
		}
		t.took = append(t.took, time.Since(began))
		t.rows = n
	}
	return t, nil
}

// benchQueries times what the app's views and the MCP tools ask, with the
// limits they use, over three windows that end where the data ends.
func benchQueries(ctx context.Context, st *store.Store, endNs int64, runs int) error {
	endMs := float64(endNs) / 1e6
	fmt.Println(timingHeader)
	show := func(name, window string, fn func() (int, error)) error {
		t, err := timeIt(name, window, runs, fn)
		if err != nil {
			return err
		}
		fmt.Println(t)
		return nil
	}
	windows := []struct {
		label string
		d     time.Duration
	}{{"15m", 15 * time.Minute}, {"1h", time.Hour}, {"24h", 24 * time.Hour}}
	for _, w := range windows {
		sinceMs := endMs - float64(w.d.Milliseconds())
		f := store.Filter{SinceMs: sinceMs}
		win := store.Window{SinceMs: sinceMs, UntilMs: endMs + 1}
		traces := func(f store.Filter) func() (int, error) {
			return func() (int, error) {
				rows, err := st.Traces(ctx, f)
				return len(rows), err
			}
		}
		logs := func(f store.Filter) func() (int, error) {
			return func() (int, error) {
				rows, err := st.Logs(ctx, f)
				return len(rows), err
			}
		}
		queries := func(f store.Filter) func() (int, error) {
			return func() (int, error) {
				rows, err := st.Queries(ctx, f)
				return len(rows), err
			}
		}
		series := func(n int) func() (int, error) {
			return func() (int, error) {
				s, err := st.SeriesBetween(ctx, sinceMs, endMs, n, "")
				if err != nil {
					return 0, err
				}
				return len(s.T), nil
			}
		}
		with := func(change func(*store.Filter)) store.Filter {
			g := f
			change(&g)
			return g
		}
		for _, q := range []struct {
			name string
			fn   func() (int, error)
		}{
			{"Traces (app, 300)", traces(with(func(f *store.Filter) { f.Limit = 300 }))},
			{"Traces search", traces(with(func(f *store.Filter) { f.Limit = 300; f.Search = "checkout" }))},
			{"Traces one service", traces(with(func(f *store.Filter) { f.Limit = 300; f.Service = "acme-worker" }))},
			{"Traces errors only (mcp)", traces(with(func(f *store.Filter) { f.Limit = 20; f.ErrorsOnly = true }))},
			{"Traces over 1s (mcp)", traces(with(func(f *store.Filter) { f.Limit = 20; f.MinDurationMs = 1000 }))},
			{"Queries (app)", queries(f)},
			{"Queries search", queries(with(func(f *store.Filter) { f.Search = "order_items" }))},
			{"Logs (app, 2000)", logs(with(func(f *store.Filter) { f.Limit = 2000 }))},
			{"Logs search", logs(with(func(f *store.Filter) { f.Limit = 2000; f.Search = "declined" }))},
			{"Logs errors", logs(with(func(f *store.Filter) { f.Limit = 2000; f.MinSeverity = 17 }))},
			{"Series (app, 90 buckets)", series(90)},
			{"Series (mcp, 1 bucket)", series(1)},
			{"Endpoints (app, sparklines)", func() (int, error) {
				e, err := st.Endpoints(ctx, win, store.EndpointQuery{Spark: 24})
				if err != nil {
					return 0, err
				}
				return len(e.Rows), nil
			}},
			{"Endpoint detail", func() (int, error) {
				d, err := st.EndpointDetail(ctx, win, store.Endpoint{Service: "acme-api", Name: "GET /api/orders"}, 60)
				if err != nil || d.Stats == nil {
					return 0, err
				}
				return int(d.Stats.Requests), nil
			}},
			{"Query detail", func() (int, error) {
				d, err := st.QueryDetail(ctx, win, store.QueryKey{DBSystem: "postgresql",
					Fingerprint: "SELECT id, product_id, quantity FROM order_items WHERE order_id = ?"}, 60)
				if err != nil || d.Stats == nil {
					return 0, err
				}
				return int(d.Stats.Calls), nil
			}},
			{"Heatmap (app, 90 columns)", func() (int, error) {
				h, err := st.RequestHeatmap(ctx, win, 90)
				if err != nil {
					return 0, err
				}
				return len(h.N), nil
			}},
			{"Breakdown by service", func() (int, error) {
				g, err := st.GroupedSeries(ctx, win, "service", 60, 6)
				if err != nil {
					return 0, err
				}
				return len(g.Groups), nil
			}},
			{"Traces of one database", traces(with(func(f *store.Filter) { f.Limit = 300; f.DBSystem = "redis" }))},
			{"Queries of one database", queries(with(func(f *store.Filter) { f.DBSystem = "redis" }))},
			{"Logs of one database", logs(with(func(f *store.Filter) { f.Limit = 2000; f.DBSystem = "redis" }))},
			{"Endpoints of one database", func() (int, error) {
				dbWin := win
				dbWin.DBSystem = "redis"
				e, err := st.Endpoints(ctx, dbWin, store.EndpointQuery{Spark: 24})
				if err != nil {
					return 0, err
				}
				return len(e.Rows), nil
			}},
			{"Traces of one user", traces(with(func(f *store.Filter) { f.Limit = 300; f.User = "u_42" }))},
			{"Breakdown database time", func() (int, error) {
				g, err := st.GroupedSeries(ctx, win, "db_time", 60, 6)
				if err != nil {
					return 0, err
				}
				return len(g.Groups), nil
			}},
		} {
			if err := show(q.name, w.label, q.fn); err != nil {
				return err
			}
		}
		fmt.Println()
	}

	if err := show("Summary", "all", func() (int, error) {
		s, err := st.Summary(ctx)
		if err != nil {
			return 0, err
		}
		return len(s.Services), nil
	}); err != nil {
		return err
	}

	// Opening a trace: its spans and its logs, as GET /api/traces/{id} does.
	// A new trace, one from the middle of the day and an old one, because a
	// lookup that leans on time order would favour one of them.
	for _, age := range []struct {
		label string
		back  time.Duration
	}{{"new", time.Minute}, {"12h", 12 * time.Hour}, {"23h", 23 * time.Hour}} {
		at := endNs - age.back.Nanoseconds()
		rows, err := st.DB().QueryContext(ctx,
			`SELECT DISTINCT trace_id FROM spans WHERE start_ns >= ? AND start_ns < ? LIMIT 20`, at, at+int64(time.Minute))
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if len(ids) == 0 {
			continue
		}
		t := timing{name: "Open one trace", window: age.label}
		for i := 0; i < runs*3; i++ {
			id := ids[i%len(ids)]
			began := time.Now()
			n, err := benchOpenTrace(ctx, st, id)
			if err != nil {
				return err
			}
			t.took = append(t.took, time.Since(began))
			t.rows = n
		}
		fmt.Println(t)
	}
	fmt.Println()
	return nil
}

// benchIngest pushes OTLP through the daemon's real handlers for a while,
// from several exporters at once, and reads like an open app meanwhile.
// tracesPerSecond paces the exporters together; zero is flat out. It
// returns where the data now ends.
func benchIngest(ctx context.Context, st *store.Store, endNs int64, tracesPerRequest, tracesPerSecond, clients int, d time.Duration, seed uint64) (int64, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return endNs, err
	}
	srv := &http.Server{Handler: api.New(st, nil).Handler()}
	go srv.Serve(ln)
	defer srv.Close()
	url := "http://" + ln.Addr().String()
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: clients * 2}}

	// The new data carries on from where the stored data ends.
	began := time.Now()
	dataNow := func() time.Time { return time.Unix(0, endNs).Add(time.Since(began)) }
	stop := began.Add(d)

	var sentSpans, sentLogs, requests atomic.Int64
	var mu sync.Mutex
	var posts []time.Duration
	var wg sync.WaitGroup
	errs := make(chan error, clients+1)
	for c := 0; c < clients; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gen := demo.New(seed + 1_000_003*uint64(c+1) + uint64(tracesPerRequest))
			last := dataNow()
			var mine []time.Duration
			// A paced exporter sends its share of the rate, one request a
			// tick; an unpaced one sends as fast as it is answered.
			var tick *time.Ticker
			if tracesPerSecond > 0 {
				tick = time.NewTicker(time.Second * time.Duration(clients*tracesPerRequest) / time.Duration(tracesPerSecond))
				defer tick.Stop()
			}
			for time.Now().Before(stop) {
				if tick != nil {
					<-tick.C
				}
				now := dataNow()
				traces, logs := gen.Batch(tracesPerRequest, last, now)
				last = now
				for path, msg := range map[string]proto.Message{"/v1/traces": traces, "/v1/logs": logs} {
					body, _ := proto.Marshal(msg)
					t0 := time.Now()
					resp, err := client.Post(url+path, "application/x-protobuf", bytes.NewReader(body))
					if err != nil {
						errs <- err
						return
					}
					resp.Body.Close()
					if resp.StatusCode != http.StatusOK {
						errs <- fmt.Errorf("POST %s: %s", path, resp.Status)
						return
					}
					mine = append(mine, time.Since(t0))
					requests.Add(1)
				}
				for _, rs := range traces.ResourceSpans {
					for _, ss := range rs.ScopeSpans {
						sentSpans.Add(int64(len(ss.Spans)))
					}
				}
				for _, rl := range logs.ResourceLogs {
					for _, sl := range rl.ScopeLogs {
						sentLogs.Add(int64(len(sl.LogRecords)))
					}
				}
			}
			mu.Lock()
			posts = append(posts, mine...)
			mu.Unlock()
		}()
	}

	// The reader is one open app: the lists, the chart and the counts over
	// the last 15 minutes, again and again.
	var reads []time.Duration
	wg.Add(1)
	go func() {
		defer wg.Done()
		for time.Now().Before(stop) {
			sinceMs := float64(dataNow().Add(-15*time.Minute).UnixNano()) / 1e6
			untilMs := float64(dataNow().UnixNano()) / 1e6
			for _, fn := range []func() error{
				func() error { _, err := st.Traces(ctx, store.Filter{SinceMs: sinceMs, Limit: 300}); return err },
				func() error { _, err := st.Logs(ctx, store.Filter{SinceMs: sinceMs, Limit: 2000}); return err },
				func() error { _, err := st.SeriesBetween(ctx, sinceMs, untilMs, 90, ""); return err },
				func() error { _, err := st.Summary(ctx); return err },
			} {
				t0 := time.Now()
				if err := fn(); err != nil {
					errs <- err
					return
				}
				reads = append(reads, time.Since(t0))
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	wg.Wait()
	took := time.Since(began)
	select {
	case err := <-errs:
		return endNs, err
	default:
	}

	// How long until the last request's rows can be read back.
	before, err := st.Summary(ctx)
	if err != nil {
		return endNs, err
	}
	marker := fmt.Sprintf("bench marker %d", time.Now().UnixNano())
	nowNs := dataNow().UnixNano()
	posted := time.Now()
	if err := st.AddLogs([]store.Log{{TsNs: nowNs, Service: "acme-api", Severity: "info", SeverityNum: 9, Body: marker, Attrs: "{}"}}); err != nil {
		return endNs, err
	}
	visible := time.Duration(-1)
	for time.Since(posted) < 5*time.Second {
		var n int
		// Asked of the database itself, as another feature's query would.
		if err := st.DB().QueryRowContext(ctx, `SELECT count(*) FROM logs WHERE ts_ns = ? AND body = ?`, nowNs, marker).Scan(&n); err != nil {
			return endNs, err
		}
		if n > 0 {
			visible = time.Since(posted)
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	sort.Slice(posts, func(i, j int) bool { return posts[i] < posts[j] })
	sort.Slice(reads, func(i, j int) bool { return reads[i] < reads[j] })
	pct := func(s []time.Duration, p float64) string {
		if len(s) == 0 {
			return "-"
		}
		return ms(s[int(float64(len(s)-1)*p)])
	}
	pace := "as fast as it goes"
	if tracesPerSecond > 0 {
		pace = fmt.Sprintf("%d traces a second", tracesPerSecond)
	}
	fmt.Printf("ingest over HTTP, %d exporters, %d trace(s) a request, %s, for %s:\n", clients, tracesPerRequest, pace, took.Round(time.Millisecond))
	fmt.Printf("  %.0f spans/s, %.0f logs/s, %.0f requests/s; a request took p50 %s, p99 %s, max %s\n",
		float64(sentSpans.Load())/took.Seconds(), float64(sentLogs.Load())/took.Seconds(), float64(requests.Load())/took.Seconds(),
		pct(posts, 0.5), pct(posts, 0.99), pct(posts, 1))
	fmt.Printf("  the app's reads meanwhile (%d): p50 %s, p99 %s, max %s\n", len(reads), pct(reads, 0.5), pct(reads, 0.99), pct(reads, 1))
	fmt.Printf("  stored %d spans by the end; a row written after that was readable after %s\n\n", before.Spans, visible.Round(time.Millisecond))
	return dataNow().UnixNano(), nil
}

func fileSize(path string) string {
	var total int64
	for _, p := range []string{path, path + ".wal"} {
		if fi, err := os.Stat(p); err == nil {
			total += fi.Size()
		}
	}
	return fmt.Sprintf("%.0f MB", float64(total)/(1<<20))
}

// benchOpenTrace is what GET /api/traces/{id} asks of the store.
func benchOpenTrace(ctx context.Context, st *store.Store, id string) (int, error) {
	spans, _, err := st.Trace(ctx, id)
	return len(spans), err
}

func benchOpen(path string, threads, memoryMB int) (*store.Store, error) {
	return store.Open(path, store.Options{Threads: threads, MemoryLimit: int64(memoryMB) << 20})
}

// benchPaging scrolls each list back through twenty pages, the way the app
// does when the user keeps scrolling, and reports what a page costs.
func benchPaging(ctx context.Context, st *store.Store, endNs int64) error {
	const pages = 20
	fmt.Println(timingHeader)
	for _, w := range []struct {
		label string
		d     time.Duration
	}{{"1h", time.Hour}, {"24h", 24 * time.Hour}} {
		sinceMs := float64(endNs-w.d.Nanoseconds()) / 1e6
		for _, list := range []struct {
			name string
			page func(before string) (n int, next string, err error)
		}{
			{"Traces, 20 pages of 200", func(before string) (int, string, error) {
				rows, err := st.Traces(ctx, store.Filter{SinceMs: sinceMs, Limit: 200, Before: before})
				if err != nil || len(rows) == 0 {
					return 0, "", err
				}
				return len(rows), rows[len(rows)-1].Cursor, nil
			}},
			{"Traces search, 20 pages", func(before string) (int, string, error) {
				rows, err := st.Traces(ctx, store.Filter{SinceMs: sinceMs, Limit: 200, Search: "checkout", Before: before})
				if err != nil || len(rows) == 0 {
					return 0, "", err
				}
				return len(rows), rows[len(rows)-1].Cursor, nil
			}},
			{"Logs, 20 pages of 500", func(before string) (int, string, error) {
				rows, err := st.Logs(ctx, store.Filter{SinceMs: sinceMs, Limit: 500, Before: before})
				if err != nil || len(rows) == 0 {
					return 0, "", err
				}
				return len(rows), rows[len(rows)-1].Cursor, nil
			}},
			{"Logs search, 20 pages", func(before string) (int, string, error) {
				rows, err := st.Logs(ctx, store.Filter{SinceMs: sinceMs, Limit: 500, Search: "declined", Before: before})
				if err != nil || len(rows) == 0 {
					return 0, "", err
				}
				return len(rows), rows[len(rows)-1].Cursor, nil
			}},
		} {
			t := timing{name: list.name, window: w.label}
			before := ""
			for i := 0; i < pages; i++ {
				began := time.Now()
				n, next, err := list.page(before)
				if err != nil {
					return fmt.Errorf("%s: %w", list.name, err)
				}
				if n == 0 {
					break
				}
				t.took = append(t.took, time.Since(began))
				t.rows += n
				before = next
			}
			if len(t.took) > 0 {
				fmt.Println(t)
			}
		}
	}
	fmt.Println()
	return nil
}

// benchRetention prunes the file back to its newest twelve hours, then
// keeps adding an hour of data and pruning the oldest hour. A store that
// gives space back holds its size through that.
func benchRetention(ctx context.Context, st *store.Store, path string, endNs int64, hours int, seed uint64) error {
	sum, err := st.Summary(ctx)
	if err != nil {
		return err
	}
	var oldest int64
	if err := st.DB().QueryRowContext(ctx, `SELECT min(start_ns) FROM spans`).Scan(&oldest); err != nil {
		return err
	}
	held := time.Duration(endNs - oldest)
	perHourSpans := int(float64(sum.Spans) / held.Hours())
	perHourLogs := int(float64(sum.Logs) / held.Hours())
	report := func(what string) error {
		used, err := st.Size(ctx)
		if err != nil {
			return err
		}
		s, err := st.Summary(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("  %-34s %9d spans %9d logs   file %5s   in use %4d MB\n", what, s.Spans, s.Logs, fileSize(path), used>>20)
		return nil
	}
	fmt.Printf("retention: %s of data, about %d spans an hour\n", held.Round(time.Minute), perHourSpans)
	if err := report("to begin with"); err != nil {
		return err
	}

	// The first prune is the big one, as after days away. Reads carry on.
	const keep = 12 * time.Hour
	var slowest atomic.Int64
	stop := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		sinceMs := float64(endNs-int64(15*time.Minute)) / 1e6
		for {
			select {
			case <-stop:
				return
			default:
			}
			t0 := time.Now()
			st.Traces(ctx, store.Filter{SinceMs: sinceMs, Limit: 200})
			st.Logs(ctx, store.Filter{SinceMs: sinceMs, Limit: 500})
			if d := time.Since(t0).Nanoseconds(); d > slowest.Load() {
				slowest.Store(d)
			}
		}
	}()
	began := time.Now()
	p, err := st.Prune(ctx, time.Unix(0, endNs).Add(-keep))
	took := time.Since(began)
	close(stop)
	readers.Wait()
	if err != nil {
		return err
	}
	fmt.Printf("  pruned %d spans and %d logs older than %s in %s, checkpoint included; the slowest read of the two lists meanwhile took %s\n",
		p.Spans, p.Logs, keep, took.Round(time.Millisecond), ms(time.Duration(slowest.Load())))
	if err := report("after pruning to 12h"); err != nil {
		return err
	}

	end := time.Unix(0, endNs)
	var slowestPrune time.Duration
	for h := 1; h <= hours; h++ {
		next := end.Add(time.Hour)
		if _, _, err := benchFill(st, perHourSpans, perHourLogs, end, next, seed+uint64(h), false); err != nil {
			return err
		}
		end = next
		began := time.Now()
		if _, err := st.Prune(ctx, end.Add(-keep)); err != nil {
			return err
		}
		slowestPrune = max(slowestPrune, time.Since(began))
		if h%4 == 0 || h == hours {
			if err := report(fmt.Sprintf("+%dh added, oldest %dh pruned", h, h)); err != nil {
				return err
			}
		}
	}
	fmt.Printf("  the slowest of those hourly prunes took %s\n\n", slowestPrune.Round(time.Millisecond))
	return nil
}
