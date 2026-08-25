#!/usr/bin/env bash
# test-g020-arch.sh —— g020 结构架构关卡自测（W5-C1 .github#224 / ADR-0070 决策 1；
# 宪法 §4E 负控制：脏 fixture 逐条红（fixHint 含 ruleId）、好 fixture 绿、阈值注入改判）。
# depcruise 经 GATE_DEPCRUISE_JSON 注入缝喂结果（零 node 依赖的自测密闭性）。
set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
QUALITY=$(cd "$HERE/.." && pwd)
# shellcheck source=../lib/contract.sh
source "$QUALITY/lib/contract.sh" || { echo "contract.sh 加载失败" >&2; exit 1; }
qc_detect_python || exit 1
GATE="$QUALITY/gates/g020-arch.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
PASS=0; FAIL=0
ok()  { echo "  ok   $1"; PASS=$((PASS + 1)); }
bad() { echo "  FAIL $1"; FAIL=$((FAIL + 1)); }
w() { cygpath -m "$1" 2>/dev/null || printf '%s' "$1"; }

rp() { # rp <report> <py-expr（r=报告 dict）>
  QC_REPORT="$1" qc_py -c 'import json, os, sys
r = json.load(open(os.environ["QC_REPORT"], encoding="utf-8"))
sys.exit(0 if '"$2"' else 1)' </dev/null
}
has_rule() { rp "$1" 'any(v["rule"]=="'"$2"'" for v in r["violations"])'; }

mk_repo() { # mk_repo <名> —— 干净基底：单导出入口 + 实现模块
  local d="$TMP/$1"
  mkdir -p "$d/src/mod"
  git -C "$d" init -q
  git -C "$d" config user.name fixture
  git -C "$d" config user.email fixture@test
  git -C "$d" config core.autocrlf false
  printf 'export function greet(name: string): string {\n  return "Hello, " + name;\n}\n' >"$d/src/index.ts"
  printf 'export function helper(x: number): number {\n  const y = x + 1;\n  return y * 2 + x;\n}\n' >"$d/src/mod/helper.ts"
  git -C "$d" add -A && git -C "$d" commit -qm "base"
  FX="$d"
}
cruise_ok()  { printf '{"summary": {"error": 0, "warn": 0}, "violations": []}\n' >"$1"; }
cruise_bad() { printf '{"summary": {"error": 1, "warn": 0}, "violations": [{"rule": {"name": "domain-not-import-infra", "severity": "error"}, "from": "src/domain/x.ts", "to": "src/infra/y.ts"}]}\n' >"$1"; }
# run_gate <报告名> <env 赋值...>（fixture 仓语境，报告路径转 Windows 形）
run_gate() {
  local rep; rep="$(w "$TMP/reports/$1")"; shift
  ( cd "$FX" && env GATE_REPORT_OUT="$rep" "$@" bash "$GATE" ) >/dev/null 2>&1
}

echo "== 好样例：干净仓 + 零 depcruise 违规 → 绿 =="
mk_repo clean
cruise_ok "$TMP/cruise-ok.json"
run_gate g020-clean.json GATE_DEPCRUISE_JSON="$(w "$TMP/cruise-ok.json")"
[[ $? -eq 0 ]] && ok "干净 fixture exit 0" || bad "干净 fixture 应 0"
run_gate g020-clean.json GATE_DEPCRUISE_JSON="$(w "$TMP/cruise-ok.json")"
rp "$(w "$TMP/reports/g020-clean.json")" 'r["status"]=="pass"' && ok "报告 status=pass" || bad "status 异常"

echo "== 脏样例：跨层依赖 + 浅模块 + api-surface 未声明 → 逐条红 =="
mk_repo dirty
BASE=$(git -C "$FX" rev-parse HEAD)
mkdir -p "$FX/src/shallow"
printf 'export function implOne(x) { return x + 1; }\n' >"$FX/src/shallow/impl.ts"
printf 'export { implOne, implTwo, implThree } from "./impl";\n' >"$FX/src/shallow/index.ts"
printf 'export function implTwo(x) { return x + 2; }\nexport function implThree(x) { return x + 3; }\n' >>"$FX/src/shallow/impl.ts"
printf 'export function greet(name: string): string {\n  return "Hi, " + name;\n}\n' >"$FX/src/index.ts" # api-surface 改动
git -C "$FX" add -A && git -C "$FX" commit -qm "dirty: 浅模块 + api-surface 变更（无 trailer）"
cruise_bad "$TMP/cruise-bad.json"
run_gate g020-dirty.json GATE_DEPCRUISE_JSON="$(w "$TMP/cruise-bad.json")" GATE_BASE="$BASE"
[[ $? -eq 1 ]] && ok "脏 fixture exit 1（fail-fixable）" || bad "脏 fixture 应 1"
REP="$(w "$TMP/reports/g020-dirty.json")"
has_rule "$REP" arch-dependency && ok "跨层依赖被拦（arch-dependency）" || bad "漏拦 arch-dependency"
rp "$REP" '"domain-not-import-infra" in next(v["message"] for v in r["violations"] if v["rule"]=="arch-dependency")' \
  && ok "违规消息含 depcruise 规则名" || bad "消息缺 depcruise 规则名"
has_rule "$REP" arch-shallow-module && ok "浅模块被拦（arch-shallow-module）" || bad "漏拦 arch-shallow-module"
has_rule "$REP" arch-api-surface-undeclared && ok "api-surface 未声明被拦" || bad "漏拦 arch-api-surface-undeclared"
rp "$REP" 'all("ruleId=" in v["fixHint"] for v in r["violations"])' && ok "fixHint 全部含 ruleId（AC-1）" || bad "fixHint 缺 ruleId"

echo "== api-surface 合法路径：带 Api-Surface trailer → 放行 =="
mk_repo trailer
BASE=$(git -C "$FX" rev-parse HEAD)
printf 'export function greet(name: string): string {\n  return "Hey, " + name;\n}\n' >"$FX/src/index.ts"
git -C "$FX" add -A && git -C "$FX" commit -qm "api change

Api-Surface: greet 返回前缀调整"
cruise_ok "$TMP/cruise-ok2.json"
run_gate g020-trailer.json GATE_DEPCRUISE_JSON="$(w "$TMP/cruise-ok2.json")" GATE_BASE="$BASE"
[[ $? -eq 0 ]] && ok "带 trailer 的 api-surface 变更 exit 0" || bad "trailer 通道应放行"

echo "== 阈值注入（证明读 contract 不读硬编码）+ infra 语义 =="
mk_repo inject
cruise_ok "$TMP/cruise-ok3.json"
run_gate g020-inject.json GATE_DEPCRUISE_JSON="$(w "$TMP/cruise-ok3.json")" GATE_TH_G020_ARCH_EXPORTS_MAX=0
[[ $? -eq 1 ]] && ok "exports-max 注入 0 → 干净仓变红" || bad "阈值注入未改判"
run_gate g020-infra.json GATE_DEPCRUISE_JSON="$(w "$TMP/不存在.json")"
[[ $? -eq 3 ]] && ok "depcruise 结果缺失 exit 3（infra）" || bad "infra 应 3"

echo "== test-g020-arch：PASS=$PASS FAIL=$FAIL =="
[ "$FAIL" -eq 0 ] || exit 1
exit 0
