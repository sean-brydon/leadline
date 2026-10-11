package runner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync/atomic"
	"time"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

const (
	exportEvery = 250 * time.Millisecond
	exportBatch = 512
	// With the daemon away, look for it again this often at most, and only
	// when there is something to send.
	retryAfter = 2 * time.Second
)

// exporter sends captured events to the daemon's /v1/logs as OTLP, in
// batches. With no daemon it drops them: nothing is kept for later, nothing
// is retried, nothing is printed.
type exporter struct {
	daemon  *Daemon
	service string
	runID   string
	// tags mark every record with the worktree the command ran in, when
	// it ran in one: on the resource, where OpenTelemetry puts what is true
	// of the whole process, and on the record, where Leadline keeps it.
	// Set before the first record and never changed.
	tags [][2]string

	queue   chan *logspb.LogRecord
	stop    chan struct{}
	done    chan struct{}
	dropped atomic.Int64
	sent    atomic.Int64

	// Only the exporter's goroutine touches these.
	down    bool
	lastTry time.Time
	// Called once, the first time the daemon takes a batch after it was
	// away when the run began.
	onUp func()
}

// newExporter starts an exporter. up says whether the daemon answered just
// now; when it did not, onUp is called if it later does.
func newExporter(d *Daemon, service, runID string, up bool, onUp func(), tags ...[2]string) *exporter {
	e := &exporter{
		daemon:  d,
		service: service,
		runID:   runID,
		tags:    tags,
		queue:   make(chan *logspb.LogRecord, 8192),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		down:    !up,
		onUp:    onUp,
	}
	if up {
		e.onUp = nil
	} else {
		e.lastTry = time.Now()
	}
	go e.run()
	return e
}

// Add queues one event. It never waits.
func (e *exporter) Add(at time.Time, stream Stream, entry Entry) {
	select {
	case e.queue <- e.record(at, stream, entry):
	default:
		e.dropped.Add(1)
	}
}

// Close sends what is queued, giving the daemon until the deadline, and
// stops.
func (e *exporter) Close(deadline time.Time) {
	close(e.stop)
	select {
	case <-e.done:
	case <-time.After(time.Until(deadline)):
	}
}

func (e *exporter) run() {
	defer close(e.done)
	defer func() { _ = recover() }()
	tick := time.NewTicker(exportEvery)
	defer tick.Stop()
	var batch []*logspb.LogRecord
	for {
		select {
		case rec := <-e.queue:
			batch = append(batch, rec)
			if len(batch) < exportBatch {
				continue
			}
		case <-tick.C:
		case <-e.stop:
			for {
				select {
				case rec := <-e.queue:
					batch = append(batch, rec)
					continue
				default:
				}
				break
			}
			// The run is over and nothing will look again: one try, even
			// if the daemon was away a moment ago.
			e.lastTry = time.Time{}
			e.send(batch)
			return
		}
		if len(batch) > 0 {
			e.send(batch)
			batch = nil
		}
	}
}

func (e *exporter) send(batch []*logspb.LogRecord) {
	if len(batch) == 0 {
		return
	}
	if e.down && time.Since(e.lastTry) < retryAfter {
		e.dropped.Add(int64(len(batch)))
		return
	}
	e.lastTry = time.Now()
	if err := e.post(batch); err != nil {
		e.down = true
		e.dropped.Add(int64(len(batch)))
		return
	}
	e.sent.Add(int64(len(batch)))
	e.down = false
	if e.onUp != nil {
		onUp := e.onUp
		e.onUp = nil
		go onUp()
	}
}

func (e *exporter) post(batch []*logspb.LogRecord) error {
	req := &collogs.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{{
			Resource: &resourcepb.Resource{Attributes: e.resource()},
			ScopeLogs: []*logspb.ScopeLogs{{
				Scope:      &commonpb.InstrumentationScope{Name: "leadline/run"},
				LogRecords: batch,
			}},
		}},
	}
	body, err := proto.Marshal(req)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, e.daemon.URL+"/v1/logs", bytes.NewReader(body))
	if err != nil {
		return err
	}
	hr.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := e.daemon.HTTP.Do(hr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/v1/logs: %s", resp.Status)
	}
	return nil
}

func (e *exporter) resource() []*commonpb.KeyValue {
	attrs := []*commonpb.KeyValue{stringAttr("service.name", e.service)}
	for _, kv := range e.tags {
		attrs = append(attrs, stringAttr(kv[0], kv[1]))
	}
	return attrs
}

// SourceAttr marks a log record as one `leadline run` read off a command's
// output, as opposed to one an app's own instrumentation sent.
const SourceAttr = "leadline.source"

func (e *exporter) record(at time.Time, stream Stream, entry Entry) *logspb.LogRecord {
	ts := uint64(at.UnixNano())
	rec := &logspb.LogRecord{
		TimeUnixNano:         ts,
		ObservedTimeUnixNano: ts,
		SeverityNumber:       logspb.SeverityNumber(entry.Severity),
		SeverityText:         severityText(entry.Severity),
		Body:                 &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: entry.Body}},
		TraceId:              entry.TraceID,
		SpanId:               entry.SpanID,
		Attributes: []*commonpb.KeyValue{
			stringAttr(SourceAttr, "run"),
			stringAttr("leadline.run.id", e.runID),
		},
	}
	if stream != Terminal {
		rec.Attributes = append(rec.Attributes, stringAttr("log.iostream", stream.String()))
	}
	for _, kv := range e.tags {
		rec.Attributes = append(rec.Attributes, stringAttr(kv[0], kv[1]))
	}
	keys := make([]string, 0, len(entry.Attrs))
	for key := range entry.Attrs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		kv := &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{}}
		switch v := entry.Attrs[key].(type) {
		case string:
			kv.Value.Value = &commonpb.AnyValue_StringValue{StringValue: v}
		case bool:
			kv.Value.Value = &commonpb.AnyValue_BoolValue{BoolValue: v}
		case int64:
			kv.Value.Value = &commonpb.AnyValue_IntValue{IntValue: v}
		case float64:
			kv.Value.Value = &commonpb.AnyValue_DoubleValue{DoubleValue: v}
		default:
			continue
		}
		rec.Attributes = append(rec.Attributes, kv)
	}
	return rec
}

func stringAttr(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
}
