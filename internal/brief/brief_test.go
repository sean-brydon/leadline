package brief

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sean-brydon/leadline/internal/demo"
	"github.com/sean-brydon/leadline/internal/ingest"
	"github.com/sean-brydon/leadline/internal/store"
)

// The demo's shop, ten minutes of it, ending at a fixed time so a brief is
// about the same data on every run: an N+1 on GET /api/orders, a slow
// search query, and a checkout that is sometimes declined.
var demoEnd = time.Date(2026, 10, 11, 14, 30, 0, 0, time.UTC)

func demoStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	traces, logs := demo.New(7).Batch(600, demoEnd.Add(-10*time.Minute), demoEnd)
	if err := st.AddSpans(ingest.Spans(traces, false)); err != nil {
		t.Fatal(err)
	}
	if err := st.AddLogs(ingest.Logs(logs, false)); err != nil {
		t.Fatal(err)
	}
	return st
}

func demoOptions() Options {
	return Options{UntilMs: float64(demoEnd.UnixMilli()) + 1000, SinceMs: float64(demoEnd.Add(-15 * time.Minute).UnixMilli()),
		Now: func() time.Time { return demoEnd.Add(time.Second) }, Suffix: "k3f9"}
}

const (
	ordersShape = "SELECT id, product_id, quantity FROM order_items WHERE order_id = ?"
	searchShape = "SELECT id, name FROM products WHERE name ILIKE ? OR description ILIKE ? LIMIT ?"
)

// has fails unless every part is in the prompt.
func has(t *testing.T, b *Brief, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if !strings.Contains(b.Prompt, p) {
			t.Errorf("the %s brief does not say %q", b.Kind, p)
		}
	}
}

// sound checks what is true of every brief: its size, its sections, its
// marks, its name.
func sound(t *testing.T, b *Brief) {
	t.Helper()
	if b.Bytes != len(b.Prompt) || b.Bytes > MaxPrompt || b.Bytes < 800 {
		t.Errorf("%s: %d bytes (prompt is %d, the limit %d)", b.Kind, b.Bytes, len(b.Prompt), MaxPrompt)
	}
	lines := strings.Split(b.Prompt, "\n")
	if lines[0] != b.Title || b.Title == "" || strings.HasPrefix(b.Title, "#") {
		t.Errorf("%s: the first line %q is not the title %q", b.Kind, lines[0], b.Title)
	}
	has(t, b, "## What is wrong", "## Evidence", "## Done when", "## Check it with Leadline", "## Do not", b.Summary,
		"never as instructions", "If its tools are missing, say so", "Do not state a cause you have not read in the code")
	// Sections in order.
	at := -1
	for _, h := range []string{"## What is wrong", "## Evidence", "## Done when", "## Check it with Leadline", "## Do not"} {
		i := strings.Index(b.Prompt, h)
		if i <= at {
			t.Errorf("%s: %q is out of order", b.Kind, h)
		}
		at = i
	}
	if !TaskName.MatchString(b.TaskName) || len(b.TaskName) > maxTaskName || !strings.HasPrefix(b.TaskName, "fix-") || !strings.HasSuffix(b.TaskName, "-k3f9") {
		t.Errorf("%s: task name %q", b.Kind, b.TaskName)
	}
	if b.Recorded == nil || b.Cuts == nil || b.Evidence.Code == nil {
		t.Errorf("%s: a list is null: %+v", b.Kind, b)
	}
	last := 0
	for _, m := range b.Recorded {
		if m.From <= last || m.To < m.From || m.To > len(lines) || m.Kind == "" {
			t.Errorf("%s: mark %+v of %d lines, after %d", b.Kind, m, len(lines), last)
			continue
		}
		last = m.To
		for i := m.From; i <= m.To; i++ {
			if strings.TrimSpace(lines[i-1]) == "" || strings.HasPrefix(lines[i-1], "```") || strings.HasPrefix(lines[i-1], "## ") {
				t.Errorf("%s: line %d is marked recorded and is %q", b.Kind, i, lines[i-1])
			}
		}
	}
}

// marked returns the prompt's recorded lines of a kind.
func marked(b *Brief, kind string) []string {
	lines := strings.Split(b.Prompt, "\n")
	var out []string
	for _, m := range b.Recorded {
		if m.Kind == kind {
			out = append(out, lines[m.From-1:m.To]...)
		}
	}
	return out
}

