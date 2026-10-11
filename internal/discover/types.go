package discover

// Everything here leaves the daemon as JSON. None of it can hold a
// connection URL as it was written: a URL is always the masked one.

// Machine is the computer the daemon runs on.
type Machine struct {
	// "darwin" or "linux".
	OS       string `json:"os"`
	Hostname string `json:"hostname"`
}

// Listening is what is listening on this machine now.
type Listening struct {
	Machine   Machine    `json:"machine"`
	Listeners []Listener `json:"listeners"`
	Limits    Limits     `json:"limits"`
}

// Listener is one port a process is listening on.
type Listener struct {
	Port int `json:"port"`
	// Address is what it is bound to; Scope says what that means:
	// "loopback" (this machine only), "all" (every interface) or
	// "address" (one address of this machine's).
	Address string `json:"address"`
	Scope   string `json:"scope"`
	// PID is 0, and the fields after it empty, for a port whose process
	// belongs to someone else (see Limits).
	PID     int    `json:"pid"`
	Process string `json:"process"`
	// Command is the command line, with any URL's password hidden.
	Command   string  `json:"command"`
	Cwd       string  `json:"cwd"`
	StartedMs float64 `json:"started_ms,omitempty"`
	UptimeS   int64   `json:"uptime_s,omitempty"`
	// Kind is what it probably is: "dev-server", "postgres", "redis",
	// "mysql", "mongodb" or "other". What is that in words ("next dev",
	// "Postgres"); Framework names a dev server's ("next", "vite").
	Kind      string `json:"kind"`
	What      string `json:"what"`
	Framework string `json:"framework,omitempty"`
	// Basis is how Kind was decided: "process" (its name), "command" (its
	// command line), "cwd" (a runtime running in a project's folder),
	// "env-url" (a connection URL in an env file points at this port) or
	// "port" (only the port number, which is a guess).
	Basis string `json:"basis,omitempty"`
	// ProjectID is the project its working directory is in, in a scan.
	ProjectID string `json:"project_id,omitempty"`
}

// Limits says what the list could not include, and why.
type Limits struct {
	// Method is how the system was asked: "libproc" (macOS), "procfs"
	// (Linux) or "none" where there is no way built in.
	Method string `json:"method"`
	// Complete is true when every process could be inspected.
	Complete bool `json:"complete"`
	// SkippedProcesses is how many processes would not show their
	// sockets: they belong to another user.
	SkippedProcesses int `json:"skipped_processes"`
	// UnownedPorts is how many listed ports have no process (Linux: the
	// port is visible, its owner is not).
	UnownedPorts int `json:"unowned_ports"`
	// Note is the same, as a sentence for a person. Empty when complete.
	Note string `json:"note,omitempty"`
}

// Scan is a scan: under way, or its result.
type Scan struct {
	ID string `json:"id"`
	// State is "running", "done", "cancelled" or "failed".
	State      string   `json:"state"`
	StartedMs  float64  `json:"started_ms"`
	FinishedMs float64  `json:"finished_ms,omitempty"`
	Error      *Problem `json:"error,omitempty"`
	Progress   Progress `json:"progress"`

	// Folders are the folders that were asked for, and what became of
	// each.
	Folders []Folder `json:"folders"`

	Machine   Machine    `json:"machine"`
	Listeners []Listener `json:"listeners"`
	Limits    Limits     `json:"limits"`
	Projects  []Project  `json:"projects"`
	// Connections are the connection URLs found in env files. A scan
	// opens no env file: this is empty until the env files of some
	// projects are asked for by name (see Service.ReadEnv), and holds
	// what was found in those.
	Connections []Connection `json:"connections"`
	// EnvProjects are the ids of the projects whose env files have been
	// read; Env adds up what was taken from them, and not.
	EnvProjects []string  `json:"env_projects"`
	Env         EnvTotals `json:"env"`

	// SecretsUntilMs is until when the daemon keeps the found URLs so
	// they can be saved. After it, saving answers "expired": scan again.
	SecretsUntilMs float64 `json:"secrets_until_ms,omitempty"`
}

