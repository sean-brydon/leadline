#!/bin/sh
# Makes a release's files in dist/: for each platform a tarball of the two
# programs, leadline_<version>_<os>_<arch>.tar.gz, and checksums.txt, the
# SHA-256 of each, which scripts/install.sh checks a download against.
#
#   scripts/release.sh                          everything this machine builds
#   scripts/release.sh linux_arm64 darwin_arm64   only these
#   scripts/release.sh checksums                only checksums.txt, over the
#                                               tarballs already in dist/
#
# The version is the VERSION file's. Linux is built in a container
# (scripts/build-linux.sh), so on a Mac or on Linux; macOS only on a Mac,
# with the Mac's own clang. The release workflow runs this once per
# platform and then once more for the checksums.
#
# The macOS programs are signed and notarized when the environment says how,
# so that Gatekeeper lets them run on a Mac that didn't build them:
#   APPLE_SIGNING_IDENTITY    "Developer ID Application: …", in a keychain:
#                             both are signed with it, the hardened runtime
#                             and a secure timestamp
#   APPLE_API_ISSUER, APPLE_API_KEY, APPLE_API_KEY_PATH
#                             an App Store Connect API key: the signed
#                             programs are sent to Apple's notary service
#                             (scripts/notarize.sh) before they are packed
# Without them the tarballs are as before, and this says so: fine on the Mac
# that built them, stopped on any other. LEADLINE_REQUIRE_SIGNED=1 (the
# release workflow, scripts/publish.sh) makes that a failure instead.
#
# A bare program can't carry Apple's ticket the way an app does (there is
# nowhere in the file to staple it), so the first time one of these runs on
# a Mac, Gatekeeper asks Apple about it. Online that takes a moment and
# happens once. Offline, a copy that was downloaded with a browser, and so
# marked as quarantined, is refused until the Mac has been online; a copy
# fetched with curl carries no such mark and runs.
set -eu

cd "$(dirname "$0")/.."

GO=${GO:-go}
version=$(cat VERSION)
case $version in
  [0-9]*.[0-9]*.[0-9]*) ;;
  *) echo "release: VERSION holds \"$version\", which is not a version (as in 0.2.0)" >&2; exit 1 ;;
esac
commit=$(git rev-parse --short --verify HEAD 2>/dev/null || true)
pkg=github.com/sean-brydon/leadline/internal/version

# The time the files in the tarballs carry: the commit's, so that the same
# commit packs to the same bytes wherever it is built.
epoch=${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct 2>/dev/null || true)}
[ -n "$epoch" ] || epoch=$(date +%s)

if [ $# -eq 0 ]; then
  set -- linux_amd64 linux_arm64
  [ "$(uname -s)" != Darwin ] || set -- "$@" darwin_arm64 darwin_amd64
fi

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# pack puts dist/<target>/leadline and leadlined into the target's tarball,
# at its top, owned by nobody in particular.
pack() {
  dir=dist/$1
  name=leadline_${version}_$1.tar.gz
  for b in leadline leadlined; do
    [ -x "$dir/$b" ] || { echo "release: $dir/$b was not built" >&2; exit 1; }
  done
  if tar --version 2>/dev/null | grep -q GNU; then
    tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="@$epoch" \
      -cf - -C "$dir" leadline leadlined | gzip -9n >"dist/$name"
  else
    # bsdtar, as on a Mac: it takes the files' own times, and must be told
    # to leave out what Finder knows about them.
    touch -t "$(date -r "$epoch" +%Y%m%d%H%M.%S)" "$dir/leadline" "$dir/leadlined"
    COPYFILE_DISABLE=1 tar --uid 0 --gid 0 --numeric-owner --no-xattrs \
      -cf - -C "$dir" leadline leadlined | gzip -9n >"dist/$name"
  fi
  echo "release: dist/$name ($(du -h "dist/$name" | awk '{print $1}'))" >&2
}

# What a signed macOS program is called in its signature. Without a name of
# its own codesign would use the file's.
mac_id() {
  case $1 in
    leadlined) echo io.brydon.leadline.daemon ;;
    *) echo io.brydon.leadline.cli ;;
  esac
}

