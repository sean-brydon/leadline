package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests cover what keeps the store usable at volume: paging that
// neither skips nor repeats, the write that happens a moment after an Add,
// and retention. The volume itself is `leadline bench`'s business.

var scaleEpoch = time.Date(2026, 5, 4, 9, 0, 0, 0, time.UTC).UnixNano()

// scaleTrace makes one trace of a root and two children. i names it; the
// root starts at startNs. Every fifth trace fails, every third is a job in
// another service, and each takes a different time.
func scaleTrace(i int, startNs int64) []Span {
	id := fmt.Sprintf("%032x", i*7919+1)
	root := fmt.Sprintf("%016x", i*3+1)
	service, name := "acme-api", "GET /api/products"
	if i%3 == 0 {
		service, name = "acme-worker", "job send-receipt"
	}
	status := "unset"
	if i%5 == 0 {
		status = "error"
	}
	dur := int64(i%40+1) * int64(50*time.Millisecond)
	return []Span{
		// Children first: they end first, so they are exported first.
		{TraceID: id, SpanID: fmt.Sprintf("%016x", i*3+2), ParentID: root, Service: "acme-api", Name: "pg.query", Kind: "client",
			StartNs: startNs + 1000, DurationNs: 2000, Status: status, DBSystem: "postgresql",
			DBStatement: "SELECT 1", DBFingerprint: "SELECT ?", Attrs: "{}"},
		{TraceID: id, SpanID: fmt.Sprintf("%016x", i*3+3), ParentID: root, Service: "acme-api", Name: "GET", Kind: "client",
			StartNs: startNs + 5000, DurationNs: 100, Status: "unset", DBSystem: "redis",
			DBStatement: "GET", DBFingerprint: "GET", Attrs: "{}"},
		{TraceID: id, SpanID: root, Service: service, Name: name, Kind: "server",
			StartNs: startNs, DurationNs: dur, Status: "unset", Attrs: "{}"},
	}
}

func scaleOpen(t *testing.T, path string, o Options) *Store {
	t.Helper()
	st, err := Open(path, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func traceIDs(rows []TraceRow) []string {
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.TraceID
	}
	return ids
}

// pageTraces reads the whole list a page at a time.
func pageTraces(t *testing.T, st *Store, f Filter, size int) []TraceRow {
	t.Helper()
	var all []TraceRow
	f.Limit = size
	for pages := 0; ; pages++ {
		page, err := st.Traces(context.Background(), f)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			return all
		}
		if len(page) > size || pages > 10_000 {
			t.Fatalf("page %d has %d rows, want at most %d", pages, len(page), size)
		}
		all = append(all, page...)
		f.Before = page[len(page)-1].Cursor
	}
}

