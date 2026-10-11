package discover

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// recorder notes every path the package opens, lists or stats.
type recorder struct {
	mu    sync.Mutex
	paths map[string]string // path -> how it was touched
}

func (r *recorder) note(how, path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paths == nil {
		r.paths = map[string]string{}
	}
	r.paths[path] = how
}

// outside lists the touched paths that are in none of the allowed places.
func (r *recorder) outside(allowed ...string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for path, how := range r.paths {
		ok := false
		for _, a := range allowed {
			ok = ok || within(a, path)
		}
		if !ok {
			out = append(out, how+" "+path)
		}
	}
	sort.Strings(out)
	return out
}

func (r *recorder) touched(path string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.paths[path]
	return ok
}

// recording is the real disk with a recorder in front of it.
func recording(r *recorder) files {
	real := osFiles()
	return files{
		readDir: func(dir string) ([]fs.DirEntry, error) { r.note("list", dir); return real.readDir(dir) },
		readFile: func(path string, limit int64) ([]byte, error) {
			r.note("read", path)
			return real.readFile(path, limit)
		},
		lstat: func(path string) (fs.FileInfo, error) { r.note("stat", path); return real.lstat(path) },
	}
}

// tempDir is a temporary folder by its real path (macOS's is behind a
// link).
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func names(ps []*Project) string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Name)
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

