package discover

import (
	"path/filepath"
	"sort"
	"strings"
)

// gitAt reads how dir stands in git, when dir is the root of a checkout:
// a `.git` directory (the repository's own checkout) or a `.git` file (a
// linked worktree). It reads small files of git's own and runs nothing.
//
// A linked worktree's HEAD is kept in the main repository, in
// .git/worktrees/<name>/. That one file is read wherever the repository
// is: it is the only thing this package opens outside the folders it was
// pointed at, and it is the worktree's own.
func (f files) gitAt(dir string) *Git {
	dotgit := filepath.Join(dir, ".git")
	st, err := f.lstat(dotgit)
	if err != nil {
		return nil
	}
	g := &Git{Root: dir}
	if st.IsDir() {
		g.Branch = f.headBranch(filepath.Join(dotgit, "HEAD"))
		g.Worktrees = f.worktreesOf(dotgit)
		return g
	}
	if !st.Mode().IsRegular() {
		return nil
	}
	b, err := f.readFile(dotgit, 4096)
	if err != nil {
		return nil
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return nil
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(dir, gitdir)
	}
	gitdir = filepath.Clean(gitdir)
	// <main>/.git/worktrees/<name>: anything else (a submodule's
	// .git/modules/<name>) is a checkout, but not a worktree to follow.
	parent := filepath.Dir(gitdir)
	if filepath.Base(parent) != "worktrees" {
		return g
	}
	g.Worktree = true
	common := filepath.Dir(parent)
	g.MainRepo = common
	if filepath.Base(common) == ".git" {
		g.MainRepo = filepath.Dir(common)
	}
	g.Branch = f.headBranch(filepath.Join(gitdir, "HEAD"))
	return g
}

// headBranch reads a HEAD file: the branch it is on, or the short commit
// when it is on none.
func (f files) headBranch(path string) string {
	b, err := f.readFile(path, 4096)
	if err != nil {
		return ""
	}
	head := strings.TrimSpace(string(b))
	if ref, ok := strings.CutPrefix(head, "ref:"); ok {
		return strings.TrimPrefix(strings.TrimSpace(ref), "refs/heads/")
	}
	if len(head) >= 7 && isHex(head) {
		return head[:7]
	}
	return ""
}

func isHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// worktreesOf lists a repository's linked worktrees from its .git
// directory: where each is, and its branch.
func (f files) worktreesOf(dotgit string) []GitWorktree {
	entries, err := f.readDir(filepath.Join(dotgit, "worktrees"))
	if err != nil {
		return nil
	}
	var out []GitWorktree
	for _, e := range entries {
		if !e.IsDir() || len(out) >= 100 {
			continue
		}
		at := filepath.Join(dotgit, "worktrees", e.Name())
		// gitdir holds the path of the worktree's own .git file.
		b, err := f.readFile(filepath.Join(at, "gitdir"), 4096)
		if err != nil {
			continue
		}
		path := strings.TrimSpace(string(b))
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(at, path)
		}
		out = append(out, GitWorktree{Path: filepath.Dir(filepath.Clean(path)), Branch: f.headBranch(filepath.Join(at, "HEAD"))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// gitRootAbove finds the checkout dir is in, looking no higher than
// boundary: dir itself, or a folder above it that has a .git.
func (f files) gitRootAbove(dir, boundary string) string {
	for at := dir; within(boundary, at); at = filepath.Dir(at) {
		if f.exists(filepath.Join(at, ".git")) {
			return at
		}
		if filepath.Dir(at) == at {
			break
		}
	}
	return ""
}
