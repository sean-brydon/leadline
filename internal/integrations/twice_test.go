package integrations

import (
	"context"
	"encoding/hex"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/sean-brydon/leadline/internal/ingest"
	"github.com/sean-brydon/leadline/internal/integrations/fake"
	"github.com/sean-brydon/leadline/internal/store"
)

func kv(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
}

// An app seen two ways at once: it exports OpenTelemetry, and it reaches
// Postgres through the proxy `leadline run` opened for it. Both report
// every statement. Each must count once, under the app's name, in the
// app's trace, with the proxy's timing: an N+1 of 25 is 25.
func TestAnAppSeenTwoWaysCountsEachQueryOnce(t *testing.T) {
	ctx := context.Background()
	pg := fake.NewPostgres(t)
	st := openStore(t)
	m := open(t, "", st)
	p, err := m.OpenProxy("postgres", pg.Addr(), "acme-orders")
	if err != nil {
		t.Fatal(err)
	}

	// The app: one request that runs a statement for the list and then one
	// per row, through the proxy, with a span around each as a driver's
	// instrumentation makes them. The connection does not name itself.
	conn, err := pgconn.Connect(ctx, "postgres://acme:"+pg.Password+"@"+p.Listen+"/acme_dev?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	trace := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	root := []byte{0, 0, 0, 0, 0, 0, 0, 1}
	began := time.Now()
	var spans []*tracepb.Span
	statements := []string{"BEGIN", "SELECT id, customer_id FROM orders ORDER BY id LIMIT 25"}
	for i := 0; i < 25; i++ {
		statements = append(statements, "SELECT count(*) FROM orders WHERE user_id = 42")
	}
	statements = append(statements, "COMMIT")
	for i, sql := range statements {
		start := time.Now()
		if _, err := conn.Exec(ctx, sql).ReadAll(); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		spans = append(spans, &tracepb.Span{
			TraceId: trace, SpanId: []byte{0, 0, 0, 0, 0, 0, 1, byte(i)}, ParentSpanId: root, Name: "pg.query",
			Kind:              tracepb.Span_SPAN_KIND_CLIENT,
			StartTimeUnixNano: uint64(start.UnixNano()), EndTimeUnixNano: uint64(time.Now().UnixNano()),
			Attributes: []*commonpb.KeyValue{kv("db.system.name", "postgresql"), kv("db.query.text", sql), kv("db.namespace", "acme_dev")},
		})
	}
	spans = append(spans, &tracepb.Span{
		TraceId: trace, SpanId: root, Name: "GET /orders", Kind: tracepb.Span_SPAN_KIND_SERVER,
		StartTimeUnixNano: uint64(began.UnixNano()), EndTimeUnixNano: uint64(time.Now().UnixNano()),
		Attributes: []*commonpb.KeyValue{kv("http.route", "/orders")},
	})
	// The proxy's spans land first, as they do: it writes within a fifth
	// of a second and an SDK exports every few.
	eventually(t, "the proxy's statements", func() bool { return m.Proxies()[0].Statements == int64(len(statements)) })
	st.Flush()
	if err := st.AddSpans(ingest.Spans(&coltrace.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource:   &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kv("service.name", "acme-orders")}},
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}},
	}}}, false)); err != nil {
		t.Fatal(err)
	}

	rows, err := st.Queries(ctx, store.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("queries = %+v, want the two SELECTs and no BEGIN or COMMIT", rows)
	}
	for _, r := range rows {
		want := int64(1)
		if strings.Contains(r.Fingerprint, "count(*)") {
			want = 25
		}
		if r.Calls != want || r.MaxPerTrace != want || r.Traces != 1 {
			t.Errorf("%s: %d calls, at most %d in one of %d traces; want %d in one", r.Fingerprint, r.Calls, r.MaxPerTrace, r.Traces, want)
		}
	}
	since := float64(began.Add(-time.Second).UnixMilli())
	eps, err := st.Endpoints(ctx, store.Window{SinceMs: since, UntilMs: since + 60_000}, store.EndpointQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(eps.Rows) != 1 || eps.Rows[0].Service != "acme-orders" || eps.Rows[0].Requests != 1 || eps.Rows[0].Queries != 26 {
		t.Errorf("endpoints = %+v, want the one request with 26 queries", eps.Rows)
	}

	// One trace holds it all: the app's spans, the proxy's under them with
	// their row counts, and the proxy's span for the transaction.
	traces, err := st.Traces(ctx, store.Filter{})
	if err != nil || len(traces) != 1 || traces[0].TraceID != hex.EncodeToString(trace) {
		t.Fatalf("traces = %+v, %v; want only the app's", traces, err)
	}
	got, err := st.TraceSpans(ctx, traces[0].TraceID)
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]int{}
	for _, sp := range got {
		roles[sp.DBRole]++
		if sp.Service != "acme-orders" {
			t.Errorf("%s is filed under %q, want the run's service", sp.Name, sp.Service)
		}
		if sp.DBRole == "query" && !strings.Contains(string(sp.Attrs), `"db.rows"`) {
			t.Errorf("the query that counts has no row count, so it is not the proxy's: %s", sp.Attrs)
		}
	}
	// 28 statements twice, the request and the transaction.
	if len(got) != 2*len(statements)+2 || roles["query"] != 26 || roles["control"] != 2 || roles["duplicate"] != len(statements) {
		t.Errorf("%d spans with roles %v; want %d spans: 26 queries, 2 control, %d second sightings", len(got), roles, 2*len(statements)+2, len(statements))
	}

	// And one service, known to arrive both ways.
	src, err := m.Sources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(src.Services) != 1 || src.Services[0].Name != "acme-orders" ||
		!slices.Equal(src.Services[0].Via, []string{"otlp", "postgres-proxy"}) {
		t.Errorf("sources = %+v, want acme-orders by otlp and postgres-proxy", src.Services)
	}
	// Which proxy, and how busy it has been lately.
	if th := src.Services[0].Through; len(th) != 1 || th[0].Via != "postgres-proxy" || th[0].ProxyID != p.ID {
		t.Errorf("through = %+v, want the run's proxy %s", th, p.ID)
	}
	if got := src.Proxies[0].WindowStatements; got != int64(len(statements)) {
		t.Errorf("statements in the last 15 minutes = %d, want %d", got, len(statements))
	}
	if later, err := m.SourcesSince(ctx, time.Now().Add(2*time.Minute)); err != nil || later.Proxies[0].WindowStatements != 0 || later.Proxies[0].Statements == 0 {
		t.Errorf("a window that starts after the statements counts them: %+v, %v", later.Proxies, err)
	}
}

