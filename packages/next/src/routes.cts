// The app's routes, read from the `app/` and `pages/` trees the way Next.js
// reads them, so Leadline can list routes that exist but were never hit.
import { existsSync, readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative, sep } from "node:path";
import type { RouteEntry } from "./types.cjs";

export const DEFAULT_PAGE_EXTENSIONS = ["tsx", "ts", "jsx", "js"];

/** Files Next.js serves from the app tree besides pages and route handlers. */
const METADATA_FILES = new Set([
  "sitemap",
  "robots",
  "manifest",
  "icon",
  "apple-icon",
  "opengraph-image",
  "twitter-image",
]);

const HTTP_METHODS = /export\s+(?:async\s+)?(?:function\s+|const\s+|let\s+|var\s+)?(GET|HEAD|POST|PUT|PATCH|DELETE|OPTIONS)\b/g;

/** Where the route trees are: Next.js looks in the project and then in `src/`. */
export function routeDirs(dir: string): { app?: string; pages?: string; base: string } {
  for (const base of [dir, join(dir, "src")]) {
    const app = join(base, "app");
    const pages = join(base, "pages");
    const found = { app: isDir(app) ? app : undefined, pages: isDir(pages) ? pages : undefined, base };
    if (found.app || found.pages) return found;
  }
  return { base: dir };
}

/**
 * Every route of the project in `dir`, sorted, with paths relative to it.
 * Reads directory names and, for route handlers, the file to find its
 * methods; nothing is compiled or run.
 */
export function scanRoutes(dir: string, pageExtensions: string[] = DEFAULT_PAGE_EXTENSIONS): RouteEntry[] {
  const ext = new Set(pageExtensions.map((e) => e.replace(/^\./, "")));
  const out: RouteEntry[] = [];
  const { app, pages, base } = routeDirs(dir);
  const file = (path: string) => relative(dir, path).split(sep).join("/");

  if (app) walkApp(app, [], [], out, ext, file);
  if (pages) walkPages(pages, [], out, ext, file);
  // `proxy` is Next 16's name for the file; `middleware` the one before it.
  for (const name of ["proxy", "middleware"]) {
    const found = withExtension(base, name, ext);
    if (found) out.push({ route: "", kind: "proxy", router: app ? "app" : "pages", file: file(found), dynamic: false });
  }

  // By code unit, not by locale: the list is compared with the last one sent,
  // and must come out the same on every machine.
  const order = (a: string, b: string) => (a < b ? -1 : a > b ? 1 : 0);
  return out.sort((a, b) => order(a.route, b.route) || order(a.kind, b.kind) || order(a.file, b.file));
}

type Rel = (path: string) => string;

function walkApp(
  at: string,
  segments: string[],
  layouts: string[],
  out: RouteEntry[],
  ext: Set<string>,
  file: Rel,
): void {
  const entries = list(at);
  const route = appRoute(segments);
  const dynamic = route.includes("[");

  // This folder's layout wraps everything below it, its own page included.
  const layout = entries.files.get("layout");
  if (layout) {
    out.push({ route, kind: "layout", router: "app", file: file(join(at, layout)), dynamic });
    layouts = [...layouts, file(join(at, layout))];
  }
  for (const [name, filename] of entries.files) {
    const path = join(at, filename);
    if (name === "page") {
      out.push({ route, kind: "page", router: "app", file: file(path), dynamic, layouts });
    } else if (name === "route") {
      const methods = routeMethods(path);
      out.push({ route, kind: "route", router: "app", file: file(path), dynamic, ...(methods.length ? { methods } : {}) });
    } else if (METADATA_FILES.has(name)) {
      out.push({ route: joinRoute(route, name), kind: "metadata", router: "app", file: file(path), dynamic });
    }
  }
  for (const name of entries.dirs) {
    // A folder that starts with an underscore is private: no routes in it.
    // (`%5F` is how a route segment that really starts with one is written.)
    if (name.startsWith("_")) continue;
    walkApp(join(at, name), [...segments, name], layouts, out, ext, file);
  }

  function list(folder: string) {
    const files = new Map<string, string>();
    const dirs: string[] = [];
    for (const entry of safeReaddir(folder)) {
      if (entry.isDirectory()) dirs.push(entry.name);
      else {
        const dot = entry.name.lastIndexOf(".");
        if (dot > 0 && ext.has(entry.name.slice(dot + 1))) {
          const name = entry.name.slice(0, dot);
          // With both page.tsx and page.js, Next.js takes the first extension.
          if (!files.has(name)) files.set(name, entry.name);
        }
      }
    }
    return { files, dirs: dirs.sort() };
  }
}

/**
 * The route for a folder under `app/`, as Next.js normalises it: route groups
 * `(shop)` and parallel slots `@modal` are not part of the URL. An
 * intercepting segment such as `(.)photo` stays, as it does in `next.route`.
 */
export function appRoute(segments: string[]): string {
  const kept = segments.filter(
    (segment) => !(segment.startsWith("(") && segment.endsWith(")")) && !segment.startsWith("@"),
  );
  return "/" + kept.map((segment) => segment.replace(/^%5F/i, "_")).join("/");
}

function walkPages(at: string, segments: string[], out: RouteEntry[], ext: Set<string>, file: Rel): void {
  for (const entry of safeReaddir(at)) {
    const path = join(at, entry.name);
    if (entry.isDirectory()) {
      walkPages(path, [...segments, entry.name], out, ext, file);
      continue;
    }
    const dot = entry.name.lastIndexOf(".");
    if (dot <= 0 || !ext.has(entry.name.slice(dot + 1))) continue;
    const name = entry.name.slice(0, dot);
    // _app, _document and _error wrap pages; they are not routes.
    if (segments.length === 0 && name.startsWith("_")) continue;
    const parts = name === "index" ? segments : [...segments, name];
    const route = "/" + parts.join("/");
    out.push({
      route,
      kind: segments[0] === "api" || (segments.length === 0 && name === "api") ? "api" : "page",
      router: "pages",
      file: file(path),
      dynamic: route.includes("["),
    });
  }
}

/** The HTTP methods a route handler exports, read from its source. */
export function routeMethods(path: string): string[] {
  try {
    const source = readFileSync(path, "utf8");
    const found = new Set<string>();
    for (const match of source.matchAll(HTTP_METHODS)) found.add(match[1]!);
    return [...found];
  } catch {
    return [];
  }
}

function joinRoute(route: string, name: string): string {
  return route === "/" ? `/${name}` : `${route}/${name}`;
}

function withExtension(dir: string, name: string, ext: Set<string>): string | undefined {
  for (const e of ext) {
    const path = join(dir, `${name}.${e}`);
    if (existsSync(path)) return path;
  }
  return undefined;
}

function isDir(path: string): boolean {
  try {
    return statSync(path).isDirectory();
  } catch {
    return false;
  }
}

function safeReaddir(dir: string) {
  try {
    return readdirSync(dir, { withFileTypes: true });
  } catch {
    return [];
  }
}
