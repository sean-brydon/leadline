import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { test } from "node:test";
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";

const require = createRequire(import.meta.url);
const { callerDir, devConfig, DRIVER_PACKAGES, isDevServer, withLeadline } = require("../dist/index.cjs");
const entry = fileURLToPath(new URL("../dist/index.cjs", import.meta.url));

test("only `next dev`'s own process counts as the dev server", () => {
  assert.equal(isDevServer({ NODE_ENV: "development", __NEXT_DEV_SERVER: "1" }), true);
  // Next 15 has no such variable; `next dev` marks its server another way.
  assert.equal(isDevServer({ NODE_ENV: "development", NEXT_PRIVATE_WORKER: "1" }), true);
  // next build, and next start.
  assert.equal(isDevServer({ NODE_ENV: "production" }), false);
  assert.equal(isDevServer({ NODE_ENV: "production", __NEXT_DEV_SERVER: "1", NEXT_PRIVATE_WORKER: "1" }), false);
  // `next build --debug-prerender` builds with NODE_ENV=development.
  assert.equal(isDevServer({ NODE_ENV: "development" }), false);
  assert.equal(isDevServer({}), false);
});

test("outside the dev server the config comes back untouched and nothing is loaded", () => {
  // In a process of its own, so what was loaded can be read off require.cache.
  const script = `
    const { withLeadline } = require(${JSON.stringify(entry)});
    const config = { reactStrictMode: true, serverExternalPackages: ["sharp"] };
    const fn = () => config;
    const loaded = () => Object.keys(require.cache).filter((file) => !file.endsWith("index.cjs"));
    console.log(JSON.stringify({
      sameObject: withLeadline(config) === config,
      sameFunction: withLeadline(fn) === fn,
      unchanged: JSON.stringify(config),
      loaded: loaded(),
      global: Object.getOwnPropertySymbols(globalThis).map(String).filter((s) => s.includes("leadline")),
    }));`;
  for (const env of [{ NODE_ENV: "production" }, { NODE_ENV: "production", __NEXT_DEV_SERVER: "1" }, { NODE_ENV: "development" }]) {
    const out = JSON.parse(execFileSync(process.execPath, ["-e", script], { env: { PATH: process.env.PATH, ...env }, encoding: "utf8" }));
    assert.deepEqual(out, {
      sameObject: true,
      sameFunction: true,
      unchanged: '{"reactStrictMode":true,"serverExternalPackages":["sharp"]}',
      loaded: [],
      global: [],
    });
  }
});

test("in this test process, which is no dev server, withLeadline is the identity", () => {
  const config = { distDir: "build" };
  assert.equal(withLeadline(config), config);
});

test("the dev config keeps database drivers out of the server bundle, and the input as it was", () => {
  const config = { reactStrictMode: true, serverExternalPackages: ["sharp", "mysql2"], transpilePackages: ["knex"] };
  const dev = devConfig(config);
  assert.notEqual(dev, config);
  assert.deepEqual(config.serverExternalPackages, ["sharp", "mysql2"]);
  assert.equal(dev.reactStrictMode, true);
  assert.deepEqual(dev.serverExternalPackages.slice(0, 2), ["sharp", "mysql2"]);
  assert.equal(dev.serverExternalPackages.filter((name) => name === "mysql2").length, 1);
  // Next.js refuses a package that is both transpiled and external.
  assert.equal(dev.serverExternalPackages.includes("knex"), false);
  for (const name of DRIVER_PACKAGES.filter((n) => n !== "knex")) assert.ok(dev.serverExternalPackages.includes(name), name);

  assert.equal(devConfig(config, { externalPackages: false }), config);
  assert.deepEqual(devConfig({}, { externalPackages: ["postgres"] }).serverExternalPackages, ["postgres"]);
  assert.deepEqual(devConfig({ serverExternalPackages: ["postgres"] }, { externalPackages: ["postgres"] }), {
    serverExternalPackages: ["postgres"],
  });
});

test("the project folder is read from the stack, where next.config called from", () => {
  const cjs = `Error: x
    at withLeadline (/repo/node_modules/@leadline/next/dist/index.cjs:8:59)
    at Object.<anonymous> (/repo/apps/web/next.config.compiled.js:24:41)
    at Module._compile (node:internal/modules/cjs/loader:1705:14)`;
  assert.equal(callerDir(cjs), "/repo/apps/web");
  const esm = `Error: x
    at withLeadline (/repo/node_modules/@leadline/next/dist/index.cjs:8:59)
    at file:///repo/apps/my%20app/next.config.mjs:5:16
    at ModuleJob.run (node:internal/modules/esm/module_job:268:25)`;
  assert.equal(callerDir(esm), "/repo/apps/my app");
  assert.equal(callerDir("Error: x\n    at somewhere (/repo/server.js:1:1)"), undefined);
  assert.equal(callerDir(undefined), undefined);
});
