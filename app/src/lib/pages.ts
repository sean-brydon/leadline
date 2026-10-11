// A live list that pages backwards: its newest page is fetched again as
// data arrives, and older pages are fetched once, as the user scrolls to
// them. This file is the bookkeeping, kept apart from React so it can be
// tested; lib/live.ts drives it.

/**
 * How a list knows its rows. `key` names a row for good; `before` says
 * whether a sits above b in the list, which is newest first.
 */
export type Paging<T> = {
  size: number;
  key: (row: T) => string;
  cursor: (row: T) => string;
  before: (a: T, b: T) => boolean;
};

export type Listing<T> = {
  rows: T[];
  /** Whether older rows may follow the last one. */
  more: boolean;
};

/**
 * withHead puts a freshly fetched newest page on top of what the list
 * already holds. Rows the page has again are replaced by its copy, which
 * is how a trace that grew gets its new numbers and its new place.
 */
export function withHead<T>(list: Listing<T> | undefined, head: T[], p: Paging<T>): Listing<T> {
  const full = head.length >= p.size;
  if (!list || list.rows.length === 0 || head.length === 0) return { rows: head, more: full };
  // A short page is the whole list: anything else held is gone, out of
  // the window or deleted.
  if (!full) return { rows: head, more: false };
  const inHead = new Set(head.map(p.key));
  // More arrived than one page holds. What lies between the page and the
  // rows held is unknown, so start again from the page and let scrolling
  // fetch the rest.
  if (!list.rows.some((r) => inHead.has(p.key(r)))) return { rows: head, more: true };
  const last = head[head.length - 1];
  const rest = list.rows.filter((r) => !inHead.has(p.key(r)) && p.before(last, r));
  return { rows: head.concat(rest), more: list.more };
}

/** withOlder adds the page that follows the list's last row. */
export function withOlder<T>(list: Listing<T>, page: T[], p: Paging<T>): Listing<T> {
  const held = new Set(list.rows.map(p.key));
  // A trace can move down the list while it is being read (its root span
  // arrives last and starts first) and so turn up on a later page too.
  const added = page.filter((r) => !held.has(p.key(r)));
  // A full page with nothing new in it would be asked for again for ever.
  return { rows: list.rows.concat(added), more: page.length >= p.size && added.length > 0 };
}

/**
 * fresh counts the rows of a newest page that the list doesn't show yet.
 * `atLeast` is set when the whole page is new, so there may be more.
 */
export function fresh<T>(list: Listing<T> | undefined, head: T[], p: Paging<T>): { count: number; atLeast: boolean } {
  if (!list) return { count: 0, atLeast: false };
  const held = new Set(list.rows.map(p.key));
  const count = head.filter((r) => !held.has(p.key(r))).length;
  return { count, atLeast: count > 0 && count === head.length && head.length >= p.size };
}

/** trimmed keeps a list from growing without end while it sits at the top. */
export function trimmed<T>(list: Listing<T>, max: number): Listing<T> {
  return list.rows.length > max ? { rows: list.rows.slice(0, max), more: true } : list;
}

// A cursor is "<nanoseconds>.<tie>". The time is exact, which the row's
// millisecond float is not, so order comes from here.
function cut(cursor: string): [bigint, string] {
  const dot = cursor.indexOf(".");
  return [BigInt(cursor.slice(0, dot)), cursor.slice(dot + 1)];
}

/** Traces run newest first, and by id, largest first, within one instant. */
export function traceBefore(a: string, b: string): boolean {
  const [at, aid] = cut(a);
  const [bt, bid] = cut(b);
  return at !== bt ? at > bt : aid > bid;
}

/** Logs run newest first, and in the order they were stored within one instant. */
export function logBefore(a: string, b: string): boolean {
  const [at, an] = cut(a);
  const [bt, bn] = cut(b);
  return at !== bt ? at > bt : Number(an) < Number(bn);
}
