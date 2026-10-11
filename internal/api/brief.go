package api

import (
	"encoding/json"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/sean-brydon/leadline/internal/brief"
	"github.com/sean-brydon/leadline/internal/discover"
	"github.com/sean-brydon/leadline/internal/integrations"
	"github.com/sean-brydon/leadline/internal/store"
)

// Sending a problem to a coding agent. internal/brief/API.md describes
// every route here; the app's "Send to Shipyard" is written from it, so
// change them together.
//
//	GET  /api/brief/query      a query shape, written up as a prompt
//	GET  /api/brief/endpoint   an endpoint
//	GET  /api/brief/trace      a trace
//	GET  /api/brief/log        a log record and the records like it
//	GET  /api/shipyard/targets where in Shipyard a service's task could start
//	POST /api/shipyard/targets the same, for a service another daemon holds
//	POST /api/shipyard/tasks   start one
//
// The briefs only read the store. The two targets routes run Shipyard's
// command line, reading only. The last is the one route in the daemon
// that starts a coding agent, and it does so once, for the request it was
// sent. (The forward routes in discover.go are the only others that
// change anything outside Leadline.)
func (s *Server) briefRoutes(mux *http.ServeMux, d *discover.Service) {
	b := brief.New(s.store)

	mux.HandleFunc("GET /api/brief/query", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		o := briefOptions(r)
		key := store.QueryKey{DBSystem: q.Get("db_system"), Fingerprint: q.Get("fingerprint")}
		if key.Fingerprint == "" {
			if q.Get("q") == "" {
				writeBriefProblem(w, http.StatusBadRequest, "bad_request", "A query brief needs a fingerprint, or q to search for one.")
				return
			}
			found, err := b.FindQuery(r.Context(), o, q.Get("q"))
			if err != nil {
				writeBriefError(w, err)
				return
			}
			key = found
		}
		out, err := b.Query(r.Context(), key, o)
		s.writeBrief(w, mux, out, err)
	})

	mux.HandleFunc("GET /api/brief/endpoint", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		o := briefOptions(r)
		// endpoint_service is the endpoint's own; service stays the app's filter.
		ep := store.Endpoint{Service: q.Get("endpoint_service"), Name: q.Get("name")}
		if ep.Name == "" {
			if q.Get("q") == "" {
				writeBriefProblem(w, http.StatusBadRequest, "bad_request", "An endpoint brief needs a name, or q to search for one.")
				return
			}
			found, err := b.FindEndpoint(r.Context(), o, q.Get("q"), ep.Service)
			if err != nil {
				writeBriefError(w, err)
				return
			}
			ep = found
		}
		out, err := b.Endpoint(r.Context(), ep, o)
		s.writeBrief(w, mux, out, err)
	})

	mux.HandleFunc("GET /api/brief/trace", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		if id == "" {
			writeBriefProblem(w, http.StatusBadRequest, "bad_request", "A trace brief needs the trace's id.")
			return
		}
		out, err := b.Trace(r.Context(), id, briefOptions(r))
		s.writeBrief(w, mux, out, err)
	})

	mux.HandleFunc("GET /api/brief/log", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		o := briefOptions(r)
		// log_service is the record's own; service stays the app's filter.
		var ref brief.LogRef
		var err error
		if cursor := q.Get("cursor"); cursor != "" {
			if ref, err = brief.ParseLogRef(cursor, q.Get("log_service")); err != nil {
				writeBriefProblem(w, http.StatusBadRequest, "bad_request", "cursor is not one from the logs list.")
				return
			}
		} else if ref, err = b.FindLog(r.Context(), o, q.Get("q"), q.Get("log_service")); err != nil {
			writeBriefError(w, err)
			return
		}
		out, err := b.Log(r.Context(), ref, o)
		s.writeBrief(w, mux, out, err)
	})

	// Where a service's task could start. With ?service= this daemon looks
	// up what it knows of the service itself; the POST takes that from the
	// caller, for a service whose data another daemon holds.
	mux.HandleFunc("GET /api/shipyard/targets", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		src := discover.TargetSource{}
		if service := q.Get("service"); service != "" {
			src = targetSource(s.sourceOf(mux, service))
		}
		writeJSON(w, d.Targets(r.Context(), src, q.Get("box")))
	})
	mux.HandleFunc("POST /api/shipyard/targets", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Source discover.TargetSource `json:"source"`
			Box    string                `json:"box"`
		}
		if !readBody(w, r, &in) {
			return
		}
		writeJSON(w, d.Targets(r.Context(), in.Source, in.Box))
	})

	// Start a task. The body is JSON and must say so: a form a web page
	// submits can't, and anything else a page sends is stopped by guard.
	mux.HandleFunc("POST /api/shipyard/tasks", func(w http.ResponseWriter, r *http.Request) {
		// A request with no Origin comes from a program on this machine,
		// which could run Shipyard itself. One from a page must be from
		// Leadline's own window (appOrigin says why).
		if origin := r.Header.Get("Origin"); origin != "" && !appOrigin(origin) {
			writeBriefProblem(w, http.StatusForbidden, "forbidden", "Only Leadline's own window can start a Shipyard task.")
			return
		}
		if kind, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); kind != "application/json" {
			writeBriefProblem(w, http.StatusUnsupportedMediaType, "bad_request", "A task is sent as JSON, with Content-Type: application/json.")
			return
		}
		var in discover.TaskRequest
		// A prompt of the largest size, written as JSON, is some three
		// times its length at the very worst.
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*discover.MaxPrompt)).Decode(&in); err != nil {
			writeBriefProblem(w, http.StatusBadRequest, "bad_request", "The request body is not JSON Leadline can read.")
			return
		}
		out, err := d.StartTask(r.Context(), in)
		if err != nil {
			writeTaskProblem(w, err)
			return
		}
		writeJSON(w, out)
	})
}

