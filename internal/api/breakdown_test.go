package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/sean-brydon/leadline/internal/demo"
	"github.com/sean-brydon/leadline/internal/store"
)

// demoServer is a daemon holding 300 demo traces from the last ten minutes.
func demoServer(t *testing.T) string {
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
	now := time.Now()
	traces, _ := demo.New(3).Batch(300, now.Add(-10*time.Minute), now.Add(-time.Second))
	body, _ := proto.Marshal(traces)
	resp, err := http.Post(ts.URL+"/v1/traces", "application/x-protobuf", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/traces: %s", resp.Status)
	}
	return ts.URL
}

// fetch decodes a GET's JSON into into and returns the status and raw body.
func fetch(t *testing.T, base, path string, params url.Values, into any) (int, string) {
	t.Helper()
	resp, err := http.Get(base + path + "?" + params.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw bytes.Buffer
	raw.ReadFrom(resp.Body)
	if resp.StatusCode == http.StatusOK && into != nil {
		if err := json.Unmarshal(raw.Bytes(), into); err != nil {
			t.Fatalf("GET %s: %v in %s", path, err, raw.String())
		}
	}
	return resp.StatusCode, raw.String()
}

func TestEndpointsAPI(t *testing.T) {
	base := demoServer(t)

	var list store.Endpoints
	if code, body := fetch(t, base, "/api/endpoints", url.Values{"spark": {"12"}, "sort": {"queries"}}, &list); code != 200 {
		t.Fatalf("endpoints: %d %s", code, body)
	}
	if len(list.Rows) != 6 || len(list.T) != 12 || len(list.Rows[0].Spark) != 12 {
		t.Fatalf("endpoints = %+v", list)
	}
	// Sorted by queries per request, the N+1 leads.
	orders := list.Rows[0]
	if orders.Name != "GET /api/orders" || orders.Queries < 7 {
		t.Fatalf("first by queries = %+v, want the order list", orders)
	}
	var requests int64
	for _, e := range list.Rows {
		requests += e.Requests
	}
	// 300 traces, and every checkout also arrives at the payments service.
	var checkouts int64
	for _, e := range list.Rows {
		if e.Name == "POST /charge" {
			checkouts = e.Requests
		}
	}
	if requests != 300+checkouts || checkouts == 0 {
		t.Errorf("%d requests with %d charges, want 300 and the charges", requests, checkouts)
	}

	var detail store.EndpointDetail
	params := url.Values{"endpoint_service": {orders.Service}, "name": {orders.Name}, "buckets": {"20"}}
	if code, body := fetch(t, base, "/api/endpoints/detail", params, &detail); code != 200 {
		t.Fatalf("endpoint detail: %d %s", code, body)
	}
	if detail.Stats == nil || detail.Stats.Requests != orders.Requests || len(detail.Series.T) != 20 {
		t.Fatalf("detail = %+v", detail)
	}
	var items *store.EndpointQueryRow
	for i, q := range detail.Queries {
		if strings.Contains(q.Fingerprint, "FROM order_items") {
			items = &detail.Queries[i]
		}
	}
	if items == nil || items.PerRequest < 6 {
		t.Fatalf("order_items in the order list = %+v, want an N+1", items)
	}
	if len(detail.Slowest) == 0 || detail.Slowest[0].DurationMs != detail.Stats.MaxMs {
		t.Errorf("slowest = %+v, max = %v", detail.Slowest, detail.Stats.MaxMs)
	}

	// From the query's side: the same numbers, and the trace list opens.
	var query store.QueryDetail
	params = url.Values{"db_system": {items.DBSystem}, "fingerprint": {items.Fingerprint}}
	if code, body := fetch(t, base, "/api/queries/detail", params, &query); code != 200 {
		t.Fatalf("query detail: %d %s", code, body)
	}
	if query.Stats == nil || query.Stats.Calls != items.Calls || len(query.Endpoints) != 1 ||
		query.Endpoints[0].Name != orders.Name || query.Endpoints[0].PerRequest != items.PerRequest {
		t.Fatalf("query detail = %+v %+v, want %d calls from the order list", query.Stats, query.Endpoints, items.Calls)
	}
	var trace struct {
		Spans []store.SpanRow `json:"spans"`
	}
	if code, _ := fetch(t, base, "/api/traces/"+query.Traces[0].TraceID, nil, &trace); code != 200 || len(trace.Spans) == 0 {
		t.Errorf("the slowest trace doesn't open: %d", code)
	}

	// The service filter narrows every breakdown.
	var worker store.Endpoints
	fetch(t, base, "/api/endpoints", url.Values{"service": {"acme-worker"}}, &worker)
	if len(worker.Rows) != 1 || worker.Rows[0].Name != "job send-receipt" {
		t.Errorf("acme-worker's endpoints = %+v", worker.Rows)
	}

	// A window that ends before the data holds nothing, and says so in
	// empty lists, not nulls.
	old := url.Values{"since_ms": {"1000"}, "until_ms": {"2000"}, "endpoint_service": {orders.Service}, "name": {orders.Name}}
	_, body := fetch(t, base, "/api/endpoints/detail", old, nil)
	for _, want := range []string{`"stats":null`, `"queries":[]`, `"slowest":[]`, `"failing":[]`, `"bins":[]`} {
		if !strings.Contains(body, want) {
			t.Errorf("empty detail lacks %s: %s", want, body)
		}
	}
}

func TestHeatmapAndBreakdownAPI(t *testing.T) {
	base := demoServer(t)

	var heat store.Heatmap
	if code, body := fetch(t, base, "/api/heatmap", url.Values{"buckets": {"30"}}, &heat); code != 200 {
		t.Fatalf("heatmap: %d %s", code, body)
	}
	var list store.Endpoints
	fetch(t, base, "/api/endpoints", nil, &list)
	var requests, cells int64
	for _, e := range list.Rows {
		requests += e.Requests
	}
	for _, n := range heat.N {
		cells += n
	}
	if cells != requests || len(heat.T) != 30 || heat.Max == 0 {
		t.Errorf("heatmap holds %d requests in %d columns, endpoints have %d", cells, len(heat.T), requests)
	}

	var grouped store.GroupedSeries
	if code, body := fetch(t, base, "/api/breakdown", url.Values{"by": {"db_calls"}, "buckets": {"15"}}, &grouped); code != 200 {
		t.Fatalf("breakdown: %d %s", code, body)
	}
	if len(grouped.Groups) != 3 || grouped.Groups[0].Key != "postgresql" || len(grouped.Groups[0].Values) != 15 {
		t.Errorf("groups = %+v", grouped.Groups)
	}

	for path, params := range map[string]url.Values{
		"/api/breakdown":        {"by": {"duration_ns; DROP TABLE spans"}},
		"/api/endpoints":        {"since_ms": {"2000"}, "until_ms": {"1000"}},
		"/api/endpoints/detail": {},
		"/api/queries/detail":   {},
	} {
		if code, _ := fetch(t, base, path, params, nil); code != http.StatusBadRequest {
			t.Errorf("GET %s %v: %d, want 400", path, params, code)
		}
	}
}
