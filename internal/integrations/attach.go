package integrations

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sean-brydon/leadline/internal/store"
)

// Attaching is how a worktree becomes a source of its own. A tool that makes
// a checkout for each task (Shipyard, with a database for each) calls
// `leadline attach` when the worktree appears and `leadline detach` when it
// goes. Attach registers the worktree under one service name and saves the
// databases its environment names as connections that the worktree owns;
// detach takes away exactly those, and the data filed under that name.
//
// What a worktree owns is told from what a person added by the Owner on
// each connection, never by where a connection points: two worktrees, and a
// person, may each have a connection to the one Postgres.

// maxAttachedConnections is how many connections worktrees may own between
// them, apart from the 50 a person may add. A worktree has one or two.
const maxAttachedConnections = 200

// maxAttachURLs is how many URLs one attach may bring.
const maxAttachURLs = 16

// Attachment is a source `leadline attach` registered, as it is kept in the
// connections file.
type Attachment struct {
	// Service is the name everything of the worktree is filed under, and
	// what names the attachment: attaching it again is an update.
	Service   string `json:"service"`
	Owner     Owner  `json:"owner"`
	CreatedMs int64  `json:"created_ms"`
	UpdatedMs int64  `json:"updated_ms"`
}

// AttachedView is an attached source as it leaves the daemon.
type AttachedView struct {
	Service string `json:"service"`
	Owner   Owner  `json:"owner"`
	// ConnectionIDs are the connections it owns, in the order they were
	// added.
	ConnectionIDs []string `json:"connection_ids"`
	CreatedMs     int64    `json:"created_ms"`
	UpdatedMs     int64    `json:"updated_ms"`
}

// AttachInput is one `leadline attach`.
type AttachInput struct {
	// Service is the source's name. Required; `leadline attach` makes it
	// "<project>/<worktree>".
	Service string `json:"service"`
	// Via is the tool that asked: "shipyard" or "cli".
	Via      string `json:"via"`
	Project  string `json:"project"`
	Worktree string `json:"worktree"`
	Branch   string `json:"branch"`
	Box      string `json:"box"`
	Path     string `json:"path"`
	// Connections are the database and cache URLs of the worktree, all of
	// them: an owned connection whose URL is no longer here is removed.
	Connections []AttachURL `json:"connections"`
	// Partial says they may not be all of them (the caller found nowhere to
	// read them from): what is given is added or updated, and nothing the
	// worktree owns is removed.
	Partial bool `json:"partial"`
	// Capture false saves new connections for their stats only. Left out,
	// a new connection to a server on this machine is captured.
	Capture *bool `json:"capture"`
}

// AttachURL is one connection URL and the variables it was found in.
type AttachURL struct {
	Env []string `json:"env"`
	URL string   `json:"url"`
}

// AttachResult is what an attach did.
type AttachResult struct {
	Service string `json:"service"`
	// Created says the source is new; Changed that anything at all was
	// added, updated or removed. Attaching twice gives false for both.
	Created bool         `json:"created"`
	Changed bool         `json:"changed"`
	Source  AttachedView `json:"source"`
	// Connections are the ones the source owns now.
	Connections []ConnectionView `json:"connections"`
	Added       int              `json:"added"`
	Updated     int              `json:"updated"`
	Removed     int              `json:"removed"`
	Unchanged   int              `json:"unchanged"`
	// Skipped are the URLs that were not saved, and why. No URL is quoted.
	Skipped []AttachSkipped `json:"skipped"`
}

// AttachSkipped is a URL attach was given and did not save.
type AttachSkipped struct {
	Env    []string `json:"env"`
	Reason string   `json:"reason"`
}

// DetachResult is what a detach did.
type DetachResult struct {
	Service string `json:"service"`
	// Found says the source was attached. Detaching one that never was is
	// not an error: there was nothing to take away.
	Found bool `json:"found"`
	// Connections and Proxies count what was removed and closed. A proxy a
	// `leadline run` is still using stays until that run ends.
	Connections int `json:"connections"`
	Proxies     int `json:"proxies"`
	// KeptData says the recorded data was left in place, as asked.
	KeptData bool            `json:"kept_data"`
	Deleted  store.Forgotten `json:"deleted"`
}