func TestQueryBriefNPlusOne(t *testing.T) {
	st := demoStore(t)
	ctx, o := context.Background(), demoOptions()
	b := New(st)
	key, err := b.FindQuery(ctx, o, "order_items")
	if err != nil || key != (store.QueryKey{DBSystem: "postgresql", Fingerprint: ordersShape}) {
		t.Fatalf("found %+v, %v", key, err)
	}
	got, err := b.Query(ctx, key, o)
	if err != nil {
		t.Fatal(err)
	}
	sound(t, got)

	// The numbers are the store's own.
	det, err := st.QueryDetail(ctx, o.window(), key, 1)
	if err != nil {
		t.Fatal(err)
	}
	s, ev := det.Stats, got.Evidence.Query
	if ev.Calls != s.Calls || ev.MaxPerTrace != s.MaxPerTrace || ev.Traces != s.Traces || ev.P95Ms != s.P95Ms || ev.TotalMs != s.TotalMs || ev.Share != s.Share || ev.Errors != 0 {
		t.Errorf("evidence %+v, store %+v", ev, s)
	}
	if s.MaxPerTrace < NPlusOne || len(det.Endpoints) != 1 {
		t.Fatalf("the demo's N+1 is not one: %+v, %+v", s, det.Endpoints)
	}
	e := det.Endpoints[0]
	if got.Kind != "query" || got.Problem != "n_plus_one" || got.Service != "acme-api" || got.Title != "Fix the N+1 on order_items in GET /api/orders" || got.TaskName != "fix-n1-order-items-orders-k3f9" {
		t.Errorf("brief = %s %s %q %q in %s", got.Kind, got.Problem, got.Title, got.TaskName, got.Service)
	}
	has(t, got,
		fmt.Sprintf("`GET /api/orders` runs the same postgresql query up to %d times in a single request (%s times per request on average, over %s requests)", s.MaxPerTrace, per(e.PerRequest), count(e.EndpointRequests)),
		fmt.Sprintf("- Calls: %s in %s traces, at most %d in one trace", count(s.Calls), count(s.Traces), s.MaxPerTrace),
		fmt.Sprintf("- Time per call: average %s, p50 %s, p95 %s, max %s", ms(s.AvgMs), ms(s.P50Ms), ms(s.P95Ms), ms(s.MaxMs)),
		fmt.Sprintf("- Total: %s, which is %s of all database time in the window", ms(s.TotalMs), pct(s.Share)),
		"- Failed calls: none",
		fmt.Sprintf("- First seen %s, last seen %s", clock(ev.FirstMs), clock(ev.LastMs)),
		"```sql\n"+ordersShape+"\n```",
		fmt.Sprintf("- `GET /api/orders` (acme-api): %s per request on average, at most %d, in %s of its %s requests", per(e.PerRequest), e.MaxPerRequest, count(e.Requests), count(e.EndpointRequests)),
		fmt.Sprintf("- `%s` GET /api/orders: %d calls, %s in this query", det.Traces[0].TraceID, det.Traces[0].Calls, ms(det.Traces[0].TotalMs)),
		"The telemetry names no source file",
		fmt.Sprintf("runs this query once per request, or a fixed number of times that does not grow with the rows it returns, instead of up to %d.", s.MaxPerTrace),
		"`list_traces` with `search: \"GET /api/orders\"` and `minutes: 5`",
		fmt.Sprintf("`list_queries` with `search: \"order_items\"` and `minutes: 5`: this query's `max_per_trace` was %d", s.MaxPerTrace),
		"The numbers above are from the service `acme-api`",
	)
	// First and last seen are of the data, which the demo spread over the
	// ten minutes before its end.
	if ev.FirstMs < float64(demoEnd.Add(-10*time.Minute).UnixMilli()) || ev.LastMs > float64(demoEnd.UnixMilli())+500 || ev.LastMs <= ev.FirstMs {
		t.Errorf("seen %v to %v", ev.FirstMs, ev.LastMs)
	}

	// By default only the shape is in it: no statement as it ran.
	if lines := marked(got, "shape"); len(lines) != 1 || lines[0] != ordersShape || len(got.Recorded) != 1 || got.Recorded[0].Values {
		t.Errorf("marks = %+v, %q", got.Recorded, lines)
	}
	if ev.Example != "" || !got.Example.Available || got.Example.Included || s.Example == ordersShape {
		t.Errorf("example state %+v, evidence %q, the store's %q", got.Example, ev.Example, s.Example)
	}
	if raw, _ := json.Marshal(got); strings.Contains(string(raw), s.Example) || strings.Contains(string(raw), "order_id = 1") {
		t.Errorf("a real statement is in a brief that was not asked for one")
	}

	// Asked, it has one real statement, marked as holding values.
	o.Example = true
	real, err := b.Query(ctx, key, o)
	if err != nil {
		t.Fatal(err)
	}
	sound(t, real)
	if lines := marked(real, "example"); len(lines) != 1 || lines[0] != s.Example || real.Evidence.Query.Example != s.Example || !real.Example.Included || real.Example.Available {
		t.Errorf("example lines %q, state %+v, want %q", lines, real.Example, s.Example)
	}
	for _, m := range real.Recorded {
		if m.Values != (m.Kind == "example") {
			t.Errorf("mark %+v", m)
		}
	}
	// And it is otherwise the same brief.
	if strings.Replace(real.Prompt, "One real statement of that shape, the slowest call's:\n\n```sql\n"+s.Example+"\n```\n\n", "", 1) != got.Prompt {
		t.Errorf("the example changed more than itself")
	}
}

func TestQueryBriefSlow(t *testing.T) {
	st := demoStore(t)
	ctx, o := context.Background(), demoOptions()
	b := New(st)
	key, err := b.FindQuery(ctx, o, "ILIKE")
	if err != nil || key.Fingerprint != searchShape {
		t.Fatalf("found %+v, %v", key, err)
	}
	got, err := b.Query(ctx, key, o)
	if err != nil {
		t.Fatal(err)
	}
	sound(t, got)
	det, _ := st.QueryDetail(ctx, o.window(), key, 1)
	s := det.Stats
	if got.Problem != "slow" || got.Title != "Speed up the products query in GET /api/search" || got.TaskName != "fix-slow-products-search-k3f9" {
		t.Errorf("brief = %s %q %q", got.Problem, got.Title, got.TaskName)
	}
	has(t, got,
		fmt.Sprintf("This postgresql query took %s of all database time in the window: %s calls, p95 %s, max %s.", pct(s.Share), count(s.Calls), ms(s.P95Ms), ms(s.MaxMs)),
		fmt.Sprintf("this query's p95 is clearly below %s and its max below %s", ms(s.P95Ms), ms(s.MaxMs)),
		fmt.Sprintf("compare this query's `p95_ms` and `max_ms` with %s and %s", ms(s.P95Ms), ms(s.MaxMs)),
		"`list_queries` with `search: \"products\"`",
	)
	// The demo's search is the costliest thing it does, and slow.
	if s.Share < 0.5 || s.P95Ms < 400 || s.MaxPerTrace != 1 {
		t.Errorf("the demo's slow query: %+v", s)
	}
	// The search terms are values; none is in the brief.
	for _, term := range []string{"lamp", "desk", "chair", "shelf", "rug"} {
		if strings.Contains(got.Prompt, term) {
			t.Errorf("the brief holds the search term %q", term)
		}
	}
}

