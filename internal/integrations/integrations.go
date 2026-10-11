// Package integrations is what a person connects to Leadline: databases
// and Redis by URL, the proxies and live captures that record what apps
// send them, and one answer to "what is feeding Leadline right now".
//
// Connections are kept in a JSON file beside the database, not in it: the
// file holds passwords, so it is the user's alone (mode 0600), and nothing
// that reads the database can read them. No URL leaves this package with
// its password; see Mask.
//
// The HTTP API over this package is described in API.md.
package integrations

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sean-brydon/leadline/internal/store"
)

// Options say where a Manager keeps things and where captures go.
type Options struct {
	// Dir is the data directory. Empty keeps connections in memory only.
	Dir string
	// Store takes the spans captures record and answers Sources. Nil is
	// for tests of the parts that need neither.
	Store *store.Store
	// Notify is called after captured spans are stored, to wake the app.
	// SetNotify sets it later, for a caller that has to exist first.
	Notify func()
	// ProxyPorts is the range proxy ports are picked from. The zero value
	// is the usual one, 15432 to 15531. Tests give each manager its own,
	// so that two test binaries running at once never want the same port.
	ProxyPorts PortRange
}

// PortRange is a range of TCP ports, both ends included.
type PortRange struct{ First, Last int }

// Manager holds the connections and everything running on their behalf.
type Manager struct {
	path   string // the connections file; "" keeps nothing
	store  *store.Store
	db     *sql.DB
	notify atomic.Pointer[func()]

	// mu guards the tables below. Nothing slow happens under it: no
	// connecting, no queries.
	mu      sync.Mutex
	conns   []*entry
	proxies []*proxy
	// attached are the sources `leadline attach` registered: one for each
	// worktree, with the connections it owns among conns.
	attached []*Attachment
	otlp     []string
	closed   bool
	// since is where the windowed counts of the views being built start;
	// zero is the last 15 minutes. Sources sets it for the length of a call.
	since time.Time

	// What captures have recorded since the daemon started, by service:
	// how much from each kind of capture, and through which proxy or
	// connection (by id).
	cmu      sync.Mutex
	captured map[string]map[string]int64
	through  map[string]map[string]string // service -> capture id -> source

	services servicesCache
	// ctx ends when the manager closes, and with it whatever Leadline was
	// in the middle of asking a server.
	ctx    context.Context
	cancel context.CancelFunc
	bg     sync.WaitGroup

	ports PortRange

	// Set before use, for tests.
	monitorMax  int
	healthEvery func(remote bool) time.Duration
}

// Open loads the connections in dir and starts what they ask for: a proxy
// for each captured Postgres, a live capture for each captured Redis. The
// daemon opens one and hands it to the API and the MCP server.
func Open(opts Options) (*Manager, error) { return newManager(opts) }

func newManager(opts Options) (*Manager, error) {
	m := &Manager{
		store:      opts.Store,
		captured:   map[string]map[string]int64{},
		through:    map[string]map[string]string{},
		ports:      opts.ProxyPorts,
		monitorMax: MonitorMaxPerSec,
		healthEvery: func(remote bool) time.Duration {
			if remote {
				return 30 * time.Second
			}
			return 10 * time.Second
		},
	}
	m.SetNotify(opts.Notify)
	m.ctx, m.cancel = context.WithCancel(context.Background())
	if m.ports.First == 0 {
		m.ports = PortRange{proxyPortFirst, proxyPortLast}
	}
	m.ports.Last = max(m.ports.Last, m.ports.First)
	if opts.Store != nil {
		m.db = opts.Store.DB()
	}
	if opts.Dir != "" {
		m.path = filepath.Join(opts.Dir, fileName)
	}
	saved, err := loadFile(m.path)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range saved.Sources {
		a := saved.Sources[i]
		if a.Service == "" {
			continue
		}
		a.Owner = a.Owner.stored()
		m.attached = append(m.attached, &a)
	}
	for _, c := range saved.Connections {
		t, err := parseURL(c.Kind, c.URL)
		if err != nil {
			// Kept, and shown as broken, rather than dropped: the next
			// save would otherwise lose what the person typed.
			slog.Warn("integrations: a saved connection's URL can't be read", "id", c.ID, "name", c.Name)
			t = &target{kind: c.Kind, raw: c.URL}
		}
		e := newEntry(c, t, err != nil)
		m.conns = append(m.conns, e)
		if perr := m.applyLocked(e, true); perr != nil {
			slog.Warn("integrations: could not start capture", "connection", c.Name, "err", perr)
		}
	}
	return m, nil
}

// SetOTLP records where the daemon takes OTLP, so Sources can tell a person
// where to point their app.
func (m *Manager) SetOTLP(endpoints ...string) {
	m.mu.Lock()
	m.otlp = append([]string(nil), endpoints...)
	m.mu.Unlock()
}

// Close stops every proxy and capture, writing what they still held, and
// closes the connections Leadline made itself. The connections file stays.
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	proxies, conns := m.proxies, m.conns
	m.proxies = nil
	m.mu.Unlock()
	m.cancel()

	for _, px := range proxies {
		px.close()
	}
	for _, e := range conns {
		if e.mon != nil {
			e.mon.halt()
			e.mon.wait()
		}
		if e.cl != nil {
			e.cl.close()
		}
	}
	m.bg.Wait()
}

// record stores what a capture saw and remembers which services it was
// for, so Sources can tell a captured service from one sending OTLP.
func (m *Manager) record(source string, spans []store.Span) error {
	if len(spans) == 0 {
		return nil
	}
	m.cmu.Lock()
	for i := range spans {
		by := m.captured[spans[i].Service]
		if by == nil {
			by = map[string]int64{}
			m.captured[spans[i].Service] = by
		}
		by[source]++
		if id := spans[i].Capture; id != "" {
			ids := m.through[spans[i].Service]
			if ids == nil {
				ids = map[string]string{}
				m.through[spans[i].Service] = ids
			}
			ids[id] = source
		}
	}
	m.cmu.Unlock()
	if m.store == nil {
		return nil
	}
	return m.store.AddSpans(spans)
}

// SetNotify sets what is called after captured spans are stored. The API
// server gives its own Notify, so the app refetches.
func (m *Manager) SetNotify(fn func()) {
	if fn == nil {
		m.notify.Store(nil)
		return
	}
	m.notify.Store(&fn)
}

func (m *Manager) wake() {
	if fn := m.notify.Load(); fn != nil {
		(*fn)()
	}
}

// capturing tells the store that a capture of a database system started or
// stopped, so it knows to pair what the capture records with the app's own
// spans for the same calls.
func (m *Manager) capturing(system string, on bool) {
	if m.store != nil {
		m.store.Capturing(system, on)
	}
}

func newID(prefix string) string {
	var b [5]byte
	rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}
