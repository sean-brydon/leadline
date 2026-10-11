# Working on Leadline

Leadline is a local-first dev tool: it captures the traces, database queries, logs and metrics of apps running on your machine and shows them in a fast desktop app. Nothing leaves the machine.

## What is where

- `cmd/leadlined`: the daemon. One HTTP listener on `127.0.0.1:4320` takes OTLP/HTTP (`/v1/traces`, `/v1/logs`, `/v1/metrics`), serves the app's JSON API (`/api/*`), a server-sent "new data" stream (`/api/events`) and the MCP endpoint (`/mcp`). It also takes OTLP on the standard `4318` when that port is free (`-otlp ""` or `LEADLINE_OTLP_ADDR=off` turns that off).
- `cmd/leadline`: the command line: `run`, `doctor`, `prompt`, `brief`, `demo`, `skills`, `bench`, `service` (the daemon as a systemd user service on a Linux machine), `version`.
- `internal/store`: DuckDB (columnar, one file in `~/.leadline`). Every question the app asks is a SQL aggregate here. `link.go` decides what each span counts as; `retention.go` drops old data.
- `internal/ingest`: OTLP to rows, and `Fingerprint`, which groups queries by shape.
- `internal/api`: the HTTP surface, and `guard`, which keeps the daemon to localhost pages.
- `internal/pgproxy`: a Postgres proxy that records every statement passing through as a span, for apps with no instrumentation.
- `internal/integrations`: what a person connects (databases and Redis by URL), the proxies and Redis captures run for them, and `/api/sources`. Its HTTP API is written down in `internal/integrations/API.md`; the app is built from that file, so change them together.
- `internal/runner`: `leadline run`. It runs a dev command as if you had run it yourself, turns what it prints into logs, and sets it up to send telemetry.
- `internal/mcpserver`: read-only MCP tools for coding agents. `skills/`: the agent skills, embedded in the CLI.
- `internal/brief`: a problem written up as a prompt for a coding agent (a query, an endpoint, a trace, a log line), behind the app's "Send to Shipyard" and `leadline brief`. `internal/discover` finds Shipyard's command line, lists its boxes and, in `task.go`, starts a task: the one thing Leadline does outside itself. `internal/brief/API.md` describes the routes; the app's dialog is built from it.
- `internal/demo`: made-up "acme" telemetry, for trying the app and as the test fixture.
- `packages/node`: `@leadline/node`, the provider a Node.js app loads to send traces and logs. `packages/next`: `@leadline/next`, the same for Next.js, with the browser's side. `examples/`: small apps wired to them.
- `app/`: Tauri 2 + React 19 + Tailwind 4. Charts are uPlot on canvas (`components/Chart.tsx`). The page talks to the daemon over HTTP, so it also runs in a browser tab. The desktop app carries the daemon and starts it (`scripts/sidecar.sh` builds that copy).
- `site/`: the landing page, in Astro (`cd site && pnpm dev`, port 4390). Its one picture, `public/hero.png`, is made by `design/hero/dither.py` from `design/hero/source/`; `src/scripts/engraving.ts` makes its dots move.
- `Makefile`: `make dev` (the app from source), `make build` (both binaries into `bin/`), `make install` (both into `~/.local/bin`), `make check` (every check below), `make app` (the bundled app), `make demo`, `make linux`, `make release`, `make release-check`, `make publish`, `make rehearse` and `make updater-test` (below).
- `scripts/`: `install.sh`, the installer for a Linux machine, and `install_test.sh`, its test; `build-linux.sh` and `linux-builder.Dockerfile`, the Linux binaries; `release.sh`, a release's tarballs; `sidecar.sh`, the daemon the app carries; `version.sh`, the version in its four places. For a signed release: `mac-release.sh` (the app), `notarize.sh`, `publish.sh` (a release from this Mac), `release-check.sh`, `updater-key.sh`, `ci-secrets.sh` and `ci-keychain.sh` (what CI signs with), `op.sh` (reading 1Password). `release-test/` holds their tests and the stand-ins those run against. "Versions and releases" below says what each is for.

## How data flows

An app exports OTLP and `ingest` flattens it to `store.Span`, `store.Log` and `store.MetricPoint`. The Postgres proxy and the Redis capture make spans of their own. All of them go to the store with `AddSpans` and friends, which only puts the rows in memory: one goroutine, the flusher, writes what is waiting a moment later (within 200 ms, and before the next query runs, so a read always sees what was added). The API's hub then wakes open event streams and the app refetches what it shows (`lib/live.ts`, at most once a second).

