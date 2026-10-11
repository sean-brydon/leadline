package integrations

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

const (
	// A connection Leadline made for itself is closed after this long
	// unused. Nobody looking at the app means nothing held open on the
	// database.
	clientIdle = 60 * time.Second
	// How long adding a connection waits for its first health check.
	addWait = 2500 * time.Millisecond
	// Stats are kept this long, so a screen that polls asks the server
	// once, not once per poll.
	statsFreshLocal  = 2 * time.Second
	statsFreshRemote = 10 * time.Second
	testTimeout      = 12 * time.Second
	statsTimeout     = 25 * time.Second
	healthTimeout    = 8 * time.Second
)

// client is Leadline's own connection to a saved server: one at a time,
// opened when first needed and closed when idle.
type client struct {
	t   *target
	own *atomic.Pointer[string]

	mu    sync.Mutex
	pg    *pgconn.PgConn
	rd    *respConn
	idle  *time.Timer
	used  time.Time
	ended bool
}

func newClient(t *target, own *atomic.Pointer[string], identify bool) *client {
	c := &client{t: t, own: own}
	if !identify {
		c.own = nil
	}
	return c
}

func (c *client) touchLocked() {
	c.used = time.Now()
	if c.idle == nil {
		c.idle = time.AfterFunc(clientIdle, c.expire)
		return
	}
	c.idle.Reset(clientIdle)
}

func (c *client) expire() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.used) >= clientIdle {
		c.dropLocked()
	}
}

func (c *client) dropLocked() {
	if c.pg != nil {
		// Close says goodbye first; give that a moment and no more.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		c.pg.Close(ctx)
		cancel()
		c.pg = nil
	}
	if c.rd != nil {
		c.rd.close()
		c.rd = nil
		if c.own != nil {
			c.own.Store(nil)
		}
	}
}

func (c *client) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ended = true
	if c.idle != nil {
		c.idle.Stop()
	}
	c.dropLocked()
}

var errClientClosed = errors.New("the connection was removed")

// postgres runs fn on the connection, opening it if need be. A connection
// that was left idle may have died quietly (the database restarted), so a
// failure on a reused one is tried once more on a new one.
func (c *client) postgres(ctx context.Context, fn func(*pgconn.PgConn) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended {
		return errClientClosed
	}
	defer c.touchLocked()
	for attempt := 0; ; attempt++ {
		reused := c.pg != nil && !c.pg.IsClosed()
		if !reused {
			c.dropLocked()
			conn, err := connectPostgres(ctx, c.t)
			if err != nil {
				return err
			}
			c.pg = conn
		}
		err := fn(c.pg)
		if err == nil {
			return nil
		}
		if c.pg.IsClosed() {
			c.dropLocked()
			if reused && attempt == 0 && ctx.Err() == nil {
				continue
			}
		}
		return err
	}
}

func (c *client) redis(ctx context.Context, fn func(*respConn) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended {
		return errClientClosed
	}
	defer c.touchLocked()
	for attempt := 0; ; attempt++ {
		reused := c.rd != nil
		if !reused {
			conn, err := dialRedis(ctx, c.t)
			if err != nil {
				return err
			}
			if c.own != nil {
				conn.identify()
				if conn.seenAs != "" {
					c.own.Store(&conn.seenAs)
				}
			}
			c.rd = conn
		}
		err := fn(c.rd)
		if err == nil {
			return nil
		}
		// An error reply leaves the connection good; anything else and
		// it can't be trusted to be at a reply boundary.
		var re respError
		if errors.As(err, &re) {
			return err
		}
		c.dropLocked()
		if reused && attempt == 0 && ctx.Err() == nil {
			continue
		}
		return err
	}
}

// busy reports whether the client is in use right now, which is itself a
// sign of life.
func (c *client) busy() bool {
	if !c.mu.TryLock() {
		return true
	}
	c.mu.Unlock()
	return false
}

// clientLocked is the entry's client, made on first use.
func (m *Manager) clientLocked(e *entry) *client {
	if e.cl == nil {
		// A local Redis may be captured, and the capture must be able
		// to leave this connection's own commands out.
		e.cl = newClient(e.t, e.own, e.c.Kind == KindRedis && !e.t.remote())
	}
	return e.cl
}

// ping is the cheapest question that proves the server answers.
func (c *client) ping(ctx context.Context) (version string, err error) {
	if c.t.kind == KindPostgres {
		err = c.postgres(ctx, func(conn *pgconn.PgConn) error {
			if _, err := pgRead(ctx, conn, "SELECT 1"); err != nil {
				return err
			}
			version = conn.ParameterStatus("server_version")
			return nil
		})
		return version, err
	}
	err = c.redis(ctx, func(conn *respConn) error {
		v, err := conn.do("INFO", "server")
		if err != nil {
			return err
		}
		version = firstOf(parseInfo(asString(v)), "redis_version", "valkey_version", "dragonfly_version")
		return nil
	})
	return version, err
}

