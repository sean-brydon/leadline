package integrations

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sean-brydon/leadline/internal/integrations/fake"
	"github.com/sean-brydon/leadline/internal/pgproxy"
	"github.com/sean-brydon/leadline/internal/store"
)

func ptr[T any](v T) *T { return &v }

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// open starts a manager on dir. It is closed with the test, before the
// store it writes to.
func open(t *testing.T, dir string, st *store.Store) *Manager {
	t.Helper()
	m, err := Open(Options{Dir: dir, Store: st, ProxyPorts: testPorts(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}

// testPorts is this test's own stretch of ports for proxies: the same one
// each time it is asked, so a "restarted" manager looks where the first did.
func testPorts(t *testing.T) PortRange {
	t.Helper()
	portsMu.Lock()
	defer portsMu.Unlock()
	if r, ok := portsFor[t.Name()]; ok {
		return r
	}
	first, last := fake.Ports(t, 8)
	portsFor[t.Name()] = PortRange{first, last}
	return portsFor[t.Name()]
}

var (
	portsMu  sync.Mutex
	portsFor = map[string]PortRange{}
)

func add(t *testing.T, m *Manager, in ConnectionInput) ConnectionView {
	t.Helper()
	v, err := m.Add(context.Background(), in)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	return v
}

// mustNotHold fails if what leaves the daemon has a password in it.
func mustNotHold(t *testing.T, what string, v any, secrets ...string) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range secrets {
		if strings.Contains(string(raw), s) {
			t.Errorf("%s contains a password: %s", what, raw)
		}
	}
}

// query runs one statement through addr the way an app called acme-web
// would.
func query(t *testing.T, addr, password, sql string) {
	t.Helper()
	queryAs(t, "acme-web", addr, password, sql)
}

func queryAs(t *testing.T, app, addr, password, sql string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pgconn.Connect(ctx, "postgres://acme:"+password+"@"+addr+"/acme_dev?sslmode=disable&application_name="+app)
	if err != nil {
		t.Fatalf("connect through %s: %v", addr, err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, sql).ReadAll(); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestConnectionsFile(t *testing.T) {
	pg, rd := fake.NewPostgres(t), fake.NewRedis(t)
	dir := t.TempDir()
	path := filepath.Join(dir, fileName)

	m := open(t, dir, nil)
	a := add(t, m, ConnectionInput{URL: ptr(pg.URL()), Name: ptr("Acme dev")})
	b := add(t, m, ConnectionInput{URL: ptr(rd.URL())})
	if a.Kind != KindPostgres || b.Kind != KindRedis || b.Name != "redis-"+portOf(rd.Addr()) {
		t.Fatalf("added %+v and %+v", a, b)
	}
	if a.Health.State != "ok" || a.Health.Version != "17.0" || b.Health.State != "ok" || b.Health.Version != "7.4.0" {
		t.Errorf("health after add: %+v, %+v", a.Health, b.Health)
	}
	m.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("connections file is %o, want 600: it holds passwords", perm)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), pg.Password) {
		t.Fatalf("the file is where the password is kept, and it isn't there: %s", raw)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(left) > 0 {
		t.Errorf("temporary files left behind: %v", left)
	}

	// A file whose permissions were widened is narrowed again on load.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	m2 := open(t, dir, nil)
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("after load the file is %o, want 600", fi.Mode().Perm())
	}
	got := m2.Connections()
	if len(got) != 2 || got[0].ID != a.ID || got[1].ID != b.ID || got[0].Name != "Acme dev" {
		t.Fatalf("after a restart: %+v", got)
	}
	if want := "postgres://acme:****@" + pg.Addr() + "/acme_dev?sslmode=disable"; got[0].URL != want {
		t.Errorf("url = %q, want %q", got[0].URL, want)
	}
	if want := "redis://:****@" + rd.Addr() + "/0"; got[1].URL != want {
		t.Errorf("url = %q, want %q", got[1].URL, want)
	}

	if err := m2.Delete(a.ID); err != nil {
		t.Fatal(err)
	}
	if err := m2.Delete(a.ID); AsProblem(err).Code != "not_found" {
		t.Errorf("deleting twice: %v", err)
	}
	raw, _ = os.ReadFile(path)
	if strings.Contains(string(raw), pg.Password) || !strings.Contains(string(raw), rd.Password) {
		t.Errorf("after deleting the Postgres connection the file is: %s", raw)
	}
}

func portOf(addr string) string {
	_, port, _ := net.SplitHostPort(addr)
	return port
}

func TestUnreadableFileIsMovedAside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, fileName)
	if err := os.WriteFile(path, []byte(`{"connections": [oops`), 0o600); err != nil {
		t.Fatal(err)
	}
	m := open(t, dir, nil)
	if n := len(m.Connections()); n != 0 {
		t.Fatalf("%d connections from an unreadable file", n)
	}
	if kept, err := os.ReadFile(path + ".unreadable"); err != nil || !strings.Contains(string(kept), "oops") {
		t.Errorf("the unreadable file was not kept: %v", err)
	}
}

