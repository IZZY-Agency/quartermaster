#!/usr/bin/env bash
# Build and pack the release archives of one version, one per platform:
#
#   quartermaster-<version>-macos-arm64.tar.gz    quartermaster-<version>-macos-x86_64.tar.gz
#   quartermaster-<version>-linux-arm64.tar.gz    quartermaster-<version>-linux-x86_64.tar.gz
#
# Each holds qm (0755), LICENSE, THIRD_PARTY_NOTICES.md, README.md and the
# worker plugin at share/quartermaster/plugins/qm-worker/; the macOS ones also
# hold the Swift privacy helper qm-privcheck (0755), built for that
# architecture from swift/privcheck.swift. Each archive comes with a
# <file>.sha256 in `shasum -a 256` format (`<hex>  <file>`), so
# `shasum -a 256 -c <file>.sha256` (or `sha256sum -c`) checks a download.
#
#   scripts/package.sh <version> <outdir>
#
# `make release` calls this with RELEASE_VERSION from the Makefile.
#
# The Go binaries are reproducible: CGO off, -trimpath, -buildvcs=false,
# stripped, the version and commit set by -ldflags. The archives are too, but
# for the Swift helper (swiftc output is not reproducible): fixed entry order
# and mtime, uid/gid 0, no owner names, no extended attributes, no gzip
# timestamp, modes 0755 for folders and executables and 0644 for the rest. Two
# runs on one commit give the same Linux archives, and macOS archives that
# differ only in qm-privcheck.
#
# Environment:
#   COMMIT          the commit printed by --version (default: git's short HEAD)
#   SOURCE_DATE_EPOCH
#                   mtime of the archive entries (default: the commit date of HEAD)
#   SIGN_IDENTITY   a Developer ID Application identity. On macOS each darwin
#                   binary (qm and qm-privcheck) is signed with it (hardened
#                   runtime, timestamp). Empty: no signing step (the linkers
#                   still give arm64 binaries the ad hoc signature macOS
#                   requires).
#   NOTARY_PROFILE  a notarytool keychain profile. With SIGN_IDENTITY, each
#                   signed binary goes through scripts/notarize.sh.
#   SWIFTC          the Swift compiler (default: swiftc on PATH)
#   PACKAGE_DEV=1   a development build: skip the release checks below
#                   (local tests only). Its archives are named
#                   quartermaster-dev-<version>-... and qm prints "qm
#                   dev-<version> (<commit>)", so it is never taken for a release.
#   PACKAGE_ALLOW_DIRTY=1
#                   build from a working tree with uncommitted changes (local
#                   tests only, with PACKAGE_DEV=1; a release is built from a clean tree)
#
# A release build (no PACKAGE_DEV) is made only in a clone of the public
# repository (origin is exactly its URL) whose HEAD is the tag v<version>, so
# the binaries print the public commit and nothing private can be packed.
set -euo pipefail

usage="usage: scripts/package.sh <version> <outdir>"
VERSION="${1:?$usage}"
OUTDIR_ARG="${2:?$usage}"
SIGN_IDENTITY="${SIGN_IDENTITY:-}"
NOTARY_PROFILE="${NOTARY_PROFILE:-}"
MODULE="github.com/IZZY-Agency/quartermaster"
PLUGIN="plugins/qm-worker"
HELPER_SRC="swift/privcheck.swift"
# The oldest macOS the helper runs on: Foundation Models needs macOS 26.
HELPER_MACOS_MIN="26.0"

fail() { echo "package: $*" >&2; exit 1; }
step() { echo "==> $*"; }

