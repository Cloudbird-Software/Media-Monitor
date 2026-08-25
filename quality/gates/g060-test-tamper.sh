#!/usr/bin/env bash
# quality/gates/g060-test-tamper.sh —— 锁定集防篡改关卡（W2-C2 .github#215 / ADR-0061 决策 2/3/4）
# 职责：非 owner PR 改动锁定路径 → exit 2（fail-escalate，只人类可解）；
#       伪造 Spec-Change trailer（spec PR 不存在/未合并/specVersion 未递增）同样 exit 2；
#       合法 spec 变更（trailer + 已合并 spec PR + specVersion 递增）→ 放行并提示"锁定集待更新"；
#       manifest（baseline.json）变更只许 CI bot 提交（首次创建 bootstrap 除外）。
# 上下文注入（ADR-0060 统一 CLI 契约）：GATE_BASE/GATE_HEAD（diff 范围）、GATE_PR/GATE_PR_AUTHOR、
# GATE_GH（spec PR 回查命令，默认 gh api）、GATE_CHANGED_FILES/GATE_LOCK_FILE（测试注入缝）。
set -euo pipefail
GATE_ID=g060-test-tamper
# shellcheck source=../lib/lock/lib.sh
source "$(cd "$(dirname "$0")" && pwd)/../lib/lock/lib.sh"

LOCK=$(lock_file_path) || gate_finish 3 infra-error "contract.yaml 不可读（lock-file 键缺失）"
[[ -f "$LOCK" ]] || gate_finish 0 skip "锁定集 manifest 不存在（尚未 bootstrap，无锁定面）"
lock_json_valid "$LOCK" || gate_finish 3 infra-error "manifest 非法 JSON：$LOCK"

BOT_PATTERNS=$(contract_value g060-test-tamper bot-patterns) || gate_finish 3 infra-error "bot-patterns 键缺失"
OWNER_LOGINS=$(contract_value g060-test-tamper owner-logins) || gate_finish 3 infra-error "owner-logins 键缺失"
SPEC_REPO=$(contract_value g060-test-tamper spec-repo) || gate_finish 3 infra-error "spec-repo 键缺失"

# ---- 变更清单来源：注入缝 > git diff（PR/CI）> 本地模式 ----
CHANGED=""
if [[ -n "${GATE_CHANGED_FILES:-}" ]]; then
  CHANGED="$GATE_CHANGED_FILES"
elif [[ -n "${GATE_BASE:-}" ]]; then
  HEAD_REF="${GATE_HEAD:-HEAD}"
  CHANGED=$(git diff --name-only "${GATE_BASE}...${HEAD_REF}")
fi

if [[ -z "$CHANGED" ]]; then
  # 本地模式：只验锁定集完整性（PR 语境的 owner/trailer 裁决留给 CI 注入面）
  while IFS= read -r entry; do
    [[ -f "$entry" ]] || gate_finish 1 fail-fixable "锁定文件缺失：$entry" "git checkout -- $entry 恢复；确需变更走 Spec-Change 流程（ADR-0061 决策 3）"
    [[ $(lock_sha256 "$entry") == "$(lock_entry_sha "$LOCK" "$entry")" ]] \
      || gate_finish 1 fail-fixable "锁定文件哈希不符：$entry" "git checkout -- $entry 恢复；确需变更走 Spec-Change 流程（commit 带 Spec-Change: <spec PR#> trailer）"
  done < <(lock_entries "$LOCK")
  gate_finish 0 pass "锁定集完好（$(lock_entries "$LOCK" | wc -l) 条本地校验通过）"
fi

