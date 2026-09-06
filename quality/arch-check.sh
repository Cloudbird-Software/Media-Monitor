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

# 2) every import must be stdlib or inside this module (internal/ boundary
#    is enforced by the module prefix check; stdlib set comes from the local
#    toolchain, so the check is exact for the compiling Go version)
STDLIB=$(go list std | tr '\n' ' ')
imports=$(go list -f '{{range .Imports}}{{.}} {{end}}' ./... | tr ' \n' '\n\n' | grep -v '^$' || true)
bad_external=""
# ADR-0100 exception: TLS/H2 fingerprint impersonation transport
# (bogdanfinn/tls-client + fhttp + utls + their transitives) is the single
# authorized stdlib-only exception; everything else still fails closed.
ADR_EXCEPTION='^(github\.com/bogdanfinn/.*|github\.com/andybalholm/brotli.*|github\.com/bdandy/.*|github\.com/quic-go/.*|github\.com/cloudflare/circl.*|github\.com/klauspost/compress.*|github\.com/jordanlewis/gcassert.*|github\.com/tam7t/hpkp.*|github\.com/xyproto/randomstring.*|github\.com/bwesterb/go-ristretto.*|go\.uber\.org/mock.*|github\.com/google/uuid.*|github\.com/stretchr/testify.*|github\.com/davecgh/go-spew.*|github\.com/pmezard/go-difflib.*|github\.com/rogpeppe/go-internal.*|github\.com/kr/.*|gopkg\.in/(check|yaml)\..*|golang\.org/x/.*)$'
while IFS= read -r imp; do
  [ -z "$imp" ] && continue
  case "$imp" in
    "$MOD"|"$MOD"/*) continue ;;
  esac
  if [[ " $STDLIB " == *" $imp "* ]]; then
    continue
  fi
  if echo "$imp" | grep -qE "$ADR_EXCEPTION"; then
    continue
  fi
  bad_external="$bad_external $imp"
done <<<"$imports"
if [ -n "$bad_external" ]; then
  echo "arch: non-module, non-stdlib imports found:$bad_external" >&2
  echo "arch: (stdlib-only policy as amended by ADR-0100; new exceptions need their own ADR)" >&2
  fail=1
fi

# 3) module graph policy（依赖边界 lint，ADR-0100 修订）：go.mod 依赖必须全部
#    落在 ADR-0100 例外集合内；出现集合外模块即失败。
if grep -qE '^require ' go.mod; then
  bad_mods=""
  while IFS= read -r line; do
    [ -z "$line" ] && continue
    modpath=$(echo "$line" | awk '{print $1}')
    case "$modpath" in
      "$MOD") continue ;;
    esac
    if echo "$modpath" | grep -qE "$ADR_EXCEPTION"; then
      continue
    fi
    bad_mods="$bad_mods $modpath"
  done <<<"$(go list -m all | tail -n +2)"
  if [ -n "$bad_mods" ]; then
    echo "arch: go.mod modules outside the ADR-0100 exception set:$bad_mods" >&2
    fail=1
  fi
fi

# 4) internal/ must never import upstream/ (INV-3: submodules are diffable
#    observation copies, not dependencies — ADR-0099). The check scans
#    import declarations textually so it also catches references that would
#    not compile.
bad_upstream=$(grep -rn --include='*.go' -E 'Media-Monitor/(upstream|vendor)' internal/ cmd/ 2>/dev/null || true)
if [ -n "$bad_upstream" ]; then
  echo "arch: internal/cmd imports upstream/ (INV-3, ADR-0099):" >&2
  echo "$bad_upstream" >&2
  fail=1
fi

if [ "$fail" -ne 0 ]; then
  echo "arch-check FAIL" >&2
  exit 1
fi
echo "arch-check PASS"