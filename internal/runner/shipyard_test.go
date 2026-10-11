//go:build unix

package runner

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func envOf(pairs ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = pairs[i+1]
	}
	return func(key string) string { return m[key] }
}

// The worktree is read from Shipyard's variables, both the ones a worktree's
// own processes get and the ones a hook gets for its event.
func TestWorktreeFromEnv(t *testing.T) {
	// What everything Shipyard starts in a worktree has.
	inside := []string{"BERTH_BOX", "devl", "BERTH_LOCATION", "acme", "BERTH_ROOT_PATH", "/home/dev/work/acme",
		"BERTH_WORKTREE_PATH", "/home/dev/work/acme-fix-orders", "BERTH_WORKTREE_NAME", "fix-orders", "BERTH_BRANCH", "fix-orders", "BERTH_PORT", "4100"}
	main := []string{"BERTH_BOX", "devl", "BERTH_LOCATION", "acme", "BERTH_ROOT_PATH", "/home/dev/work/acme",
		"BERTH_WORKTREE_PATH", "/home/dev/work/acme", "BERTH_WORKTREE_NAME", "acme", "BERTH_BRANCH", "main"}
	for _, tc := range []struct {
		name    string
		env     []string
		want    Worktree
		service string
	}{
		{"a terminal, agent or service in a worktree", inside,
			Worktree{Via: "shipyard", Project: "acme", Name: "fix-orders", Branch: "fix-orders", Box: "devl", Path: "/home/dev/work/acme-fix-orders"}, "acme/fix-orders"},
		{"the main checkout", main,
			Worktree{Via: "shipyard", Project: "acme", Branch: "main", Box: "devl", Path: "/home/dev/work/acme"}, "acme"},
		{"a repository's hook for worktree.created: the event and the worktree agree",
			append([]string{"BERTH_EVENT", "worktree.created", "BERTH_EVENT_BOX", "devl", "BERTH_NAME", "fix-orders", "BERTH_PATH", "/home/dev/work/acme-fix-orders"}, inside...),
			Worktree{Via: "shipyard", Project: "acme", Name: "fix-orders", Branch: "fix-orders", Box: "devl", Path: "/home/dev/work/acme-fix-orders"}, "acme/fix-orders"},
		// The worktree is gone, so Shipyard runs the repository's hook in the
		// main checkout, with the main checkout's variables. The event is
		// what names the worktree; the main checkout's branch is not its.
		{"a repository's hook for worktree.removed",
			append([]string{"BERTH_EVENT", "worktree.removed", "BERTH_EVENT_BOX", "devl", "BERTH_NAME", "fix-orders", "BERTH_PATH", "/home/dev/work/acme-fix-orders"}, main...),
			Worktree{Via: "shipyard", Project: "acme", Name: "fix-orders", Box: "devl", Path: "/home/dev/work/acme-fix-orders"}, "acme/fix-orders"},
		{"a box's own hook: the event and nothing else",
			[]string{"BERTH_EVENT", "worktree.created", "BERTH_EVENT_BOX", "devl", "BERTH_EVENT_ORIGIN", "app", "BERTH_LOCATION", "acme", "BERTH_NAME", "fix-orders",
				"BERTH_PATH", "/home/dev/work/acme-fix-orders", "BERTH_BRANCH", "fix-orders"},
			Worktree{Via: "shipyard", Project: "acme", Name: "fix-orders", Branch: "fix-orders", Box: "devl", Path: "/home/dev/work/acme-fix-orders"}, "acme/fix-orders"},
		// Another event's "name" is not a worktree's.
		{"a hook for a session's event, run in a worktree",
			append([]string{"BERTH_EVENT", "session.started", "BERTH_NAME", "claude-1"}, inside...),
			Worktree{Via: "shipyard", Project: "acme", Name: "fix-orders", Branch: "fix-orders", Box: "devl", Path: "/home/dev/work/acme-fix-orders"}, "acme/fix-orders"},
		{"names that would break the joins", []string{"BERTH_LOCATION", "acme shop", "BERTH_WORKTREE_NAME", "feat/fix:orders"},
			Worktree{Via: "shipyard", Project: "acme shop", Name: "feat/fix:orders"}, "acme-shop/feat-fix-orders"},
	} {
		got, ok := WorktreeFromEnv(envOf(tc.env...))
		if !ok || got != tc.want || got.Service() != tc.service {
			t.Errorf("%s:\n got %+v (%q, %v)\nwant %+v (%q)", tc.name, got, got.Service(), ok, tc.want, tc.service)
		}
	}
	if w, ok := WorktreeFromEnv(envOf("BERTH_ORIGIN", "hook", "PATH", "/usr/bin")); ok {
		t.Errorf("an environment that is not Shipyard's gave %+v", w)
	}
	if hook, named := InHook(envOf("BERTH_EVENT", "agent.finished", "BERTH_PATH", "/x")); !hook || named {
		t.Errorf("agent.finished: hook %v, names a worktree %v", hook, named)
	}
	if hook, named := InHook(envOf("BERTH_EVENT", "worktree.removed", "BERTH_NAME", "fix-orders")); !hook || !named {
		t.Errorf("worktree.removed: hook %v, names a worktree %v", hook, named)
	}

	w := Worktree{Project: "acme", Name: "fix-orders"}
	for name, want := range map[string]string{"": "acme/fix-orders", "worker": "acme/fix-orders:worker", " acme/fix-orders ": "acme/fix-orders", "a b": "acme/fix-orders:a-b"} {
		if got := w.ServiceFor(name); got != want {
			t.Errorf("ServiceFor(%q) = %q, want %q", name, got, want)
		}
	}
	if got := resourceAttributes("deployment.environment=dev,shipyard.box=mine", (Worktree{Project: "acme shop", Name: "fix-orders", Branch: "feat/x", Box: "devl"}).Tags()); got != "deployment.environment=dev,shipyard.box=mine,shipyard.project=acme%20shop,shipyard.worktree=fix-orders,shipyard.branch=feat%2Fx" {
		t.Errorf("resource attributes: %q", got)
	}
}

