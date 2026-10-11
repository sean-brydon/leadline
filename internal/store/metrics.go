package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	duckdb "github.com/duckdb/duckdb-go/v2"
)

// One row per metric data point. value, count, sum, bounds and counts are
// what the app sent. Most SDKs send running totals ("cumulative"), which
// can't be added up over time, so each row also carries what its point added
// since the one before it (d_*) and when that stretch began (from_ns). The
// charts only ever sum those.
//
// counts has one more entry than bounds: the last bucket has no upper bound.
const metricsSchema = `
CREATE TABLE IF NOT EXISTS metric_points (
	name VARCHAR, unit VARCHAR, description VARCHAR,
	type VARCHAR, cumulative BOOLEAN, monotonic BOOLEAN,
	service VARCHAR, series BIGINT, attrs VARCHAR,
	ts_ns BIGINT, start_ns BIGINT,
	value DOUBLE, count BIGINT, sum DOUBLE, bounds DOUBLE[], counts BIGINT[],
	from_ns BIGINT, d_value DOUBLE, d_count BIGINT, d_sum DOUBLE, d_counts BIGINT[]
);
`

// Metric types, as stored. An exponential histogram is stored as a
// histogram, with its buckets rewritten as explicit ones (see ingest).
const (
	MetricGauge     = "gauge"
	MetricSum       = "sum"
	MetricHistogram = "histogram"
	MetricSummary   = "summary"
)

const (
	// A gauge's last value stands in for a missing one this long, so a
	// chart with buckets narrower than the export interval has no holes.
	staleNs = int64(5 * time.Minute)
	// A running total first seen this soon after it began is counted in
	// full: the app has only just started.
	freshNs = int64(5 * time.Minute)
)

// MetricPoint is one data point as the app sent it.
type MetricPoint struct {
	Name, Unit, Description string
	Type                    string // one of the Metric* constants
	// Cumulative says the numbers are running totals since StartNs, not
	// what happened since the last export. Always false for a gauge.
	Cumulative bool
	Monotonic  bool // sums only: it never goes down, a counter
	Service    string
	// Series tells one stream of points from another: a hash of the
	// resource, the scope, the metric's name and the point's attributes.
	Series        int64
	Attrs         string // a JSON object
	TsNs, StartNs int64
	Value         float64 // gauges and sums
	// Histograms and summaries. Counts is empty, or one longer than Bounds.
	Count  int64
	Sum    float64
	Bounds []float64
	Counts []int64
}

// increase is what a point added since the one before it in its series.
type increase struct {
	from   int64
	value  float64
	count  int64
	sum    float64
	counts []int64
}

// metricState remembers each series' newest point, which is what turns the
// next running total into an increase. It is rebuilt from the table when a
// metric is first seen after a start. It belongs to one store, and is
// guarded by the store's mu.
type metricState struct {
	last    map[int64]*MetricPoint
	streams map[streamKey]*streamSeen
	loaded  map[string]bool // metric names read back from the table
}

// forget drops what is remembered of series and streams last heard from
// before cutoff: their points have been pruned.
func (st *metricState) forget(cutoff int64) {
	for id, p := range st.last {
		if p.TsNs < cutoff {
			delete(st.last, id)
		}
	}
	for key, s := range st.streams {
		if s.cur < cutoff {
			delete(st.streams, key)
		}
	}
}

// A stream is one process's exports of one metric: every series in it
// shares a start time.
type streamKey struct {
	service, name string
	start         int64
}

// streamSeen holds the two newest export times of a stream.
type streamSeen struct{ prev, cur int64 }

// before returns the stream's newest export time earlier than ts, or 0.
func (s *streamSeen) before(ts int64) int64 {
	if s.cur < ts {
		return s.cur
	}
	return s.prev
}

func newMetricState() *metricState {
	return &metricState{
		last:    map[int64]*MetricPoint{},
		streams: map[streamKey]*streamSeen{},
		loaded:  map[string]bool{},
	}
}

