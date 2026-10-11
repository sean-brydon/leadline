import { notFound } from "next/navigation";
import { query } from "@/lib/db";

export default async function Product({ params }: { params: Promise<{ id: string }> }) {
  const id = Number((await params).id);
  if (!Number.isInteger(id)) notFound();
  const [product] = await query<{ id: number; name: string; price_cents: number }>(
    "SELECT id, name, price_cents FROM products WHERE id = $1",
    [id],
  );
  if (!product) notFound();
  const [{ sold }] = await query<{ sold: number }>(
    "SELECT coalesce(sum(quantity), 0)::int AS sold FROM orders WHERE product_id = $1",
    [id],
  );
  return (
    <main>
      <h1>{product.name}</h1>
      <p>
        ${(product.price_cents / 100).toFixed(2)}, {sold} sold.
      </p>
    </main>
  );
}
