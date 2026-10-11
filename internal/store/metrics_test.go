package store

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	st, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// base is a fixed "now" for the tests: metric times count seconds from it.
var base = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func at(sec float64) int64 { return base.Add(time.Duration(sec * float64(time.Second))).UnixNano() }

// counter is a point of a cumulative, monotonic sum.
func counter(series int64, startSec, sec, value float64) MetricPoint {
	p := MetricPoint{
		Name: "jobs.processed", Unit: "{job}", Type: MetricSum, Cumulative: true, Monotonic: true,
		Service: "acme-worker", Series: series, Attrs: `{}`, TsNs: at(sec), Value: value,
	}
	if !math.IsNaN(startSec) {
		p.StartNs = at(startSec)
	}
	return p
}

// read charts a metric over the first `secs` seconds after base.
func read(t *testing.T, st *Store, name string, secs float64, buckets int, by string) *MetricSeries {
	t.Helper()
	out, err := st.MetricSeries(context.Background(), name,
		float64(at(0))/1e6, float64(at(secs))/1e6, buckets, "", by, 0)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func values(vs []*float64) []float64 {
	out := make([]float64, len(vs))
	for i, v := range vs {
		out[i] = math.NaN()
		if v != nil {
			out[i] = *v
		}
	}
	return out
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// A cumulative counter is charted as its increase per bucket, and a restart
// of the app (the total falls back towards zero) must read as a small
// increase, not as a drop or as the whole new total on top.
func TestCounterRateAcrossReset(t *testing.T) {
	st := openTest(t)
	err := st.AddMetrics([]MetricPoint{
		counter(1, 0, 10, 10), // started at 0: the first ten seconds made 10
		counter(1, 0, 20, 25),
		counter(1, 0, 30, 40),
		// The worker restarts at 32 and counts from zero again.
		counter(1, 32, 40, 5),
		counter(1, 32, 50, 12),
	})
	if err != nil {
		t.Fatal(err)
	}
	s := read(t, st, "jobs.processed", 60, 6, "")
	if s.Kind != "rate" || s.Temporality != "cumulative" || len(s.Groups) != 1 {
		t.Fatalf("kind %q, temporality %q, %d groups", s.Kind, s.Temporality, len(s.Groups))
	}
	got := values(s.Groups[0].Value)
	// 10, +15, +15, then 5 since the restart (spread over 32..40), then +7.
	// Nothing is known after the last export at 50.
	want := []float64{10, 15, 15, 5, 7, math.NaN()}
	for i := range want {
		if !near(got[i], want[i]) && !(math.IsNaN(got[i]) && math.IsNaN(want[i])) {
			t.Fatalf("increase per bucket = %v, want %v", got, want)
		}
	}
	if total := s.Groups[0].Stats.Total; !near(total, 52) {
		t.Errorf("total = %v, want 52", total)
	}
}

// With no start times, a fall in the total is the only sign of a restart.
// The first total seen sets the level: when it built up is unknown.
func TestCounterResetWithoutStartTime(t *testing.T) {
	st := openTest(t)
	nan := math.NaN()
	err := st.AddMetrics([]MetricPoint{
		counter(1, nan, 10, 100),
		counter(1, nan, 20, 130),
		counter(1, nan, 30, 20),
		counter(1, nan, 40, 50),
	})
	if err != nil {
		t.Fatal(err)
	}
	s := read(t, st, "jobs.processed", 40, 4, "")
	got := values(s.Groups[0].Value)
	if !math.IsNaN(got[0]) || !near(got[1], 30) || !near(got[2], 20) || !near(got[3], 30) {
		t.Fatalf("increase per bucket = %v, want [NaN 30 20 30]", got)
	}
	if total := s.Groups[0].Stats.Total; !near(total, 80) {
		t.Errorf("total = %v, want 80: the first 100 is not an increase", total)
	}
}

// A series that appears in a stream already being watched (a job's first
// run) was zero until then, so its first total counts in full. One export
// arriving twice counts once.
func TestNewSeriesInWatchedStream(t *testing.T) {
	st := openTest(t)
	old := -3600.0 // the process began an hour ago
	for _, batch := range [][]MetricPoint{
		{counter(1, old, 10, 500)},
		{counter(1, old, 20, 510), counter(2, old, 20, 3)},
		{counter(1, old, 20, 510), counter(2, old, 20, 3)},
		{counter(1, old, 30, 510), counter(2, old, 30, 4)},
	} {
		if err := st.AddMetrics(batch); err != nil {
			t.Fatal(err)
		}
	}
	s := read(t, st, "jobs.processed", 40, 4, "")
	if total := s.Groups[0].Stats.Total; !near(total, 10+3+1) {
		t.Errorf("total = %v, want 14 (10 more of the old series, 4 of the new)", total)
	}
}

// After the daemon restarts, running totals are measured from the points
// already in the database.
func TestCounterStateSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leadline.duckdb")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddMetrics([]MetricPoint{counter(1, -3600, 10, 1000), counter(1, -3600, 20, 1004)}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if st, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.AddMetrics([]MetricPoint{counter(1, -3600, 30, 1010), counter(2, -3600, 30, 2)}); err != nil {
		t.Fatal(err)
	}
	s := read(t, st, "jobs.processed", 40, 4, "")
	if total := s.Groups[0].Stats.Total; !near(total, 4+6+2) {
		t.Errorf("total = %v, want 12", total)
	}
}

