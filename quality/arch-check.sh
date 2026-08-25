#!/usr/bin/env bash
# quality/arch-check.sh — GO-3 (entries only under cmd/) + module boundaries
# (internal/ never imported from outside this module) + stdlib-only dependency
# policy (docs/OPERATIONS.md). Pure go-tooling based; fails loudly.
set -euo pipefail
cd "$(dirname "$0")/.."

MOD="github.com/Cloudbird-Software/Media-Monitor"
fail=0

# 1) package main must live under cmd/
mains=$(go list -f '{{if eq .Name "main"}}{{.ImportPath}}{{end}}' ./...)
while IFS= read -r m; do
  [ -z "$m" ] && continue
  if ! echo "$m" | grep -q "^$MOD/cmd/"; then
    echo "arch: package main outside cmd/: $m" >&2
    fail=1
  fi
done <<<"$mains"

# 2) everything in internal/ must not be imported by code outside this module
imports=$(go list -f '{{range .Imports}}{{.}} {{end}}' ./...)
bad_external=$(echo "$imports" | tr ' ' '\n' | grep -v "^$" | sort -u | grep -v "^$MOD/" | grep -v "^internal/" || true)
if [ -n "$bad_external" ]; then
  echo "arch: non-module imports found: $bad_external" >&2
  fail=1
fi

# 3) stdlib-only module graph（依赖边界 lint）
if grep -qE '^require ' go.mod; then
  echo "arch: go.mod has a require block (stdlib-only policy)" >&2
  fail=1
fi
mods=$(go list -m all | tail -n +2)
if [ -n "$mods" ]; then
  echo "arch: non-empty module graph: $(echo "$mods" | tr '\n' ' ')" >&2
  fail=1
fi

if [ "$fail" -ne 0 ]; then
  echo "arch-check FAIL" >&2
  exit 1
fi
echo "arch-check PASS"