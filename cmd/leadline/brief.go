package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sean-brydon/leadline/internal/runner"
)

const briefUsage = `usage: leadline brief <query|endpoint|trace|log> [what] [flags]

Prints a problem Leadline found as a prompt for a coding agent: what is
wrong with its numbers, the evidence, what done looks like, and how to check
a fix with Leadline's MCP tools. Pipe it to any agent:

  leadline brief query order_items | claude
  leadline brief endpoint "POST /api/checkout"
  leadline brief trace 4bf92f3577b34da6a3ce929d0e0e4736
  leadline brief log "checkout failed"

  query <text>     the costliest query whose statement holds the text
  endpoint <name>  the endpoint of that name, or the busiest whose name holds it
  trace <id>       one trace
  log [text]       the newest error (or warning) whose message holds the text,
                   and the records that say the same

Statements and log lines are reduced to their shapes, with ? where a value
was. --example puts one real statement, and the log line as written, in.

`

// runBrief prints a brief the daemon wrote. The daemon writes it, so the
// app, this command and Shipyard are handed the same text.
func runBrief(args []string, out, errs io.Writer) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprint(errs, briefUsage)
		return 2
	}
	kind := args[0]
	fset := flag.NewFlagSet("brief", flag.ContinueOnError)
	fset.SetOutput(errs)
	fset.Usage = func() { fmt.Fprint(errs, briefUsage); fset.PrintDefaults() }
	base := fset.String("url", envOr("LEADLINE_URL", runner.DefaultURL), "the daemon's address")
	minutes := fset.Float64("minutes", 15, "how far back to look")
	service := fset.String("service", "", "only this service")
	example := fset.Bool("example", false, "include real values: one statement, the log line as written")
	asJSON := fset.Bool("json", false, "print the whole brief as JSON: the prompt, which of its lines are recorded data, and the numbers behind it")
	// Flags may come before or after what is asked for.
	var what []string
	rest := args[1:]
	for len(rest) > 0 {
		if err := fset.Parse(rest); err != nil {
			return 2
		}
		if rest = fset.Args(); len(rest) > 0 {
			what, rest = append(what, rest[0]), rest[1:]
		}
	}
	text := strings.Join(what, " ")

	q := url.Values{}
	q.Set("since_ms", strconv.FormatInt(time.Now().Add(-time.Duration(*minutes*float64(time.Minute))).UnixMilli(), 10))
	if *example {
		q.Set("example", "1")
	}
	switch kind {
	case "query":
		q.Set("q", text)
		q.Set("service", *service)
	case "endpoint":
		q.Set("q", text)
		q.Set("endpoint_service", *service)
	case "trace":
		q.Set("id", text)
	case "log":
		q.Set("q", text)
		q.Set("log_service", *service)
	default:
		fmt.Fprint(errs, briefUsage)
		return 2
	}
	if text == "" && kind != "log" {
		fmt.Fprintf(errs, "leadline: brief %s needs to be told which one\n\n", kind)
		fmt.Fprint(errs, briefUsage)
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(*base, "/")+"/api/brief/"+kind+"?"+q.Encode(), nil)
	if err != nil {
		fmt.Fprintln(errs, "leadline:", err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(errs, "leadline: nothing answers at %s. Start Leadline first.\n", *base)
		return 1
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct{ Message string } `json:"error"`
		}
		switch {
		case json.Unmarshal(body, &e) == nil && e.Error.Message != "":
			fmt.Fprintln(errs, "leadline:", e.Error.Message)
		case resp.StatusCode == http.StatusNotFound:
			fmt.Fprintf(errs, "leadline: the Leadline at %s is too old to write briefs. Update it first.\n", *base)
		default:
			fmt.Fprintf(errs, "leadline: %s: %s\n", resp.Status, strings.TrimSpace(string(body)))
		}
		return 1
	}
	if *asJSON {
		out.Write(body)
		return 0
	}
	var b struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(body, &b); err != nil || b.Prompt == "" {
		fmt.Fprintln(errs, "leadline: the daemon answered with something that is not a brief")
		return 1
	}
	fmt.Fprint(out, b.Prompt)
	return 0
}