// Nothing that leaves the manager may carry a password: not a view, not a
// test result, not the stats, not an error.
func TestPasswordsStayIn(t *testing.T) {
	pg, rd := fake.NewPostgres(t), fake.NewRedis(t)
	pg.SetStatements("ok")
	st := openStore(t)
	m := open(t, t.TempDir(), st)
	ctx := context.Background()
	secrets := []string{pg.Password, rd.Password}

	a := add(t, m, ConnectionInput{URL: ptr(pg.URL()), Capture: ptr(true)})
	b := add(t, m, ConnectionInput{URL: ptr(rd.URL()), Capture: ptr(true)})
	mustNotHold(t, "the added connections", []any{a, b}, secrets...)
	if !strings.Contains(a.Proxy.URL, "acme:****@127.0.0.1:") || !strings.HasSuffix(a.Proxy.URL, "/acme_dev?sslmode=disable") {
		t.Errorf("proxy url = %q", a.Proxy.URL)
	}

	mustNotHold(t, "the list", m.Connections(), secrets...)
	src, err := m.Sources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mustNotHold(t, "sources", src, secrets...)
	mustNotHold(t, "proxies", m.Proxies(), secrets...)

	for _, id := range []string{a.ID, b.ID} {
		stats, err := m.Stats(ctx, id, true)
		if err != nil {
			t.Fatalf("stats: %v", err)
		}
		mustNotHold(t, "stats", stats, secrets...)
		mustNotHold(t, "a test of a saved connection", m.Test(ctx, TestInput{ID: id}), secrets...)
	}
	mustNotHold(t, "a test", m.Test(ctx, TestInput{URL: pg.URL()}), secrets...)
	mustNotHold(t, "a test", m.Test(ctx, TestInput{URL: rd.URL()}), secrets...)

	// Errors are where a driver would put the whole URL.
	wrong := "s3cret-wrong-pw"
	res := m.Test(ctx, TestInput{URL: "postgres://acme:" + wrong + "@" + pg.Addr() + "/acme_dev?sslmode=disable"})
	if res.OK {
		t.Fatal("a wrong password connected")
	}
	mustNotHold(t, "a failed test", res, wrong)
	res = m.Test(ctx, TestInput{URL: "postgres://acme:" + wrong + "@127.0.0.1:99999999/acme_dev"})
	mustNotHold(t, "an unreadable URL's error", res, wrong)
	if res.Error == nil || res.Error.Code != "bad_url" {
		t.Errorf("unreadable URL: %+v", res.Error)
	}
	_, err = m.Add(ctx, ConnectionInput{URL: ptr("postgres://acme:" + wrong + "@[bad/acme_dev")})
	mustNotHold(t, "an unreadable URL's error", AsProblem(err), wrong)

	// A password in the query string is a password too.
	q := add(t, m, ConnectionInput{URL: ptr("postgres://" + pg.Addr() + "/acme_dev?sslmode=disable&user=acme&password=" + pg.Password)})
	mustNotHold(t, "a connection with its password in the query", q, pg.Password)
	if q.Health.State != "ok" || !strings.Contains(q.URL, "password=%2A%2A%2A%2A") && !strings.Contains(q.URL, "password=****") {
		t.Errorf("query-string password: health %+v, url %q", q.Health, q.URL)
	}
}

func TestEditKeepsTheSavedPassword(t *testing.T) {
	pg := fake.NewPostgres(t)
	dir := t.TempDir()
	m := open(t, dir, nil)
	ctx := context.Background()
	a := add(t, m, ConnectionInput{URL: ptr(pg.URL())})

	// The app sends back the masked URL with the host respelled.
	edited := strings.Replace(a.URL, "127.0.0.1", "localhost", 1)
	got, err := m.Update(ctx, a.ID, ConnectionInput{URL: &edited, Name: ptr("renamed")})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "renamed" || !strings.Contains(got.URL, "localhost") || got.Health.State != "ok" {
		t.Errorf("after the edit: %+v", got)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, fileName))
	if !strings.Contains(string(raw), "acme:"+pg.Password+"@localhost") {
		t.Errorf("the saved password was not kept: %s", raw)
	}
	// And a test of the masked URL uses it too.
	if res := m.Test(ctx, TestInput{ID: a.ID, URL: got.URL}); !res.OK {
		t.Errorf("testing the masked URL of a saved connection: %+v", res.Error)
	}
	if res := m.Test(ctx, TestInput{URL: got.URL}); res.OK || res.Error.Code != "auth_failed" {
		t.Errorf("the mask is not a password: %+v", res)
	}

	if _, err := m.Update(ctx, a.ID, ConnectionInput{URL: ptr("redis://localhost:6379")}); AsProblem(err).Code != "bad_url" {
		t.Errorf("a Postgres connection took a Redis URL: %v", err)
	}
	if _, err := m.Update(ctx, "c_nope", ConnectionInput{Name: ptr("x")}); AsProblem(err).Code != "not_found" {
		t.Errorf("editing a missing connection: %v", err)
	}
}

