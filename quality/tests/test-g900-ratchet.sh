#!/usr/bin/env bash
# test-g900-ratchet.sh —— g900 棘轮关卡自测（W5-C1 .github#224 / ADR-0070 决策 4；
# 卡 AC-3 负控制：指标变差必红、变好提示 baseline 待更新、非 bot 写基线 exit 2、
# bot 放宽无 trailer exit 2、bootstrap 例外、坏 JSON infra）。
set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
QUALITY=$(cd "$HERE/.." && pwd)
# shellcheck source=../lib/contract.sh
source "$QUALITY/lib/contract.sh" || { echo "contract.sh 加载失败" >&2; exit 1; }
qc_detect_python || exit 1
GATE="$QUALITY/gates/g900-ratchet.sh"
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

mk_repo() { # 干净基底：入口 + 词表 + 零违规 depcruise 结果
  local d="$TMP/$1"
  mkdir -p "$d/src" "$d/quality"
  git -C "$d" init -q
  git -C "$d" config user.name fixture
  git -C "$d" config user.email fixture@test
  git -C "$d" config core.autocrlf false
  printf 'export function greet(name: string): string {\n  return "Hello, " + name;\n}\n' >"$d/src/index.ts"
  cp "$QUALITY/glossary.yaml" "$d/quality/glossary.yaml"
  printf '{"summary": {"error": 0}, "violations": []}\n' >"$TMP/cruise-$1.json"
  git -C "$d" add -A && git -C "$d" commit -qm base
  FX="$d"
}
write_baseline() { # write_baseline <指标 JSON 片段>
  printf '{"version": 1, "metrics": %s, "provenance": {"updatedBy": "fixture"}}\n' "$1" >"$FX/quality/baseline.json"
}
run_gate() { # <报告名> <env...>
  local rep; rep="$(w "$TMP/reports/$1")"; shift
  ( cd "$FX" && env GATE_REPORT_OUT="$rep" GATE_DEPCRUISE_JSON="$(w "$TMP/cruise-$CRUISE.json")" "$@" bash "$GATE" ) >/dev/null 2>&1
}
# 基线=当前干净指标（arch 全 0 + nav/cx 全 0）——干净 fixture 的真实形态
CLEAN_METRICS='{"arch.depcruiseErrors": 0, "arch.shallowModules": 0, "nav.dirOverloads": 0, "nav.repomapUncovered": 0, "nav.importCycles": 0, "nav.docDangling": 0, "nav.glossaryUndefined": 0, "cx.maxCognitive": 0, "cx.deadExports": 0, "cx.suppressionTotal": 0}'

echo "== 未 bootstrap：skip 放行 =="
mk_repo nobase
CRUISE=nobase
run_gate g900-nobase.json
[[ $? -eq 0 ]] && ok "无 baseline → exit 0（棘轮不适用）" || bad "无 baseline 应 0"

echo "== 持平/变差/变好 三态 =="
mk_repo parity
CRUISE=parity
write_baseline "$CLEAN_METRICS"
git -C "$FX" add -A && git -C "$FX" commit -qm "bot: baseline bootstrap" >/dev/null
run_gate g900-parity.json
[[ $? -eq 0 ]] && ok "指标持平 → exit 0" || bad "持平应 0"
printf 'export function bloat(a, b, c) {\n  let r = 0;\n  if (a) {\n    if (b) {\n      if (c) {\n        if (a && b) {\n          r += a + b + c;\n        }\n      }\n    }\n  }\n  return r;\n}\n' >"$FX/src/bloat.ts"
git -C "$FX" add -A && git -C "$FX" commit -qm "bloat: 复杂度上升"
run_gate g900-worse.json
[[ $? -eq 1 ]] && ok "指标变差（maxCognitive 上升）→ exit 1" || bad "回归应 1"
rp "$(w "$TMP/reports/g900-worse.json")" 'any(v["rule"]=="ratchet-regression" and "cx.maxCognitive" in v["message"] for v in r["violations"])' \
  && ok "回归指认具体指标键 cx.maxCognitive" || bad "回归消息缺指标键"
