# node-postgres example

A small orders API for the made-up "acme" shop: Express, `pg`, and [`@leadline/node`](../../packages/node). The app's code knows nothing about Leadline; the dev script loads the provider with one flag:

```json
"dev": "node --import @leadline/node/register server.js"
```

## Run it

1. Start Leadline (the app, or `go run ./cmd/leadlined` from the repository root).

2. Build the provider, which this example links to:

   ```sh
   (cd ../../packages/node && pnpm install && pnpm build)
   ```

3. Start Postgres and the app:

   ```sh
   pnpm install
   pnpm db        # Postgres 17 in Docker, on 127.0.0.1:5432
   pnpm dev       # http://127.0.0.1:3000
   ```

   The app creates two tables and fills them with made-up rows the first time. To use another database or port, set `DATABASE_URL` and `PORT`. If Leadline is not on `http://127.0.0.1:4320`, set `LEADLINE_URL`.

4. Make some requests:

   ```sh
   curl http://127.0.0.1:3000/customers
   curl http://127.0.0.1:3000/customers/3
   curl http://127.0.0.1:3000/orders          # the N+1
   curl http://127.0.0.1:3000/orders/fixed    # the same list in one query
   curl http://127.0.0.1:3000/boom            # a failing query
   ```

5. Look in Leadline, or ask the daemon:

   ```sh
   curl http://127.0.0.1:4320/api/queries
   curl http://127.0.0.1:4320/api/traces
   curl http://127.0.0.1:4320/api/logs
   ```

`pnpm db:stop` removes the Postgres container when you are done.

## What to look for

- **Traces** are named by route: `GET /customers/:id`, `GET /orders`.
- **Queries**: `SELECT name FROM customers WHERE id = ?` has `max_per_trace` 25. `/orders` runs it once per order; that is the N+1. `/orders/fixed` gets the same data with one join.
- **Parameters**: each `pg` span carries the values it ran with, in `db.postgresql.values`.
- **Logs**: every `console.log` in a handler has the trace id of its request. `/boom` leaves an error log with the Postgres error and its stack.

`pnpm start` runs the same app without the provider. So does `NODE_ENV=production pnpm dev`, and stopping Leadline changes nothing about how the app runs.
