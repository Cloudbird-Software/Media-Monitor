# -*- coding: utf-8 -*-
"""ratchet_lib.py —— g900-ratchet 判定核心（W5-C1 .github#224 / ADR-0070 决策 4）。

棘轮：全仓指标只许变好。quality/baseline.json 是基线（AC-3），三道执法：
  1. 指标回归：当前指标 > 基线 → fail（ruleId=ratchet-regression）；
     指标变好 → pass 且提示"baseline 待更新"（ratchetKeys 上报，ratchet-update.sh 落盘）；
  2. 写入纪律：baseline.json 变更只许 CI bot 提交（提交者身份校验——
     人类/agent 直接提交 → exit 2 fail-escalate；首次 bootstrap 创建除外）；
  3. 放宽管制：bot 提交也不得静默放宽——基线值变差须 commit 带
     "Ratchet-Loosen: <ADR-NNNN|#NN>" trailer（人类批准的可追溯形态），否则 exit 2。
口径声明：棘轮只棘轮"坏量计数"（违规计数/最大复杂度/抑制总量等，天然不增），
结构性规模指标（符号总数/平均跳数等随生长合理波动）由各关卡阈值执法、不入基线。
退出码：0=过（或变好提示更新）1=回归 2=写入纪律违规 3=infra。
"""
import json
import os
import re
import subprocess
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import gate_common as gc
import tidy_scan as ts
import arch_lint
import nav_lint
import complexity_lint

GATE = 'g900-ratchet'
RATCHET_KEYS = [
    'arch.depcruiseErrors', 'arch.shallowModules',
    'nav.dirOverloads', 'nav.repomapUncovered', 'nav.importCycles',
    'nav.docDangling', 'nav.glossaryUndefined', 'nav.glossaryDeprecated',
    'cx.maxCognitive', 'cx.deadExports', 'cx.prematureAbstractions',
    'cx.logiclessWrappers', 'cx.suppressionTotal', 'cx.exemptionCount',
]


class Escalate(Exception):
    """写入纪律违规（只人类可解，exit 2）。"""


def collect(root):
    """跑三组关卡扫描，合并指标（与各关卡同一判定面——棘轮不是第二套度量）。"""
    m_arch, _ = arch_lint.scan(root, ts.load_section('g020-arch'))
    m_nav, _ = nav_lint.scan(root, ts.load_section('g030-nav'))
    m_cx, _ = complexity_lint.scan(root, ts.load_section('g040-complexity'))
    out = {}
    for part in (m_arch, m_nav, m_cx):
        out.update(part)
    return out


def author_guard(root, th, path):
    """baseline 写入纪律（g060 决策 4 同构：manifest 只许 bot）。"""
    base = os.environ.get('GATE_BASE', '')
    if not base:
        return  # 本地模式：写入裁决留给 PR 语境（CI 注入 GATE_BASE）
    head = os.environ.get('GATE_HEAD', 'HEAD')
    rel = th['baseline-file']
    if rel not in ts.diff_files(root, base, head):
        return
    # bootstrap 判定：base 侧不存在 → 首次创建放行
    p = subprocess.run(['git', '-C', root, 'cat-file', '-e', '%s:%s' % (base, rel)],
                       capture_output=True)
    if p.returncode != 0:
        print('g900 notice: baseline 首次创建（bootstrap），写入纪律暂不适用')
        return
    authors = ts.git(root, 'log', '--format=%an <%ae>', '%s..%s' % (base, head), '--', rel).splitlines()
    if not any(re.search(th['bot-patterns'], a) for a in authors):
        raise Escalate('baseline.json 被非 bot 提交修改（%s）——基线更新只许 CI bot'
                       % '; '.join(a.strip() for a in authors[:3]))
    # 放宽管制：新基线 vs base 侧基线，变差的键须带 Ratchet-Loosen trailer
    old = json.loads(ts.git(root, 'show', '%s:%s' % (base, rel)))
    with open(path, encoding='utf-8') as f:
        new = json.load(f)
    worse = [k for k in RATCHET_KEYS
             if k in old.get('metrics', {}) and k in new.get('metrics', {})
             and new['metrics'][k] > old['metrics'][k]]
    if worse:
        msg = ts.git(root, 'log', '--format=%B', '%s..%s' % (base, head))
        if not re.search(r'^%s\s*:\s*(ADR-[0-9]+|\S*#[0-9]+)' % th['loosen-trailer'], msg, re.M):
            raise Escalate('bot 提交放宽了基线（%s）但无 %s trailer——放宽须人类批准的可追溯形态'
                           % (', '.join(worse[:5]), th['loosen-trailer']))


def load_baseline(path):
    with open(path, encoding='utf-8') as f:
        b = json.load(f)
    if not isinstance(b.get('metrics'), dict):
        raise gc.ContractError('baseline.json 缺 metrics 映射（fail-closed）：%s' % path)
    return b


def compare(baseline, current):
    reg, imp = [], []
    for k in RATCHET_KEYS:
        if k not in baseline['metrics']:
            continue
        old = baseline['metrics'][k]
        cur = current.get(k, 0)
        if cur > old:
            reg.append((k, old, cur))
        elif cur < old:
            imp.append(k)
    return reg, imp