func TestProxyPortSurvivesARestart(t *testing.T) {
	pg := fake.NewPostgres(t)
	st := openStore(t)
	dir := t.TempDir()
	ctx := context.Background()

	m := open(t, dir, st)
	a := add(t, m, ConnectionInput{URL: ptr(pg.URL()), Capture: ptr(true)})
	if r := testPorts(t); a.Proxy == nil || a.Proxy.State != "listening" || a.Proxy.Port < r.First || a.Proxy.Port > r.Last {
		t.Fatalf("proxy = %+v", a.Proxy)
	}
	port, listen := a.Proxy.Port, a.Proxy.Listen
	query(t, listen, pg.Password, "SELECT count(*) FROM orders WHERE user_id = 42")
	eventually(t, "the statement to be counted", func() bool {
		v, _ := m.Connection(a.ID)
		return v.Proxy.Statements == 1 && v.Proxy.ConnectionsTotal == 1 && v.Proxy.LastActivityMs > 0
	})

	// A second connection to the same server shares the proxy.
	b := add(t, m, ConnectionInput{URL: ptr(strings.Replace(pg.URL(), "127.0.0.1", "localhost", 1)), Name: ptr("same server")})
	b, err := m.Update(ctx, b.ID, ConnectionInput{Capture: ptr(true)})
	if err != nil {
		t.Fatal(err)
	}
	if b.Proxy.Port != port || len(m.Proxies()) != 1 {
		t.Errorf("second connection got port %d, want the first's %d; proxies %+v", b.Proxy.Port, port, m.Proxies())
	}
	m.Close()
	if answering(listen) {
		t.Fatal("the proxy is still listening after Close")
	}

	m2 := open(t, dir, st)
	got, _ := m2.Connection(a.ID)
	if got.Proxy == nil || got.Proxy.Port != port || got.Proxy.State != "listening" {
		t.Fatalf("after a restart the proxy is %+v, want port %d", got.Proxy, port)
	}
	query(t, listen, pg.Password, "SELECT count(*) FROM orders WHERE user_id = 43")

	var rows []store.QueryRow
	eventually(t, "both statements to be stored", func() bool {
		var err error
		if rows, err = st.Queries(ctx, store.Filter{}); err != nil {
			t.Fatal(err)
		}
		return len(rows) == 1 && rows[0].Calls == 2
	})
	if rows[0].Fingerprint != "SELECT count(*) FROM orders WHERE user_id = ?" {
		t.Errorf("queries = %+v", rows)
	}

	// Renaming must not restart the proxy: the app's connections are on it.
	before := m2.Proxies()[0].ID
	if _, err := m2.Update(ctx, a.ID, ConnectionInput{Name: ptr("renamed")}); err != nil {
		t.Fatal(err)
	}
	if after := m2.Proxies()[0]; after.ID != before || after.Label != "renamed, same server" {
		t.Errorf("after a rename the proxy is %+v, was %s", after, before)
	}

	// Capture off closes the port once nobody holds it, and keeps the
	// number for next time.
	for _, id := range []string{a.ID, b.ID} {
		if _, err := m2.Update(ctx, id, ConnectionInput{Capture: ptr(false)}); err != nil {
			t.Fatal(err)
		}
	}
	if answering(listen) || len(m2.Proxies()) != 0 {
		t.Errorf("capture is off and the proxy is still there: %+v", m2.Proxies())
	}
	again, err := m2.Update(ctx, a.ID, ConnectionInput{Capture: ptr(true)})
	if err != nil || again.Proxy.Port != port {
		t.Errorf("capture on again: port %v, want %d (%v)", again.Proxy, port, err)
	}
}

