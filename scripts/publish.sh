#!/usr/bin/env bash
# Publishes the macOS half of a Leadline release from this Mac, signed with
# the Developer ID in its keychain, with the same build the release workflow
# makes in CI:
#
#   make publish [NOTES="What changed"]
#
# The version is the VERSION file's: set it (scripts/version.sh 0.3.0),
# commit `release: v0.3.0` on main, and run this from that commit.
#
# 1. Builds leadline and leadlined for both kinds of Mac, signed and
#    notarized (scripts/release.sh), and the app, signed, notarized, stapled
#    and checked (scripts/mac-release.sh).
# 2. Opens the app it just built, from the dmg, on a data folder of its own,
#    and checks that its daemon starts, answers and stops with it
#    (scripts/release-check.sh). Leadline must not be running: the app's
#    daemon has one port.
# 3. Pushes main, and makes a draft GitHub release that holds the dmg, the
#    updater's archive and its signature, the two macOS tarballs, and last
#    latest.json, the feed installed apps update from.
# 4. Tags the commit and pushes the tag.
#
# The tag starts .github/workflows/release.yml. It finds the draft already
# announcing this version, so it builds nothing for macOS (and needs none of
# the signing secrets); it builds the Linux tarballs, writes checksums.txt
# over all four, adds install.sh, and only then publishes the release. So
# nothing is public until everything is there, and a failure on the way
# leaves a draft to delete, not a broken release.
#
# Credentials come from the environment, else from 1Password (the op
# command line):
#   TAURI_SIGNING_PRIVATE_KEY  op://Personal/Leadline updater signing key/private key
#   APPLE_API_ISSUER           "Issuer ID" of op://Personal/Berth Developer Id
#   APPLE_API_KEY              "Key ID" of the same item
#   APPLE_API_KEY_PATH         its AuthKey_<Key ID>.p8, written to a private
#                              temporary file that is removed when this exits
#   APPLE_SIGNING_IDENTITY     the one Developer ID Application identity in
#                              the keychain
# The Apple item is the one Shipyard's releases read: the Developer ID and
# the notary key belong to the Apple account, not to a product. The updater
# key is Leadline's own. LEADLINE_UPDATER_KEY_OP and LEADLINE_APPLE_OP_ITEM
# point at other 1Password items.
#
# No value read here is ever printed, put on a command line, or left on
# disk. The updater key goes to the build in its environment (Tauri takes
# it no other way), and only to that one command.
#
# LEADLINE_REHEARSAL=1 is for scripts/release-test/rehearse.sh: it builds
# and checks everything with stand-in credentials, and where a release
# would push, tag or upload it prints the command instead.
set -euo pipefail

repo="sean-brydon/leadline"
updater_op="${LEADLINE_UPDATER_KEY_OP:-op://Personal/Leadline updater signing key/private key}"
apple_op="${LEADLINE_APPLE_OP_ITEM:-op://Personal/Berth Developer Id}"
rehearsal="${LEADLINE_REHEARSAL:-}"
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

die() { echo "publish: $*" >&2; exit 1; }
step() { printf '==> %s\n' "$*"; }
# run COMMAND…: what changes something outside this folder. A rehearsal
# says what it would have run.
run() {
  if [ -n "$rehearsal" ]; then
    printf 'rehearsal, not run:'
    printf ' %q' "$@"
    printf '\n'
  else
    "$@"
  fi
}

[ "$(uname -s)" = Darwin ] || die "this signs with a Mac's keychain, so it runs on a Mac"
version="$(scripts/version.sh)" || exit 1
tag="v$version"

if [ -n "$rehearsal" ]; then
  echo "publish: a rehearsal of $tag: nothing is pushed, tagged or uploaded"
else
  command -v gh >/dev/null || die "the GitHub command line (gh) is not installed: brew install gh"
  gh auth status >/dev/null 2>&1 || die "gh is not signed in to GitHub: gh auth login"
  [ -z "$(git status --porcelain)" ] || die "commit your changes first: a release is built from a commit, and this folder has changes that are in none"
  [ "$(git rev-parse --abbrev-ref HEAD)" = main ] || die "publish from main (this is $(git rev-parse --abbrev-ref HEAD))"
  ! git rev-parse -q --verify "refs/tags/$tag" >/dev/null || die "$tag is already tagged here: VERSION still says $version. Set the next one with scripts/version.sh and commit it."
  [ -z "$(git ls-remote --tags origin "refs/tags/$tag")" ] || die "$tag is already tagged on GitHub"
  ! gh release view "$tag" --repo "$repo" >/dev/null 2>&1 || die "$tag already has a release (or a draft of one) on GitHub: delete it first if it is a failed attempt (gh release delete $tag)"
fi

# Everything secret that touches the disk lives here, and goes when this
# exits, however it exits.
tmp="$(mktemp -d "${TMPDIR:-/tmp}/leadline-publish.XXXXXX")"
chmod 700 "$tmp"
trap 'rm -rf "$tmp"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP

# shellcheck source=scripts/op.sh
. "$root/scripts/op.sh"

step "Credentials"
# Each read is checked for emptiness too: a field that is there and blank
# would otherwise fail much later, as a signature nobody can explain.
updater_key="${TAURI_SIGNING_PRIVATE_KEY:-}"
unset TAURI_SIGNING_PRIVATE_KEY
if [ -z "$updater_key" ]; then
  updater_key="$(op_read "$updater_op" "the updater signing key")"
  [ -n "$updater_key" ] || die "the updater signing key in 1Password is empty ($updater_op)"
  echo "  the updater signing key: from 1Password"
else
  echo "  the updater signing key: from the environment"
