// Package ingest turns OTLP requests into rows for the store.
package ingest

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/sean-brydon/leadline/internal/store"
)

// Spans flattens a trace export. jsonIDs says the request came as OTLP/JSON,
// see id.
func Spans(req *coltrace.ExportTraceServiceRequest, jsonIDs bool) []store.Span {
	var out []store.Span
	for _, rs := range req.GetResourceSpans() {
		service := serviceName(rs.GetResource())
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				attrs := attrMap(sp.GetAttributes())
				row := store.Span{
					TraceID:       id(sp.GetTraceId(), jsonIDs),
					SpanID:        id(sp.GetSpanId(), jsonIDs),
					ParentID:      id(sp.GetParentSpanId(), jsonIDs),
					Service:       service,
					Name:          sp.GetName(),
					Kind:          spanKind(sp.GetKind()),
					StartNs:       int64(sp.GetStartTimeUnixNano()),
					DurationNs:    int64(sp.GetEndTimeUnixNano()) - int64(sp.GetStartTimeUnixNano()),
					Status:        spanStatus(sp.GetStatus()),
					StatusMessage: sp.GetStatus().GetMessage(),
					// Both the stable names and the ones they replaced: plenty
					// of instrumentation still sends the old ones.
					DBSystem:    str(attrs, "db.system.name", "db.system"),
					DBStatement: str(attrs, "db.query.text", "db.statement"),
					HTTPMethod:  str(attrs, "http.request.method", "http.method"),
					HTTPRoute:   str(attrs, "http.route", "url.path", "http.target"),
					HTTPStatus:  int32(num(attrs, "http.response.status_code", "http.status_code")),
					Attrs:       toJSON(attrs),
					UserID:      str(attrs, "user.id", "enduser.id"),
				}
				if row.DurationNs < 0 {
					row.DurationNs = 0
				}
				// A span a page made: the request it led to is the server's.
				if str(attrs, "leadline.source") == store.SourceBrowser {
					row.Source = store.SourceBrowser
				}
				if row.DBSystem != "" {
					// Redis, Mongo and friends often carry no statement; the
					// span name ("GET", "users.find") is the next best shape.
					if row.DBStatement == "" {
						row.DBStatement = row.Name
					}
					if redisLike(row.DBSystem) {
						row.DBFingerprint = RedisFingerprint(strings.Fields(row.DBStatement))
					} else {
						row.DBFingerprint = Fingerprint(row.DBStatement)
						row.DBName = str(attrs, "db.namespace", "db.name")
					}
					row.DBServer = store.DBServer(str(attrs, "server.address", "net.peer.name", "network.peer.address"),
						num(attrs, "server.port", "net.peer.port", "network.peer.port"))
				}
				out = append(out, row)
			}
		}
	}
	return out
}

func redisLike(system string) bool {
	switch system {
	case "redis", "valkey", "keydb", "dragonfly":
		return true
	}
	return false
}

// kindKey is the attribute a provider uses to say what sort of record a
// log is. A kind ending in ".routes" is not a log at all; see RouteLists.
const kindKey = "leadline.kind"

func isRouteList(kind string) bool { return strings.HasSuffix(kind, ".routes") }

// RouteList is an app's list of routes, as a framework's plugin sent it.
type RouteList struct {
	Service string
	TsNs    int64
	Body    string // JSON; store.SetRoutes reads it
}

// RouteLists picks out the records of a log export that carry a route list
// (leadline.kind "next.routes"). They go to store.SetRoutes; Logs leaves
// them out.
func RouteLists(req *collogs.ExportLogsServiceRequest) []RouteList {
	var out []RouteList
	for _, rl := range req.GetResourceLogs() {
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				for _, kv := range lr.GetAttributes() {
					if kv.GetKey() != kindKey || !isRouteList(kv.GetValue().GetStringValue()) {
						continue
					}
					ts := lr.GetTimeUnixNano()
					if ts == 0 {
						ts = lr.GetObservedTimeUnixNano()
					}
					out = append(out, RouteList{Service: serviceName(rl.GetResource()), TsNs: int64(ts), Body: bodyText(lr.GetBody())})
					break
				}
			}
		}
	}
	return out
}