func TestTally(t *testing.T) {
	var c tally
	at := time.Unix(1_800_000_000, 0)
	c.add(at.Add(-20*time.Minute), 5)
	c.add(at.Add(-10*time.Minute), 3)
	c.add(at, 2)
	for _, w := range []struct {
		back time.Duration
		want int64
	}{{time.Minute, 2}, {15 * time.Minute, 5}, {time.Hour, 10}, {48 * time.Hour, 10}} {
		if got := c.since(at, at.Add(-w.back)); got != w.want {
			t.Errorf("last %s: %d, want %d", w.back, got, w.want)
		}
	}
	if got := c.since(at, time.Time{}); got != 5 {
		t.Errorf("with no window: %d, want the last 15 minutes' 5", got)
	}
	// A day on, the old minutes are gone, not counted again.
	if got := c.since(at.Add(25*time.Hour), at.Add(-time.Hour)); got != 0 {
		t.Errorf("a day later: %d, want 0", got)
	}
}

func TestShortVersion(t *testing.T) {
	for full, want := range map[string]string{
		"17.11 (Debian 17.11-1.pgdg13+2)": "17.11",
		"16.4":                            "16.4",
		"7.4.1":                           "7.4.1",
		"":                                "",
	} {
		if got := shortVersion(full); got != want {
			t.Errorf("shortVersion(%q) = %q, want %q", full, got, want)
		}
	}
}
