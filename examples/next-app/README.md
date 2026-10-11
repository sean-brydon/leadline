# next-app example

A small shop for the made-up "acme": Next.js 16 (App Router), Postgres through `pg`, and [`@leadline/next`](../../packages/next). Everything Leadline needs is three small things:

```ts
// next.config.ts
export default withLeadline(nextConfig);
```

```tsx
// app/layout.tsx
<Leadline />
```

```ts
// lib/session.ts, where the app works out who is signed in
identify(user);
```

## Run it

1. Start Leadline (the app, or `go run ./cmd/leadlined` from the repository root).

2. Build the two packages this example links to:

   ```sh
   (cd ../../packages/node && pnpm install && pnpm build)
   (cd ../../packages/next && pnpm install && pnpm build)
   ```

3. Start Postgres and the app:

   ```sh
   pnpm install
   pnpm db        # Postgres 17 in Docker, on 127.0.0.1:5432
   pnpm dev       # http://localhost:3000, with Turbopack
   ```

   `pnpm dev:webpack` is the same with webpack. The app creates three tables and fills them with made-up rows the first time. To use another database, set `DATABASE_URL`; for another port, `PORT=3001 pnpm dev`. If Leadline is not on `http://127.0.0.1:4320`, set `LEADLINE_URL`.

4. Use it in a browser:

   - **Products** (`/`) lists the products. Click one: that is a client-side navigation to `/products/[id]`.
   - **Orders** (`/orders`) has the N+1, and a form whose "Add note" is a server action.
   - **Sign in** (`/sign-in`) is a pretend sign-in: pick a customer.
   - **Playground** (`/playground`) has buttons that throw, reject a promise, log an error, and fetch a route handler that works, one that answers 404 and one that is slow.

5. Look in Leadline, or ask the daemon:

   ```sh
   curl http://127.0.0.1:4320/api/endpoints
   curl http://127.0.0.1:4320/api/queries
   curl http://127.0.0.1:4320/api/traces
   curl http://127.0.0.1:4320/api/logs
   ```

`pnpm db:stop` removes the Postgres container when you are done.

## What to look for

- **Requests by route, not by path.** `GET /products/[id]` however many products you open; `RSC GET /orders` for the client router's fetch of a page; `GET /api/orders/[id]` for the route handler; `GET /api/health` for the one Pages Router API route (`pages/api/health.ts`); `GET /_not-found` for any address that is not there.
- **Server actions by name.** `action addNote`, `action signIn`, `action signOut`, each with the page it was posted to as its route.
- **The N+1.** `SELECT name FROM customers WHERE id = ?` runs 25 times in one request to `/orders`. `/api/orders` returns the same list with one join.
- **The user.** After you sign in, requests carry `user.id`, `user.email` and `user.full_name`, because `lib/session.ts` calls `identify()`. So do the errors the browser reports.
- **One trace from the click to the query.** "Fetch the orders" in the playground starts a trace in the browser (`fetch GET /api/orders`) that continues in the route handler and its queries. Navigations and server actions do the same.
- **The browser.** "Throw an error" arrives as an error log with its stack in source positions (`app/playground/buttons.tsx`); the 404 and the slow fetch arrive as warnings tied to their traces; each page load reports its Web Vitals.
- **Next's data cache.** The products page fetches `/api/rates` with `revalidate: 60` and `/api/time` with `cache: "no-store"`. The first shows `next.fetch.cache_status: hit` once it has been fetched (and `skip` on a hard refresh, when Next.js bypasses its cache), the second always `skip`.
- **The proxy.** `proxy.ts` shows as a step at the start of every request.
- **Render and data time.** Each page request has `leadline.render_ms` and `leadline.data_ms`.
- **The route list.** A log record with `leadline.kind: next.routes` lists every route with its file, including the ones you never opened.

`pnpm build && pnpm start` runs the same app with none of this: `withLeadline` hands the config back untouched, `<Leadline />` is a function that returns `null`, and `identify()` is gone from the bundle. Stopping Leadline changes nothing about how `pnpm dev` runs.

## Two lines that are this repository's, not yours

`next.config.ts` sets `turbopack.root` to the repository root. That is only because this example links to `@leadline/next` by path (`link:../../packages/next`), which puts the package outside the app's own folder, where Turbopack will not follow it. An app that installs the package from npm needs no such line. `agentRules: false` keeps `next dev` from writing an `AGENTS.md` into the example.
