#!/usr/bin/env bash
# 套件执行器（adversary 目标目录契约）：bash run-suite.sh <impl-dir>
# impl-dir 须含 spec.md（被审"实现"= spec 文档对——IR-MM-0001 首件交付物是条款级
# 规格本身）；exit 0 = 全绿，非 0 = 套件不通过。
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"
IMPL="${1:?用法: run-suite.sh <impl-dir>}"
[[ -f "$IMPL/spec.md" ]] || { echo "impl 目录缺 spec.md: $IMPL" >&2; exit 2; }
PY="${METERING_PYTHON:-}"
if [[ -z "$PY" ]]; then
  PY=python3; command -v python3 >/dev/null 2>&1 || PY=python
fi
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/specs/IR-MM-0001/suite"
cp "$DIR"/suite/*.py "$TMP/specs/IR-MM-0001/suite/"
cp -- "$IMPL/spec.md" "$TMP/specs/IR-MM-0001/spec.md"
cd "$TMP/specs/IR-MM-0001/suite"
IMPL_DIR="$TMP/specs/IR-MM-0001" "$PY" test_spec_irmm0001.py
