# -*- coding: utf-8 -*-
"""nav_lint.py —— g030-nav 判定核心（W5-C1 .github#224 / ADR-0070 决策 2）。

AI 导航容易度关卡组（宪法 §9#5：该度量是公共知识空白——自研项；方法署名
aider repo-map：符号索引+按引用度排序、固定 token 预算封顶装配）。五项检查：
  1. 每目录文件数上限（防平铺——文件堆一个目录=agent/人都无法定位）；
  2. repo-map 覆盖率：固定 token 预算下能装进 repo-map 的符号比例；
  3. 引用图跳数：入口 BFS 平均跳数/直径/环数（ SCC>1 计环）；
  4. 文档-符号验真：README/AGENTS/docs 反引号引用的符号必须在符号宇宙中存在；
  5. 词表 lint：版本化 glossary（quality/glossary.yaml）——文档缩写术语未入表
     即 fail；弃用同义词出现即 fail。
全部输出指标、超阈值 fail（卡 AC-2）。零网络零新依赖。
"""
import json
import os
import re
import sys
import time
from collections import deque

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import gate_common as gc
import tidy_scan as ts

GATE = 'g030-nav'
RE_CODE_SPAN = re.compile(r'`([^`\n]+)`')
RE_FENCE = re.compile(r'^```.*?^```', re.M | re.S)
RE_IDENT = re.compile(r'^[A-Za-z_][A-Za-z0-9_-]{1,63}$')
RE_ACRONYM = re.compile(r'\b[A-Z][A-Z0-9]{1,6}\b')
TOKEN_OVERHEAD = 16  # repo-map 每符号条目固定开销的字符估价（路径头+花括号+换行）


def finding(file, line, rule, message, fix):
    return {'file': file, 'line': line, 'rule': rule, 'message': message,
            'fixHint': '%s（ruleId=%s）' % (fix, rule)}


def md_files(root, doc_paths):
    """文档面：doc-paths 内的 .md（文件与目录混排，'|' 连接）。"""
    out = []
    for p in ts.split_paths(doc_paths):
        full = os.path.normpath(os.path.join(root, p))
        if os.path.isfile(full) and full.endswith('.md'):
            out.append(p.replace('\\', '/'))
        elif os.path.isdir(full):
            for dirpath, _dirs, fns in os.walk(full):
                for fn in sorted(fns):
                    if fn.endswith('.md'):
                        out.append(os.path.relpath(os.path.join(dirpath, fn), root).replace('\\', '/'))
    return sorted(out)


def doc_symbols(root, files):
    """文档可引用的符号宇宙：代码符号 + make 目标 + npm scripts + CI job id +
    根文件名（文档说得到=验得到——README 的 CI 结构/命令面都是可验证对象）。"""
    sym = ts.make_symbols(root, files)
    out = set(sym)
    mk = os.path.join(root, 'Makefile')
    if os.path.isfile(mk):
        for m in ts.RE_MAKE_TARGET.finditer(ts.read(root, 'Makefile')):
            if not m.group(1).startswith('.'):
                out.add(m.group(1))
    pkg = os.path.join(root, 'package.json')
    if os.path.isfile(pkg):
        try:
            out |= set(json.load(open(pkg, encoding='utf-8')).get('scripts', {}))
        except (OSError, ValueError):
            pass
    out |= {fn for fn in os.listdir(root) if os.path.isfile(os.path.join(root, fn))}
    wf = os.path.join(root, '.github', 'workflows')
    if os.path.isdir(wf):
        for fn in sorted(os.listdir(wf)):
            if not fn.endswith(('.yml', '.yaml')):
                continue
            text = open(os.path.join(wf, fn), encoding='utf-8', errors='replace').read()
            in_jobs = False
            for line in text.splitlines():
                if re.match(r'^jobs:\s*$', line):
                    in_jobs = True
                elif in_jobs and re.match(r'^\S', line):
                    in_jobs = False
                elif in_jobs and re.match(r'^  ([A-Za-z][\w-]+):\s*$', line):
                    out.add(re.match(r'^  ([A-Za-z][\w-]+):\s*$', line).group(1))
    return out, sym


