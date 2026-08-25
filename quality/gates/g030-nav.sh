#!/usr/bin/env bash
# g030-nav.sh —— AI 导航关卡组入口（W5-C1 .github#224 / ADR-0070 决策 2）。
# 薄壳模式同 g010：判定逻辑在 quality/lib/nav_lint.py（目录文件数上限/repo-map
# 覆盖率/引用跳数/文档-符号验真/词表 lint——宪法 §9#5 自研度量，方法署名 aider
# repo-map）。全量本地检查，无 PR 语境要求。
#
# 调用约定：
#   GATE_CONTRACT          contract.yaml 路径（默认本仓 quality/contract.yaml）
#   GATE_REPORT_OUT/DIR    报告落盘（默认 <扫描根>/quality/reports/g030-nav.json）
#   GATE_TH_G030_NAV_*     阈值覆盖（仅测试注入；段名连字符归一为下划线）
# 退出码：0=pass 1=fail-fixable 3=infra-error（contract/glossary 故障）
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=../lib/contract.sh
source "$ROOT/quality/lib/contract.sh" || { echo "g030: contract.sh 加载失败" >&2; exit 3; }

qc_detect_python || exit 3
SCAN_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || printf '%s' "$ROOT")"
cd "$SCAN_ROOT" || { echo "g030: 进不了扫描根 $SCAN_ROOT" >&2; exit 3; }
qc_py "$ROOT/quality/lib/nav_lint.py" "$@"
