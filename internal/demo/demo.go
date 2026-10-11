// Package demo makes up telemetry for a small shop ("acme"), so the app has
// something to show before a real project is wired in. It is also the
// fixture for tests. Nothing here comes from a real system.
package demo

import (
	"fmt"
	"math/rand/v2"
	"time"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// Generator builds traces and their logs. The same seed gives the same data.
type Generator struct {
	r     *rand.Rand
	spans map[string][]*tracepb.Span
	logs  map[string][]*logspb.LogRecord
	// m is the services' metric instruments between exports; see Metrics.
	m *meters
}

func New(seed uint64) *Generator {
	return &Generator{r: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))}
}

// Batch makes n traces with start times spread evenly between from and to.
func (g *Generator) Batch(n int, from, to time.Time) (*coltrace.ExportTraceServiceRequest, *collogs.ExportLogsServiceRequest) {
	g.spans = map[string][]*tracepb.Span{}
	g.logs = map[string][]*logspb.LogRecord{}
	span := to.Sub(from)
	for i := 0; i < n; i++ {
		at := from.Add(time.Duration(float64(span) * (float64(i) + g.r.Float64()) / float64(n)))
		g.trace(at)
	}
	traces := &coltrace.ExportTraceServiceRequest{}
	for service, spans := range g.spans {
		traces.ResourceSpans = append(traces.ResourceSpans, &tracepb.ResourceSpans{
			Resource:   resource(service),
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}},
		})
	}
	logs := &collogs.ExportLogsServiceRequest{}
	for service, records := range g.logs {
		logs.ResourceLogs = append(logs.ResourceLogs, &logspb.ResourceLogs{
			Resource:  resource(service),
			ScopeLogs: []*logspb.ScopeLogs{{LogRecords: records}},
		})
	}
	return traces, logs
}

func (g *Generator) trace(at time.Time) {
	switch p := g.r.Float64(); {
	case p < 0.40:
		g.listProducts(at)
	case p < 0.62:
		g.listOrders(at)
	case p < 0.80:
		g.search(at)
	case p < 0.94:
		g.checkout(at)
	default:
		g.receiptJob(at)
	}
}

// listProducts is the healthy case: a cache miss, one indexed query.
func (g *Generator) listProducts(at time.Time) {
	t := g.newTrace()
	cur := at.Add(g.ms(1, 3))
	root := g.id(8)
	cur = g.redis(t, root, cur, "GET", "GET products:category:12")
	cur = g.pg(t, root, cur, g.ms(2, 9),
		fmt.Sprintf("SELECT id, name, price_cents FROM products WHERE category_id = %d ORDER BY name LIMIT 50", g.r.IntN(40)+1), false)
	cur = g.redis(t, root, cur, "SET", "SET products:category:12")
	g.server(t, root, "acme-api", "GET", "/api/products", 200, at, cur.Add(g.ms(1, 4)).Sub(at))
	g.log("acme-api", t, root, cur, logspb.SeverityNumber_SEVERITY_NUMBER_INFO, "listed products")
}

// listOrders is an N+1: one query for the orders, then one per order.
func (g *Generator) listOrders(at time.Time) {
	t := g.newTrace()
	root := g.id(8)
	cur := at.Add(g.ms(1, 3))
	user := g.r.IntN(9000) + 1000
	cur = g.pg(t, root, cur, g.ms(3, 12),
		fmt.Sprintf("SELECT id, status, total_cents FROM orders WHERE user_id = %d ORDER BY created_at DESC LIMIT 20", user), false)
	n := g.r.IntN(18) + 6
	for i := 0; i < n; i++ {
		cur = g.pg(t, root, cur, g.ms(1, 5),
			fmt.Sprintf("SELECT id, product_id, quantity FROM order_items WHERE order_id = %d", g.r.IntN(90000)+10000), false)
	}
	g.server(t, root, "acme-api", "GET", "/api/orders", 200, at, cur.Add(g.ms(1, 4)).Sub(at))
	g.log("acme-api", t, root, cur, logspb.SeverityNumber_SEVERITY_NUMBER_DEBUG, fmt.Sprintf("loaded %d orders for user %d", n, user))
}

// search runs an unindexed ILIKE that is slow, and now and then very slow.
func (g *Generator) search(at time.Time) {
	t := g.newTrace()
	root := g.id(8)
	cur := at.Add(g.ms(1, 3))
	slow := g.ms(120, 420)
	if g.r.Float64() < 0.12 {
		slow = g.ms(900, 2400)
	}
	term := []string{"lamp", "desk", "chair", "shelf", "rug"}[g.r.IntN(5)]
	cur = g.pg(t, root, cur, slow,
		fmt.Sprintf("SELECT id, name FROM products WHERE name ILIKE '%%%s%%' OR description ILIKE '%%%s%%' LIMIT 20", term, term), false)
	g.server(t, root, "acme-api", "GET", "/api/search", 200, at, cur.Add(g.ms(1, 4)).Sub(at))
	if slow > 800*time.Millisecond {
		g.log("acme-api", t, root, cur, logspb.SeverityNumber_SEVERITY_NUMBER_WARN,
			fmt.Sprintf("slow search for %q took %dms", term, slow.Milliseconds()))
	}
}

