package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Run is one command started with `leadline run`: what the app lists as
// running now, or as having run a while ago.
type Run struct {
	ID string `json:"id"`
	// The command as it was typed, and split into its arguments.
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Cwd     string   `json:"cwd"`
	// The name its logs and traces are under.
	Service string `json:"service"`
	// The command's process, and the `leadline run` that started it.
	PID       int `json:"pid"`
	RunnerPID int `json:"runner_pid,omitempty"`
	// "node" when the Node provider was loaded into it.
	Provider string `json:"provider,omitempty"`
	// Databases it reaches through a capture proxy.
	Proxies []RunProxy `json:"proxies,omitempty"`
	// Whether it has a terminal, or its output is piped.
	Terminal bool `json:"terminal"`

	Running   bool     `json:"running"`
	StartedMs float64  `json:"started_ms"`
	EndedMs   *float64 `json:"ended_ms,omitempty"`
	// Absent while it runs, and for a run that vanished without saying how
	// it ended (see Lost).
	ExitCode *int `json:"exit_code,omitempty"`
	// The signal that ended it, as in "SIGTERM".
	Signal string `json:"signal,omitempty"`
	// True when `leadline run` itself was gone before it could report the
	// end: killed, or the machine slept through it.
	Lost bool `json:"lost,omitempty"`
}

// RunProxy is a database a run reaches through one of the daemon's proxies.
type RunProxy struct {
	Kind     string `json:"kind"`
	Env      string `json:"env,omitempty"`
	Listen   string `json:"listen"`
	Upstream string `json:"upstream"`
}

const (
	// Ended runs kept for the list. They are a few hundred bytes each and
	// only in memory: a restarted daemon starts with none.
	keepEndedRuns = 50
	// However many are running, the list stops somewhere.
	maxRuns = 500
)

// runs is the daemon's memory of `leadline run` commands.
type runs struct {
	mu   sync.Mutex
	byID map[string]*Run
	// alive reports whether a process still exists; swapped out in tests.
	alive func(pid int) bool
	now   func() time.Time
}

func newRuns() *runs {
	return &runs{byID: map[string]*Run{}, alive: runnerAlive, now: time.Now}
}

var runID = regexp.MustCompile(`^[A-Za-z0-9_-]{4,64}$`)

// start records a run. An id the caller chose is kept, so the run's logs
// can carry it from their first line; starting the same id again replaces
// what was known.
func (rs *runs) start(in Run) (Run, error) {
	in.Command = strings.TrimSpace(in.Command)
	if in.Command == "" {
		return Run{}, errors.New("command is required")
	}
	if in.ID == "" {
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		in.ID = hex.EncodeToString(b)
	} else if !runID.MatchString(in.ID) {
		return Run{}, errors.New("id must be 4 to 64 letters, digits, - or _")
	}
	in.Running, in.EndedMs, in.ExitCode, in.Signal, in.Lost = true, nil, nil, "", false
	in.StartedMs = runMs(rs.now())

	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.byID[in.ID] = &in
	rs.trim()
	return in, nil
}

// end records how a run finished. Ending a run twice keeps the first
// answer.
func (rs *runs) end(id string, exitCode int, signal string) (Run, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	run, ok := rs.byID[id]
	if !ok {
		return Run{}, false
	}
	if run.Running || run.Lost {
		at := runMs(rs.now())
		run.Running, run.Lost = false, false
		run.EndedMs, run.ExitCode, run.Signal = &at, &exitCode, signal
		rs.trim()
	}
	return *run, true
}

// list returns the runs, the running ones first and the newest first within
// each. A run whose `leadline run` is no longer a process is marked lost on
// the way: nothing else would ever end it.
func (rs *runs) list() []Run {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	out := make([]Run, 0, len(rs.byID))
	for _, run := range rs.byID {
		if run.Running && run.RunnerPID > 0 && !rs.alive(run.RunnerPID) {
			at := runMs(rs.now())
			run.Running, run.Lost, run.EndedMs = false, true, &at
		}
		out = append(out, *run)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Running != out[j].Running {
			return out[i].Running
		}
		if out[i].StartedMs != out[j].StartedMs {
			return out[i].StartedMs > out[j].StartedMs
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// trim forgets the oldest ended runs beyond what is kept, and past maxRuns
// the oldest of any kind. The caller holds the lock.
func (rs *runs) trim() {
	var ended, all []*Run
	for _, run := range rs.byID {
		all = append(all, run)
		if !run.Running {
			ended = append(ended, run)
		}
	}
	oldestFirst := func(list []*Run) {
		sort.Slice(list, func(i, j int) bool { return list[i].StartedMs < list[j].StartedMs })
	}
	oldestFirst(ended)
	for i := 0; i < len(ended)-keepEndedRuns; i++ {
		delete(rs.byID, ended[i].ID)
	}
	if len(rs.byID) > maxRuns {
		oldestFirst(all)
		for _, run := range all {
			if len(rs.byID) <= maxRuns {
				break
			}
			delete(rs.byID, run.ID)
		}
	}
}

func runMs(t time.Time) float64 { return float64(t.UnixNano()) / 1e6 }

// runRoutes serves what `leadline run` reports and the app lists:
//
//	POST /api/runs            a command has started
//	POST /api/runs/{id}/end   it has ended: {"exit_code": 0, "signal": ""}
//	GET  /api/runs            {"runs": [...]}, running first, newest first
//
// The runs live in memory, for as long as this handler does.
func (s *Server) runRoutes(mux *http.ServeMux) {
	rs := newRuns()
	mux.HandleFunc("POST /api/runs", func(w http.ResponseWriter, r *http.Request) {
		var in Run
		if !readRunJSON(w, r, &in) {
			return
		}
		run, err := rs.start(in)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.hub.notify()
		writeJSON(w, run)
	})
	mux.HandleFunc("POST /api/runs/{id}/end", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ExitCode int    `json:"exit_code"`
			Signal   string `json:"signal"`
		}
		if !readRunJSON(w, r, &in) {
			return
		}
		run, ok := rs.end(r.PathValue("id"), in.ExitCode, in.Signal)
		if !ok {
			http.Error(w, "no such run", http.StatusNotFound)
			return
		}
		s.hub.notify()
		writeJSON(w, run)
	})
	mux.HandleFunc("GET /api/runs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"runs": rs.list()})
	})
}

// readRunJSON decodes a small JSON body, and answers 400 itself when it is not
// one.
func readRunJSON(w http.ResponseWriter, r *http.Request, into any) bool {
	body := http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(body).Decode(into); err != nil {
		http.Error(w, "not valid JSON: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}