// Progress is how far a running scan is.
type Progress struct {
	// Phase is "listening", "projects", "folders" or "done".
	Phase        string `json:"phase"`
	FoldersDone  int    `json:"folders_done"`
	FoldersTotal int    `json:"folders_total"`
	// Directories is how many directories have been listed so far.
	Directories int `json:"directories"`
	Projects    int `json:"projects"`
}

// Folder is one folder a person pointed at.
type Folder struct {
	// Path is the folder as asked for; Resolved is where that is.
	Path     string `json:"path"`
	Resolved string `json:"resolved,omitempty"`
	// State is "pending", "scanned", "refused" (Leadline won't walk it:
	// the home directory, the root), "missing" or "cancelled".
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	// Projects found under it, and whether the walk stopped early at
	// its limit of directories or projects.
	Projects    int  `json:"projects"`
	Directories int  `json:"directories"`
	Truncated   bool `json:"truncated,omitempty"`
}

// Project is one project: a folder with a project file, or a git root.
type Project struct {
	// ID is the same for the same path in every scan.
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
	// Sources is how it was found: "listening", "run", "folder".
	Sources []string `json:"sources"`
	// Stack is "node", "go", "python", "ruby", "rust" or "unknown".
	Stack      string      `json:"stack"`
	Frameworks []Framework `json:"frameworks"`
	// Label is the stack as a person would say it: "Next.js 16",
	// "Vite + Express", "Go 1.27".
	Label          string `json:"label"`
	PackageManager string `json:"package_manager,omitempty"`
	// DevCommand is what starts it in development, as typed in a
	// terminal; DevScript is where that came from.
	DevCommand string     `json:"dev_command,omitempty"`
	DevScript  *DevScript `json:"dev_script,omitempty"`
	// OpenTelemetry: the project already has an OpenTelemetry SDK.
	OpenTelemetry bool `json:"opentelemetry"`
	// Sends is whether it already sends to Leadline.
	Sends Sends `json:"sends"`
	// Git is absent for a folder that is in no repository.
	Git *Git `json:"git,omitempty"`
	// Connect is the recommended way to connect it.
	Connect Connect `json:"connect"`
	// Running is what of it is listening now.
	Running []Running `json:"running"`
	// EnvFiles are the env files the project has, by name. A scan does
	// not open them; one that has since been read has its counts.
	EnvFiles []EnvFile `json:"env_files"`
	// ConnectionIDs are the connection URLs found in them (see
	// Scan.Connections).
	ConnectionIDs []string `json:"connection_ids"`

	// For the advice: the project's next.config, and whether
	// @leadline/next is already a dependency. boundary is as far up as
	// anything about the project is read.
	nextConfig string
	hasPlugin  bool
	boundary   string
}

// Framework is a framework a project uses.
type Framework struct {
	// ID is a fixed word ("next", "vite", "express", "django", "rails",
	// "gin"…); Name is how it is written ("Next.js").
	ID   string `json:"id"`
	Name string `json:"name"`
	// Version is the installed version when the package is installed,
	// else the one the project file asks for; empty when neither says.
	Version string `json:"version,omitempty"`
}

// DevScript is where a dev command came from.
type DevScript struct {
	// File is the file that defines it, relative to the project.
	File string `json:"file"`
	// Name is the script's name there ("dev"); Command what it runs.
	Name    string `json:"name,omitempty"`
	Command string `json:"command,omitempty"`
}

// Sends is whether a project already sends to Leadline, and how that is
// known.
type Sends struct {
	Already bool `json:"already"`
	// Because lists what was found: "service" (Leadline holds data under
	// the project's name), "run" (a `leadline run` was seen in its
	// folder), "plugin" (@leadline/next is wired in), "provider"
	// (@leadline/node is installed), "dev-script" (its dev script already
	// uses `leadline run`).
	Because []string `json:"because"`
	// Service is the name its data is under, when there is some.
	Service string `json:"service,omitempty"`
}

// Git is where a project is in git.
type Git struct {
	// Root is the checkout the project is in, which is the project's own
	// path unless it is a package inside a repository.
	Root   string `json:"root"`
	Branch string `json:"branch,omitempty"`
	// Worktree: the checkout is a linked worktree. MainRepo is then the
	// repository it belongs to.
	Worktree bool   `json:"worktree"`
	MainRepo string `json:"main_repo,omitempty"`
	// Worktrees are a main checkout's linked worktrees.
	Worktrees []GitWorktree `json:"worktrees,omitempty"`
}

