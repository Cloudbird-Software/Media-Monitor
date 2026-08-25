#!/usr/bin/env bash
# quality/run-gates.sh — 本地/CI 同一编排器（CI 以 GATE_* env 注入 PR 上下文）。
# CI 语义以 .github/workflows/ci.yml 为准；本脚本覆盖本地可等价部分。
set -euo pipefail
cd "$(dirname "$0")/.."

MODE="${1:-pr}"

fail=0
step() { echo "== $1 =="; }

step "gofmt zero-diff (GO-1)"
if ! fmt=$(gofmt -l .); then echo "gofmt error" >&2; fail=1; fi
if [ -n "$fmt" ]; then echo "gofmt diff:"; printf '%s\n' "$fmt" >&2; fail=1; fi

step "go vet ./... (GO-2 explicit-error lint)"
if ! go vet ./... ; then fail=1; fi

step "arch check (GO-3): entries under cmd/, internal boundary, stdlib-only"
if ! bash quality/arch-check.sh; then fail=1; fi

step "tests go test ./... (property tests included)"
if ! go test ./... ; then fail=1; fi

step "adapt offline canary (schema_contract + fixture bindings)"
if ! go run ./cmd/mediactl adapt canary --offline; then fail=1; fi

if [ "$MODE" = "pr" ]; then
  step "PR metadata (advisory)"
  if [ -n "${GATE_PR:-}" ]; then
    echo "PR    : $GATE_PR (author ${GATE_PR_AUTHOR:-unknown})"
    if [ -n "${GATE_CARD:-}" ]; then
      echo "Card  : #$GATE_CARD"
    else
      echo "Card  : (absent — bootstrap/org exceptions only, see ADR-0092 precedent; noted as advisory here)"
    fi
  fi
fi

if [ "$fail" -ne 0 ]; then
  echo "run-gates FAIL" >&2
  exit 1
fi
echo "run-gates PASS ($MODE)"