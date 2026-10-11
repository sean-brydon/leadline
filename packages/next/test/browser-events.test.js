import assert from "node:assert/strict";
import { test } from "node:test";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const { convert, envelope, parseBatch, tracedFetches } = require("../dist/browser-events.cjs");
const { keyValues, nanos } = require("../dist/send.cjs");

const TS = 1791644400123;
const TID = "0af7651916cd43dd8448eb211c80319c";
const SID = "b7ad6b7169203331";

/** OTLP attributes back as a plain object. */
const plain = (attributes) =>
  Object.fromEntries(attributes.map(({ key, value }) => [key, value.stringValue ?? value.boolValue ?? value.doubleValue ?? Number(value.intValue)]));

test("a batch that is not one is dropped, and bad events with it", () => {
  assert.equal(parseBatch("not json"), undefined);
  assert.equal(parseBatch('"text"'), undefined);
  assert.equal(parseBatch('{"s":"x"}'), undefined);
  const batch = parseBatch(JSON.stringify({ s: "abc", e: [{ t: "nav", ts: TS }, { t: "nav" }, null, { ts: 1 }, "x"] }));
  assert.deepEqual(batch, { s: "abc", e: [{ t: "nav", ts: TS }] });
});

test("an uncaught error becomes an error log with its stack", () => {
  const { logs, spans } = convert(
    { s: "page1", e: [{ t: "error", ts: TS, p: "/playground", m: "boom", n: "TypeError", st: "TypeError: boom\n    at onClick (app/x.tsx:3:9)" }] },
    { user: { "user.id": "7" }, userAgent: "TestBrowser/1.0" },
  );
  assert.equal(spans.length, 0);
  const [log] = logs;
  assert.equal(log.severityNumber, 17);
  assert.equal(log.severityText, "ERROR");
  assert.equal(log.timeUnixNano, "1791644400123000000");
  assert.equal(log.body.stringValue, "Uncaught exception: TypeError: boom\n    at onClick (app/x.tsx:3:9)");
  assert.deepEqual(plain(log.attributes), {
    "leadline.source": "browser",
    "session.id": "page1",
    "user_agent.original": "TestBrowser/1.0",
    "leadline.kind": "browser.error",
    "url.path": "/playground",
    "user.id": "7",
    "exception.type": "TypeError",
    "exception.message": "boom",
    "exception.stacktrace": "TypeError: boom\n    at onClick (app/x.tsx:3:9)",
    "leadline.origin": "uncaughtException",
  });
});

test("an unhandled rejection, console output, a navigation and a web vital", () => {
  const { logs } = convert({
    e: [
      { t: "error", ts: TS, k: "rejection", m: "nobody caught this" },
      { t: "console", ts: TS, k: "warn", m: "careful" },
      { t: "console", ts: TS, k: "error", m: "broken" },
      { t: "nav", ts: TS, p: "/orders", from: "/", how: "push" },
      { t: "nav", ts: TS, p: "/", how: "load" },
      { t: "vital", ts: TS, name: "LCP", v: 1234.4, r: "good", p: "/" },
      { t: "vital", ts: TS, name: "CLS", v: 0.31, r: "poor", p: "/" },
      { t: "vital", ts: TS, name: "INP" },
    ],
  });
  assert.deepEqual(
    logs.map((log) => [log.severityText, log.body.stringValue]),
    [
      ["ERROR", "Unhandled rejection: nobody caught this"],
      ["WARN", "careful"],
      ["ERROR", "broken"],
      ["INFO", "navigated to /orders from /"],
      ["INFO", "loaded /"],
      ["INFO", "LCP 1234 ms (good)"],
      ["WARN", "CLS 0.310 (poor)"],
    ],
  );
  const vital = plain(logs[5].attributes);
  assert.equal(vital["leadline.kind"], "browser.web_vital");
  assert.equal(vital["web_vital.name"], "LCP");
  assert.equal(vital["web_vital.value"], 1234.4);
  assert.equal(plain(logs[3].attributes)["navigation.from"], "/");
});

