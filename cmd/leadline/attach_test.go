package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sean-brydon/leadline/internal/api"
	"github.com/sean-brydon/leadline/internal/integrations"
	"github.com/sean-brydon/leadline/internal/integrations/fake"
	"github.com/sean-brydon/leadline/internal/store"
)

// attachDaemon is a real daemon for attach and detach to talk to: the API
// over a store in memory, with proxy ports of the test's own.
func attachDaemon(t *testing.T) (url string, st *store.Store, m *integrations.Manager) {
	t.Helper()
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	first, last := fake.Ports(t, 8)
	m, err = integrations.Open(integrations.Options{Store: st, ProxyPorts: integrations.PortRange{First: first, Last: last}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(api.New(st, m).Handler())
	t.Cleanup(func() {
		ts.Close()
		m.Close()
		st.Close()
	})
	return ts.URL, st, m
}

// closedPort is an address nothing listens on.
func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return "http://" + addr
}

// run runs one of the commands with an environment of the test's making,
// never this process's own.
func run(command func([]string, []string, *bytes.Buffer, *bytes.Buffer) int, environ []string, args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = command(args, environ, &out, &errOut)
	return code, out.String(), errOut.String()
}

func attachCmd(args, environ []string, out, errOut *bytes.Buffer) int {
	return attachCommand(args, environ, out, errOut)
}

func detachCmd(args, environ []string, out, errOut *bytes.Buffer) int {
	return detachCommand(args, environ, out, errOut)
}

// hookEnv is the environment Shipyard gives a repository's hook for an
// event of a worktree: the event's fields, then the worktree's own.
func hookEnv(event, worktree, path string, extra ...string) []string {
	return append([]string{
		"PATH=/usr/bin:/bin", "HOME=/home/dev",
		"BERTH_EVENT=" + event, "BERTH_EVENT_BOX=devl", "BERTH_EVENT_ORIGIN=app", "BERTH_ORIGIN=hook",
		"BERTH_LOCATION=acme", "BERTH_NAME=" + worktree, "BERTH_PATH=" + path, "BERTH_BRANCH=" + worktree,
		"BERTH_BOX=devl", "BERTH_ROOT_PATH=/home/dev/work/acme", "BERTH_WORKTREE_PATH=" + path,
		"BERTH_WORKTREE_NAME=" + worktree, "BERTH_WORKTREE_SLUG=acme_" + strings.ReplaceAll(worktree, "-", "_"), "BERTH_PORT=4100",
	}, extra...)
}

// With Leadline not running a hook is not failed: one line, exit 0. With
// --strict it is.
func TestAttachAndDetachWhenLeadlineIsDown(t *testing.T) {
	down := closedPort(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".env"), "DATABASE_URL=postgres://acme:acme-s3cret@localhost:5432/acme_fix_orders\n")
	env := hookEnv("worktree.created", "fix-orders", dir, "LEADLINE_URL="+down)

	for _, tc := range []struct {
		name    string
		command func([]string, []string, *bytes.Buffer, *bytes.Buffer) int
		args    []string
		code    int
		says    string
	}{
		{"attach", attachCmd, nil, 0, "leadline: not running, so acme/fix-orders was not attached."},
		{"attach --strict", attachCmd, []string{"--strict"}, 1, "leadline: not running, so acme/fix-orders was not attached."},
		{"detach", detachCmd, nil, 0, "leadline: not running, so acme/fix-orders was not detached."},
		{"detach --strict", detachCmd, []string{"--strict"}, 1, "leadline: not running, so acme/fix-orders was not detached."},
		// Something that listens there and is not Leadline is not Leadline.
	} {
		code, stdout, stderr := run(tc.command, env, tc.args...)
		if code != tc.code {
			t.Errorf("%s: exit %d, want %d", tc.name, code, tc.code)
		}
		if stdout != "" || strings.Count(stderr, "\n") != 1 || !strings.HasPrefix(stderr, tc.says) {
			t.Errorf("%s: stdout %q, stderr %q; want one line beginning %q", tc.name, stdout, stderr, tc.says)
		}
		if strings.Contains(stderr, "acme-s3cret") {
			t.Errorf("%s printed a password: %q", tc.name, stderr)
		}
	}

	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hello")) }))
	defer other.Close()
	if code, _, stderr := run(attachCmd, env, "--daemon", other.URL); code != 0 || !strings.Contains(stderr, "not running") {
		t.Errorf("against something that is not Leadline: exit %d, %q", code, stderr)
	}
	// A wrong flag is the caller's mistake, and says so.
	if code, _, _ := run(attachCmd, env, "--no-such-flag"); code != 2 {
		t.Errorf("a wrong flag: exit %d, want 2", code)
	}
	if code, _, stderr := run(attachCmd, env, "--url", "postgres://acme:acme-s3cret@localhost/acme"); code != 2 || strings.Contains(stderr, "acme-s3cret") {
		t.Errorf("--url without a name: exit %d, %q", code, stderr)
	}
}

