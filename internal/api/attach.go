package api

import (
	"net/http"

	"github.com/sean-brydon/leadline/internal/integrations"
)

// attachRoutes serves `leadline attach` and `leadline detach`: a worktree
// becoming a source of its own, and going again.
//
//	GET  /api/attach                 {"attached": [...]}: every attached source
//	GET  /api/attach?service=NAME    {"source": {...}, "connections": [...]}: one, or 404
//	POST /api/attach                 register a worktree and the connections it owns
//	POST /api/detach                 {"service": "...", "keep_data": false}: forget one
//
// internal/integrations/API.md describes the bodies. Like every route they
// are behind guard, so only this machine can call them.
func (s *Server) attachRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/attach", s.listAttached)
	mux.HandleFunc("POST /api/attach", s.attach)
	mux.HandleFunc("POST /api/detach", s.detach)
}

func (s *Server) listAttached(w http.ResponseWriter, r *http.Request) {
	if service := r.URL.Query().Get("service"); service != "" {
		source, conns, err := s.integrations().AttachedSource(service)
		if err != nil {
			writeProblem(w, err)
			return
		}
		writeJSON(w, map[string]any{"source": source, "connections": conns})
		return
	}
	writeJSON(w, map[string]any{"attached": s.integrations().Attached()})
}

func (s *Server) attach(w http.ResponseWriter, r *http.Request) {
	var in integrations.AttachInput
	if !readBody(w, r, &in) {
		return
	}
	out, err := s.integrations().Attach(r.Context(), in)
	if err != nil {
		writeProblem(w, err)
		return
	}
	if out.Changed {
		s.hub.notify()
	}
	writeJSON(w, out)
}

func (s *Server) detach(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Service  string `json:"service"`
		KeepData bool   `json:"keep_data"`
	}
	if !readBody(w, r, &in) {
		return
	}
	out, err := s.integrations().Detach(r.Context(), in.Service, in.KeepData)
	if err != nil {
		writeProblem(w, err)
		return
	}
	s.hub.notify()
	writeJSON(w, out)
}