# sign_mac signs dist/<target>/leadline and leadlined, or says that it
# didn't. The hardened runtime is what notarization asks for; neither
# program needs an entitlement under it (AGENTS.md says how that was
# checked), so none is given. Signed tarballs are no longer the same bytes
# from one build to the next: each signature carries the time it was made.
sign_mac() {
  if [ -z "${APPLE_SIGNING_IDENTITY:-}" ]; then
    [ -z "${LEADLINE_REQUIRE_SIGNED:-}" ] || { echo "release: $1 must be signed, and APPLE_SIGNING_IDENTITY is not set" >&2; exit 1; }
    echo "release: $1 is NOT signed (APPLE_SIGNING_IDENTITY is not set): it runs on this Mac, and Gatekeeper stops it on any other" >&2
    return
  fi
  for b in leadline leadlined; do
    codesign --force --options runtime --timestamp --identifier "$(mac_id "$b")" \
      --sign "$APPLE_SIGNING_IDENTITY" "dist/$1/$b" ||
      { echo "release: could not sign dist/$1/$b as \"$APPLE_SIGNING_IDENTITY\": is that identity, with its private key, in a keychain that is unlocked?" >&2; exit 1; }
  done
}

# notarize_mac sends the signed programs of every macOS target just built to
# Apple in one zip, since Apple answers per submission, not per file.
notarized=no
notarize_mac() {
  [ -n "${APPLE_SIGNING_IDENTITY:-}" ] || return 0
  if [ -z "${APPLE_API_KEY:-}" ] || [ -z "${APPLE_API_ISSUER:-}" ] || [ -z "${APPLE_API_KEY_PATH:-}" ]; then
    [ -z "${LEADLINE_REQUIRE_SIGNED:-}" ] || { echo "release: the macOS programs must be notarized, and APPLE_API_KEY, APPLE_API_ISSUER and APPLE_API_KEY_PATH are not all set" >&2; exit 1; }
    echo "release: the macOS programs are signed but NOT notarized (no APPLE_API_KEY, APPLE_API_ISSUER and APPLE_API_KEY_PATH): Gatekeeper still stops them on another Mac" >&2
    return 0
  fi
  zip=$(mktemp -d "${TMPDIR:-/tmp}/leadline-notarize.XXXXXX")
  files=""
  for t; do files="$files $t/leadline $t/leadlined"; done
  # shellcheck disable=SC2086 # a list of paths with no spaces in them
  (cd dist && zip -q "$zip/leadline-macos.zip" $files)
  scripts/notarize.sh "$zip/leadline-macos.zip" || { rm -rf "$zip"; exit 1; }
  rm -rf "$zip"
  notarized=yes
}

