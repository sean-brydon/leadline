package mcpserver

import (
	"context"
	"sort"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// list_sources is how an agent checks that its setup worked: after it
// points an app at Leadline, is anything arriving, and from where.

const maxSourceServices = 20

type ListSourcesIn struct{}

type ListSourcesOut struct {
	OTLPEndpoints []string           `json:"otlp_endpoints" jsonschema:"where an app should send OTLP/HTTP"`
	Services      []SourceService    `json:"services" jsonschema:"each service Leadline holds data for, most recently heard from first"`
	MoreServices  int                `json:"more_services,omitempty"`
	Proxies       []SourceProxy      `json:"proxies" jsonschema:"Postgres proxies: an app pointed at one has its queries recorded"`
	Connections   []SourceConnection `json:"connections" jsonschema:"databases and Redis the user connected"`
	Hint          string             `json:"hint,omitempty"`
}

type SourceService struct {
	Name         string   `json:"name"`
	Spans        int64    `json:"spans"`
	Logs         int64    `json:"logs"`
	MetricPoints int64    `json:"metric_points"`
	LastSeen     string   `json:"last_seen"`
	Via          []string `json:"via" jsonschema:"how its data arrives: otlp, postgres-proxy, redis-monitor"`
}

type SourceProxy struct {
	Listen       string `json:"listen"`
	Upstream     string `json:"upstream"`
	Label        string `json:"label,omitempty"`
	Error        string `json:"error,omitempty" jsonschema:"set when the proxy could not open its port"`
	Connections  int64  `json:"connections" jsonschema:"client connections since it opened"`
	Statements   int64  `json:"statements" jsonschema:"statements recorded since it opened"`
	LastActivity string `json:"last_activity,omitempty"`
}

type SourceConnection struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Host    string `json:"host"`
	Remote  bool   `json:"remote,omitempty"`
	Capture bool   `json:"capture,omitempty" jsonschema:"its traffic is being recorded"`
	Health  string `json:"health" jsonschema:"ok, error, or unknown before the first check"`
	Error   string `json:"error,omitempty"`
}

func (t *tools) listSources(ctx context.Context, _ *mcp.CallToolRequest, _ ListSourcesIn) (*mcp.CallToolResult, ListSourcesOut, error) {
	out := ListSourcesOut{OTLPEndpoints: []string{}, Services: []SourceService{}, Proxies: []SourceProxy{}, Connections: []SourceConnection{}}
	src, err := t.integ.Sources(ctx)
	if err != nil {
		return nil, out, err
	}
	out.OTLPEndpoints = src.OTLP.Endpoints

	services := src.Services
	sort.SliceStable(services, func(i, j int) bool { return services[i].LastSeenMs > services[j].LastSeenMs })
	if len(services) > maxSourceServices {
		out.MoreServices = len(services) - maxSourceServices
		services = services[:maxSourceServices]
	}
	for _, s := range services {
		out.Services = append(out.Services, SourceService{Name: s.Name, Spans: s.Spans, Logs: s.Logs,
			MetricPoints: s.MetricPoints, LastSeen: seen(s.LastSeenMs), Via: s.Via})
	}
	var carried int64
	for _, p := range src.Proxies {
		sp := SourceProxy{Listen: p.Listen, Upstream: p.Upstream, Label: p.Label, Error: p.Error,
			Connections: p.ConnectionsTotal, Statements: p.Statements}
		if p.LastActivityMs > 0 {
			sp.LastActivity = clock(p.LastActivityMs)
		}
		carried += p.ConnectionsTotal
		out.Proxies = append(out.Proxies, sp)
	}
	for _, c := range src.Connections {
		// The URL stays out, masked or not: an agent has no use for it.
		out.Connections = append(out.Connections, SourceConnection{Name: c.Name, Kind: c.Kind, Host: c.Host,
			Remote: c.Remote, Capture: c.Capture, Health: c.Health.State, Error: c.Health.Message})
	}

	if len(out.Services) == 0 && carried == 0 {
		out.Hint = "Nothing is feeding Leadline yet. Send OpenTelemetry (OTLP/HTTP) to one of otlp_endpoints, " +
			"or start the app with `leadline run`, or point its database URL at a proxy's listen address. Then exercise the app and look again."
	}
	return nil, out, nil
}

// seen is when a source was last heard from. One that is attached but has
// sent nothing has no time, which must not read as 1970.
func seen(ms float64) string {
	if ms <= 0 {
		return "never"
	}
	return clock(ms)
}
