import { useCallback, useEffect, useReducer, useRef, useState, useSyncExternalStore } from "react";
import { base, forgetMissing } from "./api";
import { machines, type Machine, type Machines } from "./machines";
import { fresh, trimmed, withHead, withOlder, type Listing, type Paging } from "./pages";

// One event stream for the whole app. The daemon says "new data arrived"
// and each view refetches what it shows.
const listeners = new Set<() => void>();
const stateListeners = new Set<() => void>();
let source: EventSource | null = null;
let connected = false;

function setConnected(v: boolean) {
  if (connected === v) return;
  connected = v;
  for (const l of stateListeners) l();
}

function ensure() {
  if (source) return;
  const mine = new EventSource(`${base()}/api/events`);
  source = mine;
  const wake = () => {
    for (const l of listeners) l();
  };
  // "hello" opens every connection, a reconnect too: refetch then, so data
  // that arrived while the daemon was away shows up.
  mine.addEventListener("hello", () => {
    if (source !== mine) return;
    forgetMissing();
    setConnected(true);
    wake();
  });
  mine.addEventListener("ingest", () => source === mine && wake());
  // EventSource retries by itself.
  mine.onerror = () => source === mine && setConnected(false);
}

// The window turned to another machine's daemon: the stream follows it, and
// nothing is known about the new one until it says hello. What each view
// holds belongs to the old machine, so the hooks below let go of it.
machines.onMove(() => {
  if (!source) return;
  source.close();
  source = null;
  setConnected(false);
  ensure();
});

/** useMachines is the list of daemons the window can show, and which it is showing. */
export function useMachines(): Machines {
  return useSyncExternalStore(machines.subscribe, machines.get);
}

/** useMachine is the machine whose daemon the window is showing. */
export function useMachine(): Machine {
  useMachines();
  return machines.current();
}

/** useConnected says whether the daemon is reachable right now. */
export function useConnected(): boolean {
  return useSyncExternalStore(
    (fn) => {
      ensure();
      stateListeners.add(fn);
      return () => stateListeners.delete(fn);
    },
    () => connected,
  );
}

const REFETCH_GAP_MS = 1000;
// With no new data the time window still moves on, so charts refetch anyway.
const IDLE_REFETCH_MS = 15_000;
// An answer that was slow to come is not asked for again straight away: the
// wait is at least four times what it took, so a view on a long window keeps
// the daemon busy a fifth of the time at most.
const REST = 4;
// A request that has not answered in this long no longer holds up the next.
const GIVE_UP_MS = 30_000;

/**
 * useRefetch calls run now, again when deps change, when the daemon reports
 * new data (at most once a second, and never while the last call is still
 * out) and every 15 seconds. run is told whether its answer is still wanted.
 */
