// What is feeding Leadline, as the sidebar and the Sources page say it: the
// daemon's answer (/api/sources, /api/runs) turned into rows with a status
// and a sentence each. Kept apart from React so it can be tested.

import type { Connection, Health, Owner, Proxy, Run, ServiceSource, Sources } from "./api";
import { fmtFor } from "./format.ts";
import { dbSystem, type Source } from "./scope.ts";

/** How a source is doing: sending, gone quiet, failing, or not yet known. */
export type Tone = "ok" | "quiet" | "err" | "wait";

// An app is quiet when nothing has come from it for this long. Metrics
// arrive once a minute, so anything shorter would flicker.
export const QUIET_AFTER_MS = 2 * 60_000;

export type AppRow = {
  service: string;
  source: Source;
  tone: Tone;
  /** How it is connected, in a few words: the sidebar's second line. */
  how: string;
  /** How long since it was last heard; Infinity when never. */
  agoMs: number;
  /** What the daemon holds for it. A run that has sent nothing yet has zeros. */
  held: ServiceSource;
  /** The `leadline run` that started it, when there is one: the newest, a running one first. */
  run?: Run;
  /** The captures its database calls came through: saved connections by name, or a proxy that `leadline run` opened. */
  through: Via[];
  /** Who made it, when it belongs to an attached worktree. */
  owner?: Owner;
};

/** One capture an app's calls came through. `id` is the saved connection's, when the capture is one's. */
export type Via = { id?: string; name: string; kind: "postgres" | "redis" };

export type DbRow = {
  conn: Connection;
  source: Source;
  tone: Tone;
  how: string;
  /** The apps whose calls were captured on their way to it, in name order. */
  apps: string[];
};

export type Model = { apps: AppRow[]; databases: DbRow[]; caches: DbRow[] };

/** kindName is what a connection's kind is called. */
export function kindName(kind: string): string {
  return kind === "postgres" ? "Postgres" : kind === "redis" ? "Redis" : kind;
}

/** plainVersion is a server's version number without what the packager added: "17.11 (Debian 17.11-1.pgdg13+2)" is 17.11. */
export function plainVersion(version: string | undefined): string {
  return version?.trim().split(/\s+/)[0] ?? "";
}

/** versionOf is a server's version number as its health check gave it: the daemon's own short form, or ours of the long one. */
export function versionOf(h: Pick<Health, "version" | "version_short"> | undefined): string {
  return h?.version_short || plainVersion(h?.version);
}

/** kindVersion names a server with its version: "Postgres 17.11", or "Postgres" before it has said. */
export function kindVersion(kind: string, version: string | undefined): string {
  const v = plainVersion(version);
  return v ? `${kindName(kind)} ${v}` : kindName(kind);
}

/** shortVersion is a server's version as people say it: Postgres 17, Redis 7.4. */
export function shortVersion(kind: string, version: string | undefined): string {
  if (!version) return "";
  const parts = plainVersion(version).split(".");
  return kind === "postgres" ? parts[0] : parts.slice(0, 2).join(".");
}

/** captureWord is what capture records for a kind of connection. */
export const captureWord = (kind: string): string => (kind === "redis" ? "command" : "query");

/** connTone is a saved connection's status: the server's health first, then its capture's. */
export function connTone(c: Connection): Tone {
  if (c.health.state === "error") return "err";
  if (c.proxy?.state === "error" || c.monitor?.state === "error") return "err";
  if (c.health.state === "unknown" || c.monitor?.state === "connecting") return "wait";
  return "ok";
}

/** connProblem is what is wrong with a connection, in the daemon's words; null when nothing is. */
export function connProblem(c: Connection): { message: string; hint?: string } | null {
  if (c.health.state === "error") return { message: c.health.message || "It can't be reached.", hint: c.health.hint };
  if (c.proxy?.state === "error") return { message: `Its queries aren't being captured: ${c.proxy.error || "the proxy's port can't be opened"}.`.replace(/\.\.$/, ".") };
  if (c.monitor?.state === "error") return { message: `Its commands aren't being captured: ${c.monitor.error || "MONITOR failed"}.`.replace(/\.\.$/, ".") };
  return null;
}

/** connHow is a connection's second line in the sidebar: what it is and what Leadline does with it. */
export function connHow(c: Connection): string {
  if (c.health.state === "error") return c.health.message || "Can't be reached";
  if (c.proxy?.state === "error" || c.monitor?.state === "error") return "Capture has stopped";
  const version = shortVersion(c.kind, versionOf(c.health));
  const what = `${kindName(c.kind)}${version ? ` ${version}` : ""}`;
  return `${what} · ${c.capture ? `every ${captureWord(c.kind)}` : "stats only"}`;
}

// The services a capture writes under a name of its own are not apps: a
// captured Redis records its commands under the connection's name, and the
// connection's row already stands for it.
function isCapture(s: ServiceSource, connections: Connection[]): boolean {
  return s.via.length === 1 && s.via[0] === "redis-monitor" && connections.some((c) => c.kind === "redis" && c.name === s.name);
}