// The hooks, as Shipyard would run them: two worktrees of one project, a
// database each, attached and then one of them removed.
func TestAttachAndDetachFromHooks(t *testing.T) {
	pg, rd := fake.NewPostgres(t), fake.NewRedis(t)
	pg.AddDatabase("acme_fix_orders")
	pg.AddDatabase("acme_add_refunds")
	url, st, m := attachDaemon(t)
	root := t.TempDir()
	dbURL := func(name string) string { return strings.Replace(pg.URL(), "/acme_dev", "/"+name, 1) }

	fix := filepath.Join(root, "acme-fix-orders")
	writeFile(t, filepath.Join(fix, ".env"), "# written by the setup script\nDATABASE_URL="+dbURL("acme_fix_orders")+"\nDIRECT_URL=${DATABASE_URL}\nSESSION_SECRET=acme-not-a-url\nNEXT_PUBLIC_API=https://api.acme.example\n")
	writeFile(t, filepath.Join(fix, "apps", "api", ".env"), "REDIS_URL="+rd.URL()+"\n")
	refunds := filepath.Join(root, "acme-add-refunds")
	// This one has no .env: Shipyard's config sets its URL in the environment.
	if err := os.MkdirAll(refunds, 0o755); err != nil {
		t.Fatal(err)
	}
	secrets := []string{pg.Password, rd.Password, "acme-not-a-url"}
	check := func(what string, texts ...string) {
		t.Helper()
		for _, text := range texts {
			for _, s := range secrets {
				if strings.Contains(text, s) {
					t.Errorf("%s holds a secret: %q", what, text)
				}
			}
		}
	}

	// worktree.created, before the setup script has written anything: the
	// source is registered, with nothing to connect yet.
	early := filepath.Join(root, "acme-not-set-up")
	code, stdout, stderr := run(attachCmd, hookEnv("worktree.created", "not-set-up", early, "LEADLINE_URL="+url))
	if code != 0 || stderr != "" || stdout != "leadline: attached acme/not-set-up: no database or cache URL in its environment or .env files\n" {
		t.Errorf("attach before setup: exit %d, %q, %q", code, stdout, stderr)
	}

	// worktree.setup.finished.
	code, stdout, stderr = run(attachCmd, hookEnv("worktree.setup.finished", "fix-orders", fix, "LEADLINE_URL="+url), "--env-dir", "apps/api")
	if code != 0 || stderr != "" {
		t.Fatalf("attach: exit %d, stderr %q", code, stderr)
	}
	check("attach's output", stdout)
	conns := m.Connections()
	if len(conns) != 2 {
		t.Fatalf("connections: %+v", conns)
	}
	want := "leadline: attached acme/fix-orders: Postgres " + pg.Addr() + "/acme_fix_orders through " + conns[0].Proxy.Listen +
		" (DATABASE_URL, DIRECT_URL); Redis " + rd.Addr() + "/0, commands recorded (REDIS_URL)\n"
	if stdout != want {
		t.Errorf("attach said:\n%swant:\n%s", stdout, want)
	}
	if o := conns[0].Owner; o.Kind != "worktree" || o.Via != "shipyard" || o.Project != "acme" || o.Worktree != "fix-orders" || o.Branch != "fix-orders" || o.Box != "devl" || o.Path != fix {
		t.Errorf("owner: %+v", o)
	}

	// Again, as worktree.resumed would: nothing changes, and it says so.
	code, again, _ := run(attachCmd, hookEnv("worktree.resumed", "fix-orders", fix, "LEADLINE_URL="+url), "--env-dir", "apps/api")
	if code != 0 || !strings.HasPrefix(again, "leadline: acme/fix-orders is attached already, nothing changed: Postgres ") {
		t.Errorf("attach again: exit %d, %q", code, again)
	}
	if now := m.Connections(); len(now) != 2 || now[0].ID != conns[0].ID || now[0].Proxy.Port != conns[0].Proxy.Port {
		t.Errorf("attaching again changed the connections: %+v", now)
	}

	// The second worktree, its URL from the environment.
	code, stdout, _ = run(attachCmd, hookEnv("worktree.created", "add-refunds", refunds, "LEADLINE_URL="+url, "DATABASE_URL="+dbURL("acme_add_refunds")))
	check("attach's output", stdout)
	if code != 0 || !strings.HasPrefix(stdout, "leadline: attached acme/add-refunds: Postgres "+pg.Addr()+"/acme_add_refunds through 127.0.0.1:") {
		t.Errorf("second attach: exit %d, %q", code, stdout)
	}
	if got := m.Attached(); len(got) != 3 || len(m.Connections()) != 3 || len(m.Proxies()) != 2 {
		t.Fatalf("after two worktrees: %d attached, %d connections, %d proxies", len(got), len(m.Connections()), len(m.Proxies()))
	}

	// A hook for an event that names no worktree attaches nothing.
	code, stdout, stderr = run(attachCmd, []string{"BERTH_EVENT=agent.finished", "BERTH_LOCATION=acme", "BERTH_PATH=" + fix, "LEADLINE_URL=" + url})
	if code != 0 || stdout != "" || !strings.Contains(stderr, "the event agent.finished names no worktree, so nothing was attached") {
		t.Errorf("a hook on another event: exit %d, %q, %q", code, stdout, stderr)
	}

	// Data under each, and under a command of the first.
	now := int64(1_800_000_000_000_000_000)
	if err := st.AddLogs([]store.Log{
		{TsNs: now, Service: "acme/fix-orders", Severity: "INFO", SeverityNum: 9, Body: "ready", Attrs: "{}"},
		{TsNs: now, Service: "acme/fix-orders:worker", Severity: "INFO", SeverityNum: 9, Body: "ready", Attrs: "{}"},
		{TsNs: now, Service: "acme/add-refunds", Severity: "INFO", SeverityNum: 9, Body: "ready", Attrs: "{}"},
	}); err != nil {
		t.Fatal(err)
	}

	// worktree.removed. Shipyard runs a repository's hook for it in the
	// main checkout, with the main checkout's worktree variables; the
	// event's own fields name the worktree that went.
	removed := []string{"PATH=/usr/bin:/bin", "LEADLINE_URL=" + url,
		"BERTH_EVENT=worktree.removed", "BERTH_EVENT_BOX=devl", "BERTH_LOCATION=acme", "BERTH_NAME=fix-orders", "BERTH_PATH=" + fix,
		"BERTH_BOX=devl", "BERTH_ROOT_PATH=" + filepath.Join(root, "acme"), "BERTH_WORKTREE_PATH=" + filepath.Join(root, "acme"),
		"BERTH_WORKTREE_NAME=acme", "BERTH_BRANCH=main"}
	if err := os.RemoveAll(fix); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = run(detachCmd, removed)
	check("detach's output", stdout, stderr)
	if code != 0 || stderr != "" || stdout != "leadline: detached acme/fix-orders: removed 2 connections and closed 1 proxy, deleted 0 spans, 2 logs and 0 metric points\n" {
		t.Errorf("detach: exit %d, %q, %q", code, stdout, stderr)
	}
	left := m.Connections()
	if len(left) != 1 || left[0].Owner.Worktree != "add-refunds" || left[0].Proxy == nil || left[0].Proxy.State != "listening" {
		t.Errorf("after detach: %+v", left)
	}
	var logs int64
	st.DB().QueryRow(`SELECT count(*) FROM logs`).Scan(&logs)
	if logs != 1 {
		t.Errorf("%d logs left, want the other worktree's 1", logs)
	}
	// Twice, and for one Leadline never saw: nothing, and no failure.
	code, stdout, _ = run(detachCmd, removed)
	if code != 0 || stdout != "leadline: acme/fix-orders is not attached and has no data, nothing to do\n" {
		t.Errorf("detach again: exit %d, %q", code, stdout)
	}
	code, stdout, _ = run(detachCmd, []string{"LEADLINE_URL=" + url}, "--project", "acme", "--name", "never-seen", "--strict")
	if code != 0 || stdout != "leadline: acme/never-seen is not attached and has no data, nothing to do\n" {
		t.Errorf("detach of a worktree never seen: exit %d, %q", code, stdout)
	}
	// --keep-data, by name.
	code, stdout, _ = run(detachCmd, []string{"LEADLINE_URL=" + url}, "--service", "acme/add-refunds", "--keep-data")
	if code != 0 || stdout != "leadline: detached acme/add-refunds: removed 1 connection and closed 1 proxy, kept its data\n" {
		t.Errorf("detach --keep-data: exit %d, %q", code, stdout)
	}
	st.DB().QueryRow(`SELECT count(*) FROM logs`).Scan(&logs)
	if logs != 1 {
		t.Errorf("%d logs left after --keep-data, want 1", logs)
	}
}

