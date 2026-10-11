package integrations

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean-brydon/leadline/internal/integrations/connurl"
	"github.com/sean-brydon/leadline/internal/integrations/fake"
)

// Prisma's setup writes `?schema=public` into every DATABASE_URL, and
// its docs add `connection_limit` and friends. Postgres refuses each as a
// setting it does not have. A URL pasted by hand must test, save, edit
// and reconnect all the same: Leadline connects without them, and what
// it shows and hands back for the app's .env still has them, because the
// app needs them.
func TestAPrismaURLConnects(t *testing.T) {
	pg := fake.NewPostgres(t)
	st := openStore(t)
	dir := t.TempDir()
	ctx := context.Background()
	typed := pg.URL() + "&schema=public&connection_limit=5&pgbouncer=true"

	m := open(t, dir, st)
	res := m.Test(ctx, TestInput{URL: typed})
	if !res.OK || res.Database != pg.Database {
		t.Fatalf("testing a Prisma URL: %+v %+v", res, res.Error)
	}
	if !strings.Contains(res.URL, "schema=public") || strings.Contains(res.URL, pg.Password) {
		t.Errorf("the test shows %q, not the URL as typed with its password masked", res.URL)
	}

	a := add(t, m, ConnectionInput{URL: &typed, Capture: ptr(true)})
	if a.Health.State != "ok" {
		t.Fatalf("saved, and its health is %+v", a.Health)
	}
	// Shown as typed, and the line for the app's .env keeps what the ORM
	// reads from it.
	for _, want := range []string{"schema=public", "connection_limit=5", "pgbouncer=true"} {
		if !strings.Contains(a.URL, want) {
			t.Errorf("url = %q, without %s", a.URL, want)
		}
		if a.Proxy == nil || !strings.Contains(a.Proxy.URL, want) {
			t.Errorf("proxy = %+v, without %s", a.Proxy, want)
		}
	}
	if a.Proxy != nil && !strings.Contains(a.Proxy.URL, "sslmode=disable") {
		t.Errorf("proxy url = %q", a.Proxy.URL)
	}
	// And kept as typed: the file is what the proxy's URL is made from.
	raw, _ := os.ReadFile(filepath.Join(dir, fileName))
	if !strings.Contains(string(raw), "schema=public") {
		t.Errorf("the saved URL lost its parameters: %s", raw)
	}

	// An edit that sends the masked URL back with one more of them.
	edited := a.URL + "&pool_timeout=10"
	got, err := m.Update(ctx, a.ID, ConnectionInput{URL: &edited})
	if err != nil || got.Health.State != "ok" || !strings.Contains(got.URL, "pool_timeout=10") {
		t.Fatalf("after the edit: %+v, %v", got, err)
	}
	if res := m.Test(ctx, TestInput{ID: a.ID}); !res.OK {
		t.Errorf("testing the saved connection: %+v", res.Error)
	}
	if _, err := m.Stats(ctx, a.ID, true); err != nil {
		t.Errorf("its stats: %v", err)
	}

	// A daemon that starts again reads the file and connects the same way.
	m.Close()
	m2 := open(t, dir, st)
	again := m2.Test(ctx, TestInput{ID: a.ID})
	if !again.OK {
		t.Errorf("after a restart: %+v", again.Error)
	}

	// None of it reached the server, in any connection Leadline made.
	if len(pg.Startups()) < 4 {
		t.Fatalf("the server saw %d connections", len(pg.Startups()))
	}
	for _, start := range pg.Startups() {
		for _, name := range connurl.ORMParams() {
			if _, sent := start[name]; sent {
				t.Errorf("%s was sent to Postgres: %v", name, start)
			}
		}
	}

	// A parameter that is nobody's is still sent, and the server's
	// refusal is still what the person reads.
	odd := m2.Test(ctx, TestInput{URL: pg.URL() + "&acme_setting=1"})
	if odd.OK || odd.Error == nil || !strings.Contains(odd.Error.Message, "acme_setting") {
		t.Errorf("a setting Postgres does not have: %+v %+v", odd, odd.Error)
	}
	mustNotHold(t, "a Prisma URL's answers", []any{res, a, got, again, odd}, pg.Password)
}

// A Redis URL has no ORM parameters to leave out: it is dialled as given.
func TestOnlyPostgresURLsAreChanged(t *testing.T) {
	rd, err := parseURL("", "redis://localhost:6379/2?schema=public")
	if err != nil || rd.dial != rd.raw {
		t.Errorf("redis: %+v, %v", rd, err)
	}
	pg, err := parseURL("", "postgres://acme:pw@localhost:5432/acme?schema=public&application_name=acme")
	if err != nil || pg.dial != "postgres://acme:pw@localhost:5432/acme?application_name=acme" || !strings.HasSuffix(pg.raw, "application_name=acme") || !strings.Contains(pg.raw, "schema=public") {
		t.Errorf("postgres: %+v, %v", pg, err)
	}
	if got := pg.proxyURL(15432); got != "postgres://acme:****@127.0.0.1:15432/acme?application_name=acme&schema=public&sslmode=disable" {
		t.Errorf("proxy url = %q", got)
	}
}

