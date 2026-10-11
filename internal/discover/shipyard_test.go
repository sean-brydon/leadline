package discover

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeBerth writes a script to stand in for Shipyard's command line. It
// notes the arguments of every run in a log, then does what body says.
func fakeBerth(t *testing.T, body string) (cli, log string) {
	t.Helper()
	dir := t.TempDir()
	cli, log = filepath.Join(dir, "berth"), filepath.Join(dir, "args.log")
	script := "#!/bin/sh\necho \"$@\" >> '" + log + "'\n" + body + "\n"
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return cli, log
}

func ran(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// shipyardService is a Service whose berth is the script at cli. The
// script is given a long time: a file written a moment ago can take
// seconds to start the first time on a busy macOS, which checks it first.
func shipyardService(cli string) *Service {
	return New(Options{Processes: NoProcesses(), Berth: func() string { return cli }, BerthTimeout: time.Minute})
}

// What `berth boxes --json` and `berth locations BOX --json` print, with
// made-up boxes. They carry more than Leadline takes, as the real ones do.
const (
	boxesJSON = `[
  {"name":"hetzner-1","address":"100.64.0.11:1376","network":"acme","fingerprint":"aa11bb22cc33dd44","state":"online","latency_ms":86,
   "since":"2026-10-09T08:00:00Z","route":"tailnet","routes":[{"id":"tailnet","state":"up"}]},
  {"name":"build-box","address":"100.64.0.12:1376","fingerprint":"ee55ff66aa77bb88","state":"offline","error":"dial tcp: i/o timeout",
   "since":"2026-10-08T17:30:00Z","retry_at":"2026-10-10T10:00:05Z","attempts":14},
  {"name":"this-mac","address":"127.0.0.1:1376","fingerprint":"0011223344556677","state":"online","since":"2026-10-10T07:00:00Z","local":true},
  {"name":"--json","address":"100.64.0.13:1376","state":"online"},
  {"name":"bad name; rm -rf /","state":"online"}
]`
	locationsJSON = `[
  {"name":"acme","path":"/home/dev/acme","repo":true,"remote":"https://x-access-token:REMOTETOKEN@git.acme.test/acme/acme.git",
   "slug":"acme/acme","default_branch":"main","repo_trust":"trusted","scripts":{"setup":"pnpm install"},
   "worktrees":[
     {"name":"main","path":"/home/dev/acme","branch":"main","head":"3f3761e","main":true,"port":3000},
     {"name":"fix-orders-n1","path":"/home/dev/.berth/worktrees/acme/fix-orders-n1","branch":"fix-orders-n1","head":"9a1b2c3","port":3004,"title":"Fix the orders N+1"}
   ]},
  {"name":"notes","path":"/home/dev/notes","repo":false,"scripts":{}}
]`
)

func berthThatAnswers(t *testing.T) (cli, log string) {
	return fakeBerth(t, `case "$1" in
boxes) cat <<'JSON'
`+boxesJSON+`
JSON
;;
locations) cat <<'JSON'
`+locationsJSON+`
JSON
;;
*) echo "berth: this command must never be run" >&2; exit 64 ;;
esac`)
}

func TestShipyardNotInstalled(t *testing.T) {
	s := shipyardService("")
	got := s.Shipyard(context.Background(), false)
	if got.Installed || got.Available || got.CLI != "" || len(got.Boxes) != 0 || got.Boxes == nil ||
		got.Problem == nil || got.Problem.Code != "shipyard_not_found" || got.CheckedMs == 0 {
		t.Errorf("not installed = %+v", got)
	}
	if _, err := s.BoxProjects(context.Background(), "hetzner-1"); !IsNotFound(err) || asProblem(err).Code != "shipyard_not_found" {
		t.Errorf("box projects with no Shipyard: %v", err)
	}
	// On the daemon's PATH and in none of the usual places: not found.
	t.Setenv("PATH", t.TempDir())
	if at := findBerth(t.TempDir()); at != "" && !strings.HasPrefix(at, "/usr/local/") && !strings.HasPrefix(at, "/opt/homebrew/") && !strings.HasPrefix(at, "/Applications/") {
		t.Errorf("findBerth found %q with an empty PATH and home", at)
	}
}

func TestFindBerth(t *testing.T) {
	// On PATH.
	cli, _ := fakeBerth(t, "")
	t.Setenv("PATH", filepath.Dir(cli))
	if got := findBerth(t.TempDir()); got != cli {
		t.Errorf("on PATH: found %q, want %q", got, cli)
	}
	// Not on PATH, as for a daemon a desktop app started: in the home
	// directory's own bin folder, where Shipyard links it.
	home := t.TempDir()
	t.Setenv("PATH", t.TempDir())
	writeFiles(t, home, map[string]string{".local/bin/berth": "#!/bin/sh\n"})
	if got := findBerth(home); got != "" && strings.HasPrefix(got, home) {
		t.Errorf("a file that can't be run was taken for berth: %q", got)
	}
	if err := os.Chmod(filepath.Join(home, ".local", "bin", "berth"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := findBerth(home); got != filepath.Join(home, ".local", "bin", "berth") {
		t.Errorf("in ~/.local/bin: found %q", got)
	}
	// LEADLINE_BERTH names it outright, whatever is on PATH or at home;
	// "off", or a path with nothing to run at it, is no Shipyard at all.
	t.Setenv("PATH", filepath.Dir(cli))
	t.Setenv("LEADLINE_BERTH", filepath.Join(home, ".local", "bin", "berth"))
	if got := findBerth(home); got != filepath.Join(home, ".local", "bin", "berth") {
		t.Errorf("LEADLINE_BERTH: found %q", got)
	}
	for _, none := range []string{"off", filepath.Join(home, "nowhere"), home} {
		t.Setenv("LEADLINE_BERTH", none)
		if got := findBerth(home); got != "" {
			t.Errorf("LEADLINE_BERTH=%s: found %q", none, got)
		}
	}
}

func TestShipyardBoxes(t *testing.T) {
	cli, log := berthThatAnswers(t)
	s := shipyardService(cli)
	got := s.Shipyard(context.Background(), false)
	if !got.Installed || !got.Available || got.CLI != cli || got.Problem != nil {
		t.Fatalf("listing = %+v", got)
	}
	// The two whose names could be read as something else are left out.
	if len(got.Boxes) != 3 {
		t.Fatalf("boxes = %+v", got.Boxes)
	}
	online, asleep, local := got.Boxes[0], got.Boxes[1], got.Boxes[2]
	if online.Name != "hetzner-1" || online.State != "online" || online.LatencyMs != 86 || online.Local ||
		online.SinceMs != float64(time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC).UnixMilli()) {
		t.Errorf("online box = %+v", online)
	}
	if asleep.Name != "build-box" || asleep.State != "offline" || asleep.Error != "dial tcp: i/o timeout" || asleep.LatencyMs != 0 {
		t.Errorf("offline box = %+v", asleep)
	}
	if !local.Local {
		t.Errorf("local box = %+v", local)
	}
	// Listing the boxes opens nothing, so it does not know whether
	// Leadline is on one, and every box says so in a shape that can later
	// say more.
	for _, b := range got.Boxes {
		if b.Leadline.State != "unknown" || b.Leadline.Installed || b.Leadline.Reachable || b.Leadline.URL != "" || b.Leadline.Note == "" {
			t.Errorf("%s leadline = %+v", b.Name, b.Leadline)
		}
	}
	raw, _ := json.Marshal(got)
	for _, never := range []string{"fingerprint", "aa11bb22cc33dd44", "100.64.0.11", "routes"} {
		if strings.Contains(string(raw), never) {
			t.Errorf("the listing passes on %q", never)
		}
	}
	if !strings.Contains(string(raw), `"leadline":{"state":"unknown","installed":false,"reachable":false,`) {
		t.Errorf("leadline's fields are not all there: %s", raw)
	}

	// Kept briefly: asked again at once, berth is not run again.
	s.Shipyard(context.Background(), false)
	if runs := ran(t, log); len(runs) != 1 || runs[0] != "boxes --json" {
		t.Errorf("berth ran %q", runs)
	}
	s.Shipyard(context.Background(), true)
	if runs := ran(t, log); len(runs) != 2 {
		t.Errorf("fresh did not ask again: %q", runs)
	}
	// And once what was kept is old.
	s.ship.now = func() time.Time { return time.Now().Add(time.Minute) }
	s.Shipyard(context.Background(), false)
	if runs := ran(t, log); len(runs) != 3 {
		t.Errorf("an old answer was not asked for again: %q", runs)
	}
}

func TestShipyardBoxProjects(t *testing.T) {
	cli, log := berthThatAnswers(t)
	s := shipyardService(cli)
	got, err := s.BoxProjects(context.Background(), "hetzner-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Box != "hetzner-1" || len(got.Projects) != 2 || got.CheckedMs == 0 {
		t.Fatalf("projects = %+v", got)
	}
	acme := got.Projects[0]
	if acme.Name != "acme" || acme.Path != "/home/dev/acme" || !acme.Repo || acme.Slug != "acme/acme" || acme.DefaultBranch != "main" || len(acme.Worktrees) != 2 {
		t.Errorf("acme = %+v", acme)
	}
	if wt := acme.Worktrees[1]; wt != (BoxWorktree{Name: "fix-orders-n1", Path: "/home/dev/.berth/worktrees/acme/fix-orders-n1", Branch: "fix-orders-n1", Title: "Fix the orders N+1", Port: 3004}) {
		t.Errorf("worktree = %+v", wt)
	}
	if !acme.Worktrees[0].Main || got.Projects[1].Repo || got.Projects[1].Worktrees == nil {
		t.Errorf("main = %+v, notes = %+v", acme.Worktrees[0], got.Projects[1])
	}
	// A repository's remote can carry a token. It is not passed on.
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "REMOTETOKEN") || strings.Contains(string(raw), "git.acme.test") {
		t.Errorf("the remote URL was passed on: %s", raw)
	}

	// A box Shipyard did not list is not asked about, whatever it is
	// called: no name can become a flag or another command.
	for _, name := range []string{"nope", "--json", "-h", "bad name; rm -rf /", "hetzner-1 --json", "exec", ""} {
		if _, err := s.BoxProjects(context.Background(), name); err == nil || asProblem(err).Code != "no_box" || !IsNotFound(err) {
			t.Errorf("box %q: %v", name, err)
		}
	}
	s.BoxProjects(context.Background(), "hetzner-1") // kept: not run again
	s.BoxProjects(context.Background(), "build-box")

	// In all of that, berth was run with these arguments and no others.
	want := []string{"boxes --json", "locations hetzner-1 --json", "locations build-box --json"}
	if runs := ran(t, log); strings.Join(runs, "|") != strings.Join(want, "|") {
		t.Errorf("berth ran:\n  %s\nwant:\n  %s", strings.Join(runs, "\n  "), strings.Join(want, "\n  "))
	}
}

// Whatever goes wrong, the answer is "not available", in good time, and
// without passing on what berth printed.
func TestShipyardDegrades(t *testing.T) {
	// The ones that never end are given a moment; the ones that answer
	// are given long enough to do it on a busy machine.
	const short, long = 300 * time.Millisecond, 20 * time.Second
	cases := []struct {
		name, body, message string
		timeout             time.Duration
	}{
		{"hangs", "sleep 20", "in time", short},
		{"hangs in a child that holds the pipe", "sleep 20 &\nwait", "in time", short},
		{"prints without end", "yes '[{\"name\":\"a\"},'", "in time", short},
		{"prints garbage", "echo 'Usage: berth <command> STDOUTSECRET'", "can't read", long},
		{"prints nothing", "", "can't read", long},
		{"prints json of another shape", `echo '{"boxes":[]}'`, "can't read", long},
		{"fails", "echo 'berth: agent refused STDERRSECRET' >&2; echo STDOUTSECRET; exit 1", "could not list", long},
	}
	for _, c := range cases {
		cli, _ := fakeBerth(t, c.body)
		s := shipyardService(cli)
		s.ship.boxesTimeout, s.ship.locationsTimeout = c.timeout, c.timeout
		began := time.Now()
		got := s.Shipyard(context.Background(), false)
		took := time.Since(began)
		if !got.Installed || got.Available || len(got.Boxes) != 0 || got.Boxes == nil || got.Problem == nil || got.Problem.Code != "shipyard_failed" {
			t.Errorf("%s: %+v", c.name, got)
			continue
		}
		if !strings.Contains(got.Problem.Message, c.message) {
			t.Errorf("%s: message = %q", c.name, got.Problem.Message)
		}
		if c.timeout == short && took > 5*time.Second {
			t.Errorf("%s: took %s to give up", c.name, took)
		}
		raw, _ := json.Marshal(got)
		if strings.Contains(string(raw), "SECRET") {
			t.Errorf("%s: berth's own words were passed on: %s", c.name, raw)
		}
		if _, err := s.BoxProjects(context.Background(), "hetzner-1"); err == nil || asProblem(err).Code != "shipyard_failed" {
			t.Errorf("%s: box projects: %v", c.name, err)
		}
	}

	// The listing works and one box's projects do not.
	cli, _ := fakeBerth(t, `case "$1" in
boxes) echo '[{"name":"hetzner-1","state":"online"}]' ;;
*) echo 'not json' ;;
esac`)
	s := shipyardService(cli)
	if _, err := s.BoxProjects(context.Background(), "hetzner-1"); err == nil || asProblem(err).Code != "shipyard_failed" || IsNotFound(err) {
		t.Errorf("garbage for locations: %v", err)
	}
}
