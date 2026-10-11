package integrations

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sean-brydon/leadline/internal/store"
)

// Sources is everything feeding Leadline right now: who sends telemetry,
// which proxies are carrying queries, and whether each saved connection
// answers. The app's onboarding screen polls it.
type Sources struct {
	NowMs float64 `json:"now_ms"`
	// SinceMs is where the windowed counts start (window_statements on a
	// proxy, window_recorded on a live capture): the since_ms asked for,
	// or 15 minutes before now.
	SinceMs float64 `json:"since_ms"`
	// OTLP is where an app should send OpenTelemetry.
	OTLP OTLPInfo `json:"otlp"`
	// Services is every service Leadline holds data for, by name, and every
	// attached source, whether or not it has sent anything yet.
	Services    []ServiceSource  `json:"services"`
	Proxies     []ProxyView      `json:"proxies"`
	Connections []ConnectionView `json:"connections"`
	// Attached is every source `leadline attach` registered: a worktree
	// each, with the connections it owns.
	Attached []AttachedView `json:"attached"`
}

type OTLPInfo struct {
	// Endpoints take OTLP/HTTP: POST to /v1/traces, /v1/logs, /v1/metrics.
	Endpoints []string `json:"endpoints"`
}

// ServiceSource is one service and how much of each signal Leadline holds
// for it. The counts are of what is stored now, so they fall as old data
// is dropped.
type ServiceSource struct {
	Name         string `json:"name"`
	Spans        int64  `json:"spans"`
	Logs         int64  `json:"logs"`
	MetricPoints int64  `json:"metric_points"`
	// LastSeenMs is the time of its newest span, log or metric point.
	LastSeenMs float64 `json:"last_seen_ms"`
	// Owner is the worktree the service belongs to, when it is an attached
	// source or a command run in one. Absent for any other service.
	Owner *Owner `json:"owner,omitempty"`
	// Via is how its data arrives: "otlp" (the app exports it),
	// "postgres-proxy" or "redis-monitor" (Leadline captured it). An app
	// can be all three.
	Via []string `json:"via"`
	// Through names the captures some of its database calls came through,
	// since the daemon started: which proxy, and for which connections.
	Through []Through `json:"through"`
}

// Through is one capture a service's database calls were recorded by.
type Through struct {
	// Via is "postgres-proxy" or "redis-monitor".
	Via string `json:"via"`
	// ProxyID is the proxy's id (see proxies), for a Postgres proxy that
	// is still open.
	ProxyID string `json:"proxy_id,omitempty"`
	// ConnectionIDs are the saved connections the capture is for: the
	// proxy's holders, or the Redis connection being watched.
	ConnectionIDs []string `json:"connection_ids"`
}

// ownerOfService finds the attached source a service belongs to: the one
// of that name, or the one it is a command of ("acme/fix-orders:worker").
func ownerOfService(name string, attached []*Attachment) *Owner {
	for _, a := range attached {
		if under(name, a.Service) {
			o := a.Owner.labelled()
			return &o
		}
	}
	return nil
}

const (
	maxServices = 50
	// The per-service totals are recomputed when the store has changed,
	// but no more often than this, and no more often than twenty times
	// what the last computation took: so never more than a twentieth of
	// a core, however big the store and however often it is asked.
	servicesMinEvery = 500 * time.Millisecond
	servicesMaxEvery = 10 * time.Second
)

type serviceRow struct {
	name                          string
	spans, appSpans, logs, points int64
	lastNs                        int64
}

// servicesCache holds the last per-service totals and what the store's
// row counts were when they were taken.
type servicesCache struct {
	mu    sync.Mutex
	known bool
	probe [3]int64
	at    time.Time
	took  time.Duration
	rows  []serviceRow
	// peeked is what was found by looking, for services that have only
	// database spans, and more of them than the captures running now have
	// written: were the rest captured before a restart, or does the app
	// send only database spans?
	peeked map[string]string
}

// Sources answers "what is feeding Leadline right now". It is made to be
// polled: with nothing new in the store it costs three row counts.
func (m *Manager) Sources(ctx context.Context) (*Sources, error) {
	return m.SourcesSince(ctx, time.Time{})
}