// tripwireTree is a folder to scan (root) with, all around it, things a
// scan must not open: a project above it, one beside it, links leading
// out of it.
func tripwireTree(t *testing.T) (base, root string) {
	t.Helper()
	base = tempDir(t)
	root = filepath.Join(base, "root")
	writeFiles(t, base, map[string]string{
		// Above the folder: if the runner's naming walked up, a project
		// with no name of its own would be called this.
		"package.json": `{"name":"TRIPWIRE","scripts":{"dev":"vite"}}`,
		".env":         "DATABASE_URL=postgres://trip:TRIPWIRESECRET@localhost:5432/trip\n",
		// Beside it.
		"outside/app/package.json": `{"name":"outside-app","scripts":{"dev":"vite"},"devDependencies":{"vite":"7.0.0"}}`,
		"outside/app/.env":         "DATABASE_URL=postgres://out:TRIPWIRESECRET@localhost:5432/out\n",
		// A repository elsewhere, of which root/wt is a linked worktree.
		"main-repo/package.json":           `{"name":"main-repo"}`,
		"main-repo/.git/HEAD":              "ref: refs/heads/main\n",
		"main-repo/.git/worktrees/wt/HEAD": "ref: refs/heads/feature\n",
		"main-repo/.git/config":            "[remote \"origin\"]\n\turl = https://TRIPWIRESECRET@git.acme.test/acme/main-repo.git\n",

		"root/acme/package.json":             `{"name":"acme","scripts":{"dev":"turbo dev"}}`,
		"root/acme/.git/HEAD":                "ref: refs/heads/main\n",
		"root/acme/pnpm-lock.yaml":           "",
		"root/acme/apps/web/package.json":    `{"name":"@acme/web","scripts":{"dev":"next dev"},"dependencies":{"next":"16.0.0"}}`,
		"root/acme/packages/ui/package.json": `{"name":"@acme/ui","scripts":{"build":"tsup"}}`,
		"root/nameless/package.json":         `{"scripts":{"dev":"vite"},"devDependencies":{"vite":"7.0.0"}}`,
		"root/l1/l2/l3/package.json":         `{"name":"depth-three"}`,
		"root/l1/l2/l3/l4/package.json":      `{"name":"depth-four"}`,
		"root/node_modules/dep/package.json": `{"name":"dep"}`,
		"root/.cache/x/package.json":         `{"name":"hidden"}`,
		"root/acme/dist/package.json":        `{"name":"built"}`,
		"root/wt/.git":                       "gitdir: " + filepath.Join(base, "main-repo", ".git", "worktrees", "wt") + "\n",
		"root/wt/go.mod":                     "module acme.test/wt\n\ngo 1.27\n",
		"root/notes/README.md":               "",
	})
	for link, target := range map[string]string{
		filepath.Join(root, "link-out"):               filepath.Join(base, "outside"),
		filepath.Join(root, "acme", "shared"):         filepath.Join(base, "outside", "app"),
		filepath.Join(root, "link-up"):                base,
		filepath.Join(root, "nameless", ".env.local"): filepath.Join(base, "does-not-exist"),
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	return base, root
}

func TestWalkStopsAtItsDepthAndOpensNothingOutside(t *testing.T) {
	base, root := tripwireTree(t)
	rec := &recorder{}
	f := recording(rec)
	dirs := 0
	w := f.walk(context.Background(), root, func() { dirs++ })

	// Found: the repository, the one package in it that is run, the
	// nameless one under its folder's name, the one exactly three levels
	// down, and the worktree. Not found: four levels down, node_modules,
	// a dot-directory, build output, a library package, and anything
	// reached through a link.
	if got, want := names(w.projects), "@acme/web acme depth-three nameless wt"; got != want {
		t.Errorf("projects = %s\n    want   %s", got, want)
	}
	if w.truncated || w.dirs != dirs {
		t.Errorf("truncated=%v dirs=%d counted=%d", w.truncated, w.dirs, dirs)
	}
	// The one thing read outside the folder: the linked worktree's HEAD,
	// in the repository it belongs to.
	head := filepath.Join(base, "main-repo", ".git", "worktrees", "wt", "HEAD")
	if out := rec.outside(root, head); len(out) > 0 {
		t.Errorf("opened outside the folder:\n  %s", strings.Join(out, "\n  "))
	}
	if !rec.touched(head) {
		t.Error("the worktree's HEAD was not read (so the test proves less than it says)")
	}
	for _, never := range []string{
		filepath.Join(root, "l1", "l2", "l3", "l4"),
		filepath.Join(root, "node_modules"), filepath.Join(root, ".cache"), filepath.Join(root, "acme", "dist"),
		filepath.Join(root, "link-out"), filepath.Join(root, "link-up"), filepath.Join(root, "acme", "shared"),
	} {
		rec.mu.Lock()
		how := rec.paths[never]
		rec.mu.Unlock()
		if how == "list" || how == "read" {
			t.Errorf("%s was opened (%s)", never, how)
		}
	}

	for _, p := range w.projects {
		switch p.Name {
		case "wt":
			if p.Git == nil || !p.Git.Worktree || p.Git.Branch != "feature" || p.Git.MainRepo != filepath.Join(base, "main-repo") {
				t.Errorf("worktree git = %+v", p.Git)
			}
		case "@acme/web":
			// A package in a repository: its checkout is the repository,
			// and its package manager is the workspace's.
			if p.Git == nil || p.Git.Root != filepath.Join(root, "acme") || p.Git.Branch != "main" || p.PackageManager != "pnpm" {
				t.Errorf("nested package: git=%+v manager=%s", p.Git, p.PackageManager)
			}
		case "nameless":
			if p.Git != nil {
				t.Errorf("a folder in no repository has git = %+v", p.Git)
			}
		}
	}
}

func TestWalkGivesUpAtItsLimits(t *testing.T) {
	root := tempDir(t)
	tree := map[string]string{}
	for _, name := range []string{"a", "b", "c", "d", "e", "f"} {
		tree[name+"/package.json"] = `{"name":"` + name + `"}`
		tree[name+"/src/x/y.ts"] = ""
	}
	writeFiles(t, root, tree)
	defer func(d, p int) { maxDirs, maxProjects = d, p }(maxDirs, maxProjects)

	maxDirs, maxProjects = 5, 200
	if w := osFiles().walk(context.Background(), root, func() {}); !w.truncated || w.dirs != 5 {
		t.Errorf("at 5 directories: truncated=%v dirs=%d", w.truncated, w.dirs)
	}
	maxDirs, maxProjects = 5000, 3
	if w := osFiles().walk(context.Background(), root, func() {}); !w.truncated || len(w.projects) != 3 {
		t.Errorf("at 3 projects: truncated=%v projects=%d", w.truncated, len(w.projects))
	}
	maxDirs, maxProjects = 5000, 200
	if w := osFiles().walk(context.Background(), root, func() {}); w.truncated || len(w.projects) != 6 {
		t.Errorf("unlimited: truncated=%v projects=%d", w.truncated, len(w.projects))
	}

	// A cancelled scan stops walking.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if w := osFiles().walk(ctx, root, func() {}); w.dirs != 0 || len(w.projects) != 0 {
		t.Errorf("after cancel: dirs=%d projects=%d", w.dirs, len(w.projects))
	}
}

