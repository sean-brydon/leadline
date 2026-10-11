package demo

import (
	"fmt"
	"math"
	"sort"
	"time"

	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
)

// metricInterval is how often the acme services export their metrics.
const metricInterval = 10 * time.Second

// The worker restarts every quarter of an hour, so any stretch of demo data
// that long has a counter going back to zero in it.
const workerRestartEvery = 90 // exports

// Request durations use the bounds OpenTelemetry's HTTP conventions advise.
var durationBounds = []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10}

var metricDefs = map[string]struct{ unit, description string }{
	"http.server.request.duration": {"s", "Duration of HTTP server requests."},
	"db.client.connections.usage":  {"{connection}", "The number of connections that are currently in the state described by the state attribute."},
	"process.runtime.memory":       {"By", "Heap memory in use."},
	"process.cpu.utilization":      {"1", "Share of one CPU the process used."},
	"jobs.processed":               {"{job}", "Background jobs finished."},
	"cache.requests":               {"{request}", "Lookups in the product cache."},
}

// meters is what the acme processes' instruments hold between exports: the
// running totals that make a metric cumulative.
type meters struct {
	started map[string]time.Time // each service's process start
	series  map[string]*meterSeries
	order   []string           // series keys, in the order first recorded
	heap    map[string]float64 // bytes in use, by service
	exports int
}

type meterSeries struct {
	service, name string
	attrs         []*commonpb.KeyValue
	total         float64  // counters
	buckets       []uint64 // histograms: one per bound, and one above
	count         uint64
	sum           float64
}

// meters returns the generator's instruments, made on the first call:
// counters must run on from one call of Metrics to the next.
func (g *Generator) meters(start time.Time) *meters {
	if g.m == nil {
		g.m = &meters{
			started: map[string]time.Time{"acme-api": start, "acme-payments": start, "acme-worker": start},
			series:  map[string]*meterSeries{},
			heap:    map[string]float64{"acme-api": 180 << 20, "acme-payments": 64 << 20, "acme-worker": 96 << 20},
		}
	}
	return g.m
}

// Metrics makes the metrics the acme services exported between from
// (exclusive) and to: one export every ten seconds, on the clock's ten
// second marks, so calls for back-to-back stretches neither repeat nor skip
// one. Counters and histograms are cumulative, as SDKs send them by default,
// and carry on from the last call.
func (g *Generator) Metrics(from, to time.Time) *colmetrics.ExportMetricsServiceRequest {
	m := g.meters(from)
	out := &metricsOut{scopes: map[string]*metricspb.ScopeMetrics{}, metrics: map[string]*metricspb.Metric{}}
	for t := from.Truncate(metricInterval).Add(metricInterval); !t.After(to); t = t.Add(metricInterval) {
		m.exports++
		if m.exports%workerRestartEvery == workerRestartEvery/2 {
			m.restart("acme-worker", t.Add(-g.ms(2000, 6000)))
		}
		g.tick(m, out, t)
		m.export(out, t)
	}
	return out.request()
}

