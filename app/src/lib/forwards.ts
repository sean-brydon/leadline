// Reaching a Shipyard box's Leadline, as the app does it: have this
// computer's daemon open (or find) the forward, then ask through it whether
// a Leadline answers. Everything here that runs Shipyard's command line goes
// through this computer's own daemon (lib/api.ts, forwardOpen and
// forwardClose), and is called because a person added, used or removed a
// machine: nothing in this file runs when the window opens.

import { api, ApiError, type Fault, type OpenedForward } from "./api";
import { closing, machines, portOf, reach, type Machine } from "./machines";
import type { Probe } from "./reach.ts";

/** faultOf turns a failed request to this computer's daemon into a sentence for the person. */
export function faultOf(e: unknown): Fault {
  if (e instanceof ApiError) {
    // A daemon without the route answers 404 in plain text.
    if (!e.code && (e.status === 404 || e.status === 405)) return { code: "old", message: "This Mac's Leadline is too old to reach a Shipyard box.", hint: "Update Leadline on this Mac, then try again." };
    return { code: e.code, message: e.message, hint: e.hint || undefined };
  }
  return { code: "unreachable", message: "This Mac's Leadline isn't answering, so Shipyard can't be asked.", hint: "A machine you forwarded yourself can still be added by its address." };
}

const pause = (ms: number) => new Promise((done) => setTimeout(done, ms));

/**
 * answers asks an address whether a Leadline is there. A forward that was
 * opened a moment ago carries its first connection a little later than it
 * says it is open, so "nothing" is asked again a few times before it is
 * believed; anything that does answer is believed at once.
 */
export async function answers(url: string, tries = 3): Promise<Probe> {
  let probe: Probe = { state: "none" };
  for (let i = 0; i < tries; i++) {
    if (i > 0) await pause(600);
    probe = await api.probe(url);
    if (probe.state !== "none") break;
  }
  return probe;
}

/** What trying to reach a box came to. */
export type Reached =
  | { state: "leadline"; forward: OpenedForward; version: string }
  /** The forward is open and no Leadline answered through it. `closed`: Leadline had opened it for this try, and closed it again. */
  | { state: "silent"; forward: OpenedForward; probe: Probe; closed: boolean }
  | { state: "failed"; fault: Fault };

/**
 * reachBox opens the way to a box's Leadline and finds out whether one is
 * there. `prefer` is the local port the box had before. With `tidy`, a
 * forward this call opened is closed again when nothing answered through
 * it: that is for a box that is not on the list, where an open forward to
 * nothing would be left behind. One that was there already is never closed.
 */
export async function reachBox(box: string, prefer: number | undefined, tidy: boolean): Promise<Reached> {
  let forward: OpenedForward;
  try {
    forward = await api.forwardOpen(box, prefer);
  } catch (e) {
    return { state: "failed", fault: faultOf(e) };
  }
  const probe = await answers(forward.url, forward.opened ? 4 : 2);
  if (probe.state === "leadline") return { state: "leadline", forward, version: probe.version };
  let closed = false;
  if (tidy && forward.opened) {
    closed = await api.forwardClose(forward.id).then(
      (c) => c.ok,
      () => false,
    );
  }
  return { state: "silent", forward, probe, closed };
}

// Reconnecting a machine that is on the list. Each machine has one link
// state, which the window's "no answer" screen and the Machines view both
// show, so a try started in one is seen in the other.

export type Link =
  | { state: "idle" }
  | { state: "trying" }
  /** The forward is there and nothing answered through it. */
  | { state: "silent"; probe: Probe }
  | { state: "failed"; fault: Fault };

const IDLE: Link = { state: "idle" };
const links = new Map<string, Link>();
const linkListeners = new Set<() => void>();

function setLink(id: string, link: Link) {
  links.set(id, link);
  for (const l of linkListeners) l();
}

export const link = {
  of: (id: string): Link => links.get(id) ?? IDLE,
  subscribe(fn: () => void): () => void {
    linkListeners.add(fn);
    return () => linkListeners.delete(fn);
  },
};

/**
 * reconnect has a box's forward opened again, on the port it had when that
 * is still free, and moves the machine to it. A forward that is still
 * there is found and used, so this is safe to ask whenever a box does not
 * answer. One try at a time for a machine; the answer is its link state.
 */
export async function reconnect(m: Machine): Promise<Link> {
  if (reach(m) !== "shipyard" || !m.box) return IDLE;
  if (link.of(m.id).state === "trying") return link.of(m.id);
  setLink(m.id, { state: "trying" });
  const got = await reachBox(m.box, Number(portOf(m)) || undefined, false);
  // Removed while the answer was out: nothing to move, and its forward was closed with it or is closed now.
  if (!machines.get().machines.some((x) => x.id === m.id)) {
    if (got.state !== "failed" && got.forward.opened) void api.forwardClose(got.forward.id).catch(() => {});
    links.delete(m.id);
    return IDLE;
  }
  let next: Link;
  if (got.state === "failed") next = { state: "failed", fault: got.fault };
  else {
    machines.addBox(m.box, got.forward);
    if (got.state === "leadline") {
      machines.seen(m.id, got.version);
      next = IDLE;
    } else next = { state: "silent", probe: got.probe };
  }
  setLink(m.id, next);
  return next;
}

/**
 * forget takes a machine off the list, closing the forward Leadline opened
 * to it first. When that forward can't be closed the machine stays and the
 * reason comes back, so the person can choose to remove it all the same
 * (`anyway`), which leaves the forward for them to close.
 */
export async function forget(m: Machine, anyway = false): Promise<Fault | null> {
  const id = closing(m);
  if (id && !anyway) {
    try {
      await api.forwardClose(id);
    } catch (e) {
      return faultOf(e);
    }
  }
  links.delete(m.id);
  machines.remove(m.id);
  return null;
}
