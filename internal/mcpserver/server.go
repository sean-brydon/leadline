// Package mcpserver lets a coding agent read what Leadline captured, over
// the Model Context Protocol. Every tool only reads. Results are kept
// small by default: an agent's context is the scarce thing here.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sean-brydon/leadline/internal/integrations"
	"github.com/sean-brydon/leadline/internal/store"
)

// NPlusOne is how many times one trace must run the same query before it
// is called an N+1.
const NPlusOne = 5

const instructions = `Leadline holds the traces, database queries and logs that apps on this machine sent while running in development. Use it to see what code really did: which queries a request ran and how long they took, where a request failed, what was logged.

Start with "overview". After changing code, exercise it (run the request or the test), then look again: the newest data is what your change did. Times are local to this machine's clock, in ISO 8601. Durations are milliseconds.`

type tools struct {
	store *store.Store
	integ *integrations.Manager
}

// Handler serves the MCP endpoint. It is stateless: every tool is a plain
// read, and a client's connection then survives the daemon restarting.
// integ is what list_sources reads.
func Handler(st *store.Store, integ *integrations.Manager) http.Handler {
	t := &tools{store: st, integ: integ}
	server := mcp.NewServer(&mcp.Implementation{Name: "leadline", Title: "Leadline", Version: "0.0.1"},
		&mcp.ServerOptions{Instructions: instructions})

	mcp.AddTool(server, tool("overview", "Overview",
		"What happened lately: request, failure, query and log counts, the costliest queries, and any N+1 queries. The place to start."),
		t.overview)
	mcp.AddTool(server, tool("list_queries", "List queries",
		"Database queries grouped by shape (values replaced by ?), costliest first, with call counts and timings. A query flagged n_plus_one ran many times within one request."),
		t.listQueries)
	mcp.AddTool(server, tool("list_traces", "List traces",
		"Recent traces (one per request or job), newest first. Filter to failures or slow ones, then read one with get_trace."),
		t.listTraces)
	mcp.AddTool(server, tool("get_trace", "Get trace",
		"Everything one trace did, in order: each span with its timing, the exact SQL of each database call, and the logs written during it."),
		t.getTrace)
	mcp.AddTool(server, tool("search_logs", "Search logs",
		"Log records, newest first, filtered by text, level or service. A record's trace_id leads to its request with get_trace."),
		t.searchLogs)
	mcp.AddTool(server, tool("get_metrics", "Get metrics",
		"The metrics apps export (request durations, pool sizes, memory, counters). Without a name: each metric's kind, unit and latest value. With a name: that metric over time in a few buckets, split by service or by an attribute when asked."),
		t.getMetrics)
	mcp.AddTool(server, tool("list_routes", "List routes",
		"The routes an app has, from its framework's own route list (sent by the Next.js plugin), with the file that serves each. By default only the ones nothing has requested in the window: code no test or click has exercised."),
		t.listRoutes)
	mcp.AddTool(server, tool("list_sources", "List sources",
		"What is feeding Leadline right now: each service with how much it has sent and when it was last heard from, each Postgres proxy with its statement count, each connected database with its health. Use it to check that setting an app up worked."),
		t.listSources)

	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
}

func tool(name, title, description string) *mcp.Tool {
	closed := false
	return &mcp.Tool{
		Name: name, Title: title, Description: description,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &closed},
	}
}

type Query struct {
	Database    string  `json:"database"`
	Query       string  `json:"query" jsonschema:"the query's shape, with values replaced by ?"`
	Example     string  `json:"example,omitempty" jsonschema:"one real statement of this shape"`
	Calls       int64   `json:"calls"`
	TotalMs     float64 `json:"total_ms"`
	AvgMs       float64 `json:"avg_ms"`
	P95Ms       float64 `json:"p95_ms"`
	MaxMs       float64 `json:"max_ms"`
	Errors      int64   `json:"errors,omitempty"`
	MaxPerTrace int64   `json:"max_per_trace" jsonschema:"the most times a single trace ran it"`
	NPlusOne    bool    `json:"n_plus_one,omitempty"`
}

func queryOut(r store.QueryRow, example bool) Query {
	q := Query{
		Database: r.DBSystem, Query: r.Fingerprint, Calls: r.Calls,
		TotalMs: round(r.TotalMs), AvgMs: round(r.AvgMs), P95Ms: round(r.P95Ms), MaxMs: round(r.MaxMs),
		Errors: r.Errors, MaxPerTrace: r.MaxPerTrace, NPlusOne: r.MaxPerTrace >= NPlusOne,
	}
	if example && r.Example != r.Fingerprint {
		q.Example = r.Example
	}
	return q
}

