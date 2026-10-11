package api

import (
	"bytes"
	"context"
	"encoding/json"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/sean-brydon/leadline/internal/discover"
	"github.com/sean-brydon/leadline/internal/integrations"
)

// The discovery API: what is listening on this machine, the projects
// behind it, the connection URLs in their env files, and the owner's
// Shipyard boxes. internal/discover/API.md describes every response and,
// more to the point, exactly what is read to make them; the app's
// onboarding is written from it, so change them together.

// discoverOptions lets a test change how discovery looks at the machine.
// Without it a test of these routes would read whatever happens to be
// running, and installed, where the test runs.
var discoverOptions func(*discover.Options)

// It returns the service, which the routes that send a problem to
// Shipyard ask about boxes too.
func (s *Server) discoverRoutes(mux *http.ServeMux) *discover.Service {
	opts := discover.Options{
		Connections: discoverConnections{s.integ},
		Runs:        func() []discover.Run { return seenRuns(mux) },
		Services: func(ctx context.Context) []string {
			src, err := s.integ.Sources(ctx)
			if err != nil {
				return nil
			}
			names := make([]string, len(src.Services))
			for i, svc := range src.Services {
				names[i] = svc.Name
			}
			return names
		},
	}
	if discoverOptions != nil {
		discoverOptions(&opts)
	}
	d := discover.New(opts)

	mux.HandleFunc("GET /api/discover/listening", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, d.Listening())
	})

	mux.HandleFunc("POST /api/discover/scan", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Folders []string `json:"folders"`
		}
		if r.ContentLength != 0 && !readBody(w, r, &in) {
			return
		}
		// The advice names the daemon by its own address, not by a port a
		// forward delivered the request through (selfURL).
		scan, err := d.Start(discover.ScanOptions{Folders: in.Folders, Daemon: selfURL(r)})
		if err != nil {
			writeDiscoverProblem(w, err)
			return
		}
		writeJSON(w, scan)
	})
	mux.HandleFunc("GET /api/discover/scan", func(w http.ResponseWriter, r *http.Request) {
		// wait_ms holds the answer until the scan ends, so a caller need
		// not poll in a tight loop.
		wait, _ := strconv.Atoi(r.URL.Query().Get("wait_ms"))
		wait = max(0, min(wait, 30_000))
		scan, ok := d.Wait(r.Context(), time.Duration(wait)*time.Millisecond)
		if !ok {
			writeDiscoverProblem(w, &discover.Problem{Code: "no_scan", Message: "There is no scan.", Hint: "Start one with POST /api/discover/scan."})
			return
		}
		writeJSON(w, scan)
	})
	mux.HandleFunc("DELETE /api/discover/scan", func(w http.ResponseWriter, r *http.Request) {
		d.Cancel()
		writeJSON(w, map[string]any{"ok": true})
	})

	// The one route that opens env files, and only those of the projects
	// it is given: a scan opens none, and there is no "all of them".
	mux.HandleFunc("POST /api/discover/env", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Projects []string `json:"projects"`
		}
		if !readBody(w, r, &in) {
			return
		}
		scan, err := d.ReadEnv(r.Context(), in.Projects)
		if err != nil {
			writeDiscoverProblem(w, err)
			return
		}
		writeJSON(w, scan)
	})

	mux.HandleFunc("POST /api/discover/save", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Connections []discover.SaveItem `json:"connections"`
		}
		if !readBody(w, r, &in) {
			return
		}
		out, err := d.Save(r.Context(), in.Connections)
		if err != nil {
			writeDiscoverProblem(w, err)
			return
		}
		if len(out.Saved) > 0 {
			s.hub.notify()
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("POST /api/discover/test", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			IDs    []string `json:"ids"`
			Remote bool     `json:"remote"`
		}
		if !readBody(w, r, &in) {
			return
		}
		out, err := d.Test(r.Context(), in.IDs, in.Remote)
		if err != nil {
			writeDiscoverProblem(w, err)
			return
		}
		writeJSON(w, out)
	})

	mux.HandleFunc("GET /api/discover/shipyard", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, d.Shipyard(r.Context(), r.URL.Query().Get("fresh") == "1"))
	})
	mux.HandleFunc("GET /api/discover/shipyard/boxes/{box}", func(w http.ResponseWriter, r *http.Request) {
		out, err := d.BoxProjects(r.Context(), r.PathValue("box"))
		if err != nil {
			writeDiscoverProblem(w, err)
			return
		}
		writeJSON(w, out)
	})
	s.forwardRoutes(mux, d)
	return d
}

