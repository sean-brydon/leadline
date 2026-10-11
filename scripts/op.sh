#!/usr/bin/env bash
# Reading from 1Password, for the scripts that need a credential:
# scripts/publish.sh, scripts/ci-secrets.sh and scripts/updater-key.sh
# source this. It is not run by itself.
#
# The scripts that source it define die (a sentence to stderr, then exit 1)
# and tmp (a folder only this user can read, removed when the script exits).
: "${tmp:?scripts/op.sh is sourced by a script that has made its temporary folder}"

# op_read REFERENCE WHAT: the value at a reference (op://Vault/Item/field)
# on stdout, or a plain sentence about why not. WHAT names the value for a
# person ("the updater signing key"). op's own words go with the sentence:
# they name the item and the field, and never hold a value.
op_read() {
  local ref="$1" what="$2" said
  command -v op >/dev/null ||
    die "$what is not in the environment, and 1Password's command line (op) is not installed to read it from ($ref). Install it (brew install 1password-cli), or set the variable."
  op read "$ref" 2>"$tmp/op.err" && return 0
  said="$(op_said)"
  case "$said" in
    *"not signed in"* | *"not currently signed in"* | *"sign in"* | *"session expired"* | *"authorization"*)
      die "1Password is signed out, so $what could not be read ($ref). Sign in (op signin, or unlock the 1Password app with its command-line integration turned on) and run this again. op said: $said" ;;
    *"isn't a field"* | *"does not have a field"* | *"isn't a file"* | *"no file"*)
      die "the 1Password item has no such field, so $what could not be read ($ref). op said: $said" ;;
    *"isn't an item"* | *"isn't a vault"* | *"could not find item"* | *"no item"*)
      die "there is no such 1Password item, so $what could not be read ($ref). op said: $said" ;;
    *)
      die "could not read $what from 1Password ($ref). op said: ${said:-nothing}" ;;
  esac
}

# What op last complained of, without the date it puts in front.
op_said() {
  sed -e 's/^\[ERROR\] [0-9/]* [0-9:]* //' "$tmp/op.err" 2>/dev/null | head -3
}
