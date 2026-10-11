package discover

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Sending a problem to Shipyard. Two more berth commands are run from this
// file, and they are of two kinds:
//
//	berth agents BOX --json    the agent CLIs a box can start; reads only
//	berth task new BOX/PROJECT/NAME --agent ID --open tab [--base REF] --json --prompt=TEXT
//
// The second changes something in Shipyard: it makes a worktree and starts
// an agent in it. It is run from StartTask and nowhere else, and StartTask
// is called by one route, when a person presses Send. Nothing here
// retries, and nothing runs it on a timer or because data arrived. (The
// only other commands that change anything there open and close a forward
// to a box's Leadline: forward.go.)

// Agent is an agent CLI a box can start.
type Agent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// TaskRequest is a task to start: where, under what name, and what to tell
// the agent.
type TaskRequest struct {
	Box     string `json:"box"`
	Project string `json:"project"`
	// Name is the task's: its worktree's and its branch's.
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
	// Agent is the agent to start. Empty picks the box's first, Claude
	// Code when it has it.
	Agent string `json:"agent,omitempty"`
	// Base is the ref the task's branch starts from. Empty is what the
	// project has checked out.
	Base string `json:"base,omitempty"`
}

// TaskResult is what Shipyard reported about a task it started.
type TaskResult struct {
	Box     string `json:"box"`
	Project string `json:"project"`
	// Name is the worktree's name, and Ref how Shipyard addresses it
	// ("devl/acme/fix-orders").
	Name   string `json:"name"`
	Ref    string `json:"ref"`
	Path   string `json:"path,omitempty"`
	Branch string `json:"branch,omitempty"`
	Base   string `json:"base,omitempty"`
	Agent  string `json:"agent"`
	// Session is the agent's session on the box.
	Session string `json:"session,omitempty"`
	// Service is the name the worktree's telemetry will be filed under in
	// Leadline once it sends any.
	Service string `json:"service"`
	// PromptBytes is how much was sent.
	PromptBytes int `json:"prompt_bytes"`
}

var (
	// A task's name becomes a branch, a folder and a host name. This is
	// narrower than what Shipyard takes: nothing that passes can be read as
	// a flag or as a ref with meaning to git.
	taskName = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)
	// A project, an agent: Shipyard's own names.
	plainName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	// A ref to branch from: a branch, a tag, a commit, origin/main.
	refName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
)

const (
	// MaxPrompt is the longest prompt sent. It travels as one argument of a
	// command, and the operating system bounds those.
	MaxPrompt   = 64 << 10
	maxTaskName = 63
	taskTimeout = 90 * time.Second
)

// Agents lists the agent CLIs a box can start. The box must be one
// Shipyard listed.
func (s *Service) Agents(ctx context.Context, box string) ([]Agent, error) {
	cli, err := s.knownBox(ctx, box)
	if err != nil {
		return nil, err
	}
	sh := s.ship
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if c, ok := sh.agents[box]; ok && sh.now().Sub(c.at) < sh.keep {
		return c.out, nil
	}
	raw, err := sh.run(ctx, cli, sh.locationsTimeout, "agents", box, "--json")
	if err != nil {
		return nil, err
	}
	var listed []Agent
	// A box with none prints null.
	if err := json.Unmarshal(bytes.TrimSpace(raw), &listed); err != nil {
		return nil, &Problem{Code: "shipyard_failed", Message: "Shipyard's command line answered with something Leadline can't read."}
	}
	out := []Agent{}
	for _, a := range listed {
		if !plainName.MatchString(a.ID) {
			continue
		}
		if a.Name == "" {
			a.Name = a.ID
		}
		out = append(out, Agent{ID: a.ID, Name: clip(a.Name, 64)})
	}
	sh.agents[box] = cachedAgents{out: out, at: sh.now()}
	return out, nil
}

type cachedAgents struct {
	out []Agent
	at  time.Time
}

// knownBox returns the berth command when Shipyard is here and lists the
// box, and why not otherwise.
func (s *Service) knownBox(ctx context.Context, box string) (cli string, err error) {
	listing := s.Shipyard(ctx, false)
	if !listing.Available {
		if listing.Problem != nil && listing.Installed {
			return "", listing.Problem
		}
		return "", errNoShipyard
	}
	for _, b := range listing.Boxes {
		if b.Name == box && boxName.MatchString(box) {
			return listing.CLI, nil
		}
	}
	return "", &Problem{Code: "no_box", Message: "Shipyard has no box of that name."}
}