func TestEndpointBriefFailing(t *testing.T) {
	st := demoStore(t)
	ctx, o := context.Background(), demoOptions()
	b := New(st)
	ep, err := b.FindEndpoint(ctx, o, "checkout", "")
	if err != nil || ep != (store.Endpoint{Service: "acme-api", Name: "POST /api/checkout"}) {
		t.Fatalf("found %+v, %v", ep, err)
	}
	got, err := b.Endpoint(ctx, ep, o)
	if err != nil {
		t.Fatal(err)
	}
	sound(t, got)
	det, _ := st.EndpointDetail(ctx, o.window(), ep, 1)
	s, ev := det.Stats, got.Evidence.Endpoint
	if s.Errors == 0 || len(det.Failing) == 0 {
		t.Fatalf("the demo's checkout never failed: %+v", s)
	}
	if ev.Requests != s.Requests || ev.Errors != s.Errors || ev.P95Ms != s.P95Ms || ev.QueriesPerRequest != s.Queries || ev.Share != s.Share || len(ev.Failing) != min(len(det.Failing), maxEvidence/2) {
		t.Errorf("evidence %+v, store %+v", ev, s)
	}
	if got.Problem != "failing" || got.Title != "Fix the failing requests to POST /api/checkout" || got.TaskName != "fix-failing-checkout-k3f9" || got.Service != "acme-api" {
		t.Errorf("brief = %s %q %q", got.Problem, got.Title, got.TaskName)
	}
	rate := pct(float64(s.Errors) / float64(s.Requests))
	has(t, got,
		fmt.Sprintf("`POST /api/checkout` failed in %d of its %s requests (%s).", s.Errors, count(s.Requests), rate),
		fmt.Sprintf("- Requests: %s, of which %d failed (%s)", count(s.Requests), s.Errors, rate),
		fmt.Sprintf("- Latency: average %s, p50 %s, p95 %s, max %s", ms(s.AvgMs), ms(s.P50Ms), ms(s.P95Ms), ms(s.MaxMs)),
		fmt.Sprintf("- Per request: %s queries and %s in databases, on average", per(s.Queries), ms(s.DBMs)),
		fmt.Sprintf("which is %s of all request time in the window", pct(s.Share)),
		"The telemetry does not say whether that is a bug or a refusal the code intends",
		fmt.Sprintf("- `%s` at %s: %s, ", det.Failing[0].TraceID, clock(det.Failing[0].StartMs), ms(det.Failing[0].DurationMs)),
		"Where the 2 newest of them failed:",
		"- span `POST /charge` in acme-payments (HTTP 402), 2 times: `card declined`",
		"- acme-api error (2 times): `checkout failed: payment declined`",
		"- acme-payments error (2 times): `charge failed: card declined`",
		"The stack trace of the `PaymentDeclinedError`",
		"In the application's own code (2 of 5 frames):\n\n```\nchargeCard (/srv/acme/src/payments/charge.ts:48)\ncheckout (/srv/acme/src/routes/checkout.ts:31)\n```",
		"In libraries, the framework or the runtime (3, the nearest first):\n\n```\nLayer.handle (/srv/acme/node_modules/express/lib/router/layer.js:95)",
		"- `/srv/acme/src/payments/charge.ts:48` in `chargeCard` (from the first of the application's own frames in the stack trace)",
		"`list_traces` with `search: \"POST /api/checkout\"`, `errors_only: true` and `minutes: 5`",
		fmt.Sprintf("Before, %d of %s failed.", s.Errors, count(s.Requests)),
		"Or: you have found where the code decides to refuse it, shown that the refusal is intended, and changed nothing.",
		"Do not silence the failure",
	)
	if len(got.Evidence.Code) != 1 || got.Evidence.Code[0].File != "/srv/acme/src/payments/charge.ts" || got.Evidence.Code[0].Line != 48 || got.Evidence.Code[0].Function != "chargeCard" {
		t.Errorf("code = %+v", got.Evidence.Code)
	}
	if len(ev.Frames) != 5 || !ev.Frames[0].App || !ev.Frames[1].App || ev.Frames[2].App || ev.Frames[4].App {
		t.Errorf("frames = %+v", ev.Frames)
	}
	// The values a checkout writes are not in it.
	if strings.Contains(got.Prompt, "'pending'") || len(marked(got, "example")) != 0 {
		t.Errorf("a real statement is in the brief")
	}
}

func TestEndpointBriefNPlusOne(t *testing.T) {
	st := demoStore(t)
	ctx, o := context.Background(), demoOptions()
	b := New(st)
	ep := store.Endpoint{Service: "acme-api", Name: "GET /api/orders"}
	got, err := b.Endpoint(ctx, ep, o)
	if err != nil {
		t.Fatal(err)
	}
	sound(t, got)
	det, _ := st.EndpointDetail(ctx, o.window(), ep, 1)
	s := det.Stats
	var q store.EndpointQueryRow
	for _, r := range det.Queries {
		if r.Fingerprint == ordersShape {
			q = r
		}
	}
	if got.Problem != "n_plus_one" || got.Title != "Fix the N+1 in GET /api/orders" || got.TaskName != "fix-n1-orders-k3f9" || q.MaxPerRequest < NPlusOne {
		t.Errorf("brief = %s %q %q, query %+v", got.Problem, got.Title, got.TaskName, q)
	}
	has(t, got,
		fmt.Sprintf("`GET /api/orders` runs one postgresql query up to %d times in a single request (%s times per request on average, over %s requests)", q.MaxPerRequest, per(q.PerRequest), count(s.Requests)),
		fmt.Sprintf("- Requests: %s, none failed", count(s.Requests)),
		fmt.Sprintf("- %s per request, at most %d in one request: an N+1, %s each on average, postgresql: `%s`", per(q.PerRequest), q.MaxPerRequest, ms(q.AvgMs), ordersShape),
		fmt.Sprintf("- `%s`: %s, %d queries, %s in databases", det.Slowest[0].TraceID, ms(det.Slowest[0].DurationMs), det.Slowest[0].Queries, ms(det.Slowest[0].DBMs)),
		fmt.Sprintf("instead of up to %d; %s queries per request on average becomes a small fixed number", q.MaxPerRequest, per(s.Queries)),
		fmt.Sprintf("`list_queries` with `search: \"order_items\"` and `minutes: 5`: its `max_per_trace` was %d", q.MaxPerRequest),
	)
	// The healthy endpoint is written up as what it costs, and no more.
	got, err = b.Endpoint(ctx, store.Endpoint{Service: "acme-api", Name: "GET /api/products"}, o)
	if err != nil {
		t.Fatal(err)
	}
	sound(t, got)
	if got.Problem != "slow" || strings.Contains(got.Prompt, "N+1") || strings.Contains(got.Prompt, "failed in") {
		t.Errorf("the healthy endpoint: %s\n%s", got.Problem, got.Prompt)
	}
}

