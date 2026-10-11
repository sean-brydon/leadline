package api

import (
	"net/http"
	"strconv"
	"time"

	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"

	"github.com/sean-brydon/leadline/internal/ingest"
)

func (s *Server) ingestMetrics(w http.ResponseWriter, r *http.Request) {
	req := &colmetrics.ExportMetricsServiceRequest{}
	isJSON, ok := readOTLP(w, r, req)
	if !ok {
		return
	}
	if err := s.store.AddMetrics(ingest.Metrics(req)); err != nil {
		fail(w, err)
		return
	}
	s.hub.notify()
	writeOTLP(w, isJSON, &colmetrics.ExportMetricsServiceResponse{})
}

// metrics lists the metric names with points in the window.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.Metrics(r.Context(), filter(r))
	respond(w, out, err)
}

// metricSeries charts one metric. `by` splits it on "service" or on an
// attribute key, into at most `groups` lines.
func (s *Server) metricSeries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := q.Get("name")
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	until := float64(time.Now().UnixNano()) / 1e6
	if v, err := strconv.ParseFloat(q.Get("until_ms"), 64); err == nil {
		until = v
	}
	since := until - 15*60*1000
	if v, err := strconv.ParseFloat(q.Get("since_ms"), 64); err == nil {
		since = v
	}
	if until <= since {
		http.Error(w, "until_ms must be after since_ms", http.StatusBadRequest)
		return
	}
	buckets := 60
	if v, err := strconv.Atoi(q.Get("buckets")); err == nil && v > 0 && v <= 2000 {
		buckets = v
	}
	groups := 6
	if v, err := strconv.Atoi(q.Get("groups")); err == nil && v > 0 && v <= 50 {
		groups = v
	}
	out, err := s.store.MetricSeries(r.Context(), name, since, until, buckets, q.Get("service"), q.Get("by"), groups)
	respond(w, out, err)
}