func TestTracesPageWithoutGapsOrRepeats(t *testing.T) {
	st := scaleOpen(t, "", Options{})
	// 400 traces. They come in fives that start in the same nanosecond, so
	// a page of seven always ends inside such a group; and a few start
	// hours earlier, far from the rest.
	const n = 400
	var spans []Span
	for i := 0; i < n; i++ {
		start := scaleEpoch + int64(i/5)*int64(time.Second)
		if i < 20 {
			start = scaleEpoch - int64(20-i/5)*int64(time.Hour)
		}
		spans = append(spans, scaleTrace(i, start)...)
	}
	if err := st.AddSpans(spans); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	for _, f := range []Filter{
		{},
		{SinceMs: float64(scaleEpoch)/1e6 + 10_000},
		{Service: "acme-worker"},
		{Search: "PRODUCTS"},
		{ErrorsOnly: true},
		{MinDurationMs: 1000},
		{Service: "acme-api", ErrorsOnly: true, MinDurationMs: 500, SinceMs: float64(scaleEpoch)/1e6 - 1},
	} {
		whole := f
		whole.Limit = 5000
		want, err := st.Traces(ctx, whole)
		if err != nil {
			t.Fatal(err)
		}
		if len(want) == 0 {
			t.Fatalf("%+v: the filter matched nothing, so it tests nothing", f)
		}
		// Newest first, and by id among traces of one instant.
		if !sort.SliceIsSorted(want, func(a, b int) bool {
			if want[a].StartMs != want[b].StartMs {
				return want[a].StartMs > want[b].StartMs
			}
			return want[a].TraceID > want[b].TraceID
		}) {
			t.Errorf("%+v: not newest first", f)
		}
		for _, size := range []int{7, 50} {
			got := pageTraces(t, st, f, size)
			if !slices.Equal(traceIDs(got), traceIDs(want)) {
				t.Errorf("%+v in pages of %d: %d traces, want the same %d in the same order", f, size, len(got), len(want))
			}
			for i := range got {
				if i < len(want) && got[i] != want[i] {
					t.Errorf("%+v: paged row %d = %+v, want %+v", f, i, got[i], want[i])
					break
				}
			}
		}
	}

	// The filters mean what they meant before paging.
	all, _ := st.Traces(ctx, Filter{Limit: 5000})
	if len(all) != n {
		t.Fatalf("%d traces, want %d", len(all), n)
	}
	for _, r := range all {
		if r.Spans != 3 || r.Queries != 2 {
			t.Fatalf("trace %s has %d spans and %d queries, want 3 and 2", r.TraceID, r.Spans, r.Queries)
		}
	}
	count := func(f Filter) int {
		f.Limit = 5000
		rows, err := st.Traces(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	if got, want := count(Filter{Service: "acme-worker"}), (n+2)/3; got != want {
		t.Errorf("worker traces = %d, want %d", got, want)
	}
	if got, want := count(Filter{ErrorsOnly: true}), n/5; got != want {
		t.Errorf("failed traces = %d, want %d", got, want)
	}
	if got, want := count(Filter{Search: "send-receipt"}), (n+2)/3; got != want {
		t.Errorf("traces named send-receipt = %d, want %d", got, want)
	}
	if got, want := count(Filter{MinDurationMs: 1000}), n*21/40; got != want {
		t.Errorf("traces of a second or more = %d, want %d", got, want)
	}

	for _, bad := range []string{"nonsense", "12", "12.", ".abc", "x.abc"} {
		if _, err := st.Traces(ctx, Filter{Before: bad}); !errors.Is(err, ErrBadCursor) {
			t.Errorf("cursor %q: err = %v, want ErrBadCursor", bad, err)
		}
	}
}

// A trace's root arrives last and starts first, so a trace moves down the
// list after the page it was on was read. It must then show up once more,
// at its new place, not vanish.
func TestTracesPagingWhileTracesGrow(t *testing.T) {
	st := scaleOpen(t, "", Options{})
	ctx := context.Background()
	for i := 0; i < 30; i++ {
		tr := scaleTrace(i, scaleEpoch+int64(i)*int64(time.Second))
		if i == 29 {
			tr = tr[:2] // the newest trace is still running: no root yet
		}
		st.AddSpans(tr)
	}
	first, err := st.Traces(ctx, Filter{Limit: 10})
	if err != nil || len(first) != 10 {
		t.Fatalf("first page: %d rows, %v", len(first), err)
	}
	if first[0].Name != "pg.query" {
		t.Errorf("a trace without its root is named %q, want its earliest span's name", first[0].Name)
	}
	// Its root arrives, with a start that sorts it into the second page.
	late := scaleTrace(29, scaleEpoch+15*int64(time.Second)+1)[2:]
	st.AddSpans(late)
	second, err := st.Traces(ctx, Filter{Limit: 10, Before: first[9].Cursor})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(traceIDs(second), first[0].TraceID) {
		t.Errorf("the trace that moved is on neither page")
	}
}

func logBodies(rows []LogRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Body
	}
	return out
}

func pageLogs(t *testing.T, st *Store, f Filter, size int) []LogRow {
	t.Helper()
	var all []LogRow
	f.Limit = size
	for pages := 0; ; pages++ {
		page, err := st.Logs(context.Background(), f)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			return all
		}
		if len(page) > size || pages > 10_000 {
			t.Fatalf("page %d has %d rows, want at most %d", pages, len(page), size)
		}
		all = append(all, page...)
		f.Before = page[len(page)-1].Cursor
	}
}

