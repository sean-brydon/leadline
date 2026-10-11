package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Filter narrows a query. Zero values mean "no limit on this".
type Filter struct {
	SinceMs     float64 // only rows at or after this time
	Service     string
	Search      string // substring, case-insensitive
	MinSeverity int32  // logs only: OTLP severity number
	// Traces only.
	ErrorsOnly    bool
	MinDurationMs float64
	Limit         int
	// User keeps the traces that have a span for this user ("user.id"), and
	// the logs written for them.
	User string
	// DBSystem and DBServer narrow to one database: the queries sent to
	// it, the traces that sent it one, and the logs written during those
	// traces. DBServer is a host:port, however spelled (see DBServer).
	DBSystem, DBServer string
	// Kind keeps the logs of one "leadline.kind" (logs only).
	Kind string
	// Control lists transaction and session control (BEGIN, COMMIT, SET)
	// in place of the queries (Queries only).
	Control bool
	// Before pages Traces and Logs: it is the Cursor of the last row of
	// the page before, and the rows returned are the ones that follow it.
	Before string
}

func (f Filter) limit(def int) int {
	if f.Limit <= 0 || f.Limit > 5000 {
		return def
	}
	return f.Limit
}

func nsFromMs(ms float64) int64 { return int64(ms * 1e6) }

// ErrBadCursor is returned for a Filter.Before that no row ever carried.
var ErrBadCursor = errors.New("not a cursor from an earlier page")

// A cursor is a row's exact place in its list: its time in nanoseconds,
// which a millisecond float can't hold, a dot, and what settles ties among
// rows of that time. cutCursor splits one.
func cutCursor(cursor string) (ns int64, tie string, err error) {
	at, tie, ok := strings.Cut(cursor, ".")
	if ns, err = strconv.ParseInt(at, 10, 64); err != nil || !ok || tie == "" {
		return 0, "", ErrBadCursor
	}
	return ns, tie, nil
}

type TraceRow struct {
	TraceID    string  `json:"trace_id"`
	Name       string  `json:"name"`
	Service    string  `json:"service"`
	StartMs    float64 `json:"start_ms"`
	DurationMs float64 `json:"duration_ms"`
	Spans      int64   `json:"spans"`
	Errors     int64   `json:"errors"`
	Queries    int64   `json:"queries"`
	DBMs       float64 `json:"db_ms"`
	// UserID is who the trace ran for, when the app says ("user.id").
	UserID string `json:"user_id,omitempty"`
	// Cursor is this row's place in the list, for Filter.Before.
	Cursor string `json:"cursor,omitempty"`
}

// A trace is named after its root span; until the root arrives (it ends
// last, so it is exported last) the earliest span stands in.
const (
	traceName    = `coalesce(arg_min(name, start_ns) FILTER (WHERE parent_id = ''), arg_min(name, start_ns))`
	traceService = `coalesce(arg_min(service, start_ns) FILTER (WHERE parent_id = ''), arg_min(service, start_ns))`
)

