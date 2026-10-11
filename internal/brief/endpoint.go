package brief

import (
	"context"
	"fmt"

	"github.com/sean-brydon/leadline/internal/store"
)

// EndpointEvidence is one endpoint's numbers over the window.
type EndpointEvidence struct {
	Service  string `json:"service"`
	Name     string `json:"name"`
	Requests int64  `json:"requests"`
	// Errors is the requests in which any span failed.
	Errors  int64   `json:"errors"`
	TotalMs float64 `json:"total_ms"`
	AvgMs   float64 `json:"avg_ms"`
	P50Ms   float64 `json:"p50_ms"`
	P95Ms   float64 `json:"p95_ms"`
	MaxMs   float64 `json:"max_ms"`
	// QueriesPerRequest and DBMs are averages over its requests.
	QueriesPerRequest float64 `json:"queries_per_request"`
	DBMs              float64 `json:"db_ms"`
	// Share is its part of all request time in the window, 0..1.
	Share   float64 `json:"share"`
	FirstMs float64 `json:"first_ms"`
	LastMs  float64 `json:"last_ms"`
	// Queries are the shapes its requests run, costliest first.
	Queries []EndpointQuery    `json:"queries"`
	Slowest []store.RequestRow `json:"slowest"`
	Failing []store.RequestRow `json:"failing"`
	// Failures, Logs and Frames are read from its newest failing requests.
	Failures []Failure `json:"failures"`
	Logs     []LogLine `json:"logs"`
	Frames   []Frame   `json:"frames"`
}

// EndpointQuery is one query shape as an endpoint runs it.
type EndpointQuery struct {
	DBSystem      string  `json:"db_system"`
	Shape         string  `json:"shape"`
	Calls         int64   `json:"calls"`
	Requests      int64   `json:"requests"`
	PerRequest    float64 `json:"per_request"`
	MaxPerRequest int64   `json:"max_per_request"`
	AvgMs         float64 `json:"avg_ms"`
	TotalMs       float64 `json:"total_ms"`
}

// FindEndpoint picks the endpoint a search names: one called exactly that,
// else the one with the most request time whose name holds the text.
func (b *Builder) FindEndpoint(ctx context.Context, o Options, search, service string) (store.Endpoint, error) {
	list, err := b.store.Endpoints(ctx, o.window(), store.EndpointQuery{Search: search, Limit: 200})
	if err != nil {
		return store.Endpoint{}, err
	}
	var first *store.EndpointRow
	for i := range list.Rows {
		r := &list.Rows[i]
		if service != "" && r.Service != service {
			continue
		}
		if r.Name == search {
			return store.Endpoint{Service: r.Service, Name: r.Name}, nil
		}
		if first == nil {
			first = r
		}
	}
	if first == nil {
		return store.Endpoint{}, &ErrNotFound{What: fmt.Sprintf("No endpoint in this window is named like %q.", search)}
	}
	return store.Endpoint{Service: first.Service, Name: first.Name}, nil
}

// How many of an endpoint's failing requests are opened to see what failed.
const failingTracesRead = 2