func TestProxyPortTaken(t *testing.T) {
	pg := fake.NewPostgres(t)
	dir := t.TempDir()
	ctx := context.Background()

	// Something else has the port a person asks for.
	other, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := other.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	taken := other.Addr().(*net.TCPAddr).Port

	m := open(t, dir, nil)
	_, err = m.Add(ctx, ConnectionInput{URL: ptr(pg.URL()), Capture: ptr(true), ProxyPort: &taken})
	if AsProblem(err).Code != "port_in_use" {
		t.Fatalf("a taken port: %v", err)
	}
	if n := len(m.Connections()); n != 0 {
		t.Fatalf("a connection that could not start was saved (%d)", n)
	}

	// The same at startup is not fatal: the connection is there, says
	// why its proxy isn't, and gets its port when the port comes free.
	a := add(t, m, ConnectionInput{URL: ptr(pg.URL()), Capture: ptr(true)})

	// Moving a running proxy to a taken port fails without stopping it:
	// the app's connections are on it.
	before := m.Proxies()[0].ID
	if _, err := m.Update(ctx, a.ID, ConnectionInput{ProxyPort: &taken}); AsProblem(err).Code != "port_in_use" {
		t.Fatalf("moving to a taken port: %v", err)
	}
	if ps := m.Proxies(); len(ps) != 1 || ps[0].ID != before || ps[0].Port != a.Proxy.Port {
		t.Errorf("a failed move restarted the proxy: %+v", ps)
	}
	m.Close()
	raw, _ := os.ReadFile(filepath.Join(dir, fileName))
	swapped := strings.Replace(string(raw), `"proxy_port": `+portOf(a.Proxy.Listen), `"proxy_port": `+portOf(other.Addr().String()), 1)
	if swapped == string(raw) {
		t.Fatalf("no proxy_port in the file: %s", raw)
	}
	os.WriteFile(filepath.Join(dir, fileName), []byte(swapped), 0o600)

	m2 := open(t, dir, nil)
	got, _ := m2.Connection(a.ID)
	if got.Proxy == nil || got.Proxy.State != "error" || !strings.Contains(got.Proxy.Error, "another program") || got.Proxy.Port != taken {
		t.Fatalf("proxy on a taken port: %+v", got.Proxy)
	}
	other.Close()
	m2.mu.Lock()
	m2.conns[0].pxTried = time.Time{}
	m2.mu.Unlock()
	if got = m2.Connections()[0]; got.Proxy.State != "listening" || got.Proxy.Port != taken {
		t.Errorf("after the port came free: %+v", got.Proxy)
	}
}

func TestCaptureIsForThisMachineOnly(t *testing.T) {
	for url, remote := range map[string]bool{
		"postgres://acme@localhost/acme_dev":                     false,
		"postgres://acme@127.0.0.1:5433/acme_dev":                false,
		"postgres://acme@[::1]/acme_dev":                         false,
		"postgres:///acme_dev?host=/var/run/postgresql":          false,
		"postgres://acme@db.acme.example/acme_prod":              true,
		"postgres://acme@203.0.113.9:5432/acme_prod":             true,
		"redis://localhost":                                      false,
		"rediss://:pw@cache.acme.example:6380/1":                 true,
		"postgres://acme@acme.localhost:5432/acme_dev":           false,
		"postgresql://acme@db.acme.example/acme?sslmode=require": true,
	} {
		tg, err := parseURL("", url)
		if err != nil {
			t.Errorf("%s: %v", url, err)
			continue
		}
		if tg.remote() != remote {
			t.Errorf("%s: remote = %v, want %v", url, tg.remote(), remote)
		}
		if offer := captureOffer(tg); offer.Available == remote || offer.Note == "" {
			t.Errorf("%s: capture offer %+v", url, offer)
		}
	}

	m := open(t, "", nil)
	ctx := context.Background()
	_, err := m.Add(ctx, ConnectionInput{URL: ptr("postgres://acme:pw@db.acme.example/acme_prod"), Capture: ptr(true)})
	if p := AsProblem(err); p.Code != "capture_remote" || !strings.Contains(p.Message, "TLS") {
		t.Errorf("capturing a remote Postgres: %v", err)
	}
	_, err = m.Add(ctx, ConnectionInput{URL: ptr("rediss://:pw@cache.acme.example:6380"), Capture: ptr(true)})
	if p := AsProblem(err); p.Code != "capture_remote" || !strings.Contains(p.Message, "MONITOR") {
		t.Errorf("capturing a remote Redis: %v", err)
	}
	if _, err := m.OpenProxy(KindPostgres, "db.acme.example:5432", "acme-web"); AsProblem(err).Code != "capture_remote" {
		t.Errorf("a run proxy to a remote database: %v", err)
	}
	if n := len(m.Connections()) + len(m.Proxies()); n != 0 {
		t.Errorf("%d things were kept", n)
	}
}

