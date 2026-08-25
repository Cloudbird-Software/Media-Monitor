#!/usr/bin/env bash
# run-all.sh —— quality 关卡自测（W2-C1 .github#214；宪法 §4E「验证器之验证」：
# 变更 lint/关卡逻辑必须带测试；负控制=坏样例必须红、好样例必须绿）。
# 硬约束：零网络、零 LLM、零新依赖——只用 bash + python 标准库。
# 退出码：0=全绿；1=有断言红（本脚本自身就是关卡，红=lint 逻辑被改坏）。
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=../lib/contract.sh
source "$ROOT/quality/lib/contract.sh" || { echo "run-all: contract.sh 加载失败" >&2; exit 1; }
qc_detect_python || exit 1
cd "$ROOT"
FIX=quality/fixtures
OUT=quality/reports
mkdir -p "$OUT"
TMPIDX="$OUT/tmp-index-$$.txt"; trap 'rm -f "$TMPIDX" "$OUT"/tmp-report-*.json' EXIT

PASS=0
FAIL=0
GATE=quality/gates/g010-spec-schema.sh
IDX="$FIX/tests-index.txt"
IR="$FIX/ir-index.txt"

ck() { # ck <名称> <实际> <期望> —— 计数并打印一行
  if [ "$2" = "$3" ]; then PASS=$((PASS + 1)); echo "  ok   $1"; else FAIL=$((FAIL + 1)); echo "  FAIL $1（实际=$2 期望=$3）"; fi
}

rp() { # rp <report.json> <py-expr（变量 r）> —— 断言表达式为真
  QC_REPORT="$1" qc_py -c 'import json, os, sys
r = json.load(open(os.environ["QC_REPORT"], encoding="utf-8"))
sys.exit(0 if '"$2"' else 1)' </dev/null
}

run_gate() { # run_gate <报告名> <spec...> —— 跑 g010（吃当前环境变量），回显退出码
  local rep="$OUT/$1"
  shift
  GATE_REPORT_OUT="$rep" bash "$GATE" "$@" >/dev/null 2>&1
}

echo "== AC-1 阈值唯一来源（contract.yaml + GATE_TH_* 注入通道）=="
ck "qc_get 读到 min_ac_clauses=1（阈值在 contract 不在脚本）" "$(qc_get thresholds.g010.spec.min_ac_clauses)" "1"
ck "GATE_TH_ 覆盖通道生效（99 注入）" "$(GATE_TH_G010_SPEC_MIN_AC_CLAUSES=99 qc_get thresholds.g010.spec.min_ac_clauses)" "99"
GATE_TESTS_INDEX="$IDX" GATE_IR_INDEX="$IR" GATE_TH_G010_SPEC_MIN_AC_CLAUSES=99 run_gate tmp-report-inject.json "$FIX/spec-ok.md"
ck "阈值经 env 注入后 ok 样例变红（证明读的是 contract 不是硬编码）" "$?" "1"
# 硬编码阈值 grep：阈值形命名（min/max/limit/threshold/timeout/sla）不得带数字赋值。
# GATE_TH_*= 注入通道本身豁免（那是唯一允许的运行期覆盖）；exit 0/1/2/3 是
# IFACE-04 协议语义不是阈值，不在本 grep 范围。
BAD=$(grep -rnE '(MIN|MAX|LIMIT|THRESHOLD|TIMEOUT|SLA)[A-Za-z_]*=[^$]*[0-9]|_(min|max)_[a-z]+=[0-9]' \
  quality/gates quality/lib quality/tests Makefile 2>/dev/null | grep -v '\.py:' | grep -v 'GATE_TH_' || true)
ck "quality/**(.sh)+Makefile 无硬编码数字阈值" "$(printf '%s' "$BAD" | grep -c . || true)" "0"