def repomap(universe, refs, budget):
    """aider repo-map 简化：按引用数降序（并列按名）装配，token 价=(名+路径+开销)//4。"""
    ranked = sorted(universe, key=lambda n: (-refs.get(n, 0), n))
    used, covered = 0, 0
    for name in ranked:
        home = min(universe[name]['files'])  # 多文件同名取字典序最小——输出可复现（ratchet 稳定）
        cost = (len(name) + len(home) + TOKEN_OVERHEAD) // 4
        if used + cost > int(budget):
            continue
        used += cost
        covered += 1
    return covered, used


def hop_metrics(graph, entry):
    dist = {}
    if entry in graph:
        dist[entry] = 0
        q = deque([entry])
        while q:
            cur = q.popleft()
            for nxt in sorted(graph[cur]):
                if nxt not in dist:
                    dist[nxt] = dist[cur] + 1
                    q.append(nxt)
    reach = [d for f, d in dist.items() if f != entry]
    avg = round(sum(reach) / len(reach), 2) if reach else 0.0
    return avg, (max(reach) if reach else 0), dist


def count_cycles(graph):
    """环数=强连通分量（size>1）+自环数（迭代 Tarjan，图小也稳）。"""

    def scc_count(adj):
        index, low, on, stack = {}, {}, set(), []
        cnt = [0]  # 下发计数器
        found = [0]  # 环计数器
        for start in adj:
            if start in index:
                continue
            work = [(start, iter(sorted(adj[start])))]
            index[start] = low[start] = cnt[0]
            cnt[0] += 1
            stack.append(start)
            on.add(start)
            while work:
                node, it = work[-1]
                pushed = False
                for nxt in it:
                    if nxt not in adj:
                        continue
                    if nxt not in index:
                        index[nxt] = low[nxt] = cnt[0]
                        cnt[0] += 1
                        stack.append(nxt)
                        on.add(nxt)
                        work.append((nxt, iter(sorted(adj[nxt]))))
                        pushed = True
                        break
                    if nxt in on:
                        low[node] = min(low[node], index[nxt])
                if not pushed:
                    work.pop()
                    if work:
                        low[work[-1][0]] = min(low[work[-1][0]], low[node])
                    if low[node] == index[node]:
                        comp = []
                        while True:
                            m = stack.pop()
                            on.discard(m)
                            comp.append(m)
                            if m == node:
                                break
                        if len(comp) > 1 or node in adj[node]:
                            found[0] += 1
        return found[0]

    return scc_count(graph)


def strip_prose(text):
    """去掉围栏代码块与行内代码跨度 → 只剩散文（词表/缩写只管散文）。"""
    return RE_CODE_SPAN.sub(' ', RE_FENCE.sub(' ', text))


def load_glossary(root, th):
    path = os.path.normpath(os.path.join(root, th['glossary-file']))
    with open(path, encoding='utf-8') as f:
        g = gc.load_yaml(f.read())
    if not isinstance(g.get('acronyms'), dict) or not isinstance(g.get('version'), int):
        raise gc.ContractError('glossary.yaml 缺 acronyms 映射或 version 整数（fail-closed）: %s' % path)
    return g