// GitWorktree is one linked worktree of a repository.
type GitWorktree struct {
	Path   string `json:"path"`
	Branch string `json:"branch,omitempty"`
}

// Connect is advice on connecting a project. Nothing is changed by it.
type Connect struct {
	// Kind is "next-plugin", "run-wrap", "otel-env" or "already".
	Kind string `json:"kind"`
	// Summary is one sentence for a person.
	Summary string `json:"summary"`
	// Command is the exact command to run, when there is one.
	Command string `json:"command,omitempty"`
	// Lines are the exact lines to add to File (relative to the project).
	File  string   `json:"file,omitempty"`
	Lines []string `json:"lines,omitempty"`
	// EnvLines are lines to add to EnvFile as well, when the daemon is
	// not at its usual address: where the plugin should send.
	EnvFile  string   `json:"env_file,omitempty"`
	EnvLines []string `json:"env_lines,omitempty"`
	// Traces is a further step, for a project whose first step gives
	// logs and queries but not traces.
	Traces *ConnectStep `json:"traces,omitempty"`
}

// ConnectStep is an optional further step of the advice.
type ConnectStep struct {
	Summary string `json:"summary"`
	Command string `json:"command,omitempty"`
}

// Running is a listener that belongs to a project.
type Running struct {
	Port    int    `json:"port"`
	PID     int    `json:"pid"`
	What    string `json:"what"`
	UptimeS int64  `json:"uptime_s,omitempty"`
}

// EnvFile is one env file of a project.
type EnvFile struct {
	// File is its name, relative to the project.
	File string `json:"file"`
	// Read is false for a file that is only known to be there: it has
	// not been opened, and the counts are zero.
	Read bool `json:"read"`
	// URLs is how many connection URLs were taken from it; OtherKeys how
	// many other variables it sets, whose values were not kept.
	URLs      int `json:"urls"`
	OtherKeys int `json:"other_keys"`
}

// EnvTotals adds the env files of every project up.
type EnvTotals struct {
	FilesRead int `json:"files_read"`
	// URLs is how many connection URLs were taken, before the same URL
	// in two places was made one.
	URLs      int `json:"urls"`
	OtherKeys int `json:"other_keys"`
}

// Connection is a connection URL found in an env file. The daemon keeps
// the URL itself, under ID, until SecretsUntilMs.
type Connection struct {
	ID string `json:"id"`
	// Kind is "postgres", "redis", "mysql" or "mongodb".
	Kind string `json:"kind"`
	// URL has **** where the password is.
	URL      string `json:"url"`
	Host     string `json:"host"`
	Database string `json:"database"`
	User     string `json:"user"`
	// Remote: the server is on another machine, by the same rule saved
	// connections go by.
	Remote bool `json:"remote"`
	// Connectable: Leadline can save and read it. When false, Reason is
	// "not_supported" (MySQL, MongoDB), "interpolated" (the URL refers
	// to other variables, which were not read) or "bad_url", and Note
	// says it in a sentence.
	Connectable bool   `json:"connectable"`
	Reason      string `json:"reason,omitempty"`
	Note        string `json:"note,omitempty"`
	// CaptureAvailable: capture can be turned on when saving it (a
	// server on this machine). Suggested is whether onboarding should
	// have it ticked to begin with.
	CaptureAvailable bool `json:"capture_available"`
	Suggested        bool `json:"suggested"`
	// FoundIn is every place the URL was found.
	FoundIn []FoundIn `json:"found_in"`
	// Saved: it is already a saved connection, whose id is SavedID.
	Saved   bool   `json:"saved"`
	SavedID string `json:"saved_id,omitempty"`
	// Listening is the listener on this machine the URL points at.
	Listening *Running `json:"listening,omitempty"`
}

// FoundIn is one place a connection URL was found.
type FoundIn struct {
	ProjectID string `json:"project_id"`
	Project   string `json:"project"`
	// Key is the variable; File the env file, relative to the project.
	Key  string `json:"key"`
	File string `json:"file"`
	// Effective is false when a file that outranks this one sets the
	// same variable, so the dev tools would not use this value.
	Effective bool `json:"effective"`
}

