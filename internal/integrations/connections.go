package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

const (
	fileName       = "connections.json"
	fileVersion    = 1
	maxConnections = 50
	maxName        = 80
)

// Connection is a saved database or Redis, as it is kept in the file. It
// is never sent anywhere as it is: the URL has the password in it.
type Connection struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name"`
	URL  string `json:"url"`
	// Capture records what apps send: through a proxy for Postgres, with
	// MONITOR for Redis. Every connection can be read for stats whether
	// or not it is captured.
	Capture bool `json:"capture"`
	// ProxyPort is the port a captured Postgres is proxied on. It is kept
	// when capture is turned off, so turning it on again gives the port
	// the app's .env already has.
	ProxyPort int   `json:"proxy_port,omitempty"`
	CreatedMs int64 `json:"created_ms"`
	// Owner is who made it, when that was not a person typing its URL: the
	// worktree it was attached for, or the scan that found it. Absent for
	// a connection someone added by hand.
	Owner *Owner `json:"owner,omitempty"`
	// Env names the variables the URL was found in (DATABASE_URL), for a
	// connection attached from a worktree's environment.
	Env []string `json:"env,omitempty"`
}

// connectionsFile is the file as it is written. Sources came after
// connections: a file from before has none, and a daemon from before that
// rewrites the file drops them, which only means the worktrees have to be
// attached again.
type connectionsFile struct {
	Version     int          `json:"version"`
	Connections []Connection `json:"connections"`
	Sources     []Attachment `json:"sources,omitempty"`
}

// loadFile reads the connections file. No file is no connections. A file
// that can't be read as JSON is moved aside rather than overwritten by the
// next save: it may be the only copy of someone's URLs.
func loadFile(path string) (connectionsFile, error) {
	var f connectionsFile
	if path == "" {
		return f, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, fmt.Errorf("read %s: %w", path, err)
	}
	// It holds passwords. If something widened its permissions (a
	// restore, a copy), narrow them again.
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o600); err != nil {
			slog.Warn("integrations: the connections file is readable by others and could not be fixed", "path", path, "err", err)
		}
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		aside := path + ".unreadable"
		if rerr := os.Rename(path, aside); rerr != nil {
			return connectionsFile{}, fmt.Errorf("%s is not valid JSON and could not be moved aside: %w", path, rerr)
		}
		slog.Error("integrations: the connections file is not valid JSON; it was moved aside and Leadline starts with none", "moved_to", aside, "err", err)
		return connectionsFile{}, nil
	}
	return f, nil
}

