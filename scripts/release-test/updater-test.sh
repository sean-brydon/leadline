#!/usr/bin/env bash
# Watches an installed app update itself, for real, on this Mac alone.
#
#   scripts/release-test/updater-test.sh [--keep]     (make updater-test)
#
# It builds the app twice with scripts/mac-release.sh, as this version and
# as the next one, signed with the stand-ins a rehearsal uses (a certificate
# and an updater key made here; scripts/release-test/lib.sh). It installs
# the first in a folder of its own, serves the second's archive and feed
# from a port on this machine, opens the first, and waits. Then it must be
# true that:
#
#   - the app found the feed, downloaded the archive and took it (the
#     archive's signature was checked against the key the app was built with)
#   - the app in that folder is now the next version, and is running
#   - its daemon is the next version too, running out of the new app, on the
#     same data folder as before
#   - the old app and the old daemon are gone
#   - quitting the new app stops the new daemon
#
# Nothing is sent anywhere, and nothing of the Leadline you use is touched:
# the app it builds has another bundle identifier, another port for its
# daemon, and looks for its updates on localhost. Two builds take a while.
#
# The second build needs the next version in the four places it is written,
# so this sets it (scripts/version.sh) and sets it back, whatever happens.
#
# REHEARSAL_PORT and FEED_PORT name the two ports (default: ones free now).
# --keep leaves the work folder: both builds' output, the app's and the
# feed's logs.
set -euo pipefail

keep=""
case "${1:-}" in
  "") ;;
  --keep) keep=1 ;;
  *) echo "usage: scripts/release-test/updater-test.sh [--keep]" >&2; exit 2 ;;
esac

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
cd "$root"

die() { echo "updater-test: $*" >&2; exit 1; }
step() { printf '\n=== %s\n' "$*"; }

[ "$(uname -s)" = Darwin ] || die "the app is a Mac's, so this runs on a Mac"
command -v minisign >/dev/null || die "minisign is needed to check the updater's signature: brew install minisign"
version="$(scripts/version.sh)" || exit 1
next="${version%.*}.$((${version##*.} + 1))"
rehearsal_id=io.brydon.leadline.rehearsal

