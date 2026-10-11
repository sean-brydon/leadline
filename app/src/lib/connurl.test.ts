import assert from "node:assert/strict";
import test from "node:test";
import { hidden, masked, passwordIn, urlKind, withMask, withPassword } from "./connurl.ts";

test("the password typed goes into the proxy's URL", () => {
  assert.equal(
    withPassword("postgres://acme:****@127.0.0.1:15432/acme_dev?sslmode=disable", "postgres://acme:s3cret@127.0.0.1:54341/acme_dev"),
    "postgres://acme:s3cret@127.0.0.1:15432/acme_dev?sslmode=disable",
  );
});

test("it goes across exactly as typed, escapes and all", () => {
  assert.equal(
    withPassword("postgres://acme:****@127.0.0.1:15432/acme", "postgres://acme:p%40ss%2Fw%3Ard@localhost/acme"),
    "postgres://acme:p%40ss%2Fw%3Ard@127.0.0.1:15432/acme",
  );
  // An "@" or a ":" its owner didn't escape still belongs to the password.
  assert.equal(passwordIn("postgres://acme:p@ss:w0rd@localhost:5432/acme"), "p@ss:w0rd");
  assert.equal(passwordIn("postgres://acme:p/ss@localhost:5432/acme"), "p/ss");
  // An "@" further along is not a user part.
  assert.equal(passwordIn("postgres://localhost:5432/acme?options=a@b"), null);
  assert.equal(passwordIn("postgres://localhost/acme?options=a@b"), null);
});

test("a Redis password has no user before it", () => {
  assert.equal(passwordIn("redis://:hunter2@127.0.0.1:6379/0"), "hunter2");
  assert.equal(withPassword("redis://:****@127.0.0.1:56341/0", "redis://:hunter2@127.0.0.1:56341"), "redis://:hunter2@127.0.0.1:56341/0");
});

test("a password in a parameter is filled in there", () => {
  const typed = "postgres://127.0.0.1:5432/acme?user=acme&password=s3cret&sslmode=disable";
  assert.equal(passwordIn(typed), "s3cret");
  assert.equal(
    withPassword("postgres://127.0.0.1:15432/acme?user=acme&password=****&sslmode=disable", typed),
    "postgres://127.0.0.1:15432/acme?user=acme&password=s3cret&sslmode=disable",
  );
});

test("with nothing to fill in, or nothing to fill it with, the URL is left alone", () => {
  const shown = "postgres://acme:****@127.0.0.1:15432/acme";
  // No password was typed.
  assert.equal(withPassword(shown, "postgres://acme@localhost/acme"), shown);
  assert.equal(withPassword(shown, "postgres://localhost/acme"), shown);
  // The typed URL was the masked one: the page never had the password.
  assert.equal(withPassword(shown, "postgres://acme:****@localhost/acme"), shown);
  // The shown URL has no password in it.
  assert.equal(withPassword("postgres://127.0.0.1:15432/acme", "postgres://acme:s3cret@localhost/acme"), "postgres://127.0.0.1:15432/acme");
  assert.equal(withPassword("redis://127.0.0.1:6379", "redis://:pw@127.0.0.1:6379"), "redis://127.0.0.1:6379");
});

test("only the password is touched: four stars elsewhere stay", () => {
  assert.equal(
    withPassword("postgres://acme:****@127.0.0.1:15432/db****?application_name=****", "postgres://acme:pw@h/db"),
    "postgres://acme:pw@127.0.0.1:15432/db****?application_name=****",
  );
});

test("a URL says whether its password is still to be filled in", () => {
  assert.equal(masked("postgres://acme:****@h/db"), true);
  assert.equal(masked("postgres://h/db?password=****"), true);
  assert.equal(masked("postgres://acme:pw@h/db"), false);
  assert.equal(masked("postgres://h/db"), false);
  assert.equal(masked("nonsense"), false);
});

test("a password can be masked or shown as dots", () => {
  assert.equal(withMask("postgres://acme:s3cret@h:5432/db"), "postgres://acme:****@h:5432/db");
  assert.equal(withMask("postgres://h/db?password=s3cret&x=1"), "postgres://h/db?password=****&x=1");
  assert.equal(withMask("postgres://acme@h/db"), "postgres://acme@h/db");
  assert.equal(hidden("postgres://acme:s3cret@h:5432/db"), "postgres://acme:••••••••@h:5432/db");
  assert.equal(hidden("postgres://acme:****@h:5432/db"), "postgres://acme:****@h:5432/db");
  assert.equal(hidden("redis://127.0.0.1:6379"), "redis://127.0.0.1:6379");
});

test("a URL's scheme says what it is for", () => {
  assert.equal(urlKind("postgres://x"), "postgres");
  assert.equal(urlKind(" PostgreSQL://x"), "postgres");
  assert.equal(urlKind("redis://x"), "redis");
  assert.equal(urlKind("rediss://x"), "redis");
  assert.equal(urlKind("mysql://x"), null);
  assert.equal(urlKind("postgres"), null);
});
