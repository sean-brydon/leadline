// Who is signed in, read from the session cookie of the auth library the app
// uses, for the cases where the cookie itself says so. Nothing here makes a
// request, asks a database or calls into the library: where the cookie is
// only a key to a session stored somewhere else, there is no user to report
// and `identify()` is the way.
//
// What leaves this file is an id, and a name and an email if the session
// holds them. Tokens, cookies and secrets never do.
import { createDecipheriv, createHash, createHmac, hkdfSync, timingSafeEqual } from "node:crypto";
import type { LeadlineUser } from "./types.cjs";

export type AuthSource = "authjs" | "clerk" | "better-auth";

export interface Detected {
  source: AuthSource;
  user: LeadlineUser;
}

type Env = Record<string, string | undefined>;

/** A cheap test, so requests with none of these cookies pay for one regex. */
const ANY_SESSION_COOKIE = /(?:authjs|next-auth)\.session-token|__session|better-auth\.session_data/;

const AUTHJS_COOKIES = [
  "authjs.session-token",
  "__Secure-authjs.session-token",
  "next-auth.session-token",
  "__Secure-next-auth.session-token",
];
const BETTER_AUTH_COOKIES = ["better-auth.session_data", "__Secure-better-auth.session_data"];

/** Remembers what each session cookie decoded to, so a request costs a lookup. */
export class UserDetector {
  private readonly seen = new Map<string, Detected | null>();

  constructor(
    private readonly env: Env = process.env,
    private readonly now: () => number = Date.now,
  ) {}

  detect(cookieHeader: string | undefined): Detected | undefined {
    if (!cookieHeader || !ANY_SESSION_COOKIE.test(cookieHeader)) return undefined;
    // Keyed by a digest: the cookie is a credential, and this map outlives
    // the request.
    const key = createHash("sha256").update(cookieHeader).digest("base64");
    const known = this.seen.get(key);
    if (known !== undefined) return known ?? undefined;
    let found: Detected | undefined;
    try {
      found = detectUser(cookieHeader, this.env, this.now());
    } catch {
      // A cookie that does not parse is not ours to complain about.
    }
    this.seen.set(key, found ?? null);
    if (this.seen.size > 200) this.seen.delete(this.seen.keys().next().value!);
    return found;
  }
}

/** Reads the signed-in user from a `Cookie` header, if a known library's cookie holds one. */
export function detectUser(cookieHeader: string, env: Env = process.env, now = Date.now()): Detected | undefined {
  const cookies = parseCookies(cookieHeader);

  for (const name of AUTHJS_COOKIES) {
    const token = chunked(cookies, name);
    if (!token) continue;
    const user = authJsUser(token, name, env, now);
    if (user) return { source: "authjs", user };
  }
  for (const [name, value] of cookies) {
    // Clerk's cookie is `__session`, or `__session_<suffix>` when several
    // Clerk apps share a domain.
    if (name !== "__session" && !name.startsWith("__session_")) continue;
    const user = clerkUser(value, now);
    if (user) return { source: "clerk", user };
  }
  for (const name of BETTER_AUTH_COOKIES) {
    const data = chunked(cookies, name);
    if (!data) continue;
    const user = betterAuthUser(data, now);
    if (user) return { source: "better-auth", user };
  }
  return undefined;
}

/**
 * Auth.js (NextAuth v5) and NextAuth v4 with the default JWT sessions: the
 * cookie is the session, encrypted with a key derived from AUTH_SECRET. It
 * is decrypted here the way the library does it, with the secret the app
 * already has in its environment. With database sessions the cookie is a
 * random token, which this declines.
 */
export function authJsUser(token: string, cookieName: string, env: Env, now: number): LeadlineUser | undefined {
  const secrets = [env.AUTH_SECRET, env.AUTH_SECRET_1, env.AUTH_SECRET_2, env.AUTH_SECRET_3, env.NEXTAUTH_SECRET].filter(
    (secret): secret is string => Boolean(secret),
  );
  for (const secret of secrets) {
    const claims = decryptJwe(token, secret, cookieName);
    if (!claims) continue;
    if (typeof claims.exp === "number" && claims.exp * 1000 < now) return undefined;
    return pick(claims.sub ?? claims.id, claims.email, claims.name);
  }
  return undefined;
}

/**
 * Clerk's session cookie is a signed JWT whose `sub` is the user id. It is
 * read, not verified: verifying needs Clerk's keys, which may mean a
 * request, and this only labels telemetry on the developer's own machine.
 * The token carries no name or email unless the app added those claims;
 * fetching them (`currentUser()`) is a call to Clerk's API, so it is not done.
 */
export function clerkUser(token: string, now: number): LeadlineUser | undefined {
  const claims = jwtClaims(token);
  if (!claims) return undefined;
  const { sub, sid } = claims;
  // `__session` is also what Firebase Hosting calls its cookie. A Clerk
  // token names a Clerk user and a Clerk session.
  if (typeof sub !== "string" || !sub.startsWith("user_")) return undefined;
  if (typeof sid !== "string" || !sid.startsWith("sess_")) return undefined;
  if (typeof claims.exp === "number" && claims.exp * 1000 < now) return undefined;
  return pick(sub, claims.email, claims.name);
}

/**
 * Better Auth keeps sessions in the database: `better-auth.session_token`
 * is only a key to one, so there is nothing to read from it. With
 * `session.cookieCache` enabled it also sets `better-auth.session_data`, a
 * copy of the session and user, and that is read here ("compact" and "jwt"
 * strategies; the "jwe" one is encrypted with a key this does not derive).
 */
