import assert from "node:assert/strict";
import test from "node:test";
import { added, addedBox, chosen, closing, current, first, hereOf, LOCAL_ID, loopbackUrl, machines, portOf, reach, reachLine, removed, removing, renamed, restored, saved, seen } from "./machines.ts";

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

// Shipyard boxes, and what the list remembers of the forward to each.

const fwd = (id: string, port: number, opened: boolean) => ({ id, url: `http://127.0.0.1:${port}`, opened });

test("a Shipyard box is added by the forward to it, and remembers that forward", () => {
  const s = addedBox(first(LOCAL), "hetzner-1", fwd("f0000000", 14321, true));
  assert.deepEqual(s.machines[1], { id: "box:hetzner-1", name: "hetzner-1", url: "http://127.0.0.1:14321", via: "shipyard", box: "hetzner-1", forward: { id: "f0000000", ours: true } });
  assert.equal(s.current, LOCAL_ID);
  assert.equal(reach(s.machines[0]), "local");
  assert.equal(reach(s.machines[1]), "shipyard");
  assert.equal(portOf(s.machines[1]), "14321");
  // What can't be a forward to a box is refused.
  assert.throws(() => addedBox(first(LOCAL), "hetzner-1", { id: "f0000000", url: "http://203.0.113.9:4320", opened: true }), /not a forward/);
  assert.throws(() => addedBox(first(LOCAL), "--json", fwd("f0000000", 14321, true)), /not a forward/);
  assert.throws(() => addedBox(first(LOCAL), "hetzner-1", fwd("-f", 14321, true)), /not a forward/);
});

test("a forward found again stays Leadline's own; one that was there already is not", () => {
  // Opened by Leadline, then found again on the next use: still ours.
  let s = addedBox(first(LOCAL), "hetzner-1", fwd("f0000000", 14321, true));
  s = addedBox(s, "hetzner-1", fwd("f0000000", 14321, false));
  assert.deepEqual(s.machines[1].forward, { id: "f0000000", ours: true });
  assert.equal(closing(s.machines[1]), "f0000000");
  // It went away and was opened again: a new id, on the same port, and ours.
  s = addedBox(s, "hetzner-1", fwd("f0000001", 14321, true));
  assert.deepEqual(s.machines[1].forward, { id: "f0000001", ours: true });
  // Someone's own forward to the box was found instead: used, never closed by Leadline.
  s = addedBox(s, "hetzner-1", fwd("ab12cd34", 15000, false));
  assert.equal(s.machines[1].url, "http://127.0.0.1:15000");
  assert.deepEqual(s.machines[1].forward, { id: "ab12cd34", ours: false });
  assert.equal(closing(s.machines[1]), null);
  assert.equal(s.machines.length, 2);
});

test("a box keeps the name it was given, its place and being shown when its forward moves", () => {
  let s = addedBox(first(LOCAL), "hetzner-1", fwd("f0000000", 14321, true), "Staging");
  s = added(s, "VPS", "http://127.0.0.1:4336");
  assert.equal(s.machines[1].name, "Staging");
  s = chosen(s, "box:hetzner-1");
  s = addedBox(s, "hetzner-1", fwd("f0000001", 14322, true), "ignored");
  assert.deepEqual(s.machines.map((m) => m.name), ["This Mac", "Staging", "VPS"]);
  assert.equal(current(s).url, "http://127.0.0.1:14322");
  // An address added by hand that turns out to be a box's forward becomes the box's one row.
  s = added(first(LOCAL), "by hand", "http://127.0.0.1:14320");
  s = chosen(s, s.machines[1].id);
  s = addedBox(s, "hetzner-1", fwd("ab12cd34", 14320, false));
  assert.deepEqual(s.machines.map((m) => m.id), [LOCAL_ID, "box:hetzner-1"]);
  assert.equal(s.current, LOCAL_ID);
});

test("renaming, and an empty name going back to what the machine is", () => {
  let s = addedBox(first(LOCAL), "hetzner-1", fwd("f0000000", 14321, true));
  s = added(s, "VPS", "http://127.0.0.1:4336");
  s = renamed(s, "box:hetzner-1", "  Staging box ");
  assert.equal(s.machines[1].name, "Staging box");
  assert.equal(s.machines[1].box, "hetzner-1");
  assert.equal(renamed(s, "box:hetzner-1", "Staging box"), s);
  s = renamed(s, "box:hetzner-1", " ");
  assert.equal(s.machines[1].name, "hetzner-1");
  s = renamed(s, s.machines[2].id, "");
  assert.equal(s.machines[2].name, "127.0.0.1:4336");
  // This Mac keeps its name, and a machine that isn't there changes nothing.
  assert.equal(renamed(s, LOCAL_ID, "Mine"), s);
  assert.equal(renamed(s, "box:nope", "x"), s);
});