// tick is ten seconds in the shop: requests served, jobs run, memory used.
func (g *Generator) tick(m *meters, out *metricsOut, t time.Time) {
	secs := metricInterval.Seconds()
	// Traffic comes in waves of about seven minutes.
	load := 1 + 0.45*math.Sin(2*math.Pi*float64(t.Unix())/420)

	routes := []struct {
		service, method, route string
		perSec, median, spread float64
		status                 int64
	}{
		{"acme-api", "GET", "/api/products", 1.6, 0.012, 0.35, 200},
		{"acme-api", "GET", "/api/orders", 0.9, 0.065, 0.4, 200},
		{"acme-api", "GET", "/api/search", 0.7, 0.24, 0.45, 200},
		{"acme-api", "POST", "/api/checkout", 0.44, 0.23, 0.3, 200},
		{"acme-api", "POST", "/api/checkout", 0.06, 0.2, 0.3, 402},
		{"acme-payments", "POST", "/charge", 0.44, 0.17, 0.3, 200},
		{"acme-payments", "POST", "/charge", 0.06, 0.14, 0.3, 402},
	}
	for _, r := range routes {
		for i := g.poisson(r.perSec * secs * load); i > 0; i-- {
			d := r.median * math.Exp(r.spread*g.r.NormFloat64())
			// The search's unindexed scan is now and then very slow.
			if r.route == "/api/search" && g.r.Float64() < 0.12 {
				d = 0.9 + 1.5*g.r.Float64()
			}
			m.observe(r.service, "http.server.request.duration", d,
				"http.request.method", r.method, "http.route", r.route, "http.response.status_code", r.status)
		}
	}

	jobs := []struct {
		name   string
		perSec float64
	}{{"send-receipt", 0.4}, {"resize-image", 0.2}, {"sync-inventory", 0.1}}
	for _, j := range jobs {
		for i := g.poisson(j.perSec * secs * load); i > 0; i-- {
			status := "ok"
			if g.r.Float64() < 0.04 {
				status = "failed"
			}
			m.count("acme-worker", "jobs.processed", 1, "job.name", j.name, "job.status", status)
		}
	}

	// The cache counts in deltas: each export says what happened since the
	// one before, as an SDK set to delta temporality would.
	lookups := g.poisson(1.6 * secs * load)
	hits := 0
	for i := 0; i < lookups; i++ {
		if g.r.Float64() < 0.82 {
			hits++
		}
	}
	for i, n := range []int{hits, lookups - hits} {
		out.add("acme-api", "cache.requests", sumOf(false, true), &metricspb.NumberDataPoint{
			Attributes:        attrs("cache.result", []string{"hit", "miss"}[i]),
			StartTimeUnixNano: ns(t.Add(-metricInterval)), TimeUnixNano: ns(t),
			Value: &metricspb.NumberDataPoint_AsInt{AsInt: int64(n)},
		})
	}

	// The pool is an up-down counter: a sum that isn't monotonic.
	used := math.Round(2 + 5*load + 2*g.r.Float64())
	open := math.Max(10, used+2)
	for i, n := range []float64{used, open - used} {
		out.add("acme-api", "db.client.connections.usage", sumOf(true, false), &metricspb.NumberDataPoint{
			Attributes:        attrs("pool.name", "acme-pg", "state", []string{"used", "idle"}[i]),
			StartTimeUnixNano: ns(m.started["acme-api"]), TimeUnixNano: ns(t),
			Value: &metricspb.NumberDataPoint_AsInt{AsInt: int64(n)},
		})
	}

	// Heaps grow until the collector runs. The worker's also leaks.
	limits := map[string]float64{"acme-api": 260 << 20, "acme-payments": 96 << 20, "acme-worker": 150 << 20}
	for _, service := range []string{"acme-api", "acme-payments", "acme-worker"} {
		m.heap[service] += load * limits[service] * (0.02 + 0.03*g.r.Float64())
		limit := limits[service]
		if service == "acme-worker" {
			limit += t.Sub(m.started[service]).Minutes() * (6 << 20)
		}
		if m.heap[service] > limit {
			m.heap[service] = limit * (0.62 + 0.08*g.r.Float64())
		}
		out.add(service, "process.runtime.memory", gauge(), &metricspb.NumberDataPoint{
			TimeUnixNano: ns(t),
			Value:        &metricspb.NumberDataPoint_AsInt{AsInt: int64(m.heap[service])},
		})
		busy := map[string]float64{"acme-api": 0.22, "acme-payments": 0.06, "acme-worker": 0.14}[service]
		out.add(service, "process.cpu.utilization", gauge(), &metricspb.NumberDataPoint{
			TimeUnixNano: ns(t),
			Value:        &metricspb.NumberDataPoint_AsDouble{AsDouble: busy * load * (0.8 + 0.4*g.r.Float64())},
		})
	}
}

// restart begins a service's process again: its totals go back to zero.
func (m *meters) restart(service string, at time.Time) {
	m.started[service] = at
	keep := m.order[:0]
	for _, key := range m.order {
		if m.series[key].service == service {
			delete(m.series, key)
			continue
		}
		keep = append(keep, key)
	}
	m.order = keep
	m.heap[service] = 96 << 20
}

// one returns the series for a set of attributes, starting it on first use:
// like an SDK, the demo exports a series only once something was recorded.
func (m *meters) one(service, name string, kv []any) *meterSeries {
	key := fmt.Sprint(service, name, kv)
	s := m.series[key]
	if s == nil {
		s = &meterSeries{service: service, name: name, attrs: attrs(kv...)}
		m.series[key] = s
		m.order = append(m.order, key)
	}
	return s
}

