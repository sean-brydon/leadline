import { existsSync, readFileSync } from "node:fs";
import { basename, dirname, join, sep } from "node:path";

/**
 * The name the app shows under in Leadline: `OTEL_SERVICE_NAME` (or
 * `service.name` in `OTEL_RESOURCE_ATTRIBUTES`), else the name in the nearest
 * `package.json`, else the folder name.
 */
export function resolveServiceName(
  env: NodeJS.ProcessEnv,
  cwd: string,
  entry?: string,
): string {
  const fromEnv = env.OTEL_SERVICE_NAME?.trim() || resourceAttribute(env, "service.name");
  if (fromEnv) return fromEnv;

  // The script being run is the best clue in a monorepo started from its
  // root. A script inside node_modules is a tool (next, tsx, vitest), and the
  // package it belongs to is not the app; there the working directory is.
  const starts: string[] = [];
  if (entry && !entry.split(sep).includes("node_modules")) starts.push(dirname(entry));
  starts.push(cwd);
  for (const start of starts) {
    const name = nearestPackageName(start);
    if (name) return name;
  }
  return basename(cwd) || "node";
}

/** Walks up from `dir` to the first `package.json` that has a name. */
export function nearestPackageName(dir: string): string | undefined {
  for (let at = dir; ; at = dirname(at)) {
    const file = join(at, "package.json");
    if (existsSync(file)) {
      try {
        const name: unknown = JSON.parse(readFileSync(file, "utf8")).name;
        // A package.json with no name (a `{"type":"module"}` marker in a
        // build folder, say) is not the app's: keep walking.
        if (typeof name === "string" && name.trim()) return name.trim();
      } catch {
        // Unreadable or half-written: not a reason to stop the app.
      }
    }
    if (dirname(at) === at) return undefined;
  }
}

function resourceAttribute(env: NodeJS.ProcessEnv, key: string): string | undefined {
  for (const pair of (env.OTEL_RESOURCE_ATTRIBUTES ?? "").split(",")) {
    const eq = pair.indexOf("=");
    if (eq > 0 && pair.slice(0, eq).trim() === key) {
      const raw = pair.slice(eq + 1).trim();
      try {
        return decodeURIComponent(raw) || undefined;
      } catch {
        return raw || undefined;
      }
    }
  }
  return undefined;
}
