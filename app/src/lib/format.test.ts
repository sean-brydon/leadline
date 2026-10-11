import { test } from "node:test";
import assert from "node:assert/strict";
import { bandMs, fmtAgo, fmtBytes, fmtCount, fmtFor, fmtInt, fmtMs, fmtMsShort, fmtPct, fmtPer, fmtRange, fmtRate, fmtSpan } from "./format.ts";

test("fmtMs picks a unit that keeps three figures", () => {
  assert.equal(fmtMs(0.412), "412µs");
  assert.equal(fmtMs(2.345), "2.35ms");
  assert.equal(fmtMs(12.34), "12.3ms");
  assert.equal(fmtMs(412.6), "413ms");
  assert.equal(fmtMs(1240), "1.24s");
  assert.equal(fmtMs(90_000), "1.5m");
  assert.equal(fmtMs(NaN), "–");
});

test("fmtCount shortens thousands and millions", () => {
  assert.equal(fmtCount(950), "950");
  assert.equal(fmtCount(1234), "1.2k");
  assert.equal(fmtCount(45_600), "46k");
  assert.equal(fmtCount(3_400_000), "3.4M");
});

test("fmtSpan rounds to the unit a person would say", () => {
  assert.equal(fmtSpan(500), "500ms");
  assert.equal(fmtSpan(10_000), "10s");
  assert.equal(fmtSpan(240_000), "4m");
  assert.equal(fmtSpan(2 * 3_600_000), "2h");
});

test("fmtMsShort drops the digits an axis has no room for", () => {
  assert.equal(fmtMsShort(0), "0");
  assert.equal(fmtMsShort(0.25), "250µs");
  assert.equal(fmtMsShort(3.16), "3.2ms");
  assert.equal(fmtMsShort(10), "10ms");
  assert.equal(fmtMsShort(1000), "1s");
  assert.equal(fmtMsShort(1500), "1.5s");
  assert.equal(fmtMsShort(Infinity), "–");
});

test("fmtPct keeps small shares visible", () => {
  assert.equal(fmtPct(0), "0%");
  assert.equal(fmtPct(0.0004), "<0.1%");
  assert.equal(fmtPct(0.042), "4.2%");
  assert.equal(fmtPct(0.488), "49%");
  assert.equal(fmtPct(1), "100%");
});

test("fmtPer writes averages without false precision", () => {
  assert.equal(fmtPer(0.04), "0.04");
  assert.equal(fmtPer(1), "1");
  assert.equal(fmtPer(3.96), "4");
  assert.equal(fmtPer(13.24), "13.2");
  assert.equal(fmtPer(15), "15");
  assert.equal(fmtPer(2400), "2.4k");
});

test("latency bands are log-spaced and named by their ends", () => {
  assert.equal(bandMs(0, 4), 1);
  assert.equal(bandMs(8, 4), 100);
  assert.equal(Math.round(bandMs(9, 4)), 178);
  assert.equal(fmtRange(bandMs(8, 4), bandMs(9, 4)), "100–180ms");
  assert.equal(fmtRange(bandMs(11, 4), bandMs(12, 4)), "560ms–1s");
  assert.equal(fmtRange(0.5, 1), "500µs–1ms");
});

test("fmtBytes picks the unit that keeps the number short", () => {
  assert.equal(fmtBytes(0), "0 B");
  assert.equal(fmtBytes(812), "812 B");
  assert.equal(fmtBytes(1_626_416), "1.6 MB");
  assert.equal(fmtBytes(26_228_403), "25 MB");
  assert.equal(fmtBytes(10_780_672), "10.3 MB");
  assert.equal(fmtBytes(312 * 1024 ** 3), "312 GB");
  assert.equal(fmtBytes(2 * 1024 ** 3), "2 GB");
  assert.equal(fmtBytes(NaN), "–");
});

test("fmtInt writes a count in full", () => {
  assert.equal(fmtInt(112941), "112,941");
  assert.equal(fmtInt(7), "7");
});

test("fmtFor says how long in words, to the largest unit", () => {
  assert.equal(fmtFor(1000), "1 second");
  assert.equal(fmtFor(40_500), "40 seconds");
  assert.equal(fmtFor(12 * 60_000 + 30_000), "12 minutes");
  assert.equal(fmtFor(60 * 60_000), "1 hour");
  assert.equal(fmtFor(49 * 3_600_000), "2 days");
});

test("fmtAgo is short enough for a column", () => {
  assert.equal(fmtAgo(900), "now");
  assert.equal(fmtAgo(8_400), "8 s ago");
  assert.equal(fmtAgo(12 * 60_000), "12 m ago");
  assert.equal(fmtAgo(3 * 3_600_000), "3 h ago");
  assert.equal(fmtAgo(50 * 3_600_000), "2 d ago");
  // A clock a little ahead of the daemon's is still "now".
  assert.equal(fmtAgo(-300), "now");
});

test("fmtRate is a count per minute, and nothing for none", () => {
  assert.equal(fmtRate(42), "42/m");
  assert.equal(fmtRate(1130), "1.1k/m");
  assert.equal(fmtRate(0.4), "<1/m");
  assert.equal(fmtRate(0), "");
});
