import { query } from "@/lib/db";
import { currentUser } from "@/lib/session";

// The order list in one query: what /orders should have done.
export async function GET() {
  await currentUser();
  const orders = await query(
    `SELECT o.id, o.quantity, o.note, c.name AS customer, p.name AS product
       FROM orders o
       JOIN customers c ON c.id = o.customer_id
       JOIN products p ON p.id = o.product_id
      ORDER BY o.id LIMIT 25`,
  );
  console.log("listed %d orders with a join", orders.length);
  return Response.json(orders);
}
