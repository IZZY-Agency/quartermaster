#!/usr/bin/env bash
# Write (or, with --check, verify) THIRD_PARTY_NOTICES.md with
# internal/tools/notices: Go's licence and the notices its standard library
# carries, and every Go module in the build of qm.
#
#   scripts/notices.sh            make notices
#   scripts/notices.sh --check    make notices-check (part of make check)
set -euo pipefail

check=()
case "${1:-}" in
  "") ;;
  --check) check=(-check) ;;
  *) echo "usage: scripts/notices.sh [--check]" >&2; exit 2 ;;
esac

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$ROOT"
go run ./internal/tools/notices ${check[@]+"${check[@]}"}
