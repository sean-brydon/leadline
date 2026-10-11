// What runs in the page, in development only. It has no dependencies and
// knows nothing about Leadline beyond one path on the server it came from:
// it notes what happened, in small plain events, and posts them there once a
// second. The server names them, adds the user, and passes them on.

export interface BrowserOptions {
  /** A `fetch` that takes this long is reported as slow. Default: 1000 ms. */
  slowFetchMs?: number;
  /** Report `console.error` and `console.warn`. Default: true. */
  console?: boolean;
}

/** One event, as ./browser-events.cts reads it. */
type Event = Record<string, unknown>;

const RELAY = "/__leadline/b";
const FLUSH_MS = 1000;
const MAX_QUEUE = 200;
const MAX_TEXT = 8000;

/** Starts reporting. Safe to call on every render: it runs once per page load. */
export function start(options: BrowserOptions = {}): void {
  const w = window as Window & { __leadline?: boolean };
  if (w.__leadline) return;
  w.__leadline = true;
  try {
    run(options);
  } catch {
    // A development tool must not break the page it watches.
  }
}

function run(options: BrowserOptions): void {
  const session = hex(8);
  const nativeFetch = window.fetch.bind(window);
  const queue: Event[] = [];
  let timer: ReturnType<typeof setTimeout> | undefined;
  /** The server has no relay (no `withLeadline`, or it is switched off): stop. */
  let off = false;
  let quietUntil = 0;

  const push = (event: Event) => {
    if (off || queue.length >= MAX_QUEUE) return;
    queue.push({ ts: Date.now(), p: location.pathname, ...event });
    timer ??= setTimeout(flush, FLUSH_MS);
  };

  const flush = (leaving?: boolean) => {
    clearTimeout(timer);
    timer = undefined;
    if (off || queue.length === 0) return;
    const events = queue.splice(0);
    if (Date.now() < quietUntil) return;
    for (const event of events) {
      // A fetch is noted when its headers arrive. By now the browser usually
      // knows when the body ended too, which is when the server finished.
      const href = event._u as string | undefined;
      const began = event._t as number | undefined;
      delete event._u;
      delete event._t;
      if (!href || began === undefined) continue;
      try {
        const entry = (performance.getEntriesByName(href, "resource") as PerformanceResourceTiming[])
          .reverse()
          .find((e) => e.startTime >= began - 5 && e.responseEnd > 0);
        if (entry) event.dur = Math.max(event.dur as number, entry.responseEnd - began);
      } catch {
        // No resource timing: the time to the headers stands.
      }
    }
    const body = JSON.stringify({ s: session, e: events });
    if (leaving && navigator.sendBeacon) {
      navigator.sendBeacon(RELAY, body);
      return;
    }
    nativeFetch(RELAY, { method: "POST", body, keepalive: true }).then(
      (response) => {
        if (response.status !== 200) off = true;
      },
      () => {
        // The dev server is restarting or gone. Say nothing for a while.
        quietUntil = Date.now() + 10_000;
      },
    );
  };

  // Errors nobody caught. Each Error is remembered, so the same one logged
  // again through console.error (React and Next.js both do) is sent once.
  const reported = new WeakSet<object>();
  const error = (reason: unknown, message: string | undefined, kind?: string) => {
    const e = reason as { message?: unknown; stack?: unknown; name?: unknown } | null | undefined;
    if (e && typeof e === "object") reported.add(e);
    push({
      t: "error",
      k: kind,
      m: clip(typeof e?.message === "string" ? e.message : (message ?? show(reason))),
      st: typeof e?.stack === "string" ? clip(e.stack) : undefined,
      n: typeof e?.name === "string" ? e.name : undefined,
    });
  };
  window.addEventListener("error", (event) => {
    // An <img> or <script> that failed to load also fires "error", with no
    // message: that is not an exception.
    if (event.error != null || event.message) error(event.error, event.message);
  });
  window.addEventListener("unhandledrejection", (event) => error(event.reason, undefined, "rejection"));

  if (options.console !== false) {
    for (const level of ["error", "warn"] as const) {
      const original = console[level];
      console[level] = function (this: unknown, ...args: unknown[]) {
        original.apply(this, args);
        try {
          const thrown = args.find((arg): arg is Error => arg instanceof Error);
          if (thrown && reported.has(thrown)) return;
          push({ t: "console", k: level, m: clip(format(args)), st: thrown?.stack ? clip(thrown.stack) : undefined });
        } catch {
          // The console printed it; that is the part the page relies on.
        }
      };
    }
  }

  // Web Vitals, measured with the browser's own observers. LCP is final at
  // the first input; CLS and INP are reported when the page is left, by a
  // client-side navigation or for real.
  const vital = (name: string, value: number, good: number, poor: number, path?: string) =>
    push({
      t: "vital",
      name,
      v: Math.round(value * 1000) / 1000,
      r: value <= good ? "good" : value <= poor ? "needs-improvement" : "poor",
      ...(path ? { p: path } : {}),
    });
  const observe = (type: string, each: (entry: any) => void, more?: object) => {
    try {
      const observer = new PerformanceObserver((list) => list.getEntries().forEach(each));
      observer.observe({ type, buffered: true, ...more });
      return observer;
    } catch {
      return undefined; // This browser does not measure that.
    }
  };
  const navigation = performance.getEntriesByType?.("navigation")[0] as PerformanceNavigationTiming | undefined;
  if (navigation && navigation.responseStart > 0) vital("TTFB", navigation.responseStart, 800, 1800);
  observe("paint", (entry) => {
    if (entry.name === "first-contentful-paint") vital("FCP", entry.startTime, 1800, 3000);
  });
  let lcp = 0;
  const lcpObserver = observe("largest-contentful-paint", (entry) => (lcp = entry.startTime));
  const settleLcp = () => {
    lcpObserver?.takeRecords().forEach((entry) => (lcp = entry.startTime));
    lcpObserver?.disconnect();
    if (lcp) vital("LCP", lcp, 2500, 4000);
    lcp = 0;
  };
  for (const type of ["keydown", "pointerdown"]) {
    window.addEventListener(type, settleLcp, { once: true, capture: true });
  }
  // Layout shifts count in bursts: the worst run of shifts less than a second
  // apart, lasting at most five.
  let burst = 0;
  let burstStart = 0;
  let lastShift = 0;
  let cls = 0;
  let shifted = false;
  observe("layout-shift", (entry) => {
    if (entry.hadRecentInput) return;
    if (burst && entry.startTime - lastShift < 1000 && entry.startTime - burstStart < 5000) burst += entry.value;
    else [burst, burstStart] = [entry.value, entry.startTime];
    lastShift = entry.startTime;
    cls = Math.max(cls, burst);
    shifted = true;
  });
  // INP proper is a high percentile of all interactions; on a page a
  // developer pokes at for a minute, the slowest one is the same thing.
  let inp = 0;
  observe("event", (entry) => {
    if (entry.interactionId && entry.duration > inp) inp = entry.duration;
  }, { durationThreshold: 40 });
  const leavePage = (path: string) => {
    settleLcp();
    if (shifted) vital("CLS", cls, 0.1, 0.25, path);
    if (inp) vital("INP", inp, 200, 500, path);
    [burst, cls, inp, shifted] = [0, 0, 0, false];
  };

  // Client-side navigations. Both routers move through the History API.
  let here = location.pathname + location.search;
  let herePath = location.pathname;
  const moved = (how: string) => {
    const now = location.pathname + location.search;
    if (now === here) return;
    leavePage(herePath);
    push({ t: "nav", how, from: here });
    here = now;
    herePath = location.pathname;
  };
  for (const method of ["pushState", "replaceState"] as const) {
    const original = history[method];
    history[method] = function (this: History, ...args: Parameters<History["pushState"]>) {
      const result = original.apply(this, args);
      // Next.js calls these while it renders; reading the location is cheap,
      // queuing an event is too.
      moved(method === "pushState" ? "push" : "replace");
      return result;
    };
  }
  window.addEventListener("popstate", () => moved("pop"));
  push({ t: "nav", how: "load" });

  const leaving = () => {
    leavePage(herePath);
    flush(true);
  };
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "hidden") leaving();
  });
  window.addEventListener("pagehide", leaving);

  // fetch. A request to this origin gets a `traceparent` header, so the
  // server's trace of it continues one that starts here, and is always
  // reported so that trace has its first span. A request anywhere else is
  // left exactly as it was, and reported only if it failed or was slow.
  const slowMs = options.slowFetchMs ?? 1000;
  window.fetch = function (input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
    let url: URL;
    let method: string;
    const request = typeof Request !== "undefined" && input instanceof Request ? input : undefined;
    try {
      url = new URL(request ? request.url : String(input), location.href);
      method = (init?.method ?? request?.method ?? "GET").toUpperCase();
    } catch {
      return nativeFetch(input, init);
    }
    const same = url.origin === location.origin;
    // The dev server's own traffic (chunks, hot reload, the error overlay)
    // is not the app's.
    if (same && /^\/(?:_next\/(?!data\/)|__)/.test(url.pathname)) return nativeFetch(input, init);

    let tid: string | undefined;
    let sid: string | undefined;
    if (same) {
      try {
        const headers = new Headers(init?.headers ?? request?.headers);
        if (!headers.has("traceparent")) {
          tid = hex(16);
          sid = hex(8);
          headers.set("traceparent", `00-${tid}-${sid}-01`);
          init = { ...init, headers };
        }
      } catch {
        tid = sid = undefined; // Headers this cannot copy: send the request untouched.
      }
    }

    const began = performance.now();
    const ts = Date.now();
    const done = (status: number, err?: string, aborted?: boolean) => {
      const dur = performance.now() - began;
      const slow = dur >= slowMs;
      if (!tid && !err && status < 400 && !slow) return;
      push({
        t: "fetch",
        ts,
        method,
        // The path only: a query string can carry a token.
        url: same ? url.pathname : url.origin + url.pathname,
        status,
        dur,
        err,
        tid,
        sid,
        slow: slow || undefined,
        k: aborted ? "aborted" : undefined,
        _u: url.href,
        _t: began,
      });
    };
    return nativeFetch(input, init).then(
      (response) => {
        done(response.status);
        return response;
      },
      (reason: unknown) => {
        // The router aborts requests it no longer needs. That is not a failure.
        const aborted = (reason as { name?: string } | null)?.name === "AbortError";
        done(0, aborted ? undefined : clip(show(reason)), aborted);
        throw reason;
      },
    );
  };
}

