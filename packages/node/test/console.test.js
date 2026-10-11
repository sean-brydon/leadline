import assert from "node:assert/strict";
import { Console } from "node:console";
import { Writable } from "node:stream";
import { test } from "node:test";

import { captureConsole } from "../dist/console.cjs";

/** A console that writes into strings, like the real one writes to the terminal. */
function memoryConsole() {
  const written = { stdout: "", stderr: "" };
  const sink = (key) =>
    new Writable({
      write(chunk, _encoding, done) {
        written[key] += chunk;
        done();
      },
    });
  return { console: new Console({ stdout: sink("stdout"), stderr: sink("stderr") }), written };
}

const calls = (c) => {
  c.debug("d %d", 1);
  c.log("hello", { a: 1 }, [1, 2]);
  c.info("i %s", "x");
  c.warn("w");
  c.error("e", new TypeError("bad"));
};

test("the console prints exactly what it printed before", () => {
  const plain = memoryConsole();
  calls(plain.console);

  const captured = memoryConsole();
  const restore = captureConsole(() => {}, captured.console);
  calls(captured.console);
  restore();

  assert.equal(captured.written.stdout, plain.written.stdout);
  // The stack's line and column differ between the two calls() runs; the
  // text up to it does not.
  assert.equal(captured.written.stderr.split("\n")[0], plain.written.stderr.split("\n")[0]);
  assert.match(captured.written.stdout, /^d 1\nhello { a: 1 } \[ 1, 2 \]\ni x\n$/);
  assert.match(captured.written.stderr, /^w\ne TypeError: bad\n/);
});

test("each method maps to its severity, stream and printed text", () => {
  const { console: c } = memoryConsole();
  const records = [];
  const restore = captureConsole((record) => records.push(record), c);
  calls(c);
  restore();

  assert.deepEqual(
    records.map((r) => [r.method, r.severityNumber, r.severityText, r.stream]),
    [
      ["debug", 5, "DEBUG", "stdout"],
      ["log", 9, "INFO", "stdout"],
      ["info", 9, "INFO", "stdout"],
      ["warn", 13, "WARN", "stderr"],
      ["error", 17, "ERROR", "stderr"],
    ],
  );
  assert.equal(records[0].body, "d 1");
  assert.equal(records[1].body, "hello { a: 1 } [ 1, 2 ]");
  assert.equal(records[2].body, "i x");
  assert.match(records[4].body, /^e TypeError: bad\n\s+at /);
  assert.equal(records[4].error?.name, "TypeError");
  assert.equal(records[1].error, undefined);
});

test("a console call made while emitting is printed but not captured", () => {
  const { console: c, written } = memoryConsole();
  const records = [];
  const restore = captureConsole((record) => {
    records.push(record);
    c.error("exporter says: could not send"); // what a chatty exporter would do
  }, c);
  c.log("one");
  c.log("two");
  restore();

  assert.deepEqual(records.map((r) => r.body), ["one", "two"]);
  assert.equal(written.stdout, "one\ntwo\n");
  assert.equal(written.stderr, "exporter says: could not send\n".repeat(2));
});

test("a failing emit never reaches the caller", () => {
  const { console: c, written } = memoryConsole();
  const restore = captureConsole(() => {
    throw new Error("exporter broke");
  }, c);
  assert.doesNotThrow(() => c.log("still printed"));
  restore();
  assert.equal(written.stdout, "still printed\n");
});

test("restore puts the original methods back, unless something wrapped them since", () => {
  const { console: c } = memoryConsole();
  const original = c.log;
  const restore = captureConsole(() => {}, c);
  assert.notEqual(c.log, original);
  assert.equal(c.log.name, "log");
  restore();
  assert.equal(c.log, original);

  const restoreAgain = captureConsole(() => {}, c);
  const theirs = () => {};
  c.log = theirs;
  restoreAgain();
  assert.equal(c.log, theirs);
});

test("a very long line is cut before it is sent, not before it is printed", () => {
  const { console: c, written } = memoryConsole();
  const records = [];
  const restore = captureConsole((record) => records.push(record), c);
  c.log("x".repeat(200_000));
  restore();
  assert.equal(written.stdout.length, 200_001);
  assert.ok(records[0].body.length < 70_000);
  assert.match(records[0].body, /\(200000 characters\)$/);
});
