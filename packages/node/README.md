# @leadline/node

See a Node.js app in [Leadline](../../README.md) while you develop it: every request as a trace, every database query with its SQL and parameters, and everything the app logs, linked to the request that logged it.

Add it once. It sets up OpenTelemetry for you and sends to the Leadline daemon on your machine. It is for development: with `NODE_ENV=production` it does nothing.

## Install

```sh
npm install --save-dev @leadline/node
```

Node.js 20.6 or newer.

## Load it

Pick one. The provider has to start before the app loads `http`, `pg` and the rest, which is why the first way is the one to prefer: it needs no change to the app and always runs first.

**1. A flag** (CommonJS and ES module apps):

```sh
node --import @leadline/node/register server.js
```

Put it in the dev script, or in `NODE_OPTIONS="--import @leadline/node/register"` for tools that start Node for you.

**2. The app's first line:**

```js
import "@leadline/node/register";
```

In a CommonJS app (`require("@leadline/node/register")`, Node 20.19 or 22.12 and newer) this sees everything. In an ES module app the other `import`s in that file are resolved before this line runs, so HTTP and the database drivers are captured but a framework's route names may not be; use the flag there.

**3. A call, when you want options:**

```js
import { register } from "@leadline/node";

register({ serviceName: "checkout", batchDelayMs: 200 });
```

Load that file first (`node --import ./leadline.mjs server.js`).

### Next.js

Next bundles what `instrumentation.ts` imports, and OpenTelemetry cannot be bundled. Keep the provider out of the bundle in `next.config`:

```js
export default {
  serverExternalPackages: ["@leadline/node"],
};
```

```ts
// instrumentation.ts
export async function register() {
  if (process.env.NEXT_RUNTIME === "nodejs") {
    const { register } = await import("@leadline/node");
    register();
  }
}
```

## Options

Every option has an environment variable or a default, so `@leadline/node/register` needs no code.

| Option | Environment | Default | |
| --- | --- | --- | --- |
| `url` | `LEADLINE_URL` | `http://127.0.0.1:4320` | Where the Leadline daemon listens. |
| `serviceName` | `OTEL_SERVICE_NAME` | nearest `package.json` name | The name the app shows under. |
| `force` | `LEADLINE_FORCE=1` | off | Run even with `NODE_ENV=production`. |
| `enabled` | `LEADLINE_DISABLED=1` | on | Turn the provider off. |
| `console` | `LEADLINE_CONSOLE=0` | on | Send `console.*` output as logs. |
| `metrics` | `LEADLINE_METRICS=1` | off | Send metrics to `/v1/metrics` every 5 seconds. |
| `batchDelayMs` | | `500` | How long spans and logs wait to be sent together. |
| `instrumentationConfig` | | | Settings for the automatic instrumentations, by package name, laid over the provider's own. |
| `instrumentations` | | | More instrumentations to run, such as Prisma's. |
| `spanProcessors`, `logRecordProcessors` | | | More OpenTelemetry processors, run before Leadline's own. |
| `keepSpan`, `keepLog` | | | Called with each finished span or log record; return `false` to keep it from being sent. |
| `debug` | `LEADLINE_DEBUG=1` | off | Print what the provider is doing to stderr. |

`register()` returns `{ active, reason, serviceName, url, flush(), shutdown() }`. The first call in a process wins; later calls return the same object.

OpenTelemetry's own variables still apply where they make sense: `OTEL_RESOURCE_ATTRIBUTES`, `OTEL_NODE_DISABLED_INSTRUMENTATIONS`, `OTEL_NODE_ENABLED_INSTRUMENTATIONS`, `OTEL_SDK_DISABLED`, `OTEL_LOG_LEVEL`. `OTEL_EXPORTER_OTLP_ENDPOINT` is ignored on purpose: it often names a real collector, and full SQL must not follow it there.

## What it captures