// check health-checks an entry in the background, and waits up to wait for
// the answer.
func (m *Manager) check(e *entry, wait time.Duration) {
	m.mu.Lock()
	done := m.checkLocked(e)
	m.mu.Unlock()
	if done == nil || wait <= 0 {
		return
	}
	select {
	case <-done:
	case <-time.After(wait):
	}
}

// checkLocked starts a health check unless one is under way, and returns
// the channel that closes when it ends.
func (m *Manager) checkLocked(e *entry) chan struct{} {
	if m.closed || e.broken {
		return nil
	}
	if e.checking != nil {
		return e.checking
	}
	done := make(chan struct{})
	e.checking = done
	cl, t := m.clientLocked(e), e.t
	m.bg.Add(1)
	go func() {
		defer m.bg.Done()
		h := Health{State: "ok"}
		if !cl.busy() {
			ctx, cancel := context.WithTimeout(m.ctx, healthTimeout)
			began := time.Now()
			version, err := cl.ping(ctx)
			cancel()
			h.Version, h.LatencyMs = version, round1(float64(time.Since(began).Microseconds())/1000)
			if err != nil {
				p := explain(err, t)
				h = Health{State: "error", Code: p.Code, Message: p.Message, Hint: p.Hint}
			}
		}
		h.CheckedMs = float64(time.Now().UnixMilli())

		m.mu.Lock()
		// The entry may have been edited to point somewhere else while
		// this ran; then the answer is about a server it no longer has.
		if e.t == t {
			if h.Version == "" && h.State == "ok" {
				h.Version, h.LatencyMs = e.health.Version, e.health.LatencyMs
			}
			e.health, e.checkedAt = h, time.Now()
		}
		e.checking = nil
		m.mu.Unlock()
		close(done)
	}()
	return done
}

// refreshHealthLocked re-checks connections whose last check is old. It is
// called when someone looks, so a daemon nobody is looking at leaves the
// databases alone.
func (m *Manager) refreshHealthLocked() {
	now := time.Now()
	for _, e := range m.conns {
		if e.broken || e.checking != nil {
			continue
		}
		if e.checkedAt.IsZero() || now.Sub(e.checkedAt) >= m.healthEvery(e.t.remote()) {
			m.checkLocked(e)
		}
	}
}

// TestInput is a connection to try. Give a URL to try one before saving
// it, an ID to try a saved one, or both to try a saved one with changes
// (a URL whose password is Mask uses the saved password).
type TestInput struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	URL  string `json:"url"`
}

// TestResult is what trying a connection found.
type TestResult struct {
	OK       bool   `json:"ok"`
	Kind     string `json:"kind,omitempty"`
	URL      string `json:"url,omitempty"`
	Host     string `json:"host,omitempty"`
	Database string `json:"database,omitempty"`
	User     string `json:"user,omitempty"`
	// Remote: the server is on another machine.
	Remote  bool   `json:"remote"`
	Version string `json:"version,omitempty"`
	// VersionShort is the version's number alone, as in "17.11".
	VersionShort string  `json:"version_short,omitempty"`
	LatencyMs    float64 `json:"latency_ms,omitempty"`
	TLS          bool    `json:"tls"`
	// Capture says whether this server's traffic could be captured.
	Capture *CaptureOffer `json:"capture,omitempty"`
	// Statements is whether pg_stat_statements is there (Postgres only).
	Statements *PgStatStatements `json:"pg_stat_statements,omitempty"`
	Error      *Problem          `json:"error,omitempty"`
}

