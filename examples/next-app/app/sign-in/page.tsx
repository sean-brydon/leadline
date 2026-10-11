import { signIn } from "../actions";
import { query } from "@/lib/db";

export default async function SignIn() {
  const customers = await query<{ id: number; name: string }>("SELECT id, name FROM customers ORDER BY id");
  return (
    <main>
      <h1>Sign in</h1>
      <p>Pick a customer. There is no password: this is a pretend sign-in.</p>
      <form action={signIn}>
        <select name="customer" aria-label="Customer">
          {customers.map((customer) => (
            <option key={customer.id} value={customer.id}>
              {customer.name}
            </option>
          ))}
        </select>{" "}
        <button type="submit">Sign in</button>
      </form>
    </main>
  );
}
