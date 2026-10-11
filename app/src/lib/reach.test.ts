import assert from "node:assert/strict";
import test from "node:test";
import { answerLine, compared, holdsLine, homed, INSTALL_LINE, noAnswer, ownAddress, probeOf, seenLine, sourcesLine, sshLine, versionNote } from "./reach.ts";

test("a Leadline says its name and a version; anything else is something else", () => {
  assert.deepEqual(probeOf({ ok: true, name: "leadline", version: "0.2.0" }), { state: "leadline", version: "0.2.0" });
  assert.deepEqual(probeOf({ ok: true, name: "leadline", version: "dev" }), { state: "leadline", version: "dev" });
  for (const other of [null, "ok", 3, [], {}, { ok: true }, { name: "leadline" }, { name: "leadline", version: "" }, { name: "leadline", version: 2 }, { name: "grafana", version: "11.0.0" }]) {
    assert.deepEqual(probeOf(other), { state: "other" }, JSON.stringify(other));
  }
});

test("versions are ordered when they are numbers, and not guessed at when they aren't", () => {
  assert.equal(compared("0.2.0", "0.2.0"), 0);
  assert.equal(compared("0.1.9", "0.2.0"), -1);
  assert.equal(compared("0.10.0", "0.9.3"), 1);
  assert.equal(compared("v1.0.0", "1.0.0"), 0);
  assert.equal(compared("1.0.1-rc1", "1.0.0"), 1);
  assert.equal(compared("dev", "0.2.0"), null);
  assert.equal(compared("dev", "dev"), 0);
});

test("a different version is said, never refused", () => {
  assert.equal(versionNote("0.2.0", "0.2.0"), "");
  assert.equal(versionNote("0.2.0", undefined), "");
  assert.equal(versionNote("0.1.4", "0.2.0"), "Older than this Mac's 0.2.0: a screen may show less there.");
  assert.equal(versionNote("0.3.0", "0.2.0"), "Newer than this Mac's 0.2.0: update Leadline here to see everything it has.");
  assert.equal(versionNote("dev", "0.2.0"), "Not the version on this Mac (0.2.0).");
  assert.equal(versionNote("0.2.0", "dev"), "Not the version on this Mac (dev).");
});

test("what a machine's row says of its Leadline", () => {
  assert.equal(answerLine(undefined), "Checking…");
  assert.equal(answerLine({ state: "leadline", version: "0.2.0" }), "Leadline 0.2.0");
  assert.equal(answerLine({ state: "other" }), "Not a Leadline");
  assert.equal(answerLine({ state: "none" }), "No answer");
  assert.equal(noAnswer({ state: "none" }, "hetzner-1", "14321"), "Nothing answers from hetzner-1 on port 14321.");
  assert.equal(noAnswer({ state: "other" }, "hetzner-1", "14321"), "Something answers on port 14321, and it isn't Leadline.");
});

test("what it holds, how many sources, and when it last answered", () => {
  const summary = { spans: 20_480, logs: 2_610, traces: 3_020, services: ["web", "api", "worker"] };
  assert.equal(holdsLine({ summary, storage: { size_bytes: 8_600_000, retain_ms: 259_200_000 } }), "8.2 MB · 20k spans, 2.6k logs");
  assert.equal(holdsLine({ summary, storage: { size_bytes: 0, retain_ms: 0 } }), "In memory · 20k spans, 2.6k logs");
  // A daemon too old to say what it stores still says what it holds.
  assert.equal(holdsLine({ summary, storage: null }), "20k spans, 2.6k logs");
  assert.equal(holdsLine({ summary: { ...summary, spans: 1, logs: 1 } }), "1 span, 1 log");
  assert.equal(holdsLine({}), "");
  assert.equal(sourcesLine({ summary }), "3 sources");
  assert.equal(sourcesLine({ summary: { ...summary, services: ["web"] } }), "1 source");
  assert.equal(sourcesLine({ summary: { ...summary, services: [] } }), "Nothing has reached it yet");
  assert.equal(sourcesLine({}), "");
  const now = 1_791_650_000_000;
  assert.equal(seenLine(true, now - 5_000_000, now), "now");
  assert.equal(seenLine(false, now - 2 * 3_600_000, now), "2 h ago");
  assert.equal(seenLine(false, undefined, now), "never");
});

test("the commands to copy", () => {
  assert.equal(INSTALL_LINE, "curl -fsSL https://github.com/sean-brydon/leadline/releases/latest/download/install.sh | sh");
  assert.equal(sshLine(), "ssh -N -L 4336:127.0.0.1:4320 user@host");
  assert.equal(sshLine(4400), "ssh -N -L 4400:127.0.0.1:4320 user@host");
});

test("a daemon's own address is what it says it listens at, not the port it is reached through", () => {
  assert.equal(ownAddress(["http://127.0.0.1:4320", "http://127.0.0.1:4318"], "http://127.0.0.1:14321"), "http://127.0.0.1:4320");
  // This computer's own, and one that hasn't said or names none: the address it is reached at.
  assert.equal(ownAddress(["http://127.0.0.1:4320"], "http://127.0.0.1:4320"), "http://127.0.0.1:4320");
  assert.equal(ownAddress(undefined, "http://127.0.0.1:14321"), "http://127.0.0.1:14321");
  assert.equal(ownAddress([], "http://127.0.0.1:14321"), "http://127.0.0.1:14321");
  assert.equal(ownAddress(["not an address"], "http://127.0.0.1:14321"), "http://127.0.0.1:14321");
});

test("text an older daemon wrote through a forward is put back to its own address", () => {
  const through = "http://127.0.0.1:14321";
  const own = "http://127.0.0.1:4320";
  assert.equal(homed(`Fetch ${through}/setup.md and follow it. Leadline is at ${through}.`, through, own), `Fetch ${own}/setup.md and follow it. Leadline is at ${own}.`);
  assert.equal(homed(`leadline run --url ${through} -- pnpm dev`, through, own), `leadline run --url ${own} -- pnpm dev`);
  assert.equal(homed(`LEADLINE_URL=${through}`, through, own), `LEADLINE_URL=${own}`);
  // A current daemon names its own address already: nothing to change.
  const right = `Fetch ${own}/setup.md and follow it.`;
  assert.equal(homed(right, through, own), right);
  // Reached where it listens: nothing to change either, whatever the text says.
  assert.equal(homed(`at ${own}`, own, own), `at ${own}`);
  // Another port that merely starts the same is not it.
  assert.equal(homed("see http://127.0.0.1:143210/x", through, own), "see http://127.0.0.1:143210/x");
  assert.equal(homed("", through, own), "");
});