// forwardRoutes are how this computer reaches the Leadline on one of the
// owner's Shipyard boxes: a local port Shipyard forwards to the box's
// daemon. internal/discover/API.md describes them under "Forwards".
//
//	GET  /api/shipyard/forwards        the forwards Shipyard holds to a box's Leadline
//	POST /api/shipyard/forwards        make sure there is one to a box: {"box", "local"}
//	POST /api/shipyard/forwards/close  close one: {"id"}
//
// The two POSTs run `berth forward` and `berth unforward`, which change
// Shipyard's state, and nothing else in the daemon runs either: no timer,
// no start-up step. All three are for Leadline's own window (ownWindow).
func (s *Server) forwardRoutes(mux *http.ServeMux, d *discover.Service) {
	mux.HandleFunc("GET /api/shipyard/forwards", func(w http.ResponseWriter, r *http.Request) {
		if !ownWindow(w, r, false) {
			return
		}
		out, err := d.Forwards(r.Context())
		if err != nil {
			writeForwardProblem(w, err)
			return
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("POST /api/shipyard/forwards", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Box string `json:"box"`
			// Local is the port the forward had before, to have again if it is free.
			Local int `json:"local"`
		}
		if !ownWindow(w, r, true) || !readForwardBody(w, r, &in) {
			return
		}
		out, err := d.OpenForward(r.Context(), in.Box, in.Local)
		if err != nil {
			writeForwardProblem(w, err)
			return
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("POST /api/shipyard/forwards/close", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ID string `json:"id"`
		}
		if !ownWindow(w, r, true) || !readForwardBody(w, r, &in) {
			return
		}
		out, err := d.CloseForward(r.Context(), in.ID)
		if err != nil {
			writeForwardProblem(w, err)
			return
		}
		writeJSON(w, out)
	})
}

// ownWindow lets a request through when it may act for the person. A
// request with no Origin comes from a program on this machine, which could
// run Shipyard itself. One from a page must be from Leadline's own window
// (appOrigin says why): the app being debugged is a page on this machine
// too, and must not be able to open or close a forward. With `body`, what
// was sent must also say it is JSON, which a form a web page submits
// can't. It writes the refusal itself.
func ownWindow(w http.ResponseWriter, r *http.Request, body bool) bool {
	if origin := r.Header.Get("Origin"); origin != "" && !appOrigin(origin) {
		writeForwardProblem(w, &discover.Problem{Code: "forbidden", Message: "Only Leadline's own window can open, list or close a Shipyard forward."})
		return false
	}
	if !body {
		return true
	}
	if kind, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); kind != "application/json" {
		writeForwardProblem(w, &discover.Problem{Code: "not_json", Message: "A forward is asked for as JSON, with Content-Type: application/json."})
		return false
	}
	return true
}

// readForwardBody reads a small JSON body, and answers in the API's own
// shape when it can't.
func readForwardBody(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(into); err != nil {
		writeForwardProblem(w, &discover.Problem{Code: "bad_request", Message: "The request body is not JSON Leadline can read."})
		return false
	}
	return true
}

// writeForwardProblem answers with why a forward was not opened, listed
// or closed, in the shape the other routes' errors have.
func writeForwardProblem(w http.ResponseWriter, err error) {
	p, ok := err.(*discover.Problem)
	if !ok {
		p = &discover.Problem{Code: "failed", Message: "Leadline could not do that."}
	}
	status := http.StatusBadGateway
	switch p.Code {
	case "bad_request", "box_local":
		status = http.StatusBadRequest
	case "forbidden":
		status = http.StatusForbidden
	case "not_json":
		status = http.StatusUnsupportedMediaType
	case "shipyard_not_found", "no_box":
		status = http.StatusNotFound
	case "port_in_use", "no_port", "not_leadline":
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

// writeDiscoverProblem answers with a discovery error, in the shape the
// integrations API's errors have.
func writeDiscoverProblem(w http.ResponseWriter, err error) {
	p, ok := err.(*discover.Problem)
	if !ok {
		// Not one of discovery's own: say nothing of what it was.
		p = &discover.Problem{Code: "failed", Message: "Leadline could not do that."}
	}
	status := http.StatusInternalServerError
	switch p.Code {
	case "bad_request":
		status = http.StatusBadRequest
	case "no_scan", "no_project", "shipyard_not_found", "no_box":
		status = http.StatusNotFound
	case "scan_not_done":
		status = http.StatusConflict
	case "shipyard_failed":
		status = http.StatusBadGateway
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": p})
}

// seenRuns lists the `leadline run` commands the daemon has seen. They
// are kept by runRoutes, behind GET /api/runs and nowhere else, so that
// route is asked, in process.
func seenRuns(mux *http.ServeMux) []discover.Run {
	req, err := http.NewRequest(http.MethodGet, "/api/runs", nil)
	if err != nil {
		return nil
	}
	var w memoryResponse
	mux.ServeHTTP(&w, req)
	var got struct {
		Runs []Run `json:"runs"`
	}
	if w.status >= 300 || json.Unmarshal(w.body.Bytes(), &got) != nil {
		return nil
	}
	out := make([]discover.Run, 0, len(got.Runs))
	for _, r := range got.Runs {
		out = append(out, discover.Run{Cwd: r.Cwd, Service: r.Service})
	}
	return out
}

// memoryResponse is a response kept in memory.
type memoryResponse struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (m *memoryResponse) Header() http.Header {
	if m.header == nil {
		m.header = http.Header{}
	}
	return m.header
}
func (m *memoryResponse) Write(p []byte) (int, error) { return m.body.Write(p) }
func (m *memoryResponse) WriteHeader(status int)      { m.status = status }

// discoverConnections is the integrations manager as discovery sees it:
// what is saved, and saving and trying a found URL. The URL passes through
// here with its password on the way in; nothing comes back but the
// manager's own masked views.
type discoverConnections struct{ m *integrations.Manager }

func (c discoverConnections) Saved() []discover.SavedConnection {
	views := c.m.Connections()
	out := make([]discover.SavedConnection, len(views))
	for i, v := range views {
		out[i] = discover.SavedConnection{ID: v.ID, Kind: v.Kind, Host: v.Host, Database: v.Database, User: v.User}
	}
	return out
}

func (c discoverConnections) Add(ctx context.Context, kind, url string, name *string, capture bool, found discover.Found) (any, error) {
	// The connection says it was found by a scan, and where: names only.
	owner := integrations.ScanOwner(found.Project, found.Path, found.Branch, found.Key, found.File)
	view, err := c.m.Add(ctx, integrations.ConnectionInput{Kind: kind, Name: name, URL: &url, Capture: &capture, Owner: &owner})
	if err != nil {
		return nil, discoverProblem(err)
	}
	return view, nil
}

func (c discoverConnections) SetCapture(ctx context.Context, id string, capture bool) (any, error) {
	view, err := c.m.Update(ctx, id, integrations.ConnectionInput{Capture: &capture})
	if err != nil {
		return nil, discoverProblem(err)
	}
	return view, nil
}

func (c discoverConnections) Test(ctx context.Context, kind, url string) any {
	return c.m.Test(ctx, integrations.TestInput{Kind: kind, URL: url})
}

// discoverProblem carries an integrations error across: its code, message
// and hint, which that package has already cleared of any password.
func discoverProblem(err error) *discover.Problem {
	p := integrations.AsProblem(err)
	return &discover.Problem{Code: p.Code, Message: p.Message, Hint: p.Hint}
}
