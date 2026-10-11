package mcpserver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/proto"

	"github.com/sean-brydon/leadline/internal/api"
	"github.com/sean-brydon/leadline/internal/demo"
	"github.com/sean-brydon/leadline/internal/mcpserver"
	"github.com/sean-brydon/leadline/internal/store"
)

// connect starts a daemon holding demo data and returns an MCP client
// session on it: the same path a coding agent takes.
func connect(t *testing.T) *mcp.ClientSession {
	t.Helper()
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(api.New(st, nil).Handler())
	t.Cleanup(func() {
		ts.Close()
		st.Close()
	})

	now := time.Now()
	gen := demo.New(7)
	traces, logs := gen.Batch(300, now.Add(-10*time.Minute), now)
	metrics := gen.Metrics(now.Add(-10*time.Minute), now)
	for path, msg := range map[string]proto.Message{"/v1/traces": traces, "/v1/logs": logs, "/v1/metrics": metrics} {
		body, _ := proto.Marshal(msg)
		resp, err := http.Post(ts.URL+path, "application/x-protobuf", strings.NewReader(string(body)))
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("POST %s: %v %v", path, err, resp)
		}
		resp.Body.Close()
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// call runs a tool and decodes its structured result.
func call[Out any](t *testing.T, s *mcp.ClientSession, name string, args map[string]any) Out {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s: tool error: %+v", name, res.Content)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out Out
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: %v in %s", name, err, raw)
	}
	return out
}

func TestToolsAreReadOnly(t *testing.T) {
	s := connect(t)
	list, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 8 {
		t.Errorf("got %d tools, want 8", len(list.Tools))
	}
	for _, tool := range list.Tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s is not marked read-only", tool.Name)
		}
	}
}

func TestOverviewFindsTheNPlusOne(t *testing.T) {
	out := call[mcpserver.OverviewOut](t, connect(t), "overview", nil)
	// 300 traces, and the payments service's side of each checkout.
	if out.Requests <= 300 || out.Requests > 400 {
		t.Errorf("requests = %d, want the 300 traces and a few dozen calls between services", out.Requests)
	}
	if len(out.CostliestQueries) != 5 {
		t.Errorf("got %d costliest queries, want 5", len(out.CostliestQueries))
	}
	if len(out.NPlusOneQueries) != 1 || !strings.Contains(out.NPlusOneQueries[0].Query, "FROM order_items") {
		t.Errorf("n+1 queries = %+v, want the order_items one", out.NPlusOneQueries)
	}
}

// The walk an agent makes from "something failed" to the cause: failing
// traces, then one trace's spans and logs.
func TestFromFailureToCause(t *testing.T) {
	s := connect(t)
	list := call[mcpserver.ListTracesOut](t, s, "list_traces", map[string]any{"errors_only": true, "limit": 3})
	if len(list.Traces) == 0 {
		t.Fatal("no failing traces in the demo data")
	}
	for _, tr := range list.Traces {
		if tr.Errors == 0 {
			t.Errorf("errors_only returned %s, which has none", tr.TraceID)
		}
	}

	trace := call[mcpserver.GetTraceOut](t, s, "get_trace", map[string]any{"trace_id": list.Traces[0].TraceID})
	var failed, rolledBack bool
	for _, sp := range trace.Spans {
		failed = failed || sp.Error != ""
		rolledBack = rolledBack || sp.Statement == "ROLLBACK"
		if sp.Attributes != nil {
			t.Error("attributes returned without being asked for")
		}
	}
	if !failed || !rolledBack {
		t.Errorf("trace shows failed=%v rolledBack=%v, want both", failed, rolledBack)
	}
	if len(trace.Logs) == 0 {
		t.Error("the failing checkout's logs are missing")
	}

	logs := call[mcpserver.SearchLogsOut](t, s, "search_logs", map[string]any{"level": "error", "search": "declined"})
	if len(logs.Logs) == 0 {
		t.Fatal("no error logs")
	}
	for _, l := range logs.Logs {
		if l.Level != "error" || l.TraceID == "" {
			t.Errorf("log = %+v, want an error with a trace id", l)
		}
	}
}

func TestUnknownTraceIsAToolError(t *testing.T) {
	res, err := connect(t).CallTool(context.Background(), &mcp.CallToolParams{
		Name: "get_trace", Arguments: map[string]any{"trace_id": "0000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("an unknown trace id should be a tool error the agent can read")
	}
}
