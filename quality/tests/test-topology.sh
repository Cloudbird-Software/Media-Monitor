#!/usr/bin/env bash
# quality/tests/test-topology.sh —— W2-C2 自测试（.github#215 / ADR-0061 决策 6：
# 关卡自带负控制——伪造 trailer 必须被拒、合法 spec 变更必须放行、退出码语义逐值断言）。
# 全部 fixture 在 mktemp 目录内运行时构造，不落仓库；gh/vitest 以测试替身注入
# （GATE_GH / GATE_TEST_RUNNER 注入缝，编排器与 CI 用真实现）。
set -euo pipefail

# 密闭性：自测自带全部 fixture 语境——外层（CI/PR/编排器）注入的 GATE_* 一律清空，
# 防止断言语境被劫持（编排器 run-gates.sh 也做同样隔离，这里兜底直调场景）
unset GATE_BASE GATE_HEAD GATE_CARD GATE_PR GATE_PR_AUTHOR GATE_CHANGED_FILES \
  GATE_TEST_RUNNER GATE_LOCK_FILE GATE_REPORT_OUT GATE_REPORT_DIR GATE_GH

HERE=$(cd "$(dirname "$0")" && pwd)
QUALITY=$(cd "$HERE/.." && pwd)
G050="$QUALITY/gates/g050-fail-before.sh"
G060="$QUALITY/gates/g060-test-tamper.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
PASS=0; FAIL=0

ok()  { echo "  ok   $1"; PASS=$((PASS + 1)); }
bad() { echo "  FAIL $1"; FAIL=$((FAIL + 1)); }

# expect_rc <want> <desc> cmd...（set -e 安全：失败折叠进 rc）
expect_rc() {
  local want=$1 desc=$2; shift 2
  local rc=0; "$@" >/dev/null 2>&1 || rc=$?
  [[ $rc -eq $want ]] && ok "$desc" || bad "$desc（want=$want got=$rc）"
}

# ---- 通用 fixture：临时 git 仓 + 真实 quality/ 树 + 一条锁定测试 ----
mk_fixture() {
  local d="$TMP/$1"
  mkdir -p "$d"
  git -C "$d" init -q
  git -C "$d" config user.name fixture; git -C "$d" config user.email fixture@test
  git -C "$d" config core.autocrlf false # Windows 全局 autocrlf 会改写换行引入噪音/哈希漂移
  cp -r "$QUALITY" "$d/quality"
  rm -rf "$d/quality/reports" "$d/quality/evidence" "$d/quality/tests"
  mkdir -p "$d/tests"
  printf 'import { it } from "vitest";\nit("locked", () => {});\n' >"$d/tests/locked.test.ts"
  git -C "$d" add -A
  git -C "$d" commit -qm "base: quality 骨架 + 待锁定测试"
  LOCK="$d/quality/lock/baseline.json"
  printf '{\n  "version": 1,\n  "specVersion": 1,\n  "entries": {\n    "tests/locked.test.ts": {\n      "sha256": "%s",\n      "card": "fixture"\n    }\n  }\n}\n' \
    "$(sha256sum "$d/tests/locked.test.ts" | cut -d' ' -f1)" >"$LOCK"
  git -C "$d" add -A && git -C "$d" -c user.name='spec-lock-bot[bot]' -c user.email='bot@x' commit -qm "lock: bootstrap 锁定集"
  FIXTURE="$d"
}
fx() { git -C "$FIXTURE" "$@"; }
# run_g060 <env 赋值串...> —— 在 fixture 仓语境跑 g060（LOCK 经 GATE_LOCK_FILE 指向 fixture manifest）
run_g060() {
  ( cd "$FIXTURE" && env GATE_LOCK_FILE="$LOCK" GATE_REPORT_DIR="$TMP/reports" "$@" bash "$G060" )
}

echo "== g060：篡改与例外通道（AC-2/AC-3）=="
mk_fixture tamper
expect_rc 0 "锁定集完好（本地模式）→ 0" run_g060 X=1
printf 'tampered\n' >>"$FIXTURE/tests/locked.test.ts"
expect_rc 1 "本地改动锁定文件（无 PR 语境）→ 1 fail-fixable" run_g060 X=1
fx add -A; fx -c user.name=agent -c user.email=agent@x commit -qm "tamper: 改锁定测试"
expect_rc 2 "非 owner PR 改锁定路径、无 trailer → 2 fail-escalate" \
  run_g060 GATE_BASE="HEAD~1" GATE_PR_AUTHOR=some-agent

