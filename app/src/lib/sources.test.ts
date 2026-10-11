import assert from "node:assert/strict";
import test from "node:test";
import type { Connection, Owner, Run, ServiceSource, Sources } from "./api.ts";
import { appHow, byProject, captured, connHow, connProblem, connTone, listed, model, ownerLine, plainVersion, ratePerMin, sampled, shortVersion, signals, tally, throughOf, total, versionOf, windowed } from "./sources.ts";

const NOW = 1_791_644_887_000;

const svc = (name: string, over: Partial<ServiceSource> = {}): ServiceSource => ({
  name,
  spans: 10,
  logs: 2,
  metric_points: 0,
  last_seen_ms: NOW - 1000,
  via: ["otlp"],
  ...over,
});

const conn = (over: Partial<Connection> = {}): Connection => ({
  id: "c_1",
  kind: "postgres",
  name: "acme",
  url: "postgres://acme:****@127.0.0.1:5432/acme",
  host: "127.0.0.1:5432",
  database: "acme",
  user: "acme",
  remote: false,
  capture: false,
  capture_offer: { available: true, how: "proxy", note: "" },
  health: { state: "ok", version: "17.10" },
  created_ms: NOW,
  ...over,
});

const run = (over: Partial<Run> = {}): Run => ({
  id: "r1",
  command: "pnpm dev",
  cwd: "/code/acme",
  service: "acme-web",
  pid: 1,
  terminal: true,
  running: true,
  started_ms: NOW - 60_000,
  ...over,
});

const sources = (over: Partial<Sources> = {}): Sources => ({ now_ms: NOW, otlp: { endpoints: [] }, services: [], proxies: [], connections: [], ...over });

test("nothing loaded is no sources", () => {
  const m = model(undefined, null);
  assert.equal(total(m), 0);
  assert.equal(tally(m), "None yet");
});

test("apps are in name order, databases and caches apart", () => {
  const m = model(
    sources({
      services: [svc("acme-web"), svc("acme-api")],
      connections: [conn({ id: "c_r", kind: "redis", name: "cache" }), conn()],
    }),
    [],
  );
  assert.deepEqual(m.apps.map((a) => a.service), ["acme-api", "acme-web"]);
  assert.deepEqual(m.databases.map((d) => d.conn.id), ["c_1"]);
  assert.deepEqual(m.caches.map((d) => d.conn.id), ["c_r"]);
  assert.deepEqual(m.databases[0].source, { kind: "db", id: "c_1", name: "acme", system: "postgresql", server: "127.0.0.1:5432" });
  assert.deepEqual(m.caches[0].source, { kind: "db", id: "c_r", name: "cache", system: "redis", server: "127.0.0.1:5432" });
  assert.equal(total(m), 4);
});

test("a captured Redis's own service is not an app", () => {
  const m = model(
    sources({
      services: [svc("acme-cache", { via: ["redis-monitor"], logs: 0 }), svc("acme-api")],
      connections: [conn({ id: "c_r", kind: "redis", name: "acme-cache", capture: true })],
    }),
    [],
  );
  assert.deepEqual(m.apps.map((a) => a.service), ["acme-api"]);
  // An app that happens to share the name but exports telemetry still shows.
  const both = model(
    sources({ services: [svc("acme-cache", { via: ["otlp", "redis-monitor"] })], connections: [conn({ kind: "redis", name: "acme-cache" })] }),
    [],
  );
  assert.equal(both.apps.length, 1);
});

test("an app is quiet after two minutes without a word", () => {
  const m = model(sources({ services: [svc("a", { last_seen_ms: NOW - 119_000 }), svc("b", { last_seen_ms: NOW - 12 * 60_000 })] }), []);
  assert.equal(m.apps[0].tone, "ok");
  assert.equal(m.apps[1].tone, "quiet");
  assert.equal(m.apps[1].how, "quiet for 12 minutes");
  assert.equal(tally(m), "1 connected, 1 quiet");
});

