import assert from "node:assert/strict";
import test from "node:test";
import { BROWSER_KINDS, kindLabel, kindMark } from "./logkinds.ts";

test("a record with no kind has no mark", () => {
  assert.equal(kindMark(undefined), null);
  assert.equal(kindMark(""), null);
});

test("every browser kind has a mark and a name in the filter", () => {
  for (const k of BROWSER_KINDS) {
    const m = kindMark(k.kind);
    assert.equal(m?.browser, true, k.kind);
    assert.equal(m?.word, k.word);
    assert.match(m?.label ?? "", /^From the browser: /);
    assert.equal(kindLabel(k.kind), k.label);
  }
  assert.equal(kindMark("browser.web_vital")?.word, "web vital");
});

test("a kind this app has not heard of is shown in its own words", () => {
  assert.deepEqual(kindMark("browser.long_task"), { browser: true, word: "long task", label: "From the browser: long task" });
  assert.deepEqual(kindMark("job.retry"), { browser: false, word: "job.retry", label: "Kind: job.retry" });
  assert.equal(kindLabel("job.retry"), "job.retry");
  assert.equal(kindLabel(""), "Any kind");
});
