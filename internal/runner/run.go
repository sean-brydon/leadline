//go:build unix

package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// Options is one `leadline run`.
type Options struct {
	// The command and its arguments.
	Command []string
	// --name: the service to show the run under; empty works it out.
	Name string
	// The daemon's address.
	URL string
	// --no-proxy: leave database URLs alone.
	NoProxy bool
	// --no-inject: set nothing in the command's environment.
	NoInject bool
	// --quiet: do not print the line that says what was set up.
	Quiet bool

	// Where the command runs and what it inherits. Zero values mean this
	// process's own.
	Dir                   string
	Env                   []string
	Stdin, Stdout, Stderr *os.File
}

// How long the daemon gets, at the start, to say it is there, and at the
// end to take the last lines. Both are waits the command's user would feel.
const (
	helloTimeout   = 300 * time.Millisecond
	goodbyeTimeout = 700 * time.Millisecond
)

// Run runs the command and returns the exit code to leave with: the
// command's own, 128 plus the signal's number if a signal ended it, and
// 127 or 126 if it could not be started, as a shell would give.
func Run(opts Options) int {
	if opts.Stdin == nil {
		opts.Stdin = os.Stdin
	}
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	if opts.Env == nil {
		opts.Env = os.Environ()
	}
	if opts.Dir == "" {
		opts.Dir, _ = os.Getwd()
	}
	if opts.URL == "" {
		opts.URL = DefaultURL
	}

	path, err := lookPath(opts.Command[0], opts.Env, opts.Dir)
	if err != nil {
		fmt.Fprintf(opts.Stderr, "leadline: %s: %s\n", opts.Command[0], startError(err))
		return startCode(err)
	}

	daemon := NewDaemon(opts.URL)
	hello, cancel := context.WithTimeout(context.Background(), helloTimeout)
	up := daemon.Healthy(hello)
	cancel()

	plan := makePlan(opts, daemon, up)
	// For the ways out before the command has run; after it, the proxies
	// are stopped with the rest of the goodbyes, and this finds it done.
	defer plan.cleanup()

	cmd := exec.Command(path, opts.Command[1:]...)
	cmd.Args[0] = opts.Command[0]
	cmd.Dir = opts.Dir

	runID := newID()
	var register sync.Once
	var run *running
	announce := func() {
		register.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = daemon.StartRun(ctx, plan.runInfo(runID, opts, run))
		})
	}
	// When the daemon was away at the start, the exporter says when it is
	// first reached, and the run is made known then.
	exp := newExporter(daemon, plan.service, runID, up, announce, plan.tags()...)
	capt := newCapture(func(at time.Time, stream Stream, e Entry) { exp.Add(at, stream, e) })

	run, err = prepare(cmd, stdio{opts.Stdin, opts.Stdout, opts.Stderr}, capt.Offer)
	if err != nil {
		fmt.Fprintf(opts.Stderr, "leadline: %v\n", err)
		return 1
	}
	cmd.Env = plan.env
	if !opts.NoInject {
		cmd.Env = setEnv(cmd.Env, "LEADLINE_RUN_STDIO", run.streamIDs())
	}
	if !opts.Quiet {
		printNote(opts.Stderr, plan.note(up))
	}
	// Whatever happens below, the terminal goes back the way it was.
	defer run.restore()
	if err := run.start(); err != nil {
		fmt.Fprintf(opts.Stderr, "leadline: %s: %s\n", opts.Command[0], startError(err))
		return startCode(err)
	}
	if up {
		go announce()
	}

	code, sig := run.wait()

	// The command is done and the terminal is the user's again. What is
	// left is Leadline's own, and it gets a bounded moment.
	deadline := time.Now().Add(goodbyeTimeout)
	done := make(chan struct{})
	go func() {
		defer close(done)
		capt.Close()
		exp.Close(deadline)
		if up || exp.sent.Load() > 0 {
			ctx, cancel := context.WithDeadline(context.Background(), deadline)
			defer cancel()
			announce()
			_ = daemon.EndRun(ctx, runID, code, sig)
		}
		plan.cleanup()
	}()
	select {
	case <-done:
	case <-time.After(time.Until(deadline)):
	}
	return code
}

