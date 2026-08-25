#!/usr/bin/env bash
# g020-arch.sh —— 结构架构关卡入口（W5-C1 .github#224 / ADR-0070 决策 1）。
# 薄壳模式同 g010：判定逻辑在 quality/lib/arch_lint.py，本壳只做解释器探测与委派
# （IFACE-04 统一 CLI 契约）。扫描根=当前 git 仓 toplevel（无 git 语境退化为脚本
# 所在仓根）——fixture/临时仓自测靠这个换扫描面，契约仍读本仓 contract.yaml。
#
# 调用约定：
#   GATE_CONTRACT        contract.yaml 路径（默认本仓 quality/contract.yaml）
#   GATE_DEPCRUISE_JSON  depcruise 结果 JSON 文件注入缝（测试/无 node 语境替身）
#   GATE_BASE/GATE_HEAD  PR 范围（api-surface 声明检查；无 GATE_BASE=本地模式跳过）
#   GATE_REPORT_OUT/DIR  报告落盘路径（默认 <扫描根>/quality/reports/g020-arch.json）
#   GATE_TH_G020_ARCH_*  阈值覆盖（仅测试注入；段名连字符归一为下划线）
# 退出码：0=pass 1=fail-fixable 3=infra-error（depcruise/contract 故障）
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=../lib/contract.sh
source "$ROOT/quality/lib/contract.sh" || { echo "g020: contract.sh 加载失败" >&2; exit 3; }

qc_detect_python || exit 3
SCAN_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || printf '%s' "$ROOT")"
cd "$SCAN_ROOT" || { echo "g020: 进不了扫描根 $SCAN_ROOT" >&2; exit 3; }
qc_py "$ROOT/quality/lib/arch_lint.py" "$@"
