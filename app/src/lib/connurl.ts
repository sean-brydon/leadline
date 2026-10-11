// A database URL as the daemon shows it has **** where its password is: in
// the user part and in a `password=` parameter alike. Straight after a
// connection is added the page still holds the URL the person typed, so it
// can write the proxy's URL out whole, to be copied into a .env in one go.
// That typed URL lives in the dialog's state and nowhere else.
//
// Nothing here decodes or re-encodes: the password goes across exactly as
// it was typed, percent signs and all, which is what the app's own driver
// will be given.

export const MASK = "****";

type Parts = { before: string; password: string | null; after: string };

// cutAuthority finds the password in the part before the host. The user part
// ends at the last "@" before the path, since a password may hold an "@"
// its owner didn't escape.
function cutAuthority(url: string): Parts | null {
  const start = url.indexOf("://");
  if (start < 0) return null;
  const from = start + 3;
  let end = url.length;
  for (const stop of ["/", "?", "#"]) {
    const at = url.indexOf(stop, from);
    if (at >= 0 && at < end) end = at;
  }
  // An unescaped "/" or "?" in a password hides the "@" behind it: look in
  // the whole URL then, as the daemon's own reader does.
  let at = url.lastIndexOf("@", end - 1);
  if (at < from) {
    // "host:5432/db?x=a@b" has an "@" too, and no password: what follows
    // its colon is a port.
    const colon = url.indexOf(":", from);
    if (colon < 0 || colon > end || /^\d*$/.test(url.slice(colon + 1, end))) return null;
    at = url.lastIndexOf("@");
    if (at < from) return null;
  }
  const colon = url.indexOf(":", from);
  if (colon < 0 || colon > at) return { before: url.slice(0, at), password: null, after: url.slice(at) };
  return { before: url.slice(0, colon + 1), password: url.slice(colon + 1, at), after: url.slice(at) };
}

const PARAM = /([?&]password=)([^&#]*)/i;

/** passwordIn reads the password out of a URL as typed: from its user part, else from a `password=` parameter. Null when it has none. */
export function passwordIn(url: string): string | null {
  const auth = cutAuthority(url.trim());
  if (auth?.password) return auth.password;
  const param = PARAM.exec(url);
  return param && param[2] ? param[2] : null;
}

/** masked says whether a URL still has a password to be filled in. */
export function masked(url: string): boolean {
  const auth = cutAuthority(url);
  return auth?.password === MASK || PARAM.exec(url)?.[2] === MASK;
}

/**
 * withPassword writes the password of `typed` into `shown`, a URL from the
 * daemon with **** in its place. It returns `shown` as it was when there is
 * nothing to fill in, or nothing to fill it with: the typed URL had no
 * password, or was itself masked (which to the daemon means "the one you
 * have", and the page never had it).
 */
export function withPassword(shown: string, typed: string): string {
  const password = passwordIn(typed);
  if (!password || password === MASK) return shown;
  let out = shown;
  const auth = cutAuthority(out);
  if (auth?.password === MASK) out = auth.before + password + auth.after;
  const param = PARAM.exec(out);
  if (param && param[2] === MASK) out = out.slice(0, param.index) + param[1] + password + out.slice(param.index + param[0].length);
  return out;
}

/** hidden writes a URL with its password as dots, for showing a line that will be copied whole. */
export function hidden(url: string): string {
  const password = passwordIn(url);
  if (!password || password === MASK) return url;
  return withPassword(withMask(url), `x://x:${"•".repeat(8)}@x`);
}

/** withMask puts **** where a URL's password is. */
export function withMask(url: string): string {
  let out = url;
  const auth = cutAuthority(out);
  if (auth?.password) out = auth.before + MASK + auth.after;
  const param = PARAM.exec(out);
  if (param && param[2]) out = out.slice(0, param.index) + param[1] + MASK + out.slice(param.index + param[0].length);
  return out;
}

/** urlKind says which kind of source a typed URL is for, from its scheme; null while it is neither. */
export function urlKind(url: string): "postgres" | "redis" | null {
  const u = url.trim().toLowerCase();
  if (u.startsWith("postgres://") || u.startsWith("postgresql://")) return "postgres";
  if (u.startsWith("redis://") || u.startsWith("rediss://")) return "redis";
  return null;
}