// By hand, outside Shipyard, git says which worktree a folder is.
func TestWorktreeAt(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "acme")
	linked := filepath.Join(root, "acme-fix-orders")
	write(t, filepath.Join(main, ".git", "HEAD"), "ref: refs/heads/main\n")
	write(t, filepath.Join(linked, ".git"), "gitdir: "+filepath.Join(main, ".git", "worktrees", "acme-fix-orders")+"\n")
	plain := filepath.Join(root, "notes")
	write(t, filepath.Join(plain, "README"), "")

	for dir, want := range map[string]string{main: "acme", linked: "acme/fix-orders", plain: "notes"} {
		w := WorktreeAt(dir)
		if w.Service() != want || w.Via != "cli" || w.Path != dir {
			t.Errorf("%s: %+v gives %q, want %q", dir, w, w.Service(), want)
		}
	}
}

// The URLs of a worktree's databases are found in its environment and its
// .env files, and nothing shown of them has a password in it.
func TestFindConnURLs(t *testing.T) {
	const secret = "acme-s3cret"
	dir := t.TempDir()
	write(t, filepath.Join(dir, ".env"), strings.Join([]string{
		"DATABASE_URL=postgres://app:" + secret + "@localhost:5432/acme_fix_orders",
		"DIRECT_URL=${DATABASE_URL}",
		"REDIS_URL='redis://:" + secret + "@127.0.0.1:6379/2'",
		"API_URL=https://api.acme.example/v1",
		"SESSION_SECRET=" + secret,
		"PYTHON_URL=postgresql+psycopg://app:" + secret + "@db.acme.example/acme",
		"JDBC_URL=jdbc:postgresql://localhost:5432/acme",
	}, "\n")+"\n")
	write(t, filepath.Join(dir, ".env.local"), "CACHE_URL=rediss://cache.acme.example\n")
	env := []string{"PATH=/usr/bin", "DATABASE_URL=postgres://app:" + secret + "@localhost:5432/acme_from_env", "HOME=/home/dev"}
	dotenv := ReadDotenv(dir, "", func(key string) string { return getEnv(env, key) })
	if !reflect.DeepEqual(dotenv.Files, []string{".env.local", ".env"}) {
		t.Errorf("files read: %v", dotenv.Files)
	}

	got := FindConnURLs(env, dotenv)
	var shown []string
	for _, c := range got {
		shown = append(shown, strings.Join(c.Keys, "+")+" "+c.String())
		if strings.Contains(c.String()+c.Addr+c.Database+strings.Join(c.Keys, ""), secret) {
			t.Errorf("%s shows a password", c.Keys)
		}
	}
	want := []string{
		"CACHE_URL Redis cache.acme.example:6379/0",
		// The environment outranks the file, and ${DATABASE_URL} follows it.
		"DATABASE_URL+DIRECT_URL Postgres localhost:5432/acme_from_env",
		"PYTHON_URL Postgres db.acme.example:5432/acme",
		"REDIS_URL Redis 127.0.0.1:6379/2",
	}
	if !reflect.DeepEqual(shown, want) {
		t.Errorf("found:\n%s\nwant:\n%s", strings.Join(shown, "\n"), strings.Join(want, "\n"))
	}
	if got[2].Local || !got[1].Local || !got[3].Local {
		t.Errorf("local: %+v", got)
	}
	// SQLAlchemy's driver suffix is not part of the URL the daemon gets.
	if !strings.HasPrefix(got[2].raw, "postgresql://app:") {
		t.Errorf("the scheme was not made plain: %q", strings.Replace(got[2].raw, secret, "…", 1))
	}

	if c, err := ParseConnFlag("DATABASE_URL=postgres://app:" + secret + "@localhost/acme?sslmode=disable"); err != nil || c.Keys[0] != "DATABASE_URL" || c.Addr != "localhost:5432" {
		t.Errorf("--url: %+v, %v", c, err)
	}
	for _, bad := range []string{"postgres://app:" + secret + "@localhost/acme", "DATABASE_URL", "X=https://app:" + secret + "@acme.example", "=redis://localhost"} {
		_, err := ParseConnFlag(bad)
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Errorf("--url %q: %v", strings.Replace(bad, secret, "…", 1), err)
		}
	}
}

