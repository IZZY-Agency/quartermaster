#!/usr/bin/env bash
# Tests for the release scripts: publish.sh, package.sh, verify-release.sh,
# render-formula.sh, tap.sh and release.sh, and the Makefile's changelog
# gate. Each case builds throwaway private, public and tap repositories from a
# small stand-in for quartermaster (same module path, same version line, a
# stand-in plugin and Swift helper), so the real repository, any real public
# checkout and the real tap are never touched, and nothing goes over the
# network: git may only use local transports, and the scripts read assets and
# history from local folders through their rehearsal switches. A stand-in
# swiftc, first on PATH, "builds" the helper as a small shell script, so the
# macOS archives are packed on Linux too.
#
#   bash scripts/release_test.sh
# shellcheck disable=SC2015,SC2016,SC2012 # ok() always returns 0; backticks are Markdown; ls lists known names
set -uo pipefail

# Hermetic git: no user or system config, no prompts, local transports only.
unset GIT_CONFIG GIT_CONFIG_PARAMETERS GIT_CONFIG_COUNT GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE
while IFS= read -r v; do unset "$v"; done < <(env | sed -n 's/^\(GIT_CONFIG_[A-Za-z0-9_]*\)=.*/\1/p')
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 GIT_TERMINAL_PROMPT=0
export GIT_ASKPASS=/usr/bin/false SSH_ASKPASS=/usr/bin/false GIT_ALLOW_PROTOCOL=file
export HOMEBREW_NO_AUTO_UPDATE=1
unset PUBLISH_TEST_TARGET PUBLISH_TEST_REMOTE PUBLISH_IDENT TAP_TEST_ASSETS_DIR TAP_TEST_REMOTE
unset SWIFTC SIGN_IDENTITY NOTARY_PROFILE COMMIT PACKAGE_ALLOW_DIRTY PACKAGE_DEV REQUIRE_SIGNATURE EXPECTED_TEAM_ID
unset MAKEFLAGS MFLAGS GNUMAKEFLAGS MAKELEVEL MAKEFILES

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
repo="$(cd "$here/.." && pwd -P)"
work="$(mktemp -d)" || { echo "release_test: mktemp failed" >&2; exit 2; }
work="$(cd "$work" && pwd -P)"
case "$work" in /?*) ;; *) echo "release_test: bad scratch folder" >&2; exit 2 ;; esac
trap 'rm -rf "$work"' EXIT

PUBLIC_URL="https://github.com/IZZY-Agency/quartermaster.git"
TAP_URL="https://github.com/IZZY-Agency/homebrew-tap.git"
IDENT="Test Publisher <publisher@example.com>"
V="9.9.9"
dd="docs"   # the unexported folder, never spelled as a path in this exported file
deny_word="zorblaxian"
PLUGIN_DIR="share/quartermaster/plugins/qm-worker"

# The stand-in Swift compiler: "builds" qm-privcheck as a shell script that
# answers status with "unavailable" (exit 3), or exits 1 when the source holds
# BROKEN. Each build differs (a random line), as swiftc's output does, so the
# rebuild check must leave the helper out.
fakeswift="$work/fake-swift"; mkdir -p "$fakeswift"
cat > "$fakeswift/swiftc" <<'EOF'
#!/bin/sh
out=""; target=""; src=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift ;;
    -target) target="$2"; shift ;;
    -O) ;;
    *) src="$1" ;;
  esac
  shift
done
[ -n "$out" ] && [ -n "$target" ] && [ -f "$src" ] || { echo "fake swiftc: bad arguments" >&2; exit 2; }
if grep -q BROKEN "$src"; then
  printf '#!/bin/sh\nexit 1\n' > "$out"
else
  printf '#!/bin/sh\n# %s %s\nif [ "$1" = status ]; then echo "unavailable: stand-in helper"; exit 3; fi\nexit 2\n' \
    "$target" "$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')" > "$out"
fi
chmod 755 "$out"
EOF
chmod +x "$fakeswift/swiftc"
export PATH="$fakeswift:$PATH"

