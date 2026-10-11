package brief

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/sean-brydon/leadline/internal/store"
)

// QueryEvidence is one query shape's numbers over the window.
type QueryEvidence struct {
	DBSystem string `json:"db_system"`
	// Shape is the statement with its values replaced by "?".
	Shape string `json:"shape"`
	// Example is one real statement, the slowest call's. Only when asked for.
	Example     string  `json:"example,omitempty"`
	Calls       int64   `json:"calls"`
	Errors      int64   `json:"errors"`
	Traces      int64   `json:"traces"`
	MaxPerTrace int64   `json:"max_per_trace"`
	TotalMs     float64 `json:"total_ms"`
	AvgMs       float64 `json:"avg_ms"`
	P50Ms       float64 `json:"p50_ms"`
	P95Ms       float64 `json:"p95_ms"`
	MaxMs       float64 `json:"max_ms"`
	// Share is its part of all database time in the window, 0..1.
	Share float64 `json:"share"`
	// FirstMs and LastMs are when it was first and last run, in everything
	// Leadline still holds, not only the window.
	FirstMs float64 `json:"first_ms"`
	LastMs  float64 `json:"last_ms"`
	// Endpoints are the requests that run it, and how often each does.
	Endpoints []store.QueryEndpointRow `json:"endpoints"`
	// Slowest are the traces that spent longest in it.
	Slowest []store.QueryTraceRow `json:"slowest"`
	// Failures are the messages its failed calls ended with.
	Failures []Failure `json:"failures"`
}

// Failure is a way something failed, and how many times.
type Failure struct {
	// Message is the error's shape, or the error as written when asked for.
	Message string `json:"message"`
	Count   int64  `json:"count,omitempty"`
	// Where it failed: the span's name and service, and its HTTP status.
	Span       string `json:"span,omitempty"`
	Service    string `json:"service,omitempty"`
	HTTPStatus int32  `json:"http_status,omitempty"`
	TraceID    string `json:"trace_id,omitempty"`
}

// FindQuery picks the query a search names: the costliest in the window
// whose statement holds the text.
func (b *Builder) FindQuery(ctx context.Context, o Options, search string) (store.QueryKey, error) {
	w := o.window()
	rows, err := b.store.Queries(ctx, store.Filter{SinceMs: w.SinceMs, Service: w.Service, Search: search, Limit: 1})
	if err != nil {
		return store.QueryKey{}, err
	}
	if len(rows) == 0 {
		return store.QueryKey{}, &ErrNotFound{What: fmt.Sprintf("No query in this window holds %q.", search)}
	}
	return store.QueryKey{DBSystem: rows[0].DBSystem, Fingerprint: rows[0].Fingerprint}, nil
}

