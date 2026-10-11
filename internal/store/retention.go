package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Retention says how much the store keeps. Without it nothing is ever
// deleted and the file only grows.
type Retention struct {
	// MaxAge is how long a row is kept. Zero keeps rows whatever their age.
	MaxAge time.Duration
	// MaxBytes is how large the database may get before its oldest rows go
	// early. Zero is no cap. It needs a database file.
	MaxBytes int64
	// Every is how often to look. Zero means once a minute.
	Every time.Duration
}

// aged is every table that grows with time, and the column that says when a
// row is from. A feature that adds such a table adds it here, or its rows
// outlive every span and log.
var aged = []struct{ table, column string }{
	{"spans", "start_ns"},
	{"logs", "ts_ns"},
	{"metric_points", "ts_ns"},
}

// Pruned counts what a prune deleted.
type Pruned struct {
	Spans, Logs, Other int64
}

func (p Pruned) rows() int64 { return p.Spans + p.Logs + p.Other }

// Prune deletes everything older than cutoff and gives the space back.
//
// Rows arrive in time order, so the old ones sit together at the front of
// the file. The delete finds them from the blocks' time ranges without
// reading the rest, and ingest and reads carry on beside it: DuckDB lets
// appends and a delete of other rows commit side by side. The checkpoint
// that follows is what frees the space, and it frees whole blocks of rows
// (about 120,000 each), which is why spans and logs have no index: DuckDB
// cannot free a block of a table that has one. The file does not get
// smaller. Its freed blocks are reused, so it stops growing.
func (s *Store) Prune(ctx context.Context, cutoff time.Time) (Pruned, error) {
	var p Pruned
	for _, t := range aged {
		var exists bool
		err := s.db.QueryRowContext(ctx, `
SELECT count(*) > 0 FROM information_schema.columns WHERE table_name = ? AND column_name = ?`,
			t.table, t.column).Scan(&exists)
		if err != nil {
			return p, err
		}
		if !exists {
			continue
		}
		res, err := s.db.ExecContext(ctx, `DELETE FROM `+t.table+` WHERE `+t.column+` < ?`, cutoff.UnixNano())
		if err != nil {
			return p, fmt.Errorf("prune %s: %w", t.table, err)
		}
		n, _ := res.RowsAffected()
		switch t.table {
		case "spans":
			p.Spans = n
		case "logs":
			p.Logs = n
		default:
			p.Other += n
		}
	}
	// What is remembered of each metric series goes the way of its points:
	// a series whose newest point is gone will not be measured from again,
	// and without this the memory of every series ever seen is kept.
	s.mu.Lock()
	s.metrics.forget(cutoff.UnixNano())
	s.mu.Unlock()
	if p.rows() == 0 {
		return p, nil
	}
	s.gen.Add(1)
	if s.path == "" {
		return p, nil
	}
	return p, s.checkpoint()
}

// Forgotten counts what Forget deleted.
type Forgotten struct {
	Spans        int64 `json:"spans"`
	Logs         int64 `json:"logs"`
	MetricPoints int64 `json:"metric_points"`
}

func (f Forgotten) rows() int64 { return f.Spans + f.Logs + f.MetricPoints }

// forgettable is every table with rows filed under a service. A feature
// that adds such a table adds it here, or a forgotten service leaves rows
// behind in it.
var forgettable = []string{"spans", "logs", "metric_points", "route_lists"}

