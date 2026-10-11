// Package api is the daemon's HTTP surface: OTLP/HTTP in, and the JSON the
// app reads out.
package api

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/sean-brydon/leadline/internal/ingest"
	"github.com/sean-brydon/leadline/internal/integrations"
	"github.com/sean-brydon/leadline/internal/mcpserver"
	"github.com/sean-brydon/leadline/internal/store"
	"github.com/sean-brydon/leadline/internal/version"
)

const maxBody = 64 << 20

type Server struct {
	store *store.Store
	hub   *hub
	integ *integrations.Manager
}

// New makes the server for a store. integ is the manager of what the user
// connected (databases, proxies, captures); its captures wake the app
// through this server from here on. Nil gives the server one of its own
// that keeps its connections in memory, which is what tests and the bench
// want.
func New(st *store.Store, integ *integrations.Manager) *Server {
	s := &Server{store: st, hub: newHub()}
	if integ == nil {
		integ, _ = integrations.Open(integrations.Options{Store: st})
	}
	s.integ = integ
	integ.SetNotify(s.Notify)
	return s
}

// Notify tells open event streams that data arrived by some way other than
// OTLP, so the app refetches.
func (s *Server) Notify() { s.hub.notify() }

// Handler serves OTLP and the app's API together.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.otlpRoutes(mux)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "name": "leadline", "version": version.Version})
	})
	mux.HandleFunc("GET /api/summary", s.summary)
	mux.HandleFunc("GET /api/traces", s.traces)
	mux.HandleFunc("GET /api/traces/{id}", s.trace)
	mux.HandleFunc("GET /api/queries", s.queries)
	mux.HandleFunc("GET /api/logs", s.logs)
	mux.HandleFunc("GET /api/series", s.series)
	mux.HandleFunc("GET /api/metrics", s.metrics)
	mux.HandleFunc("GET /api/metrics/series", s.metricSeries)
	mux.HandleFunc("GET /api/routes", s.routes)
	mux.HandleFunc("GET /api/debug/memory", s.memory)
	mux.HandleFunc("GET /api/storage", s.storage)
	s.breakdownRoutes(mux)
	s.integrationRoutes(mux)
	s.attachRoutes(mux)
	s.runRoutes(mux)
	s.setupRoutes(mux)
	s.briefRoutes(mux, s.discoverRoutes(mux))
	mux.HandleFunc("GET /api/events", s.events)
	mux.Handle("/mcp", mcpserver.Handler(s.store, s.integ))
	return guard(mux)
}

// OTLPHandler serves only the OTLP routes, for the standard port 4318.
func (s *Server) OTLPHandler() http.Handler {
	mux := http.NewServeMux()
	s.otlpRoutes(mux)
	return guard(mux)
}

func (s *Server) otlpRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/traces", s.ingestTraces)
	mux.HandleFunc("POST /v1/logs", s.ingestLogs)
	mux.HandleFunc("POST /v1/metrics", s.ingestMetrics)
}

// guard keeps the daemon to this machine's own pages. What it holds (SQL,
// logs, request data) is not for any website the developer happens to open:
// a request must name a loopback host, which stops DNS rebinding, and only
// loopback and app origins get CORS headers.
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			http.Error(w, "leadline only answers on localhost", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			if !allowedOrigin(origin) {
				http.Error(w, "origin not allowed", http.StatusForbidden)
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Encoding")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func allowedOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	// The desktop app's webview: tauri://localhost on macOS and Linux.
	if u.Scheme == "tauri" {
		return true
	}
	return (u.Scheme == "http" || u.Scheme == "https") && loopbackHost(u.Host)
}

// appOrigin reports whether a request's Origin is Leadline's own window:
// the desktop app's webview, or the app's dev server. guard lets any page on
// this machine read, which a dev tool needs (an app under test reports from
// the browser). A route that acts for the person, such as starting a coding
// agent, is for Leadline's own window alone: the app being debugged is also
// a page on this machine, and must not be able to do that. LEADLINE_APP_ORIGINS
// adds origins, comma separated, for running the interface somewhere else.
func appOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if u.Scheme == "tauri" || (u.Scheme == "http" && u.Host == "tauri.localhost") {
		return true
	}
	if u.Scheme == "http" && (u.Host == "localhost:1520" || u.Host == "127.0.0.1:1520") {
		return true
	}
	for _, extra := range strings.Split(os.Getenv("LEADLINE_APP_ORIGINS"), ",") {
		if extra = strings.TrimSpace(extra); extra != "" && extra == origin {
			return true
		}
	}
	return false
}

func (s *Server) ingestTraces(w http.ResponseWriter, r *http.Request) {
	req := &coltrace.ExportTraceServiceRequest{}
	isJSON, ok := readOTLP(w, r, req)
	if !ok {
		return
	}
	spans := ingest.Spans(req, isJSON)
	if err := s.store.AddSpans(spans); err != nil {
		fail(w, err)
		return
	}
	s.hub.notify()
	writeOTLP(w, isJSON, &coltrace.ExportTraceServiceResponse{})
}

