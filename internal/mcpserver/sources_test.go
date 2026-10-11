package mcpserver_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sean-brydon/leadline/internal/mcpserver"
)

func TestListSources(t *testing.T) {
	s := connect(t)
	out := call[mcpserver.ListSourcesOut](t, s, "list_sources", nil)
	if len(out.Services) < 3 || out.Hint != "" {
		t.Fatalf("list_sources = %+v", out)
	}
	for _, svc := range out.Services {
		if svc.Spans+svc.Logs == 0 || svc.LastSeen == "" || !slices.Contains(svc.Via, "otlp") {
			t.Errorf("service = %+v", svc)
		}
	}
	// It is for a quick check, so it has to stay a small one.
	if raw, _ := json.Marshal(out); len(raw) > 4000 {
		t.Errorf("list_sources is %d bytes", len(raw))
	}
}

// get_metrics lists what there is, then gives one metric over time. Both
// answers have to stay small enough to sit in an agent's context.
func TestGetMetrics(t *testing.T) {
	s := connect(t)
	list := call[mcpserver.GetMetricsOut](t, s, "get_metrics", nil)
	if len(list.Metrics) != 6 || list.Metric != nil {
		t.Fatalf("metrics = %+v, want the demo's six names", list)
	}
	kinds := map[string]string{}
	for _, m := range list.Metrics {
		kinds[m.Name] = m.Kind
		if m.LastSeen == "" || len(m.Services) == 0 {
			t.Errorf("metric = %+v", m)
		}
	}
	if kinds["http.server.request.duration"] != "histogram" || kinds["jobs.processed"] != "rate" || kinds["process.runtime.memory"] != "gauge" {
		t.Errorf("kinds = %v", kinds)
	}
	if raw, _ := json.Marshal(list); len(raw) > 1500 {
		t.Errorf("the list of six metrics is %d bytes", len(raw))
	}

	one := call[mcpserver.GetMetricsOut](t, s, "get_metrics", map[string]any{"name": "http.server.request.duration"})
	if one.Metric == nil || one.Metric.Kind != "histogram" || one.Metric.Unit != "s" || len(one.Metric.Lines) != 1 {
		t.Fatalf("metric = %+v", one.Metric)
	}
	line := one.Metric.Lines[0]
	if len(line.Values) != 12 || len(line.P95) != 12 || line.Total <= 0 || line.Avg == nil || line.P95All == nil {
		t.Errorf("line = %+v, want 12 buckets with percentiles", line)
	}
	if !slices.Contains(one.Metric.Keys, "http.route") {
		t.Errorf("keys = %v, want the attributes it can be split by", one.Metric.Keys)
	}
	if raw, _ := json.Marshal(one); len(raw) > 1500 {
		t.Errorf("one metric by default is %d bytes", len(raw))
	}

	// Split by route: still a handful of lines.
	split := call[mcpserver.GetMetricsOut](t, s, "get_metrics", map[string]any{"name": "http.server.request.duration", "by": "http.route", "buckets": 6})
	if len(split.Metric.Lines) < 2 || len(split.Metric.Lines) > 4 || len(split.Metric.Lines[0].Values) != 6 || split.Metric.Lines[0].Label == "" {
		t.Errorf("split = %+v", split.Metric)
	}
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_metrics", Arguments: map[string]any{"name": "no.such.metric"}})
	if err != nil || !res.IsError {
		t.Errorf("an unknown metric should be a tool error the agent can read: %v", err)
	}
}

// list_routes shows the routes nothing has asked for, once a framework has
// sent its list.
func TestListRoutes(t *testing.T) {
	s := connect(t)
	none := call[mcpserver.ListRoutesOut](t, s, "list_routes", nil)
	if len(none.Routes) != 0 || none.Hint == "" {
		t.Errorf("with no route list: %+v", none)
	}
}
