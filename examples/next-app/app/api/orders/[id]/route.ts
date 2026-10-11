import { query } from "@/lib/db";

export async function GET(_request: Request, { params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  const [order] = await query("SELECT id, customer_id, product_id, quantity, note FROM orders WHERE id = $1", [
    Number(id) || 0,
  ]);
  if (!order) {
    console.warn("no order with id", id);
    return Response.json({ error: "not found" }, { status: 404 });
  }
  return Response.json(order);
}