test("a traced fetch becomes the first span of the server's trace, named after what the server made of it", () => {
  const batch = { s: "page1", e: [{ t: "fetch", ts: TS, p: "/orders", method: "post", url: "/orders", status: 200, dur: 41.5, tid: TID, sid: SID }] };
  assert.deepEqual(tracedFetches(batch), [{ traceId: TID, spanId: SID }]);

  const { spans, logs } = convert(batch, {
    user: { "user.id": "from-cookie" },
    caused: (traceId, spanId) =>
      traceId === TID && spanId === SID
        ? { name: "action addNote", route: "/orders", attributes: { "next.kind": "action", "next.action.name": "addNote" }, user: { "user.id": "7" } }
        : undefined,
  });
  assert.equal(logs.length, 0, "a fetch that worked and was quick is not a log");
  const [span] = spans;
  assert.equal(span.traceId, TID);
  assert.equal(span.spanId, SID);
  assert.equal(span.name, "fetch action addNote");
  assert.equal(span.kind, 3);
  assert.equal(span.startTimeUnixNano, "1791644400123000000");
  assert.equal(span.endTimeUnixNano, "1791644400164500000");
  assert.deepEqual(span.status, { code: 0 });
  const attributes = plain(span.attributes);
  assert.equal(attributes["http.route"], "/orders");
  assert.equal(attributes["http.request.method"], "POST");
  assert.equal(attributes["http.response.status_code"], 200);
  assert.equal(attributes["next.action.name"], "addNote");
  assert.equal(attributes["leadline.page"], "/orders");
  // The server said who the request was for; the cookie's guess is not used.
  assert.equal(attributes["user.id"], "7");
});

test("when the server saw no such request, the fetch keeps its path and the cookie's user", () => {
  const { spans } = convert(
    { e: [{ t: "fetch", ts: TS, method: "GET", url: "/logo.png", status: 200, dur: 3, tid: TID, sid: SID }] },
    { user: { "user.id": "from-cookie" }, caused: () => undefined },
  );
  assert.equal(spans[0].name, "fetch GET /logo.png");
  assert.equal(plain(spans[0].attributes)["user.id"], "from-cookie");
  assert.equal(plain(spans[0].attributes)["url.path"], "/logo.png");
});

test("failed and slow fetches are also logs, tied to the trace", () => {
  const { spans, logs } = convert({
    e: [
      { t: "fetch", ts: TS, method: "GET", url: "/api/orders/9999", status: 404, dur: 12, tid: TID, sid: SID },
      { t: "fetch", ts: TS, method: "GET", url: "/api/slow", status: 200, dur: 1500, slow: true, tid: TID, sid: "b7ad6b7169203332" },
      { t: "fetch", ts: TS, method: "GET", url: "https://api.acme.example/v1/rates", status: 0, dur: 30, err: "Failed to fetch" },
      { t: "fetch", ts: TS, method: "GET", url: "/api/gone", status: 0, dur: 5, k: "aborted", tid: TID, sid: "b7ad6b7169203333" },
    ],
  });
  assert.equal(spans.length, 3, "another origin's fetch carries no traceparent, so it has no span");
  assert.equal(spans[0].status.code, 2);
  assert.equal(plain(spans[2].attributes)["leadline.aborted"], true);
  assert.deepEqual(spans[2].status, { code: 0 });
  assert.deepEqual(
    logs.map((log) => [log.severityText, log.body.stringValue, log.traceId]),
    [
      ["WARN", "fetch GET /api/orders/9999 404 in 12 ms", TID],
      ["WARN", "slow fetch GET /api/slow 200 in 1500 ms", TID],
      ["ERROR", "fetch GET https://api.acme.example/v1/rates failed: Failed to fetch in 30 ms", undefined],
    ],
  );
  assert.equal(plain(logs[2].attributes)["url.full"], "https://api.acme.example/v1/rates");
});

test("ids that are not ids make no span", () => {
  const bad = [
    { tid: "xyz", sid: SID },
    { tid: TID.toUpperCase(), sid: SID },
    { tid: "0".repeat(32), sid: SID },
    { tid: TID, sid: "short" },
    { tid: TID },
  ];
  const batch = { e: bad.map((ids) => ({ t: "fetch", ts: TS, method: "GET", url: "/x", status: 200, dur: 1, ...ids })) };
  assert.deepEqual(tracedFetches(batch), []);
  assert.equal(convert(batch).spans.length, 0);
});

test("the envelope names the app's own service", () => {
  const spans = envelope("spans", "acme-shop", [{ name: "x" }]);
  assert.equal(plain(spans.resourceSpans[0].resource.attributes)["service.name"], "acme-shop");
  assert.deepEqual(spans.resourceSpans[0].scopeSpans[0].spans, [{ name: "x" }]);
  const logs = envelope("logs", "acme-shop", [{ body: {} }]);
  assert.equal(logs.resourceLogs[0].scopeLogs[0].logRecords.length, 1);
});

test("OTLP/JSON values", () => {
  assert.deepEqual(keyValues({ a: "x", b: 2, c: 2.5, d: true, e: undefined, f: null, g: NaN }), [
    { key: "a", value: { stringValue: "x" } },
    { key: "b", value: { intValue: "2" } },
    { key: "c", value: { doubleValue: 2.5 } },
    { key: "d", value: { boolValue: true } },
  ]);
  // A float cannot hold nanoseconds since 1970; the string can.
  assert.equal(nanos(1791644400123.25), "1791644400123250000");
  assert.equal(nanos(0), "0");
});
