package integrations

import (
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sean-brydon/leadline/internal/pgproxy"
	"github.com/sean-brydon/leadline/internal/store"
)

// The names captures go by in Sources.
const (
	sourceOTLP          = "otlp"
	sourcePostgresProxy = "postgres-proxy"
	sourceRedisMonitor  = "redis-monitor"
)

// Proxy ports Leadline picks itself come from here: easy to recognise
// (1 + Postgres's 5432) and outside the range systems hand out for
// outgoing connections, so the same port is still free after a restart.
const (
	proxyPortFirst = 15432
	proxyPortLast  = 15531

	proxyRetryEvery = 5 * time.Second
)

// proxy is one listening port in front of one Postgres, and who wants it
// there. It closes when nobody does.
type proxy struct {
	id       string
	kind     string
	listen   string
	port     int
	upstream string
	key      string // upstream, spelled one way
	label    string
	// service is who the proxy files statements under, for a connection
	// that doesn't name itself: the run it was opened for. Empty leaves
	// that to the database's name.
	service string
	// own is the attached source the proxy was opened for, when it was:
	// the worktree's service name. Everything through such a proxy is that
	// worktree's and is filed under it, whatever a connection calls itself,
	// and only that worktree's connections share it. Set before the proxy
	// starts and never changed.
	own string
	m   *Manager

	// Who holds it open: the -pg flag, `leadline run` (counted), saved
	// connections (by id).
	declared bool
	runs     int
	holders  map[string]struct{}

	// p is nil while the port can't be had; err says why.
	p     *pgproxy.Proxy
	rec   *pgproxy.Recorder
	err   string
	tried time.Time

	statements, failed atomic.Int64
	lastNs             atomic.Int64
	recent             tally // statements, minute by minute
}

func (px *proxy) unused() bool { return !px.declared && px.runs == 0 && len(px.holders) == 0 }

func (px *proxy) close() {
	if px.p != nil {
		px.p.Close()
		if px.m != nil {
			px.m.capturing(systemPostgres, false)
		}
	}
	if px.rec != nil {
		px.rec.Close()
	}
}

// What the store calls the systems captures record.
const (
	systemPostgres = "postgresql"
	systemRedis    = "redis"
)

// proxySink counts what a proxy records on its way to the store.
type proxySink struct {
	m  *Manager
	px *proxy
}

func (s proxySink) AddSpans(spans []store.Span) error {
	var statements, failed int64
	for i := range spans {
		// A transaction's own span has no db_system: it frames the
		// statements, it isn't one.
		if spans[i].DBSystem == "" {
			continue
		}
		statements++
		if spans[i].Status == "error" {
			failed++
		}
	}
	for i := range spans {
		spans[i].Capture = s.px.id
		if s.px.own != "" {
			spans[i].Service = s.px.own
		}
	}
	s.px.recent.add(time.Now(), statements)
	s.px.statements.Add(statements)
	s.px.failed.Add(failed)
	s.px.lastNs.Store(time.Now().UnixNano())
	return s.m.record(sourcePostgresProxy, spans)
}

// ProxyView is a proxy as the app sees it.
type ProxyView struct {
	ID       string `json:"id,omitempty"`
	Kind     string `json:"kind"`
	Listen   string `json:"listen,omitempty"`
	Port     int    `json:"port,omitempty"`
	Upstream string `json:"upstream"`
	Label    string `json:"label,omitempty"`
	// Origins says who asked for it: "connection" (a saved connection
	// with capture on), "run" (`leadline run`), "flag" (the daemon's -pg).
	Origins       []string `json:"origins"`
	ConnectionIDs []string `json:"connection_ids"`
	// State is "listening", or "error" with Error saying why.
	State string `json:"state"`
	Error string `json:"error,omitempty"`
	// URL is only set inside a connection: what to put in the app, with
	// the password masked.
	URL string `json:"url,omitempty"`
	// Owner is who it is for: the worktree it was attached for
	// ("worktree"), a `leadline run` ("run"), the -pg flag ("flag"), or a
	// person's saved connections ("user"; "scan" when a scan found every
	// one of them in the same place).
	Owner Owner `json:"owner"`

	ConnectionsOpen  int64 `json:"connections_open"`
	ConnectionsTotal int64 `json:"connections_total"`
	Statements       int64 `json:"statements"`
	FailedStatements int64 `json:"failed_statements"`
	// UnreadConnections were relayed without being understood (a
	// replication stream, say): their statements are not recorded.
	UnreadConnections int64 `json:"unread_connections"`
	// LastActivityMs is when a connection last arrived or a statement was
	// last recorded; 0 when nothing has come through.
	LastActivityMs float64 `json:"last_activity_ms"`
	// WindowStatements is the statements recorded since the time Sources
	// was asked about (its since_ms; the last 15 minutes elsewhere), to
	// the minute. Statements counts from when the proxy opened.
	WindowStatements int64 `json:"window_statements"`
}

