package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sean-brydon/leadline/internal/runner"
)

// runDoctor answers three questions in plain words: is Leadline running,
// what has it heard from, and is the project in this folder wired to it.
// Then it says what to do next. It changes nothing.
func runDoctor(args []string) error {
	fset := flag.NewFlagSet("doctor", flag.ExitOnError)
	base := fset.String("url", envOr("LEADLINE_URL", runner.DefaultURL), "the daemon's address")
	fset.Parse(args)
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	report := examine(strings.TrimRight(*base, "/"), dir, os.Getenv, time.Now())
	report.print(os.Stdout)
	if !report.Running {
		os.Exit(1)
	}
	return nil
}

// checkup is everything doctor found, before it is put into words.
type checkup struct {
	URL     string
	Running bool
	// The daemon answers but knows nothing of runs: it is from before
	// `leadline run`.
	Old      bool
	Services []serviceSeen
	Runs     []string

	Dir     string
	Service string
	// What kind of project: "node", "python", "go", "ruby" or "".
	Stack string
	// The file and line that start the app through `leadline run`.
	RunIn string
	// Node only: whether @leadline/node resolves, and whether the project
	// has some OpenTelemetry of its own.
	Provider bool
	OwnOTel  bool
	// OTEL_EXPORTER_OTLP_ENDPOINT as the project sets it, and where.
	Endpoint, EndpointIn string
	MCP                  []string
	Skills               bool
	// When this project's service last sent anything; zero for never.
	LastSeen time.Time
	now      time.Time
}

type serviceSeen struct {
	Name string
	Last time.Time
}

func examine(base, dir string, env func(string) string, now time.Time) *checkup {
	c := &checkup{URL: base, Dir: dir, now: now}
	daemon := runner.NewDaemon(base)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c.Running = daemon.Healthy(ctx)

	dotenv := runner.ReadDotenv(dir, env("NODE_ENV"), env)
	project := func(key string) string {
		if v := env(key); v != "" {
			return v
		}
		return dotenv.Values[key]
	}
	c.Service = runner.ServiceName("", project, dir)
	c.Stack = stackOf(dir)
	c.RunIn = findRunLine(dir)
	_, c.Provider = runner.NodeProvider(dir)
	c.OwnOTel = hasOwnOTel(dir)
	if v := env("OTEL_EXPORTER_OTLP_ENDPOINT"); v != "" {
		c.Endpoint, c.EndpointIn = v, "the environment"
	} else if v := dotenv.Values["OTEL_EXPORTER_OTLP_ENDPOINT"]; v != "" {
		c.Endpoint, c.EndpointIn = v, dotenv.Source["OTEL_EXPORTER_OTLP_ENDPOINT"]
	}
	c.MCP = mcpAgents(dir, base, env)
	c.Skills = exists(filepath.Join(dir, ".claude", "skills", "leadline", "SKILL.md"))

	if c.Running {
		var summary struct {
			Services []string `json:"services"`
		}
		if getInto(ctx, base+"/api/summary", &summary) == nil {
			for i, name := range summary.Services {
				if i == 30 {
					break
				}
				seen := serviceSeen{Name: name, Last: lastSeen(ctx, base, name)}
				c.Services = append(c.Services, seen)
				if name == c.Service {
					c.LastSeen = seen.Last
				}
			}
			sort.SliceStable(c.Services, func(i, j int) bool { return c.Services[i].Last.After(c.Services[j].Last) })
		}
		var runs struct {
			Runs []struct {
				Command string `json:"command"`
				Service string `json:"service"`
				Running bool   `json:"running"`
			} `json:"runs"`
		}
		if err := getInto(ctx, base+"/api/runs", &runs); err != nil {
			c.Old = true
		}
		for _, run := range runs.Runs {
			if run.Running {
				c.Runs = append(c.Runs, fmt.Sprintf("%s (%s)", run.Command, run.Service))
			}
		}
	}
	return c
}