func TestTestErrorsAreForPeople(t *testing.T) {
	pg, rd := fake.NewPostgres(t), fake.NewRedis(t)
	m := open(t, "", nil)
	ctx := context.Background()

	// A port nothing listens on.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	ln.Close()
	// Something that is neither.
	web := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(web.Close)
	webAddr := strings.TrimPrefix(web.URL, "http://")

	for _, c := range []struct {
		url, code, message string
	}{
		{"postgres://acme:wrong@" + pg.Addr() + "/acme_dev?sslmode=disable", "auth_failed", `Password authentication failed for user "acme".`},
		{"postgres://bob:" + pg.Password + "@" + pg.Addr() + "/acme_dev?sslmode=disable", "auth_failed", `Password authentication failed for user "bob".`},
		{"postgres://acme:" + pg.Password + "@" + pg.Addr() + "/nope?sslmode=disable", "no_database", `Database "nope" does not exist.`},
		{"postgres://acme:pw@" + dead + "/acme_dev", "unreachable", "Nothing is listening at " + dead + "."},
		{"postgres://acme:" + pg.Password + "@" + pg.Addr() + "/acme_dev?sslmode=require", "tls", "Postgres at " + pg.Addr() + " does not accept TLS, and the URL asks for it."},
		{"mysql://acme@localhost/acme", "bad_url", "Leadline connects to Postgres (postgres://) and Redis (redis://). This URL is for neither."},
		{"localhost:5432", "bad_url", ""},
		{"", "bad_url", "The URL is empty."},
		{"redis://" + rd.Addr(), "auth_failed", "This Redis needs a password and the URL has none."},
		{"redis://:wrong@" + rd.Addr(), "auth_failed", "Redis refused the user or password."},
		{"redis://" + dead, "unreachable", "Nothing is listening at " + dead + "."},
		{"redis://" + webAddr, "not_redis", "Whatever answers at " + webAddr + " does not speak the Redis protocol."},
		{"redis://localhost:6379/cache", "bad_url", "A Redis URL ends in a database number, as in redis://localhost:6379/0."},
	} {
		res := m.Test(ctx, TestInput{URL: c.url})
		if res.OK || res.Error == nil {
			t.Errorf("%s: connected", c.url)
			continue
		}
		if res.Error.Code != c.code || (c.message != "" && res.Error.Message != c.message) {
			t.Errorf("%s:\n got %s: %s\nwant %s: %s", c.url, res.Error.Code, res.Error.Message, c.code, c.message)
		}
		if strings.Contains(res.Error.Message, "pgconn") || strings.Contains(res.Error.Message, "dial tcp") || strings.Contains(res.Error.Message, "\n") {
			t.Errorf("%s: the message is a driver's, not a person's: %q", c.url, res.Error.Message)
		}
	}

	// And what a good one says.
	res := m.Test(ctx, TestInput{URL: pg.URL()})
	if !res.OK || res.Version != "17.0" || res.Remote || res.Database != "acme_dev" || res.User != "acme" ||
		!res.Capture.Available || res.Statements == nil || res.Statements.Available || res.Statements.Reason != "not_loaded" {
		t.Errorf("a good Postgres: %+v (statements %+v)", res, res.Statements)
	}
	res = m.Test(ctx, TestInput{URL: rd.URL()})
	if !res.OK || res.Version != "7.4.0" || res.Remote || res.Statements != nil {
		t.Errorf("a good Redis: %+v", res)
	}
	if n := len(m.Connections()); n != 0 {
		t.Errorf("testing saved %d connections", n)
	}
}

