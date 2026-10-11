// Package fakeberth writes a script that stands in for Shipyard's command
// line, `berth`, in tests. It answers the commands Leadline runs the way
// the real one does (the shapes are copied from Shipyard's own output) and
// writes down, exactly, what it was run with. It talks to nothing: no test
// that uses it can reach a real box.
package fakeberth

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The made-up fleet the script answers with unless a file in its folder
// says otherwise: an online box with two projects, one of them with a
// worktree; an offline box; and this computer.
const (
	Boxes = `[
  {"name":"hetzner-1","address":"100.64.0.11:1376","network":"acme","fingerprint":"aa11bb22cc33dd44","state":"online","latency_ms":86,
   "since":"2026-10-09T08:00:00Z","route":"tailnet"},
  {"name":"build-box","address":"100.64.0.12:1376","fingerprint":"ee55ff66aa77bb88","state":"offline","error":"dial tcp: i/o timeout",
   "since":"2026-10-08T17:30:00Z"},
  {"name":"this-mac","address":"127.0.0.1:1376","fingerprint":"0011223344556677","state":"online","since":"2026-10-10T07:00:00Z","local":true}
]`
	Locations = `[
  {"name":"acme","path":"/home/dev/acme","repo":true,"remote":"https://x-access-token:REMOTETOKEN@git.acme.test/acme/acme.git",
   "slug":"acme/acme","default_branch":"main",
   "agents":[{"id":"reviewer","name":"Acme reviewer","command":"claude --append-system-prompt REVIEWSECRET"}],
   "worktrees":[
     {"name":"main","path":"/home/dev/acme","branch":"main","head":"3f3761e","main":true,"port":3000},
     {"name":"fix-orders","path":"/home/dev/acme-fix-orders","branch":"fix-orders","head":"9a1b2c3","port":3004,"title":"Fix the orders N+1"}
   ]},
  {"name":"storefront","path":"/home/dev/storefront","repo":true,"default_branch":"main",
   "worktrees":[{"name":"main","path":"/home/dev/storefront","branch":"main","main":true}]},
  {"name":"notes","path":"/home/dev/notes","repo":false}
]`
	Agents = `[
  {"id":"claude","name":"Claude Code","command":"claude","model_flag":"--model","models":["opus","sonnet","haiku"]},
  {"id":"codex","name":"Codex","command":"codex"}
]`
)

// Write puts the script and its answers in dir and returns the script's
// path. A test changes an answer by writing over its file:
//
//	boxes.json             what `berth boxes --json` prints
//	locations.json         `berth locations BOX --json`, for every box;
//	locations-BOX.json     for one
//	agents.json            `berth agents BOX --json`
//	task.sh                sourced before `task new` answers: `exit 1`
//	                       after a line on stderr makes it fail
//
// Every run adds a line to runs.log (the command and its box, never a
// prompt), and every `task new` writes its arguments, NUL after each, to
// task-N.args, and its environment to task-N.env.
func Write(dir string) (string, error) {
	files := map[string]string{"boxes.json": Boxes, "locations.json": Locations, "agents.json": Agents}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body+"\n"), 0o644); err != nil {
			return "", err
		}
	}
	cli := filepath.Join(dir, "berth")
	script := `#!/bin/sh
dir='` + dir + `'
case "$1" in
boxes)
  echo "boxes" >> "$dir/runs.log"
  cat "$dir/boxes.json" ;;
locations)
  echo "locations $2" >> "$dir/runs.log"
  if [ -f "$dir/locations-$2.json" ]; then cat "$dir/locations-$2.json"; else cat "$dir/locations.json"; fi ;;
agents)
  echo "agents $2" >> "$dir/runs.log"
  cat "$dir/agents.json" ;;
task)
  echo "task $2 $3" >> "$dir/runs.log"
  n=$(ls "$dir" | grep -c '^task-.*\.args$')
  for a in "$@"; do printf '%s\0' "$a"; done > "$dir/task-$n.args"
  env > "$dir/task-$n.env"
  if [ -f "$dir/task.sh" ]; then . "$dir/task.sh"; fi
  ref=$3
  name=${ref##*/}
  rest=${ref%/*}
  project=${rest##*/}
  cat <<JSON
{
  "worktree": { "name": "$name", "path": "/home/dev/$project-$name", "branch": "$name", "head": "3f3761e" },
  "session": { "name": "$project-$name-claude", "location": "$project/$name", "agent": "claude" }
}
JSON
  ;;
*)
  echo "other $*" >> "$dir/runs.log"
  echo "berth: this command must never be run" >&2
  exit 64 ;;
esac
`
	return cli, os.WriteFile(cli, []byte(script), 0o755)
}

// Runs lists what the script was run with, one line a run: "boxes",
// "locations hetzner-1", "agents hetzner-1", "task new hetzner-1/acme/x".
func Runs(dir string) []string {
	b, err := os.ReadFile(filepath.Join(dir, "runs.log"))
	if err != nil || len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// Tasks returns the arguments of every `task new` the script was run with,
// in order, each exactly as it arrived.
func Tasks(dir string) [][]string {
	names, _ := filepath.Glob(filepath.Join(dir, "task-*.args"))
	sort.Slice(names, func(i, j int) bool {
		if len(names[i]) != len(names[j]) {
			return len(names[i]) < len(names[j])
		}
		return names[i] < names[j]
	})
	var out [][]string
	for _, name := range names {
		b, err := os.ReadFile(name)
		if err != nil {
			continue
		}
		args := strings.Split(string(b), "\x00")
		out = append(out, args[:len(args)-1])
	}
	return out
}

// TaskEnv returns the environment the n-th `task new` ran with.
func TaskEnv(dir string, n int) string {
	names, _ := filepath.Glob(filepath.Join(dir, "task-*.env"))
	sort.Strings(names)
	if n >= len(names) {
		return ""
	}
	b, _ := os.ReadFile(names[n])
	return string(b)
}
