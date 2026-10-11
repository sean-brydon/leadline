package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/sean-brydon/leadline/internal/integrations"
)

// The integrations API: saved connections, their stats, the proxies, and
// what is feeding Leadline. internal/integrations/API.md describes every
// response; the app's screens are written from it, so change them together.

func (s *Server) integrationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/sources", s.sources)

	mux.HandleFunc("GET /api/connections", s.listConnections)
	mux.HandleFunc("POST /api/connections", s.addConnection)
	mux.HandleFunc("POST /api/connections/test", s.testConnection)
	mux.HandleFunc("GET /api/connections/{id}", s.getConnection)
	mux.HandleFunc("PATCH /api/connections/{id}", s.editConnection)
	mux.HandleFunc("DELETE /api/connections/{id}", s.deleteConnection)
	mux.HandleFunc("POST /api/connections/{id}/test", s.testConnection)
	mux.HandleFunc("GET /api/connections/{id}/stats", s.connectionStats)
	// The same edit and delete as POSTs, from when guard let a page on
	// another origin (the app's dev server, the desktop webview) send only
	// GET and POST. It allows PATCH and DELETE now; these stay for the
	// callers that were written to them.
	mux.HandleFunc("POST /api/connections/{id}", s.editConnection)
	mux.HandleFunc("POST /api/connections/{id}/delete", s.deleteConnection)

	mux.HandleFunc("GET /api/proxies", s.listProxies)
	mux.HandleFunc("POST /api/proxies", s.openProxy)
	mux.HandleFunc("DELETE /api/proxies/{id}", s.closeProxy)
}

// integrations is the manager the server was made with.
func (s *Server) integrations() *integrations.Manager { return s.integ }

// writeProblem answers with an error a person can act on:
// {"error":{"code":"…","message":"…","hint":"…"}}.
func writeProblem(w http.ResponseWriter, err error) {
	p := integrations.AsProblem(err)
	status := http.StatusBadGateway // the database or Redis is what failed
	switch p.Code {
	case "not_found":
		status = http.StatusNotFound
	case "bad_request", "bad_url", "capture_remote", "too_many":
		status = http.StatusBadRequest
	case "port_in_use", "no_port":
		status = http.StatusConflict
	case "not_saved", "failed":
		status = http.StatusInternalServerError
	case "shutting_down":
		status = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": p})
}

// readBody decodes a small JSON body, writing the error itself on failure.
func readBody(w http.ResponseWriter, r *http.Request, into any) bool {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(into)
	if err != nil {
		writeProblem(w, &integrations.Problem{Code: "bad_request", Message: "The request body is not JSON Leadline can read."})
		return false
	}
	return true
}

// sources takes an optional since_ms: where the windowed counts of proxies
// and live captures start. Without it they cover the last 15 minutes.
func (s *Server) sources(w http.ResponseWriter, r *http.Request) {
	var since time.Time
	if ms, err := strconv.ParseFloat(r.URL.Query().Get("since_ms"), 64); err == nil && ms > 0 {
		since = time.UnixMicro(int64(ms * 1000))
	}
	out, err := s.integrations().SourcesSince(r.Context(), since)
	respond(w, out, err)
}

func (s *Server) listConnections(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"connections": s.integrations().Connections()})
}

func (s *Server) getConnection(w http.ResponseWriter, r *http.Request) {
	out, err := s.integrations().Connection(r.PathValue("id"))
	if err != nil {
		writeProblem(w, err)
		return
	}
	writeJSON(w, out)
}

func (s *Server) addConnection(w http.ResponseWriter, r *http.Request) {
	var in integrations.ConnectionInput
	if !readBody(w, r, &in) {
		return
	}
	out, err := s.integrations().Add(r.Context(), in)
	if err != nil {
		writeProblem(w, err)
		return
	}
	s.hub.notify()
	writeJSON(w, out)
}

func (s *Server) editConnection(w http.ResponseWriter, r *http.Request) {
	var in integrations.ConnectionInput
	if !readBody(w, r, &in) {
		return
	}
	out, err := s.integrations().Update(r.Context(), r.PathValue("id"), in)
	if err != nil {
		writeProblem(w, err)
		return
	}
	s.hub.notify()
	writeJSON(w, out)
}

func (s *Server) deleteConnection(w http.ResponseWriter, r *http.Request) {
	if err := s.integrations().Delete(r.PathValue("id")); err != nil {
		writeProblem(w, err)
		return
	}
	s.hub.notify()
	writeJSON(w, map[string]any{"ok": true})
}

// testConnection tries a connection without saving anything. A server
// that refuses is not a failed request: the answer is 200 with ok false
// and the reason.
func (s *Server) testConnection(w http.ResponseWriter, r *http.Request) {
	var in integrations.TestInput
	if r.ContentLength != 0 && !readBody(w, r, &in) {
		return
	}
	if id := r.PathValue("id"); id != "" {
		in.ID = id
	}
	out := s.integrations().Test(r.Context(), in)
	if out.Error != nil && out.Error.Code == "not_found" {
		writeProblem(w, out.Error)
		return
	}
	writeJSON(w, out)
}

func (s *Server) connectionStats(w http.ResponseWriter, r *http.Request) {
	out, err := s.integrations().Stats(r.Context(), r.PathValue("id"), r.URL.Query().Get("fresh") == "1")
	if err != nil {
		writeProblem(w, err)
		return
	}
	writeJSON(w, out)
}

func (s *Server) listProxies(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"proxies": s.integrations().Proxies()})
}

// openProxy and closeProxy are for `leadline run`: a proxy for as long as
// a command runs.
func (s *Server) openProxy(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind     string `json:"kind"`
		Upstream string `json:"upstream"`
		Label    string `json:"label"`
	}
	if !readBody(w, r, &in) {
		return
	}
	out, err := s.integrations().OpenProxy(in.Kind, in.Upstream, in.Label)
	if err != nil {
		writeProblem(w, err)
		return
	}
	s.hub.notify()
	writeJSON(w, out)
}

func (s *Server) closeProxy(w http.ResponseWriter, r *http.Request) {
	closed, err := s.integrations().CloseProxy(r.PathValue("id"))
	if err != nil {
		writeProblem(w, err)
		return
	}
	s.hub.notify()
	writeJSON(w, map[string]any{"ok": true, "closed": closed})
}
