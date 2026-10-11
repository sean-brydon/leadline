// What a trace's spans say beyond their timing: what each database call
// counts as, and who the request was for. Kept apart from React so it can
// be tested.

/** The fields of a span this file reads; api.ts's Span fits it. */
export type RoleSpan = { db_system: string; db_role?: string; attrs?: Record<string, unknown> };

/** What a secondary database span is called in the waterfall, and why it is dimmed. */
export const ROLE_MARK: Record<string, { word: string; why: string }> = {
  duplicate: {
    word: "seen twice",
    why: "The app's own report of a statement the capture also recorded. It is counted once, on the captured span.",
  },
  control: {
    word: "control",
    why: "Transaction and session control (BEGIN, COMMIT, SET). In the trace and in database time, not counted among the queries.",
  },
};

/** secondary says a span is a database call that isn't a query of its own: control, or a second sighting. */
export function secondary(s: RoleSpan): boolean {
  return s.db_role === "control" || s.db_role === "duplicate";
}

/**
 * queryCount is a trace's "N queries": the database calls that count as
 * one. A daemon too old to say what a span counts as marks none of them,
 * and then every database call is counted, as it always was.
 */
export function queryCount(spans: RoleSpan[]): number {
  const dbs = spans.filter((s) => s.db_system);
  if (!dbs.some((s) => s.db_role)) return dbs.length;
  return dbs.filter((s) => s.db_role === "query").length;
}

export type User = { id: string; name: string; email: string };

const text = (v: unknown): string => (typeof v === "string" ? v : typeof v === "number" ? String(v) : "");

/** userOf is who a trace was made for: the first span that carries "user.id", with the name and email beside it. */
export function userOf(spans: RoleSpan[]): User | null {
  for (const s of spans) {
    const id = text(s.attrs?.["user.id"]);
    if (id) return { id, name: text(s.attrs?.["user.full_name"]) || text(s.attrs?.["user.name"]), email: text(s.attrs?.["user.email"]) };
  }
  return null;
}

/** userLabel is what to call a user in a line: their name, else their email, else their id. */
export function userLabel(u: User): string {
  return u.name || u.email || u.id;
}
