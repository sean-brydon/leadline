package store_test

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/sean-brydon/leadline/internal/demo"
	"github.com/sean-brydon/leadline/internal/ingest"
	"github.com/sean-brydon/leadline/internal/store"
)

const (
	ordersQuery = "SELECT id, status, total_cents FROM orders WHERE user_id = ? ORDER BY created_at DESC LIMIT ?"
	itemsQuery  = "SELECT id, product_id, quantity FROM order_items WHERE order_id = ?"
)

// fixture is a store holding the demo's telemetry, with the same spans kept
// in Go so a test can work out what a breakdown ought to say.
type fixture struct {
	st     *store.Store
	spans  []store.Span
	window store.Window
}

func load(t *testing.T, traces int) *fixture {
	t.Helper()
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	// A fixed window, so the buckets are the same on every run.
	to := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	from := to.Add(-10 * time.Minute)
	req, _ := demo.New(7).Batch(traces, from, to)
	spans := ingest.Spans(req, false)
	if err := st.AddSpans(spans); err != nil {
		t.Fatal(err)
	}
	return &fixture{st: st, spans: spans, window: store.Window{
		SinceMs: float64(from.UnixMilli()),
		// Past the end: the last trace starts just inside it.
		UntilMs: float64(to.Add(time.Minute).UnixMilli()),
	}}
}

// want is what a breakdown should say about one endpoint, counted the slow
// and obvious way.
type want struct {
	requests, failed, queries int
	totalNs, dbNs, maxNs      int64
}

func (f *fixture) expect(service, name string) want {
	var w want
	for _, e := range f.spans {
		if e.Service != service || e.Name != name || (e.ParentID != "" && e.Kind != "server" && e.Kind != "consumer") {
			continue
		}
		w.requests++
		w.totalNs += e.DurationNs
		w.maxNs = max(w.maxNs, e.DurationNs)
		failed := e.Status == "error"
		for _, d := range f.spans {
			if d.TraceID != e.TraceID || d.StartNs < e.StartNs || d.StartNs > e.StartNs+e.DurationNs {
				continue
			}
			if d.DBSystem != "" {
				// BEGIN and COMMIT take time in the database and are no
				// query.
				if !control(d) {
					w.queries++
				}
				w.dbNs += d.DurationNs
			}
			failed = failed || d.Status == "error"
		}
		if failed {
			w.failed++
		}
	}
	return w
}

func control(sp store.Span) bool {
	return sp.DBStatement == "BEGIN" || sp.DBStatement == "COMMIT" || sp.DBStatement == "ROLLBACK"
}

func near(a, b float64) bool { return math.Abs(a-b) <= 1e-6*math.Max(1, math.Abs(b)) }

