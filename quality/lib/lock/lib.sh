#!/usr/bin/env bash
# quality/lib/lock/lib.sh —— W2-C2 lock-tests 共享库（.github#215 / ADR-0061）
# 职责：契约读取（复用 W2-C1 的 quality/lib/contract.sh qc_get——阈值唯一来源的
# 唯一读取面，不另造方言）+ 锁定集 manifest 读写（node 为 JSON 确定性工具，Node 仓
# 内 node 必在）+ gate-report 落盘（quality/schema/gate-report.schema.json 契约，
# 落盘后经 gate_common.py validate-report 自校验，不过 schema 即 exit 3）+ 四值退出码收尾。
# 退出码（ADR-0060 统一 CLI 契约）：0=通过 1=fail-fixable 2=fail-escalate 3=infra-error。
set -euo pipefail

LOCK_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
QUALITY_ROOT="$(cd "$LOCK_LIB_DIR/../.." && pwd)"

# 复用 W2-C1 契约读取面（qc_get/qc_py；缺键/解析失败其自身 exit 3 = infra-error）
# shellcheck source=../contract.sh
source "$QUALITY_ROOT/lib/contract.sh"

# contract_value <section> <key> —— W2-C2 关卡读契约的统一入口（值全为标量：
# 列表型值以 | 连接的单 ERE/单词串表示，见 contract.yaml W2-C2 段注释）
contract_value() { qc_get "$1.$2"; }

# 锁定集 manifest 路径（GATE_LOCK_FILE 注入优先——测试/编排注入缝）
lock_file_path() {
  if [[ -n "${GATE_LOCK_FILE:-}" ]]; then printf '%s\n' "$GATE_LOCK_FILE"
  else contract_value g060-test-tamper lock-file; fi
}

lock_sha256() { sha256sum "$1" | cut -d' ' -f1; }

lock_node() {
  local js="$1"; shift
  node -e "$js" "$@"
}

lock_entries() { # → 换行分隔的锁定路径列表
  lock_node 'const j=JSON.parse(require("fs").readFileSync(process.argv[1],"utf8"));console.log(Object.keys(j.entries||{}).join("\n"))' "$1"
}
lock_entry_sha() { lock_node 'const j=JSON.parse(require("fs").readFileSync(process.argv[1],"utf8"));const e=(j.entries||{})[process.argv[2]];process.stdout.write(e?e.sha256:"")' "$1" "$2"; }
lock_spec_version() { lock_node 'const j=JSON.parse(require("fs").readFileSync(process.argv[1],"utf8"));process.stdout.write(String(j.specVersion??""))' "$1"; }
lock_json_valid() { lock_node 'JSON.parse(require("fs").readFileSync(process.argv[1],"utf8"))' "$1" 2>/dev/null; }

# gate_report <gate> <status:pass|fail|error|skip> <exit> <summary> [fixHint] [violationFile]
# —— 按 W2-C1 的 gate-report schema 落盘（status/severity/ownerRole/metrics/violations/
# durationMs 必填；summary/context 为附加键），并经 gate_common.py validate-report
# 自校验（ADR-0060 决策 2：不过 schema 本身即 infra-error）。
# 路径：GATE_REPORT_OUT（W2-C1 约定）> GATE_REPORT_DIR/<gate>.json > quality/reports/<gate>.json
gate_report() {
  local gate="$1" status="$2" code="$3" summary="$4" hint="${5:-}" vfile="${6:-}"
  local out
  if [[ -n "${GATE_REPORT_OUT:-}" ]]; then out="$GATE_REPORT_OUT"
  else out="${GATE_REPORT_DIR:-$QUALITY_ROOT/reports}/$gate.json"; fi
  mkdir -p "$(dirname "$out")"
  node -e '
    const fs = require("fs");
    const [out, gate, status, code, summary, hint, vfile, ms] = process.argv.slice(1);
    const violations = vfile && fs.existsSync(vfile)
      ? fs.readFileSync(vfile, "utf8").split("\n").filter(Boolean).map((l) => {
          const [file, rule, message] = l.split("\t");
          return { file, rule, message };
        })
      : [];
    if (!violations.length && Number(code) !== 0) {
      violations.push({ file: "-", rule: "gate-verdict", message: summary, ...(hint ? { fixHint: hint } : {}) });
    }
    const rep = {
      gate, status,
      severity: Number(code) === 2 ? "block" : "warn",
      ownerRole: gate === "g050-fail-before" ? "test-author" : "refactorer",
      metrics: { exitCode: Number(code), violations: violations.length },
      violations,
      durationMs: Number(ms) || 0,
      summary,
      fixHint: hint || null,
      context: { pr: process.env.GATE_PR || null, card: process.env.GATE_CARD || null,
        author: process.env.GATE_PR_AUTHOR || null, base: process.env.GATE_BASE || null },
      ranAt: new Date().toISOString() };
    fs.writeFileSync(out, JSON.stringify(rep, null, 2) + "\n");
  ' "$out" "$gate" "$status" "$code" "$summary" "$hint" "$vfile" "$((SECONDS * 1000))" 2>/dev/null || true
  if ! qc_py "$QUALITY_ROOT/lib/gate_common.py" validate-report "$out" >/dev/null 2>&1; then
    echo "[gate-report] $out 未过 gate-report schema——infra-error（ADR-0060 决策 2）" >&2
    return 3
  fi
}

# gate_finish <exit> <verdict（忽略，status 由退出码推导）> <summary> [fixHint]
# —— 落报告（报告写不出/不过 schema → exit 3 infra）、向 stderr 摘要可见、以该退出码退出
gate_finish() {
  local code="$1" status
  case "$code" in
    0) status="pass" ;;
    3) status="error" ;;
    *) status="fail" ;;
  esac
  gate_report "$GATE_ID" "$status" "$code" "$3" "${4:-}" || exit 3
  echo "[gate:$GATE_ID] $status（exit $code）：$3" >&2
  [[ -z "${4:-}" ]] || echo "[gate:$GATE_ID] fixHint: $4" >&2
  exit "$code"
}
