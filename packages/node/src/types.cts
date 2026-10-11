import type { Instrumentation } from "@opentelemetry/instrumentation";
import type { InstrumentationConfigMap } from "@opentelemetry/auto-instrumentations-node";
import type { LogRecordProcessor, ReadableLogRecord } from "@opentelemetry/sdk-logs";
import type { ReadableSpan, SpanProcessor } from "@opentelemetry/sdk-trace";

/** What `register()` takes. Every field has an environment variable or a default. */
export interface LeadlineOptions {
  /** Where the Leadline daemon listens. Default: `LEADLINE_URL`, else `http://127.0.0.1:4320`. */
  url?: string;
  /** The name the app shows under. Default: `OTEL_SERVICE_NAME`, else the nearest `package.json` name. */
  serviceName?: string;
  /** Run even when `NODE_ENV=production`. Default: `LEADLINE_FORCE=1`. */
  force?: boolean;
  /** `false` turns the provider off. Default: on, unless `LEADLINE_DISABLED=1`. */
  enabled?: boolean;
  /** Send `console.*` output as logs. Default: on, unless `LEADLINE_CONSOLE=0`. */
  console?: boolean;
  /** Send metrics to `/v1/metrics`. Default: off, unless `LEADLINE_METRICS=1`. */
  metrics?: boolean;
  /** How long spans and logs wait to be sent together, in milliseconds. Default: 500. */
  batchDelayMs?: number;
  /**
   * Settings for the automatic instrumentations, by package name, laid over
   * Leadline's own. `{ "@opentelemetry/instrumentation-fs": { enabled: true } }`
   * turns file system spans back on.
   */
  instrumentationConfig?: InstrumentationConfigMap;
  /** More instrumentations to run, such as `new PrismaInstrumentation()`. */
  instrumentations?: Instrumentation[];
  /**
   * More span processors, run before Leadline's own. For a package built on
   * this one (`@leadline/next`) that names spans or adds attributes to them.
   */
  spanProcessors?: SpanProcessor[];
  /** More log record processors, run before Leadline's own. */
  logRecordProcessors?: LogRecordProcessor[];
  /** Called with each finished span; return false to keep it from being sent. */
  keepSpan?: (span: ReadableSpan) => boolean;
  /** Called with each log record; return false to keep it from being sent. */
  keepLog?: (record: ReadableLogRecord) => boolean;
  /** Print what the provider is doing to stderr. Default: `LEADLINE_DEBUG=1`. */
  debug?: boolean;
}

/** Options with the environment and defaults applied. */
export interface ResolvedOptions {
  active: boolean;
  /** Why the provider is not running, when it is not. */
  reason?: "production" | "disabled" | "invalid-url";
  url: string;
  serviceName: string;
  console: boolean;
  metrics: boolean;
  batchDelayMs: number;
  debug: boolean;
  instrumentationConfig: InstrumentationConfigMap;
  instrumentations: Instrumentation[];
  spanProcessors: SpanProcessor[];
  logRecordProcessors: LogRecordProcessor[];
  keepSpan?: (span: ReadableSpan) => boolean;
  keepLog?: (record: ReadableLogRecord) => boolean;
}

/** What `register()` returns. */
export interface Leadline {
  /** False when the provider decided not to run; see `reason`. */
  active: boolean;
  reason?: ResolvedOptions["reason"] | "otel-already-set-up" | "failed";
  url: string;
  serviceName: string;
  /** Send what is waiting now. Resolves either way; never rejects. */
  flush(): Promise<void>;
  /** Stop sending and put `console` back. */
  shutdown(): Promise<void>;
}