func TestEndpoints(t *testing.T) {
	f := load(t, 400)
	ctx := context.Background()
	got, err := f.st.Endpoints(ctx, f.window, store.EndpointQuery{Spark: 20})
	if err != nil {
		t.Fatal(err)
	}
	// Five kinds of trace, and the payments service's side of a checkout.
	if len(got.Rows) != 6 || got.Total != 6 {
		t.Fatalf("got %d endpoints (total %d), want 6: %+v", len(got.Rows), got.Total, got.Rows)
	}
	if len(got.T) != 20 {
		t.Errorf("spark has %d buckets, want 20", len(got.T))
	}
	var share float64
	for i, e := range got.Rows {
		w := f.expect(e.Service, e.Name)
		if int(e.Requests) != w.requests || int(e.Errors) != w.failed {
			t.Errorf("%s: %d requests and %d failed, want %d and %d", e.Name, e.Requests, e.Errors, w.requests, w.failed)
		}
		if !near(e.TotalMs, float64(w.totalNs)/1e6) || !near(e.MaxMs, float64(w.maxNs)/1e6) {
			t.Errorf("%s: total %v max %v, want %v and %v", e.Name, e.TotalMs, e.MaxMs, float64(w.totalNs)/1e6, float64(w.maxNs)/1e6)
		}
		if !near(e.Queries, float64(w.queries)/float64(w.requests)) {
			t.Errorf("%s: %v queries per request, want %v", e.Name, e.Queries, float64(w.queries)/float64(w.requests))
		}
		if !near(e.DBMs, float64(w.dbNs)/1e6/float64(w.requests)) {
			t.Errorf("%s: %vms in the database per request, want %v", e.Name, e.DBMs, float64(w.dbNs)/1e6/float64(w.requests))
		}
		if e.P50Ms > e.P95Ms || e.P95Ms > e.MaxMs {
			t.Errorf("%s: p50 %v, p95 %v, max %v are out of order", e.Name, e.P50Ms, e.P95Ms, e.MaxMs)
		}
		var spark float64
		for _, v := range e.Spark {
			spark += v
		}
		if len(e.Spark) != 20 || spark != float64(e.Requests) {
			t.Errorf("%s: sparkline has %d buckets adding to %v, want 20 and %d", e.Name, len(e.Spark), spark, e.Requests)
		}
		if i > 0 && e.TotalMs > got.Rows[i-1].TotalMs {
			t.Errorf("%s: not in order of total time", e.Name)
		}
		share += e.Share
	}
	if !near(share, 1) {
		t.Errorf("shares add to %v, want 1", share)
	}

	by := map[string]store.EndpointRow{}
	for _, e := range got.Rows {
		by[e.Name] = e
	}
	// The demo's order list is an N+1: an orders query, then 6 to 23 more.
	if q := by["GET /api/orders"].Queries; q < 7 || q > 24 {
		t.Errorf("GET /api/orders runs %v queries per request, want an N+1", q)
	}
	if q := by["GET /api/products"].Queries; q != 3 {
		t.Errorf("GET /api/products runs %v queries per request, want 3", q)
	}
	// Its search is one slow query: nearly all of the request is the database.
	if s := by["GET /api/search"]; s.P50Ms < 120 || s.DBMs < 0.9*s.AvgMs {
		t.Errorf("GET /api/search: p50 %vms, %vms of %vms in the database", s.P50Ms, s.DBMs, s.AvgMs)
	}
	// A declined card fails a span inside the checkout, not the checkout's own.
	if c := by["POST /api/checkout"]; c.Errors == 0 || c.Errors != by["POST /charge"].Errors {
		t.Errorf("checkout failed %d times, the charge %d times", c.Errors, by["POST /charge"].Errors)
	}
	if by["POST /charge"].Service != "acme-payments" || by["job send-receipt"].Service != "acme-worker" {
		t.Errorf("services are wrong: %+v", got.Rows)
	}
}

