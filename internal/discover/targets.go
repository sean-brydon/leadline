package discover

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
)

// Where a problem should be sent: which Shipyard box and project the
// service's code is in. It is worked out from what Leadline knows of the
// service and what Shipyard lists, by reading only (`berth boxes`,
// `berth locations BOX`, `berth agents BOX`), and only when asked.

// TargetSource is what is known of a service's code. It comes from the
// daemon that holds the service's data, which may be on another machine
// than the one Shipyard's command line is on.
type TargetSource struct {
	Service string `json:"service"`
	// The worktree the service was attached from, when it was.
	Via      string `json:"via,omitempty"`
	Project  string `json:"project,omitempty"`
	Worktree string `json:"worktree,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Box      string `json:"box,omitempty"`
	Path     string `json:"path,omitempty"`
	// Folders the service's commands were run from.
	Folders []string `json:"folders,omitempty"`
	// Remote says the service's data is held by a daemon on another
	// machine than this one, so its folders are that machine's.
	Remote bool `json:"remote,omitempty"`
}

// Target is one place a task could start.
type Target struct {
	Box     string `json:"box"`
	Project string `json:"project"`
	// Base is the ref the task's branch would start from; "" is what the
	// project has checked out.
	Base string `json:"base"`
	// How it was found: "worktree" (the service was attached by that
	// Shipyard worktree) or "folder" (the service's folder is that
	// project's).
	How string `json:"how"`
	// Why says both in a sentence, and BaseWhy why that base.
	Why     string `json:"why"`
	BaseWhy string `json:"base_why"`
}

// Targets is where a task could go: Shipyard's boxes, one box's projects
// and agents, and the place Leadline would pick, if it can tell.
type Targets struct {
	Installed bool     `json:"installed"`
	Available bool     `json:"available"`
	Problem   *Problem `json:"problem,omitempty"`
	Boxes     []Box    `json:"boxes"`
	// Suggested is nil when nothing ties the service to a project: the
	// person picks.
	Suggested *Target `json:"suggested"`
	// Box is the box whose projects and agents are listed: the one asked
	// for, the suggested one, or the first that is online.
	Box      string       `json:"box"`
	Projects []BoxProject `json:"projects"`
	Agents   []Agent      `json:"agents"`
	// BoxProblem says why that box's projects or agents could not be
	// listed.
	BoxProblem *Problem `json:"box_problem,omitempty"`
}

// How many boxes are asked for their projects when looking for a folder.
const maxBoxesSearched = 4

// Targets lists where a task for a service could start. box, when not
// empty, is the box whose projects to list; otherwise it is the suggested
// one's.
func (s *Service) Targets(ctx context.Context, src TargetSource, box string) Targets {
	listing := s.Shipyard(ctx, false)
	out := Targets{Installed: listing.Installed, Available: listing.Available, Problem: listing.Problem,
		Boxes: listing.Boxes, Projects: []BoxProject{}, Agents: []Agent{}}
	if !listing.Available {
		return out
	}
	online := func(name string) bool {
		for _, b := range listing.Boxes {
			if b.Name == name {
				return b.State == "online"
			}
		}
		return false
	}
	known := func(name string) bool {
		for _, b := range listing.Boxes {
			if b.Name == name {
				return true
			}
		}
		return false
	}
	// The boxes worth asking, the likeliest first: this computer for a
	// service recorded here, the others for one recorded elsewhere.
	var search []string
	for _, b := range listing.Boxes {
		if b.State == "online" {
			search = append(search, b.Name)
		}
	}
	local := map[string]bool{}
	for _, b := range listing.Boxes {
		local[b.Name] = b.Local
	}
	sort.SliceStable(search, func(i, j int) bool {
		return local[search[i]] != local[search[j]] && local[search[i]] != src.Remote
	})
	if len(search) > maxBoxesSearched {
		search = search[:maxBoxesSearched]
	}

	// 1. Attached by a Shipyard worktree: it says its project, and its box.
	if src.Via == "shipyard" && src.Project != "" {
		boxes := search
		if known(src.Box) {
			boxes = []string{src.Box}
			if !online(src.Box) {
				// Its own box is away. It is still where the task belongs:
				// say so, and let the box's state say why it can't go yet,
				// rather than find a project of the same folder elsewhere.
				boxes = nil
				out.Suggested = &Target{Box: src.Box, Project: src.Project, How: "worktree",
					Why: fmt.Sprintf("%s was attached by Shipyard from %s on %s.", src.Service, src.Project, src.Box)}
			}
		}
		for _, name := range boxes {
			projects, err := s.BoxProjects(ctx, name)
			if err != nil {
				continue
			}
			for _, p := range projects.Projects {
				if p.Name == src.Project {
					out.Suggested = worktreeTarget(name, p, src)
					break
				}
			}
			if out.Suggested != nil {
				break
			}
		}
	}

	// 2. The service's folder is a Shipyard project's, or inside one.
	if out.Suggested == nil {
		folders := append([]string{}, src.Folders...)
		if src.Path != "" {
			folders = append([]string{src.Path}, folders...)
		}
		best := 0
		for _, name := range search {
			if len(folders) == 0 {
				break
			}
			projects, err := s.BoxProjects(ctx, name)
			if err != nil {
				continue
			}
			for _, p := range projects.Projects {
				if t, depth := folderTarget(name, p, folders); t != nil && depth > best {
					out.Suggested, best = t, depth
				}
			}
			// The first box that has it is the one: boxes are searched in
			// the order they are likely.
			if out.Suggested != nil {
				break
			}
		}
	}

	// The box to list: asked for, suggested, or the first online.
	switch {
	case box != "":
		out.Box = box
	case out.Suggested != nil:
		out.Box = out.Suggested.Box
	case len(search) > 0:
		out.Box = search[0]
	case len(listing.Boxes) > 0:
		out.Box = listing.Boxes[0].Name
	}
	if out.Box == "" {
		return out
	}
	if !known(out.Box) {
		out.BoxProblem = &Problem{Code: "no_box", Message: "Shipyard has no box of that name."}
		return out
	}
	if !online(out.Box) {
		for _, b := range listing.Boxes {
			if b.Name == out.Box {
				out.BoxProblem = boxAway(b)
			}
		}
		return out
	}
	projects, err := s.BoxProjects(ctx, out.Box)
	if err != nil {
		out.BoxProblem = asProblem(err)
		return out
	}
	out.Projects = projects.Projects
	if agents, err := s.Agents(ctx, out.Box); err != nil {
		out.BoxProblem = asProblem(err)
	} else {
		out.Agents = agents
	}
	return out
}

// worktreeTarget is the target for a service a Shipyard worktree attached.
//
// The task starts from that worktree's branch, not from the project's
// default: the problem was recorded running that branch's code, which may
// not be on the default branch at all, and a fix cut from anywhere else
// could not be checked against the code that showed it. When the worktree
// is gone, or is the project's own checkout, there is no such branch to
// name and the task starts from what the project has checked out.
func worktreeTarget(box string, p BoxProject, src TargetSource) *Target {
	t := &Target{Box: box, Project: p.Name, How: "worktree"}
	if src.Worktree == "" {
		t.Why = fmt.Sprintf("%s is the project %s on %s, attached by Shipyard.", src.Service, p.Name, box)
		t.BaseWhy = "It runs the project's own checkout, so the task starts from what that has checked out."
		return t
	}
	t.Why = fmt.Sprintf("%s is the worktree %s of %s on %s, attached by Shipyard.", src.Service, src.Worktree, p.Name, box)
	for _, w := range p.Worktrees {
		if w.Name == src.Worktree && !w.Main && w.Branch != "" && refName.MatchString(w.Branch) {
			t.Base = w.Branch
			t.BaseWhy = fmt.Sprintf("The problem was recorded running the branch %s, so the task starts from it. What is not committed there is not in it.", w.Branch)
			return t
		}
	}
	t.BaseWhy = fmt.Sprintf("The worktree %s is no longer there, so the task starts from what the project has checked out.", src.Worktree)
	return t
}

// folderTarget matches a service's folders against a project: the
// project's own folder, one of its worktrees', or a folder inside either.
// depth says how exact the match is, so the nearest one wins.
func folderTarget(box string, p BoxProject, folders []string) (t *Target, depth int) {
	inside := func(folder, root string) bool {
		folder, root = path.Clean(folder), path.Clean(root)
		return root != "/" && root != "." && (folder == root || strings.HasPrefix(folder, root+"/"))
	}
	for _, folder := range folders {
		if folder == "" {
			continue
		}
		for _, w := range p.Worktrees {
			if w.Main || w.Path == "" || !inside(folder, w.Path) || len(w.Path) <= depth {
				continue
			}
			t, depth = &Target{Box: box, Project: p.Name, How: "folder"}, len(w.Path)
			t.Why = fmt.Sprintf("Its folder is the worktree %s of %s on %s.", w.Name, p.Name, box)
			if w.Branch != "" && refName.MatchString(w.Branch) {
				t.Base = w.Branch
				t.BaseWhy = fmt.Sprintf("The problem was recorded running the branch %s, so the task starts from it. What is not committed there is not in it.", w.Branch)
			} else {
				t.BaseWhy = "The task starts from what the project has checked out."
			}
		}
		if p.Path != "" && inside(folder, p.Path) && len(p.Path) > depth {
			t, depth = &Target{Box: box, Project: p.Name, How: "folder"}, len(p.Path)
			t.Why = fmt.Sprintf("Its folder is the project %s on %s.", p.Name, box)
			t.BaseWhy = "It runs the project's own checkout, so the task starts from what that has checked out."
		}
	}
	return t, depth
}
