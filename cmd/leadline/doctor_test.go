package main

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"

	"github.com/sean-brydon/leadline/internal/api"
	"github.com/sean-brydon/leadline/internal/demo"
	"github.com/sean-brydon/leadline/internal/store"
	"github.com/sean-brydon/leadline/skills"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testDaemon(t *testing.T) *httptest.Server {
	t.Helper()
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(api.New(st, nil).Handler())
	t.Cleanup(func() {
		ts.Close()
		st.Close()
	})
	return ts
}

// An environment with nothing in it, and agent settings that live in an
// empty folder: the test must not read the real ones.
func emptyEnv(t *testing.T) func(string) string {
	home := t.TempDir()
	t.Setenv("HOME", home)
	return func(key string) string {
		switch key {
		case "CLAUDE_CONFIG_DIR", "CODEX_HOME":
			return home
		}
		return ""
	}
}

func report(c *checkup) string {
	var b bytes.Buffer
	c.print(&b)
	return b.String()
}

func TestDoctorOnAProjectWithNothingSetUp(t *testing.T) {
	ts := testDaemon(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "package.json"), `{"name":"acme-web","scripts":{"dev":"vite --port 3000"}}`)

	c := examine(ts.URL, dir, emptyEnv(t), time.Now())
	if !c.Running || c.Service != "acme-web" || c.Stack != "node" || c.RunIn != "" || c.Provider || len(c.MCP) != 0 || c.Skills {
		t.Fatalf("%+v", c)
	}
	out := report(c)
	for _, want := range []string{
		"ok  running at " + ts.URL,
		"none yet",
		`it shows in Leadline as "acme-web"`,
		"no  nothing here starts the app through `leadline run`",
		// The example is the project's own dev command.
		"`leadline run -- vite --port 3000`",
		"no  @leadline/node is not installed",
		"claude mcp add --transport http leadline " + ts.URL + "/mcp",
		"Run `leadline skills`",
		"`leadline prompt` prints the line to paste",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestDoctorOnAProjectThatIsSetUp(t *testing.T) {
	ts := testDaemon(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "package.json"), `{"name":"web","scripts":{"build":"vite build","dev":"leadline run -- vite"}}`)
	writeFile(t, filepath.Join(dir, "node_modules", "@leadline", "node", "package.json"), `{}`)
	writeFile(t, filepath.Join(dir, "node_modules", "@leadline", "node", "dist", "register.js"), "")
	writeFile(t, filepath.Join(dir, ".mcp.json"), `{"mcpServers":{"leadline":{"type":"http","url":"`+ts.URL+`/mcp"}}}`)
	writeFile(t, filepath.Join(dir, ".cursor", "mcp.json"), `{"mcpServers":{"other":{"url":"https://mcp.acme.example"},"ll":{"url":"`+ts.URL+`/mcp"}}}`)
	writeFile(t, filepath.Join(dir, ".claude", "skills", "leadline", "SKILL.md"), "x")
	// The service is named by the project's own .env, and has sent data.
	writeFile(t, filepath.Join(dir, ".env.local"), "OTEL_SERVICE_NAME=web\n")
	now := time.Now()
	traces, logs := demo.New(7).Batch(5, now.Add(-time.Minute), now)
	for _, rs := range traces.ResourceSpans {
		for _, kv := range rs.Resource.Attributes {
			if kv.Key == "service.name" {
				kv.Value = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "web"}}
			}
		}
	}
	if err := post(ts.URL+"/v1/traces", traces); err != nil {
		t.Fatal(err)
	}
	if err := post(ts.URL+"/v1/logs", logs); err != nil {
		t.Fatal(err)
	}

	c := examine(ts.URL, dir, emptyEnv(t), now)
	if c.RunIn != `package.json script "dev"` || !c.Provider || !c.Skills || c.LastSeen.IsZero() {
		t.Fatalf("%+v", c)
	}
	if strings.Join(c.MCP, ",") != "Claude Code,Cursor" {
		t.Errorf("MCP: %v", c.MCP)
	}
	out := report(c)
	if !strings.Contains(out, "Nothing: this project is set up") || strings.Contains(out, "\n  no  ") {
		t.Errorf("a project that is set up still has something asked of it:\n%s", out)
	}
	if !strings.Contains(out, `Leadline last heard from "web"`) {
		t.Errorf("missing when it last sent:\n%s", out)
	}
}

func TestDoctorWithNoDaemon(t *testing.T) {
	ts := testDaemon(t)
	url := ts.URL
	ts.Close()
	c := examine(url, t.TempDir(), emptyEnv(t), time.Now())
	out := report(c)
	if c.Running || !strings.Contains(out, "no  nothing answers at "+url) || !strings.Contains(out, "Start Leadline") {
		t.Errorf("%s", out)
	}
	if strings.Contains(out, "Services that have sent data") {
		t.Errorf("it lists services with no daemon to ask:\n%s", out)
	}
}

func TestDoctorSeesAnEndpointThatPointsElsewhere(t *testing.T) {
	ts := testDaemon(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module github.com/acme/api\n")
	writeFile(t, filepath.Join(dir, ".env"), "OTEL_EXPORTER_OTLP_ENDPOINT=https://collector.acme.example\n")
	out := report(examine(ts.URL, dir, emptyEnv(t), time.Now()))
	if !strings.Contains(out, "points somewhere else (.env)") {
		t.Errorf("%s", out)
	}
	if !strings.Contains(out, `it shows in Leadline as "api"`) || !strings.Contains(out, "Go has no automatic instrumentation") {
		t.Errorf("%s", out)
	}
}

func TestRunCommandUsage(t *testing.T) {
	// No command to run is a mistake in the call: 2, and nothing started.
	if code := runCommand([]string{"--name", "x"}); code != 2 {
		t.Errorf("no command: %d", code)
	}
	if code := runCommand([]string{"--no-such-flag", "--", "true"}); code != 2 {
		t.Errorf("an unknown flag: %d", code)
	}
}

func TestPromptIsTheOneTheDaemonServes(t *testing.T) {
	ts := testDaemon(t)
	got, err := daemonPrompt(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got != skills.SetupPrompt(ts.URL) {
		t.Errorf("got %q", got)
	}
	ts.Close()
	if _, err := daemonPrompt(ts.URL); err == nil {
		t.Error("no error with no daemon")
	}
}