func TestEndpointsSortSearchAndScope(t *testing.T) {
	f := load(t, 300)
	ctx := context.Background()

	got, err := f.st.Endpoints(ctx, f.window, store.EndpointQuery{Sort: "requests", Asc: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(got.Rows); i++ {
		if got.Rows[i].Requests < got.Rows[i-1].Requests {
			t.Fatal("not sorted by requests, fewest first")
		}
	}
	if len(got.T) != 0 || got.Rows[0].Spark != nil {
		t.Error("sparklines came without being asked for")
	}

	// A sort the store doesn't know must not reach the SQL.
	if _, err := f.st.Endpoints(ctx, f.window, store.EndpointQuery{Sort: "1; DROP TABLE spans"}); err != nil {
		t.Fatal(err)
	}

	all, _ := f.st.Endpoints(ctx, f.window, store.EndpointQuery{})
	found, err := f.st.Endpoints(ctx, f.window, store.EndpointQuery{Search: "ORDERS", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(found.Rows) != 1 || found.Rows[0].Name != "GET /api/orders" {
		t.Fatalf("search found %+v", found.Rows)
	}
	// Searching narrows the list, not what "all request time" means.
	for _, e := range all.Rows {
		if e.Name == "GET /api/orders" && !near(e.Share, found.Rows[0].Share) {
			t.Errorf("share changed with the search: %v, then %v", e.Share, found.Rows[0].Share)
		}
	}

	pay := f.window
	pay.Service = "acme-payments"
	only, err := f.st.Endpoints(ctx, pay, store.EndpointQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(only.Rows) != 1 || only.Rows[0].Name != "POST /charge" || !near(only.Rows[0].Share, 1) {
		t.Errorf("acme-payments has %+v", only.Rows)
	}

	// Half the window holds about half the requests, and never all of them.
	half := f.window
	half.SinceMs += 5 * 60 * 1000
	later, err := f.st.Endpoints(ctx, half, store.EndpointQuery{})
	if err != nil {
		t.Fatal(err)
	}
	var total, recent int64
	for _, e := range all.Rows {
		total += e.Requests
	}
	for _, e := range later.Rows {
		recent += e.Requests
	}
	if recent < total/3 || recent > 2*total/3 {
		t.Errorf("the later half has %d of %d requests", recent, total)
	}

	none := f.window
	none.SinceMs, none.UntilMs = 0, 1000
	empty, err := f.st.Endpoints(ctx, none, store.EndpointQuery{Spark: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Rows) != 0 || empty.Rows == nil {
		t.Errorf("an empty window has rows %+v", empty.Rows)
	}
}

func TestEndpointDetail(t *testing.T) {
	f := load(t, 400)
	ctx := context.Background()
	ep := store.Endpoint{Service: "acme-api", Name: "GET /api/orders"}
	d, err := f.st.EndpointDetail(ctx, f.window, ep, 30)
	if err != nil {
		t.Fatal(err)
	}
	w := f.expect(ep.Service, ep.Name)
	if d.Stats == nil || int(d.Stats.Requests) != w.requests {
		t.Fatalf("stats = %+v, want %d requests", d.Stats, w.requests)
	}
	list, _ := f.st.Endpoints(ctx, f.window, store.EndpointQuery{})
	for _, e := range list.Rows {
		if e.Name == ep.Name && (!near(e.Share, d.Stats.Share) || !near(e.P95Ms, d.Stats.P95Ms) || !near(e.Queries, d.Stats.Queries)) {
			t.Errorf("the list says %+v and the detail %+v", e, *d.Stats)
		}
	}

	var requests float64
	for i, v := range d.Series.Requests {
		requests += v
		if (v == 0) != (d.Series.P95Ms[i] == nil) {
			t.Errorf("bucket %d has %v requests but p95 %v", i, v, d.Series.P95Ms[i])
		}
	}
	if len(d.Series.T) != 30 || int(requests) != w.requests {
		t.Errorf("series has %d buckets and %v requests, want 30 and %d", len(d.Series.T), requests, w.requests)
	}

	var binned int64
	for i, b := range d.Histogram.Bins {
		binned += b.Count
		if b.HiMs <= b.LoMs || (i > 0 && !near(b.LoMs, d.Histogram.Bins[i-1].HiMs)) {
			t.Errorf("bin %d runs %v to %v after %+v", i, b.LoMs, b.HiMs, d.Histogram.Bins[max(i-1, 0)])
		}
	}
	if int(binned) != w.requests {
		t.Errorf("the histogram holds %d requests, want %d", binned, w.requests)
	}
	if first, last := d.Histogram.Bins[0], d.Histogram.Bins[len(d.Histogram.Bins)-1]; first.Count == 0 || last.Count == 0 ||
		first.LoMs > d.Stats.P50Ms || last.HiMs < d.Stats.MaxMs {
		t.Errorf("the histogram runs %v to %v, around a p50 of %v and a max of %v", first.LoMs, last.HiMs, d.Stats.P50Ms, d.Stats.MaxMs)
	}

	var cells int64
	for _, n := range d.Heatmap.N {
		cells += n
	}
	if int(cells) != w.requests {
		t.Errorf("the heatmap holds %d requests, want %d", cells, w.requests)
	}

	// Two shapes: the orders query once a request, the items query many times.
	if len(d.Queries) != 2 {
		t.Fatalf("queries = %+v, want 2 shapes", d.Queries)
	}
	var calls int64
	for _, q := range d.Queries {
		calls += q.Calls
		switch q.Fingerprint {
		case ordersQuery:
			if q.PerRequest != 1 || q.MaxPerRequest != 1 || int(q.Requests) != w.requests {
				t.Errorf("orders query: %+v", q)
			}
		case itemsQuery:
			if q.PerRequest < 6 || q.MaxPerRequest < 6 || q.MaxPerRequest > 23 || int(q.Requests) != w.requests {
				t.Errorf("order_items query isn't an N+1: %+v", q)
			}
			if !strings.Contains(q.Example, "order_id = ") || strings.Contains(q.Example, "?") {
				t.Errorf("example = %q, want a real statement", q.Example)
			}
		default:
			t.Errorf("unexpected query %q", q.Fingerprint)
		}
	}
	if int(calls) != w.queries {
		t.Errorf("queries add to %d calls, want %d", calls, w.queries)
	}

	if len(d.Slowest) != 12 || !near(d.Slowest[0].DurationMs, float64(w.maxNs)/1e6) {
		t.Errorf("slowest = %+v, want 12 led by %vms", d.Slowest, float64(w.maxNs)/1e6)
	}
	for i := 1; i < len(d.Slowest); i++ {
		if d.Slowest[i].DurationMs > d.Slowest[i-1].DurationMs {
			t.Error("slowest requests are out of order")
		}
	}
	if len(d.Failing) != 0 {
		t.Errorf("the order list never fails, got %+v", d.Failing)
	}

	checkout, err := f.st.EndpointDetail(ctx, f.window, store.Endpoint{Service: "acme-api", Name: "POST /api/checkout"}, 30)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(checkout.Failing)) != min(checkout.Stats.Errors, 12) || len(checkout.Failing) == 0 {
		t.Errorf("checkout lists %d failing requests of %d", len(checkout.Failing), checkout.Stats.Errors)
	}
	for i, r := range checkout.Failing {
		if !r.Failed || (i > 0 && r.StartMs > checkout.Failing[i-1].StartMs) {
			t.Errorf("failing[%d] = %+v, want failed ones, newest first", i, r)
		}
	}

	gone, err := f.st.EndpointDetail(ctx, f.window, store.Endpoint{Service: "acme-api", Name: "GET /nowhere"}, 30)
	if err != nil {
		t.Fatal(err)
	}
	if gone.Stats != nil || len(gone.Histogram.Bins) != 0 || gone.Queries == nil || gone.Slowest == nil {
		t.Errorf("an unknown endpoint has %+v", gone)
	}
}

func TestQueryDetail(t *testing.T) {
	f := load(t, 400)
	ctx := context.Background()
	d, err := f.st.QueryDetail(ctx, f.window, store.QueryKey{DBSystem: "postgresql", Fingerprint: itemsQuery}, 30)
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	var totalNs, dbNs int64
	perTrace := map[string]int64{}
	for _, sp := range f.spans {
		if sp.DBSystem != "" {
			dbNs += sp.DurationNs
		}
		if sp.DBFingerprint == itemsQuery {
			calls++
			totalNs += sp.DurationNs
			perTrace[sp.TraceID] += sp.DurationNs
		}
	}
	s := d.Stats
	if s == nil || int(s.Calls) != calls || int(s.Traces) != len(perTrace) || !near(s.TotalMs, float64(totalNs)/1e6) {
		t.Fatalf("stats = %+v, want %d calls in %d traces", s, calls, len(perTrace))
	}
	if !near(s.Share, float64(totalNs)/float64(dbNs)) || s.MaxPerTrace < 6 {
		t.Errorf("share %v (want %v), max per trace %d", s.Share, float64(totalNs)/float64(dbNs), s.MaxPerTrace)
	}

	var seen float64
	for _, v := range d.Series.Calls {
		seen += v
	}
	var binned int64
	for _, b := range d.Histogram.Bins {
		binned += b.Count
	}
	if int(seen) != calls || int(binned) != calls {
		t.Errorf("series holds %v calls and the histogram %d, want %d", seen, binned, calls)
	}

	// Only the order list runs it, many times a request.
	orders := f.expect("acme-api", "GET /api/orders")
	if len(d.Endpoints) != 1 {
		t.Fatalf("endpoints = %+v, want only the order list", d.Endpoints)
	}
	e := d.Endpoints[0]
	if e.Name != "GET /api/orders" || int(e.Calls) != calls || int(e.EndpointRequests) != orders.requests ||
		e.Requests != e.EndpointRequests || e.PerRequest < 6 {
		t.Errorf("endpoint = %+v", e)
	}

	var worst int64
	for _, ns := range perTrace {
		worst = max(worst, ns)
	}
	if len(d.Traces) != 12 || !near(d.Traces[0].TotalMs, float64(worst)/1e6) || d.Traces[0].Name != "GET /api/orders" || d.Traces[0].Calls < 6 {
		t.Errorf("traces = %+v, want 12 led by %vms", d.Traces, float64(worst)/1e6)
	}

	// The slow search: one call a request, and the example is the slowest.
	var searchQuery string
	for _, sp := range f.spans {
		if strings.Contains(sp.DBFingerprint, "ILIKE") {
			searchQuery = sp.DBFingerprint
		}
	}
	search, err := f.st.QueryDetail(ctx, f.window, store.QueryKey{DBSystem: "postgresql", Fingerprint: searchQuery}, 30)
	if err != nil {
		t.Fatal(err)
	}
	if search.Stats.P50Ms < 120 || search.Stats.MaxPerTrace != 1 || !strings.Contains(search.Stats.Example, "ILIKE '%") {
		t.Errorf("search stats = %+v", search.Stats)
	}
	if len(search.Endpoints) != 1 || search.Endpoints[0].Name != "GET /api/search" || search.Endpoints[0].PerRequest != 1 {
		t.Errorf("search endpoints = %+v", search.Endpoints)
	}
	if !near(search.Traces[0].SlowestMs, search.Stats.MaxMs) {
		t.Errorf("the slowest trace took %vms, the slowest call %vms", search.Traces[0].SlowestMs, search.Stats.MaxMs)
	}

	// The worker's service never runs it.
	other := f.window
	other.Service = "acme-worker"
	none, err := f.st.QueryDetail(ctx, other, store.QueryKey{DBSystem: "postgresql", Fingerprint: itemsQuery}, 30)
	if err != nil {
		t.Fatal(err)
	}
	if none.Stats != nil || len(none.Endpoints) != 0 || len(none.Traces) != 0 || none.Endpoints == nil {
		t.Errorf("acme-worker has %+v", none)
	}
}

func TestRequestHeatmap(t *testing.T) {
	f := load(t, 400)
	h, err := f.st.RequestHeatmap(context.Background(), f.window, 40)
	if err != nil {
		t.Fatal(err)
	}
	var want int
	for _, sp := range f.spans {
		if sp.ParentID == "" || sp.Kind == "server" || sp.Kind == "consumer" {
			want++
		}
	}
	var got, most int64
	cells := map[[2]int32]bool{}
	for i, n := range h.N {
		got += n
		most = max(most, n)
		cell := [2]int32{h.X[i], h.Y[i]}
		if cells[cell] || h.X[i] < 0 || h.X[i] >= 40 || h.Y[i] < h.YMin || h.Y[i] > h.YMax || n <= 0 {
			t.Fatalf("bad cell %v with %d", cell, n)
		}
		cells[cell] = true
	}
	if int(got) != want || most != h.Max || len(h.T) != 40 {
		t.Errorf("heatmap holds %d requests (want %d), max %d (says %d), %d columns", got, want, most, h.Max, len(h.T))
	}
	// The demo's requests run from a few milliseconds to a few seconds.
	lo := math.Pow(10, float64(h.YMin)/float64(h.PerDecade))
	hi := math.Pow(10, float64(h.YMax+1)/float64(h.PerDecade))
	if lo < 1 || lo > 20 || hi < 400 || hi > 5000 {
		t.Errorf("bands run from %vms to %vms", lo, hi)
	}
}

func TestGroupedSeries(t *testing.T) {
	f := load(t, 400)
	ctx := context.Background()

	db, err := f.st.GroupedSeries(ctx, f.window, "db_time", 30, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{}
	for _, sp := range f.spans {
		if sp.DBSystem != "" {
			want[sp.DBSystem] += sp.DurationNs
		}
	}
	if len(db.Groups) != 3 || db.Groups[0].Key != "postgresql" {
		t.Fatalf("groups = %+v, want postgresql first of 3", db.Groups)
	}
	for _, g := range db.Groups {
		var sum float64
		for _, v := range g.Values {
			sum += v
		}
		if len(g.Values) != 30 || !near(sum, float64(want[g.Key])/1e6) || !near(g.Total, sum) {
			t.Errorf("%s: %d buckets adding to %v (total %v), want %v", g.Key, len(g.Values), sum, g.Total, float64(want[g.Key])/1e6)
		}
	}

	status, err := f.st.GroupedSeries(ctx, f.window, "status", 30, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, g := range status.Groups {
		got[g.Key] = g.Total
	}
	// Declined cards answer 402, twice over: the charge and the checkout.
	if got["2xx"] == 0 || got["4xx"] == 0 || got["other"] == 0 || got["5xx"] != 0 || int(got["4xx"])%2 != 0 {
		t.Errorf("status classes = %v", got)
	}

	top, err := f.st.GroupedSeries(ctx, f.window, "service", 30, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(top.Groups) != 2 || top.Groups[0].Key != "acme-api" {
		t.Errorf("top 2 services = %+v", top.Groups)
	}

	if _, err := f.st.GroupedSeries(ctx, f.window, "nonsense", 30, 0); err == nil {
		t.Error("an unknown grouping was accepted")
	}
}
