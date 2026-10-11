#!/usr/bin/env bash
# Builds Leadline for macOS the way a release ships it, checks it, and puts
# what a GitHub release carries of it in dist/mac/:
#
#   Leadline-macos-universal.dmg              the download (Apple silicon and Intel)
#   Leadline-macos-universal.app.tar.gz(.sig) the app's updater's archive and signature
#   latest.json                               the feed installed apps update from
#
#   scripts/mac-release.sh [--no-notarize]
#
# The version is the VERSION file's. The release workflow
# (.github/workflows/release.yml) and scripts/publish.sh both run this, so a
# release built on a Mac and one built in CI are the same.
#
# It needs, in the environment:
#   APPLE_SIGNING_IDENTITY     "Developer ID Application: …", in a keychain
#   TAURI_SIGNING_PRIVATE_KEY  the updater's key (and _PASSWORD if it has one)
#   APPLE_API_ISSUER, APPLE_API_KEY, APPLE_API_KEY_PATH
#                              an App Store Connect API key, for notarization
# and the Rust targets aarch64-apple-darwin and x86_64-apple-darwin, and
# minisign (brew install minisign), which checks the updater's signature.
#
# One app for both kinds of Mac: a person never has to know which they have,
# the updater's feed has one archive, and every Mac that can run the daemon
# at all (macOS 13 on) is covered. It costs a larger download, since the app
# then holds the daemon twice, and DuckDB is most of the daemon.
#
# Tauri signs the app and the leadlined it carries with the hardened
# runtime, notarizes the app and staples it, then makes the dmg and the
# updater's archive from the stapled app. Neither program gets an
# entitlement: under the hardened runtime the daemon does everything it
# does (AGENTS.md says how that was checked), and what the runtime refuses,
# loading code Apple or this team did not sign, such as a DuckDB extension,
# Leadline never asks for. This then signs the dmg if Tauri did not,
# notarizes and staples it too, and fails unless every check passes:
#
#   - codesign --verify --deep --strict, a Developer ID, a team, the
#     hardened runtime and a secure timestamp, on the app and on the daemon
#     inside it; both architectures in each; no entitlements on either
#   - spctl, Gatekeeper's own verdict, which must say "Notarized Developer
#     ID", and stapler validate
#   - all of that again on the app inside the dmg and the app inside the
#     updater's archive, since those copies are what people run
#   - the daemon inside starts as signed, answers, and says this version
#   - the updater's signature against the public key the app carries, and
#     the version inside that signature, which the app insists on
#
# --no-notarize skips Apple (nothing is uploaded) and the checks that need
# its ticket, for trying a signed build locally.
#
# LEADLINE_REHEARSAL=1 is for scripts/release-test/rehearse.sh: the identity
# may be a stand-in certificate, so the checks that only a real Developer ID
# and Apple's real ticket can pass are skipped, and LEADLINE_REHEARSAL_CONFIG
# may name a Tauri config to lay over the app's (a throwaway updater key, a
# local feed, another bundle identifier). Whatever either of those builds is
# marked dist/mac/NOT-A-RELEASE, and nothing that publishes will take it.
set -euo pipefail

notarize=1
case "${1:-}" in
  "") ;;
  --no-notarize) notarize=0 ;;
  *) echo "usage: scripts/mac-release.sh [--no-notarize]" >&2; exit 2 ;;
esac
rehearsal="${LEADLINE_REHEARSAL:-}"
repo="sean-brydon/leadline"
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

die() { echo "mac-release: $*" >&2; exit 1; }
step() { printf '==> %s\n' "$*"; }
ok() { printf '  ok: %s\n' "$*"; }

[ "$(uname -s)" = Darwin ] || die "this builds the macOS app, so it runs on a Mac"
# The four places the version is written must agree (scripts/version.sh).
version="$(scripts/version.sh)" || exit 1
tag="v$version"

