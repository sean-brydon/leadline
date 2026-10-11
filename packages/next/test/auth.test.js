import assert from "node:assert/strict";
import { test } from "node:test";
import { createRequire } from "node:module";

const require = createRequire(import.meta.url);
const { UserDetector, authJsUser, betterAuthUser, clerkUser, decryptJwe, detectUser, parseCookies } = require("../dist/auth.cjs");

// Every token here is made up, for a user who does not exist, with a secret
// that protects nothing. The Auth.js and NextAuth ones were produced by the
// libraries themselves (`encode` from @auth/core 0.41.3 and next-auth 4), and
// the Better Auth ones were checked against better-auth 1.7.7's own
// `getCookieCache` and `signJWT`, so these tests hold the readers to the real
// formats. They expire in 2046.
const SECRET = "acme-test-secret-not-a-real-one";
const USER = { id: "acme-user-7", email: "ada@acme.example", name: "Ada Acme" };
const NOW = Date.UTC(2026, 9, 10);

const AUTHJS_V5 =
  "eyJhbGciOiJkaXIiLCJlbmMiOiJBMjU2Q0JDLUhTNTEyIiwia2lkIjoiaWxQS1lZT3N4dkthRFp4eWVobUFFTThjUlViaGM1T0w3c1N6R2FXX0duMy01TlFvbzFGQllaeldWQzNTYlRmN1gwcVQ1MXlSLUUxYVNURHdOUlJBbGcifQ..zKn_hZux4z_zsWOOm4ZHLg.2SzD9h8HNgJpoOEJl5dKYJGPeOjKjs77Lzip7iUtbG6KclOitrj76nEJJD0davo-IkaloiY_PV9VVsiHbiYOzy4cwJU58Xifg-F27voi6ivFH_qUNfikSTulGqTWLE4yZnHuQwzPDGfuN6PHlHz4Lqf9KNznhJj4Q5qGoOmMr2GcVehZ1JZW0BhlwslzyqWLpl1VG9MDGVi2uNdjPOiNWg.WmiLmALiXzmEr5BtJ-MTUY0djaYpznjz3qB0xtnjvEA";
const NEXTAUTH_V4 =
  "eyJhbGciOiJkaXIiLCJlbmMiOiJBMjU2R0NNIn0..rkkL98QDCrPRCZWK.UWfMaB0oXykSFWPxwY3cgvLqW5jzn92haHqf3S4mqQAujzX0WHVrK-X8WML7Cd0LScMokd3Kb-XHnnnre3mPWEpVSbdqPA250mtwJqWUWVbxjMsvVSNkByBl4YT19eS-asLkofvh2jsyqFZlqe7aEAMJgOelTDlTItNpjSTjFNjo8faI3mcteCNYsyfihu4hgA.9812pHXOzyBbi-IDQErUMg";
const BETTER_AUTH_COMPACT =
  "eyJzZXNzaW9uIjp7InNlc3Npb24iOnsiaWQiOiJzZXNzLTEiLCJ1c2VySWQiOiJhY21lLXVzZXItNyIsInRva2VuIjoidDBrZW4iLCJleHBpcmVzQXQiOiIyMDQ2LTAxLTAxVDAwOjAwOjAwLjAwMFoifSwidXNlciI6eyJpZCI6ImFjbWUtdXNlci03IiwibmFtZSI6IkFkYSBBY21lIiwiZW1haWwiOiJhZGFAYWNtZS5leGFtcGxlIiwiZW1haWxWZXJpZmllZCI6dHJ1ZX0sInVwZGF0ZWRBdCI6MTc5MTAwMDAwMDAwMCwidmVyc2lvbiI6IjEifSwiZXhwaXJlc0F0IjoyMzk4Mzc3NjAwMDAwLCJzaWduYXR1cmUiOiJSYmcxMk5wcE9GQXU5VzdOOW9mRzUyN1RZLW55TVZRR3psNHNiWU9tVC1jIn0";
const BETTER_AUTH_JWT =
  "eyJhbGciOiJIUzI1NiJ9.eyJzZXNzaW9uIjp7ImlkIjoic2Vzcy0xIn0sInVzZXIiOnsiaWQiOiJhY21lLXVzZXItNyIsIm5hbWUiOiJBZGEgQWNtZSIsImVtYWlsIjoiYWRhQGFjbWUuZXhhbXBsZSJ9LCJ1cGRhdGVkQXQiOjEsInZlcnNpb24iOiIxIiwiaWF0IjoxNzkxNjQ0NzEwLCJleHAiOjI0MjIzNjQ3MTB9.UH8hmoil8tgbpnaV3CM4ahLdCXtvQBFxjDKUNkq7O7A";

/** A JWT with these claims and a signature nobody checks. */
const jwt = (claims) =>
  ["eyJhbGciOiJSUzI1NiJ9", Buffer.from(JSON.stringify(claims)).toString("base64url"), "c2lnbmF0dXJl"].join(".");

