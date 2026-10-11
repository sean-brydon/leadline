// One record per incoming request, built up from the spans that start and end
// inside it. When the request's own span ends, the record says what the
// request was in Next.js's terms, and the span is named after that.
import { createHash } from "node:crypto";
import { SpanKind, trace } from "@opentelemetry/api";
import type { Attributes, Context, HrTime } from "@opentelemetry/api";
import type { LogRecordProcessor, SdkLogRecord } from "@opentelemetry/sdk-logs";
import type { ReadableSpan, Span, SpanProcessor } from "@opentelemetry/sdk-trace";
import { classify, covered, mergeIntervals, pathOf } from "./classify.cjs";
import type { ActionInfo, Classified, RequestHints, Seen } from "./classify.cjs";
import type { LeadlineUser, RouteEntry } from "./types.cjs";

const HTTP_SCOPE = "@opentelemetry/instrumentation-http";
const HANDLE_REQUEST = "BaseServer.handleRequest";
const RENDER_TYPES = new Set(["AppRender.getBodyResult", "Render.renderDocument"]);
/**
 * Where the dev server itself calls when it starts: its telemetry, and the
 * npm registry for its version and advisory checks. Only a call with no
 * request around it is taken for one of those.
 */
const NEXT_HOSTS = new Set(["telemetry.nextjs.org", "registry.npmjs.org"]);

/** How many finished requests are remembered for the browser's report of them. */
const REMEMBER_MAX = 500;

interface Request {
  root: Span;
  hints: RequestHints;
  seen: Seen;
  user?: Attributes;
  /** `identify()` beats what was read from a session cookie. */
  userFrom?: "identify" | "cookie";
  /** The spans of this request that have not ended. */
  live: Set<Span>;
  /** Every span id of this request, to forget them together. */
  ids: string[];
  /** When the request waited on something outside: queries and fetches. */
  data: Array<[number, number]>;
  render?: [number, number];
  cookie?: string;
  /** "traceId:spanId" of the browser span that caused this request. */
  cause?: string;
  waiters: Array<() => void>;
}

/** What the browser's report of a request is told about it. */
export interface Outcome extends Classified {
  user?: Attributes;
}

export interface RequestsOptions {
  /** Looks up a server action's id. */
  action?: (id: string) => ActionInfo | undefined;
  /** The current route list, to say which file answered. */
  routes?: () => readonly RouteEntry[];
  /** Called as each request ends. */
  onRequest?: () => void;
}

export class Requests {
  private readonly bySpan = new Map<string, Request>();
  /** Requests still running that a browser span is waiting to hear about. */
  private readonly open = new Map<string, Request>();
  private readonly done = new Map<string, Outcome>();
  /** Spans that had a child, by identity. */
  private readonly parents = new WeakSet<object>();
  /** Spans of the dev server's own calls home, and everything under them. */
  private readonly noise = new WeakSet<object>();
  /** Who a browser is, by a digest of the cookies it sends. */
  private readonly users = new Map<string, Attributes>();

  constructor(private readonly options: RequestsOptions = {}) {}

  readonly spans: SpanProcessor = {
    onStart: (span, parentContext) => this.started(span, parentContext),
    onEnding: (span) => this.ending(span),
    onEnd: (span) => this.ended(span),
    forceFlush: async () => {},
    shutdown: async () => {},
  };

  readonly logs: LogRecordProcessor = {
    // A log line written during a request says who the request was for.
    onEmit: (record: SdkLogRecord) => {
      const id = record.spanContext?.spanId;
      const user = id ? this.bySpan.get(id)?.user : undefined;
      if (user) record.setAttributes(user);
    },
    forceFlush: async () => {},
    shutdown: async () => {},
  };

  /**
   * False for spans that say nothing about the app: the dev server's
   * bookkeeping around a request, and its own calls home.
   */
  readonly keep = (span: ReadableSpan): boolean => {
    if (this.noise.has(span)) return false;
    const type = span.attributes["next.span_type"];
    // With a proxy.ts, Next.js runs its request handler twice: once to run
    // the proxy and once for the route. The first pass is a "GET" span of a
    // millisecond with nothing in it; the proxy's own span tells that story.
    if (
      type === HANDLE_REQUEST &&
      span.attributes["next.bubble"] === true &&
      !span.attributes["next.route"] &&
      !this.parents.has(span)
    ) {
      return false;
    }
    if (!span.parentSpanContext) {
      if (type === "Instrumentation.loadModule" || type === "Instrumentation.register") return false;
      if (span.kind === SpanKind.CLIENT && NEXT_HOSTS.has(String(span.attributes["server.address"]))) return false;
    }
    return true;
  };

