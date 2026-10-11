package mcpserver

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sean-brydon/leadline/internal/store"
)

// get_metrics is one tool with two answers, so an agent pays for one tool's
// description: the list of metric names, or one metric over time. Both are
// small by default: thirty names, twelve buckets, four lines.

const (
	defaultMetricNames   = 30
	defaultMetricBuckets = 12
	maxMetricBuckets     = 60
	defaultMetricGroups  = 4
)

type GetMetricsIn struct {
	Name    string  `json:"name,omitempty" jsonschema:"one metric's name, to get it over time; leave out to list the metrics"`
	Minutes float64 `json:"minutes,omitempty" jsonschema:"how far back to look, in minutes (default 15)"`
	Service string  `json:"service,omitempty" jsonschema:"only this service"`
	Search  string  `json:"search,omitempty" jsonschema:"listing: only names containing this text"`
	By      string  `json:"by,omitempty" jsonschema:"with name: split into lines by service, or by one of the metric's attribute keys"`
	Buckets int     `json:"buckets,omitempty" jsonschema:"with name: how many points in time (default 12, at most 60)"`
	Limit   int     `json:"limit,omitempty" jsonschema:"listing: how many names (default 30); with name: how many lines (default 4)"`
}

type Metric struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind" jsonschema:"gauge (a level), rate (a counter: how much it rose) or histogram (a distribution, such as durations)"`
	Unit      string   `json:"unit,omitempty"`
	LastValue float64  `json:"last_value" jsonschema:"a gauge's level now, a counter's running total, a histogram's mean"`
	LastSeen  string   `json:"last_seen"`
	Services  []string `json:"services,omitempty"`
}

type MetricLine struct {
	Label string `json:"label,omitempty" jsonschema:"the value of what it was split by; empty for the one line of everything"`
	// One per bucket; null where the metric said nothing.
	Values []*float64 `json:"values" jsonschema:"per bucket: a gauge's average, a counter's increase, or how many observations a histogram took"`
	P95    []*float64 `json:"p95,omitempty" jsonschema:"histograms: the 95th percentile per bucket, estimated from the bucket bounds"`
	Total  float64    `json:"total,omitempty" jsonschema:"over the window: a counter's increase, a histogram's observations"`
	Last   *float64   `json:"last,omitempty" jsonschema:"a gauge's newest value"`
	Avg    *float64   `json:"avg,omitempty" jsonschema:"a histogram's mean over the window"`
	P95All *float64   `json:"p95_all,omitempty" jsonschema:"a histogram's 95th percentile over the window"`
}

type MetricOverTime struct {
	Name          string       `json:"name"`
	Kind          string       `json:"kind"`
	Unit          string       `json:"unit,omitempty"`
	Description   string       `json:"description,omitempty"`
	Start         string       `json:"start" jsonschema:"when the first bucket begins"`
	BucketSeconds float64      `json:"bucket_seconds"`
	By            string       `json:"by,omitempty"`
	Keys          []string     `json:"keys,omitempty" jsonschema:"attribute keys it can be split by"`
	Lines         []MetricLine `json:"lines"`
	MoreLines     int          `json:"more_lines,omitempty"`
}

type GetMetricsOut struct {
	Metrics []Metric        `json:"metrics,omitempty"`
	Metric  *MetricOverTime `json:"metric,omitempty"`
	Hint    string          `json:"hint,omitempty"`
}