// Logs flattens a log export.
func Logs(req *collogs.ExportLogsServiceRequest, jsonIDs bool) []store.Log {
	var out []store.Log
	for _, rl := range req.GetResourceLogs() {
		service := serviceName(rl.GetResource())
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				ts := lr.GetTimeUnixNano()
				if ts == 0 {
					ts = lr.GetObservedTimeUnixNano()
				}
				attrs := attrMap(lr.GetAttributes())
				kind := str(attrs, kindKey)
				if isRouteList(kind) {
					continue
				}
				num, text := severity(int32(lr.GetSeverityNumber()), lr.GetSeverityText())
				out = append(out, store.Log{
					TsNs:        int64(ts),
					Service:     service,
					Severity:    text,
					SeverityNum: num,
					Body:        bodyText(lr.GetBody()),
					TraceID:     id(lr.GetTraceId(), jsonIDs),
					SpanID:      id(lr.GetSpanId(), jsonIDs),
					Attrs:       toJSON(attrs),
					Kind:        kind,
					UserID:      str(attrs, "user.id", "enduser.id"),
				})
			}
		}
	}
	return out
}

// id renders a trace or span id as hex. OTLP/JSON already writes ids as hex,
// but the protobuf JSON decoder reads every bytes field as base64, so an id
// from JSON arrives base64-decoded: encoding it back gives the hex it was.
func id(b []byte, fromJSON bool) string {
	if len(b) == 0 {
		return ""
	}
	if fromJSON {
		return strings.ToLower(base64.StdEncoding.EncodeToString(b))
	}
	return hex.EncodeToString(b)
}

func serviceName(r *resourcepb.Resource) string {
	for _, kv := range r.GetAttributes() {
		if kv.GetKey() == "service.name" {
			if s := kv.GetValue().GetStringValue(); s != "" {
				return s
			}
		}
	}
	return "unknown"
}

func spanKind(k tracepb.Span_SpanKind) string {
	switch k {
	case tracepb.Span_SPAN_KIND_SERVER:
		return "server"
	case tracepb.Span_SPAN_KIND_CLIENT:
		return "client"
	case tracepb.Span_SPAN_KIND_PRODUCER:
		return "producer"
	case tracepb.Span_SPAN_KIND_CONSUMER:
		return "consumer"
	}
	return "internal"
}

func spanStatus(s *tracepb.Status) string {
	switch s.GetCode() {
	case tracepb.Status_STATUS_CODE_OK:
		return "ok"
	case tracepb.Status_STATUS_CODE_ERROR:
		return "error"
	}
	return "unset"
}

// severity settles a log's level from its OTLP number, or failing that its
// text. A record with neither counts as info.
func severity(num int32, text string) (int32, string) {
	if num <= 0 {
		switch strings.ToLower(text) {
		case "trace":
			num = 1
		case "debug":
			num = 5
		case "warn", "warning":
			num = 13
		case "error", "err":
			num = 17
		case "fatal", "critical", "panic":
			num = 21
		default:
			num = 9
		}
	}
	switch {
	case num <= 4:
		return num, "trace"
	case num <= 8:
		return num, "debug"
	case num <= 12:
		return num, "info"
	case num <= 16:
		return num, "warn"
	case num <= 20:
		return num, "error"
	}
	return num, "fatal"
}

func attrMap(kvs []*commonpb.KeyValue) map[string]any {
	m := make(map[string]any, len(kvs))
	for _, kv := range kvs {
		m[kv.GetKey()] = anyValue(kv.GetValue())
	}
	return m
}

func anyValue(v *commonpb.AnyValue) any {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_BoolValue:
		return x.BoolValue
	case *commonpb.AnyValue_IntValue:
		return x.IntValue
	case *commonpb.AnyValue_DoubleValue:
		return x.DoubleValue
	case *commonpb.AnyValue_BytesValue:
		return hex.EncodeToString(x.BytesValue)
	case *commonpb.AnyValue_ArrayValue:
		arr := make([]any, 0, len(x.ArrayValue.GetValues()))
		for _, e := range x.ArrayValue.GetValues() {
			arr = append(arr, anyValue(e))
		}
		return arr
	case *commonpb.AnyValue_KvlistValue:
		return attrMap(x.KvlistValue.GetValues())
	}
	return nil
}

func bodyText(v *commonpb.AnyValue) string {
	switch x := anyValue(v).(type) {
	case nil:
		return ""
	case string:
		return x
	default:
		return toJSON(x)
	}
}

func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// str returns the first of keys that holds a non-empty string.
func str(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func num(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		switch n := m[k].(type) {
		case int64:
			return n
		case float64:
			return int64(n)
		}
	}
	return 0
}
