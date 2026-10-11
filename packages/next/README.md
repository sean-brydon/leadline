# @leadline/next

See a Next.js app in [Leadline](../../README.md) while you develop it, in Next's own terms: requests grouped by route (`/blog/[slug]`, not `/blog/hello`), server actions by name, renders, database queries, who the user was, and what happened in the browser.

It is one line in `next.config`. It is for development: in `next build` and `next start` it does nothing, and none of it is in the production bundle.

It builds on [`@leadline/node`](../node), which does the tracing, the database queries and the logs. This package adds what is particular to Next.js.

## Install

```sh
npm install --save-dev @leadline/next
```

If your production build installs without devDependencies, make it a regular dependency instead: the app's code imports `<Leadline />` and `identify`, even though both compile to nothing there.

## The one line

```ts
// next.config.ts
import type { NextConfig } from "next";
import { withLeadline } from "@leadline/next";

const nextConfig: NextConfig = {};

export default withLeadline(nextConfig);
```

That is the whole server side. There is no `instrumentation.ts` to write and nothing to add to the app's code: `next.config` is evaluated inside the dev server's own process before it takes a request, and that is where the provider starts. Run `next dev` (Turbopack or `--webpack`) with Leadline open and the app is there.

`withLeadline` takes a config object or a config function, and gives back the same kind of thing, so it composes with other plugins in either order. If you already have OpenTelemetry set up in `instrumentation.ts`, the two will fight over the tracer; keep one.

## The browser: `<Leadline />`

```tsx
// app/layout.tsx
import { Leadline } from "@leadline/next/client";

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body>
        <Leadline />
        {children}
      </body>
    </html>
  );
}
```

(In the Pages Router, put it in `pages/_app.tsx`.) It renders nothing. While you develop, the page reports:

- **Uncaught errors and unhandled rejections**, with their stacks in source positions (`app/cart/button.tsx:21:17`, not a chunk URL).
- **`console.error` and `console.warn`.**
- **Navigations**: the first load and every client-side one.
- **Web Vitals**: TTFB, FCP, LCP, CLS and INP, with the usual good / needs-improvement / poor rating.
- **`fetch` calls that failed** (a network error, or a 4xx or 5xx) **or were slow** (a second or more; `<Leadline slowFetchMs={500} />` to change it).
- **Every same-origin `fetch`** as the first span of a trace. The request gets a `traceparent` header, so a click, the server action or route handler it called, and that request's queries are one trace. This covers the router's own requests: client-side navigations and server actions.

