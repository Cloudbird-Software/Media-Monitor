# -*- coding: utf-8 -*-
"""arch_lint.py —— g020-arch 判定核心（W5-C1 .github#224 / ADR-0070 决策 1）。

结构架构组三件事：
  1. 依赖架构：判定引擎=dependency-cruiser（宪法 §9#5 署名/§9 原则"判定轮子
     不自研"），规则维护在 .dependency-cruiser.cjs；本关卡只消费其 JSON 结果
     +contract 阈值（error 级违规数上限）。GATE_DEPCRUISE_JSON 可注入结果
     文件（测试/无 node 语境的注替缝）。
  2. 模块形状：exports 上限 / fanOut 上限 / depthRatio 下限——depthRatio 口径=
     入口文件代码行数/导出符号数（接口窄而实现深；纯转发/浅模块比率≈1），
     只对 ≥2 导出的入口执法（单导出=最小公共面，不受形状约束）。
  3. api-surface 变更声明：PR 改到 api-surface-paths 内的文件而 commit 不带
     Api-Surface: trailer → fail（公共 API/env 集合变更必须显式声明）。
退出码：0=过 1=违规 3=infra（contract/depcruise 基础设施故障）。
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

GATE = 'g020-arch'


def finding(file, line, rule, message, fix):
    return {'file': file, 'line': line, 'rule': rule, 'message': message,
            'fixHint': '%s（ruleId=%s）' % (fix, rule)}


def load_cruise(root, th):
    """depcruise 结果（dict）。注入缝 > 实跑 contract 命令。"""
    inj = os.environ.get('GATE_DEPCRUISE_JSON', '')
    if inj:
        with open(inj, encoding='utf-8') as f:
            return json.load(f)
    p = subprocess.run(th['depcruise-cmd'], shell=True, cwd=root,
                       capture_output=True, text=True)
    if p.returncode not in (0, 1):  # depcruise：1=发现违规（仍产 JSON），其它=配置/环境故障
        raise gc.ContractError('depcruise 退出码 %d（infra）：%s'
                               % (p.returncode, p.stderr.strip()[:200]))
    try:
        return json.loads(p.stdout)
    except ValueError as e:
        raise gc.ContractError('depcruise 输出非 JSON（infra）：%s' % e)


def scan(root, th):
    """→ (metrics, findings)。ratchet 复用（只取 metrics）。"""
    F = []
    cruise = load_cruise(root, th)
    viol = cruise.get('violations', [])
    errs = [v for v in viol if (v.get('rule') or {}).get('severity') == 'error']
    warns = [v for v in viol if (v.get('rule') or {}).get('severity') != 'error']
    if len(errs) > int(th['max-depcruise-errors']):
        for v in errs[:50]:
            F.append(finding(v.get('from', '?'), 1, 'arch-dependency',
                             '依赖架构违规：%s → %s 规则=%s'
                             % (v.get('from', '?'), v.get('to', '?'), (v.get('rule') or {}).get('name', '?')),
                             '按 .dependency-cruiser.cjs 规则拆依赖；确需豁免走 quality/exemptions.yaml（带 ADR/issue 引用）'))

    files = ts.list_files(root, th['src-paths'])
    entries = [f for f in files if f.endswith('index.ts')]
    max_exp, max_fan, shallow = 0, 0, 0
    ratios = []
    for rel in files:
        text = ts.read(root, rel)
        exps = ts.ts_exports(text)
        fan = len({s for s, _ in ts.ts_imports(text)})
        max_exp = max(max_exp, len(exps))
        max_fan = max(max_fan, fan)
        if rel in entries and len(exps) > int(th['exports-max']):
            F.append(finding(rel, exps[0][1] if exps else 1, 'arch-module-exports',
                             '模块入口导出 %d 个 > 上限 %d' % (len(exps), int(th['exports-max'])),
                             '拆分模块：入口只导出稳定公共面，内部实现移出 index'))
        if fan > int(th['fanout-max']):
            F.append(finding(rel, 1, 'arch-fanout',
                             '文件导入 %d 个不同模块 > 上限 %d' % (fan, int(th['fanout-max'])),
                             '收敛职责或拆文件：fanOut 过大=模块边界失焦'))
        if rel in entries and len(exps) >= 2:
            ratio = ts.code_lines(text) / max(len(exps), 1)
            ratios.append(ratio)
            if ratio < float(th['depthratio-min']):
                shallow += 1
                F.append(finding(rel, 1, 'arch-shallow-module',
                                 '浅模块：depthRatio=%.1f（代码行/导出数）< 下限 %s'
                                 % (ratio, th['depthratio-min']),
                                 '删除纯转发导出或下沉实现：入口应有真实逻辑深度（Ousterhout 深模块）'))

    # api-surface 变更声明（PR 语境；本地无 GATE_BASE 不适用）
    api_hits = []
    base = os.environ.get('GATE_BASE', '')
    if base:
        changed = ts.diff_files(root, base, os.environ.get('GATE_HEAD', 'HEAD'))
        api_hits = [c for c in changed
                    if any(c == p or c.startswith(p + '/') for p in ts.split_paths(th['api-surface-paths']))]
        if api_hits:
            trailers = ts.git(root, 'log', '--format=%B', '%s...%s' % (base, os.environ.get('GATE_HEAD', 'HEAD')))
            if not re.search(r'^%s\s*:' % th['api-surface-trailer'], trailers, re.M):
                F.append(finding(api_hits[0], 1, 'arch-api-surface-undeclared',
                                 'api-surface 变更未声明：%s' % ', '.join(api_hits[:5]),
                                 '公共 API/env 集合变更须在 commit message 加 trailer：%s: <变更摘要>' % th['api-surface-trailer']))
    m = {'arch.depcruiseErrors': len(errs), 'arch.depcruiseWarns': len(warns),
         'arch.modules': len(entries), 'arch.maxModuleExports': max_exp,
         'arch.maxFanOut': max_fan, 'arch.shallowModules': shallow,
         'arch.minDepthRatio': round(min(ratios), 2) if ratios else 0}
    return m, F


def run(argv):
    t0 = time.time()
    root = os.getcwd()
    th = ts.load_section(GATE)
    metrics, findings = scan(root, th)
    code = gc.EXIT_FAIL_FIXABLE if findings else gc.EXIT_PASS
    report = {'gate': GATE, 'severity': 'block', 'ownerRole': 'refactorer',
              'status': 'fail' if findings else 'pass',
              'metrics': dict(metrics, violationCount=len(findings), exitCode=code),
              'ratchetKeys': [], 'violations': findings,
              'durationMs': int((time.time() - t0) * 1000),
              'summary': '结构架构：depcruise error=%d 浅模块=%d api-surface 声明=%s'
                         % (metrics['arch.depcruiseErrors'], metrics['arch.shallowModules'],
                            'n/a（本地）' if not os.environ.get('GATE_BASE') else 'ok'),
              'fixHint': '见逐条 violations 的 fixHint' if findings else None}
    out = os.environ.get('GATE_REPORT_OUT') or os.path.join(
        root, 'quality', 'reports', GATE + '.json')
    os.makedirs(os.path.dirname(out), exist_ok=True)
    with open(out, 'w', encoding='utf-8') as f:
        json.dump(report, f, ensure_ascii=False, indent=2)
        f.write('\n')
    errs = gc.validate_schema(report, json.load(open(gc.gate_report_schema_path(), encoding='utf-8')))
    if errs:
        print('infra: gate-report 不过 schema：%s' % '; '.join(errs), file=sys.stderr)
        return gc.EXIT_INFRA
    print('%s: %s（%d violation，report=%s）' % (GATE, {0: 'pass', 1: 'fail-fixable'}.get(code, code), len(findings), out))
    for x in findings[:19]:
        print('  - %s:%s %s %s' % (x['file'], x['line'], x['rule'], x['message']))
    return code


if __name__ == '__main__':
    try:
        sys.exit(run(sys.argv[1:]))
    except (gc.ContractError, ts.ScanError, OSError) as e:
        print('infra-error: %s' % e, file=sys.stderr)
        sys.exit(gc.EXIT_INFRA)
