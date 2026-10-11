// A pretend sign-in: the cookie holds a customer id and nothing checks it.
// It stands in for whatever auth the real app has, to show where identify()
// goes: the one place the app works out who is asking.
import { identify } from "@leadline/next/server";
import { cookies } from "next/headers";
import { query } from "./db";

export const SESSION_COOKIE = "acme_user";

export interface User {
  id: string;
  name: string;
  email: string;
}

/** The signed-in customer, or null. Tells Leadline who it is. */
export async function currentUser(): Promise<User | null> {
  const id = Number((await cookies()).get(SESSION_COOKIE)?.value);
  if (!Number.isInteger(id)) return null;
  const rows = await query<{ id: number; name: string; email: string }>(
    "SELECT id, name, email FROM customers WHERE id = $1",
    [id],
  );
  if (rows.length === 0) return null;
  const user = { id: String(rows[0].id), name: rows[0].name, email: rows[0].email };
  identify(user);
  return user;
}
