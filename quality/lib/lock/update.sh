#!/usr/bin/env bash
# quality/lib/lock/update.sh —— 锁定集重算（AC-3 合法 spec 变更路径的执行面；ADR-0061 决策 4）
# 用法：
#   update.sh --init                                    # 首次 bootstrap（建 manifest 骨架并锁定 tests/ 下全部 *.test.ts）
#   update.sh [--add <repo相对路径>]... [--remove <路径>]...
#   update.sh --spec-version <N> --spec-pr <M>          # 合法 spec 变更后：重算哈希 + 递增 specVersion + 记 provenance
# 身份纪律：本脚本产物（manifest 变更）只许 CI bot 提交——g060 对非 bot 提交的 manifest 变更 exit 2。
set -euo pipefail
GATE_ID=g060-lock-update
# shellcheck source=lib.sh
source "$(cd "$(dirname "$0")" && pwd)/lib.sh"

LOCK=$(lock_file_path) || { echo "contract.yaml 不可读" >&2; exit 3; }
case "$LOCK" in /*) ;; *) LOCK="$PWD/$LOCK" ;; esac
cd "$(git rev-parse --show-toplevel 2>/dev/null || pwd)" # 条目路径为仓相对路径，统一在仓根解析
INIT=0; DECL_VER=""; DECL_PR=""
declare -a ADD=() REMOVE=()
while (($#)); do
  case "$1" in
    --init) INIT=1 ;;
    --add) ADD+=("$2"); shift ;;
    --remove) REMOVE+=("$2"); shift ;;
    --spec-version) DECL_VER="$2"; shift ;;
    --spec-pr) DECL_PR="$2"; shift ;;
    *) echo "未知参数：$1（用法见文件头）" >&2; exit 2 ;;
  esac
  shift
done

if [[ ! -f "$LOCK" ]]; then
  [[ $INIT -eq 1 ]] || { echo "manifest 不存在：$LOCK（首次请用 --init）" >&2; exit 2; }
  mkdir -p "$(dirname "$LOCK")"
  printf '{\n  "version": 1,\n  "specVersion": 1,\n  "entries": {}\n}\n' >"$LOCK"
fi
# 裸调用 = 重算既有条目哈希（锁定=重算，bot 例行刷新入口）

if [[ $INIT -eq 1 ]]; then
  while IFS= read -r f; do ADD+=("$f"); done < <(git ls-files 'tests/*.test.ts')
fi

node -e '
  const fs = require("fs"), crypto = require("crypto");
  const a = process.argv.slice(1); // node -e：用户参数从 argv[1] 起（无脚本占位）
  const i1 = a.indexOf("--"), i2 = a.indexOf("--", i1 + 1);
  const lock = a[0], add = a.slice(1, i1), remove = a.slice(i1 + 1, i2);
  const [declVer, declPr, card] = a.slice(i2 + 1);
  const j = JSON.parse(fs.readFileSync(lock, "utf8"));
  for (const p of remove) delete j.entries[p];
  for (const p of add) {
    if (!fs.existsSync(p)) { console.error(`--add 路径不存在：${p}`); process.exit(2); }
    j.entries[p] = { sha256: crypto.createHash("sha256").update(fs.readFileSync(p)).digest("hex"), card: card || j.entries[p]?.card || null };
  }
  for (const p of Object.keys(j.entries)) { // 锁定 = 重算：既有条目哈希一律按当前文件刷新
    if (!fs.existsSync(p)) { console.error(`锁定路径缺失（先 --remove 或恢复文件）：${p}`); process.exit(2); }
    j.entries[p].sha256 = crypto.createHash("sha256").update(fs.readFileSync(p)).digest("hex");
  }
  const names = Object.keys(j.entries).sort();
  j.entries = Object.fromEntries(names.map((n) => [n, j.entries[n]]));
  if (declVer) {
    if (!(Number(declVer) > j.specVersion)) { console.error(`specVersion 必须递增（当前 ${j.specVersion}）`); process.exit(2); }
    j.specVersion = Number(declVer);
    j.provenance = { specPR: declPr || null, updatedBy: process.env.GATE_PR_AUTHOR || "lock-update", at: new Date().toISOString() };
  }
  fs.writeFileSync(lock, JSON.stringify(j, null, 2) + "\n");
  console.log(`锁定集已更新：${names.length} 条，specVersion=${j.specVersion}`);
' "$LOCK" "${ADD[@]}" -- "${REMOVE[@]}" -- "$DECL_VER" "$DECL_PR" "${GATE_CARD:-}"

echo "下一步（bot 通道）：以 bot 身份提交 $LOCK——人类/agent 直接提交会被 g060 exit 2（ADR-0061 决策 4）"