// Endpoint writes up one endpoint.
func (b *Builder) Endpoint(ctx context.Context, ep store.Endpoint, o Options) (*Brief, error) {
	w := o.window()
	det, err := b.store.EndpointDetail(ctx, w, ep, 1)
	if err != nil {
		return nil, err
	}
	s := det.Stats
	if s == nil {
		return nil, &ErrNotFound{What: "This endpoint had no requests in this window."}
	}
	seen, err := b.store.EndpointSeen(ctx, ep)
	if err != nil {
		return nil, err
	}
	d := &doc{}
	name := clipName(ep.Name)
	ev := &EndpointEvidence{Service: ep.Service, Name: ep.Name, Requests: s.Requests, Errors: s.Errors,
		TotalMs: s.TotalMs, AvgMs: s.AvgMs, P50Ms: s.P50Ms, P95Ms: s.P95Ms, MaxMs: s.MaxMs,
		QueriesPerRequest: s.Queries, DBMs: s.DBMs, Share: s.Share, FirstMs: seen.FirstMs, LastMs: seen.LastMs,
		Queries: []EndpointQuery{}, Slowest: head(det.Slowest, maxEvidence/2), Failing: head(det.Failing, maxEvidence/2),
		Failures: []Failure{}, Logs: []LogLine{}, Frames: []Frame{}}
	// The query a request repeats most, if any repeats enough to flag.
	var repeated *EndpointQuery
	worst := -1
	real := false
	for _, q := range head(det.Queries, maxEvidence) {
		ev.Queries = append(ev.Queries, EndpointQuery{DBSystem: q.DBSystem, Shape: q.Fingerprint, Calls: q.Calls, Requests: q.Requests,
			PerRequest: q.PerRequest, MaxPerRequest: q.MaxPerRequest, AvgMs: q.AvgMs, TotalMs: q.TotalMs})
		if q.MaxPerRequest >= NPlusOne && (worst < 0 || q.MaxPerRequest > ev.Queries[worst].MaxPerRequest) {
			worst = len(ev.Queries) - 1
		}
		real = real || (q.Example != "" && q.Example != q.Fingerprint)
	}
	if worst >= 0 {
		repeated = &ev.Queries[worst]
	}

	out := &Brief{Kind: "endpoint", Service: ep.Service, Evidence: Evidence{Endpoint: ev},
		Window: Window{SinceMs: w.SinceMs, UntilMs: w.UntilMs, Service: w.Service}}
	if attrs, err := b.store.EndpointAttrs(ctx, w, ep); err == nil && attrs != nil {
		out.Evidence.Code = codeFromAttrs(decode(attrs), "the request's span")
	}

	// What failed, read from the newest failing requests.
	var facts traceFacts
	read := 0
	for i, r := range det.Failing {
		if i == failingTracesRead {
			break
		}
		spans, logs, err := b.store.Trace(ctx, r.TraceID)
		if err != nil || len(spans) == 0 {
			continue
		}
		shapes, _ := b.store.TraceShapes(ctx, r.TraceID)
		f := readTrace(d, o, r.TraceID, spans, logs, shapes)
		read++
		facts.failures = mergeFailures(facts.failures, f.failures)
		facts.logs = append(facts.logs, f.logs...)
		if len(facts.frames) == 0 {
			facts.frames, facts.head, facts.exceptionType = f.frames, f.head, f.exceptionType
		}
	}
	ev.Failures, ev.Logs, ev.Frames = orEmpty(facts.failures), orEmpty(facts.logs), orEmpty(facts.frames)
	out.Evidence.Code = append(codeFromFrames(facts.frames), out.Evidence.Code...)

	failRate := float64(s.Errors) / float64(max(s.Requests, 1))
	switch {
	case s.Errors > 0:
		out.Problem = "failing"
		out.Title = "Fix the failing requests to " + name
		out.Summary = fmt.Sprintf("%s failed in %s of its %s (%s).", code(name), count(s.Errors), plural(s.Requests, "request", "requests"), pct(failRate))
		out.TaskName = taskName(o, "failing "+routeWords(ep.Name))
	case repeated != nil:
		out.Problem = "n_plus_one"
		out.Title = fmt.Sprintf("Fix the N+1 in %s", name)
		out.Summary = fmt.Sprintf("%s runs one %s query up to %s times in a single request (%s times per request on average, over %s). One statement repeated inside a request is what Leadline calls an N+1.",
			code(name), repeated.DBSystem, count(repeated.MaxPerRequest), per(repeated.PerRequest), plural(s.Requests, "request", "requests"))
		out.TaskName = taskName(o, "n1 "+routeWords(ep.Name))
	default:
		out.Problem = "slow"
		out.Title = "Speed up " + name
		out.Summary = fmt.Sprintf("%s took %s of all request time in the window: %s, p95 %s, max %s, with %s per request in databases.",
			code(name), pct(s.Share), plural(s.Requests, "request", "requests"), ms(s.P95Ms), ms(s.MaxMs), ms(s.DBMs))
		out.TaskName = taskName(o, "slow "+routeWords(ep.Name))
	}

	d.say(out.Title)
	d.blank()
	d.intro(w, ep.Service, o.now())
	d.blank()
	d.say("## What is wrong")
	d.blank()
	d.say(out.Summary)
	d.blank()
	if s.Errors > 0 {
		d.sayf("- Requests: %s, of which %s failed (%s)", count(s.Requests), count(s.Errors), pct(failRate))
	} else {
		d.sayf("- Requests: %s, none failed", count(s.Requests))
	}
	d.sayf("- Latency: average %s, p50 %s, p95 %s, max %s", ms(s.AvgMs), ms(s.P50Ms), ms(s.P95Ms), ms(s.MaxMs))
	d.sayf("- Per request: %s queries and %s in databases, on average", per(s.Queries), ms(s.DBMs))
	d.sayf("- Total: %s, which is %s of all request time in the window", ms(s.TotalMs), pct(s.Share))
	d.sayf("- First seen %s, last seen %s", clock(seen.FirstMs), clock(seen.LastMs))
	if out.Problem == "failing" {
		d.blank()
		d.say(failedMeans)
		if repeated != nil {
			d.sayf("It also runs one query up to %s times in a request, an N+1: the query list below has it.", count(repeated.MaxPerRequest))
		}
	}
	d.blank()

	d.evidence(func() {
		d.say("## Evidence")
		d.blank()
		if out.Problem == "failing" {
			if len(det.Failing) > 0 {
				d.say("Its newest failing requests (`get_trace` reads one):")
				d.blank()
				for i, r := range det.Failing {
					if i == maxRows-1 {
						break
					}
					d.sayf("- `%s` at %s: %s, %s", r.TraceID, clock(r.StartMs), ms(r.DurationMs), plural(r.Queries, "query", "queries"))
				}
				d.blank()
			}
			d.tracesEvidence(traceFacts{failures: facts.failures, logs: facts.logs, head: facts.head, frames: facts.frames, exceptionType: facts.exceptionType}, o, read)
		}
		if len(ev.Queries) > 0 {
			d.say("The queries a request runs, costliest first (values replaced by `?`):")
			d.blank()
			for i, q := range ev.Queries {
				if i == maxRows {
					d.sayf("- and %s more", plural(int64(len(ev.Queries)-maxRows), "query", "queries"))
					break
				}
				flag := ""
				if q.MaxPerRequest >= NPlusOne {
					flag = fmt.Sprintf(", at most %s in one request: an N+1", count(q.MaxPerRequest))
				}
				d.rec("shape", false, fmt.Sprintf("- %s per request%s, %s each on average, %s: %s", per(q.PerRequest), flag, ms(q.AvgMs), q.DBSystem,
					code(d.clip(oneLine(q.Shape), maxStatement/2, "a query's shape"))))
			}
			d.blank()
			if real && !o.Example {
				d.held = true
			}
		} else {
			d.say("Its requests ran no database call in the window.")
			d.blank()
		}
		if out.Problem != "failing" && len(det.Slowest) > 0 {
			d.say("Its slowest requests (`get_trace` reads one):")
			d.blank()
			for i, r := range det.Slowest {
				if i == maxRows-1 {
					break
				}
				d.sayf("- `%s`: %s, %s, %s in databases", r.TraceID, ms(r.DurationMs), plural(r.Queries, "query", "queries"), ms(r.DBMs))
			}
			d.blank()
		}
		d.codeLines(out.Evidence.Code, "The telemetry names no source file for this endpoint.")
	})

	d.blank()
	d.say("## Done when")
	d.blank()
	switch out.Problem {
	case "failing":
		d.say("- A request that failed before, made again the same way, ends with no span in error. Or: you have found where the code decides to refuse it, shown that the refusal is intended, and changed nothing.")
		d.say("- Requests that succeeded before still do.")
	case "n_plus_one":
		d.sayf("- %s runs that query once per request, or a fixed number of times that does not grow with the rows it returns, instead of up to %s; %s queries per request on average becomes a small fixed number.", code(name), count(repeated.MaxPerRequest), per(s.Queries))
		d.say("- It still answers with what it answered before.")
	default:
		d.sayf("- Over several requests, %s's p95 is clearly below %s. Say the numbers you measured.", code(name), ms(s.P95Ms))
		d.say("- It still answers with what it answered before.")
	}
	d.blank()

	d.checkIntro(ep.Service)
	d.sayf("1. Run the app from your worktree and make the request %s a few times%s.", code(name), map[bool]string{true: ", the way the failing ones were made", false: ""}[out.Problem == "failing"])
	switch out.Problem {
	case "failing":
		d.sayf("2. `list_traces` with `search: %s`, `errors_only: true` and `minutes: 5`: none of your requests should be listed. Before, %s of %s failed.", quoted(name), count(s.Errors), count(s.Requests))
		d.say("3. `get_trace` on your newest request, and `search_logs` with `level: \"error\"` and `minutes: 5`: no span in error, and none of the errors above.")
	case "n_plus_one":
		d.sayf("2. `list_traces` with `search: %s` and `minutes: 5`, then `get_trace` on the newest: count how often the repeated statement runs. Before, at most %s times.", quoted(name), count(repeated.MaxPerRequest))
		d.sayf("3. `list_queries` with `search: %s` and `minutes: 5`: its `max_per_trace` was %s and should be 1 or a small fixed number, with no `n_plus_one`.", quoted(searchFor(repeated.DBSystem, repeated.Shape)), count(repeated.MaxPerRequest))
	default:
		d.sayf("2. `list_traces` with `search: %s` and `minutes: 5`: compare `duration_ms`, `queries` and `database_ms` with p95 %s, %s queries and %s above.", quoted(name), ms(s.P95Ms), per(s.Queries), ms(s.DBMs))
		d.say("3. `get_trace` on one of them shows where its time goes now.")
	}
	d.blank()
	d.say("Report the numbers before and after, and the id of the trace you compared.")

	switch out.Problem {
	case "failing":
		d.never("Do not silence the failure: no catching the error and carrying on, no answering 200 for a request that did not succeed, no removing the log that reports it.")
	case "n_plus_one":
		d.never("Do not hide the repetition behind a cache or by switching instrumentation off. Fewer queries has to come from asking the database for the rows in one go.")
	default:
		d.never("Do not hide the cost behind a cache or a smaller answer unless that is the right fix and you say so.")
	}
	d.seal(out, o)
	return out, nil
}

// mergeFailures adds one request's failures to another's, counting the
// ones that are the same.
func mergeFailures(all, more []Failure) []Failure {
	for _, m := range more {
		found := false
		for i := range all {
			if all[i].Span == m.Span && all[i].Service == m.Service && all[i].Message == m.Message && all[i].HTTPStatus == m.HTTPStatus {
				all[i].Count += m.Count
				found = true
				break
			}
		}
		if !found {
			all = append(all, m)
		}
	}
	return all
}