// Traces lists traces, newest first, and among traces that start in the
// same nanosecond by id, so a page always ends at an exact place.
//
// It asks twice. Working out every column of every trace in the window, to
// keep a few hundred, was most of the cost on a long window: grouping a day
// of spans by trace is cheap, the per-trace names and sums are not. So the
// first query finds which traces are on the page, computing only what the
// filter needs, and the second works out the columns for those alone, over
// the stretch of time they cover.
func (s *Store) Traces(ctx context.Context, f Filter) ([]TraceRow, error) {
	since := nsFromMs(f.SinceMs)
	var having []string
	args := []any{since}
	if f.Before != "" {
		at, id, err := cutCursor(f.Before)
		if err != nil {
			return nil, err
		}
		having = append(having, `(s < ? OR (s = ? AND trace_id < ?))`)
		args = append(args, at, at, id)
	}
	if f.Service != "" {
		having = append(having, traceService+` = ?`)
		args = append(args, f.Service)
	}
	if f.Search != "" {
		having = append(having, traceName+` ILIKE ?`)
		args = append(args, like(f.Search))
	}
	if f.ErrorsOnly {
		having = append(having, `count(*) FILTER (WHERE status = 'error') > 0`)
	}
	if f.User != "" {
		having = append(having, `count(*) FILTER (WHERE user_id = ?) > 0`)
		args = append(args, f.User)
	}
	if db := dbFilter("", f.DBSystem, f.DBServer); db != "" {
		having = append(having, `count(*) FILTER (WHERE `+db+`) > 0`)
	}
	if f.MinDurationMs > 0 {
		having = append(having, `(max(start_ns + duration_ns) - min(start_ns)) / 1e6 >= ?`)
		args = append(args, f.MinDurationMs)
	}
	q := `SELECT trace_id, min(start_ns) AS s, max(start_ns) AS l FROM spans WHERE start_ns >= ? GROUP BY trace_id`
	if len(having) > 0 {
		q += ` HAVING ` + strings.Join(having, ` AND `)
	}
	q += ` ORDER BY s DESC, trace_id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, append(args, f.limit(200))...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids strings.Builder
	var n int
	var first, last int64
	for rows.Next() {
		var id string
		var start, end int64
		if err := rows.Scan(&id, &start, &end); err != nil {
			return nil, err
		}
		if n == 0 || start < first {
			first = start
		}
		if n == 0 || end > last {
			last = end
		}
		if n++; n > 1 {
			ids.WriteByte(',')
		}
		ids.WriteString(quote(id))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []TraceRow{}
	if n == 0 {
		return out, nil
	}

	// first is at or after the window's start, so this counts the same
	// spans the first query did, and the time bounds let DuckDB skip every
	// block of rows outside the page.
	detail, err := s.db.QueryContext(ctx, `
SELECT trace_id, `+traceName+`, `+traceService+`,
	min(start_ns) AS s,
	(max(start_ns + duration_ns) - min(start_ns)) / 1e6,
	count(*),
	count(*) FILTER (WHERE `+isFailed+`),
	count(*) FILTER (WHERE `+isQuery+`),
	coalesce(sum(duration_ns) FILTER (WHERE `+isDBTime+`), 0) / 1e6,
	coalesce(max(user_id), '')
FROM spans
WHERE start_ns >= ? AND start_ns <= ? AND trace_id IN (`+ids.String()+`)
GROUP BY trace_id
ORDER BY s DESC, trace_id DESC`, first, last)
	if err != nil {
		return nil, err
	}
	defer detail.Close()
	for detail.Next() {
		var t TraceRow
		var start int64
		if err := detail.Scan(&t.TraceID, &t.Name, &t.Service, &start, &t.DurationMs,
			&t.Spans, &t.Errors, &t.Queries, &t.DBMs, &t.UserID); err != nil {
			return nil, err
		}
		t.StartMs = float64(start) / 1e6
		t.Cursor = strconv.FormatInt(start, 10) + "." + t.TraceID
		out = append(out, t)
	}
	return out, detail.Err()
}

// quote writes a string as a SQL literal.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

type SpanRow struct {
	SpanID        string          `json:"span_id"`
	ParentID      string          `json:"parent_id"`
	Service       string          `json:"service"`
	Name          string          `json:"name"`
	Kind          string          `json:"kind"`
	StartMs       float64         `json:"start_ms"`
	DurationMs    float64         `json:"duration_ms"`
	Status        string          `json:"status"`
	StatusMessage string          `json:"status_message"`
	DBSystem      string          `json:"db_system"`
	DBStatement   string          `json:"db_statement"`
	HTTPMethod    string          `json:"http_method"`
	HTTPRoute     string          `json:"http_route"`
	HTTPStatus    int32           `json:"http_status"`
	Attrs         json.RawMessage `json:"attrs"`
	// DBRole is what a database call counts as: "query"; "control" for
	// transaction and session bookkeeping (BEGIN, COMMIT, SET), which is
	// kept out of the query list; "duplicate" for a span that reports a
	// call another span of the trace already counts (the app's own span
	// around a statement the Postgres proxy timed, say). Empty otherwise.
	DBRole string `json:"db_role,omitempty"`
	// Entry says this span is a request: what the endpoints count.
	Entry bool `json:"entry,omitempty"`
	// DBServer is the host:port a database call went to, when known.
	DBServer string `json:"db_server,omitempty"`
}

// TraceSpans returns every span of one trace in start order.
func (s *Store) TraceSpans(ctx context.Context, traceID string) ([]SpanRow, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT span_id, parent_id, service, name, kind, start_ns / 1e6, duration_ns / 1e6,
	status, status_message, db_system, db_statement, http_method, http_route, http_status, attrs,
	db_role, entry, db_server
FROM spans WHERE trace_id = ? ORDER BY start_ns`, traceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SpanRow{}
	for rows.Next() {
		var sp SpanRow
		var attrs string
		var role int8
		if err := rows.Scan(&sp.SpanID, &sp.ParentID, &sp.Service, &sp.Name, &sp.Kind,
			&sp.StartMs, &sp.DurationMs, &sp.Status, &sp.StatusMessage, &sp.DBSystem,
			&sp.DBStatement, &sp.HTTPMethod, &sp.HTTPRoute, &sp.HTTPStatus, &attrs,
			&role, &sp.Entry, &sp.DBServer); err != nil {
			return nil, err
		}
		sp.Attrs, sp.DBRole = rawJSON(attrs), roleName(role)
		out = append(out, sp)
	}
	return out, rows.Err()
}

