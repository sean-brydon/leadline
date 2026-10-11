package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/sean-brydon/leadline/internal/store"
)

// The breakdowns: endpoints, one endpoint, one query, and the charts that
// cut requests by latency band or by a dimension. All of them take the same
// window (since_ms, until_ms, service); until_ms defaults to now.

func (s *Server) breakdownRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/endpoints", s.endpoints)
	mux.HandleFunc("GET /api/endpoints/detail", s.endpointDetail)
	mux.HandleFunc("GET /api/queries/detail", s.queryDetail)
	mux.HandleFunc("GET /api/heatmap", s.heatmap)
	mux.HandleFunc("GET /api/breakdown", s.groupedSeries)
}

// routes answers with the route lists frameworks sent, each route with how
// many requests it had in the window: the routes an app has, hit or not.
func (s *Server) routes(w http.ResponseWriter, r *http.Request) {
	win, ok := window(w, r)
	if !ok {
		return
	}
	lists, err := s.store.Routes(r.Context(), win)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"lists": lists})
}

// window reads the time window and service of a request. It writes the
// error itself and reports false when the window is empty or backwards.
func window(w http.ResponseWriter, r *http.Request) (store.Window, bool) {
	q := r.URL.Query()
	win := store.Window{Service: q.Get("service"), UntilMs: float64(time.Now().UnixNano()) / 1e6,
		DBSystem: q.Get("db_system"), DBServer: q.Get("db_server")}
	if v, err := strconv.ParseFloat(q.Get("until_ms"), 64); err == nil {
		win.UntilMs = v
	}
	win.SinceMs = win.UntilMs - 15*60*1000
	if v, err := strconv.ParseFloat(q.Get("since_ms"), 64); err == nil {
		win.SinceMs = v
	}
	if win.UntilMs <= win.SinceMs {
		http.Error(w, "until_ms must be after since_ms", http.StatusBadRequest)
		return win, false
	}
	return win, true
}

// intParam reads a whole number between 1 and max, or returns def.
func intParam(r *http.Request, name string, def, max int) int {
	if v, err := strconv.Atoi(r.URL.Query().Get(name)); err == nil && v > 0 && v <= max {
		return v
	}
	return def
}

func (s *Server) endpoints(w http.ResponseWriter, r *http.Request) {
	win, ok := window(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	spark := 0
	if q.Has("spark") {
		spark = intParam(r, "spark", 24, 200)
	}
	out, err := s.store.Endpoints(r.Context(), win, store.EndpointQuery{
		Search: q.Get("q"),
		Sort:   q.Get("sort"),
		Asc:    q.Get("dir") == "asc",
		Limit:  intParam(r, "limit", 200, 1000),
		Spark:  spark,
	})
	respond(w, out, err)
}

func (s *Server) endpointDetail(w http.ResponseWriter, r *http.Request) {
	win, ok := window(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if q.Get("name") == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	// endpoint_service is the endpoint's own; service stays the app's filter.
	ep := store.Endpoint{Service: q.Get("endpoint_service"), Name: q.Get("name")}
	out, err := s.store.EndpointDetail(r.Context(), win, ep, intParam(r, "buckets", 60, 2000))
	respond(w, out, err)
}

func (s *Server) queryDetail(w http.ResponseWriter, r *http.Request) {
	win, ok := window(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if q.Get("fingerprint") == "" {
		http.Error(w, "fingerprint is required", http.StatusBadRequest)
		return
	}
	key := store.QueryKey{DBSystem: q.Get("db_system"), Fingerprint: q.Get("fingerprint")}
	out, err := s.store.QueryDetail(r.Context(), win, key, intParam(r, "buckets", 60, 2000))
	respond(w, out, err)
}

func (s *Server) heatmap(w http.ResponseWriter, r *http.Request) {
	win, ok := window(w, r)
	if !ok {
		return
	}
	out, err := s.store.RequestHeatmap(r.Context(), win, intParam(r, "buckets", 90, 2000))
	respond(w, out, err)
}

func (s *Server) groupedSeries(w http.ResponseWriter, r *http.Request) {
	win, ok := window(w, r)
	if !ok {
		return
	}
	by := r.URL.Query().Get("by")
	if !store.KnownGrouping(by) {
		http.Error(w, "by must be one of "+store.Groupings(), http.StatusBadRequest)
		return
	}
	out, err := s.store.GroupedSeries(r.Context(), win, by, intParam(r, "buckets", 60, 2000), intParam(r, "limit", 6, 20))
	respond(w, out, err)
}
