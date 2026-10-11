import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, rmSync, utimesSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const { Actions, describeAction, devDistDir } = require("../dist/actions.cjs");
const { applyAnswers, parseStack, served } = require("../dist/stacks.cjs");
const { routesRecord } = require("../dist/route-list.cjs");
const { format, hex } = await import("../dist/browser.js");

function project(t) {
  const dir = mkdtempSync(join(tmpdir(), "leadline-next-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  return dir;
}

test("a server action's id is looked up in the manifest Next.js writes", (t) => {
  const dir = project(t);
  const file = join(dir, ".next", "dev", "server", "server-reference-manifest.json");
  mkdirSync(join(dir, ".next", "dev", "server"), { recursive: true });
  mkdirSync(join(dir, "app"), { recursive: true });
  writeFileSync(join(dir, "app", "actions.ts"), "");
  const write = (node, when) => {
    writeFileSync(file, JSON.stringify({ node, edge: {}, encryptionKey: "x" }));
    utimesSync(file, when, when);
  };
  write({ "40abc": { exportedName: "addNote", filename: "app/actions.ts" } }, new Date(2026, 0, 1));

  const actions = new Actions(dir);
  assert.deepEqual(actions.lookup("40abc"), { name: "addNote", file: "app/actions.ts" });
  assert.equal(actions.lookup("nope"), undefined);

  // The manifest is rewritten when a file is compiled; the next lookup sees it.
  write(
    {
      "40abc": { exportedName: "addNote", filename: "app/actions.ts" },
      "7fdef": { exportedName: "$$RSC_SERVER_ACTION_0", filename: "app/orders/page.tsx" },
    },
    new Date(2026, 0, 2),
  );
  assert.deepEqual(actions.lookup("7fdef"), { file: "app/orders/page.tsx" });
  assert.equal(devDistDir(dir), join(dir, ".next", "dev"));
});

test("no manifest, no name, no error", (t) => {
  const dir = project(t);
  assert.equal(new Actions(dir).lookup("40abc"), undefined);
  assert.equal(devDistDir(dir), join(dir, ".next"));
  assert.equal(devDistDir(dir, "build"), join(dir, "build"));
});

test("an action's file is given from the project, wherever the bundler counts from", (t) => {
  const dir = project(t);
  mkdirSync(join(dir, "app"), { recursive: true });
  writeFileSync(join(dir, "app", "actions.ts"), "");
  // Turbopack, with its root above the project.
  assert.deepEqual(describeAction({ exportedName: "save", filename: "apps/web/app/actions.ts" }, dir), {
    name: "save",
    file: "app/actions.ts",
  });
  assert.equal(describeAction({ exportedName: "save", filename: join(dir, "app", "actions.ts") }, dir).file, "app/actions.ts");
  assert.equal(describeAction({ exportedName: "save", filename: "lib/gone.ts" }, dir).file, "lib/gone.ts");
  assert.deepEqual(describeAction({}, dir), {});
});

test("a browser stack is parsed frame by frame, whichever browser wrote it", () => {
  const chrome = parseStack(
    "Error: boom\n    at onClick (http://localhost:3000/_next/static/chunks/a.js:21:15)\n    at async http://localhost:3000/_next/static/chunks/b.js:7:3\n    at <anonymous>",
  );
  assert.deepEqual(chrome.map((line) => line.frame ?? null), [
    null,
    { method: "onClick", file: "http://localhost:3000/_next/static/chunks/a.js", line: 21, column: 15 },
    { method: "<unknown>", file: "http://localhost:3000/_next/static/chunks/b.js", line: 7, column: 3 },
    null,
  ]);
  const firefox = parseStack("onClick@http://localhost:3000/_next/static/chunks/a.js:21:15\n@http://localhost:3000/x.js:1:2");
  assert.deepEqual(firefox.map((line) => line.frame?.method), ["onClick", "<unknown>"]);
});

test("chunk URLs are asked about as files in the dev server's output", () => {
  assert.equal(
    served("http://localhost:3000/_next/static/chunks/a%20b.js?v=1", "/repo/.next/dev/"),
    "file:///repo/.next/dev/static/chunks/a%20b.js?v=1",
  );
  assert.equal(served("https://cdn.acme.example/lib.js", "/repo/.next/dev"), "https://cdn.acme.example/lib.js");
});

test("frames the dev server could place are rewritten; the rest are left alone", () => {
  const lines = parseStack(
    "Error: boom\n    at onClick (http://localhost:3000/_next/static/chunks/a.js:21:15)\n    at run (http://localhost:3000/_next/static/chunks/b.js:7:3)\n    at step (http://localhost:3000/_next/static/chunks/c.js:1:1)",
  );
  const answers = [
    { status: "fulfilled", value: { originalStackFrame: { file: "app/playground/buttons.tsx", line1: 21, column1: 17, methodName: "onClick" } } },
    { status: "rejected", reason: "no source map" },
    // The spelling before Next 16.
    { status: "fulfilled", value: { originalStackFrame: { file: "lib/steps.ts", lineNumber: 4, column: 9, methodName: "" } } },
  ];
  assert.equal(
    applyAnswers(lines, answers),
    "Error: boom\n    at onClick (app/playground/buttons.tsx:21:17)\n    at run (http://localhost:3000/_next/static/chunks/b.js:7:3)\n    at step (lib/steps.ts:4:9)",
  );
  // An answer in a shape this does not know changes nothing.
  const raw = lines.map((line) => line.text).join("\n");
  assert.equal(applyAnswers(lines, { error: "changed" }), raw);
  assert.equal(applyAnswers(lines, []), raw);
});

test("the route list goes out as one log record with a JSON body", () => {
  const routes = [{ route: "/blog/[slug]", kind: "page", router: "app", file: "app/blog/[slug]/page.tsx", dynamic: true }];
  const request = routesRecord({ dir: "/repo/acme", service: "acme-shop", nextVersion: "16.4.0" }, routes, 1791644400123);
  const [resourceLogs] = request.resourceLogs;
  assert.deepEqual(resourceLogs.resource.attributes, [{ key: "service.name", value: { stringValue: "acme-shop" } }]);
  const [record] = resourceLogs.scopeLogs[0].logRecords;
  assert.equal(record.timeUnixNano, "1791644400123000000");
  assert.equal(record.severityText, "DEBUG");
  assert.deepEqual(record.attributes, [
    { key: "leadline.kind", value: { stringValue: "next.routes" } },
    { key: "leadline.routes.count", value: { intValue: "1" } },
  ]);
  assert.deepEqual(JSON.parse(record.body.stringValue), { version: 1, framework: "next", next: "16.4.0", root: "/repo/acme", routes });
});

test("the page formats console arguments the way the console would", () => {
  assert.equal(format(["plain", 1, { a: 1 }, null, undefined]), 'plain 1 {"a":1} null undefined');
  assert.equal(format(["%s is %d years, %o", "Ada", 36, { x: 1 }, "extra"]), 'Ada is 36 years, {"x":1} extra');
  assert.equal(format(["%cstyled%c text", "color: red", ""]), "styled text");
  assert.equal(format(["100%% sure, %s"]), "100% sure, %s");
  assert.equal(format([new TypeError("bad")]), "TypeError: bad");
  const loop = {};
  loop.self = loop;
  assert.equal(format([loop]), "[object Object]");
});

test("trace and span ids are random hex of the right length", () => {
  assert.match(hex(16), /^[0-9a-f]{32}$/);
  assert.match(hex(8), /^[0-9a-f]{16}$/);
  assert.notEqual(hex(16), hex(16));
});