The script is about 8 kB of plain JavaScript (3 kB gzipped) with no dependencies, not even React. It does not talk to Leadline: it posts small events to the dev server it came from (`/__leadline/b`), which adds what it knows (the route pattern, the action's name, the user) and passes them on. So there is no cross-origin request, nothing to configure, and nothing printed in the browser's console when Leadline is not running. `<Leadline console={false} />` leaves the console alone.

## Users: `identify()`

```ts
import { identify } from "@leadline/next/server";

export async function currentUser() {
  const session = await getSession(); // whatever your app does
  if (session) identify({ id: session.userId, email: session.email, name: session.name });
  return session;
}
```

Call it where the app works out who is asking: a Server Component, a route handler, a server action, `getServerSideProps`. From then on that request's spans and logs carry `user.id`, and `user.email` and `user.full_name` if you gave them. Only those three fields are read, whatever else is on the object. Spans that had already ended when you called it are not rewritten, so call it early.

### Without calling it

For three auth libraries the session cookie itself says who the user is, and `withLeadline` reads it on each request. Nothing is fetched, no database is asked and the library is not called. `identify()` always wins over this, and `withLeadline(config, { users: false })` turns it off.

| Library | What is read | What you get |
| --- | --- | --- |
| **Auth.js (NextAuth v5)** and **NextAuth v4**, with the default JWT sessions | The session cookie, decrypted the way the library does it, with the `AUTH_SECRET` (or `NEXTAUTH_SECRET`) already in the app's environment. | id, name, email |
| **Clerk** | The `sub` claim of the `__session` token. It is read, not verified: verifying can mean a request to Clerk. | id only. A name or an email would be a call to Clerk's API (`currentUser()`), so it is not made; they are taken only if your session token has `name` or `email` claims of its own. |
| **Better Auth**, with `session.cookieCache` enabled | The `better-auth.session_data` cookie ("compact" and "jwt" strategies). | id, name, email |

Not covered, because the cookie is only a key to a session kept somewhere else, and looking it up would cost a query on every request: Auth.js with database sessions, Better Auth without the cookie cache (or with its encrypted "jwe" strategy), and a secret passed in code instead of the environment. Use `identify()` there.

What is sent is an id, a name and an email. Cookies, tokens and secrets never are; a cookie is remembered between requests only as a SHA-256 digest.

## What it captures

**Requests, named as Next.js names them.** Next.js emits OpenTelemetry spans of its own; this reads them and names each request after them.

| Request | Shown as | `next.kind` |
| --- | --- | --- |
| A page | `GET /blog/[slug]` | `page` |
| The client router fetching a page (a Flight request) | `RSC GET /blog/[slug]`, with `next.rsc`, and `next.prefetch` for a prefetch | `page` |
| A server action | `action addNote`, with `next.action.name`, `next.action.file`, and the page it was posted to as `http.route` | `action` |
| A route handler | `GET /api/orders/[id]` | `route` |
| A Pages Router API route | `GET /api/health` | `api` |
| Any 404 | `GET /_not-found` | `not-found` |
| A file from `public/`, or an optimised image | `GET /logo.png` | `asset` |

Each carries `http.route` (the pattern), `next.route`, and `next.file` (the page or handler that answered). `proxy.ts` (or `middleware.ts`) shows as a step inside the request it ran for, with `next.kind: proxy`.

Not traced at all: `/_next/static`, hot reload, the error overlay's own requests, `favicon.ico`, and the dev server's calls home (telemetry, version checks).

**The route list.** When the dev server starts, and whenever a file is added to or removed from `app/` or `pages/`, the whole route list goes to Leadline: every page, layout, route handler (with its methods), API route and metadata route, with its pattern and file, and each page's layouts. That is how a route that exists but was never hit can be shown.

**Queries, logs and crashes**, as [`@leadline/node`](../node#what-it-captures) captures them, inside the request that made them. So an N+1 shows against `GET /orders`, not against a path.

**Next's data cache.** Each `fetch` on the server has `next.fetch.cache_status` (`hit`, `miss`, `skip`, or `hmr` for one replayed after a hot reload) and `next.fetch.cache_reason` (`revalidate: 60`, `cache: no-store`, ...). These are Next's own attributes, which 16.4 sets; they are passed on as they are.

**Render time and data time.** Each request has `leadline.data_ms`, the time it spent waiting on queries and fetches (overlaps counted once), and a page has `leadline.render_ms`, what is left of Next's render span when that waiting is taken out.

## What it does not capture

- **Layouts as separate spans.** Next.js renders a page and its layouts as one tree and emits one render span for it. The route list says which layouts a page has.
- **The Edge runtime.** A route or proxy with `runtime: "edge"` runs in a sandbox the provider is not in.
- **Server actions submitted without JavaScript.** The action's id is then in the form body, not a header, and the request is named `POST /page`.
- **`XMLHttpRequest`, WebSockets and `fetch` inside a Web Worker** in the browser.
- **Queries from a driver Next.js bundled.** A database driver has to be loaded by Node, not from inside the server bundle, to be seen. Next.js keeps `pg`, `mongodb`, `mongoose` and `@prisma/client` out of the bundle itself; in development `withLeadline` adds `mysql2`, `mysql`, `ioredis`, `redis`, `knex`, `tedious`, `oracledb`, `cassandra-driver` and `memcached`. For another one: `withLeadline(config, { externalPackages: [...] })`, and `externalPackages: false` to leave Next's list alone.

## What it changes in development

Nothing in the app's code or its output, and these four things in the dev server:

- The dev server answers `POST /__leadline/b`, for the page's reports. Only a same-origin POST is taken.
- Each incoming request gets a `traceparent` header naming its own span, which is what puts `proxy.ts` in the request's trace. Code that reads that header will see one in development.
- The driver packages above are added to `serverExternalPackages`.
- Once a page reports its own errors, the copy Next.js prints in the terminal as `[browser] …` is still printed but no longer sent to Leadline as a second entry.

## Options

`withLeadline(nextConfig, options)`. Every option is optional.

| Option | Default | |
| --- | --- | --- |
| `url` | `LEADLINE_URL`, else `http://127.0.0.1:4320` | Where the Leadline daemon listens. |
| `serviceName` | `OTEL_SERVICE_NAME`, else the app's `package.json` name | The name the app shows under. |
| `enabled` | on, unless `LEADLINE_DISABLED=1` | `false` turns everything off. |
| `users` | on | Read the user from a session cookie (above). |
| `routes` | on | Send the route list. |
| `externalPackages` | the drivers above | Packages to keep out of the server bundle in development. |
| `console`, `metrics`, `batchDelayMs`, `instrumentations`, `instrumentationConfig`, `debug` | | Passed to [`@leadline/node`](../node#options). |
| `dir` | `next.config`'s folder | The project folder. |

## How it stays out of the way

- **Production.** `withLeadline` returns the config object it was given and loads nothing else. In a production build `<Leadline />` compiles to a function that returns `null`, and `identify()` to nothing at all.
- **No daemon.** Nothing is retried or printed, in the terminal or in the browser. The app shows up, with its route list, soon after you start Leadline.
- **Cost.** Starting the provider adds about 0.2 s to the dev server's startup. A request costs well under a millisecond more.
- **Failure.** Nothing here throws into the app. `LEADLINE_DEBUG=1` says what it is doing.

## Tested with

Next.js 16.4.0, with Turbopack and with webpack, React 19.3, Node.js 22.22, OpenTelemetry JS SDK 2.12 / 0.223 (through `@leadline/node`). The App Router is what was exercised in a browser; of the Pages Router, one API route was, and the rest is covered only by the unit tests of how requests are named.

Next.js 15.5.27 (webpack) was run once through the same browser flow: requests by route, actions by name, the browser's reports and its stacks all arrived. That is all that is known about Next 15; Turbopack there, and its fetch cache attributes, were not looked at. A custom server (`next({ dev: true })`) is recognised on Next 16 only. The session cookie readers were checked against tokens made by `@auth/core` 0.41.3, `next-auth` 4 and `better-auth` 1.7.7; Clerk's against the token format in `@clerk/backend` 3.23, not against a live Clerk app.

Two things lean on Next.js internals and may need attention in a later version: the action names come from `server-reference-manifest.json`, and the source positions in browser stacks from the dev server's `/__nextjs_original-stack-frames`. If either changes, actions are named by their page and stacks stay as the browser gave them.

## Develop

```sh
(cd ../node && pnpm install && pnpm build)
pnpm install
pnpm test     # builds, then runs the unit tests; no network needed
```

[`examples/next-app`](../../examples/next-app) is a small app that uses all of it.
