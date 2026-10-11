import { Leadline } from "@leadline/next/client";
import type { Metadata } from "next";
import Link from "next/link";
import { signOut } from "./actions";
import { currentUser } from "@/lib/session";

export const metadata: Metadata = { title: "acme shop" };

export default async function RootLayout({ children }: { children: React.ReactNode }) {
  const user = await currentUser();
  return (
    <html lang="en">
      <body style={{ fontFamily: "system-ui, sans-serif", maxWidth: 720, margin: "2rem auto", padding: "0 1rem" }}>
        <Leadline />
        <nav style={{ display: "flex", gap: "1rem", alignItems: "center", marginBottom: "1.5rem" }}>
          <Link href="/">Products</Link>
          <Link href="/orders">Orders</Link>
          <Link href="/playground">Playground</Link>
          <span style={{ marginLeft: "auto" }}>
            {user ? (
              <form action={signOut}>
                {user.name} <button type="submit">Sign out</button>
              </form>
            ) : (
              <Link href="/sign-in">Sign in</Link>
            )}
          </span>
        </nav>
        {children}
      </body>
    </html>
  );
}