/**
 * throughOf names the captures a service's database calls came through:
 * what ties an app to its database. A proxy that is no saved connection's
 * (one `leadline run` opened) is named by where it forwards to.
 */
export function throughOf(s: ServiceSource, sources: Pick<Sources, "connections" | "proxies">): Via[] {
  const out: Via[] = [];
  const add = (v: Via) => {
    if (!out.some((o) => (v.id ? o.id === v.id : !o.id && o.name === v.name))) out.push(v);
  };
  for (const t of s.through ?? []) {
    const kind = t.via === "redis-monitor" ? "redis" : "postgres";
    const conns = (t.connection_ids ?? []).map((id) => sources.connections.find((c) => c.id === id)).filter((c): c is Connection => !!c);
    for (const c of conns) add({ id: c.id, name: c.name, kind });
    if (conns.length === 0 && t.proxy_id) {
      const p: Proxy | undefined = sources.proxies.find((x) => x.id === t.proxy_id);
      if (p) add({ name: p.upstream, kind });
    }
  }
  return out;
}

/** listed joins names as a sentence does: "a", "a and b", "a, b and c". */
export function listed(names: string[]): string {
  if (names.length <= 1) return names[0] ?? "";
  return `${names.slice(0, -1).join(", ")} and ${names[names.length - 1]}`;
}

function runFor(service: string, runs: Run[]): Run | undefined {
  const mine = runs.filter((r) => r.service === service);
  return mine.find((r) => r.running) ?? mine.sort((a, b) => b.started_ms - a.started_ms)[0];
}

/** appHow is an app's second line in the sidebar. */
export function appHow(s: ServiceSource, run: Run | undefined, agoMs: number): string {
  if (run?.running) return `${run.command} · leadline run`;
  if (agoMs === Infinity) return "Nothing yet";
  if (agoMs > QUIET_AFTER_MS) return `quiet for ${fmtFor(agoMs)}`;
  const otlp = s.via.includes("otlp");
  const proxy = s.via.includes("postgres-proxy");
  if (otlp) return proxy ? "OpenTelemetry, Postgres proxy" : "OpenTelemetry";
  if (proxy) return "Queries only, through the proxy";
  if (s.via.includes("redis-monitor")) return "Redis commands only";
  return "OpenTelemetry";
}

/** signals says which kinds of telemetry Leadline holds from a service: "traces, logs and metrics". */
export function signals(s: ServiceSource): string {
  const have = [s.spans > 0 && "traces", s.logs > 0 && "logs", s.metric_points > 0 && "metrics"].filter(Boolean) as string[];
  if (have.length === 0) return "";
  if (have.length === 1) return have[0];
  return `${have.slice(0, -1).join(", ")} and ${have[have.length - 1]}`;
}

/**
 * model sorts what the daemon reports into the sidebar's three groups. Apps
 * are in name order, so a row keeps its place while others come and go;
 * connections are in the order they were added.
 */
export function model(sources: Sources | undefined, runs: Run[] | null | undefined): Model {
  if (!sources) return { apps: [], databases: [], caches: [] };
  const all = runs ?? [];
  const now = sources.now_ms;
  const services = sources.services.filter((s) => !isCapture(s, sources.connections));
  // A command just started with `leadline run` is a source before its first line arrives.
  for (const r of all) {
    if (r.running && r.service && !sources.services.some((s) => s.name === r.service)) {
      services.push({ name: r.service, spans: 0, logs: 0, metric_points: 0, last_seen_ms: 0, via: [] });
    }
  }
  const seen = new Set<string>();
  const apps: AppRow[] = [];
  for (const s of services) {
    if (seen.has(s.name)) continue;
    seen.add(s.name);
    const run = runFor(s.name, all);
    const agoMs = s.last_seen_ms > 0 ? Math.max(0, now - s.last_seen_ms) : Infinity;
    const tone: Tone = run?.running ? (agoMs === Infinity ? "wait" : "ok") : agoMs > QUIET_AFTER_MS ? "quiet" : "ok";
    apps.push({ service: s.name, source: { kind: "app", service: s.name }, tone, how: appHow(s, run, agoMs), agoMs, held: s, run, through: throughOf(s, sources), owner: s.owner });
  }
  apps.sort((a, b) => a.service.localeCompare(b.service));

  const row = (c: Connection): DbRow => ({
    conn: c,
    source: { kind: "db", id: c.id, name: c.name, system: dbSystem(c.kind), server: c.host },
    tone: connTone(c),
    how: connHow(c),
    apps: apps.filter((a) => a.through.some((v) => v.id === c.id)).map((a) => a.service),
  });
  return {
    apps,
    databases: sources.connections.filter((c) => c.kind !== "redis").map(row),
    caches: sources.connections.filter((c) => c.kind === "redis").map(row),
  };
}

