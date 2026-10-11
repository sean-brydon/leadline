package discover

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sean-brydon/leadline/internal/discover/fakeberth"
)

// fleet is a Service whose berth is the fake one, with its made-up boxes.
func fleet(t *testing.T) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	cli, err := fakeberth.Write(dir)
	if err != nil {
		t.Fatal(err)
	}
	return New(Options{Processes: NoProcesses(), Berth: func() string { return cli }, BerthTimeout: time.Minute, TaskTimeout: time.Minute}), dir
}

func set(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A prompt that would go wrong in every way a shell, a flag parser or a
// careless join could make it go wrong.
const awkwardPrompt = "--agent codex\n" +
	"Fix the N+1 on `order_items` in \"GET /api/orders\"; it's $(rm -rf ~) `whoami` $HOME ${PATH}\n" +
	"\tSELECT * FROM t WHERE a = 'x' AND b = \"y\" -- comment\n" +
	"100% done && echo no | cat > /tmp/x; \\n is not a newline, \\\\ is two\n" +
	"--\n" +
	"-- BOX/LOC/NAME hetzner-1/acme/other --prompt=other --json\n" +
	"unicode: é ✓ 日本語 🚢, a trailing space and blank lines follow \n\n\n"

func TestStartTaskPromptArrivesByteForByte(t *testing.T) {
	s, dir := fleet(t)
	// A session this daemon might have been started from is not passed on.
	t.Setenv("BERTH_SESSION", "acme-main-claude")
	got, err := s.StartTask(context.Background(), TaskRequest{Box: "hetzner-1", Project: "acme", Name: "fix-n1-order-items-k3f9", Prompt: awkwardPrompt, Agent: "codex", Base: "fix-orders"})
	if err != nil {
		t.Fatal(err)
	}
	tasks := fakeberth.Tasks(dir)
	if len(tasks) != 1 {
		t.Fatalf("berth task new ran %d times", len(tasks))
	}
	want := []string{"task", "new", "hetzner-1/acme/fix-n1-order-items-k3f9", "--agent", "codex", "--open", "tab", "--base", "fix-orders", "--json", "--prompt=" + awkwardPrompt}
	if len(tasks[0]) != len(want) {
		t.Fatalf("berth got %d arguments, want %d: %q", len(tasks[0]), len(want), tasks[0])
	}
	for i := range want {
		if tasks[0][i] != want[i] {
			t.Errorf("argument %d:\n got %q\nwant %q", i, tasks[0][i], want[i])
		}
	}
	// The box reference is the first argument after the command words, so
	// berth takes it for the box whatever the prompt holds.
	if tasks[0][2] != "hetzner-1/acme/fix-n1-order-items-k3f9" {
		t.Errorf("the reference is not first: %q", tasks[0])
	}
	if env := fakeberth.TaskEnv(dir, 0); strings.Contains(env, "BERTH_SESSION=") {
		t.Errorf("BERTH_SESSION was passed on")
	}
	want2 := TaskResult{Box: "hetzner-1", Project: "acme", Name: "fix-n1-order-items-k3f9", Ref: "hetzner-1/acme/fix-n1-order-items-k3f9",
		Path: "/home/dev/acme-fix-n1-order-items-k3f9", Branch: "fix-n1-order-items-k3f9", Base: "fix-orders", Agent: "codex",
		Session: "acme-fix-n1-order-items-k3f9-claude", Service: "acme/fix-n1-order-items-k3f9", PromptBytes: len(awkwardPrompt)}
	if got != want2 {
		t.Errorf("result:\n got %+v\nwant %+v", got, want2)
	}
	// Nothing of the prompt is in the answer.
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "whoami") {
		t.Errorf("the answer carries the prompt: %s", raw)
	}
}

