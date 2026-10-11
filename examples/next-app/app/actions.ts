"use server";

import { revalidatePath } from "next/cache";
import { cookies } from "next/headers";
import { redirect } from "next/navigation";
import { query } from "@/lib/db";
import { currentUser, SESSION_COOKIE } from "@/lib/session";

export async function signIn(formData: FormData) {
  const id = Number(formData.get("customer"));
  const rows = await query("SELECT id FROM customers WHERE id = $1", [id]);
  if (rows.length === 0) throw new Error(`no customer ${id}`);
  (await cookies()).set(SESSION_COOKIE, String(id), { httpOnly: true, sameSite: "lax" });
  console.log("customer %d signed in", id);
  redirect("/");
}

export async function signOut() {
  (await cookies()).delete(SESSION_COOKIE);
  redirect("/");
}

export async function addNote(formData: FormData) {
  const user = await currentUser();
  const orderId = Number(formData.get("order"));
  const note = String(formData.get("note") ?? "").slice(0, 200);
  await query("UPDATE orders SET note = $1 WHERE id = $2", [note, orderId]);
  console.log("%s left a note on order %d", user?.name ?? "someone", orderId);
  revalidatePath("/orders");
}
