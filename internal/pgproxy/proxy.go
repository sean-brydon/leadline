// Package pgproxy is a Postgres proxy that records every statement passing
// through it as a span. An app pointed at the proxy's port instead of the
// database's shows up in Leadline with no instrumentation, whatever
// language it is written in.
//
// The proxy never holds traffic back to look at it. Each direction of a
// connection is a plain relay: read some bytes, write them on. A scanner
// reads the same bytes on the way past to find the message boundaries, and
// a tracker pairs what the client asked with what the server answered. If
// either meets something it doesn't understand, the tracker turns itself
// off and the relay carries on untouched.
package pgproxy

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Route says where one proxy listens and which Postgres it forwards to.
type Route struct {
	Listen   string // host:port on this machine
	Upstream string // host:port, or the path of a unix socket
	// Service is the service to file statements under. A connection that
	// names itself (application_name) goes by that name instead; with
	// neither, the database's name is all there is.
	Service string
}

// Routes is a list of routes that can be a repeated command-line flag.
type Routes []Route

func (r *Routes) String() string {
	parts := make([]string, len(*r))
	for i, route := range *r {
		parts[i] = route.Listen + "=" + route.Upstream
	}
	return strings.Join(parts, ",")
}

func (r *Routes) Set(s string) error {
	routes, err := ParseRoutes(s)
	if err != nil {
		return err
	}
	*r = append(*r, routes...)
	return nil
}

// ParseRoutes reads "listen=upstream" pairs separated by commas. Either
// side may be a bare port, which means that port on 127.0.0.1.
func ParseRoutes(s string) (Routes, error) {
	var routes Routes
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		listen, upstream, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("pg proxy %q: want listen=upstream, as in 127.0.0.1:5433=127.0.0.1:5432", pair)
		}
		listen, err := listenAddr(strings.TrimSpace(listen))
		if err != nil {
			return nil, fmt.Errorf("pg proxy %q: %w", pair, err)
		}
		upstream, err = upstreamAddr(strings.TrimSpace(upstream))
		if err != nil {
			return nil, fmt.Errorf("pg proxy %q: %w", pair, err)
		}
		routes = append(routes, Route{Listen: listen, Upstream: upstream})
	}
	return routes, nil
}

// RoutesOr returns the routes given as flags, or failing any, those in the
// environment variable's value.
func RoutesOr(flagged Routes, env string) (Routes, error) {
	if len(flagged) > 0 {
		return flagged, nil
	}
	return ParseRoutes(env)
}

// listenAddr insists on loopback. The proxy speaks plaintext, and a
// database that trusts local connections would trust anyone who reached
// the proxy from outside.
func listenAddr(s string) (string, error) {
	if _, err := strconv.ParseUint(s, 10, 16); err == nil {
		return "127.0.0.1:" + s, nil
	}
	host, _, err := net.SplitHostPort(s)
	if err != nil {
		return "", fmt.Errorf("listen address %q: %w", s, err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return "", fmt.Errorf("listen address %q is not on this machine only; use 127.0.0.1", s)
	}
	return s, nil
}

func upstreamAddr(s string) (string, error) {
	switch {
	case s == "":
		return "", errors.New("no upstream address")
	case strings.HasPrefix(s, "/"):
		return s, nil
	}
	if _, err := strconv.ParseUint(s, 10, 16); err == nil {
		return "127.0.0.1:" + s, nil
	}
	if _, _, err := net.SplitHostPort(s); err != nil {
		if strings.Contains(s, ":") {
			return "", fmt.Errorf("upstream address %q: %w", s, err)
		}
		return net.JoinHostPort(s, "5432"), nil
	}
	return s, nil
}

// Group is a daemon's proxies and the recorder they share.
type Group struct {
	proxies []*Proxy
	rec     *Recorder
}

// Start opens a proxy for each route and serves them until Close. A route
// whose port can't be had is logged and left out: the rest of the daemon
// is still useful without it.
func Start(routes []Route, sink Sink, notify func()) *Group {
	g := &Group{}
	if len(routes) == 0 {
		return g
	}
	g.rec = NewRecorder(sink, notify)
	for _, route := range routes {
		p, err := Listen(route, g.rec)
		if err != nil {
			slog.Error("pg proxy: can't listen; queries sent to this port will not arrive", "listen", route.Listen, "err", err)
			continue
		}
		g.proxies = append(g.proxies, p)
		go p.Serve()
		slog.Info("proxying postgres", "listen", p.Addr().String(), "upstream", route.Upstream)
	}
	return g
}

// Close stops the proxies, ends their connections and writes what was
// still waiting to be stored.
func (g *Group) Close() {
	for _, p := range g.proxies {
		p.Close()
	}
	if g.rec != nil {
		g.rec.Close()
	}
}

// Proxy is one listening port in front of one Postgres.
type Proxy struct {
	route Route
	ln    net.Listener
	rec   *Recorder

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup

	// fallbacks counts connections the proxy stopped reading and only
	// relayed.
	fallbacks atomic.Int64
	lastWarn  atomic.Int64

	// accepted counts client connections ever taken, open those still up,
	// and lastConn is when the newest arrived; see Stats.
	accepted atomic.Int64
	open     atomic.Int64
	lastConn atomic.Int64
}

// Listen takes the route's port. Call Serve to start accepting.
func Listen(route Route, rec *Recorder) (*Proxy, error) {
	ln, err := net.Listen("tcp", route.Listen)
	if err != nil {
		return nil, err
	}
	return &Proxy{route: route, ln: ln, rec: rec, conns: make(map[net.Conn]struct{})}, nil
}

func (p *Proxy) Addr() net.Addr { return p.ln.Addr() }

// Serve accepts connections until Close.
func (p *Proxy) Serve() error {
	var delay time.Duration
	for {
		client, err := p.ln.Accept()
		if err != nil {
			if p.isClosed() {
				return nil
			}
			// Out of file descriptors, most likely. Give it a moment
			// rather than spin.
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			time.Sleep(delay)
			continue
		}
		delay = 0
		if !p.hold(client) {
			client.Close()
			return nil
		}
		p.accepted.Add(1)
		p.open.Add(1)
		p.lastConn.Store(time.Now().UnixNano())
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer p.open.Add(-1)
			p.handle(client)
		}()
	}
}

// Close stops accepting, ends the open connections and waits for them.
func (p *Proxy) Close() error {
	p.mu.Lock()
	p.closed = true
	for c := range p.conns {
		c.Close()
	}
	p.mu.Unlock()
	err := p.ln.Close()
	p.wg.Wait()
	return err
}

func (p *Proxy) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// hold remembers a connection so Close can end it. It reports false when
// the proxy has already closed.
func (p *Proxy) hold(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

func (p *Proxy) release(c net.Conn) {
	c.Close()
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
}

func (p *Proxy) dial() (net.Conn, error) {
	network := "tcp"
	if strings.HasPrefix(p.route.Upstream, "/") {
		network = "unix"
	}
	return net.DialTimeout(network, p.route.Upstream, 5*time.Second)
}

// warn logs at most once every ten seconds: an app retrying a dead
// database would otherwise fill the log.
func (p *Proxy) warn(msg string, args ...any) {
	now := time.Now().Unix()
	if last := p.lastWarn.Load(); now-last < 10 || !p.lastWarn.CompareAndSwap(last, now) {
		return
	}
	slog.Warn(msg, args...)
}
