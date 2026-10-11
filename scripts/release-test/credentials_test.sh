#!/usr/bin/env bash
# Tests the scripts that handle a release's credentials, with no credential
# and no network: 1Password and GitHub are the stand-ins beside this file,
# and the Mac's keychain is a stand-in written below, so nothing here reads
# a real secret, touches a real keychain or sets anything on GitHub. It runs
# the same on a Mac and in CI (`make check` runs it).
#
#   scripts/release-test/credentials_test.sh
#   VERBOSE=3 scripts/release-test/credentials_test.sh    and the last three
#                                                         lines each case said
#
# What it checks:
#   scripts/ci-secrets.sh    what it sets, that every value reaches gh on its
#                            standard input and is printed nowhere, and that
#                            each way of failing (no op, signed out, a field
#                            missing or blank, a file that is not a key, a
#                            certificate that won't import, no answer to
#                            "Set them?") says so in a sentence and sets
#                            nothing
#   scripts/updater-key.sh   where the two halves of a new key go, and that
#                            it never makes a second key
#   scripts/publish.sh       (on a Mac) that it stops, in a sentence, before
#                            building anything, when a credential can't be
#                            had, and leaves no file behind, also when it is
#                            cut short
#
# A whole release with stand-ins is scripts/release-test/rehearse.sh.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
work="$(mktemp -d "${TMPDIR:-/tmp}/leadline-credentials-test.XXXXXX")"
work="$(cd "$work" && pwd -P)"
trap 'rm -rf "$work"' EXIT

failures=0
fail() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}
# names DIR: the files in a folder, on one line.
names() { find "$1" -type f -exec basename {} \; | sort | tr '\n' ' '; }
sum() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum | awk '{print $1}'; else shasum -a 256 | awk '{print $1}'; fi
}

# The stand-ins for this test only. The keychain: `security`, which makes a
# keychain by making a file, and finds in it the identity it is told to.
mkdir -p "$work/shim" "$work/none"
cp "$here/standins/op" "$here/standins/gh" "$work/shim/"
cat >"$work/shim/security" <<'EOF'
#!/bin/sh
echo "security $1" >>"$STANDIN_LOG"
case $1 in
  create-keychain) for a; do last=$a; done; : >"$last" ;;
  delete-keychain) rm -f "$2" ;;
  import) [ "${STANDIN_SECURITY:-}" != bad-password ] || { echo "security: SecKeychainItemImport: MAC verification failed during PKCS12 import (wrong password?)" >&2; exit 1; } ;;
  list-keychains) [ "$#" -gt 3 ] || echo '    "/Users/acme/Library/Keychains/login.keychain-db"' ;;
  find-identity)
    [ -z "${STANDIN_SLEEP:-}" ] || sleep "$STANDIN_SLEEP"
    n=0
    for name in ${STANDIN_IDENTITIES-acme}; do
      n=$((n + 1))
      echo "  $n) 00000000000000000000000000000000000ACE0$n \"Developer ID Application: Acme $name (ACME00000$n)\""
    done
    echo "     $n valid identities found"
    ;;
