#!/usr/bin/env bash
# A whole release from this Mac, rehearsed: scripts/publish.sh from start to
# finish with stand-ins for everything that is secret or leaves the machine.
#
#   scripts/release-test/rehearse.sh [--keep] [--reject]     (make rehearse)
#
# What stands in for what:
#   the Developer ID      a certificate made here, signed by nobody, in a
#                         keychain made here (scripts/ci-keychain.sh, as the
#                         release workflow uses it) and deleted afterwards
#   the updater key       a key pair made here; its public half is laid over
#                         the app's config for this build only
#   1Password             standins/op, which reads made-up values from a folder
#   Apple's notary        standins/xcrun, which accepts everything (--reject:
#                         refuses everything, to see a release stop there)
#   Apple's timestamps    standins/codesign, which signs without asking for one
#   GitHub                nothing: publish.sh prints what it would push,
#                         tag and upload instead of doing it
#
# So the build, the signing with the hardened runtime, every check that does
# not need Apple's real answer, the dmg, the tarballs, the updater's archive
# and the feed all happen for real, and nothing leaves this Mac. The app it
# builds is its own thing: another bundle identifier and another port for
# its daemon, so opening it never meets the Leadline you use or its data.
#
# Afterwards it looks for the made-up secrets everywhere they must not be:
# in everything the release printed, in the command lines of the processes
# that ran (looked at twice a second while it ran), and on disk.
#
# The result is in dist/ and dist/mac/, marked NOT-A-RELEASE. --keep leaves
# the work folder (the stand-ins' log, the release's output) for a look.
#
# REHEARSAL_PORT names the port the rehearsal's app keeps its daemon on
# (default: one that is free now).
set -euo pipefail

keep="" reject=""
for a; do
  case "$a" in
    --keep) keep=1 ;;
    --reject) reject=1 ;;
    *) echo "usage: scripts/release-test/rehearse.sh [--keep] [--reject]" >&2; exit 2 ;;
  esac
done

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
cd "$root"

die() { echo "rehearse: $*" >&2; exit 1; }
step() { printf '\n=== %s\n' "$*"; }

[ "$(uname -s)" = Darwin ] || die "a release from a Mac is rehearsed on a Mac"
command -v minisign >/dev/null || die "minisign is needed to check the updater's signature: brew install minisign"
version="$(scripts/version.sh)" || exit 1

