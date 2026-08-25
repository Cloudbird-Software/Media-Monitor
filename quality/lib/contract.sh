#!/usr/bin/env bash
# contract.sh —— quality 关卡的 bash 侧公共库（ADR-0060 决策 1）。
# 为什么存在：bash 关卡（g010 及后续 bash 实现的关卡）读阈值必须走同一入口，
# 不许各自 grep/sed contract.yaml（那会把"唯一来源"读出 N 个方言）。
# 用法：source 本文件后调用 qc_py / qc_get。
set -uo pipefail

# qc_root —— 仓根（lib/ 在 quality/lib/ 下，向上两级）
qc_root() { cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd; }

# qc_detect_python —— 选一个真 python（>=3.8）。
# 为什么逐个验证：Windows 商店的 python3 是 stub（退出码非 0 或无输出），
# command -v 存在不等于可用；CI 上 python3 是真的。验证不过就换下一个。
qc_detect_python() {
  local cand out
  for cand in python3 python py; do
    command -v "$cand" >/dev/null 2>&1 || continue
    # py 启动器要 -3；其余直接跑。验证真执行（打印 OK）而非只看存在性。
    if [ "$cand" = py ]; then
      out=$(py -3 -c 'import sys; print("OK" if sys.version_info >= (3, 8) else "")' 2>/dev/null)
    else
      out=$("$cand" -c 'import sys; print("OK" if sys.version_info >= (3, 8) else "")' 2>/dev/null)
    fi
    if [ "$out" = OK ]; then
      if [ "$cand" = py ]; then
        QC_PY=(py -3)
      else
        QC_PY=("$cand")
      fi
      return 0
    fi
  done
  echo "qc: 找不到可用的 python>=3.8（python3/python/py 均验证失败）——infra-error" >&2
  return 1
}

# qc_py —— 用探测到的解释器执行 python 代码（脚本/关卡统一入口）。
qc_py() {
  [ -n "${QC_PY:-}" ] || qc_detect_python || return 3
  PYTHONDONTWRITEBYTECODE=1 "${QC_PY[@]}" "$@"
}

# qc_get <dotted.key> —— 读 contract.yaml 阈值（唯一读取面）。
# 覆盖通道：GATE_TH_<KEY>（点变下划线、去 thresholds 前缀，如
# GATE_TH_G010_SPEC_MIN_AC_CLAUSES）——只给测试注入用，见 AC-1。
# 缺键/解析失败 → 退出 3（infra-error：contract 是基础设施，配错不是业务红）。
qc_get() {
  [ $# -eq 1 ] || { echo "qc_get: 用法 qc_get <dotted.key>" >&2; return 3; }
  local out rc
  out=$(qc_py "$(qc_root)/quality/lib/gate_common.py" get "$1" 2>&1) || {
    rc=$?
    echo "qc_get($1) 失败: $out" >&2
    return 3
  }
  printf '%s\n' "$out"
}
