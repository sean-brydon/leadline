// A server action arrives as a POST with a `Next-Action` header holding an
// id, a hash that says nothing to a person. Next.js writes what each id is
// to server-reference-manifest.json; this reads it.
import { existsSync, readFileSync, statSync } from "node:fs";
import { isAbsolute, join, relative, sep } from "node:path";
import type { ActionInfo } from "./classify.cjs";

/** Next.js's own prefix for an action written inline in a component. */
const INLINE_PREFIX = "$$RSC_SERVER_ACTION_";

interface ManifestEntry {
  exportedName?: string;
  filename?: string;
}
interface Manifest {
  node?: Record<string, ManifestEntry>;
  edge?: Record<string, ManifestEntry>;
}

/**
 * Where the dev server writes its output. Next 16 keeps it in `.next/dev`;
 * before that it shared `.next` with the build.
 */
export function devDistDir(dir: string, distDir = ".next"): string {
  const dist = isAbsolute(distDir) ? distDir : join(dir, distDir);
  return existsSync(join(dist, "dev", "server")) || existsSync(join(dist, "dev", "static")) ? join(dist, "dev") : dist;
}

export class Actions {
  private readonly files: string[];
  private loaded?: { file: string; mtimeMs: number; manifest: Manifest };

  /** `dir` is the project; `distDir` its build folder (`.next`). */
  constructor(
    private readonly dir: string,
    distDir = ".next",
  ) {
    const dist = isAbsolute(distDir) ? distDir : join(dir, distDir);
    // Next 16 keeps the dev server's output in `.next/dev`; before that it
    // shared `.next` with the build.
    this.files = [
      join(dist, "dev", "server", "server-reference-manifest.json"),
      join(dist, "server", "server-reference-manifest.json"),
    ];
  }

  /** What the manifest says about an action id, or undefined if it is not there. */
  lookup(id: string): ActionInfo | undefined {
    const manifest = this.read();
    const entry = manifest?.node?.[id] ?? manifest?.edge?.[id];
    if (!entry) return undefined;
    return describeAction(entry, this.dir);
  }

  // The manifest is rewritten whenever a file with actions is compiled, so it
  // is read again when its modification time moves. One stat per action call.
  private read(): Manifest | undefined {
    for (const file of this.files) {
      try {
        const { mtimeMs } = statSync(file);
        if (this.loaded?.file === file && this.loaded.mtimeMs === mtimeMs) return this.loaded.manifest;
        const manifest = JSON.parse(readFileSync(file, "utf8")) as Manifest;
        this.loaded = { file, mtimeMs, manifest };
        return manifest;
      } catch {
        // Not there (yet), or caught half-written: try the next place.
      }
    }
    return undefined;
  }
}

export function describeAction(entry: ManifestEntry, dir: string): ActionInfo {
  const info: ActionInfo = {};
  const name = entry.exportedName;
  if (name && !name.startsWith(INLINE_PREFIX)) info.name = name;
  if (entry.filename) {
    // Turbopack writes the path from the project, webpack sometimes an
    // absolute one.
    const file = isAbsolute(entry.filename) ? relative(dir, entry.filename) : entry.filename;
    info.file = inProject(file.split(sep).join("/").replace(/^\[project\]\//, ""), dir);
  }
  return info;
}

/**
 * Turbopack names a file from its root, which in a monorepo is above the
 * project: "apps/web/app/actions.ts" for the project in apps/web. The part
 * that exists under the project is the path from the project.
 */
function inProject(file: string, dir: string): string {
  const segments = file.split("/");
  for (let i = 0; i < segments.length; i++) {
    const candidate = segments.slice(i).join("/");
    if (existsSync(join(dir, candidate))) return candidate;
  }
  return file;
}