failures=0
ok()  { echo "ok   $1"; }
failed_names=""
bad() { echo "FAIL $1"; printf '%s\n' "$2" | sed 's/^/    /'; failures=$((failures + 1)); failed_names="$failed_names
  - $1"; }
# expect <name> <exit> <needle or -> <command...>: the command exits <exit>
# and, unless the needle is "-", prints the needle.
expect() {
  local name="$1" want="$2" needle="$3" out rc
  shift 3
  out="$("$@" 2>&1)"; rc=$?
  if [[ "$rc" == "$want" && ( "$needle" == "-" || "$out" == *"$needle"* ) ]]; then
    ok "$name"
  else
    bad "$name" "exit $rc, want $want; want output: $needle
$out"
  fi
  LAST_OUT="$out"
}
commit_all() {
  git -C "$1" add -A
  git -C "$1" -c user.name=test -c user.email=test@example.com -c commit.gpgsign=false \
    commit -q --allow-empty -m "${2:-change}"
}
sha_line() { if command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1"; else sha256sum "$1"; fi; }

# A deny-list for the gate, outside every repository.
cfg="$work/cfg"
mkdir -p "$cfg/quartermaster-dev"
printf '%s\n' "$deny_word" > "$cfg/quartermaster-dev/denylist"
export XDG_CONFIG_HOME="$cfg"

# The allowlisted paths, as publish.sh names them.
read -r -a ALLOW <<<"$(sed -n '/^ALLOWLIST=(/,/^)/p' "$here/publish.sh" | sed -e '1d' -e '$d' -e 's/#.*//' | tr '\n' ' ')"

# make_private <name> [<path>]: a small private repository laid out like
# quartermaster: the exported paths, the release scripts, and private
# material beside them (docs/, CLAUDE.md, AGENTS.md, .claude/).
make_private() {
  local p="${2:-$work/private-$1}"
  mkdir -p "$p"/{cmd/qm,internal/buildinfo,swift,scripts/formula,.claude-plugin,.github/workflows,testdata} \
           "$p"/plugins/qm-worker/{.claude-plugin,hooks} "$p"/plugins/qm-coordinator/.claude-plugin \
           "$p/$dd/superpowers" "$p/.claude"
  cp "$here"/{publish.sh,package.sh,notarize.sh,verify-release.sh,tap.sh,render-formula.sh,release.sh,check-private.sh} "$p/scripts/"
  cp "$here/formula/quartermaster.rb.tmpl" "$p/scripts/formula/"
  cp "$repo/.gitignore" "$p/.gitignore"
  printf 'module github.com/IZZY-Agency/quartermaster\n\ngo 1.27\n' > "$p/go.mod"
  cat > "$p/internal/buildinfo/buildinfo.go" <<'EOF'
package buildinfo

var (
	Version = "dev"
	Commit  = "unknown"
)
EOF
  cat > "$p/cmd/qm/main.go" <<'EOF'
package main

import (
	"fmt"
	"os"

	"github.com/IZZY-Agency/quartermaster/internal/buildinfo"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Printf("qm %s (%s)\n", buildinfo.Version, buildinfo.Commit)
		return
	}
	fmt.Println("qm")
}
EOF
  echo '// the privacy helper' > "$p/swift/privcheck.swift"
  echo '{"name":"quartermaster","owner":{"name":"IZZY Agency"},"plugins":[{"name":"qm-coordinator","source":"./plugins/qm-coordinator"}]}' > "$p/.claude-plugin/marketplace.json"
  echo '{"name":"qm-worker"}' > "$p/plugins/qm-worker/.claude-plugin/plugin.json"
  echo '{"hooks":{}}' > "$p/plugins/qm-worker/hooks/hooks.json"
  echo '{"name":"qm-coordinator"}' > "$p/plugins/qm-coordinator/.claude-plugin/plugin.json"
  cat > "$p/Makefile" <<EOF
RELEASE_VERSION := $V

check:
	@test ! -f break-check || { echo "check broke"; exit 1; }
	@echo "check passed with \$(PRIVATE_FLAGS)"

build:
	@echo built
EOF
  echo 'name: ci' > "$p/.github/workflows/ci.yml"
  echo 'version: "2"' > "$p/.golangci.yml"
  echo '{"name":"placeholder"}' > "$p/testdata/f.json"
  echo 'MIT License' > "$p/LICENSE"
  echo '# Third-party notices' > "$p/THIRD_PARTY_NOTICES.md"
  printf '# quartermaster\n\nSee https://example.com/docs/intro for more.\n' > "$p/README.md"
  echo '# Security' > "$p/SECURITY.md"
  printf '# Changelog\n\n## %s\n\nFirst release.\n\n- One.\n\n## 9.9.8\n\nOlder.\n' "$V" > "$p/CHANGELOG.md"
  echo "plan about $deny_word" > "$p/$dd/superpowers/plan.md"
  echo "rules about $deny_word" > "$p/CLAUDE.md"
  echo 'Read CLAUDE.md.' > "$p/AGENTS.md"
  echo '{}' > "$p/.claude/settings.json"
  git -C "$p" init -q -b main
  git -C "$p" add -f .claude/settings.json   # tracked although .gitignore ignores it
  commit_all "$p" init
  echo "$p"
}
make_target() { local t="$work/public-$1"; git init -q -b main "$t"; echo "$t"; }
# A public checkout for a commit rehearsal: its origin is a local bare
# repository standing in for the public one ($work/bare-<name>.git).
make_remote_target() {
  local b="$work/bare-$1.git" t="$work/public-$1"
  git init -q --bare -b main "$b"
  git init -q -b main "$t"
  git -C "$t" remote add origin "$b"
  echo "$t"
}
rehearse() { env PUBLISH_TEST_TARGET=1 bash "$@"; }
# state <checkout>: everything a run could change: HEAD, refs, index, the
# working tree's files and the object count.
state() {
  { git -C "$1" rev-parse -q --verify HEAD || echo unborn
    git -C "$1" for-each-ref
    git -C "$1" ls-files -s
    (cd "$1" && find . -path ./.git -prune -o -type f -print | LC_ALL=C sort | xargs cat 2>/dev/null | cksum)
    git -C "$1" count-objects -v | grep -E '^(count|in-pack):'
  } 2>/dev/null
}
# a target with some published content already in it
seeded_target() {
  local t; t="$(make_target "$1")"
  echo 'published before' > "$t/OLD.md"; commit_all "$t" "published before"
  echo "$t"
}
upper() { printf '%s' "$1" | tr '[:lower:]' '[:upper:]'; }
# case_insensitive <path>: the file system answers to another spelling.
case_insensitive() { [ "$(upper "$1")" != "$1" ] && [ -e "$(upper "$1")" ] && [ "$(upper "$1")" -ef "$1" ]; }

# --- publish.sh ------------------------------------------------------------------

# P1. A rehearsal stages exactly the allowlisted tracked files, keeps modes,
# runs every gate and commits nothing; a second run accepts its own staging.
p="$(make_private p1)"; t="$(make_target p1)"
echo 'loose' > "$p/notes.txt"
expect "rehearsal stages the export and commits nothing" 0 "staged only" rehearse "$p/scripts/publish.sh" "$t"
got="$(git -C "$t" ls-files | LC_ALL=C sort | tr '\n' ' ')"
want="$(cd "$p" && git ls-files -- "${ALLOW[@]}" | LC_ALL=C sort | tr '\n' ' ')"
[[ "$got" == "$want" && "$got" != *"$dd/"* && "$got" != *notes.txt* && "$got" != *".claude/"* \
   && "$got" != *CLAUDE.md* && "$got" != *AGENTS.md* ]] \
  && ok "publish_refuses_docs, publish_refuses_dot_claude: the export is the allowlist, without docs, CLAUDE.md, AGENTS.md, .claude or untracked files" \
  || bad "the export is the allowlist" "got:  $got
want: $want"
m_fail=""
for f in .claude-plugin/marketplace.json plugins/qm-worker/.claude-plugin/plugin.json plugins/qm-worker/hooks/hooks.json \
         plugins/qm-coordinator/.claude-plugin/plugin.json swift/privcheck.swift SECURITY.md testdata/f.json; do
  [[ " $got" == *" $f "* ]] || m_fail="$m_fail $f"
done
[[ -z "$m_fail" ]] && ok "publish_exports_marketplace_and_plugins: the marketplace, both plugins, the Swift helper and SECURITY.md are exported" \
  || bad "publish_exports_marketplace_and_plugins: missing from the export:$m_fail" "$got"
[[ "$(git -C "$t" ls-files -s scripts/publish.sh | cut -d' ' -f1)" == 100755 ]] \
  && ok "file modes are kept" || bad "file modes are kept" "$(git -C "$t" ls-files -s scripts/publish.sh)"
git -C "$t" rev-parse -q --verify HEAD >/dev/null && bad "rehearsal committed" "" || ok "rehearsal made no commit"
[[ "$LAST_OUT" == *"check passed with --no-denylist"* ]] \
  && ok "the build gate ran make check with --no-denylist" || bad "build gate output" "$LAST_OUT"
[[ "$LAST_OUT" == *"check-private: clean (tree mode; deny-list used)"* ]] \
  && ok "the deny-list gate ran on the export" || bad "deny-list gate output" "$LAST_OUT"
expect "a second stage-only run accepts the staging it left" 0 "staged only" rehearse "$p/scripts/publish.sh" "$t"

# P1b. Paths a commit needs but a stage-only run does without: the run names
# them, and a commit is refused. go.sum (absent here) is exported when present.
p="$(make_private p1b)"; t="$(make_target p1b)"
git -C "$p" rm -q -r -- plugins swift SECURITY.md .claude-plugin; commit_all "$p" "before the plugins"
expect "a stage-only run without the plugins names what a commit needs" 0 "note: plugins is not tracked yet" rehearse "$p/scripts/publish.sh" "$t"
[[ "$LAST_OUT" == *"note: .claude-plugin/marketplace.json is not tracked yet"* && "$LAST_OUT" != *"go.sum"* ]] \
  && ok "the notes name the missing paths and not go.sum, which is exported only when present" || bad "missing-path notes" "$LAST_OUT"
t="$(make_remote_target p1b2)"
expect "a commit without every allowlisted path is refused" 1 "not tracked yet: .claude-plugin/marketplace.json plugins swift SECURITY.md" \
  env PUBLISH_TEST_REMOTE="$work/bare-p1b2.git" PUBLISH_IDENT="$IDENT" bash "$p/scripts/publish.sh" "$t" --commit
# A real stage-only run (no rehearsal switch) stops on them too, before it
# reads the target or the network.
t="$(make_target p1b3)"; before="$(state "$t")"
expect "a real stage-only run without every allowlisted path is refused" 1 "the public tree needs every allowlisted path, and these are not tracked yet: .claude-plugin/marketplace.json plugins swift SECURITY.md" \
  bash "$p/scripts/publish.sh" "$t"
[[ "$(state "$t")" == "$before" ]] && ok "the refused real run left the target as it was" || bad "the refused real run touched the target" ""

# P1c. A file named in the allowlist below the top level is one file: a folder
# of that name (git matches a path as a folder prefix too) is refused.
for named in .github/workflows/ci.yml .claude-plugin/marketplace.json; do
  p="$(make_private "p1c-${named##*/}")"; t="$(make_target "p1c-${named##*/}")"
  rm -f "$p/$named"; mkdir -p "$p/$named"
  echo '{"x":1}' > "$p/$named/evil.json"; commit_all "$p"
  before="$(state "$t")"
  expect "a folder named $named is refused" 1 "allowlisted file is not one tracked file: $named" rehearse "$p/scripts/publish.sh" "$t"
  [[ "$(state "$t")" == "$before" ]] && ok "the refused folder $named left nothing in the target" || bad "the folder $named touched the target" ""
done

# P1d. Every tracked file is accounted for: a tracked path that is neither
# exported nor kept back on purpose (a new top-level file or folder, a second
# file beside an allowlisted one) stops the run instead of being dropped.
for stray in NOTES.md tools/gen.go .github/dependabot.yml .claude-plugin/extra.json; do
  p="$(make_private "p1d-${stray//\//-}")"; t="$(make_target "p1d-${stray//\//-}")"
  mkdir -p "$p/$(dirname "$stray")"; echo 'x' > "$p/$stray"; commit_all "$p"
  before="$(state "$t")"
  expect "publish_refuses_unaccounted: tracked $stray stops the run" 1 "neither exported nor kept back" rehearse "$p/scripts/publish.sh" "$t"
  [[ "$LAST_OUT" == *"    $stray"* ]] && ok "the unaccounted file $stray is named" || bad "the unaccounted file $stray is named" "$LAST_OUT"
  [[ "$(state "$t")" == "$before" ]] && ok "the unaccounted $stray left nothing in the target" || bad "the unaccounted $stray touched the target" ""
done
# Kept back on purpose, in any case: docs/, .claude/, .codex/, CLAUDE.md,
# AGENTS.md (the stand-in tracks them), .env* and keys.
p="$(make_private p1d-kept)"; t="$(make_target p1d-kept)"
mkdir -p "$p/.codex"; echo '{}' > "$p/.codex/config.json"; echo 'X=1' > "$p/.env.local"; echo 'k' > "$p/deploy.KEY"
git -C "$p" add -f .env.local deploy.KEY; commit_all "$p"
expect "files kept back on purpose do not stop the run" 0 "staged only" rehearse "$p/scripts/publish.sh" "$t"

# P2. The deny-list gate: a private word in an exported file stops the export;
# without a deny-list the export fails closed.
p="$(make_private p2)"; t="$(make_target p2)"
echo "// about $deny_word" >> "$p/cmd/qm/main.go"; commit_all "$p"
expect "publish_stops_on_denylist_word: a deny-list word in the export stops it" 1 "the public tree failed check-private" \
  rehearse "$p/scripts/publish.sh" "$t"
[[ "$LAST_OUT" != *orblaxian* ]] && ok "the gate never prints the word" || bad "the gate printed the word" "$LAST_OUT"
[[ -z "$(git -C "$t" ls-files)" ]] && ok "publish_stops_on_denylist_word: the target is untouched" || bad "deny-list stop touched the target" ""
p="$(make_private p2p)"; t="$(make_target p2p)"
echo "{\"description\":\"for $deny_word\"}" > "$p/plugins/qm-worker/hooks/hooks.json"; commit_all "$p"
expect "publish_stops_on_denylist_word: a deny-list word in a plugin stops the export" 1 "the public tree failed check-private" \
  rehearse "$p/scripts/publish.sh" "$t"
p="$(make_private p2b)"; t="$(make_target p2b)"
expect "no deny-list: the export fails closed" 1 "deny-list missing, run scripts/make-denylist.sh" \
  env XDG_CONFIG_HOME="$work/no-cfg" PUBLISH_TEST_TARGET=1 bash "$p/scripts/publish.sh" "$t"

# P3. The build gate: the export must pass make check from a clean clone.
# Here the Makefile needs a file kept back on purpose.
p="$(make_private p3)"; t="$(make_target p3)"
mkdir -p "$p/.codex"; echo 'kept private' > "$p/.codex/extra.txt"
sed -i.bak "s|test ! -f break-check|test -f .codex/extra.txt|" "$p/Makefile"; rm "$p/Makefile.bak"; commit_all "$p"
expect "an export missing a file make check needs is refused" 1 "the exported tree does not pass" \
  rehearse "$p/scripts/publish.sh" "$t"
p="$(make_private p3b)"; t="$(make_target p3b)"
sed -i.bak 's/test ! -f break-check/test ! -f cmd\/qm\/main.go/' "$p/Makefile"; rm "$p/Makefile.bak"; commit_all "$p"
expect "the build gate runs the exported Makefile" 1 "the exported tree does not pass" \
  rehearse "$p/scripts/publish.sh" "$t"

# P3c. publish_refuses_docs: the export may not point at docs/ files it does
# not carry; a URL that merely has /docs/ in it is fine (P1's README has one).
p="$(make_private p3c)"; t="$(make_target p3c)"
printf '// The rule is explained in %s/superpowers/plan.md.\n' "$dd" >> "$p/cmd/qm/main.go"; commit_all "$p"
expect "publish_refuses_docs: a reference to an unexported docs/ file is refused" 1 "cmd/qm/main.go:" \
  rehearse "$p/scripts/publish.sh" "$t"
[[ "$LAST_OUT" == *"not exported"* ]] && ok "the docs/ gate names the reference" || bad "docs gate message" "$LAST_OUT"
n=0
for ref in "../../$dd/superpowers/plan.md" "./$dd/superpowers/plan.md"; do
  n=$((n + 1))
  p_r="$(make_private "p3r-$n")"; t="$(make_target "p3r-$n")"
  printf '// see %s\n' "$ref" >> "$p_r/cmd/qm/main.go"; commit_all "$p_r"
  expect "publish_refuses_docs: a relative pointer $ref is refused" 1 "cmd/qm/main.go:" rehearse "$p_r/scripts/publish.sh" "$t"
done

# P4. Symlinks are never exported.
p="$(make_private p4)"; t="$(seeded_target p4)"
ln -s ../README.md "$p/scripts/link.md"; commit_all "$p"
before="$(state "$t")"
expect "a symlink in the export set is refused" 1 "not a regular file" rehearse "$p/scripts/publish.sh" "$t"
[[ "$(state "$t")" == "$before" ]] && ok "a refused export leaves a published target untouched" || bad "target touched" "$(git -C "$t" status --short)"

# P5. Private kinds of files inside allowlisted folders, and tracked files
# .gitignore ignores, are refused before anything is touched.
n=0
dd_up="$(printf '%s' "$dd" | sed 's/^d/D/')"
for f in "internal/$dd/plan.md" "plugins/qm-worker/$dd/notes.md" "internal/$dd_up/plan.md"; do
  n=$((n + 1))
  p="$(make_private "p5d-$n")"; t="$(seeded_target "p5d-$n")"
  mkdir -p "$(dirname "$p/$f")"; echo 'placeholder' > "$p/$f"; commit_all "$p"
  before="$(state "$t")"
  expect "publish_refuses_docs: a docs folder inside the export ($f) is refused" 1 "inside a .claude, .codex or $dd folder" \
    rehearse "$p/scripts/publish.sh" "$t"
  [[ "$(state "$t")" == "$before" ]] || bad "export of $f touched the target" ""
done
n=0
for f in internal/x/CLAUDE.md cmd/AGENTS.md plugins/qm-worker/CLAUDE.md internal/agents.md cmd/claude.md; do
  n=$((n + 1))
  p="$(make_private "p5c-$n")"; t="$(seeded_target "p5c-$n")"
  mkdir -p "$(dirname "$p/$f")"; echo 'placeholder' > "$p/$f"; commit_all "$p"
  before="$(state "$t")"
  expect "publish_refuses_claude_md_and_agents_md: $f is refused" 1 "$f: a private kind of file" rehearse "$p/scripts/publish.sh" "$t"
  [[ "$(state "$t")" == "$before" ]] || bad "export of $f touched the target" ""
done
n=0
for f in internal/.claude/settings.json plugins/qm-worker/.claude/settings.json internal/.CLAUDE/x.json internal/.codex/s.json; do
  n=$((n + 1))
  p="$(make_private "p5a-$n")"; t="$(seeded_target "p5a-$n")"
  mkdir -p "$(dirname "$p/$f")"; echo '{}' > "$p/$f"
  git -C "$p" add -f -- "$f"; commit_all "$p"
  before="$(state "$t")"
  expect "publish_refuses_dot_claude: $f is refused" 1 "inside a .claude, .codex or $dd folder" rehearse "$p/scripts/publish.sh" "$t"
  [[ "$(state "$t")" == "$before" ]] || bad "export of $f touched the target" ""
done
# The top-level .claude folder is tracked in every stand-in (P1 shows it is
# not exported); .claude-plugin is another folder and is exported (P1).
n=0
for f in cmd/.env cmd/.env.local internal/key.pem scripts/id.key internal/.npmrc internal/x/id_rsa cmd/.netrc cmd/cert.p12 \
         internal/state.sqlite cmd/.main.go.swp cmd/.ENV internal/KEY.PEM internal/data.bin internal/x/notes.yaml \
         plugins/qm-worker/hook.py swift/helper.m plugins/qm-worker/notes.txt; do
  n=$((n + 1))
  p="$(make_private "p5-$n")"; t="$(seeded_target "p5-$n")"
  mkdir -p "$(dirname "$p/$f")"; echo 'placeholder' > "$p/$f"
  git -C "$p" add -f -- "$f"; commit_all "$p"
  before="$(state "$t")"
  expect "export refuses $f" 1 "forbidden files in the export set" rehearse "$p/scripts/publish.sh" "$t"
  [[ "$(state "$t")" == "$before" ]] || bad "export of $f touched the target" ""
done
# Secrets inside files of allowed kinds: the guard's generic rules, run in
# the scratch repository before the target is written to.
k_aws="AK""IA$(printf 'Q%.0s' $(seq 1 16))"
k_slack="xox""b-1234567890-abcdefghij"
k_key="-----BEGIN ""OPENSSH PRIV""ATE KEY-----"
n=0
for planted in "internal/x/creds.json:{\"key\":\"$k_aws\"}" "plugins/qm-worker/hooks/s.json:{\"t\":\"$k_slack\"}" \
               "scripts/k.md:$k_key"; do
  n=$((n + 1))
  f="${planted%%:*}"
  p="$(make_private "p5s-$n")"; t="$(seeded_target "p5s-$n")"
  mkdir -p "$(dirname "$p/$f")"; printf '%s\n' "${planted#*:}" > "$p/$f"; commit_all "$p"
  before="$(state "$t")"
  expect "export refuses a secret in $f" 1 "the public tree failed check-private" rehearse "$p/scripts/publish.sh" "$t"
  [[ "$(state "$t")" == "$before" ]] && ok "the refused secret left nothing in the target" || bad "secret export touched the target" "$(git -C "$t" count-objects -v)"
done
# Binary content stops the run whatever the name; names that merely contain a
# key marker do too.
for planted in internal/x/data.go testdata/x.jsonl; do
  p="$(make_private "p5b-${planted##*/}")"; t="$(make_target "p5b-${planted##*/}")"
  mkdir -p "$(dirname "$p/$planted")"; printf 'package x\n\000\001\002' > "$p/$planted"; commit_all "$p"
  expect "binary content in $planted is refused" 1 "binary content" rehearse "$p/scripts/publish.sh" "$t"
done
for f in cmd/server.pem.go internal/id_rsa_backup.go cmd/x.key.go; do
  p="$(make_private "p5n-${f##*/}")"; t="$(make_target "p5n-${f##*/}")"
  mkdir -p "$(dirname "$p/$f")"; echo 'package x' > "$p/$f"; commit_all "$p"
  expect "a name containing a key marker is refused: $f" 1 "a private kind of file" rehearse "$p/scripts/publish.sh" "$t"
done
# Text that is not UTF-8 but has no NUL byte is text, on macOS as on Linux.
p="$(make_private p5-latin1)"; t="$(make_target p5-latin1)"
printf 'caf\351 au lait\n' > "$p/internal/latin1.txt"; commit_all "$p"
expect "a Latin-1 text file without a NUL byte is not binary" 0 "staged only" env LC_ALL=en_US.UTF-8 PUBLISH_TEST_TARGET=1 bash "$p/scripts/publish.sh" "$t"
[[ "$LAST_OUT" != *"Illegal byte sequence"* ]] && ok "the binary test reads Latin-1 without an illegal byte sequence" || bad "illegal byte sequence" "$LAST_OUT"
# A path that is not UTF-8 is still checked whole: a docs folder below it is
# refused. The path lives in the index only (macOS file systems refuse it).
p="$(make_private p5-path)"; t="$(make_target p5-path)"
blob="$(echo 'package y' | git -C "$p" hash-object -w --stdin)"
git -C "$p" update-index --add --cacheinfo "100644,$blob,internal/x"$'\377'"/$dd/a.go"
git -C "$p" -c user.name=test -c user.email=test@example.com -c commit.gpgsign=false commit -q -m 'a path that is not UTF-8'
expect "a docs folder below a path that is not UTF-8 is refused" 1 "inside a .claude, .codex or $dd folder" env LC_ALL=en_US.UTF-8 PUBLISH_TEST_TARGET=1 bash "$p/scripts/publish.sh" "$t"
p="$(make_private p5-big)"; t="$(make_target p5-big)"
head -c 1100000 /dev/zero | tr '\0' 'a' > "$p/internal/big.json"; commit_all "$p"
expect "export refuses a file over the size cap" 1 "more than 1048576" rehearse "$p/scripts/publish.sh" "$t"
p="$(make_private p5-ign)"; t="$(make_target p5-ign)"
echo '{}' > "$p/internal/ignored.json"
printf 'ignored.json\n' >> "$p/.gitignore"; git -C "$p" add -f internal/ignored.json; commit_all "$p"
expect "export refuses a tracked file that .gitignore ignores" 1 "that .gitignore ignores" rehearse "$p/scripts/publish.sh" "$t"

# P6. The target's own ignore rules cannot drop an exported file.
p="$(make_private p6)"; t="$(make_target p6)"
printf 'LICENSE\nCHANGELOG.md\nplugins/\n' > "$t/.git/info/exclude"
expect "an export into a target that ignores files" 0 "staged only" rehearse "$p/scripts/publish.sh" "$t"
git -C "$t" ls-files --error-unmatch LICENSE CHANGELOG.md plugins/qm-worker/hooks/hooks.json >/dev/null 2>&1 \
  && [[ "$(git -C "$t" ls-files | wc -l)" == "$(git -C "$p" ls-files -- "${ALLOW[@]}" | wc -l)" ]] \
  && ok "the target's ignore rules dropped nothing" || bad "files dropped by the target's ignore rules" "$(git -C "$t" ls-files | wc -l)"
# A non-ASCII file name exports like any other.
p="$(make_private p6u)"; t="$(make_target p6u)"
echo 'package x' > "$p/internal/caf"$'\xc3\xa9'".go"; commit_all "$p"
expect "an export with a non-ASCII file name" 0 "staged only" rehearse "$p/scripts/publish.sh" "$t"
git -C "$t" -c core.quotePath=false ls-files | grep -q "caf"$'\xc3\xa9'".go" && ok "the non-ASCII file is staged" || bad "non-ASCII file missing" ""
# The target's own config cannot run a program.
p="$(make_private p6f)"; t="$(make_target p6f)"
printf '#!/bin/sh\ntouch "%s"\n' "$work/fsmonitor-ran" > "$work/fsmonitor.sh"; chmod +x "$work/fsmonitor.sh"
git -C "$t" config core.fsmonitor "$work/fsmonitor.sh"
expect "an export into a target with an fsmonitor hook" 0 "staged only" rehearse "$p/scripts/publish.sh" "$t"
[[ ! -e "$work/fsmonitor-ran" ]] && ok "the target's fsmonitor program never ran" || bad "the fsmonitor program ran" ""

# An interrupted write puts the target back; a refused export does not even
# update the target's tracking ref. publish.sh interrupts itself with SIGINT;
# a non-interactive shell starts its background jobs with SIGINT ignored, so
# perl puts SIGINT back to its default before publish.sh starts.
p="$(make_private p6i)"; t="$(seeded_target p6i)"
before="$(state "$t")"
with_sigint() { perl -e '$SIG{INT} = "DEFAULT"; exec { $ARGV[0] } @ARGV or die "exec $ARGV[0]: $!\n"' "$@"; }
expect "an interrupted write stops the run" 130 "put back as it was" \
  with_sigint env PUBLISH_TEST_INTERRUPT_AFTER=3 PUBLISH_TEST_TARGET=1 bash "$p/scripts/publish.sh" "$t"
[[ "$(state "$t")" == "$before" ]] && ok "the interrupted write left the target as it was" || bad "interrupted write changed the target" "$(git -C "$t" status --short | head)"
p="$(make_private p6t)"; t="$(make_remote_target p6t)"; b6t="$work/bare-p6t.git"
seed="$work/seed-p6t"; git init -q -b main "$seed"; echo old > "$seed/OLD.md"; commit_all "$seed" "old"
git -C "$seed" push -q "$b6t" HEAD:refs/heads/main
git -C "$t" fetch -q "$b6t" main && git -C "$t" reset -q --hard FETCH_HEAD
echo "// about $deny_word" >> "$p/cmd/qm/main.go"; commit_all "$p"
before="$(state "$t")"
expect "a refused export in a commit rehearsal" 1 "the public tree failed check-private" \
  env PUBLISH_TEST_REMOTE="$b6t" PUBLISH_IDENT="$IDENT" bash "$p/scripts/publish.sh" "$t" --update "x"
[[ "$(state "$t")" == "$before" ]] && ok "a refused export leaves the target's refs and objects as they were" || bad "refused export changed the target" "$(git -C "$t" for-each-ref)"

# P7. The target is never the private repository, inside it, around it, a
# worktree or clone of it, or a checkout with uncommitted work, however the
# path is spelled; a relative target is the caller's.
p="$(make_private p7)"
expect "a target that is the private repository is refused" 1 "inside the private repository" rehearse "$p/scripts/publish.sh" "$p"
git init -q -b main "$p/sub"
expect "a nested checkout inside the private repository is refused" 1 "inside the private repository" \
  rehearse "$p/scripts/publish.sh" "$p/sub"
rm -rf "$p/sub"
ln -s "$p" "$work/link-to-private"
expect "a symlink to the private repository is refused" 1 "inside the private repository" \
  rehearse "$p/scripts/publish.sh" "$work/link-to-private"
mkdir -p "$work/outer"; git init -q -b main "$work/outer"
inner="$(make_private p7in "$work/outer/inner")"
expect "a target that contains the private repository is refused" 1 "inside the target" \
  rehearse "$inner/scripts/publish.sh" "$work/outer"
[[ -f "$inner/README.md" ]] && ok "the private repository inside the target is untouched" || bad "inner repository damaged" ""
git clone -q "$p" "$work/sibling"
echo "unsaved edit" >> "$work/sibling/README.md"
expect "a clone of the private repository is refused" 1 "private repository's history" \
  rehearse "$p/scripts/publish.sh" "$work/sibling"
grep -q "unsaved edit" "$work/sibling/README.md" && [[ -d "$work/sibling/$dd" ]] \
  && ok "the sibling clone keeps its edit and its docs" || bad "sibling clone damaged" ""
git -C "$p" worktree add -q -b wt "$work/wt-private" 2>/dev/null
expect "a worktree of the private repository is refused" 1 "private repository" \
  rehearse "$p/scripts/publish.sh" "$work/wt-private"
git -C "$p" worktree remove --force "$work/wt-private"; git -C "$p" branch -q -D wt
if case_insensitive "$p"; then
  expect "another spelling of the private path is refused" 1 "inside the private repository" \
    rehearse "$p/scripts/publish.sh" "$(upper "$p")"
else
  echo "skip case-variant paths (case-sensitive file system)"
fi
t="$(make_target p7m)"
echo 'kept' > "$t/notes.md"; commit_all "$t" notes
echo 'uncommitted' >> "$t/notes.md"
expect "a target with uncommitted changes is refused" 1 "uncommitted changes" rehearse "$p/scripts/publish.sh" "$t"
grep -q uncommitted "$t/notes.md" && ok "the uncommitted change is kept" || bad "uncommitted change lost" ""
t="$(make_target p7s)"
echo 'kept' > "$t/notes.md"; commit_all "$t" notes
echo 'staged by hand' >> "$t/notes.md"; git -C "$t" add notes.md
expect "a target with staged changes publish.sh did not make is refused" 1 "did not make" rehearse "$p/scripts/publish.sh" "$t"
for flag in --assume-unchanged --skip-worktree; do
  t="$(make_target "p7h$flag")"
  echo 'kept' > "$t/notes.md"; commit_all "$t" notes
  echo 'hidden edit' >> "$t/notes.md"; git -C "$t" update-index "$flag" notes.md
  expect "a target with $flag on an edited file is refused" 1 "assume-unchanged or skip-worktree" rehearse "$p/scripts/publish.sh" "$t"
  grep -q 'hidden edit' "$t/notes.md" && ok "the hidden edit ($flag) is kept" || bad "hidden edit lost ($flag)" ""
done
t="$(make_target p7pu)"; git -C "$t" remote add origin "$work/somewhere.git"; git -C "$t" config remote.origin.pushurl "$PUBLIC_URL"
expect "a target with a push URL is refused" 1 "push URL" rehearse "$p/scripts/publish.sh" "$t"
t="$(make_target p7up)"; git -C "$t" remote add upstream "$PUBLIC_URL"
expect "a rehearsal refuses a checkout with the public repository as any remote" 1 "a remote that is the public repository" \
  rehearse "$p/scripts/publish.sh" "$t"
# The public repository in any spelling counts: SSH, no .git, other case.
for u in "git@github.com:IZZY-Agency/quartermaster.git" "https://github.com/IZZY-Agency/quartermaster" \
         "https://GitHub.com/izzy-agency/Quartermaster.git/" "ssh://git@github.com/IZZY-Agency/quartermaster.git" \
         "ssh://git@github.com:22/IZZY-Agency/quartermaster.git" "git@github.com-work:IZZY-Agency/quartermaster.git"; do
  t="$(make_target "p7sp-$(printf '%s' "$u" | cksum | cut -d' ' -f1)")"; git -C "$t" remote add origin "$u"
  expect "a rehearsal refuses a checkout whose remote is $u" 1 "a remote that is the public repository" \
    rehearse "$p/scripts/publish.sh" "$t"
done
# A target whose path holds glob characters: .git's empty folders survive.
t="$work/glob[x]*?/public"; mkdir -p "$(dirname "$t")"; git init -q -b main "$t"
expect "a target path with glob characters is staged" 0 "staged only" rehearse "$p/scripts/publish.sh" "$t"
[[ -d "$t/.git/refs/tags" && -d "$t/.git/objects/info" ]] \
  && ok "the empty folders inside .git survive a target path with glob characters" \
  || bad "folders inside .git were deleted" "$(cd "$t/.git" && find . -type d | LC_ALL=C sort)"
t="$(make_target p7u)"
echo stray > "$t/stray.txt"
expect "a target with untracked files is refused" 1 "untracked or ignored files" rehearse "$p/scripts/publish.sh" "$t"
make_target p7rel >/dev/null
expect "a relative target is the caller's folder" 0 "staged only" \
  bash -c 'cd "$1" && PUBLISH_TEST_TARGET=1 bash "$2/scripts/publish.sh" public-p7rel' _ "$work" "$p"
[[ -n "$(git -C "$work/public-p7rel" ls-files)" && ! -e "$p/public-p7rel" ]] \
  && ok "the relative target got the export" || bad "relative target" "$(ls "$p")"
t="$(make_target p7o)"
git -C "$t" remote add origin "https://github.com/someone/fork.git"
expect "a target whose origin is not the public repository is refused" 1 "not the public repository" \
  env PUBLISH_IDENT="$IDENT" bash "$p/scripts/publish.sh" "$t"
expect "a stage-only rehearsal never commits" 1 "never commits" \
  env PUBLISH_TEST_TARGET=1 PUBLISH_IDENT="$IDENT" bash "$p/scripts/publish.sh" "$t" --commit
expect "unknown option" 2 "unknown option" rehearse "$p/scripts/publish.sh" "$t" --push

# P8. A commit rehearsal against a stand-in: never in a checkout whose origin
# is real, no push command, last-published untouched; the commit message is
# scanned, and a commit that fails is undone.
p="$(make_private p8)"; t="$(make_remote_target p8)"; b="$work/bare-p8.git"
real() { env PUBLISH_TEST_REMOTE="$b" PUBLISH_IDENT="$IDENT" bash "$p/scripts/publish.sh" "$t" "$@"; }
t_real="$(make_target p8real)"; git -C "$t_real" remote add origin "$PUBLIC_URL"
expect "a commit rehearsal refuses a checkout whose origin is the public URL" 1 "a remote that is the public repository" \
  env PUBLISH_TEST_REMOTE="$b" PUBLISH_IDENT="$IDENT" bash "$p/scripts/publish.sh" "$t_real" --commit
[[ -z "$(git -C "$t_real" ls-files)" ]] && ok "the real-origin checkout is untouched" || bad "real-origin checkout touched" ""
echo dirty >> "$p/README.md"
expect "a dirty private tree is refused" 1 "not clean" real
git -C "$p" checkout -q -- README.md
expect "committing needs PUBLISH_IDENT" 1 "PUBLISH_IDENT" \
  env PUBLISH_TEST_REMOTE="$b" bash "$p/scripts/publish.sh" "$t" --commit
expect "a release commits and tags v$V" 0 "tagged v$V" real --commit
[[ "$LAST_OUT" == *"history mode, all refs"* ]] \
  && ok "the gate's history scan reads every ref of the target, not a branch scope" \
  || bad "the gate's history scan is not the full one" "$LAST_OUT"
subject="$(git -C "$t" log -1 --format=%s)"
body="$(git -C "$t" log -1 --format=%b)"
who="$(git -C "$t" log -1 --format='%an <%ae>|%cn <%ce>')"
tagger="$(git -C "$t" for-each-ref refs/tags/v$V --format='%(taggername) %(taggeremail)')"
if [[ "$subject" == "quartermaster $V" && "$body" == *"First release."* && "$body" != *Older* \
      && "$who" == "$IDENT|$IDENT" && "$tagger" == "$IDENT" ]]; then
  ok "the release commit and tag carry the changelog entry and the identity"
else
  bad "release commit" "subject=$subject who=$who tagger=$tagger
$body"
fi
[[ "$LAST_OUT" == *"REHEARSAL: no push command"* && "$LAST_OUT" != *"push with:"* ]] \
  && ok "a rehearsal prints no push command" || bad "rehearsal push command" "$LAST_OUT"
git -C "$p" rev-parse -q --verify last-published >/dev/null \
  && bad "a rehearsal moved last-published" "" || ok "a rehearsal leaves last-published alone"
git -C "$t" push -q "$b" HEAD:refs/heads/main "refs/tags/v$V"   # the manual push, to the stand-in
expect "a released version cannot be released again" 1 "already released" real --commit
expect "an update with no change has nothing to commit" 1 "nothing to commit" real --update "Docs"
echo "more words" >> "$p/README.md"; commit_all "$p" "readme"
before="$(state "$t")"
expect "an update whose subject holds a private word is refused before any commit" 1 "the commit message failed check-private" \
  real --update "notes for ${deny_word}"
[[ "$(state "$t")" == "$before" ]] && ok "the refused subject left the target untouched" || bad "refused subject touched the target" "$(git -C "$t" log --oneline -2)"
[[ "$LAST_OUT" != *orblaxian* ]] && ok "the message gate never prints the word" || bad "message gate printed the word" "$LAST_OUT"
git -C "$p" branch secret-plan
private_c="$(git -C "$p" rev-parse --short HEAD)"
for subject in "fix from $private_c" "work from secret-plan" "see $dd/superpowers/plan.md" "work from SECRET-PLAN" \
               "fix ${private_c:0:6}" "id abc${private_c}def"; do
  expect "an update subject naming private things is refused: $subject" 1 "the subject names" real --update "$subject"
done
[[ "$(state "$t")" == "$before" ]] && ok "the refused subjects left the target untouched" || bad "refused subjects touched the target" ""
# Stopped between the commit and its gates: the commit is undone and the
# staged tree stays as publish.sh's own, so the next run goes on.
head_before="$(git -C "$t" rev-parse HEAD)"
expect "an update stopped after its commit exits as Ctrl-C does" 130 "it was undone" \
  with_sigint env PUBLISH_TEST_INTERRUPT_COMMIT=1 PUBLISH_TEST_REMOTE="$b" PUBLISH_IDENT="$IDENT" \
    bash "$p/scripts/publish.sh" "$t" --update "README: more words"
[[ "$(git -C "$t" rev-parse HEAD)" == "$head_before" ]] \
  && ok "the stopped commit was undone" || bad "the stopped commit stayed" "$(git -C "$t" log --oneline -3)"
git -C "$t" config i18n.commitEncoding ISO-8859-1
expect "an update commits without a tag" 0 "(no tag)" real --update "README: more words"
[[ "$(git -C "$t" log -1 --format=%s)" == "README: more words" && "$(git -C "$t" tag | wc -l | tr -d ' ')" == 1 ]] \
  && ok "the update commit has the subject and no new tag" || bad "update commit" "$(git -C "$t" log --oneline)"
git -C "$t" cat-file commit HEAD | grep -q '^encoding ' && bad "the target's commitEncoding added a header" "" || ok "the commit has no encoding header"

# An undo removes the release tag and leaves no trace; a history scan that
# errors (exit 2) undoes the commit too.
p10="$(make_private p10)"; t10="$(make_remote_target p10)"; b10="$work/bare-p10.git"
seed="$work/seed-p10"; git init -q -b main "$seed"; echo old > "$seed/OLD.md"; commit_all "$seed" "old notes for ${deny_word}"
git -C "$seed" push -q "$b10" HEAD:refs/heads/main
git -C "$t10" fetch -q "$b10" main && git -C "$t10" reset -q --hard FETCH_HEAD
head10="$(git -C "$t10" rev-parse HEAD)"
git -C "$t10" config gc.auto 1   # an auto-gc would pack the commit before the undo
expect "a release whose history scan fails is undone" 1 "failed check-private --history" \
  env PUBLISH_TEST_REMOTE="$b10" PUBLISH_IDENT="$IDENT" bash "$p10/scripts/publish.sh" "$t10" --commit
[[ -z "$(git -C "$t10" tag)" && "$(git -C "$t10" rev-parse HEAD)" == "$head10" ]] \
  && ok "the undo removed the release tag and the commit" || bad "undo left a tag or commit" "$(git -C "$t10" tag; git -C "$t10" log --oneline -2)"
! git -C "$t10" fsck --unreachable --no-reflogs 2>/dev/null | grep -qE 'commit|tag' && ! git -C "$t10" reflog 2>/dev/null | grep -qi 'quartermaster' \
  && ok "the undone commit and tag are pruned and gone from the reflog" || bad "the undone commit left traces" "$(git -C "$t10" fsck --unreachable --no-reflogs 2>&1 | head -3)"
p11="$(make_private p11)"; t11="$(make_remote_target p11)"; b11="$work/bare-p11.git"
commit_all "$t11" "first public"
git -C "$t11" push -q "$b11" HEAD:refs/heads/main
git -C "$t11" update-ref refs/check-private/pull/1/head HEAD
head11="$(git -C "$t11" rev-parse HEAD)"
expect "a history scan that errors undoes the commit" 1 "failed check-private --history" \
  env PUBLISH_TEST_REMOTE="$b11" PUBLISH_IDENT="$IDENT" bash "$p11/scripts/publish.sh" "$t11" --update "Snapshot"
[[ "$(git -C "$t11" rev-parse HEAD)" == "$head11" ]] && ok "the erroring scan left HEAD as it was" || bad "erroring scan kept the commit" ""
p9="$(make_private p9)"; t9="$(make_remote_target p9)"
sed -i.bak "s/^RELEASE_VERSION := .*/RELEASE_VERSION := 9.9.10/" "$p9/Makefile"; rm "$p9/Makefile.bak"; commit_all "$p9"
expect "a release needs a changelog entry for its version" 1 "no '## 9.9.10'" \
  env PUBLISH_TEST_REMOTE="$work/bare-p9.git" PUBLISH_IDENT="$IDENT" bash "$p9/scripts/publish.sh" "$t9" --commit
expect "a malformed PUBLISH_IDENT is refused" 1 "PUBLISH_IDENT" \
  env PUBLISH_TEST_REMOTE="$work/bare-p9.git" PUBLISH_IDENT="nobody" bash "$p9/scripts/publish.sh" "$t9" --update "x"

# --- package.sh and verify-release.sh -------------------------------------------

p="$(make_private pk)"
c="$(git -C "$p" rev-parse --short HEAD)"
out1="$work/rel1"
dev() { env PACKAGE_DEV=1 "$@"; }
DV="dev-$V"
expect "package builds the four archives (development build)" 0 "quartermaster-$DV-linux-x86_64.tar.gz" \
  dev env SOURCE_DATE_EPOCH=1700000000 bash "$p/scripts/package.sh" "$V" "$out1"
names="$(cd "$out1" && ls | tr '\n' ' ')"
want_names=""
for s in linux-arm64 linux-x86_64 macos-arm64 macos-x86_64; do
  want_names="$want_names quartermaster-$DV-$s.tar.gz quartermaster-$DV-$s.tar.gz.sha256"
done
[[ " $names" == "$want_names " ]] && ok "archive and checksum names" || bad "archive names" "got:  $names
want:$want_names"
# The Go instruction-set variables of the shell do not reach the build.
expect "a build with GOAMD64=v3 in the environment" 0 "-" \
  dev env SOURCE_DATE_EPOCH=1700000000 GOAMD64=v3 GOEXPERIMENT= bash "$p/scripts/package.sh" "$V" "$work/rel-v3"
# (The macOS archive also holds the stand-in helper, different on each build:
# its qm is compared on its own.)
mqm() { tar -xOzf "$1/quartermaster-$DV-macos-x86_64.tar.gz" qm | cksum; }
[[ "$(cat "$out1"/*-linux-x86_64.tar.gz.sha256)" == "$(cat "$work/rel-v3"/*-linux-x86_64.tar.gz.sha256)" \
   && "$(mqm "$out1")" == "$(mqm "$work/rel-v3")" ]] \
  && ok "GOAMD64 in the environment does not change the x86_64 builds" \
  || bad "GOAMD64 changed the build" "$(cat "$out1"/*-linux-x86_64.tar.gz.sha256 "$work/rel-v3"/*-linux-x86_64.tar.gz.sha256; mqm "$out1"; mqm "$work/rel-v3")"
expect "PACKAGE_ALLOW_DIRTY=1 is refused for a release build" 1 "development builds only" \
  env PACKAGE_ALLOW_DIRTY=1 bash "$p/scripts/package.sh" "$V" "$work/rel-dirty"
echo 'extra.go' >> "$p/.git/info/exclude"; echo 'package main' > "$p/cmd/qm/extra.go"
expect "an ignored Go file in the tree is refused" 1 "ignored Go files would be compiled in: cmd/qm/extra.go" \
  dev bash "$p/scripts/package.sh" "$V" "$work/rel-ignored"
rm -f "$p/cmd/qm/extra.go"
# package_contents_per_platform: the helper only in the macOS archives, the
# worker plugin (and not the coordinator) in all four.
pc_fail=""
for s in macos-arm64 macos-x86_64 linux-arm64 linux-x86_64; do
  files="$(tar -tzf "$out1/quartermaster-$DV-$s.tar.gz" | grep -v '/$' | LC_ALL=C sort | tr '\n' ' ')"
  case "$s" in
    macos-*) want="LICENSE README.md THIRD_PARTY_NOTICES.md qm qm-privcheck" ;;
    *)       want="LICENSE README.md THIRD_PARTY_NOTICES.md qm" ;;
  esac
  want="$(printf '%s\n' $want "$PLUGIN_DIR/.claude-plugin/plugin.json" "$PLUGIN_DIR/hooks/hooks.json" | LC_ALL=C sort | tr '\n' ' ')"
  [[ "$files" == "$want" ]] || pc_fail="$pc_fail
$s: files '$files', want '$want'"
done
[[ -z "$pc_fail" ]] && ok "package_contents_per_platform: qm-privcheck only in the macOS archives, the worker plugin in all four" \
  || bad "package_contents_per_platform" "$pc_fail"
modes="$(tar -tvzf "$out1/quartermaster-$DV-macos-arm64.tar.gz" | awk '{ print $1, $NF }' | tr '\n' ';')"
[[ "$modes" == *"-rwxr-xr-x qm;"* && "$modes" == *"-rwxr-xr-x qm-privcheck;"* && "$modes" == *"-rw-r--r-- $PLUGIN_DIR/hooks/hooks.json;"* \
   && "$modes" == *"drwxr-xr-x $PLUGIN_DIR/"* ]] \
  && ok "archive modes: executables and folders 0755, the rest 0644" || bad "archive modes" "$modes"
(cd "$out1" && (shasum -a 256 -c ./*.sha256 >/dev/null 2>&1 || sha256sum -c ./*.sha256 >/dev/null 2>&1)) \
  && ok "checksum files verify" || bad "checksum files verify" "$(cat "$out1"/*.sha256)"
# Two clones in different folders build the same Linux bytes.
for clone in ca cb; do
  git clone -q "$p" "$work/clone-$clone"
  dev env SOURCE_DATE_EPOCH=1700000000 bash "$work/clone-$clone/scripts/package.sh" "$V" "$work/rel-$clone" >/dev/null 2>&1
done
lin() { (cd "$1" && cat ./*linux*.sha256); }
if [[ -s "$work/rel-ca/quartermaster-$DV-linux-x86_64.tar.gz.sha256" ]] && cmp -s <(lin "$work/rel-ca") <(lin "$work/rel-cb") \
   && cmp -s <(lin "$out1") <(lin "$work/rel-ca"); then
  ok "Linux builds of one commit in two different folders are byte for byte the same"
else
  bad "reproducible archives" "$(cat "$work"/rel-ca/*.sha256 "$work"/rel-cb/*.sha256 2>&1)"
fi
expect "verify_release_version_line: a development build says so in --version" 0 "qm $DV ($c)" \
  bash "$p/scripts/verify-release.sh" "$out1" "$DV" "$c"
case "$(uname -s)" in
  Darwin)
    [[ "$LAST_OUT" == *"qm-privcheck status: unavailable: stand-in helper"* ]] \
      && ok "verify-release runs qm-privcheck status (unavailable passes)" || bad "verify-release helper run" "$LAST_OUT" ;;
  *) [[ "$LAST_OUT" == *"not run on"* ]] && ok "the macOS archives are checked but not run here" || bad "verify-release on Linux" "$LAST_OUT" ;;
esac
# Archives that are named 9.9.7 but hold the 9.9.9 build: --version tells.
mkdir -p "$work/rel-renamed"
for s in macos-arm64 macos-x86_64 linux-arm64 linux-x86_64; do
  cp "$out1/quartermaster-$DV-$s.tar.gz" "$work/rel-renamed/quartermaster-dev-9.9.7-$s.tar.gz"
  (cd "$work/rel-renamed" && sha_line "quartermaster-dev-9.9.7-$s.tar.gz" > "quartermaster-dev-9.9.7-$s.tar.gz.sha256")
done
expect "verify_release_version_line: archives whose binary is another version are refused" 1 "want 'qm dev-9.9.7 ($c)'" \
  bash "$p/scripts/verify-release.sh" "$work/rel-renamed" "dev-9.9.7" "$c"
expect "verify_release_version_line: another commit is refused" 1 "want 'qm $DV (0000000)'" \
  bash "$p/scripts/verify-release.sh" "$out1" "$DV" 0000000
expect "verify-release needs the commit" 1 "usage" bash "$p/scripts/verify-release.sh" "$out1" "$DV"
expect "verify-release --rebuild finds the same bytes" 0 "is byte for byte the same" \
  dev env SOURCE_DATE_EPOCH=1700000000 bash "$p/scripts/verify-release.sh" "$out1" "$DV" "$c" --rebuild
[[ "$LAST_OUT" == *"quartermaster-$DV-macos-arm64.tar.gz is the same, entry by entry, but for qm-privcheck"* ]] \
  && ok "the rebuild compares the macOS archives without the helper, whose bytes differ" || bad "rebuild of the macOS archives" "$LAST_OUT"
expect "REQUIRE_SIGNATURE=1 refuses unsigned archives" 1 "FAIL" \
  env REQUIRE_SIGNATURE=1 bash "$p/scripts/verify-release.sh" "$out1" "$DV" "$c"
fakeos="$work/fake-linux"; mkdir -p "$fakeos"
printf '#!/bin/sh\ncase "$1" in -s) echo Linux ;; -m) echo x86_64 ;; *) echo Linux ;; esac\n' > "$fakeos/uname"; chmod +x "$fakeos/uname"
expect "REQUIRE_SIGNATURE=1 off macOS fails, never skips" 1 "needs macOS" \
  env PATH="$fakeos:$PATH" REQUIRE_SIGNATURE=1 bash "$p/scripts/verify-release.sh" "$out1" "$DV" "$c"
expect "EXPECTED_TEAM_ID must look like a team id" 2 "EXPECTED_TEAM_ID" \
  env REQUIRE_SIGNATURE=1 EXPECTED_TEAM_ID='x;y' bash "$p/scripts/verify-release.sh" "$out1" "$DV" "$c"
cp -R "$out1" "$work/rel-bad"
printf 'x' >> "$work/rel-bad/quartermaster-$DV-macos-x86_64.tar.gz"
expect "verify-release refuses a changed archive" 1 "checksum" bash "$p/scripts/verify-release.sh" "$work/rel-bad" "$DV" "$c"
# An archive without the plugin, a Linux archive with the helper, or a plugin
# folder with something beside qm-worker, is refused.
mkdir -p "$work/stage-x"; tar -xzf "$out1/quartermaster-$DV-macos-arm64.tar.gz" -C "$work/stage-x"
mkdir -p "$work/stage-x/share/quartermaster/plugins/qm-coordinator"; echo '{}' > "$work/stage-x/share/quartermaster/plugins/qm-coordinator/p.json"
mk_archive() {  # <dir> <suffix> <entries...>: replace one archive with those entries of stage-x
  local d="$1" s="$2"; shift 2
  (cd "$work/stage-x" && tar -cf - "$@") | gzip -n > "$d/quartermaster-$DV-$s.tar.gz"
  (cd "$d" && sha_line "quartermaster-$DV-$s.tar.gz" > "quartermaster-$DV-$s.tar.gz.sha256")
}
cp -R "$out1" "$work/rel-noplugin"; mk_archive "$work/rel-noplugin" linux-arm64 LICENSE README.md THIRD_PARTY_NOTICES.md qm
expect "verify-release refuses an archive without the plugin" 1 "linux-arm64.tar.gz: holds" \
  bash "$p/scripts/verify-release.sh" "$work/rel-noplugin" "$DV" "$c"
cp -R "$out1" "$work/rel-linuxhelper"; mk_archive "$work/rel-linuxhelper" linux-arm64 LICENSE README.md THIRD_PARTY_NOTICES.md qm qm-privcheck "$PLUGIN_DIR"
expect "verify-release refuses a Linux archive with the helper" 1 "linux-arm64.tar.gz: holds" \
  bash "$p/scripts/verify-release.sh" "$work/rel-linuxhelper" "$DV" "$c"
cp -R "$out1" "$work/rel-coord"; mk_archive "$work/rel-coord" linux-arm64 LICENSE README.md THIRD_PARTY_NOTICES.md qm share
expect "verify-release refuses a share folder with more than the worker plugin" 1 "share/ holds more than the qm-worker plugin" \
  bash "$p/scripts/verify-release.sh" "$work/rel-coord" "$DV" "$c"
if [[ "$(uname -s)" == Darwin ]]; then
  # A helper that answers status with neither answer fails the run.
  pb="$(make_private pk-broken)"
  echo '// BROKEN' > "$pb/swift/privcheck.swift"; commit_all "$pb"
  dev bash "$pb/scripts/package.sh" "$V" "$work/rel-broken" >/dev/null 2>&1
  expect "verify-release refuses a helper whose status is neither answer" 1 "qm-privcheck status exited 1" \
    bash "$pb/scripts/verify-release.sh" "$work/rel-broken" "$DV" "$(git -C "$pb" rev-parse --short HEAD)"
fi

# The version is a plain X.Y.Z (or dev-X.Y.Z): nothing that could be a path or an option.
for badv in ../x -h "1.2" "9.9.9;x" "9.9.9/.." "-9.9.9"; do
  expect "verify-release refuses the version '$badv'" 2 "bad version" bash "$p/scripts/verify-release.sh" "$out1" "$badv" "$c"
done
expect "verify-release takes a pre-release version, as package.sh does" 1 "archive or .sha256 missing" bash "$p/scripts/verify-release.sh" "$out1" "1.2.3-rc.1" "$c"
# With REQUIRE_SIGNATURE=1 the signature is checked before the binaries run: a
# macOS x86_64 archive whose qm and qm-privcheck leave a mark when run, on a
# host that says it is macOS x86_64, with a codesign that refuses everything.
stage_m="$work/stage-m"; mkdir -p "$stage_m"; tar -xzf "$out1/quartermaster-$DV-macos-x86_64.tar.gz" -C "$stage_m"
for b in qm qm-privcheck; do
  printf '#!/bin/sh\ntouch "$RAN_MARK"\necho "qm %s (%s)"\n' "$DV" "$c" > "$stage_m/$b"; chmod 755 "$stage_m/$b"
done
cp -R "$out1" "$work/rel-mark"
(cd "$stage_m" && tar -cf - LICENSE README.md THIRD_PARTY_NOTICES.md qm qm-privcheck share) | gzip -n > "$work/rel-mark/quartermaster-$DV-macos-x86_64.tar.gz"
(cd "$work/rel-mark" && sha_line "quartermaster-$DV-macos-x86_64.tar.gz" > "quartermaster-$DV-macos-x86_64.tar.gz.sha256")
fakemac="$work/fake-mac"; mkdir -p "$fakemac"
printf '#!/bin/sh\ncase "$1" in -s) echo Darwin ;; -m) echo x86_64 ;; *) echo Darwin ;; esac\n' > "$fakemac/uname"
printf '#!/bin/sh\nexit 1\n' > "$fakemac/codesign"; cp "$fakemac/codesign" "$fakemac/spctl"; chmod +x "$fakemac"/*
rm -f "$work/ran-mark"
expect "verify-release checks the signature before it runs a binary" 1 "macos-x86_64.tar.gz: qm: the signature does not verify" \
  env PATH="$fakemac:$PATH" RAN_MARK="$work/ran-mark" REQUIRE_SIGNATURE=1 bash "$p/scripts/verify-release.sh" "$work/rel-mark" "$DV" "$c"
[[ ! -e "$work/ran-mark" ]] && ok "an archive that fails the signature check is never run" || bad "binary ran before the signature check" "$LAST_OUT"
# qm passes and qm-privcheck fails: still refused and never run; both pass: run.
printf '#!/bin/sh\ncase "$1" in -dvv) echo "Authority=Developer ID Application: Test (TESTTEAM00)"; echo "flags=0x10000(runtime)" ;; --verify) for a; do case "$a" in */qm-privcheck) exit 1 ;; esac; done ;; esac\nexit 0\n' > "$fakemac/codesign"
printf '#!/bin/sh\necho "x: accepted source=Notarized Developer ID"\n' > "$fakemac/spctl"
rm -f "$work/ran-mark"
expect "verify-release refuses an archive whose helper fails the signature check" 1 "macos-x86_64.tar.gz: qm-privcheck: the signature does not verify" \
  env PATH="$fakemac:$PATH" RAN_MARK="$work/ran-mark" REQUIRE_SIGNATURE=1 bash "$p/scripts/verify-release.sh" "$work/rel-mark" "$DV" "$c"
[[ ! -e "$work/ran-mark" ]] && ok "an archive with one unsigned binary is never run" || bad "binary ran after a partial signature failure" "$LAST_OUT"
printf '#!/bin/sh\ncase "$1" in -dvv) echo "Authority=Developer ID Application: Test (TESTTEAM00)"; echo "flags=0x10000(runtime)" ;; esac\nexit 0\n' > "$fakemac/codesign"
rm -f "$work/ran-mark"
env PATH="$fakemac:$PATH" RAN_MARK="$work/ran-mark" REQUIRE_SIGNATURE=1 bash "$p/scripts/verify-release.sh" "$work/rel-mark" "$DV" "$c" > "$work/vr-signed.out" 2>&1
[[ -e "$work/ran-mark" ]] && grep -q "macos-x86_64.tar.gz: qm: Developer ID signature" "$work/vr-signed.out" \
  && ok "a signed archive is checked, then run" || bad "signed archive" "$(cat "$work/vr-signed.out")"

# notarize.sh: the submission name is a plain word (no path, no option).
fakebin="$work/notarize-bin"; : > "$work/notarize-target"
for badn in "../x" "-x" "a/b" "a b" ".hidden"; do
  expect "notarize refuses the submission name '$badn'" 1 "bad submission name" \
    bash "$here/notarize.sh" "$work/notarize-target" prof "$badn"
done
if [[ "$(uname -s)" == Darwin ]]; then
  # A run that dies while the quarantined binary runs leaves no quarantine
  # attribute behind. Stand-in tools (first on PATH): codesign, ditto, xcrun,
  # spctl and lipo accept everything; perl, which runs the binary, kills the
  # script with SIGTERM.
  mkdir -p "$fakebin"
  cat > "$fakebin/codesign" <<'EOF'
#!/bin/sh
case "$1" in -dvv) echo "Authority=Developer ID Application: Test (TESTTEAM00)"; echo "flags=0x10000(runtime)" ;; esac
exit 0
EOF
  printf '#!/bin/sh\n: > "$4"\n' > "$fakebin/ditto"
  printf '#!/bin/sh\necho "{\\"status\\":\\"Accepted\\",\\"id\\":\\"abc\\"}"\n' > "$fakebin/xcrun"
  printf '#!/bin/sh\necho "x: accepted source=Notarized Developer ID"\n' > "$fakebin/spctl"
  printf '#!/bin/sh\nuname -m\n' > "$fakebin/lipo"
  # The stand-in perl notes that the attribute is set, then kills the script
  # (the pid its starter wrote) and its own parent, the command substitution.
  printf '#!/bin/sh\nxattr -p com.apple.quarantine "$NBIN" > "$NMARK" 2>/dev/null\nkill -TERM "$(cat "$NPID")" $PPID\nsleep 1\n' > "$fakebin/perl"
  chmod +x "$fakebin"/*
  nbin="$work/notarize-bin-target"; printf '#!/bin/sh\necho ok\n' > "$nbin"; chmod 755 "$nbin"
  rm -f "$work/nmark"
  env PATH="$fakebin:$PATH" NBIN="$nbin" NMARK="$work/nmark" NPID="$work/npid" \
    sh -c 'echo $$ > "$NPID"; exec bash "$0" "$@"' "$here/notarize.sh" "$nbin" prof test-name >/dev/null 2>&1
  nrc=$?
  if [[ ! -s "$work/nmark" || "$nrc" == 0 ]]; then
    bad "notarize quarantine test did not reach the quarantined launch" "exit $nrc, mark: $(cat "$work/nmark" 2>/dev/null)"
  elif xattr -p com.apple.quarantine "$nbin" >/dev/null 2>&1; then
    bad "notarize left the quarantine attribute after a killed run" ""
  else
    ok "notarize removes the quarantine attribute when the run is killed"
  fi
fi

# A release build is made only in a clone of the public repository at the tag
# of its version.
expect "package refuses a release build outside the public repository" 1 "clone of the public repository" \
  bash "$p/scripts/package.sh" "$V" "$work/rel-priv"
git -C "$p" remote add origin "$PUBLIC_URL"
expect "package refuses a release build of an untagged commit" 1 "is not the tag v$V" \
  bash "$p/scripts/package.sh" "$V" "$work/rel-untagged"
git -C "$p" -c user.name=test -c user.email=test@example.com -c tag.gpgsign=false tag -a "v$V" -m "quartermaster $V"
git -C "$p" tag aaa-also-here   # a second tag on HEAD must not hide v$V
expect "package builds a release at the tag in the public repository" 0 "release archives" \
  bash "$p/scripts/package.sh" "$V" "$work/rel-tagged"
[[ -s "$work/rel-tagged/quartermaster-$V-linux-x86_64.tar.gz" ]] && ok "a release build has the release names" || bad "release names" "$(ls "$work/rel-tagged")"
expect "a relative output folder is the caller's" 0 "release archives" \
  bash -c 'cd "$1" && bash "$2/scripts/package.sh" "$3" rel-relative' _ "$work" "$p" "$V"
[[ -s "$work/rel-relative/quartermaster-$V-linux-x86_64.tar.gz" && ! -e "$p/rel-relative" ]] \
  && ok "the relative output folder got the archives" || bad "relative output folder" "$(ls "$p")"
git -C "$p" tag -d "v$V" aaa-also-here >/dev/null; git -C "$p" remote remove origin
[[ ! -e "$work/rel-priv" && ! -e "$work/rel-untagged" ]] \
  && ok "refused release builds created nothing" || bad "refused builds created output" "$(ls -d "$work"/rel-priv "$work"/rel-untagged 2>&1)"

for f in THIRD_PARTY_NOTICES.md plugins/qm-worker/.claude-plugin/plugin.json swift/privcheck.swift; do
  pm="$(make_private "pk-no-${f##*/}")"
  git -C "$pm" rm -q -- "$f"; commit_all "$pm" "drop $f"
  expect "package refuses a build without $f" 1 "$f is missing" dev bash "$pm/scripts/package.sh" "$V" "$work/rel-no-${f##*/}"
done
echo dirty >> "$p/README.md"
expect "package refuses a dirty tree" 1 "not clean" dev bash "$p/scripts/package.sh" "$V" "$work/rel4"
git -C "$p" checkout -q -- README.md
expect "package refuses a bad version" 1 "bad version" dev bash "$p/scripts/package.sh" "v1" "$work/rel5"
expect "package refuses NOTARY_PROFILE without SIGN_IDENTITY" 1 "NOTARY_PROFILE needs SIGN_IDENTITY" \
  dev env NOTARY_PROFILE=x bash "$p/scripts/package.sh" "$V" "$work/rel6"

# --- render-formula.sh and tap.sh -------------------------------------------------

s1="$(printf 'a%.0s' $(seq 1 64))"; s2="$(printf 'b%.0s' $(seq 1 64))"
s3="$(printf 'c%.0s' $(seq 1 64))"; s4="$(printf 'd%.0s' $(seq 1 64))"
formula="$(bash "$here/render-formula.sh" 1.2.3 "$s1" "$s2" "$s3" "$s4" 2>&1)"
# The address tap.sh downloads the archives from, as tap.sh builds it: the
# formula must point at the same files.
# shellcheck disable=SC2034 # VERSION is read by the evaluated DL line
tap_dl="$(VERSION=1.2.3; eval "$(sed -n -e '/^REPO=/p' -e '/^DL=/p' "$here/tap.sh")"; echo "${DL:-}")"
[[ "$tap_dl" == "https://github.com/IZZY-Agency/quartermaster/releases/download/v1.2.3" ]] \
  && ok "tap.sh downloads from the release's own address" || bad "tap.sh download address" "'$tap_dl'"
f_fail=""
for needle in 'class Quartermaster < Formula' 'homepage "https://github.com/IZZY-Agency/quartermaster"' 'license "MIT"' \
  'strategy :github_latest' '    bin.install "qm"
' '    bin.install "qm-privcheck" if OS.mac?' '    pkgshare.install "share/quartermaster/plugins"' \
  'def caveats' 'Run qm init' 'shell_output("#{bin}/qm --version")' 'assert_match "qm #{version} ("' \
  "url \"$tap_dl/quartermaster-1.2.3-macos-arm64.tar.gz\"
      sha256 \"$s1\"" \
  "url \"$tap_dl/quartermaster-1.2.3-macos-x86_64.tar.gz\"
      sha256 \"$s2\"" \
  "url \"$tap_dl/quartermaster-1.2.3-linux-arm64.tar.gz\"
      sha256 \"$s3\"" \
  "url \"$tap_dl/quartermaster-1.2.3-linux-x86_64.tar.gz\"
      sha256 \"$s4\""; do
  [[ "$formula" == *"$needle"* ]] || f_fail="$f_fail
missing: $needle"
done
[[ "$formula" == *"{{"* ]] && f_fail="$f_fail
a placeholder is left"
[[ "$formula" == *"github.com/github.com"* ]] && f_fail="$f_fail
an address has the host twice"
[[ -z "$f_fail" ]] && ok "render_formula_installs_plugin_and_helper: qm, the helper on macOS, the plugin into pkgshare, the caveat and the version test" \
  || bad "render_formula_installs_plugin_and_helper" "$f_fail
$formula"
expect "render refuses a bad checksum" 1 "sha256" bash "$here/render-formula.sh" 1.2.3 "$s1" "$s2" "$s3" nothex
expect "render refuses a bad version" 1 "version" bash "$here/render-formula.sh" 'v1;x' "$s1" "$s2" "$s3" "$s4"

# A tap checkout for a rehearsal: its origin is a local bare repository
# standing in for the tap, and its README lists packages.
make_tap() {
  local b="$work/tap-$1.git" seed="$work/tap-seed-$1" t="$work/tap-$1"
  git init -q --bare -b main "$b"
  git init -q -b main "$seed"
  printf '# tap\n\n```sh\nbrew install izzy-agency/tap/other\n```\n\n| Package | What | Source |\n|---|---|---|\n| `other` (formula) | Other tool | x |\n' > "$seed/README.md"
  mkdir -p "$seed/Formula"; echo '# other' > "$seed/Formula/other.rb"
  commit_all "$seed" seed
  git -C "$seed" push -q "$b" HEAD:refs/heads/main
  git clone -q "$b" "$t"
  echo "$t"
}
p="$(make_private tp)"
mkdir -p "$p/bin/release"
git -C "$p" remote add origin "$PUBLIC_URL"
git -C "$p" -c user.name=test -c user.email=test@example.com -c tag.gpgsign=false tag -a "v$V" -m "quartermaster $V"
bash "$p/scripts/package.sh" "$V" "$p/bin/release/$V" >/dev/null 2>&1
pub="$work/published"; cp -R "$p/bin/release/$V" "$pub"
tap_env() { env TAP_TEST_ASSETS_DIR="$pub" TAP_TEST_REMOTE="$work/tap-$1.git" TAP_NO_STYLE=1 PUBLISH_IDENT="$IDENT" "${@:2}"; }
tap="$(make_tap t1)"
expect "tap_rehearsal: tap writes and commits the formula" 0 "REHEARSAL: no push command" tap_env t1 bash "$p/scripts/tap.sh" "$tap"
tap_fail=""
grep -q "quartermaster-$V-macos-arm64.tar.gz" "$tap/Formula/quartermaster.rb" || tap_fail="no archive url"
sha_pub="$(cut -d' ' -f1 "$pub/quartermaster-$V-linux-arm64.tar.gz.sha256")"
grep -q "sha256 \"$sha_pub\"" "$tap/Formula/quartermaster.rb" || tap_fail="$tap_fail; no published checksum"
grep -q 'pkgshare.install "share/quartermaster/plugins"' "$tap/Formula/quartermaster.rb" || tap_fail="$tap_fail; no plugin install"
grep -q 'brew install izzy-agency/tap/quartermaster' "$tap/README.md" || tap_fail="$tap_fail; no install line"
grep -q '`quartermaster` (formula)' "$tap/README.md" || tap_fail="$tap_fail; no table row"
[[ "$(git -C "$tap" log -1 --format='%s|%an <%ae>')" == "quartermaster $V|$IDENT" ]] || tap_fail="$tap_fail; commit $(git -C "$tap" log -1 --format='%s|%an <%ae>')"
[[ "$LAST_OUT" != *"push --no-follow-tags"* ]] || tap_fail="$tap_fail; a rehearsal printed a push command"
[[ -z "$tap_fail" ]] && ok "tap_rehearsal: the tap commit holds the formula, README lines and identity" || bad "tap_rehearsal: tap result" "$tap_fail"
git -C "$tap" push -q "$work/tap-t1.git" HEAD:refs/heads/main   # the manual push, to the stand-in
expect "tap_rehearsal: with nothing new there is nothing to commit" 0 "nothing to commit" tap_env t1 bash "$p/scripts/tap.sh" "$tap"
tap2="$(make_tap t2)"
cp "$pub/quartermaster-$V-linux-arm64.tar.gz" "$work/keep.tgz"
printf 'x' >> "$pub/quartermaster-$V-linux-arm64.tar.gz"
expect "tap refuses a published archive that does not match its checksum file" 1 "hashes to" \
  tap_env t2 bash "$p/scripts/tap.sh" "$tap2"
cp "$work/keep.tgz" "$pub/quartermaster-$V-linux-arm64.tar.gz"
cp "$p/bin/release/$V/quartermaster-$V-linux-arm64.tar.gz.sha256" "$work/keep.sha256"
printf '%064d  quartermaster-%s-linux-arm64.tar.gz\n' 0 "$V" > "$p/bin/release/$V/quartermaster-$V-linux-arm64.tar.gz.sha256"
expect "tap refuses when the local build's checksum differs from the published one" 1 "local" \
  tap_env t2 bash "$p/scripts/tap.sh" "$tap2"
cp "$work/keep.sha256" "$p/bin/release/$V/quartermaster-$V-linux-arm64.tar.gz.sha256"
[[ -z "$(git -C "$tap2" status --porcelain)" && "$(git -C "$tap2" rev-list --count HEAD)" == 1 ]] \
  && ok "a refused tap run leaves the tap untouched" || bad "tap untouched" "$(git -C "$tap2" status --short)"
tap3="$(make_tap t3)"
git -C "$tap3" remote set-url origin "$TAP_URL"
expect "a tap rehearsal refuses a checkout whose origin is the real tap" 1 "not the tap checkout" \
  tap_env t3 bash "$p/scripts/tap.sh" "$tap3"
expect "TAP_TEST_ASSETS_DIR without TAP_TEST_REMOTE is refused" 1 "needs TAP_TEST_REMOTE" \
  env TAP_TEST_ASSETS_DIR="$pub" PUBLISH_IDENT="$IDENT" bash "$p/scripts/tap.sh" "$tap3"
[[ -z "$(git -C "$tap3" status --porcelain)" && "$(git -C "$tap3" rev-list --count HEAD)" == 1 ]] \
  && ok "the real-origin tap checkout is untouched" || bad "real tap touched" "$(git -C "$tap3" status --short)"
tap4="$(make_tap t4)"; mkdir -p "$tap4/Formula/sub"
expect "tap refuses a folder inside the checkout" 1 "not the checkout root" tap_env t4 bash "$p/scripts/tap.sh" "$tap4/Formula"
expect "a relative tap path is the caller's folder" 0 "REHEARSAL: no push command" \
  tap_env t4 bash -c 'cd "$1" && bash "$2/scripts/tap.sh" tap-t4' _ "$work" "$p"
tap5="$(make_tap t5)"
fakebrew="$work/fake-brew"; mkdir -p "$fakebrew"
printf '#!/bin/sh\necho "style: offence" >&2\nexit 1\n' > "$fakebrew/brew"; chmod +x "$fakebrew/brew"
expect "a formula brew style rejects is refused" 1 "brew style rejected" \
  env PATH="$fakebrew:$PATH" TAP_TEST_ASSETS_DIR="$pub" TAP_TEST_REMOTE="$work/tap-t5.git" PUBLISH_IDENT="$IDENT" \
  bash "$p/scripts/tap.sh" "$tap5"
[[ -z "$(git -C "$tap5" status --porcelain)" && ! -e "$tap5/Formula/quartermaster.rb" ]] \
  && ok "a rejected formula leaves the tap as it was" || bad "tap left dirty" "$(git -C "$tap5" status --short)"
tap6="$(make_tap t6)"; git -C "$tap6" config remote.origin.pushurl "$TAP_URL"
expect "tap refuses a checkout with a push URL" 1 "push URL" tap_env t6 bash "$p/scripts/tap.sh" "$tap6"
tap7="$(make_tap t7)"; git -C "$tap7" remote add upstream "$TAP_URL"
expect "a tap rehearsal refuses a checkout with the real tap as any remote" 1 "a remote that is the tap" tap_env t7 bash "$p/scripts/tap.sh" "$tap7"
for u in "git@github.com:IZZY-Agency/homebrew-tap.git" "https://github.com/izzy-agency/Homebrew-Tap/" \
         "git@github.com-work:IZZY-Agency/homebrew-tap.git"; do
  tap7b="$(make_tap "t7b-$(printf '%s' "$u" | cksum | cut -d' ' -f1)")"; git -C "$tap7b" remote add upstream "$u"
  expect "a tap rehearsal refuses a checkout whose remote is $u" 1 "a remote that is the tap" \
    tap_env "t7b-$(printf '%s' "$u" | cksum | cut -d' ' -f1)" bash "$p/scripts/tap.sh" "$tap7b"
done
expect "TAP_NO_STYLE=1 in a real run is refused" 1 "rehearsals only" \
  env TAP_NO_STYLE=1 PUBLISH_IDENT="$IDENT" bash "$p/scripts/tap.sh" "$tap7"
if [[ "$(id -u)" != 0 ]]; then
  tap8="$(make_tap t8)"
  chmod 555 "$tap8"   # Formula/ stays writable: the formula is written, then the README update aborts
  out="$(tap_env t8 bash "$p/scripts/tap.sh" "$tap8" 2>&1)"; rc=$?
  chmod 755 "$tap8"
  [[ "$rc" != 0 && -z "$(git -C "$tap8" status --porcelain)" && ! -e "$tap8/Formula/quartermaster.rb" ]] \
    && ok "an abort after the formula is written restores the tap" || bad "abort left the tap dirty" "exit $rc: $(git -C "$tap8" status --short)
$out"
fi

# --- release.sh ----------------------------------------------------------------------

# A public repository stand-in with v$V tagged, a fake gh that records its
# calls, and a dry run: it checks, prints the plan, and builds or publishes
# nothing.
p="$(make_private rl)"
pubb="$work/release-public.git"
git init -q --bare -b main "$pubb"
git -C "$p" push -q "$pubb" HEAD:refs/heads/main
git -C "$p" -c user.name=test -c user.email=test@example.com -c tag.gpgsign=false tag -a "v$V" -m "quartermaster $V"
git -C "$p" push -q "$pubb" "refs/tags/v$V"
fakebin="$work/fake-gh"; mkdir -p "$fakebin"; ghlog="$work/gh.log"; : > "$ghlog"
cat > "$fakebin/gh" <<EOF
#!/bin/sh
echo "\${GH_HOST:-}|\${GH_REPO:-}|\$*" >> "$ghlog"
case "\$1 \$2" in
  "auth status") exit 0 ;;
  "release view") [ -n "\${FAKE_RELEASE_EXISTS:-}" ] && exit 0; exit 1 ;;