As it writes, the store settles what each span counts as, in two columns every aggregate filters on (`internal/store/link.go`):

- `entry`: the span is a request. A server or consumer span, or a span with no parent; not a span the browser made, not a server span inside a server span of the same service, not a statement a capture recorded. The overview and the endpoints count the same spans.
- `db_role`: `1` a query, `2` transaction and session control (`BEGIN`, `COMMIT`, `SET`, Redis `PING`: in the trace and in database time, not among the queries), `3` a second sighting of a call another span already counts.

An app can be seen two ways at once: its own OpenTelemetry spans, and the proxy recording the same statements off the wire. The store pairs the two (same system, same fingerprint, the wire's timing inside the app's span) so each statement is one query: the proxy's span moves into the app's trace, under the app's span, and is the one counted.

A database call is a span with `db_system` set; "queries" are the spans with `db_role = 1`, grouped by `db_fingerprint`.

Times are unix nanoseconds in the store and milliseconds (floats) in the API: a nanosecond timestamp doesn't fit a JS number.

## Checks

`make check` runs all of these, and CI runs the same:

```sh
go vet ./... && go test -race ./...
scripts/install_test.sh                              # the installer, on a made-up release in a temporary home
scripts/version.sh                                   # the four places the version is written say the same
scripts/release-test/credentials_test.sh             # the scripts that handle a release's credentials, against stand-ins
scripts/sidecar.sh                                   # the daemon the app carries; cargo needs it to exist
(cd app/src-tauri && cargo check && cargo test)
(cd app && CI=true pnpm install --frozen-lockfile && pnpm test && pnpm build)
(cd packages/node && CI=true pnpm install --frozen-lockfile && pnpm test)
```

