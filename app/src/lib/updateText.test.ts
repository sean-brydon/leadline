import assert from "node:assert/strict";
import { test } from "node:test";

import { updateOffer } from "./updateText.ts";

test("says nothing until an update is waiting", () => {
  assert.equal(updateOffer(null), null);
  assert.equal(updateOffer({ state: "idle" }), null);
});

test("offers the restart, and says what it costs", () => {
  const offer = updateOffer({ state: "ready", version: "0.3.0" });
  assert.equal(offer?.label, "Restart to update");
  assert.equal(offer?.busy, false);
  assert.match(offer?.tip ?? "", /Leadline 0\.3\.0 is downloaded/);
  assert.match(offer?.tip ?? "", /daemon/);
});

test("can't be pressed twice while it installs", () => {
  assert.equal(updateOffer({ state: "installing", version: "0.3.0" })?.busy, true);
});

test("a failure says why, and can be tried again", () => {
  const offer = updateOffer({ state: "failed", version: "0.3.0", reason: "Read-only file system (os error 30)" });
  assert.equal(offer?.busy, false);
  assert.match(offer?.tip ?? "", /Read-only file system/);
});