type OverviewIn struct {
	Minutes float64 `json:"minutes,omitempty" jsonschema:"how far back to look, in minutes (default 15)"`
	Service string  `json:"service,omitempty" jsonschema:"only this service"`
}

type OverviewOut struct {
	WindowMinutes     float64  `json:"window_minutes"`
	Services          []string `json:"services" jsonschema:"every service Leadline has data for"`
	Requests          int64    `json:"requests" jsonschema:"calls arriving at a service: what list_traces and the app's endpoints count"`
	FailedSpans       int64    `json:"failed_spans"`
	Queries           int64    `json:"queries" jsonschema:"database statements, each counted once; BEGIN, COMMIT and the like are left out"`
	QueriesPerRequest float64  `json:"queries_per_request"`
	DatabaseMs        float64  `json:"database_ms" jsonschema:"total time spent in database calls"`
	Logs              int64    `json:"logs"`
	ErrorLogs         int64    `json:"error_logs"`
	CostliestQueries  []Query  `json:"costliest_queries"`
	NPlusOneQueries   []Query  `json:"n_plus_one_queries"`
	Hint              string   `json:"hint,omitempty"`
}

func (t *tools) overview(ctx context.Context, _ *mcp.CallToolRequest, in OverviewIn) (*mcp.CallToolResult, OverviewOut, error) {
	minutes := orDefault(in.Minutes, 15)
	now := nowMs()
	since := now - minutes*60_000
	out := OverviewOut{WindowMinutes: minutes, CostliestQueries: []Query{}, NPlusOneQueries: []Query{}}

	sum, err := t.store.Summary(ctx)
	if err != nil {
		return nil, out, err
	}
	out.Services = sum.Services
	if sum.Spans == 0 && sum.Logs == 0 && sum.MetricPoints == 0 {
		out.Hint = "Leadline has no data at all. The app must export OpenTelemetry to this daemon and then handle some requests."
		return nil, out, nil
	}

	series, err := t.store.SeriesBetween(ctx, since, now, 1, in.Service)
	if err != nil {
		return nil, out, err
	}
	out.Requests, out.FailedSpans = int64(series.Requests[0]), int64(series.Errors[0])
	out.Queries, out.DatabaseMs = int64(series.Queries[0]), round(series.DBMs[0])
	out.Logs, out.ErrorLogs = int64(series.Logs[0]), int64(series.LogErrors[0])
	if out.Requests > 0 {
		out.QueriesPerRequest = round(float64(out.Queries) / float64(out.Requests))
	}

	rows, err := t.store.Queries(ctx, store.Filter{SinceMs: since, Service: in.Service, Limit: 200})
	if err != nil {
		return nil, out, err
	}
	for i, r := range rows {
		if i < 5 {
			out.CostliestQueries = append(out.CostliestQueries, queryOut(r, false))
		}
		if r.MaxPerTrace >= NPlusOne && len(out.NPlusOneQueries) < 10 {
			out.NPlusOneQueries = append(out.NPlusOneQueries, queryOut(r, false))
		}
	}
	if out.Requests == 0 && out.Queries == 0 && out.Logs == 0 {
		out.Hint = fmt.Sprintf("No requests, queries or logs arrived in the last %g minutes. Exercise the app, or pass a larger minutes.", minutes)
		if sum.Spans == 0 && sum.Logs == 0 {
			out.Hint = "This app has sent only metrics so far; get_metrics lists them."
		}
	}
	return nil, out, nil
}

type ListQueriesIn struct {
	Minutes float64 `json:"minutes,omitempty" jsonschema:"how far back to look, in minutes (default 15)"`
	Service string  `json:"service,omitempty" jsonschema:"only this service"`
	Search  string  `json:"search,omitempty" jsonschema:"only statements containing this text, such as a table name"`
	Limit   int     `json:"limit,omitempty" jsonschema:"how many to return (default 20)"`
}

type ListQueriesOut struct {
	Queries []Query `json:"queries"`
}

