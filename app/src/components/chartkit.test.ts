import { test } from "node:test";
import assert from "node:assert/strict";
import { alpha, heat } from "./chartkit.ts";

test("alpha writes the two hex digits a six-digit colour takes", () => {
  assert.equal(alpha(0), "00");
  assert.equal(alpha(0.5), "80");
  assert.equal(alpha(1), "ff");
  assert.equal(alpha(7), "ff");
  assert.equal(alpha(-1), "00");
});

test("heat keeps one request visible beside thousands", () => {
  assert.equal(heat(0, 100), 0);
  assert.equal(heat(100, 100), 1);
  // A lone slow request must not fade into the panel.
  assert.ok(heat(1, 5000) >= 0.2);
  // More is never fainter.
  let last = 0;
  for (const n of [1, 2, 5, 20, 100, 900, 5000]) {
    const h = heat(n, 5000);
    assert.ok(h > last, `heat(${n}) = ${h} after ${last}`);
    last = h;
  }
  // A count past the stated maximum (a refetch racing the scale) is clamped.
  assert.equal(heat(200, 100), 1);
});