func (px *proxy) view(since time.Time) ProxyView {
	v := ProxyView{ID: px.id, Kind: px.kind, Listen: px.listen, Port: px.port, Upstream: px.upstream,
		Label: px.label, Origins: []string{}, ConnectionIDs: []string{}, State: "listening", Error: px.err}
	for id := range px.holders {
		v.ConnectionIDs = append(v.ConnectionIDs, id)
	}
	sort.Strings(v.ConnectionIDs)
	if len(px.holders) > 0 {
		v.Origins = append(v.Origins, "connection")
	}
	if px.runs > 0 {
		v.Origins = append(v.Origins, "run")
	}
	if px.declared {
		v.Origins = append(v.Origins, "flag")
	}
	switch {
	case px.own != "":
		v.Owner = Owner{Kind: OwnerWorktree, Service: px.own}
	case len(px.holders) > 0:
		v.Owner = userOwner
	case px.runs > 0:
		v.Owner = Owner{Kind: OwnerRun, Service: px.service}
	case px.declared:
		v.Owner = Owner{Kind: OwnerFlag}
	default:
		v.Owner = userOwner
	}
	v.Owner = v.Owner.labelled()
	if px.p == nil {
		v.State = "error"
		return v
	}
	st := px.p.Stats()
	v.ConnectionsOpen, v.ConnectionsTotal, v.UnreadConnections = st.Open, st.Accepted, st.Unread
	v.Statements, v.FailedStatements = px.statements.Load(), px.failed.Load()
	v.WindowStatements = px.recent.since(time.Now(), since)
	if last := max(st.LastConnNs, px.lastNs.Load()); last > 0 {
		v.LastActivityMs = float64(last) / 1e6
	}
	return v
}

func listenOn(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }

