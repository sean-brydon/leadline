package discover

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

const (
	// A named folder is walked this many levels below itself: enough for
	// ~/code/acme/apps/web from ~/code, and no more.
	maxDepth = 3
	// How many folders one scan takes.
	maxFolders = 8
	// How far above a working directory its project is looked for.
	maxUp = 6
)

// Where a walk gives up, so pointing at something huge costs a moment and
// not a minute. Variables only so a test can make them small.
var (
	maxDirs     = 5000
	maxProjects = 200
)

// skipDirs are never entered: what a build or an install made. Every
// directory whose name starts with a dot is skipped too.
var skipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "dist": true, "build": true, "out": true, "target": true,
	"coverage": true, "__pycache__": true, "venv": true, "bower_components": true, "Pods": true,
	"DerivedData": true, "tmp": true, "Library": true,
}

// resolveFolder turns a folder a person named into the directory to walk,
// or says why it won't be: "refused" for the home directory, anything
// above it and the root, "missing" for what is not a readable directory.
func resolveFolder(path, home string) (resolved, state, reason string) {
	path = strings.TrimSpace(path)
	if path == "~" {
		path = home
	} else if rest, ok := strings.CutPrefix(path, "~/"); ok && home != "" {
		path = filepath.Join(home, rest)
	}
	if path == "" || !filepath.IsAbs(path) {
		return "", "missing", "Give the folder's full path."
	}
	path = filepath.Clean(path)
	// Where it really is: a link to the home directory is the home
	// directory.
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", "missing", "There is no folder there."
	}
	st, err := os.Stat(real)
	if err != nil || !st.IsDir() {
		return "", "missing", "That is not a folder."
	}
	if real == string(filepath.Separator) {
		return real, "refused", "Leadline won't walk the whole disk. Name the folder your projects are in."
	}
	if home != "" && within(real, home) {
		return real, "refused", "Leadline won't walk the home directory or a folder above it. Name the folder your projects are in, such as ~/code."
	}
	return real, "scanned", ""
}

// walked is what walking one folder found.
type walked struct {
	projects  []*Project
	dirs      int
	truncated bool
}

// walk looks for projects in root and to maxDepth levels below it. It
// follows no symbolic link and enters no dot-directory or build output,
// and reads nothing outside root except a linked worktree's HEAD (see
// gitAt). count is told of each directory listed, for progress.
func (f files) walk(ctx context.Context, root string, count func()) walked {
	var out walked
	gits := map[string]*Git{}
	gitFor := func(dir string) *Git {
		at := f.gitRootAbove(dir, root)
		if at == "" {
			return nil
		}
		g, ok := gits[at]
		if !ok {
			g = f.gitAt(at)
			gits[at] = g
		}
		return g
	}
	var visit func(dir string, depth int, inside bool)
	visit = func(dir string, depth int, inside bool) {
		if ctx.Err() != nil || out.truncated {
			return
		}
		if out.dirs >= maxDirs || len(out.projects) >= maxProjects {
			out.truncated = true
			return
		}
		out.dirs++
		count()
		entries, err := f.readDir(dir)
		if err != nil {
			return
		}
		if p, ok := f.detectIn(dir, root, entries); ok {
			// A package inside a repository is a project of its own only
			// when it is something one runs.
			if !inside || p.DevCommand != "" || runnable(p) {
				p.Git = gitFor(dir)
				out.projects = append(out.projects, p)
			}
			inside = true
		}
		if depth >= maxDepth {
			return
		}
		for _, e := range entries {
			// IsDir is false for a link to a directory, which is how
			// links are not followed.
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || skipDirs[e.Name()] {
				continue
			}
			visit(filepath.Join(dir, e.Name()), depth+1, inside)
		}
	}
	visit(root, 0, false)
	return out
}

// runnable reports whether a project has a framework of its own. Go's
// entry for the language itself counts: a nested module is a program.
func runnable(p *Project) bool { return len(p.Frameworks) > 0 }

// rootFor finds the project a working directory belongs to: the nearest
// folder at or above it with a project file or a .git, looking at most
// maxUp levels up and never at the home directory or above. boundary is
// the checkout that project is in (itself, when it is in none): as far up
// as anything about it is read.
func (f files) rootFor(cwd, home string) (root, boundary string, ok bool) {
	if cwd == "" || !filepath.IsAbs(cwd) {
		return "", "", false
	}
	cwd = filepath.Clean(cwd)
	off := func(dir string) bool {
		return dir == string(filepath.Separator) || home != "" && within(dir, home)
	}
	at := cwd
	for up := 0; up <= maxUp && !off(at); up, at = up+1, filepath.Dir(at) {
		if f.isProjectDir(at) {
			root = at
			break
		}
	}
	if root == "" {
		return "", "", false
	}
	boundary = root
	at = root
	for up := 0; up <= maxUp && !off(at); up, at = up+1, filepath.Dir(at) {
		if f.exists(filepath.Join(at, ".git")) {
			boundary = at
			break
		}
	}
	return root, boundary, true
}

func (f files) isProjectDir(dir string) bool {
	for _, m := range markers {
		if f.exists(filepath.Join(dir, m)) {
			return true
		}
	}
	return f.exists(filepath.Join(dir, ".git"))
}
