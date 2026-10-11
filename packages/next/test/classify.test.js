import assert from "node:assert/strict";
import { test } from "node:test";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const { classify, covered, isNoise, mergeIntervals, pathOf, readHints } = require("../dist/classify.cjs");

const hints = (over = {}) => ({ method: "GET", path: "/blog/hello", rsc: false, prefetch: false, ...over });
const seen = (route, ...types) => ({ route, types: new Set(types) });

test("the dev server's own traffic is noise; the app's is not", () => {
  for (const path of [
    "/_next/static/chunks/main.js",
    "/_next/webpack-hmr",
    "/__nextjs_original-stack-frames",
    "/__leadline/b",
    "/favicon.ico",
    "/.well-known/appspecific/com.chrome.devtools.json",
  ]) {
    assert.equal(isNoise(path), true, path);
  }
  for (const path of ["/", "/blog/hello", "/_next/image", "/_next/data/dev/blog.json", "/logo.png", "/api/orders"]) {
    assert.equal(isNoise(path), false, path);
  }
});

test("hints come from the request's own headers", () => {
  assert.deepEqual(readHints("post", "/orders?page=2", { "next-action": "40abc", rsc: "1" }), {
    method: "POST",
    path: "/orders",
    actionId: "40abc",
    rsc: true,
    prefetch: false,
  });
  assert.deepEqual(readHints(undefined, undefined, { "next-router-prefetch": ["1"] }), {
    method: "GET",
    path: "/",
    rsc: false,
    prefetch: true,
  });
  assert.equal(pathOf("/a/b#c"), "/a/b");
});

test("a page is named by its pattern", () => {
  const got = classify(hints(), seen("/blog/[slug]", "BaseServer.handleRequest", "AppRender.getBodyResult"), 200);
  assert.equal(got.kind, "page");
  assert.equal(got.name, "GET /blog/[slug]");
  assert.equal(got.route, "/blog/[slug]");
  assert.deepEqual(got.attributes, { "next.kind": "page", "next.route": "/blog/[slug]" });
});

test("a Flight request for a page is named the way Next.js names it", () => {
  const got = classify(hints({ rsc: true, prefetch: true }), seen("/blog/[slug]", "AppRender.getBodyResult"), 200);
  assert.equal(got.name, "RSC GET /blog/[slug]");
  assert.equal(got.attributes["next.rsc"], true);
  assert.equal(got.attributes["next.prefetch"], true);
});

test("a server action is named after the action, and keeps the page as its route", () => {
  const action = hints({ method: "POST", path: "/orders", actionId: "40abc" });
  const named = classify(action, seen("/orders", "AppRender.getBodyResult"), 200, { name: "addNote", file: "app/actions.ts" });
  assert.equal(named.kind, "action");
  assert.equal(named.name, "action addNote");
  assert.equal(named.route, "/orders");
  assert.deepEqual(named.attributes, {
    "next.action.id": "40abc",
    "next.action.file": "app/actions.ts",
    "next.action.name": "addNote",
    "next.kind": "action",
    "next.route": "/orders",
  });

  // Written inline in a component: there is no name, only where it is.
  assert.equal(classify(action, seen("/orders"), 200, { file: "app/orders/page.tsx" }).name, "action (inline) app/orders/page.tsx");
  // Not in the manifest at all.
  assert.equal(classify(action, seen("/orders"), 200).name, "action /orders");
});

test("route handlers and Pages Router API routes", () => {
  const route = classify(hints({ path: "/api/orders/7" }), seen("/api/orders/[id]", "AppRouteRouteHandlers.runHandler"), 200);
  assert.equal(route.kind, "route");
  assert.equal(route.name, "GET /api/orders/[id]");
  const api = classify(hints({ path: "/api/health" }), seen("/api/health", "Node.runHandler"), 200);
  assert.equal(api.kind, "api");
  assert.equal(api.name, "GET /api/health");
});

test("a Pages Router page", () => {
  const got = classify(hints({ path: "/blog/x" }), seen("/blog/[slug]", "Render.getServerSideProps", "Render.renderDocument"), 200);
  assert.equal(got.kind, "page");
  assert.equal(got.name, "GET /blog/[slug]");
});

test("every 404 is one endpoint, and so is the error page", () => {
  const missing = classify(hints({ path: "/nope" }), seen("/_not-found", "AppRender.getBodyResult"), 404);
  assert.equal(missing.kind, "not-found");
  assert.equal(missing.name, "GET /_not-found");
  assert.equal(classify(hints(), seen("/_error", "Render.renderDocument"), 500).kind, "error");
  // A page that called notFound() is still that page.
  assert.equal(classify(hints(), seen("/blog/[slug]", "AppRender.getBodyResult"), 404).kind, "page");
});

test("what never reached a route keeps its path", () => {
  const asset = classify(hints({ path: "/logo.png" }), seen(undefined, "Middleware.execute"), 200);
  assert.equal(asset.kind, "asset");
  assert.equal(asset.name, "GET /logo.png");
  assert.equal(asset.route, "/logo.png");
  assert.equal(classify(hints({ path: "/old" }), seen(undefined, "Middleware.execute"), 307).kind, "proxy");
  assert.equal(classify(hints({ path: "/gone.png" }), seen(undefined), 404).kind, "not-found");
  assert.equal(classify(hints({ path: "/x" }), seen(undefined, "BaseServer.handleRequest"), 200).kind, "unknown");
});

test("overlapping waits are counted once", () => {
  const merged = mergeIntervals([[10, 20], [15, 30], [40, 50], [50, 50], [45, 48]]);
  assert.deepEqual(merged, [[10, 30], [40, 50]]);
  assert.equal(covered(merged, 0, Infinity), 30);
  assert.equal(covered(merged, 25, 45), 10);
  assert.equal(covered(merged, 60, 70), 0);
});