// answering reports whether something already accepts connections at addr.
// Listening alone doesn't tell: on macOS a loopback listener is allowed
// beside another program's listener on every address, and would then take
// that program's traffic.
func answering(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func portFree(port int) bool {
	addr := listenOn(port)
	if answering(addr) {
		return false
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// pickPortLocked finds a port for a new proxy, at or after from: free now,
// and not one a saved connection is keeping for later.
func (m *Manager) pickPortLocked(from int) (int, error) {
	taken := map[int]bool{}
	for _, e := range m.conns {
		taken[e.c.ProxyPort] = true
	}
	for _, px := range m.proxies {
		taken[px.port] = true
	}
	for port := max(from, m.ports.First); port <= m.ports.Last; port++ {
		if !taken[port] && portFree(port) {
			return port, nil
		}
	}
	return 0, problem("no_port", fmt.Sprintf("No port between %d and %d is free for a proxy.", m.ports.First, m.ports.Last),
		"Give the connection a port of your own choosing.")
}

// startProxyLocked opens a proxy and adds it to the table. service is who
// to file its statements under ("" for the database's name). owned says the
// service is an attached source's, and the proxy that source's alone.
func (m *Manager) startProxyLocked(listen, upstream, service string, owned bool) (*proxy, error) {
	_, portText, _ := net.SplitHostPort(listen)
	port, _ := strconv.Atoi(portText)
	inUse := func(why string) *Problem {
		return problem("port_in_use", fmt.Sprintf("Port %d can't be used for the proxy: %s.", port, why),
			"Pick another port, or leave the port out and Leadline picks one.")
	}
	if port != 0 && answering(listen) {
		return nil, inUse("another program is listening on it")
	}
	px := &proxy{id: newID("p"), kind: KindPostgres, upstream: upstream, key: upstreamKey(upstream), service: service,
		m: m, holders: map[string]struct{}{}}
	if owned {
		px.own, px.label = service, service
	}
	px.rec = pgproxy.NewRecorder(proxySink{m: m, px: px}, m.wake)
	p, err := pgproxy.Listen(pgproxy.Route{Listen: listen, Upstream: upstream, Service: service}, px.rec)
	if err != nil {
		px.rec.Close()
		return nil, inUse(lastClause(err))
	}
	px.p = p
	m.capturing(systemPostgres, true)
	px.listen = p.Addr().String()
	if tcp, ok := p.Addr().(*net.TCPAddr); ok {
		px.port = tcp.Port
	}
	go p.Serve()
	m.proxies = append(m.proxies, px)
	slog.Info("proxying postgres", "listen", px.listen, "upstream", upstream)
	return px, nil
}

func (m *Manager) dropIfUnusedLocked(px *proxy) bool {
	if !px.unused() {
		return false
	}
	for i, x := range m.proxies {
		if x == px {
			m.proxies = append(m.proxies[:i:i], m.proxies[i+1:]...)
			break
		}
	}
	// Under the lock, so the port is free again before anyone asks for
	// it. Closing ends the proxy's connections and writes what it held;
	// neither waits on anything that wants this lock.
	px.close()
	if px.p != nil {
		slog.Info("closed postgres proxy", "listen", px.listen, "upstream", px.upstream)
	}
	return true
}

// proxyForLocked finds or opens the proxy a captured Postgres connection
// should have. It returns the port even when the proxy can't open, so the
// port is remembered and tried again.
func (m *Manager) proxyForLocked(e *entry) (*proxy, int, error) {
	upstream := e.t.addr()
	key := upstreamKey(upstream)
	port := e.c.ProxyPort
	// A worktree's connection gets that worktree's proxy and no other: one
	// port for each worktree and server, which files under the worktree's
	// name. Two worktrees with a database each on one Postgres are two
	// proxies, and taking one away leaves the other as it was.
	own := ""
	if o := e.c.Owner; o != nil && o.Kind == OwnerWorktree {
		own = o.Service
	}
	if port == 0 {
		// Two connections to one server (its dev and test databases,
		// say) share a proxy: the database is named by the client.
		for _, px := range m.proxies {
			if px.key == key && px.p != nil && len(px.holders) > 0 && px.service == own && px.own == own {
				return px, px.port, nil
			}
		}
		// A port can be free when it is picked and gone when it is
		// listened on (another program, another daemon's tests), so try
		// the next.
		for from := m.ports.First; ; {
			port, err := m.pickPortLocked(from)
			if err != nil {
				return nil, 0, err
			}
			px, err := m.startProxyLocked(listenOn(port), upstream, own, own != "")
			if err == nil {
				return px, port, nil
			}
			from = port + 1
		}
	}
	for _, px := range m.proxies {
		if px.port != port || px.p == nil {
			continue
		}
		if px.key == key && px.own == own {
			return px, port, nil
		}
		if px.key == key {
			return nil, port, problem("port_in_use", fmt.Sprintf("Port %d is already Leadline's proxy for %s, for another source.", port, px.upstream),
				"Pick another port, or leave the port out and Leadline picks one.")
		}
		return nil, port, problem("port_in_use", fmt.Sprintf("Port %d is already Leadline's proxy for %s.", port, px.upstream),
			"Pick another port, or leave the port out and Leadline picks one.")
	}
	px, err := m.startProxyLocked(listenOn(port), upstream, own, own != "")
	return px, port, err
}

// portUsableLocked reports why a proxy for the upstream could not have
// port, if it couldn't: another of Leadline's proxies is on it for a
// different server, or another program is.
func (m *Manager) portUsableLocked(port int, key string) error {
	hint := "Pick another port, or leave the port out and Leadline picks one."
	for _, px := range m.proxies {
		if px.port != port || px.p == nil {
			continue
		}
		if px.key == key {
			return nil
		}
		return problem("port_in_use", fmt.Sprintf("Port %d is already Leadline's proxy for %s.", port, px.upstream), hint)
	}
	if answering(listenOn(port)) {
		return problem("port_in_use", fmt.Sprintf("Port %d can't be used for the proxy: another program is listening on it.", port), hint)
	}
	return nil
}

// retryProxiesLocked tries again for proxies whose port was busy, now and
// then: after a restart the old daemon may still have been letting go.
func (m *Manager) retryProxiesLocked() {
	if m.closed {
		return
	}
	now := time.Now()
	for _, e := range m.conns {
		if e.c.Kind != KindPostgres || !e.c.Capture || e.px != nil || e.broken || e.t.remote() || now.Sub(e.pxTried) < proxyRetryEvery {
			continue
		}
		e.pxTried = now
		m.applyLocked(e, true)
	}
	for _, px := range m.proxies {
		if px.p != nil || now.Sub(px.tried) < proxyRetryEvery {
			continue
		}
		px.tried = now
		m.listenDeclaredLocked(px)
	}
}

// Declare opens the proxies the daemon was started with (-pg). They are
// not saved: the flag is where they live. One that can't listen is shown
// as failed rather than stopping the daemon.
func (m *Manager) Declare(routes []pgproxy.Route) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range routes {
		joined := false
		for _, px := range m.proxies {
			if px.p != nil && px.listen == r.Listen && px.key == upstreamKey(r.Upstream) {
				px.declared, joined = true, true
			}
		}
		if joined {
			continue
		}
		px := &proxy{id: newID("p"), kind: KindPostgres, listen: r.Listen, upstream: r.Upstream, key: upstreamKey(r.Upstream),
			label: "-pg", declared: true, m: m, holders: map[string]struct{}{}, tried: time.Now()}
		if _, portText, err := net.SplitHostPort(r.Listen); err == nil {
			px.port, _ = strconv.Atoi(portText)
		}
		m.proxies = append(m.proxies, px)
		m.listenDeclaredLocked(px)
	}
}

func (m *Manager) listenDeclaredLocked(px *proxy) {
	if answering(px.listen) {
		px.err = fmt.Sprintf("Port %d is in use by another program.", px.port)
		slog.Error("pg proxy: can't listen; queries sent to this port will not arrive", "listen", px.listen, "err", px.err)
		return
	}
	rec := pgproxy.NewRecorder(proxySink{m: m, px: px}, m.wake)
	p, err := pgproxy.Listen(pgproxy.Route{Listen: px.listen, Upstream: px.upstream}, rec)
	if err != nil {
		rec.Close()
		px.err = sentence(lastClause(err))
		slog.Error("pg proxy: can't listen; queries sent to this port will not arrive", "listen", px.listen, "err", err)
		return
	}
	px.p, px.rec, px.err = p, rec, ""
	m.capturing(systemPostgres, true)
	px.listen = p.Addr().String()
	if tcp, ok := p.Addr().(*net.TCPAddr); ok {
		px.port = tcp.Port
	}
	go p.Serve()
	slog.Info("proxying postgres", "listen", px.listen, "upstream", px.upstream)
}

// RunProxy is the answer to `leadline run` asking for a proxy.
type RunProxy struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Listen   string `json:"listen"`
	Upstream string `json:"upstream"`
}

// OpenProxy gives a caller a proxy in front of upstream for as long as it
// runs: `leadline run` asks for one, points the app at it, and gives it
// back with CloseProxy. Nothing is saved.
//
// The label is the run's service name, and the proxy files what it records
// under it (a connection that sets application_name goes by that instead).
// So a proxy is shared only by runs of the same name: asking again for the
// same upstream under the same label returns the one there is, and another
// label, or none, gets a proxy of its own. With no label the statements go
// under the database's name, and an upstream that has any proxy, a saved
// connection's included, is given that one.
func (m *Manager) OpenProxy(kind, upstream, label string) (RunProxy, error) {
	if kind != KindPostgres {
		return RunProxy{}, problem("bad_request", "Leadline can only proxy Postgres.", `Send {"kind":"postgres","upstream":"127.0.0.1:5432"}.`)
	}
	upstream = strings.TrimSpace(upstream)
	if strings.ContainsAny(upstream, " ,=@") {
		return RunProxy{}, problem("bad_request", "The upstream is a host and port, as in 127.0.0.1:5432, not a URL.", "")
	}
	upstream, err := pgproxy.Upstream(upstream)
	if err != nil {
		return RunProxy{}, problem("bad_request", "The upstream is a host and port, as in 127.0.0.1:5432.", "")
	}
	host := upstream
	if !strings.HasPrefix(upstream, "/") {
		host, _, _ = net.SplitHostPort(upstream)
	}
	if !localHost(host) {
		return RunProxy{}, problem("capture_remote", proxyRemoteNote, "Run the app against the database directly.")
	}
	key := upstreamKey(upstream)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return RunProxy{}, problem("shutting_down", "Leadline is shutting down.", "")
	}
	label = clipText(strings.Join(strings.Fields(label), " "), maxName)
	for _, px := range m.proxies {
		if px.key == key && px.p != nil && px.service == label {
			px.runs++
			return RunProxy{ID: px.id, Kind: px.kind, Listen: px.listen, Upstream: px.upstream}, nil
		}
	}
	// A port from the system, since nothing has to remember it. It is
	// found first and listened on second so that the proxy knows its own
	// address when it has to name itself in an error to the app; should
	// something take the port in between, another is found.
	var px *proxy
	for attempt := 0; ; attempt++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return RunProxy{}, problem("no_port", "This machine has no free port for a proxy.", "")
		}
		listen := ln.Addr().String()
		ln.Close()
		if px, err = m.startProxyLocked(listen, upstream, label, false); err == nil {
			break
		}
		if attempt == 4 {
			return RunProxy{}, err
		}
	}
	px.runs, px.label = 1, label
	return RunProxy{ID: px.id, Kind: px.kind, Listen: px.listen, Upstream: px.upstream}, nil
}

