// What app code imports on the server. It is bundled into the app, so it
// holds no logic of its own: it calls what `withLeadline` put on the global
// object when the dev server started. In production nothing was put there,
// and this does nothing.

/** Who made a request. Only what is given is sent. */
export interface LeadlineUser {
  id: string | number;
  email?: string | null;
  name?: string | null;
}

interface Runtime {
  identify(user: LeadlineUser): void;
}

/**
 * Tells Leadline who the current request is for. Call it where the app works
 * out the session, in a Server Component, a route handler, a server action or
 * `getServerSideProps`. From then on, the spans and logs of that request
 * carry `user.id` (and `user.email`, `user.full_name` if given).
 *
 * Pass an id, and an email and a name if you want them shown: never a token
 * or the session itself. Extra fields are ignored.
 */
export function identify(user: LeadlineUser): void {
  // A constant in the app's bundle: in a production build the rest of this
  // function is removed.
  if (process.env.NODE_ENV !== "development") return;
  try {
    (globalThis as { [key: symbol]: Runtime | undefined })[Symbol.for("leadline.next")]?.identify(user);
  } catch {
    // A development tool must not take the request down with it.
  }
}