// saveFile writes the connections file whole: to a temporary file that was
// never readable by anyone else, then renamed over the old one, so a crash
// leaves either the old file or the new and never half of one.
func saveFile(path string, conns []Connection, sources []Attachment) error {
	if path == "" {
		return nil
	}
	if conns == nil {
		conns = []Connection{}
	}
	raw, err := json.MarshalIndent(connectionsFile{Version: fileVersion, Connections: conns, Sources: sources}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, fileName+".*.tmp") // CreateTemp makes it 0600
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (m *Manager) saveLocked() error {
	conns := make([]Connection, len(m.conns))
	for i, e := range m.conns {
		conns[i] = e.c
	}
	sources := make([]Attachment, len(m.attached))
	for i, a := range m.attached {
		sources[i] = *a
	}
	if err := saveFile(m.path, conns, sources); err != nil {
		slog.Error("integrations: could not save connections", "err", err)
		return problem("not_saved", "Leadline could not write its connections file.", err.Error())
	}
	return nil
}

// entry is a saved connection and what is running for it.
type entry struct {
	c Connection
	t *target
	// broken: the saved URL could not be read. It is kept and shown, and
	// editing its URL mends it.
	broken bool

	cl *client
	// own is the address Redis sees Leadline's stats connection at, so a
	// live capture can leave Leadline's own commands out.
	own     *atomic.Pointer[string]
	px      *proxy
	pxErr   string // why the proxy could not start, if it couldn't
	pxTried time.Time
	mon     *monitor

	health    Health
	checking  chan struct{} // closed when the check under way ends
	checkedAt time.Time

	stats     any
	statsAt   time.Time
	statsCall *statsCall
}

func newEntry(c Connection, t *target, broken bool) *entry {
	return &entry{c: c, t: t, broken: broken, own: new(atomic.Pointer[string])}
}

// Health is whether Leadline itself can reach a connection.
type Health struct {
	// State is "unknown" until the first check, then "ok" or "error".
	State   string `json:"state"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Hint    string `json:"hint,omitempty"`
	Version string `json:"version,omitempty"`
	// VersionShort is the version's number alone: "17.11" of
	// "17.11 (Debian 17.11-1.pgdg13+2)".
	VersionShort string  `json:"version_short,omitempty"`
	LatencyMs    float64 `json:"latency_ms,omitempty"`
	CheckedMs    float64 `json:"checked_ms,omitempty"`
}

// shortVersion is a server's version without the build it came from: the
// first word of what the server says.
func shortVersion(v string) string {
	if fields := strings.Fields(v); len(fields) > 0 {
		return strings.TrimRight(fields[0], ",;")
	}
	return ""
}

// ConnectionView is a connection as it leaves the daemon: URL masked.
type ConnectionView struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name"`
	// URL has Mask where the password is.
	URL      string `json:"url"`
	Host     string `json:"host"`
	Database string `json:"database"`
	User     string `json:"user"`
	// Remote: the server is on another machine. Leadline reads its stats
	// and nothing more.
	Remote  bool `json:"remote"`
	Capture bool `json:"capture"`
	// CaptureOffer says whether capture can be turned on here, and what
	// it costs or why not.
	CaptureOffer CaptureOffer `json:"capture_offer"`
	Proxy        *ProxyView   `json:"proxy,omitempty"`
	Monitor      *MonitorView `json:"monitor,omitempty"`
	Health       Health       `json:"health"`
	CreatedMs    int64        `json:"created_ms"`
	// Owner is who made it: a person ("user"), the worktree it was
	// attached for ("worktree"), or a scan that found it ("scan").
	Owner Owner `json:"owner"`
	// Env names the variables its URL was found in, for a connection
	// attached from a worktree's environment; empty for any other.
	Env []string `json:"env"`
}

// CaptureOffer is whether a server's traffic can be captured, and how.
type CaptureOffer struct {
	Available bool `json:"available"`
	// How is "proxy" (Postgres) or "monitor" (Redis).
	How  string `json:"how"`
	Note string `json:"note"`
}

const (
	proxyNote = "Leadline opens a port on this machine that forwards to the database and records every statement on the way. " +
		"Point the app at the proxy's URL instead of the database's and its queries appear with no other change."
	proxyRemoteNote = "A remote database can't be captured. Leadline's proxy can read queries only when they are not encrypted, so it doesn't speak TLS; " +
		"putting it in front of a database on another machine would send the app's queries and results over the network in the clear. " +
		"Leadline still reads this database's own statistics."
	monitorRemoteNote = "A remote Redis can't be captured. Live capture uses MONITOR, which makes the server send every command it runs and can cost a busy server up to half its throughput. " +
		"Leadline only does that to a Redis on this machine. It still reads this Redis's own statistics."
)

func captureOffer(t *target) CaptureOffer {
	switch {
	case t.kind == KindPostgres && t.remote():
		return CaptureOffer{How: "proxy", Note: proxyRemoteNote}
	case t.kind == KindPostgres:
		return CaptureOffer{Available: true, How: "proxy", Note: proxyNote}
	case t.remote():
		return CaptureOffer{How: "monitor", Note: monitorRemoteNote}
	}
	return CaptureOffer{Available: true, How: "monitor", Note: MonitorNote}
}

func (m *Manager) viewLocked(e *entry) ConnectionView {
	v := ConnectionView{ID: e.c.ID, Kind: e.c.Kind, Name: e.c.Name, Capture: e.c.Capture,
		Health: e.health, CreatedMs: e.c.CreatedMs,
		Owner: ownerOf(&e.c).labelled(), Env: append([]string{}, e.c.Env...)}
	if v.Health.State == "" {
		v.Health.State = "unknown"
	}
	v.Health.VersionShort = shortVersion(v.Health.Version)
	if e.broken {
		v.Health = Health{State: "error", Code: "bad_url", Message: "The saved URL can't be read.", Hint: "Edit this connection and enter the URL again."}
		return v
	}
	t := e.t
	v.URL, v.Host, v.Database, v.User, v.Remote = t.masked(), t.addr(), t.database, t.user, t.remote()
	v.CaptureOffer = captureOffer(t)
	if !e.c.Capture {
		return v
	}
	switch {
	case e.px != nil:
		pv := m.proxyViewLocked(e.px)
		pv.URL = t.proxyURL(e.px.port)
		v.Proxy = &pv
	case e.c.Kind == KindPostgres:
		// Capture is on but the proxy isn't up: say so where the URL
		// would be.
		v.Proxy = &ProxyView{Kind: KindPostgres, Upstream: t.addr(), Port: e.c.ProxyPort, State: "error", Error: e.pxErr,
			ConnectionIDs: []string{e.c.ID}}
		if e.c.ProxyPort != 0 {
			v.Proxy.Listen = listenOn(e.c.ProxyPort)
			v.Proxy.URL = t.proxyURL(e.c.ProxyPort)
		}
	case e.mon != nil:
		v.Monitor = e.mon.view(m.since)
	}
	return v
}

// Connections lists the saved connections, in the order they were added.
func (m *Manager) Connections() []ConnectionView {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retryProxiesLocked()
	m.refreshHealthLocked()
	out := make([]ConnectionView, len(m.conns))
	for i, e := range m.conns {
		out[i] = m.viewLocked(e)
	}
	return out
}

func (m *Manager) findLocked(id string) *entry {
	for _, e := range m.conns {
		if e.c.ID == id {
			return e
		}
	}
	return nil
}

// Connection returns one connection.
func (m *Manager) Connection(id string) (ConnectionView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.findLocked(id)
	if e == nil {
		return ConnectionView{}, notFound("connection")
	}
	return m.viewLocked(e), nil
}

// ConnectionInput is what adding or editing a connection takes. A nil
// field is left as it is (or, when adding, at its default).
type ConnectionInput struct {
	Kind    string  `json:"kind"`
	Name    *string `json:"name"`
	URL     *string `json:"url"`
	Capture *bool   `json:"capture"`
	// ProxyPort picks the proxy's port; 0 lets Leadline choose again.
	ProxyPort *int `json:"proxy_port"`
	// Owner says where a new connection came from, when that was not a
	// person typing its URL: a scan (ScanOwner). It is not read from a
	// request's body, so nothing outside the daemon can claim one, and
	// it is only looked at when adding.
	Owner *Owner `json:"-"`
}

func cleanName(name string, t *target) string {
	name = strings.Join(strings.Fields(name), " ")
	if len(name) > maxName {
		name = clipText(name, maxName)
	}
	if name != "" {
		return name
	}
	// A name of its own: the database for Postgres, as the proxy would
	// call an app that gives no name; "redis" for Redis.
	switch {
	case t.kind == KindPostgres && t.database != "":
		return t.database
	case t.kind == KindPostgres:
		return "postgres"
	case t.port != 6379:
		return fmt.Sprintf("redis-%d", t.port)
	}
	return "redis"
}

func checkPort(p int) error {
	if p < 0 || p > 65535 || (p > 0 && p < 1024) {
		return problem("bad_request", "The proxy port must be between 1024 and 65535.", "Leave it out and Leadline picks a free one.")
	}
	return nil
}

func refuseCapture(t *target) error {
	if offer := captureOffer(t); !offer.Available {
		return problem("capture_remote", offer.Note, "Add it with capture off to read its stats.")
	}
	return nil
}

// Add saves a new connection and starts its capture if it asks for one.
// It does not insist the server is reachable: a database that is down
// today is still worth having in the list. The view it returns has been
// health-checked if the server answered within a couple of seconds.
func (m *Manager) Add(ctx context.Context, in ConnectionInput) (ConnectionView, error) {
	if in.URL == nil {
		return ConnectionView{}, errURL("A connection needs a URL.")
	}
	t, err := parseURL(in.Kind, *in.URL)
	if err != nil {
		return ConnectionView{}, err
	}
	c := Connection{ID: newID("c"), Kind: t.kind, URL: t.raw, CreatedMs: time.Now().UnixMilli()}
	if in.Name != nil {
		c.Name = *in.Name
	}
	c.Name = cleanName(c.Name, t)
	// Only a scan's: a worktree's connections are made by Attach, which
	// is what knows how to take them away again.
	if in.Owner != nil && in.Owner.Kind == OwnerScan {
		o := in.Owner.stored()
		c.Owner = &o
	}
	if in.Capture != nil && *in.Capture {
		if err := refuseCapture(t); err != nil {
			return ConnectionView{}, err
		}
		c.Capture = true
	}
	if in.ProxyPort != nil {
		if err := checkPort(*in.ProxyPort); err != nil {
			return ConnectionView{}, err
		}
		c.ProxyPort = *in.ProxyPort
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ConnectionView{}, problem("shutting_down", "Leadline is shutting down.", "")
	}
	if m.countLocked(false) >= maxConnections {
		m.mu.Unlock()
		return ConnectionView{}, problem("too_many", fmt.Sprintf("Leadline keeps at most %d connections.", maxConnections), "Delete one you no longer use.")
	}
	e := newEntry(c, t, false)
	if err := m.applyLocked(e, false); err != nil {
		m.mu.Unlock()
		return ConnectionView{}, err
	}
	m.conns = append(m.conns, e)
	if err := m.saveLocked(); err != nil {
		m.conns = m.conns[:len(m.conns)-1]
		m.stopLocked(e)
		m.mu.Unlock()
		return ConnectionView{}, err
	}
	m.mu.Unlock()

	m.check(e, addWait)
	return m.Connection(c.ID)
}

// Update changes a saved connection. A URL whose password is Mask keeps
// the saved password, so the app can send back what it was shown.
func (m *Manager) Update(ctx context.Context, id string, in ConnectionInput) (ConnectionView, error) {
	if in.ProxyPort != nil {
		if err := checkPort(*in.ProxyPort); err != nil {
			return ConnectionView{}, err
		}
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ConnectionView{}, problem("shutting_down", "Leadline is shutting down.", "")
	}
	e := m.findLocked(id)
	if e == nil {
		m.mu.Unlock()
		return ConnectionView{}, notFound("connection")
	}
	next, t := e.c, e.t
	urlChanged := false
	if in.URL != nil {
		raw := strings.TrimSpace(*in.URL)
		if !e.broken {
			raw = withSavedPassword(raw, e.t)
		}
		nt, err := parseURL(e.c.Kind, raw)
		if err != nil {
			m.mu.Unlock()
			return ConnectionView{}, err
		}
		urlChanged = nt.raw != e.c.URL
		next.URL, t = nt.raw, nt
	}
	if in.Name != nil {
		next.Name = cleanName(*in.Name, t)
	}
	if in.Capture != nil {
		next.Capture = *in.Capture
	}
	if in.ProxyPort != nil {
		next.ProxyPort = *in.ProxyPort
	}
	if next.Capture && !(e.broken && in.URL == nil) {
		if err := refuseCapture(t); err != nil {
			m.mu.Unlock()
			return ConnectionView{}, err
		}
	}

	// A proxy carries the app's open connections, so it is only restarted
	// when what it does changes: not for a new name or a new password.
	restart := e.broken || next.Capture != e.c.Capture || next.ProxyPort != e.c.ProxyPort
	if next.Capture && !restart {
		switch e.c.Kind {
		case KindPostgres:
			restart = e.px == nil || t.addr() != e.t.addr()
		case KindRedis:
			restart = urlChanged || next.Name != e.c.Name
		}
	}
	// Before anything is stopped: a port that can't be had is found out
	// while the old proxy is still carrying the app.
	if restart && next.Capture && e.c.Kind == KindPostgres && next.ProxyPort != 0 && (e.px == nil || e.px.port != next.ProxyPort) {
		if err := m.portUsableLocked(next.ProxyPort, upstreamKey(t.addr())); err != nil {
			m.mu.Unlock()
			return ConnectionView{}, err
		}
	}
	old := *e
	if restart {
		m.stopLocked(e)
	}
	e.c, e.t = next, t
	if in.URL != nil {
		e.broken = false
	}
	var err error
	if restart {
		err = m.applyLocked(e, false)
	}
	if err == nil {
		err = m.saveLocked()
	}
	if err != nil {
		// Back to how it was, running what ran before.
		if restart {
			m.stopLocked(e)
		}
		e.c, e.t, e.broken = old.c, old.t, old.broken
		if restart {
			if rerr := m.applyLocked(e, true); rerr != nil {
				slog.Warn("integrations: could not restore capture after a failed edit", "connection", e.c.Name, "err", rerr)
			}
		}
		m.mu.Unlock()
		return ConnectionView{}, err
	}
	var stale *client
	if urlChanged {
		stale, e.cl = e.cl, nil
		e.health, e.checkedAt = Health{}, time.Time{}
		e.stats = nil
	}
	m.mu.Unlock()

	m.discard(stale)
	if urlChanged {
		m.check(e, addWait)
	}
	return m.Connection(id)
}

// Delete forgets a connection and stops what ran for it.
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	e := m.findLocked(id)
	if e == nil {
		m.mu.Unlock()
		return notFound("connection")
	}
	for i, x := range m.conns {
		if x == e {
			m.conns = append(m.conns[:i:i], m.conns[i+1:]...)
			break
		}
	}
	err := m.saveLocked()
	m.stopLocked(e)
	cl := e.cl
	e.cl = nil
	m.mu.Unlock()
	m.discard(cl)
	return err
}

