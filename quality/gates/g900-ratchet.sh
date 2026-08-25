#!/usr/bin/env bash
# g900-ratchet.sh —— 棘轮关卡入口（W5-C1 .github#224 / ADR-0070 决策 4）。
# 薄壳模式同 g010：判定逻辑在 quality/lib/ratchet_lib.py（全仓指标只许变好：
# baseline.json 基线比对 + bot 写入纪律 + Ratchet-Loosen 放宽管制）。
#
# 调用约定：
#   GATE_CONTRACT          contract.yaml 路径（默认本仓 quality/contract.yaml）
#   GATE_DEPCRUISE_JSON    depcruise 结果注入缝（与 g020 同协议——collect 复用其扫描）
#   GATE_BASE/GATE_HEAD    PR 范围（写入纪律裁决；无 GATE_BASE=本地模式只做指标比对）
#   GATE_REPORT_OUT/DIR    报告落盘（默认 <扫描根>/quality/reports/g900-ratchet.json）
#   GATE_TH_G900_RATCHET_* 阈值/配置覆盖（仅测试注入）
# 退出码：0=pass（变好时提示 baseline 待更新）1=指标回归 2=写入纪律违规 3=infra
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=../lib/contract.sh
source "$ROOT/quality/lib/contract.sh" || { echo "g900: contract.sh 加载失败" >&2; exit 3; }

qc_detect_python || exit 3
SCAN_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || printf '%s' "$ROOT")"
cd "$SCAN_ROOT" || { echo "g900: 进不了扫描根 $SCAN_ROOT" >&2; exit 3; }
qc_py "$ROOT/quality/lib/ratchet_lib.py" "$@"