// briefOptions reads a brief's window and whether it may quote real
// values: since_ms, until_ms, service, example=1.
func briefOptions(r *http.Request) brief.Options {
	q := r.URL.Query()
	o := brief.Options{Service: q.Get("service"), Example: q.Get("example") == "1"}
	o.SinceMs, _ = strconv.ParseFloat(q.Get("since_ms"), 64)
	o.UntilMs, _ = strconv.ParseFloat(q.Get("until_ms"), 64)
	return o
}

// writeBrief answers with a brief, after saying where its service's code
// is as far as this daemon knows.
func (s *Server) writeBrief(w http.ResponseWriter, mux *http.ServeMux, b *brief.Brief, err error) {
	if err != nil {
		writeBriefError(w, err)
		return
	}
	b.Source = s.sourceOf(mux, b.Service)
	writeJSON(w, b)
}

// sourceOf is what this daemon knows of where a service's code is: the
// worktree that attached it, and the folders its commands were run from.
// Names and paths only.
func (s *Server) sourceOf(mux *http.ServeMux, service string) *brief.Source {
	src := &brief.Source{Service: service}
	if service == "" {
		return src
	}
	under := func(name, source string) bool {
		return name == source || strings.HasPrefix(name, source+integrations.SubSep)
	}
	for _, a := range s.integ.Attached() {
		if under(service, a.Service) && a.Owner.Kind == integrations.OwnerWorktree {
			o := a.Owner
			src.Via, src.Project, src.Worktree, src.Branch, src.Box, src.Path = o.Via, o.Project, o.Worktree, o.Branch, o.Box, o.Path
			break
		}
	}
	seen := map[string]bool{src.Path: true, "": true}
	for _, run := range seenRuns(mux) {
		if under(run.Service, service) || under(service, run.Service) {
			if !seen[run.Cwd] && len(src.Folders) < 4 {
				seen[run.Cwd] = true
				src.Folders = append(src.Folders, run.Cwd)
			}
		}
	}
	return src
}

func targetSource(src *brief.Source) discover.TargetSource {
	return discover.TargetSource{Service: src.Service, Via: src.Via, Project: src.Project, Worktree: src.Worktree,
		Branch: src.Branch, Box: src.Box, Path: src.Path, Folders: src.Folders}
}

func writeBriefError(w http.ResponseWriter, err error) {
	if brief.IsNotFound(err) {
		writeBriefProblem(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	fail(w, err)
}

func writeBriefProblem(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": message}})
}

// writeTaskProblem answers with why a task was not started, in the shape
// the other routes' errors have.
func writeTaskProblem(w http.ResponseWriter, err error) {
	p, ok := err.(*discover.Problem)
	if !ok {
		p = &discover.Problem{Code: "failed", Message: "Leadline could not do that."}
	}
	status := http.StatusBadGateway
	switch p.Code {
	case "bad_request", "bad_name", "bad_base", "bad_prompt", "no_prompt":
		status = http.StatusBadRequest
	case "prompt_too_long":
		status = http.StatusRequestEntityTooLarge
	case "shipyard_not_found", "no_box", "no_project", "no_agent":
		status = http.StatusNotFound
	case "worktree_exists", "not_a_repo":
		status = http.StatusConflict
	case "box_offline":
		status = http.StatusServiceUnavailable
	case "shipyard_timeout":
		status = http.StatusGatewayTimeout
	case "failed":
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": p})
}
