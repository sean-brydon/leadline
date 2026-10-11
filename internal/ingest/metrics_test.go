package ingest

import (
	"math"
	"testing"

	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/sean-brydon/leadline/internal/store"
)

// One of each metric type, as an SDK writes OTLP/JSON.
const metricsJSON = `{"resourceMetrics":[{
"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"acme-api"}}]},
"scopeMetrics":[{"scope":{"name":"acme"},"metrics":[
	{"name":"process.runtime.memory","unit":"By","gauge":{"dataPoints":[
		{"timeUnixNano":"1700000010000000000","asInt":"1048576"},
		{"timeUnixNano":"1700000010000000000","flags":1}]}},
	{"name":"jobs.processed","unit":"{job}","sum":{"aggregationTemporality":2,"isMonotonic":true,"dataPoints":[
		{"startTimeUnixNano":"1700000000000000000","timeUnixNano":"1700000010000000000","asDouble":7,
		 "attributes":[{"key":"job.name","value":{"stringValue":"send-receipt"}}]},
		{"startTimeUnixNano":"1700000000000000000","timeUnixNano":"1700000010000000000","asDouble":2,
		 "attributes":[{"key":"job.name","value":{"stringValue":"resize-image"}}]}]}},
	{"name":"cache.requests","sum":{"aggregationTemporality":1,"isMonotonic":true,"dataPoints":[
		{"timeUnixNano":"1700000010000000000","asInt":"3"}]}},
	{"name":"http.server.request.duration","unit":"s","histogram":{"aggregationTemporality":2,"dataPoints":[
		{"startTimeUnixNano":"1700000000000000000","timeUnixNano":"1700000010000000000",
		 "count":"3","sum":0.6,"explicitBounds":[0.1,0.5],"bucketCounts":["1","2","0"]}]}},
	{"name":"queue.wait","exponentialHistogram":{"aggregationTemporality":2,"dataPoints":[
		{"timeUnixNano":"1700000010000000000","count":"6","sum":20,"scale":0,"zeroCount":"1",
		 "positive":{"offset":1,"bucketCounts":["2","3"]}}]}},
	{"name":"rpc.duration","summary":{"dataPoints":[
		{"timeUnixNano":"1700000010000000000","count":"4","sum":2,
		 "quantileValues":[{"quantile":0.5,"value":0.4}]}]}}
]}]}]}`

func TestMetrics(t *testing.T) {
	req := &colmetrics.ExportMetricsServiceRequest{}
	if err := protojson.Unmarshal([]byte(metricsJSON), req); err != nil {
		t.Fatal(err)
	}
	points := Metrics(req)
	by := map[string][]store.MetricPoint{}
	for _, p := range points {
		if p.Service != "acme-api" || p.TsNs != 1700000010000000000 {
			t.Errorf("%s: service %q at %d", p.Name, p.Service, p.TsNs)
		}
		by[p.Name] = append(by[p.Name], p)
	}

	// The second gauge point is flagged "no recorded value" and is dropped.
	if g := by["process.runtime.memory"]; len(g) != 1 || g[0].Type != store.MetricGauge || g[0].Value != 1048576 || g[0].Cumulative || g[0].Unit != "By" {
		t.Errorf("gauge = %+v", g)
	}
	jobs := by["jobs.processed"]
	if len(jobs) != 2 || !jobs[0].Cumulative || !jobs[0].Monotonic || jobs[0].Value != 7 ||
		jobs[0].Attrs != `{"job.name":"send-receipt"}` || jobs[0].StartNs != 1700000000000000000 {
		t.Fatalf("cumulative sum = %+v", jobs)
	}
	if jobs[0].Series == jobs[1].Series {
		t.Error("two attribute sets share a series")
	}
	if c := by["cache.requests"]; len(c) != 1 || c[0].Cumulative || c[0].Value != 3 {
		t.Errorf("delta sum = %+v", c)
	}
	h := by["http.server.request.duration"]
	if len(h) != 1 || h[0].Type != store.MetricHistogram || h[0].Count != 3 || h[0].Sum != 0.6 ||
		len(h[0].Bounds) != 2 || len(h[0].Counts) != 3 || h[0].Counts[1] != 2 {
		t.Errorf("histogram = %+v", h)
	}
	// Scale 0 is base 2: offset 1 starts at 2, so the buckets end at 4 and 8.
	// The zero count sits below the first of them.
	e := by["queue.wait"]
	if len(e) != 1 || e[0].Type != store.MetricHistogram || e[0].Count != 6 {
		t.Fatalf("exponential histogram = %+v", e)
	}
	wantBounds, wantCounts := []float64{2, 4, 8}, []int64{1, 2, 3, 0}
	for i := range wantCounts {
		if len(e[0].Counts) != 4 || e[0].Counts[i] != wantCounts[i] || (i < 3 && math.Abs(e[0].Bounds[i]-wantBounds[i]) > 1e-9) {
			t.Fatalf("exponential buckets = %v %v, want %v %v", e[0].Bounds, e[0].Counts, wantBounds, wantCounts)
		}
	}
	if s := by["rpc.duration"]; len(s) != 1 || s[0].Type != store.MetricSummary || s[0].Count != 4 || s[0].Sum != 2 || !s[0].Cumulative {
		t.Errorf("summary = %+v", s)
	}
}

func TestExplicitBucketsNegative(t *testing.T) {
	// Scale 1 is base sqrt(2). Negative bucket 0 covers [-sqrt(2), -1).
	bounds, counts := explicitBuckets(&metricspb.ExponentialHistogramDataPoint{
		Scale:     1,
		ZeroCount: 2,
		Negative:  &metricspb.ExponentialHistogramDataPoint_Buckets{Offset: 0, BucketCounts: []uint64{5, 7}},
	})
	// Lowest first: the bucket ending at -sqrt(2), the one ending at -1,
	// then zero.
	if len(bounds) != 3 || math.Abs(bounds[0]+math.Sqrt2) > 1e-9 || bounds[1] != -1 || bounds[2] != 0 {
		t.Errorf("bounds = %v", bounds)
	}
	if len(counts) != 4 || counts[0] != 7 || counts[1] != 5 || counts[2] != 2 || counts[3] != 0 {
		t.Errorf("counts = %v", counts)
	}
	if b, c := explicitBuckets(&metricspb.ExponentialHistogramDataPoint{}); b != nil || c != nil {
		t.Errorf("an empty point gave %v %v", b, c)
	}
}