: "${APPLE_SIGNING_IDENTITY:?set APPLE_SIGNING_IDENTITY to the Developer ID Application identity}"
: "${TAURI_SIGNING_PRIVATE_KEY:?set TAURI_SIGNING_PRIVATE_KEY, the updater signing key}"
# Unset, Tauri would stop and ask for it at a prompt nobody is at.
export TAURI_SIGNING_PRIVATE_KEY_PASSWORD="${TAURI_SIGNING_PRIVATE_KEY_PASSWORD:-}"
if [ "$notarize" = 1 ]; then
  : "${APPLE_API_ISSUER:?set APPLE_API_ISSUER (or pass --no-notarize)}"
  : "${APPLE_API_KEY:?set APPLE_API_KEY, the API key ID (or pass --no-notarize)}"
  : "${APPLE_API_KEY_PATH:?set APPLE_API_KEY_PATH to the .p8 file (or pass --no-notarize)}"
  [ -f "$APPLE_API_KEY_PATH" ] || die "no API key at APPLE_API_KEY_PATH"
else
  # Tauri notarizes whenever these are set.
  unset APPLE_API_ISSUER APPLE_API_KEY APPLE_API_KEY_PATH APPLE_ID APPLE_PASSWORD APPLE_TEAM_ID
fi
# Tauri imports a certificate of its own when it finds one here, and would
# then ignore the keychain the identity is in.
unset APPLE_CERTIFICATE APPLE_CERTIFICATE_PASSWORD
# Known now, not when codesign fails at the end of the build.
security find-identity -p codesigning 2>/dev/null | grep -qF -e "$APPLE_SIGNING_IDENTITY" ||
  die "there is no signing identity \"$APPLE_SIGNING_IDENTITY\" in a keychain on this Mac's list: \`security find-identity -v -p codesigning\` shows the ones there are. It is the Developer ID Application certificate together with its private key."

overlay=""
if [ -n "$rehearsal" ]; then
  overlay="${LEADLINE_REHEARSAL_CONFIG:-}"
  [ -z "$overlay" ] || [ -f "$overlay" ] || die "LEADLINE_REHEARSAL_CONFIG names $overlay, which is not there"
else
  # A release's app talks to the daemon on its usual port and to the real
  # feed, whatever is left in the environment from a test.
  unset LEADLINE_APP_ADDR VITE_LEADLINE_URL
fi

# setting POINTER: a value of the app's config as it will be built, the
# rehearsal's overlay laid over tauri.conf.json.
setting() {
  python3 - "$1" "$overlay" <<'PY'
import json, sys
pointer, overlay = sys.argv[1:3]
def at(doc):
    for key in pointer.strip("/").split("/"):
        if not isinstance(doc, dict) or key not in doc:
            return None
        doc = doc[key]
    return doc
value = at(json.load(open("app/src-tauri/tauri.conf.json")))
if overlay:
    over = at(json.load(open(overlay)))
    value = value if over is None else over
print("" if value is None else value)
PY
}

# Before anything is built: an app shipped without the real public key
# could never take an update, and no later release could fix that.
pubkey="$(setting /plugins/updater/pubkey)"
case "$pubkey" in
  "" | UPDATER-PUBLIC-KEY-NOT-SET)
    die "the updater has no public key yet: plugins.updater.pubkey in app/src-tauri/tauri.conf.json is still the placeholder. Make the key pair once with scripts/updater-key.sh, which says where each half goes, and commit the config." ;;
esac

installed="$(rustup target list --installed)"
for t in aarch64-apple-darwin x86_64-apple-darwin; do
  grep -qx "$t" <<<"$installed" || die "missing the Rust target $t: rustup target add $t"
done
# A release whose updater signature nobody checked is how every installed
# app ends up refusing every update, so this is not skipped where minisign
# is missing.
command -v minisign >/dev/null || die "minisign is needed to check the updater's signature: brew install minisign"
[ -d app/node_modules ] || (cd app && CI=true pnpm install --frozen-lockfile)