func (m *meters) count(service, name string, n float64, kv ...any) {
	m.one(service, name, kv).total += n
}

func (m *meters) observe(service, name string, v float64, kv ...any) {
	s := m.one(service, name, kv)
	if s.buckets == nil {
		s.buckets = make([]uint64, len(durationBounds)+1)
	}
	s.buckets[sort.SearchFloat64s(durationBounds, v)]++
	s.count++
	s.sum += v
}

// export adds every series' running total as a point at t.
func (m *meters) export(out *metricsOut, t time.Time) {
	for _, key := range m.order {
		s := m.series[key]
		start := ns(m.started[s.service])
		if s.buckets != nil {
			sum := s.sum
			out.add(s.service, s.name, histogram(), &metricspb.HistogramDataPoint{
				Attributes:        s.attrs,
				StartTimeUnixNano: start, TimeUnixNano: ns(t),
				Count: s.count, Sum: &sum,
				ExplicitBounds: durationBounds,
				BucketCounts:   append([]uint64(nil), s.buckets...),
			})
			continue
		}
		out.add(s.service, s.name, sumOf(true, true), &metricspb.NumberDataPoint{
			Attributes:        s.attrs,
			StartTimeUnixNano: start, TimeUnixNano: ns(t),
			Value: &metricspb.NumberDataPoint_AsInt{AsInt: int64(s.total)},
		})
	}
}

// metricsOut collects data points into one export request.
type metricsOut struct {
	scopes  map[string]*metricspb.ScopeMetrics // by service
	metrics map[string]*metricspb.Metric       // by service and name
}

func gauge() *metricspb.Metric {
	return &metricspb.Metric{Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{}}}
}

func sumOf(cumulative, monotonic bool) *metricspb.Metric {
	return &metricspb.Metric{Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
		AggregationTemporality: temporality(cumulative), IsMonotonic: monotonic,
	}}}
}

func histogram() *metricspb.Metric {
	return &metricspb.Metric{Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{
		AggregationTemporality: temporality(true),
	}}}
}

func temporality(cumulative bool) metricspb.AggregationTemporality {
	if cumulative {
		return metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE
	}
	return metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA
}

// add appends a point to a service's metric, which is made from blank the
// first time: an empty metric of the right type.
func (o *metricsOut) add(service, name string, blank *metricspb.Metric, point any) {
	m := o.metrics[service+"\n"+name]
	if m == nil {
		def := metricDefs[name]
		m = blank
		m.Name, m.Unit, m.Description = name, def.unit, def.description
		o.metrics[service+"\n"+name] = m
		scope := o.scopes[service]
		if scope == nil {
			scope = &metricspb.ScopeMetrics{Scope: &commonpb.InstrumentationScope{Name: "acme"}}
			o.scopes[service] = scope
		}
		scope.Metrics = append(scope.Metrics, m)
	}
	switch data := m.Data.(type) {
	case *metricspb.Metric_Gauge:
		data.Gauge.DataPoints = append(data.Gauge.DataPoints, point.(*metricspb.NumberDataPoint))
	case *metricspb.Metric_Sum:
		data.Sum.DataPoints = append(data.Sum.DataPoints, point.(*metricspb.NumberDataPoint))
	case *metricspb.Metric_Histogram:
		data.Histogram.DataPoints = append(data.Histogram.DataPoints, point.(*metricspb.HistogramDataPoint))
	}
}

func (o *metricsOut) request() *colmetrics.ExportMetricsServiceRequest {
	req := &colmetrics.ExportMetricsServiceRequest{}
	for _, service := range []string{"acme-api", "acme-payments", "acme-worker"} {
		if scope := o.scopes[service]; scope != nil {
			req.ResourceMetrics = append(req.ResourceMetrics, &metricspb.ResourceMetrics{
				Resource:     resource(service),
				ScopeMetrics: []*metricspb.ScopeMetrics{scope},
			})
		}
	}
	return req
}

// poisson picks how many things happened when mean were expected.
func (g *Generator) poisson(mean float64) int {
	if mean > 30 {
		return max(0, int(math.Round(mean+math.Sqrt(mean)*g.r.NormFloat64())))
	}
	limit, p, n := math.Exp(-mean), 1.0, 0
	for {
		p *= g.r.Float64()
		if p <= limit {
			return n
		}
		n++
	}
}
