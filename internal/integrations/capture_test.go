package integrations

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sean-brydon/leadline/internal/integrations/fake"
	"github.com/sean-brydon/leadline/internal/store"
)

func TestLimiterIsAHardCap(t *testing.T) {
	l := limiter{max: 3}
	let := 0
	for range 10 {
		if l.allow(100) {
			let++
		}
	}
	if let != 3 {
		t.Errorf("%d let through in one second, want 3", let)
	}
	// A quiet stretch buys nothing: the next second has 3 again, not 6.
	let = 0
	for range 10 {
		if l.allow(105) {
			let++
		}
	}
	if let != 3 {
		t.Errorf("%d let through after a quiet stretch, want 3", let)
	}
}

func TestParseMonitorLine(t *testing.T) {
	for line, want := range map[string]monitorCmd{
		`1339518083.107412 [0 127.0.0.1:60866] "keys" "*"`: {
			ns: 1339518083107412000, db: "0", client: "127.0.0.1:60866", args: []string{"keys", "*"}},
		`1339518087.877697 [0 127.0.0.1:60866] "set" "x" "a \"quoted\" \\ value\n\x00\xff"`: {
			ns: 1339518087877697000, db: "0", client: "127.0.0.1:60866", args: []string{"set", "x", "a \"quoted\" \\ value\n\x00\xff"}},
		`1339518090.420270 [0 lua] "get" "x"`: {
			ns: 1339518090420270000, db: "0", client: "lua", args: []string{"get", "x"}},
		`1339518090.5 [3 unix:/tmp/redis.sock] "hset" "h" "f" ""`: {
			ns: 1339518090500000000, db: "3", client: "unix:/tmp/redis.sock", args: []string{"hset", "h", "f", ""}},
	} {
		got, ok := parseMonitorLine(line)
		if !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("%s\n got %+v (%v)\nwant %+v", line, got, ok, want)
		}
	}
	for _, bad := range []string{"OK", "", `1339518083.107412 [0 127.0.0.1:60866]`, `x [0 a] "get"`, `1.2 [0 a] "unterminated`, `1.2 [0 a] bare`} {
		if got, ok := parseMonitorLine(bad); ok {
			t.Errorf("%q parsed as %+v", bad, got)
		}
	}
}

func TestRedisStatementAndFingerprint(t *testing.T) {
	for _, c := range []struct {
		args            []string
		statement, kind string
	}{
		{[]string{"get", "user:42"}, "GET user:42", "GET user:?"},
		{[]string{"GET", "user:43"}, "GET user:43", "GET user:?"},
		{[]string{"set", "user:42", `{"id":42,"name":"Ada Lovelace"}`, "EX", "60"}, `SET user:42 "{\"id\":42,\"name\":\"Ada Lovelace\"}" EX 60`, "SET user:? …"},
		{[]string{"hgetall", "cart:7"}, "HGETALL cart:7", "HGETALL cart:?"},
		{[]string{"get", "session:9f8a7c6b5d4e3f2a1b0c9d8e7f6a5b4c"}, "GET session:9f8a7c6b5d4e3f2a1b0c9d8e7f6a5b4c", "GET session:?"},
		{[]string{"del", "job:3f2504e0-4f89-11d3-9a0c-0305e82c3301"}, "DEL job:3f2504e0-4f89-11d3-9a0c-0305e82c3301", "DEL job:?"},
		{[]string{"config", "get", "maxmemory"}, "CONFIG GET maxmemory", "CONFIG GET maxmemory"},
		{[]string{"ping"}, "PING", "PING"},
		// Credentials never reach a statement.
		{[]string{"auth", "hunter2-secret"}, "AUTH ?", "AUTH"},
		{[]string{"AUTH", "default", "hunter2-secret"}, "AUTH ?", "AUTH"},
		{[]string{"hello", "3", "AUTH", "default", "hunter2-secret", "SETNAME", "web"}, "HELLO 3 AUTH ? ? SETNAME web", "HELLO ? …"},
		{[]string{"config", "set", "requirepass", "hunter2-secret"}, "CONFIG SET requirepass ?", "CONFIG SET requirepass …"},
		{[]string{"acl", "setuser", "bob", "on", ">hunter2-secret", "~*"}, "ACL SETUSER bob ? ? ?", "ACL SETUSER bob …"},
		{[]string{"migrate", "10.0.0.2", "6379", "", "0", "5000", "AUTH", "hunter2-secret", "KEYS", "a"}, `MIGRATE 10.0.0.2 6379 "" 0 5000 AUTH ? KEYS a`, "MIGRATE ?.? …"},
	} {
		if got := redisStatement(c.args); got != c.statement {
			t.Errorf("statement of %q\n got %s\nwant %s", c.args, got, c.statement)
		}
		if got := redisFingerprint(c.args); got != c.kind {
			t.Errorf("fingerprint of %q\n got %s\nwant %s", c.args, got, c.kind)
		}
		if strings.Contains(redisStatement(c.args)+redisFingerprint(c.args), "hunter2") {
			t.Errorf("%q: a credential got through", c.args)
		}
	}
	long := redisStatement([]string{"set", "blob", strings.Repeat("x", 5000)})
	if len(long) > 100 || !strings.HasSuffix(long, "…") {
		t.Errorf("a long value is %d bytes: %s", len(long), long)
	}
	many := make([]string, 200)
	for i := range many {
		many[i] = fmt.Sprintf("k%d", i)
	}
	if got := redisStatement(append([]string{"mget"}, many...)); !strings.HasSuffix(got, "… 184 more") {
		t.Errorf("many arguments: %s", got)
	}
}

