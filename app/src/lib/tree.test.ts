import { test } from "node:test";
import assert from "node:assert/strict";
import { waterfall } from "./tree.ts";

const span = (span_id: string, parent_id: string, start_ms: number) => ({ span_id, parent_id, start_ms });
const shape = (rows: { span: { span_id: string }; depth: number }[]) => rows.map((r) => `${r.depth}:${r.span.span_id}`);

test("children follow their parent in start order", () => {
  const rows = waterfall([span("c2", "root", 30), span("root", "", 0), span("c1", "root", 10), span("g", "c1", 12)]);
  assert.deepEqual(shape(rows), ["0:root", "1:c1", "2:g", "1:c2"]);
});

test("a span whose parent is missing is kept at the top level", () => {
  const rows = waterfall([span("a", "gone", 5), span("b", "a", 6)]);
  assert.deepEqual(shape(rows), ["0:a", "1:b"]);
});

test("a cycle of parents still lists every span once", () => {
  const rows = waterfall([span("a", "b", 1), span("b", "a", 2)]);
  assert.deepEqual(shape(rows).sort(), ["0:a", "1:b"]);
});
