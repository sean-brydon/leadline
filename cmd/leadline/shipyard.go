package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const shipyardUsage = `usage: leadline shipyard hooks [flags]

Prints the hooks that have Shipyard attach each worktree to Leadline when it
is created and detach it when it is removed, in the format Shipyard's config
files take.

  leadline shipyard hooks                              print them, and where they go
  leadline shipyard hooks --json                       print only the JSON
  leadline shipyard hooks --write .berth/config.json   add them to a repository's config

They go in one of two places, and the entries are the same in both:

  a repository's .berth/config.json   for that repository's worktrees, on
                                      every box that has it and trusts the file
  ~/.berth/hooks.json on a box        for every repository on that box

  --write <file>   add the hooks to this config file, leaving everything
                   else in it exactly as it is. Hooks that are there already
                   are not added twice. Leadline never writes into
                   Shipyard's own folders (~/.berth, its state directory):
                   for a box-wide install, paste the entries into the box's
                   ~/.berth/hooks.json, or add them in Shipyard's app under
                   Automations
  --bin <path>     the leadline to run, when it is not on the PATH of the
                   box's hooks (default: leadline)
  --json           print only the JSON
`

// hookEntry is one hook as Shipyard's config holds it: {"on": ..., "run": ...}.
type hookEntry struct {
	On  string `json:"on"`
	Run string `json:"run"`
}

// shipyardHooks are the hooks that tie a worktree's life to Leadline's.
//
// A worktree is attached when it appears, and again when its setup script
// has finished: the script is usually what creates the database and writes
// the .env file, so the first attach may find no connection yet. Attaching
// again on resume changes nothing unless something did while it was paused
// (or Leadline was not running the first time). Nothing runs on pause: a
// paused worktree keeps its files and its database, and reads as quiet.
//
// Each command leaves quietly when leadline is not installed on the box,
// so a repository's config can carry the hooks for the people who have it
// without failing for the people who don't.
func shipyardHooks(bin string) []hookEntry {
	found := "command -v " + bin + " >/dev/null 2>&1"
	if strings.ContainsRune(bin, '/') {
		found = "[ -x " + bin + " ]"
	}
	run := func(command string) string { return found + " || exit 0; " + bin + " " + command }
	return []hookEntry{
		{On: "worktree.created", Run: run("attach")},
		{On: "worktree.setup.finished", Run: run("attach")},
		{On: "worktree.resumed", Run: run("attach")},
		{On: "worktree.removed", Run: run("detach")},
	}
}

// entryJSON is a hook on one line, spaced the way Shipyard's docs write it.
func entryJSON(h hookEntry) string {
	return `{ "on": ` + jsonString(h.On) + `, "run": ` + jsonString(h.Run) + ` }`
}

// jsonString quotes s for JSON without turning > and & into \u escapes: a
// shell command should read as one.
func jsonString(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(s)
	return strings.TrimRight(b.String(), "\n")
}

// hooksJSON is a whole file holding only the hooks.
func hooksJSON(hooks []hookEntry) string {
	var b strings.Builder
	b.WriteString("{\n  \"hooks\": [\n")
	for i, h := range hooks {
		b.WriteString("    " + entryJSON(h))
		if i < len(hooks)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString("  ]\n}\n")
	return b.String()
}

// shipyardCommand is `leadline shipyard`. It returns the exit code.
func shipyardCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "hooks" {
		fmt.Fprint(stderr, shipyardUsage)
		if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
			return 0
		}
		return 2
	}
	fset := flag.NewFlagSet("shipyard hooks", flag.ContinueOnError)
	fset.SetOutput(stderr)
	fset.Usage = func() { fmt.Fprint(stderr, shipyardUsage) }
	write := fset.String("write", "", "")
	bin := fset.String("bin", "leadline", "")
	onlyJSON := fset.Bool("json", false, "")
	if err := fset.Parse(args[1:]); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fset.NArg() > 0 || strings.TrimSpace(*bin) == "" || strings.ContainsAny(*bin, " \t\n\"'\\$`;&|<>") {
		if fset.NArg() == 0 {
			fmt.Fprintln(stderr, "leadline: --bin is a command name or a path without spaces or quotes")
		} else {
			fmt.Fprint(stderr, shipyardUsage)
		}
		return 2
	}
	hooks := shipyardHooks(*bin)

	if *write != "" {
		added, err := writeHooks(*write, hooks)
		if err != nil {
			fmt.Fprintln(stderr, "leadline:", err)
			return 1
		}
		if added == 0 {
			fmt.Fprintf(stdout, "%s already has Leadline's hooks; nothing was changed.\n", *write)
			return 0
		}
		fmt.Fprintf(stdout, "Added %d hooks to %s. Nothing else in it was changed.\n", added, *write)
		fmt.Fprint(stdout, "Shipyard runs a repository's config only once the box trusts it, and asks again after a change:\ntrust it in the app's Project settings, or with \"berth location config BOX/PROJECT --trust HASH\".\n")
		return 0
	}
	if *onlyJSON {
		fmt.Fprint(stdout, hooksJSON(hooks))
		return 0
	}
	fmt.Fprintf(stdout, `Shipyard hooks for Leadline: each worktree becomes a source of its own
("<project>/<worktree>") with its databases attached, and is cleaned up,
data and all, when Shipyard removes it.

For one repository, add them to "hooks" in its .berth/config.json
(leadline shipyard hooks --write .berth/config.json does it). Shipyard runs
them on the box, in the worktree, once the box trusts the file:

%s
For every repository on a box, the same entries go in that box's
~/.berth/hooks.json (the file has the same shape), or in Shipyard's app
under Automations, Advanced: shell hooks. Leadline does not write there.

Leadline has to be running on the box: the hooks talk to the daemon on the
machine the worktree is on. Where it is not, or leadline is not installed,
they do nothing and fail nothing. To try one by hand:

  BERTH_EVENT=worktree.created BERTH_LOCATION=acme BERTH_NAME=fix-orders BERTH_PATH="$PWD" %s attach
`, hooksJSON(hooks), *bin)
	return 0
}

