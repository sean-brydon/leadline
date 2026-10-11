import { headers } from "next/headers";
import Link from "next/link";
import { query } from "@/lib/db";

export default async function Products() {
  // Two fetches to this app's own route handlers, to show Next's data cache
  // in Leadline: the rates are kept for a minute, the time is fetched on
  // every render.
  const origin = `http://${(await headers()).get("host")}`;
  const products = await query<{ id: number; name: string; price_cents: number }>(
    "SELECT id, name, price_cents FROM products ORDER BY id",
  );
  const rates: { eur: number } = await fetch(`${origin}/api/rates`, { next: { revalidate: 60 } }).then((r) =>
    r.json(),
  );
  const { now }: { now: string } = await fetch(`${origin}/api/time`, { cache: "no-store" }).then((r) => r.json());

  return (
    <main>
      <h1>Products</h1>
      <ul>
        {products.map((product) => (
          <li key={product.id}>
            <Link href={`/products/${product.id}`}>{product.name}</Link>: ${(product.price_cents / 100).toFixed(2)} (€
            {((product.price_cents / 100) * rates.eur).toFixed(2)})
          </li>
        ))}
      </ul>
      <p>
        <small>Prices as of {now}.</small>
      </p>
    </main>
  );
}