work="$(mktemp -d "${TMPDIR:-/tmp}/leadline-updater-test.XXXXXX")"
work="$(cd "$work" && pwd -P)"
chmod 700 "$work"
# shellcheck source=scripts/release-test/lib.sh
. "$here/lib.sh"
installed="$work/Applications/Leadline.app"
server="" bumped=""
versioned=(VERSION app/package.json app/src-tauri/tauri.conf.json app/src-tauri/Cargo.toml app/src-tauri/Cargo.lock)
before="$(cat "${versioned[@]}" | shasum -a 256)"
cleanup() {
  # Only what this started: whatever runs out of the folder it installed
  # into, and the feed's server.
  pkill -f "^$installed/Contents/MacOS/" 2>/dev/null || true
  [ -z "$server" ] || kill "$server" 2>/dev/null || true
  if [ -d "$work/mnt" ]; then hdiutil detach -quiet "$work/mnt" 2>/dev/null || true; fi
  if [ -n "$bumped" ]; then
    scripts/version.sh "$version" >/dev/null 2>&1 || echo "updater-test: could not set the version back to $version: scripts/version.sh $version" >&2
    [ "$(cat "${versioned[@]}" | shasum -a 256)" = "$before" ] || echo "updater-test: the version files are not as they were: look at git diff" >&2
  fi
  standins_remove
  # What the test app's window kept, under its own name and nobody else's.
  rm -rf "$HOME/Library/WebKit/$rehearsal_id" "$HOME/Library/Caches/$rehearsal_id" "$(getconf DARWIN_USER_TEMP_DIR)$rehearsal_id"
  [ -z "${port:-}" ] || rm -f "$(getconf DARWIN_USER_TEMP_DIR)leadline-$port.lock"
  if [ -n "$keep" ]; then echo "updater-test: kept $work"; else rm -rf "$work"; fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP

step "Stand-ins"
standin_identity
standin_updater_key
port="${REHEARSAL_PORT:-$(free_port)}"
feed_port="${FEED_PORT:-$(free_port)}"
addr="127.0.0.1:$port"
python3 - "$work/updater.key.pub" "$work/overlay.json" "$feed_port" "$rehearsal_id" <<'PY'
import json, sys
pub, out, feed_port, identifier = sys.argv[1:5]
json.dump({
    "identifier": identifier,
    "plugins": {"updater": {
        "pubkey": open(pub).read().strip(),
        "endpoints": [f"http://127.0.0.1:{feed_port}/latest.json"],
        # Only for this: a release's feed is https, and the app refuses less.
        "dangerousInsecureTransportProtocol": True,
    }},
}, open(out, "w"), indent=2)
PY
echo "the app: $rehearsal_id, its daemon on $addr, its feed at http://127.0.0.1:$feed_port/latest.json"

# build WHERE: the app as VERSION now says, into $work/WHERE.
build() {
  PATH="$here/standins:$PATH" STANDIN_LOG="$work/standins.log" \
    LEADLINE_REHEARSAL=1 LEADLINE_REHEARSAL_CONFIG="$work/overlay.json" \
    LEADLINE_APP_ADDR="$addr" VITE_LEADLINE_URL="http://$addr" \
    APPLE_SIGNING_IDENTITY="$identity" TAURI_SIGNING_PRIVATE_KEY="$work/updater.key" CI=true \
    scripts/mac-release.sh --no-notarize >"$work/build-$1.log" 2>&1 ||
    { tail -15 "$work/build-$1.log"; die "building the $1 app failed (log: $work/build-$1.log)"; }
  mkdir "$work/$1"
  cp dist/mac/* "$work/$1/"
}

step "Building $version, the app that will update"
build old
step "Building $next, the update"
bumped=1
scripts/version.sh "$next" >/dev/null 2>&1
build new
scripts/version.sh "$version" >/dev/null 2>&1
bumped=""
[ "$(cat "${versioned[@]}" | shasum -a 256)" = "$before" ] || die "the version files are not as they were after setting the version back: look at git diff"

step "The feed, on this machine"
asset=Leadline-macos-universal.app.tar.gz
mkdir "$work/feed"
cp "$work/new/$asset" "$work/feed/"
# The release's own feed, pointing here and not at GitHub.
python3 - "$work/new/latest.json" "$work/feed/latest.json" "http://127.0.0.1:$feed_port/$asset" <<'PY'
import json, sys
src, out, url = sys.argv[1:4]
feed = json.load(open(src))
for entry in feed["platforms"].values():
    entry["url"] = url
json.dump(feed, open(out, "w"), indent=2)
PY
python3 -m http.server "$feed_port" --bind 127.0.0.1 --directory "$work/feed" >"$work/feed.log" 2>&1 &
server=$!
disown "$server"
echo "serving $next from http://127.0.0.1:$feed_port"

step "Installing $version and opening it"
mkdir -p "$work/mnt" "$work/Applications"
hdiutil attach -quiet -nobrowse -readonly -mountpoint "$work/mnt" "$work/old/Leadline-macos-universal.dmg"
ditto "$work/mnt/Leadline.app" "$installed"
hdiutil detach -quiet "$work/mnt"
rmdir "$work/mnt"
[ "$(plutil -extract CFBundleShortVersionString raw "$installed/Contents/Info.plist")" = "$version" ] || die "the app installed is not $version"

health() { curl -fsS -m 2 "http://$addr/api/health" 2>/dev/null || true; }
# until_version VERSION SECONDS: waits for the daemon on the app's port to
# answer as that version.
until_version() {
  local n=0
  until [[ "$(health)" == *"\"version\":\"$1\""* ]]; do
    [ "$n" -lt "$(($2 * 2))" ] || return 1
    sleep 0.5
    n=$((n + 1))
  done
}
gone() {
  local n=0
  while kill -0 "$1" 2>/dev/null; do
    [ "$n" -lt "$(($2 * 5))" ] || return 1
    sleep 0.2
    n=$((n + 1))
  done
}

# install: take the update as soon as it is ready, where a person would
# press "Restart to update". The rest is the path that press takes.
LEADLINE_UPDATES=install LEADLINE_DATA="$work/data" LEADLINE_OTLP_ADDR=off LEADLINE_BERTH=off \
  "$installed/Contents/MacOS/leadline" >>"$work/app.log" 2>&1 &
old_app=$!
disown "$old_app"
until_version "$version" 30 || die "the $version app's daemon did not answer on $addr: $(tail -3 "$work/app.log")"
old_daemon="$(pgrep -P "$old_app" -x leadlined | head -1)"
[ -n "$old_daemon" ] || die "the $version app did not start its own daemon"
echo "$version is running: the app is pid $old_app, its daemon pid $old_daemon ($(health))"

step "Waiting for it to update itself"
until_version "$next" 180 || die "after three minutes the daemon on $addr still isn't $next. The app said: $(tail -5 "$work/app.log") The feed was asked: $(cat "$work/feed.log")"
echo "the daemon on $addr now answers as $next"

failures=0
fail() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}
grep -q '"GET /latest.json' "$work/feed.log" || fail "the app never asked for the feed"
grep -q "\"GET /$asset" "$work/feed.log" || fail "the app never downloaded the archive"
echo "the app asked the feed and downloaded the archive:"
sed -n 's/^.*\("GET [^"]*" [0-9]*\).*$/  \1/p' "$work/feed.log"
now="$(plutil -extract CFBundleShortVersionString raw "$installed/Contents/Info.plist")"
[ "$now" = "$next" ] || fail "the app in the folder says it is $now, not $next"
echo "the app in the folder is $now"
gone "$old_app" 10 || fail "the old app (pid $old_app) is still running"
gone "$old_daemon" 10 || fail "the old daemon (pid $old_daemon) is still running"
echo "the old app and the old daemon are gone"
new_app="$(pgrep -f "^$installed/Contents/MacOS/leadline( |\$)" | head -1)"
[ -n "$new_app" ] || fail "no app is running out of the folder"
new_daemon="$(pgrep -P "${new_app:-0}" -x leadlined | head -1)"
[ -n "$new_daemon" ] || fail "the new app has no daemon of its own"
if [ -n "$new_daemon" ]; then
  said="$(ps -o command= -p "$new_daemon")"
  [[ "$said" == "$installed/Contents/MacOS/leadlined "* ]] || fail "the new daemon is not the one in the new app: $said"
  [[ "$said" == *"-data $work/data"* ]] || fail "the new daemon is not on the data folder the old one had: $said"
  [ "$("$installed/Contents/MacOS/leadlined" --version | cut -d' ' -f2)" = "$next" ] || fail "the daemon in the folder is not $next"
  echo "the new app is pid $new_app; its daemon, pid $new_daemon, is the one it carries, on the same data folder"
fi

step "Quitting the updated app"
if [ -n "$new_app" ]; then
  osascript -l JavaScript -e "ObjC.import('AppKit'); \$.NSRunningApplication.runningApplicationWithProcessIdentifier($new_app).terminate" >/dev/null || fail "could not ask the new app to quit"
  gone "$new_app" 20 || fail "the new app did not quit"
  [ -z "$new_daemon" ] || gone "$new_daemon" 5 || fail "the new app quit and left its daemon running"
  [ ! -e "$work/data/leadline.duckdb.wal" ] || fail "the new daemon stopped without closing its database"
  echo "it quit, and its daemon with it"
fi

if [ "$failures" != 0 ]; then die "$failures failure(s), above"; fi
echo
echo "updater-test: $version found $next, installed it and came back as $next, with the new daemon running and the old one gone."
