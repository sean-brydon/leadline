import assert from "node:assert/strict";
import test from "node:test";
import { askKey, baseChoices, briefParams, briefPath, byteLength, countMarked, kb, markedLines, promptFault, recordedTexts, taskNameFault, tidyTaskName, type BriefAsk, type Project } from "./brief.ts";

const scope = { sinceMs: 1000, service: "acme-api" };
const SHAPE = "SELECT id, product_id, quantity FROM order_items WHERE order_id = ?";

test("each kind of problem is asked for at its own route, with its own name for its service", () => {
  const query: BriefAsk = { kind: "query", db_system: "postgresql", fingerprint: SHAPE };
  assert.equal(briefPath(query), "/api/brief/query");
  assert.deepEqual(briefParams(query, scope, false), { since_ms: 1000, example: undefined, service: "acme-api", db_system: "postgresql", fingerprint: SHAPE });
  assert.deepEqual(briefParams({ kind: "endpoint", service: "acme-api", name: "GET /api/orders" }, { sinceMs: 5, service: "" }, true), {
    since_ms: 5, example: 1, service: "", endpoint_service: "acme-api", name: "GET /api/orders",
  });
  // A trace is not in a window: it is asked for by its id alone.
  assert.deepEqual(briefParams({ kind: "trace", id: "4bf9" }, scope, true), { id: "4bf9", example: 1 });
  assert.deepEqual(briefParams({ kind: "log", cursor: "1760180531000000000.1", service: "acme-payments" }, scope, false), {
    since_ms: 1000, example: undefined, cursor: "1760180531000000000.1", log_service: "acme-payments",
  });
  assert.equal(briefPath({ kind: "log", cursor: "1.1", service: "" }), "/api/brief/log");
  assert.notEqual(askKey(query), askKey({ ...query, db_system: "mysql" }));
  assert.notEqual(askKey({ kind: "trace", id: "a" }), askKey({ kind: "trace", id: "b" }));
});

const PROMPT = ["Fix the N+1 on order_items", "", "The query:", "", "```sql", SHAPE, "```", "", "- acme-api error: `checkout failed`", "- acme-api error: `charge failed`", "", "## Done when"].join("\n");
const MARKS = [
  { from: 6, to: 6, kind: "shape", values: false },
  { from: 9, to: 10, kind: "log", values: true },
];

test("the lines the daemon marked are the ones shown as recorded", () => {
  const recorded = recordedTexts(PROMPT, MARKS);
  assert.deepEqual([...recorded], [SHAPE, "- acme-api error: `checkout failed`", "- acme-api error: `charge failed`"]);
  assert.deepEqual(markedLines(PROMPT, recorded), [false, false, false, false, false, true, false, false, true, true, false, false]);
  assert.equal(countMarked(PROMPT, recorded), 3);
  // A mark that runs past the end, or lands on a blank line, marks nothing.
  assert.deepEqual([...recordedTexts("a\n\nb", [{ from: 2, to: 9, kind: "log", values: true }])], ["b"]);
  assert.deepEqual(markedLines("", new Set()), [false]);
});

test("a recorded line stays marked as the prompt is edited around it, and loses its mark when it is itself changed", () => {
  const recorded = recordedTexts(PROMPT, MARKS);
  // Lines added above move it down; it is still marked.
  const moved = `Please be careful.\nRead the code first.\n${PROMPT}`;
  assert.deepEqual(markedLines(moved, recorded).indexOf(true), 7);
  assert.equal(countMarked(moved, recorded), 3);
  // Removing a recorded line removes its mark and no other.
  const cut = PROMPT.replace("- acme-api error: `charge failed`\n", "");
  assert.equal(countMarked(cut, recorded), 2);
  // Changing it makes it the person's own words.
  const edited = PROMPT.replace("order_id = ?", "order_id IN (?)");
  assert.equal(countMarked(edited, recorded), 2);
  assert.equal(markedLines(edited, recorded)[5], false);
  // Pasting it somewhere else marks it there too: it is the same data.
  assert.equal(countMarked(`${PROMPT}\n${SHAPE}`, recorded), 4);
  // A blank line is never data.
  assert.equal(markedLines("\n\n", recorded).some(Boolean), false);
});

