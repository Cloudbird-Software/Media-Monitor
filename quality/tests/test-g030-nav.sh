#!/usr/bin/env bash
# test-g030-nav.sh —— g030 AI 导航关卡自测（W5-C1 .github#224 / ADR-0070 决策 2；
# 宪法 §4E 负控制：平铺目录/文档断链/词表未定义逐条红、好样例绿、阈值注入改判）。
# fixture 自带 quality/glossary.yaml（词表按扫描根解析——关卡与仓解耦的自证）。
set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
QUALITY=$(cd "$HERE/.." && pwd)
# shellcheck source=../lib/contract.sh
source "$QUALITY/lib/contract.sh" || { echo "contract.sh 加载失败" >&2; exit 1; }
qc_detect_python || exit 1
GATE="$QUALITY/gates/g030-nav.sh"
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

mk_repo() { # mk_repo <名> —— 干净基底：单入口 + 词表
  local d="$TMP/$1"
  mkdir -p "$d/src" "$d/quality"
  git -C "$d" init -q
  git -C "$d" config user.name fixture
  git -C "$d" config user.email fixture@test
  git -C "$d" config core.autocrlf false
  printf 'export function greet(name: string): string {\n  return "Hello, " + name;\n}\n' >"$d/src/index.ts"
  cp "$QUALITY/glossary.yaml" "$d/quality/glossary.yaml"
  FX="$d"
}
run_gate() { # <报告名> <env...>
  local rep; rep="$(w "$TMP/reports/$1")"; shift
  ( cd "$FX" && env GATE_REPORT_OUT="$rep" "$@" bash "$GATE" ) >/dev/null 2>&1
}

echo "== 好样例：干净 fixture → 绿，指标齐备（卡 AC-2：全部输出指标）=="
mk_repo clean
run_gate g030-clean.json
[[ $? -eq 0 ]] && ok "干净 fixture exit 0" || bad "干净 fixture 应 0"
REP="$(w "$TMP/reports/g030-clean.json")"
rp "$REP" 'all(k in r["metrics"] for k in ("nav.repomapCoverage","nav.repomapUncovered","nav.avgHops","nav.graphDiameter","nav.importCycles","nav.docDangling","nav.glossaryUndefined"))' \
  && ok "AC-2 指标齐备（覆盖率/跳数/文档验真/词表）" || bad "指标缺失"

echo "== 脏样例：平铺目录 + 文档断链 + 词表未定义/弃用词 → 逐条红 =="
mk_repo dirty
mkdir -p "$FX/src/flat"
i=1
while [ $i -le 13 ]; do
  printf 'export const flat%s = %s;\n' "$i" "$i" >"$FX/src/flat/f$i.ts"
  i=$((i + 1))
done
mkdir -p "$FX/docs"
printf '# 模块说明\n\n调用 `greet` 与 `ghost_symbol_xyz`；详见 XYZZY 协议；质量门已废弃叫法。\n' >"$FX/docs/guide.md"
printf 'version: 1\nacronyms:\n  ADR: 架构决策记录\ndeprecated:\n  质量门: 关卡\n' >"$FX/quality/glossary.yaml"
run_gate g030-dirty.json
[[ $? -eq 1 ]] && ok "脏 fixture exit 1（fail-fixable）" || bad "脏 fixture 应 1"
REP="$(w "$TMP/reports/g030-dirty.json")"
has_rule "$REP" nav-flat-directory && ok "平铺目录被拦（nav-flat-directory）" || bad "漏拦 nav-flat-directory"
rp "$REP" '"src/flat/" in next(v["file"] for v in r["violations"] if v["rule"]=="nav-flat-directory")' \
  && ok "平铺违规指认具体目录" || bad "平铺违规缺目录名"
has_rule "$REP" nav-doc-symbol-dangling && ok "文档断链被拦（ghost_symbol_xyz）" || bad "漏拦 nav-doc-symbol-dangling"
rp "$REP" '"ghost_symbol_xyz" in next(v["message"] for v in r["violations"] if v["rule"]=="nav-doc-symbol-dangling")' \
  && ok "断链指认具体符号" || bad "断链消息缺符号名"
has_rule "$REP" nav-glossary-undefined && ok "未入表缩写被拦（XYZZY）" || bad "漏拦 nav-glossary-undefined"
has_rule "$REP" nav-glossary-deprecated && ok "弃用词被拦（质量门→关卡）" || bad "漏拦 nav-glossary-deprecated"
rp "$REP" 'all("ruleId=" in v["fixHint"] for v in r["violations"])' && ok "fixHint 全部含 ruleId（AC-1）" || bad "fixHint 缺 ruleId"
rp "$REP" '"greet" not in "".join(v["message"] for v in r["violations"])' && ok "真实符号 greet 不误报" || bad "误报真实符号"

echo "== 阈值注入改判（证明读 contract 不读硬编码）=="
mk_repo inject
run_gate g030-inject.json GATE_TH_G030_NAV_MAX_FILES_PER_DIR=0
[[ $? -eq 1 ]] && has_rule "$(w "$TMP/reports/g030-inject.json")" nav-flat-directory \
  && ok "max-files-per-dir 注入 0 → 干净仓变红" || bad "阈值注入未改判"
run_gate g030-inject2.json GATE_TH_G030_NAV_GLOSSARY_FILE="quality/不存在.yaml"
[[ $? -eq 3 ]] && ok "词表缺失 exit 3（infra，fail-closed）" || bad "词表缺失应 3"

echo "== test-g030-nav：PASS=$PASS FAIL=$FAIL =="
[ "$FAIL" -eq 0 ] || exit 1
exit 0
