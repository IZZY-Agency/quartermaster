#!/usr/bin/env bash
# End-to-end tests of the notices build (make notices-check runs them):
#   - notices_check_fails_when_stale: -check fails on a notices file that is
#     out of date, and passes on the committed one
# shellcheck disable=SC2015 # ok() always returns 0
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
repo="$(cd "$here/.." && pwd -P)"
work="$(mktemp -d)" || exit 2
work="$(cd "$work" && pwd -P)"
case "$work" in /?*) ;; *) echo "notices_test: bad scratch folder" >&2; exit 2 ;; esac
trap 'rm -rf "$work"' EXIT

failures=0
ok()  { echo "ok   $1"; }
bad() { echo "FAIL $1"; printf '%s\n' "$2" | sed 's/^/    /'; failures=$((failures + 1)); }
notices() { (cd "$repo" && go run ./internal/tools/notices "$@") 2>&1; }

cp "$repo/THIRD_PARTY_NOTICES.md" "$work/stale.md"
printf '\nedited by hand\n' >> "$work/stale.md"
out="$(notices -out "$work/stale.md" -check)"; rc=$?
[[ "$rc" == 1 && "$out" == *"is out of date"* ]] \
  && ok "notices_check_fails_when_stale: -check fails on a notices file that is out of date" \
  || bad "notices_check_fails_when_stale: -check on drift" "exit $rc: $out"
cmp -s "$work/stale.md" <(cat "$repo/THIRD_PARTY_NOTICES.md"; printf '\nedited by hand\n') \
  && ok "notices_check_fails_when_stale: -check writes nothing" || bad "notices_check_fails_when_stale: -check wrote the file" ""
out="$(notices -out "$repo/THIRD_PARTY_NOTICES.md" -check)"; rc=$?
[[ "$rc" == 0 ]] && ok "notices_check_fails_when_stale: -check passes on the committed file" || bad "-check on the committed file" "exit $rc: $out"
out="$(notices -out "$work/fresh.md")"; rc=$?
[[ "$rc" == 0 ]] && cmp -s "$work/fresh.md" "$repo/THIRD_PARTY_NOTICES.md" \
  && ok "make notices writes the committed file, byte for byte" || bad "regenerated notices differ" "exit $rc: $out"

if ((failures > 0)); then
  echo "$failures case(s) failed"
  exit 1
fi
echo "all cases passed"
