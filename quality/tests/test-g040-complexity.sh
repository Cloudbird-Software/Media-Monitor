#!/usr/bin/env bash
# test-g040-complexity.sh —— g040 抗复杂度关卡自测（W5-C1 .github#224 / ADR-0070 决策 3；
# 宪法 §4E 负控制：高复杂度/死通用性/wrapper/抑制无引用/RoT/LOC 预算逐条红，
# 好样例绿、阈值注入改判、带引用的抑制走豁免通道）。
set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
QUALITY=$(cd "$HERE/.." && pwd)
# shellcheck source=../lib/contract.sh
source "$QUALITY/lib/contract.sh" || { echo "contract.sh 加载失败" >&2; exit 1; }
qc_detect_python || exit 1
GATE="$QUALITY/gates/g040-complexity.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
PASS=0; FAIL=0
ok()  { echo "  ok   $1"; PASS=$((PASS + 1)); }
bad() { echo "  FAIL $1"; FAIL=$((FAIL + 1)); }
w() { cygpath -m "$1" 2>/dev/null || printf '%s' "$1"; }

rp() {
  QC_REPORT="$1" qc_py -c 'import json, os, sys
r = json.load(open(os.environ["QC_REPORT"], encoding="utf-8"))
sys.exit(0 if '"$2"' else 1)' </dev/null
}
has_rule() { rp "$1" 'any(v["rule"]=="'"$2"'" for v in r["violations"])'; }

mk_repo() { # 干净基底：入口 + 被引用的 clamp（复杂度 1，供阈值注入改判）
  local d="$TMP/$1"
  mkdir -p "$d/src/mod" "$d/quality"
  git -C "$d" init -q
  git -C "$d" config user.name fixture
  git -C "$d" config user.email fixture@test
  git -C "$d" config core.autocrlf false
  printf 'export { clamp } from "./mod/clamp";\n' >"$d/src/index.ts"
  printf 'export function clamp(x) {\n  if (x > 10) {\n    return 10;\n  }\n  return x;\n}\n' >"$d/src/mod/clamp.ts"
  printf 'version: 1\nexemptions: []\n' >"$d/quality/exemptions.yaml"
  git -C "$d" add -A && git -C "$d" commit -qm base
  FX="$d"
}
run_gate() { # <报告名> <env...>
  local rep; rep="$(w "$TMP/reports/$1")"; shift
  ( cd "$FX" && env GATE_REPORT_OUT="$rep" "$@" bash "$GATE" ) >/dev/null 2>&1
}

echo "== 好样例：干净 fixture（本地模式）→ 绿 =="
mk_repo clean
run_gate g040-clean.json
[[ $? -eq 0 ]] && ok "干净 fixture exit 0" || bad "干净 fixture 应 0"

echo "== 脏样例（PR 语境）：六类违规逐条红 =="
mk_repo dirty
BASE=$(git -C "$FX" rev-parse HEAD)
cat >"$FX/src/mess.ts" <<'EOF'
export function convoluted(a, b, c) {
  let r = 0;
  if (a) {
    for (let i = 0; i < b; i++) {
      while (c > 0) {
        if (a && b || c) {
          r += i;
        } else if (c) {
          r -= i;
        }
        c = c - 1;
      }
    }
  }
  if (a && b && c) {
    r = r * 2;
  }
  return r;
}
export function forwardIt(x) {
  return realWork(x);
}
function realWork(x) {
  return x * 2 + 1;
}
export function unusedThing() {
  return 1;
}
export function onceUsed(y) {
  return y + 1;
}
EOF
printf 'import { onceUsed } from "./mess";\nexport function caller(y) {\n  return onceUsed(y) * 2;\n}\n' >"$FX/src/caller.ts"
DIS='eslint-dis''able' # 拆形字面量：org 抑制预算对全树计 \b<标记>\b，自测脚本不自计入（ADR-0036 同纪律）
printf 'export const suppressed = 1; // %s-next-line no-unused-vars\n' "$DIS" >"$FX/src/suppressed.ts"
printf 'version: 1\nexemptions:\n  - gate: g030-nav\n    path: docs/x.md\n    reason: 缺 ref 的坏豁免\n' >"$FX/quality/exemptions.yaml"
git -C "$FX" add -A && git -C "$FX" commit -qm "dirty: 复杂度/死导出/wrapper/RoT/抑制/坏豁免"
run_gate g040-dirty.json GATE_BASE="$BASE"
[[ $? -eq 1 ]] && ok "脏 fixture exit 1（fail-fixable）" || bad "脏 fixture 应 1"
REP="$(w "$TMP/reports/g040-dirty.json")"
has_rule "$REP" cx-cognitive-complexity && ok "高认知复杂度被拦（cx-cognitive-complexity）" || bad "漏拦 cx-cognitive-complexity"
rp "$REP" '"convoluted" in next(v["message"] for v in r["violations"] if v["rule"]=="cx-cognitive-complexity")' \
  && ok "复杂度违规指认函数名 convoluted" || bad "复杂度消息缺函数名"