const acme: Project = {
  name: "acme",
  repo: true,
  default_branch: "main",
  worktrees: [
    { name: "main", branch: "main", main: true },
    { name: "fix-orders", branch: "fix-orders" },
    { name: "checkout-retry", branch: "feature/checkout-retry" },
    { name: "detached" },
  ],
};

test("a task's name is one Shipyard can make a branch and a folder of", () => {
  for (const ok of ["fix-n1-order-items-k3f9", "a", "fix2", "x-1-y"]) assert.equal(taskNameFault(ok), null, ok);
  assert.equal(taskNameFault(""), "A task needs a name.");
  for (const bad of ["Fix-Orders", "fix orders", "fix_orders", "-fix", "fix-", "fix--orders", "1fix", "fix/orders", "--prompt=x", "fix.orders", "ünï"]) {
    assert.equal(taskNameFault(bad), "Use lower-case letters, digits and dashes, starting with a letter.", bad);
  }
  assert.equal(taskNameFault("art-orders"), "A name can't start with art-: Shipyard keeps those.");
  assert.equal(taskNameFault(`fix-${"a".repeat(60)}`), "That is longer than a name can be (63 characters).");
  // A worktree, or a branch one has, of that name is already there.
  assert.equal(taskNameFault("fix-orders", acme), "acme already has a worktree named fix-orders.");
  assert.equal(taskNameFault("main", acme), "acme already has a worktree named main.");
  assert.equal(taskNameFault("fix-orders-2", acme), null);
});

test("what is typed for a name is tidied into one as it is typed", () => {
  assert.equal(tidyTaskName("Fix Orders N+1"), "fix-orders-n-1");
  assert.equal(tidyTaskName("fix-"), "fix-"); // mid-word: the next letter makes it whole
  assert.equal(tidyTaskName("--prompt=$(rm -rf ~)"), "prompt-rm-rf-");
  assert.equal(tidyTaskName("123abc"), "abc");
  assert.equal(tidyTaskName("a".repeat(80)).length, 63);
  assert.equal(taskNameFault(tidyTaskName("Fix the Orders page!").replace(/-$/, "")), null);
});

test("a task starts from what the project has checked out, or from a worktree's branch", () => {
  assert.deepEqual(baseChoices(acme), [
    { value: "", label: "main, as the project has it", hint: "The project's own checkout" },
    { value: "fix-orders", label: "fix-orders", hint: "A worktree's branch" },
    { value: "feature/checkout-retry", label: "feature/checkout-retry", hint: "The branch of the worktree checkout-retry" },
  ]);
  // A base the daemon proposed is always there to pick.
  assert.equal(baseChoices(acme, "release-2").at(-1)?.value, "release-2");
  assert.equal(baseChoices(acme, "fix-orders").length, 3);
  // No project picked yet, or one whose branch is not known.
  assert.deepEqual(baseChoices(undefined), [{ value: "", label: "What the project has checked out", hint: "The project's own checkout" }]);
  assert.equal(baseChoices({ name: "x", repo: true, worktrees: [] })[0].label, "What the project has checked out");
});

test("sizes and what can't be sent", () => {
  assert.equal(kb(412), "412 bytes");
  assert.equal(kb(3072), "3.0 KB");
  assert.equal(byteLength("é✓🚢"), 2 + 3 + 4);
  assert.equal(promptFault("Fix it."), null);
  assert.equal(promptFault(" \n\t"), "There is nothing to send.");
  assert.equal(promptFault("a".repeat(64 * 1024)), null);
  assert.equal(promptFault("a".repeat(64 * 1024 + 1)), "The prompt is 65 KB, and 64 KB is the most that can be sent.");
});
