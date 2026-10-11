// The machines whose Leadline this window can show. "This Mac" is always
// first, at the address the app was built for. Others are added beside it
// in the Machines view: one of the owner's Shipyard boxes, reached through
// a port Shipyard forwards to this computer, or a machine whose port a
// person forwarded themselves. Nothing here reaches another host: a
// machine's address is always a loopback one, which is also all the
// daemon's guard and the desktop app's connect-src allow.
//
// This file is the list and nothing else, kept apart from React so it can be
// tested; lib/api.ts and lib/live.ts read the current machine from it, and
// lib/forwards.ts is what opens and closes a box's forward.

/** A Shipyard forward a machine is reached through, as far as the list needs to remember it. */
export type MachineForward = {
  /** Shipyard's id for it, which closes it. */
  id: string;
  /** Leadline opened it, so Leadline closes it when the machine is removed. False for one that was there already. */
  ours: boolean;
};

export type Machine = {
  id: string;
  /** What the person calls it. */
  name: string;
  /** The daemon's address as this computer reaches it, as in http://127.0.0.1:4320. */
  url: string;
  /** How it is reached, for one that isn't this computer. Absent on this Mac, and on a machine saved before there were boxes: that one is an address. */
  via?: "shipyard" | "address";
  /** For a Shipyard box: its name there. */
  box?: string;
  /** For a Shipyard box: the forward its address is the local end of. */
  forward?: MachineForward;
  /** When its Leadline last answered this window, and the version it said. */
  seenMs?: number;
  version?: string;
};

export type Machines = {
  machines: Machine[];
  /** The id of the machine the window is showing. */
  current: string;
};

/** The machine this app runs on. It is always first, and can't be removed. */
export const LOCAL_ID = "local";
const LOCAL_NAME = "This Mac";

/**
 * loopbackUrl reads a daemon's address and returns it in one spelling, or
 * null when it isn't an http address on this machine. "127.0.0.1:4336" is
 * enough.
 */