// AddMetrics writes metric points and flushes.
func (s *Store) AddMetrics(points []MetricPoint) error {
	if len(points) == 0 {
		return nil
	}
	// An increase is measured from the point before, so time order matters.
	points = slices.Clone(points)
	slices.SortStableFunc(points, func(a, b MetricPoint) int {
		switch {
		case a.TsNs < b.TsNs:
			return -1
		case a.TsNs > b.TsNs:
			return 1
		}
		return 0
	})

	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.metrics
	if err := st.load(s.db, points); err != nil {
		return err
	}
	// Metrics arrive a few times a minute, so an appender per call is cheap.
	app, err := duckdb.NewAppenderFromConn(s.conn, "", "metric_points")
	if err != nil {
		return err
	}
	for i := range points {
		p := &points[i]
		if len(p.Counts) != len(p.Bounds)+1 {
			p.Bounds, p.Counts = nil, nil
		}
		d := st.step(p)
		err := app.AppendRow(
			p.Name, p.Unit, p.Description,
			p.Type, p.Cumulative, p.Monotonic,
			p.Service, p.Series, p.Attrs,
			p.TsNs, p.StartNs,
			p.Value, p.Count, p.Sum, list(p.Bounds), list(p.Counts),
			d.from, d.value, d.count, d.sum, list(d.counts),
		)
		if err != nil {
			app.Close()
			return fmt.Errorf("append metric point: %w", err)
		}
	}
	return app.Close()
}

