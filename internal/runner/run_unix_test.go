//go:build unix

package runner

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
	"google.golang.org/protobuf/proto"
)

// The test binary doubles as a command to run: with RUNNER_HELPER set it
// does that one thing and exits, before any test starts.
func TestMain(m *testing.M) {
	switch os.Getenv("RUNNER_HELPER") {
	case "stdio":
		// Print what LEADLINE_RUN_STDIO should say about this process's own
		// stdout and stderr, as the Node provider works it out.
		var ids []string
		for _, fd := range []int{1, 2} {
			var st unix.Stat_t
			if err := unix.Fstat(fd, &st); err != nil {
				os.Exit(3)
			}
			ids = append(ids, fmt.Sprintf("%d:%d", uint64(st.Dev), uint64(st.Ino)))
		}
		os.WriteFile(os.Getenv("RUNNER_HELPER_OUT"), []byte(strings.Join(ids, ",")+"\n"+os.Getenv("LEADLINE_RUN_STDIO")), 0o644)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type gotLog struct {
	Service, Body, Severity string
	Attrs                   map[string]string
}

// stub is a daemon as far as a run can tell: health, OTLP logs, runs, and
// the proxy contract (POST /api/proxies, DELETE /api/proxies/{id}).
type stub struct {
	*httptest.Server
	mu      sync.Mutex
	logs    []gotLog
	started []RunInfo
	ended   map[string]map[string]any
	asked   []map[string]string
	removed []string
	// False makes it a daemon from before proxies: the route is not there.
	proxies bool
}

func newStub(t *testing.T) *stub {
	t.Helper()
	s := &stub{ended: map[string]map[string]any{}, proxies: true}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"name":"leadline","ok":true}`)
	})
	mux.HandleFunc("POST /v1/logs", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req := &collogs.ExportLogsServiceRequest{}
		if r.Header.Get("Content-Type") != "application/x-protobuf" || proto.Unmarshal(body, req) != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, rl := range req.GetResourceLogs() {
			service := ""
			for _, kv := range rl.GetResource().GetAttributes() {
				if kv.GetKey() == "service.name" {
					service = kv.GetValue().GetStringValue()
				}
			}
			for _, sl := range rl.GetScopeLogs() {
				for _, lr := range sl.GetLogRecords() {
					l := gotLog{Service: service, Body: lr.GetBody().GetStringValue(), Severity: lr.GetSeverityText(), Attrs: map[string]string{}}
					for _, kv := range lr.GetAttributes() {
						l.Attrs[kv.GetKey()] = kv.GetValue().GetStringValue()
					}
					s.logs = append(s.logs, l)
				}
			}
		}
	})
	mux.HandleFunc("POST /api/runs", func(w http.ResponseWriter, r *http.Request) {
		var run RunInfo
		json.NewDecoder(r.Body).Decode(&run)
		s.mu.Lock()
		s.started = append(s.started, run)
		s.mu.Unlock()
		io.WriteString(w, `{}`)
	})
	mux.HandleFunc("POST /api/runs/{id}/end", func(w http.ResponseWriter, r *http.Request) {
		var end map[string]any
		json.NewDecoder(r.Body).Decode(&end)
		s.mu.Lock()
		s.ended[r.PathValue("id")] = end
		s.mu.Unlock()
		io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/api/proxies", func(w http.ResponseWriter, r *http.Request) {
		if !s.proxies || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		s.mu.Lock()
		s.asked = append(s.asked, in)
		n := len(s.asked)
		s.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]string{
			"id": "p" + strconv.Itoa(n), "kind": "postgres", "listen": "127.0.0.1:" + strconv.Itoa(61000+n), "upstream": in["upstream"],
		})
	})
	mux.HandleFunc("DELETE /api/proxies/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.removed = append(s.removed, r.PathValue("id"))
		s.mu.Unlock()
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func (s *stub) bodies() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, l := range s.logs {
		out = append(out, l.Body)
	}
	return out
}

func (s *stub) log(body string) (gotLog, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.logs {
		if l.Body == body {
			return l, true
		}
	}
	return gotLog{}, false
}

// everything is all a stub was sent, as text, for looking for what must
// never be in it.
func (s *stub) everything() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"logs": s.logs, "started": s.started, "ended": s.ended, "asked": s.asked})
	return string(b)
}

// minimalEnv is an environment with nothing of this machine's in it but
// what a command needs to start.
func minimalEnv(extra ...string) []string {
	return append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}, extra...)
}

// piped runs a command the way CI would: stdout and stderr are pipes and
// stdin is empty. It returns what the command's reader would have seen.
func piped(t *testing.T, opts Options) (code int, stdout, stderr string) {
	t.Helper()
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	var wg sync.WaitGroup
	read := func(r *os.File, into *string) {
		defer wg.Done()
		b, _ := io.ReadAll(r)
		*into = string(b)
		r.Close()
	}
	wg.Add(2)
	go read(outR, &stdout)
	go read(errR, &stderr)

	opts.Stdin, opts.Stdout, opts.Stderr = null, outW, errW
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	if opts.Env == nil {
		opts.Env = minimalEnv()
	}
	code = Run(opts)
	outW.Close()
	errW.Close()
	wg.Wait()
	return code, stdout, stderr
}

func sh(script string) []string { return []string{"/bin/sh", "-c", script} }

func TestRunPipedPassesEverythingThrough(t *testing.T) {
	d := newStub(t)
	code, stdout, stderr := piped(t, Options{
		URL:     d.URL,
		Name:    "acme-api",
		Command: sh(`echo "out one"; echo "ERROR on stderr" >&2; if [ -t 1 ]; then echo tty; else echo "not a terminal"; fi; printf 'no newline at the end'; exit 3`),
	})
	if code != 3 {
		t.Errorf("exit code %d, want the command's 3", code)
	}
	if stdout != "out one\nnot a terminal\nno newline at the end" {
		t.Errorf("stdout: %q", stdout)
	}
	// One line from leadline, then the command's stderr, untouched.
	note, rest, _ := strings.Cut(stderr, "\n")
	if rest != "ERROR on stderr\n" {
		t.Errorf("stderr after the note: %q", rest)
	}
	if !strings.HasPrefix(note, "leadline: acme-api → "+d.URL) || strings.Contains(note, "\x1b") {
		t.Errorf("note: %q", note)
	}

	for body, want := range map[string]gotLog{
		"out one":               {Severity: "INFO", Attrs: map[string]string{"log.iostream": "stdout"}},
		"ERROR on stderr":       {Severity: "ERROR", Attrs: map[string]string{"log.iostream": "stderr"}},
		"not a terminal":        {Severity: "INFO", Attrs: map[string]string{"log.iostream": "stdout"}},
		"no newline at the end": {Severity: "INFO", Attrs: map[string]string{"log.iostream": "stdout"}},
	} {
		got, ok := d.log(body)
		if !ok {
			t.Errorf("no log %q in %q", body, d.bodies())
			continue
		}
		if got.Service != "acme-api" || got.Severity != want.Severity || got.Attrs["log.iostream"] != want.Attrs["log.iostream"] {
			t.Errorf("%q: %+v", body, got)
		}
		if got.Attrs[SourceAttr] != "run" || got.Attrs["leadline.run.id"] == "" {
			t.Errorf("%q does not say it came from run: %v", body, got.Attrs)
		}
	}
	if n := len(d.bodies()); n != 4 {
		t.Errorf("%d logs, want 4: %q", n, d.bodies())
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.started) != 1 {
		t.Fatalf("runs started: %+v", d.started)
	}
	run := d.started[0]
	if run.Service != "acme-api" || run.PID <= 0 || run.RunnerPID != os.Getpid() || run.Terminal || !strings.HasPrefix(run.Command, "/bin/sh -c echo") {
		t.Errorf("run: %+v", run)
	}
	end := d.ended[run.ID]
	if end == nil || end["exit_code"] != float64(3) || end["signal"] != "" {
		t.Errorf("end: %v", end)
	}
}

func TestRunExitCodes(t *testing.T) {
	d := newStub(t)
	for _, tc := range []struct {
		name    string
		command []string
		want    int
		stderr  string
	}{
		{"success", sh("exit 0"), 0, ""},
		{"failure", sh("exit 42"), 42, ""},
		{"killed by a signal", sh("kill -TERM $$"), 128 + int(syscall.SIGTERM), ""},
		{"killed outright", sh("kill -KILL $$"), 128 + int(syscall.SIGKILL), ""},
		{"not found", []string{"no-such-command-acme"}, 127, "leadline: no-such-command-acme: command not found\n"},
		{"not found by path", []string{"./nope"}, 127, "leadline: ./nope: command not found\n"},
		{"not executable", []string{os.DevNull}, 126, "leadline: " + os.DevNull + ": permission denied\n"},
	} {
		code, _, stderr := piped(t, Options{URL: d.URL, Quiet: true, Command: tc.command})
		if code != tc.want {
			t.Errorf("%s: exit code %d, want %d", tc.name, code, tc.want)
		}
		if stderr != tc.stderr {
			t.Errorf("%s: stderr %q, want %q", tc.name, stderr, tc.stderr)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var signals []string
	for _, end := range d.ended {
		if s, _ := end["signal"].(string); s != "" {
			signals = append(signals, s)
		}
	}
	if len(signals) != 2 || !strings.Contains(strings.Join(signals, " "), "SIGTERM") || !strings.Contains(strings.Join(signals, " "), "SIGKILL") {
		t.Errorf("signals reported: %v", signals)
	}
}

func TestRunPassesSignalsOn(t *testing.T) {
	d := newStub(t)
	for _, tc := range []struct {
		sig  syscall.Signal
		name string
	}{{syscall.SIGTERM, "TERM"}, {syscall.SIGHUP, "HUP"}, {syscall.SIGUSR1, "USR1"}} {
		null, _ := os.Open(os.DevNull)
		outR, outW, _ := os.Pipe()
		done := make(chan int, 1)
		go func() {
			done <- Run(Options{
				URL: d.URL, Quiet: true, Dir: t.TempDir(), Env: minimalEnv(),
				Stdin: null, Stdout: outW, Stderr: outW,
				Command: sh(`trap 'echo "caught ` + tc.name + `"; exit 7' ` + tc.name + `; echo ready; while :; do sleep 0.05; done`),
			})
			outW.Close()
		}()
		lines := bufio.NewScanner(outR)
		if !lines.Scan() || lines.Text() != "ready" {
			t.Fatalf("%s: the command did not start: %q", tc.name, lines.Text())
		}
		// To this process, as a terminal closing or `kill` would send it.
		syscall.Kill(os.Getpid(), tc.sig)
		select {
		case code := <-done:
			if code != 7 {
				t.Errorf("%s: exit code %d; the command's own handler exits 7", tc.name, code)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s was not passed on", tc.name)
		}
		if !lines.Scan() || lines.Text() != "caught "+tc.name {
			t.Errorf("%s: the command printed %q", tc.name, lines.Text())
		}
		outR.Close()
		null.Close()
	}
}

func TestRunSetsTheCommandUp(t *testing.T) {
	d := newStub(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "env.txt")
	// A local database in the environment, another in .env, and a remote
	// one that must come through untouched. .env also sets a variable run
	// would otherwise set.
	write(t, filepath.Join(dir, ".env"), "SHADOW_DATABASE_URL=postgres://app:"+secret+"@127.0.0.1:5433/shadow\nOTEL_EXPORTER_OTLP_PROTOCOL=http/json\n")
	const remote = "postgres://app:" + secret + "@db.acme.example:5432/acme"
	code, stdout, stderr := piped(t, Options{
		URL: d.URL,
		Dir: dir,
		Env: minimalEnv(
			"DATABASE_URL=postgres://app:"+secret+"@localhost:5432/acme?application_name=orders",
			"REPLICA_URL="+remote,
			"NODE_OPTIONS=--max-old-space-size=512",
		),
		Command: sh(`env > "` + out + `"; echo started`),
	})
	if code != 0 || stdout != "started\n" {
		t.Fatalf("code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	env := map[string]string{}
	b, _ := os.ReadFile(out)
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			env[k] = v
		}
	}
	service := filepath.Base(dir)
	for key, want := range map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": d.URL,
		"LEADLINE_URL":                d.URL,
		"OTEL_SERVICE_NAME":           service,
		"OTEL_BSP_SCHEDULE_DELAY":     "500",
		// Rewritten to the proxies, everything else as it was.
		"DATABASE_URL":        "postgres://app:" + secret + "@127.0.0.1:61001/acme?application_name=orders",
		"SHADOW_DATABASE_URL": "postgres://app:" + secret + "@127.0.0.1:61002/shadow",
		// A remote database is left alone.
		"REPLICA_URL": remote,
		// No provider is installed here, so NODE_OPTIONS is as it was.
		"NODE_OPTIONS": "--max-old-space-size=512",
	} {
		if env[key] != want {
			t.Errorf("%s = %q, want %q", key, env[key], want)
		}
	}
	// The project's own .env sets the protocol; its tools will read it there.
	if v, ok := env["OTEL_EXPORTER_OTLP_PROTOCOL"]; ok {
		t.Errorf("OTEL_EXPORTER_OTLP_PROTOCOL was set to %q over the project's .env", v)
	}
	if env["LEADLINE_RUN_STDIO"] == "" {
		t.Error("LEADLINE_RUN_STDIO is not set")
	}

	d.mu.Lock()
	asked, removed, started := d.asked, d.removed, d.started
	d.mu.Unlock()
	want := []map[string]string{
		{"kind": "postgres", "upstream": "localhost:5432", "label": service},
		{"kind": "postgres", "upstream": "127.0.0.1:5433", "label": service},
	}
	if !reflect.DeepEqual(asked, want) {
		t.Errorf("proxies asked for: %v", asked)
	}
	if !reflect.DeepEqual(removed, []string{"p1", "p2"}) {
		t.Errorf("proxies removed at the end: %v", removed)
	}
	if len(started) != 1 || len(started[0].Proxies) != 2 || started[0].Proxies[0] != (RunProxy{Kind: "postgres", Env: "DATABASE_URL", Listen: "127.0.0.1:61001", Upstream: "localhost:5432"}) {
		t.Errorf("run: %+v", started)
	}

	// What was set up is said once, with addresses and never a URL.
	note := strings.TrimSpace(stderr)
	if strings.Count(note, "\n") != 0 {
		t.Errorf("more than one line: %q", stderr)
	}
	for _, part := range []string{"logs", "Postgres localhost:5432 through 127.0.0.1:61001 (DATABASE_URL)", "Postgres 127.0.0.1:5433 through 127.0.0.1:61002 (SHADOW_DATABASE_URL)"} {
		if !strings.Contains(note, part) {
			t.Errorf("note lacks %q: %q", part, note)
		}
	}
	for name, text := range map[string]string{"output": stdout + stderr, "what the daemon was sent": d.everything()} {
		if strings.Contains(text, secret) || strings.Contains(text, "app:") {
			t.Errorf("a database password is in %s:\n%s", name, text)
		}
		if strings.Contains(text, "db.acme.example") {
			t.Errorf("the remote database is mentioned in %s", name)
		}
	}
}

func TestRunLeavesTheEnvironmentAloneWhenAsked(t *testing.T) {
	d := newStub(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "env.txt")
	const local = "postgres://app@localhost:5432/acme"
	code, _, stderr := piped(t, Options{
		URL: d.URL, Dir: dir, NoInject: true, NoProxy: true,
		Env:     minimalEnv("DATABASE_URL=" + local),
		Command: sh(`env > "` + out + `"`),
	})
	if code != 0 {
		t.Fatalf("code %d, %q", code, stderr)
	}
	b, _ := os.ReadFile(out)
	for _, line := range strings.Split(string(b), "\n") {
		key, value, _ := strings.Cut(line, "=")
		if strings.HasPrefix(key, "OTEL_") || strings.HasPrefix(key, "LEADLINE_") || key == "NODE_OPTIONS" {
			t.Errorf("--no-inject, yet %s is set", key)
		}
		if key == "DATABASE_URL" && value != local {
			t.Errorf("--no-proxy, yet DATABASE_URL is %q", value)
		}
	}
	if len(d.asked) != 0 {
		t.Errorf("--no-proxy, yet a proxy was asked for: %v", d.asked)
	}
	// The logs are still captured: that needs nothing from the command.
	if len(d.started) != 1 {
		t.Errorf("runs: %+v", d.started)
	}
}

func TestRunWithADaemonTooOldForProxies(t *testing.T) {
	d := newStub(t)
	d.proxies = false
	dir := t.TempDir()
	out := filepath.Join(dir, "url.txt")
	const local = "postgres://app@localhost:5432/acme"
	code, stdout, stderr := piped(t, Options{
		URL: d.URL, Dir: dir,
		Env:     minimalEnv("DATABASE_URL="+local, "OTHER_URL=postgres://app@localhost:5433/other"),
		Command: sh(`printf %s "$DATABASE_URL" > "` + out + `"; echo fine`),
	})
	if code != 0 || stdout != "fine\n" {
		t.Fatalf("code %d, stdout %q", code, stdout)
	}
	if b, _ := os.ReadFile(out); string(b) != local {
		t.Errorf("DATABASE_URL was changed to %q with no proxy behind it", b)
	}
	// Said once, in the one line, however many databases there were.
	if strings.Count(stderr, "\n") != 1 || strings.Count(stderr, "no database proxy") != 1 {
		t.Errorf("stderr: %q", stderr)
	}
}

func TestRunWithNoDaemon(t *testing.T) {
	// A port that was just free: nothing answers there.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "http://" + ln.Addr().String()
	ln.Close()

	dir := t.TempDir()
	out := filepath.Join(dir, "env.txt")
	const local = "postgres://app@localhost:5432/acme"
	began := time.Now()
	code, stdout, stderr := piped(t, Options{
		URL: url, Dir: dir,
		Env:     minimalEnv("DATABASE_URL=" + local),
		Command: sh(`env > "` + out + `"; echo one; echo two >&2; exit 5`),
	})
	took := time.Since(began)
	if code != 5 || stdout != "one\n" {
		t.Errorf("code %d, stdout %q", code, stdout)
	}
	note, rest, _ := strings.Cut(stderr, "\n")
	if rest != "two\n" {
		t.Errorf("stderr after the note: %q", rest)
	}
	if !strings.Contains(note, "not running") || !strings.Contains(note, "leadlined") {
		t.Errorf("the note does not say Leadline is down and how to start it: %q", note)
	}
	if took > 2*time.Second {
		t.Errorf("a missing daemon cost %s", took)
	}
	b, _ := os.ReadFile(out)
	env := string(b)
	if !strings.Contains(env, "DATABASE_URL="+local+"\n") {
		t.Error("DATABASE_URL was changed with no daemon to proxy it")
	}
	// An OpenTelemetry SDK pointed at nothing complains; so it is not.
	if strings.Contains(env, "OTEL_EXPORTER_OTLP_ENDPOINT") {
		t.Error("OTEL_EXPORTER_OTLP_ENDPOINT points at a daemon that is not there")
	}
}

func TestRunLoadsTheNodeProvider(t *testing.T) {
	d := newStub(t)
	dir := t.TempDir()
	pkg := filepath.Join(dir, "node_modules", "@leadline", "node")
	write(t, filepath.Join(pkg, "package.json"), `{"exports":{"./register":{"default":"./dist/register.js"}}}`)
	write(t, filepath.Join(pkg, "dist", "register.js"), "")
	write(t, filepath.Join(dir, "package.json"), `{"name":"acme-web"}`)
	// A `node` that only knows its version, first on the PATH.
	bin := filepath.Join(dir, "bin")
	write(t, filepath.Join(bin, "node"), "#!/bin/sh\necho v22.4.0\n")
	os.Chmod(filepath.Join(bin, "node"), 0o755)
	out := filepath.Join(dir, "options.txt")

	run := func(nodeVersion string, extra ...string) (string, string) {
		write(t, filepath.Join(bin, "node"), "#!/bin/sh\necho "+nodeVersion+"\n")
		os.Remove(out)
		env := append([]string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH")}, extra...)
		code, _, stderr := piped(t, Options{URL: d.URL, Dir: dir, Env: env, Command: sh(`printf %s "$NODE_OPTIONS" > "` + out + `"`)})
		if code != 0 {
			t.Fatalf("code %d: %s", code, stderr)
		}
		b, _ := os.ReadFile(out)
		return string(b), stderr
	}

	real, _ := filepath.EvalSymlinks(pkg)
	options, note := run("v22.4.0", "NODE_OPTIONS=--inspect")
	if !strings.HasPrefix(options, "--inspect --import file://") || !strings.HasSuffix(options, "/node_modules/@leadline/node/dist/register.js") {
		t.Errorf("NODE_OPTIONS = %q (package at %s)", options, real)
	}
	if !strings.Contains(note, "acme-web") || !strings.Contains(note, "Node provider") {
		t.Errorf("note: %q", note)
	}
	// A Node that would refuse the flag is not given it.
	options, note = run("v18.19.0", "NODE_OPTIONS=--inspect")
	if options != "--inspect" || !strings.Contains(note, "needs Node 20.6") {
		t.Errorf("old Node: NODE_OPTIONS = %q, note %q", options, note)
	}
}

func TestRunSkipsLinesTheProviderSent(t *testing.T) {
	d := newStub(t)
	// As the Node provider prints under run: its own console lines carry
	// the mark; a line from anything else does not.
	script := `printf 'plain before\n'; printf '%s\n' 'from console.log` + Mark + `' 'Error: sent too` + Mark + `' '    at f (x.js:1:1)` + Mark + `'; printf 'plain after\n'`
	code, stdout, _ := piped(t, Options{URL: d.URL, Quiet: true, Command: sh(script)})
	if code != 0 {
		t.Fatal(code)
	}
	// The reader gets every line, and no mark.
	if stdout != "plain before\nfrom console.log\nError: sent too\n    at f (x.js:1:1)\nplain after\n" {
		t.Errorf("stdout: %q", stdout)
	}
	if got := d.bodies(); !reflect.DeepEqual(got, []string{"plain before", "plain after"}) {
		t.Errorf("captured %q; the marked lines are the provider's to send", got)
	}
}

func TestRunNamesItsStreamsAsTheChildSeesThem(t *testing.T) {
	d := newStub(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "ids.txt")
	check := func(what string, code int) {
		t.Helper()
		if code != 0 {
			t.Fatalf("%s: helper exited %d", what, code)
		}
		b, _ := os.ReadFile(out)
		seen, named, _ := strings.Cut(string(b), "\n")
		if seen == "" || seen != named {
			t.Errorf("%s: the child's fstat says %q, LEADLINE_RUN_STDIO says %q", what, seen, named)
		}
	}
	env := minimalEnv("RUNNER_HELPER=stdio", "RUNNER_HELPER_OUT="+out)

	code, _, _ := piped(t, Options{URL: d.URL, Quiet: true, Dir: dir, Env: env, Command: []string{self}})
	check("pipes", code)

	tty := openTerminal(t)
	code = Run(Options{URL: d.URL, Quiet: true, Dir: dir, Env: env, Command: []string{self}, Stdin: tty.slave, Stdout: tty.slave, Stderr: tty.slave})
	check("a terminal", code)

	// A child that sends its own stdout elsewhere must not match: that is
	// what keeps marks out of `node build.js > out.json`.
	redirected := filepath.Join(dir, "redirected.txt")
	code, _, _ = piped(t, Options{URL: d.URL, Quiet: true, Dir: dir, Env: env, Command: sh(`"` + self + `" > "` + redirected + `"`)})
	if code != 0 {
		t.Fatal(code)
	}
	b, _ := os.ReadFile(out)
	seen, named, _ := strings.Cut(string(b), "\n")
	if strings.Split(seen, ",")[0] == strings.Split(named, ",")[0] {
		t.Errorf("a redirected stdout (%s) still matches the named one (%s)", seen, named)
	}
}

// terminal is a pseudo-terminal standing in for the one a person sits at:
// the test holds the screen-and-keyboard side, run gets the other.
type terminal struct {
	master, slave *os.File
	mu            sync.Mutex
	screen        strings.Builder
}

func openTerminal(t *testing.T) *terminal {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Skipf("no pseudo-terminal here: %v", err)
	}
	tty := &terminal{master: master, slave: slave}
	pty.Setsize(master, &pty.Winsize{Rows: 33, Cols: 111})
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			tty.mu.Lock()
			tty.screen.Write(buf[:n])
			tty.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		slave.Close()
		master.Close()
	})
	return tty
}

func (tty *terminal) shown() string {
	tty.mu.Lock()
	defer tty.mu.Unlock()
	return tty.screen.String()
}

func (tty *terminal) waitFor(t *testing.T, text string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(tty.shown(), text) {
		if time.Now().After(deadline) {
			t.Fatalf("%q never appeared; the terminal shows %q", text, tty.shown())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (tty *terminal) state(t *testing.T) *term.State {
	t.Helper()
	state, err := term.GetState(int(tty.slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func (tty *terminal) run(d *stub, dir string, command []string) chan int {
	done := make(chan int, 1)
	go func() {
		done <- Run(Options{URL: d.URL, Dir: dir, Env: minimalEnv("TERM=xterm-256color"), Command: command, Stdin: tty.slave, Stdout: tty.slave, Stderr: tty.slave})
	}()
	return done
}

func waitExit(t *testing.T, done chan int) int {
	t.Helper()
	select {
	case code := <-done:
		return code
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return")
		return -1
	}
}

func TestRunInATerminalFeelsDirect(t *testing.T) {
	d := newStub(t)
	tty := openTerminal(t)
	before := tty.state(t)

	// The command finds a terminal of the real one's size, can colour its
	// output, and reads what is typed.
	done := tty.run(d, t.TempDir(), sh(`if [ -t 0 ] && [ -t 1 ] && [ -t 2 ]; then echo "all three are terminals"; fi; echo "size $(stty size)"; printf '\033[32mgreen\033[0m\n'; printf 'name? '; read name; echo "hello $name"; exit 4`))
	tty.waitFor(t, "name? ")
	tty.master.WriteString("ada\r")
	if code := waitExit(t, done); code != 4 {
		t.Errorf("exit code %d, want 4", code)
	}
	screen := tty.shown()
	for _, want := range []string{"all three are terminals", "size 33 111", "\x1b[32mgreen\x1b[0m", "hello ada"} {
		if !strings.Contains(screen, want) {
			t.Errorf("the terminal never showed %q:\n%q", want, screen)
		}
	}
	// The one line from leadline is dim, on a terminal.
	if !strings.Contains(screen, "\x1b[2mleadline: ") {
		t.Errorf("no dim note: %q", screen)
	}
	if after := tty.state(t); !reflect.DeepEqual(before, after) {
		t.Error("the terminal's settings were not put back")
	}

	// Captured as text, without the colour, and the prompt with its answer.
	bodies := d.bodies()
	for _, want := range []string{"all three are terminals", "size 33 111", "green", "name? ada", "hello ada"} {
		found := false
		for _, body := range bodies {
			found = found || body == want
		}
		if !found {
			t.Errorf("no log %q in %q", want, bodies)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.started) != 1 || !d.started[0].Terminal {
		t.Errorf("run: %+v", d.started)
	}
}

func TestRunRestoresTheTerminalWhenTheCommandIsKilled(t *testing.T) {
	d := newStub(t)
	tty := openTerminal(t)
	before := tty.state(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")

	done := tty.run(d, dir, sh(`echo $$ > "`+pidFile+`"; echo started; exec sleep 60`))
	tty.waitFor(t, "started")
	// While the command runs the real terminal is raw; if it were not, this
	// test could not tell a restored terminal from an untouched one.
	if reflect.DeepEqual(before, tty.state(t)) {
		t.Fatal("the terminal was never put in raw mode")
	}
	b, _ := os.ReadFile(pidFile)
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	syscall.Kill(pid, syscall.SIGKILL)

	if code := waitExit(t, done); code != 128+int(syscall.SIGKILL) {
		t.Errorf("exit code %d, want %d", code, 128+int(syscall.SIGKILL))
	}
	if after := tty.state(t); !reflect.DeepEqual(before, after) {
		t.Error("the terminal was left raw after the command was killed")
	}
}

func TestRunInATerminalCtrlCReachesTheCommand(t *testing.T) {
	d := newStub(t)
	tty := openTerminal(t)
	before := tty.state(t)

	// Ctrl-C is a key: it goes to the command's terminal, which interrupts
	// the command, exactly as when it is run directly.
	done := tty.run(d, t.TempDir(), sh(`trap 'echo interrupted; exit 9' INT; echo waiting; while :; do sleep 0.05; done`))
	tty.waitFor(t, "waiting")
	tty.master.Write([]byte{3})
	if code := waitExit(t, done); code != 9 {
		t.Errorf("exit code %d; the command's own handler exits 9", code)
	}
	tty.waitFor(t, "interrupted")

	if after := tty.state(t); !reflect.DeepEqual(before, after) {
		t.Error("the terminal's settings were not put back")
	}

	// And with no handler, the command dies of the signal: 130, as a shell
	// says. On a terminal of its own: the first run's reader of the keyboard
	// is still there in this process, where a real one has exited.
	tty = openTerminal(t)
	before = tty.state(t)
	done = tty.run(d, t.TempDir(), sh(`echo waiting again; exec sleep 60`))
	tty.waitFor(t, "waiting again")
	tty.master.Write([]byte{3})
	if code := waitExit(t, done); code != 128+int(syscall.SIGINT) {
		t.Errorf("exit code %d, want 130", code)
	}
	if after := tty.state(t); !reflect.DeepEqual(before, after) {
		t.Error("the terminal's settings were not put back")
	}
}

func TestRunInATerminalFollowsItsSize(t *testing.T) {
	d := newStub(t)
	tty := openTerminal(t)
	done := tty.run(d, t.TempDir(), sh(`trap 'echo "now $(stty size)"; exit 0' WINCH; echo "first $(stty size)"; while :; do sleep 0.05; done`))
	tty.waitFor(t, "first 33 111")
	pty.Setsize(tty.master, &pty.Winsize{Rows: 50, Cols: 160})
	// The kernel tells the foreground job of the terminal that was resized,
	// which here is this process.
	syscall.Kill(os.Getpid(), syscall.SIGWINCH)
	if code := waitExit(t, done); code != 0 {
		t.Errorf("exit code %d", code)
	}
	tty.waitFor(t, "now 50 160")
}

func TestRunKeepsStderrApartWhenItIsRedirected(t *testing.T) {
	d := newStub(t)
	tty := openTerminal(t)
	errR, errW, _ := os.Pipe()
	done := make(chan int, 1)
	go func() {
		// As in `leadline run -- cmd 2>errors.log` at a terminal.
		done <- Run(Options{URL: d.URL, Quiet: true, Dir: t.TempDir(), Env: minimalEnv(), Stdin: tty.slave, Stdout: tty.slave, Stderr: errW,
			Command: sh(`echo "to the screen"; echo "to the file" >&2; if [ -t 1 ]; then echo "stdout is a terminal"; fi`)})
		errW.Close()
	}()
	b, _ := io.ReadAll(errR)
	if code := waitExit(t, done); code != 0 {
		t.Errorf("exit code %d", code)
	}
	if string(b) != "to the file\n" {
		t.Errorf("stderr got %q", b)
	}
	if screen := tty.shown(); !strings.Contains(screen, "to the screen") || !strings.Contains(screen, "stdout is a terminal") || strings.Contains(screen, "to the file") {
		t.Errorf("the terminal shows %q", screen)
	}
	if got, ok := d.log("to the file"); !ok || got.Attrs["log.iostream"] != "stderr" {
		t.Errorf("log: %+v %v", got, ok)
	}
}

func TestRunStartsLateWhenTheDaemonAppears(t *testing.T) {
	// Nothing listens at first; the daemon comes up while the command runs.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	dir := t.TempDir()
	gate := filepath.Join(dir, "go")
	outR, outW, _ := os.Pipe()
	null, _ := os.Open(os.DevNull)
	defer null.Close()
	done := make(chan int, 1)
	go func() {
		done <- Run(Options{URL: "http://" + addr, Quiet: true, Dir: dir, Env: minimalEnv(), Stdin: null, Stdout: outW, Stderr: outW,
			Command: sh(`echo early; while [ ! -e "` + gate + `" ]; do sleep 0.05; done; echo late`)})
		outW.Close()
	}()
	lines := bufio.NewScanner(outR)
	if !lines.Scan() || lines.Text() != "early" {
		t.Fatal("the command did not start")
	}

	d := newStub(t)
	d.Server.Close()
	late, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("could not take the port back: %v", err)
	}
	d.Server = httptest.NewUnstartedServer(d.Config.Handler)
	d.Server.Listener.Close()
	d.Server.Listener = late
	d.Server.Start()
	t.Cleanup(d.Server.Close)

	// Past the wait between looks for the daemon.
	time.Sleep(retryAfter + 200*time.Millisecond)
	os.WriteFile(gate, nil, 0o644)
	if code := waitExit(t, done); code != 0 {
		t.Errorf("exit code %d", code)
	}
	// Reading on also keeps the pipe's far end open: a reader that is
	// collected closes it, and the command's output then has nowhere to go.
	if !lines.Scan() || lines.Text() != "late" {
		t.Errorf("the command's second line did not come through: %q", lines.Text())
	}
	// What was printed while it was away is gone; what came after is there,
	// and the run is known.
	if got := d.bodies(); !reflect.DeepEqual(got, []string{"late"}) {
		t.Errorf("logs: %q", got)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.started) != 1 || len(d.ended) != 1 {
		t.Errorf("started %d, ended %d", len(d.started), len(d.ended))
	}
}

func TestRunWhenTheReaderGoesAway(t *testing.T) {
	d := newStub(t)
	// As in `leadline run -- cmd | head -1`, once head has left.
	outR, outW, _ := os.Pipe()
	outR.Close()
	defer outW.Close()
	null, _ := os.Open(os.DevNull)
	defer null.Close()
	done := make(chan int, 1)
	go func() {
		done <- Run(Options{URL: d.URL, Quiet: true, Dir: t.TempDir(), Env: minimalEnv(), Stdin: null, Stdout: outW, Stderr: outW,
			Command: sh(`echo first; sleep 0.3; echo second; sleep 0.3; echo third; exit 0`)})
	}()
	// Run directly, the command's next write would end it with SIGPIPE.
	// Here it learns the same one write later, and this process outlives it.
	if code := waitExit(t, done); code != 128+int(syscall.SIGPIPE) {
		t.Errorf("exit code %d, want %d: the command should have been told nobody is reading", code, 128+int(syscall.SIGPIPE))
	}
	if got := d.bodies(); !reflect.DeepEqual(got, []string{"first"}) {
		t.Errorf("logs: %q", got)
	}
}

// The proxy and the app's own instrumentation run together: the daemon
// counts a statement both report once, so there is nothing to choose.
func TestRunUsesTheProxyBesideTheProvider(t *testing.T) {
	d := newStub(t)
	dir := t.TempDir()
	provider := filepath.Join(dir, "node_modules", "@leadline", "node")
	write(t, filepath.Join(provider, "package.json"), `{}`)
	write(t, filepath.Join(provider, "dist", "register.js"), "")
	write(t, filepath.Join(dir, "node_modules", "pg", "package.json"), `{"name":"pg"}`)
	bin := filepath.Join(dir, "bin")
	write(t, filepath.Join(bin, "node"), "#!/bin/sh\necho v22.4.0\n")
	os.Chmod(filepath.Join(bin, "node"), 0o755)
	write(t, filepath.Join(bin, "opentelemetry-instrument"), "#!/bin/sh\nexec \"$@\"\n")
	os.Chmod(filepath.Join(bin, "opentelemetry-instrument"), 0o755)
	out := filepath.Join(dir, "url.txt")
	const local = "postgres://app@localhost:5432/acme"
	env := []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "DATABASE_URL=" + local}
	print := `printf %s "$DATABASE_URL" > "` + out + `"`

	run := func(opts Options) (url, note string) {
		t.Helper()
		opts.URL, opts.Dir, opts.Env = d.URL, dir, env
		code, _, stderr := piped(t, opts)
		if code != 0 {
			t.Fatalf("code %d: %s", code, stderr)
		}
		b, _ := os.ReadFile(out)
		return string(b), stderr
	}
	const proxied = "postgres://app@127.0.0.1:"

	// The provider is loaded and the project has the driver it instruments:
	// the proxy is there all the same.
	if url, note := run(Options{Command: sh(print)}); !strings.HasPrefix(url, proxied) ||
		!strings.Contains(note, "Node provider") || !strings.Contains(note, "through") {
		t.Errorf("provider and pg: DATABASE_URL %q, note %q", url, note)
	}
	// So it is under OpenTelemetry's launcher for Python.
	if url, note := run(Options{Command: []string{"opentelemetry-instrument", "/bin/sh", "-c", print}}); !strings.HasPrefix(url, proxied) || !strings.Contains(note, "through") {
		t.Errorf("opentelemetry-instrument: DATABASE_URL %q, note %q", url, note)
	}
	// --no-proxy is the way to leave the database URL alone.
	if url, _ := run(Options{NoProxy: true, Command: sh(print)}); url != local {
		t.Errorf("--no-proxy: DATABASE_URL %q", url)
	}

	// It would also send every log line that this run captures off the
	// terminal. One copy is kept: the printed one.
	env = append(env, "CHECK=1")
	print = `printf %s "$OTEL_LOGS_EXPORTER" > "` + out + `"`
	if exporter, _ := run(Options{Command: []string{"opentelemetry-instrument", "/bin/sh", "-c", print}}); exporter != "none" {
		t.Errorf("OTEL_LOGS_EXPORTER under the launcher: %q", exporter)
	}
	if exporter, _ := run(Options{Command: sh(print)}); exporter != "" {
		t.Errorf("OTEL_LOGS_EXPORTER without the launcher: %q", exporter)
	}
	env = append(env, "OTEL_LOGS_EXPORTER=otlp")
	if exporter, _ := run(Options{Command: []string{"opentelemetry-instrument", "/bin/sh", "-c", print}}); exporter != "otlp" {
		t.Errorf("the project's own OTEL_LOGS_EXPORTER was overridden: %q", exporter)
	}
}