- **Requests.** HTTP servers and clients, `fetch`/undici, and the frameworks in [`@opentelemetry/auto-instrumentations-node`](https://www.npmjs.com/package/@opentelemetry/auto-instrumentations-node) (Express, Koa, Hapi, NestJS, GraphQL, gRPC and more). A server span is named by its route (`GET /users/:id`) when the framework reports one, and by its path otherwise.
- **Database queries.** `pg`, `mysql2`, `mysql`, `mongodb`, `mongoose`, `redis`, `ioredis`, `knex`, `tedious`, `oracledb`, `cassandra-driver`, `memcached`. SQL is sent whole. Parameter values are sent where the instrumentation can read them: `pg` (`db.postgresql.values`), `mysql`, `oracledb`, and `mysql2`, which writes them into the statement. MongoDB commands keep their shape with values replaced by `?`, and Redis commands keep their keys but not their values: that is what lets Leadline group them.
- **Logs.** `console.debug`, `log`, `info`, `warn` and `error` become log records with the matching severity and the ids of the trace and span they ran in. The console prints exactly what it did before. `pino` and `bunyan` records are sent by their own instrumentations, and `winston`'s once `@opentelemetry/winston-transport` is installed.
- **Crashes.** An uncaught exception or unhandled rejection is recorded as a log (`fatal` when it ends the process), and the process then fails exactly as it would have. Rejections are seen in Node's default mode; with `--unhandled-rejections=warn`, or a handler of your own, Node does not report them to anyone else.

Off by default, because they bury a request under its own plumbing: `fs`, `dns` and `net` spans, and the `router` instrumentation (it repeats every Express 5 layer). Turn one back on with `instrumentationConfig: { "@opentelemetry/instrumentation-fs": { enabled: true } }`.

When a query builder and the driver under it both report the same statement (knex over `pg`, Prisma's instrumentation over `pg`), the driver's span is the one counted as the query. The outer span stays in the trace.

## ORMs and drivers

Most ORMs run on a driver from the list above and need nothing more: Drizzle, TypeORM, Sequelize, MikroORM and Kysely on `pg`, `mysql2` or `tedious`; knex; mongoose.

**Prisma 7** talks to the database through a driver adapter. With `@prisma/adapter-pg` every query already shows, with its parameters, through `pg`. With an adapter whose driver has no instrumentation (`@prisma/adapter-better-sqlite3`, `-libsql`, `-mariadb`, `-neon`, `-planetscale`, `-d1`), or to see Prisma's own steps in the trace, add Prisma's instrumentation:

```sh
npm install --save-dev @prisma/instrumentation
```

```js
// leadline.mjs, loaded with: node --import ./leadline.mjs server.js
import { register } from "@leadline/node";
import { PrismaInstrumentation } from "@prisma/instrumentation";

register({ instrumentations: [new PrismaInstrumentation()] });
```

Its query spans carry the SQL but not the parameter values. **Prisma 6 and older** by default run queries in Prisma's own engine, not through a Node driver, so they need `@prisma/instrumentation` (the same major version as `@prisma/client`) to show any queries at all.

**Not covered** by the automatic set: `postgres` (postgres.js), `better-sqlite3`, `node:sqlite`, `@libsql/client`, `@neondatabase/serverless`, and Fastify (use [`@fastify/otel`](https://www.npmjs.com/package/@fastify/otel)). If one has an OpenTelemetry instrumentation, pass it in `instrumentations`. Leadline lists a span as a query when it has `db.system.name` (or `db.system`) and takes the statement from `db.query.text` (or `db.statement`).

## How it stays out of the way

- **Production.** With `NODE_ENV=production` nothing is loaded, patched or sent, unless `force` is set. The provider records full SQL, parameters and logs and sends them unauthenticated to a local port; that is right on your machine and nowhere else.
- **No daemon.** Before sending, the provider checks that something is listening. If not, the batch is dropped: no retries, no errors, nothing printed. It checks again at most every two seconds, and only when there is something to send, so the app shows up soon after you start Leadline.
- **Startup.** No network call is waited on. Loading OpenTelemetry's instrumentations takes about 150 ms of startup, the same as OpenTelemetry's own `register`.
- **Exit.** When the process ends, on its own, by `process.exit()` or by crashing, what was not yet sent is handed to a short-lived helper process to deliver, so the last half second of a crash is not lost. A process killed by a signal (Ctrl-C) can lose it.
- **`leadline run`.** `leadline run -- <your dev command>` loads the provider by itself when it is installed, so the dev script needs no flag. It also reads the command's output, to capture what no provider sends, so a `console.log` would reach Leadline twice. To prevent that, the provider ends each console line it has sent with a mark the terminal does not show (an APC escape sequence), and `leadline run` removes the mark and leaves that line to the provider's record, the one with the trace id. The mark is written only to a stream `leadline run` itself reads (it names them in `LEADLINE_RUN_STDIO`, and the provider checks with `fstat`), so `node build.js > out.json` writes exactly what it would have.
- **Another OpenTelemetry SDK.** If the app already set one up, the provider stays out and says so once. Point that SDK's OTLP/HTTP exporter at Leadline instead.
- **Failure.** `register()` never throws. Set `LEADLINE_DEBUG=1` to see why nothing is arriving.

## Develop

```sh
pnpm install
pnpm test     # builds, then runs the unit tests; no network needed
```