// By hand, outside Shipyard: flags, and the folder.
func TestAttachByHand(t *testing.T) {
	pg := fake.NewPostgres(t)
	pg.AddDatabase("acme_by_hand")
	url, _, m := attachDaemon(t)
	root := t.TempDir()
	main := filepath.Join(root, "acme")
	linked := filepath.Join(root, "acme-by-hand")
	writeFile(t, filepath.Join(main, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(linked, ".git"), "gitdir: "+filepath.Join(main, ".git", "worktrees", "acme-by-hand")+"\n")
	env := []string{"PATH=/usr/bin:/bin", "LEADLINE_URL=" + url}
	dbURL := strings.Replace(pg.URL(), "/acme_dev", "/acme_by_hand", 1)

	// Nothing but a folder: git names it, and with no file to read and no
	// URL it attaches the source alone.
	code, stdout, stderr := run(attachCmd, env, "--path", linked)
	if code != 0 || stdout != "leadline: attached acme/by-hand: no database or cache URL in its environment or .env files\n" {
		t.Fatalf("attach --path: exit %d, %q, %q", code, stdout, stderr)
	}
	code, stdout, _ = run(attachCmd, env, "--path", linked, "--no-env", "--url", "DATABASE_URL="+dbURL, "--no-capture", "--branch", "by-hand")
	if code != 0 || strings.Contains(stdout, pg.Password) ||
		stdout != "leadline: updated acme/by-hand: Postgres "+pg.Addr()+"/acme_by_hand, statistics only (DATABASE_URL)\n" {
		t.Errorf("attach --url: exit %d, %q", code, stdout)
	}
	conns := m.Connections()
	if len(conns) != 1 || conns[0].Capture || conns[0].Owner.Via != "cli" || conns[0].Owner.Service != "acme/by-hand" || conns[0].Owner.Branch != "by-hand" {
		t.Fatalf("connections: %+v", conns)
	}
	// Run from a folder with nothing to read, the attach leaves what the
	// worktree has alone: it may be the wrong folder.
	code, stdout, _ = run(attachCmd, env, "--path", linked)
	if code != 0 || len(m.Connections()) != 1 || !strings.Contains(stdout, "is attached already, nothing changed") {
		t.Errorf("attach with nothing to read: exit %d, %q, %d connections", code, stdout, len(m.Connections()))
	}
	// --quiet says nothing when it worked.
	if code, stdout, stderr = run(attachCmd, env, "--project", "acme", "--name", "quiet", "--quiet"); code != 0 || stdout+stderr != "" {
		t.Errorf("--quiet: exit %d, %q, %q", code, stdout, stderr)
	}
	code, stdout, _ = run(detachCmd, env, "--path", linked)
	if code != 0 || stdout != "leadline: detached acme/by-hand: removed 1 connection and closed 0 proxies, it had no data\n" {
		t.Errorf("detach --path: exit %d, %q", code, stdout)
	}
}

func shipyardCmd(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = shipyardCommand(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// What `leadline shipyard hooks` prints is what Shipyard's docs say a hook
// is: {"hooks": [{"on": …, "run": …}]}, the same in a repository's config
// and a box's hooks.json.
func TestShipyardHooksArePrinted(t *testing.T) {
	code, stdout, stderr := shipyardCmd("hooks", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d, %q", code, stderr)
	}
	var cfg struct {
		Hooks []map[string]string `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(stdout), &cfg); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	got := map[string]string{}
	for _, h := range cfg.Hooks {
		if len(h) != 2 {
			t.Errorf("a hook has fields other than on and run: %v", h)
		}
		got[h["on"]] = h["run"]
	}
	want := map[string]string{
		"worktree.created":        "command -v leadline >/dev/null 2>&1 || exit 0; leadline attach",
		"worktree.setup.finished": "command -v leadline >/dev/null 2>&1 || exit 0; leadline attach",
		"worktree.resumed":        "command -v leadline >/dev/null 2>&1 || exit 0; leadline attach",
		"worktree.removed":        "command -v leadline >/dev/null 2>&1 || exit 0; leadline detach",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("hooks: %v", got)
	}
	// Read as a shell command, not as \u003e.
	if strings.Contains(stdout, `\u00`) {
		t.Errorf("the commands are escaped: %s", stdout)
	}
	// With words around it, and both places named.
	code, stdout, _ = shipyardCmd("hooks", "--bin", "/opt/leadline/bin/leadline")
	for _, want := range []string{".berth/config.json", "~/.berth/hooks.json", `"run": "[ -x /opt/leadline/bin/leadline ] || exit 0; /opt/leadline/bin/leadline detach"`} {
		if code != 0 || !strings.Contains(stdout, want) {
			t.Errorf("exit %d; missing %q in:\n%s", code, want, stdout)
		}
	}
	if code, _, _ = shipyardCmd("hooks", "--bin", "leadline; rm -rf /"); code != 2 {
		t.Errorf("a --bin that is more than a path: exit %d, want 2", code)
	}
	if code, _, _ = shipyardCmd(); code != 2 {
		t.Errorf("no subcommand: exit %d, want 2", code)
	}
}

// --write adds the hooks to a config file and leaves every other byte of
// it where it was, however the file is laid out.
func TestShipyardHooksMergeKeepsTheFile(t *testing.T) {
	hooks := shipyardHooks("leadline")
	files := map[string]string{
		"the example in Shipyard's docs": `{
  "setup": "pnpm install && createdb $BERTH_WORKTREE_SLUG",
  "archive": "dropdb --if-exists $BERTH_WORKTREE_SLUG",
  "ports": 2,
  "env": {
    "DATABASE_URL": "postgres://localhost/$BERTH_WORKTREE_SLUG",
    "NEXT_PUBLIC_WEBAPP_URL": "$BERTH_URL"
  },
  "services": [
    { "name": "web", "run": "pnpm dev --port $BERTH_PORT", "autostart": true },
    { "name": "worker", "run": "pnpm worker" }
  ],
  "hooks": [
    { "on": "worktree.created", "run": "cp ../shop/.env .env" }
  ],
  "flows": []
}
`,
		"tabs, hooks first, no newline at the end": "{\n\t\"hooks\": [\n\t\t{\"on\": \"agent.waiting\", \"run\": \"say \\\"an agent needs you\\\" # a ] and a } in a string\", \"timeout\": \"10s\"},\n\t\t{\"on\": \"before:worktree.create\", \"run\": \"true\", \"tool\": \"orca\"}\n\t],\n\t\"ports\":   3\n}",
		"hooks on one line":                        `{"ports": 1, "hooks": [{"on": "worktree.created", "run": "pnpm install", "timeout": "10m"}], "env": {"A": "[\"x\"]"}}`,
		"an empty list of hooks":                   "{\n    \"setup\": \"make bootstrap\",\n    \"hooks\": [],\n    \"ports\": 1\n}\n",
		"no hooks at all":                          "{\n  \"setup\": \"make bootstrap\",\n  \"env\": { \"WEIRD\": \"a \\\"hooks\\\": [ in a string\" }\n}\n",
		"no hooks, on one line":                    `{"ports": 2}`,
		"an empty object":                          "{}\n",
		"a box's hooks.json":                       "{\n  \"hooks\": [\n    { \"on\": \"agent.waiting\", \"run\": \"osascript -e 'display notification \\\"$BERTH_PATH\\\" with title \\\"An agent needs you\\\"'\" },\n    { \"on\": \"worktree.created\", \"run\": \"cd \\\"$BERTH_PATH\\\" && pnpm install\", \"timeout\": \"10m\" }\n  ]\n}\n",
		"windows line endings":                     "{\r\n  \"ports\": 2,\r\n  \"hooks\": [\r\n    { \"on\": \"worktree.created\", \"run\": \"pnpm install\" }\r\n  ]\r\n}\r\n",
		"hooks nested somewhere else too":          "{\n  \"flows\": [ { \"id\": \"x\", \"hooks\": [] } ],\n  \"hooks\": [\n    { \"on\": \"*\", \"run\": \"true\" }\n  ]\n}\n",
	}
	for name, old := range files {
		out, added, err := mergeHooks([]byte(old), hooks)
		if err != nil || added != len(hooks) {
			t.Errorf("%s: added %d, %v", name, added, err)
			continue
		}
		// One piece of text went in, and nothing else moved: the file is
		// the old one's start, the new text, and the old one's end.
		inserted := len(out) - len(old)
		kept := false
		for cut := 0; cut <= len(old) && !kept; cut++ {
			kept = string(out[:cut]) == old[:cut] && string(out[cut+inserted:]) == old[cut:]
		}
		if !kept {
			t.Errorf("%s: the file's own content changed:\n%s\n-- became --\n%s", name, old, out)
			continue
		}
		// What Shipyard would read: everything it read before, and the
		// hooks after the ones that were there.
		var before, after map[string]json.RawMessage
		if err := json.Unmarshal([]byte(old), &before); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(out, &after); err != nil {
			t.Errorf("%s: the result is not JSON: %v\n%s", name, err, out)
			continue
		}
		var had, have []map[string]string
		json.Unmarshal(before["hooks"], &had)
		if err := json.Unmarshal(after["hooks"], &have); err != nil || len(have) != len(had)+len(hooks) || !reflect.DeepEqual(have[:len(had)], append([]map[string]string{}, had...)) {
			t.Errorf("%s: hooks were %v and are %v (%v)", name, had, have, err)
			continue
		}
		for i, h := range hooks {
			if got := have[len(had)+i]; got["on"] != h.On || got["run"] != h.Run || len(got) != 2 {
				t.Errorf("%s: hook %d is %v", name, i, got)
			}
		}
		delete(before, "hooks")
		delete(after, "hooks")
		if !reflect.DeepEqual(before, after) {
			t.Errorf("%s: a key other than hooks changed", name)
		}
		// Again: they are there, so nothing is added and nothing changes.
		twice, added, err := mergeHooks(out, hooks)
		if err != nil || added != 0 || !bytes.Equal(twice, out) {
			t.Errorf("%s: a second merge added %d (%v)", name, added, err)
		}
	}

	// A file that is not what it should be is left alone.
	for name, bad := range map[string]string{
		"not JSON":              `{"hooks": [`,
		"JSON with a comment":   "{\n  // ours\n  \"hooks\": []\n}",
		"a list":                `[]`,
		"hooks that is no list": `{"hooks": {"on": "x"}}`,
	} {
		if out, added, err := mergeHooks([]byte(bad), hooks); err == nil || added != 0 || out != nil {
			t.Errorf("%s: merged anyway (%d added)", name, added)
		}
	}
	// One of the hooks is there already: only the others are added.
	some := "{\n  \"hooks\": [\n    " + entryJSON(hooks[0]) + "\n  ]\n}\n"
	if out, added, err := mergeHooks([]byte(some), hooks); err != nil || added != len(hooks)-1 || strings.Count(string(out), `"worktree.created"`) != 1 {
		t.Errorf("with one there already: added %d, %v:\n%s", added, err, out)
	}
}

// --write writes the file it is pointed at, and never Shipyard's own.
func TestShipyardHooksWrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BERTH_USER_DIR", "")
	t.Setenv("BERTH_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	repo := filepath.Join(t.TempDir(), "acme")
	config := filepath.Join(repo, ".berth", "config.json")
	const old = "{\n  \"setup\": \"pnpm install && createdb $BERTH_WORKTREE_SLUG\",\n  \"archive\": \"dropdb --if-exists $BERTH_WORKTREE_SLUG\",\n  \"ports\": 2\n}\n"
	writeFile(t, config, old)
	if err := os.Chmod(config, 0o600); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := shipyardCmd("hooks", "--write", config)
	if code != 0 || stderr != "" || !strings.Contains(stdout, "Added 4 hooks to "+config) || !strings.Contains(stdout, "trust") {
		t.Fatalf("exit %d, %q, %q", code, stdout, stderr)
	}
	got, _ := os.ReadFile(config)
	want := "{\n  \"setup\": \"pnpm install && createdb $BERTH_WORKTREE_SLUG\",\n  \"archive\": \"dropdb --if-exists $BERTH_WORKTREE_SLUG\",\n  \"ports\": 2,\n  \"hooks\": [\n" +
		"    { \"on\": \"worktree.created\", \"run\": \"command -v leadline >/dev/null 2>&1 || exit 0; leadline attach\" },\n" +
		"    { \"on\": \"worktree.setup.finished\", \"run\": \"command -v leadline >/dev/null 2>&1 || exit 0; leadline attach\" },\n" +
		"    { \"on\": \"worktree.resumed\", \"run\": \"command -v leadline >/dev/null 2>&1 || exit 0; leadline attach\" },\n" +
		"    { \"on\": \"worktree.removed\", \"run\": \"command -v leadline >/dev/null 2>&1 || exit 0; leadline detach\" }\n" +
		"  ]\n}\n"
	if string(got) != want {
		t.Errorf("the file is:\n%s\nwant:\n%s", got, want)
	}
	if fi, _ := os.Stat(config); fi.Mode().Perm() != 0o600 {
		t.Errorf("the file's mode became %o", fi.Mode().Perm())
	}
	if left, _ := filepath.Glob(filepath.Join(repo, ".berth", "*.tmp")); len(left) > 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
	// Twice changes nothing.
	code, stdout, _ = shipyardCmd("hooks", "--write", config)
	if again, _ := os.ReadFile(config); code != 0 || !bytes.Equal(again, got) || !strings.Contains(stdout, "nothing was changed") {
		t.Errorf("a second write: exit %d, %q", code, stdout)
	}
	// A file that is not there yet is made, with the hooks alone.
	fresh := filepath.Join(repo, "other", ".berth", "config.json")
	if code, _, stderr = shipyardCmd("hooks", "--write", fresh); code != 0 {
		t.Fatalf("a new file: exit %d, %q", code, stderr)
	}
	if b, _ := os.ReadFile(fresh); string(b) != hooksJSON(shipyardHooks("leadline")) {
		t.Errorf("a new file is:\n%s", b)
	}
	// A broken file is not touched.
	broken := filepath.Join(repo, "broken.json")
	writeFile(t, broken, `{"hooks": [`)
	if code, _, stderr = shipyardCmd("hooks", "--write", broken); code != 1 || !strings.Contains(stderr, "not valid JSON") {
		t.Errorf("a broken file: exit %d, %q", code, stderr)
	}
	if b, _ := os.ReadFile(broken); string(b) != `{"hooks": [` {
		t.Errorf("a broken file was changed: %s", b)
	}

	// Shipyard's own folders are never written to: not its hooks.json, not
	// by another name for the folder, not through a link into it.
	own := filepath.Join(home, ".berth")
	writeFile(t, filepath.Join(own, "hooks.json"), "{\n  \"hooks\": []\n}\n")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(own, link); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(t.TempDir(), "berth-user")
	for _, target := range []string{
		filepath.Join(own, "hooks.json"),
		filepath.Join(own, "plugins", "x", "new.json"),
		filepath.Join(link, "hooks.json"),
		filepath.Join(home, "Library", "Application Support", "berth", "box", "locations.json"),
		filepath.Join(home, ".config", "berth", "x.json"),
	} {
		code, stdout, stderr = shipyardCmd("hooks", "--write", target)
		if code != 1 || stdout != "" || !strings.Contains(stderr, "Shipyard's own folder") {
			t.Errorf("--write %s: exit %d, %q, %q", target, code, stdout, stderr)
		}
	}
	t.Setenv("BERTH_USER_DIR", moved)
	if code, _, stderr = shipyardCmd("hooks", "--write", filepath.Join(moved, "hooks.json")); code != 1 || !strings.Contains(stderr, "Shipyard's own folder") {
		t.Errorf("--write into BERTH_USER_DIR: exit %d, %q", code, stderr)
	}
	if b, _ := os.ReadFile(filepath.Join(own, "hooks.json")); string(b) != "{\n  \"hooks\": []\n}\n" {
		t.Errorf("Shipyard's hooks.json was changed: %s", b)
	}
	for _, p := range []string{filepath.Join(own, "plugins"), filepath.Join(home, "Library"), filepath.Join(home, ".config"), moved} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s was created", p)
		}
	}
}