echo "== g060：Spec-Change trailer 回查（替身 gh：888 合且 v2 / 777 未合 / 666 404）=="
cat >"$TMP/fake-gh.sh" <<'EOF'
#!/usr/bin/env bash
case "$1" in
  */pulls/888) printf '{"number":888,"state":"closed","merged":true,"body":"specVersion: 2\\n验收行为变更"}' ;;
  */pulls/777) printf '{"number":777,"state":"open","merged":false,"body":"specVersion: 2"}' ;;
  */pulls/666) echo "gh: Not Found (HTTP 404)" >&2; exit 1 ;;
  *) echo "gh: connection refused (HTTP 500)" >&2; exit 1 ;;
esac
EOF
mk_fixture trailer
printf 'spec-change\n' >>"$FIXTURE/tests/locked.test.ts"
fx add -A; fx -c user.name=agent -c user.email=agent@x commit -qm "spec: 动锁定测试

Spec-Change: 666"
expect_rc 2 "伪造 trailer（spec PR 不存在 404）→ 2" \
  run_g060 GATE_BASE="HEAD~1" GATE_PR_AUTHOR=agent GATE_GH="bash $TMP/fake-gh.sh"
fx reset -q --hard HEAD~1; printf 'spec-change\n' >>"$FIXTURE/tests/locked.test.ts"
fx add -A; fx -c user.name=agent -c user.email=agent@x commit -qm "spec: 动锁定测试

Spec-Change: 777"
expect_rc 2 "trailer 指向未合并 spec PR → 2" \
  run_g060 GATE_BASE="HEAD~1" GATE_PR_AUTHOR=agent GATE_GH="bash $TMP/fake-gh.sh"
fx reset -q --hard HEAD~1; printf 'spec-change\n' >>"$FIXTURE/tests/locked.test.ts"
fx add -A; fx -c user.name=agent -c user.email=agent@x commit -qm "spec: 动锁定测试

Spec-Change: 888"
out=$(run_g060 GATE_BASE="HEAD~1" GATE_PR_AUTHOR=agent GATE_GH="bash $TMP/fake-gh.sh" 2>&1) \
  && ok "合法 trailer（已合并 + specVersion 1→2）→ 0" || bad "合法 trailer 应放行（实得 $out）"
grep -q "锁定集待更新" <<<"$out" && ok "放行时输出'锁定集待更新'指令" || bad "缺少'锁定集待更新'指令提示"
# specVersion 未递增（锁定集已 bot 推到 v2，spec PR 仍只声明 v2）→ 拒；
# 范围含 tamper commit + bot lock commit 两步
node -e 'const f=process.argv[1],j=JSON.parse(require("fs").readFileSync(f,"utf8"));j.specVersion=2;require("fs").writeFileSync(f,JSON.stringify(j,null,2)+"\n")' "$LOCK"
fx add -A; fx -c user.name='spec-lock-bot[bot]' -c user.email=bot@x commit -qm "lock: v2"
expect_rc 2 "specVersion 未递增（==锁定集版本）→ 2" \
  run_g060 GATE_BASE="HEAD~2" GATE_PR_AUTHOR=agent GATE_GH="bash $TMP/fake-gh.sh"

echo "== g060：manifest 只许 bot 提交（ADR-0061 决策 4）=="
mk_fixture manifest
node -e 'const f=process.argv[1],j=JSON.parse(require("fs").readFileSync(f,"utf8"));j.entries["tests/evil.test.ts"]={sha256:"0".repeat(64),card:null};require("fs").writeFileSync(f,JSON.stringify(j,null,2)+"\n")' "$LOCK"
fx add -A; fx -c user.name=human -c user.email=human@x commit -qm "lock: 人类直改 manifest"
expect_rc 2 "人类直改 manifest（非 bot 提交）→ 2" \
  run_g060 GATE_BASE="HEAD~1" GATE_PR_AUTHOR=agent
# infra 语义：回查通道故障（HTTP 500）不得冒充通过——fail-closed 判 exit 3
fx reset -q --hard HEAD~1
printf 'spec-change\n' >>"$FIXTURE/tests/locked.test.ts"; fx add -A
fx -c user.name=agent -c user.email=agent@x commit -qm "spec: 动锁定测试

Spec-Change: 500"
expect_rc 3 "spec PR 回查 infra 故障（HTTP 500）→ 3 fail-closed" \
  run_g060 GATE_BASE="HEAD~1" GATE_PR_AUTHOR=agent GATE_GH="bash $TMP/fake-gh.sh"

echo "== g060：owner 放行 + update.sh 重算（AC-3 执行面）=="
mk_fixture owner
printf 'owner-edit\n' >>"$FIXTURE/tests/locked.test.ts"
fx add -A; fx -c user.name=randypanding -c user.email=owner@x commit -qm "owner: 动锁定测试"
out=$(run_g060 GATE_BASE="HEAD~1" GATE_PR_AUTHOR=randypanding 2>&1) \
  && ok "owner PR 改锁定路径 → 0" || bad "owner 应放行（实得 $out）"
