package store

import (
	"context"
	"database/sql"
	"encoding/json"
)

// What a brief needs beyond the breakdowns: when a problem was first and
// last seen, the attributes that name a file, and a trace's query shapes.
// Each is asked once, when a person asks for a brief, so none of them is
// tuned the way the app's polled questions are.

// Seen is when something was first and last recorded, in everything the
// store still holds, and the service most of it ran in.
type Seen struct {
	FirstMs, LastMs float64
	Service         string
}

// QuerySeen says when a query shape was first and last run. service ""
// is any service.
func (s *Store) QuerySeen(ctx context.Context, key QueryKey, service string) (Seen, error) {
	return s.seen(ctx, `
SELECT min(start_ns) / 1e6, max(start_ns) / 1e6, mode(service) FROM spans
WHERE db_system = ? AND db_fingerprint = ? AND `+isQuery+` AND (? = '' OR service = ?)`,
		key.DBSystem, key.Fingerprint, service, service)
}

// EndpointSeen says when an endpoint was first and last requested.
func (s *Store) EndpointSeen(ctx context.Context, ep Endpoint) (Seen, error) {
	return s.seen(ctx, `
SELECT min(start_ns) / 1e6, max(start_ns) / 1e6, mode(service) FROM spans
WHERE service = ? AND name = ? AND `+isEntry, ep.Service, ep.Name)
}

func (s *Store) seen(ctx context.Context, q string, args ...any) (Seen, error) {
	var first, last sql.NullFloat64
	var service sql.NullString
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&first, &last, &service); err != nil {
		return Seen{}, err
	}
	return Seen{FirstMs: first.Float64, LastMs: last.Float64, Service: service.String}, nil
}

// EndpointAttrs returns the attributes of an endpoint's newest request in
// the window: where a framework says which file serves it ("next.file",
// "code.filepath"). Nil when it had no request.
func (s *Store) EndpointAttrs(ctx context.Context, w Window, ep Endpoint) (json.RawMessage, error) {
	return s.attrs(ctx, `
SELECT attrs FROM spans WHERE `+inWindow+` AND `+entrySpan+oneEndpoint+`
ORDER BY start_ns DESC LIMIT 1`, w.args(ep.Service, ep.Name)...)
}

// QueryAttrs returns the attributes of a query's slowest call in the
// window. Nil when it did not run.
func (s *Store) QueryAttrs(ctx context.Context, w Window, key QueryKey) (json.RawMessage, error) {
	return s.attrs(ctx, `
SELECT attrs FROM spans WHERE `+inWindow+` AND `+oneQuery+`
ORDER BY duration_ns DESC LIMIT 1`, w.args(key.DBSystem, key.Fingerprint)...)
}

func (s *Store) attrs(ctx context.Context, q string, args ...any) (json.RawMessage, error) {
	var attrs string
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&attrs)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return rawJSON(attrs), nil
}

// TraceShapes maps each database span of a trace to its statement's shape.
func (s *Store) TraceShapes(ctx context.Context, traceID string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT span_id, db_fingerprint FROM spans WHERE trace_id = ? AND db_system <> ''`, traceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, shape string
		if err := rows.Scan(&id, &shape); err != nil {
			return nil, err
		}
		out[id] = shape
	}
	return out, rows.Err()
}

// LogAt finds one log record by where it stands in the logs list: its time
// in nanoseconds and its place, from 1, among the records of that instant
// (the two halves of LogRow.Cursor). With a service, only that service's
// records are counted. A place past the end gives the first; nil when no
// record has that time.
func (s *Store) LogAt(ctx context.Context, ns int64, place int, service string) (*LogRow, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT ts_ns / 1e6, service, severity, body, trace_id, span_id, attrs FROM logs
WHERE ts_ns = ? AND (? = '' OR service = ?) ORDER BY rowid LIMIT 64`, ns, service, service)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []LogRow
	for rows.Next() {
		var l LogRow
		var attrs string
		if err := rows.Scan(&l.TsMs, &l.Service, &l.Severity, &l.Body, &l.TraceID, &l.SpanID, &attrs); err != nil {
			return nil, err
		}
		l.Attrs = rawJSON(attrs)
		all = append(all, l)
	}
	if err := rows.Err(); err != nil || len(all) == 0 {
		return nil, err
	}
	if place < 1 || place > len(all) {
		place = 1
	}
	return &all[place-1], nil
}

// ServiceLogs returns a service's newest records at or above a severity,
// from sinceMs on, newest first and at most limit of them: what a brief
// reads to count the records that say the same thing.
func (s *Store) ServiceLogs(ctx context.Context, service string, minSeverity int32, sinceMs float64, limit int) ([]LogRow, error) {
	return s.logRows(ctx, `
SELECT ts_ns / 1e6, service, severity, body, trace_id, span_id, attrs FROM logs
WHERE ts_ns >= ? AND service = ? AND severity_num >= ? ORDER BY ts_ns DESC LIMIT ?`,
		nsFromMs(sinceMs), service, minSeverity, limit)
}

// ErrorCount is an error message and how many spans ended with it.
type ErrorCount struct {
	Message string
	Count   int64
}

// QueryErrors lists the messages a query's failed calls ended with in the
// window, the commonest first.
func (s *Store) QueryErrors(ctx context.Context, w Window, key QueryKey, limit int) ([]ErrorCount, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT status_message, count(*) AS n FROM spans
WHERE `+inWindow+` AND `+oneQuery+` AND status = 'error'
GROUP BY status_message ORDER BY n DESC, status_message LIMIT $6`, w.args(key.DBSystem, key.Fingerprint, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ErrorCount
	for rows.Next() {
		var e ErrorCount
		if err := rows.Scan(&e.Message, &e.Count); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
