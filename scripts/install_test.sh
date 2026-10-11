#!/bin/sh
# Tests scripts/install.sh without a Linux machine, a network or a real
# release: the "release" is two tiny scripts in a tarball, the home folder
# is a temporary one, and uname is a stand-in that says Linux, so this runs
# the same on a Mac and in CI. What it checks is the installer's own logic:
# that it verifies before it installs, upgrades, uninstalls, and does
# nothing when cut short. The service is tested for real in containers
# (README, "Run it on a server"), never here: every run passes --no-service.
#
#   scripts/install_test.sh          sh
#   SH=dash scripts/install_test.sh  another shell
set -eu

here=$(cd "$(dirname "$0")" && pwd)
installer=$here/install.sh
shell=${SH:-sh}

work=$(mktemp -d "${TMPDIR:-/tmp}/leadline-install-test.XXXXXX")
trap 'rm -rf "$work"' EXIT

failures=0
fail() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}

# A uname that says this is a Linux machine with an arm64 processor.
mkdir -p "$work/shim"
cat >"$work/shim/uname" <<'EOF'
#!/bin/sh
case ${1:-} in
  -m) echo aarch64 ;;
  *) echo Linux ;;
esac
EOF
chmod +x "$work/shim/uname"

sum() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# release <version> <folder> makes leadline_<version>_linux_arm64.tar.gz and
# checksums.txt in the folder. Its "programs" only know their version.
release() {
  mkdir -p "$2" "$work/build"
  for b in leadline leadlined; do
    printf '#!/bin/sh\necho "%s %s (linux/arm64)"\n' "$b" "$1" >"$work/build/$b"
    chmod +x "$work/build/$b"
  done
  tar -czf "$2/leadline_$1_linux_arm64.tar.gz" -C "$work/build" leadline leadlined
  (cd "$2" && for f in leadline_*.tar.gz; do printf '%s  %s\n' "$(sum "$f")" "$f"; done >checksums.txt)
}

# install runs the installer in a home folder of its own. $out has what it
# printed and $code how it ended.
install() {
  code=0
  out=$(HOME=$home PATH="$work/shim:$PATH" XDG_CONFIG_HOME='' LEADLINE_VERSION='' LEADLINE_FROM='' LEADLINE_BIN_DIR='' \
    "$shell" "$installer" "$@" 2>&1) || code=$?
}

fresh() {
  home=$work/home.$1
  rm -rf "$home"
  mkdir -p "$home"
}

expect_ok() { [ "$code" -eq 0 ] || fail "$1: exit $code: $out"; }
expect_refused() {
  [ "$code" -ne 0 ] || fail "$1: was not refused: $out"
  case $out in
    *"Nothing was installed"*) ;;
    *) fail "$1: does not say that nothing was installed: $out" ;;
  esac
  if [ -e "$home/.local/bin/leadline" ] || [ -e "$home/.local/bin/leadlined" ]; then
    fail "$1: something was installed all the same"
  fi
  # Not even a half-unpacked folder is left behind.
  if [ -d "$home/.local/bin" ] && [ -n "$(ls -A "$home/.local/bin")" ]; then
    fail "$1: left $(ls -A "$home/.local/bin") in the bin folder"
  fi
}
expect_says() {
  case $out in
    *"$2"*) ;;
    *) fail "$1: does not say \"$2\": $out" ;;
  esac
}

release 9.9.9 "$work/rel"
tarball=$work/rel/leadline_9.9.9_linux_arm64.tar.gz

# Installs from a tarball with its checksums beside it.
fresh install
install --from "$tarball" --no-service
expect_ok "install"
expect_says "install" "Leadline 9.9.9 is installed."
expect_says "install" "Checksum matches"
expect_says "install" "is not on your PATH"
[ -x "$home/.local/bin/leadline" ] && [ -x "$home/.local/bin/leadlined" ] || fail "install: the programs are not there"
[ "$("$home/.local/bin/leadlined" --version)" = "leadlined 9.9.9 (linux/arm64)" ] || fail "install: not the program from the tarball"
[ -d "$home/.leadline" ] || fail "install: no data folder"

# From the folder, which also says which version there is.
fresh folder
install --from "$work/rel" --no-service
expect_ok "from a folder"
expect_says "from a folder" "Leadline 9.9.9 is installed."

# Run again with a newer release: an upgrade in place, data untouched.
release 9.9.10 "$work/rel"
echo kept >"$home/.leadline/marker"
install --from "$work/rel" --no-service
expect_ok "upgrade"
expect_says "upgrade" "Leadline 9.9.10 is installed (it was 9.9.9)."
[ "$(cat "$home/.leadline/marker")" = kept ] || fail "upgrade: the data folder was touched"
install --from "$work/rel" --version 9.9.9 --no-service
expect_ok "a named version"
expect_says "a named version" "Leadline 9.9.9 is installed (it was 9.9.10)."

# Uninstall keeps the data unless told to delete it.
install --uninstall
expect_ok "uninstall"
[ ! -e "$home/.local/bin/leadline" ] && [ ! -e "$home/.local/bin/leadlined" ] || fail "uninstall: the programs are still there"
[ -f "$home/.leadline/marker" ] || fail "uninstall: deleted the data without being asked"
expect_says "uninstall" "Your data is still in"
install --uninstall --purge
expect_ok "purge"
[ ! -e "$home/.leadline" ] || fail "purge: the data is still there"
install --uninstall
expect_says "uninstall twice" "is not installed here"
install --purge
[ "$code" -ne 0 ] || fail "--purge alone was accepted"