/**
 * ownerLine says who made a source, for the quiet line of its row: "from
 * Shipyard · fix-orders", or for a database a scan found, where its URL
 * was: "found in acme-web · DATABASE_URL". It is short and leads with what
 * tells one row from the next, since the line is cut where the row ends.
 * Null for one a person added by hand, and for one with no owner at all
 * (an older daemon): there is nothing to say.
 */
export function ownerLine(o: Owner | undefined): string | null {
  if (!o || o.kind === "user") return null;
  if (o.kind === "worktree") return `${o.via === "shipyard" ? "from Shipyard" : "attached"} · ${o.worktree || o.branch || "main checkout"}`;
  if (o.kind === "scan") return [o.project ? `found in ${o.project}` : "found by a scan", o.env_key].filter(Boolean).join(" · ");
  return o.label || null;
}

/** A sidebar entry: a row by itself, or the rows of one project's worktrees under its name. */
export type Entry<R> = { kind: "row"; row: R } | { kind: "project"; project: string; shipyard: boolean; rows: { row: R; name: string }[] };

/**
 * byProject gathers the rows that are worktrees of one project, so that
 * three checkouts of acme read as one project and not three strangers. A
 * project with a single worktree is left as the row it is. A group stands
 * where its first row stood. `full` is the row's whole name (a service, a
 * connection); inside a group it is cut to what the group doesn't already
 * say: the worktree, and what follows the source's own name.
 */
export function byProject<R>(rows: R[], ownerOf: (r: R) => Owner | undefined, full: (r: R) => string): Entry<R>[] {
  const project = (r: R) => {
    const o = ownerOf(r);
    return o?.kind === "worktree" && o.project ? o.project : "";
  };
  const count = new Map<string, number>();
  for (const r of rows) if (project(r)) count.set(project(r), (count.get(project(r)) ?? 0) + 1);
  const out: Entry<R>[] = [];
  const groups = new Map<string, { row: R; name: string }[]>();
  for (const r of rows) {
    const p = project(r);
    if (!p || (count.get(p) ?? 0) < 2) {
      out.push({ kind: "row", row: r });
      continue;
    }
    const o = ownerOf(r) as Owner;
    const wt = o.worktree || "main checkout";
    const name = full(r);
    // "acme/fix-orders:worker" is fix-orders:worker; "acme/fix-orders · acme_dev" is fix-orders · acme_dev.
    const short = o.service && name.startsWith(o.service) ? `${wt}${name.slice(o.service.length)}` : name;
    let g = groups.get(p);
    if (!g) {
      g = [];
      groups.set(p, g);
      out.push({ kind: "project", project: p, shipyard: o.via === "shipyard", rows: g });
    }
    g.push({ row: r, name: short });
  }
  return out;
}

/** total is how many sources there are. */
export const total = (m: Model): number => m.apps.length + m.databases.length + m.caches.length;

/** tally counts the sources by how they are doing, for the line under the Sources page's title: "7 connected, 1 quiet". */
export function tally(m: Model): string {
  const tones = [...m.apps, ...m.databases, ...m.caches].map((r) => r.tone);
  const n = (t: Tone) => tones.filter((x) => x === t).length;
  const parts = [
    `${n("ok") + n("wait")} connected`,
    n("quiet") > 0 && `${n("quiet")} quiet`,
    n("err") > 0 && `${n("err")} failing`,
  ].filter(Boolean);
  return tones.length === 0 ? "None yet" : parts.join(", ");
}

// A counter that only goes up (a proxy's statements, a monitor's commands),
// sampled each time the sources are fetched, gives a rate without asking
// the daemon for one.
export type Sample = { t: number; n: number };

/** sampled adds a reading and drops those older than keepMs. A counter that fell has started again (the daemon restarted), and so does the list. */
export function sampled(samples: Sample[], t: number, n: number, keepMs = 60_000): Sample[] {
  const last = samples[samples.length - 1];
  if (last && n < last.n) return [{ t, n }];
  if (last && t <= last.t) return samples;
  return [...samples.filter((s) => t - s.t <= keepMs), { t, n }];
}

/** ratePerMin is the counter's rise per minute over the readings held; null until they span long enough to say. */
export function ratePerMin(samples: Sample[], minSpanMs = 4000): number | null {
  if (samples.length < 2) return null;
  const a = samples[0];
  const b = samples[samples.length - 1];
  const span = b.t - a.t;
  if (span < minSpanMs) return null;
  return ((b.n - a.n) / span) * 60_000;
}

/**
 * windowed is what a connection's capture recorded since the sources'
 * `since_ms`: statements through its proxy, or Redis commands kept. Null
 * when it has no capture, or the daemon is too old to count by window.
 */
export function windowed(c: Connection): number | null {
  return c.proxy?.window_statements ?? c.monitor?.window_recorded ?? null;
}

/** captured is a connection's capture counter: statements through its proxy or commands MONITOR reported; null when it has no capture. */
export function captured(c: Connection): number | null {
  if (c.proxy) return c.proxy.statements;
  if (c.monitor) return c.monitor.seen;
  return null;
}
