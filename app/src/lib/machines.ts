// The daemons this window can show. Today there is one, "This Mac", at the
// address the app was built for. The list exists so that a daemon on another
// machine (a VPS, reached through a port forwarded to this one) can be added
// beside it and switched to. Nothing here reaches another host: a machine's
// address is always a loopback one, which is also all the daemon's guard and
// the desktop app's connect-src allow.
//
// This file is the list and nothing else, kept apart from React so it can be
// tested; lib/api.ts and lib/live.ts read the current machine from it.

export type Machine = {
  id: string;
  /** What the person calls it. */
  name: string;
  /** The daemon's address, as in http://127.0.0.1:4320. */
  url: string;
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

/** first is the list with nothing added: this Mac alone. */
export function first(localUrl: string): Machines {
  return { machines: [{ id: LOCAL_ID, name: LOCAL_NAME, url: localUrl }], current: LOCAL_ID };
}

/**
 * added puts a machine on the list, or renames the one already at that
 * address. It throws, with a sentence for the person, when the address is
 * not one this window may reach.
 */
export function added(state: Machines, name: string, address: string): Machines {
  const url = loopbackUrl(address);
  if (!url) throw new Error("A machine's address must be on this computer, such as http://127.0.0.1:4336. Forward the other machine's port here first.");
  const label = name.trim() || url.replace("http://", "");
  const at = state.machines.find((m) => m.url === url);
  if (at) {
    // This Mac keeps its name.
    if (at.id === LOCAL_ID) return state;
    return { ...state, machines: state.machines.map((m) => (m === at ? { ...m, name: label } : m)) };
  }
  return { ...state, machines: [...state.machines, { id: `m:${url.replace("http://", "")}`, name: label, url }] };
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
      for (const m of raw.machines as { name?: unknown; url?: unknown }[]) {
        if (typeof m?.url !== "string" || !loopbackUrl(m.url)) continue;
        state = added(state, typeof m.name === "string" ? m.name : "", m.url);
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
  add(name: string, address: string): Machine {
    set(added(state, name, address));
    const url = loopbackUrl(address);
    return state.machines.find((m) => m.url === url) as Machine;
  },
  remove: (id: string) => set(removed(state, id)),
};