test("the words for how a machine is reached, and for the machine in a sentence", () => {
  let s = addedBox(first(LOCAL), "hetzner-1", fwd("f0000000", 14321, true));
  s = added(s, "VPS", "http://127.0.0.1:4336");
  const [mac, box, vps] = s.machines;
  assert.equal(hereOf(mac), "this Mac");
  assert.equal(hereOf(box), "hetzner-1");
  assert.equal(hereOf(vps), "VPS");
  assert.equal(reachLine(mac), "This computer");
  assert.equal(reachLine(box), "Shipyard box, through port 14321");
  assert.equal(reachLine(renamed(s, box.id, "Staging").machines[1]), "Shipyard box hetzner-1, through port 14321");
  assert.equal(reachLine(vps), "A port you forwarded, 127.0.0.1:4336");
  // A machine saved before there were boxes has no `via`: it is an address.
  assert.equal(reach({ id: "m:127.0.0.1:4336", name: "old", url: "http://127.0.0.1:4336" }), "address");
});

test("removing says what it does and doesn't, and which forward is closed", () => {
  let s = addedBox(first(LOCAL), "hetzner-1", fwd("f0000000", 14321, true));
  const ours = s.machines[1];
  assert.equal(closing(ours), "f0000000");
  assert.deepEqual(removing(ours), [
    "Leadline forgets hetzner-1: it leaves this list and the machine switcher.",
    "The forward Leadline opened to it in Shipyard, on port 14321, is closed.",
    "Nothing is uninstalled on hetzner-1 and no data there is deleted. Its Leadline keeps running and recording, and you can add it again at any time.",
  ]);
  const theirs = addedBox(first(LOCAL), "hetzner-1", fwd("ab12cd34", 15000, false)).machines[1];
  assert.equal(closing(theirs), null);
  assert.equal(removing(theirs)[1], "The Shipyard forward on port 15000 was there before Leadline used it, so it is left open.");
  s = added(s, "VPS", "http://127.0.0.1:4336");
  assert.equal(closing(s.machines[2]), null);
  assert.equal(removing(s.machines[2])[1], "The port you forwarded yourself (4336) is left as it is.");
  assert.equal(closing(s.machines[0]), null);
  // Removing takes it off, and the forward's record with it.
  s = removed(chosen(s, ours.id), ours.id);
  assert.deepEqual(s.machines.map((m) => m.name), ["This Mac", "VPS"]);
  assert.equal(s.current, LOCAL_ID);
});

test("a box, its forward, its name and when it last answered are kept between launches", () => {
  let s = addedBox(first(LOCAL), "hetzner-1", fwd("f0000000", 14321, true), "Staging");
  s = addedBox(s, "build-box", fwd("ab12cd34", 15000, false));
  s = added(s, "VPS", "http://127.0.0.1:4336");
  s = seen(s, "box:hetzner-1", 1_791_650_000_000, "0.2.0");
  assert.equal(seen(s, "box:hetzner-1", 1_791_650_000_000, "0.2.0"), s);
  s = chosen(s, "box:hetzner-1");
  const back = restored(saved(s), LOCAL);
  assert.deepEqual(back, s);
  assert.deepEqual(back.machines[1], { id: "box:hetzner-1", name: "Staging", url: "http://127.0.0.1:14321", via: "shipyard", box: "hetzner-1", forward: { id: "f0000000", ours: true }, seenMs: 1_791_650_000_000, version: "0.2.0" });
  assert.equal(closing(back.machines[1]), "f0000000");
  assert.equal(closing(back.machines[2]), null);
  assert.equal(current(back).name, "Staging");
});

test("a save from before there were boxes still reads, and a box's record that can't be trusted is an address", () => {
  const old = JSON.stringify({ current: "m:127.0.0.1:14320", machines: [{ id: "m:127.0.0.1:14320", name: "hetzner-1", url: "http://127.0.0.1:14320" }] });
  const s = restored(old, LOCAL);
  assert.deepEqual(s.machines[1], { id: "m:127.0.0.1:14320", name: "hetzner-1", url: "http://127.0.0.1:14320", via: "address" });
  assert.equal(current(s).name, "hetzner-1");
  const odd = restored(JSON.stringify({ machines: [{ name: "x", url: "http://127.0.0.1:14321", via: "shipyard", box: "bad name; rm", forward: { id: "f0000000", ours: true } }, { name: "y", url: "http://127.0.0.1:14322", via: "shipyard", box: "ok", forward: { id: "--json" }, seenMs: "soon" }] }), LOCAL);
  assert.deepEqual(odd.machines.slice(1).map((m) => [m.name, reach(m), closing(m), m.seenMs]), [["x", "address", null, undefined], ["y", "address", null, undefined]]);
});

test("the window's list adds, renames and forgets a box", () => {
  let changes = 0;
  const off = machines.subscribe(() => changes++);
  const m = machines.addBox("hetzner-1", fwd("f0000000", 14321, true));
  assert.equal(m.id, "box:hetzner-1");
  machines.rename(m.id, "Staging");
  assert.equal(machines.get().machines[1].name, "Staging");
  // When it last answered is written down, but not on every answer.
  machines.seen(m.id, "0.2.0", 1_000_000);
  machines.seen(m.id, "0.2.0", 1_010_000);
  assert.equal(machines.get().machines[1].seenMs, 1_000_000);
  machines.seen(m.id, "0.2.0", 1_070_000);
  assert.equal(machines.get().machines[1].seenMs, 1_070_000);
  machines.seen(m.id, "0.3.0", 1_071_000);
  assert.equal(machines.get().machines[1].version, "0.3.0");
  assert.equal(changes, 5);
  machines.remove(m.id);
  assert.equal(machines.get().machines.length, 1);
  off();
});