// attachedStub is a daemon that also knows one attached worktree.
type attachedStub struct {
	*stub
	server *httptest.Server
	mu     sync.Mutex
	asked  []string
	// What GET /api/attach?service= answers, by service; none is a 404.
	attached map[string]string
}

func newAttachedStub(t *testing.T) *attachedStub {
	t.Helper()
	a := &attachedStub{stub: newStub(t), attached: map[string]string{}}
	inner := a.stub.Config.Handler
	a.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/attach" {
			inner.ServeHTTP(w, r)
			return
		}
		service := r.URL.Query().Get("service")
		a.mu.Lock()
		a.asked = append(a.asked, r.Method+" "+service)
		body, ok := a.attached[service]
		a.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":{"code":"not_found","message":"There is no attached source with that id."}}`)
			return
		}
		io.WriteString(w, body)
	}))
	t.Cleanup(a.server.Close)
	return a
}

func readEnvFile(t *testing.T, path string) map[string]string {
	t.Helper()
	env := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			env[k] = v
		}
	}
	return env
}

// In a worktree Shipyard made, a run is the worktree's: named as attach
// names it, tagged with where it is from, and pointed at the proxy the
// worktree already has, which is neither opened nor closed for it.
func TestRunInAnAttachedWorktree(t *testing.T) {
	const secret = "acme-s3cret"
	d := newAttachedStub(t)
	d.attached["acme/fix-orders"] = `{"source":{"service":"acme/fix-orders"},"connections":[
		{"id":"c_1","kind":"postgres","host":"127.0.0.1:5432","database":"acme_fix_orders","capture":true,"env":["DATABASE_URL"],
		 "proxy":{"listen":"127.0.0.1:15440","state":"listening"}},
		{"id":"c_2","kind":"redis","host":"127.0.0.1:6379","database":"0","capture":true,"env":["REDIS_URL"]}]}`
	dir := t.TempDir()
	out := filepath.Join(dir, "env.txt")
	// The project names itself, the same in every worktree.
	write(t, filepath.Join(dir, "package.json"), `{"name":"acme-web"}`)
	write(t, filepath.Join(dir, ".env"), "OTEL_SERVICE_NAME=acme-web\nDATABASE_URL=postgres://app:"+secret+"@localhost:5432/acme_fix_orders\n")
	shipyard := []string{"BERTH_BOX=devl", "BERTH_LOCATION=acme", "BERTH_ROOT_PATH=/home/dev/work/acme", "BERTH_WORKTREE_PATH=" + dir,
		"BERTH_WORKTREE_NAME=fix-orders", "BERTH_WORKTREE_SLUG=acme_fix_orders", "BERTH_BRANCH=fix/orders", "BERTH_PORT=4100"}
	code, stdout, stderr := piped(t, Options{
		URL: d.server.URL, Dir: dir,
		Env: minimalEnv(append(shipyard,
			// A second database on a server the worktree has no proxy for.
			"ANALYTICS_URL=postgres://app:"+secret+"@127.0.0.1:5433/analytics",
			"OTEL_RESOURCE_ATTRIBUTES=deployment.environment=dev")...),
		Command: sh(`env > "` + out + `"; echo "listening on 4100"`),
	})
	if code != 0 || stdout != "listening on 4100\n" {
		t.Fatalf("code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	env := readEnvFile(t, out)
	for key, want := range map[string]string{
		// The worktree's name, over the project's own.
		"OTEL_SERVICE_NAME":        "acme/fix-orders",
		"OTEL_RESOURCE_ATTRIBUTES": "deployment.environment=dev,shipyard.project=acme,shipyard.worktree=fix-orders,shipyard.branch=fix%2Forders,shipyard.box=devl",
		// The worktree's own proxy, on the port it always has.
		"DATABASE_URL": "postgres://app:" + secret + "@127.0.0.1:15440/acme_fix_orders",
		// No proxy of the worktree's is in front of this one: one for the run.
		"ANALYTICS_URL": "postgres://app:" + secret + "@127.0.0.1:61001/analytics",
	} {
		if env[key] != want {
			t.Errorf("%s = %q, want %q", key, strings.ReplaceAll(env[key], secret, "…"), strings.ReplaceAll(want, secret, "…"))
		}
	}

	d.stub.mu.Lock()
	asked, removed, started, logs := d.stub.asked, d.stub.removed, d.stub.started, d.stub.logs
	d.stub.mu.Unlock()
	// Only the database the worktree has no proxy for was asked for, under
	// the worktree's name, and only that one given back.
	if !reflect.DeepEqual(asked, []map[string]string{{"kind": "postgres", "upstream": "127.0.0.1:5433", "label": "acme/fix-orders"}}) {
		t.Errorf("proxies asked for: %v", asked)
	}
	if !reflect.DeepEqual(removed, []string{"p1"}) {
		t.Errorf("proxies given back: %v", removed)
	}
	d.mu.Lock()
	lookups := d.asked
	d.mu.Unlock()
	if !reflect.DeepEqual(lookups, []string{"GET acme/fix-orders"}) {
		t.Errorf("attachment lookups: %v", lookups)
	}
	if len(started) != 1 || started[0].Service != "acme/fix-orders" || len(started[0].Proxies) != 2 ||
		started[0].Proxies[1].Env != "DATABASE_URL" || started[0].Proxies[1].Listen != "127.0.0.1:15440" {
		t.Errorf("run: %+v", started)
	}
	if len(logs) != 1 || logs[0].Service != "acme/fix-orders" || logs[0].Body != "listening on 4100" ||
		logs[0].Attrs["shipyard.project"] != "acme" || logs[0].Attrs["shipyard.worktree"] != "fix-orders" ||
		logs[0].Attrs["shipyard.branch"] != "fix/orders" || logs[0].Attrs["shipyard.box"] != "devl" {
		t.Errorf("logs: %+v", logs)
	}
	note := strings.TrimSpace(stderr)
	if strings.Count(note, "\n") != 0 || strings.Contains(note, secret) ||
		!strings.Contains(note, "leadline: acme/fix-orders → ") ||
		!strings.Contains(note, "Postgres localhost:5432 through 127.0.0.1:15440 (DATABASE_URL, attached)") ||
		!strings.Contains(note, "Postgres 127.0.0.1:5433 through 127.0.0.1:61001 (ANALYTICS_URL)") {
		t.Errorf("note: %q", note)
	}
	if b, _ := json.Marshal(started); strings.Contains(string(b), secret) {
		t.Errorf("the run's record holds a password: %s", b)
	}
}

// A worktree nobody attached still files under the worktree's name: its
// proxy is one for the run, asked for by that name. And --name there is the
// name of one command among the worktree's.
func TestRunInAWorktreeThatIsNotAttached(t *testing.T) {
	d := newAttachedStub(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "env.txt")
	shipyard := []string{"BERTH_LOCATION=acme", "BERTH_WORKTREE_NAME=add-refunds", "BERTH_WORKTREE_PATH=" + dir,
		"DATABASE_URL=postgres://app@localhost:5432/acme_add_refunds"}
	code, _, stderr := piped(t, Options{URL: d.server.URL, Dir: dir, Name: "worker", Env: minimalEnv(shipyard...),
		Command: sh(`env > "` + out + `"; echo started`)})
	if code != 0 {
		t.Fatalf("code %d: %s", code, stderr)
	}
	env := readEnvFile(t, out)
	if env["OTEL_SERVICE_NAME"] != "acme/add-refunds:worker" || env["DATABASE_URL"] != "postgres://app@127.0.0.1:61001/acme_add_refunds" ||
		env["OTEL_RESOURCE_ATTRIBUTES"] != "shipyard.project=acme,shipyard.worktree=add-refunds" {
		t.Errorf("env: service %q, database %q, resource %q", env["OTEL_SERVICE_NAME"], env["DATABASE_URL"], env["OTEL_RESOURCE_ATTRIBUTES"])
	}
	d.stub.mu.Lock()
	asked, logs := d.stub.asked, d.stub.logs
	d.stub.mu.Unlock()
	// The queries are the worktree's whichever command sent them.
	if !reflect.DeepEqual(asked, []map[string]string{{"kind": "postgres", "upstream": "localhost:5432", "label": "acme/add-refunds"}}) {
		t.Errorf("proxies asked for: %v", asked)
	}
	if len(logs) != 1 || logs[0].Service != "acme/add-refunds:worker" {
		t.Errorf("logs: %+v", logs)
	}

	// Outside Shipyard nothing has changed: the project's name, no lookup.
	d2 := newAttachedStub(t)
	write(t, filepath.Join(dir, "package.json"), `{"name":"acme-web"}`)
	code, _, _ = piped(t, Options{URL: d2.server.URL, Dir: dir, Env: minimalEnv("DATABASE_URL=postgres://app@localhost:5432/acme"),
		Command: sh(`env > "` + out + `"`)})
	env = readEnvFile(t, out)
	if _, tagged := env["OTEL_RESOURCE_ATTRIBUTES"]; code != 0 || env["OTEL_SERVICE_NAME"] != "acme-web" || tagged {
		t.Errorf("outside Shipyard: code %d, service %q, resource attributes set: %v", code, env["OTEL_SERVICE_NAME"], tagged)
	}
	d2.mu.Lock()
	if len(d2.asked) != 0 {
		t.Errorf("a run outside Shipyard asked about attachments: %v", d2.asked)
	}
	d2.mu.Unlock()
}
