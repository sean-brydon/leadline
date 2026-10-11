import { addNote } from "../actions";
import { query } from "@/lib/db";

interface Order {
  id: number;
  customer_id: number;
  product_id: number;
  quantity: number;
  note: string | null;
  customer?: string;
}

// The deliberate N+1: one query for the orders, then one more per order for
// its customer. In Leadline the customer query shows 25 calls per request.
// /api/orders returns the same list with one join.
export default async function Orders() {
  const orders = await query<Order>(
    "SELECT id, customer_id, product_id, quantity, note FROM orders ORDER BY id LIMIT 25",
  );
  for (const order of orders) {
    const [customer] = await query<{ name: string }>("SELECT name FROM customers WHERE id = $1", [
      order.customer_id,
    ]);
    order.customer = customer?.name;
  }
  console.log("listed %d orders, one customer query each", orders.length);

  return (
    <main>
      <h1>Orders</h1>
      <form action={addNote} style={{ display: "flex", gap: ".5rem", marginBottom: "1rem" }}>
        <input name="order" type="number" defaultValue={1} min={1} max={25} aria-label="Order" style={{ width: "4rem" }} />
        <input name="note" placeholder="A note for this order" aria-label="Note" style={{ flex: 1 }} />
        <button type="submit">Add note</button>
      </form>
      <table>
        <tbody>
          {orders.map((order) => (
            <tr key={order.id}>
              <td>#{order.id}</td>
              <td>{order.customer}</td>
              <td>
                {order.quantity} × product {order.product_id}
              </td>
              <td>{order.note}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </main>
  );
}
