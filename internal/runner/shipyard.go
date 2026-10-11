package runner

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Worktree is one checkout of a project, made for one task: what Shipyard
// runs each agent in. Leadline shows each as a source of its own, so two
// agents working on one project are two rows and not one muddle.
type Worktree struct {
	// Project is the repository as the tool names it (Shipyard's location);
	// Name the worktree's own name, empty for the project's main checkout.
	Project, Name string
	Branch, Box   string
	Path          string
	// Via is the tool whose variables said so: "shipyard", or "cli" when it
	// was worked out from flags and the folder.
	Via string
}

// SubSep joins a worktree's service name to the name of one of several
// commands run in it: `leadline run --name worker` in acme/fix-orders
// files under "acme/fix-orders:worker". The daemon knows the same rule
// (integrations.SubSep): what is filed under such a name is the worktree's.
const SubSep = ":"

// Service is the name a worktree is filed under: "<project>/<worktree>", the
// way Shipyard itself addresses one, or the project's name alone for its
// main checkout. It is made of names that do not change for the life of
// the worktree, so it is the same from the hook that attaches it, from every
// `leadline run` in it, and from the hook that detaches it.
func (w Worktree) Service() string {
	project, name := namePart(w.Project), namePart(w.Name)
	switch {
	case project == "":
		return name
	case name == "" || name == project:
		return project
	}
	return project + "/" + name
}

// ServiceFor is the name for one command among several in the worktree:
// the worktree's own when no name is given.
func (w Worktree) ServiceFor(name string) string {
	base := w.Service()
	// The worktree's own name, given back, is no second name.
	if strings.TrimSpace(name) == base {
		return base
	}
	if name = namePart(name); name == "" {
		return base
	}
	return base + SubSep + name
}

// Tags are what a run in the worktree marks its telemetry with.
func (w Worktree) Tags() [][2]string {
	var out [][2]string
	for _, kv := range [][2]string{
		{"shipyard.project", w.Project}, {"shipyard.worktree", w.Name},
		{"shipyard.branch", w.Branch}, {"shipyard.box", w.Box},
	} {
		if kv[1] != "" {
			out = append(out, kv)
		}
	}
	return out
}

// namePart makes a name safe to be one part of a service name: no slash and
// no colon, which are the joins, and no white space or control characters.
func namePart(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '/' || r == ':' || r == '\\' || r <= 0x20 || r == 0x7f:
			b.WriteByte('-')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 60 {
		out = strings.ToValidUTF8(out[:60], "")
	}
	return out
}

// WorktreeFromEnv reads the worktree out of the variables Shipyard sets. It
// sets them two ways, and both are read:
//
//   - everything it starts in a worktree (terminals, agents, services, a
//     repository's hooks) has BERTH_LOCATION, BERTH_WORKTREE_NAME,
//     BERTH_WORKTREE_PATH, BERTH_BRANCH and BERTH_BOX;
//   - a hook also has the event it runs for, as BERTH_EVENT and a variable
//     for each of its fields: BERTH_LOCATION, BERTH_NAME, BERTH_PATH,
//     BERTH_BRANCH, and the box as BERTH_EVENT_BOX.
//
// In a hook the event is what names the worktree. They mostly agree, but
// not for worktree.removed: the worktree is gone by then, so Shipyard runs
// a repository's hook in the project's main checkout, with the main
// checkout's BERTH_WORKTREE_* and the removed worktree's BERTH_NAME. Going
// by the first there would detach the wrong one.
//
// ok is false when the environment names no project, which is how to tell
// that this is not Shipyard's.
func WorktreeFromEnv(env func(string) string) (w Worktree, ok bool) {
	w = Worktree{Via: "shipyard", Project: env("BERTH_LOCATION"), Name: env("BERTH_WORKTREE_NAME"),
		Branch: env("BERTH_BRANCH"), Box: env("BERTH_BOX"), Path: env("BERTH_WORKTREE_PATH")}
	if event := env("BERTH_EVENT"); strings.HasPrefix(event, "worktree.") || strings.HasPrefix(event, "task.") {
		if name := env("BERTH_NAME"); name != "" {
			if w.Name != "" && w.Name != name {
				// The environment is another worktree's (the main one's):
				// nothing in it describes this one.
				w.Branch, w.Path = "", ""
			}
			w.Name = name
		}
		if path := env("BERTH_PATH"); path != "" {
			w.Path = path
		}
	}
	if w.Box == "" {
		w.Box = env("BERTH_EVENT_BOX")
	}
	// An event may name both at once, as "shop/checkout".
	if project, name, cut := strings.Cut(w.Project, "/"); cut {
		w.Project = project
		if w.Name == "" {
			w.Name = name
		}
	}
	w.Project, w.Name = strings.TrimSpace(w.Project), strings.TrimSpace(w.Name)
	if w.Project == "" {
		return Worktree{}, false
	}
	// The main checkout is a worktree to Shipyard, named for its folder.
	if root := env("BERTH_ROOT_PATH"); root != "" && w.Path != "" && samePath(root, w.Path) {
		w.Name = ""
	}
	if w.Name == w.Project {
		w.Name = ""
	}
	return w, true
}

// InHook reports whether the environment is a Shipyard hook's, and whether
// its event names a worktree. A hook for an event that names none has
// nothing to attach or detach.
func InHook(env func(string) string) (hook, named bool) {
	event := env("BERTH_EVENT")
	if event == "" {
		return false, false
	}
	return true, (strings.HasPrefix(event, "worktree.") || strings.HasPrefix(event, "task.")) && env("BERTH_NAME") != ""
}

// WorktreeAt works a worktree out from a folder alone, for `leadline attach`
// run by hand outside Shipyard. Git says most of it: a linked worktree's
// .git is a file that names the main checkout's, so the project is the
// main checkout's folder and the worktree this one's. A folder that is its
// own repository, or none, is a project with no worktree name.
func WorktreeAt(dir string) Worktree {
	w := Worktree{Via: "cli", Path: dir}
	base := filepath.Base(dir)
	if base == "." || base == string(filepath.Separator) {
		base = ""
	}
	w.Project = base
	raw, err := os.ReadFile(filepath.Join(dir, ".git"))
	if err != nil {
		// A directory (the main checkout), or no repository at all.
		return w
	}
	// "gitdir: /path/to/main/.git/worktrees/<name>"
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir:")
	if !ok {
		return w
	}
	gitdir = filepath.ToSlash(strings.TrimSpace(gitdir))
	if i := strings.Index(gitdir, "/.git/worktrees/"); i > 0 {
		w.Project, w.Name = filepath.Base(gitdir[:i]), base
		// Shipyard and git both name a worktree's folder "<project>-<name>".
		if rest, cut := strings.CutPrefix(base, w.Project+"-"); cut && rest != "" {
			w.Name = rest
		}
	}
	return w
}

func samePath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

// resourceAttributes adds a worktree's tags to an OTEL_RESOURCE_ATTRIBUTES
// value, leaving what is there and never setting a key twice. The format
// is OpenTelemetry's: key=value pairs with commas between, values
// percent-encoded.
func resourceAttributes(have string, tags [][2]string) string {
	set := map[string]bool{}
	for _, pair := range strings.Split(have, ",") {
		key, _, _ := strings.Cut(pair, "=")
		set[strings.TrimSpace(key)] = true
	}
	out := strings.TrimRight(strings.TrimSpace(have), ",")
	for _, kv := range tags {
		if set[kv[0]] {
			continue
		}
		if out != "" {
			out += ","
		}
		out += kv[0] + "=" + url.PathEscape(kv[1])
	}
	return out
}
