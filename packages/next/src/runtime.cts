// Everything that runs in the dev server's process: the provider from
// @leadline/node with this package's processors in it, the route list, and
// the relay for what the browser reports.
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { join } from "node:path";
import { Actions, devDistDir } from "./actions.cjs";
import { UserDetector } from "./auth.cjs";
import { convert, envelope, parseBatch, tracedFetches } from "./browser-events.cjs";
import type { Caused } from "./browser-events.cjs";
import { isNoise, pathOf, readHints } from "./classify.cjs";
import { installRelay } from "./relay.cjs";
import { Requests, userAttributes } from "./requests.cjs";
import { RouteList } from "./route-list.cjs";
import { routeDirs } from "./routes.cjs";
import { Sender } from "./send.cjs";
import { mapStack } from "./stacks.cjs";
import type { AttributeValue } from "./send.cjs";
import type { LeadlineNextOptions, LeadlineUser } from "./types.cjs";

type Node = typeof import("@leadline/node", { with: { "resolution-mode": "import" } });
type Headers = Record<string, string | string[] | undefined>;

/** What `identify()` in the app's bundle finds on the global object. */
export interface Runtime {
  /** False until the provider is running, and when it decided not to. */
  active: boolean;
  identify(user: LeadlineUser): void;
  /** What Next.js's resolved config says, once it is known. */
  configure(config: { pageExtensions?: string[]; distDir?: string }): void;
  stop(): Promise<void>;
}

export interface Project {
  /** The folder with next.config in it. */
  dir: string;
  pageExtensions?: string[];
  distDir?: string;
}

/** A console line Next.js forwarded from the browser, with or without colour codes in front. */
const FORWARDED = /^(?:\x1b\[[0-9;]*m)*\[browser\]/;

const KEY = Symbol.for("leadline.next");
type Holder = { [KEY]?: Runtime };

/**
 * Starts the provider in this process, once. Never throws: a development
 * tool that fails must leave the dev server as it found it.
 */
export function start(options: LeadlineNextOptions, project: Project): Runtime {
  const holder = globalThis as Holder;
  if (holder[KEY]) return holder[KEY];

  const say = (message: string) => void process.stderr.write(`[leadline] ${message}\n`);
  const debug = options.debug ?? /^(1|true|yes|on)$/i.test(process.env.LEADLINE_DEBUG ?? "");
  const log = debug ? say : () => {};
  const failed = (err: unknown) => log(`not started: ${err instanceof Error ? (err.stack ?? err.message) : String(err)}`);

  const pending = { ...project };
  const runtime: Runtime = {
    active: false,
    identify: () => {},
    configure: (config) => Object.assign(pending, config),
    stop: async () => {},
  };
  holder[KEY] = runtime;

  const boot = (node: Node) => {
    const actions = new Actions(pending.dir, pending.distDir);
    let routes: RouteList | undefined;
    const requests = new Requests({
      action: (id) => actions.lookup(id),
      routes: () => routes?.current ?? [],
      onRequest: () => routes?.nudge(),
    });
    const detector = options.users === false ? undefined : new UserDetector();
    let pageReports = false;

    const handle = node.register({
      url: options.url,
      serviceName: options.serviceName ?? packageName(pending.dir),
      enabled: options.enabled,
      console: options.console,
      metrics: options.metrics,
      batchDelayMs: options.batchDelayMs,
      debug: options.debug,
      instrumentations: options.instrumentations,
      instrumentationConfig: httpHooks(options.instrumentationConfig, requests, detector),
      spanProcessors: [requests.spans],
      logRecordProcessors: [requests.logs],
      keepSpan: requests.keep,
      // Next.js prints the browser's errors and warnings in the dev server's
      // terminal, as "[browser] ...". Once the page reports them itself, with
      // the user and the page they happened on, that copy would be a second
      // entry for the same event.
      keepLog: (record) => !(pageReports && typeof record.body === "string" && FORWARDED.test(record.body)),
    });
    // Switched off, or the app has an OpenTelemetry SDK of its own.
    if (!handle.active) return;

    const sender = new Sender(handle.url, log);
    if (options.routes !== false) {
      const list = (routes = new RouteList({
        dir: pending.dir,
        pageExtensions: pending.pageExtensions,
        service: handle.serviceName,
        nextVersion: nextVersion(pending.dir),
        sender,
        log,
      }));
      sender.onBack = () => list.forget();
      // After the config has been read and the server is up: reading the
      // route folders is not worth a millisecond of anyone's startup.
      setImmediate(() => list.start());
      runtime.configure = (config) => {
        if (config.pageExtensions && String(config.pageExtensions) !== String(pending.pageExtensions)) {
          list.setPageExtensions(config.pageExtensions);
        }
        Object.assign(pending, config);
      };
    }

    const removeRelay = installRelay((body, headers, port) => {
      void relay(body, headers, port).catch(failed);
    });

    async function relay(body: string, headers: Headers, port: number): Promise<void> {
      const batch = parseBatch(body);
      if (!batch?.e?.length) return;
      pageReports = true;
      // Stacks in source positions, for the few events that have one.
      const where = {
        port,
        distDir: devDistDir(pending.dir, pending.distDir),
        appDirectory: Boolean(routeDirs(pending.dir).app),
      };
      await Promise.all(
        batch.e
          .filter((event) => typeof event.st === "string" && event.st)
          .slice(0, 8)
          .map(async (event) => {
            event.st = await mapStack(event.st!, where);
          }),
      );
      // A fetch the page reports was answered by a request traced here. Wait
      // for that one to end: it knows the route and the action's name.
      const caused = new Map<string, Caused>();
      await Promise.all(
        tracedFetches(batch).map(async ({ traceId, spanId }) => {
          const outcome = await requests.outcomeFor(traceId, spanId);
          if (!outcome) return;
          caused.set(`${traceId}:${spanId}`, {
            name: outcome.name,
            route: outcome.route,
            attributes: outcome.attributes,
            user: outcome.user as Record<string, AttributeValue> | undefined,
          });
        }),
      );
      const cookie = typeof headers.cookie === "string" ? headers.cookie : undefined;
      const user =
        requests.userForCookie(cookie) ?? userAttributes(detector?.detect(cookie)?.user);
      const userAgent = typeof headers["user-agent"] === "string" ? headers["user-agent"] : undefined;
      const { spans, logs } = convert(batch, {
        caused: (traceId, spanId) => caused.get(`${traceId}:${spanId}`),
        user: user as Record<string, AttributeValue> | undefined,
        userAgent,
      });
      if (spans.length > 0) void sender.post("/v1/traces", envelope("spans", handle.serviceName, spans));
      if (logs.length > 0) void sender.post("/v1/logs", envelope("logs", handle.serviceName, logs));
    }

    runtime.active = true;
    runtime.identify = (user) => requests.identify(user);
    runtime.stop = async () => {
      removeRelay();
      routes?.stop();
      runtime.active = false;
      runtime.identify = () => {};
      if (holder[KEY] === runtime) delete holder[KEY];
      await handle.shutdown();
    };
  };

  try {
    // Synchronous where Node can: the instrumentation should be in place
    // before the dev server takes its first request. @leadline/node is an ES
    // module, which Node 20.19 and 22.12 and newer can require().
    boot(createRequire(__filename)("@leadline/node") as Node);
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === "ERR_REQUIRE_ESM") {
      void import("@leadline/node").then(boot).catch(failed);
    } else {
      failed(err);
    }
  }
  return runtime;
}

