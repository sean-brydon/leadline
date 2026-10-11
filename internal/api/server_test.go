package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/sean-brydon/leadline/internal/demo"
	"github.com/sean-brydon/leadline/internal/store"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	st, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(New(st, nil).Handler())
	t.Cleanup(func() {
		ts.Close()
		st.Close()
	})
	return ts
}

func post(t *testing.T, url, contentType string, body []byte) *http.Response {
	t.Helper()
	resp, err := http.Post(url, contentType, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func getJSON(t *testing.T, url string, into any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s", url, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
}

func TestIngestAndQuery(t *testing.T) {
	ts := newTestServer(t)
	now := time.Now()
	traces, logs := demo.New(1).Batch(200, now.Add(-10*time.Minute), now)

	for path, msg := range map[string]proto.Message{"/v1/traces": traces, "/v1/logs": logs} {
		body, _ := proto.Marshal(msg)
		if resp := post(t, ts.URL+path, "application/x-protobuf", body); resp.StatusCode != http.StatusOK {
			t.Fatalf("POST %s: %s", path, resp.Status)
		}
	}

	var sum store.Summary
	getJSON(t, ts.URL+"/api/summary", &sum)
	if sum.Traces != 200 {
		t.Errorf("traces = %d, want 200", sum.Traces)
	}
	if sum.Logs == 0 || len(sum.Services) < 3 {
		t.Errorf("summary = %+v, want logs and at least 3 services", sum)
	}

	var list []store.TraceRow
	getJSON(t, ts.URL+"/api/traces?limit=500", &list)
	if len(list) != 200 {
		t.Fatalf("listed %d traces, want 200", len(list))
	}
	for i := 1; i < len(list); i++ {
		if list[i].StartMs > list[i-1].StartMs {
			t.Fatal("traces are not newest first")
		}
	}

	var detail struct {
		Spans []store.SpanRow `json:"spans"`
	}
	getJSON(t, ts.URL+"/api/traces/"+list[0].TraceID, &detail)
	if int64(len(detail.Spans)) != list[0].Spans {
		t.Errorf("trace has %d spans, list said %d", len(detail.Spans), list[0].Spans)
	}

	// The demo's order list runs one order_items query per order: every
	// value differs, so it only shows as one row if fingerprints group it,
	// and it must be flagged as running many times in a trace.
	var queries []store.QueryRow
	getJSON(t, ts.URL+"/api/queries", &queries)
	var items *store.QueryRow
	for i := range queries {
		if strings.Contains(queries[i].Fingerprint, "FROM order_items") {
			if items != nil {
				t.Fatal("order_items query is split across fingerprints")
			}
			items = &queries[i]
		}
	}
	if items == nil {
		t.Fatal("no order_items query")
	}
	if items.Fingerprint != "SELECT id, product_id, quantity FROM order_items WHERE order_id = ?" {
		t.Errorf("fingerprint = %q", items.Fingerprint)
	}
	if items.MaxPerTrace < 6 {
		t.Errorf("max per trace = %d, want an N+1", items.MaxPerTrace)
	}

	var errLogs []store.LogRow
	getJSON(t, ts.URL+"/api/logs?min_severity=17", &errLogs)
	for _, l := range errLogs {
		if l.Severity != "error" {
			t.Errorf("min_severity=17 returned a %q log", l.Severity)
		}
	}

	var series store.Series
	getJSON(t, ts.URL+"/api/series?buckets=20", &series)
	var requests float64
	for _, v := range series.Requests {
		requests += v
	}
	// A request is a call arriving at a service: the 200 traces, and the
	// payments service's side of each checkout. The endpoints say the same.
	var eps store.Endpoints
	getJSON(t, ts.URL+"/api/endpoints", &eps)
	var listed float64
	for _, e := range eps.Rows {
		listed += float64(e.Requests)
	}
	if len(series.T) != 20 || requests <= 200 || requests != listed {
		t.Errorf("series has %d buckets and %v requests; the endpoints have %v, and there are 200 traces", len(series.T), requests, listed)
	}
}

// OTLP/JSON writes ids as hex, where protobuf's JSON would use base64.
func TestJSONIngestKeepsHexIDs(t *testing.T) {
	ts := newTestServer(t)
	const traceID = "5b8efff798038103d269b633813fc60c"
	body := `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"acme-web"}}]},
		"scopeSpans":[{"spans":[{"traceId":"` + traceID + `","spanId":"eee19b7ec3c1b174","name":"GET /",
		"kind":2,"startTimeUnixNano":"1700000000000000000","endTimeUnixNano":"1700000000050000000"}]}]}]}`
	resp := post(t, ts.URL+"/v1/traces", "application/json", []byte(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST: %s", resp.Status)
	}

	var detail struct {
		Spans []store.SpanRow `json:"spans"`
	}
	getJSON(t, ts.URL+"/api/traces/"+traceID, &detail)
	if len(detail.Spans) != 1 || detail.Spans[0].SpanID != "eee19b7ec3c1b174" {
		t.Fatalf("spans = %+v", detail.Spans)
	}
	if d := detail.Spans[0].DurationMs; d != 50 {
		t.Errorf("duration = %vms, want 50", d)
	}
}

func TestGuard(t *testing.T) {
	ts := newTestServer(t)
	get := func(host, origin string) int {
		req, _ := http.NewRequest("GET", ts.URL+"/api/health", nil)
		if host != "" {
			req.Host = host
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	cases := []struct {
		host, origin string
		want         int
	}{
		{"", "", 200},
		{"", "http://localhost:1520", 200},
		{"", "tauri://localhost", 200},
		{"", "https://example.com", 403},
		{"evil.example.com", "", 403}, // DNS rebinding: a foreign name pointed at 127.0.0.1
	}
	for _, c := range cases {
		if got := get(c.host, c.origin); got != c.want {
			t.Errorf("host %q origin %q: got %d, want %d", c.host, c.origin, got, c.want)
		}
	}
}

// A request that parses as JSON must also be answered as JSON.
func TestJSONResponse(t *testing.T) {
	ts := newTestServer(t)
	traces, _ := demo.New(2).Batch(1, time.Now().Add(-time.Minute), time.Now())
	body, _ := protojson.Marshal(traces)
	resp := post(t, ts.URL+"/v1/traces", "application/json", body)
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q", ct)
	}
}

// The app's page is on another origin than the daemon (the dev server, the
// desktop webview), so the browser asks first. PATCH and DELETE are allowed
// to the same origins as everything else, and to no others.
func TestGuardAllowsEditsFromTheAppOnly(t *testing.T) {
	ts := newTestServer(t)
	preflight := func(origin, method string) (int, string) {
		req, _ := http.NewRequest("OPTIONS", ts.URL+"/api/connections/c_1", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", method)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode, resp.Header.Get("Access-Control-Allow-Methods")
	}
	for _, origin := range []string{"http://localhost:1520", "tauri://localhost"} {
		for _, method := range []string{"PATCH", "DELETE"} {
			code, allowed := preflight(origin, method)
			if code != http.StatusNoContent || !strings.Contains(allowed, method) {
				t.Errorf("%s from %s: %d, allowed %q", method, origin, code, allowed)
			}
		}
	}
	if code, allowed := preflight("https://example.com", "DELETE"); code != http.StatusForbidden || allowed != "" {
		t.Errorf("DELETE from a foreign origin: %d, allowed %q", code, allowed)
	}
}

// A framework's route list arrives as a log record and is kept apart from
// the logs; a browser's records carry their kind and their user.
func TestRouteListAndKindsOfLog(t *testing.T) {
	ts := newTestServer(t)
	now := time.Now()
	traces, _ := demo.New(3).Batch(50, now.Add(-5*time.Minute), now)
	body, _ := proto.Marshal(traces)
	post(t, ts.URL+"/v1/traces", "application/x-protobuf", body)

	routes := `{"version":1,"framework":"next","next":"16.4.0","root":"/work/acme","routes":[` +
		`{"route":"/api/products","kind":"route","router":"app","file":"app/api/products/route.ts","methods":["GET"]},` +
		`{"route":"/api/refunds","kind":"route","router":"app","file":"app/api/refunds/route.ts","methods":["POST"]}]}`
	logs := fmt.Sprintf(`{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"acme-api"}}]},
"scopeLogs":[{"logRecords":[
 {"timeUnixNano":"%[1]d","severityNumber":5,"body":{"stringValue":%[2]q},"attributes":[{"key":"leadline.kind","value":{"stringValue":"next.routes"}}]},
 {"timeUnixNano":"%[1]d","severityNumber":17,"body":{"stringValue":"TypeError: x is undefined"},"attributes":[
   {"key":"leadline.kind","value":{"stringValue":"browser.error"}},{"key":"user.id","value":{"stringValue":"u_7"}}]},
 {"timeUnixNano":"%[1]d","severityNumber":9,"body":{"stringValue":"plain"}}]}]}]}`, now.UnixNano(), routes)
	if resp := post(t, ts.URL+"/v1/logs", "application/json", []byte(logs)); resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/logs: %s", resp.Status)
	}

	var all, errors, mine []store.LogRow
	getJSON(t, ts.URL+"/api/logs", &all)
	getJSON(t, ts.URL+"/api/logs?kind=browser.error", &errors)
	getJSON(t, ts.URL+"/api/logs?user=u_7", &mine)
	if len(all) != 2 {
		t.Errorf("%d logs, want the two that are logs and not the route list", len(all))
	}
	if len(errors) != 1 || errors[0].Kind != "browser.error" || errors[0].UserID != "u_7" || len(mine) != 1 {
		t.Errorf("by kind: %+v; by user: %+v", errors, mine)
	}

	var got struct {
		Lists []store.RouteList `json:"lists"`
	}
	getJSON(t, ts.URL+"/api/routes?service=acme-api", &got)
	if len(got.Lists) != 1 || got.Lists[0].Framework != "next" || len(got.Lists[0].Routes) != 2 {
		t.Fatalf("routes = %+v", got)
	}
	if r := got.Lists[0].Routes; r[0].Requests == 0 || r[1].Requests != 0 {
		t.Errorf("requests per route = %+v, want some for /api/products and none for /api/refunds", r)
	}
	var eps store.Endpoints
	getJSON(t, ts.URL+"/api/endpoints", &eps)
	if len(eps.NeverHit) != 1 || eps.NeverHit[0].Route != "/api/refunds" || eps.NeverHit[0].File != "app/api/refunds/route.ts" {
		t.Errorf("never hit = %+v, want /api/refunds", eps.NeverHit)
	}
}