// CloseProxy gives back a proxy from OpenProxy. The proxy closes when the
// last of its users has: two `leadline run`s on one database share a
// proxy, and the first to exit must not cut the other off. closed reports
// whether this call closed it.
func (m *Manager) CloseProxy(id string) (closed bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, px := range m.proxies {
		if px.id != id {
			continue
		}
		if px.runs > 0 {
			px.runs--
		}
		return m.dropIfUnusedLocked(px), nil
	}
	return false, notFound("proxy")
}

// Proxies lists every proxy, oldest first.
func (m *Manager) Proxies() []ProxyView {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retryProxiesLocked()
	return m.proxyViewsLocked()
}

func (m *Manager) proxyViewsLocked() []ProxyView {
	out := make([]ProxyView, 0, len(m.proxies))
	for _, px := range m.proxies {
		out = append(out, m.proxyViewLocked(px))
	}
	return out
}

// windowDefault is how far back the windowed counts look when nobody says.
const windowDefault = 15 * time.Minute

// tally counts things minute by minute over the last day, so "how many in
// the last quarter of an hour" can be answered without asking the store.
type tally struct {
	mu     sync.Mutex
	newest int64 // the minute the last count went into
	counts [24 * 60]int32
}

func (t *tally) add(now time.Time, n int64) {
	if n <= 0 {
		return
	}
	minute := now.Unix() / 60
	t.mu.Lock()
	t.roll(minute)
	t.counts[minute%int64(len(t.counts))] += int32(min(n, 1<<30))
	t.mu.Unlock()
}