func TestPostgresStats(t *testing.T) {
	pg := fake.NewPostgres(t)
	m := open(t, "", nil)
	ctx := context.Background()
	a := add(t, m, ConnectionInput{URL: ptr(pg.URL())})

	read := func() *PostgresStats {
		t.Helper()
		out, err := m.Stats(ctx, a.ID, true)
		if err != nil {
			t.Fatal(err)
		}
		return out.(*PostgresStats)
	}
	st := read()
	if st.Version != "17.0" || st.Database != "acme_dev" || st.SizeBytes != 26121907 || !st.ReadOnly || st.Remote {
		t.Errorf("overview = %+v", st)
	}
	if c := st.Connections; c.Used != 7 || c.Max != 100 || c.Active != 2 || c.IdleInTransaction != 1 || c.Waiting != 1 {
		t.Errorf("connections = %+v", c)
	}
	if len(st.Tables) != 3 || st.Tables[0].Name != "orders" || st.Tables[0].Rows != 120000 || st.Tables[0].TotalBytes != 10772480 || st.TableCount != 3 {
		t.Errorf("tables = %+v", st.Tables)
	}
	if len(st.MissingIndexHints) != 1 || st.MissingIndexHints[0].RowsPerScan != 120000 ||
		st.MissingIndexHints[0].Why != "orders was read from end to end 27 times, about 120,000 rows each time, against 0 lookups by index." {
		t.Errorf("hints = %+v", st.MissingIndexHints)
	}
	if ix := st.Indexes; ix.Count != 5 || ix.UnusedCount != 2 || len(ix.Largest) != 3 || ix.Largest[0].Unused || !ix.Largest[1].Unused {
		t.Errorf("indexes = %+v", ix)
	}
	if len(st.Running) != 2 || st.Running[0].State != "idle in transaction" || st.Running[0].Waiting != "" || st.Running[1].Waiting != "Lock: transactionid" {
		t.Errorf("running = %+v", st.Running)
	}
	if len(st.Locks) != 1 || st.Locks[0].PID != 812 || len(st.Locks[0].BlockedBy) != 1 || st.Locks[0].BlockedBy[0] != 790 {
		t.Errorf("locks = %+v", st.Locks)
	}
	if len(st.Incomplete) != 0 {
		t.Errorf("incomplete = %+v", st.Incomplete)
	}

	for version, old := range map[string]bool{"1.7": true, "1.8": false, "1.11": false, "1.10": false, "2.0": false, "0.9": true, "": false} {
		if olderThan(version, 1, 8) != old {
			t.Errorf("olderThan(%q, 1, 8) = %v", version, !old)
		}
	}
	if got := plural(1, "lookup", "lookups") + ", " + plural(1200, "time", "times"); got != "1 lookup, 1,200 times" {
		t.Errorf("plural: %s", got)
	}

	// pg_stat_statements: each way of not being there says what to do.
	for state, want := range map[string]struct {
		reason string
		steps  int
	}{"": {"not_loaded", 3}, "loaded": {"not_installed", 1}, "created": {"not_loaded", 2}} {
		pg.SetStatements(state)
		s := read().Statements
		if s.Available || s.Reason != want.reason || len(s.HowTo) != want.steps || s.Message == "" || len(s.Statements) != 0 {
			t.Errorf("pg_stat_statements %q: %+v", state, s)
		}
	}
	pg.SetStatements("ok")
	s := read().Statements
	if !s.Available || s.Version != "1.11" || len(s.Statements) != 2 || s.Statements[0].Calls != 1204 || s.Statements[0].TotalMs != 9321.5 ||
		s.Statements[0].CacheHitRate == nil || *s.Statements[0].CacheHitRate != 0.9 || s.Statements[1].CacheHitRate != nil {
		t.Errorf("pg_stat_statements = %+v", s)
	}

	// Never a write: every statement is a read inside a read-only
	// transaction with its own limits, and the session opened read-only.
	inTx := false
	for _, sql := range pg.Ran() {
		word := strings.ToUpper(strings.Fields(sql)[0])
		switch {
		case sql == "BEGIN READ ONLY":
			inTx = true
		case sql == "ROLLBACK":
			inTx = false
		case word == "SET" && strings.HasPrefix(sql, "SET LOCAL ") && inTx:
		case word == "SELECT" && inTx:
		default:
			t.Errorf("Leadline sent %q (in a read-only transaction: %v)", sql, inTx)
		}
	}
	for _, params := range pg.Startups() {
		if params["default_transaction_read_only"] != "on" || params["statement_timeout"] != "3000" || params["application_name"] != "leadline" {
			t.Errorf("session opened with %v", params)
		}
	}
	if len(pg.Startups()) != 1 {
		t.Errorf("%d connections for the stats, want the one kept open", len(pg.Startups()))
	}
}

// A pooler refuses startup parameters it doesn't know. Leadline still
// connects, and still only reads.
func TestPostgresThroughAPooler(t *testing.T) {
	pg := fake.NewPostgres(t)
	pg.SetPooler(true)
	m := open(t, "", nil)
	res := m.Test(context.Background(), TestInput{URL: pg.URL()})
	if !res.OK {
		t.Fatalf("through a pooler: %+v", res.Error)
	}
	for _, sql := range pg.Ran() {
		if strings.HasPrefix(sql, "SET ") && !strings.HasPrefix(sql, "SET LOCAL ") {
			t.Errorf("%q would stay on a pooled server connection that the app gets next", sql)
		}
	}
	if ran := strings.Join(pg.Ran(), "; "); !strings.Contains(ran, "BEGIN READ ONLY; SET LOCAL statement_timeout = 3000") {
		t.Errorf("reads were not guarded: %s", ran)
	}
}

func TestPostgresStatsOutlivesARestartOfTheDatabase(t *testing.T) {
	pg := fake.NewPostgres(t)
	m := open(t, "", nil)
	ctx := context.Background()
	a := add(t, m, ConnectionInput{URL: ptr(pg.URL())})
	if _, err := m.Stats(ctx, a.ID, true); err != nil {
		t.Fatal(err)
	}
	// The kept connection dies; the next read opens another.
	m.mu.Lock()
	cl := m.conns[0].cl
	m.mu.Unlock()
	cl.mu.Lock()
	cl.pg.Conn().Close()
	cl.mu.Unlock()
	if _, err := m.Stats(ctx, a.ID, true); err != nil {
		t.Fatalf("after the connection died: %v", err)
	}

	pg.Close()
	cl.close()
	m.mu.Lock()
	m.conns[0].cl = nil
	m.mu.Unlock()
	_, err := m.Stats(ctx, a.ID, true)
	if p := AsProblem(err); p.Code != "unreachable" {
		t.Fatalf("with the database down: %v", err)
	}
	if v, _ := m.Connection(a.ID); v.Health.State != "error" || v.Health.Code != "unreachable" || v.Health.Hint == "" {
		t.Errorf("health with the database down: %+v", v.Health)
	}
}

