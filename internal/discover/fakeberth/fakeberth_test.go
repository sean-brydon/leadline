package fakeberth

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The fake answers each command Leadline runs with JSON of the real one's
// shape, writes down a task's arguments exactly, and refuses anything else.
//
// FAKEBERTH_DIR=/some/folder keeps the script there instead of in a
// temporary folder, for trying a daemon against it by hand:
//
//	FAKEBERTH_DIR=/tmp/fake go test ./internal/discover/fakeberth
//	LEADLINE_BERTH=/tmp/fake/berth leadlined -addr 127.0.0.1:4348 -data :memory: -otlp off
func TestFake(t *testing.T) {
	dir := os.Getenv("FAKEBERTH_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	cli, err := Write(dir)
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, error) {
		out, err := exec.Command(cli, args...).Output()
		return string(out), err
	}
	for _, c := range [][]string{{"boxes", "--json"}, {"locations", "hetzner-1", "--json"}, {"agents", "hetzner-1", "--json"}} {
		out, err := run(c...)
		var list []map[string]any
		if err != nil || json.Unmarshal([]byte(out), &list) != nil || len(list) < 2 {
			t.Errorf("berth %v: %v\n%s", c, err, out)
		}
	}
	prompt := "two\nlines, \"quoted\", $(not run) and `not run`\n"
	out, err := run("task", "new", "hetzner-1/acme/fix-x", "--agent", "claude", "--json", "--prompt="+prompt)
	var task struct {
		Worktree struct{ Name, Path, Branch string }
		Session  struct{ Name string }
	}
	if err != nil || json.Unmarshal([]byte(out), &task) != nil || task.Worktree.Name != "fix-x" || task.Worktree.Path != "/home/dev/acme-fix-x" || task.Session.Name != "acme-fix-x-claude" {
		t.Errorf("berth task new: %v\n%s", err, out)
	}
	tasks := Tasks(dir)
	if len(tasks) != 1 || len(tasks[0]) != 7 || tasks[0][6] != "--prompt="+prompt {
		t.Errorf("tasks = %q", tasks)
	}
	if _, err := run("exec", "hetzner-1/acme", "--", "rm", "-rf", "/"); err == nil {
		t.Errorf("a command Leadline never runs was answered")
	}
	if runs := strings.Join(Runs(dir), "|"); runs != "boxes|locations hetzner-1|agents hetzner-1|task new hetzner-1/acme/fix-x|other exec hetzner-1/acme -- rm -rf /" {
		t.Errorf("runs = %q", runs)
	}
	if os.Getenv("FAKEBERTH_DIR") != "" {
		// Left as new for whoever asked for it.
		os.Remove(dir + "/runs.log")
		for _, f := range []string{"task-0.args", "task-0.env"} {
			os.Remove(dir + "/" + f)
		}
	}
}

// Forwards: opened, listed and closed as the real command answers, with
// nothing forwarded anywhere.
func TestFakeForwards(t *testing.T) {
	dir := t.TempDir()
	cli, err := Write(dir)
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, string, error) {
		cmd := exec.Command(cli, args...)
		var errs strings.Builder
		cmd.Stderr = &errs
		out, err := cmd.Output()
		return string(out), strings.TrimSpace(errs.String()), err
	}
	type forward struct {
		ID, Box       string
		Local, Remote int
		State, Error  string
	}
	list := func() []forward {
		t.Helper()
		out, _, err := run("forwards", "--json")
		var got []forward
		if err != nil || json.Unmarshal([]byte(out), &got) != nil {
			t.Fatalf("berth forwards --json: %v\n%s", err, out)
		}
		return got
	}
	if got := list(); len(got) != 0 {
		t.Errorf("forwards to begin with: %+v", got)
	}
	out, _, err := run("forward", "hetzner-1", "14321:4320", "--json")
	var opened []forward
	if err != nil || json.Unmarshal([]byte(out), &opened) != nil || len(opened) != 1 || opened[0] != (forward{ID: "f0000000", Box: "hetzner-1", Local: 14321, Remote: 4320}) {
		t.Fatalf("berth forward: %v\n%s", err, out)
	}
	// A second on the same local port is refused in the real one's words, as is a box it does not have.
	if _, said, err := run("forward", "hetzner-1", "14321:3000", "--json"); err == nil || !strings.Contains(said, "local port 14321 is in use") {
		t.Errorf("a taken port: %v %q", err, said)
	}
	if _, said, err := run("forward", "nowhere", "14322:4320", "--json"); err == nil || said != `berth: localhost:14322: no paired box named "nowhere"` {
		t.Errorf("an unknown box: %v %q", err, said)
	}
	// A hook can fail it, or do something as it opens.
	os.WriteFile(dir+"/forward.sh", []byte("echo \"$box $local $remote\" > \"$dir/hook.txt\"\n"), 0o644)
	if _, _, err := run("forward", "build-box", "14322:4320", "--json"); err != nil {
		t.Errorf("a forward to an offline box opens, as the real one's does: %v", err)
	}
	if b, _ := os.ReadFile(dir + "/hook.txt"); strings.TrimSpace(string(b)) != "build-box 14322 4320" {
		t.Errorf("forward.sh saw %q", b)
	}
	os.WriteFile(dir+"/forwards.tsv", []byte("f0000000 hetzner-1 14321 4320 listening\nf0000001 build-box 14322 4320 failed local port 14322 is in use\n"), 0o644)
	got := list()
	if len(got) != 2 || got[0].State != "listening" || got[1] != (forward{ID: "f0000001", Box: "build-box", Local: 14322, Remote: 4320, State: "failed", Error: "local port 14322 is in use"}) {
		t.Errorf("forwards = %+v", got)
	}
	if out, _, err := run("unforward", "f0000000"); err != nil || strings.TrimSpace(out) != "Stopped localhost:14321 → hetzner-1:4320" {
		t.Errorf("unforward: %v %q", err, out)
	}
	if _, said, err := run("unforward", "f0000000"); err == nil || said != "berth: no forward with that id" {
		t.Errorf("unforward twice: %v %q", err, said)
	}
	if left := Forwards(dir); len(left) != 1 || left[0] != (Forward{ID: "f0000001", Box: "build-box", Local: 14322, Remote: 4320, State: "failed"}) {
		t.Errorf("left = %+v", left)
	}
	if runs := strings.Join(Runs(dir), "|"); runs != "forwards|forward hetzner-1 14321:4320|forward hetzner-1 14321:3000|forward nowhere 14322:4320|forward build-box 14322:4320|forwards|unforward f0000000|unforward f0000000" {
		t.Errorf("runs = %q", runs)
	}
}