# ---- 决策 4：manifest 变更只许 bot（首次创建 bootstrap 除外）----
# git 操作与 diff 输出比对需要仓相对路径（GATE_LOCK_FILE 可能是测试注入的绝对路径）
LOCK_REL="$LOCK"
case "$LOCK" in "$PWD"/*) LOCK_REL="${LOCK#"$PWD"/}" ;; esac
if grep -qxF "$LOCK_REL" <<<"$CHANGED"; then
  if git cat-file -e "${GATE_BASE}:${LOCK_REL}" 2>/dev/null; then
    non_bot=1
    while IFS= read -r who; do
      grep -qE "$BOT_PATTERNS" <<<"$who" && non_bot=0
    done < <(git log --format='%an <%ae>' "${GATE_BASE}..${GATE_HEAD:-HEAD}" -- "$LOCK_REL")
    [[ $non_bot -eq 0 ]] || gate_finish 2 fail-escalate \
      "锁定集 manifest 被非 bot 提交修改（$LOCK）——manifest 更新只许 CI bot（ADR-0061 决策 4）" \
      "回退本 PR 对 manifest 的直接修改；合法路径=已合并 spec PR + commit 带 Spec-Change trailer 后由 bot 重算"
  else
    echo "[g060] notice: 锁定集 manifest 首次创建（bootstrap），放行"
  fi
fi

# ---- 决策 2/3：锁定路径改动裁决 ----
CUR_SPEC_VER=$(lock_spec_version "$LOCK")
while IFS= read -r entry; do
  grep -qxF "$entry" <<<"$CHANGED" || continue
  now_sha=$([[ -f "$entry" ]] && lock_sha256 "$entry" || echo "")
  [[ "$now_sha" == "$(lock_entry_sha "$LOCK" "$entry")" ]] && continue # 同 sha（如纯重命名提交）→ 无实质篡改

  if grep -qxE "${GATE_PR_AUTHOR:-}" <<<"$OWNER_LOGINS"; then
    echo "[g060] notice: owner PR 改动锁定路径 $entry——放行；锁定集待更新：bash quality/lib/lock/update.sh"
    continue
  fi

  # Spec-Change trailer 回查（决策 3）：commit message 带 Spec-Change: <PR#>
  [[ -n "${GATE_BASE:-}" ]] || gate_finish 2 fail-escalate \
    "非 owner 改动锁定路径：$entry（无 GATE_BASE——trailer 无法回查，fail-closed）" \
    "在 PR 语境（GATE_BASE 注入）下重跑；本地先恢复文件：git checkout -- $entry"
  trailers=$(git log --format=%B "${GATE_BASE}..${GATE_HEAD:-HEAD}" \
    | grep -E "^[[:space:]]*Spec-Change:[[:space:]]*[0-9]+" | grep -oE "[0-9]+" | sort -u || true)
  [[ -n "$trailers" ]] || gate_finish 2 fail-escalate \
    "非 owner PR 改动锁定路径：$entry（无 Spec-Change trailer）" \
    "验收测试是合同——改实现不改测试；确属 spec 变更：先在 spec 仓合 PR，再在本 PR commit message 加 trailer：Spec-Change: <spec PR#>"

  ok_trailer=""
  for n in $trailers; do
    err=$(mktemp); out=""
    if ! out=$(${GATE_GH:-gh api} "repos/$SPEC_REPO/pulls/$n" 2>"$err"); then
      if grep -q "HTTP 404" "$err"; then
        rm -f "$err"
        gate_finish 2 fail-escalate "伪造 Spec-Change trailer：$SPEC_REPO#$n 不存在" \
          "trailer 必须指向真实已合并的 spec PR（ADR-0061 决策 3）"
      fi
      rm -f "$err"
      gate_finish 3 infra-error "spec PR 状态回查失败（$SPEC_REPO#$n）——fail-closed，不当作通过" "重试；确认 gh 凭据/网络后复跑 g060"
    fi
    rm -f "$err"
    merged=$(printf '%s' "$out" | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>process.stdout.write(String(JSON.parse(s).merged===true)))')
    body=$(printf '%s' "$out" | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>process.stdout.write(JSON.parse(s).body||""))')
    [[ "$merged" == "true" ]] || gate_finish 2 fail-escalate \
      "Spec-Change: $n 对应 spec PR 未合并（state!=merged）——伪造通道拒绝" \
      "等 spec PR 合并后再推送本 commit，或移除 trailer 走 owner 评审"
    spec_ver=$(grep -oE "specVersion:[[:space:]]*[0-9]+" <<<"$body" | head -1 | grep -oE "[0-9]+" || true)
    [[ -n "$spec_ver" && "$spec_ver" -gt "$CUR_SPEC_VER" ]] || gate_finish 2 fail-escalate \
      "spec PR #$n 已合并但 specVersion 未递增（要求 > $CUR_SPEC_VER，实得 '${spec_ver:-缺失}'）" \
      "spec 变更须显式递增 specVersion 并写入 spec PR body（如 'specVersion: $((CUR_SPEC_VER+1))'）"
    ok_trailer="$n"
    break
  done
  echo "[g060] notice: 合法 spec 变更（trailer #$ok_trailer，specVersion $CUR_SPEC_VER→$spec_ver）——放行 $entry；锁定集待更新：bash quality/lib/lock/update.sh --spec-version $spec_ver --spec-pr $ok_trailer"
done < <(lock_entries "$LOCK")

gate_finish 0 pass "锁定集校验通过（无未授权篡改）"
