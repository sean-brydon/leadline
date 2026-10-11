#!/usr/bin/env bash
# Puts a signing certificate into a keychain of its own, for a machine that
# has none: the release workflow's macOS runner, and a rehearsal.
#
#   scripts/ci-keychain.sh create     make the keychain and import the certificate
#   scripts/ci-keychain.sh delete     remove it (run this whatever happened)
#
# `create` needs, in the environment:
#   KEYCHAIN_PATH                where to make the keychain (a file that
#                                isn't there yet, ending .keychain-db)
#   APPLE_CERTIFICATE            the Developer ID Application certificate
#                                with its private key, a .p12, as base64
#   APPLE_CERTIFICATE_PASSWORD   the .p12's password
# It prints the identity's name, the value for APPLE_SIGNING_IDENTITY, and
# in a workflow also sets that for the steps that follow.
#
# The keychain's own password is made up here and kept nowhere: nothing
# needs it again, since the keychain is unlocked, the tools that sign are
# let in below, and `delete` asks for no password.
#
# The .p12's password and the keychain's are arguments of `security`, which
# takes them no other way, so they show in that machine's process list for
# an instant. On a hosted runner nobody else is there to look.
#
# LEADLINE_REHEARSAL=1 takes a certificate that is no Developer ID (a
# rehearsal's stand-in), and fetches nothing from Apple.
set -euo pipefail

die() {
  if [ -n "${GITHUB_ACTIONS:-}" ]; then echo "::error::$*"; fi
  echo "ci-keychain: $*" >&2
  exit 1
}

: "${KEYCHAIN_PATH:?set KEYCHAIN_PATH to where the temporary keychain goes}"

case "${1:-}" in
  delete)
    # Deleting it also takes it off the list codesign searches.
    if [ -e "$KEYCHAIN_PATH" ]; then security delete-keychain "$KEYCHAIN_PATH" || true; fi
    rm -f "$KEYCHAIN_PATH.p12" "$KEYCHAIN_PATH.cer"
    exit 0
    ;;
  create) ;;
  *) echo "usage: scripts/ci-keychain.sh create|delete" >&2; exit 2 ;;
esac

: "${APPLE_CERTIFICATE:?set APPLE_CERTIFICATE to the base64 of the .p12}"
: "${APPLE_CERTIFICATE_PASSWORD:?set APPLE_CERTIFICATE_PASSWORD to the password of the .p12}"
[ ! -e "$KEYCHAIN_PATH" ] || die "$KEYCHAIN_PATH is already there; this never touches a keychain it didn't make"

umask 077
cert="$KEYCHAIN_PATH.p12"
trap 'rm -f "$cert"' EXIT
printf '%s' "$APPLE_CERTIFICATE" | base64 --decode >"$cert" 2>/dev/null ||
  die "APPLE_CERTIFICATE is not base64. It should be the output of: base64 -i DeveloperID.p12"
password="$(openssl rand -hex 24)"

security create-keychain -p "$password" "$KEYCHAIN_PATH"
# No lock on sleep or after a while: a release takes its time.
security set-keychain-settings -lut 21600 "$KEYCHAIN_PATH"
security unlock-keychain -p "$password" "$KEYCHAIN_PATH"
security import "$cert" -k "$KEYCHAIN_PATH" -f pkcs12 -P "$APPLE_CERTIFICATE_PASSWORD" -T /usr/bin/codesign -T /usr/bin/security >/dev/null ||
  die "the .p12 in APPLE_CERTIFICATE could not be imported: is APPLE_CERTIFICATE_PASSWORD its password, and was it exported with its private key?"
rm -f "$cert"
# The Developer ID intermediate, in case the .p12 left it out: without it
# the certificate is not trusted and codesign finds no identity.
if [ -z "${LEADLINE_REHEARSAL:-}" ]; then
  if curl -fsSL https://www.apple.com/certificateauthority/DeveloperIDG2CA.cer -o "$KEYCHAIN_PATH.cer"; then
    security import "$KEYCHAIN_PATH.cer" -k "$KEYCHAIN_PATH" >/dev/null 2>&1 || true
  fi
  rm -f "$KEYCHAIN_PATH.cer"
fi
# Let codesign use the key without a prompt no one can answer.
security set-key-partition-list -S apple-tool:,apple:,codesign: -s -k "$password" "$KEYCHAIN_PATH" >/dev/null
unset password

# codesign searches the user's list of keychains: put this one first, and
# keep the ones that were there.
existing=()
while IFS= read -r line; do
  line="${line#"${line%%[![:space:]]*}"}"
  line="${line#\"}"
  [ -z "${line%\"}" ] || existing+=("${line%\"}")
done < <(security list-keychains -d user)
security list-keychains -d user -s "$KEYCHAIN_PATH" ${existing[@]+"${existing[@]}"}

if [ -n "${LEADLINE_REHEARSAL:-}" ]; then
  # A stand-in is trusted by nothing, so it is not "valid"; it still signs.
  identity="$(security find-identity -p codesigning "$KEYCHAIN_PATH" | sed -n 's/^ *[0-9]*) [0-9A-F]* "\([^"]*\)".*/\1/p' | head -1)"
  [ -n "$identity" ] || die "the .p12 holds no code-signing identity"
else
  identity="$(security find-identity -v -p codesigning "$KEYCHAIN_PATH" | sed -n 's/.*"\(Developer ID Application: [^"]*\)".*/\1/p' | head -1)"
  [ -n "$identity" ] || die "APPLE_CERTIFICATE holds no valid Developer ID Application identity (export the certificate together with its private key, from Keychain Access, as a .p12)"
fi

[ -z "${GITHUB_ENV:-}" ] || echo "APPLE_SIGNING_IDENTITY=$identity" >>"$GITHUB_ENV"
echo "ci-keychain: signing as $identity" >&2
echo "$identity"
