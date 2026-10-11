package discover

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Connections is the part of the integrations manager discovery needs. It
// is an interface so this package does not import that one (and so builds
// without the database engine, for a Linux test in a bare container); the
// API wires the real manager in.
type Connections interface {
	// Saved lists the saved connections.
	Saved() []SavedConnection
	// Add saves a connection by its URL and returns it as the API shows
	// one. found is where the URL was found, which the saved connection
	// then says as its owner. An error it returns is a *Problem.
	Add(ctx context.Context, kind, url string, name *string, capture bool, found Found) (any, error)
	// SetCapture turns capture on or off for a saved connection.
	SetCapture(ctx context.Context, id string, capture bool) (any, error)
	// Test tries a URL without saving it, and returns what the API
	// answers for a test.
	Test(ctx context.Context, kind, url string) any
}

// SavedConnection is a saved connection, as much as is needed to tell
// whether a found URL is it.
type SavedConnection struct {
	ID       string
	Kind     string
	Host     string
	Database string
	User     string
}

// Found is where a connection URL that is being saved was found: the
// project (its name, folder and branch), the variable and the env file.
// Names only. When the URL is in several places it is the one the dev
// tools would use: the first that no other file outranks.
type Found struct {
	Project string
	Path    string
	Branch  string
	Key     string
	File    string
}

// Run is a `leadline run` command the daemon has seen.
type Run struct {
	Cwd     string
	Service string
}

// Options are what a Service is made with. Every field may be left out.
type Options struct {
	Connections Connections
	// Runs lists the `leadline run` commands the daemon has seen.
	Runs func() []Run
	// Services lists the names Leadline holds data under.
	Services func(ctx context.Context) []string

	// The rest are for tests; the zero value of each is the real thing.

	// Processes is how the machine is asked what is listening.
	Processes Processes
	// Home is the home directory, which is never walked.
	Home string
	// Berth finds Shipyard's command line; "" from it means not installed.
	// BerthTimeout is how long it is given to answer.
	Berth        func() string
	BerthTimeout time.Duration
	// TaskTimeout is how long starting a Shipyard task may take.
	TaskTimeout time.Duration
	// SecretsFor is how long found URLs are kept after a scan.
	SecretsFor time.Duration
}

// Processes is how a Service asks what is listening. The zero value asks
// the operating system.
type Processes struct{ sys system }

// NoProcesses is a machine with nothing running on it. A test of the API
// uses it, so that what it finds does not depend on, and it never reads,
// whatever is running on the machine the test runs on.
func NoProcesses() Processes { return Processes{sys: nothingRunning{}} }

type nothingRunning struct{}

func (nothingRunning) sockets() ([]rawSocket, Limits) {
	return nil, Limits{Method: "none", Complete: true}
}
func (nothingRunning) process(int) (procInfo, bool) { return procInfo{}, false }

// Service is discovery for one daemon: the scan under way or last made,
// and what is known of Shipyard.
type Service struct {
	opts Options
	sys  system
	fs   files
	home string
	self int
	now  func() time.Time
	// secretsFor is how long found URLs are kept after a scan ends.
	secretsFor time.Duration

	mu  sync.Mutex
	cur *job
	// envMu lets one ReadEnv run at a time.
	envMu sync.Mutex

	ship *shipyard
}

// New makes a Service that looks at this machine.
func New(opts Options) *Service {
	home := opts.Home
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	s := &Service{opts: opts, sys: opts.Processes.sys, fs: osFiles(), home: home, self: os.Getpid(), now: time.Now,
		secretsFor: opts.SecretsFor, ship: newShipyard(home)}
	if s.sys == nil {
		s.sys = hostSystem()
	}
	if s.secretsFor <= 0 {
		s.secretsFor = 30 * time.Minute
	}
	if opts.Berth != nil {
		s.ship.find = opts.Berth
	}
	if opts.BerthTimeout > 0 {
		s.ship.boxesTimeout, s.ship.locationsTimeout = opts.BerthTimeout, opts.BerthTimeout
	}
	if opts.TaskTimeout > 0 {
		s.ship.taskTimeout = opts.TaskTimeout
	}
	return s
}

