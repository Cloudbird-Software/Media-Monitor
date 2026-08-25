#!/usr/bin/env bash
# quality/run-gates.sh —— 关卡统一编排器入口（W2-C2 .github#215 / ADR-0061 决策 5）
# Makefile 三命令（card-test / gates-fast / gates-pr）与 CI（ci.yml quality-gates job）
# 都调本脚本：同一脚本、同一 GATE_* env 注入协议 → 本地与 CI 结果一致（AC-4）。
# 用法：run-gates.sh {fast|pr|card <卡号>}
#   fast —— 本地快跑：quality 脚本语法（bash -n）+ contract.yaml 解析冒烟 + quality 自测
#   pr   —— 本地复现 CI 关卡等价物：fast + g050/g060 关卡 + node 检查面（= make check 同命令组）
#   card —— 卡级测试编排：选中 tests/card-<卡号>-*.test.ts（g160 之前的临时绑定），无则全量
set -euo pipefail

usage() { echo "用法：quality/run-gates.sh {fast|pr|card <卡号>}" >&2; exit 2; }
(($# >= 1)) || usage
MODE="$1"

ROOT=$(git rev-parse --show-toplevel 2>/dev/null) || { echo "不在 git 仓内" >&2; exit 3; }
cd "$ROOT"
[[ -d quality ]] || { echo "quality/ 不存在——非 quality 化仓" >&2; exit 3; }

WORST=0
remember() { # 记录最差退出码（escalate(2) 优先于 infra(3) 优先于 fixable(1)——最严重者上浮）
  local rc=$1
  if [[ $rc -eq 2 ]]; then WORST=2
  elif [[ $WORST -ne 2 && $rc -eq 3 ]]; then WORST=3
  elif [[ $WORST -eq 0 && $rc -eq 1 ]]; then WORST=1
  fi
}
run_step() { echo "== $1 =="; shift; rc=0; "$@" || rc=$?; remember "$rc"; echo "-- 退出码 $rc"; }

run_fast() {
  local -a scripts=()
  mapfile -t scripts < <(find quality -name '*.sh' -not -path '*/reports/*')
  ((${#scripts[@]})) || { echo "quality 下无脚本" >&2; exit 3; }
  for s in "${scripts[@]}"; do bash -n "$s" && echo "OK   bash -n $s" || { echo "FAIL bash -n $s" >&2; remember 1; }; done

  echo "== contract.yaml 解析冒烟（fail-closed：缺键即红）=="
  # shellcheck source=quality/lib/lock/lib.sh
  source "$ROOT/quality/lib/lock/lib.sh"
  local keys=(
    g050-fail-before.test-paths g050-fail-before.runner-cmd
    g050-fail-before.assert-signatures g050-fail-before.collection-signatures
    g060-test-tamper.lock-file g060-test-tamper.owner-logins
    g060-test-tamper.bot-patterns g060-test-tamper.spec-repo
    g020-arch.depcruise-cmd g020-arch.max-depcruise-errors g020-arch.exports-max
    g900-ratchet.baseline-file g900-ratchet.bot-patterns g900-ratchet.loosen-trailer
  )
  for k in "${keys[@]}"; do
    v=$(contract_value "${k%.*}" "${k#*.}") || { remember 3; continue; }
    echo "OK   $k = $(head -1 <<<"$v")"
  done

  local t
  # 自测密闭性：清掉外层 GATE_* 上下文——自测自带 fixture（自己的 GATE_BASE/替身 gh/
  # 测试注入缝），CI/PR 语境泄漏进来会把断言语境搅乱（W2-C1 run-all 的 GATE_CARD
  # 冲突检测、W2-C2 本地模式判定都会被外层值劫持）
  local -a SANITIZE=(env -u GATE_BASE -u GATE_HEAD -u GATE_CARD -u GATE_PR -u GATE_PR_AUTHOR
    -u GATE_CHANGED_FILES -u GATE_TEST_RUNNER -u GATE_LOCK_FILE -u GATE_REPORT_OUT
    -u GATE_REPORT_DIR -u GATE_GH)
  # run-all.sh（W2-C1 自测）与 test-*.sh（W2-C2 拓扑自测）一并跑——gates-fast/check 的自测面
  for t in quality/tests/run-all.sh quality/tests/test-*.sh; do
    [[ -e "$t" ]] || continue
    run_step "quality 自测 $t" "${SANITIZE[@]}" bash "$t"
  done
}

run_node_face() { # 与 Makefile check 同命令组（本地 gates-pr 与 CI 关卡等价物的命令面）
  [[ -d node_modules ]] || { echo "node_modules 缺失——先 make setup（infra，exit 3）" >&2; exit 3; }
  run_step "prettier --check ." npx prettier --check .
  run_step "eslint ." npx eslint .
  run_step "tsc --noEmit" npx tsc --noEmit
  run_step "depcruise src" npx depcruise src
  run_step "vitest run --coverage" npx vitest run --coverage
}

case "$MODE" in
  fast)
    run_fast
    ;;
  pr)
    # 关卡（g050 红复现）与 node 检查面都依赖仓内 node_modules——先挡一道，避免 npx 联网拉包
    [[ -d node_modules ]] || { echo "node_modules 缺失——先 make setup（infra，exit 3）" >&2; exit 3; }
    run_fast
    # W5-C1 起关卡号段扩到 g9xx（g900-ratchet）——glob 覆盖全部 gNNN 关卡入口
    for g in quality/gates/g*.sh; do
      [[ -e "$g" ]] || continue
      run_step "关卡 $g" bash "$g"
    done
    run_node_face
    ;;
  card)
    (($# == 2)) || usage
    export GATE_CARD="$2"
    mapfile -t cardtests < <(git ls-files "tests/card-$2-*.test.ts")
    if ((${#cardtests[@]})); then
      run_step "卡 #$2 测试集（${cardtests[*]}）" npx vitest run --coverage "${cardtests[@]}"
    else
      run_step "卡 #$2 无专属测试——跑全量 npm test" npm test
    fi
    ;;
  *) usage ;;
esac

echo "== run-gates($MODE) 汇总：最差退出码 $WORST（0=通过 1=fail-fixable 2=fail-escalate 3=infra）=="
exit "$WORST"