func sourceName(raw string) (string, error) {
	name := strings.Join(strings.Fields(raw), " ")
	switch {
	case name == "":
		return "", problem("bad_request", "An attached source needs a service name.", `Send {"service": "acme/fix-orders", …}.`)
	case len(name) > maxName:
		return "", problem("bad_request", fmt.Sprintf("A source's name is at most %d characters.", maxName), "")
	case strings.Contains(name, SubSep):
		return "", problem("bad_request", `A source's name can't contain ":".`, `The colon is what joins a source's name to a command's, as in "acme/fix-orders:worker".`)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", problem("bad_request", "A source's name can't contain control characters.", "")
		}
	}
	return name, nil
}

// identity is what makes two URLs the same connection of a worktree: the
// server, the database and the user. A new password, or a new option, is
// the same connection with a new URL.
func identity(t *target) string {
	return t.kind + "|" + upstreamKey(t.addr()) + "|" + t.database + "|" + t.user
}

func (m *Manager) attachmentLocked(service string) *Attachment {
	for _, a := range m.attached {
		if a.Service == service {
			return a
		}
	}
	return nil
}

func (m *Manager) attachedViewLocked(a *Attachment) AttachedView {
	v := AttachedView{Service: a.Service, Owner: a.Owner.labelled(), ConnectionIDs: []string{},
		CreatedMs: a.CreatedMs, UpdatedMs: a.UpdatedMs}
	for _, e := range m.conns {
		if ownedBy(&e.c, a.Service) {
			v.ConnectionIDs = append(v.ConnectionIDs, e.c.ID)
		}
	}
	return v
}

// countLocked counts the connections worktrees own (owned), or the ones
// people added.
func (m *Manager) countLocked(owned bool) int {
	n := 0
	for _, e := range m.conns {
		if (e.c.Owner != nil && e.c.Owner.Kind == OwnerWorktree) == owned {
			n++
		}
	}
	return n
}

// Attached lists the attached sources, in the order they were attached.
func (m *Manager) Attached() []AttachedView {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]AttachedView, 0, len(m.attached))
	for _, a := range m.attached {
		out = append(out, m.attachedViewLocked(a))
	}
	return out
}

// AttachedSource returns one attached source and the connections it owns.
func (m *Manager) AttachedSource(service string) (AttachedView, []ConnectionView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.attachmentLocked(strings.Join(strings.Fields(service), " "))
	if a == nil {
		return AttachedView{}, nil, &Problem{Code: "not_found", Message: "No source of that name is attached.",
			Hint: `"leadline attach" in the worktree attaches it.`}
	}
	m.retryProxiesLocked()
	conns := []ConnectionView{}
	for _, e := range m.conns {
		if ownedBy(&e.c, a.Service) {
			conns = append(conns, m.viewLocked(e))
		}
	}
	return m.attachedViewLocked(a), conns, nil
}

// attachName is what a worktree's connection is called: the source and the
// database, so a list of them reads by itself.
func attachName(service string, t *target) string {
	what := "redis"
	if t.kind == KindPostgres {
		what = t.database
		if what == "" {
			what = "postgres"
		}
	} else if t.database != "" && t.database != "0" {
		what = "redis " + t.database
	}
	return clipText(service+" · "+what, maxName)
}

