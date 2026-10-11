package runner

import (
	"reflect"
	"sync"
	"testing"
	"time"
)

type captured struct {
	at     time.Time
	stream Stream
	entry  Entry
}

func captureAll(feed func(c *capture)) []captured {
	var mu sync.Mutex
	var out []captured
	c := newCapture(func(at time.Time, stream Stream, e Entry) {
		mu.Lock()
		out = append(out, captured{at, stream, e})
		mu.Unlock()
	})
	feed(c)
	c.Close()
	return out
}

func TestCaptureKeepsTheOrderOfLinesReadTogether(t *testing.T) {
	at := time.Unix(1_800_000_000, 0)
	got := captureAll(func(c *capture) {
		// One read, three lines, one moment.
		c.Offer(Terminal, at, []byte("first\nsecond\nthird\n"))
		// A clock that went back does not put a later line before them.
		c.Offer(Terminal, at.Add(-time.Second), []byte("fourth\n"))
	})
	var bodies []string
	for i, rec := range got {
		bodies = append(bodies, rec.entry.Body)
		if i > 0 && !rec.at.After(got[i-1].at) {
			t.Errorf("%q is not after %q: %v then %v", rec.entry.Body, got[i-1].entry.Body, got[i-1].at, rec.at)
		}
	}
	if !reflect.DeepEqual(bodies, []string{"first", "second", "third", "fourth"}) {
		t.Errorf("got %q", bodies)
	}
}

func TestCaptureGroupsPerStreamAndFlushesWhenQuiet(t *testing.T) {
	at := time.Now()
	var got []captured
	var mu sync.Mutex
	c := newCapture(func(at time.Time, stream Stream, e Entry) {
		mu.Lock()
		got = append(got, captured{at, stream, e})
		mu.Unlock()
	})
	// A stack trace on stderr, with a line of stdout arriving in the middle
	// of it: two streams, so the trace is not cut in two.
	c.Offer(Stderr, at, []byte("Error: boom\n    at a (a.js:1:1)\n"))
	c.Offer(Stdout, at, []byte("GET /boom 500\n"))
	c.Offer(Stderr, at, []byte("    at b (b.js:2:2)\n"))

	// Nothing more comes; the groups are handed over without a next line
	// to end them.
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d records after waiting; want 2 without Close", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.Close()
	byStream := map[Stream]Entry{}
	for _, rec := range got {
		byStream[rec.stream] = rec.entry
	}
	if e := byStream[Stderr]; e.Body != "Error: boom\n    at a (a.js:1:1)\n    at b (b.js:2:2)" || e.Severity != sevError {
		t.Errorf("stderr: %+v", e)
	}
	if e := byStream[Stdout]; e.Body != "GET /boom 500" || e.Severity != sevInfo {
		t.Errorf("stdout: %+v", e)
	}
}

func TestCaptureNeverHoldsTheCommandUp(t *testing.T) {
	// A sink that never returns: the pump's side must not notice.
	block := make(chan struct{})
	c := newCapture(func(time.Time, Stream, Entry) { <-block })
	done := make(chan struct{})
	go func() {
		for i := 0; i < 5000; i++ {
			c.Offer(Stdout, time.Now(), []byte("line\n"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Offer blocked on a stuck capture")
	}
	if c.dropped.Load() == 0 {
		t.Error("nothing was counted as dropped, though the queue cannot have held it all")
	}
	close(block)
	c.Close()
}

func TestRecordCarriesWhatAJSONLineHad(t *testing.T) {
	e := &exporter{service: "acme", runID: "run1"}
	entry := Classify([]string{`{"level":"warn","msg":"slow","took_ms":812,"ratio":0.5,"cached":true,"route":"/orders","trace_id":"0af7651916cd43dd8448eb211c80319c","span_id":"b7ad6b7169203331"}`}, Stdout)
	rec := e.record(time.Unix(1_800_000_000, 5), Stdout, entry)

	if rec.GetBody().GetStringValue() != "slow" || rec.GetSeverityText() != "WARN" || rec.GetSeverityNumber() != 13 {
		t.Errorf("record: %v", rec)
	}
	if rec.GetTimeUnixNano() != 1_800_000_000_000_000_005 {
		t.Errorf("time: %d", rec.GetTimeUnixNano())
	}
	if len(rec.GetTraceId()) != 16 || len(rec.GetSpanId()) != 8 {
		t.Errorf("ids: %x %x", rec.GetTraceId(), rec.GetSpanId())
	}
	attrs := map[string]any{}
	for _, kv := range rec.GetAttributes() {
		val := kv.GetValue()
		switch {
		case val.GetStringValue() != "":
			attrs[kv.GetKey()] = val.GetStringValue()
		case val.GetIntValue() != 0:
			attrs[kv.GetKey()] = val.GetIntValue()
		case val.GetDoubleValue() != 0:
			attrs[kv.GetKey()] = val.GetDoubleValue()
		default:
			attrs[kv.GetKey()] = val.GetBoolValue()
		}
	}
	want := map[string]any{
		SourceAttr: "run", "leadline.run.id": "run1", "log.iostream": "stdout",
		"took_ms": int64(812), "ratio": 0.5, "cached": true, "route": "/orders",
	}
	if !reflect.DeepEqual(attrs, want) {
		t.Errorf("attributes:\n got %v\nwant %v", attrs, want)
	}
}