def scan(root, th):
    F = []
    files = ts.list_files(root, th['scan-paths'], th['ignore-paths'])

    # 1. 每目录文件数上限
    counts = {}
    for rel in files:
        d = os.path.dirname(rel) or '.'
        counts[d] = counts.get(d, 0) + 1
    over = {d: c for d, c in sorted(counts.items()) if c > int(th['max-files-per-dir'])}
    for d, c in over.items():
        F.append(finding(d + '/', 1, 'nav-flat-directory',
                         '目录 %s 内 %d 个文件 > 上限 %d（平铺=无法定位）' % (d, c, int(th['max-files-per-dir'])),
                         '按职责建子目录分组；每目录一个主题，目录名即可当索引'))

    # 2. repo-map 覆盖率
    universe = ts.make_symbols(root, files)
    refs, _occ = ts.ref_counts(root, files, universe)
    covered, used = repomap(universe, refs, th['repomap-token-budget'])
    total = len(universe)
    coverage = round(covered / total, 4) if total else 1.0
    if total and coverage < float(th['min-repomap-coverage']):
        F.append(finding('repo-map', 1, 'nav-repomap-coverage',
                         'repo-map 符号覆盖率 %.0f%%（%d/%d 符号，token 预算 %s 内装不下）'
                         % (coverage * 100, covered, total, th['repomap-token-budget']),
                         '收敛公共面（删未引用导出）或提高符号复用；预算是导航成本上限，不许靠调预算放行'))

    # 3. 引用图跳数/直径/环
    graph = ts.import_graph(root, files)
    avg, diameter, _dist = hop_metrics(graph, th['entry-file'])
    cycles = count_cycles(graph)
    if avg > float(th['max-avg-hops']):
        F.append(finding(th['entry-file'], 1, 'nav-hop-distance',
                         '引用图平均跳数 %.2f > 上限 %s（agent 从入口定位符号太远）' % (avg, th['max-avg-hops']),
                         '入口直接导出高频符号；中间转发层要下沉为真实实现'))
    if diameter > int(th['max-graph-diameter']):
        F.append(finding(th['entry-file'], 1, 'nav-graph-diameter',
                         '引用图直径 %d > 上限 %s（最远符号藏太深）' % (diameter, th['max-graph-diameter']),
                         '把深层能力提升到入口可见的模块，或缩短引用链'))
    if cycles > int(th['max-import-cycles']):
        F.append(finding('import-graph', 1, 'nav-import-cycle',
                         '导入环 %d 个 > 上限 %s' % (cycles, th['max-import-cycles']),
                         '拆出共享底层模块打破环（depcruise no-circular 同口径）'))

    # 4/5. 文档面：符号验真 + 词表 lint
    docs = md_files(root, th['doc-paths'])
    known, _sym = doc_symbols(root, files)
    known |= set(ts.split_paths(th['doc-symbol-ignore']))
    gloss = load_glossary(root, th)
    dangling = undefined = deprecated = 0
    for rel in docs:
        text = ts.read(root, rel)
        for i, line in enumerate(text.splitlines(), 1):
            for m in RE_CODE_SPAN.finditer(line):
                tok = m.group(1).strip()
                if RE_IDENT.match(tok) and tok not in known:
                    dangling += 1
                    F.append(finding(rel, i, 'nav-doc-symbol-dangling',
                                     '文档引用的符号 %r 不存在于代码/make/npm 符号宇宙' % tok,
                                     '改引真实符号名，或在 contract g030-nav.doc-symbol-ignore 登记（仅限非代码专名）'))
        prose_lines = strip_prose(text)
        for m in RE_ACRONYM.finditer(prose_lines):
            if m.group(0) not in gloss['acronyms']:
                undefined += 1
                F.append(finding(rel, 1, 'nav-glossary-undefined',
                                 '缩写术语 %s 未入词表（glossary 版本 %d）' % (m.group(0), gloss['version']),
                                 '先在 quality/glossary.yaml 的 acronyms 补定义（含全称），再使用缩写'))
        for bad, good in (gloss.get('deprecated') or {}).items():
            if bad in prose_lines:
                deprecated += 1
                F.append(finding(rel, 1, 'nav-glossary-deprecated',
                                 '弃用词「%s」出现（规范词：%s）' % (bad, good),
                                 '统一改用规范词；同义词漂移是 agent 检索的死敌'))

    m = {'nav.dirOverloads': len(over), 'nav.repomapTotal': total,
         'nav.repomapUncovered': total - covered, 'nav.repomapCoverage': coverage,
         'nav.repomapTokens': used, 'nav.avgHops': avg, 'nav.graphDiameter': diameter,
         'nav.importCycles': cycles, 'nav.docFiles': len(docs),
         'nav.docDangling': dangling, 'nav.glossaryUndefined': undefined,
         'nav.glossaryDeprecated': deprecated}
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
              'summary': 'AI 导航：目录超载=%d 覆盖率=%.0f%% 平均跳数=%s 环=%d 文档断链=%d 词表未定义=%d'
                         % (metrics['nav.dirOverloads'], metrics['nav.repomapCoverage'] * 100,
                            metrics['nav.avgHops'], metrics['nav.importCycles'],
                            metrics['nav.docDangling'], metrics['nav.glossaryUndefined']),
              'fixHint': '见逐条 violations 的 fixHint' if findings else None}
    out = os.environ.get('GATE_REPORT_OUT') or os.path.join(root, 'quality', 'reports', GATE + '.json')
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