// lastSeen is the time of a service's newest log or trace.
func lastSeen(ctx context.Context, base, service string) time.Time {
	var newest float64
	q := "?limit=1&service=" + url.QueryEscape(service)
	var logs []struct {
		TsMs float64 `json:"ts_ms"`
	}
	if getInto(ctx, base+"/api/logs"+q, &logs) == nil && len(logs) > 0 {
		newest = logs[0].TsMs
	}
	var traces []struct {
		StartMs float64 `json:"start_ms"`
	}
	if getInto(ctx, base+"/api/traces"+q, &traces) == nil && len(traces) > 0 && traces[0].StartMs > newest {
		newest = traces[0].StartMs
	}
	if newest == 0 {
		return time.Time{}
	}
	return time.UnixMilli(int64(newest))
}

func getInto(ctx context.Context, url string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s", resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(into)
}

func (c *checkup) print(w io.Writer) {
	say := func(ok bool, format string, args ...any) {
		mark := "ok  "
		if !ok {
			mark = "no  "
		}
		fmt.Fprintf(w, "  %s%s\n", mark, fmt.Sprintf(format, args...))
	}
	note := func(format string, args ...any) { fmt.Fprintf(w, "      %s\n", fmt.Sprintf(format, args...)) }
	var next []string

	fmt.Fprintln(w, "Leadline")
	if c.Running {
		say(true, "running at %s", c.URL)
	} else {
		say(false, "nothing answers at %s", c.URL)
		next = append(next, "Start Leadline: open the app, or run `leadlined` in a terminal.")
	}
	if c.Old {
		note("it is an older Leadline that does not list runs; update it to see `leadline run` commands in the app")
	}

	if c.Running {
		fmt.Fprintln(w, "\nServices that have sent data")
		if len(c.Services) == 0 {
			fmt.Fprintln(w, "  none yet")
		}
		for _, s := range c.Services {
			fmt.Fprintf(w, "  %-28s %s\n", s.Name, ago(c.now, s.Last))
		}
		if len(c.Runs) > 0 {
			fmt.Fprintln(w, "\nRunning under `leadline run`")
			for _, run := range c.Runs {
				fmt.Fprintf(w, "  %s\n", run)
			}
		}
	}

	fmt.Fprintf(w, "\nThis project (%s)\n", c.Dir)
	fmt.Fprintf(w, "      it shows in Leadline as %q\n", c.Service)
	if c.RunIn != "" {
		say(true, "starts through `leadline run` (%s)", c.RunIn)
	} else {
		say(false, "nothing here starts the app through `leadline run`")
		next = append(next, "Put `leadline run --` in front of the dev command, for example `leadline run -- "+exampleCommand(c.Dir, c.Stack)+"`. That alone captures its logs.")
	}

	switch {
	case c.Stack == "node" && c.Provider:
		say(true, "@leadline/node is installed; `leadline run` loads it, for traces and queries")
	case c.Stack == "node" && c.OwnOTel:
		say(true, "the project has its own OpenTelemetry; `leadline run` points it at Leadline")
	case c.Stack == "node":
		say(false, "@leadline/node is not installed, so there will be logs but no traces")
		next = append(next, "Add the Node provider as a development dependency (`npm install --save-dev @leadline/node`, or your package manager's way). `leadline run` loads it by itself.")
	case c.Stack == "python" && c.OwnOTel:
		say(true, "OpenTelemetry is among the dependencies; run the app under `opentelemetry-instrument` for traces")
	case c.Stack == "python":
		say(false, "no OpenTelemetry among the dependencies, so there will be logs but no traces")
		next = append(next, "For traces in Python: install `opentelemetry-distro` and `opentelemetry-exporter-otlp-proto-http`, run `opentelemetry-bootstrap -a install`, and start the app as `leadline run -- opentelemetry-instrument <command>`.")
	case c.Stack != "" && c.OwnOTel:
		say(true, "OpenTelemetry is among the dependencies; `leadline run` gives it Leadline's address")
	case c.Stack != "":
		note("%s has no automatic instrumentation: `leadline run` gives logs, and Postgres queries through its proxy. Traces need the OpenTelemetry SDK in the app.", strings.ToUpper(c.Stack[:1])+c.Stack[1:])
	default:
		note("no package.json, pyproject.toml, go.mod or Gemfile here; is this the project's root?")
	}

	switch {
	case c.Endpoint == "":
		note("OTEL_EXPORTER_OTLP_ENDPOINT is not set by the project; `leadline run` sets it for the app")
	case strings.TrimRight(c.Endpoint, "/") == c.URL:
		say(true, "OTEL_EXPORTER_OTLP_ENDPOINT points at Leadline (%s)", c.EndpointIn)
	default:
		say(false, "OTEL_EXPORTER_OTLP_ENDPOINT points somewhere else (%s), and `leadline run` leaves it as the project set it", c.EndpointIn)
		next = append(next, "For development, set OTEL_EXPORTER_OTLP_ENDPOINT="+c.URL+" (in a development-only file such as .env.local), or remove it and let `leadline run` set it.")
	}

	if len(c.MCP) > 0 {
		say(true, "MCP server added for %s", strings.Join(c.MCP, ", "))
	} else {
		say(false, "no coding agent here has Leadline's MCP server")
		next = append(next, "Let your coding agent ask Leadline what the code did: `claude mcp add --transport http leadline "+c.URL+"/mcp` (Codex: `codex mcp add leadline --url "+c.URL+"/mcp`).")
	}
	if c.Skills {
		say(true, "skills installed in .claude/skills")
	} else {
		say(false, "Leadline's skills are not in .claude/skills")
		next = append(next, "Run `leadline skills` to install the agent skills into this project.")
	}

	if c.Running {
		if c.LastSeen.IsZero() {
			say(false, "Leadline has heard nothing from %q yet", c.Service)
			if c.RunIn != "" {
				next = append(next, "Start the app and make a request; it should then be listed above.")
			}
		} else {
			say(true, "Leadline last heard from %q %s", c.Service, ago(c.now, c.LastSeen))
		}
	}

	fmt.Fprintln(w, "\nNext")
	if len(next) == 0 {
		fmt.Fprintln(w, "  Nothing: this project is set up. Start the app and look in Leadline.")
		return
	}
	for i, step := range next {
		fmt.Fprintf(w, "  %d. %s\n", i+1, step)
	}
	if len(next) > 1 {
		fmt.Fprintln(w, "\n  Or hand all of it to a coding agent: `leadline prompt` prints the line to paste.")
	}
}