test("an app says how it is connected", () => {
  assert.equal(appHow(svc("a"), undefined, 1000), "OpenTelemetry");
  assert.equal(appHow(svc("a", { via: ["otlp", "postgres-proxy"] }), undefined, 1000), "OpenTelemetry, Postgres proxy");
  assert.equal(appHow(svc("a", { via: ["postgres-proxy"] }), undefined, 1000), "Queries only, through the proxy");
  assert.equal(appHow(svc("a"), run(), 1000), "pnpm dev · leadline run");
  // A run that has ended no longer speaks for the app.
  assert.equal(appHow(svc("a"), run({ running: false }), 1000), "OpenTelemetry");
});

test("a running command is a source before it has sent anything, and is not quiet while it runs", () => {
  const m = model(sources({ services: [svc("old", { last_seen_ms: NOW - 3_600_000 })] }), [run(), run({ id: "r0", service: "old", command: "go run ./cmd/api" })]);
  assert.deepEqual(m.apps.map((a) => [a.service, a.tone]), [
    ["acme-web", "wait"],
    ["old", "ok"],
  ]);
  assert.equal(m.apps[0].how, "pnpm dev · leadline run");
  assert.equal(m.apps[0].agoMs, Infinity);
  assert.equal(m.apps[1].run?.id, "r0");
});

test("the run shown for an app is the running one, else the newest", () => {
  const m = model(sources({ services: [svc("acme-web")] }), [
    run({ id: "old", running: false, started_ms: NOW - 9000 }),
    run({ id: "new", running: false, started_ms: NOW - 100 }),
  ]);
  assert.equal(m.apps[0].run?.id, "new");
  const live = model(sources({ services: [svc("acme-web")] }), [run({ id: "new", running: false, started_ms: NOW - 100 }), run({ id: "live", started_ms: NOW - 9000 })]);
  assert.equal(live.apps[0].run?.id, "live");
});

test("a connection says what it is and what Leadline does with it", () => {
  assert.equal(connHow(conn()), "Postgres 17 · stats only");
  assert.equal(connHow(conn({ capture: true })), "Postgres 17 · every query");
  assert.equal(connHow(conn({ kind: "redis", capture: true, health: { state: "ok", version: "7.4.11" } })), "Redis 7.4 · every command");
  assert.equal(connHow(conn({ health: { state: "unknown" } })), "Postgres · stats only");
  assert.equal(shortVersion("postgres", "17.10"), "17");
  assert.equal(shortVersion("redis", "7.4.11"), "7.4");
  assert.equal(shortVersion("redis", undefined), "");
  // A packaged Postgres says more than its number.
  assert.equal(plainVersion("17.11 (Debian 17.11-1.pgdg13+2)"), "17.11");
  assert.equal(shortVersion("postgres", "17.11 (Debian 17.11-1.pgdg13+2)"), "17");
  assert.equal(plainVersion(undefined), "");
});

test("a connection that fails says why, in the daemon's words", () => {
  const down = conn({ health: { state: "error", code: "unreachable", message: "Nothing is listening at 127.0.0.1:5432.", hint: "Is Postgres running?" } });
  assert.equal(connTone(down), "err");
  assert.equal(connHow(down), "Nothing is listening at 127.0.0.1:5432.");
  assert.deepEqual(connProblem(down), { message: "Nothing is listening at 127.0.0.1:5432.", hint: "Is Postgres running?" });
  assert.equal(connProblem(conn()), null);
  assert.equal(connTone(conn({ health: { state: "unknown" } })), "wait");
  assert.equal(tally(model(sources({ connections: [down, conn({ id: "c_2" })] }), [])), "1 connected, 1 failing");
});

test("a capture that stopped is a failure of its own", () => {
  const proxy = { id: "p", kind: "postgres", listen: "", port: 15432, upstream: "", origins: [], connection_ids: [], state: "error" as const, error: "port 15432 is in use", connections_open: 0, connections_total: 0, statements: 0, failed_statements: 0, unread_connections: 0, last_activity_ms: 0 };
  const c = conn({ capture: true, proxy });
  assert.equal(connTone(c), "err");
  assert.equal(connHow(c), "Capture has stopped");
  assert.equal(connProblem(c)?.message, "Its queries aren't being captured: port 15432 is in use.");
});