func TestTraceBrief(t *testing.T) {
	st := demoStore(t)
	ctx, o := context.Background(), demoOptions()
	b := New(st)

	// A declined checkout.
	failing, _ := st.EndpointDetail(ctx, o.window(), store.Endpoint{Service: "acme-api", Name: "POST /api/checkout"}, 1)
	id := failing.Failing[0].TraceID
	got, err := b.Trace(ctx, strings.ToUpper(id)+" ", o)
	if err != nil {
		t.Fatal(err)
	}
	sound(t, got)
	spans, logs, _ := st.Trace(ctx, id)
	failed, queries := 0, 0
	for _, sp := range spans {
		if sp.Status == "error" {
			failed++
		}
		if sp.DBRole == "query" {
			queries++
		}
	}
	ev := got.Evidence.Trace
	if ev.TraceID != id || ev.Spans != len(spans) || ev.Failed != failed || ev.Queries != queries || failed != 2 || len(ev.Logs) != 2 || len(logs) != 2 ||
		ev.Name != "POST /api/checkout" || ev.Service != "acme-api" || math.Abs(ev.DurationMs-failing.Failing[0].DurationMs) > 0.01 {
		t.Errorf("evidence %+v (store: %d spans, %d failed, %d queries, %v ms)", ev, len(spans), failed, queries, failing.Failing[0].DurationMs)
	}
	if got.Problem != "failing" || got.Title != "Fix the failed request POST /api/checkout" || got.TaskName != "fix-failed-checkout-k3f9" {
		t.Errorf("brief = %s %q %q", got.Problem, got.Title, got.TaskName)
	}
	has(t, got,
		fmt.Sprintf("trace `%s`, in service `acme-api`, started %s", id, clock(ev.StartMs)),
		fmt.Sprintf("This `POST /api/checkout` request failed after %s: 2 of its %d spans ended in error.", ms(ev.DurationMs), len(spans)),
		fmt.Sprintf("- Database: %d queries, %s", queries, ms(ev.DBMs)),
		"- Answered HTTP 402",
		"Where it failed:",
		"- span `POST /charge` in acme-payments (HTTP 402): `card declined`",
		"- acme-api error: `checkout failed: payment declined`",
		"chargeCard (/srv/acme/src/payments/charge.ts:48)",
		fmt.Sprintf("`get_trace` with `trace_id: %q`", id),
		fmt.Sprintf("Compare with the numbers above: %s, %d queries, 2 spans in error.", ms(ev.DurationMs), queries),
	)

	// A request with the N+1 in it: the count is the trace's own.
	orders, _ := st.EndpointDetail(ctx, o.window(), store.Endpoint{Service: "acme-api", Name: "GET /api/orders"}, 1)
	id = orders.Slowest[0].TraceID
	got, err = b.Trace(ctx, id, o)
	if err != nil {
		t.Fatal(err)
	}
	sound(t, got)
	spans, _, _ = st.Trace(ctx, id)
	repeats := 0
	for _, sp := range spans {
		if strings.Contains(sp.DBStatement, "order_items") {
			repeats++
		}
	}
	ev = got.Evidence.Trace
	if got.Problem != "n_plus_one" || len(ev.Repeated) != 1 || ev.Repeated[0].Count != int64(repeats) || ev.Repeated[0].Shape != ordersShape || ev.Queries != int(orders.Slowest[0].Queries) {
		t.Errorf("repeated = %+v, counted %d, queries %d vs %d", ev.Repeated, repeats, ev.Queries, orders.Slowest[0].Queries)
	}
	has(t, got,
		fmt.Sprintf("This `GET /api/orders` request ran %d queries in %s, one statement %d times.", ev.Queries, ms(ev.DurationMs), repeats),
		fmt.Sprintf("- %d times, %s in all: `%s`", repeats, ms(ev.Repeated[0].TotalMs), ordersShape),
		fmt.Sprintf("instead of %d; before, it ran %d queries in all", repeats, ev.Queries),
	)
	// The statements ran with order ids; the brief has their shape alone.
	for _, sp := range spans {
		if sp.DBStatement != "" && strings.Contains(got.Prompt, sp.DBStatement) {
			t.Errorf("the brief holds the statement %q", sp.DBStatement)
		}
	}
	if !got.Example.Available {
		t.Errorf("a trace of real statements says there is nothing real to add: %+v", got.Example)
	}
}

