#!/usr/bin/env bash
# Sends one file to Apple's notary service and waits for the answer.
#
#   scripts/notarize.sh FILE       a .zip, a .dmg or a .pkg
#
# It needs an App Store Connect API key in the environment:
#   APPLE_API_ISSUER      the issuer ID
#   APPLE_API_KEY         the key ID
#   APPLE_API_KEY_PATH    the key itself, a .p8 file
#
# Apple's answer is "Accepted" or it is a failure: on anything else this
# prints Apple's log for the submission, which names each file it refused
# and why (not signed with a Developer ID, no hardened runtime, no secure
# timestamp), and exits 1. A ticket is not attached to anything here: the
# caller staples what can be stapled (an app, a dmg) and says what can't (a
# bare program).
#
# scripts/release.sh and scripts/mac-release.sh both use it, and Tauri calls
# the same `xcrun notarytool` for the app itself. A rehearsal puts a
# stand-in xcrun first on PATH (scripts/release-test/standins), so nothing
# is sent to Apple.
set -euo pipefail

die() { echo "notarize: $*" >&2; exit 1; }

file="${1:?usage: scripts/notarize.sh FILE}"
[ -f "$file" ] || die "there is no $file"
: "${APPLE_API_ISSUER:?set APPLE_API_ISSUER, the App Store Connect issuer ID}"
: "${APPLE_API_KEY:?set APPLE_API_KEY, the App Store Connect key ID}"
: "${APPLE_API_KEY_PATH:?set APPLE_API_KEY_PATH to the .p8 file of that key}"
[ -f "$APPLE_API_KEY_PATH" ] || die "APPLE_API_KEY_PATH names a file that is not there"

# The key is read from its file by notarytool; only its path, its ID and the
# issuer's are on the command line.
creds=(--key "$APPLE_API_KEY_PATH" --key-id "$APPLE_API_KEY" --issuer "$APPLE_API_ISSUER")
field() { python3 -c 'import json, sys; print(json.load(sys.stdin).get(sys.argv[1], ""))' "$1" 2>/dev/null || true; }

echo "notarize: sending $(basename "$file") to Apple and waiting (usually a few minutes)" >&2
# An hour: Apple is sometimes slow, and a release should wait rather than
# be cut again from the start.
result="$(xcrun notarytool submit "$file" "${creds[@]}" --wait --timeout 1h --output-format json)" ||
  die "notarytool could not submit $(basename "$file"): ${result:-it said nothing; is the API key right, and is this machine online?}"
status="$(field status <<<"$result")"
id="$(field id <<<"$result")"
if [ "$status" != Accepted ]; then
  # Apple's reasons, file by file.
  [ -z "$id" ] || xcrun notarytool log "$id" "${creds[@]}" >&2 || true
  die "Apple did not accept $(basename "$file") (status: ${status:-unknown}). Its log above says which file and why."
fi
echo "notarize: Apple accepted $(basename "$file")${id:+ ($id)}" >&2