// writeHooks adds the hooks to the config file at path and reports how many
// it added. The file's other content is kept byte for byte: the new entries
// are spliced into the text, which is never parsed and written back.
func writeHooks(path string, hooks []hookEntry) (int, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, err
	}
	if err := refuseShipyardOwn(abs); err != nil {
		return 0, err
	}
	old, err := os.ReadFile(abs)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	out, added, err := mergeHooks(old, hooks)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	if added == 0 {
		return 0, nil
	}
	mode := fs.FileMode(0o644)
	if fi, err := os.Stat(abs); err == nil {
		if fi.IsDir() {
			return 0, fmt.Errorf("%s is a folder; --write takes the config file itself", path)
		}
		mode = fi.Mode().Perm()
	}
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	// Whole or not at all: Shipyard reads the file again for every event.
	tmp, err := os.CreateTemp(dir, filepath.Base(abs)+".*.tmp")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	return added, os.Rename(tmp.Name(), abs)
}

// refuseShipyardOwn says no to a path inside the folders Shipyard keeps for
// itself. Its app edits those files too, and a box's hooks.json is a
// person's to change, not another tool's.
func refuseShipyardOwn(abs string) error {
	home, _ := os.UserHomeDir()
	var own []string
	if dir := os.Getenv("BERTH_USER_DIR"); dir != "" {
		own = append(own, dir)
	}
	if dir := os.Getenv("BERTH_HOME"); dir != "" {
		own = append(own, dir)
	}
	if home != "" {
		own = append(own, filepath.Join(home, ".berth"),
			filepath.Join(home, "Library", "Application Support", "berth"),
			filepath.Join(home, ".config", "berth"))
	}
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		own = append(own, filepath.Join(dir, "berth"))
	}
	// As written, and with every link along the way followed, as far as
	// the path exists.
	paths := []string{abs}
	for at := abs; ; at = filepath.Dir(at) {
		if real, err := filepath.EvalSymlinks(at); err == nil {
			paths = append(paths, filepath.Join(real, strings.TrimPrefix(abs, at)))
			break
		}
		if filepath.Dir(at) == at {
			break
		}
	}
	for _, dir := range own {
		dirs := []string{filepath.Clean(dir)}
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			dirs = append(dirs, real)
		}
		for _, d := range dirs {
			for _, p := range paths {
				if p == d || strings.HasPrefix(p, d+string(filepath.Separator)) {
					return fmt.Errorf("%s is inside Shipyard's own folder (%s), which Leadline does not write to. Paste the entries from \"leadline shipyard hooks\" into the file yourself, or add them in Shipyard's app", abs, d)
				}
			}
		}
	}
	return nil
}

