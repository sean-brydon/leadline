import assert from "node:assert/strict";
import { test } from "node:test";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const { context, SpanKind, trace, ROOT_CONTEXT } = require("@opentelemetry/api");
const { AsyncLocalStorageContextManager } = require("@opentelemetry/context-async-hooks");
const { TracerProvider } = require("@opentelemetry/sdk-trace");
const { Requests, userAttributes } = require("../dist/requests.cjs");
const { readHints } = require("../dist/classify.cjs");

context.setGlobalContextManager(new AsyncLocalStorageContextManager().enable());

/** A tracer provider with the Requests processor in front of one that collects what ended, as the provider has it. */
function setup(options) {
  const requests = new Requests(options);
  const ended = [];
  const collect = { onStart() {}, onEnd: (span) => void ended.push(span), forceFlush: async () => {}, shutdown: async () => {} };
  const provider = new TracerProvider({ spanProcessors: [requests.spans, collect] });
  const http = provider.getTracer("@opentelemetry/instrumentation-http");
  const next = provider.getTracer("next.js");
  const other = provider.getTracer("pg");

  /** Starts a request the way the http instrumentation does, and makes it the active span. */
  function incoming(method, url, headers = {}, parent = ROOT_CONTEXT) {
    const span = http.startSpan(method, { kind: SpanKind.SERVER, attributes: { "http.request.method": method, "url.path": url } }, parent);
    const request = { method, url, headers };
    requests.incoming(span, request, readHints(method, url, headers));
    return { span, request, ctx: trace.setSpan(parent, span) };
  }
  /** A Next.js span under `ctx`. */
  const nextSpan = (ctx, name, type, attributes = {}, kind) =>
    next.startSpan(name, { kind, attributes: { "next.span_type": type, ...attributes } }, ctx);
  const finished = () => ended;
  const exported = () => finished().filter((span) => requests.keep(span));
  return { requests, incoming, nextSpan, other, finished, exported };
}

test("a request is renamed to its route pattern when it ends", () => {
  const { incoming, nextSpan, exported } = setup({
    routes: () => [{ route: "/blog/[slug]", kind: "page", router: "app", file: "app/blog/[slug]/page.tsx", dynamic: true }],
  });
  const { span: root, ctx } = incoming("GET", "/blog/hello?x=1");
  assert.equal(root.name, "GET /blog/hello");

  const handle = nextSpan(ctx, "GET", "BaseServer.handleRequest", {}, SpanKind.SERVER);
  const render = nextSpan(trace.setSpan(ctx, handle), "render route (app) /blog/[slug]", "AppRender.getBodyResult", {
    "next.route": "/blog/[slug]",
  });
  render.end();
  handle.setAttributes({ "next.route": "/blog/[slug]", "http.route": "/blog/[slug]" });
  handle.end();
  root.setAttribute("http.response.status_code", 200);
  root.end();

  const [sent] = exported().filter((span) => span.kind === SpanKind.SERVER);
  assert.equal(sent.name, "GET /blog/[slug]");
  assert.equal(sent.attributes["http.route"], "/blog/[slug]");
  assert.equal(sent.attributes["next.kind"], "page");
  assert.equal(sent.attributes["next.file"], "app/blog/[slug]/page.tsx");
  assert.equal(typeof sent.attributes["leadline.render_ms"], "number");
});

test("Next.js's own request span stops claiming to be a second request", () => {
  const { incoming, nextSpan, exported } = setup();
  const { span: root, ctx } = incoming("GET", "/");
  nextSpan(ctx, "GET /", "BaseServer.handleRequest", { "next.route": "/" }, SpanKind.SERVER).end();
  root.end();
  assert.deepEqual(
    exported().map((span) => [span.name, span.kind]),
    [["GET /", SpanKind.INTERNAL], ["GET /", SpanKind.SERVER]],
  );
});