// checkout writes in a transaction and calls the payments service, which
// sometimes declines.
func (g *Generator) checkout(at time.Time) {
	t := g.newTrace()
	root := g.id(8)
	cur := at.Add(g.ms(1, 3))
	cur = g.pg(t, root, cur, g.ms(0, 1), "BEGIN", false)
	cur = g.pg(t, root, cur, g.ms(2, 7),
		fmt.Sprintf("INSERT INTO orders (user_id, total_cents, status) VALUES (%d, %d, 'pending') RETURNING id", g.r.IntN(9000)+1000, g.r.IntN(40000)+500), false)
	cur = g.pg(t, root, cur, g.ms(2, 8),
		fmt.Sprintf("UPDATE inventory SET reserved = reserved + %d WHERE product_id = %d", g.r.IntN(3)+1, g.r.IntN(500)+1), false)

	declined := g.r.Float64() < 0.08
	call, pay := g.id(8), g.id(8)
	payStart := cur.Add(g.ms(1, 3))
	payDur := g.ms(60, 240)
	g.add("acme-payments", &tracepb.Span{
		TraceId: t, SpanId: pay, ParentSpanId: call, Name: "POST /charge",
		Kind:              tracepb.Span_SPAN_KIND_SERVER,
		StartTimeUnixNano: ns(payStart), EndTimeUnixNano: ns(payStart.Add(payDur)),
		Attributes: attrs("http.request.method", "POST", "http.route", "/charge",
			"http.response.status_code", statusCode(declined, 402)),
		Status: status(declined, "card declined"),
	})
	g.add("acme-api", &tracepb.Span{
		TraceId: t, SpanId: call, ParentSpanId: root, Name: "POST",
		Kind:              tracepb.Span_SPAN_KIND_CLIENT,
		StartTimeUnixNano: ns(cur), EndTimeUnixNano: ns(payStart.Add(payDur).Add(g.ms(1, 3))),
		Attributes: attrs("http.request.method", "POST", "url.full", "http://payments.acme.test/charge",
			"http.response.status_code", statusCode(declined, 402)),
		Status: status(declined, ""),
	})
	cur = payStart.Add(payDur).Add(g.ms(2, 5))

	if declined {
		cur = g.pg(t, root, cur, g.ms(0, 2), "ROLLBACK", false)
		g.log("acme-payments", t, pay, payStart.Add(payDur), logspb.SeverityNumber_SEVERITY_NUMBER_ERROR, "charge failed: card declined")
		g.logWith("acme-api", t, root, cur, logspb.SeverityNumber_SEVERITY_NUMBER_ERROR, "checkout failed: payment declined",
			attrs("exception.type", "PaymentDeclinedError", "exception.message", "payment declined", "exception.stacktrace", declinedStack))
		g.server(t, root, "acme-api", "POST", "/api/checkout", 402, at, cur.Add(g.ms(1, 4)).Sub(at))
		return
	}
	cur = g.pg(t, root, cur, g.ms(1, 4), "COMMIT", false)
	g.log("acme-api", t, root, cur, logspb.SeverityNumber_SEVERITY_NUMBER_INFO, "order placed")
	g.server(t, root, "acme-api", "POST", "/api/checkout", 200, at, cur.Add(g.ms(1, 4)).Sub(at))
}

// receiptJob is a background job that writes to a document store.
func (g *Generator) receiptJob(at time.Time) {
	t := g.newTrace()
	root := g.id(8)
	cur := at.Add(g.ms(2, 6))
	end := cur.Add(g.ms(4, 30))
	g.add("acme-worker", &tracepb.Span{
		TraceId: t, SpanId: g.id(8), ParentSpanId: root, Name: "insert acme.receipts",
		Kind:              tracepb.Span_SPAN_KIND_CLIENT,
		StartTimeUnixNano: ns(cur), EndTimeUnixNano: ns(end),
		Attributes: attrs("db.system.name", "mongodb", "db.collection.name", "receipts", "db.operation.name", "insert"),
	})
	g.add("acme-worker", &tracepb.Span{
		TraceId: t, SpanId: root, Name: "job send-receipt",
		Kind:              tracepb.Span_SPAN_KIND_CONSUMER,
		StartTimeUnixNano: ns(at), EndTimeUnixNano: ns(end.Add(g.ms(20, 90))),
		Attributes: attrs("messaging.system", "acme-queue", "messaging.destination.name", "receipts"),
	})
	g.log("acme-worker", t, root, end, logspb.SeverityNumber_SEVERITY_NUMBER_INFO, "receipt sent")
}

