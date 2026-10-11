package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/sean-brydon/leadline/internal/brief"
	"github.com/sean-brydon/leadline/internal/demo"
	"github.com/sean-brydon/leadline/internal/discover"
	"github.com/sean-brydon/leadline/internal/discover/fakeberth"
	"github.com/sean-brydon/leadline/internal/store"
)

// briefServer is a daemon holding the demo's traces and logs, whose
// Shipyard is the fake one in dir ("" for a machine without Shipyard).
func briefServer(t *testing.T, withShipyard bool) (base, dir string) {
	t.Helper()
	cli := ""
	if withShipyard {
		dir = t.TempDir()
		var err error
		if cli, err = fakeberth.Write(dir); err != nil {
			t.Fatal(err)
		}
	}
	discoverOptions = func(o *discover.Options) {
		o.Processes = discover.NoProcesses()
		o.Home = t.TempDir()
		o.Berth = func() string { return cli }
		o.BerthTimeout, o.TaskTimeout = time.Minute, time.Minute
	}
	t.Cleanup(func() { discoverOptions = nil })
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(New(st, nil).Handler())
	t.Cleanup(func() {
		ts.Close()
		st.Close()
	})
	now := time.Now()
	traces, logs := demo.New(7).Batch(600, now.Add(-10*time.Minute), now.Add(-time.Second))
	for path, msg := range map[string]proto.Message{"/v1/traces": traces, "/v1/logs": logs} {
		body, _ := proto.Marshal(msg)
		resp, err := http.Post(ts.URL+path, "application/x-protobuf", bytes.NewReader(body))
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("POST %s: %v", path, err)
		}
		resp.Body.Close()
	}
	return ts.URL, dir
}

type problemBody struct {
	Error struct{ Code, Message, Hint string } `json:"error"`
}

// send posts JSON and returns the status, and the answer decoded into into
// or, for a refusal, into a problem.
func send(t *testing.T, url string, body any, into any) (int, problemBody) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var p problemBody
	if resp.StatusCode == http.StatusOK {
		if into != nil {
			if err := json.Unmarshal(raw, into); err != nil {
				t.Fatalf("POST %s: %v in %s", url, err, raw)
			}
		}
	} else if err := json.Unmarshal(raw, &p); err != nil || p.Error.Code == "" {
		t.Fatalf("POST %s: %s is not a problem: %s", url, resp.Status, raw)
	}
	return resp.StatusCode, p
}

