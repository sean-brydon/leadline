// The made-up "acme" shop's database: 10 customers, 12 products, 200 orders,
// created the first time the app needs them.
import { Pool } from "pg";

const pool = new Pool({
  connectionString: process.env.DATABASE_URL ?? "postgres://acme:acme@127.0.0.1:5432/acme",
});

let seeded: Promise<void> | undefined;

/** Runs a query, once the tables exist. */
export async function query<Row extends object>(text: string, values: unknown[] = []): Promise<Row[]> {
  await (seeded ??= seed().catch((err) => {
    seeded = undefined;
    throw err;
  }));
  const { rows } = await pool.query(text, values);
  return rows as Row[];
}

async function seed() {
  await pool.query(`
    CREATE TABLE IF NOT EXISTS customers (
      id serial PRIMARY KEY,
      name text NOT NULL,
      email text NOT NULL
    )`);
  await pool.query(`
    CREATE TABLE IF NOT EXISTS products (
      id serial PRIMARY KEY,
      name text NOT NULL,
      price_cents integer NOT NULL
    )`);
  await pool.query(`
    CREATE TABLE IF NOT EXISTS orders (
      id serial PRIMARY KEY,
      customer_id integer NOT NULL REFERENCES customers (id),
      product_id integer NOT NULL REFERENCES products (id),
      quantity integer NOT NULL,
      note text
    )`);
  const { rows } = await pool.query("SELECT count(*)::int AS n FROM customers");
  if (rows[0].n > 0) return;
  await pool.query(`
    INSERT INTO customers (name, email)
    SELECT 'Customer ' || n, 'customer' || n || '@acme.example'
      FROM generate_series(1, 10) AS n`);
  await pool.query(`
    INSERT INTO products (name, price_cents)
    SELECT 'Acme widget no. ' || n, 500 + (n * 371) % 9000
      FROM generate_series(1, 12) AS n`);
  await pool.query(`
    INSERT INTO orders (customer_id, product_id, quantity)
    SELECT 1 + (n % 10), 1 + (n % 12), 1 + (n % 4)
      FROM generate_series(1, 200) AS n`);
  console.info("seeded 10 customers, 12 products and 200 orders");
}
