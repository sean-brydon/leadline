import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const { appRoute, routeMethods, scanRoutes } = require("../dist/routes.cjs");

/** A made-up project: every path is a file with the given text. */
function project(files) {
  const dir = mkdtempSync(join(tmpdir(), "leadline-next-routes-"));
  for (const [path, text] of Object.entries(files)) {
    mkdirSync(dirname(join(dir, path)), { recursive: true });
    writeFileSync(join(dir, path), text);
  }
  return dir;
}

const brief = (routes) => routes.map((r) => `${r.kind} ${r.route} <- ${r.file}`);

test("app routes: groups and slots are not in the path, private folders are not routes", (t) => {
  const dir = project({
    "app/layout.tsx": "",
    "app/page.tsx": "",
    "app/(shop)/products/[id]/page.tsx": "",
    "app/(shop)/layout.tsx": "",
    "app/blog/[...slug]/page.jsx": "",
    "app/docs/[[...path]]/page.tsx": "",
    "app/@modal/login/page.tsx": "",
    "app/_components/button.tsx": "",
    "app/_components/page.tsx": "",
    "app/feed/(.)photo/[id]/page.tsx": "",
    "app/api/orders/route.ts": "export async function GET() {}\nexport const POST = async () => {}",
    "app/sitemap.ts": "",
    "app/products/styles.css": "",
    "app/products/helpers.ts": "",
  });
  t.after(() => rmSync(dir, { recursive: true, force: true }));

  assert.deepEqual(brief(scanRoutes(dir)), [
    "layout / <- app/(shop)/layout.tsx",
    "layout / <- app/layout.tsx",
    "page / <- app/page.tsx",
    "route /api/orders <- app/api/orders/route.ts",
    "page /blog/[...slug] <- app/blog/[...slug]/page.jsx",
    "page /docs/[[...path]] <- app/docs/[[...path]]/page.tsx",
    "page /feed/(.)photo/[id] <- app/feed/(.)photo/[id]/page.tsx",
    "page /login <- app/@modal/login/page.tsx",
    "page /products/[id] <- app/(shop)/products/[id]/page.tsx",
    "metadata /sitemap <- app/sitemap.ts",
  ]);
});

test("a page knows its layouts, a route handler its methods, and both whether they are dynamic", (t) => {
  const dir = project({
    "app/layout.tsx": "",
    "app/(shop)/layout.tsx": "",
    "app/(shop)/products/[id]/page.tsx": "",
    "app/api/orders/[id]/route.ts": "export function GET() {}\nexport async function DELETE() {}\n// export function PUT",
  });
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const routes = scanRoutes(dir);

  const page = routes.find((r) => r.kind === "page");
  assert.deepEqual(page, {
    route: "/products/[id]",
    kind: "page",
    router: "app",
    file: "app/(shop)/products/[id]/page.tsx",
    dynamic: true,
    layouts: ["app/layout.tsx", "app/(shop)/layout.tsx"],
  });
  const handler = routes.find((r) => r.kind === "route");
  assert.equal(handler.route, "/api/orders/[id]");
  assert.equal(handler.dynamic, true);
  // The regular expression reads source, so a commented-out export counts too.
  assert.deepEqual(handler.methods, ["GET", "DELETE", "PUT"]);
});

test("pages routes: index files, API routes, and no _app or _document", (t) => {
  const dir = project({
    "pages/index.tsx": "",
    "pages/_app.tsx": "",
    "pages/_document.tsx": "",
    "pages/about.js": "",
    "pages/blog/[slug].tsx": "",
    "pages/blog/index.tsx": "",
    "pages/api/health.ts": "",
    "pages/api/users/[id].ts": "",
    "pages/readme.md": "",
  });
  t.after(() => rmSync(dir, { recursive: true, force: true }));

  assert.deepEqual(brief(scanRoutes(dir)), [
    "page / <- pages/index.tsx",
    "page /about <- pages/about.js",
    "api /api/health <- pages/api/health.ts",
    "api /api/users/[id] <- pages/api/users/[id].ts",
    "page /blog <- pages/blog/index.tsx",
    "page /blog/[slug] <- pages/blog/[slug].tsx",
  ]);
});

test("routes under src/, with the proxy file beside them", (t) => {
  const dir = project({ "src/app/page.tsx": "", "src/proxy.ts": "", "src/pages/api/ping.ts": "" });
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  assert.deepEqual(brief(scanRoutes(dir)), [
    "proxy  <- src/proxy.ts",
    "page / <- src/app/page.tsx",
    "api /api/ping <- src/pages/api/ping.ts",
  ]);
});

test("pageExtensions decide what is a page", (t) => {
  const dir = project({ "app/page.tsx": "", "app/docs/page.mdx": "", "app/blog/page.page.tsx": "", "middleware.ts": "" });
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  assert.deepEqual(brief(scanRoutes(dir, ["tsx", "mdx"])), ["page / <- app/page.tsx", "page /docs <- app/docs/page.mdx"]);
  // The default set still finds middleware.ts, the proxy's name before Next 16.
  assert.ok(brief(scanRoutes(dir)).includes("proxy  <- middleware.ts"));
});

test("a project with no route folders has no routes", (t) => {
  const dir = project({ "package.json": "{}" });
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  assert.deepEqual(scanRoutes(dir), []);
  assert.deepEqual(routeMethods(join(dir, "missing.ts")), []);
});

test("appRoute", () => {
  assert.equal(appRoute([]), "/");
  assert.equal(appRoute(["(marketing)", "about"]), "/about");
  assert.equal(appRoute(["@team", "settings"]), "/settings");
  assert.equal(appRoute(["%5Fprivate"]), "/_private");
  assert.equal(appRoute(["feed", "(..)photo", "[id]"]), "/feed/(..)photo/[id]");
});