// The first and last buckets with data are seldom covered whole. Half a
// bucket or more is scaled to a whole one, so the chart doesn't end on a
// dip; less than that is too little to draw.
func TestPartBucketsAtTheEnds(t *testing.T) {
	st := openTest(t)
	err := st.AddMetrics([]MetricPoint{
		counter(1, -3600, 4, 100), // sets the level: the total is an hour old
		counter(1, -3600, 14, 110),
		counter(1, -3600, 24, 120),
		counter(1, -3600, 34, 130),
	})
	if err != nil {
		t.Fatal(err)
	}
	s := read(t, st, "jobs.processed", 40, 4, "")
	got := values(s.Groups[0].Value)
	// 4..10 holds 6 of the first 10 and is scaled up; 30..34 holds 4 of the
	// last and is left out.
	if !near(got[0], 10) || !near(got[1], 10) || !near(got[2], 10) || !math.IsNaN(got[3]) {
		t.Errorf("increase per bucket = %v, want [10 10 10 NaN]", got)
	}
	if total := s.Groups[0].Stats.Total; !near(total, 30) {
		t.Errorf("total = %v, want 30", total)
	}
}

// A delta counter already says what each interval added.
func TestDeltaCounter(t *testing.T) {
	st := openTest(t)
	var points []MetricPoint
	for i, v := range []float64{4, 0, 6} {
		p := counter(1, float64(i*10), float64(i*10+10), v)
		p.Cumulative = false
		points = append(points, p)
	}
	if err := st.AddMetrics(points); err != nil {
		t.Fatal(err)
	}
	s := read(t, st, "jobs.processed", 30, 3, "")
	got := values(s.Groups[0].Value)
	if s.Temporality != "delta" || !near(got[0], 4) || !near(got[1], 0) || !near(got[2], 6) {
		t.Errorf("temporality %q, values %v", s.Temporality, got)
	}
}

// An increase is shared over the stretch since the export before, so a
// chart with buckets narrower than the export interval shows a steady rate.
func TestSpread(t *testing.T) {
	w := window{since: 100, bucket: 10, n: 5}
	out := make([]float64, 5)
	first, last := w.spread(out, 95, 125, 30) // 5 fall before the window, then 10, 10, 5
	if first != 0 || last != 2 || !near(out[0], 10) || !near(out[1], 10) || !near(out[2], 5) {
		t.Errorf("spread = %v over %d..%d", out, first, last)
	}
	clear(out)
	if first, last := w.spread(out, 120, 120, 7); first != 2 || last != 2 || out[2] != 7 {
		t.Errorf("a point with no stretch = %v over %d..%d", out, first, last)
	}
	if first, _ := w.spread(out, 150, 160, 1); first != -1 {
		t.Error("a stretch after the window was counted")
	}
}

