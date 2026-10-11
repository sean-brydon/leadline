import assert from "node:assert/strict";
import test from "node:test";
import { queryCount, ROLE_MARK, secondary, userLabel, userOf, type RoleSpan } from "./spans.ts";

const span = (db_system: string, db_role?: string, attrs: Record<string, unknown> = {}): RoleSpan => ({ db_system, db_role, attrs });

test("a trace's queries are the calls that count as one", () => {
  const spans = [
    span(""),
    span("postgresql", "control"), // BEGIN
    span("postgresql", "duplicate"), // the app's own span around the next
    span("postgresql", "query"),
    span("postgresql", "query"),
    span("postgresql", "control"), // COMMIT
    span("redis", "query"),
  ];
  assert.equal(queryCount(spans), 3);
  assert.equal(queryCount([span(""), span("")]), 0);
});

test("a daemon that marks no span counts every database call, as before", () => {
  assert.equal(queryCount([span(""), span("postgresql"), span("redis")]), 2);
});

test("control and second sightings are secondary, and have words for why", () => {
  assert.equal(secondary(span("postgresql", "control")), true);
  assert.equal(secondary(span("postgresql", "duplicate")), true);
  assert.equal(secondary(span("postgresql", "query")), false);
  assert.equal(secondary(span("postgresql")), false);
  assert.equal(secondary(span("")), false);
  for (const role of ["control", "duplicate"]) {
    assert.ok(ROLE_MARK[role].word);
    assert.ok(ROLE_MARK[role].why.length > 20);
  }
});

test("the user is the first span's that names one", () => {
  assert.equal(userOf([span(""), span("postgresql", "query")]), null);
  const u = userOf([span(""), span("", undefined, { "user.id": "u_7", "user.email": "ada@acme.test", "user.full_name": "Ada Acme" }), span("", undefined, { "user.id": "u_9" })]);
  assert.deepEqual(u, { id: "u_7", name: "Ada Acme", email: "ada@acme.test" });
  // An id that is a number is still an id.
  assert.equal(userOf([span("", undefined, { "user.id": 42 })])?.id, "42");
});

test("a user is called by name, then email, then id", () => {
  assert.equal(userLabel({ id: "u_7", name: "Ada Acme", email: "ada@acme.test" }), "Ada Acme");
  assert.equal(userLabel({ id: "u_7", name: "", email: "ada@acme.test" }), "ada@acme.test");
  assert.equal(userLabel({ id: "u_7", name: "", email: "" }), "u_7");
});
