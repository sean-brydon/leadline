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
