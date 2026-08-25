#!/usr/bin/env bash
# quality/lib/ratchet-update.sh —— 棘轮基线重算（W5-C1 .github#224 / ADR-0070 决策 4）。
# CI bot 专用执行面：重算全仓指标并写 quality/baseline.json。身份纪律：产物变更
# 只许 bot 提交——人类/agent 直接提交会被 g900-ratchet exit 2（提交者身份校验）；
# 指标变差时 bot 提交必须带 "Ratchet-Loosen: <ADR-NNNN|#NN>" trailer（人类批准）。
set -euo pipefail

LIB_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$LIB_DIR/../.." && pwd)"
# shellcheck source=contract.sh
source "$LIB_DIR/contract.sh"

qc_detect_python || exit 3
SCAN_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || printf '%s' "$REPO_ROOT")"
cd "$SCAN_ROOT" || { echo "ratchet-update: 进不了扫描根 $SCAN_ROOT" >&2; exit 3; }
qc_py "$REPO_ROOT/quality/lib/ratchet_lib.py" update
BASELINE=$(qc_get g900-ratchet.baseline-file)
echo "下一步（bot 通道）：以 bot 身份提交 $BASELINE——非 bot 提交会被 g900 exit 2"
