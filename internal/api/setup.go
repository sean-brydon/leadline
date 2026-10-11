package api

import (
	"net/http"

	"github.com/sean-brydon/leadline/skills"
)

// setupRoutes serves the agent install: a person pastes one line into a
// coding agent, and the agent fetches the rest from here.
//
//	GET /setup.md           the instructions, as Markdown
//	GET /api/setup/prompt   {"prompt": "...", "url": "...", "setup_url": "..."}
//
// Both name the address the request came to, so the instructions work as
// they are for a daemon on another port. guard has already made sure that
// address is this machine's.
func (s *Server) setupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /setup.md", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte(skills.SetupMarkdown(requestURL(r))))
	})
	mux.HandleFunc("GET /api/setup/prompt", func(w http.ResponseWriter, r *http.Request) {
		url := requestURL(r)
		writeJSON(w, map[string]string{
			"prompt":    skills.SetupPrompt(url),
			"url":       url,
			"setup_url": url + "/setup.md",
		})
	})
}

// requestURL is the daemon's address as the caller reached it.
func requestURL(r *http.Request) string {
	return "http://" + r.Host
}