To see it: `make dev`, then `go run ./cmd/leadline demo`. Or by hand: `go run ./cmd/leadlined`, and `cd app && pnpm dev` (http://localhost:1520).

After a change to a query, measure it: `go run ./cmd/leadline bench -db /tmp/bench.duckdb` fills a file with five million spans and times every question the app asks. Run it before and after, back to back; the file is large, so delete it when done.

CI also runs `shellcheck scripts/*.sh scripts/release-test/*.sh scripts/release-test/standins/*`, and `actionlint` is worth running over `.github/workflows` when you change one; run them when you change a script.

## Versions and releases

The project has one version, the `VERSION` file (`0.2.0`, no `v`). The Makefile and the scripts stamp it into both programs as they build (`internal/version`, by `-ldflags -X`), with the commit: `leadline version`, `leadlined --version` and `"version"` in `GET /api/health` say it. A plain `go build` or `go run` says `dev`. The desktop app has a version of its own in `app/package.json`, `app/src-tauri/tauri.conf.json` and `app/src-tauri/Cargo.toml`; the four are kept the same, and `scripts/version.sh` does it: with a version it sets all four (and `Cargo.lock`), with nothing it prints the version and fails if they differ.

- **`make install`** puts `leadline` and `leadlined` in `~/.local/bin` (`PREFIX=` changes where). Two files and nothing else: no service, no data.
- **`make linux`** builds the Linux programs, amd64 and arm64, into `dist/linux_<arch>/`, in a container (`scripts/build-linux.sh`). The container is AlmaLinux 8, where DuckDB builds the static library the daemon links, so the programs need glibc 2.28 or newer and nothing else of the machine: the C++ runtime is linked in. The script fails if either stops being true. Each architecture builds on its own kind of processor; the other one runs under emulation, which Docker has to have turned on (the script says how). An amd64 program does not *run* under qemu without `--ulimit stack=-1` (DuckDB's allocator and qemu's addresses), which is only about testing on a Mac.
- **`make release`** makes `dist/leadline_<version>_<os>_<arch>.tar.gz` for Linux and macOS, both architectures of each, and `dist/checksums.txt` (`scripts/release.sh`; name targets to build fewer). Each tarball holds the two programs. The macOS ones are signed and notarized when the environment says how (below), and the script says so when they are not.
- **`scripts/install.sh`** installs a release on a Linux machine: both programs into `~/.local/bin` and the daemon as a systemd user service (the unit is written by the script, so it is the place to change it). It installs nothing it could not check against `checksums.txt` or `--sha256`. Test a change to it with `scripts/install_test.sh`, and for real with `sh scripts/install.sh --from dist/leadline_<version>_linux_<arch>.tar.gz` in a container that runs systemd.

### What a release carries

A release is a GitHub release of the tag `v<version>`:

| File | Made by | For |
| --- | --- | --- |
| `leadline_<version>_linux_<arch>.tar.gz` | `scripts/release.sh` | A server: `install.sh` |
| `leadline_<version>_darwin_<arch>.tar.gz` | `scripts/release.sh` | The command line on a Mac, signed and notarized |
| `checksums.txt`, `install.sh` | `scripts/release.sh`, as it is | The installer checks its download against the first |
| `Leadline-macos-universal.dmg` | `scripts/mac-release.sh` | The desktop app: the download |
| `Leadline-macos-universal.app.tar.gz`, `.sig` | `scripts/mac-release.sh` | The app's updater: the new app and its signature |
| `latest.json` | `scripts/mac-release.sh` | The feed installed apps update from |

The app's files carry no version in their names, so `https://github.com/sean-brydon/leadline/releases/latest/download/<name>` is always the newest.

### How it is signed

Everything for a Mac is signed with the Developer ID Application certificate, with the hardened runtime and a secure timestamp, and notarized by Apple with an App Store Connect API key. `scripts/mac-release.sh` (the app) and `scripts/release.sh` (the two programs) do it, and `scripts/notarize.sh` is the one place that talks to Apple's notary.

- **One app for both kinds of Mac.** The app is universal: a person never has to know which processor they have, the feed has one archive, and every Mac that can run the daemon at all (macOS 13 on) is covered. It costs a larger download (a dmg of 52 MB, against 25 for Apple silicon alone), since the app then holds the daemon twice and DuckDB is most of the daemon. `scripts/sidecar.sh universal-apple-darwin` builds the daemon for each and joins them.
- **No entitlements, on the app or on the daemon inside it.** The hardened runtime takes away four things unless an entitlement gives them back: memory that is both written and run (a JIT), loading code signed by someone else, `DYLD_` variables, and being debugged. The daemon is Go with DuckDB linked in, and uses none of them: DuckDB compiles nothing at run time, and everything of it that Leadline uses is linked into the daemon (the JSON functions among it). This was checked, not assumed: the Go tests of every package that opens DuckDB (`store`, `ingest`, `api`, `integrations`, `pgproxy`, `mcpserver`, `brief`) were built as test programs, signed with the hardened runtime and no entitlement, and passed; and every release starts the signed daemon and asks it (`check_daemon` in `scripts/mac-release.sh`). What the runtime does refuse is `LOAD` of a DuckDB extension that is downloaded rather than linked in (`dlopen` fails library validation). If Leadline ever needs one, the entitlement is `com.apple.security.cs.disable-library-validation`, on the daemon alone, and `mac-release.sh`, which fails a release whose programs carry any entitlement, is where to say so. The app's own program is a window around the system's web view, which runs pages in a process of Apple's.
- **The app and its dmg carry Apple's ticket** (stapled), so Gatekeeper opens them on a Mac that is offline. A bare program has nowhere to carry one: the first time `leadline` from a tarball runs, Gatekeeper asks Apple. Online that takes a moment, once. Offline, a copy downloaded with a browser (and so marked as quarantined) is refused until the Mac has been online; a copy fetched with `curl` has no such mark and runs.
- **The checks are part of the build.** `mac-release.sh` fails unless `codesign --verify --deep --strict`, `spctl` (which must say "Notarized Developer ID") and `stapler validate` all pass, on the app, on the app inside the dmg and on the app inside the updater's archive; unless the app and its daemon both have the hardened runtime, both architectures and no entitlements; unless the daemon starts as signed and says this version; and unless the updater's signature verifies against the public key in `tauri.conf.json` and names this version.

### How the app updates itself

The app carries Tauri's updater, driven from Rust (`app/src-tauri/src/update.rs`): the page has no permission to it, and is only told when an update is ready. Ten seconds after it opens, and every four hours, the app reads `latest.json` from the latest release. A newer version is downloaded and its signature checked against `plugins.updater.pubkey`; the signature must also name the version it is announced as (`requireSignedVersion`), so an old archive can't be served as a new one. Then the bar shows **Restart to update**, and nothing else happens until that is pressed: the new app replaces the old one where it is, the app quits, which stops the daemon it started, and opens again, which starts the daemon the new app carries. A daemon somebody else started (`leadlined` in a terminal) is left running, as always, and stays the version it was until that person restarts it.

A copy of the app looks for no update when it is a development build, when it was built without the public key (`make app` before the key exists), when it runs from a place that can't be written to (straight from the dmg), or with `LEADLINE_UPDATES=off`. (`LEADLINE_UPDATES=install` takes an update the moment it is ready, without the press: that is for `scripts/release-test/updater-test.sh`.)

The key pair is made once, with `scripts/updater-key.sh`: the public half goes into `tauri.conf.json` and is committed; the private half goes into 1Password and nowhere else. **It is never made again.** Every installed app trusts the public key it was built with and no other, so a second pair, or losing the private half, leaves all of them unable to update. Until the pair exists the config holds a placeholder, and a release build refuses to start (`build.rs`, and `mac-release.sh` before it).

### Once, before the first release

1. **The updater key:** `scripts/updater-key.sh`. It makes the pair, puts the private half in 1Password at `op://Personal/Leadline updater signing key/private key`, writes the public half into `app/src-tauri/tauri.conf.json`, and says what to commit. (`--file PATH` writes the private half to a file instead, for you to store and delete.)
2. **The Apple item in 1Password**, `op://Personal/Berth Developer Id`, which Shipyard's releases already read: the Developer ID and the notary key belong to the Apple account, not to a product. A release from this Mac reads its `Issuer ID`, `Key ID` and the file `AuthKey_<Key ID>.p8`, and signs with the Developer ID Application identity in the login keychain. A release from CI needs two more things in that item, since a runner has no keychain: the certificate with its private key, exported from Keychain Access as a `.p12` and attached as the file `DeveloperID.p12`, and that file's password in a field `p12 password`.
3. **The repository's secrets:** `scripts/ci-secrets.sh --dry-run`, then without `--dry-run`. It reads the six values from 1Password, tries the certificate in a keychain of its own, lists what it will set, asks, and sets `APPLE_CERTIFICATE`, `APPLE_CERTIFICATE_PASSWORD`, `APPLE_API_ISSUER`, `APPLE_API_KEY`, `APPLE_API_KEY_P8` and `TAURI_SIGNING_PRIVATE_KEY` with `gh secret set`, each on standard input.

`LEADLINE_APPLE_OP_ITEM`, `LEADLINE_UPDATER_KEY_OP`, `LEADLINE_CERTIFICATE_OP` and `LEADLINE_CERTIFICATE_PASSWORD_OP` name other places in 1Password. Nothing is needed in Apple's portal beyond what Shipyard's releases use: the Developer ID Application certificate and an App Store Connect API key that may notarize. An app distributed outside the App Store needs no identifier registered there.

### To release

1. `scripts/version.sh 0.3.0`, and commit `release: v0.3.0`, on main.
2. `make check`, then `make release-check` with Leadline quit: it runs the checks again, builds the app, takes it out of its dmg into a folder of its own and opens it there on a data folder of its own. Its daemon must start, answer as this version, and stop when the app quits, and again when the app is killed. Try a Linux tarball when the installer or the daemon's start changed: `sh scripts/install.sh --from dist/…` on a Linux machine or in a container.
3. Either tag that commit `v0.3.0` and push the tag, and `.github/workflows/release.yml` builds, signs, notarizes, checks and publishes everything; or `make publish`, which builds the macOS half on this Mac, with the Developer ID in its keychain and the rest from 1Password, opens the app it built, puts it on GitHub as a draft, and pushes the tag itself, after which the workflow adds the Linux half and publishes. Either way the release is a draft until its last file is there, the workflow runs all of CI first, and it refuses a tag that isn't `VERSION`'s or a signed release whose secrets are missing, in its first minute.

To rehearse: `make rehearse` is a whole release from this Mac with stand-ins for every credential and for Apple, and nothing pushed (`scripts/release-test/`); `make updater-test` watches an installed app update itself to the next version from a feed on this machine; and the workflow run by hand from the Actions tab builds, signs and notarizes for real and publishes nothing. `.claude/skills/release/SKILL.md` has the commands for after the tag, and for a release that failed halfway.

## Rules

- **Ports:** the app's dev server is 1520 and the daemon 4320. Never use or kill 1420 or 1377-1379 (another app's). Something else often holds 4318; the daemon copes, tests must not need it. For a second daemon or dev server pick a free port (`leadlined -addr 127.0.0.1:43xx -data :memory: -otlp ""`, `vite --port 15xx`) and check it is free first. Kill only PIDs you started.
- **Launching the app starts a real daemon.** `pnpm tauri dev`, the bundled app and the Rust tests run `leadlined` on the user's own data unless told otherwise. Anything that starts one sets `LEADLINE_DATA` to a temporary folder (and `LEADLINE_ADDR` to a free port), so a test never writes to `~/.leadline`. The app's own daemon is always on 4320, so a bundled app that must not meet a running Leadline is built for another port: `LEADLINE_APP_ADDR=127.0.0.1:43xx` and `VITE_LEADLINE_URL=http://127.0.0.1:43xx` in the build's environment (and another bundle identifier, so it shares no settings), as `scripts/release-test/rehearse.sh` does. A release is never built that way: `scripts/mac-release.sh` drops both.
- **Localhost only:** the daemon holds SQL and logs. It binds loopback, and `guard` refuses foreign Host and Origin headers. Keep new endpoints behind it.
- **The proxy must never break the app.** It relays bytes whatever happens: if it meets something it doesn't understand it stops reading and carries on relaying, and nothing it does to record a statement may delay or change one. The same goes for `leadline run`: the command first, Leadline second.
- **Tests need no network and no Docker.** Use `store.Open("")` (in memory), `internal/demo`, and the fake servers in `internal/integrations/fake`. Always `-race`. Measurements over millions of rows belong to `leadline bench`, not `go test`.
- **Fixtures are synthetic** ("acme"). Nothing from a real company or database goes in the repository.
- **A release's credentials stay where they are kept.** The release scripts read them from the environment or from 1Password, hand them on in an environment or on standard input, never as an argument, print none, and remove what they wrote when they exit, however they exit. Nothing in the repository's tests reads a real one: `scripts/release-test/standins` has stand-ins for `op`, `gh`, Apple's notary and its timestamps, and `scripts/release-test/credentials_test.sh` holds the scripts to this.
- **No passwords out.** A database URL has one. Nothing that leaves `internal/integrations` or `internal/runner` carries it: not a response, not a log line, not a span.
- **API responses only grow.** The app is written against the JSON as it is. Add fields; don't rename or remove them, and don't change what one means.
- **New columns go last.** A database file from an older version gets a new column by `ALTER`, which adds at the end, and the appenders write by position. Add the column at the end of the `CREATE TABLE` and to `columnsAdded` in `internal/store/link.go`.
- **DuckDB types:** `sum` of a BIGINT is a HUGEINT, which the Go driver scans as `*big.Int`. Divide to a DOUBLE or cast before scanning. Appender values must match the column type exactly (`int64` for BIGINT, `int32` for INTEGER, `int8` for TINYINT).
- **New unit test files in the app** go into `app/package.json`'s `"test"` script list.
- **Shipyard is only read, except when a person acts.** Leadline runs seven `berth` commands: four that read (`boxes`, `locations`, `agents`, `forwards`), `task new` (only `POST /api/shipyard/tasks`), and `forward` / `unforward` (only `POST /api/shipyard/forwards` and `/close`, when a person adds, uses or removes a machine). Those three routes are for Leadline's own window. `LEADLINE_FORWARD_PORTS=FIRST-LAST` moves the range a forward's local port is picked from. No test runs a real `berth`: use `internal/discover/fakeberth`, and start a daemon you drive by hand with `LEADLINE_BERTH` set to that fake, or to `off`.
- **A brief holds no values unless asked.** Statements, log lines and error messages go in as shapes; `example=1` is the only way a value gets into a prompt, and `recorded` says which lines hold one.
- **MCP results stay small:** an agent's context is scarce. Default to short lists and a few buckets, and put anything large behind an argument.
- **Commit messages** are lowercase conventional (`feat(app): …`, `fix(daemon): …`). The subject says what the user gets.
- Comments say why, in plain words. Match the density of the file you are in.
