package api

import (
	"net"
	"net/http"
	"strconv"

	"github.com/sean-brydon/leadline/skills"
)

// setupRoutes serves the agent install: a person pastes one line into a
// coding agent, and the agent fetches the rest from here.
//
//	GET /setup.md           the instructions, as Markdown
//	GET /api/setup/prompt   {"prompt": "...", "url": "...", "setup_url": "..."}
//
// Both name the daemon's own address (selfURL), so the instructions work as
// they are for a daemon on another port, and for one that is being looked
// at from another computer through a forwarded port.
func (s *Server) setupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /setup.md", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte(skills.SetupMarkdown(selfURL(r))))
	})
	mux.HandleFunc("GET /api/setup/prompt", func(w http.ResponseWriter, r *http.Request) {
		url := selfURL(r)
		writeJSON(w, map[string]string{
			"prompt":    skills.SetupPrompt(url),
			"url":       url,
			"setup_url": url + "/setup.md",
		})
	})
}

// selfURL is the daemon's own address: the one a program on the daemon's
// machine reaches it at, which is what a command or an instruction written
// for a person has to name.
//
// That is not always the address the request named. A daemon on a remote
// machine is looked at through a forwarded port (ssh -L, or Shipyard's
// forward), and then the request's Host is the forward's end on the
// viewer's computer: "127.0.0.1:14320" for a daemon that listens on 4320.
// An agent prompt or a `leadline run --url` built from it would point at a
// port that does not exist where it is used. So the port is taken from the
// connection the request arrived on, which is the daemon's listener
// whatever was forwarded in between.
//
// When the request did name the port the daemon listens on, its Host is
// kept as it was spelled ("localhost:4320" stays so): guard has already
// made sure it is this machine's.
func selfURL(r *http.Request) string {
	local, ok := r.Context().Value(http.LocalAddrContextKey).(*net.TCPAddr)
	if !ok || local.Port == 0 {
		// Not a request off a listener (one made in process): its Host is all there is.
		return "http://" + r.Host
	}
	named := "80"
	if _, port, err := net.SplitHostPort(r.Host); err == nil {
		named = port
	}
	if named == strconv.Itoa(local.Port) {
		return "http://" + r.Host
	}
	host := "127.0.0.1"
	if local.IP != nil && !local.IP.IsUnspecified() {
		host = local.IP.String()
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(local.Port))
}