// With no agent chosen the box's own list decides: Claude Code when it is
// there, else the first; and no base means none is named.
func TestStartTaskDefaults(t *testing.T) {
	s, dir := fleet(t)
	got, err := s.StartTask(context.Background(), TaskRequest{Box: "hetzner-1", Project: "storefront", Name: "fix-slow-search", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	args := fakeberth.Tasks(dir)[0]
	if strings.Join(args, " ") != "task new hetzner-1/storefront/fix-slow-search --agent claude --open tab --json --prompt=p" {
		t.Errorf("args = %q", args)
	}
	if got.Agent != "claude" || got.Base != "" || got.Service != "storefront/fix-slow-search" {
		t.Errorf("result = %+v", got)
	}

	s, dir = fleet(t)
	set(t, dir, "agents.json", `[{"id":"codex","name":"Codex"},{"id":"gemini","name":"Gemini CLI"}]`)
	if got, err = s.StartTask(context.Background(), TaskRequest{Box: "hetzner-1", Project: "acme", Name: "fix-x", Prompt: "p"}); err != nil || got.Agent != "codex" {
		t.Errorf("no claude: %+v, %v", got, err)
	}

	// What berth prints without --json's shape is still an answer: the
	// names asked for stand.
	s, dir = fleet(t)
	set(t, dir, "task.sh", "echo 'Created acme/fix-y at /home/dev/acme-fix-y on fix-y'\necho 'Started session acme-fix-y-claude'\nexit 0\n")
	if got, err = s.StartTask(context.Background(), TaskRequest{Box: "hetzner-1", Project: "acme", Name: "fix-y", Prompt: "p"}); err != nil ||
		got.Name != "fix-y" || got.Ref != "hetzner-1/acme/fix-y" || got.Branch != "fix-y" || got.Service != "acme/fix-y" {
		t.Errorf("plain output: %+v, %v", got, err)
	}
}

// Every way it can fail, in words, and whether berth was asked to start
// anything on the way.
func TestStartTaskErrors(t *testing.T) {
	ok := TaskRequest{Box: "hetzner-1", Project: "acme", Name: "fix-orders-n1", Prompt: "Fix it.", Agent: "claude"}
	with := func(change func(*TaskRequest)) TaskRequest {
		r := ok
		change(&r)
		return r
	}
	cases := []struct {
		name string
		req  TaskRequest
		// arrange changes the fake before the request.
		arrange       func(t *testing.T, dir string)
		code, message string
		hint          string
		// ran: berth task new was run.
		ran bool
	}{
		{name: "the box is offline", req: with(func(r *TaskRequest) { r.Box = "build-box" }),
			code: "box_offline", message: "The box build-box is offline.", hint: "Shipyard said: dial tcp: i/o timeout"},
		{name: "the box is connecting", req: ok, arrange: func(t *testing.T, dir string) {
			set(t, dir, "boxes.json", `[{"name":"hetzner-1","state":"connecting"}]`)
		}, code: "box_offline", message: "Shipyard is still connecting to the box hetzner-1.", hint: "Try again in a moment."},
		{name: "the box is untrusted", req: ok, arrange: func(t *testing.T, dir string) {
			set(t, dir, "boxes.json", `[{"name":"hetzner-1","state":"untrusted"}]`)
		}, code: "box_offline", message: "The box hetzner-1 no longer trusts this computer.", hint: "Pair it again in Shipyard."},
		{name: "no such box", req: with(func(r *TaskRequest) { r.Box = "devl" }), code: "no_box", message: "Shipyard has no box named devl."},
		{name: "no such project", req: with(func(r *TaskRequest) { r.Project = "shop" }), code: "no_project", message: "Shipyard has no project named shop on hetzner-1."},
		{name: "not a repository", req: with(func(r *TaskRequest) { r.Project = "notes" }), code: "not_a_repo", message: "notes on hetzner-1 is not a git repository, so Shipyard can't make a worktree of it."},
		{name: "the worktree is there", req: with(func(r *TaskRequest) { r.Name = "fix-orders" }),
			code: "worktree_exists", message: "A worktree named fix-orders already exists in acme on hetzner-1.", hint: "Give the task another name."},
		{name: "the branch is there, which only git knows", req: ok, arrange: func(t *testing.T, dir string) {
			set(t, dir, "task.sh", "echo \"berth: git worktree add: fatal: a branch named 'fix-orders-n1' already exists\" >&2\nexit 1\n")
		}, code: "worktree_exists", message: "A worktree or a branch named fix-orders-n1 already exists in acme on hetzner-1.", hint: "Give the task another name.", ran: true},
		{name: "the name is a flag", req: with(func(r *TaskRequest) { r.Name = "--prompt=x" }), code: "bad_name", message: "That is not a name a task can have."},
		{name: "the name has a slash", req: with(func(r *TaskRequest) { r.Name = "fix/orders" }), code: "bad_name", message: "That is not a name a task can have."},
		{name: "the name has capitals", req: with(func(r *TaskRequest) { r.Name = "Fix-Orders" }), code: "bad_name", message: "That is not a name a task can have."},
		{name: "the name is empty", req: with(func(r *TaskRequest) { r.Name = "" }), code: "bad_name", message: "That is not a name a task can have."},
		{name: "the name is kept for artifacts", req: with(func(r *TaskRequest) { r.Name = "art-orders" }), code: "bad_name", message: "That is not a name a task can have."},
		{name: "the name is too long", req: with(func(r *TaskRequest) { r.Name = "fix-" + strings.Repeat("a", 70) }), code: "bad_name", message: "That is not a name a task can have."},
		{name: "the base is a flag", req: with(func(r *TaskRequest) { r.Base = "--upload-pack=x" }), code: "bad_base", message: "That is not a branch or a commit a task can start from."},
		{name: "the base is a range", req: with(func(r *TaskRequest) { r.Base = "main..fix" }), code: "bad_base", message: "That is not a branch or a commit a task can start from."},
		{name: "the agent is a flag", req: with(func(r *TaskRequest) { r.Agent = "-x" }), code: "bad_request", message: "That is not an agent's id."},
		{name: "no prompt", req: with(func(r *TaskRequest) { r.Prompt = " \n\t" }), code: "no_prompt", message: "There is no prompt to send."},
		{name: "the prompt is too long", req: with(func(r *TaskRequest) { r.Prompt = strings.Repeat("a", MaxPrompt+1) }),
			code: "prompt_too_long", message: "The prompt is 65 KB, and 64 KB is the most that can be sent."},
		{name: "the prompt holds a NUL", req: with(func(r *TaskRequest) { r.Prompt = "a\x00b" }), code: "bad_prompt", message: "The prompt holds a NUL character, which can't be sent."},
		{name: "the box has no agent", req: with(func(r *TaskRequest) { r.Agent = "" }), arrange: func(t *testing.T, dir string) {
			set(t, dir, "agents.json", "null\n")
		}, code: "no_agent", message: "Shipyard found no agent it can start on hetzner-1."},
		{name: "the box can't start that agent", req: with(func(r *TaskRequest) { r.Agent = "zed" }), arrange: func(t *testing.T, dir string) {
			set(t, dir, "task.sh", "echo 'berth: unknown agent \"zed\"' >&2\nexit 1\n")
		}, code: "no_agent", message: "The box hetzner-1 can't start the agent zed.", hint: `Shipyard said: unknown agent "zed"`, ran: true},
		{name: "the box went away meanwhile", req: ok, arrange: func(t *testing.T, dir string) {
			set(t, dir, "task.sh", "echo 'berth: hetzner-1 is offline; Shipyard is reconnecting' >&2\nexit 1\n")
		}, code: "box_offline", message: "The box hetzner-1 is offline, or did not answer.", hint: "Shipyard said: hetzner-1 is offline; Shipyard is reconnecting", ran: true},
		{name: "a hook refused it", req: ok, arrange: func(t *testing.T, dir string) {
			set(t, dir, "task.sh", "echo 'berth: before:task.create refused: no tasks on Fridays' >&2\necho STDOUTSECRET\nexit 1\n")
		}, code: "shipyard_failed", message: "Shipyard could not start the task.", hint: "Shipyard said: before:task.create refused: no tasks on Fridays", ran: true},
		{name: "git failed", req: ok, arrange: func(t *testing.T, dir string) {
			set(t, dir, "task.sh", "echo \"berth: git worktree add: fatal: invalid reference: nope\" >&2\nexit 1\n")
		}, code: "git_failed", message: "git could not make the worktree.", hint: "Shipyard said: git worktree add: fatal: invalid reference: nope", ran: true},
		{name: "it fails without a word", req: ok, arrange: func(t *testing.T, dir string) {
			set(t, dir, "task.sh", "exit 3\n")
		}, code: "shipyard_failed", message: "Shipyard could not start the task.", hint: "Its command line ended without saying why.", ran: true},
		{name: "it never answers", req: ok, arrange: func(t *testing.T, dir string) {
			set(t, dir, "task.sh", "sleep 30\n")
		}, code: "shipyard_timeout", message: "Shipyard did not answer in time.", hint: "The task may have started all the same: look in Shipyard before sending it again.", ran: true},
	}
	for _, c := range cases {
		s, dir := fleet(t)
		if c.arrange != nil {
			c.arrange(t, dir)
		}
		if c.code == "shipyard_timeout" {
			// Long enough for the script to start on a busy machine, and
			// far short of its sleep.
			s.ship.taskTimeout = 3 * time.Second
		}
		got, err := s.StartTask(context.Background(), c.req)
		p := asProblem(err)
		if err == nil || p.Code != c.code || p.Message != c.message || (c.hint != "" && p.Hint != c.hint) {
			t.Errorf("%s:\n got %v %+v (result %+v)\nwant %s: %s (%s)", c.name, err, p, got, c.code, c.message, c.hint)
		}
		if strings.Contains(p.Message+p.Hint, "STDOUTSECRET") {
			t.Errorf("%s: berth's output was passed on: %+v", c.name, p)
		}
		if ran := len(fakeberth.Tasks(dir)) == 1; ran != c.ran {
			t.Errorf("%s: berth task new ran = %v, want %v (runs %q)", c.name, ran, c.ran, fakeberth.Runs(dir))
		}
	}

	// Shipyard is not installed.
	s := New(Options{Processes: NoProcesses(), Berth: func() string { return "" }})
	if _, err := s.StartTask(context.Background(), ok); asProblem(err).Code != "shipyard_not_found" || asProblem(err).Message != "Shipyard is not installed on this machine." {
		t.Errorf("not installed: %v", err)
	}
	if _, err := s.Agents(context.Background(), "hetzner-1"); asProblem(err).Code != "shipyard_not_found" {
		t.Errorf("agents, not installed: %v", err)
	}
	// Shipyard is there and does not answer.
	dir := t.TempDir()
	cli := filepath.Join(dir, "berth")
	os.WriteFile(cli, []byte("#!/bin/sh\necho 'berth: agent refused' >&2\nexit 1\n"), 0o755)
	s = New(Options{Processes: NoProcesses(), Berth: func() string { return cli }, BerthTimeout: time.Minute})
	if _, err := s.StartTask(context.Background(), ok); asProblem(err).Code != "shipyard_failed" {
		t.Errorf("berth fails: %v", err)
	}
}

func TestAgents(t *testing.T) {
	s, dir := fleet(t)
	got, err := s.Agents(context.Background(), "hetzner-1")
	if err != nil || len(got) != 2 || got[0] != (Agent{ID: "claude", Name: "Claude Code"}) || got[1] != (Agent{ID: "codex", Name: "Codex"}) {
		t.Fatalf("agents = %+v, %v", got, err)
	}
	s.Agents(context.Background(), "hetzner-1") // kept: not run again
	if _, err := s.Agents(context.Background(), "--json"); asProblem(err).Code != "no_box" {
		t.Errorf("a box that is not listed: %v", err)
	}
	if runs := strings.Join(fakeberth.Runs(dir), "|"); runs != "boxes|agents hetzner-1" {
		t.Errorf("berth ran %q", runs)
	}
	// A project's own agents come with the project, by id and name only.
	projects, err := s.BoxProjects(context.Background(), "hetzner-1")
	if err != nil || len(projects.Projects[0].Agents) != 1 || projects.Projects[0].Agents[0] != (Agent{ID: "reviewer", Name: "Acme reviewer"}) {
		t.Errorf("project agents = %+v, %v", projects.Projects, err)
	}
	raw, _ := json.Marshal(projects)
	if strings.Contains(string(raw), "REVIEWSECRET") || strings.Contains(string(raw), "REMOTETOKEN") {
		t.Errorf("an agent's command or a remote was passed on: %s", raw)
	}
}

// Where a task would start, in the three cases: the service was attached by
// a Shipyard worktree; its folder is a Shipyard project's; neither.
func TestTargets(t *testing.T) {
	ctx := context.Background()

	// 1. Attached by a Shipyard worktree: its box and project, starting
	// from the worktree's own branch.
	s, dir := fleet(t)
	got := s.Targets(ctx, TargetSource{Service: "acme/fix-orders", Via: "shipyard", Project: "acme", Worktree: "fix-orders", Branch: "fix-orders", Box: "hetzner-1", Path: "/home/dev/acme-fix-orders"}, "")
	if !got.Installed || !got.Available || got.Suggested == nil {
		t.Fatalf("worktree: %+v", got)
	}
	if *got.Suggested != (Target{Box: "hetzner-1", Project: "acme", Base: "fix-orders", How: "worktree",
		Why:     "acme/fix-orders is the worktree fix-orders of acme on hetzner-1, attached by Shipyard.",
		BaseWhy: "The problem was recorded running the branch fix-orders, so the task starts from it. What is not committed there is not in it."}) {
		t.Errorf("worktree target = %+v", *got.Suggested)
	}
	if got.Box != "hetzner-1" || len(got.Projects) != 3 || len(got.Agents) != 2 || len(got.Boxes) != 3 || got.BoxProblem != nil {
		t.Errorf("worktree listing = %+v", got)
	}
	if runs := strings.Join(fakeberth.Runs(dir), "|"); runs != "boxes|locations hetzner-1|agents hetzner-1" {
		t.Errorf("berth ran %q", runs)
	}
	// The project's own checkout: no branch is named.
	got = s.Targets(ctx, TargetSource{Service: "acme", Via: "shipyard", Project: "acme", Branch: "main", Box: "hetzner-1"}, "")
	if got.Suggested == nil || got.Suggested.Base != "" || got.Suggested.How != "worktree" || !strings.Contains(got.Suggested.BaseWhy, "own checkout") {
		t.Errorf("main checkout = %+v", got.Suggested)
	}
	// A worktree that has since been removed: the project still, and no branch.
	got = s.Targets(ctx, TargetSource{Service: "acme/gone", Via: "shipyard", Project: "acme", Worktree: "gone", Branch: "gone", Box: "hetzner-1"}, "")
	if got.Suggested == nil || got.Suggested.Project != "acme" || got.Suggested.Base != "" || !strings.Contains(got.Suggested.BaseWhy, "no longer there") {
		t.Errorf("removed worktree = %+v", got.Suggested)
	}
	// The box it names is not one of this computer's: the online boxes are
	// asked for a project of that name.
	got = s.Targets(ctx, TargetSource{Service: "storefront/x", Via: "shipyard", Project: "storefront", Worktree: "x", Box: "somewhere-else"}, "")
	if got.Suggested == nil || got.Suggested.Project != "storefront" || got.Suggested.How != "worktree" {
		t.Errorf("unknown box = %+v", got.Suggested)
	}

	// Its own box is offline: still that box, which says why it can't go
	// there now, and no other box is asked for a project of the same name.
	s, dir = fleet(t)
	got = s.Targets(ctx, TargetSource{Service: "acme/fix-orders", Via: "shipyard", Project: "acme", Worktree: "fix-orders", Branch: "fix-orders", Box: "build-box", Path: "/home/dev/acme-fix-orders"}, "")
	if got.Suggested == nil || got.Suggested.Box != "build-box" || got.Suggested.Project != "acme" || got.Suggested.Base != "" || got.Box != "build-box" ||
		got.BoxProblem == nil || got.BoxProblem.Message != "The box build-box is offline." || len(got.Projects) != 0 {
		t.Errorf("worktree on an offline box = %+v, %+v", got.Suggested, got)
	}
	if runs := strings.Join(fakeberth.Runs(dir), "|"); runs != "boxes" {
		t.Errorf("berth ran %q", runs)
	}

	// 2. Not attached: the folder its commands ran in is a project's, or
	// one of its worktrees', or inside one.
	s, _ = fleet(t)
	got = s.Targets(ctx, TargetSource{Service: "storefront-web", Folders: []string{"/home/dev/storefront/apps/web"}}, "")
	if got.Suggested == nil || *got.Suggested != (Target{Box: "this-mac", Project: "storefront", How: "folder",
		Why: "Its folder is the project storefront on this-mac.", BaseWhy: "It runs the project's own checkout, so the task starts from what that has checked out."}) {
		t.Errorf("folder target = %+v", got.Suggested)
	}
	got = s.Targets(ctx, TargetSource{Service: "api", Via: "cli", Project: "api", Path: "/home/dev/acme-fix-orders"}, "")
	if got.Suggested == nil || got.Suggested.Project != "acme" || got.Suggested.Base != "fix-orders" || got.Suggested.How != "folder" ||
		got.Suggested.Why != "Its folder is the worktree fix-orders of acme on this-mac." {
		t.Errorf("worktree folder target = %+v", got.Suggested)
	}
	// A folder that only starts with a project's path is not inside it.
	got = s.Targets(ctx, TargetSource{Service: "x", Folders: []string{"/home/dev/acme-other", "/home/dev", "/"}}, "")
	if got.Suggested != nil {
		t.Errorf("a neighbouring folder matched: %+v", got.Suggested)
	}
	// Recorded on another machine: this computer's own box is asked last.
	got = s.Targets(ctx, TargetSource{Service: "storefront-web", Folders: []string{"/home/dev/storefront"}, Remote: true}, "")
	if got.Suggested == nil || got.Suggested.Box != "hetzner-1" {
		t.Errorf("remote folder target = %+v", got.Suggested)
	}

	// 3. Nothing ties it to a project: the person picks, from the first
	// online box's projects, or another box's when they ask.
	s, dir = fleet(t)
	got = s.Targets(ctx, TargetSource{Service: "mystery"}, "")
	if got.Suggested != nil || got.Box != "this-mac" || len(got.Projects) != 3 || len(got.Agents) != 2 {
		t.Errorf("no match = %+v", got)
	}
	got = s.Targets(ctx, TargetSource{Service: "mystery"}, "hetzner-1")
	if got.Suggested != nil || got.Box != "hetzner-1" || len(got.Projects) != 3 {
		t.Errorf("another box = %+v", got)
	}
	got = s.Targets(ctx, TargetSource{Service: "mystery"}, "build-box")
	if got.Box != "build-box" || len(got.Projects) != 0 || got.BoxProblem == nil || got.BoxProblem.Message != "The box build-box is offline." {
		t.Errorf("an offline box = %+v", got)
	}
	got = s.Targets(ctx, TargetSource{}, "nope")
	if got.BoxProblem == nil || got.BoxProblem.Code != "no_box" {
		t.Errorf("an unknown box = %+v", got)
	}
	// In all of that nothing but the three reading commands ran.
	for _, run := range fakeberth.Runs(dir) {
		if w := strings.Fields(run)[0]; w != "boxes" && w != "locations" && w != "agents" {
			t.Errorf("berth ran %q", run)
		}
	}
	if len(fakeberth.Tasks(dir)) != 0 {
		t.Errorf("listing targets started a task")
	}

	// Shipyard is not here: said, and nothing else.
	s = New(Options{Processes: NoProcesses(), Berth: func() string { return "" }})
	got = s.Targets(ctx, TargetSource{Service: "acme", Via: "shipyard", Project: "acme"}, "")
	if got.Installed || got.Available || got.Suggested != nil || got.Problem == nil || got.Problem.Code != "shipyard_not_found" || got.Boxes == nil || got.Projects == nil || got.Agents == nil {
		t.Errorf("not installed = %+v", got)
	}
}