test("the proxy's pass through the request handler is not sent; the proxy's span is", () => {
  const { incoming, nextSpan, exported, finished } = setup({
    routes: () => [{ route: "", kind: "proxy", router: "app", file: "proxy.ts", dynamic: false }],
  });
  const { span: root, request, ctx } = incoming("GET", "/orders");
  // The request now names its own span as the parent, which is where Next.js
  // starts the proxy's span from.
  const [, traceId, spanId] = request.headers.traceparent.split("-");
  assert.equal(traceId, root.spanContext().traceId);
  assert.equal(spanId, root.spanContext().spanId);
  const remote = trace.setSpanContext(ROOT_CONTEXT, { traceId, spanId, traceFlags: 1, isRemote: true });

  const pass = nextSpan(ctx, "GET", "BaseServer.handleRequest", {}, SpanKind.SERVER);
  const proxy = nextSpan(remote, "middleware GET", "Middleware.execute");
  proxy.end();
  pass.setAttribute("next.bubble", true);
  pass.end();
  root.setAttribute("http.response.status_code", 200);
  root.end();

  assert.equal(finished().length, 3);
  const sent = exported();
  assert.deepEqual(sent.map((span) => span.name), ["middleware GET", "GET /orders"]);
  assert.equal(sent[0].attributes["next.kind"], "proxy");
  assert.equal(sent[0].attributes["next.file"], "proxy.ts");
  // The proxy alone does not make the request something Next.js routed.
  assert.equal(sent[1].attributes["next.kind"], "asset");
});

test("a server action takes its name from the manifest", () => {
  const { incoming, nextSpan, exported } = setup({
    action: (id) => (id === "40abc" ? { name: "addNote", file: "app/actions.ts" } : undefined),
  });
  const { span: root, ctx } = incoming("POST", "/orders", { "next-action": "40abc" });
  nextSpan(ctx, "POST /orders", "BaseServer.handleRequest", { "next.route": "/orders" }).end();
  root.end();
  const sent = exported().at(-1);
  assert.equal(sent.name, "action addNote");
  assert.equal(sent.attributes["http.route"], "/orders");
  assert.equal(sent.attributes["next.action.name"], "addNote");
  assert.equal(sent.attributes["next.action.file"], "app/actions.ts");
});

test("identify() marks the request's open spans, the ones that follow, and its logs", () => {
  const { requests, incoming, other, exported } = setup();
  const { span: root, ctx } = incoming("GET", "/account");
  const before = other.startSpan("pg.query:SELECT", { kind: SpanKind.CLIENT }, ctx);
  before.end();

  context.with(ctx, () => requests.identify({ id: 7, email: "ada@acme.example", name: "Ada Acme", token: "secret" }));
  const after = other.startSpan("pg.query:UPDATE", { kind: SpanKind.CLIENT }, ctx);

  const logged = {};
  const record = { spanContext: after.spanContext(), setAttributes: (attributes) => Object.assign(logged, attributes) };
  requests.logs.onEmit(record);
  after.end();
  root.end();

  const user = { "user.id": "7", "user.email": "ada@acme.example", "user.full_name": "Ada Acme" };
  const byName = Object.fromEntries(exported().map((span) => [span.name, span.attributes]));
  assert.equal(byName["pg.query:SELECT"]["user.id"], undefined, "a span that had already ended is not rewritten");
  for (const [key, value] of Object.entries(user)) {
    assert.equal(byName["pg.query:UPDATE"][key], value);
    assert.equal(byName["GET /account"][key], value);
  }
  assert.deepEqual(logged, user);
  assert.equal(JSON.stringify(byName).includes("secret"), false);
});

test("identify() outside a request, or with no id, does nothing", () => {
  const { requests, incoming, exported } = setup();
  requests.identify({ id: "nobody" });
  const { span: root, ctx } = incoming("GET", "/");
  context.with(ctx, () => requests.identify({ email: "no-id@acme.example" }));
  context.with(ctx, () => requests.identify(null));
  root.end();
  assert.equal(exported()[0].attributes["user.id"], undefined);
  assert.equal(exported()[0].attributes["user.email"], undefined);
});

test("identify() beats what a session cookie said, whichever comes first", () => {
  const { requests, incoming, exported } = setup();
  const first = incoming("GET", "/a");
  requests.identifyFromCookie(first.span.spanContext().spanId, { id: "from-cookie" });
  context.with(first.ctx, () => requests.identify({ id: "from-app" }));
  first.span.end();

  const second = incoming("GET", "/b");
  context.with(second.ctx, () => requests.identify({ id: "from-app" }));
  requests.identifyFromCookie(second.span.spanContext().spanId, { id: "from-cookie" });
  second.span.end();

  assert.deepEqual(exported().map((span) => span.attributes["user.id"]), ["from-app", "from-app"]);
});