test("signals reads as a list", () => {
  assert.equal(signals(svc("a", { spans: 1, logs: 1, metric_points: 1 })), "traces, logs and metrics");
  assert.equal(signals(svc("a", { spans: 1, logs: 0, metric_points: 1 })), "traces and metrics");
  assert.equal(signals(svc("a", { spans: 1, logs: 0 })), "traces");
  assert.equal(signals(svc("a", { spans: 0, logs: 0 })), "");
});

test("a counter's readings make a rate per minute", () => {
  let s = sampled([], 0, 100);
  assert.equal(ratePerMin(s), null);
  s = sampled(s, 2000, 110);
  // Two seconds is too short to say.
  assert.equal(ratePerMin(s), null);
  s = sampled(s, 10_000, 190);
  assert.equal(ratePerMin(s), 540);
  // Readings older than a minute fall away.
  s = sampled(s, 65_000, 190);
  assert.deepEqual(s.map((x) => x.t), [10_000, 65_000]);
  assert.equal(ratePerMin(s), 0);
});

test("a counter that fell starts the readings again", () => {
  let s = sampled(sampled([], 0, 500), 5000, 600);
  s = sampled(s, 10_000, 3);
  assert.deepEqual(s, [{ t: 10_000, n: 3 }]);
  // The same moment twice adds nothing.
  assert.equal(sampled(s, 10_000, 3), s);
});

test("a connection's counter is its capture's", () => {
  assert.equal(captured(conn()), null);
  const monitor = { state: "streaming" as const, seen: 6, recorded: 6, dropped: 0, max_per_sec: 1000, last_activity_ms: 0, note: "" };
  assert.equal(captured(conn({ kind: "redis", monitor })), 6);
});

const proxy = (over: Partial<NonNullable<Connection["proxy"]>> = {}): NonNullable<Connection["proxy"]> => ({
  id: "p_1",
  kind: "postgres",
  listen: "127.0.0.1:15432",
  port: 15432,
  upstream: "127.0.0.1:5432",
  origins: ["connection"],
  connection_ids: ["c_1"],
  state: "listening",
  connections_open: 0,
  connections_total: 1,
  statements: 400,
  failed_statements: 0,
  unread_connections: 0,
  last_activity_ms: NOW,
  ...over,
});

test("an app is tied to the database its calls were captured on the way to", () => {
  const pg = conn({ capture: true, proxy: proxy() });
  const cache = conn({ id: "c_r", kind: "redis", name: "cache" });
  const s = sources({
    services: [
      svc("acme-web", { via: ["otlp", "postgres-proxy"], through: [{ via: "postgres-proxy", proxy_id: "p_1", connection_ids: ["c_1"] }] }),
      svc("acme-api", { via: ["otlp", "postgres-proxy", "redis-monitor"], through: [{ via: "postgres-proxy", proxy_id: "p_1", connection_ids: ["c_1"] }, { via: "redis-monitor", connection_ids: ["c_r"] }] }),
      svc("acme-worker"),
    ],
    proxies: [proxy()],
    connections: [pg, cache],
  });
  const m = model(s, []);
  assert.deepEqual(m.apps.map((a) => [a.service, a.through.map((v) => v.name)]), [
    ["acme-api", ["acme", "cache"]],
    ["acme-web", ["acme"]],
    ["acme-worker", []],
  ]);
  assert.deepEqual(m.databases[0].apps, ["acme-api", "acme-web"]);
  assert.deepEqual(m.caches[0].apps, ["acme-api"]);
  // The scope a database row narrows to tells two servers of one system apart.
  assert.deepEqual(m.databases[0].source, { kind: "db", id: "c_1", name: "acme", system: "postgresql", server: "127.0.0.1:5432" });
});

