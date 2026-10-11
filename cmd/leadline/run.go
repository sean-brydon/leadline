package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/sean-brydon/leadline/internal/runner"
)

const runUsage = `usage: leadline run [flags] -- <command> [arguments]

Runs a dev command as if you had run it yourself, and shows it in Leadline:
every line it prints becomes a log, and it is set up to send its traces.

  leadline run -- yarn dev
  leadline run -- python manage.py runserver
  leadline run --name api -- go run ./cmd/api

  --name <service>  the name to show it under (default: OTEL_SERVICE_NAME,
                    else the project's name, else the folder's)
  --url <address>   the daemon's address (default: LEADLINE_URL, else
                    http://127.0.0.1:4320)
  --no-proxy        leave database URLs alone; without this, a Postgres on
                    this machine is reached through a proxy that records
                    its queries (a query the app's own telemetry reports
                    too is still counted once)
  --no-inject       set nothing in the command's environment
  --quiet           do not print the line that says what was set up
`

// runCommand is `leadline run`. It returns the exit code to leave with,
// which is the command's own.
func runCommand(args []string) int {
	fset := flag.NewFlagSet("run", flag.ContinueOnError)
	fset.Usage = func() { fmt.Fprint(os.Stderr, runUsage) }
	opts := runner.Options{}
	fset.StringVar(&opts.Name, "name", "", "")
	fset.StringVar(&opts.URL, "url", envOr("LEADLINE_URL", runner.DefaultURL), "")
	fset.BoolVar(&opts.NoProxy, "no-proxy", false, "")
	// --proxy once asked for the proxy beside an app's own instrumentation.
	// That is what happens now; the flag is taken so old scripts still run.
	fset.Bool("proxy", false, "")
	fset.BoolVar(&opts.NoInject, "no-inject", false, "")
	fset.BoolVar(&opts.Quiet, "quiet", false, "")
	fset.BoolVar(&opts.Quiet, "q", false, "")
	if err := fset.Parse(args); err != nil {
		// 2 is "you called it wrongly", here and in a shell.
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	opts.Command = fset.Args()
	if len(opts.Command) == 0 {
		fmt.Fprint(os.Stderr, runUsage)
		return 2
	}
	return runner.Run(opts)
}