type Hook = (...args: any[]) => unknown;
type Config = NonNullable<LeadlineNextOptions["instrumentationConfig"]>;

/** This package's hooks for the http instrumentation, in front of any the caller gave. */
function httpHooks(config: Config | undefined, requests: Requests, detector: UserDetector | undefined): Config {
  const name = "@opentelemetry/instrumentation-http";
  const theirs = ((config as Record<string, unknown> | undefined)?.[name] ?? {}) as Record<string, Hook | undefined>;
  const ours = {
    ...theirs,
    ignoreIncomingRequestHook: (request: { url?: string }) =>
      isNoise(pathOf(request.url)) || theirs.ignoreIncomingRequestHook?.(request) === true,
    requestHook: (
      span: { spanContext(): { traceId: string; spanId: string }; updateName(name: string): unknown },
      request: { method?: string; url?: string; headers?: Headers },
    ) => {
      // The hook is also called for outgoing requests; only an incoming one
      // has an HTTP version yet.
      if ("httpVersion" in request && request.headers) {
        const headers = request.headers;
        requests.incoming(span, { method: request.method, url: request.url, headers }, readHints(request.method, request.url, headers));
        const found = detector?.detect(typeof headers.cookie === "string" ? headers.cookie : undefined);
        if (found) requests.identifyFromCookie(span.spanContext().spanId, found.user);
      }
      theirs.requestHook?.(span, request);
    },
  };
  return { ...config, [name]: ours } as Config;
}

function packageName(dir: string): string | undefined {
  // OTEL_SERVICE_NAME, when set, is the provider's to read.
  if (process.env.OTEL_SERVICE_NAME?.trim()) return undefined;
  try {
    const name: unknown = JSON.parse(readFileSync(join(dir, "package.json"), "utf8")).name;
    return typeof name === "string" && name.trim() ? name.trim() : undefined;
  } catch {
    return undefined;
  }
}

function nextVersion(dir: string): string | undefined {
  try {
    const file = createRequire(join(dir, "package.json")).resolve("next/package.json");
    return JSON.parse(readFileSync(file, "utf8")).version;
  } catch {
    return undefined;
  }
}