esac
exit 0
EOF
chmod +x "$fakebin/gh"
rel() { env PATH="$fakebin:$PATH" RELEASE_TEST_REMOTE="$pubb" SIGN_IDENTITY="Developer ID Application: Test" NOTARY_PROFILE=test \
  bash "$p/scripts/release.sh" "$@"; }
expect "release.sh is a dry run by default" 0 "dry run: nothing was built or published" rel
r_fail=""
for needle in "make release" "make verify-release REQUIRE_SIGNATURE=1" "gh release create v$V" \
  "--repo github.com/IZZY-Agency/quartermaster" "quartermaster-$V-linux-x86_64.tar.gz.sha256" "First release." "scripts/tap.sh" \
  "release page: https://github.com/IZZY-Agency/quartermaster/releases/tag/v$V"; do
  [[ "$LAST_OUT" == *"$needle"* ]] || r_fail="$r_fail; missing: $needle"
done
[[ "$LAST_OUT" == *Older* ]] && r_fail="$r_fail; notes hold the older entry"
[[ "$LAST_OUT" == *"github.com/github.com"* ]] && r_fail="$r_fail; an address has the host twice"
grep -q 'release create' "$ghlog" && r_fail="$r_fail; gh release create was called"
[[ -e "$p/bin/release" ]] && r_fail="$r_fail; something was built"
[[ -z "$r_fail" ]] && ok "the dry run prints the plan and the notes, and runs nothing" || bad "dry run" "${r_fail#; }
$LAST_OUT"
# --go cannot run here (it needs real signing and refuses a stand-in), so its
# release address is checked in the script's text: REPO holds the host, and no
# code line puts it in again after a scheme.
rs="$here/release.sh"
twice="$(grep -nE '^[[:space:]]*[^#[:space:]].*[a-z]+://[^[:space:]"]*github\.com/\${?REPO' "$rs")"
[[ -z "$twice" ]] && ok "release.sh never builds an address with the host before \$REPO" || bad "release.sh adds the host to \$REPO" "$twice"
if grep -q '^||release view v'"$V"' --repo github.com/IZZY-Agency/quartermaster' "$ghlog"; then
  ok "gh is called with the host in --repo and no GH_HOST or GH_REPO"
else
  bad "gh call" "$(cat "$ghlog")"
fi
: > "$ghlog"
expect "release.sh ignores GH_HOST and GH_REPO" 0 "dry run" env GH_HOST=elsewhere.example GH_REPO=someone/else \
  PATH="$fakebin:$PATH" RELEASE_TEST_REMOTE="$pubb" SIGN_IDENTITY="Developer ID Application: Test" NOTARY_PROFILE=test \
  bash "$p/scripts/release.sh"
grep -q 'elsewhere\|someone/else' "$ghlog" && bad "gh saw GH_HOST or GH_REPO" "$(cat "$ghlog")" || ok "gh never sees GH_HOST or GH_REPO"
FAKE_RELEASE_EXISTS=1 expect "release.sh refuses a release that exists" 1 "already exists" rel
git -C "$p" push -q "$pubb" ":refs/tags/v$V"
expect "release.sh refuses when the tag is not on the public repository" 1 "not on the public repository" rel
expect "release.sh refuses unknown options" 2 "usage" rel --push
# The release's make sees no MAKEFLAGS, MAKEFILES or Go switches from the
# environment, and REQUIRE_SIGNATURE=1 as an argument wins.
mkdir -p "$work/mk"
printf 'y:\n\t@echo "swiftc=[$$SWIFTC$$SDKROOT$$TOOLCHAINS]"\n' > "$work/mk/Makefile.y"
printf 'x:\n\t@echo "sig=$(REQUIRE_SIGNATURE) extra=$(EXTRA) goflags=[$$GOFLAGS] goenv=[$$GOENV] gotoolchain=[$$GOTOOLCHAIN]"\n' > "$work/mk/Makefile"
printf 'EXTRA := from-makefiles\n' > "$work/mk/extra.mk"
eval "$(sed -n '/^clean_make() {/,/^}/p' "$here/release.sh")"
mk_out="$(cd "$work/mk" && MAKEFLAGS='REQUIRE_SIGNATURE=0' MAKEFILES="$work/mk/extra.mk" GOFLAGS=-overlay=/x GOTOOLCHAIN=go1.0 \
  clean_make -s x REQUIRE_SIGNATURE=1 2>&1)"
[[ "$mk_out" == "sig=1 extra= goflags=[] goenv=[off] gotoolchain=[]" ]] \
  && ok "the release's make ignores MAKEFLAGS, MAKEFILES and Go switches" || bad "clean_make" "$mk_out"
sw_out="$(cd "$work/mk" && SWIFTC=/evil/swiftc SDKROOT=/x TOOLCHAINS=y clean_make -s -f Makefile.y y 2>&1)"
[[ "$sw_out" == "swiftc=[]" ]] && ok "the release's make does not see SWIFTC" || bad "clean_make SWIFTC" "$sw_out"
grep -q 'clean_make verify-release REQUIRE_SIGNATURE=1' "$here/release.sh" \
  && ok "release.sh passes REQUIRE_SIGNATURE=1 to make as an argument" || bad "release.sh verify call" ""
: > "$ghlog"
expect "release.sh --go refuses RELEASE_TEST_REMOTE" 1 "never runs with RELEASE_TEST_REMOTE" rel --go
grep -q 'release create' "$ghlog" && bad "--go with a stand-in called gh release create" "" || ok "--go with a stand-in called nothing"

# --- this repository -------------------------------------------------------------

# changelog_must_open_with_version: make check's changelog gate passes only
# when CHANGELOG.md's first "## " heading is the Makefile's RELEASE_VERSION.
mk_version="$(sed -n 's/^RELEASE_VERSION := //p' "$repo/Makefile")"
printf '# Changelog\n\n## %s\n\nThe release.\n' "$mk_version" > "$work/cl-good.md"
printf '# Changelog\n\n## 0.0.1-other\n\nAnother.\n\n## %s\n\nThe release.\n' "$mk_version" > "$work/cl-second.md"
printf '# Changelog\n\nNothing yet.\n' > "$work/cl-none.md"
expect "changelog_must_open_with_version: a changelog that opens with the version passes" 0 "-" \
  make -s -C "$repo" changelog-check CHANGELOG="$work/cl-good.md"
expect "changelog_must_open_with_version: a changelog whose first entry is another version fails" 2 "must open with '## $mk_version'" \
  make -s -C "$repo" changelog-check CHANGELOG="$work/cl-second.md"
expect "changelog_must_open_with_version: a changelog without an entry fails" 2 "must open with '## $mk_version'" \
  make -s -C "$repo" changelog-check CHANGELOG="$work/cl-none.md"
expect "changelog_must_open_with_version: this repository's changelog opens with the version" 0 "-" \
  make -s -C "$repo" changelog-check
# A release cannot pack a stale notices file: make release runs the notices
# check before it packs anything.
plan="$(make -s -n -C "$repo" release 2>/dev/null)"
before="${plan%%scripts/package.sh*}"
[[ "$plan" == *"scripts/package.sh"* && "$before" == *"scripts/notices.sh --check"* ]] \
  && ok "make release checks the notices before it packs" || bad "make release does not check the notices first" "$plan"
# Of .github, only the CI workflow is exported, by name.
gh_allowed="$(printf '%s\n' "${ALLOW[@]}" | grep '^\.github' | tr '\n' ' ')"
gh_kinds="$(sed -n '/^KINDS=(/,/^)/p' "$here/publish.sh" | grep '"\.github[:=]' | tr -d ' "' | tr '\n' ' ')"
[[ "$gh_allowed" == ".github/workflows/ci.yml " && "$gh_kinds" == ".github:.yml " ]] \
  && ok "of .github, only ci.yml is exported, by name" || bad "the .github export set changed" "allowlist: $gh_allowed
kinds: $gh_kinds"
# Nothing private is in the allowlist, and what the design exports is.
a_fail=""
for path in "${ALLOW[@]}"; do
  case "$path" in "$dd"|"$dd"/*|.claude|.claude/*|.codex|.codex/*|CLAUDE.md|AGENTS.md|.env*) a_fail="$a_fail $path" ;; esac
done
for path in .claude-plugin/marketplace.json plugins cmd internal swift scripts testdata go.mod go.sum Makefile LICENSE \
            THIRD_PARTY_NOTICES.md README.md SECURITY.md CHANGELOG.md .github/workflows/ci.yml .golangci.yml .gitignore; do
  [[ " ${ALLOW[*]} " == *" $path "* ]] || a_fail="$a_fail missing:$path"
done
[[ -z "$a_fail" ]] && ok "the allowlist holds what the design exports and nothing private" || bad "allowlist" "$a_fail"
# Allowlisted paths this repository does not track yet (a later change adds
# them; publish.sh refuses a commit without them).
for path in "${ALLOW[@]}"; do
  git -C "$repo" ls-files --error-unmatch -- "$path" >/dev/null 2>&1 || echo "note: not tracked here yet: $path"
done
# publish.sh's docs/ pointer gate, run on this repository's own export set
# (its regex, read from publish.sh): a stray pointer fails make check here,
# not on the day of a publication.
ptr_re="$(sed -n "s/.*grep -rIonE --exclude-dir=.git '\([^']*\)'.*/\1/p" "$here/publish.sh")"
if [[ -z "$ptr_re" ]]; then
  bad "the docs/ pointer regex was not found in publish.sh" ""
else
  ptrs="$(cd "$repo" && git ls-files -z -- "${ALLOW[@]}" | xargs -0 env LC_ALL=C grep -IonE "$ptr_re" 2>/dev/null || true)"
  [[ -z "$ptrs" ]] && ok "this repository's export set points at no $dd/ file" || bad "the export set points at $dd/ files" "$ptrs"
fi
# install_without_swiftc: on a Mac without a Swift compiler, make install
# skips the helper (and never installs a stale bin/qm-privcheck) but still
# installs qm and the worker plugin. IS_DARWIN=Darwin plays macOS anywhere;
# the copy is this repository's tracked files as they are in the work tree.
inst="$work/install-copy"; prefix="$work/install-prefix"; mkdir -p "$inst/bin"
git -C "$repo" ls-files -z | (cd "$repo" && tar --null -T - -cf -) | (cd "$inst" && tar -xf -)
printf '#!/bin/sh\necho stale\n' > "$inst/bin/qm-privcheck"; chmod 755 "$inst/bin/qm-privcheck"
expect "install_without_swiftc: make privcheck says it skips" 0 "skip: privacy helper (no swiftc)" \
  make -s -C "$inst" privcheck IS_DARWIN=Darwin SWIFTC="$work/no-such-swiftc"
expect "install_without_swiftc: make install skips the helper" 0 "skip: privacy helper (no swiftc)" \
  make -s -C "$inst" install IS_DARWIN=Darwin SWIFTC="$work/no-such-swiftc" PREFIX="$prefix"
[[ -x "$prefix/bin/qm" && -f "$prefix/share/quartermaster/plugins/qm-worker/.claude-plugin/plugin.json" && ! -e "$prefix/bin/qm-privcheck" ]] \
  && ok "install_without_swiftc: qm and the worker plugin are installed, the helper is not" \
  || bad "install_without_swiftc: what was installed" "$(cd "$prefix" 2>/dev/null && find . | LC_ALL=C sort)"
# package_needs_swiftc: a release archive without the helper is wrong, so
# package.sh refuses without a compiler, before it creates anything.
expect "package_needs_swiftc: package.sh refuses without a Swift compiler" 1 "no Swift compiler" \
  env PACKAGE_DEV=1 SWIFTC="$work/no-such-swiftc" bash "$repo/scripts/package.sh" "$mk_version" "$work/rel-noswift"
[[ ! -e "$work/rel-noswift" ]] && ok "package_needs_swiftc: nothing was created" || bad "package_needs_swiftc: the output folder was created" ""

x_fail=""
for s in publish.sh package.sh notarize.sh verify-release.sh tap.sh render-formula.sh release.sh release_test.sh notices.sh notices_test.sh; do
  [[ -x "$here/$s" && "$(git -C "$repo" ls-files -s "scripts/$s" | cut -d' ' -f1)" == 100755 ]] || x_fail="$x_fail scripts/$s"
done
[[ -z "$x_fail" ]] && ok "release scripts are executable, in git too" || bad "not executable in git:$x_fail" ""

if ((failures > 0)); then
  echo "$failures case(s) failed:$failed_names"
  exit 1
fi
echo "all cases passed"