// SourcesSince is Sources with the windowed counts of proxies and live
// captures starting at since; the zero time is 15 minutes ago.
func (m *Manager) SourcesSince(ctx context.Context, since time.Time) (*Sources, error) {
	now := time.Now()
	if since.IsZero() {
		since = now.Add(-windowDefault)
	}
	out := &Sources{NowMs: ms(now), SinceMs: ms(since), Services: []ServiceSource{}}
	rows, err := m.serviceRows(ctx)
	if err != nil {
		return nil, err
	}
	// A captured call that the app also reported is stored under the app's
	// name, and counted here by it.
	// The captures behind each service: capture id -> source.
	wire := map[string]string{store.SourcePostgresWire: sourcePostgresProxy, store.SourceRedisMonitor: sourceRedisMonitor}
	paired := map[string]map[string]int64{}
	through := map[string]map[string]string{}
	note := func(service, id, source string) {
		if id == "" {
			return
		}
		if through[service] == nil {
			through[service] = map[string]string{}
		}
		through[service][id] = source
	}
	if m.store != nil {
		for k, n := range m.store.Paired() {
			if paired[k.Service] == nil {
				paired[k.Service] = map[string]int64{}
			}
			paired[k.Service][wire[k.Source]] += n
			note(k.Service, k.Capture, wire[k.Source])
		}
	}
	m.cmu.Lock()
	for service, ids := range m.through {
		for id, source := range ids {
			note(service, id, source)
		}
	}
	for _, r := range rows {
		s := ServiceSource{Name: r.name, Spans: r.spans, Logs: r.logs, MetricPoints: r.points,
			LastSeenMs: float64(r.lastNs) / 1e6, Via: []string{}, Through: []Through{}}
		captured := m.captured[r.name]
		peek := m.services.peeked[r.name]
		// It sends OTLP if it has anything a capture can't have written:
		// logs, metrics, or a span that isn't a database call. Database
		// spans alone could be either; peek has looked at them.
		otlp := r.logs > 0 || r.points > 0 || r.appSpans > 0 || peek == sourceOTLP
		if otlp {
			s.Via = append(s.Via, sourceOTLP)
		}
		for _, source := range []string{sourcePostgresProxy, sourceRedisMonitor} {
			if captured[source] > 0 || peek == source || paired[r.name][source] > 0 {
				s.Via = append(s.Via, source)
			}
		}
		out.Services = append(out.Services, s)
	}
	m.cmu.Unlock()

	m.mu.Lock()
	m.retryProxiesLocked()
	m.refreshHealthLocked()
	m.since = since
	out.OTLP.Endpoints = append([]string{}, m.otlp...)
	out.Proxies = m.proxyViewsLocked()
	out.Connections = make([]ConnectionView, len(m.conns))
	for i, e := range m.conns {
		out.Connections[i] = m.viewLocked(e)
	}
	m.since = time.Time{}
	// An attached worktree is a source from the moment it is attached: it
	// is listed with nothing held until its first span or log arrives.
	out.Attached = make([]AttachedView, 0, len(m.attached))
	listed := map[string]bool{}
	for i := range out.Services {
		listed[out.Services[i].Name] = true
		out.Services[i].Owner = ownerOfService(out.Services[i].Name, m.attached)
	}
	for _, a := range m.attached {
		out.Attached = append(out.Attached, m.attachedViewLocked(a))
		if !listed[a.Service] {
			o := a.Owner.labelled()
			out.Services = append(out.Services, ServiceSource{Name: a.Service, Owner: &o, Via: []string{}, Through: []Through{}})
		}
	}
	sort.SliceStable(out.Services, func(i, j int) bool { return out.Services[i].Name < out.Services[j].Name })
	// A capture's id is a proxy's or, for Redis, the connection's own.
	for i := range out.Services {
		svc := &out.Services[i]
		ids := make([]string, 0, len(through[svc.Name]))
		for id := range through[svc.Name] {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			t := Through{Via: through[svc.Name][id], ConnectionIDs: []string{}}
			if t.Via == sourceRedisMonitor {
				if m.findLocked(id) == nil {
					continue // the connection has been deleted
				}
				t.ConnectionIDs = []string{id}
			} else {
				var px *proxy
				for _, x := range m.proxies {
					if x.id == id {
						px = x
					}
				}
				if px == nil {
					continue // a run's proxy that has closed
				}
				t.ProxyID = id
				for holder := range px.holders {
					t.ConnectionIDs = append(t.ConnectionIDs, holder)
				}
				sort.Strings(t.ConnectionIDs)
			}
			svc.Through = append(svc.Through, t)
		}
	}
	m.mu.Unlock()
	return out, nil
}

