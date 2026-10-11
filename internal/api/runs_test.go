package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func postJSON(t *testing.T, url string, body any, into any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if into != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
			t.Fatalf("POST %s: %v", url, err)
		}
	}
	return resp.StatusCode
}

func listRuns(t *testing.T, base string) []Run {
	t.Helper()
	var out struct {
		Runs []Run `json:"runs"`
	}
	getJSON(t, base+"/api/runs", &out)
	return out.Runs
}

func TestRunsStartEndAndList(t *testing.T) {
	ts := newTestServer(t)

	if got := listRuns(t, ts.URL); got == nil || len(got) != 0 {
		t.Fatalf("an empty list should be [], got %#v", got)
	}

	var started Run
	status := postJSON(t, ts.URL+"/api/runs", map[string]any{
		"id": "run-acme-1", "command": "yarn dev", "args": []string{"yarn", "dev"},
		"cwd": "/work/acme", "service": "acme-web", "pid": 4242, "provider": "node", "terminal": true,
		"proxies": []map[string]string{{"kind": "postgres", "env": "DATABASE_URL", "listen": "127.0.0.1:61001", "upstream": "127.0.0.1:5432"}},
	}, &started)
	if status != http.StatusOK {
		t.Fatalf("start: %d", status)
	}
	if started.ID != "run-acme-1" || !started.Running || started.StartedMs == 0 || started.ExitCode != nil {
		t.Errorf("started: %+v", started)
	}

	// With no id, the daemon makes one.
	var second Run
	postJSON(t, ts.URL+"/api/runs", map[string]any{"command": "go run ./cmd/api", "cwd": "/work/api", "service": "api", "pid": 4243}, &second)
	if len(second.ID) < 8 {
		t.Errorf("no id was made: %+v", second)
	}

	var ended Run
	if status := postJSON(t, ts.URL+"/api/runs/run-acme-1/end", map[string]any{"exit_code": 130, "signal": "SIGINT"}, &ended); status != http.StatusOK {
		t.Fatalf("end: %d", status)
	}
	if ended.Running || ended.ExitCode == nil || *ended.ExitCode != 130 || ended.Signal != "SIGINT" || ended.EndedMs == nil {
		t.Errorf("ended: %+v", ended)
	}
	// Ending it again keeps the first answer.
	postJSON(t, ts.URL+"/api/runs/run-acme-1/end", map[string]any{"exit_code": 0}, &ended)
	if *ended.ExitCode != 130 {
		t.Errorf("a second end overwrote the first: %+v", ended)
	}

	runs := listRuns(t, ts.URL)
	if len(runs) != 2 || runs[0].ID != second.ID || !runs[0].Running || runs[1].ID != "run-acme-1" || runs[1].Running {
		t.Fatalf("want the running one first: %+v", runs)
	}
	got := runs[1]
	if got.Command != "yarn dev" || got.Cwd != "/work/acme" || got.Service != "acme-web" || got.PID != 4242 ||
		got.Provider != "node" || !got.Terminal || len(got.Proxies) != 1 || got.Proxies[0].Upstream != "127.0.0.1:5432" {
		t.Errorf("listed: %+v", got)
	}
	// An exit code of 0 is still an exit code.
	postJSON(t, ts.URL+"/api/runs/"+second.ID+"/end", map[string]any{"exit_code": 0}, nil)
	resp, err := http.Get(ts.URL + "/api/runs")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), `"exit_code":0`) {
		t.Errorf("a clean exit lost its code: %s", raw)
	}
}

func TestRunsRefuseWhatTheyCannotUse(t *testing.T) {
	ts := newTestServer(t)
	for name, body := range map[string]any{
		"no command": map[string]any{"cwd": "/work"},
		"a bad id":   map[string]any{"id": "../../etc", "command": "x"},
	} {
		if status := postJSON(t, ts.URL+"/api/runs", body, nil); status != http.StatusBadRequest {
			t.Errorf("%s: %d", name, status)
		}
	}
	resp, _ := http.Post(ts.URL+"/api/runs", "application/json", strings.NewReader("{not json"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("not JSON: %d", resp.StatusCode)
	}
	if status := postJSON(t, ts.URL+"/api/runs/nobody/end", map[string]any{"exit_code": 1}, nil); status != http.StatusNotFound {
		t.Errorf("ending a run that never started: %d", status)
	}
	// The same guard as everything else the daemon holds.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/runs", nil)
	req.Host = "evil.example"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a foreign Host got %d", resp.StatusCode)
	}
}

func TestRunsNoticeARunnerThatIsGone(t *testing.T) {
	rs := newRuns()
	alive := map[int]bool{100: true, 200: true}
	rs.alive = func(pid int) bool { return alive[pid] }
	now := time.Unix(1_800_000_000, 0)
	rs.now = func() time.Time { return now }

	rs.start(Run{ID: "stays", Command: "yarn dev", RunnerPID: 100})
	rs.start(Run{ID: "dies", Command: "go run .", RunnerPID: 200})
	// One with no runner pid cannot be checked, and is believed.
	rs.start(Run{ID: "unknown", Command: "make dev"})

	now = now.Add(time.Minute)
	alive[200] = false // killed -9: it never said how it ended
	byID := map[string]Run{}
	for _, run := range rs.list() {
		byID[run.ID] = run
	}
	if !byID["stays"].Running || !byID["unknown"].Running {
		t.Errorf("runs that are fine were ended: %+v", byID)
	}
	lost := byID["dies"]
	if lost.Running || !lost.Lost || lost.ExitCode != nil || lost.EndedMs == nil {
		t.Errorf("want it lost, with no exit code: %+v", lost)
	}
	// If it does report after all (the machine slept), that answer stands.
	if run, _ := rs.end("dies", 0, ""); run.Lost || run.ExitCode == nil {
		t.Errorf("a late end was ignored: %+v", run)
	}
}

func TestRunsKeepOnlyRecentEndedOnes(t *testing.T) {
	rs := newRuns()
	rs.alive = func(int) bool { return true }
	at := time.Unix(1_800_000_000, 0)
	rs.now = func() time.Time { at = at.Add(time.Second); return at }

	rs.start(Run{ID: "still-running", Command: "yarn dev"})
	for i := 0; i < keepEndedRuns+20; i++ {
		id := fmt.Sprintf("ended-%03d", i)
		rs.start(Run{ID: id, Command: "true"})
		rs.end(id, 0, "")
	}
	runs := rs.list()
	if len(runs) != keepEndedRuns+1 {
		t.Fatalf("%d runs kept, want %d ended and the one running", len(runs), keepEndedRuns)
	}
	if runs[0].ID != "still-running" {
		t.Errorf("the running one is not first: %s", runs[0].ID)
	}
	if runs[1].ID != fmt.Sprintf("ended-%03d", keepEndedRuns+19) || runs[len(runs)-1].ID != "ended-020" {
		t.Errorf("kept %s … %s, want the newest", runs[1].ID, runs[len(runs)-1].ID)
	}
}

func TestRunsWakeTheApp(t *testing.T) {
	st := newTestServer(t)
	resp, err := http.Get(st.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 256)
	resp.Body.Read(buf) // hello

	postJSON(t, st.URL+"/api/runs", map[string]any{"command": "yarn dev"}, nil)
	got := make(chan string, 1)
	go func() {
		n, _ := resp.Body.Read(buf)
		got <- string(buf[:n])
	}()
	select {
	case event := <-got:
		if !strings.Contains(event, "event: ingest") {
			t.Errorf("event: %q", event)
		}
	case <-time.After(5 * time.Second):
		t.Error("a new run did not wake the event stream")
	}
}