// mergeHooks returns old with the hooks it lacks added to its top-level
// "hooks" array, and how many that was. Everything that was in old is in
// the result, in place and unchanged: the result is old with one piece of
// text inserted. An empty old is a new file.
func mergeHooks(old []byte, hooks []hookEntry) ([]byte, int, error) {
	if len(bytes.TrimSpace(old)) == 0 {
		return []byte(hooksJSON(hooks)), len(hooks), nil
	}
	if !json.Valid(old) {
		return nil, 0, errors.New("not valid JSON, so it was left alone")
	}
	var have struct {
		Hooks []hookEntry `json:"hooks"`
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(old, &top); err != nil {
		return nil, 0, errors.New("not a JSON object, so it was left alone")
	}
	if raw, ok := top["hooks"]; ok {
		if err := json.Unmarshal(raw, &have.Hooks); err != nil {
			return nil, 0, errors.New(`its "hooks" is not a list of hooks, so it was left alone`)
		}
	}
	var missing []hookEntry
	for _, h := range hooks {
		found := false
		for _, x := range have.Hooks {
			found = found || (x.On == h.On && x.Run == h.Run)
		}
		if !found {
			missing = append(missing, h)
		}
	}
	if len(missing) == 0 {
		return old, 0, nil
	}

	obj := scanObject(old, skipSpace(old, 0))
	at, text := 0, ""
	if m, ok := obj.member("hooks"); ok {
		arr := scanArray(old, m.value)
		if len(arr.items) > 0 {
			// After the last hook, in the style of the ones there.
			first, last := arr.items[0], arr.items[len(arr.items)-1]
			indent, lines := indentOf(old, first.start)
			at = last.end
			for _, h := range missing {
				if lines {
					text += ",\n" + indent + entryJSON(h)
				} else {
					text += ", " + entryJSON(h)
				}
			}
		} else {
			// An empty list: one hook to a line, one step in from the key.
			indent, _ := indentOf(old, m.key)
			step := indent
			if step == "" {
				step = "  "
			}
			at = m.value + 1
			for i, h := range missing {
				if i > 0 {
					text += ","
				}
				text += "\n" + indent + step + entryJSON(h)
			}
			text += "\n" + indent
		}
	} else {
		list := func(indent, step string) string {
			s := `"hooks": [`
			for i, h := range missing {
				if i > 0 {
					s += ","
				}
				s += "\n" + indent + step + entryJSON(h)
			}
			return s + "\n" + indent + "]"
		}
		if len(obj.members) > 0 {
			// After the last key, as a key like the others.
			indent, lines := indentOf(old, obj.members[0].key)
			at = obj.members[len(obj.members)-1].end
			step := indent
			if step == "" {
				step = "  "
			}
			if lines {
				text = ",\n" + indent + list(indent, step)
			} else {
				text = ", " + list("", "  ")
			}
		} else {
			at = obj.start + 1
			text = "\n  " + list("  ", "  ") + "\n"
		}
	}
	out := make([]byte, 0, len(old)+len(text))
	out = append(out, old[:at]...)
	out = append(out, text...)
	out = append(out, old[at:]...)
	if !json.Valid(out) {
		// Not reached for JSON that json.Valid passed; the file is not
		// written if it ever is.
		return nil, 0, errors.New("could not be added to safely, so it was left alone")
	}
	return out, len(missing), nil
}

// What follows reads just enough of a JSON text to know where things are
// in it. It is only ever given text json.Valid has passed.

type jsonMember struct {
	name            string
	key, value, end int // where the key starts, and where the value starts and ends
}

type jsonObject struct {
	start, end int
	members    []jsonMember
}

// member finds a key. A key written twice means its last, as it does to
// whoever parses the file.
func (o jsonObject) member(name string) (jsonMember, bool) {
	for i := len(o.members) - 1; i >= 0; i-- {
		if o.members[i].name == name {
			return o.members[i], true
		}
	}
	return jsonMember{}, false
}

type jsonItem struct{ start, end int }

type jsonArray struct{ items []jsonItem }

func skipSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

// scanString returns the index after the string that starts at i.
func scanString(b []byte, i int) int {
	for i++; i < len(b); i++ {
		switch b[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return len(b)
}

// scanValue returns the index after the value that starts at i.
func scanValue(b []byte, i int) int {
	if i >= len(b) {
		return len(b)
	}
	switch b[i] {
	case '"':
		return scanString(b, i)
	case '{', '[':
		depth := 0
		for i < len(b) {
			switch b[i] {
			case '"':
				i = scanString(b, i)
				continue
			case '{', '[':
				depth++
			case '}', ']':
				if depth--; depth == 0 {
					return i + 1
				}
			}
			i++
		}
		return len(b)
	}
	for i < len(b) && !strings.ContainsRune(",]} \t\r\n", rune(b[i])) {
		i++
	}
	return i
}

// scanObject reads the members of the object that starts at i.
func scanObject(b []byte, i int) jsonObject {
	o := jsonObject{start: i, end: scanValue(b, i)}
	for j := skipSpace(b, i+1); j < len(b) && b[j] == '"'; {
		m := jsonMember{key: j}
		keyEnd := scanString(b, j)
		json.Unmarshal(b[j:keyEnd], &m.name)
		m.value = skipSpace(b, skipSpace(b, keyEnd)+1) // past the colon
		m.end = scanValue(b, m.value)
		o.members = append(o.members, m)
		next := skipSpace(b, m.end)
		if next >= len(b) || b[next] != ',' {
			break
		}
		j = skipSpace(b, next+1)
	}
	return o
}

// scanArray reads the items of the array that starts at i.
func scanArray(b []byte, i int) jsonArray {
	var a jsonArray
	for j := skipSpace(b, i+1); j < len(b) && b[j] != ']'; {
		end := scanValue(b, j)
		a.items = append(a.items, jsonItem{j, end})
		next := skipSpace(b, end)
		if next >= len(b) || b[next] != ',' {
			break
		}
		j = skipSpace(b, next+1)
	}
	return a
}

// indentOf is the white space a thing at i is indented with, when it is the
// first thing on its line; lines says whether it is.
func indentOf(b []byte, i int) (indent string, lines bool) {
	j := i
	for j > 0 && (b[j-1] == ' ' || b[j-1] == '\t') {
		j--
	}
	if j > 0 && b[j-1] == '\n' {
		return string(b[j:i]), true
	}
	return "", false
}
