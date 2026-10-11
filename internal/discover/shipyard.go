package discover

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// Shipyard is the owner's other product: it runs coding agents on their
// own machines ("boxes"). Its command line, `berth`, answers what boxes
// there are. Leadline asks it two things here and both only read:
//
//	berth boxes --json           the boxes and whether each is online
//	berth locations BOX --json   one box's projects and their worktrees
//
// No other berth command is ever run from this file: the two argument
// lists below are the only ones it builds. task.go has the other two: a
// third that only reads (a box's agents), and the one that starts a task,
// which runs when a person presses Send and at no other time.
type shipyard struct {
	home string
	// find locates the berth command; swapped out in tests.
	find func() string
	// How long each command may take, and how long an answer is kept.
	boxesTimeout, locationsTimeout, keep time.Duration
	// And how long starting a task may take.
	taskTimeout time.Duration
	now         func() time.Time

	mu        sync.Mutex
	boxes     *Shipyard
	boxesAt   time.Time
	locations map[string]cachedLocations
	agents    map[string]cachedAgents

	// sending is held while a task is being started: one at a time.
	sending sync.Mutex
}

type cachedLocations struct {
	out BoxProjects
	at  time.Time
}

func newShipyard(home string) *shipyard {
	s := &shipyard{home: home, boxesTimeout: 4 * time.Second, locationsTimeout: 6 * time.Second, keep: 10 * time.Second,
		taskTimeout: taskTimeout, now: time.Now, locations: map[string]cachedLocations{}, agents: map[string]cachedAgents{}}
	s.find = func() string { return findBerth(home) }
	return s
}

// findBerth looks for the berth command: on PATH, then where Shipyard
// puts it. A daemon started by a desktop app has the PATH of the login
// session, which rarely has ~/.local/bin in it.
//
// LEADLINE_BERTH says where it is instead, for a machine that keeps it
// somewhere else, and "off" says not to look: Shipyard is then not
// installed as far as this daemon goes, and it runs no berth at all.
func findBerth(home string) string {
	if at := os.Getenv("LEADLINE_BERTH"); at != "" {
		if at == "off" {
			return ""
		}
		if st, err := os.Stat(at); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			if abs, err := filepath.Abs(at); err == nil {
				return abs
			}
		}
		return ""
	}
	if path, err := exec.LookPath("berth"); err == nil {
		if abs, err := filepath.Abs(path); err == nil {
			return abs
		}
	}
	var places []string
	if home != "" {
		places = append(places, filepath.Join(home, ".local", "bin", "berth"), filepath.Join(home, "bin", "berth"))
	}
	places = append(places, "/usr/local/bin/berth", "/opt/homebrew/bin/berth",
		"/Applications/Shipyard.app/Contents/MacOS/berth-cli")
	if home != "" {
		places = append(places, filepath.Join(home, "Applications", "Shipyard.app", "Contents", "MacOS", "berth-cli"))
	}
	for _, p := range places {
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

const boxNote = "Leadline can't run on a box yet."

var (
	errNoShipyard = &Problem{Code: "shipyard_not_found", Message: "Shipyard is not installed on this machine."}
	// What a box's name may be, so one can never be read as a flag.
	boxName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// run runs berth with args and returns what it printed. It gets no
// input, a time limit, and a bound on what it may print.
func (s *shipyard) run(ctx context.Context, cli string, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, cli, args...)
	var out limited
	cmd.Stdout = &out
	// When the time is up the process is killed; a child of its that
	// holds the pipe is not waited on for long.
	cmd.WaitDelay = 500 * time.Millisecond
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, &Problem{Code: "shipyard_failed", Message: "Shipyard did not answer in time."}
	}
	if err != nil {
		// What berth printed to explain itself is not passed on: it is
		// another program's text about another program's state.
		return nil, &Problem{Code: "shipyard_failed", Message: "Shipyard's command line could not list that."}
	}
	return out.buf.Bytes(), nil
}

// limited keeps the first megabyte written to it.
type limited struct{ buf bytes.Buffer }