// job is one scan. Its public face is scan; secrets is the other half:
// the found URLs as written, by id, which no response is made from.
type job struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	// Guarded by Service.mu. The slices in scan are replaced, never
	// changed in place, so a copy of scan handed out stays as it was.
	scan Scan
	// What reading env files has found. secrets is the found URLs as
	// written, by id; identities says which id a URL already has, so the
	// same one found again is the same connection. Both hold passwords,
	// and neither is what a response is made from.
	secrets    map[string]*target
	identities map[string]string
	// envRead is the projects whose env files have been read.
	envRead map[string]bool
	expired bool
	expire  *time.Timer
}

// Listening is what is listening now. It reads no project file: a runtime
// (node, python…) is taken for a dev server when its working directory is
// a project, which is a few stats.
func (s *Service) Listening() Listening {
	ls, limits := listening(s.sys, s.self, s.now())
	for i := range ls {
		if ls[i].Kind != "other" {
			continue
		}
		if rt := runtimeOf(ls[i].Process); rt != "" {
			if _, _, ok := s.fs.rootFor(ls[i].Cwd, s.home); ok {
				ls[i].Kind, ls[i].What, ls[i].Framework, ls[i].Basis = "dev-server", rt, rt, "cwd"
			}
		}
	}
	sortListeners(ls)
	return Listening{Machine: thisMachine(), Listeners: ls, Limits: limits}
}

// ScanOptions say what to scan.
type ScanOptions struct {
	// Folders a person pointed at. Without any, the scan is of what is
	// listening and of the `leadline run` commands seen.
	Folders []string
	// Daemon is the daemon's address as the app reached it, for the
	// advice that names it.
	Daemon string
}

// Start begins a scan and returns it, running. A scan under way is
// cancelled, and what the last one found is forgotten: its ids stop
// working.
func (s *Service) Start(opts ScanOptions) (Scan, error) {
	if len(opts.Folders) > maxFolders {
		return Scan{}, &Problem{Code: "bad_request", Message: "A scan takes at most " + strconv.Itoa(maxFolders) + " folders."}
	}
	ctx, cancel := context.WithCancel(context.Background())
	j := &job{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	j.scan = Scan{ID: newID("s"), State: "running", StartedMs: ms(s.now()),
		Progress: Progress{Phase: "listening", FoldersTotal: len(opts.Folders)},
		Folders:  []Folder{}, Machine: thisMachine(), Listeners: []Listener{}, Projects: []Project{}, Connections: []Connection{}, EnvProjects: []string{}}
	for _, f := range opts.Folders {
		j.scan.Folders = append(j.scan.Folders, Folder{Path: f, State: "pending"})
	}

	s.mu.Lock()
	s.dropLocked()
	s.cur = j
	out := j.scan
	s.mu.Unlock()

	go s.run(j, opts)
	return out, nil
}

// dropLocked stops the current scan and forgets its secrets.
func (s *Service) dropLocked() {
	j := s.cur
	if j == nil {
		return
	}
	j.cancel()
	if j.expire != nil {
		j.expire.Stop()
	}
	j.secrets, j.identities = nil, nil
	if j.scan.State == "running" {
		j.scan.State = "cancelled"
		j.scan.FinishedMs = ms(s.now())
	}
	s.cur = nil
}

// Cancel stops the scan and forgets what it found.
func (s *Service) Cancel() {
	s.mu.Lock()
	s.dropLocked()
	s.mu.Unlock()
}

// Scan returns the latest scan; false when there is none.
func (s *Service) Scan() (Scan, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return Scan{}, false
	}
	return s.cur.scan, true
}

