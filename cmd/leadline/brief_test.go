package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/sean-brydon/leadline/internal/api"
	"github.com/sean-brydon/leadline/internal/demo"
	"github.com/sean-brydon/leadline/internal/store"
)

// `leadline brief` prints what the daemon wrote: the same prompt the app
// shows, on standard output and nothing else there, so it can be piped.
func TestBriefCommand(t *testing.T) {
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(api.New(st, nil).Handler())
	defer func() {
		ts.Close()
		st.Close()
	}()
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
	run := func(args ...string) (int, string, string) {
		var out, errs bytes.Buffer
		code := runBrief(append(args, "--url", ts.URL), &out, &errs)
		return code, out.String(), errs.String()
	}

	code, out, errs := run("query", "order_items")
	if code != 0 || errs != "" || !strings.HasPrefix(out, "Fix the N+1 on order_items in GET /api/orders\n") ||
		!strings.Contains(out, "SELECT id, product_id, quantity FROM order_items WHERE order_id = ?") || strings.Contains(out, "One real statement") || !strings.HasSuffix(out, "not why.\n") {
		t.Errorf("brief query: %d\n%s\n%s", code, out, errs)
	}
	// Flags before or after, and several words as one search.
	if code, out, _ = run("--example", "query", "order_items"); code != 2 {
		t.Errorf("a flag before the kind: %d %s", code, out)
	}
	if code, out, _ = run("query", "--example", "FROM", "order_items"); code != 0 || !strings.Contains(out, "One real statement of that shape") {
		t.Errorf("brief query --example: %d\n%s", code, out)
	}
	if code, out, _ = run("endpoint", "POST /api/checkout", "--service", "acme-api"); code != 0 || !strings.HasPrefix(out, "Fix the failing requests to POST /api/checkout\n") {
		t.Errorf("brief endpoint: %d\n%s", code, out)
	}
	if code, out, _ = run("log", "checkout", "failed"); code != 0 || !strings.HasPrefix(out, "Error in acme-api: checkout failed: payment declined\n") {
		t.Errorf("brief log: %d\n%s", code, out)
	}
	// With no words, the newest error or warning.
	if code, out, _ = run("log"); code != 0 || !strings.Contains(out, "## Done when") {
		t.Errorf("brief log with no words: %d\n%s", code, out)
	}
	// --json is the whole brief, with the trace to ask about next.
	code, out, _ = run("log", "checkout failed", "--json")
	var b struct {
		Kind     string
		Prompt   string
		Evidence struct {
			Log struct {
				TraceID string `json:"trace_id"`
			}
		}
	}
	if code != 0 || json.Unmarshal([]byte(out), &b) != nil || b.Kind != "log" || b.Evidence.Log.TraceID == "" {
		t.Fatalf("brief log --json: %d\n%s", code, out)
	}
	if code, out, _ = run("trace", b.Evidence.Log.TraceID); code != 0 || !strings.HasPrefix(out, "Fix the failed request POST /api/checkout\n") {
		t.Errorf("brief trace: %d\n%s", code, out)
	}

	// What goes wrong goes to standard error, in words, and nothing is
	// printed for an agent to take as a prompt.
	for _, c := range []struct {
		args []string
		code int
		says string
	}{
		{[]string{"query", "no_such_table"}, 1, `leadline: No query in this window holds "no_such_table".`},
		{[]string{"trace", "00000000000000000000000000000000"}, 1, "leadline: Leadline holds no trace with that id."},
		{[]string{"query"}, 2, "leadline: brief query needs to be told which one"},
		{[]string{"metric", "x"}, 2, "usage: leadline brief"},
		{[]string{}, 2, "usage: leadline brief"},
	} {
		code, out, errs := run(c.args...)
		if len(c.args) == 0 {
			var o, e bytes.Buffer
			code, out, errs = runBrief(nil, &o, &e), o.String(), e.String()
		}
		if code != c.code || out != "" || !strings.Contains(errs, c.says) {
			t.Errorf("brief %v: %d, out %q, err %q", c.args, code, out, errs)
		}
	}
	var o, e bytes.Buffer
	if code := runBrief([]string{"query", "x", "--url", "http://127.0.0.1:1"}, &o, &e); code != 1 || o.Len() != 0 || !strings.Contains(e.String(), "nothing answers at http://127.0.0.1:1") {
		t.Errorf("no daemon: %d %q %q", code, o.String(), e.String())
	}
}
