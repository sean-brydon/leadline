#!/usr/bin/env bash
# What the release tests share: the stand-ins a release is rehearsed with.
# scripts/release-test/rehearse.sh and updater-test.sh source this; it is not
# run by itself. They set root (the repository) and work (a folder of their
# own, private, which they remove), and call standins_remove as they exit.
: "${root:?scripts/release-test/lib.sh is sourced by a test that knows the repository}"
: "${work:?scripts/release-test/lib.sh is sourced by a test that has made its work folder}"

# The name of the stand-in certificate. No Developer ID has a name like it.
identity="Leadline Rehearsal (not a Developer ID)"
export KEYCHAIN_PATH="$work/rehearsal.keychain-db"

# standin_identity: a certificate for code signing, signed by itself and
# good for two days, in a keychain of its own (scripts/ci-keychain.sh, as
# the release workflow uses it). It signs; nothing trusts it.
standin_identity() {
  local made
  mkdir -p "$work/cert"
  cat >"$work/cert/req.cnf" <<CNF
[req]
distinguished_name = dn
x509_extensions = ext
prompt = no
[dn]
CN = $identity
O = Acme Rehearsal
[ext]
basicConstraints = critical,CA:false
keyUsage = critical,digitalSignature
extendedKeyUsage = critical,codeSigning
CNF
  # The system's own openssl: a newer one writes a .p12 that the keychain
  # can't read.
  /usr/bin/openssl req -x509 -newkey rsa:2048 -nodes -days 2 -config "$work/cert/req.cnf" \
    -keyout "$work/cert/key.pem" -out "$work/cert/cert.pem" 2>/dev/null
  /usr/bin/openssl pkcs12 -export -inkey "$work/cert/key.pem" -in "$work/cert/cert.pem" \
    -name "$identity" -passout pass:acme-rehearsal -out "$work/cert/standin.p12"
  keychains_before="$(security list-keychains -d user)"
  made="$(LEADLINE_REHEARSAL=1 APPLE_CERTIFICATE="$(base64 -i "$work/cert/standin.p12")" APPLE_CERTIFICATE_PASSWORD=acme-rehearsal "$root/scripts/ci-keychain.sh" create)"
  [ "$made" = "$identity" ] || { echo "the keychain holds \"$made\", not the stand-in certificate" >&2; return 1; }
  echo "the signing identity: $identity, in a keychain of its own"
}

# standin_updater_key: a key pair for the updater, in $work/updater.key and
# $work/updater.key.pub.
standin_updater_key() {
  [ -d "$root/app/node_modules" ] || (cd "$root/app" && CI=true pnpm install --frozen-lockfile)
  (cd "$root/app" && pnpm tauri signer generate --ci -w "$work/updater.key" >/dev/null 2>&1) ||
    { echo "could not make the stand-in updater key" >&2; return 1; }
  echo "the updater key: made here, its public half laid over the app's config"
}

# standins_remove: the keychain, and with it the certificate. The list of
# keychains is then what it was.
standins_remove() {
  LEADLINE_REHEARSAL=1 "$root/scripts/ci-keychain.sh" delete || true
  rm -rf "$work/cert" "$work/updater.key"
}

# keychains_as_before: whether the list of keychains is what it was before
# standin_identity (the stand-in's own aside).
keychains_as_before() {
  [ "$(security list-keychains -d user | grep -vF "$KEYCHAIN_PATH")" = "${keychains_before:-}" ]
}

# free_port: a port nothing is listening on now.
free_port() {
  python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])'
}