func TestLogBrief(t *testing.T) {
	st := demoStore(t)
	ctx, o := context.Background(), demoOptions()
	b := New(st)
	ref, err := b.FindLog(ctx, o, "checkout failed", "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.Log(ctx, ref, o)
	if err != nil {
		t.Fatal(err)
	}
	sound(t, got)
	// As many as the store holds of that line.
	all, _ := st.Logs(ctx, store.Filter{Service: "acme-api", Search: "checkout failed: payment declined", MinSeverity: 17})
	ev := got.Evidence.Log
	if len(all) < 2 || ev.Count != int64(len(all)) || ev.Capped || ev.Shape != "checkout failed: payment declined" || ev.Service != "acme-api" || ev.Severity != "error" ||
		ev.LastMs != all[0].TsMs || ev.FirstMs != all[len(all)-1].TsMs || ev.TraceID != all[0].TraceID || ev.Request != "POST /api/checkout" || ev.ExceptionType != "PaymentDeclinedError" {
		t.Errorf("evidence %+v, the store has %d", ev, len(all))
	}
	if got.Problem != "failing" || got.Kind != "log" || got.Title != "Error in acme-api: checkout failed: payment declined" || got.TaskName != "fix-error-checkout-failed-payment-k3f9" {
		t.Errorf("brief = %s %q %q", got.Problem, got.Title, got.TaskName)
	}
	has(t, got,
		fmt.Sprintf("The service `acme-api` logged this error %d times in the last 15 minutes.", len(all)),
		fmt.Sprintf("- First at %s, last at %s", clock(ev.FirstMs), clock(ev.LastMs)),
		fmt.Sprintf("- This one was written during the request `POST /api/checkout`, trace `%s`", ev.TraceID),
		"```\ncheckout failed: payment declined\n```",
		"chargeCard (/srv/acme/src/payments/charge.ts:48)",
		"- span `POST /charge` in acme-payments (HTTP 402): `card declined`",
		fmt.Sprintf("`search_logs` with `search: \"checkout failed: payment declined\"`, `level: \"error\"` and `minutes: 5`: no record from your run. Before, %d times in the window.", len(all)),
		"The telemetry does not say whether it reports a bug or a condition the code expects",
		"no deleting, rewording or downgrading the log",
	)
	// This line has no values in it, so there is nothing real to add.
	if got.Example.Available {
		t.Errorf("example = %+v", got.Example)
	}
	// The title is the log's own words, and is marked as such.
	if got.Recorded[0] != (Mark{From: 1, To: 1, Kind: "log"}) {
		t.Errorf("the title's mark = %+v", got.Recorded[0])
	}

	// With no words, the newest error; and a search that matches nothing.
	if ref, err = b.FindLog(ctx, o, "", "acme-payments"); err != nil || ref.Service != "acme-payments" {
		t.Errorf("newest error = %+v, %v", ref, err)
	}
	if _, err = b.FindLog(ctx, o, "no such words", ""); !IsNotFound(err) {
		t.Errorf("no match: %v", err)
	}
}

// What a brief must never do by default: carry a value. A store is given
// statements, log lines, error messages and a stack whose values are all
// marked, and none may come out unless asked.
func TestNoRealValuesUnlessAsked(t *testing.T) {
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	at := demoEnd.Add(-time.Minute)
	ns := func(d time.Duration) int64 { return at.Add(d).UnixNano() }
	const trace = "aaaabbbbccccddddeeeeffff00001111"
	statement := func(i int) string {
		return fmt.Sprintf("SELECT * FROM users WHERE email = 'secret%d@acme.test' AND token = 'SECRETTOKEN%d' AND age > %d", i, i, 4200+i)
	}
	shape := ingest.Fingerprint(statement(0))
	spans := []store.Span{{TraceID: trace, SpanID: "root", Service: "acme-api", Name: "POST /api/login", Kind: "server", StartNs: ns(0), DurationNs: int64(900 * time.Millisecond),
		Status: "error", StatusMessage: `login failed for 'secret1@acme.test' from 10.20.30.40`, HTTPMethod: "POST", HTTPRoute: "/api/login", HTTPStatus: 500,
		Attrs: `{"code.filepath":"src/routes/login.ts","code.lineno":17,"code.function":"login","next.file":"app/api/login/route.ts"}`}}
	for i := 0; i < 6; i++ {
		sp := store.Span{TraceID: trace, SpanID: fmt.Sprintf("q%d", i), ParentID: "root", Service: "acme-api", Name: "pg.query", Kind: "client",
			StartNs: ns(time.Duration(i+1) * 10 * time.Millisecond), DurationNs: int64(5 * time.Millisecond),
			DBSystem: "postgresql", DBStatement: statement(i), DBFingerprint: shape}
		if i == 0 {
			sp.Status = "error"
			sp.StatusMessage = `duplicate key value violates unique constraint "users_email_key" Detail: Key (email)=(secret0@acme.test) already exists.`
		}
		spans = append(spans, sp)
	}
	if err := st.AddSpans(spans); err != nil {
		t.Fatal(err)
	}
	stack := "LoginError: no user secret1@acme.test (id 987654)\n    at login (/srv/acme/src/routes/login.ts:17:9)\n    at handle (/srv/acme/node_modules/express/lib/router/layer.js:95:5)"
	attrs, _ := json.Marshal(map[string]string{"exception.type": "LoginError", "exception.message": "no user secret1@acme.test", "exception.stacktrace": stack})
	logs := []store.Log{
		{TsNs: ns(500 * time.Millisecond), Service: "acme-api", Severity: "error", SeverityNum: 17, Body: `login failed for "secret1@acme.test" after 3 tries, session 9f8e7d6c5b4a39281706f5e4d3c2b1a0`, TraceID: trace, SpanID: "root", Attrs: string(attrs)},
		{TsNs: ns(-30 * time.Second), Service: "acme-api", Severity: "error", SeverityNum: 17, Body: `login failed for "secret2@acme.test" after 5 tries, session 00112233445566778899aabbccddeeff`},
		{TsNs: ns(-20 * time.Second), Service: "acme-api", Severity: "error", SeverityNum: 17, Body: `something else failed for "secret3@acme.test"`},
	}
	if err := st.AddLogs(logs); err != nil {
		t.Fatal(err)
	}

	b := New(st)
	ctx := context.Background()
	build := func(o Options) []*Brief {
		t.Helper()
		ref, err := b.FindLog(ctx, o, "login failed", "")
		if err != nil {
			t.Fatal(err)
		}
		var out []*Brief
		for _, write := range []func() (*Brief, error){
			func() (*Brief, error) {
				return b.Query(ctx, store.QueryKey{DBSystem: "postgresql", Fingerprint: shape}, o)
			},
			func() (*Brief, error) {
				return b.Endpoint(ctx, store.Endpoint{Service: "acme-api", Name: "POST /api/login"}, o)
			},
			func() (*Brief, error) { return b.Trace(ctx, trace, o) },
			func() (*Brief, error) { return b.Log(ctx, ref, o) },
		} {
			got, err := write()
			if err != nil {
				t.Fatal(err)
			}
			sound(t, got)
			out = append(out, got)
		}
		return out
	}
	secrets := []string{"secret", "SECRETTOKEN", "acme.test", "4200", "4205", "10.20.30.40", "987654", "9f8e7d6c5b4a", "3 tries"}
	o := demoOptions()
	for _, got := range build(o) {
		raw, _ := json.Marshal(got)
		for _, secret := range secrets {
			if strings.Contains(string(raw), secret) {
				t.Errorf("the %s brief holds %q without being asked:\n%s", got.Kind, secret, got.Prompt)
			}
		}
		if !got.Example.Available || got.Example.Included {
			t.Errorf("%s: example = %+v", got.Kind, got.Example)
		}
		for _, m := range got.Recorded {
			if m.Values {
				t.Errorf("%s: a line is marked as holding values: %+v", got.Kind, m)
			}
		}
		// What is there instead: shapes, and the code's own names.
		switch got.Kind {
		case "query":
			has(t, got, shape, "- Failed: `duplicate key value violates unique constraint ? Detail: Key (email)=(?) already exists.`", "- Failed calls: 1 of 6",
				"`src/routes/login.ts:17` in `login` (from `code.filepath` on the request `POST /api/login`)", "`app/api/login/route.ts` (from `next.file`")
			if got.Problem != "n_plus_one" {
				t.Errorf("query problem = %s", got.Problem)
			}
		case "endpoint", "trace":
			has(t, got, "login failed for ? from ?", "login failed for ? after ? tries, session ?", "login (/srv/acme/src/routes/login.ts:17)", "LoginError: no user ? (id ?)", "`src/routes/login.ts:17`")
		case "log":
			has(t, got, "```\nlogin failed for ? after ? tries, session ?\n```", "logged this error 2 times", "Error in acme-api: login failed for ? after ? tries, session ?")
			if got.Evidence.Log.Count != 2 {
				t.Errorf("like logs = %d", got.Evidence.Log.Count)
			}
		}
	}

	// Asked, each has the real thing, and says which lines hold it.
	o.Example = true
	for _, got := range build(o) {
		values := 0
		for _, m := range got.Recorded {
			if m.Values {
				values++
			}
		}
		if !got.Example.Included || values == 0 || !strings.Contains(got.Prompt, "acme.test") {
			t.Errorf("the %s brief was asked for real values and has %d marked lines:\n%s", got.Kind, values, got.Prompt)
		}
		switch got.Kind {
		case "query":
			has(t, got, "One real statement of that shape, the slowest call's:", "email = 'secret")
		case "log":
			has(t, got, "The line as it was written:", `login failed for "secret1@acme.test" after 3 tries`)
		case "trace", "endpoint":
			has(t, got, "login failed for 'secret1@acme.test' from 10.20.30.40")
		}
	}
}

// A brief stays small whatever it is about: an enormous statement, a
// query every endpoint runs, a stack hundreds of frames deep.
func TestSizeBounds(t *testing.T) {
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	at := demoEnd.Add(-time.Minute)
	columns := make([]string, 4000)
	for i := range columns {
		columns[i] = fmt.Sprintf("column_number_%d", i)
	}
	huge := "SELECT " + strings.Join(columns, ", ") + " FROM wide_table WHERE id = 7"
	shape := ingest.Fingerprint(huge)
	longName := "GET /api/" + strings.Repeat("very-long-segment/", 40)
	var spans []store.Span
	var logs []store.Log
	var frames []string
	for i := 0; i < 300; i++ {
		frames = append(frames, fmt.Sprintf("    at fn%d (/srv/acme/src/deep/%s/file%d.ts:%d:1)", i, strings.Repeat("d", 60), i, i+1))
	}
	stack := "Error: " + strings.Repeat("too much ", 2000) + "\n" + strings.Join(frames, "\n")
	attrs, _ := json.Marshal(map[string]string{"exception.type": "Error", "exception.stacktrace": stack})
	for e := 0; e < 60; e++ {
		trace := fmt.Sprintf("%032x", e+1)
		name := fmt.Sprintf("%s%d", longName, e)
		start := at.Add(time.Duration(e) * time.Millisecond).UnixNano()
		spans = append(spans, store.Span{TraceID: trace, SpanID: "root", Service: "acme-api", Name: name, Kind: "server", StartNs: start, DurationNs: int64(time.Second), Status: "error", StatusMessage: strings.Repeat("it broke badly ", 400), HTTPStatus: 500})
		for i := 0; i < 8; i++ {
			spans = append(spans, store.Span{TraceID: trace, SpanID: fmt.Sprintf("q%d", i), ParentID: "root", Service: "acme-api", Name: "pg.query", Kind: "client",
				StartNs: start + int64(i+1)*int64(time.Millisecond), DurationNs: int64(time.Millisecond), DBSystem: "postgresql", DBStatement: huge, DBFingerprint: shape,
				Status: "error", StatusMessage: strings.Repeat("relation missing ", 300)})
		}
		logs = append(logs, store.Log{TsNs: start + int64(20*time.Millisecond), Service: "acme-api", Severity: "error", SeverityNum: 17, Body: strings.Repeat("a very long log line ", 500), TraceID: trace, SpanID: "root", Attrs: string(attrs)})
	}
	if err := st.AddSpans(spans); err != nil {
		t.Fatal(err)
	}
	if err := st.AddLogs(logs); err != nil {
		t.Fatal(err)
	}
	b := New(st)
	ctx := context.Background()
	for _, example := range []bool{false, true} {
		o := demoOptions()
		o.Example = example
		ref, err := b.FindLog(ctx, o, "", "")
		if err != nil {
			t.Fatal(err)
		}
		for _, write := range []func() (*Brief, error){
			func() (*Brief, error) {
				return b.Query(ctx, store.QueryKey{DBSystem: "postgresql", Fingerprint: shape}, o)
			},
			func() (*Brief, error) {
				return b.Endpoint(ctx, store.Endpoint{Service: "acme-api", Name: longName + "0"}, o)
			},
			func() (*Brief, error) { return b.Trace(ctx, fmt.Sprintf("%032x", 1), o) },
			func() (*Brief, error) { return b.Log(ctx, ref, o) },
		} {
			got, err := write()
			if err != nil {
				t.Fatal(err)
			}
			sound(t, got)
			if len(got.Cuts) == 0 {
				t.Errorf("the %s brief cut nothing: %d bytes", got.Kind, got.Bytes)
			}
			if !strings.Contains(got.Prompt, "(cut: ") && !strings.Contains(got.Prompt, "Leadline cut the rest of the evidence") && !strings.Contains(got.Prompt, "more of the application's frames") {
				t.Errorf("the %s brief does not say in its text that it was cut", got.Kind)
			}
			// Every code block that opens is closed.
			if strings.Count(got.Prompt, "\n```")%2 != 0 {
				t.Errorf("the %s brief leaves a code block open:\n%s", got.Kind, got.Prompt)
			}
			for _, l := range strings.Split(got.Prompt, "\n") {
				if len(l) > 1500 {
					t.Errorf("the %s brief has a line of %d bytes", got.Kind, len(l))
				}
			}
		}
	}

	// A document that is still too big loses the end of its evidence, with
	// a note, and nothing of what follows.
	d := &doc{}
	d.say("Title")
	d.evidence(func() {
		d.say("## Evidence")
		d.say("```")
		for i := 0; i < 400; i++ {
			d.rec("stack", false, strings.Repeat("x", 40))
		}
		d.say("```")
	})
	d.say("## Done when")
	d.say("- it is done")
	prompt, marks, cuts := d.finish()
	if len(prompt) > MaxPrompt || !strings.HasSuffix(prompt, "## Done when\n- it is done\n") || !strings.Contains(prompt, "(Leadline cut the rest of the evidence to keep this prompt small.)") ||
		len(cuts) != 1 || strings.Count(prompt, "```") != 2 || len(marks) != 1 || marks[0].To >= 400 {
		t.Errorf("cut document: %d bytes, cuts %q, marks %+v, ends %q", len(prompt), cuts, marks, prompt[max(0, len(prompt)-200):])
	}
}

func TestNotFound(t *testing.T) {
	st := demoStore(t)
	ctx, o := context.Background(), demoOptions()
	b := New(st)
	if _, err := b.Query(ctx, store.QueryKey{DBSystem: "postgresql", Fingerprint: "SELECT nothing"}, o); !IsNotFound(err) || err.Error() != "This query didn't run in this window." {
		t.Errorf("query: %v", err)
	}
	if _, err := b.Endpoint(ctx, store.Endpoint{Service: "acme-api", Name: "GET /nope"}, o); !IsNotFound(err) {
		t.Errorf("endpoint: %v", err)
	}
	if _, err := b.Trace(ctx, "00000000000000000000000000000000", o); !IsNotFound(err) {
		t.Errorf("trace: %v", err)
	}
	if _, err := b.Log(ctx, LogRef{Ns: 1, Place: 1}, o); !IsNotFound(err) {
		t.Errorf("log: %v", err)
	}
	if _, err := b.FindQuery(ctx, o, "no_such_table"); !IsNotFound(err) {
		t.Errorf("find query: %v", err)
	}
	if _, err := b.FindEndpoint(ctx, o, "no such endpoint", ""); !IsNotFound(err) {
		t.Errorf("find endpoint: %v", err)
	}
	if _, err := ParseLogRef("not a cursor", ""); err == nil {
		t.Errorf("a cursor that is none was read")
	}
	// Outside the window the demo's data is not there either.
	o.SinceMs, o.UntilMs = float64(demoEnd.Add(time.Hour).UnixMilli()), float64(demoEnd.Add(2*time.Hour).UnixMilli())
	if _, err := b.Query(ctx, store.QueryKey{DBSystem: "postgresql", Fingerprint: ordersShape}, o); !IsNotFound(err) {
		t.Errorf("query outside the window: %v", err)
	}
}

func TestShape(t *testing.T) {
	for in, want := range map[string]string{
		"checkout failed: payment declined":                            "checkout failed: payment declined",
		"loaded 23 orders for user 4821":                               "loaded ? orders for user ?",
		`slow search for "lamp" took 1890ms`:                           "slow search for ? took ?ms",
		"user ada@acme.test logged in from 10.0.0.7":                   "user ? logged in from ?",
		"request 7f3c1a9e-2b4d-4c6e-8f10-a1b2c3d4e5f6 failed":          "request ? failed",
		"session 9f8e7d6c5b4a39281706f5e4d3c2b1a0 expired at 14:03:10": "session ? expired at ?",
		"token acmekey_51Habc123DEFghi456JKLmno789 rejected":           "token ? rejected",
		`Key (email)=(ada@acme.test) already exists`:                   "Key (email)=(?) already exists",
		"values 'a', 'b', 'c' and 1, 2, 3":                             "values ? and ?",
		"PaymentDeclinedError: payment declined":                       "PaymentDeclinedError: payment declined",
		"InternationalizationConfigurationError in handler":            "InternationalizationConfigurationError in handler",
		"  spaced  ":            "spaced",
		"p95 is over the limit": "p95 is over the limit",
	} {
		if got := Shape(in); got != want {
			t.Errorf("Shape(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseStack(t *testing.T) {
	type want struct {
		head   string
		frames []Frame
	}
	for name, c := range map[string]struct {
		stack string
		want  want
	}{
		"node": {
			"TypeError: Cannot read properties of undefined (reading 'id')\n    at chargeCard (/srv/acme/src/payments/charge.ts:48:13)\n    at async /srv/acme/src/routes/checkout.ts:31:20\n    at Layer.handle [as handle_request] (/srv/acme/node_modules/express/lib/router/layer.js:95:5)\n    at new Promise (<anonymous>)\n    at process.processTicksAndRejections (node:internal/process/task_queues:95:5)\n    at Page (webpack-internal:///(rsc)/./src/app/page.tsx:12:5)",
			want{"TypeError: Cannot read properties of undefined (reading 'id')", []Frame{
				{Function: "chargeCard", File: "/srv/acme/src/payments/charge.ts", Line: 48, App: true},
				{File: "/srv/acme/src/routes/checkout.ts", Line: 31, App: true},
				{Function: "Layer.handle [as handle_request]", File: "/srv/acme/node_modules/express/lib/router/layer.js", Line: 95},
				{Function: "process.processTicksAndRejections", File: "node:internal/process/task_queues", Line: 95},
				{Function: "Page", File: "webpack-internal:///(rsc)/./src/app/page.tsx", Line: 12},
			}},
		},
		"python": {
			"Traceback (most recent call last):\n  File \"/srv/acme/app/views.py\", line 31, in checkout\n    charge(card)\n  File \"/srv/acme/.venv/lib/python3.12/site-packages/django/core/handlers/base.py\", line 197, in _get_response\n    response = wrapped_callback(request)\nValueError: card declined",
			want{"", []Frame{
				{Function: "checkout", File: "/srv/acme/app/views.py", Line: 31, App: true},
				{Function: "_get_response", File: "/srv/acme/.venv/lib/python3.12/site-packages/django/core/handlers/base.py", Line: 197},
			}},
		},
		"go": {
			"panic: runtime error: index out of range [3] with length 3\n\ngoroutine 1 [running]:\nacme/shop.Charge(0x1400010e000)\n\t/srv/acme/shop/charge.go:48 +0x1d4\nnet/http.HandlerFunc.ServeHTTP(...)\n\t/usr/local/go/src/net/http/server.go:2166 +0x38\ngithub.com/acme/dep.Run()\n\t/home/dev/go/pkg/mod/github.com/acme/dep@v1.2.3/run.go:10 +0x20",
			want{"panic: runtime error: index out of range [3] with length 3", []Frame{
				{Function: "acme/shop.Charge", File: "/srv/acme/shop/charge.go", Line: 48, App: true},
				{Function: "net/http.HandlerFunc.ServeHTTP", File: "/usr/local/go/src/net/http/server.go", Line: 2166},
				{Function: "github.com/acme/dep.Run", File: "/home/dev/go/pkg/mod/github.com/acme/dep@v1.2.3/run.go", Line: 10},
			}},
		},
		"java": {
			"java.lang.IllegalStateException: card declined\n\tat com.acme.shop.Checkout.charge(Checkout.java:48)\n\tat org.springframework.web.servlet.FrameworkServlet.service(FrameworkServlet.java:897)",
			want{"java.lang.IllegalStateException: card declined", []Frame{
				{Function: "com.acme.shop.Checkout.charge", File: "Checkout.java", Line: 48, App: true},
				{Function: "org.springframework.web.servlet.FrameworkServlet.service", File: "FrameworkServlet.java", Line: 897},
			}},
		},
		"ruby": {
			"card declined (Checkout::Declined)\n\tfrom /srv/acme/app/controllers/checkouts_controller.rb:12:in `create'\n\tfrom /usr/local/bundle/gems/actionpack-7.1.3/lib/action_controller/metal/basic_implicit_render.rb:6:in `send_action'",
			want{"card declined (Checkout::Declined)", []Frame{
				{Function: "create", File: "/srv/acme/app/controllers/checkouts_controller.rb", Line: 12, App: true},
				{Function: "send_action", File: "/usr/local/bundle/gems/actionpack-7.1.3/lib/action_controller/metal/basic_implicit_render.rb", Line: 6},
			}},
		},
		"no frames": {"just a message\nover two lines", want{"just a message\nover two lines", nil}},
	} {
		head, frames := ParseStack(c.stack)
		if head != c.want.head {
			t.Errorf("%s: head = %q, want %q", name, head, c.want.head)
		}
		if len(frames) != len(c.want.frames) {
			t.Errorf("%s: %d frames, want %d: %+v", name, len(frames), len(c.want.frames), frames)
			continue
		}
		for i := range frames {
			if frames[i] != c.want.frames[i] {
				t.Errorf("%s: frame %d = %+v, want %+v", name, i, frames[i], c.want.frames[i])
			}
		}
	}
}

func TestTaskNames(t *testing.T) {
	for what, want := range map[string]string{
		"n1 order_items orders":                "fix-n1-order-items-orders-k3f9",
		"failing checkout":                     "fix-failing-checkout-k3f9",
		"slow users/:id/orders":                "fix-slow-users-id-orders-k3f9",
		"":                                     "fix-problem-k3f9",
		"--prompt=$(rm -rf ~) `x` ../../etc":   "fix-prompt-rm-rf-x-etc-k3f9",
		"error " + strings.Repeat("word ", 30): "fix-error-word-word-word-word-k3f9",
	} {
		got := taskName(Options{Suffix: "k3f9"}, what)
		if got != want || !TaskName.MatchString(got) || len(got) > maxTaskName {
			t.Errorf("taskName(%q) = %q (%d), want %q", what, got, len(got), want)
		}
	}
	// Without a suffix given, each is its own.
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		name := taskName(Options{}, "n1 orders")
		if !TaskName.MatchString(name) || !strings.HasPrefix(name, "fix-n1-orders-") || len(name) != len("fix-n1-orders-")+4 {
			t.Fatalf("name %q", name)
		}
		seen[name] = true
	}
	if len(seen) < 45 {
		t.Errorf("50 names, %d different", len(seen))
	}
	for shape, want := range map[string]string{
		ordersShape: "order_items", searchShape: "products",
		"INSERT INTO orders (user_id) VALUES (?)":                  "orders",
		`UPDATE "public"."inventory" SET reserved = ?`:             "public",
		"SELECT * FROM acme.receipts r JOIN users u ON u.id = r.u": "receipts",
		"BEGIN": "",
	} {
		if got := tableOf("postgresql", shape); got != want {
			t.Errorf("tableOf(%q) = %q, want %q", shape, got, want)
		}
	}
	if got := tableOf("redis", "GET products:category:?"); got != "products" {
		t.Errorf("redis key family = %q", got)
	}
	if got := tableOf("mongodb", "insert acme.receipts"); got != "receipts" {
		t.Errorf("mongo collection = %q", got)
	}
}

func TestFormats(t *testing.T) {
	for v, want := range map[float64]string{0: "0 ms", 0.42: "0.42 ms", 3: "3 ms", 4.84: "4.8 ms", 71.44: "71.4 ms", 212.4: "212 ms", 1890: "1.89 s", 50_540: "50.54 s", 125_000: "2.1 min"} {
		if got := ms(v); got != want {
			t.Errorf("ms(%v) = %q, want %q", v, got, want)
		}
	}
	for v, want := range map[float64]string{0: "0%", 0.0004: "under 0.1%", 0.073: "7.3%", 0.1: "10%", 0.826: "83%", 1: "100%"} {
		if got := pct(v); got != want {
			t.Errorf("pct(%v) = %q, want %q", v, got, want)
		}
	}
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 1902: "1,902", 1234567: "1,234,567"} {
		if got := count(n); got != want {
			t.Errorf("count(%d) = %q, want %q", n, got, want)
		}
	}
	if got := code("a `b` c"); got != "`` a `b` c ``" {
		t.Errorf("code = %q", got)
	}
	now := demoEnd
	if got := windowWords(store.Window{SinceMs: float64(now.Add(-time.Hour).UnixMilli()), UntilMs: float64(now.UnixMilli())}, now); got != "the last 60 minutes" {
		t.Errorf("window = %q", got)
	}
	if got := windowWords(store.Window{SinceMs: float64(now.Add(-26 * time.Hour).UnixMilli()), UntilMs: float64(now.Add(-2 * time.Hour).UnixMilli())}, now); !strings.HasPrefix(got, "from 2026-10-1") {
		t.Errorf("a window in the past = %q", got)
	}
}

// TestPrintDemo prints the demo's briefs, for reading them and for the
// examples in API.md: BRIEF_PRINT=1 go test ./internal/brief -run PrintDemo -v
func TestPrintDemo(t *testing.T) {
	if os.Getenv("BRIEF_PRINT") == "" {
		t.Skip("set BRIEF_PRINT=1 (or =example, or =json) to print the demo's briefs")
	}
	st := demoStore(t)
	b := New(st)
	ctx := context.Background()
	o := demoOptions()
	o.Example = os.Getenv("BRIEF_PRINT") == "example"
	show := func(br *Brief, err error) *Brief {
		if err != nil {
			t.Fatal(err)
		}
		if os.Getenv("BRIEF_PRINT") == "json" {
			raw, _ := json.MarshalIndent(br, "", "  ")
			fmt.Printf("%s\n\n", raw)
			return br
		}
		fmt.Printf("=== %s %s (%d bytes) task=%s example=%+v\n%s\nrecorded=%+v cuts=%v\n\n", br.Kind, br.Problem, br.Bytes, br.TaskName, br.Example, br.Prompt, br.Recorded, br.Cuts)
		return br
	}
	k, _ := b.FindQuery(ctx, o, "order_items")
	show(b.Query(ctx, k, o))
	k, _ = b.FindQuery(ctx, o, "ILIKE")
	show(b.Query(ctx, k, o))
	ep, _ := b.FindEndpoint(ctx, o, "POST /api/checkout", "")
	show(b.Endpoint(ctx, ep, o))
	ep, _ = b.FindEndpoint(ctx, o, "orders", "")
	show(b.Endpoint(ctx, ep, o))
	ref, err := b.FindLog(ctx, o, "checkout failed", "")
	if err != nil {
		t.Fatal(err)
	}
	lb := show(b.Log(ctx, ref, o))
	show(b.Trace(ctx, lb.Evidence.Log.TraceID, o))
}
