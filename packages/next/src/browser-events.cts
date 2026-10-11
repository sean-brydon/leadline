// What the browser reports, turned into OTLP. The page sends small plain
// events; everything Leadline needs to know about them (the service, the
// route pattern, the action's name, the user) is known here on the server
// and added here, which is what keeps the script in the page small.
import { keyValues, nanos } from "./send.cjs";
import type { AttributeValue, KeyValue } from "./send.cjs";

/** One event from the page. Field names are short: they are sent often. */
export interface BrowserEvent {
  /** What happened. */
  t: "error" | "console" | "nav" | "vital" | "fetch";
  /** When, in Unix milliseconds. For a fetch, when it started. */
  ts: number;
  /** The page's path when it happened. */
  p?: string;
  /** error, console: the message. */
  m?: string;
  /** error: the stack. */
  st?: string;
  /** error: the error's class name. */
  n?: string;
  /** error: "rejection" for an unhandled promise rejection. console: "warn" or "error". */
  k?: string;
  /** nav: where from, and how ("load", "push", "replace", "pop"). */
  from?: string;
  how?: string;
  /** vital: its name, value and rating. */
  name?: string;
  v?: number;
  r?: string;
  /** fetch */
  method?: string;
  /** The path and query for this origin, the whole URL for another. */
  url?: string;
  status?: number;
  /** Milliseconds until the response had arrived. */
  dur?: number;
  /** The network error, when there was no response. */
  err?: string;
  /** The trace and span ids sent in `traceparent`. */
  tid?: string;
  sid?: string;
  /** Slower than the page's threshold. */
  slow?: boolean;
}

export interface BrowserBatch {
  /** A random id for this page load. */
  s?: string;
  e?: BrowserEvent[];
}

/** What the server knows about the request a browser fetch caused. */
export interface Caused {
  name: string;
  route: string;
  attributes: Record<string, AttributeValue>;
  user?: Record<string, AttributeValue>;
}

export interface OtlpSpan {
  traceId: string;
  spanId: string;
  name: string;
  kind: number;
  startTimeUnixNano: string;
  endTimeUnixNano: string;
  attributes: KeyValue[];
  status: { code: number; message?: string };
}

export interface OtlpLog {
  timeUnixNano: string;
  severityNumber: number;
  severityText: string;
  body: { stringValue: string };
  attributes: KeyValue[];
  traceId?: string;
  spanId?: string;
}

export interface Converted {
  spans: OtlpSpan[];
  logs: OtlpLog[];
}

export interface ConvertContext {
  /** What became of the request with this trace and parent span id, if the server saw it. */
  caused?: (traceId: string, spanId: string) => Caused | undefined;
  /** The user this browser was last identified as. */
  user?: Record<string, AttributeValue>;
  userAgent?: string;
}

const MAX_EVENTS = 200;
const MAX_TEXT = 16 * 1024;
const SPAN_KIND_CLIENT = 3;
const STATUS_ERROR = 2;

const SEVERITY = { info: [9, "INFO"], warn: [13, "WARN"], error: [17, "ERROR"] } as const;

/** Reads a batch posted by the page. Anything malformed is dropped, not repaired. */
export function parseBatch(text: string): BrowserBatch | undefined {
  try {
    const value: unknown = JSON.parse(text);
    if (!value || typeof value !== "object") return undefined;
    const { s, e } = value as Record<string, unknown>;
    if (!Array.isArray(e)) return undefined;
    return {
      s: typeof s === "string" ? s.slice(0, 64) : undefined,
      e: e.filter((event): event is BrowserEvent => {
        if (!event || typeof event !== "object") return false;
        const { t, ts } = event as Record<string, unknown>;
        return typeof t === "string" && typeof ts === "number" && Number.isFinite(ts);
      }).slice(0, MAX_EVENTS),
    };
  } catch {
    return undefined;
  }
}

/** The trace and span ids of the fetches in a batch that carried a `traceparent`. */
export function tracedFetches(batch: BrowserBatch): Array<{ traceId: string; spanId: string }> {
  const out: Array<{ traceId: string; spanId: string }> = [];
  for (const event of batch.e ?? []) {
    if (event.t === "fetch" && isId(event.tid, 32) && isId(event.sid, 16)) {
      out.push({ traceId: event.tid, spanId: event.sid });
    }
  }
  return out;
}