function useRefetch(run: (wanted: () => boolean) => Promise<unknown>, deps: unknown[]) {
  const runRef = useRef(run);
  runRef.current = run;

  // Another machine is another question, whatever the deps say.
  const at = useMachine().url;
  useEffect(() => {
    let alive = true;
    let seq = 0;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let last = 0;
    let took = 0;
    let out = false; // a call is out
    let again = false; // and data arrived meanwhile

    const go = () => {
      timer = undefined;
      const began = Date.now();
      if (out && began - last < GIVE_UP_MS) {
        again = true;
        return;
      }
      last = began;
      out = true;
      const mine = ++seq;
      const done = () => {
        if (!alive || mine !== seq) return;
        out = false;
        took = Date.now() - began;
        if (again) {
          again = false;
          schedule();
        }
      };
      runRef.current(() => alive && mine === seq).then(done, done);
    };
    const schedule = () => {
      if (timer !== undefined) return;
      const gap = Math.max(REFETCH_GAP_MS, took * REST);
      timer = setTimeout(go, Math.max(0, gap - (Date.now() - last)));
    };

    go();
    ensure();
    listeners.add(schedule);
    const idle = setInterval(schedule, IDLE_REFETCH_MS);
    return () => {
      alive = false;
      listeners.delete(schedule);
      clearInterval(idle);
      if (timer !== undefined) clearTimeout(timer);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, at]);
}

const message = (e: unknown) => (e instanceof Error ? e.message : String(e));

/**
 * useLive loads data, and loads it again when deps change, when the daemon
 * reports new data (at most once a second) and every 15 seconds.
 */
export function useLive<T>(load: () => Promise<T>, deps: unknown[]): { data: T | undefined; error: string | null } {
  // What was loaded, and from which machine: an answer from the one the
  // window has left is not shown as the new one's.
  const at = useMachine().url;
  const [got, setGot] = useState<{ at: string; data: T | undefined; error: string | null }>({ at, data: undefined, error: null });
  useRefetch(
    (wanted) =>
      load().then(
        (d) => {
          if (wanted()) setGot({ at, data: d, error: null });
        },
        (e: unknown) => {
          if (wanted()) setGot((was) => ({ at, data: was.at === at ? was.data : undefined, error: message(e) }));
        },
      ),
    deps,
  );
  return got.at === at ? { data: got.data, error: got.error } : { data: undefined, error: null };
}

// A list left at the top keeps at most this many rows; scrolling fetches
// the rest again.
const MAX_ROWS = 5000;

export type LiveList<T> = {
  /** Newest first. Undefined until the first page arrives. */
  rows: T[] | undefined;
  error: string | null;
  /** Whether older rows may follow, and whether they are being fetched. */
  more: boolean;
  loadingMore: boolean;
  loadMore: () => void;
  /** Rows that arrived while the user was scrolled away, not yet shown. */
  fresh: { count: number; atLeast: boolean };
  /**
   * The view says whether the user has scrolled away from the top. While
   * they have, new rows are held back, so nothing moves under them; back
   * at the top the list takes them in and is live again.
   */
  hold: (away: boolean) => void;
  /** Goes up when the list starts over, which is when to scroll to the top. */
  epoch: number;
};

/**
 * useLiveList is useLive for a list that pages backwards. load fetches the
 * newest page, or with a cursor the page after that row. The newest page
 * is refetched like any live data; older pages are fetched once, by
 * loadMore, and kept.
 */
export function useLiveList<T>(load: (before?: string) => Promise<T[]>, paging: Paging<T>, deps: unknown[]): LiveList<T> {
  const [, render] = useReducer((n: number) => n + 1, 0);
  const loadRef = useRef(load);
  loadRef.current = load;
  const s = useRef({
    list: undefined as Listing<T> | undefined,
    held: null as T[] | null, // the newest page, fetched while scrolled away
    away: false,
    stale: false, // deps changed: the rows shown answer the old question
    loadingMore: false,
    error: null as string | null,
    epoch: 0,
    at: "", // the machine the rows came from
  }).current;

  // Before the refetch below, so the first answer to new deps starts over.
  useEffect(() => {
    s.stale = true;
    s.held = null;
    s.loadingMore = false;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);
  // Another machine's rows are not this one's: the list is empty until it answers.
  const at = useMachine().url;
  if (s.at !== at) {
    s.at = at;
    s.list = undefined;
    s.held = null;
    s.stale = true;
    s.loadingMore = false;
    s.error = null;
  }

  useRefetch(
    (wanted) =>
      loadRef.current().then(
        (head) => {
          if (!wanted()) return;
          s.error = null;
          if (s.stale || !s.list) {
            s.list = withHead(undefined, head, paging);
            s.stale = false;
            s.held = null;
            s.away = false; // the view scrolls a new list to its top
            s.epoch++;
          } else if (s.away && s.list.rows.length > 0) {
            s.held = head;
          } else {
            s.list = trimmed(withHead(s.list, head, paging), MAX_ROWS);
          }
          render();
        },
        (e: unknown) => {
          if (!wanted()) return;
          s.error = message(e);
          render();
        },
      ),
    deps,
  );

  const hold = useCallback((away: boolean) => {
    if (s.away === away) return;
    s.away = away;
    if (!away && s.held && s.list) {
      s.list = trimmed(withHead(s.list, s.held, paging), MAX_ROWS);
      s.held = null;
      render();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const loadMore = useCallback(() => {
    const from = s.list;
    if (!from || !from.more || from.rows.length === 0 || s.loadingMore || s.stale) return;
    const last = from.rows[from.rows.length - 1];
    const epoch = s.epoch;
    s.loadingMore = true;
    render();
    loadRef.current(paging.cursor(last)).then(
      (page) => {
        if (epoch !== s.epoch || s.stale || !s.list) return;
        s.loadingMore = false;
        // Only if the list still ends where this page begins.
        const end = s.list.rows[s.list.rows.length - 1];
        if (end && paging.key(end) === paging.key(last)) s.list = withOlder(s.list, page, paging);
        render();
      },
      (e: unknown) => {
        if (epoch !== s.epoch || s.stale) return;
        s.loadingMore = false;
        s.error = message(e);
        render();
      },
    );
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return {
    rows: s.list?.rows,
    error: s.error,
    more: s.list?.more ?? false,
    loadingMore: s.loadingMore,
    loadMore,
    fresh: s.away && s.held ? fresh(s.list, s.held, paging) : NONE_FRESH,
    hold,
    epoch: s.epoch,
  };
}

const NONE_FRESH = { count: 0, atLeast: false };

/** useDebounced returns value once it has stopped changing for ms. */
export function useDebounced<T>(value: T, ms = 200): T {
  const [v, setV] = useState(value);
  useEffect(() => {
    const t = setTimeout(() => setV(value), ms);
    return () => clearTimeout(t);
  }, [value, ms]);
  return v;
}

/**
 * useEvery loads data now, again when deps change, and then every `ms`. It
 * is for what costs someone else something to answer (a database asked for
 * its own statistics), which useLive's "whenever anything arrives" would
 * ask for far too often. `again` asks now.
 */
export function useEvery<T>(load: () => Promise<T>, ms: number, deps: unknown[]): { data: T | undefined; error: unknown; loading: boolean; again: () => void } {
  const at = useMachine().url;
  const [got, setGot] = useState<{ key: string; data: T | undefined; error: unknown }>({ key: "", data: undefined, error: null });
  const [loading, setLoading] = useState(false);
  const [tick, kick] = useReducer((n: number) => n + 1, 0);
  const loadRef = useRef(load);
  loadRef.current = load;
  // What the answer is to: other deps, or another machine, and it is not shown.
  const key = JSON.stringify([at, ...deps]);
  useEffect(() => {
    let alive = true;
    let out = false;
    const go = () => {
      if (out) return;
      out = true;
      setLoading(true);
      loadRef.current().then(
        (data) => {
          out = false;
          if (!alive) return;
          setLoading(false);
          setGot({ key, data, error: null });
        },
        (error: unknown) => {
          out = false;
          if (!alive) return;
          setLoading(false);
          setGot((was) => ({ key, data: was.key === key ? was.data : undefined, error }));
        },
      );
    };
    go();
    const timer = setInterval(go, ms);
    return () => {
      alive = false;
      clearInterval(timer);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, ms, tick]);
  return got.key === key ? { data: got.data, error: got.error, loading, again: kick } : { data: undefined, error: null, loading, again: kick };
}
