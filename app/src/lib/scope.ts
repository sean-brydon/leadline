// What the window is narrowed to. Picking a source in the sidebar narrows
// every view to it; this file says what that means for each kind of source
// and each view, and nothing here touches React or the network.

/** The sidebar's choice: everything, one app, or one saved database. */
export type Source =
  | { kind: "all" }
  /** An app is a service name, as its telemetry carries it. */
  | { kind: "app"; service: string }
  /**
   * A saved connection. `system` is the db_system its calls are recorded
   * under and `server` its host:port, which tells two Postgres apart.
   */
  | { kind: "db"; id: string; name: string; system: string; server: string };

export const ALL: Source = { kind: "all" };

/**
 * What a request to the daemon is narrowed by. `service` is an app. `db` is
 * a database system ("postgresql", "redis") and `dbServer` one server of it
 * (a host:port): the traces, queries, logs and endpoints take both as
 * filters; the charts of every request do not (see narrowed).
 */
export type Scope = { sinceMs: number; service: string; db: string; dbServer: string };

/** The views a source can narrow. */
export type View = "Overview" | "Endpoints" | "Traces" | "Queries" | "Logs" | "Metrics";

/** dbSystem is the db_system a saved connection's calls are recorded under. */
export function dbSystem(kind: string): string {
  return kind === "postgres" ? "postgresql" : kind;
}

/** scopeOf turns the sidebar's choice and the window's start into what a request carries. */
export function scopeOf(source: Source, sinceMs: number): Scope {
  return {
    sinceMs,
    service: source.kind === "app" ? source.service : "",
    db: source.kind === "db" ? source.system : "",
    dbServer: source.kind === "db" ? source.server : "",
  };
}

/** scopeKey names what a scope narrows to, whenever its window starts: what a view refetches on. */
export function scopeKey(s: Scope): string {
  return `${s.service}\n${s.db}\n${s.dbServer}`;
}

/** dbParams are the daemon's two database filters, left out when the scope has none. */
export function dbParams(s: Scope): { db_system?: string; db_server?: string } {
  return { db_system: s.db || undefined, db_server: s.dbServer || undefined };
}

/** sourceKey names a source, for comparing two and for a list's key. */
export function sourceKey(s: Source): string {
  return s.kind === "all" ? "all" : s.kind === "app" ? `app:${s.service}` : `db:${s.id}`;
}

export const sameSource = (a: Source, b: Source): boolean => sourceKey(a) === sourceKey(b);

/** sourceName is what the top bar calls the source. */
export function sourceName(s: Source): string {
  return s.kind === "all" ? "All sources" : s.kind === "app" ? s.service : s.name;
}

/**
 * narrowed says whether a view shows only the source's own data.
 *
 * An app narrows every view: the daemon filters by service.
 *
 * A database narrows what the daemon can filter by it: its queries, the
 * traces and endpoints whose requests sent it a call, and the logs written
 * during those requests. The Overview's figures and lists are its own too;
 * its one chart is cut by system, not by server, and says so itself when
 * that makes a difference (chartNote). A metric doesn't say which database
 * it is about: that view stays on every source, and says so (scopeNote).
 */
export function narrowed(view: View, s: Source): boolean {
  if (s.kind !== "db") return true;
  return view !== "Metrics";
}

/** scopeNote is the sentence a view shows when it could not narrow to the source; null when it did. */
export function scopeNote(view: View, s: Source): string | null {
  if (s.kind !== "db" || narrowed(view, s)) return null;
  return `A database sends Leadline no metrics, so these are every app's. Its own numbers are in its stats, under Sources.`;
}

/**
 * chartNote is what to say under a chart that counts a whole database
 * system when the window is narrowed to one server of it. The chart can't
 * be asked for one server, so when it holds more calls than the server's
 * own list does, the difference is said; null when they agree. `all` and
 * `own` are counts of calls, `name` the connection's, `system` as people
 * write it ("Postgres").
 */
export function chartNote(name: string, system: string, all: number, own: number): string | null {
  // The two are counted a moment apart while data arrives: a few calls' difference is not another server.
  if (all - own <= Math.max(2, all * 0.02)) return null;
  const n = (v: number) => Math.round(v).toLocaleString("en-US");
  // An app's own spans don't always carry the server's address, and then nothing ties them to a connection.
  if (own <= 0) return `The chart counts every ${system} call Leadline holds, ${n(all)} in this window. None of them says it reached ${name}: an app's own spans don't always name the server they called.`;
  return `The chart counts every ${system} call Leadline holds, ${n(all)} in this window. ${n(own)} of them reached ${name}; the figures and lists here are ${name}'s alone.`;
}

/** groupsInDb picks the scope's database system out of a chart split by system; every group when there is none. */
export function groupsInDb<G extends { key: string }>(groups: G[], db: string): G[] {
  return db ? groups.filter((g) => g.key === db) : groups;
}
