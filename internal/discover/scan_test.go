package discover

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeConnections stands in for the integrations manager: it keeps what
// it was asked to save, URL and all, so a test can check the URL arrived
// as written.
type fakeConnections struct {
	mu    sync.Mutex
	saved []SavedConnection
	// What Add was called with, and where it was told each was found.
	added []addCall
	found []Found
	// refuse makes Add fail for a URL containing it.
	refuse string
	tested []string
}

type addCall struct {
	kind, url, name string
	capture         bool
}

func (c *fakeConnections) Saved() []SavedConnection {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]SavedConnection{}, c.saved...)
}

func (c *fakeConnections) Add(_ context.Context, kind, url string, name *string, capture bool, found Found) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refuse != "" && strings.Contains(url, c.refuse) {
		return nil, &Problem{Code: "capture_remote", Message: "A remote database can't be captured.", Hint: "Add it with capture off."}
	}
	call := addCall{kind: kind, url: url, capture: capture}
	if name != nil {
		call.name = *name
	}
	c.added = append(c.added, call)
	c.found = append(c.found, found)
	t := describe(kind, url)
	id := "c_" + strings.Repeat("0", 9) + string(rune('a'+len(c.saved)))
	c.saved = append(c.saved, SavedConnection{ID: id, Kind: kind, Host: t.addr(), Database: t.database, User: t.user})
	return map[string]any{"id": id, "kind": kind, "url": t.masked, "capture": capture}, nil
}

func (c *fakeConnections) SetCapture(_ context.Context, id string, capture bool) (any, error) {
	return map[string]any{"id": id, "capture": capture}, nil
}

func (c *fakeConnections) Test(_ context.Context, kind, url string) any {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tested = append(c.tested, url)
	return map[string]any{"ok": true, "kind": kind, "url": describe(kind, url).masked}
}

// secrets are every password, and every value that is not a connection
// URL, in the env files of acmeMachine. None may appear in anything the
// package gives out.
var secrets = []string{
	"PWLOCAL-7Hq2", "PWSHARED-x9", "PW%40ODD%2Fchars", "PW@ODD/chars", "PWMYSQL-1", "PWMONGO-2", "PWINTERP-3", "PWREDIS-4",
	"OTHER-stripe-key", "OTHER-session", "OTHER-private-key-body", "OTHER-token", "OTHER-dsn-key", "OTHER-flags",
	"CMDLINE-pw",
}

// acmeMachine is a made-up machine: a home directory with three projects,
// two of them with a dev server listening, a Postgres and a Redis, and a
// folder of projects to point a scan at.
func acmeMachine(t *testing.T) (s *Service, conns *fakeConnections, home string, rec *recorder) {
	t.Helper()
	base := tempDir(t)
	home = filepath.Join(base, "Users", "acme")
	code := filepath.Join(home, "code")
	writeFiles(t, home, map[string]string{
		// In the home directory itself: never read.
		".env":         "DATABASE_URL=postgres://home:OTHER-token@localhost:5432/home\n",
		"package.json": `{"name":"TRIPWIRE"}`,

		"code/acme/package.json":   `{"name":"acme","scripts":{"dev":"next dev"},"dependencies":{"next":"^16.0.1"}}`,
		"code/acme/pnpm-lock.yaml": "",
		"code/acme/.git/HEAD":      "ref: refs/heads/main\n",
		"code/acme/.env.local": strings.Join([]string{
			`DATABASE_URL="postgres://acme:PWLOCAL-7Hq2@localhost:5432/acme"`,
			`REDIS_URL=redis://:PWREDIS-4@localhost:6379`,
			`STRIPE_SECRET_KEY='OTHER-stripe-key'`,
			`SESSION_PASSWORD=OTHER-session`,
			`PRIVATE_KEY="-----BEGIN-----`,
			`OTHER-private-key-body`,
			`-----END-----"`,
			`export SENTRY_DSN=https://OTHER-dsn-key@o0.ingest.acme.test/42`,
		}, "\n"),
		"code/acme/.env": strings.Join([]string{
			`DATABASE_URL=postgres://acme:PWSHARED-x9@db.acme.test:5432/acme`,
			`FEATURE_FLAGS=OTHER-flags`,
		}, "\n"),

		"code/acme-admin/package.json": `{"name":"acme-admin","scripts":{"dev":"node server.js"},"dependencies":{"express":"^5.1.0"}}`,
		"code/acme-admin/.env": strings.Join([]string{
			// The same Postgres as acme's: one connection, found twice.
			`DATABASE_URL=postgres://acme:PWLOCAL-7Hq2@localhost:5432/acme`,
			// And once more as Prisma spells it: still the one.
			`PRISMA_URL="postgresql://acme:PWLOCAL-7Hq2@127.0.0.1:5432/acme?schema=public"`,
			`LEGACY_URL=mysql://root:PWMYSQL-1@127.0.0.1:3306/legacy`,
			`ADMIN_TOKEN=OTHER-token`,
		}, "\n"),

		"code/billing-service/go.mod":              "module github.com/acme/billing-service\n\ngo 1.27\n\nrequire go.opentelemetry.io/otel v1.38.0\n",
		"code/billing-service/cmd/billing/main.go": "package main\n",
		"code/billing-service/.env": strings.Join([]string{
			`ANALYTICS_URL=postgresql://reader:PW%40ODD%2Fchars@db.acme.test:5432/analytics?sslmode=require&schema=public`,
			`EVENTS_URL=mongodb+srv://events:PWMONGO-2@cluster0.acme.test/events`,
			`CACHE_URL=redis://default:${CACHE_PASSWORD}@cache.acme.test:6379/1`,
			`CACHE_PASSWORD=PWINTERP-3`,
		}, "\n"),
	})

	started := time.Unix(1791640000, 0)
	lo := net.ParseIP("127.0.0.1").To4()
	sys := fakeSystem{
		socks: []rawSocket{
			{pid: 4242, port: 3000, ip: lo},
			{pid: 4300, port: 3001, ip: lo},
			{pid: 611, port: 5432, ip: lo},
			// Redis in a container: the port, under the runtime's process.
			{pid: 800, port: 6379, ip: lo},
			{pid: 900, port: 7000, ip: lo},
		},
		procs: map[int]procInfo{
			4242: {pid: 4242, ppid: 1, name: "node", exe: "/usr/local/bin/node", cwd: filepath.Join(code, "acme"), started: started,
				args: []string{"node", "node_modules/.bin/next", "dev"}},
			// A plain node script: a dev server because of where it runs.
			4300: {pid: 4300, ppid: 1, name: "node", exe: "/usr/local/bin/node", cwd: filepath.Join(code, "acme-admin"), started: started,
				args: []string{"node", "server.js", "--db", "postgres://acme:CMDLINE-pw@localhost/acme"}},
			611: {pid: 611, ppid: 1, name: "postgres", exe: "/usr/local/bin/postgres", cwd: "/usr/local/var/postgres", started: started},
			800: {pid: 800, ppid: 1, name: "container-helper", exe: "/Applications/Containers.app/Contents/MacOS/container-helper", cwd: "/", started: started},
			// A node process in the home directory: no project.
			900: {pid: 900, ppid: 1, name: "node", exe: "/usr/local/bin/node", cwd: home, started: started, args: []string{"node", "tool.js"}},
		},
		limits: Limits{Method: "libproc", Complete: true},
	}
	conns = &fakeConnections{}
	rec = &recorder{}
	s = New(Options{Connections: conns, Home: home, Processes: Processes{sys: sys},
		Runs:     func() []Run { return []Run{{Cwd: filepath.Join(code, "billing-service"), Service: "billing"}} },
		Services: func(context.Context) []string { return []string{"acme", "billing"} },
		Berth:    func() string { return "" },
	})
	s.fs = recording(rec)
	s.self = 1 << 30
	s.now = func() time.Time { return started.Add(3 * time.Hour) }
	return s, conns, home, rec
}

