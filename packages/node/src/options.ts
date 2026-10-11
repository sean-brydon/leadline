import { resolveServiceName } from "./service.js";
import type { LeadlineOptions, ResolvedOptions } from "./types.cjs";

export const DEFAULT_URL = "http://127.0.0.1:4320";
export const DEFAULT_BATCH_DELAY_MS = 500;

/** Where the process was started: what the service name is worked out from. */
export interface Where {
  cwd: string;
  entry?: string;
}

/**
 * Applies the environment and the defaults to what `register()` was given,
 * and decides whether the provider runs at all.
 */
export function resolveOptions(
  options: LeadlineOptions = {},
  env: NodeJS.ProcessEnv = process.env,
  where: Where = { cwd: process.cwd(), entry: process.argv[1] },
): ResolvedOptions {
  const resolved: ResolvedOptions = {
    active: true,
    // Only LEADLINE_URL, never OTEL_EXPORTER_OTLP_ENDPOINT: that one often
    // names a real collector, and full SQL and every log line must not
    // follow it there.
    url: (options.url ?? (env.LEADLINE_URL?.trim() || DEFAULT_URL)).trim().replace(/\/+$/, ""),
    serviceName: options.serviceName?.trim() || resolveServiceName(env, where.cwd, where.entry),
    console: options.console ?? flag(env.LEADLINE_CONSOLE) ?? true,
    metrics: options.metrics ?? flag(env.LEADLINE_METRICS) ?? false,
    batchDelayMs: delay(options.batchDelayMs),
    debug: options.debug ?? flag(env.LEADLINE_DEBUG) ?? false,
    instrumentationConfig: options.instrumentationConfig ?? {},
    instrumentations: options.instrumentations ?? [],
    spanProcessors: options.spanProcessors ?? [],
    logRecordProcessors: options.logRecordProcessors ?? [],
    keepSpan: options.keepSpan,
    keepLog: options.keepLog,
  };

  const enabled =
    options.enabled ?? !(flag(env.LEADLINE_DISABLED) ?? flag(env.OTEL_SDK_DISABLED) ?? false);
  if (!enabled) return { ...resolved, active: false, reason: "disabled" };

  // Leadline is a development tool. It records full SQL with its parameter
  // values and everything the app logs, samples every request, and sends it
  // all unauthenticated to a daemon on this machine. None of that belongs in
  // a production process: the data is sensitive, the overhead is spent on
  // detail nobody is looking at, and there is no daemon there to receive it.
  // NODE_ENV=production is the one signal nearly every Node app sets, so it
  // makes the provider a no-op unless it is forced.
  const force = options.force ?? flag(env.LEADLINE_FORCE) ?? false;
  if (env.NODE_ENV === "production" && !force) {
    return { ...resolved, active: false, reason: "production" };
  }

  try {
    const { protocol } = new URL(resolved.url);
    if (protocol !== "http:" && protocol !== "https:") throw new Error(protocol);
  } catch {
    return { ...resolved, active: false, reason: "invalid-url" };
  }
  return resolved;
}

function flag(value: string | undefined): boolean | undefined {
  switch (value?.trim().toLowerCase()) {
    case "1":
    case "true":
    case "yes":
    case "on":
      return true;
    case "0":
    case "false":
    case "no":
    case "off":
      return false;
  }
  return undefined;
}

function delay(ms: number | undefined): number {
  return typeof ms === "number" && Number.isFinite(ms) && ms >= 0 ? ms : DEFAULT_BATCH_DELAY_MS;
}
