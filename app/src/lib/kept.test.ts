import assert from "node:assert/strict";
import test from "node:test";
import type { Storage, Summary } from "./api.ts";
import { fmtKeep, kept } from "./kept.ts";

const NOW = 1_791_650_000_000;
const summary: Summary = { spans: 20_362, logs: 2582, traces: 3016, services: ["acme-api"] };
const storage = (over: Partial<Storage> = {}): Storage => ({
  size_bytes: 8_570_591,
  file_bytes: 8_582_879,
  max_bytes: 2 * 1024 ** 3,
  retain_ms: 72 * 3_600_000,
  oldest_ms: NOW - 14 * 60_000,
  ...over,
});

test("how long data is kept reads in days when it is whole days", () => {
  assert.equal(fmtKeep(72 * 3_600_000), "3 days");
  assert.equal(fmtKeep(24 * 3_600_000), "1 day");
  assert.equal(fmtKeep(36 * 3_600_000), "36 hours");
  assert.equal(fmtKeep(3_600_000), "1 hour");
  assert.equal(fmtKeep(30 * 60_000), "30 minutes");
});

test("with storage, the lines say how much and for how long", () => {
  const k = kept("this Mac", summary, storage(), NOW);
  assert.equal(k.short, "Everything stays on this Mac. 8.2 MB kept, for up to 3 days.");
  assert.match(k.long, /^Nothing leaves this Mac\. Leadline holds 8\.2 MB on disk: 20k spans, 2\.6k logs and 3\.0k traces\./);
  assert.match(k.long, /The oldest is from 14 m ago\./);
  assert.match(k.long, /kept for 3 days, or until it passes 2 GB; the oldest goes first\./);
  assert.equal(k.fullText, "8.2 MB of 2 GB");
  assert.ok(k.full !== null && k.full > 0.003 && k.full < 0.005);
});

test("no cap, and no time limit, are said plainly", () => {
  const forever = kept("this Mac", summary, storage({ retain_ms: 0, max_bytes: 0 }), NOW);
  assert.match(forever.short, /8\.2 MB kept, until you remove it\./);
  assert.match(forever.long, /Nothing is dropped by itself\./);
  assert.equal(forever.full, null);
  const capped = kept("this Mac", summary, storage({ retain_ms: 0 }), NOW);
  assert.match(capped.long, /kept until it passes 2 GB; then the oldest goes first\./);
});

test("a store kept in memory has no size to give", () => {
  const k = kept("this Mac", summary, storage({ size_bytes: 0, file_bytes: 0 }), NOW);
  assert.equal(k.short, "Everything stays on this Mac. Kept in memory, for up to 3 days.");
  assert.match(k.long, /in memory, and writes none of it to disk/);
  assert.equal(k.full, null);
  assert.equal(k.fullText, "");
});

test("an older daemon, or none yet, promises no more than it knows", () => {
  assert.deepEqual(kept("hetzner-1", undefined, undefined, NOW), { short: "Everything stays on hetzner-1.", long: "Nothing leaves hetzner-1.", full: null, fullText: "" });
  const old = kept("this Mac", summary, null, NOW);
  assert.match(old.short, /Holding 20k spans and 2\.6k logs\./);
  assert.match(old.long, /drops the oldest by itself/);
  assert.match(kept("this Mac", { spans: 1, logs: 8, traces: 1, services: [] }, storage(), NOW).long, /: 1 span, 8 logs and 1 trace\./);
  // Storage before the first summary.
  assert.match(kept("this Mac", undefined, storage(), NOW).long, /holds 8\.2 MB on disk\./);
});

test("another machine's data is said to be kept there, and read from here", () => {
  const k = kept("hetzner-1", summary, storage(), NOW, true);
  assert.equal(k.short, "Everything is kept on hetzner-1; this window only reads it. 8.2 MB kept, for up to 3 days.");
  assert.match(k.long, /^It is all kept on hetzner-1, and leaves only for this window, through the port forwarded to this Mac\. Leadline holds 8\.2 MB on disk: /);
  assert.ok(!k.long.includes("Nothing leaves"));
  // An older daemon there, and one that hasn't answered yet.
  assert.equal(kept("hetzner-1", summary, null, NOW, true).short, "Everything is kept on hetzner-1; this window only reads it. Holding 20k spans and 2.6k logs.");
  assert.equal(kept("hetzner-1", undefined, undefined, NOW, true).short, "Everything is kept on hetzner-1; this window only reads it.");
});
