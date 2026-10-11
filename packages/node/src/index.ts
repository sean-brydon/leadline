import { createRequire } from "node:module";
import { resolveOptions } from "./options.js";
import type { Leadline, LeadlineOptions, ResolvedOptions } from "./types.cjs";

export type { Leadline, LeadlineOptions } from "./types.cjs";
export { DEFAULT_URL } from "./options.js";

// One provider per process, however many copies of this package are loaded
// and however many times register() runs (the --import flag and a call in
// the app, or a dev server that reloads its entry).
const KEY = Symbol.for("leadline.node");
type Global = { [KEY]?: Leadline };

/**
 * Starts sending this process's traces, database queries and logs to the
 * Leadline daemon. Call it before the app loads the modules it should see
 * (`http`, `pg`, ...). Does nothing in production, and never throws.
 */
export function register(options?: LeadlineOptions): Leadline {
  const holder = globalThis as Global;
  if (holder[KEY]) return holder[KEY];

  let resolved: ResolvedOptions | undefined;
  let handle: Leadline;
  try {
    resolved = resolveOptions(options);
    if (!resolved.active) {
      if (resolved.reason === "invalid-url") {
        process.stderr.write(`[leadline] not started: "${resolved.url}" is not an http(s) URL\n`);
      } else if (resolved.debug) {
        process.stderr.write(`[leadline] not started: ${resolved.reason}\n`);
      }
      handle = idle(resolved, resolved.reason);
    } else {
      // The OpenTelemetry SDK is some 700 modules. It is loaded here, not at
      // the top of the file, so a production process that still carries the
      // --import flag pays nothing for it. require() keeps the load
      // synchronous: the instrumentation must be in place before the next
      // line of the app runs.
      const { start } = createRequire(import.meta.url)("./start.cjs") as typeof import("./start.cjs");
      handle = start(resolved);
    }
  } catch (err) {
    // A development tool that fails must not take the app down with it.
    if (resolved?.debug ?? process.env.LEADLINE_DEBUG) {
      process.stderr.write(`[leadline] not started: ${err instanceof Error ? err.stack : String(err)}\n`);
    }
    handle = idle(resolved, "failed");
  }
  // After shutdown() a later register() starts afresh.
  const { shutdown } = handle;
  handle.shutdown = async () => {
    if (holder[KEY] === handle) delete holder[KEY];
    await shutdown();
  };
  holder[KEY] = handle;
  return handle;
}

function idle(resolved: ResolvedOptions | undefined, reason: Leadline["reason"]): Leadline {
  return {
    active: false,
    reason,
    url: resolved?.url ?? "",
    serviceName: resolved?.serviceName ?? "",
    flush: async () => {},
    shutdown: async () => {},
  };
}