type QueryRow struct {
	DBSystem    string  `json:"db_system"`
	Fingerprint string  `json:"fingerprint"`
	Example     string  `json:"example"`
	Calls       int64   `json:"calls"`
	TotalMs     float64 `json:"total_ms"`
	AvgMs       float64 `json:"avg_ms"`
	P50Ms       float64 `json:"p50_ms"`
	P95Ms       float64 `json:"p95_ms"`
	MaxMs       float64 `json:"max_ms"`
	Errors      int64   `json:"errors"`
	LastMs      float64 `json:"last_ms"`
	Traces      int64   `json:"traces"`
	// MaxPerTrace is the most times one trace ran this query: a high number
	// is the mark of an N+1.
	MaxPerTrace int64 `json:"max_per_trace"`
}

// Queries groups database spans by fingerprint, costliest first. Only the
// spans that count as queries are in it: a statement seen twice is there
// once, and BEGIN, COMMIT and their like are left to Filter.Control.
//
// The timings and the per-trace counts are worked out side by side and
// joined at the end, a dozen rows to a dozen rows. Joining every database
// span to the per-trace counts first, as this once did, cost more than
// both aggregates together.
func (s *Store) Queries(ctx context.Context, f Filter) ([]QueryRow, error) {
	where := isQuery + ` AND start_ns >= ?`
	if f.Control {
		where = `db_role = 2 AND start_ns >= ?`
	}
	args := []any{nsFromMs(f.SinceMs)}
	if f.DBSystem != "" {
		where += ` AND db_system = ?`
		args = append(args, f.DBSystem)
	}
	if f.DBServer != "" {
		where += ` AND db_server = ?`
		args = append(args, DBServer(f.DBServer, 0))
	}
	if f.Service != "" {
		where += ` AND service = ?`
		args = append(args, f.Service)
	}
	if f.Search != "" {
		where += ` AND db_statement ILIKE ?`
		args = append(args, like(f.Search))
	}
	q := `
WITH q AS (
	SELECT db_system, db_fingerprint, db_statement, trace_id, duration_ns, start_ns, status
	FROM spans WHERE ` + where + `
), timing AS (
	SELECT db_system, db_fingerprint, any_value(db_statement) AS example,
		count(*) AS calls, sum(duration_ns) AS total_ns, avg(duration_ns) AS avg_ns,
		quantile_cont(duration_ns, [0.5, 0.95]) AS qs, max(duration_ns) AS max_ns,
		count(*) FILTER (WHERE status = 'error') AS errors, max(start_ns) AS last_ns
	FROM q GROUP BY db_system, db_fingerprint
), per_trace AS (
	SELECT db_system, db_fingerprint, count(*) AS traces, max(n) AS max_n FROM (
		SELECT db_system, db_fingerprint, trace_id, count(*) AS n FROM q
		GROUP BY db_system, db_fingerprint, trace_id
	) GROUP BY db_system, db_fingerprint
)
SELECT t.db_system, t.db_fingerprint, t.example, t.calls, t.total_ns / 1e6, t.avg_ns / 1e6,
	t.qs[1] / 1e6, t.qs[2] / 1e6, t.max_ns / 1e6, t.errors, t.last_ns / 1e6, p.traces, p.max_n
FROM timing t JOIN per_trace p ON p.db_system = t.db_system AND p.db_fingerprint = t.db_fingerprint
ORDER BY t.total_ns DESC
LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, append(args, f.limit(200))...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []QueryRow{}
	for rows.Next() {
		var r QueryRow
		if err := rows.Scan(&r.DBSystem, &r.Fingerprint, &r.Example, &r.Calls, &r.TotalMs,
			&r.AvgMs, &r.P50Ms, &r.P95Ms, &r.MaxMs, &r.Errors, &r.LastMs, &r.Traces,
			&r.MaxPerTrace); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type LogRow struct {
	TsMs     float64         `json:"ts_ms"`
	Service  string          `json:"service"`
	Severity string          `json:"severity"`
	Body     string          `json:"body"`
	TraceID  string          `json:"trace_id"`
	SpanID   string          `json:"span_id"`
	Attrs    json.RawMessage `json:"attrs"`
	// Kind is what sort of record a provider says this is ("leadline.kind":
	// "browser.error", "browser.web_vital"), and UserID who it was written
	// for ("user.id"). Only Logs sets them.
	Kind   string `json:"kind,omitempty"`
	UserID string `json:"user_id,omitempty"`
	// Cursor is this row's place in the list, for Filter.Before. Only Logs
	// sets it.
	Cursor string `json:"cursor,omitempty"`
}

// Logs lists log records, newest first. Records of the same nanosecond come
// in the order they were stored.
//
// It reads backwards in widening stretches of time and stops once it has
// enough. Rows arrive in roughly time order, so DuckDB knows from each
// block's oldest and newest time which blocks a stretch can skip: the
// newest page costs the same on a day of logs as on a minute. Only a search
// that matches almost nothing reads the whole window.
func (s *Store) Logs(ctx context.Context, f Filter) ([]LogRow, error) {
	limit := f.limit(500)
	since := nsFromMs(f.SinceMs)
	out := []LogRow{}

	// A log has no id, so its place among records of its own nanosecond is
	// a count: the cursor "t.3" is the third record stamped t, and the page
	// after it skips three.
	var top, tiedAt int64
	var tied int
	paged := f.Before != ""
	if paged {
		at, k, err := cutCursor(f.Before)
		if err != nil {
			return nil, err
		}
		if tied, err = strconv.Atoi(k); err != nil || tied < 1 {
			return nil, ErrBadCursor
		}
		top, tiedAt = at, at
	}
	// The table's oldest and newest times come from its statistics, at no
	// cost. They say where to start and when there is nothing left.
	var oldest, newest sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT min(ts_ns), max(ts_ns) FROM logs`).Scan(&oldest, &newest); err != nil {
		return nil, err
	}
	if !newest.Valid {
		return out, nil
	}
	if !paged {
		top = newest.Int64
	}
	since = max(since, oldest.Int64)

	where := ``
	var args []any
	if f.Service != "" {
		where += ` AND service = ?`
		args = append(args, f.Service)
	}
	if f.Search != "" {
		where += ` AND body ILIKE ?`
		args = append(args, like(f.Search))
	}
	if f.MinSeverity > 0 {
		where += ` AND severity_num >= ?`
		args = append(args, f.MinSeverity)
	}
	if f.Kind != "" {
		where += ` AND kind = ?`
		args = append(args, f.Kind)
	}
	if f.User != "" {
		where += ` AND user_id = ?`
		args = append(args, f.User)
	}
	// Logs written during the traces that touched the database. A trace's
	// spans are looked for around the stretch of time being read, not over
	// the whole window.
	db := dbFilter("", f.DBSystem, f.DBServer)

	var prev int64
	run := 0 // how many records of time prev have gone by
	newestRead := top
	for width := int64(time.Minute); len(out) < limit && top >= since; {
		// The last stretch takes whatever is left; so does one whose
		// width has outgrown an int64.
		from := since
		if lo := top - width + 1; width > 0 && lo > since && lo <= top {
			from = lo
		}
		q := `SELECT ts_ns, service, severity, body, trace_id, span_id, attrs, kind, user_id FROM logs WHERE ts_ns >= ?`
		bounds := []any{from}
		// The newest page has no upper bound: a record that landed since
		// the statistics were read belongs on it.
		if paged || len(out) > 0 || top != newest.Int64 {
			q += ` AND ts_ns <= ?`
			bounds = append(bounds, top)
		}
		q += where
		params := append(bounds, args...)
		if db != "" {
			q += ` AND trace_id <> '' AND trace_id IN (SELECT trace_id FROM spans WHERE start_ns >= ? AND start_ns <= ? AND ` + db + `)`
			params = append(params, from-int64(traceLogSlack), top+int64(traceLogSlack))
		}
		q += ` ORDER BY ts_ns DESC, rowid ASC LIMIT ?`
		rows, err := s.db.QueryContext(ctx, q, append(params, limit-len(out)+tied)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var l LogRow
			var ts int64
			var attrs string
			if err := rows.Scan(&ts, &l.Service, &l.Severity, &l.Body, &l.TraceID, &l.SpanID, &attrs, &l.Kind, &l.UserID); err != nil {
				rows.Close()
				return nil, err
			}
			if ts != prev {
				prev, run = ts, 0
			}
			if run++; tied > 0 && ts == tiedAt && run <= tied {
				continue // the page before ended after this one
			}
			if len(out) == limit {
				break
			}
			l.TsMs = float64(ts) / 1e6
			l.Attrs = rawJSON(attrs)
			l.Cursor = strconv.FormatInt(ts, 10) + "." + strconv.Itoa(run)
			out = append(out, l)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		tied = 0
		if from <= since {
			break
		}
		top = from - 1
		// How much further back to read: as far as the rows found so far
		// say the rest should need, and half as much again, or a lot
		// further when there were none. Never less than eight times the
		// last stretch, so a wrong guess still gets there in a few goes.
		width *= 8
		guess := float64(width) * 8
		if len(out) > 0 {
			guess = float64(newestRead-from+1) * float64(limit-len(out)) / float64(len(out)) * 1.5
		}
		if guess > float64(width) {
			width = int64(min(guess, 1e18))
		}
	}
	return out, nil
}