// Attach registers a worktree as a source and makes the connections it owns
// match the URLs given: new ones are added (captured, when the server is on
// this machine), ones whose URL changed are updated in place, and ones no
// longer named are removed. Attaching the same thing twice changes nothing.
//
// It does not wait on any server: a database that is still being created is
// still worth attaching, and whoever called is a hook that should be quick.
func (m *Manager) Attach(ctx context.Context, in AttachInput) (AttachResult, error) {
	service, err := sourceName(in.Service)
	if err != nil {
		return AttachResult{}, err
	}
	if len(in.Connections) > maxAttachURLs {
		return AttachResult{}, problem("bad_request", fmt.Sprintf("One attach brings at most %d connection URLs.", maxAttachURLs), "")
	}
	owner := Owner{Kind: OwnerWorktree, Via: cleanText(in.Via, 32), Service: service,
		Project: cleanText(in.Project, maxName), Worktree: cleanText(in.Worktree, maxName),
		Branch: cleanText(in.Branch, 200), Box: cleanText(in.Box, maxName), Path: cleanText(in.Path, 1024)}
	if owner.Via == "" {
		owner.Via = "cli"
	}
	out := AttachResult{Service: service, Connections: []ConnectionView{}, Skipped: []AttachSkipped{}}

	// What the worktree should own, one for each server, database and user.
	type wanted struct {
		t   *target
		env []string
	}
	var want []*wanted
	byIdentity := map[string]*wanted{}
	for _, c := range in.Connections {
		env := cleanEnv(c.Env)
		t, perr := parseURL("", c.URL)
		if perr != nil {
			out.Skipped = append(out.Skipped, AttachSkipped{Env: env, Reason: AsProblem(perr).Message})
			continue
		}
		if w, ok := byIdentity[identity(t)]; ok {
			w.env = mergeEnv(w.env, env)
			continue
		}
		w := &wanted{t: t, env: env}
		byIdentity[identity(t)] = w
		want = append(want, w)
	}

	now := time.Now().UnixMilli()
	var stale []*client
	var fresh []*entry

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return AttachResult{}, problem("shutting_down", "Leadline is shutting down.", "")
	}
	att := m.attachmentLocked(service)
	out.Created = att == nil
	if att != nil {
		// What this attach does not say stays as it was said before. Not
		// every hook knows everything: the event of a finished setup
		// carries no branch, and the one that made the worktree did.
		keep := func(now *string, was string) {
			if *now == "" {
				*now = was
			}
		}
		keep(&owner.Project, att.Owner.Project)
		keep(&owner.Worktree, att.Owner.Worktree)
		keep(&owner.Branch, att.Owner.Branch)
		keep(&owner.Box, att.Owner.Box)
		keep(&owner.Path, att.Owner.Path)
	}
	ownerChanged := att != nil && att.Owner != owner

	have := map[string]*entry{}
	for _, e := range m.conns {
		if ownedBy(&e.c, service) && !e.broken {
			have[identity(e.t)] = e
		}
	}
	kept := map[*entry]bool{}
	for _, w := range want {
		if e, ok := have[identity(w.t)]; ok {
			kept[e] = true
			changed := false
			if e.c.URL != w.t.raw {
				// The same database with a new password or option. The
				// proxy is in front of the server, not the URL, so it and
				// its port stay; Leadline's own connection is made again.
				e.c.URL, e.t = w.t.raw, w.t
				if e.cl != nil {
					stale = append(stale, e.cl)
					e.cl = nil
				}
				e.health, e.checkedAt, e.stats = Health{}, time.Time{}, nil
				if e.mon != nil {
					m.stopLocked(e)
					if perr := m.applyLocked(e, true); perr != nil {
						e.pxErr = AsProblem(perr).Message
					}
				}
				fresh = append(fresh, e)
				changed = true
			}
			if !sameEnv(e.c.Env, w.env) {
				e.c.Env, changed = w.env, true
			}
			if e.c.Owner == nil || *e.c.Owner != owner {
				o := owner
				e.c.Owner = &o
			}
			if changed {
				out.Updated++
			} else {
				out.Unchanged++
			}
			continue
		}
		if m.countLocked(true) >= maxAttachedConnections {
			out.Skipped = append(out.Skipped, AttachSkipped{Env: w.env,
				Reason: fmt.Sprintf("Leadline keeps at most %d connections for worktrees. Detach one that is gone.", maxAttachedConnections)})
			continue
		}
		o := owner
		c := Connection{ID: newID("c"), Kind: w.t.kind, Name: attachName(service, w.t), URL: w.t.raw,
			CreatedMs: now, Owner: &o, Env: w.env}
		c.Capture = (in.Capture == nil || *in.Capture) && !w.t.remote()
		e := newEntry(c, w.t, false)
		// A proxy that can't open is remembered and tried again, as at a
		// start: the connection is worth having either way.
		if perr := m.applyLocked(e, true); perr != nil {
			e.pxErr = AsProblem(perr).Message
		}
		m.conns = append(m.conns, e)
		kept[e] = true
		fresh = append(fresh, e)
		out.Added++
	}
	// After the new ones, so that a worktree whose database was renamed
	// keeps its proxy and its port: the new connection holds the proxy
	// before the old one lets go.
	for i := 0; i < len(m.conns) && !in.Partial; i++ {
		e := m.conns[i]
		if !ownedBy(&e.c, service) || kept[e] {
			continue
		}
		m.conns = append(m.conns[:i:i], m.conns[i+1:]...)
		i--
		m.stopLocked(e)
		if e.cl != nil {
			stale = append(stale, e.cl)
			e.cl = nil
		}
		out.Removed++
	}

	if att == nil {
		att = &Attachment{Service: service, Owner: owner, CreatedMs: now, UpdatedMs: now}
		m.attached = append(m.attached, att)
	}
	out.Changed = out.Created || ownerChanged || out.Added+out.Updated+out.Removed > 0
	var saveErr error
	if out.Changed {
		att.Owner, att.UpdatedMs = owner, now
		saveErr = m.saveLocked()
	}
	out.Source = m.attachedViewLocked(att)
	for _, e := range m.conns {
		if ownedBy(&e.c, service) {
			out.Connections = append(out.Connections, m.viewLocked(e))
		}
	}
	// The first health check of each new connection starts now and is not
	// waited for.
	for _, e := range fresh {
		m.checkLocked(e)
	}
	m.mu.Unlock()

	for _, cl := range stale {
		m.discard(cl)
	}
	if out.Changed {
		m.forgetCounts()
	}
	return out, saveErr
}

