package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Breakdowns: the same spans cut by endpoint, by query and by latency band.
// Every function here takes a Window, filters by start_ns before anything
// else, and leaves all the arithmetic to DuckDB; Go only lays the buckets it
// gets back into arrays for the charts.
//
// A "request" is an entry span: a trace's root, or the server or consumer
// span where a call arrives at a service (link.go has the exceptions, and
// works it out once, as a span is written). The overview's SeriesBetween
// counts the same spans. What a request did (its queries, its failed
// spans) is whatever started in the same trace while it ran.
// That is a join on trace_id and a time range, with no walk up the parent
// chain, so it stays one pass over the window however deep the traces are.
// Everything runs on one machine, so the clocks agree.

// Window is the stretch of time, and the service, a breakdown looks at.
// An empty Service means all of them.
type Window struct {
	SinceMs, UntilMs float64
	Service          string
	// DBSystem and DBServer keep the requests that sent a database call to
	// one system, or one server (a host:port). Endpoints and
	// EndpointDetail's figures honour them; the charts of every request
	// (the heatmap, the grouped series) do not.
	DBSystem, DBServer string
}

// touched is the SQL for "this span, d, is a call to the database the
// window is narrowed to", or "" when it is not narrowed.
func (w Window) touched() string { return dbFilter("d", w.DBSystem, w.DBServer) }

// The first three parameters of every query here.
func (w Window) args(more ...any) []any {
	return append([]any{nsFromMs(w.SinceMs), nsFromMs(w.UntilMs), w.Service}, more...)
}

// bucket splits the window into n equal buckets and returns their length in
// nanoseconds and their start times in milliseconds.
func (w Window) buckets(n int) (int64, []float64) {
	if n < 1 {
		n = 1
	}
	since, until := nsFromMs(w.SinceMs), nsFromMs(w.UntilMs)
	// Rounded up, so the last bucket never spills into an n+1th.
	bucket := (until - since + int64(n) - 1) / int64(n)
	if bucket < 1 {
		bucket = 1
	}
	t := make([]float64, n)
	for i := range t {
		t[i] = float64(since+int64(i)*bucket) / 1e6
	}
	return bucket, t
}

const (
	entrySpan = isEntry
	inWindow  = `start_ns >= $1 AND start_ns < $2 AND ($3 = '' OR service = $3)`

	// Latency bands are log-spaced: band i holds durations from 10^(i/k) to
	// 10^((i+1)/k) milliseconds, k bands to the decade. Durations are clamped
	// to 1µs..1h so one broken span can't stretch a chart to nothing.
	bandSQL = `CAST(floor(log10(least(greatest(duration_ns, 1000), 3600000000000) / 1e6) * %d) AS INTEGER)`

	// Bands per decade. Ten puts edges near 1, 2 and 5, which label well;
	// the chart of every request spans more decades, so its bands are wider.
	fineBands   = 10
	coarseBands = 5
)

// requestsCTE defines r: one row per request in the window, with what ran
// inside it. cond narrows the entry spans further ("" for all of them).
//
// The join wants the requests as its hash table and the window's spans read
// past it. DuckDB picks by its guess of each side's size, and guesses from
// how many filters a side has, not from what they keep. So when cond has
// cut the requests to one endpoint's, the spans are given no filter but
// the time: one more on them and the guess tips, the hash table is built
// from every span in the window, and the query takes three times as long.
//
// touched, when not empty, keeps only the requests in which a span d
// matches it: the requests that used one database.
func requestsCTE(cond, touched string) string {
	inside := `(d.db_role BETWEEN 1 AND 2 OR d.status = 'error')`
	if cond != "" {
		inside = `true`
	}
	touch, only := `0`, ``
	if touched != "" {
		touch, only = `count(*) FILTER (WHERE `+touched+`)`, `
	WHERE coalesce(i.touched, 0) > 0`
	}
	return `
e AS (
	SELECT trace_id, span_id, service, name, start_ns, duration_ns,
		(status = 'error' OR http_status >= 500) AS own_error
	FROM spans
	WHERE ` + inWindow + ` AND ` + entrySpan + cond + `
), inside AS (
	SELECT e.trace_id, e.span_id,
		count(*) FILTER (WHERE d.db_role = 1) AS queries,
		coalesce(sum(d.duration_ns) FILTER (WHERE d.db_role BETWEEN 1 AND 2), 0)::DOUBLE AS db_ns,
		count(*) FILTER (WHERE d.status = 'error') AS errors,
		` + touch + ` AS touched
	FROM e JOIN spans d ON d.trace_id = e.trace_id
		AND d.start_ns >= e.start_ns AND d.start_ns <= e.start_ns + e.duration_ns
	WHERE d.start_ns >= $1 AND ` + inside + `
	GROUP BY e.trace_id, e.span_id
), r AS (
	SELECT e.trace_id, e.service, e.name, e.start_ns, e.duration_ns,
		coalesce(i.queries, 0) AS queries, coalesce(i.db_ns, 0) AS db_ns,
		(e.own_error OR coalesce(i.errors, 0) > 0) AS failed
	FROM e LEFT JOIN inside i ON i.trace_id = e.trace_id AND i.span_id = e.span_id` + only + `
)`
}

