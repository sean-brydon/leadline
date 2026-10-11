// Everything that needs the OpenTelemetry SDK. This file is CommonJS, like
// the SDK, so index.ts can load it late and synchronously with require() on
// every Node version the SDK supports.
import * as nodeModule from "node:module";
import { pathToFileURL } from "node:url";
import { context, diag, DiagLogLevel, metrics, trace } from "@opentelemetry/api";
import type { Context, DiagLogger, Span as ApiSpan } from "@opentelemetry/api";
import { logs } from "@opentelemetry/api-logs";
import { getNodeAutoInstrumentations } from "@opentelemetry/auto-instrumentations-node";
import type { InstrumentationConfigMap } from "@opentelemetry/auto-instrumentations-node";
import { diagLogLevelFromString, isTracingSuppressed } from "@opentelemetry/core";
import { OTLPLogExporter } from "@opentelemetry/exporter-logs-otlp-proto";
import { OTLPMetricExporter } from "@opentelemetry/exporter-metrics-otlp-proto";
import { OTLPTraceExporter } from "@opentelemetry/exporter-trace-otlp-proto";
import type { Instrumentation } from "@opentelemetry/instrumentation";
import { ProtobufLogsSerializer, ProtobufTraceSerializer } from "@opentelemetry/otlp-transformer";
import { envDetector, processDetector } from "@opentelemetry/resources";
import { BatchLogRecordProcessor } from "@opentelemetry/sdk-logs";
import type { ReadableLogRecord, SdkLogRecord } from "@opentelemetry/sdk-logs";
import { PeriodicExportingMetricReader } from "@opentelemetry/sdk-metrics";
import { NodeSDK } from "@opentelemetry/sdk-node";
import { BatchSpanProcessor } from "@opentelemetry/sdk-trace";
import type { ReadableSpan, Span } from "@opentelemetry/sdk-trace";
import { format } from "node:util";

import { captureConsole } from "./console.cjs";
import { sendFromExit, Tail } from "./exit.cjs";
import { Gate, gated } from "./gate.cjs";
import { markLines, runStreams } from "./run.cjs";
import type { Leadline, ResolvedOptions } from "./types.cjs";

/** A local daemon answers in milliseconds. A short limit also bounds how long a retry can hold up a process that wants to exit. */
const EXPORT_TIMEOUT_MS = 2000;
const METRICS_INTERVAL_MS = 5000;

