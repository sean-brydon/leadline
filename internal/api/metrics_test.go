package api

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"testing"
	"time"

	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/sean-brydon/leadline/internal/demo"
	"github.com/sean-brydon/leadline/internal/store"
)

// truth adds up what a cumulative metric really gained over a request: the
// last total of every series of every run of the process. A run is told by
// its start time, so this needs none of the store's reasoning about resets.
// It also reports whether the process ran more than once.
func truth(points []*metricspb.NumberDataPoint) (total float64, reset bool) {
	last := map[string]int64{}
	starts := map[uint64]bool{}
	for _, p := range points {
		last[fmt.Sprint(p.GetStartTimeUnixNano(), p.GetAttributes())] = p.GetAsInt()
		starts[p.GetStartTimeUnixNano()] = true
	}
	for _, v := range last {
		total += float64(v)
	}
	return total, len(starts) > 1
}

// seriesURL charts a metric from just before `from`, so that the first
// export's ten seconds are inside the window whole.
func seriesURL(base, name string, from, to time.Time, more string) string {
	return fmt.Sprintf("%s/api/metrics/series?name=%s&since_ms=%d&until_ms=%d&buckets=30%s",
		base, url.QueryEscape(name), from.Add(-metricLead).UnixMilli(), to.UnixMilli(), more)
}

const metricLead = 10 * time.Second

func TestMetricsIngestAndQuery(t *testing.T) {
	ts := newTestServer(t)
	now := time.Now()
	from := now.Add(-15 * time.Minute)
	req := demo.New(7).Metrics(from, now)

	body, _ := proto.Marshal(req)
	if resp := post(t, ts.URL+"/v1/metrics", "application/x-protobuf", body); resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/metrics: %s", resp.Status)
	}

	var list []store.MetricInfo
	getJSON(t, ts.URL+"/api/metrics", &list)
	byName := map[string]store.MetricInfo{}
	for _, m := range list {
		byName[m.Name] = m
	}
	for name, want := range map[string]struct{ kind, unit, temporality string }{
		"http.server.request.duration": {"histogram", "s", "cumulative"},
		"db.client.connections.usage":  {"gauge", "{connection}", "cumulative"},
		"process.runtime.memory":       {"gauge", "By", ""},
		"jobs.processed":               {"rate", "{job}", "cumulative"},
		"cache.requests":               {"rate", "{request}", "delta"},
	} {
		m, ok := byName[name]
		if !ok || m.Kind != want.kind || m.Unit != want.unit || m.Temporality != want.temporality {
			t.Errorf("%s = %+v, want %+v", name, m, want)
		}
		if m.LastMs < float64(now.Add(-10*time.Second).UnixMilli()) || m.LastMs > float64(now.UnixMilli()) {
			t.Errorf("%s was last seen at %v, want within ten seconds of now", name, m.LastMs)
		}
	}
	if got := byName["process.runtime.memory"].Services; len(got) != 3 {
		t.Errorf("memory is reported by %v, want the three services", got)
	}

	// The demo's worker restarts halfway, so its job counter falls back to
	// zero. The chart must add up to the jobs actually run.
	var jobs, cache []*metricspb.NumberDataPoint
	for _, rm := range req.ResourceMetrics {
		for _, m := range rm.ScopeMetrics[0].Metrics {
			switch m.Name {
			case "jobs.processed":
				jobs = m.GetSum().DataPoints
			case "cache.requests":
				cache = m.GetSum().DataPoints
			}
		}
	}
	wantJobs, reset := truth(jobs)
	if !reset || wantJobs == 0 {
		t.Fatalf("the demo's job counter never reset (total %v): this test needs it to", wantJobs)
	}
	var s store.MetricSeries
	getJSON(t, seriesURL(ts.URL, "jobs.processed", from, now, ""), &s)
	if len(s.T) != 30 || len(s.Groups) != 1 || len(s.Groups[0].Value) != 30 {
		t.Fatalf("series has %d buckets and %d groups", len(s.T), len(s.Groups))
	}
	if got := s.Groups[0].Stats.Total; math.Abs(got-wantJobs) > 1e-6 {
		t.Errorf("jobs processed = %v, want %v", got, wantJobs)
	}
	// About seven jobs every ten seconds; a reset taken for an increase
	// would put hundreds in one bucket.
	for i, v := range s.Groups[0].Value {
		if v != nil && (*v < 0 || *v > wantJobs/5) {
			t.Errorf("bucket %d has %v of %v jobs", i, *v, wantJobs)
		}
	}

	// Split by an attribute, the groups add up to the same.
	getJSON(t, seriesURL(ts.URL, "jobs.processed", from, now, "&by=job.name"), &s)
	var split float64
	for _, g := range s.Groups {
		split += g.Stats.Total
	}
	if len(s.Groups) != 3 || math.Abs(split-wantJobs) > 1e-6 || s.Groups[0].Label != "send-receipt" {
		t.Errorf("by job.name: %d groups adding up to %v, first %q", len(s.Groups), split, s.Groups[0].Label)
	}

	// A delta counter's points are each interval's own count.
	var wantCache float64
	for _, p := range cache {
		wantCache += float64(p.GetAsInt())
	}
	getJSON(t, seriesURL(ts.URL, "cache.requests", from, now, ""), &s)
	if got := s.Groups[0].Stats.Total; math.Abs(got-wantCache) > 1e-6 {
		t.Errorf("cache requests = %v, want %v", got, wantCache)
	}

	getJSON(t, seriesURL(ts.URL, "http.server.request.duration", from, now, "&by=http.route"), &s)
	routes := map[string]store.MetricGroup{}
	for _, g := range s.Groups {
		routes[g.Label] = g
	}
	products, search := routes["/api/products"], routes["/api/search"]
	if s.Kind != "histogram" || products.Stats.P95 == nil || search.Stats.P95 == nil {
		t.Fatalf("kind %q, groups %v", s.Kind, len(s.Groups))
	}
	// Products answer in about 12ms; the search takes a quarter of a second
	// and now and then two.
	if p := *products.Stats.P50; p < 0.005 || p > 0.025 {
		t.Errorf("products p50 = %vs", p)
	}
	if *search.Stats.P95 < 0.5 || *search.Stats.P50 > *search.Stats.P95 {
		t.Errorf("search p50 = %vs, p95 = %vs", *search.Stats.P50, *search.Stats.P95)
	}

	// The service filter narrows a metric several services report.
	getJSON(t, seriesURL(ts.URL, "process.runtime.memory", from, now, "&by=service"), &s)
	if len(s.Groups) != 3 || s.Groups[0].Stats.Last == nil {
		t.Fatalf("memory by service has %d groups", len(s.Groups))
	}
	getJSON(t, seriesURL(ts.URL, "process.runtime.memory", from, now, "&by=service&service=acme-worker"), &s)
	if len(s.Groups) != 1 || s.Groups[0].Label != "acme-worker" {
		t.Errorf("filtered to the worker: %d groups", len(s.Groups))
	}
	getJSON(t, ts.URL+"/api/metrics?service=acme-payments", &list)
	if len(list) != 3 {
		t.Errorf("payments reports %d metrics, want its duration, memory and cpu", len(list))
	}
	getJSON(t, ts.URL+"/api/metrics?q=jobs", &list)
	if len(list) != 1 || list[0].Name != "jobs.processed" {
		t.Errorf("search for jobs found %d metrics", len(list))
	}
}