  /** The http instrumentation's request hook: what the request said about itself. */
  incoming(
    span: { spanContext(): { traceId: string; spanId: string }; updateName(name: string): unknown },
    request: { method?: string; url?: string; headers: Record<string, string | string[] | undefined> },
    hints: RequestHints,
  ): void {
    const { traceId, spanId } = span.spanContext();
    const req = this.bySpan.get(spanId);
    if (!req) return;
    req.hints = hints;
    req.cookie = typeof request.headers.cookie === "string" ? request.headers.cookie : undefined;
    // Until the route is known, the path says which request this is.
    span.updateName(`${hints.method} ${hints.path}`);
    // Next.js starts the proxy's span from the request's `traceparent`
    // header, not from the span that is active, so without one the proxy
    // lands in a trace of its own. Naming this request's span as the parent
    // puts it back inside the request.
    request.headers.traceparent = `00-${traceId}-${spanId}-01`;
  }

  /** Attaches a user to the request that is running now. */
  identify(user: LeadlineUser): void {
    const attributes = userAttributes(user);
    const span = trace.getActiveSpan();
    const req = span && this.bySpan.get(span.spanContext().spanId);
    if (attributes && req) this.setUser(req, attributes, "identify");
  }

  /** Attaches a user read from a session cookie, unless `identify()` has spoken. */
  identifyFromCookie(spanId: string, user: LeadlineUser): void {
    const attributes = userAttributes(user);
    const req = this.bySpan.get(spanId);
    if (attributes && req && req.userFrom !== "identify") this.setUser(req, attributes, "cookie");
  }

  /** Who the browser sending these cookies was last identified as. */
  userForCookie(cookie: string | undefined): Attributes | undefined {
    return cookie ? this.users.get(digest(cookie)) : undefined;
  }

  /**
   * What became of the request a browser span caused, waiting a little if it
   * is still running. Undefined when no such request was seen.
   */
  async outcomeFor(traceId: string, spanId: string, waitMs = 5000): Promise<Outcome | undefined> {
    const key = `${traceId}:${spanId}`;
    const req = this.open.get(key);
    if (req) {
      await new Promise<void>((resolve) => {
        const timer = setTimeout(resolve, waitMs);
        timer.unref?.();
        req.waiters.push(() => {
          clearTimeout(timer);
          resolve();
        });
      });
    }
    return this.done.get(key);
  }

  private setUser(req: Request, attributes: Attributes, from: "identify" | "cookie"): void {
    req.user = attributes;
    req.userFrom = from;
    for (const span of req.live) {
      if (span.isRecording()) span.setAttributes(attributes);
    }
    if (req.cookie) {
      // Not the cookies: a digest of them, to recognise the same browser when
      // it reports an error a moment later.
      const key = digest(req.cookie);
      this.users.delete(key);
      this.users.set(key, attributes);
      if (this.users.size > 200) this.users.delete(this.users.keys().next().value!);
    }
  }

  private started(span: Span, parentContext: Context): void {
    const { traceId, spanId } = span.spanContext();
    const parent = span.parentSpanContext;
    const parentSpan = trace.getSpan(parentContext);
    if (parentSpan) {
      this.parents.add(parentSpan);
      if (this.noise.has(parentSpan)) this.noise.add(span);
    } else if (!parent && span.kind === SpanKind.CLIENT) {
      const host = span.attributes["server.address"] ?? span.attributes["net.peer.name"];
      if (NEXT_HOSTS.has(String(host))) this.noise.add(span);
    }

    if (span.kind === SpanKind.SERVER && span.instrumentationScope.name === HTTP_SCOPE) {
      const req: Request = {
        root: span,
        hints: {
          method: String(span.attributes["http.request.method"] ?? "GET"),
          path: pathOf(String(span.attributes["url.path"] ?? "/")),
          rsc: false,
          prefetch: false,
        },
        seen: { types: new Set() },
        live: new Set([span]),
        ids: [spanId],
        data: [],
        waiters: [],
      };
      this.bySpan.set(spanId, req);
      if (parent?.isRemote) {
        req.cause = `${traceId}:${parent.spanId}`;
        this.open.set(req.cause, req);
      }
      return;
    }

    const req = parent && this.bySpan.get(parent.spanId);
    if (!req) return;
    this.bySpan.set(spanId, req);
    req.ids.push(spanId);
    req.live.add(span);
    if (req.user) span.setAttributes(req.user);
    // Next.js marks its request span as a server span too. Under the http
    // span there would be two "requests" for every request; the outer one is
    // the request, and this one is a step of it.
    if (span.kind === SpanKind.SERVER && span.attributes["next.span_type"] === HANDLE_REQUEST) {
      (span as { kind: SpanKind }).kind = SpanKind.INTERNAL;
    }
  }

