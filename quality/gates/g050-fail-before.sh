#!/usr/bin/env bash
# quality/gates/g050-fail-before.sh —— 测试先行 fail-before 关卡（W2-C2 .github#215 / ADR-0061 决策 1）
# 判定 AC-1：测试 commit 早于实现 commit（git 历史可证）+ 在测试 commit 上复现运行，
# 红必须是断言失败（解析输出签名：assertion/expect 类才算红；import/编译/收集错=无效红，判 exit 3）。
# 上下文注入：GATE_BASE/GATE_HEAD（commit 范围；无 GATE_BASE 时退化 origin/main）、
# GATE_CARD/GATE_PR（卡面——拓扑检查只对卡 PR 的新增验收测试生效）、
# GATE_TEST_RUNNER（运行器注入缝，默认取 contract.yaml g050.runner-cmd）。
set -euo pipefail
GATE_ID=g050-fail-before
# shellcheck source=../lib/lock/lib.sh
source "$(cd "$(dirname "$0")" && pwd)/../lib/lock/lib.sh"

ROOT=$(git rev-parse --show-toplevel 2>/dev/null) || gate_finish 3 infra-error "不在 git 仓内——g050 需要 git 历史"
cd "$ROOT"

BASE="${GATE_BASE:-}"
[[ -n "$BASE" ]] || BASE=$(git rev-parse --verify -q origin/main || true)
[[ -n "$BASE" ]] || gate_finish 0 skip "无基准分支（非 PR 上下文）——fail-before 检查不适用"
HEADC="${GATE_HEAD:-HEAD}"
RANGE="$BASE..$HEADC"

[[ -n "${GATE_CARD:-}" || -n "${GATE_PR:-}" ]] \
  || gate_finish 0 skip "非卡面 PR（无 GATE_CARD/GATE_PR）——test-first 拓扑为卡级约定（宪法 §4E）"

TEST_PATHS=$(contract_value g050-fail-before test-paths) || gate_finish 3 infra-error "test-paths 键缺失"
RUNNER="${GATE_TEST_RUNNER:-$(contract_value g050-fail-before runner-cmd)}" \
  || gate_finish 3 infra-error "runner-cmd 键缺失"
ASSERT_SIGS=$(contract_value g050-fail-before assert-signatures) || gate_finish 3 infra-error "assert-signatures 键缺失"
COLLECT_SIGS=$(contract_value g050-fail-before collection-signatures) || gate_finish 3 infra-error "collection-signatures 键缺失"

mapfile -t PATHS <<<"$TEST_PATHS"
mapfile -t NEW_TESTS < <(git diff --name-only --diff-filter=A "$BASE...$HEADC" -- "${PATHS[@]}")
((${#NEW_TESTS[@]})) || gate_finish 0 pass "无新增验收测试——fail-before 检查不适用"

# ---- 拓扑：test-author commit（改动全部落在 test-paths）必须先于实现 commit（有任何其它路径改动）----
first_test=""; first_impl=""
while read -r c; do
  files=$(git show --name-only --format= "$c")
  test_hit=0; non_test=0
  while IFS= read -r f; do
    hit=0
    for p in "${PATHS[@]}"; do [[ "$f" == "$p"* ]] && hit=1; done
    if [[ $hit -eq 1 ]]; then test_hit=1; else non_test=1; fi
  done <<<"$files"
  [[ -z "$first_impl" && $non_test -eq 1 ]] && first_impl="$c"
  [[ -z "$first_test" && $test_hit -eq 1 && $non_test -eq 0 ]] && first_test="$c"
done < <(git rev-list --reverse "$RANGE")

[[ -n "$first_impl" ]] || gate_finish 0 pass \
  "红阶段：仅测试 commit（$(git rev-parse --short "$first_test" 2>/dev/null || echo '?')）——实现 commit 落地后复评 fail-before"
[[ -n "$first_test" ]] || gate_finish 1 fail-fixable \
  "新增验收测试但无 test-author 先行 commit（实现先行，或测试与实现同 commit——历史不可证红）" \
  "拆 commit：验收测试单独先行 commit 且保证红（断言失败），实现随后"
git merge-base --is-ancestor "$first_test" "$first_impl" || gate_finish 1 fail-fixable \
  "拓扑违约：首个实现 commit（$(git rev-parse --short "$first_impl")）早于 test-author commit（$(git rev-parse --short "$first_test")）" \
  "rebase 重排：测试先行 commit 在前"

# ---- 红复现：在 test-author commit 的 worktree 上跑新测试并分类红的来源 ----
WT="$ROOT/reports/g050-worktree"
git worktree remove --force "$WT" >/dev/null 2>&1 || true
cleanup() { git worktree remove --force "$WT" >/dev/null 2>&1 || true; }
trap cleanup EXIT
mkdir -p "$ROOT/reports"
git worktree add --detach --quiet "$WT" "$first_test"

read -ra RA <<<"$RUNNER"
rc=0; out=$(cd "$ROOT" && "${RA[@]}" --root "$WT" "${NEW_TESTS[@]}" 2>&1) || rc=$?

sig_hit() { grep -qE "$2" <<<"$1"; }
if [[ $rc -eq 0 ]]; then
  gate_finish 1 fail-fixable "新测试在实现 commit 前已是绿——非 fail-before（或断言空转）" \
    "让测试在实现缺失时真实失败：断言目标行为而非只断言存在"
fi
if sig_hit "$out" "$COLLECT_SIGS"; then
  gate_finish 3 infra-error \
    "无效红：import/编译/收集错误（语法错不算红——ADR-0061 决策 1）；命中上下文：$(grep -m1 -E "$COLLECT_SIGS" <<<"$out" | head -c 200)" \
    "修正测试：spawn/import 异常折叠为哨兵值后再 expect（保证红是断言失败）"
fi
if sig_hit "$out" "$ASSERT_SIGS"; then
  gate_finish 0 pass \
    "fail-before 可证：test commit $(git rev-parse --short "$first_test") 早于实现 commit $(git rev-parse --short "$first_impl")，红=断言失败"
fi
gate_finish 3 infra-error "红但签名无法分类（判定器保守拒绝）" "把实际输出对齐 contract.yaml 签名表后复跑"