def build_report(status, code, findings, metrics, improved, summary, t0):
    return {'gate': GATE, 'severity': 'block', 'ownerRole': 'hardener',
            'status': status,
            'metrics': dict({k: metrics.get(k, 0) for k in RATCHET_KEYS},
                            exitCode=code, violationCount=len(findings)),
            'ratchetKeys': improved, 'violations': findings,
            'durationMs': int((time.time() - t0) * 1000),
            'summary': summary, 'fixHint': None}


def emit(report, code):
    out = os.environ.get('GATE_REPORT_OUT') or os.path.join(
        os.getcwd(), 'quality', 'reports', GATE + '.json')
    os.makedirs(os.path.dirname(out), exist_ok=True)
    with open(out, 'w', encoding='utf-8') as f:
        json.dump(report, f, ensure_ascii=False, indent=2)
        f.write('\n')
    errs = gc.validate_schema(report, json.load(open(gc.gate_report_schema_path(), encoding='utf-8')))
    if errs:
        print('infra: gate-report 不过 schema：%s' % '; '.join(errs), file=sys.stderr)
        return gc.EXIT_INFRA
    print('%s: %s（report=%s）' % (GATE, {0: 'pass', 1: 'fail-fixable', 2: 'fail-escalate'}.get(code, code), out))
    return code


def run(argv):
    t0 = time.time()
    root = os.getcwd()
    th = ts.load_section(GATE)
    path = os.path.normpath(os.path.join(root, th['baseline-file']))
    try:
        author_guard(root, th, path)
    except Escalate as e:
        f = [{'file': th['baseline-file'], 'line': 1, 'rule': 'ratchet-baseline-illegal-write',
              'message': str(e),
              'fixHint': '回退对 baseline.json 的直接修改；合法路径=CI bot 经 ratchet-update.sh 更新（放宽须 Ratchet-Loosen trailer）（ruleId=ratchet-baseline-illegal-write）'}]
        return emit(build_report('fail', gc.EXIT_FAIL_ESCALATE, f, {}, [], str(e), t0), gc.EXIT_FAIL_ESCALATE)
    if not os.path.isfile(path):
        return emit(build_report('skip', gc.EXIT_PASS, [], {}, [],
                                 'baseline 未 bootstrap——棘轮不适用（先 bash quality/lib/ratchet-update.sh）', t0),
                    gc.EXIT_PASS)
    try:
        baseline = load_baseline(path)
    except ValueError as e:
        print('infra: baseline.json 非法 JSON：%s' % e, file=sys.stderr)
        return gc.EXIT_INFRA
    current = collect(root)
    reg, imp = compare(baseline, current)
    if reg:
        findings = [{'file': 'quality/baseline.json', 'line': 1, 'rule': 'ratchet-regression',
                     'message': '指标回归：%s（基线→当前）'
                                % '; '.join('%s %s→%s' % (k, o, c) for k, o, c in reg[:8]),
                     'fixHint': '修复回归（各关卡 ruleId 见其报告）；确需放宽=bot 提交带 Ratchet-Loosen trailer（ruleId=ratchet-regression）'}]
        return emit(build_report('fail', gc.EXIT_FAIL_FIXABLE, findings, current, imp,
                                 '棘轮回归 %d 项' % len(reg), t0), gc.EXIT_FAIL_FIXABLE)
    summary = '棘轮持平（%d 项基线指标无回归）' % len(baseline['metrics'])
    if imp:
        summary = '棘轮变好 %d 项：%s——baseline 待更新（CI bot 专用）：bash quality/lib/ratchet-update.sh' % (
            len(imp), ', '.join(imp[:6]))
    return emit(build_report('pass', gc.EXIT_PASS, [], current, imp, summary, t0), gc.EXIT_PASS)


def update(argv):
    """bot 侧重算并写基线（ratchet-update.sh 委派）。"""
    root = os.getcwd()
    th = ts.load_section(GATE)
    path = os.path.normpath(os.path.join(root, th['baseline-file']))
    current = collect(root)
    metrics = {k: current.get(k, 0) for k in RATCHET_KEYS}
    old = load_baseline(path) if os.path.isfile(path) else {'metrics': {}}
    worse = [k for k in metrics if k in old['metrics'] and metrics[k] > old['metrics'][k]]
    doc = {'version': 1, 'metrics': metrics,
           'provenance': {'card': os.environ.get('GATE_CARD', ''), 'updatedBy': 'ratchet-update',
                          'at': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())}}
    os.makedirs(os.path.dirname(path), exist_ok=True)
    # newline='\n'：Windows 文本模式会把 \n 翻译成 CRLF，prettier --check 即红
    with open(path, 'w', encoding='utf-8', newline='\n') as f:
        json.dump(doc, f, ensure_ascii=False, indent=2)
        f.write('\n')
    print('baseline 已写入 %s（%d 项指标）' % (path, len(metrics)))
    if worse:
        print('警告：以下指标变差，bot 提交必须带 %s trailer（人类批准）：%s'
              % (th['loosen-trailer'], ', '.join(worse)))
    return 0


def main(argv):
    try:
        if argv[:1] == ['update']:
            return update(argv[1:])
        return run(argv)
    except Escalate as e:
        print('escalate: %s' % e, file=sys.stderr)
        return gc.EXIT_FAIL_ESCALATE
    except (gc.ContractError, ts.ScanError, OSError) as e:
        print('infra-error: %s' % e, file=sys.stderr)
        return gc.EXIT_INFRA


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
