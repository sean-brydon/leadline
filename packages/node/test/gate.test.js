import assert from "node:assert/strict";
import { test } from "node:test";

import { Tail } from "../dist/exit.cjs";
import { Gate, gated } from "../dist/gate.cjs";

const SUCCESS = 0;
const FAILED = 1;

/** An exporter that records what it was asked to send and answers with `code`. */
function fakeExporter(code = SUCCESS) {
  return {
    sent: [],
    code,
    export(items, done) {
      this.sent.push(items);
      done({ code: this.code, error: this.code === SUCCESS ? undefined : new Error("ECONNREFUSED") });
    },
    shutdown() {
      return Promise.resolve("shut down by " + (this === exporter ? "itself" : "someone else"));
    },
  };
}
let exporter;

const exportOnce = (wrapped, items) => new Promise((resolve) => wrapped.export(items, resolve));

test("with the daemon down, batches are dropped quietly and the exporter is never called", async () => {
  let probes = 0;
  const gate = new Gate("127.0.0.1", 4320, undefined, async () => (probes++, false));
  exporter = fakeExporter();
  const wrapped = gated(exporter, gate);

  for (let i = 0; i < 5; i++) {
    assert.deepEqual(await exportOnce(wrapped, [i]), { code: SUCCESS });
  }
  assert.deepEqual(exporter.sent, []);
  assert.equal(gate.state, "down");
  // One look, not one per batch: the answer is reused for a while.
  assert.equal(probes, 1);
});

test("with the daemon up, batches go through", async () => {
  let probes = 0;
  const gate = new Gate("127.0.0.1", 4320, undefined, async () => (probes++, true));
  exporter = fakeExporter();
  const wrapped = gated(exporter, gate);

  await exportOnce(wrapped, ["a"]);
  await exportOnce(wrapped, ["b"]);
  assert.deepEqual(exporter.sent, [["a"], ["b"]]);
  assert.equal(gate.state, "up");
  assert.equal(probes, 1);
});

test("a failed export is not reported to the SDK, and marks the daemon down", async () => {
  const gate = new Gate("127.0.0.1", 4320, undefined, async () => true);
  exporter = fakeExporter(FAILED);
  const messages = [];
  const wrapped = gated(exporter, gate, (m) => messages.push(m));

  assert.deepEqual(await exportOnce(wrapped, ["a"]), { code: SUCCESS });
  assert.equal(gate.state, "down");
  assert.deepEqual(messages, ["export failed: ECONNREFUSED"]);
  // Down now: the next batch does not reach the exporter.
  await exportOnce(wrapped, ["b"]);
  assert.deepEqual(exporter.sent, [["a"]]);
});

test("a refusal over HTTP is a failure of that export, not of the daemon", async () => {
  const gate = new Gate("127.0.0.1", 4320, undefined, async () => true);
  // What the OTLP exporter reports when /v1/metrics answers 404.
  const notFound = {
    sent: 0,
    export(_items, done) {
      this.sent++;
      done({ code: FAILED, error: Object.assign(new Error("Not Found"), { code: 404 }) });
    },
  };
  const messages = [];
  const metrics = gated(notFound, gate, (m) => messages.push(m));
  exporter = fakeExporter();
  const traces = gated(exporter, gate);

  assert.deepEqual(await exportOnce(metrics, ["m"]), { code: SUCCESS });
  assert.equal(gate.state, "up");
  assert.deepEqual(messages, ["export failed: Not Found"]);
  // Spans sent right after still go out.
  await exportOnce(traces, ["span"]);
  assert.deepEqual(exporter.sent, [["span"]]);
});

test("an exporter that throws is contained", async () => {
  const gate = new Gate("127.0.0.1", 4320, undefined, async () => true);
  const wrapped = gated({ export() { throw new Error("broken"); } }, gate);
  assert.deepEqual(await exportOnce(wrapped, ["a"]), { code: SUCCESS });
  assert.equal(gate.state, "down");
});

test("a daemon that starts later is found once the retry time has passed", async (t) => {
  t.mock.timers.enable({ apis: ["Date"] });
  let up = false;
  let probes = 0;
  const gate = new Gate("127.0.0.1", 4320, undefined, async () => (probes++, up));
  exporter = fakeExporter();
  const wrapped = gated(exporter, gate);

  await exportOnce(wrapped, ["lost"]);
  up = true; // the developer starts Leadline
  t.mock.timers.tick(500);
  await exportOnce(wrapped, ["still lost"]); // too soon to look again
  assert.equal(probes, 1);
  t.mock.timers.tick(2000);
  await exportOnce(wrapped, ["arrives"]);
  assert.equal(probes, 2);
  assert.deepEqual(exporter.sent, [["arrives"]]);
});

test("the gate's messages name each change once", async () => {
  const messages = [];
  const gate = new Gate("127.0.0.1", 4320, (m) => messages.push(m), async () => false);
  await gate.ready();
  gate.sent(false);
  gate.sent(true);
  gate.sent(true);
  assert.deepEqual(messages, ["daemon at 127.0.0.1:4320 is down", "daemon at 127.0.0.1:4320 is up"]);
});

test("the exporter's other methods pass through with the right this", async () => {
  const gate = new Gate("127.0.0.1", 4320, undefined, async () => true);
  exporter = fakeExporter();
  const wrapped = gated(exporter, gate);
  assert.equal(await wrapped.shutdown(), "shut down by itself");
  assert.equal(wrapped.code, SUCCESS);
});

test("what was handed to the exporter is reported, sent or dropped", async () => {
  const tail = new Tail();
  const gate = new Gate("127.0.0.1", 4320, undefined, async () => false);
  const wrapped = gated(fakeExporter(), gate, undefined, (items) => tail.forget(items));
  const [a, b, c] = [{ id: "a" }, { id: "b" }, { id: "c" }];
  tail.add(a);
  tail.add(b);
  tail.add(c);
  await exportOnce(wrapped, [a, b]);
  assert.deepEqual(tail.take(), [c]);
  assert.deepEqual(tail.take(), []);
});

test("the tail forgets its oldest entries past its cap", () => {
  const tail = new Tail();
  for (let i = 0; i < 5000; i++) tail.add(i);
  const kept = tail.take();
  assert.equal(kept.length, 4096);
  assert.equal(kept[0], 5000 - 4096);
  assert.equal(kept.at(-1), 4999);
});