test("the browser that sent the same cookies is the same user", () => {
  const { requests, incoming } = setup();
  const { span, ctx } = incoming("GET", "/", { cookie: "session=abc" });
  context.with(ctx, () => requests.identify({ id: "7" }));
  span.end();
  assert.deepEqual(requests.userForCookie("session=abc"), { "user.id": "7" });
  assert.equal(requests.userForCookie("session=other"), undefined);
  assert.equal(requests.userForCookie(undefined), undefined);
});

test("time waiting on data is separated from the render", () => {
  const { incoming, other, exported } = setup();
  const t0 = Date.now();
  const at = (ms) => new Date(t0 + ms);
  const { span: root, ctx } = incoming("GET", "/orders");
  // A 100 ms render holding two overlapping queries that cover 30 ms of it
  // between them, and a third query after the render.
  const render = other.startSpan("render", { startTime: at(100), attributes: { "next.span_type": "AppRender.getBodyResult" } }, ctx);
  const inRender = trace.setSpan(ctx, render);
  other.startSpan("q1", { kind: SpanKind.CLIENT, startTime: at(110) }, inRender).end(at(130));
  other.startSpan("q2", { kind: SpanKind.CLIENT, startTime: at(120) }, inRender).end(at(140));
  render.end(at(200));
  other.startSpan("q3", { kind: SpanKind.CLIENT, startTime: at(210) }, ctx).end(at(220));
  root.end(at(230));

  const sent = exported().at(-1);
  assert.equal(sent.attributes["leadline.render_ms"], 70);
  assert.equal(sent.attributes["leadline.data_ms"], 40);
});

test("the dev server's own calls home are not sent, nor what is under them", () => {
  const { other, exported, finished } = setup();
  const check = other.startSpan("fetch GET https://registry.npmjs.org/-/package/next/dist-tags", {
    kind: SpanKind.CLIENT,
    attributes: { "net.peer.name": "registry.npmjs.org" },
  });
  other.startSpan("GET", { kind: SpanKind.CLIENT }, trace.setSpan(ROOT_CONTEXT, check)).end();
  check.end();
  const telemetry = other.startSpan("POST", { kind: SpanKind.CLIENT });
  telemetry.setAttribute("server.address", "telemetry.nextjs.org");
  telemetry.end();
  other.startSpan("load instrumentation module", { attributes: { "next.span_type": "Instrumentation.loadModule" } }).end();
  const mine = other.startSpan("GET", { kind: SpanKind.CLIENT, attributes: { "server.address": "api.acme.example" } });
  mine.end();

  assert.equal(finished().length, 5);
  assert.deepEqual(exported().map((span) => span.attributes["server.address"]), ["api.acme.example"]);
});

test("a browser span is told what became of the request it caused", async () => {
  const { requests, incoming, nextSpan } = setup({ action: () => ({ name: "addNote" }) });
  const traceId = "0af7651916cd43dd8448eb211c80319c";
  const spanId = "b7ad6b7169203331";
  const remote = trace.setSpanContext(ROOT_CONTEXT, { traceId, spanId, traceFlags: 1, isRemote: true });
  const { span: root, ctx } = incoming("POST", "/orders", { "next-action": "40abc" }, remote);
  context.with(ctx, () => requests.identify({ id: "7" }));

  // Asked while the request is still running: the answer waits for it.
  const pending = requests.outcomeFor(traceId, spanId, 2000);
  nextSpan(ctx, "POST /orders", "BaseServer.handleRequest", { "next.route": "/orders" }).end();
  root.end();
  const outcome = await pending;
  assert.equal(outcome.name, "action addNote");
  assert.equal(outcome.route, "/orders");
  assert.deepEqual(outcome.user, { "user.id": "7" });
  // And afterwards it is remembered.
  assert.equal((await requests.outcomeFor(traceId, spanId)).name, "action addNote");
  assert.equal(await requests.outcomeFor(traceId, "0000000000000001"), undefined);
});

test("userAttributes takes an id, an email and a name, and nothing else", () => {
  assert.deepEqual(userAttributes({ id: 12, name: "Ada", email: "", sessionToken: "x" }), {
    "user.id": "12",
    "user.full_name": "Ada",
  });
  assert.equal(userAttributes({ id: "" }), undefined);
  assert.equal(userAttributes({ id: {} }), undefined);
  assert.equal(userAttributes("ada"), undefined);
  assert.equal(userAttributes(undefined), undefined);
});
