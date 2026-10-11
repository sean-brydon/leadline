package integrations

import "strings"

// Who a connection, a proxy or a source belongs to.
const (
	// OwnerUser: a person added it, in the app or through the API.
	OwnerUser = "user"
	// OwnerRun: a `leadline run` asked for it, for as long as it runs.
	OwnerRun = "run"
	// OwnerWorktree: `leadline attach` made it for one worktree of a
	// project, and `leadline detach` takes it away again.
	OwnerWorktree = "worktree"
	// OwnerFlag: the daemon was started with it (-pg).
	OwnerFlag = "flag"
	// OwnerScan: a person ticked it in a scan of the machine, which had
	// found its URL in a project's env file. It is theirs like one added
	// by hand; the owner says where it came from.
	OwnerScan = "scan"
)

// SubSep is what stands between an attached source's name and the name of
// one of several commands in it: `leadline run --name worker` in the
// worktree "acme/fix-orders" files under "acme/fix-orders:worker". A
// service named that way belongs to the source, and goes when it does.
const SubSep = ":"

// Owner says who made something and for what. It is what lets detach take
// away exactly what a worktree brought, and the app say where a row came
// from.
type Owner struct {
	// Kind is "user", "run", "worktree", "flag" or "scan".
	Kind string `json:"kind"`
	// Via is the tool that attached a worktree: "shipyard" when Shipyard's
	// hooks did, "cli" when a person ran `leadline attach`.
	Via string `json:"via,omitempty"`
	// Service is the source this belongs to: the worktree's own service
	// name, or the run's.
	Service string `json:"service,omitempty"`
	// What the worktree is: the project it is a checkout of, its own name,
	// its branch, the machine it is on and where.
	Project  string `json:"project,omitempty"`
	Worktree string `json:"worktree,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Box      string `json:"box,omitempty"`
	Path     string `json:"path,omitempty"`
	// For a scan: the variable the URL was found in and the env file it
	// was in, by name (DATABASE_URL, .env.local). Project and Path are
	// then the project that file belongs to, and Branch its branch.
	EnvKey  string `json:"env_key,omitempty"`
	EnvFile string `json:"env_file,omitempty"`
	// Label says it in a few words, for showing as it is: "from Shipyard,
	// worktree fix-orders". It is worked out for each answer, never saved.
	Label string `json:"label,omitempty"`
}

var userOwner = Owner{Kind: OwnerUser}

// labelled is the owner with its Label filled in.
func (o Owner) labelled() Owner {
	switch o.Kind {
	case OwnerWorktree:
		what := "worktree " + o.Worktree
		if o.Worktree == "" {
			what = "project " + o.Project
		}
		if o.Worktree == "" && o.Project == "" {
			what = o.Service
		}
		if o.Via == "shipyard" {
			o.Label = "from Shipyard, " + what
		} else {
			o.Label = "attached, " + what
		}
	case OwnerRun:
		o.Label = "leadline run"
	case OwnerFlag:
		o.Label = "the daemon's -pg flag"
	case OwnerScan:
		o.Label = "found by a scan"
		switch {
		case o.EnvKey != "" && o.Project != "":
			o.Label += ", " + o.EnvKey + " in " + o.Project
		case o.Project != "":
			o.Label += ", in " + o.Project
		case o.EnvKey != "":
			o.Label += ", " + o.EnvKey
		}
	default:
		o.Kind, o.Label = OwnerUser, "added by hand"
	}
	return o
}

// stored is the owner as it is kept: without the label.
func (o Owner) stored() Owner {
	o.Label = ""
	return o
}

// ScanOwner is the owner of a connection saved from a scan: the project
// its URL was found in, that project's folder and branch, and the
// variable and env file that held it. Names only; nothing of a value.
func ScanOwner(project, path, branch, envKey, envFile string) Owner {
	return Owner{Kind: OwnerScan, Project: cleanText(project, 128), Path: clipText(path, 512), Branch: cleanText(branch, 128),
		EnvKey: cleanText(envKey, 128), EnvFile: cleanText(envFile, 128)}
}

// ownerOf is a connection's owner: a person, unless something else says so.
func ownerOf(c *Connection) Owner {
	if c.Owner == nil {
		return userOwner
	}
	return *c.Owner
}

// ownedBy reports whether a connection was attached for the source.
func ownedBy(c *Connection, service string) bool {
	return c.Owner != nil && c.Owner.Kind == OwnerWorktree && c.Owner.Service == service
}

// under reports whether a service is the source's, by its name: the
// source's own, or one of the names under it.
func under(service, source string) bool {
	return service == source || strings.HasPrefix(service, source+SubSep)
}

func cleanText(s string, limit int) string {
	return clipText(strings.Join(strings.Fields(s), " "), limit)
}
