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
	"strconv"
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
//	forwards.tsv           the forwards it holds, one a line:
//	                       "ID BOX LOCAL REMOTE STATE", as in
//	                       "9f3a1c2e hetzner-1 14320 4320 listening"
//	forward.sh             sourced before `forward` opens one, with $box,
//	                       $local and $remote set: it can fail the way
//	                       task.sh does, or start something listening on
//	                       $local to stand in for the box
//	unforward.sh           sourced before `unforward` closes one, with $id
//	                       and, when the forward is there, $box and $local
//
// Every run adds a line to runs.log (the command and its box, never a
// prompt), and every `task new` writes its arguments, NUL after each, to
// task-N.args, and its environment to task-N.env.
//
// A forward here forwards nothing: `forward` writes a line to
// forwards.tsv and answers as the real command does, `forwards` prints
// those lines as JSON, and `unforward` takes one out. Like the real one,
// it refuses a box it does not list and a local port a forward already
// has, and it does open a forward to a box that is offline.
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
forwards)
  echo "forwards" >> "$dir/runs.log"
  printf '['
  sep=''
  if [ -f "$dir/forwards.tsv" ]; then
    while read -r id box local remote state rest; do
      [ -n "$id" ] || continue
      printf '%s\n  {"id":"%s","box":"%s","local":%s,"remote":%s,"state":"%s"' "$sep" "$id" "$box" "$local" "$remote" "${state:-listening}"
      if [ -n "$rest" ]; then printf ',"error":"%s"' "$rest"; fi
      printf '}'
      sep=','
    done < "$dir/forwards.tsv"
  fi
  if [ -n "$sep" ]; then printf '\n'; fi
  echo ']' ;;
forward)
  echo "forward $2 $3" >> "$dir/runs.log"
  box=$2
  local=${3%%:*}
  remote=${3##*:}
  if ! grep -q "\"name\":\"$box\"" "$dir/boxes.json"; then
    echo "berth: localhost:$local: no paired box named \"$box\"" >&2
    exit 1
  fi
  if [ -f "$dir/forwards.tsv" ] && grep -q "^[^ ]* [^ ]* $local " "$dir/forwards.tsv"; then
    echo "berth: localhost:$local: local port $local is in use: listen tcp 127.0.0.1:$local: bind: address already in use" >&2
    exit 1
  fi
  if [ -f "$dir/forward.sh" ]; then . "$dir/forward.sh"; fi
  n=$(cat "$dir/forwards.count" 2>/dev/null || echo 0)
  echo $((n + 1)) > "$dir/forwards.count"
  id=$(printf 'f%07x' "$n")
  echo "$id $box $local $remote listening" >> "$dir/forwards.tsv"
  cat <<JSON
[
  {
    "id": "$id",
    "box": "$box",
    "local": $local,
    "remote": $remote
  }
]
JSON
  ;;
unforward)
  echo "unforward $2" >> "$dir/runs.log"
  id=$2
  box=''
  local=''
  remote=''
  if [ -f "$dir/forwards.tsv" ]; then
    while read -r fid fbox flocal fremote rest; do
      if [ "$fid" = "$id" ]; then box=$fbox; local=$flocal; remote=$fremote; fi
    done < "$dir/forwards.tsv"
  fi
  if [ -f "$dir/unforward.sh" ]; then . "$dir/unforward.sh"; fi
  if [ -z "$box" ]; then
    echo "berth: no forward with that id" >&2
    exit 1
  fi
  grep -v "^$id " "$dir/forwards.tsv" > "$dir/forwards.tmp"
  mv "$dir/forwards.tmp" "$dir/forwards.tsv"
  echo "Stopped localhost:$local → $box:$remote" ;;
*)
  echo "other $*" >> "$dir/runs.log"
  echo "berth: this command must never be run" >&2
  exit 64 ;;
esac
`
	return cli, os.WriteFile(cli, []byte(script), 0o755)
}

// Runs lists what the script was run with, one line a run: "boxes",
// "locations hetzner-1", "agents hetzner-1", "task new hetzner-1/acme/x",
// "forwards", "forward hetzner-1 14320:4320", "unforward f0000000".
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

// Forward is one forward the script holds.
type Forward struct {
	ID, Box       string
	Local, Remote int
	State         string
}

// Forwards lists the forwards the script holds now: the ones `forward`
// opened and a test wrote, less the ones `unforward` closed.
func Forwards(dir string) []Forward {
	b, err := os.ReadFile(filepath.Join(dir, "forwards.tsv"))
	if err != nil {
		return nil
	}
	var out []Forward
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		fw := Forward{ID: f[0], Box: f[1], State: "listening"}
		fw.Local, _ = strconv.Atoi(f[2])
		fw.Remote, _ = strconv.Atoi(f[3])
		if len(f) > 4 {
			fw.State = f[4]
		}
		out = append(out, fw)
	}
	return out
}