/** `bytes` random bytes as hex: 16 for a trace id, 8 for a span id. */
export function hex(bytes: number): string {
  const buffer = new Uint8Array(bytes);
  crypto.getRandomValues(buffer);
  let out = "";
  for (const byte of buffer) out += (byte + 0x100).toString(16).slice(1);
  return out;
}

/** What the console would print for these arguments, near enough, as one string. */
export function format(args: unknown[]): string {
  const rest = args.slice();
  let out = "";
  if (typeof rest[0] === "string") {
    // React and Next.js log with printf-style placeholders.
    out = (rest.shift() as string).replace(/%([sdifoOc%])/g, (match, kind: string) => {
      if (kind === "%") return "%";
      if (rest.length === 0) return match;
      const value = rest.shift();
      return kind === "c" ? "" : show(value);
    });
  }
  for (const value of rest) out += (out ? " " : "") + show(value);
  return out;
}

function show(value: unknown): string {
  if (typeof value === "string") return value;
  if (value instanceof Error) return `${value.name}: ${value.message}`;
  if (value === undefined) return "undefined";
  try {
    return typeof value === "object" && value !== null ? JSON.stringify(value) : String(value);
  } catch {
    return String(value);
  }
}

function clip(text: string): string {
  return text.length > MAX_TEXT ? text.slice(0, MAX_TEXT) : text;
}