func TestLogsPageWithoutGapsOrRepeats(t *testing.T) {
	st := scaleOpen(t, "", Options{})
	// Seven logs to every millisecond, so pages of ten end inside a group
	// of equal times; then a handful that are minutes, hours and days
	// older, which the widening search has to reach.
	var logs []Log
	n := 0
	add := func(ts int64) {
		sev, text := int32(9), "info"
		if n%4 == 0 {
			sev, text = 17, "error"
		}
		service := "acme-api"
		if n%3 == 0 {
			service = "acme-worker"
		}
		logs = append(logs, Log{TsNs: ts, Service: service, Severity: text, SeverityNum: sev,
			Body: fmt.Sprintf("line %04d", n), Attrs: "{}"})
		n++
	}
	for _, back := range []time.Duration{40 * 24 * time.Hour, 3 * 24 * time.Hour, 26 * time.Hour, 9 * time.Hour, 70 * time.Minute, 5 * time.Minute, 5 * time.Minute, 90 * time.Second} {
		add(scaleEpoch - int64(back))
	}
	for i := 0; i < 700; i++ {
		add(scaleEpoch + int64(i/7)*int64(time.Millisecond))
	}
	// Stored in two goes, so equal times also span separate writes.
	st.AddLogs(logs[:300])
	st.Flush()
	st.AddLogs(logs[300:])

	// What the list should be: newest first, and logs of one instant in
	// the order they were stored.
	expect := func(keep func(Log) bool) []string {
		var kept []Log
		for _, l := range logs {
			if keep(l) {
				kept = append(kept, l)
			}
		}
		sort.SliceStable(kept, func(a, b int) bool { return kept[a].TsNs > kept[b].TsNs })
		out := make([]string, len(kept))
		for i, l := range kept {
			out[i] = l.Body
		}
		return out
	}
	since := scaleEpoch - int64(10*time.Hour)
	for name, c := range map[string]struct {
		f    Filter
		keep func(Log) bool
	}{
		"everything":   {Filter{}, func(Log) bool { return true }},
		"a window":     {Filter{SinceMs: float64(since) / 1e6}, func(l Log) bool { return l.TsNs >= since }},
		"one service":  {Filter{Service: "acme-worker"}, func(l Log) bool { return l.Service == "acme-worker" }},
		"errors":       {Filter{MinSeverity: 17}, func(l Log) bool { return l.SeverityNum >= 17 }},
		"a search":     {Filter{Search: "LINE 00"}, func(l Log) bool { return strings.HasPrefix(l.Body, "line 00") }},
		"a rare match": {Filter{Search: "line 0001"}, func(l Log) bool { return l.Body == "line 0001" }},
	} {
		want := expect(c.keep)
		if len(want) == 0 {
			t.Fatalf("%s: matched nothing, so it tests nothing", name)
		}
		for _, size := range []int{10, 64, 5000} {
			got := pageLogs(t, st, c.f, size)
			if !slices.Equal(logBodies(got), want) {
				t.Errorf("%s in pages of %d: %d logs, want the same %d in the same order", name, size, len(got), len(want))
			}
		}
	}

	if rows, err := st.Logs(context.Background(), Filter{SinceMs: float64(scaleEpoch)/1e6 + 1e6}); err != nil || len(rows) != 0 {
		t.Errorf("a window after the newest log: %d rows, %v", len(rows), err)
	}
	for _, bad := range []string{"nonsense", "12.0", "12.x", "x.1", "12"} {
		if _, err := st.Logs(context.Background(), Filter{Before: bad}); !errors.Is(err, ErrBadCursor) {
			t.Errorf("cursor %q: err = %v, want ErrBadCursor", bad, err)
		}
	}
}

// A log stamped with the very time the last page ended on can arrive before
// the next page is asked for. The next page carries on where the last one
// stopped, and the newcomer follows the others of its instant.
func TestLogsPagingWhileLogsArrive(t *testing.T) {
	st := scaleOpen(t, "", Options{})
	ctx := context.Background()
	var logs []Log
	for i := 0; i < 30; i++ {
		logs = append(logs, Log{TsNs: scaleEpoch + int64(i/6), Service: "acme-api", Severity: "info", SeverityNum: 9, Body: fmt.Sprintf("line %02d", i), Attrs: "{}"})
	}
	st.AddLogs(logs)
	first, err := st.Logs(ctx, Filter{Limit: 8})
	if err != nil || len(first) != 8 {
		t.Fatalf("first page: %d rows, %v", len(first), err)
	}
	// Page one ended two logs into the instant of lines 18 to 23.
	st.AddLogs([]Log{{TsNs: scaleEpoch + 3, Service: "acme-api", Severity: "info", SeverityNum: 9, Body: "late", Attrs: "{}"}})
	second, err := st.Logs(ctx, Filter{Limit: 8, Before: first[7].Cursor})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"line 20", "line 21", "line 22", "line 23", "late", "line 12", "line 13", "line 14"}
	if got := logBodies(second); !slices.Equal(got, want) {
		t.Errorf("second page = %v, want %v", got, want)
	}
}