echo "== AC-2 合法/非法 spec 判定（EARS/禁词/fixHint 行号）=="
GATE_TESTS_INDEX="$IDX" GATE_IR_INDEX="$IR" run_gate tmp-report-ok.json "$FIX/spec-ok.md"
OK_RC=$?
ck "合法 spec 过（exit 0）" "$OK_RC" "0"
ck "合法 spec 报告 status=pass" "$(rp "$OUT/tmp-report-ok.json" 'r["status"]=="pass"' && echo y || echo n)" "y"
ck "BUDGET 绑定的禁词被豁免（exemptedVague=1）" "$(rp "$OUT/tmp-report-ok.json" 'r["metrics"]["exemptedVague"]==1' && echo y || echo n)" "y"
GATE_TESTS_INDEX="$IDX" GATE_IR_INDEX="$IR" run_gate tmp-report-bad.json "$FIX/spec-bad.md"
BAD_RC=$?
ck "非法 spec 红（exit 1）" "$BAD_RC" "1"
for rule in ears-pattern vague-term ac-numbering trace-orphan-clause trace-gilding-scope; do
  ck "违规规则 $rule 被抓到" \
    "$(rp "$OUT/tmp-report-bad.json" 'any(v["rule"]=="'"$rule"'" for v in r["violations"])' && echo y || echo n)" "y"
done
ck "断链抓到 3 处（IR-9999/TEST-42/BUDGET-9）" \
  "$(rp "$OUT/tmp-report-bad.json" 'sum(1 for v in r["violations"] if v["rule"]=="trace-broken-link")==3' && echo y || echo n)" "y"
ck "fixHint 指向具体违规行号（第N行）" \
  "$(rp "$OUT/tmp-report-bad.json" 'any(v.get("fixHint","").startswith("第") and "行" in v.get("fixHint","") for v in r["violations"] if v["rule"] in ("ears-pattern","vague-term","trace-broken-link"))' && echo y || echo n)" "y"
ck "EARS 违规带正文行号（line>1）" \
  "$(rp "$OUT/tmp-report-bad.json" 'next(v["line"] for v in r["violations"] if v["rule"]=="ears-pattern")>1' && echo y || echo n)" "y"
ck "孤儿条款报出 UID AC-3" \
  "$(rp "$OUT/tmp-report-bad.json" '"AC-3" in next(v["message"] for v in r["violations"] if v["rule"]=="trace-orphan-clause")' && echo y || echo n)" "y"
ck "镀金报出测试 UID TEST-202" \
  "$(rp "$OUT/tmp-report-bad.json" '"TEST-202" in next(v["message"] for v in r["violations"] if v["rule"]=="trace-gilding-scope")' && echo y || echo n)" "y"

echo "== AC-4 exit 码四值语义（fail-fixable/fail-escalate/infra-error）=="
GATE_TESTS_INDEX="$IDX" GATE_IR_INDEX="$IR" run_gate tmp-report-esc.json "$FIX/spec-escalate.md"
ck "篡改类（AC 重复定义）exit 2" "$?" "2"
ck "escalate 报告 metrics.exitCode=2" "$(rp "$OUT/tmp-report-esc.json" 'r["metrics"]["exitCode"]==2' && echo y || echo n)" "y"
run_gate tmp-report-infra.json "$FIX/不存在.md"
ck "输入文件缺失 exit 3（infra，不计业务红）" "$?" "3"
printf 'TEST-x 我不是合法索引行\n' >"$TMPIDX"
GATE_TESTS_INDEX="$TMPIDX" GATE_IR_INDEX="$IR" run_gate tmp-report-idx.json "$FIX/spec-ok.md"
ck "索引行格式坏 exit 1 且规则=trace-index-malformed" "$?" "1"

echo "== 判定物有效性：schema 校验器负控制（红必须真红）=="
printf '{"gate":"t","status":"bogus","metrics":[1],"violations":[{}],"durationMs":true}' >"$OUT/tmp-report-schema-bad.json"
ck "坏 report 被 schema 校验器拒绝" \
  "$(qc_py quality/lib/gate_common.py validate-report "$OUT/tmp-report-schema-bad.json" >/dev/null 2>&1 && echo pass || echo reject)" "reject"
ck "g010 产物过 schema 校验（好样例报告）" \
  "$(qc_py quality/lib/gate_common.py validate-report "$OUT/tmp-report-ok.json" >/dev/null 2>&1 && echo pass || echo reject)" "pass"

echo "== 挂载点：Makefile gates-fast（CI 经 make check 跑到这里）=="
ck "Makefile 有 gates-fast 目标" "$(grep -c '^gates-fast:' Makefile)" "1"
ck "Makefile check 串了 gates-fast" "$(grep -cE '^check:.*gates-fast' Makefile)" "1"

echo "== run-all 结果：PASS=$PASS FAIL=$FAIL =="
[ "$FAIL" -eq 0 ] || exit 1
exit 0