func (l *limited) Write(p []byte) (int, error) {
	if room := 1<<20 - l.buf.Len(); room > 0 {
		l.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// berthBox is what `berth boxes --json` prints for a box, as far as it is
// used here.
type berthBox struct {
	Name      string    `json:"name"`
	State     string    `json:"state"`
	Error     string    `json:"error"`
	LatencyMs int64     `json:"latency_ms"`
	Since     time.Time `json:"since"`
	Local     bool      `json:"local"`
}

// Shipyard reports whether Shipyard is here and lists its boxes. Whatever
// goes wrong, it answers: with Available false, which the app shows as
// "Shipyard not found".
func (s *Service) Shipyard(ctx context.Context, fresh bool) Shipyard {
	sh := s.ship
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if !fresh && sh.boxes != nil && sh.now().Sub(sh.boxesAt) < sh.keep {
		return *sh.boxes
	}
	out := Shipyard{Boxes: []Box{}, CheckedMs: ms(sh.now())}
	cli := sh.find()
	if cli == "" {
		out.Problem = errNoShipyard
	} else {
		out.Installed, out.CLI = true, cli
		boxes, err := sh.listBoxes(ctx, cli)
		if err != nil {
			out.Problem = asProblem(err)
		} else {
			out.Available, out.Boxes = true, boxes
		}
	}
	sh.boxes, sh.boxesAt = &out, sh.now()
	return out
}

func (s *shipyard) listBoxes(ctx context.Context, cli string) ([]Box, error) {
	raw, err := s.run(ctx, cli, s.boxesTimeout, "boxes", "--json")
	if err != nil {
		return nil, err
	}
	var listed []berthBox
	if err := json.Unmarshal(bytes.TrimSpace(raw), &listed); err != nil {
		return nil, &Problem{Code: "shipyard_failed", Message: "Shipyard's command line answered with something Leadline can't read."}
	}
	boxes := []Box{}
	for _, b := range listed {
		if !boxName.MatchString(b.Name) {
			continue
		}
		box := Box{Name: b.Name, State: b.State, Error: clip(b.Error, 200), LatencyMs: b.LatencyMs, Local: b.Local,
			Leadline: BoxLeadline{State: "unsupported", Note: boxNote}}
		if !b.Since.IsZero() {
			box.SinceMs = ms(b.Since)
		}
		boxes = append(boxes, box)
	}
	return boxes, nil
}

// berthLocation is what `berth locations BOX --json` prints for a
// location, as far as it is used here. Its `remote` (the repository's
// origin URL) is left unread on purpose: such a URL can carry a token.
type berthLocation struct {
	Name          string `json:"name"`
	Path          string `json:"path"`
	Repo          bool   `json:"repo"`
	Slug          string `json:"slug"`
	DefaultBranch string `json:"default_branch"`
	Worktrees     []struct {
		Name   string `json:"name"`
		Path   string `json:"path"`
		Branch string `json:"branch"`
		Main   bool   `json:"main"`
		Title  string `json:"title"`
		Port   int    `json:"port"`
	} `json:"worktrees"`
	// The agents the repository's own config adds: their ids and names.
	// Their commands are not read.
	Agents []Agent `json:"agents"`
}

// BoxProjects lists one box's projects and their worktrees. The box must
// be one Shipyard listed.
func (s *Service) BoxProjects(ctx context.Context, box string) (BoxProjects, error) {
	listing := s.Shipyard(ctx, false)
	if !listing.Available {
		if listing.Problem != nil && listing.Installed {
			return BoxProjects{}, listing.Problem
		}
		return BoxProjects{}, errNoShipyard
	}
	known := false
	for _, b := range listing.Boxes {
		known = known || b.Name == box
	}
	if !known || !boxName.MatchString(box) {
		return BoxProjects{}, &Problem{Code: "no_box", Message: "Shipyard has no box of that name."}
	}

	sh := s.ship
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if c, ok := sh.locations[box]; ok && sh.now().Sub(c.at) < sh.keep {
		return c.out, nil
	}
	raw, err := sh.run(ctx, listing.CLI, sh.locationsTimeout, "locations", box, "--json")
	if err != nil {
		return BoxProjects{}, err
	}
	var listed []berthLocation
	if err := json.Unmarshal(bytes.TrimSpace(raw), &listed); err != nil {
		return BoxProjects{}, &Problem{Code: "shipyard_failed", Message: "Shipyard's command line answered with something Leadline can't read."}
	}
	out := BoxProjects{Box: box, Projects: []BoxProject{}, CheckedMs: ms(sh.now())}
	for _, l := range listed {
		p := BoxProject{Name: l.Name, Path: l.Path, Repo: l.Repo, Slug: l.Slug, DefaultBranch: l.DefaultBranch, Worktrees: []BoxWorktree{}}
		for _, w := range l.Worktrees {
			p.Worktrees = append(p.Worktrees, BoxWorktree{Name: w.Name, Path: w.Path, Branch: w.Branch, Main: w.Main, Title: w.Title, Port: w.Port})
		}
		for _, a := range l.Agents {
			if plainName.MatchString(a.ID) {
				p.Agents = append(p.Agents, Agent{ID: a.ID, Name: clip(orID(a.Name, a.ID), 64)})
			}
		}
		out.Projects = append(out.Projects, p)
	}
	sh.locations[box] = cachedLocations{out: out, at: sh.now()}
	return out, nil
}

// IsNotFound reports whether err says that Shipyard, or the box, is not
// there, as against having failed to answer.
func IsNotFound(err error) bool {
	var p *Problem
	return errors.As(err, &p) && (p.Code == "shipyard_not_found" || p.Code == "no_box")
}

func orID(name, id string) string {
	if name == "" {
		return id
	}
	return name
}