export function start(options: ResolvedOptions): Leadline {
  const say = (message: string) => void process.stderr.write(`[leadline] ${message}\n`);
  const log = options.debug ? say : () => {};

  // Two SDKs in one process fight over the global tracer: the second
  // registration fails and its spans go nowhere. If the app has its own
  // OpenTelemetry setup, pointing that at Leadline is the way in.
  if (otelAlreadySetUp()) {
    say(
      `OpenTelemetry is already set up in this process, so @leadline/node stays out of it. ` +
        `Point its OTLP/HTTP exporter at ${options.url} to see the app in Leadline.`,
    );
    return idle(options, "otel-already-set-up");
  }

  const endpoint = new URL(options.url);
  const gate = new Gate(
    endpoint.hostname.replace(/^\[|\]$/g, ""),
    Number(endpoint.port) || (endpoint.protocol === "https:" ? 443 : 80),
    log,
  );
  // Look now, so the answer is in before the first batch is due.
  void gate.ready();

  const spanTail = new Tail<ReadableSpan>();
  const logTail = new Tail<ReadableLogRecord>();
  const tracesUrl = `${options.url}/v1/traces`;
  const logsUrl = `${options.url}/v1/logs`;

  // The stock batching, with a short delay so the app feels live in Leadline.
  // On top of it: each span's parent is remembered for countQueryOnce, and
  // what has not left yet is noted for the exit handler below.
  class Spans extends BatchSpanProcessor {
    override onStart(span: Span, parentContext: Context): void {
      const parent = trace.getSpan(parentContext);
      if (parent) parents.set(span, parent);
    }
    onEnding(span: Span): void {
      countQueryOnce(span, parents.get(span));
    }
    override onEnd(span: ReadableSpan): void {
      if (options.keepSpan && !keep(options.keepSpan, span)) return;
      spanTail.add(span);
      super.onEnd(span);
    }
  }
  class Logs extends BatchLogRecordProcessor {
    override onEmit(record: SdkLogRecord): void {
      if (options.keepLog && !keep(options.keepLog, record)) return;
      logTail.add(record);
      super.onEmit(record);
    }
  }
  const spans = new Spans({
    exporter: gated(
      new OTLPTraceExporter({ url: tracesUrl, timeoutMillis: EXPORT_TIMEOUT_MS }),
      gate,
      log,
      (sent: ReadableSpan[]) => spanTail.forget(sent),
    ),
    scheduledDelayMillis: options.batchDelayMs,
    exportTimeoutMillis: EXPORT_TIMEOUT_MS,
  });
  const logRecords = new Logs({
    exporter: gated(
      new OTLPLogExporter({ url: logsUrl, timeoutMillis: EXPORT_TIMEOUT_MS }),
      gate,
      log,
      (sent: ReadableLogRecord[]) => logTail.forget(sent),
    ),
    scheduledDelayMillis: options.batchDelayMs,
    exportTimeoutMillis: EXPORT_TIMEOUT_MS,
  });

  const instrumentations = [
    ...getNodeAutoInstrumentations(instrumentationConfig(options)),
    ...options.instrumentations,
  ];
  hookEsmImports(instrumentations, log);

  const sdk = new NodeSDK({
    serviceName: options.serviceName,
    instrumentations,
    // The caller's processors first, so what they add to a span or a record
    // is there when ours looks at it.
    spanProcessors: [...options.spanProcessors, spans],
    logRecordProcessors: [...options.logRecordProcessors, logRecords],
    // An empty list, not a missing one: left out, the SDK reads
    // OTEL_METRICS_EXPORTER and defaults to exporting metrics over OTLP.
    metricReaders: options.metrics
      ? [
          new PeriodicExportingMetricReader({
            exporter: gated(
              new OTLPMetricExporter({
                url: `${options.url}/v1/metrics`,
                timeoutMillis: EXPORT_TIMEOUT_MS,
              }),
              gate,
              log,
            ),
            exportIntervalMillis: METRICS_INTERVAL_MS,
            exportTimeoutMillis: EXPORT_TIMEOUT_MS,
          }),
        ]
      : [],
    // The default set also asks the OS for a machine id, which on macOS
    // starts a child process on every boot. These two only read what the
    // process already knows. OTEL_NODE_RESOURCE_DETECTORS still overrides.
    ...(process.env.OTEL_NODE_RESOURCE_DETECTORS
      ? {}
      : { resourceDetectors: [envDetector, processDetector] }),
  });
  rerouteDiagnostics();
  sdk.start();
  // An instrumentation the app constructed itself may have looked up the
  // tracer before there was one, through its own copy of @opentelemetry/api
  // (Prisma's does, when it is created). Handing it the real providers and
  // enabling it again settles that; for one that was fine it changes nothing.
  for (const instrumentation of options.instrumentations) {
    try {
      instrumentation.setTracerProvider(trace.getTracerProvider());
      instrumentation.setLoggerProvider?.(logs.getLoggerProvider());
      if (options.metrics) instrumentation.setMeterProvider(metrics.getMeterProvider());
      instrumentation.enable();
    } catch (err) {
      log(`could not wire ${instrumentation.instrumentationName}: ${String(err)}`);
    }
  }

  const restoreConsole = options.console ? sendConsole() : () => {};

  const flush = async () => {
    await Promise.allSettled([spans.forceFlush(), logRecords.forceFlush()]);
  };
  // The batch timers do not keep the process alive, so a script that simply
  // finishes would leave with its last half second unsent.
  const onBeforeExit = () => void flush();
  // process.exit() and a crash leave no time to flush. Whatever is still
  // waiting is handed to a helper process; see sendFromExit.
  const onExit = () => {
    if (gate.state === "down") return;
    try {
      const lastSpans = spanTail.take();
      const lastLogs = logTail.take();
      sendFromExit([
        { url: tracesUrl, body: (lastSpans.length && ProtobufTraceSerializer.serializeRequest(lastSpans)) || new Uint8Array() },
        { url: logsUrl, body: (lastLogs.length && ProtobufLogsSerializer.serializeRequest(lastLogs)) || new Uint8Array() },
      ]);
    } catch (err) {
      log(`nothing sent on exit: ${String(err)}`);
    }
  };
  // The "monitor" event is told about an uncaught exception, or a rejection
  // nobody handled, without becoming the handler for it: the process still
  // prints the error and exits exactly as it would have.
  const onUncaught = (err: unknown, origin: string) => {
    try {
      // With no handler of the app's own, this is the last thing it does.
      const fatal = process.listenerCount("uncaughtException") === 0;
      const what = origin === "unhandledRejection" ? "Unhandled rejection" : "Uncaught exception";
      logs.getLogger("@leadline/node").emit({
        severityNumber: fatal ? 21 : 17,
        severityText: fatal ? "FATAL" : "ERROR",
        body: `${what}: ${err instanceof Error ? (err.stack ?? err.message) : format(err)}`,
        attributes: { "leadline.origin": origin, ...exceptionAttributes(err) },
      });
    } catch {
      // The app is already going down with its own error; keep ours out of it.
    }
  };
  process.on("beforeExit", onBeforeExit);
  process.on("exit", onExit);
  process.on("uncaughtExceptionMonitor", onUncaught);

  log(`sending "${options.serviceName}" to ${options.url}`);
  return {
    active: true,
    url: options.url,
    serviceName: options.serviceName,
    flush,
    shutdown: async () => {
      restoreConsole();
      process.off("beforeExit", onBeforeExit);
      process.off("exit", onExit);
      process.off("uncaughtExceptionMonitor", onUncaught);
      await sdk.shutdown().catch(() => {});
    },
  };
}