func scanAndWait(t *testing.T, s *Service, folders ...string) Scan {
	t.Helper()
	if _, err := s.Start(ScanOptions{Folders: folders, Daemon: "http://127.0.0.1:4320"}); err != nil {
		t.Fatal(err)
	}
	scan, ok := s.Wait(context.Background(), 10*time.Second)
	if !ok || scan.State != "done" {
		t.Fatalf("scan did not finish: %+v", scan)
	}
	return scan
}

// withEnv reads the env files of every project of the latest scan: the
// second step, which a scan never takes by itself.
func withEnv(t *testing.T, s *Service) Scan {
	t.Helper()
	scan, ok := s.Scan()
	if !ok {
		t.Fatal("no scan")
	}
	var ids []string
	for _, p := range scan.Projects {
		ids = append(ids, p.ID)
	}
	out, err := s.ReadEnv(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func projectNamed(t *testing.T, scan Scan, name string) Project {
	t.Helper()
	for _, p := range scan.Projects {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("no project %q among %d", name, len(scan.Projects))
	return Project{}
}

func connectionTo(t *testing.T, scan Scan, kind, host string) Connection {
	t.Helper()
	for _, c := range scan.Connections {
		if c.Kind == kind && c.Host == host {
			return c
		}
	}
	t.Fatalf("no %s connection to %s among %+v", kind, host, scan.Connections)
	return Connection{}
}

func TestScanWithoutFoldersFindsWhatIsRunning(t *testing.T) {
	s, _, home, rec := acmeMachine(t)
	scan := scanAndWait(t, s)

	if len(scan.Projects) != 3 {
		t.Fatalf("projects = %d, want 3: acme and acme-admin from what listens, billing-service from its run", len(scan.Projects))
	}
	acme := projectNamed(t, scan, "acme")
	if strings.Join(acme.Sources, " ") != "listening" || acme.Label != "Next.js 16" || acme.DevCommand != "pnpm dev" ||
		len(acme.Running) != 1 || acme.Running[0].Port != 3000 || acme.Git == nil || acme.Git.Branch != "main" {
		t.Errorf("acme = %+v", acme)
	}
	// Leadline holds data under its name: it already sends.
	if !acme.Sends.Already || strings.Join(acme.Sends.Because, " ") != "service" || acme.Connect.Kind != "already" {
		t.Errorf("acme sends = %+v connect = %+v", acme.Sends, acme.Connect)
	}
	admin := projectNamed(t, scan, "acme-admin")
	if admin.Sends.Already || admin.Connect.Kind != "run-wrap" || admin.Connect.Command != "leadline run -- npm run dev" || len(admin.Running) != 1 {
		t.Errorf("acme-admin = %+v", admin)
	}
	billing := projectNamed(t, scan, "billing-service")
	if strings.Join(billing.Sources, " ") != "run" || strings.Join(billing.Sends.Because, " ") != "run service" || billing.Sends.Service != "billing" || len(billing.Running) != 0 {
		t.Errorf("billing-service = %+v", billing)
	}

	byPort := map[int]Listener{}
	for _, l := range scan.Listeners {
		byPort[l.Port] = l
	}
	if l := byPort[3000]; l.Kind != "dev-server" || l.What != "next dev" || l.ProjectID != acme.ID {
		t.Errorf("3000 = %+v", l)
	}
	// A runtime in a project's folder is that project's dev server.
	if l := byPort[3001]; l.Kind != "dev-server" || l.Basis != "cwd" || l.Framework != "express" || l.ProjectID != admin.ID {
		t.Errorf("3001 = %+v", l)
	}
	// A port that is only a number is a guess, until an env file's URL
	// that points at it says what it is.
	if l := byPort[6379]; l.Kind != "redis" || l.Basis != "port" {
		t.Errorf("6379 = %+v", l)
	}
	for _, l := range withEnv(t, s).Listeners {
		if l.Port == 6379 && (l.Kind != "redis" || l.What != "Redis" || l.Basis != "env-url") {
			t.Errorf("6379 after the env files were read = %+v", l)
		}
	}
	if l := byPort[5432]; l.Kind != "postgres" || l.Basis != "process" {
		t.Errorf("5432 = %+v", l)
	}
	// A runtime in the home directory is nobody's dev server.
	if l := byPort[7000]; l.Kind != "other" || l.ProjectID != "" {
		t.Errorf("7000 = %+v", l)
	}

	// Nothing in the home directory itself was looked at, though a
	// process runs there and it has a package.json and a .env.
	if out := rec.outside(filepath.Join(home, "code")); len(out) > 0 {
		t.Errorf("opened outside the projects:\n  %s", strings.Join(out, "\n  "))
	}
	for _, p := range scan.Projects {
		if p.Name == "TRIPWIRE" {
			t.Error("the home directory was taken for a project")
		}
	}
}

func TestScanConnections(t *testing.T) {
	s, _, home, _ := acmeMachine(t)
	scanAndWait(t, s, filepath.Join(home, "code"))
	scan := withEnv(t, s)

	if scan.Folders[0].State != "scanned" || scan.Folders[0].Projects != 3 || scan.Progress.Phase != "done" || scan.Progress.FoldersDone != 1 {
		t.Errorf("folders = %+v progress = %+v", scan.Folders, scan.Progress)
	}
	if got := strings.Join(projectNamed(t, scan, "acme").Sources, " "); got != "folder listening" {
		t.Errorf("acme sources = %s", got)
	}
	if want := (EnvTotals{FilesRead: 4, URLs: 9, OtherKeys: 7}); scan.Env != want {
		t.Errorf("env totals = %+v, want %+v", scan.Env, want)
	}
	// Nine URLs in four files; the same one in two files, and spelled
	// another way in a third place, is one.
	if len(scan.Connections) != 7 {
		t.Fatalf("connections = %d, want 7", len(scan.Connections))
	}

	pg := connectionTo(t, scan, "postgres", "localhost:5432")
	if pg.URL != "postgres://acme:****@localhost:5432/acme" || pg.Remote || !pg.Connectable || !pg.CaptureAvailable || !pg.Suggested || pg.Saved {
		t.Errorf("local postgres = %+v", pg)
	}
	if len(pg.FoundIn) != 3 || pg.FoundIn[0].Project != "acme" || pg.FoundIn[0].File != ".env.local" || pg.FoundIn[0].Key != "DATABASE_URL" ||
		pg.FoundIn[1].Project != "acme-admin" || pg.FoundIn[1].File != ".env" || pg.FoundIn[2].Key != "PRISMA_URL" {
		t.Errorf("found in = %+v", pg.FoundIn)
	}
	if pg.Listening == nil || pg.Listening.Port != 5432 || pg.Listening.PID != 611 {
		t.Errorf("listening = %+v", pg.Listening)
	}
	shared := connectionTo(t, scan, "postgres", "db.acme.test:5432")
	_ = shared
	var remotes []Connection
	for _, c := range scan.Connections {
		if c.Kind == "postgres" && c.Remote {
			remotes = append(remotes, c)
		}
	}
	if len(remotes) != 2 {
		t.Fatalf("remote postgres = %d, want 2", len(remotes))
	}
	for _, c := range remotes {
		if !c.Connectable || c.CaptureAvailable || c.Suggested || c.Listening != nil {
			t.Errorf("remote = %+v", c)
		}
		// .env.local outranks .env for DATABASE_URL in acme.
		if c.Database == "acme" && c.FoundIn[0].Effective {
			t.Errorf("the .env value is shadowed, and says effective: %+v", c.FoundIn)
		}
	}
	redis := connectionTo(t, scan, "redis", "localhost:6379")
	if redis.URL != "redis://:****@localhost:6379" || !redis.Suggested || redis.Listening == nil {
		t.Errorf("redis = %+v", redis)
	}
	for _, c := range scan.Connections {
		switch c.Kind {
		case "mysql", "mongodb":
			if c.Connectable || c.Reason != "not_supported" || c.Note == "" || c.Suggested || c.CaptureAvailable {
				t.Errorf("%s = %+v", c.Kind, c)
			}
		}
		if c.Reason == "interpolated" && (c.Connectable || !c.Remote || c.URL != "redis://default:****@cache.acme.test:6379/1") {
			t.Errorf("interpolated = %+v", c)
		}
	}
	// What can be connected here comes first.
	if !scan.Connections[0].Connectable || scan.Connections[0].Remote || scan.Connections[len(scan.Connections)-1].Connectable {
		t.Errorf("order: first %+v, last %+v", scan.Connections[0], scan.Connections[len(scan.Connections)-1])
	}
	acme := projectNamed(t, scan, "acme")
	if len(acme.ConnectionIDs) != 3 || len(acme.EnvFiles) != 2 || acme.EnvFiles[0] != (EnvFile{File: ".env.local", Read: true, URLs: 2, OtherKeys: 4}) {
		t.Errorf("acme: ids=%v env=%+v", acme.ConnectionIDs, acme.EnvFiles)
	}
}

func TestSaveByID(t *testing.T) {
	s, conns, home, _ := acmeMachine(t)
	scanAndWait(t, s, filepath.Join(home, "code"))
	scan := withEnv(t, s)
	pg := connectionTo(t, scan, "postgres", "localhost:5432")
	redis := connectionTo(t, scan, "redis", "localhost:6379")
	var analytics, mysql Connection
	for _, c := range scan.Connections {
		switch {
		case c.Database == "analytics":
			analytics = c
		case c.Kind == "mysql":
			mysql = c
		}
	}
	name := "cache"
	conns.refuse = "analytics"
	out, err := s.Save(context.Background(), []SaveItem{
		{ID: pg.ID, Capture: true},
		{ID: redis.ID, Capture: false, Name: &name},
		{ID: analytics.ID, Capture: true},
		{ID: mysql.ID},
		{ID: "u_0000000000"},
		{ID: pg.ID, Capture: true}, // twice in one request: once
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Saved) != 2 || out.Saved[0].ID != pg.ID || out.Saved[0].Already || out.Saved[1].ID != redis.ID {
		t.Errorf("saved = %+v", out.Saved)
	}
	codes := map[string]string{}
	for _, f := range out.Failed {
		codes[f.ID] = f.Error.Code
	}
	if len(out.Failed) != 3 || codes[analytics.ID] != "capture_remote" || codes[mysql.ID] != "not_connectable" || codes["u_0000000000"] != "not_found" {
		t.Errorf("failed = %v", codes)
	}
	// The manager was handed the URLs as the files had them.
	got := map[string]addCall{}
	for _, a := range conns.added {
		got[a.kind] = a
	}
	if len(conns.added) != 2 ||
		got["postgres"] != (addCall{kind: "postgres", url: "postgres://acme:PWLOCAL-7Hq2@localhost:5432/acme", capture: true}) ||
		got["redis"] != (addCall{kind: "redis", url: "redis://:PWREDIS-4@localhost:6379", name: "cache", capture: false}) {
		t.Errorf("added = %+v", conns.added)
	}

	// And where each was found, for the saved connection to say: the
	// project, its folder and branch, the variable and the file. The
	// Postgres URL is in three places; the one named is the one the dev
	// tools would use, acme's .env.local, which outranks its .env.
	where := map[string]Found{}
	for i, a := range conns.added {
		where[a.kind] = conns.found[i]
	}
	acme := filepath.Join(home, "code", "acme")
	if where["postgres"] != (Found{Project: "acme", Path: acme, Branch: "main", Key: "DATABASE_URL", File: ".env.local"}) ||
		where["redis"] != (Found{Project: "acme", Path: acme, Branch: "main", Key: "REDIS_URL", File: ".env.local"}) {
		t.Errorf("found = %+v", where)
	}

	// The scan now says they are saved, and no longer suggests them.
	after, _ := s.Scan()
	for _, c := range after.Connections {
		if c.ID == pg.ID || c.ID == redis.ID {
			if !c.Saved || c.SavedID == "" || c.Suggested {
				t.Errorf("after saving: %+v", c)
			}
		} else if c.Saved {
			t.Errorf("not saved, and says it is: %+v", c)
		}
	}
	// Saving one again changes its capture and adds nothing.
	out, err = s.Save(context.Background(), []SaveItem{{ID: pg.ID, Capture: false}})
	if err != nil || len(out.Saved) != 1 || !out.Saved[0].Already || len(conns.added) != 2 {
		t.Errorf("saving again: %+v, %v, added %d", out, err, len(conns.added))
	}

	// A percent-encoded password goes through byte for byte, and so does
	// an ORM's own parameter: the URL is saved as the file has it.
	conns.refuse = ""
	if _, err = s.Save(context.Background(), []SaveItem{{ID: analytics.ID}}); err != nil {
		t.Fatal(err)
	}
	if last := conns.added[len(conns.added)-1]; last.url != "postgresql://reader:PW%40ODD%2Fchars@db.acme.test:5432/analytics?sslmode=require&schema=public" {
		t.Errorf("analytics was saved as %q", last.url)
	}
	// A folder in no repository has no branch to name.
	if last := conns.found[len(conns.found)-1]; last != (Found{Project: "billing-service", Path: filepath.Join(home, "code", "billing-service"), Key: "ANALYTICS_URL", File: ".env"}) {
		t.Errorf("analytics was found in %+v", last)
	}

	// A new scan finds them saved already, by where they point.
	scanAndWait(t, s)
	scan = withEnv(t, s)
	if c := connectionTo(t, scan, "postgres", "localhost:5432"); !c.Saved || c.Suggested || c.ID == pg.ID {
		t.Errorf("rescanned postgres = %+v (old id %s)", c, pg.ID)
	}
	// And the old scan's ids are good for nothing.
	out, _ = s.Save(context.Background(), []SaveItem{{ID: redis.ID}})
	if len(out.Failed) != 1 || out.Failed[0].Error.Code != "not_found" {
		t.Errorf("an old id: %+v", out)
	}
}

// One database is often in two files, spelled two ways: plainly in a
// file whose value some other file outranks, and with Prisma's `?schema=`
// in the file the project really runs on. They are one connection, and the
// spelling shown, tried and saved is the one the dev tools would use,
// whichever file was read first. Saved the other way, the line Leadline
// hands back for the project's .env would send it to the wrong schema.
func TestTheWorkingSpellingIsTheOneSaved(t *testing.T) {
	const plain = "postgres://app:PWLOCAL-7Hq2@localhost:5432/shared"
	const prisma = "postgresql://app:PWLOCAL-7Hq2@127.0.0.1:5432/shared?schema=tenant_a&connection_limit=5"
	for _, order := range [][]string{{"acme-web", "acme-api"}, {"acme-api", "acme-web"}, {"both"}} {
		base := tempDir(t)
		home := filepath.Join(base, "Users", "acme")
		writeFiles(t, home, map[string]string{
			"code/acme-web/package.json": `{"name":"acme-web","scripts":{"dev":"next dev"},"dependencies":{"next":"^16.0.1"}}`,
			// .env.local sets DATABASE_URL, so the one in .env is not used.
			"code/acme-web/.env.local": "DATABASE_URL=postgres://app:PWLOCAL-7Hq2@localhost:5432/shop\n",
			"code/acme-web/.env":       "DATABASE_URL=" + plain + "\n",

			"code/acme-api/package.json": `{"name":"acme-api","scripts":{"dev":"node server.js"}}`,
			"code/acme-api/.env":         `DATABASE_URL="` + prisma + `"` + "\n",
		})
		conns := &fakeConnections{}
		s := New(Options{Connections: conns, Home: home, Processes: NoProcesses(), Berth: func() string { return "" }})
		scan := scanAndWait(t, s, filepath.Join(home, "code"))
		var err error
		for _, name := range order {
			ids := []string{projectNamed(t, scan, "acme-web").ID, projectNamed(t, scan, "acme-api").ID}
			if name != "both" {
				ids = []string{projectNamed(t, scan, name).ID}
			}
			if scan, err = s.ReadEnv(context.Background(), ids); err != nil {
				t.Fatal(err)
			}
		}
		var shared Connection
		for _, c := range scan.Connections {
			if c.Database == "shared" {
				shared = c
			}
		}
		if len(scan.Connections) != 2 || len(shared.FoundIn) != 2 {
			t.Fatalf("%v: connections = %+v", order, scan.Connections)
		}
		if shared.URL != "postgresql://app:****@127.0.0.1:5432/shared?schema=tenant_a&connection_limit=5" || shared.Host != "127.0.0.1:5432" {
			t.Errorf("%v: shown as %q at %s", order, shared.URL, shared.Host)
		}
		if _, err := s.Test(context.Background(), []string{shared.ID}, false); err != nil || len(conns.tested) != 1 || conns.tested[0] != prisma {
			t.Errorf("%v: tried %q, %v", order, conns.tested, err)
		}
		out, err := s.Save(context.Background(), []SaveItem{{ID: shared.ID, Capture: true}})
		if err != nil || len(out.Saved) != 1 || len(conns.added) != 1 {
			t.Fatalf("%v: saved = %+v, %v", order, out, err)
		}
		if conns.added[0].url != prisma {
			t.Errorf("%v: saved as %q", order, conns.added[0].url)
		}
		if conns.found[0] != (Found{Project: "acme-api", Path: filepath.Join(home, "code", "acme-api"), Key: "DATABASE_URL", File: ".env"}) {
			t.Errorf("%v: found in %+v", order, conns.found[0])
		}
		// The id it had before the second file was read is still good,
		// and the scan says it is saved.
		after, _ := s.Scan()
		for _, c := range after.Connections {
			if c.ID == shared.ID && (!c.Saved || c.Suggested) {
				t.Errorf("%v: after saving: %+v", order, c)
			}
		}
	}
}

func TestTestByID(t *testing.T) {
	s, conns, home, _ := acmeMachine(t)
	scanAndWait(t, s, filepath.Join(home, "code"))
	scan := withEnv(t, s)
	pg := connectionTo(t, scan, "postgres", "localhost:5432")
	var analytics Connection
	for _, c := range scan.Connections {
		if c.Database == "analytics" {
			analytics = c
		}
	}
	out, err := s.Test(context.Background(), []string{pg.ID, analytics.ID, "u_nope"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 3 || out.Results[0].Result == nil || out.Results[1].Error == nil || out.Results[1].Error.Code != "remote" ||
		out.Results[2].Error == nil || out.Results[2].Error.Code != "not_found" {
		t.Errorf("results = %+v", out.Results)
	}
	// A remote server is connected to only when asked for by name.
	if len(conns.tested) != 1 || !strings.Contains(conns.tested[0], "localhost:5432") {
		t.Errorf("tested = %d", len(conns.tested))
	}
	out, _ = s.Test(context.Background(), []string{analytics.ID}, true)
	if out.Results[0].Result == nil || len(conns.tested) != 2 {
		t.Errorf("remote, when asked: %+v", out.Results)
	}
	ids := make([]string, maxTest+1)
	if _, err := s.Test(context.Background(), ids, false); err == nil {
		t.Error("too many ids were accepted")
	}
}

func TestSecretsAreLetGo(t *testing.T) {
	s, _, home, _ := acmeMachine(t)
	s.secretsFor = 30 * time.Millisecond
	// A scan holds no URL, so there is nothing to let go of yet.
	if scan := scanAndWait(t, s, filepath.Join(home, "code")); scan.SecretsUntilMs != 0 {
		t.Errorf("a scan that read no env file has secrets_until_ms = %v", scan.SecretsUntilMs)
	}
	scan := withEnv(t, s)
	if scan.SecretsUntilMs <= scan.FinishedMs {
		t.Errorf("secrets_until_ms = %v, finished_ms = %v", scan.SecretsUntilMs, scan.FinishedMs)
	}
	pg := connectionTo(t, scan, "postgres", "localhost:5432")
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, err := s.Save(context.Background(), []SaveItem{{ID: pg.ID}})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Failed) == 1 && out.Failed[0].Error.Code == "expired" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the URL was still held: %+v", out)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The result is still there to read; only the URLs are gone.
	if after, ok := s.Scan(); !ok || len(after.Connections) != len(scan.Connections) {
		t.Error("the scan's result went with its secrets")
	}
	s.mu.Lock()
	held, keys := s.cur.secrets, s.cur.identities
	s.mu.Unlock()
	if held != nil || keys != nil {
		t.Error("the secrets are still in memory")
	}

	// Cancel forgets the scan altogether.
	s.Cancel()
	if _, ok := s.Scan(); ok {
		t.Error("the scan outlived its cancel")
	}
	if _, err := s.Save(context.Background(), []SaveItem{{ID: pg.ID}}); err == nil || asProblem(err).Code != "no_scan" {
		t.Errorf("save with no scan: %v", err)
	}
}

func TestANewScanCancelsTheOld(t *testing.T) {
	s, _, home, _ := acmeMachine(t)
	// The first scan stops at the first directory it lists until let go.
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	real := s.fs.readDir
	var once sync.Once
	s.fs.readDir = func(dir string) ([]fs.DirEntry, error) {
		once.Do(func() {
			entered <- struct{}{}
			<-gate
		})
		return real(dir)
	}
	first, err := s.Start(ScanOptions{Folders: []string{filepath.Join(home, "code")}})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	if running, _ := s.Scan(); running.State != "running" || running.ID != first.ID {
		t.Fatalf("first scan = %+v", running)
	}
	// Someone waiting on the first is told when it is replaced.
	s.mu.Lock()
	firstJob := s.cur
	s.mu.Unlock()
	waited := make(chan Scan, 1)
	go func() { waited <- s.waitFor(context.Background(), firstJob, 10*time.Second) }()

	second, err := s.Start(ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := <-waited; got.ID != first.ID || got.State != "cancelled" {
		t.Errorf("the waiter got %s %s", got.ID, got.State)
	}
	close(gate)
	done, ok := s.Wait(context.Background(), 10*time.Second)
	if !ok || done.ID != second.ID || done.State != "done" || done.ID == first.ID {
		t.Errorf("latest = %s %s (second is %s)", done.ID, done.State, second.ID)
	}
	// The first, let go, wrote nothing over the second.
	time.Sleep(50 * time.Millisecond)
	if now, _ := s.Scan(); now.ID != second.ID || now.State != "done" || len(now.Folders) != 0 {
		t.Errorf("after the first scan ended: %+v", now)
	}
	if _, err := s.Start(ScanOptions{Folders: make([]string, maxFolders+1)}); err == nil {
		t.Error("too many folders were accepted")
	}
}

func TestRefusedAndMissingFolders(t *testing.T) {
	s, _, home, rec := acmeMachine(t)
	// With nothing running, so only the folders could lead anywhere.
	s.sys = nothingRunning{}
	s.opts.Runs = nil
	scan := scanAndWait(t, s, home, "~", filepath.Join(home, "nope"), filepath.Dir(home))
	for i, want := range []string{"refused", "refused", "missing", "refused"} {
		if f := scan.Folders[i]; f.State != want || f.Reason == "" || f.Projects != 0 {
			t.Errorf("folder %d = %+v, want %s", i, f, want)
		}
	}
	if len(scan.Projects) != 0 || len(scan.Connections) != 0 || scan.Env.FilesRead != 0 {
		t.Errorf("a refused folder was scanned: %d projects, %d connections", len(scan.Projects), len(scan.Connections))
	}
	if out := rec.outside(); len(out) > 0 {
		t.Errorf("a refused folder was opened:\n  %s", strings.Join(out, "\n  "))
	}
}

// Nothing the package gives out, and nothing it logs, has a password or a
// value from an env file that is not a connection URL. The API's own test
// says the same of the HTTP responses, with the real integrations manager
// behind them.
func TestNothingSecretLeavesThePackage(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)

	s, conns, home, _ := acmeMachine(t)
	var out []any
	out = append(out, s.Listening())
	started, _ := s.Start(ScanOptions{Folders: []string{filepath.Join(home, "code"), home, "/nope"}, Daemon: "http://127.0.0.1:4320"})
	out = append(out, started)
	scan, _ := s.Wait(context.Background(), 10*time.Second)
	out = append(out, scan)
	_, err := s.ReadEnv(context.Background(), []string{"p_nope", filepath.Join(home, ".env")})
	out = append(out, errText(err))
	_, err = s.ReadEnv(context.Background(), nil)
	out = append(out, errText(err))
	scan = withEnv(t, s)
	out = append(out, scan)
	if scan.State != "done" || len(scan.Connections) != 7 {
		t.Fatalf("scan = %s with %d connections", scan.State, len(scan.Connections))
	}

	ids := []string{"u_nope"}
	var items []SaveItem
	for _, c := range scan.Connections {
		ids = append(ids, c.ID)
		items = append(items, SaveItem{ID: c.ID, Capture: true})
	}
	tested, err := s.Test(context.Background(), ids, true)
	out = append(out, tested, errText(err))
	conns.refuse = "db.acme.test"
	saved, err := s.Save(context.Background(), append(items, SaveItem{ID: "u_nope"}))
	out = append(out, saved, errText(err))
	latest, _ := s.Scan()
	out = append(out, latest)
	_, err = s.Save(context.Background(), make([]SaveItem, maxSave+1))
	out = append(out, errText(err))
	s.Cancel()
	_, err = s.Save(context.Background(), items)
	out = append(out, errText(err))

	body, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body) + "\n" + logs.String()
	for _, secret := range secrets {
		if strings.Contains(text, secret) {
			at := strings.Index(text, secret)
			t.Errorf("%q is in what the package gave out: …%s…", secret, text[max(0, at-80):min(len(text), at+len(secret)+40)])
		}
	}
	// The test is of something: the URLs are there, masked, and the
	// manager did get them whole.
	if !strings.Contains(text, "postgres://acme:****@localhost:5432/acme") || !strings.Contains(text, "DATABASE_URL") {
		t.Error("the masked URLs are missing from the output")
	}
	if len(conns.added) == 0 || !strings.Contains(conns.added[0].url, "PW") {
		t.Errorf("the manager was not given the URLs: %+v", conns.added)
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// The consent this feature turns on: a scan names projects and says which
// env files each has, and opens none of them. Only ReadEnv opens an env
// file, and only those of the projects it is given.
func TestAScanOpensNoEnvFile(t *testing.T) {
	s, _, home, rec := acmeMachine(t)
	code := filepath.Join(home, "code")
	scan := scanAndWait(t, s, code)

	if len(scan.Projects) != 3 || len(scan.Connections) != 0 || scan.Connections == nil || len(scan.EnvProjects) != 0 || scan.EnvProjects == nil ||
		scan.Env != (EnvTotals{}) || scan.SecretsUntilMs != 0 {
		t.Errorf("a scan found connections without being asked: %d connections, env %+v, env_projects %v", len(scan.Connections), scan.Env, scan.EnvProjects)
	}
	// The env files are known by name, from the folder's listing.
	acme := projectNamed(t, scan, "acme")
	want := []EnvFile{{File: ".env.local"}, {File: ".env"}}
	if len(acme.EnvFiles) != 2 || acme.EnvFiles[0] != want[0] || acme.EnvFiles[1] != want[1] || len(acme.ConnectionIDs) != 0 {
		t.Errorf("acme env files = %+v, connection ids = %v", acme.EnvFiles, acme.ConnectionIDs)
	}
	// And not one was opened, or even stat'ed.
	envTouched := func() []string {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		var out []string
		for path, how := range rec.paths {
			if strings.HasPrefix(filepath.Base(path), ".env") {
				out = append(out, how+" "+path)
			}
		}
		sort.Strings(out)
		return out
	}
	if touched := envTouched(); len(touched) != 0 {
		t.Fatalf("a scan touched env files:\n  %s", strings.Join(touched, "\n  "))
	}
	s.mu.Lock()
	held := s.cur.secrets
	s.mu.Unlock()
	if held != nil {
		t.Error("a scan holds URLs")
	}
	// Nor does anything else short of ReadEnv: listing what listens,
	// reading the scan, saving or trying by an id.
	s.Listening()
	s.Scan()
	s.Save(context.Background(), []SaveItem{{ID: "u_0000000000"}})
	s.Test(context.Background(), []string{"u_0000000000"}, true)
	if touched := envTouched(); len(touched) != 0 {
		t.Fatalf("env files were touched without ReadEnv:\n  %s", strings.Join(touched, "\n  "))
	}

	// One project is named: its env files are opened, and no other's.
	admin := projectNamed(t, scan, "acme-admin")
	after, err := s.ReadEnv(context.Background(), []string{admin.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, touched := range envTouched() {
		if !strings.Contains(touched, filepath.Join(code, "acme-admin")+string(filepath.Separator)) {
			t.Errorf("reading acme-admin's env files touched %s", touched)
		}
	}
	if len(envTouched()) == 0 {
		t.Error("ReadEnv opened nothing (so the test proves less than it says)")
	}
	if strings.Join(after.EnvProjects, " ") != admin.ID || after.Env != (EnvTotals{FilesRead: 1, URLs: 3, OtherKeys: 1}) || len(after.Connections) != 2 {
		t.Errorf("after acme-admin: env_projects %v, env %+v, %d connections", after.EnvProjects, after.Env, len(after.Connections))
	}
	if got := projectNamed(t, after, "acme-admin").EnvFiles; len(got) != 1 || got[0] != (EnvFile{File: ".env", Read: true, URLs: 3, OtherKeys: 1}) {
		t.Errorf("acme-admin env files = %+v", got)
	}
	// The others' are still only names.
	if got := projectNamed(t, after, "acme").EnvFiles; got[0].Read || got[1].Read || got[0].URLs != 0 {
		t.Errorf("acme's env files were read: %+v", got)
	}
	// The scan handed out before is as it was.
	if len(scan.Connections) != 0 || projectNamed(t, scan, "acme-admin").EnvFiles[0].Read {
		t.Error("reading env files changed a scan that was already handed out")
	}
}

func TestReadEnvTakesNamesAndAddsUp(t *testing.T) {
	s, _, home, _ := acmeMachine(t)
	code := filepath.Join(home, "code")

	// Nothing to read from.
	if _, err := s.ReadEnv(context.Background(), []string{"p_0000000000"}); err == nil || asProblem(err).Code != "no_scan" {
		t.Errorf("with no scan: %v", err)
	}
	scan := scanAndWait(t, s, code)
	// There is no "all of them": the list has to be there.
	for _, none := range [][]string{nil, {}} {
		if _, err := s.ReadEnv(context.Background(), none); err == nil || asProblem(err).Code != "bad_request" {
			t.Errorf("with no projects named: %v", err)
		}
	}
	if _, err := s.ReadEnv(context.Background(), make([]string, maxEnvProjects+1)); err == nil || asProblem(err).Code != "bad_request" {
		t.Errorf("with too many: %v", err)
	}
	// A project the scan did not find is not read, whatever is there:
	// the home directory has an env file, and is nobody's project.
	admin, acme := projectNamed(t, scan, "acme-admin"), projectNamed(t, scan, "acme")
	for _, name := range []string{"p_0000000000", home, filepath.Join(home, ".env"), filepath.Join(code, "nope"), "", "*"} {
		if _, err := s.ReadEnv(context.Background(), []string{admin.ID, name}); err == nil || asProblem(err).Code != "no_project" {
			t.Errorf("naming %q: %v", name, err)
		}
	}
	// And one bad name means nothing was read, the good one included.
	if now, _ := s.Scan(); len(now.Connections) != 0 || len(now.EnvProjects) != 0 {
		t.Errorf("a refused request read env files: %+v", now.EnvProjects)
	}

	// By id.
	first, err := s.ReadEnv(context.Background(), []string{admin.ID})
	if err != nil {
		t.Fatal(err)
	}
	pg := connectionTo(t, first, "postgres", "localhost:5432")
	if len(pg.FoundIn) != 2 || len(first.Connections) != 2 {
		t.Fatalf("after acme-admin: %+v", first.Connections)
	}
	// By path, and with the one already read named again: it is not
	// read twice, ids stay, and what the two share is one connection
	// found in both.
	second, err := s.ReadEnv(context.Background(), []string{admin.ID, acme.Path + "/"})
	if err != nil {
		t.Fatal(err)
	}
	again := connectionTo(t, second, "postgres", "localhost:5432")
	if again.ID != pg.ID || len(again.FoundIn) != 3 || again.FoundIn[2].Project != "acme" {
		t.Errorf("the shared connection = %+v (was %s)", again, pg.ID)
	}
	if second.Env != (EnvTotals{FilesRead: 3, URLs: 6, OtherKeys: 6}) || len(second.EnvProjects) != 2 || len(second.Connections) != 4 {
		t.Errorf("after both: env %+v, projects %v, %d connections", second.Env, second.EnvProjects, len(second.Connections))
	}
	if len(pg.FoundIn) != 2 {
		t.Error("adding to a connection changed the one handed out before")
	}
	third, _ := s.ReadEnv(context.Background(), []string{acme.ID})
	if third.Env != second.Env || len(third.Connections) != 4 {
		t.Errorf("reading the same again changed the totals: %+v", third.Env)
	}

	// While a scan runs its projects are not known, so there is nothing
	// to name yet.
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	real := s.fs.readDir
	var once sync.Once
	s.fs.readDir = func(dir string) ([]fs.DirEntry, error) {
		once.Do(func() {
			entered <- struct{}{}
			<-gate
		})
		return real(dir)
	}
	if _, err := s.Start(ScanOptions{Folders: []string{code}}); err != nil {
		t.Fatal(err)
	}
	<-entered
	if _, err := s.ReadEnv(context.Background(), []string{acme.ID}); err == nil || asProblem(err).Code != "scan_not_done" {
		t.Errorf("while the scan runs: %v", err)
	}
	close(gate)
	s.Wait(context.Background(), 10*time.Second)
}

// Reading env files swaps new lists into the scan while others may be
// reading the one they were given. Run with -race, this says they never
// share a list that changes.
func TestReadingEnvWhileTheScanIsRead(t *testing.T) {
	s, _, home, _ := acmeMachine(t)
	scan := scanAndWait(t, s, filepath.Join(home, "code"))
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if sc, ok := s.Scan(); ok {
					json.Marshal(sc)
				}
			}
		}()
	}
	for _, p := range scan.Projects {
		if _, err := s.ReadEnv(context.Background(), []string{p.ID}); err != nil {
			t.Error(err)
		}
		if conns, ok := s.Scan(); ok && len(conns.Connections) > 0 {
			s.Save(context.Background(), []SaveItem{{ID: conns.Connections[0].ID, Capture: true}})
		}
	}
	close(stop)
	wg.Wait()
}

// Shipyard's command line is run when Shipyard is asked about, and at no
// other time: not when a Service is made, not by a scan, not by reading
// env files or saving.
func TestBerthIsNotRunUnlessAsked(t *testing.T) {
	cli, log := berthThatAnswers(t)
	looked := 0
	s, _, home, _ := acmeMachine(t)
	s.ship.find = func() string { looked++; return cli }
	s.ship.boxesTimeout = time.Minute

	s.Listening()
	scanAndWait(t, s, filepath.Join(home, "code"))
	scan := withEnv(t, s)
	s.Save(context.Background(), []SaveItem{{ID: scan.Connections[0].ID, Capture: true}})
	s.Test(context.Background(), []string{scan.Connections[0].ID}, false)
	s.Cancel()
	if runs := ran(t, log); looked != 0 || len(runs) != 0 {
		t.Fatalf("berth was looked for %d times and run %q with nobody asking about Shipyard", looked, runs)
	}
	s.Shipyard(context.Background(), false)
	if runs := ran(t, log); looked != 1 || len(runs) != 1 || runs[0] != "boxes --json" {
		t.Errorf("when asked: looked %d times, ran %q", looked, runs)
	}
}
