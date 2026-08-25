#!/usr/bin/env bash
# test-tidy-scan.sh —— W5-C1 共享扫描器自测（.github#224 / ADR-0070；
# 宪法 §4E：变更 lint/关卡逻辑必须带测试——扫描器是四组整洁关卡的地基）。
# 全部 fixture 在 mktemp 目录内构造；断言用 qc_py 内联脚本读真实返回值。
# 路径注意：Windows 商店/git-bash 的 mktemp 产出 MSYS 路径，Windows 原生 python
# 读不了——经 cygpath -m 转 C:/ 形式再进 python（CI linux 无 cygpath，原样透传）。
set -uo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
QUALITY=$(cd "$HERE/.." && pwd)
# shellcheck source=../lib/contract.sh
source "$QUALITY/lib/contract.sh" || { echo "contract.sh 加载失败" >&2; exit 1; }
qc_detect_python || exit 1
LIB="$QUALITY/lib/tidy_scan.py"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
PASS=0; FAIL=0
ok()  { echo "  ok   $1"; PASS=$((PASS + 1)); }
bad() { echo "  FAIL $1"; FAIL=$((FAIL + 1)); }
w() { cygpath -m "$1" 2>/dev/null || printf '%s' "$1"; }

# py_expr <fixture 根（bash 路径）> <py-expr（变量 root=Windows 形路径）>
py_expr() {
  T_LIB="$QUALITY/lib" T_ROOT="$(w "$1")" qc_py -c 'import os, sys
sys.path.insert(0, os.environ["T_LIB"])
import tidy_scan as t
root = os.environ["T_ROOT"]
'"$2"'' </dev/null 2>&1
}

# ---- fixture：含跨文件引用与相对导入的最小 TS 树 ----
FX="$TMP/fx"
mkdir -p "$FX/src/mod" "$FX/src/flat" "$FX/skipme"
printf 'export function alpha() {}\nexport const beta = 1;\nexport { gamma } from "./mod/gamma";\n' >"$FX/src/index.ts"
printf 'import { helper } from "./helper";\nexport function gamma(n) { return helper(n); }\n' >"$FX/src/mod/gamma.ts"
printf 'export function helper(x) { return x + 1; }\n' >"$FX/src/mod/helper.ts"
printf 'def top_fn():\n    pass\n' >"$FX/src/thing.py"
for i in 1 2 3; do printf 'export const f%s = %s;\n' "$i" "$i" >"$FX/src/flat/f$i.ts"; done
printf 'should be ignored\n' >"$FX/skipme/junk.ts"

R=$(py_expr "$FX" 'print(sorted(t.list_files(root, "src", "src/flat")))')
[[ "$R" == *"src/index.ts"* && "$R" == *"src/mod/gamma.ts"* && "$R" != *"src/flat"* ]] \
  && ok "list_files：前缀含/忽略前缀排除" || bad "list_files 输出异常：$R"

R=$(py_expr "$FX" 'sym=t.make_symbols(root, t.list_files(root, "src")); print(sorted(sym))')
for want in alpha beta gamma helper top_fn; do
  [[ "$R" == *"'$want'"* ]] && ok "符号宇宙含 $want" || bad "符号宇宙缺 $want：$R"
done

R=$(py_expr "$FX" 'files=t.list_files(root,"src"); sym=t.make_symbols(root,files); refs,occ=t.ref_counts(root,files,sym); print(refs["helper"], occ["helper"])')
[[ "$R" == *1* ]] && ok "helper 被 gamma.ts 跨文件引用（文件数/出现数均=1）" || bad "ref_counts(helper)=$R"

R=$(py_expr "$FX" 'g=t.import_graph(root, t.list_files(root,"src")); print(sorted(g["src/index.ts"]), sorted(g["src/mod/gamma.ts"]))')
[[ "$R" == *"src/mod/gamma.ts"* && "$R" == *"src/mod/helper.ts"* ]] \
  && ok "导入图解析相对导入（index→gamma、gamma→helper）" || bad "导入图异常：$R"

R=$(py_expr "$FX" 'print(t.code_lines(open(root + "/src/mod/helper.ts").read()))')
[[ "$R" == *1* ]] && ok "code_lines 单行实现=1" || bad "code_lines=$R"

# ---- GATE_TH_ 覆盖：连字符段名归一（gate_common._env_name 拼出的是带连字符的
# env 名，bash 无法注入——tidy_scan.load_section 统一把段/键连字符归一为下划线）----
R=$(T_LIB="$QUALITY/lib" GATE_TH_G050_FAIL_BEFORE_TEST_PATHS="zzz/" qc_py -c '
import os, sys
sys.path.insert(0, os.environ["T_LIB"])
import tidy_scan as t
print(t.load_section("g050-fail-before")["test-paths"])' </dev/null 2>&1)
[[ "$R" == "zzz/" ]] && ok "load_section 的 GATE_TH_ 覆盖通道生效（连字符→下划线）" || bad "覆盖通道失效：$R"

R=$(py_expr "$FX" 'print(t.load_section("g050-fail-before")["test-paths"])')
[[ "$R" == "tests/" ]] && ok "load_section 读到 contract 原值" || bad "load_section 异常：$R"

echo "== test-tidy-scan：PASS=$PASS FAIL=$FAIL =="
[ "$FAIL" -eq 0 ] || exit 1
exit 0
