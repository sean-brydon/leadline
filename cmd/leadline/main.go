// leadline is the command line for a running leadlined.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/sean-brydon/leadline/internal/demo"
	"github.com/sean-brydon/leadline/skills"
)

const usage = `usage: leadline <command> [flags]

Leadline shows what your code did while you developed it: the queries,
traces and logs of the apps on this machine. These commands talk to its
daemon, which the Leadline app starts (or run leadlined yourself).

  run     start your dev command and see it in Leadline, with nothing changed
          in the project: its output becomes logs, it is set up to send
          traces, and its Postgres queries are recorded
              leadline run -- pnpm dev
  attach  make a worktree a source of its own, with its databases: for the
          hook that runs when Shipyard creates one
  detach  take a worktree out again, with what was recorded for it: for the
          hook that runs when Shipyard removes one
  shipyard hooks  print those hooks, or add them to a config file
  doctor  check that Leadline is running and that this project is wired to it
  prompt  print the one line that has a coding agent set Leadline up here
  brief   print a problem Leadline found (a query, an endpoint, a trace, a
          log line) as a prompt for a coding agent, with how to check a fix
              leadline brief query order_items | claude
  skills  install Leadline's agent skills into this project (.claude/skills)
  demo    send made-up traces, logs and metrics, to try the app
  bench   for working on Leadline itself: fill a database file with millions
          of rows and time every query on it
  service the daemon as a background service on a Linux machine: status,
          start, stop, restart, logs (scripts/install.sh sets it up)
  version print this version, and the running daemon's

"leadline <command> -h" lists a command's flags. The daemon's address is
http://127.0.0.1:4320 unless LEADLINE_URL or --url says otherwise.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		os.Exit(runCommand(os.Args[2:]))
	case "attach":
		os.Exit(attachCommand(os.Args[2:], os.Environ(), os.Stdout, os.Stderr))
	case "detach":
		os.Exit(detachCommand(os.Args[2:], os.Environ(), os.Stdout, os.Stderr))
	case "shipyard":
		os.Exit(shipyardCommand(os.Args[2:], os.Stdout, os.Stderr))
	case "doctor":
		err = runDoctor(os.Args[2:])
	case "prompt":
		err = runPrompt(os.Args[2:])
	case "brief":
		os.Exit(runBrief(os.Args[2:], os.Stdout, os.Stderr))
	case "demo":
		err = runDemo(os.Args[2:])
	case "skills":
		err = runSkills(os.Args[2:])
	case "bench":
		err = runBench(os.Args[2:])
	case "service":
		os.Exit(runService(os.Args[2:]))
	case "version", "--version", "-version":
		err = runVersion(os.Args[2:])
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "leadline:", err)
		os.Exit(1)
	}
}

func runDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	url := fs.String("url", envOr("LEADLINE_URL", "http://127.0.0.1:4320"), "the daemon's address")
	minutes := fs.Int("minutes", 15, "spread the traces over this many minutes up to now")
	traces := fs.Int("traces", 600, "how many traces to send")
	follow := fs.Bool("follow", false, "then keep sending a few traces a second until stopped")
	seed := fs.Uint64("seed", uint64(time.Now().UnixNano()), "seed for the made-up data")
	fs.Parse(args)

	gen := demo.New(*seed)
	now := time.Now()
	if err := send(*url, gen, *traces, now.Add(-time.Duration(*minutes)*time.Minute), now); err != nil {
		return err
	}
	fmt.Printf("sent %d traces to %s\n", *traces, *url)
	for *follow {
		time.Sleep(time.Second)
		next := time.Now()
		if err := send(*url, gen, 4, now, next); err != nil {
			return err
		}
		now = next
	}
	return nil
}

// runSkills writes the skills this binary carries into a project. They are
// Leadline's own files, so a newer binary replaces them.
func runSkills(args []string) error {
	fset := flag.NewFlagSet("skills", flag.ExitOnError)
	dir := fset.String("dir", filepath.Join(".claude", "skills"), "where the agent reads skills from")
	url := fset.String("url", envOr("LEADLINE_URL", "http://127.0.0.1:4320"), "the daemon's address")
	fset.Parse(args)

	err := fs.WalkDir(skills.FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := skills.FS.ReadFile(path)
		if err != nil {
			return err
		}
		dest := filepath.Join(*dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		fmt.Println("wrote", dest)
		return os.WriteFile(dest, body, 0o644)
	})
	if err != nil {
		return err
	}
	fmt.Printf("\nAdd the MCP server too. For Claude Code:\n  claude mcp add --transport http leadline %s/mcp\n", *url)
	return nil
}

func send(url string, gen *demo.Generator, n int, from, to time.Time) error {
	traces, logs := gen.Batch(n, from, to)
	if err := post(url+"/v1/traces", traces); err != nil {
		return err
	}
	if err := post(url+"/v1/logs", logs); err != nil {
		return err
	}
	// Metrics are exported every ten seconds, so a short stretch has none.
	if metrics := gen.Metrics(from, to); len(metrics.ResourceMetrics) > 0 {
		return post(url+"/v1/metrics", metrics)
	}
	return nil
}

func post(url string, msg proto.Message) error {
	body, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	resp, err := http.Post(url, "application/x-protobuf", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("is leadlined running? %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		text, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return fmt.Errorf("%s: %s: %s", url, resp.Status, bytes.TrimSpace(text))
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
