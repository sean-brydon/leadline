#!/usr/bin/env bash
# Puts what the release workflow signs with into the repository's secrets on
# GitHub, read from 1Password. Run it once, and again when a certificate or
# a key is renewed.
#
#   scripts/ci-secrets.sh [--dry-run] [--yes]
#
#   secret                        read from (override with)
#   APPLE_CERTIFICATE             the file "DeveloperID.p12" of the Apple item, as base64
#                                 (LEADLINE_CERTIFICATE_OP)
#   APPLE_CERTIFICATE_PASSWORD    "p12 password" of the Apple item
#                                 (LEADLINE_CERTIFICATE_PASSWORD_OP)
#   APPLE_API_ISSUER              "Issuer ID" of the Apple item
#   APPLE_API_KEY                 "Key ID" of the Apple item
#   APPLE_API_KEY_P8              the file "AuthKey_<Key ID>.p8" of the Apple item
#   TAURI_SIGNING_PRIVATE_KEY     op://Personal/Leadline updater signing key/private key
#                                 (LEADLINE_UPDATER_KEY_OP)
#
# The Apple item is op://Personal/Berth Developer Id (LEADLINE_APPLE_OP_ITEM),
# the one scripts/publish.sh and Shipyard's releases read.
#
# It reads all six first, and looks at each as far as it can without showing
# it: none is empty, the API key is a private key, and the certificate goes
# into a keychain made for the purpose and comes out as a Developer ID
# Application identity, the way the workflow will use it. If any of that
# fails, nothing has been set. Then it lists what it will set, by name, and
# asks. --dry-run stops there. --yes doesn't ask.
#
# No value is printed, written to a file, or put on a command line: each
# goes to `gh secret set` on its standard input.
set -euo pipefail

dry="" yes=""
for a; do
  case "$a" in
    --dry-run) dry=1 ;;
    --yes) yes=1 ;;
    -h | --help) sed -n '2,/^set -euo/p' "$0" | sed -e '$d' -e 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "usage: scripts/ci-secrets.sh [--dry-run] [--yes]" >&2; exit 2 ;;
  esac
done

repo="${LEADLINE_REPO:-sean-brydon/leadline}"
apple_op="${LEADLINE_APPLE_OP_ITEM:-op://Personal/Berth Developer Id}"
updater_op="${LEADLINE_UPDATER_KEY_OP:-op://Personal/Leadline updater signing key/private key}"
cert_op="${LEADLINE_CERTIFICATE_OP:-$apple_op/DeveloperID.p12}"
cert_password_op="${LEADLINE_CERTIFICATE_PASSWORD_OP:-$apple_op/p12 password}"
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

die() { echo "ci-secrets: $*" >&2; exit 1; }

command -v op >/dev/null || die "1Password's command line (op) is not installed: brew install 1password-cli"
command -v gh >/dev/null || die "the GitHub command line (gh) is not installed: brew install gh"
gh auth status >/dev/null 2>&1 || die "gh is not signed in to GitHub: gh auth login"