// Query writes up one query shape.
func (b *Builder) Query(ctx context.Context, key store.QueryKey, o Options) (*Brief, error) {
	w := o.window()
	det, err := b.store.QueryDetail(ctx, w, key, 1)
	if err != nil {
		return nil, err
	}
	s := det.Stats
	if s == nil {
		return nil, &ErrNotFound{What: "This query didn't run in this window."}
	}
	seen, err := b.store.QuerySeen(ctx, key, w.Service)
	if err != nil {
		return nil, err
	}
	d := &doc{}
	ev := &QueryEvidence{
		DBSystem: key.DBSystem, Shape: key.Fingerprint,
		Calls: s.Calls, Errors: s.Errors, Traces: s.Traces, MaxPerTrace: s.MaxPerTrace,
		TotalMs: s.TotalMs, AvgMs: s.AvgMs, P50Ms: s.P50Ms, P95Ms: s.P95Ms, MaxMs: s.MaxMs, Share: s.Share,
		FirstMs: seen.FirstMs, LastMs: seen.LastMs,
		Endpoints: head(det.Endpoints, maxEvidence), Slowest: head(det.Traces, maxEvidence/2), Failures: []Failure{},
	}
	if s.Errors > 0 {
		msgs, err := b.store.QueryErrors(ctx, w, key, 3)
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			ev.Failures = append(ev.Failures, Failure{Message: d.message(m.Message, maxMessage, "an error message", o), Count: m.Count})
		}
	}

	out := &Brief{Kind: "query", Service: seen.Service, Evidence: Evidence{Query: ev},
		Window: Window{SinceMs: w.SinceMs, UntilMs: w.UntilMs, Service: w.Service}}
	// The request that runs it most is where the work is.
	var top *store.QueryEndpointRow
	if len(det.Endpoints) > 0 {
		top = &det.Endpoints[0]
		for i := range det.Endpoints {
			if det.Endpoints[i].MaxPerRequest > top.MaxPerRequest && s.MaxPerTrace >= NPlusOne {
				top = &det.Endpoints[i]
			}
		}
		if out.Service == "" {
			out.Service = top.Service
		}
	}

	// The files the telemetry names: on the query's own span, then on the
	// request that runs it.
	if attrs, err := b.store.QueryAttrs(ctx, w, key); err == nil && attrs != nil {
		out.Evidence.Code = append(out.Evidence.Code, codeFromAttrs(decode(attrs), "the query's span")...)
	}
	if top != nil {
		if attrs, err := b.store.EndpointAttrs(ctx, w, store.Endpoint{Service: top.Service, Name: top.Name}); err == nil && attrs != nil {
			out.Evidence.Code = append(out.Evidence.Code, codeFromAttrs(decode(attrs), "the request "+code(clipName(top.Name)))...)
		}
	}

	table := tableOf(key.DBSystem, key.Fingerprint)
	about := table
	if about == "" {
		about = key.DBSystem + " query"
	}
	where := ""
	if top != nil {
		where = " in " + clipName(top.Name)
	}
	failRate := float64(s.Errors) / float64(max(s.Calls, 1))

	switch {
	case s.MaxPerTrace >= NPlusOne:
		out.Problem = "n_plus_one"
		out.Title = fmt.Sprintf("Fix the N+1 on %s%s", about, where)
		who := "One request"
		avg := ""
		if top != nil {
			who = code(clipName(top.Name))
			avg = fmt.Sprintf(" (%s times per request on average, over %s)", per(top.PerRequest), plural(top.EndpointRequests, "request", "requests"))
		}
		out.Summary = fmt.Sprintf("%s runs the same %s query up to %s times in a single request%s. One statement repeated inside a request is what Leadline calls an N+1.",
			who, key.DBSystem, count(s.MaxPerTrace), avg)
		out.TaskName = taskName(o, "n1 "+about+" "+endpointWords(top))
	case s.Errors > 0:
		out.Problem = "failing"
		out.Title = fmt.Sprintf("Fix the failing %s query%s", about, where)
		out.Summary = fmt.Sprintf("This %s query failed in %s of its %s (%s).", key.DBSystem, count(s.Errors), plural(s.Calls, "call", "calls"), pct(failRate))
		out.TaskName = taskName(o, "failing "+about+" "+endpointWords(top))
	default:
		out.Problem = "slow"
		out.Title = fmt.Sprintf("Speed up the %s query%s", about, where)
		out.Summary = fmt.Sprintf("This %s query took %s of all database time in the window: %s, p95 %s, max %s.",
			key.DBSystem, pct(s.Share), plural(s.Calls, "call", "calls"), ms(s.P95Ms), ms(s.MaxMs))
		out.TaskName = taskName(o, "slow "+about+" "+endpointWords(top))
	}

	d.say(out.Title)
	d.blank()
	d.intro(w, out.Service, o.now())
	d.blank()
	d.say("## What is wrong")
	d.blank()
	d.say(out.Summary)
	d.blank()
	d.sayf("- Calls: %s in %s, at most %s in one trace", count(s.Calls), plural(s.Traces, "trace", "traces"), count(s.MaxPerTrace))
	d.sayf("- Time per call: average %s, p50 %s, p95 %s, max %s", ms(s.AvgMs), ms(s.P50Ms), ms(s.P95Ms), ms(s.MaxMs))
	d.sayf("- Total: %s, which is %s of all database time in the window", ms(s.TotalMs), pct(s.Share))
	if s.Errors > 0 {
		d.sayf("- Failed calls: %s of %s (%s)", count(s.Errors), count(s.Calls), pct(failRate))
	} else {
		d.say("- Failed calls: none")
	}
	d.sayf("- First seen %s, last seen %s", clock(seen.FirstMs), clock(seen.LastMs))
	d.blank()

	d.evidence(func() {
		d.say("## Evidence")
		d.blank()
		d.say("The query, with its values replaced by `?`:")
		d.blank()
		d.block(fenceLang(key.DBSystem), "shape", false, d.clip(key.Fingerprint, maxStatement, "the query's shape"))
		d.blank()
		if s.Example != "" && s.Example != key.Fingerprint {
			if o.Example {
				ev.Example = s.Example
				d.say("One real statement of that shape, the slowest call's:")
				d.blank()
				d.block(fenceLang(key.DBSystem), "example", true, d.clip(s.Example, maxStatement, "the example statement"))
				d.blank()
			} else {
				d.held = true
			}
		}
		for _, f := range ev.Failures {
			d.failure(f, o)
		}
		if len(ev.Failures) > 0 {
			d.blank()
		}
		if len(det.Endpoints) > 0 {
			d.say("The requests that run it:")
			d.blank()
			for i, e := range det.Endpoints {
				if i == maxRows {
					d.sayf("- and %s more", plural(int64(len(det.Endpoints)-maxRows), "endpoint", "endpoints"))
					break
				}
				d.sayf("- %s (%s): %s per request on average, at most %s, in %s of its %s", code(clipName(e.Name)), e.Service,
					per(e.PerRequest), count(e.MaxPerRequest), count(e.Requests), plural(e.EndpointRequests, "request", "requests"))
			}
			d.blank()
		} else {
			d.say("No request that began in the window ran it: it runs outside any request Leadline saw (a job, a script, a console).")
			d.blank()
		}
		if len(det.Traces) > 0 {
			d.say("The traces that spent longest in it (`get_trace` reads one):")
			d.blank()
			for i, t := range det.Traces {
				if i == maxRows-1 {
					break
				}
				name := ""
				if t.Name != "" {
					name = " " + clipName(t.Name)
				}
				d.sayf("- `%s`%s: %s, %s in this query", t.TraceID, name, plural(t.Calls, "call", "calls"), ms(t.TotalMs))
			}
			d.blank()
		}
		d.codeLines(out.Evidence.Code, "The telemetry names no source file for this query or the request that runs it.")
	})

	d.blank()
	d.say("## Done when")
	d.blank()
	route := "the request that runs it"
	if top != nil {
		route = code(clipName(top.Name))
	}
	switch out.Problem {
	case "n_plus_one":
		d.sayf("- %s runs this query once per request, or a fixed number of times that does not grow with the rows it returns, instead of up to %s.", route, count(s.MaxPerTrace))
		d.say("- The request still answers with what it answered before.")
	case "failing":
		d.sayf("- Running %s again, no call of this query fails: before, %s of %s did.", route, count(s.Errors), count(s.Calls))
		d.say("- The statement still does what it was written to do; an error the code means to raise is not a failure to remove.")
	default:
		d.sayf("- Over several runs of %s, this query's p95 is clearly below %s and its max below %s. Say the numbers you measured.", route, ms(s.P95Ms), ms(s.MaxMs))
		d.say("- It returns the same rows as before.")
	}
	d.blank()

	d.checkIntro(out.Service)
	search := searchFor(key.DBSystem, key.Fingerprint)
	step := 1
	next := func(format string, args ...any) {
		d.sayf("%d. %s", step, fmt.Sprintf(format, args...))
		step++
	}
	if top != nil {
		next("Run the app from your worktree and make the request %s, a few times.", code(clipName(top.Name)))
		if out.Problem == "n_plus_one" {
			next("`list_traces` with `search: %s` and `minutes: 5`, then `get_trace` on the newest: count how often the statement above runs in it. Before, up to %s times.", quoted(clipName(top.Name)), count(s.MaxPerTrace))
		} else {
			next("`list_traces` with `search: %s` and `minutes: 5`, then `get_trace` on the newest: it shows this statement and how long it took in your request.", quoted(clipName(top.Name)))
		}
	} else {
		next("Run the code that sends this query from your worktree.")
	}
	switch out.Problem {
	case "n_plus_one":
		next("`list_queries` with `search: %s` and `minutes: 5`: this query's `max_per_trace` was %s and should now be 1 or a small fixed number, with no `n_plus_one`.", quoted(search), count(s.MaxPerTrace))
	case "failing":
		next("`list_queries` with `search: %s` and `minutes: 5`: this query's `errors` should be absent. Before, %s of %s calls failed.", quoted(search), count(s.Errors), count(s.Calls))
	default:
		next("`list_queries` with `search: %s` and `minutes: 5`: compare this query's `p95_ms` and `max_ms` with %s and %s. Its `example` is a real statement you can run under `EXPLAIN`.", quoted(search), ms(s.P95Ms), ms(s.MaxMs))
	}
	d.blank()
	d.say("Report the numbers before and after, and the id of the trace you compared.")

	switch out.Problem {
	case "n_plus_one":
		d.never("Do not hide the repetition: no caching of the response to mask it, no switching instrumentation off. Fewer queries has to come from asking the database for the rows in one go.")
	case "failing":
		d.never("Do not silence the error: no catching it and carrying on, no downgrading or deleting the log that reports it.")
	default:
		d.never("Do not hide the cost behind a cache or a lower limit unless that is the right fix and you say so.")
	}
	d.seal(out, o)
	return out, nil
}

