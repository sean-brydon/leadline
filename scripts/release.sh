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

mkdir -p dist
for target; do
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
      pack "$target"
      ;;
    *)
      echo "release: don't know the target $target (linux_amd64, linux_arm64, darwin_arm64, darwin_amd64, checksums)" >&2
      exit 2
      ;;
  esac
done

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