func TestRowsAreWrittenWithinTheFlushInterval(t *testing.T) {
	st := scaleOpen(t, "", Options{}) // the daemon's own interval
	if err := st.AddSpans(scaleTrace(1, scaleEpoch)); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	if st.written.Load() >= st.added.Load() {
		t.Fatal("the add wrote straight away; it should leave that to the flusher")
	}
	// No statement runs here, so nothing but the flusher can write them.
	for st.written.Load() < st.added.Load() {
		if time.Since(began) > time.Second {
			t.Fatal("rows still unwritten a second after they were added")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if took := time.Since(began); took < defaultFlushEvery/2 {
		t.Errorf("written after %s; the flusher should wait about %s for more to arrive", took, defaultFlushEvery)
	}
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM spans`).Scan(&n); err != nil || n != 3 {
		t.Errorf("%d spans stored, %v; want 3", n, err)
	}
}

// Every way of asking the database sees rows added just before, however
// long the flusher would have waited.
func TestStatementsSeeRowsJustAdded(t *testing.T) {
	st := scaleOpen(t, "", Options{FlushEvery: time.Hour})
	ctx := context.Background()
	count := func() (n int) {
		t.Helper()
		if err := st.DB().QueryRowContext(ctx, `SELECT count(*) FROM spans`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	st.AddSpans(scaleTrace(1, scaleEpoch))
	if got := count(); got != 3 {
		t.Errorf("a query saw %d spans, want 3", got)
	}

	stmt, err := st.DB().PrepareContext(ctx, `SELECT count(*) FROM spans WHERE start_ns >= ?`)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	st.AddSpans(scaleTrace(2, scaleEpoch))
	var n int
	if err := stmt.QueryRowContext(ctx, 0).Scan(&n); err != nil || n != 6 {
		t.Errorf("a prepared statement saw %d spans, %v; want 6", n, err)
	}

	st.AddSpans(scaleTrace(3, scaleEpoch))
	res, err := st.DB().ExecContext(ctx, `DELETE FROM spans WHERE trace_id = ?`, scaleTrace(3, 0)[0].TraceID)
	if err != nil {
		t.Fatal(err)
	}
	if deleted, _ := res.RowsAffected(); deleted != 3 {
		t.Errorf("an exec saw %d of the 3 spans just added", deleted)
	}

	st.AddLogs([]Log{{TsNs: scaleEpoch, Service: "acme-api", Severity: "info", SeverityNum: 9, Body: "hello", Attrs: "{}"}})
	if rows, err := st.Logs(ctx, Filter{}); err != nil || len(rows) != 1 {
		t.Errorf("Logs saw %d rows, %v; want 1", len(rows), err)
	}
	if sum, err := st.Summary(ctx); err != nil || sum.Spans != 6 || sum.Traces != 2 || sum.Logs != 1 {
		t.Errorf("summary = %+v, %v; want 6 spans in 2 traces and 1 log", sum, err)
	}
}

func TestCloseWritesWhatIsWaiting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leadline.duckdb")
	// An hour between writes: only Close can have stored these.
	st, err := Open(path, Options{FlushEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		if err := st.AddSpans(scaleTrace(i, scaleEpoch+int64(i))); err != nil {
			t.Fatal(err)
		}
		st.AddLogs([]Log{{TsNs: scaleEpoch + int64(i), Service: "acme-api", Severity: "info", SeverityNum: 9, Body: "line", Attrs: "{}"}})
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSpans(scaleTrace(1, scaleEpoch)); err == nil {
		t.Error("adding to a closed store reported no error")
	}
	if err := st.Close(); err != nil {
		t.Errorf("closing twice: %v", err)
	}

	again := scaleOpen(t, path, Options{})
	sum, err := again.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Spans != 1500 || sum.Logs != 500 || sum.Traces != 500 {
		t.Errorf("after reopening: %+v, want 1500 spans in 500 traces and 500 logs", sum)
	}
}

// More rows than may wait in memory: the adder is held until the flusher
// has caught up, so memory stays bounded, and none are dropped.
func TestABurstWaitsForTheFlusher(t *testing.T) {
	st := scaleOpen(t, "", Options{FlushEvery: time.Hour})
	var spans []Span
	for i := 0; i < maxWaiting/3+10; i++ {
		spans = append(spans, scaleTrace(i, scaleEpoch+int64(i))...)
	}
	if err := st.AddSpans(spans); err != nil {
		t.Fatal(err)
	}
	if st.written.Load() != int64(len(spans)) {
		t.Errorf("%d of %d rows written when the add returned", st.written.Load(), len(spans))
	}
}

func TestAddingAndReadingTogether(t *testing.T) {
	st := scaleOpen(t, "", Options{FlushEvery: 2 * time.Millisecond})
	ctx := context.Background()
	const writers, each = 4, 150
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				n := w*each + i
				st.AddSpans(scaleTrace(n, scaleEpoch+int64(n)*1000))
				st.AddLogs([]Log{{TsNs: scaleEpoch + int64(n), Service: "acme-api", Severity: "info", SeverityNum: 9, Body: "line", Attrs: "{}"}})
			}
		}()
	}
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for r := 0; r < 2; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := st.Traces(ctx, Filter{Limit: 50}); err != nil {
					t.Error(err)
					return
				}
				if _, err := st.Logs(ctx, Filter{Limit: 50}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	readers.Wait()
	sum, err := st.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Spans != writers*each*3 || sum.Logs != writers*each || sum.Traces != writers*each {
		t.Errorf("summary = %+v, want %d spans, %d traces, %d logs", sum, writers*each*3, writers*each, writers*each)
	}
}

func TestPruneDropsOldRowsOnly(t *testing.T) {
	st := scaleOpen(t, filepath.Join(t.TempDir(), "leadline.duckdb"), Options{})
	ctx := context.Background()
	now := time.Now()
	old, fresh := now.Add(-80*time.Hour).UnixNano(), now.Add(-time.Hour).UnixNano()
	for i := 0; i < 100; i++ {
		st.AddSpans(scaleTrace(i, old+int64(i)))
		st.AddSpans(scaleTrace(1000+i, fresh+int64(i)))
		st.AddLogs([]Log{
			{TsNs: old + int64(i), Service: "acme-api", Severity: "info", SeverityNum: 9, Body: "old", Attrs: "{}"},
			{TsNs: fresh + int64(i), Service: "acme-api", Severity: "info", SeverityNum: 9, Body: "fresh", Attrs: "{}"},
		})
	}
	if sum, _ := st.Summary(ctx); sum.Traces != 200 {
		t.Fatalf("before: %+v", sum)
	}
	p, err := st.Prune(ctx, now.Add(-72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if p.Spans != 300 || p.Logs != 100 {
		t.Errorf("pruned %+v, want 300 spans and 100 logs", p)
	}
	sum, err := st.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Spans != 300 || sum.Logs != 100 || sum.Traces != 100 {
		t.Errorf("after: %+v, want the 300 fresh spans in 100 traces and 100 logs", sum)
	}
	logs, _ := st.Logs(ctx, Filter{Limit: 5000})
	for _, l := range logs {
		if l.Body != "fresh" {
			t.Fatalf("an old log survived: %+v", l)
		}
	}
	var oldest int64
	st.DB().QueryRow(`SELECT min(start_ns) FROM spans`).Scan(&oldest)
	if oldest < fresh {
		t.Errorf("a span from %s survived", time.Unix(0, oldest))
	}
	// Nothing left to prune is not an error, and prunes nothing.
	if p, err := st.Prune(ctx, now.Add(-72*time.Hour)); err != nil || p.rows() != 0 {
		t.Errorf("second prune: %+v, %v", p, err)
	}
}

// Rows keep arriving while old ones are pruned and the file is
// checkpointed, over and over. Every new row must be there at the end.
func TestIngestDuringPruneLosesNothing(t *testing.T) {
	st := scaleOpen(t, filepath.Join(t.TempDir(), "leadline.duckdb"), Options{FlushEvery: time.Millisecond})
	ctx := context.Background()
	now := time.Now()
	old := now.Add(-80 * time.Hour).UnixNano()
	const oldTraces, batches, perBatch = 3000, 60, 20
	var spans []Span
	for i := 0; i < oldTraces; i++ {
		spans = append(spans, scaleTrace(i, old+int64(i)*int64(time.Second))...)
	}
	st.AddSpans(spans)
	st.Flush()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for b := 0; b < batches; b++ {
			var spans []Span
			var logs []Log
			for i := 0; i < perBatch; i++ {
				n := 100_000 + b*perBatch + i
				at := now.UnixNano() + int64(n)
				spans = append(spans, scaleTrace(n, at)...)
				logs = append(logs, Log{TsNs: at, Service: "acme-api", Severity: "info", SeverityNum: 9, Body: "new", Attrs: "{}"})
			}
			if err := st.AddSpans(spans); err != nil {
				t.Error(err)
			}
			if err := st.AddLogs(logs); err != nil {
				t.Error(err)
			}
			time.Sleep(time.Millisecond)
		}
	}()
	// Prune a little more of the old data each time, as retention does.
	var pruned int64
	for step := 1; ; step++ {
		cutoff := time.Unix(0, old).Add(time.Duration(step) * 5 * time.Minute)
		p, err := st.Prune(ctx, cutoff)
		if err != nil {
			t.Fatal(err)
		}
		pruned += p.Spans
		select {
		case <-done:
		default:
			continue
		}
		break
	}
	p, err := st.Prune(ctx, now.Add(-72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if pruned += p.Spans; pruned != oldTraces*3 {
		t.Errorf("pruned %d old spans in all, want %d", pruned, oldTraces*3)
	}
	sum, err := st.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(batches * perBatch); sum.Spans != want*3 || sum.Traces != want || sum.Logs != want {
		t.Errorf("after ingest during pruning: %+v, want %d spans in %d traces and %d logs", sum, want*3, want, want)
	}
}

func TestRetainPrunesByItself(t *testing.T) {
	st := scaleOpen(t, "", Options{})
	now := time.Now()
	st.AddSpans(scaleTrace(1, now.Add(-80*time.Hour).UnixNano()))
	st.AddSpans(scaleTrace(2, now.Add(-time.Hour).UnixNano()))
	st.Retain(Retention{MaxAge: 72 * time.Hour, Every: 10 * time.Millisecond})
	// A row that turns old enough later goes on a later pass.
	deadline := time.Now().Add(5 * time.Second)
	for {
		sum, err := st.Summary(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if sum.Traces == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("still %d traces; retention should have dropped the old one", sum.Traces)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := st.Close(); err != nil { // and the loop stops with the store
		t.Fatal(err)
	}
}

func TestSizeCapDropsTheOldestOnce(t *testing.T) {
	st := scaleOpen(t, filepath.Join(t.TempDir(), "leadline.duckdb"), Options{})
	ctx := context.Background()
	now := time.Now().Add(-time.Hour).UnixNano()
	const traces = 20_000
	var spans []Span
	for i := 0; i < traces; i++ {
		spans = append(spans, scaleTrace(i, now+int64(i)*int64(time.Millisecond))...)
	}
	st.AddSpans(spans)
	st.Flush()
	if err := st.checkpoint(); err != nil {
		t.Fatal(err)
	}
	size, err := st.Size(ctx)
	if err != nil || size == 0 {
		t.Fatalf("size = %d, %v", size, err)
	}

	r := Retention{MaxBytes: size / 2}
	var floor int64
	if err := st.retain(ctx, r, &floor); err != nil {
		t.Fatal(err)
	}
	var left, oldest int64
	st.DB().QueryRow(`SELECT count(*), min(start_ns) FROM spans`).Scan(&left, &oldest)
	// Half the cap's worth should stay, give or take the estimate.
	if left < traces*3*3/10 || left > traces*3*6/10 {
		t.Errorf("%d of %d spans left after one pass at half the size", left, traces*3)
	}
	if oldest <= now {
		t.Error("the oldest spans are still there; the cap should take those first")
	}
	// This little data is one block of rows, which is only freed when it
	// is empty: the file is still over the cap. A second pass must not
	// cut again for space the first has yet to get back.
	if err := st.retain(ctx, r, &floor); err != nil {
		t.Fatal(err)
	}
	var after int64
	st.DB().QueryRow(`SELECT count(*) FROM spans`).Scan(&after)
	if after != left {
		t.Errorf("a second pass with nothing new cut %d more spans", left-after)
	}

	// Under the cap, nothing goes.
	if err := st.retain(ctx, Retention{MaxBytes: size * 10}, new(int64)); err != nil {
		t.Fatal(err)
	}
	st.DB().QueryRow(`SELECT count(*) FROM spans`).Scan(&after)
	if after != left {
		t.Errorf("a pass under the cap cut %d spans", left-after)
	}
}

func TestParseAgeAndSize(t *testing.T) {
	for in, want := range map[string]time.Duration{"72h": 72 * time.Hour, "7d": 7 * 24 * time.Hour, "0.5d": 12 * time.Hour, "90m": 90 * time.Minute, "0": 0, "off": 0, "": 0} {
		if got, err := ParseAge(in); err != nil || got != want {
			t.Errorf("ParseAge(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for in, want := range map[string]int64{"2GB": 2 << 30, "500mb": 500 << 20, "1.5g": 3 << 29, "4096": 4096, "0": 0, "off": 0} {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"soon", "-3h", "3 weeks", "-1d"} {
		if _, err := ParseAge(bad); err == nil {
			t.Errorf("ParseAge(%q) reported no error", bad)
		}
	}
	for _, bad := range []string{"big", "-2GB", "2TBs"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) reported no error", bad)
		}
	}
}

// Going quiet gives memory back; the store must be as usable after as it
// was before, with its memory limit where it was.
func TestTidyLeavesTheStoreAsItWas(t *testing.T) {
	ctx := context.Background()
	st := scaleOpen(t, filepath.Join(t.TempDir(), "leadline.duckdb"), Options{})
	for i := 0; i < 200; i++ {
		if err := st.AddSpans(scaleTrace(i, scaleEpoch+int64(i)*int64(time.Second))); err != nil {
			t.Fatal(err)
		}
	}
	before, err := st.Memory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st.tidy()
	after, err := st.Memory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before.DuckDB.LimitBytes < 400<<20 || after.DuckDB.LimitBytes != before.DuckDB.LimitBytes {
		t.Errorf("memory limit %d MB before and %d MB after, want the default both times", before.DuckDB.LimitBytes>>20, after.DuckDB.LimitBytes>>20)
	}
	if err := st.AddSpans(scaleTrace(999, scaleEpoch)); err != nil {
		t.Fatal(err)
	}
	if rows, err := st.Traces(ctx, Filter{Limit: 500}); err != nil || len(rows) != 201 {
		t.Errorf("after tidying: %d traces, %v; want 201", len(rows), err)
	}
}

// Storage says what is kept and what may be.
func TestStorage(t *testing.T) {
	ctx := context.Background()
	st := scaleOpen(t, filepath.Join(t.TempDir(), "leadline.duckdb"), Options{})
	st.Retain(Retention{MaxAge: 72 * time.Hour, MaxBytes: 2 << 30, Every: time.Hour})
	empty, err := st.Storage(ctx)
	if err != nil || empty.OldestMs != 0 || empty.MaxBytes != 2<<30 || empty.RetainMs != 72*3600*1000 {
		t.Fatalf("empty store: %+v, %v", empty, err)
	}
	now := time.Now().UnixNano()
	for i := 0; i < 50; i++ {
		if err := st.AddSpans(scaleTrace(i, now-int64(50-i)*int64(time.Minute))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DB().Exec(`CHECKPOINT`); err != nil {
		t.Fatal(err)
	}
	got, err := st.Storage(ctx)
	if err != nil || got.SizeBytes <= 0 || got.FileBytes < got.SizeBytes || got.OldestMs != float64(now-int64(50*time.Minute))/1e6 {
		t.Errorf("storage = %+v, %v; want a size and the oldest span's time", got, err)
	}
}