// plan is what a run sets up around the command before starting it.
type plan struct {
	service  string
	env      []string
	provider bool
	// Why the provider, though installed, was not loaded.
	providerSkipped string
	proxies         []RunProxy
	proxyIDs        []string
	// The Shipyard worktree the command runs in, when it runs in one, and
	// whether the daemon has it attached.
	worktree *Worktree
	attached bool
	// The daemon is too old to start proxies.
	noProxies bool
	daemon    *Daemon
	once      sync.Once
}

func makePlan(opts Options, daemon *Daemon, up bool) *plan {
	p := &plan{daemon: daemon, env: append([]string(nil), opts.Env...)}
	inEnv := func(key string) string { return getEnv(opts.Env, key) }
	dotenv := ReadDotenv(opts.Dir, inEnv("NODE_ENV"), inEnv)
	// What the project sets, wherever it sets it: its tools will read the
	// files, and what they find there must not be overridden from here.
	project := func(key string) string {
		if v := inEnv(key); v != "" {
			return v
		}
		return dotenv.Values[key]
	}
	p.service = ServiceName(opts.Name, project, opts.Dir)
	// In a worktree Shipyard made, the worktree is the source: the name is
	// the one `leadline attach` gave it, whatever the project calls itself.
	// The project's own name is the same in every worktree, and under it
	// two agents' work on one project would be one service. --name is then
	// the name of this command within the worktree.
	if wt, ok := WorktreeFromEnv(inEnv); ok {
		p.worktree = &wt
		p.service = wt.ServiceFor(opts.Name)
	}

	if !opts.NoInject {
		give := func(key, value string) {
			if project(key) == "" {
				p.env = setEnv(p.env, key, value)
			}
		}
		give("LEADLINE_URL", daemon.URL)
		if opts.Name != "" || p.worktree != nil {
			// A name asked for on the command line is asked of the app too,
			// and so is a worktree's.
			p.env = setEnv(p.env, "OTEL_SERVICE_NAME", p.service)
		} else {
			give("OTEL_SERVICE_NAME", p.service)
		}
		if p.worktree != nil {
			// The app's own telemetry says which worktree it is from, in
			// the resource every span, log and metric of it carries.
			if tags := p.worktree.Tags(); len(tags) > 0 {
				p.env = setEnv(p.env, "OTEL_RESOURCE_ATTRIBUTES", resourceAttributes(project("OTEL_RESOURCE_ATTRIBUTES"), tags))
			}
		}
		// An OpenTelemetry SDK pointed at nothing complains, some of them on
		// the console and every few seconds. So these only with a daemon.
		if up {
			give("OTEL_EXPORTER_OTLP_ENDPOINT", daemon.URL)
			give("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
			// An SDK's default is to send every five seconds, which in a
			// tool you watch while you work reads as "nothing arrived".
			give("OTEL_BSP_SCHEDULE_DELAY", "500")
			give("OTEL_BLRP_SCHEDULE_DELAY", "500")
		}
		// OpenTelemetry's launcher for Python also sends what the app logs,
		// and every such line is printed too, where this run captures it:
		// it would be in Leadline twice. There is nothing in Python to mark
		// the lines as the Node provider does, so the printed copy is the
		// one kept. It has no trace id; that is the price.
		if otelLauncher(opts.Command) {
			give("OTEL_LOGS_EXPORTER", "none")
		}
		if importURL, ok := NodeProvider(opts.Dir); ok {
			if nodeTakesImport(opts.Env, opts.Dir) {
				p.env = setEnv(p.env, "NODE_OPTIONS", MergeNodeOptions(inEnv("NODE_OPTIONS"), importURL))
				p.provider = true
			} else {
				p.providerSkipped = "needs Node 20.6 or newer"
			}
		}
	}

	if !opts.NoProxy && up {
		// The proxy goes in whether or not the app's own instrumentation
		// reports its queries too. The daemon pairs the two reports of a
		// statement into one query, in the request's trace and with the
		// proxy's timing, row count and parameters, which an SDK's span
		// doesn't have; and the proxy sees the statements of the drivers
		// no instrumentation covers.
		targets := FindPostgres(opts.Env, dotenv)
		byUpstream := map[string]Proxy{}
		// An attached worktree has a proxy already, on a port that is the
		// same every time, in front of each of its databases. The command
		// is pointed at that one, and nothing is opened or closed for it.
		label := p.service
		if p.worktree != nil && len(targets) > 0 {
			label = p.worktree.Service()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			conns, ok := daemon.AttachedTo(ctx, label)
			cancel()
			p.attached = ok
			for _, c := range conns {
				if listen := c.Through(); listen != "" {
					byUpstream[spellUpstream(c.Host)] = Proxy{Listen: listen, Upstream: c.Host}
				}
			}
		}
		for _, target := range targets {
			proxy, ok := byUpstream[spellUpstream(target.Upstream)]
			if !ok {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				var err error
				// Under the worktree's own name, for every command in it:
				// its queries are the worktree's, and a proxy the worktree
				// already has for this database is the one handed back.
				proxy, err = daemon.AddProxy(ctx, target.Upstream, label)
				cancel()
				if errors.Is(err, ErrNoProxies) {
					p.noProxies = true
					break
				}
				if err != nil {
					continue
				}
				byUpstream[spellUpstream(target.Upstream)] = proxy
				p.proxyIDs = append(p.proxyIDs, proxy.ID)
			}
			p.env = setEnv(p.env, target.Key, target.Through(proxy.Listen))
			p.proxies = append(p.proxies, RunProxy{Kind: "postgres", Env: target.Key, Listen: proxy.Listen, Upstream: target.Upstream, attached: proxy.ID == ""})
		}
	}
	return p
}

// tags are what the run marks its own records with: the worktree's.
func (p *plan) tags() [][2]string {
	if p.worktree == nil {
		return nil
	}
	return p.worktree.Tags()
}

// cleanup stops the proxies the run asked for. It runs once, however the
// run ends.
func (p *plan) cleanup() {
	p.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		for _, id := range p.proxyIDs {
			_ = p.daemon.RemoveProxy(ctx, id)
		}
	})
}