// Wait returns the latest scan once it is no longer running, or after
// wait, or when ctx ends, whichever is first.
func (s *Service) Wait(ctx context.Context, wait time.Duration) (Scan, bool) {
	s.mu.Lock()
	j := s.cur
	s.mu.Unlock()
	if j == nil {
		return Scan{}, false
	}
	return s.waitFor(ctx, j, wait), true
}

// waitFor is Wait for a scan already in hand.
func (s *Service) waitFor(ctx context.Context, j *job, wait time.Duration) Scan {
	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-j.done:
		case <-j.ctx.Done():
		case <-ctx.Done():
		case <-t.C:
		}
	}
	// The scan that was waited for, even if a newer one has replaced it:
	// the caller is told it was cancelled, and asks again.
	s.mu.Lock()
	defer s.mu.Unlock()
	return j.scan
}

// update changes a job's public scan, unless the job has been replaced.
func (s *Service) update(j *job, change func(*Scan)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != j || j.ctx.Err() != nil {
		return false
	}
	change(&j.scan)
	return true
}

func (s *Service) run(j *job, opts ScanOptions) {
	defer close(j.done)
	defer func() {
		// A scan must not take the daemon down with it, whatever it met
		// on disk. The message is fixed: a panic's text could quote what
		// was being read.
		if recover() != nil {
			s.update(j, func(sc *Scan) {
				sc.State, sc.FinishedMs = "failed", ms(s.now())
				sc.Error = &Problem{Code: "failed", Message: "The scan stopped on something it could not read."}
			})
		}
	}()
	ctx := j.ctx

	// What is listening.
	listeners, limits := listening(s.sys, s.self, s.now())
	if !s.update(j, func(sc *Scan) {
		sc.Listeners, sc.Limits = append([]Listener{}, listeners...), limits
		sc.Progress.Phase = "projects"
	}) {
		return
	}

	// Projects: where the dev servers and the runs are.
	found := newProjects()
	for _, l := range listeners {
		if l.Kind != "dev-server" && !(l.Kind == "other" && runtimeOf(l.Process) != "") {
			continue
		}
		if root, boundary, ok := s.fs.rootFor(l.Cwd, s.home); ok {
			found.add(s.fs, root, boundary, "listening")
		}
	}
	var runs []Run
	if s.opts.Runs != nil {
		runs = s.opts.Runs()
	}
	for _, r := range runs {
		if root, boundary, ok := s.fs.rootFor(r.Cwd, s.home); ok {
			found.add(s.fs, root, boundary, "run")
		}
	}
	if ctx.Err() != nil {
		return
	}

	// Projects: the folders a person pointed at.
	s.update(j, func(sc *Scan) { sc.Progress.Phase, sc.Progress.Projects = "folders", len(found.list) })
	for i, name := range opts.Folders {
		if ctx.Err() != nil {
			return
		}
		folder := Folder{Path: name}
		folder.Resolved, folder.State, folder.Reason = resolveFolder(name, s.home)
		if folder.State == "scanned" {
			w := s.fs.walk(ctx, folder.Resolved, func() {
				s.update(j, func(sc *Scan) { sc.Progress.Directories++ })
			})
			if ctx.Err() != nil {
				return
			}
			for _, p := range w.projects {
				found.merge(p, "folder")
			}
			folder.Projects, folder.Directories, folder.Truncated = len(w.projects), w.dirs, w.truncated
		}
		s.update(j, func(sc *Scan) {
			folders := append([]Folder{}, sc.Folders...)
			folders[i] = folder
			sc.Folders = folders
			sc.Progress.FoldersDone, sc.Progress.Projects = i+1, len(found.list)
		})
	}

	// Who is running, and who already sends.
	projects := found.list
	for i := range listeners {
		l := &listeners[i]
		p := deepest(projects, l.Cwd)
		if p == nil || l.PID == 0 {
			continue
		}
		if l.Kind == "other" {
			rt := runtimeOf(l.Process)
			if rt == "" {
				continue
			}
			// A runtime listening from inside a project: its dev server.
			l.Kind, l.What, l.Framework, l.Basis = "dev-server", rt, rt, "cwd"
			if fw := leadFramework(p); fw != "" {
				l.Framework = fw
			}
		}
		l.ProjectID = p.ID
		p.Running = append(p.Running, Running{Port: l.Port, PID: l.PID, What: l.What, UptimeS: l.UptimeS})
	}
	services := map[string]bool{}
	if s.opts.Services != nil {
		for _, name := range s.opts.Services(ctx) {
			services[name] = true
		}
	}
	for _, p := range projects {
		for _, r := range runs {
			if deepest(projects, r.Cwd) == p {
				p.Sends.Because = addUnique(p.Sends.Because, "run")
				if p.Sends.Service == "" {
					p.Sends.Service = r.Service
				}
			}
		}
		if services[p.Name] {
			p.Sends.Because = addUnique(p.Sends.Because, "service")
			p.Sends.Service = p.Name
		} else if p.Sends.Service != "" && services[p.Sends.Service] {
			p.Sends.Because = addUnique(p.Sends.Because, "service")
		}
		p.Sends.Already = len(p.Sends.Because) > 0
		p.Connect = advise(p, trimDaemon(opts.Daemon))
	}
	if ctx.Err() != nil {
		return
	}

	// Env files are not opened here. Reading them is a step of its own,
	// for the projects a person picks: see ReadEnv.
	sortListeners(listeners)
	sort.SliceStable(projects, func(a, b int) bool {
		if ra, rb := len(projects[a].Running) > 0, len(projects[b].Running) > 0; ra != rb {
			return ra
		}
		return projects[a].Path < projects[b].Path
	})
	out := make([]Project, len(projects))
	for i, p := range projects {
		out[i] = *p
	}
	s.update(j, func(sc *Scan) {
		sc.Listeners, sc.Projects = listeners, out
		sc.Progress.Phase, sc.Progress.Projects = "done", len(out)
		sc.State, sc.FinishedMs = "done", ms(s.now())
	})
}

