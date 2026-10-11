package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sean-brydon/leadline/internal/discover"
	"github.com/sean-brydon/leadline/internal/integrations"
	"github.com/sean-brydon/leadline/internal/integrations/connurl"
	"github.com/sean-brydon/leadline/internal/integrations/fake"
)

// discoverClient is a test daemon whose discovery looks at a made-up
// machine: no processes (so the test reads nothing of whatever runs where
// it runs), a home directory in a temporary folder, and berth as given
// ("" for not installed).
func discoverClient(t *testing.T, home, berth string) *client {
	t.Helper()
	discoverOptions = func(o *discover.Options) {
		o.Processes = discover.NoProcesses()
		o.Home = home
		o.Berth = func() string { return berth }
		// A script written a moment ago can take seconds to start the
		// first time on a busy macOS, which checks it first.
		o.BerthTimeout = time.Minute
	}
	t.Cleanup(func() { discoverOptions = nil })
	return newIntegrationsClient(t)
}

func writeTree(t *testing.T, dir string, tree map[string]string) {
	t.Helper()
	for name, content := range tree {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// The whole of onboarding's side of discovery over HTTP, with the real
// integrations manager and pretend servers behind it: scan, read, try,
// save, and see the saved connections where the rest of the app looks.
// And then the point of it: not one response, log line or error had a
// password in it, or any value of an env file that is not a connection
// URL.
func TestDiscoverAPI(t *testing.T) {
	var logs bytes.Buffer
	oldLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(oldLog)

	pg, rd := fake.NewPostgres(t), fake.NewRedis(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, "Users", "acme")
	code := filepath.Join(home, "code")
	// Every password, and every other value, in the files below.
	secrets := []string{pg.Password, rd.Password, "REMOTE-pw-551", "MYSQL-pw-552", "INTERP-pw-553",
		"OTHER-stripe-554", "OTHER-session-555", "OTHER-key-body-556", "OTHER-token-557", "HOME-pw-558"}
	writeTree(t, home, map[string]string{
		".env": "DATABASE_URL=postgres://home:HOME-pw-558@localhost:5432/home\n",

		"code/acme/package.json":   `{"name":"acme","scripts":{"dev":"next dev"},"dependencies":{"next":"^16.0.1"}}`,
		"code/acme/pnpm-lock.yaml": "",
		"code/acme/.env.local": strings.Join([]string{
			`DATABASE_URL="` + pg.URL() + `"`,
			`REDIS_URL=` + rd.URL(),
			`STRIPE_SECRET_KEY='OTHER-stripe-554'`,
			`SESSION_PASSWORD=OTHER-session-555`,
			`PRIVATE_KEY="-----BEGIN-----`,
			`OTHER-key-body-556`,
			`-----END-----"`,
		}, "\n"),
		"code/acme/.env": "DATABASE_URL=postgres://acme:REMOTE-pw-551@db.acme.test:5432/acme\nAPI_TOKEN=OTHER-token-557\n",

		"code/acme-admin/package.json": `{"name":"acme-admin","scripts":{"dev":"vite"},"devDependencies":{"vite":"^7.1.4"},"dependencies":{"express":"^5.1.0"}}`,
		"code/acme-admin/.env": strings.Join([]string{
			`DATABASE_URL=` + pg.URL(),
			`LEGACY_URL=mysql://root:MYSQL-pw-552@127.0.0.1:3306/legacy`,
			`CACHE_URL=redis://default:${CACHE_PASSWORD}@cache.acme.test:6379/1`,
			`CACHE_PASSWORD=INTERP-pw-553`,
		}, "\n"),

		"code/billing-service/go.mod":  "module github.com/acme/billing-service\n\ngo 1.27\n\nrequire go.opentelemetry.io/otel v1.38.0\n",
		"code/billing-service/main.go": "package main\n",
	})

	// A berth that prints made-up boxes.
	berth := filepath.Join(t.TempDir(), "berth")
	berthLog := berth + ".log"
	script := `#!/bin/sh
echo "$@" >> '` + berthLog + `'
case "$1" in
boxes) echo '[{"name":"hetzner-1","address":"100.64.0.11:1376","fingerprint":"aa11bb22","state":"online","latency_ms":86,"since":"2026-10-09T08:00:00Z"},{"name":"build-box","state":"offline","error":"dial tcp: i/o timeout","since":"2026-10-08T17:30:00Z"}]' ;;
locations) echo '[{"name":"acme","path":"/home/dev/acme","repo":true,"remote":"https://x:OTHER-token-557@git.acme.test/acme.git","slug":"acme/acme","default_branch":"main","worktrees":[{"name":"main","path":"/home/dev/acme","branch":"main","main":true,"port":3000}]}]' ;;
*) exit 64 ;;
esac
`
	if err := os.WriteFile(berth, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	c := discoverClient(t, home, berth)

	// Before any scan.
	c.fails("GET", "/api/discover/scan", "", http.StatusNotFound, "no_scan")
	c.fails("POST", "/api/discover/save", `{"connections":[{"id":"u_0000000000","capture":true}]}`, http.StatusNotFound, "no_scan")
	var listening discover.Listening
	c.ok("GET", "/api/discover/listening", "", &listening)
	if listening.Listeners == nil || len(listening.Listeners) != 0 || listening.Machine.OS == "" {
		t.Errorf("listening = %+v", listening)
	}

	// A `leadline run` the daemon has seen is a way a project is found.
	c.ok("POST", "/api/runs", jsonBody(map[string]any{"command": "go run .", "cwd": filepath.Join(code, "billing-service"), "service": "billing", "pid": 1, "runner_pid": os.Getpid()}), nil)

	var scan discover.Scan
	c.ok("POST", "/api/discover/scan", jsonBody(map[string]any{"folders": []string{code, home}}), &scan)
	if scan.State != "running" || scan.ID == "" || len(scan.Folders) != 2 || scan.Projects == nil || scan.Connections == nil || scan.Listeners == nil {
		t.Fatalf("a scan just started = %+v", scan)
	}
	first := scan.ID
	c.ok("GET", "/api/discover/scan?wait_ms=20000", "", &scan)
	if scan.State != "done" || scan.ID != first {
		t.Fatalf("scan = %s %s", scan.ID, scan.State)
	}
	if scan.Folders[0].State != "scanned" || scan.Folders[1].State != "refused" || scan.Folders[1].Reason == "" {
		t.Errorf("folders = %+v", scan.Folders)
	}
	if len(scan.Projects) != 3 {
		t.Fatalf("projects = %d", len(scan.Projects))
	}
	// A scan names projects and their env files. It opens no env file,
	// so it has no connections to show, and holds no URL.
	if len(scan.Connections) != 0 || scan.Connections == nil || scan.Env != (discover.EnvTotals{}) || len(scan.EnvProjects) != 0 || scan.SecretsUntilMs != 0 {
		t.Errorf("a scan found connections unasked: %+v, env %+v", scan.Connections, scan.Env)
	}
	var ids []string
	for _, p := range scan.Projects {
		ids = append(ids, p.ID)
		for _, f := range p.EnvFiles {
			if f.Read || f.URLs != 0 || f.OtherKeys != 0 {
				t.Errorf("%s: %s was read by the scan: %+v", p.Name, f.File, f)
			}
		}
		if len(p.ConnectionIDs) != 0 {
			t.Errorf("%s has connections before its env files were asked for", p.Name)
		}
		switch p.Name {
		case "acme":
			// This daemon is not at the usual address, so the advice
			// says where it is.
			if p.Label != "Next.js 16" || p.Connect.Kind != "next-plugin" || p.Connect.Command != "pnpm add -D @leadline/next" ||
				len(p.EnvFiles) != 2 || p.EnvFiles[0].File != ".env.local" ||
				p.Connect.EnvFile != ".env.local" || len(p.Connect.EnvLines) != 1 || p.Connect.EnvLines[0] != "LEADLINE_URL="+c.base {
				t.Errorf("acme = %+v", p)
			}
		case "acme-admin":
			if p.Label != "Vite + Express" || p.Connect.Kind != "run-wrap" || p.Connect.Command != "leadline run --url "+c.base+" -- npm run dev" {
				t.Errorf("acme-admin = %+v", p)
			}
		case "billing-service":
			// Found in the folder and by its run; the advice names the
			// address this request came to.
			if strings.Join(p.Sources, " ") != "folder run" || !p.Sends.Already || p.Sends.Service != "billing" || p.Connect.Kind != "already" {
				t.Errorf("billing-service = %+v", p)
			}
		default:
			t.Errorf("an unexpected project: %+v", p)
		}
	}
	// Env files are read for the projects named, and there is no request
	// that reads them without naming any.
	c.fails("POST", "/api/discover/env", `{}`, http.StatusBadRequest, "bad_request")
	c.fails("POST", "/api/discover/env", `{"projects":[]}`, http.StatusBadRequest, "bad_request")
	c.fails("POST", "/api/discover/env", jsonBody(map[string]any{"projects": []string{ids[0], home}}), http.StatusNotFound, "no_project")
	c.fails("POST", "/api/discover/env", jsonBody(map[string]any{"projects": []string{"p_0000000000"}}), http.StatusNotFound, "no_project")
	c.ok("GET", "/api/discover/scan", "", &scan)
	if len(scan.Connections) != 0 || len(scan.EnvProjects) != 0 {
		t.Errorf("a refused request read env files: %+v", scan.EnvProjects)
	}
	c.ok("POST", "/api/discover/env", jsonBody(map[string]any{"projects": ids}), &scan)
	if len(scan.EnvProjects) != 3 || scan.SecretsUntilMs == 0 {
		t.Errorf("after reading env files: env_projects %v, secrets_until_ms %v", scan.EnvProjects, scan.SecretsUntilMs)
	}
	for _, p := range scan.Projects {
		if p.Name == "acme" && (len(p.ConnectionIDs) != 3 || !p.EnvFiles[0].Read || p.EnvFiles[0].OtherKeys != 3) {
			t.Errorf("acme after its env files were read = %+v", p)
		}
	}
	if scan.Env != (discover.EnvTotals{FilesRead: 3, URLs: 6, OtherKeys: 5}) {
		t.Errorf("env = %+v", scan.Env)
	}
	// Six URLs, one of them in two files.
	if len(scan.Connections) != 5 {
		t.Fatalf("connections = %d: %+v", len(scan.Connections), scan.Connections)
	}
	var local, cache, remote, mysql, interp discover.Connection
	for _, conn := range scan.Connections {
		switch {
		case conn.Kind == "postgres" && !conn.Remote:
			local = conn
		case conn.Kind == "redis" && conn.Connectable:
			cache = conn
		case conn.Kind == "postgres":
			remote = conn
		case conn.Kind == "mysql":
			mysql = conn
		case conn.Reason == "interpolated":
			interp = conn
		}
	}
	if local.Host != pg.Addr() || local.URL != "postgres://acme:****@"+pg.Addr()+"/acme_dev?sslmode=disable" || len(local.FoundIn) != 2 || !local.Suggested || !local.CaptureAvailable {
		t.Errorf("local postgres = %+v", local)
	}
	if cache.URL != "redis://:****@"+rd.Addr()+"/0" || !cache.Suggested {
		t.Errorf("redis = %+v", cache)
	}
	if !remote.Remote || remote.CaptureAvailable || remote.Suggested || remote.FoundIn[0].Effective {
		t.Errorf("remote postgres = %+v", remote)
	}
	if mysql.Connectable || mysql.Reason != "not_supported" || interp.ID == "" || interp.Connectable {
		t.Errorf("mysql = %+v, interpolated = %+v", mysql, interp)
	}

	// Try them. The remote one is not connected to unless asked by name.
	var tested struct {
		Results []struct {
			ID     string                   `json:"id"`
			Result *integrations.TestResult `json:"result"`
			Error  *integrations.Problem    `json:"error"`
		} `json:"results"`
	}
	c.ok("POST", "/api/discover/test", jsonBody(map[string]any{"ids": []string{local.ID, cache.ID, remote.ID, mysql.ID, interp.ID, "u_nope"}}), &tested)
	if len(tested.Results) != 6 {
		t.Fatalf("test results = %+v", tested.Results)
	}
	if r := tested.Results[0]; r.Result == nil || !r.Result.OK || r.Result.VersionShort != "17.0" || r.Result.Database != "acme_dev" {
		t.Errorf("postgres test = %+v %+v", r.Result, r.Error)
	}
	if r := tested.Results[1]; r.Result == nil || !r.Result.OK {
		t.Errorf("redis test = %+v %+v", r.Result, r.Error)
	}
	for i, code := range map[int]string{2: "remote", 3: "not_connectable", 4: "not_connectable", 5: "not_found"} {
		if r := tested.Results[i]; r.Error == nil || r.Error.Code != code || r.Result != nil {
			t.Errorf("test result %d = %+v %+v, want %s", i, r.Result, r.Error, code)
		}
	}

	// Save them, by id.
	var saved struct {
		Saved []struct {
			ID         string                      `json:"id"`
			Already    bool                        `json:"already"`
			Connection integrations.ConnectionView `json:"connection"`
		} `json:"saved"`
		Failed []struct {
			ID    string               `json:"id"`
			Error integrations.Problem `json:"error"`
		} `json:"failed"`
	}
	c.ok("POST", "/api/discover/save", jsonBody(map[string]any{"connections": []map[string]any{
		{"id": local.ID, "capture": true},
		{"id": cache.ID, "capture": false, "name": "cache"},
		{"id": remote.ID, "capture": true},
		{"id": mysql.ID, "capture": false},
		{"id": "u_nope", "capture": false},
	}}), &saved)
	if len(saved.Saved) != 2 || len(saved.Failed) != 3 {
		t.Fatalf("saved = %+v", saved)
	}
	if s := saved.Saved[0]; s.ID != local.ID || s.Already || !s.Connection.Capture || s.Connection.Proxy == nil || s.Connection.Proxy.State != "listening" ||
		s.Connection.Health.State != "ok" || s.Connection.URL != local.URL {
		t.Errorf("saved postgres = %+v", s)
	}
	if s := saved.Saved[1]; s.Connection.Name != "cache" || s.Connection.Capture || s.Connection.Kind != "redis" || s.Connection.Health.State != "ok" {
		t.Errorf("saved redis = %+v", s)
	}
	// Each says it was found by a scan, and where: the project, its
	// folder, the variable and the file, by name. The proxy opened for
	// the Postgres one says the same.
	found := integrations.Owner{Kind: "scan", Project: "acme", Path: filepath.Join(code, "acme"), EnvKey: "DATABASE_URL", EnvFile: ".env.local",
		Label: "found by a scan, DATABASE_URL in acme"}
	if s := saved.Saved[0]; s.Connection.Owner != found || s.Connection.Proxy.Owner != found {
		t.Errorf("saved postgres' owner = %+v, its proxy's %+v", s.Connection.Owner, s.Connection.Proxy.Owner)
	}
	if o := saved.Saved[1].Connection.Owner; o.Kind != "scan" || o.EnvKey != "REDIS_URL" || o.Label != "found by a scan, REDIS_URL in acme" {
		t.Errorf("saved redis' owner = %+v", o)
	}
	failed := map[string]string{}
	for _, f := range saved.Failed {
		failed[f.ID] = f.Error.Code
	}
	if failed[remote.ID] != "capture_remote" || failed[mysql.ID] != "not_connectable" || failed["u_nope"] != "not_found" {
		t.Errorf("failed = %v", failed)
	}

	// They are where the rest of the app looks.
	var list struct {
		Connections []integrations.ConnectionView `json:"connections"`
	}
	c.ok("GET", "/api/connections", "", &list)
	if len(list.Connections) != 2 || list.Connections[0].Host != pg.Addr() || list.Connections[1].Host != rd.Addr() {
		t.Errorf("connections = %+v", list.Connections)
	}
	var sources integrations.Sources
	c.ok("GET", "/api/sources", "", &sources)
	if len(sources.Connections) != 2 || len(sources.Proxies) != 1 {
		t.Errorf("sources: %d connections, %d proxies", len(sources.Connections), len(sources.Proxies))
	}
	// And the scan knows.
	c.ok("GET", "/api/discover/scan", "", &scan)
	for _, conn := range scan.Connections {
		isSaved := conn.ID == local.ID || conn.ID == cache.ID
		if conn.Saved != isSaved || (isSaved && (conn.SavedID == "" || conn.Suggested)) {
			t.Errorf("after saving: %+v", conn)
		}
	}
	// Saving one again turns its capture on and adds nothing.
	c.ok("POST", "/api/discover/save", jsonBody(map[string]any{"connections": []map[string]any{{"id": cache.ID, "capture": true}}}), &saved)
	if len(saved.Saved) != 1 || !saved.Saved[0].Already || !saved.Saved[0].Connection.Capture {
		t.Errorf("saved again = %+v", saved)
	}
	c.ok("GET", "/api/connections", "", &list)
	if len(list.Connections) != 2 {
		t.Errorf("saving again added a connection: %d", len(list.Connections))
	}

	// Requests that can't be read.
	c.fails("POST", "/api/discover/save", `not json`, http.StatusBadRequest, "bad_request")
	c.fails("POST", "/api/discover/test", `{"ids": "x"}`, http.StatusBadRequest, "bad_request")
	c.fails("POST", "/api/discover/scan", jsonBody(map[string]any{"folders": make([]string, 9)}), http.StatusBadRequest, "bad_request")

	// Shipyard's command line has not been run by any of that: it is run
	// when Shipyard is asked about, and only then.
	if ran, err := os.ReadFile(berthLog); err == nil {
		t.Errorf("berth was run with nobody asking about Shipyard: %q", ran)
	}
	var ship discover.Shipyard
	c.ok("GET", "/api/discover/shipyard", "", &ship)
	if !ship.Installed || !ship.Available || len(ship.Boxes) != 2 || ship.Boxes[0].Name != "hetzner-1" || ship.Boxes[0].LatencyMs != 86 ||
		ship.Boxes[1].State != "offline" || ship.Boxes[0].Leadline.State != "unknown" {
		t.Errorf("shipyard = %+v", ship)
	}
	var box discover.BoxProjects
	c.ok("GET", "/api/discover/shipyard/boxes/hetzner-1", "", &box)
	if box.Box != "hetzner-1" || len(box.Projects) != 1 || len(box.Projects[0].Worktrees) != 1 || box.Projects[0].Worktrees[0].Port != 3000 {
		t.Errorf("box projects = %+v", box)
	}
	c.fails("GET", "/api/discover/shipyard/boxes/nope", "", http.StatusNotFound, "no_box")
	c.fails("GET", "/api/discover/shipyard/boxes/--json", "", http.StatusNotFound, "no_box")
	if ran, _ := os.ReadFile(berthLog); string(ran) != "boxes --json\nlocations hetzner-1 --json\n" {
		t.Errorf("berth ran:\n%s", ran)
	}

	// A new scan replaces the old, whose ids then mean nothing.
	c.ok("POST", "/api/discover/scan", "", &scan)
	if scan.ID == first {
		t.Error("a new scan has the old one's id")
	}
	c.ok("GET", "/api/discover/scan?wait_ms=20000", "", &scan)
	if scan.State != "done" || len(scan.Folders) != 0 || len(scan.Projects) != 1 {
		t.Errorf("a scan with no folders = %s, %d projects", scan.State, len(scan.Projects))
	}
	c.ok("POST", "/api/discover/save", jsonBody(map[string]any{"connections": []map[string]any{{"id": local.ID, "capture": true}}}), &saved)
	if len(saved.Failed) != 1 || saved.Failed[0].Error.Code != "not_found" {
		t.Errorf("an old id = %+v", saved)
	}

	// Forgetting it.
	c.ok("DELETE", "/api/discover/scan", "", nil)
	c.fails("GET", "/api/discover/scan", "", http.StatusNotFound, "no_scan")

	// The point: nothing that left the daemon, and nothing it logged.
	if len(c.bodies) < 20 {
		t.Errorf("only %d responses were checked", len(c.bodies))
	}
	checked := append(append([]string{}, c.bodies...), "the daemon's log: "+logs.String())
	for _, body := range checked {
		for _, secret := range secrets {
			if strings.Contains(body, secret) {
				at := strings.Index(body, secret)
				t.Errorf("%q left the daemon in %s…: …%s…", secret, body[:min(len(body), 40)], body[max(0, at-60):min(len(body), at+len(secret)+30)])
			}
		}
	}
	// And the check is of something: the URLs did come back, masked.
	all := strings.Join(c.bodies, "\n")
	for _, want := range []string{"postgres://acme:****@" + pg.Addr(), "redis://:****@" + rd.Addr(), "mysql://root:****@127.0.0.1:3306/legacy", "DATABASE_URL", `"other_keys":3`} {
		if !strings.Contains(all, want) {
			t.Errorf("the responses never had %s", want)
		}
	}
}

func TestDiscoverWithoutShipyard(t *testing.T) {
	c := discoverClient(t, t.TempDir(), "")
	var ship struct {
		discover.Shipyard
		Boxes json.RawMessage `json:"boxes"`
	}
	c.ok("GET", "/api/discover/shipyard", "", &ship)
	if ship.Installed || ship.Available || string(ship.Boxes) != "[]" || ship.Problem == nil || ship.Problem.Code != "shipyard_not_found" {
		t.Errorf("shipyard = %+v boxes = %s", ship.Shipyard, ship.Boxes)
	}
	c.fails("GET", "/api/discover/shipyard/boxes/hetzner-1", "", http.StatusNotFound, "shipyard_not_found")
}

// A project that uses Prisma has its parameters in its DATABASE_URL, and
// one on a schema of its own has `?schema=` saying which. Found by a scan
// and saved, the connection keeps the URL as the env file has it: in `url`
// and, the point of it, in `proxy.url`, which is the line the checklist
// gives back for that project's .env. A line without `schema=` would send
// the project to `public` and nobody would be told. Leadline's own
// connections leave the parameters out, or Postgres would refuse them: the
// pretend server here refuses them as the real one does.
func TestAScanSavedPrismaURLKeepsItsParameters(t *testing.T) {
	pg := fake.NewPostgres(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, "Users", "acme")
	code := filepath.Join(home, "code")
	written := strings.Replace(pg.URL(), "postgres://", "postgresql://", 1) + "&schema=tenant_a&connection_limit=5"
	writeTree(t, home, map[string]string{
		"code/acme-api/package.json": `{"name":"acme-api","scripts":{"dev":"node server.js"},"dependencies":{"@prisma/client":"^7.0.0"}}`,
		"code/acme-api/.env":         `DATABASE_URL="` + written + `"` + "\nAPI_TOKEN=OTHER-token-601\n",
	})
	c := discoverClient(t, home, "")

	var scan discover.Scan
	c.ok("POST", "/api/discover/scan", jsonBody(map[string]any{"folders": []string{code}}), &scan)
	c.ok("GET", "/api/discover/scan?wait_ms=20000", "", &scan)
	if scan.State != "done" || len(scan.Projects) != 1 {
		t.Fatalf("scan = %s, %d projects", scan.State, len(scan.Projects))
	}
	c.ok("POST", "/api/discover/env", jsonBody(map[string]any{"projects": []string{scan.Projects[0].ID}}), &scan)
	if len(scan.Connections) != 1 {
		t.Fatalf("connections = %+v", scan.Connections)
	}
	found := scan.Connections[0]
	shown := strings.Replace(written, ":"+pg.Password+"@", ":****@", 1)
	if found.URL != shown || !found.Connectable || !found.Suggested {
		t.Errorf("found = %+v\n want url %s", found, shown)
	}

	// It tests: the server is asked without them.
	var tested struct {
		Results []struct {
			Result *integrations.TestResult `json:"result"`
			Error  *integrations.Problem    `json:"error"`
		} `json:"results"`
	}
	c.ok("POST", "/api/discover/test", jsonBody(map[string]any{"ids": []string{found.ID}}), &tested)
	if r := tested.Results[0]; r.Result == nil || !r.Result.OK || r.Result.Database != "acme_dev" || r.Result.URL != shown {
		t.Fatalf("test = %+v %+v", r.Result, r.Error)
	}

	// It saves, answers, and keeps every parameter where the project needs them.
	var saved struct {
		Saved []struct {
			Already    bool                        `json:"already"`
			Connection integrations.ConnectionView `json:"connection"`
		} `json:"saved"`
		Failed []struct {
			Error integrations.Problem `json:"error"`
		} `json:"failed"`
	}
	c.ok("POST", "/api/discover/save", jsonBody(map[string]any{"connections": []map[string]any{{"id": found.ID, "capture": true}}}), &saved)
	if len(saved.Saved) != 1 || len(saved.Failed) != 0 {
		t.Fatalf("saved = %+v", saved)
	}
	conn := saved.Saved[0].Connection
	if conn.URL != shown || conn.Health.State != "ok" || conn.Owner.Kind != "scan" || conn.Owner.EnvKey != "DATABASE_URL" {
		t.Errorf("saved connection = %+v", conn)
	}
	if conn.Proxy == nil || conn.Proxy.State != "listening" {
		t.Fatalf("proxy = %+v", conn.Proxy)
	}
	for _, want := range []string{"schema=tenant_a", "connection_limit=5", "sslmode=disable", "@127.0.0.1:"} {
		if !strings.Contains(conn.Proxy.URL, want) {
			t.Errorf("proxy url = %q, without %s", conn.Proxy.URL, want)
		}
	}
	// The saved connection goes on answering: its own test, and its stats.
	var again integrations.TestResult
	c.ok("POST", "/api/connections/"+conn.ID+"/test", "", &again)
	if !again.OK {
		t.Errorf("the saved connection's test: %+v", again.Error)
	}
	var stats map[string]any
	c.ok("GET", "/api/connections/"+conn.ID+"/stats?fresh=1", "", &stats)

	// Nothing of Prisma's reached the server in any of that.
	if len(pg.Startups()) < 3 {
		t.Fatalf("the server saw %d connections", len(pg.Startups()))
	}
	for _, start := range pg.Startups() {
		for _, name := range connurl.ORMParams() {
			if _, sent := start[name]; sent {
				t.Errorf("%s was sent to Postgres: %v", name, start)
			}
		}
	}
	// A scan made afterwards finds it saved already, whatever its spelling.
	c.ok("POST", "/api/discover/scan", jsonBody(map[string]any{"folders": []string{code}}), &scan)
	c.ok("GET", "/api/discover/scan?wait_ms=20000", "", &scan)
	c.ok("POST", "/api/discover/env", jsonBody(map[string]any{"projects": []string{scan.Projects[0].ID}}), &scan)
	if len(scan.Connections) != 1 || !scan.Connections[0].Saved || scan.Connections[0].SavedID != conn.ID {
		t.Errorf("after saving, a new scan says: %+v", scan.Connections)
	}
	for _, body := range c.bodies {
		if strings.Contains(body, pg.Password) || strings.Contains(body, "OTHER-token-601") {
			t.Fatalf("a response held a secret: %s", body)
		}
	}
}

// The discovery routes hold what env files say, so they are behind guard
// like everything else: a page on another site can neither read them nor
// start a scan.
func TestDiscoverIsBehindGuard(t *testing.T) {
	c := discoverClient(t, t.TempDir(), "")
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/discover/listening"}, {"POST", "/api/discover/scan"}, {"GET", "/api/discover/scan"},
		{"POST", "/api/discover/env"}, {"POST", "/api/discover/save"}, {"POST", "/api/discover/test"}, {"GET", "/api/discover/shipyard"},
		{"GET", "/api/discover/shipyard/boxes/x"}, {"DELETE", "/api/discover/scan"},
	} {
		for _, header := range [][2]string{{"Origin", "https://evil.example"}, {"Host", "evil.example"}} {
			req, err := http.NewRequest(route.method, c.base+route.path, strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			if header[0] == "Host" {
				req.Host = header[1]
			} else {
				req.Header.Set(header[0], header[1])
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("%s %s with %s %s: %d, want 403", route.method, route.path, header[0], header[1], resp.StatusCode)
			}
		}
	}
	var scan struct{}
	if status := c.do("GET", "/api/discover/scan", "", &scan); status != http.StatusNotFound {
		t.Errorf("a refused request started a scan: %d", status)
	}
}

// A scan asked for through a forwarded port advises with the daemon's own
// address: `leadline run --url` and the plugin's LEADLINE_URL are for the
// daemon's machine, where the forwarded port does not exist.
func TestAScanThroughAForwardNamesTheDaemonsOwnAddress(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, "home", "acme")
	writeTree(t, home, map[string]string{
		"code/acme/package.json":       `{"name":"acme","scripts":{"dev":"next dev"},"dependencies":{"next":"^16.0.1"}}`,
		"code/acme-admin/package.json": `{"name":"acme-admin","scripts":{"dev":"vite"},"devDependencies":{"vite":"^7.1.4"}}`,
	})
	c := discoverClient(t, home, "")
	const through = "127.0.0.1:14320"
	body, _ := json.Marshal(map[string]any{"folders": []string{filepath.Join(home, "code")}})
	forwarded(t, c.base, "POST", "/api/discover/scan", through, string(body))
	var scan discover.Scan
	raw := forwarded(t, c.base, "GET", "/api/discover/scan?wait_ms=20000", through, "")
	if err := json.Unmarshal([]byte(raw), &scan); err != nil || scan.State != "done" || len(scan.Projects) != 2 {
		t.Fatalf("scan: %v %s", err, raw)
	}
	if strings.Contains(raw, "14320") {
		t.Errorf("the scan names the forwarded port: %s", raw)
	}
	for _, p := range scan.Projects {
		switch p.Name {
		case "acme":
			if p.Connect.Kind != "next-plugin" || len(p.Connect.EnvLines) != 1 || p.Connect.EnvLines[0] != "LEADLINE_URL="+c.base {
				t.Errorf("acme: %+v", p.Connect)
			}
		case "acme-admin":
			if p.Connect.Command != "leadline run --url "+c.base+" -- npm run dev" {
				t.Errorf("acme-admin: %+v", p.Connect)
			}
		}
	}
}