rp "$(w "$TMP/reports/g900-worse.json")" 'all("ruleId=" in v["fixHint"] for v in r["violations"])' \
  && ok "fixHint 含 ruleId" || bad "fixHint 缺 ruleId"
# 变好：删掉 bloat 恢复干净，基线维持旧值
git -C "$FX" rm -q src/bloat.ts && git -C "$FX" commit -qm "revert bloat"
write_baseline '{"arch.depcruiseErrors": 9, "cx.maxCognitive": 9, "cx.deadExports": 0}'
git -C "$FX" add -A && git -C "$FX" commit -qm "bot: 高基线（历史遗留）"
run_gate g900-better.json
[[ $? -eq 0 ]] && ok "指标变好 → exit 0" || bad "变好应 0"
rp "$(w "$TMP/reports/g900-better.json")" '"arch.depcruiseErrors" in r["ratchetKeys"] and "cx.maxCognitive" in r["ratchetKeys"]' \
  && ok "变好键进 ratchetKeys（baseline 待更新上报）" || bad "ratchetKeys 缺变好键"

echo "== 写入纪律（卡 AC-3：agent 无权修改 baseline）=="
BASE=$(git -C "$FX" rev-parse HEAD)
write_baseline '{"arch.depcruiseErrors": 9, "cx.maxCognitive": 9}'
git -C "$FX" add -A && git -C "$FX" -c user.name='stranger-agent' -c user.email='agent@x' commit -qm "agent 直改基线"
run_gate g900-agent.json GATE_BASE="$BASE"
[[ $? -eq 2 ]] && ok "非 bot 提交 baseline 变更 → exit 2（fail-escalate）" || bad "非 bot 写基线应 2"
rp "$(w "$TMP/reports/g900-agent.json")" 'any(v["rule"]=="ratchet-baseline-illegal-write" for v in r["violations"])' \
  && ok "违规规则=ratchet-baseline-illegal-write" || bad "写入纪律规则缺失"

mk_repo botwrite
CRUISE=botwrite
BASE=$(git -C "$FX" rev-parse HEAD)
write_baseline "$CLEAN_METRICS"
git -C "$FX" add -A && git -C "$FX" -c user.name='cloudbird-bot[bot]' -c user.email='bot@x' commit -qm "bot: 同值刷新"
run_gate g900-bot.json GATE_BASE="$BASE"
[[ $? -eq 0 ]] && ok "bot 同值刷新 baseline → exit 0" || bad "bot 同值刷新应 0"

echo "== 放宽管制：bot 改差基线须 Ratchet-Loosen trailer =="
BASE=$(git -C "$FX" rev-parse HEAD)
write_baseline '{"arch.depcruiseErrors": 5}'
git -C "$FX" add -A && git -C "$FX" -c user.name='cloudbird-bot[bot]' -c user.email='bot@x' commit -qm "bot: 放宽（无 trailer）"
run_gate g900-loosen-bad.json GATE_BASE="$BASE"
[[ $? -eq 2 ]] && ok "bot 放宽无 trailer → exit 2" || bad "无 trailer 放宽应 2"
git -C "$FX" reset -q --hard HEAD~1
BASE=$(git -C "$FX" rev-parse HEAD)
write_baseline '{"arch.depcruiseErrors": 5}'
git -C "$FX" add -A && git -C "$FX" -c user.name='cloudbird-bot[bot]' -c user.email='bot@x' commit -qm "bot: 放宽

Ratchet-Loosen: ADR-0099"
run_gate g900-loosen-ok.json GATE_BASE="$BASE"
[[ $? -eq 0 ]] && ok "bot 放宽带 trailer（ADR-0099）→ exit 0" || bad "带 trailer 放宽应 0"

echo "== infra：坏 JSON =="
mk_repo badjson
CRUISE=badjson
printf '{broken\n' >"$FX/quality/baseline.json"
git -C "$FX" add -A && git -C "$FX" commit -qm bad
run_gate g900-badjson.json
[[ $? -eq 3 ]] && ok "baseline 非法 JSON → exit 3（infra）" || bad "坏 JSON 应 3"

echo "== test-g900-ratchet：PASS=$PASS FAIL=$FAIL =="
[ "$FAIL" -eq 0 ] || exit 1
exit 0
