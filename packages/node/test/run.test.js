import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { Console } from "node:console";
import { Writable } from "node:stream";
import { test } from "node:test";

import { captureConsole } from "../dist/console.cjs";
import { MARK, markLines, runStreams, streamId } from "../dist/run.cjs";

/** A stream that keeps what it is given, and a console that prints to it. */
function memory() {
  const written = { text: "" };
  const stream = new Writable({
    write(chunk, _encoding, done) {
      written.text += chunk;
      done();
    },
  });
  return { stream, written, console: new Console({ stdout: stream, stderr: stream }) };
}

test("a stream is one `leadline run` reads only when its fstat identity was named", () => {
  const ids = { 1: "5:100", 2: "5:200" };
  const identify = (fd) => ids[fd];
  assert.deepEqual(runStreams({}, identify), { stdout: false, stderr: false });
  assert.deepEqual(runStreams({ LEADLINE_RUN_STDIO: "" }, identify), { stdout: false, stderr: false });
  assert.deepEqual(runStreams({ LEADLINE_RUN_STDIO: "5:100,5:200" }, identify), { stdout: true, stderr: true });
  // stdout sent elsewhere (`> out.json`): its identity is not the one named.
  assert.deepEqual(runStreams({ LEADLINE_RUN_STDIO: "5:999,5:200" }, identify), { stdout: false, stderr: true });
  // One terminal for both.
  assert.deepEqual(runStreams({ LEADLINE_RUN_STDIO: "5:100,5:100" }, () => "5:100"), { stdout: true, stderr: true });
  // fstat failed (a closed descriptor): not marked.
  assert.deepEqual(runStreams({ LEADLINE_RUN_STDIO: "5:100" }, () => undefined), { stdout: false, stderr: false });
});

test("streamId is two unsigned numbers, and the same for one file seen twice", () => {
  assert.match(streamId(1), /^\d+:\d+$/);
  assert.equal(streamId(1), streamId(1));
  assert.equal(streamId(987654), undefined);
});

test("markLines marks each line while it is on, and puts write back", () => {
  const { stream, written } = memory();
  const before = stream.write;
  const off = markLines(stream);
  stream.write("one\n");
  stream.write("two\nthree\n");
  stream.write(Buffer.from("bytes\n"));
  off();
  stream.write("four\n");
  assert.equal(written.text, `one${MARK}\ntwo${MARK}\nthree${MARK}\nbytes\nfour\n`);
  assert.equal(stream.write, before);
  assert.equal(Object.hasOwn(stream, "write"), false);
});

test("markLines leaves a write that was wrapped since alone", () => {
  const { stream } = memory();
  const off = markLines(stream);
  const theirs = () => true;
  stream.write = theirs;
  off();
  assert.equal(stream.write, theirs);
});

test("captured console lines are marked, and lines not captured are not", () => {
  const { stream, written, console: c } = memory();
  const records = [];
  let skip = false;
  const restore = captureConsole(
    (record) => {
      records.push(record.body);
      c.log("printed while emitting"); // not captured, so not marked
    },
    c,
    { skip: () => skip, around: () => markLines(stream) },
  );
  c.log("sent");
  c.error("also sent\nover two lines");
  skip = true;
  c.log("the exporter's own");
  restore();
  c.log("after");

  assert.deepEqual(records, ["sent", "also sent\nover two lines"]);
  assert.equal(
    written.text,
    `sent${MARK}\nprinted while emitting\n` +
      `also sent${MARK}\nover two lines${MARK}\nprinted while emitting\n` +
      `the exporter's own\nafter\n`,
  );
});

test("a stream that throws does not keep the mark on", () => {
  const { stream, console: c } = memory();
  const broken = () => {
    throw new Error("stream broke");
  };
  stream.write = broken;
  const restore = captureConsole(() => {}, c, { around: () => markLines(stream) });
  // The console swallows a throwing stream itself; what matters is that the
  // wrapper came off again.
  assert.doesNotThrow(() => c.log("x"));
  assert.equal(stream.write, broken);
  restore();
});

test("a process whose stdout goes elsewhere writes no mark", () => {
  // The identity named is not this child's pipe, as when `leadline run`
  // started a parent whose child redirects its output.
  const child = spawnSync(
    process.execPath,
    [
      "--input-type=module",
      "-e",
      `import { captureConsole } from ${JSON.stringify(new URL("../dist/console.cjs", import.meta.url).href)};
       import { markLines, runStreams } from ${JSON.stringify(new URL("../dist/run.cjs", import.meta.url).href)};
       const marked = runStreams();
       captureConsole(() => {}, console, { around: (s) => marked[s] ? markLines(process[s]) : undefined });
       console.log(JSON.stringify({ ok: true }));`,
    ],
    { env: { ...process.env, LEADLINE_RUN_STDIO: "1:2,3:4" }, encoding: "utf8" },
  );
  assert.equal(child.status, 0, child.stderr);
  assert.equal(child.stdout, '{"ok":true}\n');
});