func (t *tools) listQueries(ctx context.Context, _ *mcp.CallToolRequest, in ListQueriesIn) (*mcp.CallToolResult, ListQueriesOut, error) {
	out := ListQueriesOut{Queries: []Query{}}
	rows, err := t.store.Queries(ctx, store.Filter{
		SinceMs: sinceMs(in.Minutes), Service: in.Service, Search: in.Search, Limit: limit(in.Limit, 20, 200),
	})
	if err != nil {
		return nil, out, err
	}
	for _, r := range rows {
		out.Queries = append(out.Queries, queryOut(r, true))
	}
	return nil, out, nil
}

type ListTracesIn struct {
	Minutes       float64 `json:"minutes,omitempty" jsonschema:"how far back to look, in minutes (default 15)"`
	Service       string  `json:"service,omitempty" jsonschema:"only traces that start in this service"`
	Search        string  `json:"search,omitempty" jsonschema:"only traces whose name contains this text, such as a route"`
	ErrorsOnly    bool    `json:"errors_only,omitempty" jsonschema:"only traces with a failed span"`
	MinDurationMs float64 `json:"min_duration_ms,omitempty" jsonschema:"only traces at least this long"`
	Limit         int     `json:"limit,omitempty" jsonschema:"how many to return (default 20)"`
}

type Trace struct {
	TraceID    string  `json:"trace_id"`
	Name       string  `json:"name"`
	Service    string  `json:"service"`
	Start      string  `json:"start"`
	DurationMs float64 `json:"duration_ms"`
	Spans      int64   `json:"spans"`
	Errors     int64   `json:"errors,omitempty"`
	Queries    int64   `json:"queries"`
	DatabaseMs float64 `json:"database_ms"`
}

type ListTracesOut struct {
	Traces []Trace `json:"traces"`
}

func (t *tools) listTraces(ctx context.Context, _ *mcp.CallToolRequest, in ListTracesIn) (*mcp.CallToolResult, ListTracesOut, error) {
	out := ListTracesOut{Traces: []Trace{}}
	rows, err := t.store.Traces(ctx, store.Filter{
		SinceMs: sinceMs(in.Minutes), Service: in.Service, Search: in.Search,
		ErrorsOnly: in.ErrorsOnly, MinDurationMs: in.MinDurationMs, Limit: limit(in.Limit, 20, 200),
	})
	if err != nil {
		return nil, out, err
	}
	for _, r := range rows {
		out.Traces = append(out.Traces, Trace{
			TraceID: r.TraceID, Name: r.Name, Service: r.Service, Start: clock(r.StartMs),
			DurationMs: round(r.DurationMs), Spans: r.Spans, Errors: r.Errors,
			Queries: r.Queries, DatabaseMs: round(r.DBMs),
		})
	}
	return nil, out, nil
}

type GetTraceIn struct {
	TraceID           string `json:"trace_id" jsonschema:"a trace_id from list_traces or search_logs"`
	IncludeAttributes bool   `json:"include_attributes,omitempty" jsonschema:"also return every attribute of every span (large)"`
}