// roll clears the minutes that have gone by since the last count.
func (t *tally) roll(minute int64) {
	if minute <= t.newest {
		return
	}
	if minute-t.newest >= int64(len(t.counts)) {
		clear(t.counts[:])
	} else {
		for m := t.newest + 1; m <= minute; m++ {
			t.counts[m%int64(len(t.counts))] = 0
		}
	}
	t.newest = minute
}

// since adds up the minutes from since's to now's. A since of zero means
// the last 15 minutes; further back than a day is a day.
func (t *tally) since(now, since time.Time) int64 {
	if since.IsZero() {
		since = now.Add(-windowDefault)
	}
	minute, from := now.Unix()/60, since.Unix()/60
	from = max(from, minute-int64(len(t.counts))+1)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.roll(minute)
	var n int64
	for m := from; m <= minute; m++ {
		n += int64(t.counts[m%int64(len(t.counts))])
	}
	return n
}

// proxyViewLocked is the proxy's view with a label a person recognises:
// the one it was opened with, or the names of the connections it is for.
func (m *Manager) proxyViewLocked(px *proxy) ProxyView {
	v := px.view(m.since)
	if px.own != "" {
		// A worktree's proxy says all that is known of the worktree.
		if a := m.attachmentLocked(px.own); a != nil {
			v.Owner = a.Owner.labelled()
		}
	}
	if v.Owner.Kind == OwnerUser && len(px.holders) > 0 {
		// A proxy opened for saved connections belongs to whoever made
		// them. When that is one scan's find, it says so, as the
		// connection does; when they came different ways it is "a
		// person's", which is true of all of them.
		var one *Owner
		same := true
		for _, e := range m.conns {
			if _, ok := px.holders[e.c.ID]; !ok {
				continue
			}
			o := ownerOf(&e.c)
			if one == nil {
				one = &o
			} else if *one != o {
				same = false
			}
		}
		if one != nil && same && one.Kind == OwnerScan {
			v.Owner = one.labelled()
		}
	}
	if v.Label == "" {
		var names []string
		for _, e := range m.conns {
			if _, ok := px.holders[e.c.ID]; ok {
				names = append(names, e.c.Name)
			}
		}
		v.Label = strings.Join(names, ", ")
	}
	return v
}