# The output folder is the caller's: resolved before anything moves, created
# only once every check has passed.
case "$OUTDIR_ARG" in /*) OUTDIR="$OUTDIR_ARG" ;; *) OUTDIR="$PWD/$OUTDIR_ARG" ;; esac
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$ROOT"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]] || fail "bad version: '$VERSION' (want 1.2.3)"
if [ "${PACKAGE_ALLOW_DIRTY:-0}" = 1 ] && [ "${PACKAGE_DEV:-0}" != 1 ]; then
  fail "PACKAGE_ALLOW_DIRTY=1 is for development builds only (with PACKAGE_DEV=1)"
fi
for f in LICENSE THIRD_PARTY_NOTICES.md README.md cmd/qm/main.go "$HELPER_SRC" "$PLUGIN/.claude-plugin/plugin.json"; do
  [ -f "$f" ] || fail "$f is missing in $ROOT"
done
if [ "${PACKAGE_DEV:-0}" != 1 ]; then
  origin="$(git config --get-all remote.origin.url 2>/dev/null || true)"
  [ "$origin" = "https://github.com/IZZY-Agency/quartermaster.git" ] \
    || fail "a release is built in a clone of the public repository (origin here: '${origin:-none}'); PACKAGE_DEV=1 makes a local test build"
  git tag --points-at HEAD | grep -qx "v$VERSION" \
    || fail "HEAD is not the tag v$VERSION (tags here: '$(git tag --points-at HEAD | tr '\n' ' ')'); PACKAGE_DEV=1 makes a local test build"
fi
# A development build never looks like a release: its archives and its
# --version say dev-<version>.
SHOWN="$VERSION"
if [ "${PACKAGE_DEV:-0}" = 1 ]; then SHOWN="dev-$VERSION"; fi
# The macOS archives carry the Swift helper. (The tests put a stand-in swiftc
# first on PATH, so the archives can be packed on Linux too.)
# SWIFTC names another compiler. /usr/bin/swiftc ships with macOS as a shim
# that works only with the Xcode command-line tools installed.
SWIFTC="${SWIFTC:-swiftc}"
swiftc_path="$(command -v "$SWIFTC" 2>/dev/null || true)"
if [ -z "$swiftc_path" ] || { [ "$swiftc_path" = /usr/bin/swiftc ] && ! xcode-select -p >/dev/null 2>&1; }; then
  fail "no Swift compiler ($SWIFTC): the macOS archives carry the Swift helper (build on a Mac with the Xcode command-line tools)"
fi
# The tree check reads git without the user's config: a global
# status.showUntrackedFiles=no or excludes file could hide a file that go
# build compiles. Go sources the ignore rules hide are refused too.
hgit() { GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 git -c status.showUntrackedFiles=all "$@"; }
if [ "${PACKAGE_ALLOW_DIRTY:-0}" != 1 ]; then
  [ -z "$(hgit status --porcelain --untracked-files=all)" ] || fail "the working tree is not clean: commit first, a release is built from a commit"
  hidden_go="$(hgit ls-files --others --ignored --exclude-standard -- '*.go' go.mod go.sum)"
  [ -z "$hidden_go" ] || fail "ignored Go files would be compiled in: $hidden_go"
fi

host_os="$(uname -s)"
if [ -n "$NOTARY_PROFILE" ] && [ -z "$SIGN_IDENTITY" ]; then
  fail "NOTARY_PROFILE needs SIGN_IDENTITY (an unsigned binary cannot be notarized)"
fi
if [ -n "$SIGN_IDENTITY" ] && [ "$host_os" != Darwin ]; then
  fail "signing needs macOS (codesign); unset SIGN_IDENTITY for an unsigned build"
fi

COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || true)}"
[[ "$COMMIT" =~ ^[0-9a-f]{7,40}$ ]] || fail "bad commit: '${COMMIT:-none}'"

epoch="${SOURCE_DATE_EPOCH:-}"
if [ -z "$epoch" ]; then epoch="$(git log -1 --format=%ct 2>/dev/null || true)"; fi
case "$epoch" in *[!0-9]*|"") fail "SOURCE_DATE_EPOCH must be a Unix timestamp, got '$epoch'" ;; esac
# `touch -t` wants [[CC]YY]MMDDhhmm[.SS]: format in UTC, apply in UTC.
if date -u -r "$epoch" +%Y >/dev/null 2>&1; then
  stamp="$(date -u -r "$epoch" +%Y%m%d%H%M.%S)"    # BSD date
else
  stamp="$(date -u -d "@$epoch" +%Y%m%d%H%M.%S)"   # GNU date
fi

tar_flavor="$(tar --version 2>/dev/null | head -1 || true)"
case "$tar_flavor" in
  *"GNU tar"*)
    TAR_FLAGS=(--format=ustar --owner=0 --group=0 --numeric-owner --no-xattrs --no-recursion) ;;
  *bsdtar*|*libarchive*)
    TAR_FLAGS=(--format ustar --uid 0 --gid 0 --numeric-owner --no-xattrs --no-acls --no-fflags --no-recursion)
    if [ "$host_os" = Darwin ]; then TAR_FLAGS+=(--no-mac-metadata); fi ;;
  *) fail "unrecognised tar (${tar_flavor:-no --version}); GNU tar or bsdtar is needed" ;;
esac
if command -v shasum >/dev/null 2>&1; then SHA=(shasum -a 256)
elif command -v sha256sum >/dev/null 2>&1; then SHA=(sha256sum)
else fail "neither shasum nor sha256sum is on PATH"
fi

mkdir -p "$OUTDIR" || fail "cannot create $OUTDIR"
OUTDIR="$(cd "$OUTDIR" && pwd -P)"
WORK="$(mktemp -d)" || fail "could not create a scratch folder"
WORK="$(cd "$WORK" && pwd -P)"
case "$WORK" in /?*) ;; *) fail "bad scratch folder: $WORK" ;; esac
trap 'rm -rf "$WORK"' EXIT
host_goos="$(go env GOOS)"
host_goarch="$(go env GOARCH)"
ldflags="-s -w -X $MODULE/internal/buildinfo.Version=$SHOWN -X $MODULE/internal/buildinfo.Commit=$COMMIT"

# sign <file> <archive name>: sign (and notarize) one macOS binary when a
# signing identity is set. <launch argument> is what notarize.sh runs it with.
sign() {  # <file> <archive name> <launch argument>
  [ -n "$SIGN_IDENTITY" ] || return 0
  step "sign $2: ${1##*/}"
  codesign --force --timestamp --options runtime -s "$SIGN_IDENTITY" "$1"
  if [ -n "$NOTARY_PROFILE" ]; then
    step "notarize $2: ${1##*/}"
    bash scripts/notarize.sh "$1" "$NOTARY_PROFILE" "$2-${1##*/}" "$3"
  fi
}