type Span struct {
	SpanID     string         `json:"span_id"`
	ParentID   string         `json:"parent_id,omitempty"`
	Service    string         `json:"service"`
	Name       string         `json:"name"`
	OffsetMs   float64        `json:"offset_ms" jsonschema:"when it started, from the start of the trace"`
	DurationMs float64        `json:"duration_ms"`
	Error      string         `json:"error,omitempty" jsonschema:"set when the span failed"`
	Database   string         `json:"database,omitempty"`
	Statement  string         `json:"statement,omitempty" jsonschema:"the exact statement of a database call"`
	Role       string         `json:"role,omitempty" jsonschema:"set on a database call that is not counted as a query: control (BEGIN, COMMIT, SET and the like) or duplicate (the same call as its child span, which has the exact timing)"`
	HTTPStatus int32          `json:"http_status,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

type Log struct {
	Time    string `json:"time"`
	Level   string `json:"level"`
	Service string `json:"service"`
	Message string `json:"message"`
	TraceID string `json:"trace_id,omitempty"`
}

type GetTraceOut struct {
	TraceID    string  `json:"trace_id"`
	Start      string  `json:"start"`
	DurationMs float64 `json:"duration_ms"`
	Spans      []Span  `json:"spans" jsonschema:"in start order; parent_id gives the nesting"`
	Logs       []Log   `json:"logs"`
}

func (t *tools) getTrace(ctx context.Context, _ *mcp.CallToolRequest, in GetTraceIn) (*mcp.CallToolResult, GetTraceOut, error) {
	id := strings.ToLower(strings.TrimSpace(in.TraceID))
	out := GetTraceOut{TraceID: id, Spans: []Span{}, Logs: []Log{}}
	// Spans and logs together: the logs are then looked for only around
	// the time the trace ran.
	spans, logs, err := t.store.Trace(ctx, id)
	if err != nil {
		return nil, out, err
	}
	if len(spans) == 0 {
		return nil, out, fmt.Errorf("no trace with id %q; list_traces gives the ids Leadline has", in.TraceID)
	}
	start, end := spans[0].StartMs, 0.0
	for _, s := range spans {
		end = math.Max(end, s.StartMs+s.DurationMs)
	}
	out.Start, out.DurationMs = clock(start), round(end-start)
	for _, s := range spans {
		sp := Span{
			SpanID: s.SpanID, ParentID: s.ParentID, Service: s.Service, Name: s.Name,
			OffsetMs: round(s.StartMs - start), DurationMs: round(s.DurationMs),
			Database: s.DBSystem, Statement: s.DBStatement, HTTPStatus: s.HTTPStatus,
		}
		if s.Status == "error" {
			sp.Error = orString(s.StatusMessage, "failed")
		}
		if s.DBRole != "query" {
			sp.Role = s.DBRole
		}
		if in.IncludeAttributes {
			json.Unmarshal(s.Attrs, &sp.Attributes)
		}
		out.Spans = append(out.Spans, sp)
	}
	out.Logs = logsOut(logs, false)
	return nil, out, nil
}

type SearchLogsIn struct {
	Minutes float64 `json:"minutes,omitempty" jsonschema:"how far back to look, in minutes (default 15)"`
	Service string  `json:"service,omitempty" jsonschema:"only this service"`
	Search  string  `json:"search,omitempty" jsonschema:"only messages containing this text"`
	Level   string  `json:"level,omitempty" jsonschema:"the lowest level to return: debug, info, warn or error (default: everything)"`
	Limit   int     `json:"limit,omitempty" jsonschema:"how many to return (default 50)"`
}

type SearchLogsOut struct {
	Logs []Log `json:"logs"`
}

// OTLP severity numbers: the lowest number of each level.
var levels = map[string]int32{"": 0, "trace": 1, "debug": 5, "info": 9, "warn": 13, "warning": 13, "error": 17, "fatal": 21}

func (t *tools) searchLogs(ctx context.Context, _ *mcp.CallToolRequest, in SearchLogsIn) (*mcp.CallToolResult, SearchLogsOut, error) {
	out := SearchLogsOut{Logs: []Log{}}
	min, ok := levels[strings.ToLower(in.Level)]
	if !ok {
		return nil, out, fmt.Errorf("level %q is not one of debug, info, warn, error", in.Level)
	}
	rows, err := t.store.Logs(ctx, store.Filter{
		SinceMs: sinceMs(in.Minutes), Service: in.Service, Search: in.Search,
		MinSeverity: min, Limit: limit(in.Limit, 50, 500),
	})
	if err != nil {
		return nil, out, err
	}
	out.Logs = logsOut(rows, true)
	return nil, out, nil
}

func logsOut(rows []store.LogRow, withTrace bool) []Log {
	out := make([]Log, 0, len(rows))
	for _, l := range rows {
		log := Log{Time: clock(l.TsMs), Level: l.Severity, Service: l.Service, Message: l.Body}
		if withTrace {
			log.TraceID = l.TraceID
		}
		out = append(out, log)
	}
	return out
}

func nowMs() float64 { return float64(time.Now().UnixNano()) / 1e6 }

func sinceMs(minutes float64) float64 { return nowMs() - orDefault(minutes, 15)*60_000 }

func orDefault(v, def float64) float64 {
	if v <= 0 {
		return def
	}
	return v
}

func orString(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func limit(v, def, max int) int {
	if v <= 0 {
		return def
	}
	return min(v, max)
}

// round keeps three decimals: microseconds are enough, and shorter numbers
// cost an agent fewer tokens.
func round(ms float64) float64 { return math.Round(ms*1000) / 1000 }

// sig keeps six significant figures, for values of any size.
func sig(v float64) float64 {
	if v == 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	scale := math.Pow(10, 5-math.Floor(math.Log10(math.Abs(v))))
	return math.Round(v*scale) / scale
}

func containsFold(s, part string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(part))
}

func clock(ms float64) string {
	return time.UnixMicro(int64(ms * 1000)).Format("2006-01-02T15:04:05.000Z07:00")
}