// Test connects, asks the server who it is, and disconnects. It never
// changes what is saved.
func (m *Manager) Test(ctx context.Context, in TestInput) TestResult {
	raw, kind := in.URL, in.Kind
	if in.ID != "" {
		m.mu.Lock()
		e := m.findLocked(in.ID)
		var saved *target
		if e != nil && !e.broken {
			saved = e.t
		}
		m.mu.Unlock()
		switch {
		case e == nil:
			return TestResult{Error: notFound("connection")}
		case raw == "" && saved == nil:
			return TestResult{Kind: e.c.Kind, Error: AsProblem(errURL("The saved URL can't be read."))}
		case raw == "":
			raw = saved.raw
		default:
			raw = withSavedPassword(raw, saved)
		}
		kind = e.c.Kind
	}
	t, err := parseURL(kind, raw)
	if err != nil {
		return TestResult{Kind: kind, Error: AsProblem(err)}
	}
	offer := captureOffer(t)
	out := TestResult{Kind: t.kind, URL: t.masked(), Host: t.addr(), Database: t.database, User: t.user,
		Remote: t.remote(), Capture: &offer}
	ctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	fail := func(err error) TestResult {
		out.Error = explain(err, t)
		return out
	}

	began := time.Now()
	if t.kind == KindRedis {
		conn, err := dialRedis(ctx, t)
		if err != nil {
			return fail(err)
		}
		defer conn.close()
		out.LatencyMs = round1(float64(time.Since(began).Microseconds()) / 1000)
		v, err := conn.do("INFO", "server")
		if err != nil {
			return fail(err)
		}
		out.OK, out.TLS = true, t.tls
		out.Version = firstOf(parseInfo(asString(v)), "redis_version", "valkey_version", "dragonfly_version")
		out.VersionShort = shortVersion(out.Version)
		return out
	}

	conn, err := connectPostgres(ctx, t)
	if err != nil {
		return fail(err)
	}
	defer func() {
		bye, cancel := context.WithTimeout(context.Background(), time.Second)
		conn.Close(bye)
		cancel()
	}()
	out.LatencyMs = round1(float64(time.Since(began).Microseconds()) / 1000)
	res, err := pgRead(ctx, conn, "SELECT current_database(), current_user")
	if err != nil {
		return fail(err)
	}
	if len(res) == 1 && len(res[0]) == 1 {
		out.Database, out.User = res[0][0].s(0), res[0][0].s(1)
	}
	out.OK, out.TLS = true, pgUsesTLS(conn)
	out.Version = conn.ParameterStatus("server_version")
	out.VersionShort = shortVersion(out.Version)
	st := pgStatStatements(ctx, conn, t, false)
	out.Statements = &st
	return out
}

// Stats reads what a saved connection's server knows about itself: a
// *PostgresStats or a *RedisStats. With fresh false, an answer a moment
// old is returned as it is.
func (m *Manager) Stats(ctx context.Context, id string, fresh bool) (any, error) {
	m.mu.Lock()
	e := m.findLocked(id)
	if e == nil {
		m.mu.Unlock()
		return nil, notFound("connection")
	}
	if e.broken {
		m.mu.Unlock()
		return nil, errURL("The saved URL can't be read.")
	}
	keep := statsFreshLocal
	if e.t.remote() {
		keep = statsFreshRemote
	}
	if !fresh && e.stats != nil && time.Since(e.statsAt) < keep {
		out := e.stats
		m.mu.Unlock()
		return out, nil
	}
	// One read at a time per connection: a screen polling a slow server
	// waits for the read under way instead of queueing another behind it.
	if call := e.statsCall; call != nil {
		m.mu.Unlock()
		select {
		case <-call.done:
			return call.out, call.err
		case <-ctx.Done():
			return nil, problem("timeout", "The request ended before the stats were read.", "")
		}
	}
	call := &statsCall{done: make(chan struct{})}
	e.statsCall = call
	cl, t := m.clientLocked(e), e.t
	m.mu.Unlock()

	// Not the request's context: others may be waiting on this read, and
	// one of them going away must not end it for the rest.
	ctx, cancel := context.WithTimeout(m.ctx, statsTimeout)
	defer cancel()
	var (
		out     any
		version string
		began   = time.Now()
		err     error
	)
	if t.kind == KindPostgres {
		err = cl.postgres(ctx, func(conn *pgconn.PgConn) error {
			st, err := postgresStats(ctx, conn, t)
			if err == nil {
				out, version = st, st.Version
			}
			return err
		})
	} else {
		err = cl.redis(ctx, func(conn *respConn) error {
			st, err := redisStats(ctx, conn, t)
			if err == nil {
				out, version = st, st.Version
			}
			return err
		})
	}

	// Reading the stats was a health check too, and a truer one.
	h := Health{State: "ok", Version: version, CheckedMs: float64(time.Now().UnixMilli())}
	if err != nil {
		p := explain(err, t)
		h = Health{State: "error", Code: p.Code, Message: p.Message, Hint: p.Hint, CheckedMs: h.CheckedMs}
		out, call.err = nil, p
	}
	call.out = out
	m.mu.Lock()
	if e.t == t {
		if err == nil {
			h.LatencyMs = e.health.LatencyMs
			e.stats, e.statsAt = out, began
		}
		e.health, e.checkedAt = h, time.Now()
	}
	e.statsCall = nil
	m.mu.Unlock()
	close(call.done)
	return call.out, call.err
}

// statsCall is a read of one connection's stats that is under way.
type statsCall struct {
	done chan struct{}
	out  any
	err  error
}