func (t *tools) getMetrics(ctx context.Context, _ *mcp.CallToolRequest, in GetMetricsIn) (*mcp.CallToolResult, GetMetricsOut, error) {
	var out GetMetricsOut
	minutes := orDefault(in.Minutes, 15)
	now := nowMs()
	since := now - minutes*60_000

	if in.Name == "" {
		n := limit(in.Limit, defaultMetricNames, 200)
		// One more than wanted says whether there are more.
		rows, err := t.store.Metrics(ctx, store.Filter{SinceMs: since, Service: in.Service, Search: in.Search, Limit: n + 1})
		if err != nil {
			return nil, out, err
		}
		if len(rows) > n {
			rows = rows[:n]
			out.Hint = "There are more metrics than this; narrow with search, or raise limit."
		}
		out.Metrics = []Metric{}
		for _, m := range rows {
			out.Metrics = append(out.Metrics, Metric{Name: m.Name, Kind: m.Kind, Unit: m.Unit,
				LastValue: sig(m.LastValue), LastSeen: clock(m.LastMs), Services: m.Services})
		}
		if len(rows) == 0 {
			out.Hint = fmt.Sprintf("No metrics arrived in the last %g minutes. The app must export OTLP metrics to this daemon (list_sources shows what is arriving), or pass a larger minutes.", minutes)
		}
		return nil, out, nil
	}

	buckets := limit(in.Buckets, defaultMetricBuckets, maxMetricBuckets)
	series, err := t.store.MetricSeries(ctx, in.Name, since, now, buckets, in.Service, in.By, limit(in.Limit, defaultMetricGroups, 20))
	if err != nil {
		return nil, out, err
	}
	if series.Type == "" {
		return nil, out, fmt.Errorf("no metric named %q in the last %g minutes; call get_metrics without a name for the ones there are", in.Name, minutes)
	}
	m := &MetricOverTime{Name: series.Name, Kind: series.Kind, Unit: series.Unit, Description: series.Description,
		Start: clock(since), BucketSeconds: round(series.BucketMs / 1000), By: series.By, Keys: series.Keys,
		Lines: []MetricLine{}, MoreLines: series.More}
	for _, g := range series.Groups {
		line := MetricLine{Label: g.Label, Values: rounded(g.Value), P95: rounded(g.P95), Total: sig(g.Stats.Total),
			Last: roundPtr(g.Stats.Last), Avg: roundPtr(g.Stats.Avg), P95All: roundPtr(g.Stats.P95)}
		m.Lines = append(m.Lines, line)
	}
	out.Metric = m
	return nil, out, nil
}

func rounded(v []*float64) []*float64 {
	if v == nil {
		return nil
	}
	out := make([]*float64, len(v))
	for i, x := range v {
		out[i] = roundPtr(x)
	}
	return out
}

func roundPtr(v *float64) *float64 {
	if v == nil {
		return nil
	}
	// Six significant figures: a duration in seconds needs the small ones,
	// a count of bytes does not need the last few.
	r := sig(*v)
	return &r
}

type ListRoutesIn struct {
	Service string  `json:"service,omitempty" jsonschema:"only this service"`
	Minutes float64 `json:"minutes,omitempty" jsonschema:"how far back to count requests, in minutes (default 60)"`
	All     bool    `json:"all,omitempty" jsonschema:"every route with its request count, not only the ones with none"`
	Search  string  `json:"search,omitempty" jsonschema:"only routes or files containing this text"`
	Limit   int     `json:"limit,omitempty" jsonschema:"how many to return (default 40)"`
}

type Route struct {
	Service  string `json:"service"`
	Route    string `json:"route" jsonschema:"the pattern, as in /products/[id]"`
	Kind     string `json:"kind" jsonschema:"page, route (a route handler), api, layout, metadata or proxy"`
	File     string `json:"file,omitempty"`
	Requests int64  `json:"requests"`
}

type ListRoutesOut struct {
	Routes     []Route `json:"routes"`
	MoreRoutes int     `json:"more_routes,omitempty"`
	Hint       string  `json:"hint,omitempty"`
}

func (t *tools) listRoutes(ctx context.Context, _ *mcp.CallToolRequest, in ListRoutesIn) (*mcp.CallToolResult, ListRoutesOut, error) {
	out := ListRoutesOut{Routes: []Route{}}
	now := nowMs()
	lists, err := t.store.Routes(ctx, store.Window{SinceMs: now - orDefault(in.Minutes, 60)*60_000, UntilMs: now, Service: in.Service})
	if err != nil {
		return nil, out, err
	}
	if len(lists) == 0 {
		out.Hint = "No app has sent a route list. The Next.js plugin (@leadline/next) sends one; without it, list_traces shows the routes that were requested."
		return nil, out, nil
	}
	n := limit(in.Limit, 40, 500)
	for _, l := range lists {
		for _, r := range l.Routes {
			// A layout or a proxy runs inside other requests: it has none
			// of its own to be missing.
			named := r.Kind == "page" || r.Kind == "route" || r.Kind == "api"
			if !in.All && (r.Requests > 0 || !named) {
				continue
			}
			if in.Search != "" && !containsFold(r.Route, in.Search) && !containsFold(r.File, in.Search) {
				continue
			}
			if len(out.Routes) == n {
				out.MoreRoutes++
				continue
			}
			out.Routes = append(out.Routes, Route{Service: l.Service, Route: r.Route, Kind: r.Kind, File: r.File, Requests: r.Requests})
		}
	}
	return nil, out, nil
}
