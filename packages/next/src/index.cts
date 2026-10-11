// `withLeadline(nextConfig)`: the one line in next.config.
//
// next.config is evaluated inside the dev server's own process, before it
// takes a request, which makes it the place to start the provider: no
// instrumentation.ts, nothing to keep out of a bundle, and nothing in the
// app's code. Outside `next dev` this returns the config it was given, the
// same object, and loads nothing.
import { dirname } from "node:path";
import { fileURLToPath } from "node:url";
import type { LeadlineNextOptions } from "./types.cjs";

export type { LeadlineNextOptions, LeadlineUser, RouteEntry } from "./types.cjs";

/** The part of a Next.js config this reads or changes. */
interface ConfigObject {
  serverExternalPackages?: string[];
  transpilePackages?: string[];
  pageExtensions?: string[];
  distDir?: string;
}
type ConfigFunction = (phase: string, context: any) => ConfigObject | Promise<ConfigObject>;

export interface WithLeadlineOptions extends LeadlineNextOptions {
  /** The project folder, when next.config is somewhere else. Default: next.config's own folder. */
  dir?: string;
}

/**
 * Database drivers the provider instruments and Next.js would bundle. A
 * driver inside the server bundle is loaded by the bundle's own module
 * system, where instrumentation cannot reach it; kept out, Node loads it and
 * its queries are seen. (`pg`, `mongodb`, `mongoose` and `@prisma/client`
 * are already on Next.js's own list.)
 */
export const DRIVER_PACKAGES = [
  "mysql2",
  "mysql",
  "ioredis",
  "redis",
  "@redis/client",
  "knex",
  "tedious",
  "oracledb",
  "cassandra-driver",
  "memcached",
];

const PHASE_DEVELOPMENT_SERVER = "phase-development-server";

/**
 * Wraps a Next.js config so the app shows up in Leadline while you run
 * `next dev`. In `next build` and `next start` it changes nothing.
 */
export function withLeadline<C extends object>(config: C, options: WithLeadlineOptions = {}): C {
  if (!isDevServer()) return config;
  try {
    const dir = options.dir ?? callerDir(new Error().stack) ?? process.cwd();
    // Loaded here, not at the top of the file, so a production build that
    // evaluates next.config never reads it.
    const { start } = require("./runtime.cjs") as typeof import("./runtime.cjs");

    if (typeof config === "function") {
      // A config function gets its phase when Next.js calls it. The provider
      // is started now all the same: this is the dev server's process, and
      // the earlier the instrumentation is in place the more it sees.
      const runtime = start(options, { dir });
      const fn = config as unknown as ConfigFunction;
      const wrapped: ConfigFunction = async (phase, context) => {
        const resolved = await fn(phase, context);
        if (phase !== PHASE_DEVELOPMENT_SERVER || !resolved) return resolved;
        runtime.configure({ pageExtensions: resolved.pageExtensions, distDir: resolved.distDir });
        return devConfig(resolved, options);
      };
      return wrapped as unknown as C;
    }

    const object = config as ConfigObject;
    start(options, { dir, pageExtensions: object.pageExtensions, distDir: object.distDir });
    return devConfig(object, options) as C;
  } catch (err) {
    if (options.debug ?? process.env.LEADLINE_DEBUG) {
      process.stderr.write(`[leadline] not started: ${err instanceof Error ? err.stack : String(err)}\n`);
    }
    return config;
  }
}

/**
 * Whether this process is `next dev`'s server. Next.js marks that process
 * itself: `next dev` starts its server as a child with NEXT_PRIVATE_WORKER
 * set (Next 15 and 16), and Next 16 also sets __NEXT_DEV_SERVER, including
 * for a custom server started with `dev: true`. NODE_ENV alone is not
 * enough, since `next build --debug-prerender` builds with
 * NODE_ENV=development.
 */
export function isDevServer(env: NodeJS.ProcessEnv = process.env): boolean {
  if (env.NODE_ENV === "production") return false;
  return Boolean(env.__NEXT_DEV_SERVER) || env.NEXT_PRIVATE_WORKER === "1";
}

/** The config as the dev server should see it. The input is not modified. */
export function devConfig<C extends ConfigObject>(config: C, options: LeadlineNextOptions = {}): C {
  if (options.externalPackages === false) return config;
  const existing = config.serverExternalPackages ?? [];
  // Next.js refuses a package that is both transpiled and external.
  const taken = new Set([...existing, ...(config.transpilePackages ?? [])]);
  const added = (options.externalPackages ?? DRIVER_PACKAGES).filter((name) => !taken.has(name));
  return added.length > 0 ? { ...config, serverExternalPackages: [...existing, ...added] } : config;
}

/**
 * The folder of the next.config that called `withLeadline`, read from the
 * call stack. `next dev some/dir` leaves the working directory where the
 * command was run, so that is not it.
 */
export function callerDir(stack: string | undefined): string | undefined {
  for (const line of (stack ?? "").split("\n")) {
    // "at Object.<anonymous> (/app/next.config.compiled.js:24:41)", or
    // "at file:///app/next.config.mjs:5:16" for an ES module.
    const match = /((?:file:\/\/|[A-Za-z]:[\\/]|\/)[^()\n]*?next\.config\.[^():\n]+):\d+:\d+\)?\s*$/.exec(line);
    if (!match) continue;
    try {
      const file = match[1]!.startsWith("file://") ? fileURLToPath(match[1]!) : match[1]!;
      return dirname(file);
    } catch {
      return undefined;
    }
  }
  return undefined;
}
