package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sean-brydon/leadline/internal/runner"
)

const attachUsage = `usage: leadline attach [flags]

Makes a worktree a source of its own in Leadline: it shows under one name,
"<project>/<worktree>", and the databases its environment names are saved
as connections that belong to it, with their queries recorded. Made to be
run by a hook when a tool such as Shipyard creates a worktree ("leadline
shipyard hooks" prints the hooks), and safe to run by hand.

  leadline attach                         in a Shipyard hook or worktree
  leadline attach --project acme --name fix-orders --path ~/work/acme-fix-orders
  leadline attach --no-env --url DATABASE_URL=postgres://localhost:5432/acme_fix

It reads the worktree from the variables Shipyard sets (BERTH_LOCATION,
BERTH_WORKTREE_NAME or a hook's BERTH_NAME, BERTH_WORKTREE_PATH or BERTH_PATH,
BERTH_BRANCH, BERTH_BOX), and a flag outranks a variable. The connections are
the Postgres and Redis URLs in its environment and its .env files
(DATABASE_URL and the like); nothing else is read out of them, and no
password is printed. Running it again changes nothing, unless a URL has
changed: then what it owns is updated.

  --path <dir>       the worktree's folder (default: Shipyard's, else here)
  --name <name>      the worktree's name (default: Shipyard's, else git's)
  --project <name>   the project it is a checkout of (default: Shipyard's
                     location, else the main checkout's folder)
  --branch <name>    its branch, and --box <name> its machine, for the record
  --url KEY=URL      a connection to attach besides what is found; repeatable
  --env-dir <dir>    also read the .env files of this folder, as in apps/web
                     (relative to the worktree); repeatable
  --no-env           read no environment and no .env file: only --url
  --no-capture       save the connections for their statistics only
  --daemon <address> the daemon's address (default: LEADLINE_URL, else
                     http://127.0.0.1:4320)
  --strict           exit 1 when the worktree could not be attached. Without
                     it a hook is never failed: with Leadline not running it
                     says so in one line and exits 0
  --quiet            say nothing when it worked
`

// many is a flag that may be given more than once.
type many []string

func (m *many) String() string     { return strings.Join(*m, ",") }
func (m *many) Set(v string) error { *m = append(*m, v); return nil }

// worktreeFlags are the flags attach and detach share: which worktree, and
// which daemon.
type worktreeFlags struct {
	path, name, project, branch, box string
	daemon                           string
	strict, quiet                    bool
}

// lookup reads a variable out of an environment given as KEY=value lines.
// The last one wins, as it does for the process that gets them.
func lookup(environ []string) func(string) string {
	return func(key string) string {
		for i := len(environ) - 1; i >= 0; i-- {
			if v, ok := strings.CutPrefix(environ[i], key+"="); ok {
				return v
			}
		}
		return ""
	}
}

func (f *worktreeFlags) register(fset *flag.FlagSet, env func(string) string) {
	fset.StringVar(&f.path, "path", "", "")
	fset.StringVar(&f.name, "name", "", "")
	fset.StringVar(&f.project, "project", "", "")
	fset.StringVar(&f.branch, "branch", "", "")
	fset.StringVar(&f.box, "box", "", "")
	daemon := env("LEADLINE_URL")
	if daemon == "" {
		daemon = runner.DefaultURL
	}
	fset.StringVar(&f.daemon, "daemon", daemon, "")
	fset.BoolVar(&f.strict, "strict", false, "")
	fset.BoolVar(&f.quiet, "quiet", false, "")
	fset.BoolVar(&f.quiet, "q", false, "")
}

// errNoWorktree means a hook ran for an event that names no worktree.
var errNoWorktree = errors.New("names no worktree")