func TestGaugeCarriesLastValue(t *testing.T) {
	st := openTest(t)
	gauge := func(series int64, sec, v float64, state string) MetricPoint {
		return MetricPoint{
			Name: "db.client.connections.usage", Type: MetricGauge, Service: "acme-api",
			Series: series, Attrs: `{"state":"` + state + `"}`, TsNs: at(sec), Value: v,
		}
	}
	err := st.AddMetrics([]MetricPoint{
		gauge(1, -20, 4, "used"), // before the window: where the chart starts
		gauge(1, 25, 8, "used"),
		gauge(2, 25, 2, "idle"),
		gauge(1, 55, 6, "used"),
	})
	if err != nil {
		t.Fatal(err)
	}
	s := read(t, st, "db.client.connections.usage", 60, 6, "state")
	if s.Kind != "gauge" || len(s.Groups) != 2 || len(s.Keys) != 1 || s.Keys[0] != "state" {
		t.Fatalf("kind %q, %d groups, keys %v", s.Kind, len(s.Groups), s.Keys)
	}
	if s.Groups[0].Label != "used" || s.Groups[1].Label != "idle" {
		t.Fatalf("groups = %q, %q, want the larger first", s.Groups[0].Label, s.Groups[1].Label)
	}
	used, idle := values(s.Groups[0].Value), values(s.Groups[1].Value)
	for i, want := range []float64{4, 4, 8, 8, 8, 6} {
		if !near(used[i], want) {
			t.Fatalf("used = %v", used)
		}
	}
	if !math.IsNaN(idle[0]) || !math.IsNaN(idle[1]) || !near(idle[2], 2) || !near(idle[5], 2) {
		t.Errorf("idle = %v, want nothing before its first point and 2 after", idle)
	}

	// "Now" is the newest point itself, not its bucket's average.
	if err := st.AddMetrics([]MetricPoint{gauge(1, 58, 9, "used")}); err != nil {
		t.Fatal(err)
	}
	s = read(t, st, "db.client.connections.usage", 60, 6, "state")
	if last := s.Groups[0].Stats.Last; last == nil || *last != 9 || !near(*s.Groups[0].Value[5], 7.5) {
		t.Errorf("used now = %v in a bucket averaging %v, want 9 and 7.5", last, s.Groups[0].Value[5])
	}

	// Together, a gauge's series are averaged.
	all := values(read(t, st, "db.client.connections.usage", 60, 6, "").Groups[0].Value)
	if !near(all[0], 4) || !near(all[2], 5) || !near(all[5], 4.75) {
		t.Errorf("average = %v", all)
	}
}

// An up-down counter is a level too, but its series add up.
func TestUpDownCounterAddsSeries(t *testing.T) {
	st := openTest(t)
	point := func(series int64, v float64) MetricPoint {
		return MetricPoint{
			Name: "db.client.connections.usage", Type: MetricSum, Cumulative: true, Service: "acme-api",
			Series: series, Attrs: `{}`, TsNs: at(5), Value: v,
		}
	}
	if err := st.AddMetrics([]MetricPoint{point(1, 7), point(2, 3)}); err != nil {
		t.Fatal(err)
	}
	s := read(t, st, "db.client.connections.usage", 10, 1, "")
	if got := values(s.Groups[0].Value); s.Kind != "gauge" || !near(got[0], 10) {
		t.Errorf("kind %q, value %v, want a gauge of 10", s.Kind, got)
	}
}

