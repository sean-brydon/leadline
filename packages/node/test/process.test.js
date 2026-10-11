// The provider inside a real Node process. No daemon runs here: these are
// the cases where it must stay out of the way.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { createServer } from "node:net";
import { fileURLToPath } from "node:url";
import { test } from "node:test";

const register = fileURLToPath(new URL("../dist/register.js", import.meta.url));
const index = new URL("../dist/index.js", import.meta.url).href;

/** Runs `node [args] -e script` and resolves with what it printed and how it ended. */
function run(script, { env = {}, args = ["--import", register] } = {}) {
  return new Promise((resolve, reject) => {
    const started = Date.now();
    const child = spawn(process.execPath, [...args, "-e", script], {
      // A clean environment: the developer's own LEADLINE_* and NODE_OPTIONS
      // must not decide how these tests go.
      env: { PATH: process.env.PATH, ...env },
      stdio: ["ignore", "pipe", "pipe"],
    });
    let stdout = "";
    let stderr = "";
    child.stdout.on("data", (chunk) => (stdout += chunk));
    child.stderr.on("data", (chunk) => (stderr += chunk));
    child.on("error", reject);
    child.on("close", (code) => resolve({ code, stdout, stderr, ms: Date.now() - started }));
  });
}

// The same app with no provider. It still preloads a module (an empty one),
// because --import itself changes the internal frames Node prints under an
// uncaught error.
const BARE = ["--import", "data:text/javascript,"];

/** A loopback port with nothing listening on it. */
async function closedPort() {
  const server = createServer();
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const { port } = server.address();
  await new Promise((resolve) => server.close(resolve));
  return port;
}

// What the provider leaves behind when it is running.
const STATE = `
  const loaded = Object.keys(require("node:module")._cache).filter((f) => f.includes("@opentelemetry")).length;
  console.log(JSON.stringify({
    otelModules: loaded,
    exitListeners: process.listenerCount("exit"),
    monitors: process.listenerCount("uncaughtExceptionMonitor"),
    consolePatched: !/native code/.test(Function.prototype.toString.call(console.log)) &&
      !Object.getOwnPropertySymbols(console.log).length && console.log.length === 0 && console.log.name === "log" &&
      String(console.log).includes("emitting"),
  }));
`;

test("NODE_ENV=production: nothing is loaded, patched or listened for", async () => {
  const { code, stdout, stderr } = await run(STATE, { env: { NODE_ENV: "production" } });
  assert.equal(code, 0);
  assert.equal(stderr, "");
  assert.deepEqual(JSON.parse(stdout), {
    otelModules: 0,
    exitListeners: 0,
    monitors: 0,
    consolePatched: false,
  });
});

test("NODE_ENV=production with LEADLINE_FORCE=1 runs", async () => {
  const port = await closedPort();
  const { code, stdout, stderr } = await run(STATE, {
    env: { NODE_ENV: "production", LEADLINE_FORCE: "1", LEADLINE_URL: `http://127.0.0.1:${port}` },
  });
  assert.equal(code, 0);
  assert.equal(stderr, "");
  const state = JSON.parse(stdout);
  assert.ok(state.otelModules > 100, `only ${state.otelModules} OpenTelemetry modules loaded`);
  assert.equal(state.exitListeners, 1);
  assert.equal(state.monitors, 1);
  assert.equal(state.consolePatched, true);
});

test("register() reports why it is idle, and is the same object when called twice", async () => {
  const script = `
    import(${JSON.stringify(index)}).then(({ register }) => {
      const first = register();
      const second = register({ force: true });
      console.log(JSON.stringify({ active: first.active, reason: first.reason, same: first === second }));
    });
  `;
  const { stdout, stderr } = await run(script, { env: { NODE_ENV: "production" }, args: [] });
  assert.equal(stderr, "");
  assert.deepEqual(JSON.parse(stdout), { active: false, reason: "production", same: true });
});

test("no daemon: the app runs, prints what it always printed, and exits on time", async () => {
  const port = await closedPort();
  const script = `
    const http = require("node:http");
    const server = http.createServer((req, res) => {
      console.log("handling", req.url);
      console.warn("a warning");
      res.end("ok");
    });
    server.listen(0, "127.0.0.1", async () => {
      const res = await fetch("http://127.0.0.1:" + server.address().port + "/hello");
      console.log("status", res.status, await res.text());
      server.close();
    });
  `;
  const env = { LEADLINE_URL: `http://127.0.0.1:${port}` };
  const without = await run(script, { args: BARE });
  const withProvider = await run(script, { env });

  assert.equal(withProvider.code, 0);
  assert.equal(withProvider.stdout, "handling /hello\nstatus 200 ok\n");
  assert.equal(withProvider.stdout, without.stdout);
  // Nothing about the missing daemon, from the provider or from the SDK.
  assert.equal(withProvider.stderr, "a warning\n");
  assert.equal(withProvider.stderr, without.stderr);
  // No retry timers holding the process open.
  assert.ok(withProvider.ms < 3000, `took ${withProvider.ms}ms`);
});

test("no daemon: an uncaught exception still kills the process the same way", async () => {
  const port = await closedPort();
  const script = `setTimeout(() => { throw new Error("acme went wrong"); }, 10);`;
  const without = await run(script, { args: BARE });
  const withProvider = await run(script, { env: { LEADLINE_URL: `http://127.0.0.1:${port}` } });

  assert.equal(withProvider.code, 1);
  assert.equal(without.code, 1);
  assert.match(withProvider.stderr, /Error: acme went wrong/);
  assert.equal(withProvider.stderr, without.stderr);
});

test("no daemon: an unhandled rejection still kills the process the same way", async () => {
  const port = await closedPort();
  const script = `Promise.reject(new Error("acme rejected"));`;
  const without = await run(script, { args: BARE });
  const withProvider = await run(script, { env: { LEADLINE_URL: `http://127.0.0.1:${port}` } });

  assert.equal(withProvider.code, 1);
  assert.match(withProvider.stderr, /Error: acme rejected/);
  assert.equal(withProvider.stderr, without.stderr);
});

test("an app's own uncaughtException handler still gets the error", async () => {
  const port = await closedPort();
  const script = `
    process.on("uncaughtException", (err) => { console.log("handled:", err.message); });
    setTimeout(() => { throw new Error("acme recoverable"); }, 10);
  `;
  const { code, stdout } = await run(script, { env: { LEADLINE_URL: `http://127.0.0.1:${port}` } });
  assert.equal(code, 0);
  assert.equal(stdout, "handled: acme recoverable\n");
});

test("a bad LEADLINE_URL is one line on stderr, not a crash", async () => {
  const { code, stdout, stderr } = await run(`console.log("app ran")`, { env: { LEADLINE_URL: "nonsense" } });
  assert.equal(code, 0);
  assert.equal(stdout, "app ran\n");
  assert.equal(stderr, `[leadline] not started: "nonsense" is not an http(s) URL\n`);
});