target=universal-apple-darwin
target_dir="${CARGO_TARGET_DIR:-$root/app/src-tauri/target}"
bundle="$target_dir/$target/release/bundle"
out="dist/mac"
work="$(mktemp -d "${TMPDIR:-/tmp}/leadline-mac-release.XXXXXX")"
mnt="$work/mnt"
daemon_pid=""
cleanup() {
  [ -z "$daemon_pid" ] || kill "$daemon_pid" 2>/dev/null || true
  if [ -d "$mnt" ]; then hdiutil detach -quiet "$mnt" 2>/dev/null || true; fi
  rm -rf "$work"
}
trap cleanup EXIT

step "Building Leadline $version for Apple silicon and Intel"
rm -rf "$bundle" "$out"
config=(--config src-tauri/tauri.updater.conf.json)
[ -z "$overlay" ] || config+=(--config "$overlay")
# The daemon first, for both architectures and the two joined: cargo wants
# all three there as it compiles (scripts/sidecar.sh).
scripts/sidecar.sh "$target"
(cd app && pnpm tauri build --target "$target" "${config[@]}")

app="$bundle/macos/Leadline.app"
dmgs=("$bundle"/dmg/*.dmg)
[ -d "$app" ] || die "no $app"
if [ "${#dmgs[@]}" != 1 ] || [ ! -f "${dmgs[0]}" ]; then die "expected one dmg in $bundle/dmg"; fi
tarball="$bundle/macos/Leadline.app.tar.gz"
[ -f "$tarball" ] || die "no updater archive at $tarball (is createUpdaterArtifacts on?)"
[ -f "$tarball.sig" ] || die "the updater archive was not signed"

# check_signed PATH: one program or bundle, signed as a release signs it.
check_signed() {
  local path="$1" info
  info="$(codesign -dv --verbose=2 "$path" 2>&1)" || die "$path is not signed at all"
  grep -Eq "^CodeDirectory .*flags=.*runtime" <<<"$info" ||
    die "$path lacks the hardened runtime, which notarization requires"
  ! grep -q "^Signature=adhoc" <<<"$info" || die "$path is signed ad hoc, not with an identity; Gatekeeper would refuse it"
  # A rehearsal's stand-in certificate is no Developer ID, belongs to no
  # team, and its signatures get no timestamp from Apple.
  [ -z "$rehearsal" ] || return 0
  grep -q "^Authority=Developer ID Application:" <<<"$info" ||
    die "$path is not signed with a Developer ID; Gatekeeper would refuse it"
  if ! grep -q "^TeamIdentifier=" <<<"$info" || grep -q "^TeamIdentifier=not set" <<<"$info"; then
    die "$path has no team identifier"
  fi
  grep -q "^Timestamp=" <<<"$info" || die "$path's signature has no secure timestamp, which notarization requires"
}

# check_app APP: what every copy of the app must be.
check_app() {
  local app="$1" exe archs v given
  codesign --verify --deep --strict --verbose=2 "$app" 2>&1 | sed 's/^/  /' ||
    die "$app: the signature does not verify"
  check_signed "$app"
  # The app's own program, and the daemon beside it. The daemon is the one
  # that matters most here: it is a program of its own, Tauri signs it
  # separately, and Apple refuses the whole app over it.
  for exe in "$app/Contents/MacOS/leadline" "$app/Contents/MacOS/leadlined"; do
    [ -x "$exe" ] || die "no $exe"
    codesign --verify --strict "$exe" || die "$exe: the signature does not verify"
    check_signed "$exe"
    archs="$(lipo -archs "$exe")"
    [[ " $archs " == *" arm64 "* && " $archs " == *" x86_64 "* ]] ||
      die "$exe is not universal (has: $archs)"
    # None is needed, so one that appears was not decided on: say so here
    # rather than ship a wider door than anyone chose.
    given="$(codesign -d --entitlements - "$exe" 2>/dev/null || true)"
    [ -z "$given" ] || die "$exe was signed with entitlements, and Leadline needs none: $given"
  done
  v="$(plutil -extract CFBundleShortVersionString raw "$app/Contents/Info.plist")"
  [ "$v" = "$version" ] || die "$app says it is $v, not $version"
  v="$("$app/Contents/MacOS/leadlined" --version)" || die "the daemon in $app does not start"
  [[ "$v" == "leadlined $version"* ]] || die "the daemon in $app says \"$v\", not version $version"
  if [ "$notarize" = 1 ] && [ -z "$rehearsal" ]; then
    xcrun stapler validate "$app" >/dev/null || die "$app has no stapled notarization ticket"
    gatekeeper exec "$app"
  fi
  ok "$app"
}

# gatekeeper TYPE PATH: spctl must accept it, as notarized.
gatekeeper() {
  local verdict
  verdict="$(spctl -a -vv -t "$1" "$2" 2>&1)" || die "Gatekeeper rejects $2: $verdict"
  if grep -q "override=security disabled" <<<"$verdict"; then
    die "Gatekeeper assessments are off on this Mac, so spctl proves nothing; turn them on with: sudo spctl --master-enable"
  fi
  grep -q "source=Notarized Developer ID" <<<"$verdict" ||
    die "Gatekeeper accepts $2 but not as notarized: $verdict"
}

# check_daemon APP: the daemon, signed as it ships, starts under the
# hardened runtime, opens DuckDB and answers. A missing entitlement shows
# here, as a daemon that is killed or can't map its memory, and not on
# someone's Mac after the release.
check_daemon() {
  local daemon="$1/Contents/MacOS/leadlined" port answer=""
  port="$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')"
  # Its own port, nothing on disk, no second listener, and no Shipyard.
  LEADLINE_BERTH=off "$daemon" -addr "127.0.0.1:$port" -data :memory: -otlp off >"$work/daemon.log" 2>&1 &
  daemon_pid=$!
  for _ in $(seq 50); do
    answer="$(curl -fsS "http://127.0.0.1:$port/api/health" 2>/dev/null)" && break
    kill -0 "$daemon_pid" 2>/dev/null || break
    sleep 0.2
  done
  if kill -0 "$daemon_pid" 2>/dev/null; then
    kill "$daemon_pid"
    wait "$daemon_pid" || die "the signed daemon did not stop cleanly when asked: $(tail -3 "$work/daemon.log")"
  fi
  daemon_pid=""
  [[ "$answer" == *'"name":"leadline"'* && "$answer" == *"\"version\":\"$version\""* ]] ||
    die "the signed daemon did not start and answer as $version (it said: ${answer:-nothing}): $(tail -3 "$work/daemon.log")"
  ok "the daemon inside starts as signed, answers as $version, and stops when asked"
}

step "Checking the app"
check_app "$app"
check_daemon "$app"

mkdir -p "$out"
if [ -n "$rehearsal" ] || [ "$notarize" = 0 ]; then
  if [ -n "$rehearsal" ]; then why="a rehearsal, signed with a stand-in certificate"; else why="built with --no-notarize, so Apple never saw it"; fi
  echo "$why: for local checks only, never publish what is in this folder" >"$out/NOT-A-RELEASE"
fi
name="Leadline-macos-universal"
dmg="$out/$name.dmg"
cp "${dmgs[0]}" "$dmg"
cp "$tarball" "$out/$name.app.tar.gz"
cp "$tarball.sig" "$out/$name.app.tar.gz.sig"

step "Signing the dmg"
if codesign --verify --strict "$dmg" 2>/dev/null; then
  ok "Tauri signed it"
else
  codesign --force --timestamp --sign "$APPLE_SIGNING_IDENTITY" "$dmg"
fi
codesign --verify --strict --verbose=2 "$dmg" || die "the dmg's signature does not verify"

if [ "$notarize" = 1 ]; then
  step "Notarizing the dmg (the app inside already is)"
  scripts/notarize.sh "$dmg"
  # The ticket goes into the dmg itself, so Gatekeeper needs no network to
  # open it on a Mac that has never seen Leadline.
  xcrun stapler staple "$dmg" || die "could not staple Apple's ticket to the dmg"
fi

step "Checking the dmg"
codesign --verify --deep --strict --verbose=2 "$dmg" || die "the dmg's signature does not verify"
if [ "$notarize" = 1 ] && [ -z "$rehearsal" ]; then
  gatekeeper install "$dmg"
  xcrun stapler validate "$dmg" || die "the dmg has no stapled notarization ticket"
fi
mkdir -p "$mnt"
hdiutil attach -quiet -nobrowse -readonly -mountpoint "$mnt" "$dmg"
[ -L "$mnt/Applications" ] || die "the dmg has no Applications link to drag Leadline to"
check_app "$mnt/Leadline.app"
hdiutil detach -quiet "$mnt"
ok "$dmg"

step "Checking the updater archive"
mkdir -p "$work/tar"
tar -xzf "$out/$name.app.tar.gz" -C "$work/tar"
# The updater unpacks the archive's first folder over the installed app, so
# it must be the app and nothing else (not a ._ file macOS's tar can add).
holds=("$work/tar"/* "$work/tar"/.[!.]*)
[ "${holds[*]}" = "$work/tar/Leadline.app $work/tar/.[!.]*" ] || die "the updater archive holds more than Leadline.app: ${holds[*]##*/}"
check_app "$work/tar/Leadline.app"
base64 -d <<<"$pubkey" >"$work/updater.pub" || die "the updater's public key in tauri.conf.json is not base64"
base64 -d <"$out/$name.app.tar.gz.sig" >"$work/updater.minisig"
minisign -Vq -p "$work/updater.pub" -x "$work/updater.minisig" -m "$out/$name.app.tar.gz" ||
  die "the updater signature does not match the public key in tauri.conf.json, so installed apps would refuse this update. Is TAURI_SIGNING_PRIVATE_KEY the private half of that key?"