esac
EOF
# pnpm, for `pnpm tauri signer generate --ci -w FILE`: a made-up pair.
cat >"$work/shim/pnpm" <<'EOF'
#!/bin/sh
[ "$1 $2 $3" = "tauri signer generate" ] || { echo "the stand-in pnpm only makes a key pair: pnpm $*" >&2; exit 1; }
for a; do out=$a; done
printf 'acme-updater-private-%s\n' "$STANDIN_MARK" >"$out"
printf 'acme-updater-public-%s\n' "$STANDIN_MARK" >"$out.pub"
EOF
chmod +x "$work/shim"/*
# A PATH with the stand-ins and the system's tools, and no real op, gh or
# pnpm behind them.
base=/usr/bin:/bin:/usr/sbin:/sbin
export STANDIN_LOG="$work/standins.log" STANDIN_OP_DIR="$work/op" STANDIN_GH_DIR="$work/gh"
export LEADLINE_REHEARSAL=1 STANDIN_MARK="$RANDOM$RANDOM"

# Values no script would print by chance, so that finding one is a leak.
issuer="acme-issuer-$STANDIN_MARK"
key_id="ACMEKEY$STANDIN_MARK"
p8="-----BEGIN PRIVATE KEY-----
acme-notary-$STANDIN_MARK
-----END PRIVATE KEY-----"
p12="acme-certificate-$STANDIN_MARK"
p12_password="acme-p12-password-$STANDIN_MARK"
updater="acme-updater-$STANDIN_MARK"
apple="$work/op/Personal/Berth Developer Id"

# fresh: 1Password as it should be, GitHub with nothing set, an empty log.
fresh() {
  rm -rf "$work/op" "$work/gh" "$work/tmp" "$STANDIN_LOG"
  mkdir -p "$apple" "$work/op/Personal/Leadline updater signing key" "$work/tmp"
  printf '%s' "$issuer" >"$apple/Issuer ID"
  printf '%s' "$key_id" >"$apple/Key ID"
  printf '%s\n' "$p8" >"$apple/AuthKey_$key_id.p8"
  printf '%s' "$p12" >"$apple/DeveloperID.p12"
  printf '%s' "$p12_password" >"$apple/p12 password"
  printf '%s' "$updater" >"$work/op/Personal/Leadline updater signing key/private key"
  : >"$STANDIN_LOG"
}

# try NAME SCRIPT ARGS…: runs a script with the stand-ins, its output in
# $work/out and its exit status in $status. TMPDIR is the test's, so what
# the script left behind can be seen.
try() {
  name=$1
  shift
  status=0
  TMPDIR="$work/tmp" PATH="${TRY_PATH:-$work/shim:$base}" "$@" >"$work/out" 2>&1 </dev/null || status=$?
  [ -z "${VERBOSE:-}" ] || { echo "--- $name (exit $status)"; sed -e "s|$work|<work>|g" "$work/out" | tail -"${VERBOSE}"; }
}
says() { grep -qF -e "$1" "$work/out" || fail "$name: it should say \"$1\"; it said: $(tail -3 "$work/out")"; }
# No secret in what was printed or in any stand-in's arguments.
quiet() {
  for secret in "acme-notary-$STANDIN_MARK" "$p12_password" "$updater" "$(printf '%s' "$p12" | base64)" "acme-updater-private-$STANDIN_MARK"; do
    ! grep -qF -e "$secret" "$work/out" "$STANDIN_LOG" || fail "$name: a secret was printed, or passed as an argument"
  done
}
nothing_set() { [ ! -d "$work/gh/secrets" ] || fail "$name: secrets were set: $(names "$work/gh/secrets")"; }
nothing_left() {
  left="$(find "$work/tmp" -mindepth 1 2>/dev/null | head -3)"
  [ -z "$left" ] || fail "$name: left behind in its temporary folder: $left"
}

secrets=$root/scripts/ci-secrets.sh

fresh
try "ci-secrets --dry-run" "$secrets" --dry-run
[ "$status" = 0 ] || fail "$name: exit $status: $(tail -3 "$work/out")"
for s in APPLE_CERTIFICATE APPLE_CERTIFICATE_PASSWORD APPLE_API_ISSUER APPLE_API_KEY APPLE_API_KEY_P8 TAURI_SIGNING_PRIVATE_KEY; do says "$s"; done
says "nothing was set"
nothing_set
quiet
nothing_left

fresh
try "ci-secrets --yes" "$secrets" --yes
[ "$status" = 0 ] || fail "$name: exit $status: $(tail -3 "$work/out")"
# set_to NAME VALUE: gh was given exactly this, on its standard input.
set_to() {
  want="$(printf '%s' "$2" | sum) $(printf '%s' "$2" | wc -c | tr -d ' ')"
  got="$(cat "$work/gh/secrets/$1" 2>/dev/null || echo "not set")"
  [ "$got" = "$want" ] || fail "$name: $1 was set to something else than 1Password holds ($got)"
}
set_to APPLE_API_ISSUER "$issuer"
set_to APPLE_API_KEY "$key_id"
set_to APPLE_API_KEY_P8 "$p8"
set_to APPLE_CERTIFICATE "$(printf '%s' "$p12" | base64 | tr -d '\n')"
set_to APPLE_CERTIFICATE_PASSWORD "$p12_password"
set_to TAURI_SIGNING_PRIVATE_KEY "$updater"
[ "$(find "$work/gh/secrets" -type f | wc -l | tr -d ' ')" = 6 ] || fail "$name: not six secrets: $(names "$work/gh/secrets")"
grep -q "^gh secret set APPLE_CERTIFICATE --repo sean-brydon/leadline$" "$STANDIN_LOG" || fail "$name: gh was not asked the way it should be: $(grep '^gh secret' "$STANDIN_LOG" | head -2)"
quiet
nothing_left
if [ "$(uname -s)" = Darwin ]; then
  # The certificate was tried in a keychain of its own, which is gone again.
  grep -q "^security import$" "$STANDIN_LOG" || fail "$name: the certificate was not tried in a keychain"
  grep -q "^security delete-keychain$" "$STANDIN_LOG" || fail "$name: the keychain made to try the certificate was not removed"
fi

fresh
try "ci-secrets, nobody to answer" "$secrets"
[ "$status" != 0 ] || fail "$name: it went on without a yes"
nothing_set

fresh
rm "$apple/p12 password"
try "ci-secrets, a field missing" "$secrets" --yes
[ "$status" != 0 ] || fail "$name: it went on"
says "the 1Password item has no such field"
says "p12 password"
nothing_set
nothing_left

fresh
: >"$apple/Issuer ID"
try "ci-secrets, a field blank" "$secrets" --yes
says "APPLE_API_ISSUER is empty in 1Password"
nothing_set

fresh
printf 'not a key' >"$apple/AuthKey_$key_id.p8"
try "ci-secrets, a file that is no key" "$secrets" --yes
says "APPLE_API_KEY_P8 is not a private key"
nothing_set

fresh
rm -r "$work/op/Personal/Leadline updater signing key"
try "ci-secrets, an item missing" "$secrets" --yes
says "there is no such 1Password item"
nothing_set

fresh
STANDIN_OP=signed-out try "ci-secrets, 1Password signed out" "$secrets" --yes
[ "$status" != 0 ] || fail "$name: it went on"
says "1Password is signed out"
nothing_set
nothing_left

fresh
STANDIN_GH=signed-out try "ci-secrets, gh signed out" "$secrets" --yes
says "gh is not signed in to GitHub"
nothing_set

fresh
mkdir -p "$work/no-op" && cp "$work/shim/gh" "$work/shim/security" "$work/no-op/"
TRY_PATH="$work/no-op:$base" try "ci-secrets, no op" "$secrets" --yes
says "1Password's command line (op) is not installed"
nothing_set

if [ "$(uname -s)" = Darwin ]; then
  fresh
  STANDIN_SECURITY=bad-password try "ci-secrets, a certificate that won't import" "$secrets" --yes
  [ "$status" != 0 ] || fail "$name: it went on"
  says "could not be imported"
  says "Nothing was set"
  nothing_set
  grep -q "^security delete-keychain$" "$STANDIN_LOG" || fail "$name: the keychain made to try the certificate was not removed"
  nothing_left
fi

# scripts/updater-key.sh, in a copy of the little of the repository it
# touches, so the real config is never written.
tree="$work/tree"
mkdir -p "$tree/scripts" "$tree/app/src-tauri" "$tree/app/node_modules"
cp "$root/scripts/updater-key.sh" "$root/scripts/op.sh" "$tree/scripts/"
key=$tree/scripts/updater-key.sh
conf=$tree/app/src-tauri/tauri.conf.json
new_tree() {
  fresh
  rm -r "$work/op/Personal/Leadline updater signing key"
  cp "$root/app/src-tauri/tauri.conf.json" "$conf"
  # The test is of a config that has no key yet, whatever the real one has.
  sed 's/"pubkey": "[^"]*"/"pubkey": "UPDATER-PUBLIC-KEY-NOT-SET"/' "$conf" >"$conf.new" && mv "$conf.new" "$conf"
}

new_tree
try "updater-key" "$key"
[ "$status" = 0 ] || fail "$name: exit $status: $(tail -3 "$work/out")"
grep -qF "\"pubkey\": \"acme-updater-public-$STANDIN_MARK\"" "$conf" || fail "$name: the public half is not in the config"
[ "$(cat "$work/op/Personal/Leadline updater signing key/private key" 2>/dev/null)" = "acme-updater-private-$STANDIN_MARK" ] || fail "$name: the private half is not in 1Password"
grep -q "^op item create Leadline updater signing key --vault=Personal (from stdin)$" "$STANDIN_LOG" || fail "$name: the item was not given to op on its standard input: $(grep '^op item' "$STANDIN_LOG")"
python3 -c 'import json, sys; json.load(open(sys.argv[1]))' "$conf" || fail "$name: the config is no longer JSON"
quiet
nothing_left

try "updater-key, a second time" "$key"
[ "$status" != 0 ] || fail "$name: it made a second key"
says "already has an updater public key"

new_tree
mkdir -p "$work/op/Personal/Leadline updater signing key"
printf 'someone-elses' >"$work/op/Personal/Leadline updater signing key/private key"
try "updater-key, a key already in 1Password" "$key"
[ "$status" != 0 ] || fail "$name: it went on"
says "1Password already has an updater key"
grep -q '"pubkey": "UPDATER-PUBLIC-KEY-NOT-SET"' "$conf" || fail "$name: the config was changed"

new_tree
STANDIN_OP=signed-out try "updater-key, 1Password signed out" "$key"
says "1Password is signed out"
grep -q '"pubkey": "UPDATER-PUBLIC-KEY-NOT-SET"' "$conf" || fail "$name: the config was changed"
nothing_left

new_tree
try "updater-key --file" "$key" --file "$work/kept.key"
[ "$status" = 0 ] || fail "$name: exit $status: $(tail -3 "$work/out")"
[ "$(cat "$work/kept.key" 2>/dev/null)" = "acme-updater-private-$STANDIN_MARK" ] || fail "$name: the private half is not in the file"
[ -z "$(find "$work/kept.key" -perm +077 2>/dev/null || find "$work/kept.key" -perm /077)" ] || fail "$name: the file can be read by others"
grep -qF "\"pubkey\": \"acme-updater-public-$STANDIN_MARK\"" "$conf" || fail "$name: the public half is not in the config"
! grep -q "^op " "$STANDIN_LOG" || fail "$name: 1Password was asked, and should not have been"
quiet
nothing_left

# scripts/publish.sh, on a Mac: every credential it can't get stops it
# before anything is built. GO names a program that fails, so a case that
# got past the credentials would stop at once and say so here.
if [ "$(uname -s)" = Darwin ]; then
  # In a copy of the scripts and of the four files that say the version, so
  # that the one case that gets as far as building touches no dist/ but its
  # own.
  mkdir -p "$work/release/app/src-tauri"
  cp -R "$root/scripts" "$work/release/scripts"
  cp "$root/VERSION" "$work/release/"
  cp "$root/app/package.json" "$work/release/app/"
  cp "$root/app/src-tauri/tauri.conf.json" "$root/app/src-tauri/Cargo.toml" "$work/release/app/src-tauri/"
  publish=$work/release/scripts/publish.sh
  # early NAME SAYS: publish.sh stopped, said this, built and left nothing.
  early() {
    [ "$status" != 0 ] || fail "$name: it went on"
    says "$1"
    ! grep -q "The macOS programs" "$work/out" || fail "$name: it got as far as building"
    quiet
    nothing_left
  }
  export GO=/usr/bin/false
  # A port nothing listens on, whatever is running on this Mac: publish.sh
  # wants its app's port free before it builds.
  export LEADLINE_APP_ADDR=127.0.0.1:1
  unset TAURI_SIGNING_PRIVATE_KEY APPLE_API_ISSUER APPLE_API_KEY APPLE_API_KEY_PATH APPLE_SIGNING_IDENTITY

  fresh
  TRY_PATH="$work/no-op:$base" try "publish, no op" "$publish"
  early "1Password's command line (op) is not installed"

  fresh
  STANDIN_OP=signed-out try "publish, 1Password signed out" "$publish"
  early "1Password is signed out"

  fresh
  rm "$apple/AuthKey_$key_id.p8"
  try "publish, a field missing" "$publish"
  early "the 1Password item has no such field"

  fresh
  rm -r "$work/op/Personal/Leadline updater signing key"
  try "publish, an item missing" "$publish"
  early "there is no such 1Password item"

  fresh
  : >"$apple/Key ID"
  try "publish, a field blank" "$publish"
  early "\"Key ID\" in 1Password is empty"

  fresh
  STANDIN_IDENTITIES="" try "publish, no signing identity" "$publish"
  early "there is no Developer ID Application identity in this Mac's keychain"

  fresh
  STANDIN_IDENTITIES="one two" try "publish, two signing identities" "$publish"
  early "set APPLE_SIGNING_IDENTITY: there are 2 Developer ID Application identities"

  # With every credential in hand it goes on to build (and stops there, at
  # the stand-in for Go), which shows the cases above stopped for their own
  # reasons. The key it wrote for the notary is gone all the same.
  fresh
  try "publish, every credential in hand" "$publish"
  [ "$status" != 0 ] || fail "$name: the stand-in for Go should have stopped it"
  says "signing as: Developer ID Application: Acme acme"
  says "The macOS programs"
  quiet
  nothing_left

  # Cut short while the notary's key is on disk: it is removed all the same.
  fresh
  name="publish, cut short"
  STANDIN_SLEEP=3 TMPDIR="$work/tmp" PATH="$work/shim:$base" "$publish" >"$work/out" 2>&1 </dev/null &
  pid=$!
  n=0
  until [ -n "$(find "$work/tmp" -name AuthKey.p8 2>/dev/null)" ]; do
    n=$((n + 1))
    [ "$n" -lt 100 ] || { fail "$name: the notary's key was never written"; break; }
    sleep 0.1
  done
  kill -TERM "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  nothing_left
fi

if [ "$failures" != 0 ]; then
  echo "credentials_test: $failures failure(s)" >&2
  exit 1
fi
echo "credentials_test: ok"