// Detach forgets an attached source: it stops and removes the connections
// the source owns and the proxies opened for them, and, unless keepData,
// deletes what was recorded under its name (and under the names of the
// commands run in it, "<service>:<name>"). Nothing else is touched: a
// connection a person added, or another worktree owns, stays as it is even
// when it is to the same server.
//
// Detaching a source that was never attached is not an error. Its data, if
// a `leadline run` filed any under the name, is deleted all the same: the
// worktree is gone.
func (m *Manager) Detach(ctx context.Context, service string, keepData bool) (DetachResult, error) {
	service, err := sourceName(service)
	if err != nil {
		return DetachResult{}, err
	}
	out := DetachResult{Service: service, KeptData: keepData}
	var stale []*client
	var monitors []*monitor

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return DetachResult{}, problem("shutting_down", "Leadline is shutting down.", "")
	}
	open := func() (n int) {
		for _, px := range m.proxies {
			if px.own == service {
				n++
			}
		}
		return n
	}
	before := open()
	for i := 0; i < len(m.conns); i++ {
		e := m.conns[i]
		if !ownedBy(&e.c, service) {
			continue
		}
		m.conns = append(m.conns[:i:i], m.conns[i+1:]...)
		i--
		if e.mon != nil {
			monitors = append(monitors, e.mon)
		}
		// Closing a proxy writes what it still held, here and now.
		m.stopLocked(e)
		if e.cl != nil {
			stale = append(stale, e.cl)
			e.cl = nil
		}
		out.Connections++
	}
	for i, a := range m.attached {
		if a.Service == service {
			m.attached = append(m.attached[:i:i], m.attached[i+1:]...)
			out.Found = true
			break
		}
	}
	out.Proxies = before - open()
	var saveErr error
	if out.Found || out.Connections > 0 {
		saveErr = m.saveLocked()
	}
	m.mu.Unlock()

	for _, cl := range stale {
		m.discard(cl)
	}
	// A live capture's last commands are written as it stops; they have to
	// be in the store before the store is asked to forget them.
	for _, mon := range monitors {
		mon.wait()
	}
	m.cmu.Lock()
	for name := range m.captured {
		if under(name, service) {
			delete(m.captured, name)
		}
	}
	for name := range m.through {
		if under(name, service) {
			delete(m.through, name)
		}
	}
	m.cmu.Unlock()

	if !keepData && m.store != nil {
		out.Deleted, err = m.store.Forget(ctx, service, service+SubSep)
		if err != nil {
			m.forgetCounts()
			return out, &Problem{Code: "failed", Message: "The source is detached, but its recorded data could not be deleted.", Hint: scrubbed(err)}
		}
	}
	m.forgetCounts()
	return out, saveErr
}

// forgetCounts makes the next Sources count the services afresh, so a
// source that was attached or detached shows, or stops showing, at once.
func (m *Manager) forgetCounts() {
	c := &m.services
	c.mu.Lock()
	c.known = false
	c.mu.Unlock()
}

func scrubbed(err error) string {
	if err == nil {
		return ""
	}
	return clipText(err.Error(), 300)
}

// cleanEnv keeps what can be the name of an environment variable, in order
// and once each.
func cleanEnv(names []string) []string {
	out := []string{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || len(name) > 128 || strings.ContainsAny(name, "= \t\r\n/:@") {
			continue
		}
		out = mergeEnv(out, []string{name})
	}
	return out
}

func mergeEnv(a, b []string) []string {
	for _, name := range b {
		found := false
		for _, x := range a {
			found = found || x == name
		}
		if !found {
			a = append(a, name)
		}
	}
	sort.Strings(a)
	return a
}

func sameEnv(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