test("a proxy leadline run opened is named by where it forwards to", () => {
  const run = proxy({ id: "p_run", origins: ["run"], connection_ids: [], upstream: "127.0.0.1:54399" });
  const s = sources({ proxies: [run] });
  assert.deepEqual(throughOf(svc("acme-web", { through: [{ via: "postgres-proxy", proxy_id: "p_run", connection_ids: [] }] }), s), [{ name: "127.0.0.1:54399", kind: "postgres" }]);
  // A proxy that has gone, a connection that was removed, and a daemon that says nothing: no tie, and no failure.
  assert.deepEqual(throughOf(svc("x", { through: [{ via: "postgres-proxy", proxy_id: "p_gone", connection_ids: ["c_gone"] }] }), s), []);
  assert.deepEqual(throughOf(svc("x"), s), []);
  // Named once however many times it is listed.
  const twice = svc("x", { through: [{ via: "postgres-proxy", proxy_id: "p_run" }, { via: "postgres-proxy", proxy_id: "p_run" }] });
  assert.equal(throughOf(twice, s).length, 1);
});

test("names read as a sentence lists them", () => {
  assert.equal(listed([]), "");
  assert.equal(listed(["a"]), "a");
  assert.equal(listed(["a", "b"]), "a and b");
  assert.equal(listed(["a", "b", "c"]), "a, b and c");
});

test("a capture's count in the window comes from the daemon when it gives one", () => {
  assert.equal(windowed(conn()), null);
  assert.equal(windowed(conn({ proxy: proxy() })), null);
  assert.equal(windowed(conn({ proxy: proxy({ window_statements: 38 }) })), 38);
  assert.equal(windowed(conn({ proxy: proxy({ window_statements: 0 }) })), 0);
  const monitor = { state: "streaming" as const, seen: 9, recorded: 9, dropped: 0, max_per_sec: 1000, last_activity_ms: NOW, note: "", window_recorded: 4 };
  assert.equal(windowed(conn({ kind: "redis", monitor })), 4);
});

test("the daemon's short version wins over ours of the long one", () => {
  assert.equal(versionOf({ version: "17.11 (Debian 17.11-1.pgdg13+2)", version_short: "17.11" }), "17.11");
  assert.equal(versionOf({ version: "17.11 (Debian 17.11-1.pgdg13+2)" }), "17.11");
  assert.equal(versionOf({}), "");
  assert.equal(versionOf(undefined), "");
  assert.equal(connHow(conn({ health: { state: "ok", version: "17.11 (Debian 17.11-1.pgdg13+2)", version_short: "17.11" } })), "Postgres 17 · stats only");
});

const worktree = (worktree: string | undefined, over: Partial<Owner> = {}): Owner => ({
  kind: "worktree",
  via: "shipyard",
  service: worktree ? `acme/${worktree}` : "acme",
  project: "acme",
  worktree,
  branch: worktree ?? "main",
  label: `from Shipyard, worktree ${worktree ?? "main"}`,
  ...over,
});

test("who made a source is said only when it is worth saying", () => {
  assert.equal(ownerLine(undefined), null);
  assert.equal(ownerLine({ kind: "user", label: "added by hand" }), null);
  assert.equal(ownerLine(worktree("fix-orders")), "from Shipyard · fix-orders");
  assert.equal(ownerLine(worktree("fix-orders", { via: "cli" })), "attached · fix-orders");
  assert.equal(ownerLine(worktree(undefined)), "from Shipyard · main");
  assert.equal(ownerLine(worktree(undefined, { branch: undefined })), "from Shipyard · main checkout");
  assert.equal(ownerLine({ kind: "run", label: "leadline run" }), "leadline run");
  assert.equal(ownerLine({ kind: "flag", label: "the daemon's -pg flag" }), "the daemon's -pg flag");
  // A database a scan found says so, and where its URL was.
  const scanned = { kind: "scan", label: "found by a scan, DATABASE_URL in acme-web", project: "acme-web", path: "/Users/acme/code/acme-web", env_key: "DATABASE_URL", env_file: ".env.local" };
  assert.equal(ownerLine(scanned), "found in acme-web · DATABASE_URL");
  assert.equal(ownerLine({ kind: "scan", label: "found by a scan, in acme-web", project: "acme-web" }), "found in acme-web");
  assert.equal(ownerLine({ kind: "scan", label: "found by a scan, REDIS_URL", env_key: "REDIS_URL" }), "found by a scan · REDIS_URL");
  assert.equal(ownerLine({ kind: "scan", label: "found by a scan" }), "found by a scan");
  // A kind this app has not met is said in the daemon's own words.
  assert.equal(ownerLine({ kind: "something-new", label: "made by something new" }), "made by something new");
});