// worktree settles which worktree a command is about: Shipyard's variables,
// then the folder, and a flag over either.
func (f *worktreeFlags) worktree(env func(string) string) (runner.Worktree, error) {
	wt, shipyard := runner.WorktreeFromEnv(env)
	named := f.name != "" || f.project != "" || f.path != ""
	if hook, has := runner.InHook(env); hook && !has && !named {
		// A hook on some other event (a session's, a box's). Its BERTH_NAME,
		// if it has one, is not a worktree's, and going by the environment
		// would attach or detach the wrong thing.
		return wt, fmt.Errorf("the event %s %w", env("BERTH_EVENT"), errNoWorktree)
	}
	path := f.path
	if path == "" {
		path = wt.Path
	}
	if path == "" {
		path, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if !shipyard {
		wt = runner.WorktreeAt(path)
	}
	wt.Path = path
	if f.project != "" {
		wt.Project = f.project
	}
	if f.name != "" {
		wt.Name = f.name
	}
	if f.branch != "" {
		wt.Branch = f.branch
	}
	if f.box != "" {
		wt.Box = f.box
	}
	if wt.Service() == "" {
		return wt, errors.New("no project or worktree name: give --project and --name")
	}
	return wt, nil
}

// soft is how attach and detach leave when they could not do their work:
// one line, and exit 0 unless --strict. They run in hooks, and a hook must
// not fail because Leadline is not there.
func (f *worktreeFlags) soft(stderr io.Writer, format string, args ...any) int {
	fmt.Fprintf(stderr, "leadline: "+format+"\n", args...)
	if f.strict {
		return 1
	}
	return 0
}

// attachCommand is `leadline attach`, in the environment given. It returns
// the exit code.
func attachCommand(args, environ []string, stdout, stderr io.Writer) int {
	env := lookup(environ)
	fset := flag.NewFlagSet("attach", flag.ContinueOnError)
	fset.SetOutput(stderr)
	fset.Usage = func() { fmt.Fprint(stderr, attachUsage) }
	var f worktreeFlags
	f.register(fset, env)
	var urls, envDirs many
	fset.Var(&urls, "url", "")
	fset.Var(&envDirs, "env-dir", "")
	noEnv := fset.Bool("no-env", false, "")
	noCapture := fset.Bool("no-capture", false, "")
	if err := fset.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fset.NArg() > 0 {
		fmt.Fprint(stderr, attachUsage)
		return 2
	}
	wt, err := f.worktree(env)
	if errors.Is(err, errNoWorktree) {
		return f.soft(stderr, "%v, so nothing was attached.", err)
	}
	if err != nil {
		fmt.Fprintln(stderr, "leadline:", err)
		return 2
	}
	service := wt.Service()

	// The URLs: what the worktree's environment and .env files hold, and
	// what was given. A URL given by flag is added to them.
	var conns []runner.ConnURL
	sawFiles := false
	if !*noEnv {
		mode := env("NODE_ENV")
		dotenvs := []runner.Dotenv{runner.ReadDotenv(wt.Path, mode, env)}
		for _, dir := range envDirs {
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(wt.Path, dir)
			}
			dotenvs = append(dotenvs, runner.ReadDotenv(dir, mode, env))
		}
		for _, d := range dotenvs {
			sawFiles = sawFiles || len(d.Files) > 0
		}
		conns = runner.FindConnURLs(environ, dotenvs...)
	}
	for _, arg := range urls {
		c, err := runner.ParseConnFlag(arg)
		if err != nil {
			fmt.Fprintln(stderr, "leadline:", err)
			return 2
		}
		conns = append(conns, c)
	}
	// With nothing found and no file to have found it in, this may be the
	// wrong folder, or a worktree whose setup has not written its .env yet.
	// The connections it has are then left as they are.
	partial := len(conns) == 0 && !sawFiles && !*noEnv

	daemon := runner.NewDaemon(f.daemon)
	hello, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	up := daemon.Healthy(hello)
	cancel()
	if !up {
		return f.soft(stderr, "not running, so %s was not attached. Start the Leadline app (or run leadlined) and run \"leadline attach\" again.", service)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := daemon.Attach(ctx, wt, conns, !*noCapture, partial)
	if err != nil {
		return f.soft(stderr, "%s was not attached: %v", service, err)
	}
	if !f.quiet {
		fmt.Fprintln(stdout, attachLine(got))
	}
	return 0
}

// attachLine is the one line an attach says. It names addresses and
// variables, never a URL: a database URL has a password in it.
func attachLine(got runner.Attached) string {
	var parts []string
	for _, c := range got.Connections {
		what := "Postgres"
		if c.Kind == "redis" {
			what = "Redis"
		}
		s := what + " " + c.Host
		if c.Database != "" {
			s += "/" + c.Database
		}
		switch {
		case c.Through() != "":
			s += " through " + c.Through()
		case c.Kind == "postgres" && c.Capture:
			s += ", proxy not up"
			if c.Proxy != nil && c.Proxy.Error != "" {
				s += ": " + strings.TrimRight(c.Proxy.Error, ".")
			}
		case c.Capture:
			s += ", commands recorded"
		case c.Remote:
			s += ", remote: statistics only"
		default:
			s += ", statistics only"
		}
		if len(c.Env) > 0 {
			s += " (" + strings.Join(c.Env, ", ") + ")"
		}
		parts = append(parts, s)
	}
	for _, sk := range got.Skipped {
		parts = append(parts, "skipped "+strings.Join(sk.Env, ", ")+": "+strings.TrimRight(sk.Reason, "."))
	}
	what := "no database or cache URL in its environment or .env files"
	if len(parts) > 0 {
		what = strings.Join(parts, "; ")
	}
	if !got.Changed {
		return fmt.Sprintf("leadline: %s is attached already, nothing changed: %s", got.Service, what)
	}
	verb := "attached"
	if !got.Created {
		verb = "updated"
	}
	return fmt.Sprintf("leadline: %s %s: %s", verb, got.Service, what)
}