has_rule "$REP" cx-logicless-wrapper && ok "纯转发 wrapper 被拦（cx-logicless-wrapper）" || bad "漏拦 cx-logicless-wrapper"
rp "$REP" '"forwardIt" in next(v["message"] for v in r["violations"] if v["rule"]=="cx-logicless-wrapper") and "realWork" not in next(v["message"] for v in r["violations"] if v["rule"]=="cx-logicless-wrapper")' \
  && ok "wrapper 判定指认 forwardIt 且不误伤 realWork" || bad "wrapper 误报/漏报对象"
has_rule "$REP" cx-dead-export && ok "死通用性被拦（cx-dead-export）" || bad "漏拦 cx-dead-export"
has_rule "$REP" cx-premature-abstraction && ok "Rule-of-Three 被拦（onceUsed 引用点 1<3）" || bad "漏拦 cx-premature-abstraction"
has_rule "$REP" cx-suppression-growth && ok "无引用抑制标记被拦（cx-suppression-growth）" || bad "漏拦 cx-suppression-growth"
has_rule "$REP" cx-exemption-unref && ok "缺 ref 豁免被拦（cx-exemption-unref）" || bad "漏拦 cx-exemption-unref"
rp "$REP" 'all("ruleId=" in v["fixHint"] for v in r["violations"])' && ok "fixHint 全部含 ruleId（AC-1）" || bad "fixHint 缺 ruleId"
rp "$REP" 'not any("clamp" in v["message"] or "realWork" in v["message"] for v in r["violations"])' \
  && ok "有真实逻辑的符号（clamp/realWork）不误报" || bad "误报有逻辑符号"

echo "== 豁免通道：带 issue 引用的抑制标记 → 不拦、计入豁免指标 =="
mk_repo exempt
BASE=$(git -C "$FX" rev-parse HEAD)
printf 'export const oked = 1; // %s-next-line no-unused-vars #42\n' "$DIS" >"$FX/src/oked.ts"
printf 'import { oked } from "./oked";\nconst local = oked + 1;\nvoid local;\n' >"$FX/src/consume.ts"
git -C "$FX" add -A && git -C "$FX" commit -qm "suppression with ref"
run_gate g040-exempt.json GATE_BASE="$BASE"
[[ $? -eq 0 ]] && ok "带引用抑制 exit 0（零增长不适用）" || bad "带引用抑制应放行"
rp "$(w "$TMP/reports/g040-exempt.json")" 'r["metrics"]["cx.exemptionCount"]==1' \
  && ok "带引用抑制计入豁免行数指标" || bad "豁免行数指标异常"

echo "== 阈值注入改判 + LOC 预算 =="
mk_repo inject
run_gate g040-inject.json GATE_TH_G040_COMPLEXITY_MAX_COGNITIVE_COMPLEXITY=0
[[ $? -eq 1 ]] && has_rule "$(w "$TMP/reports/g040-inject.json")" cx-cognitive-complexity \
  && ok "max-cognitive 注入 0 → clamp(=1) 变红" || bad "阈值注入未改判"
mk_repo budget
BASE=$(git -C "$FX" rev-parse HEAD)
for i in 1 2 3 4 5 6 7 8; do printf 'export const pad%s = "%s";\n' "$i" "$i" >"$FX/src/pad$i.ts"; done
git -C "$FX" add -A && git -C "$FX" commit -qm "pad"
run_gate g040-budget.json GATE_BASE="$BASE" GATE_TH_G040_COMPLEXITY_MAX_NET_LOC=5
[[ $? -eq 1 ]] && has_rule "$(w "$TMP/reports/g040-budget.json")" cx-loc-budget \
  && ok "净增 LOC 超预算被拦（注入 max-net-loc=5）" || bad "LOC 预算未拦"

echo "== test-g040-complexity：PASS=$PASS FAIL=$FAIL =="
[ "$FAIL" -eq 0 ] || exit 1
exit 0