// maxEnvProjects is how many projects one request may name.
const maxEnvProjects = 200

// ReadEnv reads the env files of the named projects of the latest scan
// for connection URLs, and returns the scan with what was found. It is
// the only thing in this package that opens an env file, and it opens
// those of the projects it is given and no others: there is no way to ask
// for "all of them" but to name them.
//
// A project is named by its id or its path, as the scan gave them. One
// whose env files were already read is left as it is, so asking again
// with a longer list reads only what is new, and ids stay as they were.
func (s *Service) ReadEnv(ctx context.Context, names []string) (Scan, error) {
	if len(names) == 0 {
		return Scan{}, &Problem{Code: "bad_request", Message: "Name the projects whose env files to read.",
			Hint: "Send {\"projects\": [...]} with ids or paths from the scan."}
	}
	if len(names) > maxEnvProjects {
		return Scan{}, &Problem{Code: "bad_request", Message: "At most " + strconv.Itoa(maxEnvProjects) + " projects can be named at once."}
	}
	// One at a time: two readers adding to the same scan would each need
	// the other's connections to tell what is new.
	s.envMu.Lock()
	defer s.envMu.Unlock()

	s.mu.Lock()
	j := s.cur
	if j == nil {
		s.mu.Unlock()
		return Scan{}, &Problem{Code: "no_scan", Message: "There is no scan to take projects from.", Hint: "Start a scan first."}
	}
	if j.scan.State != "done" {
		s.mu.Unlock()
		return Scan{}, &Problem{Code: "scan_not_done", Message: "The scan has not finished, so its projects are not known yet.", Hint: "Wait for its state to be \"done\"."}
	}
	var todo []Project
	picked := map[string]bool{}
	for _, name := range names {
		found := false
		for _, p := range j.scan.Projects {
			if p.ID == name || p.Path == filepath.Clean(name) {
				found = true
				if !j.envRead[p.ID] && !picked[p.ID] {
					picked[p.ID] = true
					todo = append(todo, p)
				}
				break
			}
		}
		if !found {
			s.mu.Unlock()
			return Scan{}, &Problem{Code: "no_project", Message: "The latest scan has no project with that id or path: " + clip(name, 200) + ".",
				Hint: "Projects are named by the id or path the scan gave them."}
		}
	}
	s.mu.Unlock()

	// The reading, with no lock held.
	read := make([]projectEnv, len(todo))
	for i, p := range todo {
		if ctx.Err() != nil || j.ctx.Err() != nil {
			break
		}
		read[i] = s.fs.readEnv(p.Path)
	}
	var saved []SavedConnection
	if s.opts.Connections != nil {
		saved = s.opts.Connections.Saved()
	}
	savedBy := map[string]string{}
	for _, c := range saved {
		savedBy[savedKey(c.Kind, c.Host, c.Database, c.User)] = c.ID
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != j || j.ctx.Err() != nil {
		return Scan{}, &Problem{Code: "no_scan", Message: "The scan was replaced while its env files were being read.", Hint: "Read the latest scan and ask again."}
	}
	if j.secrets == nil {
		j.secrets, j.identities = map[string]*target{}, map[string]string{}
	}
	if j.envRead == nil {
		j.envRead = map[string]bool{}
	}
	// Everything below is built anew and swapped in: a scan handed out
	// earlier may be being read at this moment.
	sc := &j.scan
	projects := append([]Project{}, sc.Projects...)
	conns := append([]Connection{}, sc.Connections...)
	totals := sc.Env
	at := func(id string) int {
		for i := range conns {
			if conns[i].ID == id {
				return i
			}
		}
		return -1
	}
	for n, p := range todo {
		env := read[n]
		k := -1
		for i := range projects {
			if projects[i].ID == p.ID {
				k = i
			}
		}
		if k < 0 {
			continue
		}
		// The files the scan listed by name, now with their counts; one
		// that could not be opened stays as it was, unread.
		files := append([]EnvFile{}, projects[k].EnvFiles...)
		for _, f := range env.files {
			totals.FilesRead++
			totals.URLs += f.URLs
			totals.OtherKeys += f.OtherKeys
			listed := false
			for i := range files {
				if files[i].File == f.File {
					files[i], listed = f, true
				}
			}
			if !listed {
				files = append(files, f)
			}
		}
		ids := append([]string{}, projects[k].ConnectionIDs...)
		for _, u := range env.urls {
			where := FoundIn{ProjectID: p.ID, Project: p.Name, Key: u.key, File: u.file, Effective: u.effective}
			t := describeURL(u)
			// By what a URL connects to and as whom: the same one in two
			// files, or spelled two ways, is one connection.
			key := t.identity
			if key == "" || u.refs {
				// Not whole, or not readable: the same only as itself.
				key = "raw\x00" + u.kind + "\x00" + normalScheme(u.raw)
			}
			if id, ok := j.identities[key]; ok {
				if i := at(id); i >= 0 {
					// One connection, spelled another way here. The
					// spelling kept, to show and to save, is that of the
					// first place the dev tools would use: a value some
					// other file outranks is nobody's working URL, and it
					// may lack what the working one has (an ORM's
					// `?schema=`).
					if where.Effective && t.connectable && !anyEffective(conns[i].FoundIn) {
						conns[i].URL, conns[i].Host = t.masked, t.addr()
						j.secrets[id] = t
					}
					conns[i].FoundIn = append(append([]FoundIn{}, conns[i].FoundIn...), where)
					ids = addUnique(ids, id)
					continue
				}
			}
			c := Connection{ID: newID("u"), Kind: t.kind, URL: t.masked, Host: t.addr(), Database: t.database, User: t.user,
				Remote: t.remote, Connectable: t.connectable, Reason: t.reason, Note: t.note,
				CaptureAvailable: t.connectable && !t.remote, FoundIn: []FoundIn{where}}
			if t.connectable {
				if id, ok := savedBy[savedKey(t.kind, t.addr(), t.database, t.user)]; ok {
					c.Saved, c.SavedID = true, id
				}
			}
			c.Suggested = c.Connectable && !c.Remote && !c.Saved
			conns = append(conns, c)
			j.secrets[c.ID], j.identities[key] = t, c.ID
			ids = addUnique(ids, c.ID)
		}
		projects[k].EnvFiles, projects[k].ConnectionIDs = files, ids
		j.envRead[p.ID] = true
	}
	// What can be connected here first, then what is elsewhere, then
	// what can't be; in the order found within each.
	rank := func(c Connection) int {
		switch {
		case !c.Connectable:
			return 2
		case c.Remote:
			return 1
		}
		return 0
	}
	sort.SliceStable(conns, func(a, b int) bool { return rank(conns[a]) < rank(conns[b]) })

	// A URL that points at a port something listens on says what that
	// something is, and the other way round.
	listeners := append([]Listener{}, sc.Listeners...)
	for i := range conns {
		c := &conns[i]
		t := j.secrets[c.ID]
		if t == nil || t.remote || t.port == 0 || t.hostname == "" {
			continue
		}
		for k := range listeners {
			l := &listeners[k]
			if l.Port != t.port {
				continue
			}
			if l.Kind == "other" || l.Basis == "port" {
				l.Kind, l.What, l.Basis = c.Kind, kindNames[c.Kind], "env-url"
			}
			c.Listening = &Running{Port: l.Port, PID: l.PID, What: l.What, UptimeS: l.UptimeS}
			break
		}
	}
	sortListeners(listeners)

	done := make([]string, 0, len(j.envRead))
	for id := range j.envRead {
		done = append(done, id)
	}
	sort.Strings(done)
	sc.Listeners, sc.Projects, sc.Connections, sc.Env, sc.EnvProjects = listeners, projects, conns, totals, done

	// The URLs are kept for a while, to be saved by id, and then let go
	// whether or not anyone came for them. Reading more starts the wait
	// again.
	if j.expire != nil {
		j.expire.Stop()
	}
	j.expired = false
	sc.SecretsUntilMs = ms(s.now().Add(s.secretsFor))
	j.expire = time.AfterFunc(s.secretsFor, func() {
		s.mu.Lock()
		j.secrets, j.identities, j.expired = nil, nil, true
		s.mu.Unlock()
	})
	return *sc, nil
}

func anyEffective(found []FoundIn) bool {
	for _, f := range found {
		if f.Effective {
			return true
		}
	}
	return false
}

var kindNames = map[string]string{"postgres": "Postgres", "redis": "Redis", "mysql": "MySQL", "mongodb": "MongoDB"}

func savedKey(kind, host, database, user string) string {
	return kind + "\x00" + serverKey(host) + "\x00" + database + "\x00" + user
}

// projects is the set of projects a scan has found, by path.
type projects struct {
	list   []*Project
	byPath map[string]*Project
}

func newProjects() *projects { return &projects{byPath: map[string]*Project{}} }

// add detects the project at root, unless it is already known.
func (ps *projects) add(f files, root, boundary, source string) {
	if p, ok := ps.byPath[root]; ok {
		p.Sources = addUnique(p.Sources, source)
		return
	}
	p, ok := f.detect(root, boundary)
	if !ok {
		return
	}
	if at := f.gitRootAbove(root, boundary); at != "" {
		p.Git = f.gitAt(at)
	}
	ps.merge(p, source)
}

// merge adds a detected project, or notes another way a known one was
// found.
func (ps *projects) merge(p *Project, source string) {
	if have, ok := ps.byPath[p.Path]; ok {
		have.Sources = addUnique(have.Sources, source)
		return
	}
	p.Sources = addUnique(p.Sources, source)
	ps.byPath[p.Path] = p
	ps.list = append(ps.list, p)
}

// deepest is the project a directory is in: of those that contain it, the
// one furthest down.
func deepest(projects []*Project, dir string) *Project {
	var best *Project
	if dir == "" {
		return nil
	}
	for _, p := range projects {
		if within(p.Path, dir) && (best == nil || len(p.Path) > len(best.Path)) {
			best = p
		}
	}
	return best
}

// leadFramework is the framework a project's dev server is, by id.
func leadFramework(p *Project) string {
	if len(p.Frameworks) == 0 {
		return ""
	}
	return p.Frameworks[0].ID
}

const (
	maxSave = 50
	maxTest = 20
	// How many connections are tried at once.
	atOnce = 4
)

// lookup returns the found URLs for ids and where each was found, or the
// reason each can't be had.
func (s *Service) lookup(ids []string) (map[string]*target, map[string]Found, map[string]*Problem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.cur
	if j == nil {
		return nil, nil, nil, &Problem{Code: "no_scan", Message: "There is no scan to take connections from.", Hint: "Start a scan first."}
	}
	found := map[string]*target{}
	where := map[string]Found{}
	missing := map[string]*Problem{}
	for _, id := range ids {
		t, ok := j.secrets[id]
		switch {
		case ok && !t.connectable:
			missing[id] = &Problem{Code: "not_connectable", Message: t.note}
		case ok:
			found[id] = t
			where[id] = foundIn(&j.scan, id)
		case j.expired:
			missing[id] = &Problem{Code: "expired", Message: "Leadline no longer holds that URL: found URLs are kept for a while and then let go.", Hint: "Scan again."}
		default:
			missing[id] = &Problem{Code: "not_found", Message: "The latest scan found no connection with that id.", Hint: "Ids are only good for the scan they came from."}
		}
	}
	return found, where, missing, nil
}

// foundIn is where a scan's connection was found, for its owner: the
// place the dev tools would take it from, with that project's folder and
// branch.
func foundIn(sc *Scan, id string) Found {
	for _, c := range sc.Connections {
		if c.ID != id || len(c.FoundIn) == 0 {
			continue
		}
		at := c.FoundIn[0]
		for _, f := range c.FoundIn {
			if f.Effective {
				at = f
				break
			}
		}
		out := Found{Project: at.Project, Key: at.Key, File: at.File}
		for _, p := range sc.Projects {
			if p.ID == at.ProjectID {
				out.Path = p.Path
				if p.Git != nil {
					out.Branch = p.Git.Branch
				}
				break
			}
		}
		return out
	}
	return Found{}
}

// Save saves found connections through the integrations manager, each
// with capture on or off. One that is already saved is not added again:
// its capture is set to what was asked.
func (s *Service) Save(ctx context.Context, items []SaveItem) (SaveResult, error) {
	out := SaveResult{Saved: []Saved{}, Failed: []SaveFailed{}}
	if len(items) > maxSave {
		return out, &Problem{Code: "bad_request", Message: "At most " + strconv.Itoa(maxSave) + " connections can be saved at once."}
	}
	if s.opts.Connections == nil {
		return out, &Problem{Code: "failed", Message: "This daemon has nowhere to save connections."}
	}
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	found, where, missing, err := s.lookup(ids)
	if err != nil {
		return out, err
	}
	savedBy := map[string]string{}
	for _, c := range s.opts.Connections.Saved() {
		savedBy[savedKey(c.Kind, c.Host, c.Database, c.User)] = c.ID
	}

	// One at a time, in the order asked: saved connections are listed in
	// the order they were added, and that should be the order a person
	// chose them in. Each takes a moment when its server answers, and a
	// couple of seconds when it does not.
	seen := map[string]bool{}
	for _, it := range items {
		if p, bad := missing[it.ID]; bad {
			out.Failed = append(out.Failed, SaveFailed{ID: it.ID, Error: p})
			continue
		}
		// Two of the same id in one request are one connection.
		if seen[it.ID] {
			continue
		}
		seen[it.ID] = true
		t := found[it.ID]
		key := savedKey(t.kind, t.addr(), t.database, t.user)
		existing := savedBy[key]
		var view any
		var err error
		if existing != "" {
			view, err = s.opts.Connections.SetCapture(ctx, existing, it.Capture)
		} else {
			view, err = s.opts.Connections.Add(ctx, t.kind, t.connect, it.Name, it.Capture, where[it.ID])
		}
		if err != nil {
			out.Failed = append(out.Failed, SaveFailed{ID: it.ID, Error: asProblem(err)})
			continue
		}
		out.Saved = append(out.Saved, Saved{ID: it.ID, Already: existing != "", Connection: view})
		if existing == "" {
			// Two found URLs for one server (localhost and 127.0.0.1)
			// are one connection: the second finds the first's.
			for _, c := range s.opts.Connections.Saved() {
				savedBy[savedKey(c.Kind, c.Host, c.Database, c.User)] = c.ID
			}
		}
	}
	s.markSaved()
	return out, nil
}

// markSaved brings the scan's connections up to date with what is saved.
func (s *Service) markSaved() {
	saved := s.opts.Connections.Saved()
	savedBy := map[string]string{}
	for _, c := range saved {
		savedBy[savedKey(c.Kind, c.Host, c.Database, c.User)] = c.ID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.cur
	if j == nil || j.secrets == nil {
		return
	}
	conns := append([]Connection{}, j.scan.Connections...)
	for i := range conns {
		t := j.secrets[conns[i].ID]
		if t == nil || !t.connectable {
			continue
		}
		id, ok := savedBy[savedKey(t.kind, t.addr(), t.database, t.user)]
		conns[i].Saved, conns[i].SavedID = ok, id
		conns[i].Suggested = conns[i].Connectable && !conns[i].Remote && !ok
	}
	j.scan.Connections = conns
}

// Test tries found connections without saving them. A remote server is
// only tried when remote is true: connecting to someone's production
// database is not something to do because a file named it.
func (s *Service) Test(ctx context.Context, ids []string, remote bool) (TestResults, error) {
	out := TestResults{Results: []TestedConnection{}}
	if len(ids) > maxTest {
		return out, &Problem{Code: "bad_request", Message: "At most " + strconv.Itoa(maxTest) + " connections can be tried at once."}
	}
	if s.opts.Connections == nil {
		return out, &Problem{Code: "failed", Message: "This daemon can't try connections."}
	}
	found, _, missing, err := s.lookup(ids)
	if err != nil {
		return out, err
	}
	out.Results = make([]TestedConnection, len(ids))
	var wg sync.WaitGroup
	slots := make(chan struct{}, atOnce)
	for i, id := range ids {
		out.Results[i].ID = id
		if p, bad := missing[id]; bad {
			out.Results[i].Error = p
			continue
		}
		t := found[id]
		if t.remote && !remote {
			out.Results[i].Error = &Problem{Code: "remote", Message: "This server is on another machine, and Leadline connects to those only when asked.",
				Hint: "Ask again with \"remote\": true."}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			out.Results[i].Result = s.opts.Connections.Test(ctx, t.kind, t.connect)
		}()
	}
	wg.Wait()
	return out, nil
}

// asProblem is err as a Problem. An error that is not one is not quoted:
// what an unknown error says is not known to be safe to show.
func asProblem(err error) *Problem {
	var p *Problem
	if errors.As(err, &p) {
		return p
	}
	return &Problem{Code: "failed", Message: "Leadline could not do that."}
}

func newID(prefix string) string {
	var b [5]byte
	rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}

func ms(t time.Time) float64 { return float64(t.UnixNano()) / 1e6 }
