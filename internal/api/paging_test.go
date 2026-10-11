package api

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/sean-brydon/leadline/internal/demo"
	"github.com/sean-brydon/leadline/internal/store"
)

// The lists page over HTTP as the app pages them: each request carries the
// cursor of the last row it holds, and together the pages are the list.
func TestListsPageByCursor(t *testing.T) {
	ts := newTestServer(t)
	now := time.Now()
	traces, logs := demo.New(7).Batch(300, now.Add(-10*time.Minute), now)
	for path, msg := range map[string]proto.Message{"/v1/traces": traces, "/v1/logs": logs} {
		body, _ := proto.Marshal(msg)
		if resp := post(t, ts.URL+path, "application/x-protobuf", body); resp.StatusCode != http.StatusOK {
			t.Fatalf("POST %s: %s", path, resp.Status)
		}
	}

	var allTraces []store.TraceRow
	getJSON(t, ts.URL+"/api/traces?limit=5000", &allTraces)
	if len(allTraces) != 300 {
		t.Fatalf("%d traces, want 300", len(allTraces))
	}
	var paged []string
	for before := ""; ; {
		var page []store.TraceRow
		getJSON(t, ts.URL+"/api/traces?limit=64&before="+url.QueryEscape(before), &page)
		if len(page) == 0 {
			break
		}
		for _, r := range page {
			paged = append(paged, r.TraceID)
		}
		if before = page[len(page)-1].Cursor; before == "" {
			t.Fatal("a trace row has no cursor")
		}
		if len(paged) > 2*len(allTraces) {
			t.Fatal("paging does not end")
		}
	}
	want := make([]string, len(allTraces))
	for i, r := range allTraces {
		want[i] = r.TraceID
	}
	if !slices.Equal(paged, want) {
		t.Errorf("traces in pages: %d rows, want the same %d in the same order", len(paged), len(want))
	}

	var allLogs []store.LogRow
	getJSON(t, ts.URL+"/api/logs?limit=5000", &allLogs)
	if len(allLogs) < 200 {
		t.Fatalf("only %d logs", len(allLogs))
	}
	var bodies []string
	for before := ""; ; {
		var page []store.LogRow
		getJSON(t, ts.URL+"/api/logs?limit=50&before="+url.QueryEscape(before), &page)
		if len(page) == 0 {
			break
		}
		for _, r := range page {
			bodies = append(bodies, fmt.Sprint(r.TsMs, r.Body, r.SpanID))
		}
		before = page[len(page)-1].Cursor
		if len(bodies) > 2*len(allLogs) {
			t.Fatal("paging does not end")
		}
	}
	wantLogs := make([]string, len(allLogs))
	for i, r := range allLogs {
		wantLogs[i] = fmt.Sprint(r.TsMs, r.Body, r.SpanID)
	}
	if !slices.Equal(bodies, wantLogs) {
		t.Errorf("logs in pages: %d rows, want the same %d in the same order", len(bodies), len(wantLogs))
	}

	// A trace's own logs are not a page of anything.
	var detail struct {
		Spans []store.SpanRow
		Logs  []map[string]any
	}
	getJSON(t, ts.URL+"/api/traces/"+allTraces[0].TraceID, &detail)
	if len(detail.Spans) == 0 {
		t.Fatal("the trace has no spans")
	}
	for _, l := range detail.Logs {
		if _, has := l["cursor"]; has {
			t.Errorf("a trace's log carries a cursor: %v", l)
		}
	}

	for _, path := range []string{"/api/traces?before=nonsense", "/api/logs?before=nonsense"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET %s: %s, want 400", path, resp.Status)
		}
	}
}

// Opening a trace finds the logs written during it by the time its spans
// cover, not by reading every log.
func TestTraceCarriesItsLogs(t *testing.T) {
	ts := newTestServer(t)
	now := time.Now()
	traces, logs := demo.New(3).Batch(200, now.Add(-30*time.Minute), now)
	for path, msg := range map[string]proto.Message{"/v1/traces": traces, "/v1/logs": logs} {
		body, _ := proto.Marshal(msg)
		post(t, ts.URL+path, "application/x-protobuf", body)
	}
	var allLogs []store.LogRow
	getJSON(t, ts.URL+"/api/logs?limit=5000", &allLogs)
	perTrace := map[string]int{}
	for _, l := range allLogs {
		if l.TraceID != "" {
			perTrace[l.TraceID]++
		}
	}
	if len(perTrace) < 100 {
		t.Fatalf("only %d traces have logs", len(perTrace))
	}
	for id, n := range perTrace {
		var detail struct{ Logs []store.LogRow }
		getJSON(t, ts.URL+"/api/traces/"+id, &detail)
		if len(detail.Logs) != n {
			t.Fatalf("trace %s opened with %d logs, want %d", id, len(detail.Logs), n)
		}
	}
}
