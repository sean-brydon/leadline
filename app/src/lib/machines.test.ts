import assert from "node:assert/strict";
import test from "node:test";
import { added, chosen, current, first, LOCAL_ID, loopbackUrl, machines, removed, restored, saved } from "./machines.ts";

const LOCAL = "http://127.0.0.1:4320";

test("an address is kept only when it is http on this machine", () => {
  assert.equal(loopbackUrl("http://127.0.0.1:4336"), "http://127.0.0.1:4336");
  assert.equal(loopbackUrl("127.0.0.1:4336"), "http://127.0.0.1:4336");
  assert.equal(loopbackUrl(" http://localhost:4336/ "), "http://localhost:4336");
  assert.equal(loopbackUrl("http://127.0.0.1:4336/api/summary?x=1"), "http://127.0.0.1:4336");
  for (const no of ["", "https://127.0.0.1:4336", "http://example.com:4320", "http://10.0.0.4:4320", "http://127.0.0.1.evil.test:4320", "http://user:pw@127.0.0.1:4320", "ftp://127.0.0.1", "not a url"]) {
    assert.equal(loopbackUrl(no), null, no);
  }
});

test("the list starts as this Mac alone", () => {
  const s = first(LOCAL);
  assert.deepEqual(s.machines, [{ id: LOCAL_ID, name: "This Mac", url: LOCAL }]);
  assert.equal(current(s).url, LOCAL);
});

test("adding a machine keeps this Mac first and showing", () => {
  const s = added(first(LOCAL), "Staging VPS", "127.0.0.1:4336");
  assert.deepEqual(s.machines.map((m) => m.name), ["This Mac", "Staging VPS"]);
  assert.equal(s.machines[1].url, "http://127.0.0.1:4336");
  assert.equal(s.current, LOCAL_ID);
});

test("adding the same address again renames it, and never this Mac", () => {
  let s = added(first(LOCAL), "A", "http://127.0.0.1:4336");
  s = added(s, "B", "http://127.0.0.1:4336/");
  assert.deepEqual(s.machines.map((m) => m.name), ["This Mac", "B"]);
  s = added(s, "Mine", LOCAL);
  assert.equal(s.machines[0].name, "This Mac");
  assert.equal(s.machines.length, 2);
});

test("a machine with no name is called by its address", () => {
  const s = added(first(LOCAL), "  ", "http://localhost:4400");
  assert.equal(s.machines[1].name, "localhost:4400");
});

test("an address on another host is refused in words", () => {
  assert.throws(() => added(first(LOCAL), "VPS", "http://203.0.113.9:4320"), /on this computer/);
});

test("choosing and removing", () => {
  let s = added(first(LOCAL), "VPS", "http://127.0.0.1:4336");
  const id = s.machines[1].id;
  assert.equal(chosen(s, "nope"), s);
  s = chosen(s, id);
  assert.equal(current(s).name, "VPS");
  // Removing the one shown goes back to this Mac.
  s = removed(s, id);
  assert.equal(s.current, LOCAL_ID);
  assert.equal(s.machines.length, 1);
  assert.equal(removed(s, LOCAL_ID), s);
});

test("what is saved comes back, without this Mac's address in it", () => {
  let s = added(first(LOCAL), "VPS", "http://127.0.0.1:4336");
  s = chosen(s, s.machines[1].id);
  const text = saved(s);
  assert.ok(!text.includes("4320"));
  // The app now runs its own daemon somewhere else: this Mac follows it.
  const back = restored(text, "http://127.0.0.1:4999");
  assert.equal(back.machines[0].url, "http://127.0.0.1:4999");
  assert.equal(current(back).name, "VPS");
});

test("a save that can't be trusted is dropped", () => {
  assert.deepEqual(restored("{nope", LOCAL), first(LOCAL));
  assert.deepEqual(restored(null, LOCAL), first(LOCAL));
  const s = restored(JSON.stringify({ current: "m:gone", machines: [{ name: "Evil", url: "http://example.com:4320" }, { name: 3 }, null] }), LOCAL);
  assert.deepEqual(s, first(LOCAL));
});

test("the window's list tells its listeners, and separately when the address moves", () => {
  let changes = 0;
  let moves = 0;
  const off = machines.subscribe(() => changes++);
  const offMove = machines.onMove(() => moves++);
  const m = machines.add("Second", "http://127.0.0.1:4336");
  assert.equal(changes, 1);
  assert.equal(moves, 0);
  machines.use(m.id);
  assert.equal(machines.current().url, "http://127.0.0.1:4336");
  assert.equal(moves, 1);
  machines.use(m.id);
  assert.equal(moves, 1);
  machines.remove(m.id);
  assert.equal(machines.current().id, LOCAL_ID);
  assert.equal(moves, 2);
  off();
  offMove();
});