func TestRedisStats(t *testing.T) {
	rd := fake.NewRedis(t)
	m := open(t, "", nil)
	ctx := context.Background()
	a := add(t, m, ConnectionInput{URL: ptr(rd.URL()), Name: ptr("acme-cache")})
	out, err := m.Stats(ctx, a.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	st := out.(*RedisStats)
	if st.Version != "7.4.0" || st.Memory.UsedBytes != 1145744 || st.Clients.Connected != 3 || st.Clients.Max != 10000 || st.OpsPerSec != 12 {
		t.Errorf("overview = %+v", st)
	}
	if k := st.Keyspace; k.Hits != 900 || k.Misses != 100 || k.HitRate == nil || *k.HitRate != 0.9 || k.Keys != 4 {
		t.Errorf("keyspace = %+v", k)
	}
	if len(st.Databases) != 1 || st.Databases[0].Keys != 4 || st.Databases[0].Expiring != 2 {
		t.Errorf("databases = %+v", st.Databases)
	}
	if len(st.Commands) != 3 || st.Commands[0].Command != "GET" || st.Commands[0].AvgMs != 0.004 || st.Commands[2].Command != "CONFIG GET" {
		t.Errorf("commands = %+v", st.Commands)
	}
	sl := st.SlowLog
	if sl.ThresholdMs == nil || *sl.ThresholdMs != 10 || len(sl.Entries) != 2 || sl.Entries[0].Command != "KEYS user:*" ||
		sl.Entries[0].DurationMs != 15.234 || sl.Entries[0].ClientName != "acme-web" || sl.Entries[1].Command != "AUTH ?" {
		t.Errorf("slow log = %+v", sl)
	}
	b := st.BiggestKeys
	if !b.Complete || b.Sampled != 4 || b.Total != 4 || len(b.Keys) != 4 || b.Keys[0].Key != "big:blob" || b.Keys[0].Bytes != 229432 || b.Keys[0].Type != "string" {
		t.Errorf("biggest keys = %+v", b)
	}
	mustNotHold(t, "redis stats", st, rd.Password)
}

// The key sample is bounded however large the keyspace, and KEYS is never
// sent.
func TestRedisKeySampleIsBounded(t *testing.T) {
	rd := fake.NewRedis(t)
	keys := map[string]int64{}
	for i := range 25_000 {
		keys[fmt.Sprintf("session:%05d", i)] = int64(100 + i)
	}
	rd.SetKeys(keys)
	m := open(t, "", nil)
	a := add(t, m, ConnectionInput{URL: ptr(rd.URL())})
	out, err := m.Stats(context.Background(), a.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	b := out.(*RedisStats).BiggestKeys
	if b.Complete || b.Sampled < sampleKeys || b.Sampled > sampleKeys+200 || b.Total != 25_000 || len(b.Keys) != topKeys {
		t.Errorf("sample = %+v", b)
	}
	if !strings.Contains(b.Note, "A sample") {
		t.Errorf("note = %q", b.Note)
	}
	scans := 0
	for _, cmd := range rd.Ran() {
		switch strings.ToUpper(cmd[0]) {
		case "KEYS", "FLUSHDB", "FLUSHALL", "DEL", "SET", "DEBUG":
			t.Errorf("Leadline sent %v", cmd)
		case "SCAN":
			scans++
		}
	}
	if scans < 5 || scans > 7 {
		t.Errorf("%d SCANs for a sample of %d", scans, sampleKeys)
	}
}

// `leadline run` codes against exactly this.
func TestRunProxyContract(t *testing.T) {
	pg := fake.NewPostgres(t)
	st := openStore(t)
	dir := t.TempDir()
	m := open(t, dir, st)

	p, err := m.OpenProxy("postgres", pg.Addr(), "acme-web")
	if err != nil {
		t.Fatal(err)
	}
	if p.ID == "" || p.Kind != "postgres" || p.Upstream != pg.Addr() || !strings.HasPrefix(p.Listen, "127.0.0.1:") || !answering(p.Listen) {
		t.Fatalf("proxy = %+v", p)
	}
	raw, _ := json.Marshal(p)
	var fields map[string]string
	json.Unmarshal(raw, &fields)
	if len(fields) != 4 || fields["id"] != p.ID || fields["kind"] != "postgres" || fields["listen"] != p.Listen || fields["upstream"] != pg.Addr() {
		t.Errorf("the JSON is %s, want exactly id, kind, listen, upstream", raw)
	}

	// The same upstream for the same service, however the address is
	// spelled, is the same proxy.
	again, err := m.OpenProxy("postgres", "localhost:"+portOf(pg.Addr()), " acme-web ")
	if err != nil || again != p {
		t.Fatalf("asking again: %+v, %v; want %+v", again, err, p)
	}
	query(t, p.Listen, pg.Password, "SELECT count(*) FROM orders WHERE user_id = 42")
	eventually(t, "the statement", func() bool { return m.Proxies()[0].Statements == 1 })
	if v := m.Proxies()[0]; v.Label != "acme-web" || len(v.Origins) != 1 || v.Origins[0] != "run" {
		t.Errorf("proxy view = %+v", v)
	}

	// Another service gets a proxy of its own: a proxy files what it
	// records under the service it was opened for.
	other, err := m.OpenProxy("postgres", pg.Addr(), "acme-worker")
	if err != nil || other.ID == p.ID || other.Listen == p.Listen || other.Upstream != p.Upstream {
		t.Fatalf("a second service's proxy: %+v, %v; want one apart from %+v", other, err, p)
	}
	if closed, err := m.CloseProxy(other.ID); err != nil || !closed || answering(other.Listen) || !answering(p.Listen) {
		t.Errorf("closing the second service's proxy: closed=%v err=%v", closed, err)
	}

	// Not saved: nothing on disk, and nothing after a restart.
	if _, err := os.Stat(filepath.Join(dir, fileName)); !os.IsNotExist(err) {
		t.Errorf("a run proxy wrote the connections file (%v)", err)
	}

	// Two runs share it, so it closes with the second.
	if closed, err := m.CloseProxy(p.ID); err != nil || closed || !answering(p.Listen) {
		t.Errorf("first close: closed=%v err=%v", closed, err)
	}
	if closed, err := m.CloseProxy(p.ID); err != nil || !closed || answering(p.Listen) {
		t.Errorf("second close: closed=%v err=%v", closed, err)
	}
	if _, err := m.CloseProxy(p.ID); AsProblem(err).Code != "not_found" {
		t.Errorf("closing a closed proxy: %v", err)
	}

	for _, bad := range [][2]string{{"redis", "127.0.0.1:6379"}, {"postgres", ""}, {"postgres", "postgres://acme@localhost/db"}} {
		if _, err := m.OpenProxy(bad[0], bad[1], ""); AsProblem(err).Code != "bad_request" {
			t.Errorf("OpenProxy(%q, %q): %v", bad[0], bad[1], err)
		}
	}

	// With no label there is no service to file under, and a saved
	// connection's proxy for the upstream is the one given. Giving it back
	// does not close it.
	c := add(t, m, ConnectionInput{URL: ptr(pg.URL()), Capture: ptr(true)})
	shared, err := m.OpenProxy("postgres", pg.Addr(), "")
	if err != nil || shared.Listen != c.Proxy.Listen || shared.ID != c.Proxy.ID {
		t.Fatalf("run proxy for a captured connection: %+v, %v; want %s", shared, err, c.Proxy.Listen)
	}
	if closed, _ := m.CloseProxy(shared.ID); closed || !answering(shared.Listen) {
		t.Errorf("a run closed a saved connection's proxy")
	}
	// A run with a name gets its own even then.
	named, err := m.OpenProxy("postgres", pg.Addr(), "acme-web")
	if err != nil || named.ID == c.Proxy.ID {
		t.Fatalf("a named run beside a captured connection: %+v, %v", named, err)
	}
	m.Close()
	if answering(shared.Listen) {
		t.Errorf("proxies outlived the manager")
	}
}

// The -pg flag declares proxies; they show with the rest.
func TestDeclaredProxies(t *testing.T) {
	pg := fake.NewPostgres(t)
	st := openStore(t)
	m := open(t, "", st)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	listen := ln.Addr().String()
	ln.Close()
	routes, err := pgproxy.ParseRoutes(listen + "=" + pg.Addr())
	if err != nil {
		t.Fatal(err)
	}
	m.Declare(routes)
	ps := m.Proxies()
	if len(ps) != 1 || ps[0].Listen != listen || ps[0].State != "listening" || len(ps[0].Origins) != 1 || ps[0].Origins[0] != "flag" {
		t.Fatalf("declared proxy = %+v", ps)
	}
	query(t, listen, pg.Password, "SELECT 1")
	eventually(t, "the statement", func() bool { return m.Proxies()[0].Statements == 1 })
}
