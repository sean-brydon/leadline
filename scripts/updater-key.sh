#!/usr/bin/env bash
# Makes the app's updater key pair. Run it once, ever:
#
#   scripts/updater-key.sh                 the private half into 1Password
#   scripts/updater-key.sh --file PATH     the private half into a file you
#                                          then store yourself and delete
#
# The app only installs an update whose archive is signed with this key, so
# the two halves are kept in two places:
#
#   the public half    app/src-tauri/tauri.conf.json, plugins.updater.pubkey,
#                      in place of the placeholder. This writes it; commit it.
#   the private half   1Password, at
#                      op://Personal/Leadline updater signing key/private key
#                      (LEADLINE_UPDATER_KEY_OP names another place). That is
#                      where scripts/publish.sh reads it for a release from
#                      this Mac, and where scripts/ci-secrets.sh reads it to
#                      set the repository secret TAURI_SIGNING_PRIVATE_KEY for
#                      a release from CI. It is in no file and no commit.
#
# The key has no password: it is only ever kept in those two stores, which
# are both locked already, and a password would be one more secret kept
# beside it.
#
# There is no second run. Every app that is installed trusts the public key
# it was built with and nothing else: a new pair would leave all of them
# unable to update, and only a new download would fix each one. So this
# refuses when the config already has a key, or 1Password already has one.
# Losing the private half is the same accident, which is why it is not on
# this disk.
set -euo pipefail

file=""
case "${1:-}" in
  "") ;;
  --file) file="${2:?--file needs a path}" ;;
  -h | --help) sed -n '2,/^set -euo/p' "$0" | sed -e '$d' -e 's/^# \{0,1\}//'; exit 0 ;;
  *) echo "usage: scripts/updater-key.sh [--file PATH]" >&2; exit 2 ;;
esac

updater_op="${LEADLINE_UPDATER_KEY_OP:-op://Personal/Leadline updater signing key/private key}"
placeholder=UPDATER-PUBLIC-KEY-NOT-SET
conf=app/src-tauri/tauri.conf.json
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

die() { echo "updater-key: $*" >&2; exit 1; }

grep -q "\"pubkey\": \"$placeholder\"" "$conf" ||
  die "$conf already has an updater public key. Installed apps trust that key and no other, so there is nothing to make: a new pair would leave every one of them unable to update."

tmp="$(mktemp -d "${TMPDIR:-/tmp}/leadline-updater-key.XXXXXX")"
chmod 700 "$tmp"
trap 'rm -rf "$tmp"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP
# shellcheck source=scripts/op.sh
. "$root/scripts/op.sh"

if [ -n "$file" ]; then
  [ ! -e "$file" ] || die "$file is already there, and this overwrites nothing"
else
  command -v op >/dev/null || die "1Password's command line (op) is not installed (brew install 1password-cli). Or keep the private half yourself: scripts/updater-key.sh --file PATH"
  case "$updater_op" in
    op://*/*/*) ;;
    *) die "LEADLINE_UPDATER_KEY_OP must be op://Vault/Item/field, not $updater_op" ;;
  esac
  # There must be nothing there yet. Reading it is the only way to ask, and
  # what is read goes nowhere.
  if op read "$updater_op" >/dev/null 2>"$tmp/op.err"; then
    die "1Password already has an updater key ($updater_op), and $conf has none. If that key is Leadline's, put its public half in $conf by hand; this won't make a second one over it."
  fi
  case "$(op_said)" in
    *"isn't an item"* | *"could not find item"* | *"no item"*) ;;
    *"not signed in"* | *"not currently signed in"* | *"sign in"* | *"session expired"* | *"authorization"*)
      die "1Password is signed out. Sign in (op signin, or unlock the 1Password app with its command-line integration turned on) and run this again. op said: $(op_said)" ;;
    *) die "could not tell whether 1Password already has an updater key ($updater_op). op said: $(op_said)" ;;
  esac
fi

[ -d app/node_modules ] || (cd app && CI=true pnpm install --frozen-lockfile)
(cd app && pnpm tauri signer generate --ci -w "$tmp/updater.key" >/dev/null 2>&1) || die "the Tauri command line could not make a key pair (cd app && pnpm tauri signer generate --help)"
[ -s "$tmp/updater.key" ] && [ -s "$tmp/updater.key.pub" ] || die "the Tauri command line made no key pair"

if [ -n "$file" ]; then
  (umask 077 && cp "$tmp/updater.key" "$file")
  echo "updater-key: the private half is in $file, readable only by you."
  echo "  Store its contents in your password manager (scripts/publish.sh and scripts/ci-secrets.sh read"
  echo "  $updater_op), then delete the file."
else
  # The item goes to op as JSON on its standard input, so the key is on no
  # command line.
  path="${updater_op#op://}"
  vault="${path%%/*}"
  rest="${path#*/}"
  python3 - "${rest%%/*}" "${rest#*/}" "$tmp/updater.key" <<'PY' | op item create --vault="$vault" - >/dev/null 2>"$tmp/op.err" ||
import json, sys
title, field, key = sys.argv[1:4]
json.dump({
    "title": title,
    "category": "SECURE_NOTE",
    "fields": [
        {"id": "notesPlain", "type": "STRING", "purpose": "NOTES", "label": "notesPlain",
         "value": "Signs Leadline's app updates (scripts/updater-key.sh made it). The public half is in app/src-tauri/tauri.conf.json. Never replace it: installed apps trust this key only."},
        {"id": "private_key", "type": "CONCEALED", "label": field, "value": open(key).read().strip()},
    ],
}, sys.stdout)
PY
    die "1Password would not take the new item, so nothing was changed. op said: $(op_said). To keep the private half yourself instead: scripts/updater-key.sh --file PATH"
  # Read it back: a key that was stored wrongly is a key that is lost.
  [ "$(op_read "$updater_op" "the updater signing key, just stored")" = "$(cat "$tmp/updater.key")" ] ||
    die "the key read back from 1Password ($updater_op) is not the key that was made. Nothing in the repository was changed; delete that item and run this again."
  echo "updater-key: the private half is in 1Password ($updater_op), and nowhere else."
fi

pub="$(cat "$tmp/updater.key.pub")"
sed "s|\"pubkey\": \"$placeholder\"|\"pubkey\": \"$pub\"|" "$conf" >"$tmp/conf" && cat "$tmp/conf" >"$conf"
grep -qF "\"pubkey\": \"$pub\"" "$conf" || die "could not write the public half into $conf"
echo "updater-key: the public half is in $conf. Commit it:"
echo "  git commit -m \"feat(app): the updater's public key\" $conf"
echo "Then, for releases from CI: scripts/ci-secrets.sh"
