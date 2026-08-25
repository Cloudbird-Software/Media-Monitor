#!/usr/bin/env bash
# g010-spec-schema.sh —— spec lint 关卡入口（W2-C1 .github#214 / ADR-0060）。
#
# 为什么是 bash 薄壳：关卡统一 CLI 契约（IFACE-04 / #127 §3.1）要求一个可执行入口；
# 判定逻辑在 quality/lib/spec_lint.py（零网络零 LLM 的确定性检查），本壳只做
# 解释器探测与委派，保证所有关卡入口形态一致（后续关卡照抄这个壳）。
#
# 调用约定（#127 §3.1 + 本卡扩展）：
#   quality/gates/g010-spec-schema.sh [spec.md ...]
#   GATE_CONTRACT       contract.yaml 路径（默认 quality/contract.yaml）
#   GATE_TASK_ID        不传文件参数时 → specs/<TASK_ID>/spec.md
#   GATE_SPEC           换行/冒号分隔的 spec 路径（无参数无 TASK_ID 时生效）
#   GATE_CARD           卡引用（如 Cloudbird-Software/.github#214），与 spec
#                       frontmatter card: 冲突=篡改嫌疑（exit 2）
#   GATE_TESTS_INDEX    测试索引文件（行格式：TEST-n TASK:AC-n）——孤儿/镀金/断链用
#   GATE_IR_INDEX       IR 账本本地快照（每行一个 IR-n）——不给则 IR 引用只做格式校验
#   GATE_REPORT_OUT     报告落盘路径（默认 quality/reports/g010-spec-schema.json）
#   GATE_TH_*           阈值覆盖（仅测试注入，见 quality/lib/contract.sh 头注）
#   GATE_BASE_SHA / GATE_HEAD_SHA / GATE_CHANGED_FILES —— ABI 兼容位，g010 全量跑
#
# 退出码（四值语义，AC-4）：
#   0=pass  1=fail-fixable（EARS/禁词/结构/追溯违规，agent 可自修）
#   2=fail-escalate（AC 重复定义/卡引用冲突等篡改类，只人类可解）
#   3=infra-error（文件读不了/contract 配错/报告不过 schema，重试不计红）
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=../lib/contract.sh
source "$ROOT/quality/lib/contract.sh" || { echo "g010: contract.sh 加载失败" >&2; exit 3; }

qc_detect_python || exit 3
cd "$ROOT" || { echo "g010: 进不了仓根 $ROOT" >&2; exit 3; }
qc_py "$ROOT/quality/lib/spec_lint.py" "$@"