func (s *Server) ingestLogs(w http.ResponseWriter, r *http.Request) {
	req := &collogs.ExportLogsServiceRequest{}
	isJSON, ok := readOTLP(w, r, req)
	if !ok {
		return
	}
	logs := ingest.Logs(req, isJSON)
	if err := s.store.AddLogs(logs); err != nil {
		fail(w, err)
		return
	}
	// A framework's route list travels as a log record and is kept apart.
	for _, list := range ingest.RouteLists(req) {
		if err := s.store.SetRoutes(r.Context(), list.Service, list.TsNs, list.Body); err != nil {
			fail(w, err)
			return
		}
	}
	s.hub.notify()
	writeOTLP(w, isJSON, &collogs.ExportLogsServiceResponse{})
}

// readOTLP decodes an export request, protobuf or JSON, gzipped or not. It
// reports whether the body was JSON, and writes the error itself on failure.
func readOTLP(w http.ResponseWriter, r *http.Request, into proto.Message) (isJSON, ok bool) {
	var body io.Reader = http.MaxBytesReader(w, r.Body, maxBody)
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(body)
		if err != nil {
			http.Error(w, "bad gzip body", http.StatusBadRequest)
			return false, false
		}
		defer zr.Close()
		body = io.LimitReader(zr, maxBody)
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		http.Error(w, "could not read body", http.StatusBadRequest)
		return false, false
	}
	isJSON = strings.HasPrefix(r.Header.Get("Content-Type"), "application/json")
	if isJSON {
		err = protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(raw, into)
	} else {
		err = proto.Unmarshal(raw, into)
	}
	if err != nil {
		http.Error(w, "not a valid OTLP request: "+err.Error(), http.StatusBadRequest)
		return isJSON, false
	}
	return isJSON, true
}

func writeOTLP(w http.ResponseWriter, isJSON bool, msg proto.Message) {
	if isJSON {
		b, _ := protojson.Marshal(msg)
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
		return
	}
	b, _ := proto.Marshal(msg)
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.Write(b)
}

func (s *Server) summary(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.Summary(r.Context())
	respond(w, out, err)
}

// memory says what the daemon's memory is made of. Nothing in the app
// reads it; it is for a person, or `curl`, asking why the daemon is the
// size it is.
func (s *Server) memory(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.Memory(r.Context())
	respond(w, out, err)
}

// storage says how much is kept and how much may be: the database's size,
// the size cap, how long rows are kept and how old the oldest is.
func (s *Server) storage(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.Storage(r.Context())
	respond(w, out, err)
}

func (s *Server) traces(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.Traces(r.Context(), filter(r))
	respondPage(w, out, err)
}

func (s *Server) trace(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	spans, logs, err := s.store.Trace(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	if len(spans) == 0 {
		http.Error(w, "no such trace", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{"trace_id": id, "spans": spans, "logs": logs})
}

func (s *Server) queries(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.Queries(r.Context(), filter(r))
	respond(w, out, err)
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.Logs(r.Context(), filter(r))
	respondPage(w, out, err)
}

// respondPage answers a list that takes a cursor. A cursor no row ever
// carried is the caller's mistake, not the daemon's.
func respondPage(w http.ResponseWriter, v any, err error) {
	if errors.Is(err, store.ErrBadCursor) {
		http.Error(w, "before: "+err.Error(), http.StatusBadRequest)
		return
	}
	respond(w, v, err)
}

func (s *Server) series(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	until := float64(time.Now().UnixNano()) / 1e6
	if v, err := strconv.ParseFloat(q.Get("until_ms"), 64); err == nil {
		until = v
	}
	since := until - 15*60*1000
	if v, err := strconv.ParseFloat(q.Get("since_ms"), 64); err == nil {
		since = v
	}
	buckets := 60
	if v, err := strconv.Atoi(q.Get("buckets")); err == nil && v > 0 && v <= 2000 {
		buckets = v
	}
	if until <= since {
		http.Error(w, "until_ms must be after since_ms", http.StatusBadRequest)
		return
	}
	out, err := s.store.SeriesBetween(r.Context(), since, until, buckets, q.Get("service"))
	respond(w, out, err)
}

// events is a server-sent stream that says "new data arrived". The app
// refetches what it shows; the stream carries no rows itself.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	// Listen before greeting: "hello" tells the app it may fetch, and data
	// that arrives between the greeting and the subscription would wake
	// nobody.
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)
	fmt.Fprint(w, "event: hello\ndata: {}\n\n")
	flusher.Flush()

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			fmt.Fprint(w, "event: ingest\ndata: {}\n\n")
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
		}
		flusher.Flush()
	}
}

func filter(r *http.Request) store.Filter {
	q := r.URL.Query()
	f := store.Filter{Service: q.Get("service"), Search: q.Get("q"), Before: q.Get("before"),
		User: q.Get("user"), Kind: q.Get("kind"), Control: q.Get("control") == "1",
		DBSystem: q.Get("db_system"), DBServer: q.Get("db_server")}
	f.SinceMs, _ = strconv.ParseFloat(q.Get("since_ms"), 64)
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	if n, err := strconv.Atoi(q.Get("min_severity")); err == nil {
		f.MinSeverity = int32(n)
	}
	return f
}

func respond(w http.ResponseWriter, v any, err error) {
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, v)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, err error) {
	slog.Error("request failed", "err", err)
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