// TraceLogs returns the logs written during one trace, oldest first.
func (s *Store) TraceLogs(ctx context.Context, traceID string) ([]LogRow, error) {
	return s.logRows(ctx, `
SELECT ts_ns / 1e6, service, severity, body, trace_id, span_id, attrs FROM logs
WHERE trace_id = ? ORDER BY ts_ns LIMIT 1000`, traceID)
}

// traceLogSlack is how far outside a trace's spans Trace looks for its logs.
const traceLogSlack = 5 * time.Minute

// Trace is one trace opened: its spans in start order and the logs written
// during it. A trace id says nothing about when the trace ran, so finding
// its spans means reading the id of every span stored. The logs then only
// need the stretch of time the spans cover, with five minutes either side
// for clocks that disagree; TraceLogs reads every log's id instead.
func (s *Store) Trace(ctx context.Context, traceID string) ([]SpanRow, []LogRow, error) {
	spans, err := s.TraceSpans(ctx, traceID)
	if err != nil || len(spans) == 0 {
		return spans, []LogRow{}, err
	}
	from, to := spans[0].StartMs, spans[0].StartMs
	for _, sp := range spans {
		to = max(to, sp.StartMs+sp.DurationMs)
	}
	logs, err := s.logRows(ctx, `
SELECT ts_ns / 1e6, service, severity, body, trace_id, span_id, attrs FROM logs
WHERE ts_ns >= ? AND ts_ns <= ? AND trace_id = ? ORDER BY ts_ns LIMIT 1000`,
		nsFromMs(from)-int64(traceLogSlack), nsFromMs(to)+int64(traceLogSlack), traceID)
	return spans, logs, err
}