  private ending(span: Span): void {
    const req = this.bySpan.get(span.spanContext().spanId);
    if (!req) return;
    if (span === req.root) return this.finish(req);

    const type = span.attributes["next.span_type"];
    if (typeof type !== "string") return;
    if (type === "Middleware.execute") {
      // Next.js still calls the span "middleware"; since Next 16 the file is proxy.ts.
      const file = this.options.routes?.().find((entry) => entry.kind === "proxy")?.file;
      span.setAttribute("next.kind", file && !/(^|\/)proxy\./.test(file) ? "middleware" : "proxy");
      if (file) span.setAttribute("next.file", file);
    }
    const bubbled = type === HANDLE_REQUEST && span.attributes["next.bubble"] === true;
    if (!bubbled) req.seen.types.add(type);
    const route = span.attributes["next.route"];
    if (typeof route === "string" && route && (type === HANDLE_REQUEST || !req.seen.route)) {
      // The request span's is the last word: it is what Next.js settled on.
      req.seen.route = route;
    }
  }

  private ended(span: ReadableSpan): void {
    const req = this.bySpan.get(span.spanContext().spanId);
    if (!req) return;
    req.live.delete(span as unknown as Span);
    if (span === (req.root as unknown as ReadableSpan)) {
      for (const id of req.ids) this.bySpan.delete(id);
      return;
    }
    const interval: [number, number] = [ms(span.startTime), ms(span.endTime)];
    if (span.kind === SpanKind.CLIENT) req.data.push(interval);
    else if (RENDER_TYPES.has(String(span.attributes["next.span_type"]))) req.render = interval;
  }

  /** The request's own span is about to end: say what the request was. */
  private finish(req: Request): void {
    const { root, hints } = req;
    const status = Number(root.attributes["http.response.status_code"] ?? 0);
    const action = hints.actionId ? this.options.action?.(hints.actionId) : undefined;
    const outcome: Outcome = classify(hints, req.seen, status, action);

    root.updateName(outcome.name);
    root.setAttribute("http.route", outcome.route);
    root.setAttributes(outcome.attributes);
    const file = this.fileFor(outcome);
    if (file) root.setAttribute("next.file", file);

    // Time spent waiting on queries and fetches, overlaps counted once, and
    // what is left of the render when that is taken out. Rounded to the
    // microsecond: the clocks are no better than that.
    const data = mergeIntervals(req.data);
    const start = ms(root.startTime);
    const dataMs = covered(data, start, Infinity);
    if (data.length > 0) root.setAttribute("leadline.data_ms", round(dataMs));
    if (req.render) {
      const [from, to] = req.render;
      root.setAttribute("leadline.render_ms", round(Math.max(0, to - from - covered(data, from, to))));
    }

    if (req.user) outcome.user = req.user;
    if (req.cause) {
      this.open.delete(req.cause);
      this.remember(req.cause, outcome);
    }
    for (const wake of req.waiters) wake();
    try {
      this.options.onRequest?.();
    } catch {
      // Whatever is listening, the request's span still has to end.
    }
  }

  private fileFor(outcome: Classified): string | undefined {
    const routes = this.options.routes?.();
    if (!routes) return undefined;
    const kinds: RouteEntry["kind"][] =
      outcome.kind === "route" ? ["route"] : outcome.kind === "api" ? ["api"] : ["page"];
    if (outcome.kind !== "page" && outcome.kind !== "route" && outcome.kind !== "api" && outcome.kind !== "action") {
      return undefined;
    }
    return routes.find((entry) => entry.route === outcome.route && kinds.includes(entry.kind))?.file;
  }

  private remember(key: string, outcome: Outcome): void {
    this.done.set(key, outcome);
    // A Map keeps the order things went in, so the first keys are the oldest.
    if (this.done.size > REMEMBER_MAX) {
      for (const oldest of this.done.keys()) {
        this.done.delete(oldest);
        if (this.done.size <= REMEMBER_MAX / 2) break;
      }
    }
  }
}

/**
 * A user as span attributes, by OpenTelemetry's names. Only an id, an email
 * and a name are ever taken, whatever else the object holds.
 */
export function userAttributes(user: unknown): Attributes | undefined {
  if (!user || typeof user !== "object") return undefined;
  const { id, email, name } = user as Record<string, unknown>;
  if ((typeof id !== "string" && typeof id !== "number") || String(id) === "") return undefined;
  const attributes: Attributes = { "user.id": String(id).slice(0, 256) };
  if (typeof email === "string" && email) attributes["user.email"] = email.slice(0, 320);
  if (typeof name === "string" && name) attributes["user.full_name"] = name.slice(0, 256);
  return attributes;
}

function digest(text: string): string {
  return createHash("sha256").update(text).digest("base64");
}

function ms(time: HrTime): number {
  return time[0] * 1e3 + time[1] / 1e6;
}

function round(value: number): number {
  return Math.round(value * 1e3) / 1e3;
}