func (g *Generator) server(trace, id []byte, service, method, route string, code int, at time.Time, dur time.Duration) {
	g.add(service, &tracepb.Span{
		TraceId: trace, SpanId: id, Name: method + " " + route,
		Kind:              tracepb.Span_SPAN_KIND_SERVER,
		StartTimeUnixNano: ns(at), EndTimeUnixNano: ns(at.Add(dur)),
		Attributes: attrs("http.request.method", method, "http.route", route,
			"http.response.status_code", int64(code)),
		Status: status(code >= 500, ""),
	})
}

// pg adds a Postgres span starting at cur and returns when it ended, plus a
// little time in the app before the next call.
func (g *Generator) pg(trace, parent []byte, cur time.Time, dur time.Duration, sql string, failed bool) time.Time {
	end := cur.Add(dur)
	g.add("acme-api", &tracepb.Span{
		TraceId: trace, SpanId: g.id(8), ParentSpanId: parent, Name: "pg.query",
		Kind:              tracepb.Span_SPAN_KIND_CLIENT,
		StartTimeUnixNano: ns(cur), EndTimeUnixNano: ns(end),
		Attributes: attrs("db.system.name", "postgresql", "db.namespace", "acme", "db.query.text", sql),
		Status:     status(failed, ""),
	})
	return end.Add(g.ms(0, 2))
}

func (g *Generator) redis(trace, parent []byte, cur time.Time, op, statement string) time.Time {
	end := cur.Add(time.Duration(g.r.IntN(900)+100) * time.Microsecond)
	g.add("acme-api", &tracepb.Span{
		TraceId: trace, SpanId: g.id(8), ParentSpanId: parent, Name: op,
		Kind:              tracepb.Span_SPAN_KIND_CLIENT,
		StartTimeUnixNano: ns(cur), EndTimeUnixNano: ns(end),
		Attributes: attrs("db.system.name", "redis", "db.query.text", statement),
	})
	return end.Add(g.ms(0, 1))
}

func (g *Generator) log(service string, trace, span []byte, at time.Time, sev logspb.SeverityNumber, msg string) {
	g.logWith(service, trace, span, at, sev, msg, nil)
}

// logWith is log with attributes, as a logger that was handed an error
// sends them.
func (g *Generator) logWith(service string, trace, span []byte, at time.Time, sev logspb.SeverityNumber, msg string, with []*commonpb.KeyValue) {
	g.logs[service] = append(g.logs[service], &logspb.LogRecord{
		TimeUnixNano: ns(at), SeverityNumber: sev,
		Body:    &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: msg}},
		TraceId: trace, SpanId: span,
		Attributes: with,
	})
}

// declinedStack is the stack trace the checkout's error log carries: two
// frames of the shop's own code above the framework's.
const declinedStack = `PaymentDeclinedError: payment declined
    at chargeCard (/srv/acme/src/payments/charge.ts:48:13)
    at async checkout (/srv/acme/src/routes/checkout.ts:31:20)
    at async Layer.handle (/srv/acme/node_modules/express/lib/router/layer.js:95:5)
    at async next (/srv/acme/node_modules/express/lib/router/route.js:144:13)
    at async node:internal/process/task_queues:95:5`

func (g *Generator) add(service string, sp *tracepb.Span) {
	g.spans[service] = append(g.spans[service], sp)
}

func (g *Generator) newTrace() []byte { return g.id(16) }

func (g *Generator) id(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(g.r.UintN(256))
	}
	return b
}

// ms picks a duration between lo and hi milliseconds.
func (g *Generator) ms(lo, hi int) time.Duration {
	return time.Duration(lo)*time.Millisecond + time.Duration(g.r.Int64N(int64(hi-lo)*int64(time.Millisecond)+1))
}

func ns(t time.Time) uint64 { return uint64(t.UnixNano()) }

func status(failed bool, msg string) *tracepb.Status {
	if failed {
		return &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: msg}
	}
	return &tracepb.Status{}
}

func statusCode(failed bool, code int64) int64 {
	if failed {
		return code
	}
	return 200
}

func resource(service string) *resourcepb.Resource {
	return &resourcepb.Resource{Attributes: attrs("service.name", service)}
}

// attrs builds attributes from alternating keys and values; a value is a
// string or an int64.
func attrs(kv ...any) []*commonpb.KeyValue {
	out := make([]*commonpb.KeyValue, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		v := &commonpb.AnyValue{}
		switch x := kv[i+1].(type) {
		case string:
			v.Value = &commonpb.AnyValue_StringValue{StringValue: x}
		case int64:
			v.Value = &commonpb.AnyValue_IntValue{IntValue: x}
		}
		out = append(out, &commonpb.KeyValue{Key: kv[i].(string), Value: v})
	}
	return out
}