// The four briefs over HTTP, each found the way the app asks for it and
// the way the command line does.
func TestBriefRoutes(t *testing.T) {
	base, dir := briefServer(t, true)
	const shape = "SELECT id, product_id, quantity FROM order_items WHERE order_id = ?"

	var q brief.Brief
	if code, raw := fetch(t, base, "/api/brief/query", url.Values{"db_system": {"postgresql"}, "fingerprint": {shape}}, &q); code != 200 {
		t.Fatalf("query: %d %s", code, raw)
	}
	if q.Kind != "query" || q.Problem != "n_plus_one" || q.Service != "acme-api" || !strings.Contains(q.Prompt, shape) || q.Evidence.Query == nil || q.Evidence.Query.Calls == 0 ||
		q.Source == nil || q.Source.Service != "acme-api" || q.Example.Included || !q.Example.Available || len(q.Recorded) != 1 || !brief.TaskName.MatchString(q.TaskName) {
		t.Errorf("query brief = %+v", q)
	}
	// The same one by a search, and with a real statement when asked.
	var found brief.Brief
	fetch(t, base, "/api/brief/query", url.Values{"q": {"order_items"}, "example": {"1"}}, &found)
	if found.Title != q.Title || !found.Example.Included || !strings.Contains(found.Prompt, "One real statement") || found.Evidence.Query.Example == "" {
		t.Errorf("searched query brief = %q, example %+v", found.Title, found.Example)
	}
	// Each of a brief's names is its own: two are two tasks.
	if found.TaskName == q.TaskName {
		t.Errorf("two briefs share the task name %q", q.TaskName)
	}

	var e brief.Brief
	if code, raw := fetch(t, base, "/api/brief/endpoint", url.Values{"endpoint_service": {"acme-api"}, "name": {"POST /api/checkout"}}, &e); code != 200 {
		t.Fatalf("endpoint: %d %s", code, raw)
	}
	if e.Kind != "endpoint" || e.Problem != "failing" || e.Evidence.Endpoint == nil || len(e.Evidence.Endpoint.Failing) == 0 || len(e.Evidence.Code) != 1 {
		t.Errorf("endpoint brief = %s %s %+v", e.Kind, e.Problem, e.Evidence)
	}
	fetch(t, base, "/api/brief/endpoint", url.Values{"q": {"orders"}}, &found)
	if found.Problem != "n_plus_one" || found.Evidence.Endpoint.Name != "GET /api/orders" {
		t.Errorf("searched endpoint brief = %s %+v", found.Problem, found.Evidence.Endpoint)
	}

	id := e.Evidence.Endpoint.Failing[0].TraceID
	var tr brief.Brief
	if code, raw := fetch(t, base, "/api/brief/trace", url.Values{"id": {id}}, &tr); code != 200 {
		t.Fatalf("trace: %d %s", code, raw)
	}
	if tr.Kind != "trace" || tr.Problem != "failing" || tr.Evidence.Trace.TraceID != id || tr.Evidence.Trace.Failed != 2 {
		t.Errorf("trace brief = %s %s %+v", tr.Kind, tr.Problem, tr.Evidence.Trace)
	}

	// A log record, named as the logs list names it.
	var logs []store.LogRow
	fetch(t, base, "/api/logs", url.Values{"q": {"checkout failed"}, "min_severity": {"17"}}, &logs)
	if len(logs) == 0 || logs[0].Cursor == "" {
		t.Fatalf("no checkout error in the logs list")
	}
	var l brief.Brief
	if code, raw := fetch(t, base, "/api/brief/log", url.Values{"cursor": {logs[0].Cursor}, "log_service": {logs[0].Service}}, &l); code != 200 {
		t.Fatalf("log: %d %s", code, raw)
	}
	if l.Kind != "log" || l.Evidence.Log.Count != int64(len(logs)) || l.Evidence.Log.TraceID != logs[0].TraceID || l.Evidence.Log.TsMs != logs[0].TsMs {
		t.Errorf("log brief = %+v, the list has %d", l.Evidence.Log, len(logs))
	}
	fetch(t, base, "/api/brief/log", url.Values{"q": {"charge failed"}}, &found)
	if found.Evidence.Log.Service != "acme-payments" {
		t.Errorf("searched log brief = %+v", found.Evidence.Log)
	}

	// What is not there, and what was not asked for properly.
	for path, want := range map[string]int{
		"/api/brief/query?fingerprint=SELECT+nothing&db_system=postgresql": 404,
		"/api/brief/query?q=no_such_table":                                 404,
		"/api/brief/query":                                                 400,
		"/api/brief/endpoint?name=GET+/nope&endpoint_service=acme-api":     404,
		"/api/brief/endpoint":                                              400,
		"/api/brief/trace?id=00000000000000000000000000000000":             404,
		"/api/brief/trace":                                                 400,
		"/api/brief/log?cursor=nonsense":                                   400,
		"/api/brief/log?cursor=1.1":                                        404,
		"/api/brief/log?q=no+such+words":                                   404,
	} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		var p problemBody
		json.NewDecoder(resp.Body).Decode(&p)
		resp.Body.Close()
		if resp.StatusCode != want || p.Error.Message == "" || (want == 404) != (p.Error.Code == "not_found") {
			t.Errorf("GET %s: %s %+v, want %d", path, resp.Status, p, want)
		}
	}

	// Not one of those requests had the daemon run Shipyard's command line.
	if runs := fakeberth.Runs(dir); len(runs) != 0 {
		t.Errorf("writing briefs ran berth: %q", runs)
	}
}