ok "signed with the key the app trusts"
# The app accepts an archive only for the version its signature names
# (plugins.updater.requireSignedVersion), so that an old, genuinely signed
# archive can't be served as a new version. The name is in the signature's
# trusted comment, among fields a tab apart.
tab=$'\t'
grep -Eq "^trusted comment:.*version:$version($tab|\$)" "$work/updater.minisig" ||
  die "the updater signature does not name version $version, and the app refuses one that doesn't: $(grep '^trusted comment:' "$work/updater.minisig")"
ok "the signature names $version"

step "Writing latest.json"
python3 - "$version" "$tag" "$repo" "${NOTES:-}" "$out" "$name" <<'PY'
import datetime, json, sys
version, tag, repo, notes, out, name = sys.argv[1:7]
sig = open(f"{out}/{name}.app.tar.gz.sig").read().strip()
url = f"https://github.com/{repo}/releases/download/{tag}/{name}.app.tar.gz"
# One universal archive serves both kinds of Mac.
platform = {"signature": sig, "url": url}
feed = {
    "version": version,
    "notes": notes or f"Leadline {version}: https://github.com/{repo}/releases/tag/{tag}",
    "pub_date": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    "platforms": {"darwin-aarch64": platform, "darwin-x86_64": platform},
}
with open(f"{out}/latest.json", "w") as f:
    json.dump(feed, f, indent=2)
    f.write("\n")
PY

if [ -f "$out/NOT-A-RELEASE" ]; then
  echo "mac-release: NOT for release ($(cat "$out/NOT-A-RELEASE"))" >&2
fi
(cd "$out" && shasum -a 256 ./*)