export function convert(batch: BrowserBatch, ctx: ConvertContext = {}): Converted {
  const out: Converted = { spans: [], logs: [] };
  const common: Record<string, AttributeValue> = {
    "leadline.source": "browser",
    "session.id": batch.s,
    "user_agent.original": ctx.userAgent?.slice(0, 256),
  };

  const log = (
    event: BrowserEvent,
    level: keyof typeof SEVERITY,
    kind: string,
    body: string | undefined,
    attributes: Record<string, AttributeValue>,
    ids?: { traceId: string; spanId: string },
  ) => {
    out.logs.push({
      timeUnixNano: nanos(event.ts),
      severityNumber: SEVERITY[level][0],
      severityText: SEVERITY[level][1],
      body: { stringValue: text(body, MAX_TEXT) ?? "" },
      attributes: keyValues({
        ...common,
        "leadline.kind": kind,
        "url.path": text(event.p, 2048),
        ...ctx.user,
        ...attributes,
      }),
      ...ids,
    });
  };

  for (const event of batch.e ?? []) {
    switch (event.t) {
      case "error": {
        const what = event.k === "rejection" ? "Unhandled rejection" : "Uncaught exception";
        const message = text(event.m, MAX_TEXT) || "(no message)";
        // A stack already starts with "Name: message" in most browsers.
        log(event, "error", "browser.error", `${what}: ${event.st ? text(event.st, MAX_TEXT) : message}`, {
          "exception.type": text(event.n, 256),
          "exception.message": message,
          "exception.stacktrace": text(event.st, MAX_TEXT),
          "leadline.origin": event.k === "rejection" ? "unhandledRejection" : "uncaughtException",
        });
        break;
      }
      case "console": {
        const level = event.k === "warn" ? "warn" : "error";
        log(event, level, "browser.console", text(event.m, MAX_TEXT), {
          "exception.stacktrace": text(event.st, MAX_TEXT),
        });
        break;
      }
      case "nav": {
        const to = text(event.p, 2048) ?? "/";
        const from = text(event.from, 2048);
        const how = text(event.how, 16) ?? "push";
        log(event, "info", "browser.navigation", how === "load" ? `loaded ${to}` : `navigated to ${to}${from ? ` from ${from}` : ""}`, {
          "navigation.type": how,
          "navigation.from": from,
        });
        break;
      }
      case "vital": {
        const name = text(event.name, 16);
        if (!name || typeof event.v !== "number" || !Number.isFinite(event.v)) break;
        const rating = text(event.r, 32);
        // CLS is a score; the others are times.
        const value = name === "CLS" ? event.v.toFixed(3) : `${Math.round(event.v)} ms`;
        log(event, rating === "poor" ? "warn" : "info", "browser.web_vital", `${name} ${value}${rating ? ` (${rating})` : ""}`, {
          "web_vital.name": name,
          "web_vital.value": event.v,
          "web_vital.rating": rating,
        });
        break;
      }
      case "fetch": {
        const method = (text(event.method, 16) ?? "GET").toUpperCase();
        const url = text(event.url, 2048) ?? "/";
        const path = url.replace(/[?#].*$/, "");
        const status = typeof event.status === "number" ? event.status : 0;
        const dur = typeof event.dur === "number" && event.dur >= 0 ? event.dur : 0;
        const failed = Boolean(event.err) || status >= 400;
        const ids = isId(event.tid, 32) && isId(event.sid, 16) ? { traceId: event.tid, spanId: event.sid } : undefined;
        const caused = ids ? ctx.caused?.(ids.traceId, ids.spanId) : undefined;
        // "fetch", then the name the server gave the request it caused:
        // "fetch GET /api/orders/[id]", "fetch action addNote".
        const what = `fetch ${caused?.name ?? `${method} ${path}`}`;
        const attributes: Record<string, AttributeValue> = {
          "http.request.method": method,
          "http.response.status_code": status || undefined,
          "http.route": caused?.route,
          "url.path": url.startsWith("/") ? path : undefined,
          "url.full": url.startsWith("/") ? undefined : url,
          "error.type": event.err ? text(event.err, 256) : status >= 400 ? String(status) : undefined,
          // The page gave up on it, as the router does when a navigation is overtaken.
          "leadline.aborted": event.k === "aborted" ? true : undefined,
          ...caused?.attributes,
        };
        // Who the request was for is the server's to say. The browser's own
        // idea of the user is only used when the server saw no such request.
        const user = caused ? caused.user : ctx.user;
        if (ids) {
          out.spans.push({
            ...ids,
            name: what,
            kind: SPAN_KIND_CLIENT,
            startTimeUnixNano: nanos(event.ts),
            endTimeUnixNano: nanos(event.ts + dur),
            attributes: keyValues({
              ...common,
              "leadline.page": text(event.p, 2048),
              ...user,
              ...attributes,
            }),
            status: failed ? { code: STATUS_ERROR, ...(event.err ? { message: text(event.err, 256) } : {}) } : { code: 0 },
          });
        }
        if (failed || event.slow) {
          const outcome = event.err ? `failed: ${text(event.err, 256)}` : `${status}`;
          log(
            event,
            event.err || status >= 500 ? "error" : "warn",
            failed ? "browser.fetch_failed" : "browser.fetch_slow",
            `${failed ? "" : "slow "}${what} ${outcome} in ${Math.round(dur)} ms`,
            { ...attributes, "leadline.duration_ms": Math.round(dur) },
            ids,
          );
        }
        break;
      }
    }
  }
  return out;
}

/** One export request for the spans or the logs, under the app's own service name. */
export function envelope(kind: "spans" | "logs", service: string, items: unknown[]): unknown {
  const resource = {
    attributes: keyValues({ "service.name": service, "telemetry.sdk.language": "webjs", "telemetry.sdk.name": "@leadline/next" }),
  };
  const scope = { name: "@leadline/next/browser" };
  return kind === "spans"
    ? { resourceSpans: [{ resource, scopeSpans: [{ scope, spans: items }] }] }
    : { resourceLogs: [{ resource, scopeLogs: [{ scope, logRecords: items }] }] };
}

function isId(value: unknown, length: number): value is string {
  return typeof value === "string" && value.length === length && /^[0-9a-f]+$/.test(value) && !/^0+$/.test(value);
}

function text(value: unknown, max: number): string | undefined {
  if (typeof value !== "string" || value === "") return undefined;
  return value.length > max ? `${value.slice(0, max)}…` : value;
}
