#!/usr/bin/env bash
# g040-complexity.sh —— 抗复杂度关卡入口（W5-C1 .github#224 / ADR-0070 决策 3）。
# 薄壳模式同 g010：判定逻辑在 quality/lib/complexity_lint.py（认知复杂度简化启发
# ——署名 Sonar 认知复杂度口径 + CodeScene 因素清单；死通用性/Rule-of-Three/
# wrapper 套娃/净增 LOC 预算/抑制零增长/豁免审计）。
#
# 调用约定：
#   GATE_CONTRACT          contract.yaml 路径（默认本仓 quality/contract.yaml）
#   GATE_BASE/GATE_HEAD    PR 范围（LOC 预算/Rule-of-Three/抑制零增长需要；
#                         无 GATE_BASE=本地模式，只跑全量面检查）
#   GATE_REPORT_OUT/DIR    报告落盘（默认 <扫描根>/quality/reports/g040-complexity.json）
#   GATE_TH_G040_COMPLEXITY_*  阈值覆盖（仅测试注入；段名连字符归一为下划线）
# 退出码：0=pass 1=fail-fixable 3=infra-error（contract/exemptions 故障）
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=../lib/contract.sh
source "$ROOT/quality/lib/contract.sh" || { echo "g040: contract.sh 加载失败" >&2; exit 3; }

qc_detect_python || exit 3
SCAN_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || printf '%s' "$ROOT")"
cd "$SCAN_ROOT" || { echo "g040: 进不了扫描根 $SCAN_ROOT" >&2; exit 3; }
qc_py "$ROOT/quality/lib/complexity_lint.py" "$@"