// note is the one line a run prints. It names addresses, never a URL: a
// database URL has a password in it.
func (p *plan) note(up bool) string {
	if !up {
		return "leadline: not running, so nothing is captured. Start the Leadline app (or run leadlined) and this command's logs will show."
	}
	parts := []string{"logs"}
	if p.provider {
		parts = append(parts, "Node provider")
	} else if p.providerSkipped != "" {
		parts = append(parts, "no Node provider ("+p.providerSkipped+")")
	}
	for _, proxy := range p.proxies {
		how := proxy.Env
		if proxy.attached {
			how += ", attached"
		}
		parts = append(parts, fmt.Sprintf("Postgres %s through %s (%s)", proxy.Upstream, proxy.Listen, how))
	}
	if p.noProxies {
		parts = append(parts, "no database proxy (this Leadline is too old for it)")
	}
	return fmt.Sprintf("leadline: %s → %s: %s", p.service, p.daemon.URL, strings.Join(parts, ", "))
}

func (p *plan) runInfo(id string, opts Options, run *running) RunInfo {
	info := RunInfo{
		ID:        id,
		Command:   Redact(strings.Join(opts.Command, " ")),
		Cwd:       opts.Dir,
		Service:   p.service,
		RunnerPID: os.Getpid(),
		Proxies:   p.proxies,
	}
	for _, arg := range opts.Command {
		info.Args = append(info.Args, Redact(arg))
	}
	if run != nil {
		info.PID = run.pid
		info.Terminal = run.master != nil
	}
	if p.provider {
		info.Provider = "node"
	}
	return info
}

// printNote writes the line dim on a terminal, plain anywhere else.
func printNote(w *os.File, note string) {
	if term.IsTerminal(int(w.Fd())) && os.Getenv("NO_COLOR") == "" {
		fmt.Fprintf(w, "\x1b[2m%s\x1b[0m\n", note)
		return
	}
	fmt.Fprintln(w, note)
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func startError(err error) string {
	switch {
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		return "command not found"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	}
	return err.Error()
}

func startCode(err error) int {
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return 127
	}
	return 126
}

// otelLauncher reports whether the command is run through
// `opentelemetry-instrument`, directly or behind a runner such as
// `uv run opentelemetry-instrument ...`.
func otelLauncher(command []string) bool {
	for i, arg := range command {
		if i < 4 && filepath.Base(arg) == "opentelemetry-instrument" {
			return true
		}
	}
	return false
}