// list gives the appender an empty list where a slice is nil.
func list[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

// load reads back the newest stored point of every series of the metrics in
// points that this process has not seen yet. Without it a restart of the
// daemon would take each running total for a new one.
func (st *metricState) load(db *sql.DB, points []MetricPoint) error {
	var names []any
	seen := map[string]bool{}
	for _, p := range points {
		if p.Cumulative && !st.loaded[p.Name] && !seen[p.Name] {
			seen[p.Name] = true
			names = append(names, p.Name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	rows, err := db.Query(`
SELECT series, service, name, type, monotonic, ts_ns, start_ns, value, count, sum, bounds, counts
FROM metric_points
WHERE cumulative AND name IN (`+strings.TrimSuffix(strings.Repeat("?,", len(names)), ",")+`)
QUALIFY row_number() OVER (PARTITION BY series ORDER BY ts_ns DESC) = 1`, names...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		p := &MetricPoint{Cumulative: true}
		var bounds, counts any
		if err := rows.Scan(&p.Series, &p.Service, &p.Name, &p.Type, &p.Monotonic, &p.TsNs, &p.StartNs,
			&p.Value, &p.Count, &p.Sum, &bounds, &counts); err != nil {
			return err
		}
		p.Bounds, p.Counts = scanList[float64](bounds), scanList[int64](counts)
		st.last[p.Series] = p
		key := streamKey{p.Service, p.Name, p.StartNs}
		if s := st.streams[key]; s == nil {
			st.streams[key] = &streamSeen{cur: p.TsNs}
		} else if p.TsNs > s.cur {
			s.cur = p.TsNs
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, n := range names {
		st.loaded[n.(string)] = true
	}
	return nil
}

// scanList converts a DuckDB list as the driver scans it.
func scanList[T any](v any) []T {
	items, _ := v.([]any)
	out := make([]T, 0, len(items))
	for _, it := range items {
		if x, ok := it.(T); ok {
			out = append(out, x)
		}
	}
	return out
}

// step works out what p added since the last point of its series, and
// remembers p for the next one.
func (st *metricState) step(p *MetricPoint) increase {
	whole := increase{from: p.TsNs, value: p.Value, count: p.Count, sum: p.Sum, counts: p.Counts}
	none := increase{from: p.TsNs}
	if p.Type == MetricGauge {
		return none
	}
	if !p.Cumulative {
		// Delta temporality: the point already is the increase.
		if p.StartNs > 0 && p.StartNs < p.TsNs {
			whole.from = p.StartNs
		}
		return whole
	}

	key := streamKey{p.Service, p.Name, p.StartNs}
	stream := st.streams[key]
	prev := st.last[p.Series]
	d := none
	switch {
	case prev != nil && p.TsNs <= prev.TsNs:
		// Sent twice, or late: it adds nothing, and must not become the
		// point the next one is measured from.
		return none
	case prev != nil && p.Type == MetricSum && !p.Monotonic:
		// An up-down counter is read as a level; its change may be negative.
		d = increase{from: prev.TsNs, value: p.Value - prev.Value}
	case prev != nil && !restarted(prev, p):
		d = increase{
			from:   prev.TsNs,
			value:  p.Value - prev.Value,
			count:  p.Count - prev.Count,
			sum:    p.Sum - prev.Sum,
			counts: bucketIncrease(prev, p),
		}
	case prev != nil:
		// The counter went back to zero: all of it is new since the restart.
		d = whole
		d.from = prev.TsNs
		if p.StartNs > prev.TsNs && p.StartNs < p.TsNs {
			d.from = p.StartNs
		}
	case stream != nil && stream.before(p.TsNs) != 0:
		// A new series in a stream already being watched (a route's first
		// request, say): it was absent from the export before, so all of it
		// happened since then.
		d = whole
		d.from = stream.before(p.TsNs)
	case p.StartNs > 0 && p.StartNs <= p.TsNs && p.TsNs-p.StartNs <= freshNs:
		d = whole
		d.from = p.StartNs
	default:
		// The first sight of a total that has been running a while. When
		// it built up is unknown, so it only sets the level to count from.
	}

	cp := *p
	st.last[p.Series] = &cp
	if stream == nil {
		st.streams[key] = &streamSeen{cur: p.TsNs}
	} else if p.TsNs > stream.cur {
		stream.prev, stream.cur = stream.cur, p.TsNs
	}
	return d
}

// restarted reports whether a running total began again between prev and p.
// A total that fell did; so did one whose start is after the last point. A
// changed start time alone is not trusted: some exporters move it around.
func restarted(prev, p *MetricPoint) bool {
	if p.StartNs != prev.StartNs && p.StartNs > prev.TsNs {
		return true
	}
	if p.Type == MetricSum {
		return p.Value < prev.Value
	}
	return p.Count < prev.Count
}

// bucketIncrease subtracts prev's bucket counts from p's.
func bucketIncrease(prev, p *MetricPoint) []int64 {
	if len(p.Counts) == 0 {
		return nil
	}
	old := prev.Counts
	if !slices.Equal(prev.Bounds, p.Bounds) {
		old = rebucket(prev.Bounds, prev.Counts, p.Bounds)
	}
	out := make([]int64, len(p.Counts))
	for i := range out {
		out[i] = p.Counts[i]
		if i < len(old) {
			out[i] = max(0, p.Counts[i]-old[i])
		}
	}
	return out
}

// rebucket moves counts from one set of bounds to another: each old bucket
// goes to the new one that holds its upper bound. That is exact when the new
// buckets are the old ones merged, which is how an exponential histogram
// changes scale.
func rebucket(oldBounds []float64, oldCounts []int64, bounds []float64) []int64 {
	out := make([]int64, len(bounds)+1)
	for j, c := range oldCounts {
		i := len(bounds)
		if j < len(oldBounds) {
			i = sort.SearchFloat64s(bounds, oldBounds[j])
		}
		out[i] += c
	}
	return out
}

// MetricInfo describes one metric name.
type MetricInfo struct {
	Name        string `json:"name"`
	Unit        string `json:"unit"`
	Description string `json:"description"`
	Type        string `json:"type"`
	// Temporality is "cumulative" or "delta", and empty for a gauge.
	Temporality string `json:"temporality"`
	Monotonic   bool   `json:"monotonic"`
	// Kind says how the metric's series reads: "gauge" (a level), "rate"
	// (an increase per bucket) or "histogram".
	Kind     string   `json:"kind"`
	Services []string `json:"services"`
	Series   int64    `json:"series"`
	Points   int64    `json:"points"`
	// LastValue is taken over each series' newest point: a gauge's average
	// (an up-down counter's total), a counter's total, a histogram's mean.
	LastValue float64 `json:"last_value"`
	LastMs    float64 `json:"last_ms"`
}

// metricKind picks how a metric is charted. A sum that can go down (an
// up-down counter, such as connections in use) is a level like a gauge.
func metricKind(typ string, cumulative, monotonic bool) string {
	switch typ {
	case MetricHistogram, MetricSummary:
		return "histogram"
	case MetricSum:
		if monotonic || !cumulative {
			return "rate"
		}
	}
	return "gauge"
}

// additive reports whether a level's series add up to a meaningful total.
// An up-down counter's do (idle plus used connections); a gauge's needn't.
func additive(typ string) bool { return typ == MetricSum }

func temporality(typ string, cumulative bool) string {
	switch {
	case typ == MetricGauge:
		return ""
	case cumulative:
		return "cumulative"
	}
	return "delta"
}

// Metrics lists the metric names with points in the filter's window, by name.
func (s *Store) Metrics(ctx context.Context, f Filter) ([]MetricInfo, error) {
	// A series that stopped reporting a while before the newest one is left
	// out of the last value.
	rows, err := s.db.QueryContext(ctx, `
WITH s AS (
	SELECT name, series, any_value(service) AS service, max(ts_ns) AS ts, count(*) AS n,
		arg_max(unit, ts_ns) AS unit, arg_max(description, ts_ns) AS description,
		arg_max(type, ts_ns) AS type, arg_max(cumulative, ts_ns) AS cumulative,
		arg_max(monotonic, ts_ns) AS monotonic,
		arg_max(value, ts_ns) AS v, arg_max(count, ts_ns) AS c, arg_max(sum, ts_ns) AS sm
	FROM metric_points
	WHERE ts_ns >= ? AND (? = '' OR service = ?) AND (? = '' OR name ILIKE ?)
	GROUP BY name, series
), f AS (
	SELECT *, ts >= max(ts) OVER (PARTITION BY name) - ? AS fresh FROM s
)
SELECT name, arg_max(unit, ts), arg_max(description, ts), arg_max(type, ts),
	arg_max(cumulative, ts), arg_max(monotonic, ts),
	string_agg(DISTINCT service, chr(10) ORDER BY service),
	count(*), CAST(sum(n) AS BIGINT), max(ts) / 1e6,
	coalesce(avg(v) FILTER (WHERE fresh), 0), coalesce(sum(v) FILTER (WHERE fresh), 0),
	coalesce(CAST(sum(c) FILTER (WHERE fresh) AS DOUBLE), 0), coalesce(sum(sm) FILTER (WHERE fresh), 0)
FROM f GROUP BY name ORDER BY name LIMIT ?`,
		nsFromMs(f.SinceMs), f.Service, f.Service, f.Search, like(f.Search), staleNs, f.limit(500))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MetricInfo{}
	for rows.Next() {
		var m MetricInfo
		var cumulative bool
		var services string
		var avg, total, count, sum float64
		if err := rows.Scan(&m.Name, &m.Unit, &m.Description, &m.Type, &cumulative, &m.Monotonic,
			&services, &m.Series, &m.Points, &m.LastMs, &avg, &total, &count, &sum); err != nil {
			return nil, err
		}
		m.Services = strings.Split(services, "\n")
		m.Temporality = temporality(m.Type, cumulative)
		m.Kind = metricKind(m.Type, cumulative, m.Monotonic)
		switch {
		case m.Kind == "histogram":
			if count > 0 {
				m.LastValue = sum / count
			}
		case m.Type == MetricGauge:
			m.LastValue = avg
		default:
			m.LastValue = total
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MetricSeries is one metric over time, as aligned series: T holds each
// bucket's start and every group has one value per bucket. A value is null
// where the metric said nothing.
type MetricSeries struct {
	Name        string    `json:"name"`
	Unit        string    `json:"unit"`
	Description string    `json:"description"`
	Type        string    `json:"type"`
	Temporality string    `json:"temporality"`
	Monotonic   bool      `json:"monotonic"`
	Kind        string    `json:"kind"`
	BucketMs    float64   `json:"bucket_ms"`
	T           []float64 `json:"t"`
	// By is what the groups are split on: "" (one group, everything),
	// "service", or an attribute key. Keys are the attribute keys to pick from.
	By     string        `json:"by"`
	Keys   []string      `json:"keys"`
	Groups []MetricGroup `json:"groups"`
	// More counts the groups left out for being the smallest.
	More int `json:"more"`
}

// MetricGroup is one line of a metric. Value is, by the metric's kind: a
// gauge's average, a counter's increase in the bucket, or how many
// observations a histogram took in the bucket.
type MetricGroup struct {
	Label string     `json:"label"`
	Value []*float64 `json:"value"`
	// Histograms only: the mean, and the median and 95th percentile as
	// estimated from the bucket bounds.
	Avg   []*float64  `json:"avg,omitempty"`
	P50   []*float64  `json:"p50,omitempty"`
	P95   []*float64  `json:"p95,omitempty"`
	Stats MetricStats `json:"stats"`
}

// MetricStats sums a group up over the whole window.
type MetricStats struct {
	// Total is a counter's increase or a histogram's observations.
	Total float64 `json:"total"`
	// Last is a gauge's newest value.
	Last *float64 `json:"last"`
	Avg  *float64 `json:"avg"`
	P50  *float64 `json:"p50"`
	P95  *float64 `json:"p95"`
}

// MetricSeries buckets one metric from sinceMs to untilMs into n buckets,
// split by `by` into at most maxGroups groups.
func (s *Store) MetricSeries(ctx context.Context, name string, sinceMs, untilMs float64, n int, service, by string, maxGroups int) (*MetricSeries, error) {
	if n < 1 {
		n = 1
	}
	if maxGroups < 1 {
		maxGroups = 6
	}
	w := window{since: nsFromMs(sinceMs), n: n}
	until := nsFromMs(untilMs)
	w.bucket = (until - w.since) / int64(n)
	if w.bucket < 1 {
		w.bucket = 1
	}
	out := &MetricSeries{
		Name: name, BucketMs: float64(w.bucket) / 1e6, T: make([]float64, n),
		By: by, Keys: []string{}, Groups: []MetricGroup{},
	}
	for i := range out.T {
		out.T[i] = float64(w.since+int64(i)*w.bucket) / 1e6
	}

	// A name can change type between runs of an app; the newest wins, and
	// only points of that type are charted.
	var cumulative bool
	err := s.db.QueryRowContext(ctx, `
SELECT unit, description, type, cumulative, monotonic FROM metric_points
WHERE name = ? AND (? = '' OR service = ?) AND ts_ns < ?
ORDER BY ts_ns DESC LIMIT 1`, name, service, service, until).
		Scan(&out.Unit, &out.Description, &out.Type, &cumulative, &out.Monotonic)
	if err == sql.ErrNoRows {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	out.Temporality = temporality(out.Type, cumulative)
	out.Kind = metricKind(out.Type, cumulative, out.Monotonic)

	q := metricQuery{
		ctx: ctx, db: s.db, w: w, until: until,
		where: "name = ? AND type = ? AND (? = '' OR service = ?)",
		args:  []any{name, out.Type, service, service},
	}
	q.group, q.groupArgs = groupExpr(by)

	if out.Keys, err = q.keys(); err != nil {
		return nil, err
	}
	var groups []MetricGroup
	switch out.Kind {
	case "gauge":
		groups, err = q.levels(additive(out.Type))
	case "rate":
		groups, err = q.rates()
	default:
		groups, err = q.histograms()
	}
	if err != nil {
		return nil, err
	}
	// The biggest groups first: a gauge by where it stands now, the others
	// by how much happened.
	size := func(g MetricGroup) float64 {
		if out.Kind == "gauge" && g.Stats.Last != nil {
			return *g.Stats.Last
		}
		return g.Stats.Total
	}
	sort.SliceStable(groups, func(i, j int) bool {
		if a, b := size(groups[i]), size(groups[j]); a != b {
			return a > b
		}
		return groups[i].Label < groups[j].Label
	})
	if len(groups) > maxGroups {
		out.More = len(groups) - maxGroups
		groups = groups[:maxGroups]
	}
	out.Groups = groups
	return out, nil
}

// groupExpr is the SQL that names a point's group.
func groupExpr(by string) (string, []any) {
	switch by {
	case "":
		return "''", nil
	case "service":
		return "service", nil
	}
	// A JSON path with the key quoted, since attribute keys have dots.
	key := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(by)
	return "coalesce(json_extract_string(attrs, ?), '')", []any{`$."` + key + `"`}
}

// window is a chart's time axis: n buckets of bucket nanoseconds from since.
type window struct {
	since, bucket int64
	n             int
}

// spread adds v to out, shared over the buckets that from..to overlaps in
// proportion to the overlap. A counter's increase happened somewhere in the
// stretch since the last export, so this draws a steady rate where putting
// it all at the export time would draw a spike every interval. It returns
// the first and last bucket touched, or -1, -1.
func (w window) spread(out []float64, from, to int64, v float64) (int, int) {
	end := w.since + int64(w.n)*w.bucket
	if to <= from {
		// No stretch is known: the point's own bucket takes it all.
		if to < w.since || to >= end {
			return -1, -1
		}
		i := int((to - w.since) / w.bucket)
		out[i] += v
		return i, i
	}
	if to <= w.since || from >= end {
		return -1, -1
	}
	first := int((max(from, w.since) - w.since) / w.bucket)
	last := int((min(to, end) - 1 - w.since) / w.bucket)
	for i := first; i <= last; i++ {
		lo := w.since + int64(i)*w.bucket
		overlap := min(to, lo+w.bucket) - max(from, lo)
		out[i] += v * float64(overlap) / float64(to-from)
	}
	return first, last
}

// metricQuery is one metric's points in one window, to be read by kind.
type metricQuery struct {
	ctx       context.Context
	db        *sql.DB
	w         window
	until     int64
	where     string // which points, without the time range
	args      []any
	group     string // SQL for a point's group label
	groupArgs []any
}

// keys lists the attribute keys the metric's points carry.
func (q metricQuery) keys() ([]string, error) {
	args := append(slices.Clone(q.args), q.w.since-staleNs, q.until)
	rows, err := q.db.QueryContext(q.ctx, `
SELECT DISTINCT unnest(json_keys(attrs)) AS k FROM (
	SELECT DISTINCT attrs FROM metric_points WHERE `+q.where+` AND ts_ns >= ? AND ts_ns < ?
) ORDER BY k`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// levels reads a gauge: each series' average per bucket, with its last
// value carried over buckets it skipped, then the series of a group
// averaged (or added up, for an up-down counter).
func (q metricQuery) levels(add bool) ([]MetricGroup, error) {
	w := q.w
	args := slices.Clone(q.groupArgs)
	args = append(args, w.since, w.since, w.bucket)
	args = append(args, q.args...)
	// Points from just before the window give its first buckets a value.
	args = append(args, w.since-staleNs, q.until)
	rows, err := q.db.QueryContext(q.ctx, `
SELECT g, series, b, avg(value), arg_max(value, ts_ns), max(ts_ns) FROM (
	SELECT `+q.group+` AS g, series, value, ts_ns,
		CASE WHEN ts_ns < ? THEN -1 ELSE (ts_ns - ?) // ? END AS b
	FROM metric_points WHERE `+q.where+` AND ts_ns >= ? AND ts_ns < ?
) GROUP BY g, series, b ORDER BY g, series, b`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type acc struct {
		sum []float64
		n   []int
		// The series' newest values themselves, for "now": a bucket's
		// average would blur the last two exports together.
		last  float64
		lastN int
	}
	accs := map[string]*acc{}
	var order []string
	var (
		cur      *acc
		curG     string
		curS     int64
		started  bool
		next     int // the first bucket this series has not filled
		carry    float64
		carryTs  int64
		hasCarry bool
	)
	// fill carries the series' last value up to bucket `to`, while fresh.
	fill := func(to int) {
		for ; next < to; next++ {
			if !hasCarry || w.since+int64(next)*w.bucket-carryTs > staleNs {
				continue
			}
			cur.sum[next] += carry
			cur.n[next]++
		}
	}
	// finish ends a series: its last value runs on to the end of the chart.
	finish := func() {
		fill(w.n)
		if hasCarry && q.until-carryTs <= staleNs {
			cur.last += carry
			cur.lastN++
		}
	}
	for rows.Next() {
		var g string
		var series, b, ts int64
		var avg, last float64
		if err := rows.Scan(&g, &series, &b, &avg, &last, &ts); err != nil {
			return nil, err
		}
		if !started || g != curG || series != curS {
			if started {
				finish()
			}
			started, curG, curS, next, hasCarry = true, g, series, 0, false
			if cur = accs[g]; cur == nil {
				cur = &acc{sum: make([]float64, w.n), n: make([]int, w.n)}
				accs[g] = cur
				order = append(order, g)
			}
		}
		if b >= 0 && b < int64(w.n) {
			fill(int(b))
			cur.sum[b] += avg
			cur.n[b]++
			next = int(b) + 1
		}
		carry, carryTs, hasCarry = last, ts, true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if started {
		finish()
	}

	out := make([]MetricGroup, 0, len(order))
	for _, g := range order {
		a := accs[g]
		mg := MetricGroup{Label: g, Value: make([]*float64, w.n)}
		for i := range a.sum {
			if a.n[i] == 0 {
				continue
			}
			v := a.sum[i]
			if !add {
				v /= float64(a.n[i])
			}
			mg.Value[i] = &v
			mg.Stats.Last = &v
		}
		if a.lastN > 0 {
			v := a.last
			if !add {
				v /= float64(a.lastN)
			}
			mg.Stats.Last = &v
		}
		if mg.Stats.Last != nil {
			out = append(out, mg)
		}
	}
	return out, nil
}

// rates reads a counter: how much it rose in each bucket.
func (q metricQuery) rates() ([]MetricGroup, error) {
	rows, err := q.increases("sum(d_value)", "CAST(0 AS DOUBLE)")
	if err != nil {
		return nil, err
	}
	out := make([]MetricGroup, 0, len(rows.order))
	for _, g := range rows.order {
		r := rows.by[g]
		mg := MetricGroup{Label: g, Value: r.covered(q.w, r.a)}
		for _, v := range r.a {
			mg.Stats.Total += v
		}
		out = append(out, mg)
	}
	return out, nil
}

// histograms reads a histogram: observations per bucket, their mean, and
// the percentiles its bucket counts allow.
func (q metricQuery) histograms() ([]MetricGroup, error) {
	w := q.w
	totals, err := q.increases("CAST(sum(d_count) AS DOUBLE)", "sum(d_sum)")
	if err != nil {
		return nil, err
	}
	dists, err := q.distributions()
	if err != nil {
		return nil, err
	}
	out := make([]MetricGroup, 0, len(totals.order))
	for _, g := range totals.order {
		r := totals.by[g]
		mg := MetricGroup{Label: g, Value: r.covered(q.w, r.a), Avg: make([]*float64, w.n)}
		var count, sum float64
		for i := range r.a {
			count += r.a[i]
			sum += r.b[i]
			if r.a[i] > 0 {
				v := r.b[i] / r.a[i]
				mg.Avg[i] = &v
			}
		}
		mg.Stats.Total = count
		if count > 0 {
			v := sum / count
			mg.Stats.Avg = &v
		}
		// A summary, or a histogram sent without buckets, has no
		// distribution to estimate from.
		if d := dists[g]; d != nil && len(d.les) > 1 {
			mg.P50, mg.P95 = make([]*float64, w.n), make([]*float64, w.n)
			all := make([]float64, len(d.les))
			for i, row := range d.counts {
				if row == nil {
					continue
				}
				for k, c := range row {
					all[k] += c
				}
				mg.P50[i] = quantile(0.5, d.les, row)
				mg.P95[i] = quantile(0.95, d.les, row)
			}
			mg.Stats.P50, mg.Stats.P95 = quantile(0.5, d.les, all), quantile(0.95, d.les, all)
		}
		out = append(out, mg)
	}
	return out, nil
}

// spreadRows is two per-bucket totals for each group, plus which buckets
// any point's stretch reached.
type spreadRows struct {
	order []string
	by    map[string]*spreadRow
}

type spreadRow struct {
	a, b        []float64
	first, last int // the buckets some point covered, -1 if none
	// from and to are where the points' stretches begin and end. instant
	// says some point had no stretch, so its own bucket took all of it.
	from, to int64
	instant  bool
}

// covered returns v with null outside the stretch the data covers: before
// the first export and after the newest nothing is known, which is not the
// same as nothing having happened.
//
// The buckets at the two ends are usually covered in part, and would draw
// as a dip. One that is half known or more is scaled up to a whole bucket;
// with less to go on it is left out.
func (r *spreadRow) covered(w window, v []float64) []*float64 {
	out := make([]*float64, len(v))
	if r.first < 0 {
		return out
	}
	for i := r.first; i <= r.last; i++ {
		x := v[i]
		out[i] = &x
	}
	if r.instant {
		return out
	}
	lo := w.since + int64(r.first)*w.bucket
	hi := w.since + int64(r.last+1)*w.bucket
	edge := func(i int, known int64) {
		switch {
		case known >= w.bucket:
		case known*2 >= w.bucket:
			*out[i] *= float64(w.bucket) / float64(known)
		default:
			out[i] = nil
		}
	}
	if r.first == r.last {
		edge(r.first, min(r.to, hi)-max(r.from, lo))
		return out
	}
	edge(r.first, lo+w.bucket-max(r.from, lo))
	edge(r.last, min(r.to, hi)-(hi-w.bucket))
	return out
}

// increases sums two aggregates of the points' increases per group and
// spreads them over the buckets.
func (q metricQuery) increases(a, b string) (*spreadRows, error) {
	args := slices.Clone(q.groupArgs)
	args = append(args, q.args...)
	args = append(args, q.w.since, q.until)
	rows, err := q.db.QueryContext(q.ctx, `
SELECT g, ts_ns, from_ns, coalesce(`+a+`, 0), coalesce(`+b+`, 0) FROM (
	SELECT `+q.group+` AS g, ts_ns, from_ns, d_value, d_count, d_sum
	FROM metric_points WHERE `+q.where+` AND ts_ns >= ? AND from_ns < ?
) GROUP BY g, ts_ns, from_ns`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &spreadRows{by: map[string]*spreadRow{}}
	for rows.Next() {
		var g string
		var ts, from int64
		var va, vb float64
		if err := rows.Scan(&g, &ts, &from, &va, &vb); err != nil {
			return nil, err
		}
		r := out.by[g]
		if r == nil {
			r = &spreadRow{a: make([]float64, q.w.n), b: make([]float64, q.w.n), first: -1, last: -1}
			out.by[g] = r
			out.order = append(out.order, g)
		}
		first, last := q.w.spread(r.a, from, ts, va)
		q.w.spread(r.b, from, ts, vb)
		if first < 0 {
			continue
		}
		if r.first < 0 || first < r.first {
			r.first = first
		}
		r.last = max(r.last, last)
		if r.to == 0 || from < r.from {
			r.from = from
		}
		r.to = max(r.to, ts)
		// A point that adds nothing (the first sight of a running total)
		// needs no stretch to share it over.
		r.instant = r.instant || (from >= ts && (va != 0 || vb != 0))
	}
	return out, rows.Err()
}

// distribution is a group's histogram buckets over time: counts[i][k] is
// how many observations in time bucket i fell at or under les[k]. The last
// of les is +Inf. A time bucket with no observations has a nil row.
type distribution struct {
	les    []float64
	counts [][]float64
}

func (q metricQuery) distributions() (map[string]*distribution, error) {
	inner := `SELECT ` + q.group + ` AS g, ts_ns, from_ns, bounds, d_counts
	FROM metric_points WHERE ` + q.where + ` AND ts_ns >= ? AND from_ns < ? AND len(d_counts) = len(bounds) + 1`
	args := slices.Clone(q.groupArgs)
	args = append(args, q.args...)
	args = append(args, q.w.since, q.until)

	// Every bound, the empty buckets' too: a bucket's lower edge is the
	// bound before it. Series with different bounds are read on the union.
	out := map[string]*distribution{}
	brows, err := q.db.QueryContext(q.ctx, `SELECT DISTINCT g, unnest(bounds) AS le FROM (`+inner+`) ORDER BY g, le`, args...)
	if err != nil {
		return nil, err
	}
	defer brows.Close()
	for brows.Next() {
		var g string
		var le float64
		if err := brows.Scan(&g, &le); err != nil {
			return nil, err
		}
		d := out[g]
		if d == nil {
			d = &distribution{}
			out[g] = d
		}
		d.les = append(d.les, le)
	}
	if err := brows.Err(); err != nil {
		return nil, err
	}

	rows, err := q.db.QueryContext(q.ctx, `
SELECT g, ts_ns, from_ns, le, CAST(sum(c) AS DOUBLE) FROM (
	SELECT g, ts_ns, from_ns, unnest(list_append(bounds, 'inf'::DOUBLE)) AS le, unnest(d_counts) AS c
	FROM (`+inner+`)
) WHERE c > 0 GROUP BY g, ts_ns, from_ns, le`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	scratch := make([]float64, q.w.n)
	for rows.Next() {
		var g string
		var ts, from int64
		var le, c float64
		if err := rows.Scan(&g, &ts, &from, &le, &c); err != nil {
			return nil, err
		}
		d := out[g]
		if d == nil {
			d = &distribution{}
			out[g] = d
		}
		if d.counts == nil {
			d.les = append(d.les, math.Inf(1))
			d.counts = make([][]float64, q.w.n)
		}
		k := sort.SearchFloat64s(d.les, le)
		clear(scratch)
		first, last := q.w.spread(scratch, from, ts, c)
		for i := first; i >= 0 && i <= last; i++ {
			if d.counts[i] == nil {
				d.counts[i] = make([]float64, len(d.les))
			}
			d.counts[i][k] += scratch[i]
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, d := range out {
		if d.counts == nil {
			d.les = append(d.les, math.Inf(1))
			d.counts = make([][]float64, q.w.n)
		}
	}
	return out, nil
}

// quantile estimates a quantile from bucket counts, the way Prometheus
// does: find the bucket the rank falls in and assume its observations are
// spread evenly between its edges. les are the buckets' upper bounds in
// order, ending with +Inf. It returns nil when there is nothing to go on.
func quantile(q float64, les, counts []float64) *float64 {
	var total float64
	for _, c := range counts {
		total += c
	}
	if total <= 0 {
		return nil
	}
	rank := q * total
	var seen float64
	for k, c := range counts {
		if c <= 0 || seen+c < rank {
			seen += c
			continue
		}
		var v float64
		switch {
		case math.IsInf(les[k], 1):
			// Above the last bound: all that is known is "more than this".
			if k == 0 {
				return nil
			}
			v = les[k-1]
		default:
			// The first bucket starts at zero when its bound is positive,
			// which is right for durations and sizes.
			lower := min(les[k], 0)
			if k > 0 {
				lower = les[k-1]
			}
			v = lower + (les[k]-lower)*(rank-seen)/c
		}
		return &v
	}
	return nil
}