// discard closes a client nobody will use again, without making anyone
// wait: it may be in the middle of a connection attempt that has seconds
// left to time out.
func (m *Manager) discard(cl *client) {
	if cl == nil {
		return
	}
	m.bg.Add(1)
	go func() {
		defer m.bg.Done()
		cl.close()
	}()
}

// applyLocked starts what the entry's settings ask for. With keep, a proxy
// whose port can't be had is remembered as failed and tried again later
// (the daemon is starting; the port is the one in the app's .env);
// without, it is an error for whoever asked.
func (m *Manager) applyLocked(e *entry, keep bool) error {
	if e.broken || !e.c.Capture {
		return nil
	}
	if e.t.remote() {
		// Saved before it was remote, or edited by hand in the file.
		e.pxErr = "This database is not on this machine, so it is not proxied."
		return nil
	}
	switch e.c.Kind {
	case KindPostgres:
		px, port, err := m.proxyForLocked(e)
		if port != 0 {
			e.c.ProxyPort = port
		}
		if err != nil {
			if keep {
				e.pxErr = AsProblem(err).Message
				return nil
			}
			return err
		}
		px.holders[e.c.ID] = struct{}{}
		e.px, e.pxErr = px, ""
	case KindRedis:
		own := e.own
		// A person's Redis is recorded whole, under the connection's name.
		// A worktree's is recorded under the worktree's, and only the
		// database its URL names: the server is often one every worktree
		// shares, with a database number each.
		service, onlyDB := e.c.Name, ""
		if o := e.c.Owner; o != nil && o.Kind == OwnerWorktree {
			service, onlyDB = o.Service, e.t.database
		}
		e.mon = m.startMonitorOf(e.t, e.c.ID, service, onlyDB, func(addr string) bool {
			seen := own.Load()
			return seen != nil && *seen == addr
		})
	}
	return nil
}

// stopLocked stops what is running for an entry.
func (m *Manager) stopLocked(e *entry) {
	if e.px != nil {
		delete(e.px.holders, e.c.ID)
		m.dropIfUnusedLocked(e.px)
		e.px = nil
	}
	e.pxErr = ""
	if e.mon != nil {
		mon := e.mon
		e.mon = nil
		mon.halt()
		// Its last spans are still to be written, which has no need of
		// the lock.
		m.bg.Add(1)
		go func() {
			defer m.bg.Done()
			mon.wait()
		}()
	}
}