export function loopbackUrl(input: string): string | null {
  const text = input.trim();
  if (!text) return null;
  let u: URL;
  try {
    u = new URL(/^[a-z][a-z0-9+.-]*:\/\//i.test(text) ? text : `http://${text}`);
  } catch {
    return null;
  }
  if (u.protocol !== "http:" || u.username || u.password) return null;
  if (u.hostname !== "127.0.0.1" && u.hostname !== "localhost") return null;
  return `http://${u.hostname}${u.port ? `:${u.port}` : ""}`;
}

/** Why an address is refused, for the person who typed it. */
export const NOT_LOOPBACK =
  "A machine's address must be on this computer, such as http://127.0.0.1:4336. Leadline only ever talks to localhost, so forward the other machine's port here first.";

// What Shipyard calls a box, and a forward: the same shapes the daemon takes.
const BOX_NAME = /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/;
const FORWARD_ID = /^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$/;

const boxId = (box: string) => `box:${box}`;
const bare = (url: string) => url.replace("http://", "");

/** first is the list with nothing added: this Mac alone. */
export function first(localUrl: string): Machines {
  return { machines: [{ id: LOCAL_ID, name: LOCAL_NAME, url: localUrl }], current: LOCAL_ID };
}

/**
 * added puts a machine on the list by the address a person forwarded, or
 * renames the one already at that address. It throws, with a sentence for
 * the person, when the address is not one this window may reach.
 */
export function added(state: Machines, name: string, address: string): Machines {
  const url = loopbackUrl(address);
  if (!url) throw new Error(NOT_LOOPBACK);
  const label = name.trim() || bare(url);
  const at = state.machines.find((m) => m.url === url);
  if (at) {
    // This Mac keeps its name.
    if (at.id === LOCAL_ID) return state;
    return { ...state, machines: state.machines.map((m) => (m === at ? { ...m, name: label } : m)) };
  }
  return { ...state, machines: [...state.machines, { id: `m:${bare(url)}`, name: label, url, via: "address" }] };
}

/** What the daemon said of a forward it opened or found (lib/api.ts, OpenedForward), as far as the list keeps it. */
export type Opened = { id: string; url: string; opened: boolean };

/**
 * addedBox puts a Shipyard box on the list, reached through a forward, or
 * moves the one already there to the forward it has now. `name` is what to
 * call a new one (the box's own name when empty); one already on the list
 * keeps the name the person gave it.
 *
 * Whether the forward is Leadline's to close is kept across calls: it is
 * when this call opened it, and stays so while the same forward is found
 * again. A different forward that was already there is someone else's.
 */
export function addedBox(state: Machines, box: string, forward: Opened, name = ""): Machines {
  const url = loopbackUrl(forward.url);
  if (!url || !BOX_NAME.test(box) || !FORWARD_ID.test(forward.id)) throw new Error("That is not a forward to a Shipyard box.");
  const id = boxId(box);
  const was = state.machines.find((m) => m.id === id);
  const ours = forward.opened || (was?.forward?.id === forward.id && was.forward.ours);
  const next: Machine = { ...was, id, name: was?.name ?? (name.trim() || box), url, via: "shipyard", box, forward: { id: forward.id, ours } };
  // An address someone added by hand that is this forward's local end is the same daemon: one row, the box's.
  const rest = state.machines.filter((m) => m.id === LOCAL_ID || (m.id !== id && m.url !== url));
  const at = was ? Math.min(state.machines.indexOf(was), rest.length) : rest.length;
  const machines = [...rest.slice(0, at), next, ...rest.slice(at)];
  return { machines, current: machines.some((m) => m.id === state.current) ? state.current : LOCAL_ID };
}

/** renamed gives a machine another name. An empty one goes back to what it is: the box's name, or the address. This Mac keeps its name. */
export function renamed(state: Machines, id: string, name: string): Machines {
  if (id === LOCAL_ID) return state;
  const at = state.machines.find((m) => m.id === id);
  if (!at) return state;
  const label = name.trim() || at.box || bare(at.url);
  return label === at.name ? state : { ...state, machines: state.machines.map((m) => (m === at ? { ...m, name: label } : m)) };
}

/** seen notes that a machine's Leadline answered, and the version it said. */
export function seen(state: Machines, id: string, atMs: number, version: string): Machines {
  const at = state.machines.find((m) => m.id === id);
  if (!at || (at.seenMs === atMs && at.version === version)) return state;
  return { ...state, machines: state.machines.map((m) => (m === at ? { ...m, seenMs: atMs, version } : m)) };
}

/** removed takes a machine off the list. This Mac stays; the window goes back to it if it was showing the one removed. */
export function removed(state: Machines, id: string): Machines {
  if (id === LOCAL_ID) return state;
  const machines = state.machines.filter((m) => m.id !== id);
  return { machines, current: state.current === id ? LOCAL_ID : state.current };
}

/** chosen makes a machine the one shown; an id that isn't on the list changes nothing. */
export function chosen(state: Machines, id: string): Machines {
  return state.machines.some((m) => m.id === id) && state.current !== id ? { ...state, current: id } : state;
}

/** current is the machine being shown. */
export function current(state: Machines): Machine {
  return state.machines.find((m) => m.id === state.current) ?? state.machines[0];
}

// How a machine is reached, and what to call it in a sentence.

/** reach is how this computer gets to a machine's Leadline. */
export function reach(m: Machine): "local" | "shipyard" | "address" {
  if (m.id === LOCAL_ID) return "local";
  return m.via === "shipyard" && m.box ? "shipyard" : "address";
}

/** portOf is the port of a machine's address on this computer: 14321 of http://127.0.0.1:14321. */
export function portOf(m: Pick<Machine, "url">): string {
  return m.url.split(":").pop() ?? "";
}

/**
 * hereOf names a machine inside a sentence: "this Mac" for the computer the
 * app runs on, and for any other the name the person gave it, as in
 * "Everything stays on hetzner-1". Wherever the app speaks of the daemon it
 * is showing, this is the word; "this Mac" stays only for what really is
 * this computer (where Shipyard is, where a task is sent from).
 */
export function hereOf(m: Machine): string {
  return m.id === LOCAL_ID ? "this Mac" : m.name;
}

/** reachLine says how a machine is reached, for its row in the Machines view. */
export function reachLine(m: Machine): string {
  switch (reach(m)) {
    case "local":
      return "This computer";
    case "shipyard":
      return m.box === m.name ? `Shipyard box, through port ${portOf(m)}` : `Shipyard box ${m.box}, through port ${portOf(m)}`;
    default:
      return `A port you forwarded, ${bare(m.url)}`;
  }
}

/**
 * closing is the forward to close when a machine is removed: the one
 * Leadline opened for it. Null for a machine reached any other way, and
 * for a box whose forward someone else opened, which is theirs to close.
 */
export function closing(m: Machine): string | null {
  return reach(m) === "shipyard" && m.forward?.ours ? m.forward.id : null;
}

/**
 * removing says what taking a machine off the list does and does not do,
 * for the question that is asked first.
 */
export function removing(m: Machine): string[] {
  const forgets = `Leadline forgets ${m.name}: it leaves this list and the machine switcher.`;
  const keeps = `Nothing is uninstalled on ${m.name} and no data there is deleted. Its Leadline keeps running and recording, and you can add it again at any time.`;
  if (reach(m) === "shipyard") {
    return [
      forgets,
      closing(m)
        ? `The forward Leadline opened to it in Shipyard, on port ${portOf(m)}, is closed.`
        : `The Shipyard forward on port ${portOf(m)} was there before Leadline used it, so it is left open.`,
      keeps,
    ];
  }
  return [forgets, `The port you forwarded yourself (${portOf(m)}) is left as it is.`, keeps];
}

/**
 * saved is what is kept between launches: the machines that were added and
 * which one was showing. This Mac is left out, so its address always comes
 * from the app and an old save can't point it somewhere else.
 */
export function saved(state: Machines): string {
  return JSON.stringify({ current: state.current, machines: state.machines.filter((m) => m.id !== LOCAL_ID) });
}

/** restored reads what saved wrote. Anything it can't trust is dropped, down to this Mac alone. */
export function restored(text: string | null | undefined, localUrl: string): Machines {
  let state = first(localUrl);
  if (!text) return state;
  try {
    const raw = JSON.parse(text) as { current?: unknown; machines?: unknown };
    if (Array.isArray(raw.machines)) {
      for (const m of raw.machines as Partial<Record<keyof Machine, unknown>>[]) {
        if (typeof m?.url !== "string" || !loopbackUrl(m.url)) continue;
        const name = typeof m.name === "string" ? m.name : "";
        const f = m.forward as Partial<MachineForward> | undefined;
        let id: string | undefined;
        if (m.via === "shipyard" && typeof m.box === "string" && BOX_NAME.test(m.box) && typeof f?.id === "string" && FORWARD_ID.test(f.id)) {
          // Kept as it was saved: `opened` carries whether the forward is Leadline's own.
          state = addedBox(state, m.box, { id: f.id, url: m.url, opened: f.ours === true }, name);
          id = boxId(m.box);
        } else {
          state = added(state, name, m.url);
          id = state.machines.find((x) => x.url === loopbackUrl(m.url as string))?.id;
        }
        if (id && id !== LOCAL_ID && typeof m.seenMs === "number" && Number.isFinite(m.seenMs)) {
          state = seen(state, id, m.seenMs, typeof m.version === "string" ? m.version.slice(0, 64) : "");
        }
      }
    }
    if (typeof raw.current === "string") state = chosen(state, raw.current);
  } catch {
    return first(localUrl);
  }
  return state;
}

// The window's own list. The address of this Mac's daemon is the one thing
// the build can change (VITE_LEADLINE_URL, for a dev server pointed at a
// daemon on another port).
const env = (import.meta as { env?: Record<string, string | undefined> }).env;
const LOCAL_URL = loopbackUrl(env?.VITE_LEADLINE_URL ?? "") ?? "http://127.0.0.1:4320";
const KEY = "leadline.machines";

function storage(): Storage | null {
  try {
    return typeof localStorage === "undefined" ? null : localStorage;
  } catch {
    return null;
  }
}

function load(): Machines {
  try {
    return restored(storage()?.getItem(KEY), LOCAL_URL);
  } catch {
    return first(LOCAL_URL);
  }
}

let state = load();
const listeners = new Set<() => void>();

function set(next: Machines) {
  if (next === state) return;
  const moved = current(next).url !== current(state).url;
  state = next;
  try {
    storage()?.setItem(KEY, saved(state));
  } catch {
    // A full or forbidden store loses the list at the next launch, no more.
  }
  for (const l of listeners) l();
  if (moved) for (const l of moveListeners) l();
}

const moveListeners = new Set<() => void>();

// "Last reachable" is written down at most this often: it is said to the minute, and every write wakes the window.
const SEEN_EVERY_MS = 60_000;

export const machines = {
  get: (): Machines => state,
  current: (): Machine => current(state),
  subscribe(fn: () => void): () => void {
    listeners.add(fn);
    return () => listeners.delete(fn);
  },
  /** Calls fn when the window turns to a daemon at another address. */
  onMove(fn: () => void): () => void {
    moveListeners.add(fn);
    return () => moveListeners.delete(fn);
  },
  use: (id: string) => set(chosen(state, id)),
  /** Adds a machine by an address someone forwarded themselves. */
  add(name: string, address: string): Machine {
    set(added(state, name, address));
    const url = loopbackUrl(address);
    return state.machines.find((m) => m.url === url) as Machine;
  },
  /** Adds a Shipyard box by the forward the daemon opened or found for it, or moves the one on the list to that forward. */
  addBox(box: string, forward: Opened, name = ""): Machine {
    set(addedBox(state, box, forward, name));
    return state.machines.find((m) => m.id === boxId(box)) as Machine;
  },
  rename: (id: string, name: string) => set(renamed(state, id, name)),
  /** Notes that a machine's Leadline answered just now. */
  seen(id: string, version: string, atMs = Date.now()) {
    const was = state.machines.find((m) => m.id === id);
    if (!was || id === LOCAL_ID) return;
    if (was.version === version && was.seenMs !== undefined && atMs - was.seenMs < SEEN_EVERY_MS) return;
    set(seen(state, id, atMs, version));
  },
  remove: (id: string) => set(removed(state, id)),
};
