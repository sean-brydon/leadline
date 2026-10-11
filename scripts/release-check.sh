#!/usr/bin/env bash
# What to be sure of before a tag, on a Mac: `make release-check`.
#
#   scripts/release-check.sh [--dmg PATH] [--built] [--no-checks]
#
#   1. The four places the version is written agree (scripts/version.sh).
#   2. Everything `make check` runs, and shellcheck over the scripts.
#   3. The app, as a person gets it: taken out of the dmg into a folder of
#      its own and opened there on a data folder of its own. Its daemon must
#      start, be the one inside that app, answer as this version, keep its
#      data where it was told, and stop when the app quits. Then again with
#      the app killed outright, where the watchdog must stop it.
#   4. The updater's feed, when a release build made one (dist/mac/): its
#      version, where it points, and the archive's signature against the
#      public key the app is built with.
#
# Without --dmg it builds the app as `make app` does (signed ad hoc, no
# feed) and tries that. --built takes what scripts/mac-release.sh left in
# dist/mac/ instead, skips step 2 (scripts/publish.sh runs it this way, on
# a commit the checks already passed for), and checks the feed. A dmg that
# claims a Developer ID is also held to it: Gatekeeper must call it
# notarized and its ticket must be stapled. --no-checks skips step 2.
#
# The app's daemon has one port, 4320, so Leadline must not be running: this
# says so and stops rather than try the app against somebody's own daemon.
# Nothing here reads or writes ~/.leadline, or what the installed app's
# window remembers. The window does appear, for a few seconds, twice.
#
# A rehearsal's app is built for another port and identifier
# (scripts/release-test/rehearse.sh); LEADLINE_APP_ADDR says which port.

# shellcheck disable=SC2329 # each check is a function that `run` is given by name
set -euo pipefail

dmg="" built="" checks=1
while [ $# -gt 0 ]; do
  case "$1" in
    --dmg) dmg="${2:?--dmg needs a path}"; shift 2 ;;
    --built) built=1; checks=0; shift ;;
    --no-checks) checks=0; shift ;;
    -h | --help) sed -n '2,/^set -euo/p' "$0" | sed -e '$d' -e 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "release-check: unknown option $1 (try --help)" >&2; exit 2 ;;
  esac
done

repo="sean-brydon/leadline"
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
[ "$(uname -s)" = Darwin ] || { echo "release-check: the app is a Mac's, so this runs on a Mac (CI runs the rest: make check)" >&2; exit 1; }

rehearsal="${LEADLINE_REHEARSAL:-}"
addr="${LEADLINE_APP_ADDR:-127.0.0.1:4320}"
[ -n "$rehearsal" ] || addr=127.0.0.1:4320
port="${addr##*:}"
# The names of what a release carries of the app (scripts/mac-release.sh).
asset="Leadline-macos-universal"