tmp="$(mktemp -d "${TMPDIR:-/tmp}/leadline-secrets.XXXXXX")"
chmod 700 "$tmp"
export KEYCHAIN_PATH="$tmp/check.keychain-db"
cleanup() {
  [ ! -e "$KEYCHAIN_PATH" ] || scripts/ci-keychain.sh delete >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP
# shellcheck source=scripts/op.sh
. "$root/scripts/op.sh"

echo "Reading from 1Password (nothing is set yet):"
# need NAME VALUE REFERENCE: a value that is there and not blank.
need() {
  [ -n "$2" ] || die "$1 is empty in 1Password ($3). Nothing was set."
  echo "  $1  <-  $3"
}
issuer="$(op_read "$apple_op/Issuer ID" "APPLE_API_ISSUER")"
need APPLE_API_ISSUER "$issuer" "$apple_op/Issuer ID"
key_id="$(op_read "$apple_op/Key ID" "APPLE_API_KEY")"
need APPLE_API_KEY "$key_id" "$apple_op/Key ID"
p8="$(op_read "$apple_op/AuthKey_$key_id.p8" "APPLE_API_KEY_P8 (the item's AuthKey_<Key ID>.p8 file)")"
need APPLE_API_KEY_P8 "$p8" "$apple_op/AuthKey_<Key ID>.p8"
# The .p12 is not text, so it is base64 before it is ever held in a variable.
cert="$(op_read "$cert_op" "APPLE_CERTIFICATE (the Developer ID certificate, a .p12 with its private key)" | base64 | tr -d '\n')"
need APPLE_CERTIFICATE "$cert" "$cert_op"
cert_password="$(op_read "$cert_password_op" "APPLE_CERTIFICATE_PASSWORD")"
need APPLE_CERTIFICATE_PASSWORD "$cert_password" "$cert_password_op"
updater_key="$(op_read "$updater_op" "TAURI_SIGNING_PRIVATE_KEY (the updater signing key)")"
need TAURI_SIGNING_PRIVATE_KEY "$updater_key" "$updater_op"

echo "Looking at them:"
case "$p8" in
  *"BEGIN PRIVATE KEY"*) echo "  APPLE_API_KEY_P8 is a private key" ;;
  *) die "APPLE_API_KEY_P8 is not a private key: the file in 1Password should be the AuthKey_….p8 that App Store Connect gave, starting -----BEGIN PRIVATE KEY-----. Nothing was set." ;;
esac
# The certificate, the way the workflow will use it: into a keychain of its
# own, where it must turn out to be a Developer ID Application identity.
# That catches the two usual mistakes, a .p12 exported without its private
# key and a password that is not its password, here and not in a release.
if [ "$(uname -s)" = Darwin ]; then
  identity="$(APPLE_CERTIFICATE="$cert" APPLE_CERTIFICATE_PASSWORD="$cert_password" scripts/ci-keychain.sh create 2>"$tmp/keychain.err")" ||
    die "$(sed -n 's/^ci-keychain: //p' "$tmp/keychain.err" | tail -1) Nothing was set."
  scripts/ci-keychain.sh delete >/dev/null
  echo "  APPLE_CERTIFICATE opens with APPLE_CERTIFICATE_PASSWORD and holds: $identity"
else
  echo "  (APPLE_CERTIFICATE is not looked at off a Mac)"
fi

echo
echo "On $repo, these six repository secrets ${dry:+would }${dry:-will }be set, replacing any that are there:"
echo "  APPLE_CERTIFICATE  APPLE_CERTIFICATE_PASSWORD  APPLE_API_ISSUER  APPLE_API_KEY  APPLE_API_KEY_P8  TAURI_SIGNING_PRIVATE_KEY"
if [ -n "$dry" ]; then
  echo "ci-secrets: a dry run: nothing was set."
  exit 0
fi
if [ -z "$yes" ]; then
  printf 'Set them? [y/N] '
  read -r answer || answer=""
  case "$answer" in
    y | Y | yes) ;;
    *) echo "ci-secrets: nothing was set."; exit 1 ;;
  esac
fi

# put NAME VALUE: printf is the shell's own, so the value is in no process's
# arguments; gh reads it from the pipe.
put() {
  printf '%s' "$2" | gh secret set "$1" --repo "$repo" >/dev/null ||
    die "could not set $1 on $repo (do you administer the repository?). The secrets before it in the list above were set."
  echo "  set $1"
}
put APPLE_CERTIFICATE "$cert"
put APPLE_CERTIFICATE_PASSWORD "$cert_password"
put APPLE_API_ISSUER "$issuer"
put APPLE_API_KEY "$key_id"
put APPLE_API_KEY_P8 "$p8"
put TAURI_SIGNING_PRIVATE_KEY "$updater_key"
echo "ci-secrets: done. Rehearse a release from the Actions tab (release, Run workflow): it builds, signs and notarizes everything and publishes nothing."