# A tarball that is not the one the release published is refused: one byte
# changed, cut short, or swapped for another file of the same name.
mkdir -p "$work/bad"
cp "$work/rel/checksums.txt" "$work/bad/"
bad=$work/bad/leadline_9.9.9_linux_arm64.tar.gz

fresh corrupt
size=$(wc -c <"$tarball" | tr -d ' ')
{
  head -c $((size / 2)) "$tarball"
  printf 'X'
  tail -c $((size - size / 2 - 1)) "$tarball"
} >"$bad"
[ "$(wc -c <"$bad" | tr -d ' ')" = "$size" ] || fail "the corrupted tarball is not the same size (the test is wrong)"
install --from "$bad" --no-service
expect_refused "one byte changed"
expect_says "one byte changed" "is not the file the release published"

fresh truncated
head -c $((size - 20)) "$tarball" >"$bad"
install --from "$bad" --no-service
expect_refused "cut short"

fresh swapped
release 6.6.6 "$work/other"
cp "$work/other/leadline_6.6.6_linux_arm64.tar.gz" "$bad"
install --from "$bad" --no-service
expect_refused "another file under its name"

# Nothing to check against is a refusal too, and --sha256 is the other way
# to give it.
fresh nosums
mkdir -p "$work/alone"
cp "$tarball" "$work/alone/"
install --from "$work/alone/leadline_9.9.9_linux_arm64.tar.gz" --no-service
expect_refused "no checksum"
install --from "$work/alone/leadline_9.9.9_linux_arm64.tar.gz" --sha256 "$(sum "$bad")" --no-service
expect_refused "the wrong --sha256"
install --from "$work/alone/leadline_9.9.9_linux_arm64.tar.gz" --sha256 nonsense --no-service
expect_refused "a --sha256 that is not one"
install --from "$work/alone/leadline_9.9.9_linux_arm64.tar.gz" --sha256 "$(sum "$tarball")" --no-service
expect_ok "the right --sha256"

# A tarball for another machine, and one that isn't a release at all.
fresh wrongarch
mkdir -p "$work/amd"
cp "$tarball" "$work/amd/leadline_9.9.9_linux_amd64.tar.gz"
(cd "$work/amd" && printf '%s  %s\n' "$(sum leadline_9.9.9_linux_amd64.tar.gz)" leadline_9.9.9_linux_amd64.tar.gz >checksums.txt)
install --from "$work/amd/leadline_9.9.9_linux_amd64.tar.gz" --no-service
expect_refused "another architecture"
expect_says "another architecture" "is not for this machine"

fresh notarelease
mkdir -p "$work/half"
tar -czf "$work/half/leadline_9.9.9_linux_arm64.tar.gz" -C "$work/build" leadline
(cd "$work/half" && printf '%s  %s\n' "$(sum leadline_9.9.9_linux_arm64.tar.gz)" leadline_9.9.9_linux_arm64.tar.gz >checksums.txt)
install --from "$work/half" --no-service
expect_refused "a tarball without the daemon"

# A program that won't run here is found out before anything is replaced.
fresh wontrun
install --from "$tarball" --no-service
printf '#!/bin/sh\nexit 127\n' >"$work/build/leadlined"
mkdir -p "$work/broken"
tar -czf "$work/broken/leadline_9.9.11_linux_arm64.tar.gz" -C "$work/build" leadline leadlined
(cd "$work/broken" && printf '%s  %s\n' "$(sum leadline_9.9.11_linux_arm64.tar.gz)" leadline_9.9.11_linux_arm64.tar.gz >checksums.txt)
install --from "$work/broken" --no-service
[ "$code" -ne 0 ] || fail "a daemon that won't run was installed"
expect_says "won't run" "Nothing was installed"
[ "$("$home/.local/bin/leadlined" --version)" = "leadlined 9.9.9 (linux/arm64)" ] || fail "won't run: the working install was replaced"
[ "$(find "$home/.local/bin" -mindepth 1 | wc -l | tr -d ' ')" = 2 ] || fail "won't run: left something behind in the bin folder"

# Piped and cut off anywhere, the script does nothing: no line of it acts
# until the last one has been read.
fresh piped
lines=$(wc -l <"$installer" | tr -d ' ')
for cut in 1 40 120 200 300 400 $((lines - 1)); do
  [ "$cut" -lt "$lines" ] || continue
  head -n "$cut" "$installer" | HOME=$home PATH="$work/shim:$PATH" "$shell" -s -- --from "$tarball" --no-service >/dev/null 2>&1 || true
  [ -z "$(ls -A "$home")" ] || fail "cut off after line $cut, the script still did something: $(ls -A "$home")"
done
# And whole, through a pipe, it installs.
code=0
out=$(HOME=$home PATH="$work/shim:$PATH" "$shell" -s -- --from "$tarball" --no-service <"$installer" 2>&1) || code=$?
expect_ok "piped"
[ -x "$home/.local/bin/leadlined" ] || fail "piped: nothing was installed"

if [ "$failures" -gt 0 ]; then
  echo "install_test: $failures failed" >&2
  exit 1
fi
echo "install_test: ok ($shell)"