// A brief says where its service's code is, from what this daemon knows:
// the worktree that attached it, and the folder its command ran in. The
// targets route then turns that into a box and a project.
func TestBriefSourceAndTargets(t *testing.T) {
	base, dir := briefServer(t, true)

	// Nothing known of acme-api: the person will pick.
	var q brief.Brief
	fetch(t, base, "/api/brief/query", url.Values{"q": {"order_items"}}, &q)
	if q.Source == nil || q.Source.Service != "acme-api" || q.Source.Via != "" || q.Source.Project != "" || q.Source.Path != "" || len(q.Source.Folders) != 0 {
		t.Errorf("source = %+v", q.Source)
	}
	var targets discover.Targets
	fetch(t, base, "/api/shipyard/targets", url.Values{"service": {"acme-api"}}, &targets)
	if !targets.Available || targets.Suggested != nil || targets.Box != "this-mac" || len(targets.Projects) != 3 || len(targets.Agents) != 2 || len(targets.Boxes) != 3 {
		t.Errorf("targets with nothing known = %+v", targets)
	}

	// A `leadline run` of it from a project's folder.
	if code := postJSON(t, base+"/api/runs", map[string]any{"command": "pnpm dev", "cwd": "/home/dev/storefront/apps/api", "service": "acme-api", "pid": 4242}, nil); code != 200 {
		t.Fatalf("POST /api/runs: %d", code)
	}
	fetch(t, base, "/api/brief/query", url.Values{"q": {"order_items"}}, &q)
	if len(q.Source.Folders) != 1 || q.Source.Folders[0] != "/home/dev/storefront/apps/api" {
		t.Errorf("source after a run = %+v", q.Source)
	}
	fetch(t, base, "/api/shipyard/targets", url.Values{"service": {"acme-api"}}, &targets)
	if targets.Suggested == nil || targets.Suggested.Project != "storefront" || targets.Suggested.How != "folder" || targets.Suggested.Base != "" || targets.Box != "this-mac" {
		t.Errorf("targets after a run = %+v", targets.Suggested)
	}

	// A worktree Shipyard attached.
	code, p := send(t, base+"/api/attach", map[string]any{"service": "acme/fix-orders", "via": "shipyard", "project": "acme", "worktree": "fix-orders", "branch": "fix-orders",
		"box": "hetzner-1", "path": "/home/dev/acme-fix-orders", "connections": []any{}}, nil)
	if code != 200 {
		t.Fatalf("POST /api/attach: %d %+v", code, p)
	}
	targets = discover.Targets{}
	fetch(t, base, "/api/shipyard/targets", url.Values{"service": {"acme/fix-orders:worker"}}, &targets)
	if targets.Suggested == nil || *targets.Suggested != (discover.Target{Box: "hetzner-1", Project: "acme", Base: "fix-orders", How: "worktree",
		Why:     "acme/fix-orders:worker is the worktree fix-orders of acme on hetzner-1, attached by Shipyard.",
		BaseWhy: "The problem was recorded running the branch fix-orders, so the task starts from it. What is not committed there is not in it."}) {
		t.Errorf("targets for an attached worktree = %+v", targets.Suggested)
	}

	// The same, for a service another daemon holds: the caller says what
	// that daemon knows, and names a box to list.
	targets = discover.Targets{}
	code, p = send(t, base+"/api/shipyard/targets", map[string]any{
		"source": map[string]any{"service": "storefront-web", "folders": []string{"/home/dev/storefront"}, "remote": true}, "box": "hetzner-1"}, &targets)
	if code != 200 || targets.Suggested == nil || targets.Suggested.Box != "hetzner-1" || targets.Suggested.Project != "storefront" || targets.Box != "hetzner-1" {
		t.Errorf("targets for another daemon's service = %d %+v %+v", code, p, targets.Suggested)
	}
	// And another box's projects, when the person turns to it.
	targets = discover.Targets{}
	fetch(t, base, "/api/shipyard/targets", url.Values{"box": {"build-box"}}, &targets)
	if targets.Box != "build-box" || targets.BoxProblem == nil || targets.BoxProblem.Message != "The box build-box is offline." || len(targets.Projects) != 0 {
		t.Errorf("an offline box = %+v", targets)
	}

	// All of that read, and only read.
	for _, run := range fakeberth.Runs(dir) {
		if w := strings.Fields(run)[0]; w != "boxes" && w != "locations" && w != "agents" {
			t.Errorf("berth ran %q", run)
		}
	}
	if len(fakeberth.Tasks(dir)) != 0 {
		t.Errorf("a task was started by a route that lists")
	}
}

