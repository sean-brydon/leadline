import { test } from "node:test";
import assert from "node:assert/strict";
import { chartScale, fmtBytes, fmtNum, kindLabel, metricFormat, unitLabel } from "./metricFormat.ts";

test("fmtNum keeps about three figures", () => {
  assert.equal(fmtNum(0), "0");
  assert.equal(fmtNum(0.0417), "0.042");
  assert.equal(fmtNum(0.5), "0.5");
  assert.equal(fmtNum(7), "7");
  assert.equal(fmtNum(7.254), "7.25");
  assert.equal(fmtNum(13.24), "13.2");
  assert.equal(fmtNum(240.4), "240");
  assert.equal(fmtNum(1234), "1.2k");
  assert.equal(fmtNum(-3.5), "-3.5");
  assert.equal(fmtNum(NaN), "–");
});

test("fmtBytes counts in binary units", () => {
  assert.equal(fmtBytes(512), "512B");
  assert.equal(fmtBytes(1536), "1.5KiB");
  assert.equal(fmtBytes(182 * 1024 * 1024), "182MiB");
  assert.equal(fmtBytes(2.25 * 1024 ** 3), "2.25GiB");
});

test("metricFormat writes a value by its unit", () => {
  assert.equal(metricFormat("s")(0.0123), "12.3ms");
  assert.equal(metricFormat("s", "", true)(0.25), "250ms");
  assert.equal(metricFormat("ms")(1240), "1.24s");
  assert.equal(metricFormat("By")(1048576), "1MiB");
  assert.equal(metricFormat("By/s")(2048), "2KiB/s");
  assert.equal(metricFormat("%")(41.26), "41.3%");
  assert.equal(metricFormat("{connection}")(12), "12");
  assert.equal(metricFormat("")(1500), "1.5k");
});

test("a unit of 1 is a share only when the name says so", () => {
  assert.equal(metricFormat("1", "process.cpu.utilization")(0.254), "25.4%");
  assert.equal(metricFormat("1", "http.server.active_requests")(3), "3");
});

test("unitLabel names what is counted and nothing else", () => {
  assert.equal(unitLabel("{connection}"), "connection");
  assert.equal(unitLabel("s"), "");
  assert.equal(unitLabel("By"), "");
  assert.equal(unitLabel("1"), "");
  assert.equal(unitLabel(""), "");
});

test("kindLabel tells a counter from an up-down counter", () => {
  assert.equal(kindLabel({ type: "sum", monotonic: true }), "counter");
  assert.equal(kindLabel({ type: "sum", monotonic: false }), "up-down counter");
  assert.equal(kindLabel({ type: "histogram", monotonic: false }), "histogram");
});

test("chartScale puts sizes in the unit of their peak", () => {
  const mib = chartScale("By", 260 * 1024 * 1024);
  assert.ok(mib);
  assert.equal(mib.times, 1 / 1024 ** 2);
  assert.equal(mib.format(200), "200MiB");
  assert.equal(chartScale("KiBy", 4096)?.format(2), "2MiB");
  assert.equal(chartScale("By", 900)?.times, 1);
  assert.equal(chartScale("s", 2), null);
});