func TestResolveFolder(t *testing.T) {
	base := tempDir(t)
	home := filepath.Join(base, "Users", "acme")
	writeFiles(t, base, map[string]string{
		"Users/acme/code/acme/package.json": "{}",
		"Users/acme/notes.txt":              "",
		"srv/app/package.json":              "{}",
	})
	if err := os.Symlink(home, filepath.Join(base, "srv", "home-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "code"), filepath.Join(base, "srv", "code-link")); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ in, resolved, state string }{
		{filepath.Join(home, "code"), filepath.Join(home, "code"), "scanned"},
		{"~/code", filepath.Join(home, "code"), "scanned"},
		{"  ~/code/acme/ ", filepath.Join(home, "code", "acme"), "scanned"},
		{filepath.Join(base, "srv"), filepath.Join(base, "srv"), "scanned"},
		{filepath.Join(base, "srv", "code-link"), filepath.Join(home, "code"), "scanned"},
		// The home directory, however it is spelled, and what is above it.
		{home, home, "refused"},
		{"~", home, "refused"},
		{home + "/", home, "refused"},
		{filepath.Join(home, "code", ".."), home, "refused"},
		{filepath.Join(base, "srv", "home-link"), home, "refused"},
		{filepath.Join(base, "Users"), filepath.Join(base, "Users"), "refused"},
		{base, base, "refused"},
		{"/", "/", "refused"},
		// Not folders.
		{filepath.Join(home, "nope"), "", "missing"},
		{filepath.Join(home, "notes.txt"), "", "missing"},
		{"code", "", "missing"},
		{"", "", "missing"},
	}
	for _, c := range cases {
		resolved, state, reason := resolveFolder(c.in, home)
		if resolved != c.resolved || state != c.state {
			t.Errorf("resolveFolder(%q) = (%q, %s), want (%q, %s)", c.in, resolved, state, c.resolved, c.state)
		}
		if (state != "scanned") != (reason != "") {
			t.Errorf("resolveFolder(%q): state %s with reason %q", c.in, state, reason)
		}
	}
}

// A working directory's project is looked for at and above it, and the
// looking stops short of the home directory: nothing in the home
// directory itself, or above it, is so much as stat'ed.
func TestRootForNeverReachesHome(t *testing.T) {
	base := tempDir(t)
	home := filepath.Join(base, "Users", "acme")
	writeFiles(t, base, map[string]string{
		"Users/acme/package.json":                     `{"name":"TRIPWIRE"}`,
		"Users/acme/.git/HEAD":                        "ref: refs/heads/dotfiles\n",
		"Users/acme/code/acme/.git/HEAD":              "ref: refs/heads/main\n",
		"Users/acme/code/acme/apps/web/package.json":  `{"name":"web"}`,
		"Users/acme/code/acme/apps/web/src/server.ts": "",
		"Users/acme/code/loose/script.js":             "",
		"Users/acme/Documents/deep/a/b/c/d/e/f/g/x":   "",
		"srv/app/go.mod":                              "module acme.test/app\n",
	})
	rec := &recorder{}
	f := recording(rec)
	cases := []struct{ cwd, root, boundary string }{
		// From a folder inside the project: up to the project, and its
		// checkout is as far as anything is read.
		{filepath.Join(home, "code", "acme", "apps", "web", "src"), filepath.Join(home, "code", "acme", "apps", "web"), filepath.Join(home, "code", "acme")},
		{filepath.Join(home, "code", "acme", "apps", "web"), filepath.Join(home, "code", "acme", "apps", "web"), filepath.Join(home, "code", "acme")},
		{filepath.Join(home, "code", "acme"), filepath.Join(home, "code", "acme"), filepath.Join(home, "code", "acme")},
		// No project at or above it, short of home: none, though home
		// itself has a package.json and a .git.
		{filepath.Join(home, "code", "loose"), "", ""},
		{filepath.Join(home, "Documents", "deep", "a", "b", "c", "d", "e", "f", "g"), "", ""},
		{home, "", ""},
		{filepath.Dir(home), "", ""},
		{"/", "", ""},
		{"", "", ""},
		{"relative/path", "", ""},
		// Outside home it works the same.
		{filepath.Join(base, "srv", "app"), filepath.Join(base, "srv", "app"), filepath.Join(base, "srv", "app")},
	}
	for _, c := range cases {
		root, boundary, ok := f.rootFor(c.cwd, home)
		if root != c.root || boundary != c.boundary || ok != (c.root != "") {
			t.Errorf("rootFor(%q) = (%q, %q, %v), want (%q, %q)", c.cwd, root, boundary, ok, c.root, c.boundary)
		}
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for path := range rec.paths {
		// Anything directly in home, or in a folder above it.
		if dir := filepath.Dir(path); within(dir, home) {
			t.Errorf("looked at %s, which is in the home directory or above it", path)
		}
	}
}