fi
if [ -z "${APPLE_API_ISSUER:-}" ]; then
  APPLE_API_ISSUER="$(op_read "$apple_op/Issuer ID" "APPLE_API_ISSUER")"
  [ -n "$APPLE_API_ISSUER" ] || die "\"Issuer ID\" in 1Password is empty ($apple_op)"
  echo "  APPLE_API_ISSUER: from 1Password"
fi
if [ -z "${APPLE_API_KEY:-}" ]; then
  APPLE_API_KEY="$(op_read "$apple_op/Key ID" "APPLE_API_KEY")"
  [ -n "$APPLE_API_KEY" ] || die "\"Key ID\" in 1Password is empty ($apple_op)"
  echo "  APPLE_API_KEY: from 1Password"
fi
if [ -z "${APPLE_API_KEY_PATH:-}" ]; then
  APPLE_API_KEY_PATH="$tmp/AuthKey.p8"
  # The file's name in 1Password carries the key's ID; say only the item.
  (umask 077 && op_read "$apple_op/AuthKey_$APPLE_API_KEY.p8" "the App Store Connect API key (the item's AuthKey_<Key ID>.p8 file)" >"$APPLE_API_KEY_PATH")
  [ -s "$APPLE_API_KEY_PATH" ] || die "the App Store Connect API key in 1Password is empty ($apple_op)"
  echo "  the App Store Connect API key: from 1Password, in a private temporary file"
fi
export APPLE_API_ISSUER APPLE_API_KEY APPLE_API_KEY_PATH

# The updater key proves an update came from us; it says nothing to
# Gatekeeper, which needs the Developer ID signature and Apple's ticket.
if [ -z "${APPLE_SIGNING_IDENTITY:-}" ]; then
  found="$(security find-identity -v -p codesigning 2>/dev/null | grep -c "Developer ID Application" || true)"
  case "$found" in
    1) ;;
    0) die "there is no Developer ID Application identity in this Mac's keychain to sign with. It is the certificate from developer.apple.com (Certificates, \"Developer ID Application\") together with its private key; install it, or set APPLE_SIGNING_IDENTITY." ;;
    *) die "set APPLE_SIGNING_IDENTITY: there are $found Developer ID Application identities in the keychain, and this picks one only when there is exactly one" ;;
  esac
  APPLE_SIGNING_IDENTITY="$(security find-identity -v -p codesigning | sed -n 's/.*"\(Developer ID Application: [^"]*\)".*/\1/p' | head -1)"
fi
export APPLE_SIGNING_IDENTITY
echo "  signing as: $APPLE_SIGNING_IDENTITY"

# Before the build, not after it: the app that gets built is opened before
# anything is published (scripts/release-check.sh), and its daemon needs
# its port.
port=4320
[ -z "$rehearsal" ] || [ -z "${LEADLINE_APP_ADDR:-}" ] || port="${LEADLINE_APP_ADDR##*:}"
if lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
  die "something is listening on port $port, most likely Leadline itself. Quit it and run this again: the app this builds is opened once before it is published, on a data folder of its own, and its daemon needs that port."
fi

step "The macOS programs"
LEADLINE_REQUIRE_SIGNED=1 scripts/release.sh darwin_arm64 darwin_amd64 >/dev/null

step "The app"
# The one command that is given the updater key.
TAURI_SIGNING_PRIVATE_KEY="$updater_key" NOTES="${NOTES:-}" scripts/mac-release.sh
unset updater_key

step "Opening the app that was built"
scripts/release-check.sh --built

mac=(dist/mac/Leadline-macos-universal.dmg dist/mac/Leadline-macos-universal.app.tar.gz dist/mac/Leadline-macos-universal.app.tar.gz.sig
  "dist/leadline_${version}_darwin_arm64.tar.gz" "dist/leadline_${version}_darwin_amd64.tar.gz")
for f in "${mac[@]}" dist/mac/latest.json; do
  [ -f "$f" ] || die "$f was not built"
done
if [ -z "$rehearsal" ] && [ -e dist/mac/NOT-A-RELEASE ]; then
  die "dist/mac is marked NOT-A-RELEASE (a rehearsal's, or not notarized), and this won't publish it"
fi

step "Publishing"
sha="$(git rev-parse --verify HEAD 2>/dev/null || echo HEAD)"
run git push -q origin main
# A draft, and before the tag: when the tag starts the workflow, everything
# from this Mac is already there for it to find.
if [ -n "${NOTES:-}" ]; then
  run gh release create "$tag" --repo "$repo" --draft --target "$sha" --title "Leadline $tag" --notes "$NOTES" "${mac[@]}"
else
  run gh release create "$tag" --repo "$repo" --draft --target "$sha" --title "Leadline $tag" --generate-notes "${mac[@]}"
fi
# Last, so the feed never points at an archive that is not there yet.
run gh release upload "$tag" --repo "$repo" --clobber dist/mac/latest.json
run git tag -a "$tag" -m "Leadline $tag"
run git push -q origin "$tag"

if [ -n "$rehearsal" ]; then
  echo "publish: the rehearsal is done. What a release would carry from this Mac is in dist/ and dist/mac/ (never publish these)."
else
  echo "publish: $tag is tagged, with the macOS half in a draft release. The release workflow adds the Linux half and publishes it:"
  echo "  gh run watch --repo $repo --exit-status \"\$(gh run list --repo $repo --workflow release.yml --limit 1 --json databaseId --jq '.[0].databaseId')\""
  echo "If that fails, nothing is public: fix it and run the workflow again, or delete the draft (gh release delete $tag --repo $repo) and the tag."
fi