# The plugin, as git has it: every tracked file under $PLUGIN.
PLUGIN_FILES="$WORK/plugin-files"
git ls-files -z -- "$PLUGIN" | LC_ALL=C tr '\0' '\n' | LC_ALL=C sort > "$PLUGIN_FILES"
[ -s "$PLUGIN_FILES" ] || fail "$PLUGIN holds no tracked file"
if LC_ALL=C grep -q '[[:space:]]' "$PLUGIN_FILES"; then fail "a file name in $PLUGIN has white space"; fi

# "<GOOS>/<GOARCH>:<archive suffix>"
for t in darwin/arm64:macos-arm64 darwin/amd64:macos-x86_64 linux/arm64:linux-arm64 linux/amd64:linux-x86_64; do
  goos="${t%%/*}"
  rest="${t#*/}"
  goarch="${rest%%:*}"
  name="quartermaster-$SHOWN-${rest#*:}"
  stage="$WORK/$name"
  mkdir -p "$stage"
  entries=(LICENSE README.md THIRD_PARTY_NOTICES.md qm)

  step "build $name ($goos/$goarch)"
  # The baseline instruction sets, no experiments, no extra flags and no
  # workspace, whatever the shell exports: a GOAMD64=v3 build would not run
  # on older x86_64 machines, and GOFLAGS=-tags=... would change the code.
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" GOAMD64=v1 GOARM64=v8.0 GOEXPERIMENT= GOFLAGS= GOWORK=off \
    go build -trimpath -buildvcs=false -ldflags "$ldflags" -o "$stage/qm" ./cmd/qm

  if [ "$goos" = darwin ]; then
    sign "$stage/qm" "$name" --version
    case "$goarch" in amd64) sarch=x86_64 ;; *) sarch="$goarch" ;; esac
    step "build $name: qm-privcheck ($sarch)"
    "$SWIFTC" -O -target "$sarch-apple-macos$HELPER_MACOS_MIN" "$HELPER_SRC" -o "$stage/qm-privcheck" \
      || fail "could not build the Swift helper for $sarch"
    # The helper links system frameworks and libraries only (the notices
    # say so): every library it loads is in /usr/lib or /System/Library.
    if command -v otool >/dev/null 2>&1 && otool -h "$stage/qm-privcheck" >/dev/null 2>&1; then
      libs="$(otool -L "$stage/qm-privcheck" | tail -n +2 | awk '{ print $1 }')"
      others="$(printf '%s\n' "$libs" | grep -vE '^(/usr/lib/|/System/Library/)' || true)"
      [ -z "$others" ] || fail "qm-privcheck links libraries outside the system: $others"
    fi
    sign "$stage/qm-privcheck" "$name" status
    entries+=(qm-privcheck)
  fi

  # The native build must print the version it was built with.
  if [ "$goos" = "$host_goos" ] && [ "$goarch" = "$host_goarch" ]; then
    got="$("$stage/qm" --version)"
    [ "$got" = "qm $SHOWN ($COMMIT)" ] || fail "$name prints '$got', expected 'qm $SHOWN ($COMMIT)'"
  fi

  cp LICENSE README.md THIRD_PARTY_NOTICES.md "$stage/"
  dest="share/quartermaster/plugins/qm-worker"
  while IFS= read -r f; do
    mkdir -p "$stage/$dest/$(dirname "${f#"$PLUGIN"/}")"
    cp "$f" "$stage/$dest/${f#"$PLUGIN"/}"
  done < "$PLUGIN_FILES"
  # Modes and times: folders and executables 0755, the rest 0644; one mtime.
  (cd "$stage" && find . -type d -exec chmod 0755 {} + && find . -type f -exec chmod 0644 {} +)
  chmod 0755 "$stage/qm"
  if [ -f "$stage/qm-privcheck" ]; then chmod 0755 "$stage/qm-privcheck"; fi
  while IFS= read -r f; do
    if [ -x "$f" ]; then chmod 0755 "$stage/$dest/${f#"$PLUGIN"/}"; fi
  done < "$PLUGIN_FILES"
  (cd "$stage" && find . -exec env TZ=UTC touch -t "$stamp" {} +)

  # The entries in a fixed order: the top-level files, then share/ folder by
  # folder, each folder before what it holds.
  list="$WORK/$name.list"
  printf '%s\n' "${entries[@]}" | LC_ALL=C sort > "$list"
  (cd "$stage" && find share | LC_ALL=C sort) >> "$list"
  archive="$OUTDIR/$name.tar.gz"
  rm -f "$archive" "$archive.sha256"
  (cd "$stage" && tar "${TAR_FLAGS[@]}" -cf - -T "$list") | gzip -n -9 > "$archive"
  (cd "$OUTDIR" && "${SHA[@]}" "$name.tar.gz" > "$name.tar.gz.sha256")
done

step "release archives: $OUTDIR"
(cd "$OUTDIR" && cat quartermaster-"$SHOWN"-*.tar.gz.sha256)