func (s *Store) logRows(ctx context.Context, q string, args ...any) ([]LogRow, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LogRow{}
	for rows.Next() {
		var l LogRow
		var attrs string
		if err := rows.Scan(&l.TsMs, &l.Service, &l.Severity, &l.Body, &l.TraceID, &l.SpanID, &attrs); err != nil {
			return nil, err
		}
		l.Attrs = rawJSON(attrs)
		out = append(out, l)
	}
	return out, rows.Err()
}

// Series is a set of aligned time series: every slice has one value per
// bucket, and T holds each bucket's start.
type Series struct {
	BucketMs  float64   `json:"bucket_ms"`
	T         []float64 `json:"t"`
	Requests  []float64 `json:"requests"`
	Errors    []float64 `json:"errors"`
	Queries   []float64 `json:"queries"`
	P50Ms     []float64 `json:"p50_ms"`
	P95Ms     []float64 `json:"p95_ms"`
	DBMs      []float64 `json:"db_ms"`
	Logs      []float64 `json:"logs"`
	LogErrors []float64 `json:"log_errors"`
}

// SeriesBetween buckets everything from sinceMs to untilMs into n buckets.
// Requests and latency are of entry spans, the same requests the endpoint
// breakdown counts, so the two agree with a service filter and without.
func (s *Store) SeriesBetween(ctx context.Context, sinceMs, untilMs float64, n int, service string) (*Series, error) {
	if n < 1 {
		n = 1
	}
	since, until := nsFromMs(sinceMs), nsFromMs(untilMs)
	bucket := (until - since) / int64(n)
	if bucket < 1 {
		bucket = 1
	}
	out := &Series{BucketMs: float64(bucket) / 1e6}
	for _, p := range []*[]float64{&out.T, &out.Requests, &out.Errors, &out.Queries,
		&out.P50Ms, &out.P95Ms, &out.DBMs, &out.Logs, &out.LogErrors} {
		*p = make([]float64, n)
	}
	for i := range out.T {
		out.T[i] = float64(since+int64(i)*bucket) / 1e6
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT (start_ns - ?) // ? AS b,
	count(*) FILTER (WHERE `+isEntry+`),
	count(*) FILTER (WHERE `+isFailed+`),
	count(*) FILTER (WHERE `+isQuery+`),
	coalesce(quantile_cont(duration_ns, 0.5) FILTER (WHERE `+isEntry+`), 0) / 1e6,
	coalesce(quantile_cont(duration_ns, 0.95) FILTER (WHERE `+isEntry+`), 0) / 1e6,
	coalesce(sum(duration_ns) FILTER (WHERE `+isDBTime+`), 0) / 1e6
FROM spans
WHERE start_ns >= ? AND start_ns < ? AND (? = '' OR service = ?)
GROUP BY b`, since, bucket, since, until, service, service)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var b int64
		var req, errs, queries int64
		var p50, p95, db float64
		if err := rows.Scan(&b, &req, &errs, &queries, &p50, &p95, &db); err != nil {
			return nil, err
		}
		if b < 0 || b >= int64(n) {
			continue
		}
		out.Requests[b], out.Errors[b], out.Queries[b] = float64(req), float64(errs), float64(queries)
		out.P50Ms[b], out.P95Ms[b], out.DBMs[b] = p50, p95, db
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Severity 17 and up is error and fatal.
	lrows, err := s.db.QueryContext(ctx, `
SELECT (ts_ns - ?) // ? AS b, count(*), count(*) FILTER (WHERE severity_num >= 17)
FROM logs
WHERE ts_ns >= ? AND ts_ns < ? AND (? = '' OR service = ?)
GROUP BY b`, since, bucket, since, until, service, service)
	if err != nil {
		return nil, err
	}
	defer lrows.Close()
	for lrows.Next() {
		var b, all, errs int64
		if err := lrows.Scan(&b, &all, &errs); err != nil {
			return nil, err
		}
		if b < 0 || b >= int64(n) {
			continue
		}
		out.Logs[b], out.LogErrors[b] = float64(all), float64(errs)
	}
	return out, lrows.Err()
}

type Summary struct {
	Spans  int64 `json:"spans"`
	Logs   int64 `json:"logs"`
	Traces int64 `json:"traces"`
	// MetricPoints counts stored metric data points. An app that sends
	// only metrics has no spans and no logs, and is still an app.
	MetricPoints int64    `json:"metric_points"`
	Services     []string `json:"services"`
}

// summaryCache remembers the count of distinct traces, the one figure in
// Summary that means reading every span.
type summaryCache struct {
	mu     sync.Mutex
	known  bool
	gen    int64 // the store's gen when it was counted
	traces int64
	at     time.Time
	took   time.Duration
}

// bigStore is the span count past which the trace count may lag.
const bigStore = 1_000_000

// Summary counts what the store holds and names the services seen.
//
// The app asks this every time data arrives. Spans, logs and metric points
// are counted from the tables' own bookkeeping. Traces are counted again only when rows
// have changed, and in a store of more than a million spans, where the
// count takes real time, no more often than keeps it to about a hundredth
// of the daemon's time: on five million spans it is at most a second or so
// behind.
func (s *Store) Summary(ctx context.Context) (*Summary, error) {
	out := &Summary{Services: []string{}}
	err := s.db.QueryRowContext(ctx, `
SELECT (SELECT count(*) FROM spans), (SELECT count(*) FROM logs), (SELECT count(*) FROM metric_points)`).
		Scan(&out.Spans, &out.Logs, &out.MetricPoints)
	if err != nil {
		return nil, err
	}

	c := &s.summary
	c.mu.Lock()
	gen := s.gen.Load()
	if !c.known || (c.gen != gen && (out.Spans < bigStore || time.Since(c.at) > 100*c.took)) {
		began := time.Now()
		var traces int64
		if err := s.db.QueryRowContext(ctx, `SELECT count(DISTINCT trace_id) FROM spans`).Scan(&traces); err != nil {
			c.mu.Unlock()
			return nil, err
		}
		c.known, c.gen, c.traces, c.at, c.took = true, gen, traces, time.Now(), time.Since(began)
	}
	out.Traces = c.traces
	c.mu.Unlock()

	rows, err := s.db.QueryContext(ctx, `
SELECT service FROM (
	SELECT service FROM spans UNION SELECT service FROM logs UNION SELECT service FROM metric_points
) ORDER BY service`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out.Services = append(out.Services, name)
	}
	return out, rows.Err()
}

// like turns a search term into an ILIKE pattern, with the term's own
// wildcards left as they are: a dev tool's search box can afford that.
func like(term string) string {
	if term == "" {
		return ""
	}
	return "%" + term + "%"
}

func rawJSON(s string) json.RawMessage {
	if strings.TrimSpace(s) == "" || !json.Valid([]byte(s)) {
		return json.RawMessage("{}")
	}
	return json.RawMessage(s)
}