/** A filter that throws keeps the span or record: losing one to a bug in the filter would be worse. */
function keep<T>(filter: (item: T) => boolean, item: T): boolean {
  try {
    return filter(item) !== false;
  } catch {
    return true;
  }
}

/** Each span's parent, for countQueryOnce. Weak, so it holds nothing back. */
const parents = new WeakMap<object, ApiSpan>();

const DB_SYSTEM_KEYS = ["db.system.name", "db.system"];

function dbSystem(span: { attributes?: Record<string, unknown> }): unknown {
  for (const key of DB_SYSTEM_KEYS) {
    const value = span.attributes?.[key];
    if (value) return value;
  }
  return undefined;
}

/**
 * Leadline counts every span that names a database system as one query. A
 * query builder or ORM (knex, Prisma with its instrumentation) and the driver
 * under it (pg) each make such a span for the same statement, so it would be
 * listed twice and an N+1 of 25 would read as 50. When a database span ends
 * inside another for the same system, the inner one is the query on the wire
 * and the outer one stops claiming to be: it stays in the trace, with its
 * statement, as the step that led to it.
 */
function countQueryOnce(span: Span, parent: ApiSpan | undefined): void {
  if (!parent?.isRecording()) return;
  const system = dbSystem(span);
  if (!system || dbSystem(parent as { attributes?: Record<string, unknown> }) !== system) return;
  for (const key of DB_SYSTEM_KEYS) {
    if ((parent as unknown as ReadableSpan).attributes[key]) parent.setAttribute(key, "");
  }
  parent.setAttribute("leadline.db.wraps", String(system));
}

/** Leadline's settings for the automatic instrumentations, under the caller's. */
function instrumentationConfig(options: ResolvedOptions): InstrumentationConfigMap {
  const ours: InstrumentationConfigMap = {
    // Full SQL is on by default in pg and mysql2. These add the parameter
    // values where the instrumentation can, and lift the limits and masks
    // meant for telemetry that leaves the machine. This never does.
    "@opentelemetry/instrumentation-pg": {
      enhancedDatabaseReporting: true,
      // pg-pool.connect and pg.connect carry db.system.name like a query
      // does, so each would be listed and counted as one.
      ignoreConnectSpans: true,
    },
    "@opentelemetry/instrumentation-mysql": { enhancedDatabaseReporting: true },
    "@opentelemetry/instrumentation-oracledb": {
      enhancedDatabaseReporting: true,
      dbStatementDump: true,
    },
    "@opentelemetry/instrumentation-cassandra-driver": { enhancedDatabaseReporting: true },
    "@opentelemetry/instrumentation-knex": { maxQueryLength: 65536 },
    "@opentelemetry/instrumentation-http": {
      // A server span is named by its method alone ("GET") until a framework
      // reports the route. The path says which request it was in the
      // meantime, and for a server with no router it is all there is. When
      // there is a route ("GET /users/:id"), the instrumentation renames the
      // span to it as the response finishes.
      requestHook: (span, request) => {
        if ("httpVersion" in request && request.url) {
          span.updateName(`${request.method ?? "GET"} ${request.url.split("?", 1)[0]}`);
        }
      },
    },
    // Its event-loop and heap gauges are only worth their timers when
    // metrics are going somewhere.
    "@opentelemetry/instrumentation-runtime-node": { enabled: options.metrics },
  };
  // A span for every file read, DNS lookup and socket buries the request
  // under its own plumbing. Naming instrumentations in
  // OTEL_NODE_ENABLED_INSTRUMENTATIONS is the user choosing for themselves.
  if (!process.env.OTEL_NODE_ENABLED_INSTRUMENTATIONS) {
    ours["@opentelemetry/instrumentation-fs"] = { enabled: false };
    ours["@opentelemetry/instrumentation-dns"] = { enabled: false };
    ours["@opentelemetry/instrumentation-net"] = { enabled: false };
    // Express 5 routes through the `router` package, so with this on every
    // Express layer gets a second span ("middleware - patched") beside the
    // one from the Express instrumentation.
    ours["@opentelemetry/instrumentation-router"] = { enabled: false };
  }
  const merged: Record<string, object> = { ...ours };
  for (const [name, config] of Object.entries(options.instrumentationConfig)) {
    merged[name] = { ...merged[name], ...config };
  }
  return merged as InstrumentationConfigMap;
}

