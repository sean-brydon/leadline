import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import * as http from "node:http";
import { test } from "node:test";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const { installRelay, RELAY_PATH, sameOrigin } = require("../dist/relay.cjs");

/** A request and a response good enough for the relay: no socket is opened. */
function exchange(method, url, headers = {}, body = "") {
  const request = Object.assign(new EventEmitter(), { method, url, headers, socket: { localPort: 3000 }, destroy() {} });
  const response = {
    statusCode: 0,
    headersSent: false,
    headers: {},
    setHeader(name, value) {
      this.headers[name] = value;
    },
    end(body) {
      this.headersSent = true;
      this.body = body;
    },
  };
  const send = () => {
    if (body) request.emit("data", Buffer.from(body));
    request.emit("end");
  };
  return { request, response, send };
}

test("the relay answers its path before the app's own handler, and only that path", (t) => {
  const got = [];
  const remove = installRelay((body, headers, port) => got.push({ body, cookie: headers.cookie, port }));
  t.after(remove);
  const server = http.createServer();
  const seenByApp = [];
  server.on("request", (request) => seenByApp.push(request.url));

  const report = exchange("POST", RELAY_PATH, { cookie: "a=1", "sec-fetch-site": "same-origin" }, '{"e":[]}');
  server.emit("request", report.request, report.response);
  report.send();
  assert.equal(report.response.statusCode, 200);
  assert.equal(report.response.body, "ok");
  assert.equal(report.response.headers["cache-control"], "no-store");
  assert.deepEqual(got, [{ body: '{"e":[]}', cookie: "a=1", port: 3000 }]);

  const page = exchange("GET", "/orders");
  server.emit("request", page.request, page.response);
  const near = exchange("POST", RELAY_PATH + "x");
  server.emit("request", near.request, near.response);
  assert.deepEqual(seenByApp, ["/orders", RELAY_PATH + "x"]);
  assert.equal(got.length, 1);
});

test("only a POST from the same site is taken", (t) => {
  const got = [];
  t.after(installRelay((body) => got.push(body)));
  const server = http.createServer();
  const status = (method, headers) => {
    const { request, response, send } = exchange(method, RELAY_PATH, headers, "{}");
    server.emit("request", request, response);
    send();
    return response.statusCode;
  };
  assert.equal(status("GET", {}), 405);
  assert.equal(status("POST", { "sec-fetch-site": "cross-site" }), 403);
  assert.equal(status("POST", { origin: "https://elsewhere.example", host: "localhost:3000" }), 403);
  assert.equal(got.length, 0);
  assert.equal(status("POST", { origin: "http://localhost:3000", host: "localhost:3000" }), 200);
  assert.equal(got.length, 1);
});

test("a body that is too large is refused", (t) => {
  const got = [];
  t.after(installRelay((body) => got.push(body)));
  const { request, response } = exchange("POST", RELAY_PATH, {});
  http.createServer().emit("request", request, response);
  request.emit("data", Buffer.alloc(300 * 1024));
  request.emit("end");
  assert.equal(response.statusCode, 413);
  assert.equal(got.length, 0);
});

test("removing the relay puts the server back as it was", () => {
  const before = http.Server.prototype.emit;
  const remove = installRelay(() => {});
  assert.notEqual(http.Server.prototype.emit, before);
  remove();
  assert.equal(http.Server.prototype.emit, before);
  assert.equal(Object.hasOwn(http.Server.prototype, "emit"), false);
});

test("sameOrigin", () => {
  assert.equal(sameOrigin({ "sec-fetch-site": "same-origin" }), true);
  assert.equal(sameOrigin({ "sec-fetch-site": "same-site" }), false);
  assert.equal(sameOrigin({ origin: "http://localhost:3000", host: "localhost:3000" }), true);
  assert.equal(sameOrigin({ origin: "http://localhost:4000", host: "localhost:3000" }), false);
  assert.equal(sameOrigin({ origin: "null", host: "localhost:3000" }), false);
  // No browser sends neither header on a cross-site POST; curl sends neither at all.
  assert.equal(sameOrigin({ host: "localhost:3000" }), true);
});
