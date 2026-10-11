import assert from "node:assert/strict";
import test from "node:test";
import { quietRoutes } from "./routes.ts";

const routes = [
  { service: "acme-shop", route: "/orders/[id]", kind: "page", file: "app/orders/[id]/page.tsx" },
  { service: "acme-shop", route: "/api/health", kind: "api", file: "pages/api/health.ts" },
  { service: "acme-shop", route: "/sign-in", kind: "page" },
];

test("with no filter every never-hit route is listed", () => {
  assert.equal(quietRoutes(routes, ""), routes);
  assert.equal(quietRoutes(routes, "  "), routes);
});

test("the list's filter matches a route or its file, whatever the case", () => {
  assert.deepEqual(quietRoutes(routes, "ORDERS").map((r) => r.route), ["/orders/[id]"]);
  assert.deepEqual(quietRoutes(routes, "pages/").map((r) => r.route), ["/api/health"]);
  assert.deepEqual(quietRoutes(routes, "sign").map((r) => r.route), ["/sign-in"]);
  assert.deepEqual(quietRoutes(routes, "checkout"), []);
});