func (m *Manager) serviceRows(ctx context.Context) ([]serviceRow, error) {
	if m.db == nil {
		return nil, nil
	}
	c := &m.services
	c.mu.Lock()
	defer c.mu.Unlock()

	var probe [3]int64
	err := m.db.QueryRowContext(ctx, `
SELECT (SELECT count(*) FROM spans), (SELECT count(*) FROM logs), (SELECT count(*) FROM metric_points)`).
		Scan(&probe[0], &probe[1], &probe[2])
	if err != nil {
		return nil, err
	}
	if c.known && probe == c.probe {
		return c.rows, nil
	}
	// The first data must show at once. After that a busy store may be a
	// moment ahead of what is shown.
	every := min(max(20*c.took, servicesMinEvery), servicesMaxEvery)
	if c.known && len(c.rows) > 0 && time.Since(c.at) < every {
		return c.rows, nil
	}

	began := time.Now()
	// A span counts as the app's own when it is not a database call and
	// not the "transaction" frame the Postgres proxy puts around one.
	rows, err := m.db.QueryContext(ctx, `
SELECT service, sum(spans)::BIGINT, sum(app_spans)::BIGINT, sum(logs)::BIGINT, sum(points)::BIGINT, max(last_ns)::BIGINT
FROM (
	SELECT service, count(*) AS spans,
	       count(*) FILTER (WHERE coalesce(db_system, '') = '' AND name <> 'transaction') AS app_spans,
	       0 AS logs, 0 AS points, max(start_ns) AS last_ns
	FROM spans GROUP BY service
	UNION ALL
	SELECT service, 0, 0, count(*), 0, max(ts_ns) FROM logs GROUP BY service
	UNION ALL
	SELECT service, 0, 0, 0, count(*), max(ts_ns) FROM metric_points GROUP BY service
) GROUP BY service ORDER BY max(last_ns) DESC LIMIT `+strconv.Itoa(maxServices))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []serviceRow
	for rows.Next() {
		var r serviceRow
		if err := rows.Scan(&r.name, &r.spans, &r.appSpans, &r.logs, &r.points, &r.lastNs); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	m.peek(ctx, c, out)
	c.known, c.probe, c.rows = true, probe, out
	c.at, c.took = time.Now(), time.Since(began)
	return out, nil
}

// peek settles, once per service, where a service with nothing but
// database spans got them, when no capture running now can say. It reads
// a few of the spans: captures sign theirs.
func (m *Manager) peek(ctx context.Context, c *servicesCache, rows []serviceRow) {
	for _, r := range rows {
		if r.logs > 0 || r.points > 0 || r.appSpans > 0 || r.spans == 0 {
			continue
		}
		if _, done := c.peeked[r.name]; done {
			continue
		}
		// Nothing to settle when the captures running now account for
		// every span it has.
		var captured int64
		m.cmu.Lock()
		for _, n := range m.captured[r.name] {
			captured += n
		}
		m.cmu.Unlock()
		if captured >= r.spans {
			continue
		}
		found := m.peekOne(ctx, r.name)
		if found == "" {
			continue
		}
		if c.peeked == nil {
			c.peeked = map[string]string{}
		}
		m.cmu.Lock()
		c.peeked[r.name] = found
		m.cmu.Unlock()
	}
}

func (m *Manager) peekOne(ctx context.Context, service string) string {
	rows, err := m.db.QueryContext(ctx, `SELECT attrs FROM spans WHERE service = ? AND coalesce(db_system, '') <> '' LIMIT 20`, service)
	if err != nil {
		return ""
	}
	defer rows.Close()
	found := ""
	for rows.Next() {
		var attrs string
		if rows.Scan(&attrs) != nil {
			return ""
		}
		switch {
		case strings.Contains(attrs, `"leadline.source":"pgproxy"`):
			if found == "" {
				found = sourcePostgresProxy
			}
		case strings.Contains(attrs, `"leadline.source":"redis-monitor"`):
			if found == "" {
				found = sourceRedisMonitor
			}
		default:
			// A database span no capture signed: the app sent it.
			return sourceOTLP
		}
	}
	return found
}
