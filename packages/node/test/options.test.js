import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, realpathSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { after, test } from "node:test";

import { resolveOptions } from "../dist/options.js";
import { resolveServiceName } from "../dist/service.js";

const root = realpathSync(mkdtempSync(join(tmpdir(), "leadline-test-")));
after(() => rmSync(root, { recursive: true, force: true }));

/** Makes `dir` under the temp root, with a package.json when `pkg` is given. */
function dir(path, pkg) {
  const full = join(root, path);
  mkdirSync(full, { recursive: true });
  if (pkg !== undefined) writeFileSync(join(full, "package.json"), typeof pkg === "string" ? pkg : JSON.stringify(pkg));
  return full;
}

const where = { cwd: dir("plain", { name: "acme-plain" }) };

test("production is a no-op unless forced", () => {
  const production = resolveOptions({}, { NODE_ENV: "production" }, where);
  assert.equal(production.active, false);
  assert.equal(production.reason, "production");

  assert.equal(resolveOptions({ force: true }, { NODE_ENV: "production" }, where).active, true);
  assert.equal(resolveOptions({}, { NODE_ENV: "production", LEADLINE_FORCE: "1" }, where).active, true);
  // The option wins over the variable.
  assert.equal(
    resolveOptions({ force: false }, { NODE_ENV: "production", LEADLINE_FORCE: "1" }, where).active,
    false,
  );
});

test("development, test and no NODE_ENV all run", () => {
  for (const NODE_ENV of ["development", "test", undefined]) {
    assert.equal(resolveOptions({}, { NODE_ENV }, where).active, true, String(NODE_ENV));
  }
});

test("it can be switched off, by option or by either variable", () => {
  assert.equal(resolveOptions({ enabled: false }, {}, where).reason, "disabled");
  assert.equal(resolveOptions({}, { LEADLINE_DISABLED: "1" }, where).reason, "disabled");
  assert.equal(resolveOptions({}, { OTEL_SDK_DISABLED: "true" }, where).reason, "disabled");
  assert.equal(resolveOptions({ enabled: true }, { LEADLINE_DISABLED: "1" }, where).active, true);
});

test("the daemon address comes from the option, then LEADLINE_URL, then the default", () => {
  assert.equal(resolveOptions({}, {}, where).url, "http://127.0.0.1:4320");
  assert.equal(resolveOptions({}, { LEADLINE_URL: "http://127.0.0.1:4322/" }, where).url, "http://127.0.0.1:4322");
  assert.equal(
    resolveOptions({ url: "http://localhost:9" }, { LEADLINE_URL: "http://127.0.0.1:4322" }, where).url,
    "http://localhost:9",
  );
  assert.equal(resolveOptions({}, { LEADLINE_URL: "" }, where).url, "http://127.0.0.1:4320");
  // A collector address meant for production must not attract dev data.
  assert.equal(
    resolveOptions({}, { OTEL_EXPORTER_OTLP_ENDPOINT: "https://collector.acme.example" }, where).url,
    "http://127.0.0.1:4320",
  );
});

test("an address that is not http(s) stops it instead of throwing", () => {
  for (const url of ["not a url", "grpc://127.0.0.1:4317", ""]) {
    const resolved = resolveOptions({ url }, {}, where);
    assert.equal(resolved.active, false, url);
    assert.equal(resolved.reason, "invalid-url", url);
  }
});

test("defaults: console on, metrics off, half-second batches", () => {
  const resolved = resolveOptions({}, {}, where);
  assert.equal(resolved.console, true);
  assert.equal(resolved.metrics, false);
  assert.equal(resolved.batchDelayMs, 500);
  assert.equal(resolved.debug, false);

  const tuned = resolveOptions({ batchDelayMs: 50 }, { LEADLINE_CONSOLE: "0", LEADLINE_METRICS: "1", LEADLINE_DEBUG: "true" }, where);
  assert.equal(tuned.console, false);
  assert.equal(tuned.metrics, true);
  assert.equal(tuned.batchDelayMs, 50);
  assert.equal(tuned.debug, true);
  assert.equal(resolveOptions({ batchDelayMs: -1 }, {}, where).batchDelayMs, 500);
});

test("service name: OTEL_SERVICE_NAME first", () => {
  const cwd = dir("named", { name: "acme-api" });
  assert.equal(resolveServiceName({ OTEL_SERVICE_NAME: "checkout" }, cwd), "checkout");
  assert.equal(resolveOptions({ serviceName: "explicit" }, { OTEL_SERVICE_NAME: "checkout" }, { cwd }).serviceName, "explicit");
  assert.equal(
    resolveServiceName({ OTEL_RESOURCE_ATTRIBUTES: "deployment.environment=dev,service.name=from%20attrs" }, cwd),
    "from attrs",
  );
});

test("service name: the nearest package.json that has a name", () => {
  const app = dir("mono/apps/api", { name: "@acme/api" });
  dir("mono", { name: "acme-monorepo" });
  const deep = dir("mono/apps/api/src/routes");
  assert.equal(resolveServiceName({}, deep), "@acme/api");
  assert.equal(resolveServiceName({}, app), "@acme/api");

  // A nameless package.json (a build folder's module marker) is skipped.
  const dist = dir("mono/apps/api/dist", { type: "module" });
  assert.equal(resolveServiceName({}, dist), "@acme/api");
  // So is one that does not parse.
  const broken = dir("mono/apps/broken", "{ not json");
  assert.equal(resolveServiceName({}, broken), "acme-monorepo");
});

test("service name: the script's package beats the working directory's, unless it is a tool", () => {
  const rootDir = dir("repo", { name: "acme-repo" });
  const api = dir("repo/services/billing", { name: "acme-billing" });
  assert.equal(resolveServiceName({}, rootDir, join(api, "server.js")), "acme-billing");

  // `next dev`, `tsx` and friends run from node_modules: that is not the app.
  const tool = dir("repo/node_modules/some-cli/bin", undefined);
  dir("repo/node_modules/some-cli", { name: "some-cli" });
  assert.equal(resolveServiceName({}, rootDir, join(tool, "cli.js")), "acme-repo");
});

test("service name: the folder name when there is no package.json at all", () => {
  // Nothing above the temp root is ours to control, so only check the shape.
  const bare = dir("bare-folder");
  const name = resolveServiceName({}, bare);
  assert.equal(typeof name, "string");
  assert.ok(name.length > 0);
});