// Exports in a row carry on each other's running totals: sent in pieces,
// the demo adds up to the same as sent at once.
func TestMetricsAcrossExports(t *testing.T) {
	ts := newTestServer(t)
	now := time.Now()
	from := now.Add(-10 * time.Minute)
	gen := demo.New(3)
	var want float64
	var all []*metricspb.NumberDataPoint
	for at := from; at.Before(now); at = at.Add(time.Minute) {
		req := gen.Metrics(at, at.Add(time.Minute))
		for _, rm := range req.ResourceMetrics {
			for _, m := range rm.ScopeMetrics[0].Metrics {
				if m.Name == "jobs.processed" {
					all = append(all, m.GetSum().DataPoints...)
				}
			}
		}
		// As OTLP/JSON, gzipped: what a browser or a Node SDK may send.
		raw, _ := protojson.Marshal(req)
		var zipped bytes.Buffer
		zw := gzip.NewWriter(&zipped)
		zw.Write(raw)
		zw.Close()
		hreq, _ := http.NewRequest("POST", ts.URL+"/v1/metrics", &zipped)
		hreq.Header.Set("Content-Type", "application/json")
		hreq.Header.Set("Content-Encoding", "gzip")
		resp, err := http.DefaultClient.Do(hreq)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("POST /v1/metrics: %s, %s", resp.Status, resp.Header.Get("Content-Type"))
		}
	}
	want, _ = truth(all)

	var s store.MetricSeries
	getJSON(t, seriesURL(ts.URL, "jobs.processed", from, now, ""), &s)
	if got := s.Groups[0].Stats.Total; math.Abs(got-want) > 1e-6 || want == 0 {
		t.Errorf("jobs processed = %v, want %v", got, want)
	}
}

func TestMetricSeriesNeedsAName(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/api/metrics/series")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("no name: %s, want 400", resp.Status)
	}
	// A name Leadline has never seen is an empty chart, not an error.
	var s store.MetricSeries
	getJSON(t, ts.URL+"/api/metrics/series?name=nope", &s)
	if len(s.T) != 60 || len(s.Groups) != 0 {
		t.Errorf("unknown metric: %d buckets, %d groups", len(s.T), len(s.Groups))
	}
}