/**
 * Lets the instrumentations see modules loaded with `import`. `require()` is
 * patched in-process; ES modules need a loader hook, which only takes effect
 * for imports that start after this call. That is why `--import` is the way
 * to load the provider in an ESM app.
 */
function hookEsmImports(instrumentations: Instrumentation[], log: (message: string) => void): void {
  try {
    if (typeof nodeModule.register !== "function") return;
    const names = new Set<string>();
    for (const instrumentation of instrumentations) {
      const definitions =
        (instrumentation as { getModuleDefinitions?: () => Array<{ name: string }> })
          .getModuleDefinitions?.() ?? [];
      for (const { name } of definitions) names.add(name);
    }
    if (names.size === 0) return;
    // Only the modules something instruments are wrapped. The hook's default
    // is to wrap every import, which costs startup time and is the usual way
    // a loader hook breaks an app. The pattern covers files inside those
    // packages, which some instrumentations patch one by one.
    const packages = [...names].filter((name) => !nodeModule.isBuiltin(name));
    const escaped = packages.map((name) => name.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"));
    nodeModule.register("@opentelemetry/instrumentation/hook.mjs", pathToFileURL(__filename), {
      data: { include: [...names, new RegExp(`/node_modules/(?:${escaped.join("|")})/`)] },
    });
  } catch (err) {
    log(`ES module hook not installed, so import-ed libraries may go unseen: ${String(err)}`);
  }
}

/**
 * OpenTelemetry's own diagnostics (OTEL_LOG_LEVEL) print through `console`.
 * Captured, each would become a log record whose export produces more
 * diagnostics. Writing them straight to stderr keeps them out of the loop.
 */
function rerouteDiagnostics(): void {
  const level = process.env.OTEL_LOG_LEVEL;
  if (!level) return;
  const write = (...args: unknown[]) => void process.stderr.write(`${format(...args)}\n`);
  const logger: DiagLogger = { error: write, warn: write, info: write, debug: write, verbose: write };
  diag.setLogger(logger, {
    logLevel: diagLogLevelFromString(level) ?? DiagLogLevel.INFO,
    suppressOverrideMessage: true,
  });
}

/** Turns console output into log records. The SDK adds the active trace and span ids. */
function sendConsole(): () => void {
  const logger = logs.getLogger("@leadline/node/console");
  // Under `leadline run`, which reads this output as well: the lines sent
  // from here are marked, so it leaves them to this record.
  const marked = runStreams();
  return captureConsole(
    (record) => {
      logger.emit({
        severityNumber: record.severityNumber,
        severityText: record.severityText,
        body: record.body,
        attributes: {
          "log.iostream": record.stream,
          ...(record.error ? exceptionAttributes(record.error) : {}),
        },
      });
    },
    console,
    {
      // Inside an export, tracing is suppressed: anything logged there is the
      // SDK's own business, not the app's.
      skip: () => isTracingSuppressed(context.active()),
      around: (stream) =>
        marked[stream] ? markLines(stream === "stdout" ? process.stdout : process.stderr) : undefined,
    },
  );
}

function exceptionAttributes(err: unknown): Record<string, string> {
  if (!(err instanceof Error)) return {};
  return {
    "exception.type": err.name,
    "exception.message": err.message,
    ...(err.stack ? { "exception.stacktrace": err.stack } : {}),
  };
}

function otelAlreadySetUp(): boolean {
  try {
    const provider = trace.getTracerProvider() as { getDelegate?: () => object };
    const name = provider.getDelegate?.().constructor.name;
    return name !== undefined && name !== "NoopTracerProvider";
  } catch {
    return false;
  }
}

function idle(options: ResolvedOptions, reason: Leadline["reason"]): Leadline {
  return {
    active: false,
    reason,
    url: options.url,
    serviceName: options.serviceName,
    flush: async () => {},
    shutdown: async () => {},
  };
}