// failure writes one way something failed. The message is recorded text:
// its shape by default, as written when asked.
func (d *doc) failure(f Failure, o Options) {
	where := ""
	if f.Span != "" {
		where = "span " + code(clipName(f.Span))
		if f.Service != "" {
			where += " in " + f.Service
		}
	}
	status := ""
	if f.HTTPStatus > 0 {
		status = fmt.Sprintf(" (HTTP %d)", f.HTTPStatus)
	}
	times := ""
	if f.Count > 1 {
		times = fmt.Sprintf(", %s times", count(f.Count))
	}
	msg := ""
	if f.Message != "" {
		msg = ": " + code(f.Message)
	}
	if where == "" {
		where = "Failed"
	}
	line := "- " + where + status + times + msg
	if f.Message == "" {
		d.say(line)
		return
	}
	d.rec("error", o.Example, line)
}

// message is a recorded error text as a brief may show it: its shape, or
// the text itself when real values were asked for. It notes when something
// real was held back.
func (d *doc) message(text string, limit int, what string, o Options) string {
	text = oneLine(text)
	if text == "" {
		return ""
	}
	if o.Example {
		return d.clip(text, limit, what)
	}
	shape := Shape(text)
	if shape != text {
		d.held = true
	}
	return d.clip(shape, limit, what)
}

func endpointWords(e *store.QueryEndpointRow) string {
	if e == nil {
		return ""
	}
	return routeWords(e.Name)
}

// routeWords is an endpoint's name as words for a task name: the route
// without its method and without "api".
func routeWords(name string) string {
	fields := strings.Fields(name)
	if len(fields) > 1 {
		name = strings.Join(fields[1:], " ")
	}
	name = strings.TrimPrefix(strings.TrimPrefix(name, "/"), "api/")
	return name
}

// fenceLang is the language a statement's code block is marked with.
func fenceLang(system string) string {
	switch system {
	case "postgresql", "mysql", "sqlite", "mssql", "oracle", "mariadb", "cockroachdb":
		return "sql"
	}
	return ""
}

// quoted writes a search term as the JSON string an MCP argument takes.
func quoted(s string) string { return strconv.Quote(s) }

func decode(attrs json.RawMessage) map[string]any {
	out := map[string]any{}
	_ = json.Unmarshal(attrs, &out)
	return out
}
