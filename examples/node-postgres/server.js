// A small orders API for the made-up "acme" shop. Nothing in this file knows
// about Leadline: `pnpm dev` loads the provider with
// `node --import @leadline/node/register`, and that is all the wiring there is.
import express from "express";
import pg from "pg";

const PORT = Number(process.env.PORT ?? 3000);
const pool = new pg.Pool({
  connectionString: process.env.DATABASE_URL ?? "postgres://acme:acme@127.0.0.1:5432/acme",
});

const app = express();

app.get("/", (_req, res) => {
  res.json({
    routes: ["/customers", "/customers/:id", "/orders", "/orders/fixed", "/boom"],
  });
});

app.get("/customers", async (_req, res) => {
  const { rows } = await pool.query("SELECT id, name, email FROM customers ORDER BY id LIMIT 20");
  console.log("listed %d customers", rows.length);
  res.json(rows);
});

app.get("/customers/:id", async (req, res) => {
  const { rows } = await pool.query("SELECT id, name, email FROM customers WHERE id = $1", [
    req.params.id,
  ]);
  if (rows.length === 0) {
    console.warn("no customer with id", req.params.id);
    res.status(404).json({ error: "not found" });
    return;
  }
  res.json(rows[0]);
});

// The deliberate N+1: one query for the orders, then one more per order for
// its customer. In Leadline the customer query shows `max_per_trace` 25.
app.get("/orders", async (_req, res) => {
  const { rows: orders } = await pool.query(
    "SELECT id, customer_id, total_cents FROM orders ORDER BY id LIMIT 25",
  );
  for (const order of orders) {
    const { rows } = await pool.query("SELECT name FROM customers WHERE id = $1", [
      order.customer_id,
    ]);
    order.customer = rows[0]?.name;
  }
  console.log("listed %d orders, one customer query each", orders.length);
  res.json(orders);
});

// The same answer in one query.
app.get("/orders/fixed", async (_req, res) => {
  const { rows } = await pool.query(
    `SELECT o.id, o.customer_id, o.total_cents, c.name AS customer
       FROM orders o JOIN customers c ON c.id = o.customer_id
      ORDER BY o.id LIMIT 25`,
  );
  console.log("listed %d orders with a join", rows.length);
  res.json(rows);
});

// A query that fails, to see an error span and an error log.
app.get("/boom", async (_req, res) => {
  await pool.query("SELECT * FROM table_that_is_not_there");
  res.json({ unreachable: true });
});

app.use((err, req, res, _next) => {
  console.error("request failed:", req.method, req.path, err);
  res.status(500).json({ error: err.message });
});

// Made-up data, created once: 10 customers and 200 orders.
async function seed() {
  await pool.query(`
    CREATE TABLE IF NOT EXISTS customers (
      id serial PRIMARY KEY,
      name text NOT NULL,
      email text NOT NULL
    )`);
  await pool.query(`
    CREATE TABLE IF NOT EXISTS orders (
      id serial PRIMARY KEY,
      customer_id integer NOT NULL REFERENCES customers (id),
      total_cents integer NOT NULL
    )`);
  const { rows } = await pool.query("SELECT count(*)::int AS n FROM customers");
  if (rows[0].n > 0) return;
  await pool.query(`
    INSERT INTO customers (name, email)
    SELECT 'Customer ' || n, 'customer' || n || '@acme.example'
      FROM generate_series(1, 10) AS n`);
  await pool.query(`
    INSERT INTO orders (customer_id, total_cents)
    SELECT 1 + (n % 10), 500 + (n * 37) % 9000
      FROM generate_series(1, 200) AS n`);
  console.info("seeded 10 customers and 200 orders");
}

try {
  await seed();
} catch (err) {
  console.error("could not reach Postgres; is it running? (pnpm db)\n", err.message);
  process.exit(1);
}

app.listen(PORT, "127.0.0.1", () => {
  console.info(`acme-orders listening on http://127.0.0.1:${PORT}`);
});