test("Auth.js: the session cookie is decrypted with AUTH_SECRET", () => {
  const found = detectUser(`theme=dark; authjs.session-token=${AUTHJS_V5}`, { AUTH_SECRET: SECRET }, NOW);
  assert.deepEqual(found, { source: "authjs", user: USER });
  // Split across cookies, as the library does with a large session.
  const chunks = `authjs.session-token.0=${AUTHJS_V5.slice(0, 120)}; authjs.session-token.1=${AUTHJS_V5.slice(120)}`;
  assert.deepEqual(detectUser(chunks, { AUTH_SECRET: SECRET }, NOW)?.user, USER);
});

test("Auth.js: without the right secret there is no user, and no error", () => {
  assert.equal(detectUser(`authjs.session-token=${AUTHJS_V5}`, { AUTH_SECRET: "another-secret" }, NOW), undefined);
  assert.equal(detectUser(`authjs.session-token=${AUTHJS_V5}`, {}, NOW), undefined);
  // The salt is the cookie's name: the same token under another name does not open.
  assert.equal(authJsUser(AUTHJS_V5, "__Secure-authjs.session-token", { AUTH_SECRET: SECRET }, NOW), undefined);
  // A database session's cookie is a random token, not a JWE.
  assert.equal(detectUser("authjs.session-token=5f2c1b9e-0d3a-4c57-9a55-6f0d2a1e7b11", { AUTH_SECRET: SECRET }, NOW), undefined);
  assert.equal(decryptJwe("a.b.c.d.e", SECRET, "authjs.session-token"), undefined);
});

test("Auth.js: an expired session names nobody", () => {
  assert.equal(detectUser(`authjs.session-token=${AUTHJS_V5}`, { AUTH_SECRET: SECRET }, Date.UTC(2050, 0, 1)), undefined);
});

test("NextAuth v4: the older encryption, with NEXTAUTH_SECRET", () => {
  const found = detectUser(`next-auth.session-token=${NEXTAUTH_V4}`, { NEXTAUTH_SECRET: SECRET }, NOW);
  assert.deepEqual(found, { source: "authjs", user: USER });
});

test("Clerk: the user id is read from the session token; nothing else is in it", () => {
  const token = jwt({ sub: "user_2acme", sid: "sess_2acme", iss: "https://acme.clerk.accounts.dev", exp: NOW / 1000 + 60 });
  assert.deepEqual(detectUser(`__session=${token}`, {}, NOW), { source: "clerk", user: { id: "user_2acme" } });
  assert.deepEqual(detectUser(`__session_AbC123=${token}`, {}, NOW)?.user, { id: "user_2acme" });
  // Claims the app added to the token itself are taken.
  const custom = jwt({ sub: "user_2acme", sid: "sess_2acme", email: "ada@acme.example", name: "Ada Acme" });
  assert.deepEqual(clerkUser(custom, NOW), { id: "user_2acme", email: "ada@acme.example", name: "Ada Acme" });
});

test("Clerk: a `__session` cookie that is not Clerk's, or has expired, is left alone", () => {
  assert.equal(clerkUser(jwt({ sub: "firebase-uid-1", iss: "https://session.firebase.google.com/acme" }), NOW), undefined);
  assert.equal(clerkUser(jwt({ sub: "user_2acme", sid: "sess_2acme", exp: NOW / 1000 - 1 }), NOW), undefined);
  assert.equal(clerkUser("not-a-jwt", NOW), undefined);
  assert.equal(detectUser("__session=opaque-value", {}, NOW), undefined);
});

test("Better Auth: the cookie cache holds the user, in both readable forms", () => {
  assert.deepEqual(detectUser(`better-auth.session_data=${BETTER_AUTH_COMPACT}`, {}, NOW), {
    source: "better-auth",
    user: USER,
  });
  assert.deepEqual(betterAuthUser(BETTER_AUTH_JWT, NOW), USER);
  assert.equal(betterAuthUser(BETTER_AUTH_COMPACT, Date.UTC(2050, 0, 1)), undefined, "expired");
});

test("Better Auth: the session token alone is only a key to the database", () => {
  assert.equal(detectUser("better-auth.session_token=abc123.c2lnbmF0dXJl", {}, NOW), undefined);
});

test("no known cookie, no work, no user", () => {
  assert.equal(new UserDetector({}, () => NOW).detect("theme=dark; cart=3"), undefined);
  assert.equal(new UserDetector({}, () => NOW).detect(undefined), undefined);
});

test("the detector remembers a cookie by its digest, not by its value", () => {
  const detector = new UserDetector({ AUTH_SECRET: SECRET }, () => NOW);
  const header = `authjs.session-token=${AUTHJS_V5}`;
  assert.deepEqual(detector.detect(header)?.user, USER);
  assert.deepEqual(detector.detect(header)?.user, USER);
  const kept = JSON.stringify([...detector.seen.keys()]);
  assert.equal(kept.includes(AUTHJS_V5.slice(0, 40)), false);
  assert.equal(detector.seen.size, 1);
});

test("parseCookies", () => {
  const cookies = parseCookies('a=1; b="two words"; c=%7Bx%7D; broken; a=again; d=x=y');
  assert.deepEqual([...cookies], [["a", "1"], ["b", "two words"], ["c", "{x}"], ["d", "x=y"]]);
});