// EndpointRow is one endpoint: the requests that share a service and a name.
type EndpointRow struct {
	Service  string  `json:"service"`
	Name     string  `json:"name"`
	Requests int64   `json:"requests"`
	Errors   int64   `json:"errors"` // requests in which any span failed
	TotalMs  float64 `json:"total_ms"`
	AvgMs    float64 `json:"avg_ms"`
	P50Ms    float64 `json:"p50_ms"`
	P95Ms    float64 `json:"p95_ms"`
	MaxMs    float64 `json:"max_ms"`
	// Per request, on average.
	Queries float64 `json:"queries"`
	DBMs    float64 `json:"db_ms"`
	// Share is this endpoint's part of all request time in the window, 0..1.
	Share  float64 `json:"share"`
	LastMs float64 `json:"last_ms"`
	// Spark is the request count in each of Endpoints.T's buckets.
	Spark []float64 `json:"spark,omitempty"`
	// Kind and Action are what a framework's plugin says the endpoint is,
	// read off its newest request: Next.js's "next.kind" ("page", "route",
	// "action") and the server action's name. Only Endpoints sets them.
	Kind   string `json:"kind,omitempty"`
	Action string `json:"action,omitempty"`
}

type Endpoints struct {
	BucketMs float64       `json:"bucket_ms"`
	T        []float64     `json:"t"`
	Total    int64         `json:"total"` // endpoints matching, before the limit
	Rows     []EndpointRow `json:"rows"`
	// NeverHit is the routes the app has that no request in the window
	// asked for, when its framework sent a route list (see routes.go).
	NeverHit []QuietRoute `json:"never_hit"`
}

// EndpointQuery is how to list endpoints.
type EndpointQuery struct {
	Search string // substring of the name, case-insensitive
	Sort   string // a key of endpointSorts; "" is by total time
	Asc    bool
	Limit  int
	Spark  int // sparkline buckets; 0 for none
}

// endpointSorts maps the API's sort names to columns. The SQL is built from
// this table, never from the request.
var endpointSorts = map[string]string{
	"":         "total_ms",
	"time":     "total_ms",
	"name":     "name",
	"service":  "service",
	"requests": "requests",
	"errors":   "errors / requests::DOUBLE",
	"avg":      "avg_ms",
	"p50":      "p50_ms",
	"p95":      "p95_ms",
	"max":      "max_ms",
	"queries":  "queries",
	"db":       "db_ms",
}

const endpointCols = `count(*) AS requests, count(*) FILTER (WHERE failed) AS errors,
		sum(duration_ns)::DOUBLE / 1e6 AS total_ms, avg(duration_ns) / 1e6 AS avg_ms,
		quantile_cont(duration_ns, 0.5) / 1e6 AS p50_ms, quantile_cont(duration_ns, 0.95) / 1e6 AS p95_ms,
		max(duration_ns) / 1e6 AS max_ms, avg(queries) AS queries, avg(db_ns) / 1e6 AS db_ms,
		max(start_ns) / 1e6 AS last_ms`

