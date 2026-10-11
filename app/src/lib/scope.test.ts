import assert from "node:assert/strict";
import test from "node:test";
import { ALL, chartNote, dbParams, dbSystem, groupsInDb, narrowed, sameSource, scopeKey, scopeNote, scopeOf, sourceKey, sourceName, type Source, type View } from "./scope.ts";

const api: Source = { kind: "app", service: "acme-api" };
const pg: Source = { kind: "db", id: "c_1", name: "acme", system: "postgresql", server: "127.0.0.1:5432" };
const VIEWS: View[] = ["Overview", "Endpoints", "Traces", "Queries", "Logs", "Metrics"];

test("every source asks the daemon for nothing narrower than the window", () => {
  assert.deepEqual(scopeOf(ALL, 10), { sinceMs: 10, service: "", db: "", dbServer: "" });
  assert.deepEqual(dbParams(scopeOf(ALL, 10)), { db_system: undefined, db_server: undefined });
});

test("an app is a service filter", () => {
  assert.deepEqual(scopeOf(api, 10), { sinceMs: 10, service: "acme-api", db: "", dbServer: "" });
});

test("a database is its system and its server, and no service", () => {
  assert.deepEqual(scopeOf(pg, 10), { sinceMs: 10, service: "", db: "postgresql", dbServer: "127.0.0.1:5432" });
  assert.deepEqual(dbParams(scopeOf(pg, 10)), { db_system: "postgresql", db_server: "127.0.0.1:5432" });
  assert.equal(dbSystem("postgres"), "postgresql");
  assert.equal(dbSystem("redis"), "redis");
});

test("two Postgres connections are two scopes, and a later window is the same scope", () => {
  const other: Source = { ...pg, id: "c_2", server: "127.0.0.1:5433" };
  assert.notEqual(scopeKey(scopeOf(pg, 10)), scopeKey(scopeOf(other, 10)));
  assert.equal(scopeKey(scopeOf(pg, 10)), scopeKey(scopeOf(pg, 99)));
  assert.notEqual(scopeKey(scopeOf(pg, 10)), scopeKey(scopeOf(ALL, 10)));
});

test("an app narrows every view, with nothing to explain", () => {
  for (const v of VIEWS) {
    assert.equal(narrowed(v, api), true, v);
    assert.equal(scopeNote(v, api), null, v);
    assert.equal(scopeNote(v, ALL), null, v);
  }
});

test("a database narrows every view the daemon can filter by it, and Metrics says why not", () => {
  assert.deepEqual(VIEWS.filter((v) => !narrowed(v, pg)), ["Metrics"]);
  for (const v of VIEWS) {
    const note = scopeNote(v, pg);
    assert.equal(note === null, narrowed(v, pg), v);
  }
  assert.match(scopeNote("Metrics", pg) ?? "", /no metrics/);
});

test("a chart of the whole system says so only when another server is in it", () => {
  // The same calls, counted a moment apart.
  assert.equal(chartNote("acme", "Postgres", 1000, 1000), null);
  assert.equal(chartNote("acme", "Postgres", 1000, 990), null);
  assert.equal(chartNote("acme", "Postgres", 3, 1), null);
  assert.equal(chartNote("acme", "Postgres", 0, 0), null);
  const two = chartNote("acme", "Postgres", 1500, 1000) ?? "";
  assert.match(two, /1,500/);
  assert.match(two, /1,000 of them reached acme/);
  // Calls no span ties to this server.
  assert.match(chartNote("acme", "Postgres", 400, 0) ?? "", /None of them says it reached acme/);
});

test("a chart split by system keeps the scope's system", () => {
  const groups = [{ key: "postgresql" }, { key: "redis" }];
  assert.deepEqual(groupsInDb(groups, "redis"), [{ key: "redis" }]);
  assert.equal(groupsInDb(groups, ""), groups);
});

test("sources are told apart by what they are, not by their name", () => {
  const other: Source = { kind: "db", id: "c_2", name: "acme", system: "postgresql", server: "127.0.0.1:5433" };
  assert.equal(sameSource(pg, { ...pg }), true);
  assert.equal(sameSource(pg, other), false);
  // An app could be called what a connection is called.
  assert.notEqual(sourceKey({ kind: "app", service: "c_1" }), sourceKey(pg));
  assert.equal(sourceName(ALL), "All sources");
  assert.equal(sourceName(api), "acme-api");
  assert.equal(sourceName(pg), "acme");
});