// StartTask starts a Shipyard task: a worktree of a project on a box, with
// an agent in it that is given the prompt. Everything that can be checked
// by reading is checked first, so most refusals are Leadline's own, in
// plain words, and come before anything was changed.
//
// The prompt is handed to berth as one argument of its own. No shell is
// involved and nothing is made of the prompt but that argument, so it may
// hold anything: quotes, newlines, `$( )`, backticks.
func (s *Service) StartTask(ctx context.Context, req TaskRequest) (TaskResult, error) {
	bad := func(code, message, hint string) (TaskResult, error) {
		return TaskResult{}, &Problem{Code: code, Message: message, Hint: hint}
	}
	// The request itself.
	if !taskName.MatchString(req.Name) || len(req.Name) > maxTaskName || strings.HasPrefix(req.Name, "art-") {
		return bad("bad_name", "That is not a name a task can have.", "Use lower-case letters, digits and dashes, starting with a letter, as in fix-orders-n1. A name can't start with art-.")
	}
	if !boxName.MatchString(req.Box) || !plainName.MatchString(req.Project) {
		return bad("bad_request", "A task needs a box and a project.", "")
	}
	if req.Agent != "" && !plainName.MatchString(req.Agent) {
		return bad("bad_request", "That is not an agent's id.", "")
	}
	if req.Base != "" && (!refName.MatchString(req.Base) || strings.Contains(req.Base, "..")) {
		return bad("bad_base", "That is not a branch or a commit a task can start from.", "")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return bad("no_prompt", "There is no prompt to send.", "")
	}
	if len(req.Prompt) > MaxPrompt {
		return bad("prompt_too_long", fmt.Sprintf("The prompt is %d KB, and %d KB is the most that can be sent.", (len(req.Prompt)+1023)>>10, MaxPrompt>>10), "Cut it down, or copy it and paste it to the agent yourself.")
	}
	if strings.ContainsRune(req.Prompt, 0) {
		return bad("bad_prompt", "The prompt holds a NUL character, which can't be sent.", "")
	}

	// Shipyard, the box, the project and the name, from the same listings
	// the app shows.
	listing := s.Shipyard(ctx, false)
	if !listing.Available {
		if listing.Problem != nil && listing.Installed {
			return TaskResult{}, listing.Problem
		}
		return TaskResult{}, errNoShipyard
	}
	var box *Box
	for i := range listing.Boxes {
		if listing.Boxes[i].Name == req.Box {
			box = &listing.Boxes[i]
		}
	}
	if box == nil {
		return bad("no_box", fmt.Sprintf("Shipyard has no box named %s.", req.Box), "")
	}
	if p := boxAway(*box); p != nil {
		return TaskResult{}, p
	}
	projects, err := s.BoxProjects(ctx, req.Box)
	if err != nil {
		return TaskResult{}, err
	}
	var project *BoxProject
	for i := range projects.Projects {
		if projects.Projects[i].Name == req.Project {
			project = &projects.Projects[i]
		}
	}
	if project == nil {
		return bad("no_project", fmt.Sprintf("Shipyard has no project named %s on %s.", req.Project, req.Box), "")
	}
	if !project.Repo {
		return bad("not_a_repo", fmt.Sprintf("%s on %s is not a git repository, so Shipyard can't make a worktree of it.", req.Project, req.Box), "")
	}
	for _, w := range project.Worktrees {
		if w.Name == req.Name || w.Branch == req.Name {
			return TaskResult{}, worktreeExists(req)
		}
	}
	agent := req.Agent
	if agent == "" {
		agents, err := s.Agents(ctx, req.Box)
		if err != nil {
			return TaskResult{}, err
		}
		if agent = defaultAgent(agents); agent == "" {
			return bad("no_agent", fmt.Sprintf("Shipyard found no agent it can start on %s.", req.Box), "Install Claude Code or Codex on the box, or copy the prompt and give it to an agent yourself.")
		}
	}

	ref := req.Box + "/" + req.Project + "/" + req.Name
	args := []string{"task", "new", ref, "--agent", agent, "--open", "tab"}
	if req.Base != "" {
		args = append(args, "--base", req.Base)
	}
	// --prompt=TEXT, one argument: whatever the text starts with or holds,
	// it is the value of that flag and nothing else.
	args = append(args, "--json", "--prompt="+req.Prompt)

	sh := s.ship
	// One at a time: two presses of Send are two tasks only if both names
	// are free, and the second sees what the first made.
	sh.sending.Lock()
	defer sh.sending.Unlock()
	stdout, stderr, err := runTask(ctx, listing.CLI, sh.taskTimeout, args)
	// Whatever happened, the listings are stale now.
	sh.mu.Lock()
	delete(sh.locations, req.Box)
	sh.mu.Unlock()
	if err != nil {
		return TaskResult{}, taskProblem(err, stderr, req, agent)
	}

	out := TaskResult{Box: req.Box, Project: req.Project, Name: req.Name, Ref: ref, Branch: req.Name, Base: req.Base, Agent: agent, PromptBytes: len(req.Prompt)}
	var made struct {
		Worktree struct{ Name, Path, Branch string }
		Session  struct{ Name string }
	}
	if json.Unmarshal(bytes.TrimSpace(stdout), &made) == nil && made.Worktree.Name != "" {
		out.Name, out.Path, out.Branch, out.Session = made.Worktree.Name, clip(made.Worktree.Path, 512), made.Worktree.Branch, made.Session.Name
		out.Ref = req.Box + "/" + req.Project + "/" + out.Name
	}
	// The name its telemetry is filed under: Shipyard's own address for
	// the worktree, without the box (runner.Worktree.Service).
	out.Service = req.Project + "/" + out.Name
	return out, nil
}