// Sending: the one route that starts something. What the fake berth was
// run with is what was posted, to the byte.
func TestShipyardTask(t *testing.T) {
	base, dir := briefServer(t, true)
	// The prompt is a brief, with the worst a person could type added.
	var q brief.Brief
	fetch(t, base, "/api/brief/query", url.Values{"q": {"order_items"}, "example": {"1"}}, &q)
	prompt := q.Prompt + "\n--agent codex\n$(rm -rf ~) `whoami` \"quoted\" 'single' \\n 100% ${HOME}\n--\nlast line without a newline"

	var got discover.TaskResult
	code, p := send(t, base+"/api/shipyard/tasks", map[string]any{"box": "hetzner-1", "project": "acme", "name": q.TaskName, "prompt": prompt, "base": "fix-orders"}, &got)
	if code != 200 {
		t.Fatalf("POST /api/shipyard/tasks: %d %+v", code, p)
	}
	tasks := fakeberth.Tasks(dir)
	want := []string{"task", "new", "hetzner-1/acme/" + q.TaskName, "--agent", "claude", "--open", "tab", "--base", "fix-orders", "--json", "--prompt=" + prompt}
	if len(tasks) != 1 || len(tasks[0]) != len(want) {
		t.Fatalf("berth task new ran with %q", tasks)
	}
	for i := range want {
		if tasks[0][i] != want[i] {
			t.Errorf("argument %d:\n got %q\nwant %q", i, tasks[0][i], want[i])
		}
	}
	if got.Ref != "hetzner-1/acme/"+q.TaskName || got.Service != "acme/"+q.TaskName || got.Agent != "claude" || got.Session == "" || got.PromptBytes != len(prompt) || got.Base != "fix-orders" {
		t.Errorf("result = %+v", got)
	}

	// Refusals, in words, with a status that says whose fault it is.
	for _, c := range []struct {
		body    map[string]any
		status  int
		code    string
		message string
	}{
		{map[string]any{"box": "build-box", "project": "acme", "name": "fix-a", "prompt": "p"}, 503, "box_offline", "The box build-box is offline."},
		{map[string]any{"box": "hetzner-1", "project": "acme", "name": "fix-orders", "prompt": "p"}, 409, "worktree_exists", "A worktree named fix-orders already exists in acme on hetzner-1."},
		{map[string]any{"box": "hetzner-1", "project": "shop", "name": "fix-a", "prompt": "p"}, 404, "no_project", "Shipyard has no project named shop on hetzner-1."},
		{map[string]any{"box": "hetzner-1", "project": "acme", "name": "Fix A", "prompt": "p"}, 400, "bad_name", "That is not a name a task can have."},
		{map[string]any{"box": "hetzner-1", "project": "acme", "name": "fix-a", "prompt": ""}, 400, "no_prompt", "There is no prompt to send."},
		{map[string]any{"box": "hetzner-1", "project": "acme", "name": "fix-a", "prompt": strings.Repeat("a", discover.MaxPrompt+1)}, 413, "prompt_too_long", "The prompt is 65 KB, and 64 KB is the most that can be sent."},
	} {
		status, p := send(t, base+"/api/shipyard/tasks", c.body, nil)
		if status != c.status || p.Error.Code != c.code || p.Error.Message != c.message {
			t.Errorf("%v: %d %+v, want %d %s", c.body["name"], status, p.Error, c.status, c.code)
		}
	}
	if n := len(fakeberth.Tasks(dir)); n != 1 {
		t.Errorf("a refused task was started: %d runs", n)
	}

	// Only JSON that says it is JSON starts a task: a form a web page
	// submits is refused, as is a page from anywhere but this machine.
	form := url.Values{"box": {"hetzner-1"}, "project": {"acme"}, "name": {"fix-form"}, "prompt": {"p"}}
	resp, err := http.PostForm(base+"/api/shipyard/tasks", form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("a form: %s", resp.Status)
	}
	body, _ := json.Marshal(map[string]any{"box": "hetzner-1", "project": "acme", "name": "fix-plain", "prompt": "p"})
	resp, _ = http.Post(base+"/api/shipyard/tasks", "text/plain", bytes.NewReader(body))
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("JSON sent as text/plain: %s", resp.Status)
	}
	req, _ := http.NewRequest(http.MethodPost, base+"/api/shipyard/tasks", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("another site's page: %s", resp.Status)
	}
	// A page on this machine that isn't Leadline's own window is refused
	// too: the app being debugged is such a page, and must not be able to
	// start an agent. Leadline's window and its dev server get through to
	// the body check.
	for origin, want := range map[string]int{
		"http://localhost:3000":  http.StatusForbidden,
		"http://127.0.0.1:1546":  http.StatusForbidden,
		"tauri://localhost":      http.StatusBadRequest,
		"http://localhost:1520":  http.StatusBadRequest,
		"http://tauri.localhost": http.StatusBadRequest,
	} {
		req, _ := http.NewRequest(http.MethodPost, base+"/api/shipyard/tasks", strings.NewReader("{not json"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", origin)
		resp, _ := http.DefaultClient.Do(req)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("a page at %s: %s, want %d", origin, resp.Status, want)
		}
	}
	resp, _ = http.Post(base+"/api/shipyard/tasks", "application/json", strings.NewReader("{not json"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a body that is not JSON: %s", resp.Status)
	}
	if n := len(fakeberth.Tasks(dir)); n != 1 {
		t.Errorf("one of those started a task: %d runs", n)
	}
	// In the whole test, berth was asked to start a task once.
	started := 0
	for _, run := range fakeberth.Runs(dir) {
		if strings.HasPrefix(run, "task ") {
			started++
		}
	}
	if started != 1 {
		t.Errorf("berth task ran %d times: %q", started, fakeberth.Runs(dir))
	}
}

// Without Shipyard the briefs are all there is, and they still are: the
// targets say so, and sending says so.
func TestBriefWithoutShipyard(t *testing.T) {
	base, _ := briefServer(t, false)
	var q brief.Brief
	if code, raw := fetch(t, base, "/api/brief/query", url.Values{"q": {"order_items"}}, &q); code != 200 || q.Prompt == "" {
		t.Fatalf("query: %d %s", code, raw)
	}
	var targets discover.Targets
	fetch(t, base, "/api/shipyard/targets", url.Values{"service": {"acme-api"}}, &targets)
	if targets.Installed || targets.Available || targets.Problem == nil || targets.Problem.Code != "shipyard_not_found" || targets.Boxes == nil || targets.Projects == nil {
		t.Errorf("targets = %+v", targets)
	}
	code, p := send(t, base+"/api/shipyard/tasks", map[string]any{"box": "hetzner-1", "project": "acme", "name": "fix-a", "prompt": "p"}, nil)
	if code != 404 || p.Error.Code != "shipyard_not_found" || p.Error.Message != "Shipyard is not installed on this machine." {
		t.Errorf("sending without Shipyard: %d %+v", code, p)
	}
}
