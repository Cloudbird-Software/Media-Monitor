#!/usr/bin/env bash
# test-dirty-pr.sh —— 脏 PR fixture e2e（W5-C1 .github#224 AC-1 / ADR-0070 验收 6）。
# 卡 AC-1：Given 锁定的脏 PR fixture（高复杂度/浅模块/跨层/抑制标记/平铺目录），
# When 关卡运行，Then 各违规被对应关卡逐条拦下且 fixHint 含 ruleId；修复后全绿。
# fixture 语料在 quality/fixtures/dirty-pr/（.txt 双后缀防自噬），临时 git 仓内组装。
set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
QUALITY=$(cd "$HERE/.." && pwd)
# shellcheck source=../lib/contract.sh
source "$QUALITY/lib/contract.sh" || { echo "contract.sh 加载失败" >&2; exit 1; }
qc_detect_python || exit 1
FIX="$QUALITY/fixtures/dirty-pr"
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

FX="$TMP/repo"
mkdir -p "$FX/src" "$FX/quality"
git -C "$FX" init -q
git -C "$FX" config user.name fixture
git -C "$FX" config user.email fixture@test
git -C "$FX" config core.autocrlf false
cp "$FIX/base/index.ts.txt" "$FX/src/index.ts"
cp "$QUALITY/glossary.yaml" "$FX/quality/glossary.yaml"
git -C "$FX" add -A && git -C "$FX" commit -qm "base: 干净基线"
BASE=$(git -C "$FX" rev-parse HEAD)

# ---- 脏 commit：五类违规一次落齐 ----
DIS='eslint-dis''able' # 拆形字面量：org 抑制预算全树计数，脚本不自计入（ADR-0036 同纪律）
cp "$FIX/dirty/messy.ts.txt" "$FX/src/messy.ts"
mkdir -p "$FX/src/shallow" "$FX/src/flat"
cp "$FIX/dirty/shallow-index.ts.txt" "$FX/src/shallow/index.ts"
cp "$FIX/dirty/shallow-impl.ts.txt" "$FX/src/shallow/impl.ts"
sed "s/__ESLINT_MARK__/$DIS/" "$FIX/dirty/suppressed.ts.txt" >"$FX/src/suppressed.ts"
i=1
while [ $i -le 13 ]; do
  sed "s/__N__/$i/g" "$FIX/dirty/flat.ts.txt" >"$FX/src/flat/f$i.ts"
  i=$((i + 1))
done
git -C "$FX" add -A && git -C "$FX" commit -qm "dirty: 五类违规（高复杂度/浅模块/跨层/抑制标记/平铺）"

# run_gates <报告前缀> <depcruise 注入文件> <want:g020/g030/g040> —— 三关卡断言退出码；
# g900 单独跑（fixture 无 baseline → skip=0，状态另行断言）
run_all_gates() {
  local tag="$1" cruise="$2" want="$3" g rc
  for g in g020-arch g030-nav g040-complexity g900-ratchet; do
    local expect="$want"
    [[ "$g" == g900-ratchet ]] && expect=0
    rc=0
    ( cd "$FX" && env GATE_REPORT_OUT="$(w "$TMP/$tag-$g.json")" GATE_BASE="$BASE" \
        GATE_DEPCRUISE_JSON="$(w "$cruise")" bash "$QUALITY/gates/$g.sh" ) >/dev/null 2>&1 || rc=$?
    [[ "$rc" == "$expect" ]] || bad "$tag/$g 退出码 $rc ≠ $expect"
  done
  ok "$tag：g020/g030/g040=${want}，g900=0（skip）"
}

CRUISE_DIRTY="$TMP/cruise-dirty.json"
cp "$FIX/dirty/depcruise-dirty.json.txt" "$CRUISE_DIRTY"
CRUISE_OK="$TMP/cruise-ok.json"
printf '{"summary": {"error": 0}, "violations": []}\n' >"$CRUISE_OK"

echo "== 脏 PR：五类违规逐条拦下（fixHint 含 ruleId）=="
run_all_gates dirty "$CRUISE_DIRTY" 1
D20="$(w "$TMP/dirty-g020-arch.json")"
D30="$(w "$TMP/dirty-g030-nav.json")"
D40="$(w "$TMP/dirty-g040-complexity.json")"
rp "$D20" 'any(v["rule"]=="arch-dependency" and "domain-not-import-infra" in v["message"] for v in r["violations"])' \
  && ok "①跨层依赖被 g020 拦（depcruise 规则名入消息）" || bad "跨层未拦"
rp "$D20" 'any(v["rule"]=="arch-shallow-module" and "src/shallow/index.ts" in v["file"] for v in r["violations"])' \
  && ok "②浅模块被 g020 拦（指认 src/shallow/index.ts）" || bad "浅模块未拦"
rp "$D40" 'any(v["rule"]=="cx-cognitive-complexity" and "messyProcess" in v["message"] for v in r["violations"])' \
  && ok "③高复杂度被 g040 拦（指认 messyProcess）" || bad "复杂度未拦"
rp "$D40" 'any(v["rule"]=="cx-suppression-growth" and ("eslint-dis""able") in v["message"] for v in r["violations"])' \
  && ok "④无引用抑制标记被 g040 拦（fixHint/消息含标记行）" || bad "抑制未拦"
rp "$D30" 'any(v["rule"]=="nav-flat-directory" and "src/flat/" in v["file"] for v in r["violations"])' \
  && ok "⑤平铺目录被 g030 拦（指认 src/flat/）" || bad "平铺未拦"
for rep in "$D20" "$D30" "$D40"; do
  rp "$rep" 'all("ruleId=" in v["fixHint"] for v in r["violations"])' \
    && ok "fixHint 全含 ruleId（$(basename "$rep")）" || bad "$(basename "$rep") fixHint 缺 ruleId"
done
rp "$(w "$TMP/dirty-g900-ratchet.json")" 'r["status"]=="skip"' \
  && ok "g900：fixture 无 baseline → skip（棘轮由 test-g900 专测）" || bad "g900 skip 语义异常"

echo "== 修复 PR：同一能力整洁写法 → 全绿（spec AC-7）=="
rm -rf "$FX/src/flat" "$FX/src/shallow" "$FX/src/messy.ts" "$FX/src/suppressed.ts"
cp "$FIX/fixed/clean.ts.txt" "$FX/src/clean.ts"
git -C "$FX" add -A && git -C "$FX" commit -qm "fixed: 拆函数降嵌套/删转发层/删抑制/目录归组"
run_all_gates fixed "$CRUISE_OK" 0
F40="$(w "$TMP/fixed-g040-complexity.json")"
rp "$F40" 'r["status"]=="pass" and r["metrics"]["violationCount"]==0' \
  && ok "修复版 g040 零违规（maxCognitive≤阈值）" || bad "修复版仍有违规"
F20="$(w "$TMP/fixed-g020-arch.json")"
rp "$F20" 'r["status"]=="pass" and r["metrics"]["violationCount"]==0' \
  && ok "修复版 g020 零违规" || bad "修复版 g020 仍有违规"

echo "== test-dirty-pr：PASS=$PASS FAIL=$FAIL =="
[ "$FAIL" -eq 0 ] || exit 1
exit 0