work="$(mktemp -d "${TMPDIR:-/tmp}/leadline-rehearsal.XXXXXX")"
work="$(cd "$work" && pwd -P)"
chmod 700 "$work"
# shellcheck source=scripts/release-test/lib.sh
. "$here/lib.sh"
watcher=""
cleanup() {
  [ -z "$watcher" ] || kill "$watcher" 2>/dev/null || true
  standins_remove
  if [ -n "$keep" ]; then
    # The made-up secrets go even then; the logs stay.
    rm -rf "$work/op" "$work/secrets"
    echo "rehearse: kept $work"
  else
    rm -rf "$work"
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP

step "Stand-ins"
standin_identity
standin_updater_key

# 1Password: the items publish.sh reads, with made-up values. Each is one
# nothing else would ever print, so finding it anywhere later is a leak.
mark="$(openssl rand -hex 8)"
issuer="acme-issuer-$mark"
key_id="ACME$mark"
p8="-----BEGIN PRIVATE KEY-----
acme-notary-key-$mark
-----END PRIVATE KEY-----"
mkdir -p "$work/op/Personal/Leadline updater signing key" "$work/op/Personal/Berth Developer Id"
cp "$work/updater.key" "$work/op/Personal/Leadline updater signing key/private key"
printf '%s' "$issuer" >"$work/op/Personal/Berth Developer Id/Issuer ID"
printf '%s' "$key_id" >"$work/op/Personal/Berth Developer Id/Key ID"
printf '%s\n' "$p8" >"$work/op/Personal/Berth Developer Id/AuthKey_$key_id.p8"
echo "1Password: a folder of made-up values"

# The app: its own identifier and port, and the stand-in public key.
port="${REHEARSAL_PORT:-$(free_port)}"
python3 - "$work/updater.key.pub" "$work/overlay.json" <<'PY'
import json, sys
pub, out = sys.argv[1:3]
json.dump({
    "identifier": "io.brydon.leadline.rehearsal",
    "plugins": {"updater": {"pubkey": open(pub).read().strip()}},
}, open(out, "w"), indent=2)
PY
echo "the app: io.brydon.leadline.rehearsal, its daemon on 127.0.0.1:$port"

# The secrets to look for afterwards, one a line, in a file: looking for
# them must not itself put them on a command line. The Key ID and the Issuer
# ID are not among them: they are names, and notarytool takes them as
# arguments.
{
  cat "$work/updater.key"
  echo
  echo "acme-notary-key-$mark"
} | grep . >"$work/secrets"

# The command lines of everything running, twice a second, counted and not
# kept: other people's are in there too.
: >"$work/ps.hits"
(
  while :; do
    # shellcheck disable=SC2009 # every process's whole command line, which pgrep can't search for a file of patterns
    ps -axww -o command= 2>/dev/null | grep -c -F -f "$work/secrets" >>"$work/ps.hits" || true
    sleep 0.5
  done
) &
watcher=$!

# Everything the release makes with mktemp goes here, so that what it left
# behind can be looked at.
mkdir "$work/tmp"

step "scripts/publish.sh, as a rehearsal"
status=0
env -u TAURI_SIGNING_PRIVATE_KEY -u TAURI_SIGNING_PRIVATE_KEY_PASSWORD -u APPLE_API_ISSUER -u APPLE_API_KEY -u APPLE_API_KEY_PATH \
  -u APPLE_CERTIFICATE -u APPLE_CERTIFICATE_PASSWORD -u LEADLINE_UPDATER_KEY_OP -u LEADLINE_APPLE_OP_ITEM \
  PATH="$here/standins:$PATH" TMPDIR="$work/tmp" \
  STANDIN_OP_DIR="$work/op" STANDIN_GH_DIR="$work/gh" STANDIN_LOG="$work/standins.log" STANDIN_NOTARY="${reject:+reject}" \
  LEADLINE_REHEARSAL=1 LEADLINE_REHEARSAL_CONFIG="$work/overlay.json" \
  LEADLINE_APP_ADDR="127.0.0.1:$port" VITE_LEADLINE_URL="http://127.0.0.1:$port" \
  APPLE_SIGNING_IDENTITY="$identity" CI=true \
  scripts/publish.sh >"$work/publish.log" 2>&1 || status=$?
kill "$watcher" 2>/dev/null || true
watcher=""
# What the release said, less the compiler's chatter.
grep -Ev '^ +(Compiling|Downloaded|Downloading) |^(vite|transforming|rendering|computing|dist/assets)' "$work/publish.log" || true
if [ "$status" != 0 ]; then
  [ -z "$reject" ] || echo "rehearse: the release stopped, as it must when Apple refuses it"
  [ -n "$reject" ] || echo "rehearse: scripts/publish.sh failed ($status); its output is above"
fi

step "What the stand-ins were asked"
[ ! -f "$work/standins.log" ] || sed -e "s|$work|<work>|g" -e "s|$root|.|g" "$work/standins.log" | cut -c1-220

step "Where the made-up secrets are"
leaks=0
found() {
  echo "LEAK: $*" >&2
  leaks=$((leaks + 1))
}
! grep -qF -f "$work/secrets" "$work/publish.log" || found "a secret is in what the release printed"
! grep -qF -f "$work/secrets" "$work/standins.log" 2>/dev/null || found "a secret is in the stand-ins' log (so it was an argument)"
if grep -qv '^0$' "$work/ps.hits"; then found "a secret was on a command line while the release ran"; fi
echo "in the release's output: no"
echo "on a command line while it ran: no ($(wc -l <"$work/ps.hits" | tr -d ' ') looks)"
# On disk: in what the release built, and in what it left of its temporary
# files (the key it wrote for notarytool must be gone).
left="$(grep -rlF -f "$work/secrets" --exclude-dir=.cache dist "$work/tmp" 2>/dev/null || true)"
[ -z "$left" ] || found "a secret is still on disk: $left"
echo "on disk afterwards (dist/, and the release's temporary files): no"
keychains_as_before || found "the list of keychains is not what it was"

step "Result"
if [ "$status" = 0 ] && [ -z "$reject" ]; then
  app="$work/mnt/Leadline.app"
  mkdir "$work/mnt"
  hdiutil attach -quiet -nobrowse -readonly -mountpoint "$work/mnt" dist/mac/Leadline-macos-universal.dmg
  for f in "$app" "$app/Contents/MacOS/leadlined"; do
    echo "${f#"$work/mnt/"}:"
    codesign -dv --verbose=2 "$f" 2>&1 | grep -E '^(Identifier|CodeDirectory|Authority|Signed Time|Timestamp|TeamIdentifier)' | sed -e 's/^/  /' -e 's/ size=[0-9]*//' -e 's/ hashes=.*//'
    echo "  entitlements: $(codesign -d --entitlements - "$f" 2>/dev/null | wc -c | tr -d ' ') bytes"
  done
  hdiutil detach -quiet "$work/mnt"
  (cd dist && du -h mac/* "leadline_${version}"_darwin_*.tar.gz | sed 's/^/  /')
fi
if [ "$leaks" != 0 ]; then die "$leaks leak(s), above"; fi
if [ -n "$reject" ]; then
  [ "$status" != 0 ] || die "Apple's stand-in refused the release, and it went on anyway"
  echo "rehearse: refused by the notary's stand-in, and stopped. That is the right answer."
  exit 0
fi
[ "$status" = 0 ] || exit "$status"
echo "rehearse: a whole release, with nothing sent anywhere and no secret left behind."