// A connection saved from a scan says so, and where its URL was found:
// on the connection, on the proxy opened for it, in the file, and after
// a restart.
func TestAScanSavedConnectionSaysWhereItWasFound(t *testing.T) {
	pg, rd := fake.NewPostgres(t), fake.NewRedis(t)
	st := openStore(t)
	dir := t.TempDir()

	m := open(t, dir, st)
	owner := ScanOwner("acme-web", "/Users/acme/code/acme-web", "main", "DATABASE_URL", ".env.local")
	found := add(t, m, ConnectionInput{URL: ptr(pg.URL()), Capture: ptr(true), Owner: &owner})
	want := Owner{Kind: OwnerScan, Project: "acme-web", Path: "/Users/acme/code/acme-web", Branch: "main",
		EnvKey: "DATABASE_URL", EnvFile: ".env.local", Label: "found by a scan, DATABASE_URL in acme-web"}
	if found.Owner != want {
		t.Errorf("owner = %+v\n  want  %+v", found.Owner, want)
	}
	if found.Proxy == nil || found.Proxy.Owner != want {
		t.Errorf("its proxy's owner = %+v", found.Proxy)
	}
	// `env` is a worktree's: a scan's connection keeps it empty.
	if len(found.Env) != 0 {
		t.Errorf("env = %v", found.Env)
	}

	// One typed in is a person's, as before.
	typed := add(t, m, ConnectionInput{URL: ptr(rd.URL())})
	if typed.Owner != (Owner{Kind: OwnerUser, Label: "added by hand"}) {
		t.Errorf("a typed connection's owner = %+v", typed.Owner)
	}
	// Only a scan's owner is taken: nothing else can pass for a worktree's.
	claimed := Owner{Kind: OwnerWorktree, Service: "acme/fix-orders"}
	pg2 := fake.NewPostgres(t)
	other := add(t, m, ConnectionInput{URL: ptr(pg2.URL()), Owner: &claimed})
	if other.Owner.Kind != OwnerUser {
		t.Errorf("a claimed owner was kept: %+v", other.Owner)
	}
	// And not from a request's body at all.
	var in ConnectionInput
	if err := json.Unmarshal([]byte(`{"url":"redis://localhost:6379","owner":{"kind":"scan","project":"acme"}}`), &in); err != nil || in.Owner != nil {
		t.Errorf("a request's body set the owner: %+v, %v", in.Owner, err)
	}

	// The file has the owner without its label, and nothing else of it.
	raw, _ := os.ReadFile(filepath.Join(dir, fileName))
	if !strings.Contains(string(raw), `"env_key": "DATABASE_URL"`) || strings.Contains(string(raw), "found by a scan") {
		t.Errorf("the file: %s", raw)
	}
	// It is a person's connection in every other way: it counts as one,
	// and stays when a worktree is detached.
	m.mu.Lock()
	people, worktrees := m.countLocked(false), m.countLocked(true)
	m.mu.Unlock()
	if people != 3 || worktrees != 0 {
		t.Errorf("counted %d of people's and %d of worktrees'", people, worktrees)
	}

	m.Close()
	m2 := open(t, dir, st)
	back, err := m2.Connection(found.ID)
	if err != nil || back.Owner != want {
		t.Errorf("after a restart: %+v, %v", back.Owner, err)
	}
	// A second database on the same server, typed by hand, shares the
	// proxy. The proxy is then two people's connections, not one find.
	pg.AddDatabase("acme_test")
	second := strings.Replace(pg.URL(), "/"+pg.Database+"?", "/acme_test?", 1)
	shared := add(t, m2, ConnectionInput{URL: &second, Capture: ptr(true)})
	if shared.Proxy == nil || shared.Proxy.Port != found.Proxy.Port || shared.Proxy.Owner != (Owner{Kind: OwnerUser, Label: "added by hand"}) {
		t.Errorf("a shared proxy = %+v", shared.Proxy)
	}
}

func TestScanOwnerLabels(t *testing.T) {
	cases := map[Owner]string{
		ScanOwner("acme-web", "/x", "", "DATABASE_URL", ".env"): "found by a scan, DATABASE_URL in acme-web",
		ScanOwner("acme-web", "", "", "", ""):                   "found by a scan, in acme-web",
		ScanOwner("", "", "", "REDIS_URL", ""):                  "found by a scan, REDIS_URL",
		ScanOwner("", "", "", "", ""):                           "found by a scan",
	}
	for o, want := range cases {
		if got := o.labelled().Label; got != want {
			t.Errorf("%+v is labelled %q, want %q", o, got, want)
		}
	}
	// What comes in is names, kept short and on one line.
	long := ScanOwner(strings.Repeat("a", 400), "/x", "", "KEY\nINJECTED", "")
	if len(long.Project) > 140 || strings.Contains(long.EnvKey, "\n") {
		t.Errorf("not cleaned: %+v", long)
	}
}
