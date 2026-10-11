// What a request was, in Next.js's terms, worked out from what the request
// said about itself (its path and headers) and from the spans Next.js made
// while handling it. No span is needed to run this: it is plain data in,
// a name and attributes out.

/** What kind of thing answered a request. */
export type RequestKind =
  | "page" // a page, rendered to HTML or (next.rsc) to a Flight payload
  | "action" // a server action
  | "route" // an App Router route handler
  | "api" // a Pages Router API route
  | "not-found"
  | "error"
  | "asset" // a file from public/, or an optimised image
  | "proxy" // answered by proxy.ts / middleware.ts alone (a redirect, a rewrite elsewhere)
  | "unknown";

/** What the incoming request's own headers say. */
export interface RequestHints {
  method: string;
  /** The path, without the query. */
  path: string;
  /** `Next-Action`: the id of the server action being called. */
  actionId?: string;
  /** `RSC: 1`: the client router asking for a Flight payload, not HTML. */
  rsc: boolean;
  /** `Next-Router-Prefetch: 1`. */
  prefetch: boolean;
}

/** What Next.js's spans said while the request ran. */
export interface Seen {
  /** `next.route` as Next.js reported it: `/blog/[slug]`. */
  route?: string;
  /** The `next.span_type` of each Next.js span in the request. */
  types: Set<string>;
}

export interface ActionInfo {
  /** The exported name, or undefined for an action written inline. */
  name?: string;
  /** The file it is in, relative to the project. */
  file?: string;
}

export interface Classified {
  kind: RequestKind;
  /** The span name, which is what Leadline groups requests by. */
  name: string;
  /** `http.route`: the pattern, or the path when Next.js matched nothing. */
  route: string;
  attributes: Record<string, string | boolean>;
}

/**
 * Paths that are the dev server's own business: built chunks, hot reload,
 * the error overlay's lookups, the browser's unprompted requests. They are
 * not traced at all.
 */
export function isNoise(path: string): boolean {
  if (path.startsWith("/_next/")) {
    // Two things under /_next are the app's own work: Pages Router data
    // requests (getServerSideProps as JSON) and the image optimiser.
    return !(path.startsWith("/_next/data/") || path.startsWith("/_next/image"));
  }
  return (
    path.startsWith("/__nextjs") ||
    path.startsWith("/__leadline/") ||
    path === "/favicon.ico" ||
    path.startsWith("/.well-known/appspecific/")
  );
}

/** Reads the hints from an incoming request's method, URL and headers. */
export function readHints(
  method: string | undefined,
  url: string | undefined,
  headers: Record<string, string | string[] | undefined>,
): RequestHints {
  const one = (name: string) => {
    const value = headers[name];
    return Array.isArray(value) ? value[0] : value;
  };
  const actionId = one("next-action");
  return {
    method: (method ?? "GET").toUpperCase(),
    path: pathOf(url),
    ...(actionId ? { actionId } : {}),
    rsc: one("rsc") === "1",
    prefetch: one("next-router-prefetch") === "1",
  };
}

export function pathOf(url: string | undefined): string {
  if (!url) return "/";
  const end = url.search(/[?#]/);
  return end < 0 ? url : url.slice(0, end) || "/";
}

/**
 * Names a finished request. `status` is the response's; `action` is what the
 * action manifest says about `hints.actionId`, when it knows it.
 */
export function classify(hints: RequestHints, seen: Seen, status: number, action?: ActionInfo): Classified {
  const { method } = hints;
  const route = seen.route;
  const attributes: Record<string, string | boolean> = {};
  let kind: RequestKind;
  let name: string;

  if (hints.actionId) {
    // A server action is a POST to whatever page the form is on. The page is
    // incidental; the action is what ran, so it is what the request is named
    // after. The page stays in http.route, since Next.js renders it again in
    // the same response.
    kind = "action";
    attributes["next.action.id"] = hints.actionId;
    if (action?.file) attributes["next.action.file"] = action.file;
    if (action?.name) {
      attributes["next.action.name"] = action.name;
      name = `action ${action.name}`;
    } else if (action?.file) {
      name = `action (inline) ${action.file}`;
    } else {
      name = `action ${route ?? hints.path}`;
    }
  } else if (route === "/_not-found" || (route === "/404" && status === 404)) {
    kind = "not-found";
    name = `${method} ${route}`;
  } else if (route === "/_error" || route === "/500" || route === "/_global-error") {
    kind = "error";
    name = `${method} ${route}`;
  } else if (seen.types.has("AppRouteRouteHandlers.runHandler")) {
    kind = "route";
    name = `${method} ${route ?? hints.path}`;
  } else if (seen.types.has("Node.runHandler")) {
    kind = "api";
    name = `${method} ${route ?? hints.path}`;
  } else if (
    seen.types.has("AppRender.getBodyResult") ||
    seen.types.has("Render.renderDocument") ||
    seen.types.has("Render.getServerSideProps") ||
    seen.types.has("Render.getStaticProps")
  ) {
    kind = "page";
    // Next.js's own name for a Flight request: "RSC GET /blog/[slug]".
    name = `${hints.rsc ? "RSC " : ""}${method} ${route ?? hints.path}`;
  } else if (route) {
    // Next.js matched a route but none of the spans above ran: a cached or
    // prerendered answer, or a version of Next.js that names them otherwise.
    kind = "page";
    name = `${hints.rsc ? "RSC " : ""}${method} ${route}`;
  } else if (seen.types.has("BaseServer.handleRequest")) {
    // Next.js took the request and matched no route.
    kind = status === 404 ? "not-found" : "unknown";
    name = `${method} ${hints.path}`;
  } else if (seen.types.has("Middleware.execute") && (status < 200 || status >= 300)) {
    kind = "proxy";
    name = `${method} ${hints.path}`;
  } else {
    // Never reached the render server: a file from public/, or the image
    // optimiser.
    kind = status === 404 ? "not-found" : "asset";
    name = `${method} ${hints.path}`;
  }

  attributes["next.kind"] = kind;
  if (route) attributes["next.route"] = route;
  if (hints.rsc) attributes["next.rsc"] = true;
  if (hints.prefetch) attributes["next.prefetch"] = true;
  return { kind, name, route: route ?? hints.path, attributes };
}

/** Sorted, merged copies of the spans of time, so overlaps are counted once. */
export function mergeIntervals(intervals: ReadonlyArray<readonly [number, number]>): Array<[number, number]> {
  const sorted = intervals
    .filter(([start, end]) => end > start)
    .map(([start, end]) => [start, end] as [number, number])
    .sort((a, b) => a[0] - b[0]);
  const out: Array<[number, number]> = [];
  for (const interval of sorted) {
    const last = out[out.length - 1];
    if (last && interval[0] <= last[1]) last[1] = Math.max(last[1], interval[1]);
    else out.push(interval);
  }
  return out;
}

/** How much of the merged intervals falls inside [from, to]. */
export function covered(merged: ReadonlyArray<readonly [number, number]>, from: number, to: number): number {
  let total = 0;
  for (const [start, end] of merged) {
    const overlap = Math.min(end, to) - Math.max(start, from);
    if (overlap > 0) total += overlap;
  }
  return total;
}