export function betterAuthUser(data: string, now: number): LeadlineUser | undefined {
  let user: unknown;
  let expires: number | undefined;
  if (data.split(".").length === 3) {
    const claims = jwtClaims(data);
    user = claims?.user;
    if (typeof claims?.exp === "number") expires = claims.exp * 1000;
  } else {
    const cache = json(base64Url(data)?.toString("utf8"));
    user = (cache?.session as Record<string, unknown> | undefined)?.user;
    if (typeof cache?.expiresAt === "number") expires = cache.expiresAt;
  }
  if (!user || typeof user !== "object") return undefined;
  if (expires !== undefined && expires < now) return undefined;
  const { id, email, name } = user as Record<string, unknown>;
  return pick(id, email, name);
}

function pick(id: unknown, email: unknown, name: unknown): LeadlineUser | undefined {
  if ((typeof id !== "string" && typeof id !== "number") || id === "") return undefined;
  return {
    id,
    ...(typeof email === "string" && email ? { email } : {}),
    ...(typeof name === "string" && name ? { name } : {}),
  };
}

/**
 * Decrypts a compact JWE made with `alg: dir`: Auth.js's A256CBC-HS512, or
 * the A256GCM that NextAuth v4 used. Returns the claims, or undefined when
 * the token is not one, or the secret is not the one it was made with.
 */
export function decryptJwe(token: string, secret: string, salt: string): Record<string, unknown> | undefined {
  const parts = token.split(".");
  if (parts.length !== 5) return undefined;
  const [protectedHeader, , ivPart, ciphertextPart, tagPart] = parts as [string, string, string, string, string];
  const header = json(base64Url(protectedHeader)?.toString("utf8"));
  const iv = base64Url(ivPart);
  const ciphertext = base64Url(ciphertextPart);
  const tag = base64Url(tagPart);
  if (!header || header.alg !== "dir" || !iv || !ciphertext || !tag) return undefined;
  const aad = Buffer.from(protectedHeader, "ascii");

  try {
    if (header.enc === "A256CBC-HS512") {
      const key = Buffer.from(hkdfSync("sha256", secret, salt, `Auth.js Generated Encryption Key (${salt})`, 64));
      const bits = Buffer.alloc(8);
      bits.writeBigUInt64BE(BigInt(aad.length * 8));
      const expected = createHmac("sha512", key.subarray(0, 32))
        .update(aad)
        .update(iv)
        .update(ciphertext)
        .update(bits)
        .digest()
        .subarray(0, 32);
      if (expected.length !== tag.length || !timingSafeEqual(expected, tag)) return undefined;
      const decipher = createDecipheriv("aes-256-cbc", key.subarray(32), iv);
      return json(Buffer.concat([decipher.update(ciphertext), decipher.final()]).toString("utf8"));
    }
    if (header.enc === "A256GCM") {
      // Auth.js salts the key with the cookie's name; NextAuth v4 did not.
      const infos: Array<[string, string]> = [
        [salt, `Auth.js Generated Encryption Key (${salt})`],
        ["", "NextAuth.js Generated Encryption Key"],
      ];
      for (const [keySalt, info] of infos) {
        try {
          const key = Buffer.from(hkdfSync("sha256", secret, keySalt, info, 32));
          const decipher = createDecipheriv("aes-256-gcm", key, iv);
          decipher.setAAD(aad);
          decipher.setAuthTag(tag);
          return json(Buffer.concat([decipher.update(ciphertext), decipher.final()]).toString("utf8"));
        } catch {
          // Wrong key: try the other derivation.
        }
      }
    }
  } catch {
    // Not a token this secret opens.
  }
  return undefined;
}

/** The claims of a JWT, unverified. */
export function jwtClaims(token: string): Record<string, unknown> | undefined {
  const parts = token.split(".");
  if (parts.length !== 3) return undefined;
  return json(base64Url(parts[1]!)?.toString("utf8"));
}

/** A cookie's value, put back together if the library split it into `.0`, `.1`, ... */
function chunked(cookies: Map<string, string>, name: string): string | undefined {
  const whole = cookies.get(name);
  if (whole) return whole;
  const chunks: string[] = [];
  for (let i = 0; cookies.has(`${name}.${i}`); i++) chunks.push(cookies.get(`${name}.${i}`)!);
  return chunks.length > 0 ? chunks.join("") : undefined;
}

export function parseCookies(header: string): Map<string, string> {
  const cookies = new Map<string, string>();
  for (const pair of header.split(";")) {
    const eq = pair.indexOf("=");
    if (eq < 0) continue;
    const name = pair.slice(0, eq).trim();
    let value = pair.slice(eq + 1).trim();
    if (value.startsWith('"') && value.endsWith('"')) value = value.slice(1, -1);
    try {
      value = decodeURIComponent(value);
    } catch {
      // Not percent-encoded after all.
    }
    if (name && !cookies.has(name)) cookies.set(name, value);
  }
  return cookies;
}

function base64Url(text: string): Buffer | undefined {
  if (!/^[A-Za-z0-9_-]*={0,2}$/.test(text)) return undefined;
  return Buffer.from(text, "base64url");
}

function json(text: string | undefined): Record<string, unknown> | undefined {
  if (!text) return undefined;
  try {
    const value: unknown = JSON.parse(text);
    return value && typeof value === "object" && !Array.isArray(value) ? (value as Record<string, unknown>) : undefined;
  } catch {
    return undefined;
  }
}