# check_mac looks at the two programs as they will be unpacked on someone
# else's Mac, and fails the release if they would be stopped there.
check_mac() {
  [ -n "${APPLE_SIGNING_IDENTITY:-}" ] || return 0
  for b in leadline leadlined; do
    f=dist/$1/$b
    codesign --verify --strict "$f" || { echo "release: $f: the signature does not verify" >&2; exit 1; }
    info=$(codesign -dv --verbose=2 "$f" 2>&1)
    echo "$info" | grep -Eq '^CodeDirectory .*flags=.*runtime' ||
      { echo "release: $f lacks the hardened runtime, which notarization requires" >&2; exit 1; }
    # A rehearsal signs with a stand-in certificate and stays off the
    # network, so it has no Developer ID, no timestamp and no ticket.
    [ -z "${LEADLINE_REHEARSAL:-}" ] || continue
    echo "$info" | grep -q '^Authority=Developer ID Application:' ||
      { echo "release: $f is not signed with a Developer ID (\"$APPLE_SIGNING_IDENTITY\" is something else), so Gatekeeper would refuse it" >&2; exit 1; }
    echo "$info" | grep -q '^Timestamp=' ||
      { echo "release: $f's signature has no secure timestamp, which notarization requires" >&2; exit 1; }
    [ "$notarized" = yes ] || continue
    # Apple's ticket for it, asked of Apple: what Gatekeeper does on a first
    # run. The ticket can take a moment to be there after "Accepted".
    tries=0
    until codesign --verify --strict -R=notarized --check-notarization "$f" 2>/dev/null; do
      tries=$((tries + 1))
      [ "$tries" -lt 6 ] || { echo "release: Apple accepted $f but has no notarization ticket for it (asked $tries times)" >&2; exit 1; }
      sleep 10
    done
  done
  # And they still run, signed: on this Mac's own kind of processor, or
  # under Rosetta when it is there.
  case $1 in
    darwin_arm64) run="" ; [ "$(uname -m)" = arm64 ] || return 0 ;;
    *) run="" ; [ "$(uname -m)" = x86_64 ] || { arch -x86_64 /usr/bin/true 2>/dev/null || return 0; run="arch -x86_64"; } ;;
  esac
  # shellcheck disable=SC2086 # nothing, or the two words that run it under Rosetta
  said=$($run "dist/$1/leadlined" --version) || { echo "release: the signed dist/$1/leadlined does not start" >&2; exit 1; }
  case $said in "leadlined $version"*) ;; *)
    echo "release: dist/$1/leadlined says \"$said\", not version $version" >&2; exit 1 ;;
  esac
}

mkdir -p dist
mac=""
for target; do
  # A tarball in dist/ is one that was built, signed and checked: an older
  # one must not be left standing in for a build that fails below.
  rm -f "dist/leadline_${version}_$target.tar.gz"
  case $target in
    checksums) ;;
    linux_amd64 | linux_arm64)
      scripts/build-linux.sh "${target#linux_}"
      pack "$target"
      ;;
    darwin_amd64 | darwin_arm64)
      [ "$(uname -s)" = Darwin ] || { echo "release: $target is built on a Mac (DuckDB is linked in with the Mac's own compiler)" >&2; exit 1; }
      echo "release: $target: leadline and leadlined $version" >&2
      # Without this the programs ask for the macOS of the Mac that built
      # them, or newer. 13 is the oldest that Go itself still runs on, and
      # DuckDB's library asks for less (11).
      min=-mmacosx-version-min=13.0
      CGO_CFLAGS="-O2 -g $min" CGO_CXXFLAGS="-O2 -g $min" CGO_LDFLAGS="-O2 -g $min" \
        CGO_ENABLED=1 GOOS=darwin GOARCH=${target#darwin_} "$GO" build -trimpath -buildvcs=false \
        -ldflags "-s -w -X $pkg.Version=$version -X $pkg.Commit=$commit" \
        -o "dist/$target/" ./cmd/leadline ./cmd/leadlined
      sign_mac "$target"
      mac="$mac $target"
      ;;
    *)
      echo "release: don't know the target $target (linux_amd64, linux_arm64, darwin_arm64, darwin_amd64, checksums)" >&2
      exit 2
      ;;
  esac
done

# The macOS programs are packed only once Apple has accepted them: a tarball
# in dist/ is one that may be released.
if [ -n "$mac" ]; then
  # shellcheck disable=SC2086 # the targets, one a word
  notarize_mac $mac
  for target in $mac; do
    check_mac "$target"
    pack "$target"
  done
fi

# Always over every tarball of this version that is here, so that building
# one more platform never leaves the list short.
(
  cd dist
  found=no
  for f in leadline_"$version"_*.tar.gz; do
    [ -f "$f" ] || continue
    found=yes
    printf '%s  %s\n' "$(sha256 "$f")" "$f"
  done >checksums.txt.new
  [ "$found" = yes ] || { rm -f checksums.txt.new; echo "release: there are no leadline_${version}_*.tar.gz in dist/" >&2; exit 1; }
  mv -f checksums.txt.new checksums.txt
)
echo "release: dist/checksums.txt" >&2
cat dist/checksums.txt
