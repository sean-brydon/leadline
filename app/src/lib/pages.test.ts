import assert from "node:assert/strict";
import test from "node:test";
import { fresh, logBefore, traceBefore, trimmed, withHead, withOlder, type Listing, type Paging } from "./pages.ts";

type Row = { id: string; cursor: string; note?: string };

// A row at time t (nanoseconds, far past what a float holds exactly).
const T0 = 1791640377034618000n;
const row = (t: number, id: string, note?: string): Row => ({ id, cursor: `${T0 + BigInt(t)}.${id}`, note });

const paging: Paging<Row> = {
  size: 3,
  key: (r) => r.id,
  cursor: (r) => r.cursor,
  before: (a, b) => traceBefore(a.cursor, b.cursor),
};
const ids = (l: Listing<Row>) => l.rows.map((r) => r.id).join(" ");

test("cursors order by exact time, then by tie", () => {
  // One nanosecond apart: the same number as millisecond floats.
  assert.ok(traceBefore(`${T0 + 1n}.aa`, `${T0}.ff`));
  assert.ok(!traceBefore(`${T0}.ff`, `${T0 + 1n}.aa`));
  assert.ok(traceBefore(`${T0}.ff`, `${T0}.aa`), "the larger id first within an instant");
  assert.ok(logBefore(`${T0}.2`, `${T0}.10`), "stored order within an instant, as numbers");
  assert.ok(logBefore(`${T0 + 1n}.9`, `${T0}.1`));
});

test("the first page is the list", () => {
  const l = withHead(undefined, [row(9, "i"), row(8, "h"), row(7, "g")], paging);
  assert.equal(ids(l), "i h g");
  assert.equal(l.more, true, "a full page may have more after it");
  assert.equal(withHead(undefined, [row(9, "i")], paging).more, false, "a short page is everything");
});

test("new rows go on top and held rows stay", () => {
  let l: Listing<Row> = { rows: [row(5, "e"), row(4, "d"), row(3, "c"), row(2, "b")], more: true };
  l = withHead(l, [row(7, "g"), row(6, "f"), row(5, "e")], paging);
  assert.equal(ids(l), "g f e d c b");
  assert.equal(l.more, true);
});

test("a row the page has again takes the page's copy and place", () => {
  // Trace d grew: its root arrived, with new numbers and an earlier start
  // that still falls on the page.
  const l = withHead(
    { rows: [row(50, "e"), row(40, "d", "half"), row(30, "c"), row(20, "b")], more: false },
    [row(50, "e"), row(30, "c"), row(25, "d", "whole")],
    paging,
  );
  assert.equal(ids(l), "e c d b");
  assert.equal(l.rows.find((r) => r.id === "d")?.note, "whole");
});

test("a held row the page should have had, and doesn't, is dropped", () => {
  // x sat between g and e and no longer matches (it left the filter, or
  // moved): the page covers that stretch and doesn't list it.
  const l = withHead(
    { rows: [row(60, "x"), row(50, "e"), row(40, "d")], more: true },
    [row(80, "h"), row(70, "g"), row(50, "e")],
    paging,
  );
  assert.equal(ids(l), "h g e d");
});

test("more new rows than a page starts the list again", () => {
  const l = withHead({ rows: [row(3, "c"), row(2, "b"), row(1, "a")], more: false }, [row(9, "i"), row(8, "h"), row(7, "g")], paging);
  assert.equal(ids(l), "i h g", "what lies between g and c is unknown");
  assert.equal(l.more, true);
});

test("a short page replaces everything", () => {
  const l = withHead({ rows: [row(3, "c"), row(2, "b"), row(1, "a")], more: true }, [row(3, "c")], paging);
  assert.equal(ids(l), "c");
  assert.equal(l.more, false);
});

test("an empty page empties the list", () => {
  const l = withHead({ rows: [row(3, "c")], more: true }, [], paging);
  assert.equal(l.rows.length, 0);
  assert.equal(l.more, false);
});

test("older pages append, without repeating a row that moved", () => {
  let l: Listing<Row> = { rows: [row(9, "i"), row(8, "h"), row(7, "g")], more: true };
  l = withOlder(l, [row(6, "f"), row(5, "h"), row(4, "d")], paging);
  assert.equal(ids(l), "i h g f d");
  assert.equal(l.more, true);
  l = withOlder(l, [row(3, "c")], paging);
  assert.equal(ids(l), "i h g f d c");
  assert.equal(l.more, false, "a short page is the end");
});

test("fresh counts what the list doesn't show yet", () => {
  const l: Listing<Row> = { rows: [row(5, "e"), row(4, "d"), row(3, "c")], more: true };
  assert.deepEqual(fresh(l, [row(7, "g"), row(6, "f"), row(5, "e")], paging), { count: 2, atLeast: false });
  assert.deepEqual(fresh(l, [row(5, "e"), row(4, "d"), row(3, "c")], paging), { count: 0, atLeast: false });
  assert.deepEqual(fresh(l, [row(9, "i"), row(8, "h"), row(7, "g")], paging), { count: 3, atLeast: true });
  assert.deepEqual(fresh(undefined, [row(9, "i")], paging), { count: 0, atLeast: false });
});

test("trimmed cuts the tail and says there is more", () => {
  const l: Listing<Row> = { rows: [row(5, "e"), row(4, "d"), row(3, "c")], more: false };
  assert.equal(trimmed(l, 5), l);
  const cut = trimmed(l, 2);
  assert.equal(ids(cut), "e d");
  assert.equal(cut.more, true);
});