func ago(now, t time.Time) string {
	if t.IsZero() {
		return "listed, with nothing recent"
	}
	d := now.Sub(t)
	switch {
	case d < 2*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%d seconds ago", int(d.Seconds()))
	case d < 2*time.Minute:
		return "a minute ago"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 2*time.Hour:
		return "an hour ago"
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

func stackOf(dir string) string {
	switch {
	case exists(filepath.Join(dir, "package.json")):
		return "node"
	case exists(filepath.Join(dir, "pyproject.toml")), exists(filepath.Join(dir, "requirements.txt")), exists(filepath.Join(dir, "manage.py")):
		return "python"
	case exists(filepath.Join(dir, "go.mod")):
		return "go"
	case exists(filepath.Join(dir, "Gemfile")):
		return "ruby"
	}
	return ""
}

// exampleCommand is a dev command to show: the project's own, where
// package.json names one.
func exampleCommand(dir, stack string) string {
	switch stack {
	case "node":
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}
		if b, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil && json.Unmarshal(b, &pkg) == nil {
			if dev := strings.TrimSpace(pkg.Scripts["dev"]); dev != "" && !strings.Contains(dev, "leadline run") {
				return dev
			}
		}
		return "next dev"
	case "python":
		return "python manage.py runserver"
	case "go":
		return "go run ."
	case "ruby":
		return "bin/rails server"
	}
	return "<your dev command>"
}

// The files a dev command is usually written in.
var runFiles = []string{"package.json", "Procfile.dev", "Procfile", "Makefile", "justfile", "Justfile", "Taskfile.yml", "mise.toml"}

// findRunLine names the first of those files that starts something through
// `leadline run`.
func findRunLine(dir string) string {
	for _, name := range runFiles {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if name == "package.json" {
			var pkg struct {
				Scripts map[string]string `json:"scripts"`
			}
			if json.Unmarshal(b, &pkg) != nil {
				continue
			}
			names := make([]string, 0, len(pkg.Scripts))
			for script := range pkg.Scripts {
				names = append(names, script)
			}
			sort.Strings(names)
			for _, script := range names {
				if strings.Contains(pkg.Scripts[script], "leadline run") {
					return fmt.Sprintf("package.json script %q", script)
				}
			}
			continue
		}
		if strings.Contains(string(b), "leadline run") {
			return name
		}
	}
	return ""
}

// hasOwnOTel reports whether the project's dependency files mention
// OpenTelemetry. It reads the manifests, not the installed packages: what
// matters is whether the project means to use it.
func hasOwnOTel(dir string) bool {
	for name, needle := range map[string]string{
		"package.json":     `"@opentelemetry/`,
		"pyproject.toml":   "opentelemetry",
		"requirements.txt": "opentelemetry",
		"go.mod":           "go.opentelemetry.io/otel",
		"Gemfile":          "opentelemetry",
	} {
		if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil && strings.Contains(string(b), needle) {
			return true
		}
	}
	return false
}

// mcpAgents names the coding agents that have Leadline's MCP server for
// this project, by reading where each keeps its servers. It looks for the
// daemon's /mcp address, or a server called "leadline".
func mcpAgents(dir, base string, env func(string) string) []string {
	home, _ := os.UserHomeDir()
	var out []string
	has := func(file string, look func(doc map[string]any) bool) bool {
		b, err := os.ReadFile(file)
		if err != nil {
			return false
		}
		var doc map[string]any
		return json.Unmarshal(b, &doc) == nil && look(doc)
	}
	servers := func(v any) bool {
		m, _ := v.(map[string]any)
		for name, server := range m {
			if name == "leadline" {
				return true
			}
			if s, _ := server.(map[string]any); s != nil {
				if u, _ := s["url"].(string); strings.HasPrefix(u, base+"/mcp") {
					return true
				}
			}
		}
		return false
	}
	top := func(doc map[string]any) bool { return servers(doc["mcpServers"]) }

	claudeHome := home
	if v := env("CLAUDE_CONFIG_DIR"); v != "" {
		claudeHome = v
	}
	claude := has(filepath.Join(dir, ".mcp.json"), top) ||
		has(filepath.Join(claudeHome, ".claude.json"), func(doc map[string]any) bool {
			if top(doc) {
				return true
			}
			projects, _ := doc["projects"].(map[string]any)
			project, _ := projects[dir].(map[string]any)
			return project != nil && servers(project["mcpServers"])
		})
	if claude {
		out = append(out, "Claude Code")
	}

	codexHome := filepath.Join(home, ".codex")
	if v := env("CODEX_HOME"); v != "" {
		codexHome = v
	}
	if b, err := os.ReadFile(filepath.Join(codexHome, "config.toml")); err == nil {
		if strings.Contains(string(b), "[mcp_servers.leadline]") || strings.Contains(string(b), base+"/mcp") {
			out = append(out, "Codex")
		}
	}

	if has(filepath.Join(dir, ".cursor", "mcp.json"), top) || has(filepath.Join(home, ".cursor", "mcp.json"), top) {
		out = append(out, "Cursor")
	}
	return out
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