( cd "$FIXTURE" && GATE_LOCK_FILE="$LOCK" bash "$QUALITY/lib/lock/update.sh" --spec-version 3 --spec-pr 888 >/dev/null )
run_g060 X=1 && ok "update.sh 重算后本地校验 → 0" || bad "update.sh 重算后应通过"
grep -q '"specVersion": 3' "$LOCK" && ok "update.sh 递增 specVersion 并记 provenance" || bad "specVersion 未递增"

echo "== g050：fail-before 拓扑与红的分类（AC-1，替身 runner）=="
cat >"$TMP/fake-runner.sh" <<'EOF'
#!/usr/bin/env bash
case "${FAKE_MODE:-}" in
  assert)  echo "FAIL  tests/new.test.ts"; echo "AssertionError: expected 127 to be 2"; exit 1 ;;
  collect) echo "Error: Failed to resolve import '../src/index.js'"; exit 1 ;;
  green)   echo "Test Files  1 passed (1)"; exit 0 ;;
esac
exit 1
EOF
mk_g050() { # 顺序：base → [test-commit → impl-commit] 或 [impl → test]（违约序）
  local d="$TMP/$1"; mkdir -p "$d/tests"
  git -C "$d" init -q; git -C "$d" config user.name f; git -C "$d" config user.email f@t
  git -C "$d" config core.autocrlf false
  cp -r "$QUALITY" "$d/quality"; rm -rf "$d/quality/reports" "$d/quality/evidence" "$d/quality/tests"
  printf 'x' >"$d/impl.txt"; git -C "$d" add -A; git -C "$d" commit -qm base
  FIXTURE="$d"; LOCK="$d/quality/lock/baseline.json"
}
run_g050() { ( cd "$FIXTURE" && env GATE_TEST_RUNNER="env FAKE_MODE=${FAKE_MODE:-assert} bash $TMP/fake-runner.sh" GATE_REPORT_DIR="$TMP/reports" "$@" bash "$G050" ); }

mk_g050 ok
printf 'it("new",()=>{})' >"$FIXTURE/tests/new.test.ts"; fx add -A; fx commit -qm "test: 先行红测试"
printf 'impl' >"$FIXTURE/src.txt"; fx add -A; fx commit -qm "feat: 实现"
FAKE_MODE=assert expect_rc 0 "测试先行 + 断言失败红 → 0" \
  run_g050 GATE_BASE="HEAD~2" GATE_CARD=215
FAKE_MODE=collect expect_rc 3 "红因=收集/导入错 → 3 无效红" \
  run_g050 GATE_BASE="HEAD~2" GATE_CARD=215
FAKE_MODE=green expect_rc 1 "实现前测试已是绿 → 1" \
  run_g050 GATE_BASE="HEAD~2" GATE_CARD=215
expect_rc 0 "红阶段（仅测试 commit，无实现）→ 0 放行待复评" bash -c "
  cd '$FIXTURE' && git reset -q --hard HEAD~1 && \
  env GATE_TEST_RUNNER='env FAKE_MODE=assert bash $TMP/fake-runner.sh' GATE_REPORT_DIR='$TMP/reports' \
  GATE_BASE='HEAD~1' GATE_CARD=215 bash '$G050'"
mk_g050 wrong
printf 'impl' >"$FIXTURE/src.txt"; fx add -A; fx commit -qm "feat: 实现先行"
printf 'it()' >"$FIXTURE/tests/new.test.ts"; fx add -A; fx commit -qm "test: 后补测试"
FAKE_MODE=assert expect_rc 1 "实现 commit 早于测试 commit → 1 拓扑违约" \
  run_g050 GATE_BASE="HEAD~2" GATE_CARD=215
expect_rc 0 "无 GATE_CARD/GATE_PR（非卡面）→ 0 范围外" run_g050 GATE_BASE="HEAD~2"
expect_rc 0 "无新增验收测试 → 0 不适用" \
  run_g050 GATE_BASE="HEAD~1" GATE_CARD=215

echo "== 编排器契约（AC-4 入口同一性）=="
bash -n "$QUALITY/run-gates.sh" && ok "run-gates.sh bash -n" || bad "run-gates.sh 语法"
expect_rc 2 "run-gates.sh 无参数 → 2 用法错误" bash "$QUALITY/run-gates.sh"

echo "== 结果：$PASS 通过 / $FAIL 失败 =="
[[ $FAIL -eq 0 ]] || exit 1
