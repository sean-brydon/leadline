package api

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sean-brydon/leadline/internal/integrations"
	"github.com/sean-brydon/leadline/internal/integrations/fake"
	"github.com/sean-brydon/leadline/internal/store"
)

// client talks to a test daemon and keeps every body it was sent, so a
// test can check afterwards that none of them had a password in it.
type client struct {
	t      *testing.T
	base   string
	bodies []string
}

func newIntegrationsClient(t *testing.T) *client {
	t.Helper()
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	// The store's manager, opened here to give it ports of its own: the
	// usual range is one every test binary would share.
	first, last := fake.Ports(t, 8)
	m, err := integrations.Open(integrations.Options{Store: st, ProxyPorts: integrations.PortRange{First: first, Last: last}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(New(st, m).Handler())
	t.Cleanup(func() {
		ts.Close()
		m.Close()
		st.Close()
	})
	return &client{t: t, base: ts.URL}
}

// do sends a request and decodes the answer into `into` (when not nil).
func (c *client) do(method, path, body string, into any) int {
	c.t.Helper()
	req, err := http.NewRequest(method, c.base+path, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	c.bodies = append(c.bodies, method+" "+path+": "+string(raw))
	if into != nil {
		if err := json.Unmarshal(raw, into); err != nil {
			c.t.Fatalf("%s %s: %v in %s", method, path, err, raw)
		}
	}
	return resp.StatusCode
}

func (c *client) ok(method, path, body string, into any) {
	c.t.Helper()
	if status := c.do(method, path, body, into); status != http.StatusOK {
		c.t.Fatalf("%s %s: %d: %s", method, path, status, c.bodies[len(c.bodies)-1])
	}
}

// fails expects an error a person can act on, with this status and code.
func (c *client) fails(method, path, body string, status int, code string) integrations.Problem {
	c.t.Helper()
	var out struct {
		Error integrations.Problem `json:"error"`
	}
	got := c.do(method, path, body, &out)
	if got != status || out.Error.Code != code || out.Error.Message == "" {
		c.t.Errorf("%s %s: %d %+v, want %d %s", method, path, got, out.Error, status, code)
	}
	return out.Error
}

func jsonBody(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestConnectionsAPI(t *testing.T) {
	pg, rd := fake.NewPostgres(t), fake.NewRedis(t)
	pg.SetStatements("ok")
	c := newIntegrationsClient(t)

	// Nothing yet, and the lists are lists, not null.
	var src map[string]json.RawMessage
	c.ok("GET", "/api/sources", "", &src)
	for _, key := range []string{"services", "proxies", "connections"} {
		if string(src[key]) != "[]" {
			t.Errorf("sources.%s = %s, want []", key, src[key])
		}
	}

	// Test before saving.
	var tested integrations.TestResult
	c.ok("POST", "/api/connections/test", jsonBody(map[string]any{"url": pg.URL()}), &tested)
	if !tested.OK || tested.Version != "17.0" || tested.Remote || !tested.Statements.Available {
		t.Errorf("test = %+v", tested)
	}
	c.ok("POST", "/api/connections/test", jsonBody(map[string]any{"url": strings.Replace(pg.URL(), pg.Password, "not-the-password", 1)}), &tested)
	if tested.OK || tested.Error.Code != "auth_failed" || tested.Error.Message != `Password authentication failed for user "acme".` {
		t.Errorf("test with a wrong password = %+v", tested.Error)
	}

	var a, b integrations.ConnectionView
	c.ok("POST", "/api/connections", jsonBody(map[string]any{"url": pg.URL(), "name": "Acme dev", "capture": true}), &a)
	c.ok("POST", "/api/connections", jsonBody(map[string]any{"url": rd.URL(), "name": "acme-cache"}), &b)
	if a.Proxy == nil || a.Proxy.State != "listening" || !strings.Contains(a.Proxy.URL, ":****@127.0.0.1:") || a.Health.State != "ok" {
		t.Fatalf("added = %+v", a)
	}
	if b.Kind != "redis" || b.Capture || b.Monitor != nil || b.Health.State != "ok" {
		t.Fatalf("added = %+v", b)
	}

	var list struct {
		Connections []integrations.ConnectionView `json:"connections"`
	}
	c.ok("GET", "/api/connections", "", &list)
	if len(list.Connections) != 2 || list.Connections[0].ID != a.ID || list.Connections[1].ID != b.ID {
		t.Errorf("list = %+v", list)
	}
	var one integrations.ConnectionView
	c.ok("GET", "/api/connections/"+a.ID, "", &one)
	if one.ID != a.ID || one.Name != "Acme dev" {
		t.Errorf("get = %+v", one)
	}

	var pgStats integrations.PostgresStats
	c.ok("GET", "/api/connections/"+a.ID+"/stats", "", &pgStats)
	if pgStats.Kind != "postgres" || len(pgStats.Tables) != 3 || len(pgStats.MissingIndexHints) != 1 || !pgStats.Statements.Available {
		t.Errorf("postgres stats = %+v", pgStats)
	}
	var rdStats integrations.RedisStats
	c.ok("GET", "/api/connections/"+b.ID+"/stats?fresh=1", "", &rdStats)
	if rdStats.Kind != "redis" || rdStats.Keyspace.Keys != 4 || len(rdStats.BiggestKeys.Keys) != 4 {
		t.Errorf("redis stats = %+v", rdStats)
	}
	c.ok("POST", "/api/connections/"+a.ID+"/test", "", &tested)
	if !tested.OK {
		t.Errorf("test of a saved connection = %+v", tested.Error)
	}

	// Edit, both ways: PATCH, and the POST a page on another origin uses.
	c.ok("PATCH", "/api/connections/"+a.ID, `{"name":"Acme (dev)"}`, &one)
	if one.Name != "Acme (dev)" || one.Proxy == nil || one.Proxy.ID != a.Proxy.ID {
		t.Errorf("after PATCH: %+v", one)
	}
	c.ok("POST", "/api/connections/"+b.ID, `{"capture":true}`, &one)
	if !one.Capture || one.Monitor == nil {
		t.Errorf("after turning capture on: %+v", one)
	}
	rd.WaitMonitor(t)

	// A query through the proxy shows in sources within a poll or two.
	query(t, a.Proxy.Listen, pg.Password)
	var sources integrations.Sources
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.ok("GET", "/api/sources", "", &sources)
		if len(sources.Services) == 1 && len(sources.Proxies) == 1 && sources.Proxies[0].Statements == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sources never showed the query: %s", c.bodies[len(c.bodies)-1])
		}
		time.Sleep(20 * time.Millisecond)
	}
	if s := sources.Services[0]; s.Name != "acme-web" || s.Spans != 1 || !reflect.DeepEqual(s.Via, []string{"postgres-proxy"}) {
		t.Errorf("service = %+v", s)
	}
	if len(sources.Connections) != 2 || sources.Connections[0].Health.State != "ok" {
		t.Errorf("connections in sources = %+v", sources.Connections)
	}

	// Errors say what to do, as JSON, with a status that fits.
	c.fails("POST", "/api/connections", `{"url":"mysql://acme@localhost/acme"}`, 400, "bad_url")
	c.fails("POST", "/api/connections", `{"name":"no url"}`, 400, "bad_url")
	c.fails("POST", "/api/connections", `{not json`, 400, "bad_request")
	c.fails("POST", "/api/connections", `{"url":"postgres://acme:pw@db.acme.example/acme_prod","capture":true}`, 400, "capture_remote")
	c.fails("GET", "/api/connections/c_nope", "", 404, "not_found")
	c.fails("GET", "/api/connections/c_nope/stats", "", 404, "not_found")
	c.fails("POST", "/api/connections/c_nope/test", "", 404, "not_found")
	c.fails("PATCH", "/api/connections/c_nope", `{"name":"x"}`, 404, "not_found")
	c.fails("POST", "/api/connections/"+a.ID, jsonBody(map[string]any{"proxy_port": 80}), 400, "bad_request")

	// A database that has gone away: its stats are a 502, with why.
	gone := fake.NewPostgres(t)
	var g integrations.ConnectionView
	c.ok("POST", "/api/connections", jsonBody(map[string]any{"url": gone.URL(), "name": "gone"}), &g)
	gone.Close()
	c.ok("POST", "/api/connections/"+g.ID, jsonBody(map[string]any{"url": strings.Replace(gone.URL(), "acme_dev", "acme_test", 1)}), &g)
	if p := c.fails("GET", "/api/connections/"+g.ID+"/stats", "", 502, "unreachable"); !strings.HasPrefix(p.Message, "Nothing is listening at ") || p.Hint == "" {
		t.Errorf("stats of a stopped database: %+v", p)
	}

	// Delete, both ways.
	var ok struct {
		OK bool `json:"ok"`
	}
	c.ok("DELETE", "/api/connections/"+g.ID, "", &ok)
	c.ok("POST", "/api/connections/"+b.ID+"/delete", "", &ok)
	c.ok("POST", "/api/connections/"+a.ID+"/delete", "", &ok)
	c.fails("DELETE", "/api/connections/"+a.ID, "", 404, "not_found")
	c.ok("GET", "/api/connections", "", &list)
	if len(list.Connections) != 0 {
		t.Errorf("after deleting all: %+v", list)
	}
	if conn, err := net.DialTimeout("tcp", a.Proxy.Listen, 200*time.Millisecond); err == nil {
		conn.Close()
		t.Error("the proxy outlived its connection")
	}

	// No body, of any answer, has a password in it.
	for _, body := range c.bodies {
		for _, secret := range []string{pg.Password, rd.Password, "not-the-password"} {
			if strings.Contains(body, secret) {
				t.Errorf("a response contains a password:\n%s", body)
			}
		}
	}
	if len(c.bodies) < 30 {
		t.Errorf("only %d responses were checked", len(c.bodies))
	}
}

// query runs one statement through a proxy the way an app would.
func query(t *testing.T, addr, password string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pgconn.Connect(ctx, "postgres://acme:"+password+"@"+addr+"/acme_dev?sslmode=disable&application_name=acme-web")
	if err != nil {
		t.Fatalf("connect through %s: %v", addr, err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "SELECT count(*) FROM orders WHERE user_id = 42").ReadAll(); err != nil {
		t.Fatal(err)
	}
}

// The contract `leadline run` is written against.
func TestRunProxiesAPI(t *testing.T) {
	pg := fake.NewPostgres(t)
	c := newIntegrationsClient(t)

	var raw map[string]any
	c.ok("POST", "/api/proxies", jsonBody(map[string]any{"kind": "postgres", "upstream": pg.Addr(), "label": "acme-web"}), &raw)
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"id", "kind", "listen", "upstream"}) {
		t.Fatalf("POST /api/proxies answered %v, want exactly id, kind, listen, upstream", raw)
	}
	id, listen := raw["id"].(string), raw["listen"].(string)
	if raw["kind"] != "postgres" || raw["upstream"] != pg.Addr() || !strings.HasPrefix(listen, "127.0.0.1:") || id == "" {
		t.Errorf("proxy = %v", raw)
	}
	if conn, err := net.DialTimeout("tcp", listen, time.Second); err != nil {
		t.Fatalf("the proxy is not listening on %s: %v", listen, err)
	} else {
		conn.Close()
	}

	// The same upstream again is the same proxy.
	var again map[string]any
	c.ok("POST", "/api/proxies", jsonBody(map[string]any{"kind": "postgres", "upstream": pg.Addr(), "label": "acme-web"}), &again)
	if !reflect.DeepEqual(again, raw) {
		t.Errorf("asking again: %v, want %v", again, raw)
	}

	var list struct {
		Proxies []integrations.ProxyView `json:"proxies"`
	}
	c.ok("GET", "/api/proxies", "", &list)
	if len(list.Proxies) != 1 || list.Proxies[0].ID != id || list.Proxies[0].Label != "acme-web" {
		t.Errorf("proxies = %+v", list)
	}
	// Not a saved connection.
	var conns struct {
		Connections []integrations.ConnectionView `json:"connections"`
	}
	c.ok("GET", "/api/connections", "", &conns)
	if len(conns.Connections) != 0 {
		t.Errorf("a run proxy became a connection: %+v", conns)
	}

	// Asked for twice, so given back twice; then it is gone.
	var closed struct {
		OK     bool `json:"ok"`
		Closed bool `json:"closed"`
	}
	c.ok("DELETE", "/api/proxies/"+id, "", &closed)
	if !closed.OK || closed.Closed {
		t.Errorf("first DELETE = %+v", closed)
	}
	c.ok("DELETE", "/api/proxies/"+id, "", &closed)
	if !closed.OK || !closed.Closed {
		t.Errorf("second DELETE = %+v", closed)
	}
	if conn, err := net.DialTimeout("tcp", listen, 200*time.Millisecond); err == nil {
		conn.Close()
		t.Error("the proxy is still listening after DELETE")
	}
	c.fails("DELETE", "/api/proxies/"+id, "", 404, "not_found")
	c.fails("POST", "/api/proxies", `{"kind":"redis","upstream":"127.0.0.1:6379"}`, 400, "bad_request")
	c.fails("POST", "/api/proxies", `{"kind":"postgres","upstream":"db.acme.example:5432"}`, 400, "capture_remote")
}

// Everything here is behind guard: another site's page, or a name that
// was rebound to 127.0.0.1, gets nothing.
func TestIntegrationsAreLocalhostOnly(t *testing.T) {
	c := newIntegrationsClient(t)
	for _, r := range [][2]string{
		{"GET", "/api/sources"}, {"GET", "/api/connections"}, {"POST", "/api/connections"}, {"POST", "/api/connections/test"},
		{"GET", "/api/connections/c_1/stats"}, {"DELETE", "/api/connections/c_1"}, {"POST", "/api/proxies"}, {"DELETE", "/api/proxies/p_1"},
	} {
		req, _ := http.NewRequest(r[0], c.base+r[1], strings.NewReader(`{"url":"postgres://localhost/x"}`))
		req.Host = "leadline.attacker.example"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s with a foreign Host: %d, want 403", r[0], r[1], resp.StatusCode)
		}

		req, _ = http.NewRequest(r[0], c.base+r[1], strings.NewReader(`{"url":"postgres://localhost/x"}`))
		req.Header.Set("Origin", "https://attacker.example")
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s from a foreign Origin: %d, want 403", r[0], r[1], resp.StatusCode)
		}
	}
}