type redisSpan struct {
	service, name, statement, fingerprint, attrs string
	durationNs                                   int64
}

func redisSpans(t *testing.T, st *store.Store) []redisSpan {
	t.Helper()
	rows, err := st.DB().Query(`SELECT service, name, db_statement, db_fingerprint, attrs, duration_ns FROM spans WHERE db_system = 'redis' ORDER BY start_ns, db_statement`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []redisSpan
	for rows.Next() {
		var s redisSpan
		if err := rows.Scan(&s.service, &s.name, &s.statement, &s.fingerprint, &s.attrs, &s.durationNs); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func TestRedisLiveCapture(t *testing.T) {
	rd := fake.NewRedis(t)
	st := openStore(t)
	woken := make(chan struct{}, 64)
	m, err := Open(Options{Store: st, ProxyPorts: testPorts(t), Notify: func() {
		select {
		case woken <- struct{}{}:
		default:
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	ctx := context.Background()

	// Off unless asked for.
	a := add(t, m, ConnectionInput{URL: ptr(rd.URL()), Name: ptr("acme-cache")})
	if a.Capture || a.Monitor != nil || !a.CaptureOffer.Available || !strings.Contains(a.CaptureOffer.Note, "throughput") {
		t.Fatalf("a new Redis connection: %+v", a)
	}
	for _, cmd := range rd.Ran() {
		if strings.EqualFold(cmd[0], "MONITOR") {
			t.Fatal("MONITOR was sent without being asked for")
		}
	}

	a, err = m.Update(ctx, a.ID, ConnectionInput{Capture: ptr(true)})
	if err != nil {
		t.Fatal(err)
	}
	rd.WaitMonitor(t)
	eventually(t, "the capture to be streaming", func() bool {
		v, _ := m.Connection(a.ID)
		return v.Monitor != nil && v.Monitor.State == "streaming"
	})

	// Leadline's own stats connection is not the app's.
	if _, err := m.Stats(ctx, a.ID, true); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	own := m.conns[0].own.Load()
	m.mu.Unlock()
	if own == nil {
		t.Fatal("the stats connection does not know its own address")
	}

	at := time.Now()
	rd.Emit(
		fake.MonitorLine(at, "127.0.0.1:50001", "AUTH", "(redacted)"),
		fake.MonitorLine(at.Add(time.Millisecond), "127.0.0.1:50001", "get", "user:42"),
		fake.MonitorLine(at.Add(2*time.Millisecond), "127.0.0.1:50001", "get", "user:43"),
		fake.MonitorLine(at.Add(3*time.Millisecond), "127.0.0.1:50002", "set", "user:42", `{"id":42}`),
		fake.MonitorLine(at.Add(4*time.Millisecond), *own, "info"),
	)
	var spans []redisSpan
	eventually(t, "the captured commands", func() bool {
		spans = redisSpans(t, st)
		return len(spans) == 3
	})
	want := []redisSpan{
		{"acme-cache", "GET", "GET user:42", "GET user:?", "", 0},
		{"acme-cache", "GET", "GET user:43", "GET user:?", "", 0},
		{"acme-cache", "SET", `SET user:42 {"id":42}`, "SET user:? …", "", 0},
	}
	for i, got := range spans {
		for _, part := range []string{`"leadline.source":"redis-monitor"`, `"leadline.no_duration":true`, `"db.namespace":"0"`, `"client.address":"127.0.0.1:5000`} {
			if !strings.Contains(got.attrs, part) {
				t.Errorf("span %d attrs lack %s: %s", i, part, got.attrs)
			}
		}
		got.attrs = ""
		if got != want[i] {
			t.Errorf("span %d\n got %+v\nwant %+v", i, got, want[i])
		}
	}
	select {
	case <-woken:
	case <-time.After(2 * time.Second):
		t.Error("the app was not told that data arrived")
	}

	v, _ := m.Connection(a.ID)
	if mv := v.Monitor; mv.Seen != 3 || mv.Recorded != 3 || mv.Dropped != 0 || mv.MaxPerSec != MonitorMaxPerSec || mv.LastActivityMs == 0 || !strings.Contains(mv.Note, "durations are always 0") {
		t.Errorf("monitor = %+v", mv)
	}
	// The queries list groups them like any other database's.
	qs, err := st.Queries(ctx, store.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 2 || qs[0].DBSystem != "redis" {
		t.Errorf("queries = %+v", qs)
	}

	// Off again ends the stream.
	if a, err = m.Update(ctx, a.ID, ConnectionInput{Capture: ptr(false)}); err != nil || a.Monitor != nil {
		t.Fatalf("capture off: %+v, %v", a.Monitor, err)
	}
	rd.Emit(fake.MonitorLine(time.Now(), "127.0.0.1:50001", "get", "user:44"))
	time.Sleep(300 * time.Millisecond)
	if n := len(redisSpans(t, st)); n != 3 {
		t.Errorf("%d spans after capture was turned off, want 3", n)
	}
}

func TestMonitorRateCap(t *testing.T) {
	rd := fake.NewRedis(t)
	st := openStore(t)
	m := open(t, "", st)
	const limit, sent = 40, 6000
	m.monitorMax = limit

	a := add(t, m, ConnectionInput{URL: ptr(rd.URL()), Name: ptr("acme-cache"), Capture: ptr(true)})
	rd.WaitMonitor(t)

	began := time.Now()
	lines := make([]string, 0, 500)
	for i := range sent {
		lines = append(lines, fake.MonitorLine(began, "127.0.0.1:50001", "get", fmt.Sprintf("user:%d", i)))
		if len(lines) == cap(lines) {
			rd.Emit(lines...)
			lines = lines[:0]
		}
	}
	var mv *MonitorView
	eventually(t, "every command to be seen", func() bool {
		v, _ := m.Connection(a.ID)
		mv = v.Monitor
		return mv.Seen == sent
	})
	// One allowance for each clock second the burst touched.
	seconds := time.Now().Unix() - began.Unix() + 1
	if mv.Recorded > limit*seconds || mv.Recorded < limit {
		t.Errorf("recorded %d of %d over %d s with a cap of %d a second", mv.Recorded, sent, seconds, limit)
	}
	if mv.Dropped != sent-mv.Recorded || mv.Dropped == 0 || mv.MaxPerSec != limit {
		t.Errorf("monitor = %+v", mv)
	}
	eventually(t, "the kept commands to be stored", func() bool {
		return int64(len(redisSpans(t, st))) == mv.Recorded
	})
	time.Sleep(2 * monitorFlush)
	if n := int64(len(redisSpans(t, st))); n != mv.Recorded {
		t.Errorf("%d spans stored, %d recorded", n, mv.Recorded)
	}
}

// Closing the manager with a capture running must not hang.
func TestCloseEndsALiveCapture(t *testing.T) {
	rd := fake.NewOpenRedis(t)
	m := open(t, "", openStore(t))
	a := add(t, m, ConnectionInput{URL: ptr(rd.URL()), Capture: ptr(true)})
	rd.WaitMonitor(t)
	eventually(t, "streaming", func() bool {
		v, _ := m.Connection(a.ID)
		return v.Monitor.State == "streaming"
	})
	done := make(chan struct{})
	go func() { m.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung on a live capture")
	}
}

func TestSources(t *testing.T) {
	pg, rd := fake.NewPostgres(t), fake.NewRedis(t)
	st := openStore(t)
	m := open(t, "", st)
	m.SetOTLP("http://127.0.0.1:4320")
	ctx := context.Background()
	sources := func() *Sources {
		t.Helper()
		out, err := m.Sources(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	empty := sources()
	if len(empty.Services) != 0 || len(empty.Proxies) != 0 || len(empty.Connections) != 0 || len(empty.OTLP.Endpoints) != 1 {
		t.Fatalf("with nothing connected: %+v", empty)
	}

	// An app exports OpenTelemetry: a request with a query in it, a log,
	// a metric. Another sends database spans and nothing else.
	now := time.Now().UnixNano()
	if err := st.AddSpans([]store.Span{
		{TraceID: "t1", SpanID: "s1", Service: "acme-web", Name: "GET /orders", Kind: "server", StartNs: now - 5e6, DurationNs: 4e6, Status: "ok", Attrs: "{}"},
		{TraceID: "t1", SpanID: "s2", ParentID: "s1", Service: "acme-web", Name: "SELECT orders", Kind: "client", StartNs: now - 4e6, DurationNs: 1e6,
			Status: "ok", DBSystem: "postgresql", DBStatement: "SELECT 1", DBFingerprint: "SELECT ?", Attrs: "{}"},
		{TraceID: "t2", SpanID: "s3", Service: "acme-script", Name: "SELECT users", Kind: "client", StartNs: now - 3e6, DurationNs: 1e6,
			Status: "ok", DBSystem: "postgresql", DBStatement: "SELECT 2", DBFingerprint: "SELECT ?", Attrs: `{"db.namespace":"acme_dev"}`},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddLogs([]store.Log{{TsNs: now, Service: "acme-worker", Severity: "INFO", SeverityNum: 9, Body: "sent 3 emails", Attrs: "{}"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMetrics([]store.MetricPoint{{Name: "http.server.requests", Type: store.MetricSum, Service: "acme-web", Series: 1, Attrs: "{}", TsNs: now, StartNs: now - 1e9, Value: 3, Cumulative: true, Monotonic: true}}); err != nil {
		t.Fatal(err)
	}

	// The very next poll has it: this is the onboarding screen's "it works".
	byName := func(s *Sources) map[string]ServiceSource {
		out := map[string]ServiceSource{}
		for _, svc := range s.Services {
			out[svc.Name] = svc
		}
		return out
	}
	got := byName(sources())
	for name, want := range map[string]ServiceSource{
		"acme-web":    {Spans: 2, MetricPoints: 1, Via: []string{"otlp"}},
		"acme-worker": {Logs: 1, Via: []string{"otlp"}},
		"acme-script": {Spans: 1, Via: []string{"otlp"}},
	} {
		g := got[name]
		if g.Spans != want.Spans || g.Logs != want.Logs || g.MetricPoints != want.MetricPoints || !reflect.DeepEqual(g.Via, want.Via) || g.LastSeenMs < float64(now)/1e6-1000 {
			t.Errorf("%s = %+v, want %+v", name, g, want)
		}
	}
	if len(got) != 3 {
		t.Errorf("services = %+v", got)
	}

	// Polling an unchanged store recomputes nothing.
	computed := m.services.at
	for range 5 {
		sources()
	}
	if m.services.at != computed {
		t.Error("an unchanged store was aggregated again")
	}

	// A proxy and a live capture feed it too, and say so.
	c := add(t, m, ConnectionInput{URL: ptr(pg.URL()), Name: ptr("Acme dev"), Capture: ptr(true)})
	r := add(t, m, ConnectionInput{URL: ptr(rd.URL()), Name: ptr("acme-cache"), Capture: ptr(true)})
	rd.WaitMonitor(t)
	queryAs(t, "acme-cron", c.Proxy.Listen, pg.Password, "SELECT count(*) FROM orders WHERE user_id = 42")
	queryAs(t, "acme-web", c.Proxy.Listen, pg.Password, "SELECT count(*) FROM orders WHERE user_id = 42")
	eventually(t, "the capture to be streaming", func() bool {
		v, _ := m.Connection(r.ID)
		return v.Monitor.State == "streaming"
	})
	rd.Emit(fake.MonitorLine(time.Now(), "127.0.0.1:50001", "get", "user:42"))

	var src *Sources
	eventually(t, "captured data in sources", func() bool {
		src = sources()
		got = byName(src)
		return got["acme-cron"].Spans == 1 && got["acme-cache"].Spans == 1 && got["acme-web"].Spans == 3
	})
	for name, via := range map[string][]string{
		"acme-cron":  {"postgres-proxy"},
		"acme-cache": {"redis-monitor"},
		"acme-web":   {"otlp", "postgres-proxy"},
	} {
		if !reflect.DeepEqual(got[name].Via, via) {
			t.Errorf("%s arrives via %v, want %v", name, got[name].Via, via)
		}
	}
	if len(src.Proxies) != 1 || src.Proxies[0].Statements != 2 || src.Proxies[0].ConnectionsTotal != 2 || src.Proxies[0].LastActivityMs == 0 || src.Proxies[0].Label != "Acme dev" {
		t.Errorf("proxies = %+v", src.Proxies)
	}
	if len(src.Connections) != 2 || src.Connections[0].Health.State != "ok" || src.Connections[1].Health.State != "ok" ||
		src.Connections[0].Proxy == nil || src.Connections[1].Monitor == nil || src.Connections[1].Monitor.Recorded != 1 {
		t.Errorf("connections = %+v", src.Connections)
	}
	mustNotHold(t, "sources", src, pg.Password, rd.Password)

	// After a restart nothing in memory says who wrote what; the spans
	// themselves still do.
	m.Close()
	m2 := open(t, "", st)
	out, err := m2.Sources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got = byName(out)
	for name, via := range map[string][]string{
		"acme-cron":   {"postgres-proxy"},
		"acme-cache":  {"redis-monitor"},
		"acme-web":    {"otlp"},
		"acme-script": {"otlp"},
	} {
		if !reflect.DeepEqual(got[name].Via, via) {
			t.Errorf("after a restart %s arrives via %v, want %v", name, got[name].Via, via)
		}
	}
}

// A connection that stops answering shows as unhealthy the next time
// someone looks, without anyone asking for its stats.
func TestHealthFollowsTheServer(t *testing.T) {
	pg := fake.NewPostgres(t)
	m := open(t, "", nil)
	m.healthEvery = func(bool) time.Duration { return 0 }
	a := add(t, m, ConnectionInput{URL: ptr(pg.URL())})
	if a.Health.State != "ok" {
		t.Fatalf("health = %+v", a.Health)
	}
	pg.Close()
	m.mu.Lock()
	cl := m.conns[0].cl
	m.mu.Unlock()
	cl.mu.Lock()
	cl.dropLocked()
	cl.mu.Unlock()
	eventually(t, "the connection to show as down", func() bool {
		v := m.Connections()[0]
		return v.Health.State == "error" && v.Health.Code == "unreachable"
	})
}
