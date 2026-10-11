package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sean-brydon/leadline/internal/runner"
)

const detachUsage = `usage: leadline detach [flags]

Takes a worktree out of Leadline when the worktree is gone: the connections
"leadline attach" saved for it and their proxies are stopped and removed,
the source leaves the app, and what was recorded under its name is deleted.
Only what the worktree owns is touched: a connection you added yourself, or
another worktree's, stays as it is even when it is to the same database
server. Made for the hook that runs when a tool such as Shipyard removes a
worktree; the folder need not exist any more.

  leadline detach                          in a Shipyard hook (worktree.removed)
  leadline detach --project acme --name fix-orders
  leadline detach --service acme/fix-orders --keep-data

Running it for a worktree Leadline never saw, or twice, does nothing.

  --name <name>      the worktree's name (default: the hook's BERTH_NAME, else
                     BERTH_WORKTREE_NAME, else git's for --path or this folder)
  --project <name>   its project (default: BERTH_LOCATION)
  --path <dir>       its folder, to work the names out from when no variable
                     and no flag gives them
  --service <name>   the source's name as Leadline shows it, instead of the
                     three above
  --keep-data        leave what was recorded: the spans, logs and metrics stay
                     until they age out
  --daemon <address> the daemon's address (default: LEADLINE_URL, else
                     http://127.0.0.1:4320)
  --strict           exit 1 when it could not be done. Without it a hook is
                     never failed: with Leadline not running it says so in one
                     line and exits 0
  --quiet            say nothing when it worked
`

// detachCommand is `leadline detach`, in the environment given. It returns
// the exit code.
func detachCommand(args, environ []string, stdout, stderr io.Writer) int {
	env := lookup(environ)
	fset := flag.NewFlagSet("detach", flag.ContinueOnError)
	fset.SetOutput(stderr)
	fset.Usage = func() { fmt.Fprint(stderr, detachUsage) }
	var f worktreeFlags
	f.register(fset, env)
	service := fset.String("service", "", "")
	keepData := fset.Bool("keep-data", false, "")
	if err := fset.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fset.NArg() > 0 {
		fmt.Fprint(stderr, detachUsage)
		return 2
	}
	name := strings.TrimSpace(*service)
	if name == "" {
		wt, err := f.worktree(env)
		if errors.Is(err, errNoWorktree) {
			return f.soft(stderr, "%v, so nothing was detached.", err)
		}
		if err != nil {
			fmt.Fprintln(stderr, "leadline:", err)
			return 2
		}
		name = wt.Service()
	}

	daemon := runner.NewDaemon(f.daemon)
	hello, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	up := daemon.Healthy(hello)
	cancel()
	if !up {
		return f.soft(stderr, "not running, so %s was not detached. Run \"leadline detach --service %s\" once it is.", name, name)
	}
	// Deleting from a large store takes a moment; a hook has a minute.
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	got, err := daemon.Detach(ctx, name, *keepData)
	if err != nil {
		return f.soft(stderr, "%s was not detached: %v", name, err)
	}
	if !f.quiet {
		fmt.Fprintln(stdout, detachLine(got))
	}
	return 0
}

func plural(n int64, one string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", one)
	}
	return fmt.Sprintf("%d %ss", n, one)
}

// detachLine is the one line a detach says.
func detachLine(got runner.Detached) string {
	deleted := got.Deleted.Spans + got.Deleted.Logs + got.Deleted.MetricPoints
	if !got.Found && got.Connections == 0 && deleted == 0 {
		return fmt.Sprintf("leadline: %s is not attached and has no data, nothing to do", got.Service)
	}
	var parts []string
	if got.Connections > 0 || got.Proxies > 0 {
		parts = append(parts, fmt.Sprintf("removed %s and closed %s",
			plural(int64(got.Connections), "connection"), strings.Replace(plural(int64(got.Proxies), "proxy"), "proxys", "proxies", 1)))
	}
	switch {
	case got.KeptData:
		parts = append(parts, "kept its data")
	case deleted == 0:
		parts = append(parts, "it had no data")
	default:
		parts = append(parts, fmt.Sprintf("deleted %s, %s and %s",
			plural(got.Deleted.Spans, "span"), plural(got.Deleted.Logs, "log"), plural(got.Deleted.MetricPoints, "metric point")))
	}
	return fmt.Sprintf("leadline: detached %s: %s", got.Service, strings.Join(parts, ", "))
}