// Problem is an error written for the person at the screen, in the shape
// the integrations API uses.
type Problem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

func (p *Problem) Error() string { return p.Message }

// SaveItem is one found connection to save.
type SaveItem struct {
	ID string `json:"id"`
	// Capture turns capture on or off for it.
	Capture bool `json:"capture"`
	// Name is optional; without it the connection is named as one added
	// by hand would be.
	Name *string `json:"name,omitempty"`
}

// SaveResult is what saving found connections did.
type SaveResult struct {
	Saved  []Saved      `json:"saved"`
	Failed []SaveFailed `json:"failed"`
}

// Saved is one connection that was saved.
type Saved struct {
	ID string `json:"id"`
	// Already: it was a saved connection before; only its capture
	// setting may have changed.
	Already bool `json:"already"`
	// Connection is the saved connection, as /api/connections gives it.
	Connection any `json:"connection"`
}

// SaveFailed is one that was not.
type SaveFailed struct {
	ID    string   `json:"id"`
	Error *Problem `json:"error"`
}

// TestResults is what trying found connections found.
type TestResults struct {
	Results []TestedConnection `json:"results"`
}

// TestedConnection is one found connection, tried.
type TestedConnection struct {
	ID string `json:"id"`
	// Result is what /api/connections/test answers. Absent with Error.
	Result any      `json:"result,omitempty"`
	Error  *Problem `json:"error,omitempty"`
}

// Shipyard is whether Shipyard is installed here, and its boxes.
type Shipyard struct {
	// Installed: the berth command was found. Available: it also
	// answered. When Available is false the app shows "Shipyard not
	// found" and Problem says why.
	Installed bool `json:"installed"`
	Available bool `json:"available"`
	// CLI is where the command is.
	CLI       string   `json:"cli,omitempty"`
	Boxes     []Box    `json:"boxes"`
	CheckedMs float64  `json:"checked_ms"`
	Problem   *Problem `json:"problem,omitempty"`
}

// Box is one of the owner's Shipyard boxes.
type Box struct {
	Name string `json:"name"`
	// State is Shipyard's: "online", "offline", "connecting" or
	// "untrusted".
	State     string  `json:"state"`
	Error     string  `json:"error,omitempty"`
	LatencyMs int64   `json:"latency_ms,omitempty"`
	SinceMs   float64 `json:"since_ms,omitempty"`
	// Local: the box is this computer itself.
	Local bool `json:"local,omitempty"`
	// Leadline is whether Leadline is on the box.
	Leadline BoxLeadline `json:"leadline"`
}

// BoxLeadline is Leadline's standing on a box. Today it is always
// "unsupported": Leadline cannot run on a box yet. The fields are here so
// the answer can become true without the shape changing.
type BoxLeadline struct {
	// State is "unsupported" now; later "missing", "installed",
	// "reachable".
	State     string `json:"state"`
	Installed bool   `json:"installed"`
	Reachable bool   `json:"reachable"`
	// URL is where its API is reached from this machine, when Reachable.
	URL  string `json:"url,omitempty"`
	Note string `json:"note,omitempty"`
}

// BoxProjects is one box's projects, as Shipyard knows them.
type BoxProjects struct {
	Box       string       `json:"box"`
	Projects  []BoxProject `json:"projects"`
	CheckedMs float64      `json:"checked_ms"`
}

// BoxProject is a Shipyard location: a repository or folder on a box.
type BoxProject struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// Repo: the path is the root of a git repository.
	Repo          bool          `json:"repo"`
	Slug          string        `json:"slug,omitempty"`
	DefaultBranch string        `json:"default_branch,omitempty"`
	Worktrees     []BoxWorktree `json:"worktrees"`
	// Agents are the agents the repository's own config adds to the
	// box's, by id and name. Absent when it adds none.
	Agents []Agent `json:"agents,omitempty"`
}

// BoxWorktree is one worktree of a location.
type BoxWorktree struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Branch string `json:"branch,omitempty"`
	// Main marks the repository's own checkout.
	Main bool `json:"main,omitempty"`
	// Title is the name a person gave it in Shipyard, if any.
	Title string `json:"title,omitempty"`
	// Port is the first of the worktree's own ports.
	Port int `json:"port,omitempty"`
}