func TestHistogram(t *testing.T) {
	st := openTest(t)
	bounds := []float64{0.1, 0.5, 1}
	hist := func(sec float64, count int64, sum float64, counts ...int64) MetricPoint {
		return MetricPoint{
			Name: "http.server.request.duration", Unit: "s", Type: MetricHistogram, Cumulative: true,
			Service: "acme-api", Series: 1, Attrs: `{}`, TsNs: at(sec), StartNs: at(0),
			Count: count, Sum: sum, Bounds: bounds, Counts: counts,
		}
	}
	err := st.AddMetrics([]MetricPoint{
		hist(10, 10, 0.5, 10, 0, 0, 0),    // ten fast requests
		hist(20, 20, 8.5, 10, 0, 10, 0),   // then ten between 0.5s and 1s
		hist(30, 20, 8.5, 10, 0, 10, 0),   // then none
		hist(40, 24, 18.5, 10, 0, 10, 4),  // then four over a second
		hist(50, 25, 18.52, 11, 0, 10, 4), // the next process would start here
	})
	if err != nil {
		t.Fatal(err)
	}
	s := read(t, st, "http.server.request.duration", 50, 5, "")
	if s.Kind != "histogram" || len(s.Groups) != 1 {
		t.Fatalf("kind %q, %d groups", s.Kind, len(s.Groups))
	}
	g := s.Groups[0]
	count, p50, p95, avg := values(g.Value), values(g.P50), values(g.P95), values(g.Avg)
	for i, want := range []float64{10, 10, 0, 4, 1} {
		if !near(count[i], want) {
			t.Fatalf("observations = %v", count)
		}
	}
	// The first bucket is 0..0.1, so its median is 0.05; the second export's
	// ten are spread over 0.5..1.
	if !near(p50[0], 0.05) || !near(p50[1], 0.75) || !near(p95[1], 0.975) {
		t.Errorf("p50 = %v, p95 = %v", p50, p95)
	}
	if !math.IsNaN(p50[2]) || !math.IsNaN(avg[2]) {
		t.Errorf("a bucket with no requests has p50 %v and mean %v", p50[2], avg[2])
	}
	// Above the last bound all that is known is "more than a second".
	if !near(p95[3], 1) || !near(avg[1], 0.8) || !near(avg[3], 2.5) {
		t.Errorf("p95 = %v, mean = %v", p95, avg)
	}
	if g.Stats.Total != 25 || g.Stats.P50 == nil || g.Stats.P95 == nil || !near(*g.Stats.Avg, 18.52/25) {
		t.Errorf("stats = %+v", g.Stats)
	}
	// Of 25: 11 under 0.1, 10 in 0.5..1, 4 above. The 12.5th is in 0.5..1.
	if want := 0.5 + 0.5*(12.5-11)/10; !near(*g.Stats.P50, want) {
		t.Errorf("window p50 = %v, want %v", *g.Stats.P50, want)
	}

	list, err := st.Metrics(context.Background(), Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Kind != "histogram" || list[0].Unit != "s" || !near(list[0].LastValue, 18.52/25) ||
		list[0].LastMs != float64(at(50))/1e6 || len(list[0].Services) != 1 || list[0].Points != 5 {
		t.Errorf("metrics = %+v", list)
	}
}

func TestRebucket(t *testing.T) {
	// Two old buckets fold into each new one, as when an exponential
	// histogram halves its resolution.
	got := rebucket([]float64{1, 2, 4, 8}, []int64{1, 2, 3, 4, 5}, []float64{2, 8})
	if len(got) != 3 || got[0] != 3 || got[1] != 7 || got[2] != 5 {
		t.Errorf("rebucket = %v, want [3 7 5]", got)
	}
}

func TestQuantile(t *testing.T) {
	inf := math.Inf(1)
	if q := quantile(0.5, []float64{inf}, []float64{9}); q != nil {
		t.Errorf("one unbounded bucket gave %v", *q)
	}
	if q := quantile(0.5, []float64{1, inf}, []float64{0, 0}); q != nil {
		t.Errorf("no observations gave %v", *q)
	}
	if q := quantile(0.95, []float64{10, 20, inf}, []float64{50, 50, 0}); q == nil || !near(*q, 19) {
		t.Errorf("p95 = %v, want 19", q)
	}
}

// Retention takes old metric points with everything else, and a counter
// carries on across it and across a restart: the newest point of a live
// series is never among the old ones, and that is the one the next is
// measured from.
func TestCountersAcrossPruneAndReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "leadline.duckdb")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Series 1 is live; series 2 stopped long ago.
	if err := st.AddMetrics([]MetricPoint{
		counter(2, -7200, -3000, 50), counter(2, -7200, -2990, 60),
		counter(1, -3600, -100, 900), counter(1, -3600, 0, 1000), counter(1, -3600, 10, 1004),
	}); err != nil {
		t.Fatal(err)
	}
	p, err := st.Prune(ctx, time.Unix(0, at(-50)))
	if err != nil || p.Other != 3 {
		t.Fatalf("pruned %+v, %v; want the three old metric points", p, err)
	}
	if n := len(st.metrics.last); n != 1 {
		t.Errorf("%d series remembered after the prune, want only the live one", n)
	}
	// The live counter carries on from 1004, in this process...
	if err := st.AddMetrics([]MetricPoint{counter(1, -3600, 20, 1010)}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	// ...and in the next.
	if st, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.AddMetrics([]MetricPoint{counter(1, -3600, 30, 1013)}); err != nil {
		t.Fatal(err)
	}
	s := read(t, st, "jobs.processed", 40, 4, "")
	if total := s.Groups[0].Stats.Total; !near(total, 4+6+3) {
		t.Errorf("total = %v, want 13: 1000 to 1013 and nothing counted twice", total)
	}
}

// An app that sends only metrics is held to the size cap too.
func TestSizeCapWithOnlyMetrics(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "leadline.duckdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	var points []MetricPoint
	for i := 0; i < 20000; i++ {
		points = append(points, MetricPoint{Name: "queue.depth", Type: MetricGauge, Service: "acme-worker", Series: int64(i % 50),
			Attrs: fmt.Sprintf(`{"queue":"q%d","filler":%q}`, i%50, strings.Repeat("x", 200)),
			TsNs:  now.Add(-time.Duration(20000-i) * time.Second).UnixNano(), Value: float64(i)})
	}
	if err := st.AddMetrics(points); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`CHECKPOINT`); err != nil {
		t.Fatal(err)
	}
	var floor int64
	if err := st.retain(ctx, Retention{MaxBytes: 1}, &floor); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := st.DB().QueryRow(`SELECT count(*) FROM metric_points`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left == 0 || left > 2000 {
		t.Errorf("%d metric points left of 20000, want the newest few", left)
	}
}
