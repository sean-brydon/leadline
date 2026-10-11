// The routes an app has that nobody asked for (the daemon's `never_hit`),
// as the Endpoints view lists them. Kept apart from React so it can be tested.

/** The fields of a route this file reads; api.ts's QuietRoute fits it. */
export type RouteLike = { service: string; route: string; kind: string; file?: string };

/**
 * quietRoutes keeps the routes that match what the list is filtered by (a
 * part of the route or of its file, whatever the case), since the daemon
 * filters only the endpoints that had requests.
 */
export function quietRoutes<R extends RouteLike>(routes: R[], search: string): R[] {
  const q = search.trim().toLowerCase();
  if (!q) return routes;
  return routes.filter((r) => r.route.toLowerCase().includes(q) || (r.file ?? "").toLowerCase().includes(q));
}