work="$(mktemp -d "${TMPDIR:-/tmp}/leadline-release-check.XXXXXX")"
work="$(cd "$work" && pwd -P)"
installed="$work/Applications/Leadline.app"
data="$work/data"
mkdir -p "$work/home"
app_pid=""
cleanup() {
  # Only what this started: the app, and through it its daemon.
  if [ -n "$app_pid" ] && kill -0 "$app_pid" 2>/dev/null; then
    kill "$app_pid" 2>/dev/null || true
    sleep 6
  fi
  pkill -f "^$installed/Contents/MacOS/" 2>/dev/null || true
  if [ -d "$work/mnt" ]; then hdiutil detach -quiet "$work/mnt" 2>/dev/null || true; fi
  # What a rehearsal's app left in the user's temporary folder, under its
  # own port and its own name. (The real app's are the real app's.)
  if [ -n "$rehearsal" ] && [ "$port" != 4320 ]; then
    rm -rf "$(getconf DARWIN_USER_TEMP_DIR)leadline-$port.lock" "$(getconf DARWIN_USER_TEMP_DIR)io.brydon.leadline.rehearsal"
  fi
  rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP

results=()
failed=0
# run NAME COMMAND…: one check; the rest still run after a failure.
run() {
  local name="$1"
  shift
  printf '\n=== %s\n' "$name"
  if "$@"; then
    results+=("pass  $name")
  else
    results+=("FAIL  $name")
    failed=1
    return 1
  fi
}
say() { echo "release-check: $*" >&2; }

version="$(scripts/version.sh)" || exit 1
echo "=== Leadline $version"

checks() {
  make check || return 1
  if command -v shellcheck >/dev/null; then
    shellcheck scripts/*.sh scripts/release-test/*.sh scripts/release-test/standins/* || return 1
  else
    say "shellcheck is not installed (brew install shellcheck), so the scripts were not checked; CI checks them"
  fi
}

# find_dmg: the dmg to try. Sets dmg.
find_dmg() {
  if [ -n "$dmg" ]; then
    [ -f "$dmg" ] || { say "there is no $dmg"; return 1; }
  elif [ -n "$built" ]; then
    dmg="dist/mac/$asset.dmg"
    [ -f "$dmg" ] || { say "there is no $dmg: scripts/mac-release.sh builds it"; return 1; }
  else
    say "building the app as \`make app\` does (log: $work/app-build.log)"
    local bundle="${CARGO_TARGET_DIR:-app/src-tauri/target}/release/bundle/dmg"
    rm -f "$bundle"/*.dmg
    make app >"$work/app-build.log" 2>&1 || { tail -20 "$work/app-build.log"; say "building the app failed"; return 1; }
    local dmgs=("$bundle"/*.dmg)
    [ -f "${dmgs[0]}" ] || { say "make app left no dmg in $bundle"; return 1; }
    dmg="${dmgs[0]}"
  fi
  echo "the dmg: $dmg"
}

# install: the app out of the dmg, the way dragging it to Applications
# copies it, into a folder that is nobody's Applications.
install() {
  mkdir -p "$work/mnt" "$work/Applications"
  hdiutil attach -quiet -nobrowse -readonly -mountpoint "$work/mnt" "$dmg" || { say "the dmg does not open"; return 1; }
  [ -d "$work/mnt/Leadline.app" ] || { say "the dmg holds no Leadline.app"; return 1; }
  [ -L "$work/mnt/Applications" ] || { say "the dmg has no Applications link to drag Leadline to"; return 1; }
  ditto "$work/mnt/Leadline.app" "$installed"
  hdiutil detach -quiet "$work/mnt"
  local info v said
  v="$(plutil -extract CFBundleShortVersionString raw "$installed/Contents/Info.plist")"
  [ "$v" = "$version" ] || { say "the app in the dmg is $v, and VERSION says $version: the dmg is from another build"; return 1; }
  said="$("$installed/Contents/MacOS/leadlined" --version)" || { say "the daemon in the app does not start"; return 1; }
  [[ "$said" == "leadlined $version"* ]] || { say "the daemon in the app says \"$said\", not version $version"; return 1; }
  info="$(codesign -dv --verbose=2 "$installed" 2>&1 || true)"
  if grep -q "^Authority=Developer ID Application:" <<<"$info"; then
    # It claims to be a release, so it must be all of one.
    codesign --verify --deep --strict "$installed" || { say "the app's signature does not verify"; return 1; }
    xcrun stapler validate "$installed" >/dev/null || { say "the app is signed with a Developer ID but has no stapled notarization ticket"; return 1; }
    said="$(spctl -a -vv -t exec "$installed" 2>&1)" || { say "Gatekeeper rejects the app: $said"; return 1; }
    grep -q "source=Notarized Developer ID" <<<"$said" || { say "Gatekeeper accepts the app but not as notarized: $said"; return 1; }
    echo "signed with a Developer ID, notarized, and the ticket is stapled"
  elif [ -n "$built" ] && [ -z "$rehearsal" ] && [ ! -e dist/mac/NOT-A-RELEASE ]; then
    say "the app in dist/mac is not signed with a Developer ID, and nothing marks it as not for release"
    return 1
  else
    echo "not a Developer ID build ($(grep -E '^(Signature|Authority)=' <<<"$info" | head -1 || echo unsigned)): for trying, not for releasing"
  fi
  echo "installed in a folder of its own: $v, daemon $said"
}

# The daemon the app started: its child called leadlined.
daemon_of() { pgrep -P "$1" -x leadlined | head -1; }
health() { curl -fsS -m 2 "http://$addr/api/health" 2>/dev/null; }
gone() { # gone PID SECONDS: whether the process has exited by then
  local n=0
  while kill -0 "$1" 2>/dev/null; do
    [ "$n" -lt "$(($2 * 5))" ] || return 1
    sleep 0.2
    n=$((n + 1))
  done
}

# open_app: starts the installed app, as Finder would but with its data in
# the test's folder, and waits for its daemon. Sets app_pid and daemon_pid.
open_app() {
  local holder answer="" n=0
  holder="$(lsof -nP -iTCP:"$port" -sTCP:LISTEN -Fc 2>/dev/null | sed -n 's/^c//p' | head -1)"
  if [ -n "$holder" ]; then
    case "$holder" in
      leadlined) say "Leadline is running on this Mac (its daemon has port $port). Quit it for a minute and run this again: the check opens the app being released, with a data folder of its own, and can't while another daemon has the port." ;;
      *) say "another program ($holder) is listening on port $port, which the app's daemon needs. Stop it and run this again." ;;
    esac
    return 1
  fi
  # No second listener for OpenTelemetry's port, no looking for updates
  # (this copy must stay the one being tested), no Shipyard. And a home of
  # its own for what the window keeps (CFFIXED_USER_HOME is where macOS
  # tells an app its home is): the app being tried has the installed app's
  # name, and would otherwise read and write the installed app's settings.
  CFFIXED_USER_HOME="$work/home" LEADLINE_DATA="$data" LEADLINE_OTLP_ADDR=off LEADLINE_UPDATES=off LEADLINE_BERTH=off \
    "$installed/Contents/MacOS/leadline" >>"$work/app.log" 2>&1 &
  app_pid=$!
  # Not a job of this shell's to report on: one of these is killed on purpose.
  disown "$app_pid"
  until answer="$(health)"; do
    kill -0 "$app_pid" 2>/dev/null || { say "the app exited as it started: $(tail -5 "$work/app.log")"; return 1; }
    [ "$n" -lt 150 ] || { say "the app's daemon did not answer on $addr within 30 seconds: $(tail -5 "$data/leadlined.log" 2>/dev/null)"; return 1; }
    sleep 0.2
    n=$((n + 1))
  done
  [[ "$answer" == *'"name":"leadline"'* && "$answer" == *"\"version\":\"$version\""* ]] ||
    { say "what answers on $addr is not this version's daemon: $answer"; return 1; }
  daemon_pid="$(daemon_of "$app_pid")"
  [ -n "$daemon_pid" ] || { say "a daemon answers on $addr, but the app did not start it"; return 1; }
  case "$(ps -o command= -p "$daemon_pid")" in
    "$installed/Contents/MacOS/leadlined "*) ;;
    *) say "the app's daemon is not the one it carries: $(ps -o command= -p "$daemon_pid")"; return 1 ;;
  esac
}

runs_and_quits() {
  open_app || return 1
  echo "the app started the daemon it carries (pid $daemon_pid), and it answers as $version"
  curl -fsS -m 5 "http://$addr/api/summary" >/dev/null || { say "the daemon does not answer /api/summary"; return 1; }
  [ -f "$data/leadline.duckdb" ] || { say "the daemon's database is not in the folder it was given ($data)"; return 1; }
  echo "its data is where it was told to keep it"
  # Quit, as the menu's Quit does: the app is asked, and stops its daemon
  # on the way out.
  osascript -l JavaScript -e "ObjC.import('AppKit'); \$.NSRunningApplication.runningApplicationWithProcessIdentifier($app_pid).terminate" >/dev/null ||
    { say "could not ask the app to quit"; return 1; }
  gone "$app_pid" 20 || { say "the app did not quit within 20 seconds of being asked"; return 1; }
  # The app waits for its daemon before it goes, so the daemon is gone now.
  ! kill -0 "$daemon_pid" 2>/dev/null || { say "the app quit and left its daemon running (pid $daemon_pid)"; return 1; }
  app_pid=""
  # A daemon that was let finish leaves no write-ahead log behind.
  [ ! -e "$data/leadline.duckdb.wal" ] || { say "the daemon stopped without closing its database (leadline.duckdb.wal is still there)"; return 1; }
  echo "quitting the app stopped the daemon, and the database was closed properly"
}

dies_and_is_cleaned_up() {
  open_app || return 1
  # No goodbye: a crash, a force quit. The watchdog is all that is left.
  kill -9 "$app_pid"
  app_pid=""
  gone "$daemon_pid" 15 || { say "the app was killed and its daemon (pid $daemon_pid) is still running: the watchdog did not stop it"; return 1; }
  echo "killing the app outright still stopped the daemon (the watchdog)"
}

# feed: latest.json and what it announces, as mac-release.sh left them.
feed() {
  local dir=dist/mac pubkey overlay=""
  [ -z "$rehearsal" ] || overlay="${LEADLINE_REHEARSAL_CONFIG:-}"
  command -v minisign >/dev/null || { say "minisign is needed to check the updater's signature: brew install minisign"; return 1; }
  pubkey="$(python3 - "$overlay" <<'PY'
import json, sys
key = json.load(open("app/src-tauri/tauri.conf.json"))["plugins"]["updater"]["pubkey"]
if sys.argv[1]:
    key = json.load(open(sys.argv[1])).get("plugins", {}).get("updater", {}).get("pubkey", key)
print(key)
PY
)" || return 1
  [ "$pubkey" != UPDATER-PUBLIC-KEY-NOT-SET ] || { say "tauri.conf.json has no updater public key yet (scripts/updater-key.sh makes the pair)"; return 1; }
  python3 - "$dir" "$asset" "$version" "$repo" <<'PY' || return 1
import json, sys
folder, name, version, repo = sys.argv[1:5]
feed = json.load(open(f"{folder}/latest.json"))
sig = open(f"{folder}/{name}.app.tar.gz.sig").read().strip()
url = f"https://github.com/{repo}/releases/download/v{version}/{name}.app.tar.gz"
def fail(why):
    print(f"release-check: latest.json: {why}", file=sys.stderr)
    sys.exit(1)
if feed.get("version") != version:
    fail(f"it announces {feed.get('version')}, and VERSION says {version}")
for kind in ("darwin-aarch64", "darwin-x86_64"):
    entry = feed.get("platforms", {}).get(kind)
    if not entry:
        fail(f"it has nothing for {kind}, so those Macs would never update")
    if entry.get("signature") != sig:
        fail(f"{kind}'s signature is not the archive's ({name}.app.tar.gz.sig)")
    if entry.get("url") != url:
        fail(f"{kind} points at {entry.get('url')}, not at this release's archive ({url})")
print(f"latest.json announces {version} for both kinds of Mac, at {url}")
PY
  base64 -d <<<"$pubkey" >"$work/updater.pub" || { say "the updater's public key is not base64"; return 1; }
  base64 -d <"$dir/$asset.app.tar.gz.sig" >"$work/updater.minisig"
  minisign -Vq -p "$work/updater.pub" -x "$work/updater.minisig" -m "$dir/$asset.app.tar.gz" ||
    { say "the archive's signature does not verify against the public key the app is built with: installed apps would refuse this update"; return 1; }
  echo "the archive's signature verifies against the public key in the app's config"
  # And the app being released is built with that key: the key is in its
  # program as text. An app with another key could take no update signed
  # with this one.
  if [ -d "$installed" ]; then
    LC_ALL=C grep -q -a -F -e "$pubkey" "$installed/Contents/MacOS/leadline" ||
      { say "the app that was installed does not carry that public key, so it was built from another config"; return 1; }
    echo "the app carries that same key"
  fi
}

[ "$checks" = 0 ] || run "The checks (make check, shellcheck)" checks || true
if run "The app, out of its dmg" find_dmg && run "Installed in a folder of its own" install; then
  run "It starts its daemon, and quitting stops it" runs_and_quits || true
  run "Killed outright, its daemon is still stopped" dies_and_is_cleaned_up || true
fi
# Only a release build has a feed. One left in dist/mac by an earlier build
# says nothing about the app `make app` just made.
[ -z "$built" ] || run "The updater's feed" feed || true

echo
echo "Release checks of Leadline $version:"
for r in "${results[@]}"; do echo "  $r"; done
exit "$failed"