// Forget deletes everything filed under one service: its spans, logs,
// metric points and route list. With a prefix it also takes the services
// whose names begin with it, which is how a source that files under several
// names ("acme/fix-orders" and "acme/fix-orders:worker") goes as one.
//
// It is the delete Prune does, by name and not by age. Unlike old rows a
// service's rows lie anywhere in the file, so the delete reads the service
// column from end to end: under a second, checkpoint included, to take a
// tenth of three million spans. It holds nothing ingest or a read needs
// while it does: DuckDB lets appends and a delete of other rows commit side
// by side, and measured beside it an export still took 3ms at worst and a
// read no longer than before. The rows stop being seen at once. Their space
// comes back a block at a time, as Prune frees the blocks they were in.
//
// Rows of the service that arrive after the call are kept: Forget is for a
// source that has stopped.
func (s *Store) Forget(ctx context.Context, service, prefix string) (Forgotten, error) {
	var f Forgotten
	if service == "" {
		return f, nil
	}
	where, args := `service = ?`, []any{service}
	if prefix != "" {
		where += ` OR starts_with(service, ?)`
		args = append(args, prefix)
	}
	for _, table := range forgettable {
		var n int64
		var err error
		// The flusher may be correcting one of these very rows (a query's
		// second sighting). That is a conflict, not a failure: it has
		// committed by the time this asks again.
		for try := 0; ; try++ {
			var res sql.Result
			if res, err = s.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE `+where, args...); err == nil {
				n, _ = res.RowsAffected()
				break
			}
			if try == 3 || !strings.Contains(err.Error(), "Conflict") {
				return f, fmt.Errorf("forget %s in %s: %w", service, table, err)
			}
			time.Sleep(time.Duration(try+1) * 20 * time.Millisecond)
		}
		switch table {
		case "spans":
			f.Spans = n
		case "logs":
			f.Logs = n
		case "metric_points":
			f.MetricPoints = n
		}
	}
	// What is remembered of the service's metric series goes with its
	// points: a source of the same name that starts again starts from
	// nothing, not from where this one stopped.
	gone := func(name string) bool {
		return name == service || (prefix != "" && strings.HasPrefix(name, prefix))
	}
	s.mu.Lock()
	s.metrics.forgetServices(gone)
	s.mu.Unlock()
	if f.rows() == 0 {
		return f, nil
	}
	s.gen.Add(1)
	if s.path == "" {
		return f, nil
	}
	return f, s.checkpoint()
}

// forgetServices drops what is remembered of the series and streams of the
// services gone names.
func (st *metricState) forgetServices(gone func(service string) bool) {
	for id, p := range st.last {
		if gone(p.Service) {
			delete(st.last, id)
		}
	}
	for key := range st.streams {
		if gone(key.service) {
			delete(st.streams, key)
		}
	}
}

// Size is how many bytes of the database file hold data, with the
// write-ahead log. The file itself may be larger: space freed by a prune
// stays in it, to be used again.
func (s *Store) Size(ctx context.Context) (int64, error) {
	if s.path == "" {
		return 0, nil
	}
	rows, err := s.db.QueryContext(ctx, `PRAGMA database_size`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	var size int64
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return 0, err
		}
		var block, used int64
		for i, c := range cols {
			n, _ := strconv.ParseInt(fmt.Sprint(vals[i]), 10, 64)
			switch c {
			case "block_size":
				block = n
			case "used_blocks":
				used = n
			}
		}
		size += block * used
	}
	return size + s.walSize(), rows.Err()
}

// Storage is how much the store holds and how much it is allowed to.
type Storage struct {
	// SizeBytes is the data held: the part of the database file in use,
	// and the write-ahead log. FileBytes is what the files take on disk,
	// which is more after a prune: freed space stays in the file, to be
	// used again. Both are 0 for a store kept in memory.
	SizeBytes int64 `json:"size_bytes"`
	FileBytes int64 `json:"file_bytes"`
	// MaxBytes is the size past which the oldest data goes early, and
	// RetainMs how long a row is kept; 0 is no cap and for ever.
	MaxBytes int64   `json:"max_bytes"`
	RetainMs float64 `json:"retain_ms"`
	// OldestMs is the time of the oldest span, log or metric point held;
	// 0 when there is none.
	OldestMs float64 `json:"oldest_ms"`
}

// Storage reports what is kept. It reads the tables' own bookkeeping, so
// it is cheap to ask every few seconds.
func (s *Store) Storage(ctx context.Context) (*Storage, error) {
	out := &Storage{}
	if r := s.keep.Load(); r != nil {
		out.MaxBytes, out.RetainMs = r.MaxBytes, float64(r.MaxAge.Milliseconds())
	}
	var err error
	if out.SizeBytes, err = s.Size(ctx); err != nil {
		return nil, err
	}
	if s.path != "" {
		for _, p := range []string{s.path, s.path + ".wal"} {
			if fi, err := os.Stat(p); err == nil {
				out.FileBytes += fi.Size()
			}
		}
	}
	var oldest *int64
	err = s.db.QueryRowContext(ctx, `
SELECT least((SELECT min(start_ns) FROM spans), (SELECT min(ts_ns) FROM logs), (SELECT min(ts_ns) FROM metric_points))`).Scan(&oldest)
	if err != nil {
		return nil, err
	}
	if oldest != nil {
		out.OldestMs = float64(*oldest) / 1e6
	}
	return out, nil
}

// Retain applies r now and then every r.Every, until the store closes. The
// first pass may have a lot to delete, after days switched off.
func (s *Store) Retain(r Retention) {
	s.keep.Store(&r)
	if r.MaxAge <= 0 && r.MaxBytes <= 0 {
		return
	}
	if r.Every <= 0 {
		r.Every = time.Minute
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		defer cancel()
		next := time.NewTimer(0)
		defer next.Stop()
		var floor int64
		for {
			select {
			case <-s.stop:
				return
			case <-next.C:
			}
			if err := s.retain(ctx, r, &floor); err != nil {
				slog.Error("retention", "err", err)
			}
			next.Reset(r.Every)
		}
	}()
}

// retain is one pass. floor is the size the last cut for the cap left
// behind, kept between passes; see below.
func (s *Store) retain(ctx context.Context, r Retention, floor *int64) error {
	if r.MaxAge > 0 {
		p, err := s.Prune(ctx, time.Now().Add(-r.MaxAge))
		if err != nil {
			return err
		}
		if p.rows() > 0 {
			slog.Info("dropped old data", "older_than", r.MaxAge.String(), "spans", p.Spans, "logs", p.Logs, "other", p.Other)
		}
	}
	if r.MaxBytes <= 0 {
		return nil
	}
	size, err := s.Size(ctx)
	// A cut frees whole blocks of rows, so it can leave the file over the
	// cap with rows that are deleted but not yet freed. Cutting again for
	// those would take live data for nothing, pass after pass. So after a
	// cut, the next one waits until the store has grown past what the
	// last left behind.
	if err != nil || size <= r.MaxBytes || size <= *floor {
		return err
	}
	// Drop the oldest share of rows that should bring the size to nine
	// tenths of the cap. The share is measured in the spans' time, or the
	// logs' for an app that sends none, or the metric points' for one that
	// sends only those; every table is cut at that time.
	share := min(0.95, max(0.05, 1-0.9*float64(r.MaxBytes)/float64(size)))
	q := strconv.FormatFloat(share, 'f', 3, 64)
	var cutoff *int64
	err = s.db.QueryRowContext(ctx, `
SELECT CAST(coalesce(
	(SELECT approx_quantile(start_ns, `+q+`) FROM spans),
	(SELECT approx_quantile(ts_ns, `+q+`) FROM logs),
	(SELECT approx_quantile(ts_ns, `+q+`) FROM metric_points)) AS BIGINT)`).Scan(&cutoff)
	if err != nil || cutoff == nil {
		return err
	}
	p, err := s.Prune(ctx, time.Unix(0, *cutoff))
	if err != nil {
		return err
	}
	left, err := s.Size(ctx)
	if err != nil {
		return err
	}
	*floor = left + left/20
	slog.Info("dropped the oldest data to stay under the size cap", "size_mb", size>>20, "cap_mb", r.MaxBytes>>20, "now_mb", left>>20,
		"older_than", time.Unix(0, *cutoff).Format(time.RFC3339), "spans", p.Spans, "logs", p.Logs, "other", p.Other)
	return nil
}

// ParseAge reads how long to keep data: a Go duration ("72h"), or days
// ("7d"). "0" and "off" mean forever.
func ParseAge(v string) (time.Duration, error) {
	v = strings.TrimSpace(strings.ToLower(v))
	if v == "" || v == "0" || v == "off" {
		return 0, nil
	}
	if days, ok := strings.CutSuffix(v, "d"); ok {
		n, err := strconv.ParseFloat(days, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("%q is not a number of days", v)
		}
		return time.Duration(n * 24 * float64(time.Hour)), nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%q is not a length of time like 72h or 7d", v)
	}
	return d, nil
}

// ParseSize reads a size cap: "2GB", "500MB", or plain bytes. "0" and
// "off" mean no cap.
func ParseSize(v string) (int64, error) {
	v = strings.TrimSpace(strings.ToUpper(v))
	if v == "" || v == "0" || v == "OFF" {
		return 0, nil
	}
	unit := int64(1)
	for _, u := range []struct {
		suffix string
		bytes  int64
	}{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1}} {
		if rest, ok := strings.CutSuffix(v, u.suffix); ok {
			v, unit = strings.TrimSpace(rest), u.bytes
			break
		}
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("not a size like 2GB or 500MB")
	}
	return int64(n * float64(unit)), nil
}