test("with no owners anywhere the sidebar's rows are as they were", () => {
  const rows = ["acme-api", "acme-web"];
  const plain = [{ kind: "row", row: "acme-api" }, { kind: "row", row: "acme-web" }];
  assert.deepEqual(byProject(rows, () => undefined, (r) => r), plain);
  assert.deepEqual(byProject(rows, () => ({ kind: "user", label: "added by hand" }), (r) => r), plain);
});

test("the worktrees of one project stand together under its name, where the first stood", () => {
  const owners: Record<string, Owner | undefined> = {
    acme: worktree(undefined),
    "acme-admin": undefined,
    "acme/checkout-retry": worktree("checkout-retry"),
    "acme/fix-orders": worktree("fix-orders"),
    "acme/fix-orders:worker": worktree("fix-orders"),
    "billing/main": { ...worktree("main"), project: "billing", service: "billing/main" },
    zeta: undefined,
  };
  const out = byProject(Object.keys(owners), (r) => owners[r], (r) => r);
  assert.deepEqual(out, [
    {
      kind: "project",
      project: "acme",
      shipyard: true,
      rows: [
        { row: "acme", name: "main checkout" },
        { row: "acme/checkout-retry", name: "checkout-retry" },
        { row: "acme/fix-orders", name: "fix-orders" },
        { row: "acme/fix-orders:worker", name: "fix-orders:worker" },
      ],
    },
    { kind: "row", row: "acme-admin" },
    // One worktree is not a group.
    { kind: "row", row: "billing/main" },
    { kind: "row", row: "zeta" },
  ]);
});

test("a worktree's connections are called by the worktree and the database", () => {
  const conns = [
    conn({ id: "c_1", name: "acme/fix-orders · acme_fix_orders", owner: worktree("fix-orders", { via: "cli" }) }),
    conn({ id: "c_2", name: "acme_dev", owner: { kind: "user", label: "added by hand" } }),
    conn({ id: "c_3", name: "acme/checkout-retry · acme_checkout", owner: worktree("checkout-retry", { via: "cli" }) }),
    // Renamed by a person: its own name is kept.
    conn({ id: "c_4", name: "scratch", owner: worktree("fix-orders", { via: "cli" }) }),
  ];
  const out = byProject(conns, (c) => c.owner, (c) => c.name);
  assert.equal(out.length, 2);
  const group = out[0];
  assert.ok(group.kind === "project");
  assert.equal(group.project, "acme");
  assert.equal(group.shipyard, false);
  assert.deepEqual(group.rows.map((r) => r.name), ["fix-orders · acme_fix_orders", "checkout-retry · acme_checkout", "scratch"]);
  assert.ok(out[1].kind === "row" && out[1].row.id === "c_2");
});

test("a service's owner travels with its row", () => {
  const m = model(sources({ services: [svc("acme/fix-orders", { owner: worktree("fix-orders"), last_seen_ms: 0, spans: 0, logs: 0, via: [] }), svc("acme-web")] }), []);
  const attached = m.apps.find((a) => a.service === "acme/fix-orders");
  assert.equal(attached?.owner?.worktree, "fix-orders");
  // Attached, and nothing has arrived from it yet.
  assert.equal(attached?.how, "Nothing yet");
  assert.equal(m.apps.find((a) => a.service === "acme-web")?.owner, undefined);
});