// Endpoints groups requests by service and name. The sparklines come from
// the same statement: one more grouping of the entry spans, joined to the
// page of endpoints that is returned.
func (s *Store) Endpoints(ctx context.Context, w Window, q EndpointQuery) (*Endpoints, error) {
	order, ok := endpointSorts[q.Sort]
	if !ok {
		order = endpointSorts[""]
	}
	dir := "DESC"
	if q.Asc {
		dir = "ASC"
	}
	limit := q.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	// The sparkline counts the requests listed: every one, read straight
	// off the entry spans, or with a database filter only those kept.
	sparkOf := "e"
	if w.touched() != "" {
		sparkOf = "r"
	}
	bucket, t := w.buckets(max(q.Spark, 1))
	out := &Endpoints{BucketMs: float64(bucket) / 1e6, T: t, Rows: []EndpointRow{}, NeverHit: []QuietRoute{}}
	if q.Spark <= 0 {
		out.T = []float64{}
	}

	rows, err := s.db.QueryContext(ctx, `
WITH`+requestsCTE("", w.touched())+`, g AS (
	SELECT service, name, `+endpointCols+`, max(start_ns) AS last_ns
	FROM r GROUP BY service, name
), page AS (
	SELECT *, count(*) OVER () AS total FROM (
		SELECT *, total_ms / nullif(sum(total_ms) OVER (), 0) AS share FROM g
	)
	WHERE $4 = '' OR name ILIKE $5
	ORDER BY `+order+` `+dir+`, service, name
	LIMIT $6
), spark AS (
	SELECT service, name, list(b ORDER BY b) AS bs, list(n ORDER BY b) AS ns FROM (
		SELECT service, name, (start_ns - $1) // $7 AS b, count(*) AS n
		FROM `+sparkOf+` SEMI JOIN page USING (service, name)
		GROUP BY service, name, b
	) GROUP BY service, name
)
SELECT page.service, page.name, requests, errors, total_ms, avg_ms, p50_ms, p95_ms, max_ms,
	queries, db_ms, coalesce(share, 0), last_ms, total,
	CAST(coalesce(bs, []) AS VARCHAR), CAST(coalesce(ns, []) AS VARCHAR), last_ns
FROM page LEFT JOIN spark USING (service, name)
ORDER BY `+order+` `+dir+`, page.service, page.name`,
		w.args(q.Search, like(q.Search), limit, bucket)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lasts []int64
	for rows.Next() {
		var e EndpointRow
		var bs, ns string
		var last int64
		if err := rows.Scan(&e.Service, &e.Name, &e.Requests, &e.Errors, &e.TotalMs, &e.AvgMs,
			&e.P50Ms, &e.P95Ms, &e.MaxMs, &e.Queries, &e.DBMs, &e.Share, &e.LastMs, &out.Total,
			&bs, &ns, &last); err != nil {
			return nil, err
		}
		if q.Spark > 0 {
			e.Spark = make([]float64, q.Spark)
			place(e.Spark, intList(bs), intList(ns))
		}
		out.Rows = append(out.Rows, e)
		lasts = append(lasts, last)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if err := s.endpointKinds(ctx, out.Rows, lasts); err != nil {
		return nil, err
	}
	if out.NeverHit, err = s.NeverHit(ctx, w); err != nil {
		return nil, err
	}
	return out, nil
}

// endpointKinds fills in Kind and Action from each endpoint's newest
// request, whose start is in lasts. The two are attributes few apps send,
// so they are read from the JSON of those few rows and kept in no column.
func (s *Store) endpointKinds(ctx context.Context, eps []EndpointRow, lasts []int64) error {
	if len(eps) == 0 {
		return nil
	}
	at := map[int64][]int{}
	var list strings.Builder
	lo, hi := lasts[0], lasts[0]
	for i, ns := range lasts {
		if _, ok := at[ns]; !ok {
			if list.Len() > 0 {
				list.WriteByte(',')
			}
			fmt.Fprint(&list, ns)
		}
		at[ns] = append(at[ns], i)
		lo, hi = min(lo, ns), max(hi, ns)
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT start_ns, service, name, k, a FROM (
	SELECT start_ns, service, name,
		coalesce(json_extract_string(attrs, '$."next.kind"'), '') AS k,
		coalesce(json_extract_string(attrs, '$."next.action.name"'), '') AS a
	FROM spans
	WHERE start_ns >= ? AND start_ns <= ? AND start_ns IN (`+list.String()+`) AND `+isEntry+`
) WHERE k <> '' OR a <> ''`, lo, hi)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ns int64
		var service, name, kind, action string
		if err := rows.Scan(&ns, &service, &name, &kind, &action); err != nil {
			return err
		}
		for _, i := range at[ns] {
			if eps[i].Service == service && eps[i].Name == name {
				eps[i].Kind, eps[i].Action = kind, action
			}
		}
	}
	return rows.Err()
}

// Endpoint names one endpoint.
type Endpoint struct{ Service, Name string }

const oneEndpoint = ` AND service = $4 AND name = $5`

// EndpointSeries is one endpoint over time. Buckets with no requests have
// null latencies: there is nothing to draw there, and zero would be a lie.
type EndpointSeries struct {
	BucketMs float64    `json:"bucket_ms"`
	T        []float64  `json:"t"`
	Requests []float64  `json:"requests"`
	Errors   []float64  `json:"errors"`
	P50Ms    []*float64 `json:"p50_ms"`
	P95Ms    []*float64 `json:"p95_ms"`
	AvgMs    []*float64 `json:"avg_ms"`
	// Per request, on average.
	Queries []*float64 `json:"queries"`
	DBMs    []*float64 `json:"db_ms"`
}

// EndpointQueryRow is one query shape as one endpoint runs it.
type EndpointQueryRow struct {
	DBSystem    string `json:"db_system"`
	Fingerprint string `json:"fingerprint"`
	Example     string `json:"example"`
	Calls       int64  `json:"calls"`
	// Requests is how many of the endpoint's requests ran it at all.
	Requests int64 `json:"requests"`
	// PerRequest is calls over all of the endpoint's requests: well above 1
	// is an N+1.
	PerRequest    float64 `json:"per_request"`
	MaxPerRequest int64   `json:"max_per_request"`
	AvgMs         float64 `json:"avg_ms"`
	MaxMs         float64 `json:"max_ms"`
	TotalMs       float64 `json:"total_ms"`
}

// RequestRow is one request, for the lists of slowest and failing ones.
type RequestRow struct {
	TraceID    string  `json:"trace_id"`
	StartMs    float64 `json:"start_ms"`
	DurationMs float64 `json:"duration_ms"`
	Queries    int64   `json:"queries"`
	DBMs       float64 `json:"db_ms"`
	Failed     bool    `json:"failed"`
}

type EndpointDetail struct {
	// Stats is nil when the endpoint has no requests in the window.
	Stats     *EndpointRow       `json:"stats"`
	Series    *EndpointSeries    `json:"series"`
	Histogram *Histogram         `json:"histogram"`
	Heatmap   *Heatmap           `json:"heatmap"`
	Queries   []EndpointQueryRow `json:"queries"`
	Slowest   []RequestRow       `json:"slowest"`
	Failing   []RequestRow       `json:"failing"`
}

// EndpointDetail is everything the app shows about one endpoint, over n
// time buckets.
func (s *Store) EndpointDetail(ctx context.Context, w Window, ep Endpoint, n int) (*EndpointDetail, error) {
	out := &EndpointDetail{Queries: []EndpointQueryRow{}, Slowest: []RequestRow{}, Failing: []RequestRow{}}
	var err error
	if out.Stats, err = s.endpointStats(ctx, w, ep); err != nil {
		return nil, err
	}
	if out.Series, err = s.endpointSeries(ctx, w, ep, n); err != nil {
		return nil, err
	}
	if out.Histogram, err = s.histogram(ctx, entrySpan+oneEndpoint, w.args(ep.Service, ep.Name)); err != nil {
		return nil, err
	}
	if out.Heatmap, err = s.heatmap(ctx, w, n, fineBands, entrySpan+oneEndpoint, ep.Service, ep.Name); err != nil {
		return nil, err
	}
	if out.Queries, err = s.endpointQueries(ctx, w, ep); err != nil {
		return nil, err
	}
	if out.Slowest, out.Failing, err = s.endpointRequests(ctx, w, ep, 12); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) endpointStats(ctx context.Context, w Window, ep Endpoint) (*EndpointRow, error) {
	e := &EndpointRow{Service: ep.Service, Name: ep.Name}
	// The share is of every request in the window, not just this endpoint's.
	err := s.db.QueryRowContext(ctx, `
WITH`+requestsCTE(oneEndpoint, w.touched())+`
SELECT `+endpointCols+`,
	coalesce(sum(duration_ns)::DOUBLE / nullif((
		SELECT sum(duration_ns)::DOUBLE FROM spans WHERE `+inWindow+` AND `+entrySpan+`), 0), 0)
FROM r HAVING count(*) > 0`, w.args(ep.Service, ep.Name)...).Scan(
		&e.Requests, &e.Errors, &e.TotalMs, &e.AvgMs, &e.P50Ms, &e.P95Ms, &e.MaxMs,
		&e.Queries, &e.DBMs, &e.LastMs, &e.Share)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return e, nil
}

func (s *Store) endpointSeries(ctx context.Context, w Window, ep Endpoint, n int) (*EndpointSeries, error) {
	bucket, t := w.buckets(n)
	n = len(t)
	out := &EndpointSeries{
		BucketMs: float64(bucket) / 1e6, T: t,
		Requests: make([]float64, n), Errors: make([]float64, n),
		P50Ms: make([]*float64, n), P95Ms: make([]*float64, n), AvgMs: make([]*float64, n),
		Queries: make([]*float64, n), DBMs: make([]*float64, n),
	}
	rows, err := s.db.QueryContext(ctx, `
WITH`+requestsCTE(oneEndpoint, w.touched())+`
SELECT (start_ns - $1) // $6 AS b, count(*), count(*) FILTER (WHERE failed),
	quantile_cont(duration_ns, 0.5) / 1e6, quantile_cont(duration_ns, 0.95) / 1e6,
	avg(duration_ns) / 1e6, avg(queries), avg(db_ns) / 1e6
FROM r GROUP BY b`, w.args(ep.Service, ep.Name, bucket)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var b, req, errs int64
		var p50, p95, avg, queries, db float64
		if err := rows.Scan(&b, &req, &errs, &p50, &p95, &avg, &queries, &db); err != nil {
			return nil, err
		}
		if b < 0 || b >= int64(n) {
			continue
		}
		out.Requests[b], out.Errors[b] = float64(req), float64(errs)
		out.P50Ms[b], out.P95Ms[b], out.AvgMs[b] = &p50, &p95, &avg
		out.Queries[b], out.DBMs[b] = &queries, &db
	}
	return out, rows.Err()
}

// endpointQueries groups the database calls made inside one endpoint's
// requests by shape, costliest first.
func (s *Store) endpointQueries(ctx context.Context, w Window, ep Endpoint) ([]EndpointQueryRow, error) {
	rows, err := s.db.QueryContext(ctx, `
WITH e AS (
	SELECT trace_id, span_id, start_ns, duration_ns FROM spans
	WHERE `+inWindow+` AND `+entrySpan+oneEndpoint+`
), per_request AS (
	SELECT d.db_system, d.db_fingerprint, e.span_id, any_value(d.db_statement) AS example,
		count(*) AS n, sum(d.duration_ns)::DOUBLE AS ns, max(d.duration_ns) AS max_ns
	FROM e JOIN spans d ON d.trace_id = e.trace_id
		AND d.start_ns >= e.start_ns AND d.start_ns <= e.start_ns + e.duration_ns
	WHERE d.start_ns >= $1 AND d.db_role = 1
	GROUP BY d.db_system, d.db_fingerprint, e.trace_id, e.span_id
)
SELECT db_system, db_fingerprint, any_value(example), sum(n)::BIGINT, count(*),
	sum(n) / (SELECT count(*) FROM e)::DOUBLE, max(n),
	sum(ns) / sum(n) / 1e6, max(max_ns) / 1e6, sum(ns) / 1e6
FROM per_request
GROUP BY db_system, db_fingerprint
ORDER BY sum(ns) DESC, db_fingerprint
LIMIT 50`, w.args(ep.Service, ep.Name)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EndpointQueryRow{}
	for rows.Next() {
		var q EndpointQueryRow
		if err := rows.Scan(&q.DBSystem, &q.Fingerprint, &q.Example, &q.Calls, &q.Requests,
			&q.PerRequest, &q.MaxPerRequest, &q.AvgMs, &q.MaxMs, &q.TotalMs); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// endpointRequests returns an endpoint's slowest requests and its most
// recent failing ones, up to limit of each.
func (s *Store) endpointRequests(ctx context.Context, w Window, ep Endpoint, limit int) (slowest, failing []RequestRow, err error) {
	slowest, failing = []RequestRow{}, []RequestRow{}
	const cols = `trace_id, start_ns / 1e6 AS start_ms, duration_ns / 1e6 AS duration_ms, queries, db_ns / 1e6 AS db_ms, failed`
	rows, err := s.db.QueryContext(ctx, `
WITH`+requestsCTE(oneEndpoint, w.touched())+`
SELECT * FROM (
	(SELECT 0 AS list, `+cols+` FROM r ORDER BY duration_ns DESC, trace_id LIMIT $6)
	UNION ALL
	(SELECT 1 AS list, `+cols+` FROM r WHERE failed ORDER BY start_ns DESC, trace_id LIMIT $6)
) ORDER BY list, CASE WHEN list = 0 THEN -duration_ms ELSE -start_ms END`,
		w.args(ep.Service, ep.Name, limit)...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var list int
		var r RequestRow
		if err := rows.Scan(&list, &r.TraceID, &r.StartMs, &r.DurationMs, &r.Queries, &r.DBMs, &r.Failed); err != nil {
			return nil, nil, err
		}
		if list == 0 {
			slowest = append(slowest, r)
		} else {
			failing = append(failing, r)
		}
	}
	return slowest, failing, rows.Err()
}

// Bin is one bar of a histogram: durations from LoMs up to HiMs.
type Bin struct {
	LoMs  float64 `json:"lo_ms"`
	HiMs  float64 `json:"hi_ms"`
	Count int64   `json:"count"`
}

// Histogram is a latency distribution in log-spaced bins, with no gaps
// between the first and the last bin that holds anything.
type Histogram struct {
	PerDecade int   `json:"per_decade"`
	Bins      []Bin `json:"bins"`
}

// histogram bins the durations of the spans matching cond, which may use
// parameters from $4 on.
func (s *Store) histogram(ctx context.Context, cond string, args []any) (*Histogram, error) {
	const k = fineBands
	out := &Histogram{PerDecade: k, Bins: []Bin{}}
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
WITH h AS (
	SELECT `+bandSQL+` AS i, count(*) AS n
	FROM spans WHERE `+inWindow+` AND `+cond+`
	GROUP BY i
), span AS (
	SELECT unnest(range(min(i), max(i) + 1)) AS i FROM h
)
SELECT pow(10, span.i / %d::DOUBLE), pow(10, (span.i + 1) / %d::DOUBLE), coalesce(h.n, 0)
FROM span LEFT JOIN h USING (i) ORDER BY span.i`, k, k, k), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var b Bin
		if err := rows.Scan(&b.LoMs, &b.HiMs, &b.Count); err != nil {
			return nil, err
		}
		out.Bins = append(out.Bins, b)
	}
	return out, rows.Err()
}

// Heatmap is latency over time: a grid with time across and log-spaced
// latency bands up, holding how many durations fell in each cell. Only the
// cells that hold something are sent: X[i], Y[i] and N[i] describe one cell.
// Band y covers 10^(y/PerDecade) to 10^((y+1)/PerDecade) milliseconds.
type Heatmap struct {
	BucketMs  float64   `json:"bucket_ms"`
	T         []float64 `json:"t"`
	PerDecade int       `json:"per_decade"`
	X         []int32   `json:"x"`
	Y         []int32   `json:"y"`
	N         []int64   `json:"n"`
	// The lowest and highest band in use, and the fullest cell's count.
	YMin int32 `json:"y_min"`
	YMax int32 `json:"y_max"`
	Max  int64 `json:"max"`
}

// RequestHeatmap is the latency of every request in the window over n
// columns of time.
func (s *Store) RequestHeatmap(ctx context.Context, w Window, n int) (*Heatmap, error) {
	return s.heatmap(ctx, w, n, coarseBands, entrySpan)
}

// heatmap counts the spans matching cond by time bucket and latency band,
// k bands to the decade. cond may use parameters from $4 on, given in more.
func (s *Store) heatmap(ctx context.Context, w Window, n, k int, cond string, more ...any) (*Heatmap, error) {
	bucket, t := w.buckets(n)
	out := &Heatmap{BucketMs: float64(bucket) / 1e6, T: t, PerDecade: k, X: []int32{}, Y: []int32{}, N: []int64{}}
	args := w.args(more...)
	args = append(args, bucket)
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
SELECT x, y, n, min(y) OVER (), max(y) OVER (), max(n) OVER () FROM (
	SELECT CAST((start_ns - $1) // $%d AS INTEGER) AS x, `+bandSQL+` AS y, count(*) AS n
	FROM spans WHERE `+inWindow+` AND `+cond+`
	GROUP BY x, y
) ORDER BY x, y`, len(args), k), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var x, y int32
		var c int64
		if err := rows.Scan(&x, &y, &c, &out.YMin, &out.YMax, &out.Max); err != nil {
			return nil, err
		}
		if x < 0 || int(x) >= len(t) {
			continue
		}
		out.X, out.Y, out.N = append(out.X, x), append(out.Y, y), append(out.N, c)
	}
	return out, rows.Err()
}

// QueryKey names one query shape.
type QueryKey struct{ DBSystem, Fingerprint string }

// A statement's second sightings are left out wherever it is looked up.
const oneQuery = `db_system = $4 AND db_fingerprint = $5 AND ` + isDBTime

type QueryStats struct {
	DBSystem    string `json:"db_system"`
	Fingerprint string `json:"fingerprint"`
	// Example is the statement of the slowest call: the one worth reading.
	Example     string  `json:"example"`
	Calls       int64   `json:"calls"`
	Errors      int64   `json:"errors"`
	TotalMs     float64 `json:"total_ms"`
	AvgMs       float64 `json:"avg_ms"`
	P50Ms       float64 `json:"p50_ms"`
	P95Ms       float64 `json:"p95_ms"`
	MaxMs       float64 `json:"max_ms"`
	Traces      int64   `json:"traces"`
	MaxPerTrace int64   `json:"max_per_trace"`
	LastMs      float64 `json:"last_ms"`
	// Share is this query's part of all database time in the window, 0..1.
	Share float64 `json:"share"`
}

// QuerySeries is one query shape over time. Buckets with no calls have null
// latencies.
type QuerySeries struct {
	BucketMs float64    `json:"bucket_ms"`
	T        []float64  `json:"t"`
	Calls    []float64  `json:"calls"`
	Errors   []float64  `json:"errors"`
	TotalMs  []float64  `json:"total_ms"`
	AvgMs    []*float64 `json:"avg_ms"`
	P95Ms    []*float64 `json:"p95_ms"`
}

// QueryEndpointRow is one endpoint that runs a query.
type QueryEndpointRow struct {
	Service string `json:"service"`
	Name    string `json:"name"`
	Calls   int64  `json:"calls"`
	// Requests ran the query; EndpointRequests is all the endpoint had.
	Requests         int64   `json:"requests"`
	EndpointRequests int64   `json:"endpoint_requests"`
	PerRequest       float64 `json:"per_request"` // calls over EndpointRequests
	MaxPerRequest    int64   `json:"max_per_request"`
	AvgMs            float64 `json:"avg_ms"`
	TotalMs          float64 `json:"total_ms"`
}

// QueryTraceRow is one trace that ran a query.
type QueryTraceRow struct {
	TraceID string `json:"trace_id"`
	// Name and Service are the request's, empty when it began before the
	// window.
	Name      string  `json:"name"`
	Service   string  `json:"service"`
	StartMs   float64 `json:"start_ms"` // the first call
	Calls     int64   `json:"calls"`
	Errors    int64   `json:"errors"`
	TotalMs   float64 `json:"total_ms"`
	SlowestMs float64 `json:"slowest_ms"`
}

type QueryDetail struct {
	// Stats is nil when the query wasn't run in the window.
	Stats     *QueryStats        `json:"stats"`
	Series    *QuerySeries       `json:"series"`
	Histogram *Histogram         `json:"histogram"`
	Heatmap   *Heatmap           `json:"heatmap"`
	Endpoints []QueryEndpointRow `json:"endpoints"`
	Traces    []QueryTraceRow    `json:"traces"`
}

// QueryDetail is everything the app shows about one query shape, over n
// time buckets.
func (s *Store) QueryDetail(ctx context.Context, w Window, key QueryKey, n int) (*QueryDetail, error) {
	out := &QueryDetail{Endpoints: []QueryEndpointRow{}, Traces: []QueryTraceRow{}}
	var err error
	if out.Stats, err = s.queryStats(ctx, w, key); err != nil {
		return nil, err
	}
	if out.Series, err = s.querySeries(ctx, w, key, n); err != nil {
		return nil, err
	}
	if out.Histogram, err = s.histogram(ctx, oneQuery, w.args(key.DBSystem, key.Fingerprint)); err != nil {
		return nil, err
	}
	if out.Heatmap, err = s.heatmap(ctx, w, n, fineBands, oneQuery, key.DBSystem, key.Fingerprint); err != nil {
		return nil, err
	}
	if out.Endpoints, err = s.queryEndpoints(ctx, w, key); err != nil {
		return nil, err
	}
	if out.Traces, err = s.queryTraces(ctx, w, key, 12); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) queryStats(ctx context.Context, w Window, key QueryKey) (*QueryStats, error) {
	q := &QueryStats{DBSystem: key.DBSystem, Fingerprint: key.Fingerprint}
	err := s.db.QueryRowContext(ctx, `
WITH q AS (
	SELECT trace_id, db_statement, start_ns, duration_ns, status FROM spans
	WHERE `+inWindow+` AND `+oneQuery+`
)
SELECT arg_max(db_statement, duration_ns), count(*), count(*) FILTER (WHERE status = 'error'),
	sum(duration_ns)::DOUBLE / 1e6, avg(duration_ns) / 1e6,
	quantile_cont(duration_ns, 0.5) / 1e6, quantile_cont(duration_ns, 0.95) / 1e6,
	max(duration_ns) / 1e6, count(DISTINCT trace_id),
	(SELECT max(n) FROM (SELECT count(*) AS n FROM q GROUP BY trace_id)),
	max(start_ns) / 1e6,
	coalesce(sum(duration_ns)::DOUBLE / nullif((
		SELECT sum(duration_ns)::DOUBLE FROM spans WHERE `+inWindow+` AND `+isDBTime+`), 0), 0)
FROM q HAVING count(*) > 0`, w.args(key.DBSystem, key.Fingerprint)...).Scan(
		&q.Example, &q.Calls, &q.Errors, &q.TotalMs, &q.AvgMs, &q.P50Ms, &q.P95Ms, &q.MaxMs,
		&q.Traces, &q.MaxPerTrace, &q.LastMs, &q.Share)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return q, nil
}

func (s *Store) querySeries(ctx context.Context, w Window, key QueryKey, n int) (*QuerySeries, error) {
	bucket, t := w.buckets(n)
	n = len(t)
	out := &QuerySeries{
		BucketMs: float64(bucket) / 1e6, T: t,
		Calls: make([]float64, n), Errors: make([]float64, n), TotalMs: make([]float64, n),
		AvgMs: make([]*float64, n), P95Ms: make([]*float64, n),
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT (start_ns - $1) // $6 AS b, count(*), count(*) FILTER (WHERE status = 'error'),
	sum(duration_ns)::DOUBLE / 1e6, avg(duration_ns) / 1e6, quantile_cont(duration_ns, 0.95) / 1e6
FROM spans WHERE `+inWindow+` AND `+oneQuery+`
GROUP BY b`, w.args(key.DBSystem, key.Fingerprint, bucket)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var b, calls, errs int64
		var total, avg, p95 float64
		if err := rows.Scan(&b, &calls, &errs, &total, &avg, &p95); err != nil {
			return nil, err
		}
		if b < 0 || b >= int64(n) {
			continue
		}
		out.Calls[b], out.Errors[b], out.TotalMs[b] = float64(calls), float64(errs), total
		out.AvgMs[b], out.P95Ms[b] = &avg, &p95
	}
	return out, rows.Err()
}

// queryEndpoints lists the endpoints whose requests ran a query. The
// service filter applies to the query's own spans; the request that caused
// them may belong to any service.
func (s *Store) queryEndpoints(ctx context.Context, w Window, key QueryKey) ([]QueryEndpointRow, error) {
	rows, err := s.db.QueryContext(ctx, `
WITH q AS (
	SELECT trace_id, start_ns, duration_ns FROM spans
	WHERE `+inWindow+` AND `+oneQuery+`
), e AS (
	SELECT trace_id, span_id, service, name, start_ns, duration_ns FROM spans
	WHERE start_ns >= $1 AND start_ns < $2 AND `+entrySpan+`
), per_request AS (
	SELECT e.service, e.name, count(*) AS n, sum(q.duration_ns)::DOUBLE AS ns
	FROM q JOIN e ON e.trace_id = q.trace_id
		AND q.start_ns >= e.start_ns AND q.start_ns <= e.start_ns + e.duration_ns
	GROUP BY e.service, e.name, e.trace_id, e.span_id
), ran AS (
	SELECT service, name, sum(n)::BIGINT AS calls, count(*) AS requests, max(n) AS max_n, sum(ns) AS ns
	FROM per_request GROUP BY service, name
), every AS (
	SELECT service, name, count(*) AS requests FROM e SEMI JOIN ran USING (service, name)
	GROUP BY service, name
)
SELECT ran.service, ran.name, calls, ran.requests, every.requests,
	calls / every.requests::DOUBLE, max_n, ns / calls / 1e6, ns / 1e6
FROM ran JOIN every USING (service, name)
ORDER BY ns DESC, ran.service, ran.name
LIMIT 30`, w.args(key.DBSystem, key.Fingerprint)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []QueryEndpointRow{}
	for rows.Next() {
		var e QueryEndpointRow
		if err := rows.Scan(&e.Service, &e.Name, &e.Calls, &e.Requests, &e.EndpointRequests,
			&e.PerRequest, &e.MaxPerRequest, &e.AvgMs, &e.TotalMs); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// queryTraces lists the traces that spent the longest in a query.
func (s *Store) queryTraces(ctx context.Context, w Window, key QueryKey, limit int) ([]QueryTraceRow, error) {
	rows, err := s.db.QueryContext(ctx, `
WITH top AS (
	SELECT trace_id, min(start_ns) AS first_ns, count(*) AS calls,
		count(*) FILTER (WHERE status = 'error') AS errors,
		sum(duration_ns)::DOUBLE AS ns, max(duration_ns) AS max_ns
	FROM spans WHERE `+inWindow+` AND `+oneQuery+`
	GROUP BY trace_id
	ORDER BY ns DESC, trace_id
	LIMIT $6
), named AS (
	-- The outermost request of each of those traces.
	SELECT trace_id, arg_min(name, start_ns) AS name, arg_min(service, start_ns) AS service
	FROM spans SEMI JOIN top USING (trace_id)
	WHERE start_ns >= $1 AND start_ns < $2 AND `+entrySpan+`
	GROUP BY trace_id
)
SELECT top.trace_id, coalesce(named.name, ''), coalesce(named.service, ''),
	first_ns / 1e6, calls, errors, ns / 1e6, max_ns / 1e6
FROM top LEFT JOIN named USING (trace_id)
ORDER BY ns DESC, top.trace_id`, w.args(key.DBSystem, key.Fingerprint, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []QueryTraceRow{}
	for rows.Next() {
		var t QueryTraceRow
		if err := rows.Scan(&t.TraceID, &t.Name, &t.Service, &t.StartMs, &t.Calls, &t.Errors,
			&t.TotalMs, &t.SlowestMs); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Group is one line of a GroupedSeries.
type Group struct {
	Key    string    `json:"key"`
	Total  float64   `json:"total"`
	Values []float64 `json:"values"`
}

// GroupedSeries is one measure over time, split by a dimension: a line per
// group, largest first.
type GroupedSeries struct {
	BucketMs float64   `json:"bucket_ms"`
	T        []float64 `json:"t"`
	Groups   []Group   `json:"groups"`
}

// groupings are the splits GroupedSeries knows: what to group by, what to
// add up, and which spans count.
var groupings = map[string]struct{ key, value, cond string }{
	// Time spent in each database system, in milliseconds.
	"db_time": {"db_system", "sum(duration_ns)::DOUBLE / 1e6", isDBTime},
	// Queries sent to each database system.
	"db_calls": {"db_system", "count(*)::DOUBLE", isQuery},
	// Requests by the class of their HTTP status; requests that aren't HTTP
	// (jobs, consumers) are "other".
	"status": {
		`CASE WHEN http_status BETWEEN 100 AND 599 THEN (http_status // 100)::VARCHAR || 'xx' ELSE 'other' END`,
		"count(*)::DOUBLE", entrySpan,
	},
	// Requests arriving at each service.
	"service": {"service", "count(*)::DOUBLE", entrySpan},
	// Time spent in each service's requests, in milliseconds.
	"service_time": {"service", "sum(duration_ns)::DOUBLE / 1e6", entrySpan},
}

// KnownGrouping says whether GroupedSeries can split by this.
func KnownGrouping(by string) bool {
	_, ok := groupings[by]
	return ok
}

// Groupings lists what GroupedSeries can split by, for an error message.
func Groupings() string {
	keys := make([]string, 0, len(groupings))
	for k := range groupings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// GroupedSeries splits one measure by one dimension over n buckets. by is a
// key of groupings; at most limit groups come back, the largest.
func (s *Store) GroupedSeries(ctx context.Context, w Window, by string, n, limit int) (*GroupedSeries, error) {
	g, ok := groupings[by]
	if !ok {
		return nil, fmt.Errorf("unknown grouping %q (have %s)", by, Groupings())
	}
	if limit <= 0 || limit > 20 {
		limit = 6
	}
	bucket, t := w.buckets(n)
	out := &GroupedSeries{BucketMs: float64(bucket) / 1e6, T: t, Groups: []Group{}}
	rows, err := s.db.QueryContext(ctx, `
WITH cells AS (
	SELECT `+g.key+` AS key, (start_ns - $1) // $4 AS b, `+g.value+` AS v
	FROM spans WHERE `+inWindow+` AND `+g.cond+`
	GROUP BY key, b
), top AS (
	SELECT key, sum(v) AS total FROM cells GROUP BY key ORDER BY total DESC, key LIMIT $5
)
SELECT key, total, b, v FROM cells JOIN top USING (key)
ORDER BY total DESC, key, b`, w.args(bucket, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var total, v float64
		var b int64
		if err := rows.Scan(&key, &total, &b, &v); err != nil {
			return nil, err
		}
		if last := len(out.Groups) - 1; last < 0 || out.Groups[last].Key != key {
			out.Groups = append(out.Groups, Group{Key: key, Total: total, Values: make([]float64, len(t))})
		}
		if b >= 0 && b < int64(len(t)) {
			out.Groups[len(out.Groups)-1].Values[b] = v
		}
	}
	return out, rows.Err()
}

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// intList reads a DuckDB list of integers that was cast to VARCHAR, which
// prints as "[1, 2, 3]": a list column that needs no driver-specific type.
func intList(s string) []int64 {
	var out []int64
	if json.Unmarshal([]byte(s), &out) != nil {
		return nil
	}
	return out
}

// place puts each value in the bucket it belongs to.
func place(dst []float64, buckets, values []int64) {
	for i, b := range buckets {
		if i < len(values) && b >= 0 && b < int64(len(dst)) {
			dst[b] = float64(values[i])
		}
	}
}