// errTimedOut is how runTask says the time was up.
type errTimedOut struct{}

func (errTimedOut) Error() string { return "timed out" }

// runTask runs berth for a task and returns what it printed, each stream
// bounded. It gets no input and no shell. The session Leadline itself may
// have been started from is not passed on: a task sent by a person is
// nobody's hand-off.
func runTask(ctx context.Context, cli string, timeout time.Duration, args []string) (stdout, stderr []byte, err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, cli, args...)
	var out, errs limited
	cmd.Stdout, cmd.Stderr = &out, &errs
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "BERTH_SESSION=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.WaitDelay = 500 * time.Millisecond
	err = cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, nil, errTimedOut{}
	}
	return out.buf.Bytes(), errs.buf.Bytes(), err
}

// boxAway says why a box can't take a task now, or nil when it can.
func boxAway(b Box) *Problem {
	said := ""
	if b.Error != "" {
		said = "Shipyard said: " + b.Error
	}
	switch b.State {
	case "online", "":
		return nil
	case "offline":
		return &Problem{Code: "box_offline", Message: fmt.Sprintf("The box %s is offline.", b.Name), Hint: said}
	case "connecting":
		return &Problem{Code: "box_offline", Message: fmt.Sprintf("Shipyard is still connecting to the box %s.", b.Name), Hint: "Try again in a moment."}
	case "untrusted":
		return &Problem{Code: "box_offline", Message: fmt.Sprintf("The box %s no longer trusts this computer.", b.Name), Hint: "Pair it again in Shipyard."}
	}
	return &Problem{Code: "box_offline", Message: fmt.Sprintf("The box %s is %s.", b.Name, clip(b.State, 32)), Hint: said}
}

func worktreeExists(req TaskRequest) *Problem {
	return &Problem{Code: "worktree_exists", Message: fmt.Sprintf("A worktree named %s already exists in %s on %s.", req.Name, req.Project, req.Box),
		Hint: "Give the task another name."}
}

// defaultAgent is the agent to start when none was chosen.
func defaultAgent(agents []Agent) string {
	for _, a := range agents {
		if a.ID == "claude" {
			return a.ID
		}
	}
	if len(agents) > 0 {
		return agents[0].ID
	}
	return ""
}

// taskProblem turns a failed `berth task new` into words. A person pressed
// Send and has to know why it did not work, so what berth said is passed
// on, as a hint, behind Leadline's own sentence for the cases it knows.
func taskProblem(err error, stderr []byte, req TaskRequest, agent string) *Problem {
	if _, ok := err.(errTimedOut); ok {
		return &Problem{Code: "shipyard_timeout", Message: "Shipyard did not answer in time.",
			Hint: "The task may have started all the same: look in Shipyard before sending it again."}
	}
	said := strings.TrimSpace(strings.TrimPrefix(firstLine(string(stderr)), "berth:"))
	hint := ""
	if said != "" {
		hint = "Shipyard said: " + clip(said, 300)
	}
	low := strings.ToLower(said)
	has := func(parts ...string) bool {
		for _, p := range parts {
			if strings.Contains(low, p) {
				return true
			}
		}
		return false
	}
	switch {
	case has("already exists", "already used by worktree", "already checked out"):
		p := worktreeExists(req)
		p.Message = fmt.Sprintf("A worktree or a branch named %s already exists in %s on %s.", req.Name, req.Project, req.Box)
		return p
	case has("no paired box"):
		return &Problem{Code: "no_box", Message: fmt.Sprintf("Shipyard has no box named %s.", req.Box)}
	case has("is offline", "i/o timeout", "connection refused", "no route to host", "unreachable", "not reachable"):
		return &Problem{Code: "box_offline", Message: fmt.Sprintf("The box %s is offline, or did not answer.", req.Box), Hint: hint}
	case has("unknown agent"):
		return &Problem{Code: "no_agent", Message: fmt.Sprintf("The box %s can't start the agent %s.", req.Box, agent), Hint: hint}
	case has("no location with that name"):
		return &Problem{Code: "no_project", Message: fmt.Sprintf("Shipyard has no project named %s on %s.", req.Project, req.Box)}
	case has("not a git repository"):
		return &Problem{Code: "not_a_repo", Message: fmt.Sprintf("%s on %s is not a git repository, so Shipyard can't make a worktree of it.", req.Project, req.Box)}
	case has("invalid worktree name", "worktree names can't"):
		return &Problem{Code: "bad_name", Message: "Shipyard does not take that name for a worktree.", Hint: hint}
	case has("tmux is not installed"):
		return &Problem{Code: "shipyard_failed", Message: fmt.Sprintf("tmux is not installed on the box %s, and Shipyard runs its agents in it.", req.Box)}
	case has("git worktree add", "git fetch"):
		return &Problem{Code: "git_failed", Message: "git could not make the worktree.", Hint: hint}
	}
	if hint == "" {
		hint = "Its command line ended without saying why."
	}
	return &Problem{Code: "shipyard_failed", Message: "Shipyard could not start the task.", Hint: hint}
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}
