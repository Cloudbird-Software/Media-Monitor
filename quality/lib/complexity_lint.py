# -*- coding: utf-8 -*-
"""complexity_lint.py —— g040-complexity 判定核心（W5-C1 .github#224 / ADR-0070 决策 3）。

抗复杂度组（对 AI 复杂化冲动的机器对手——人 review 拦不住规模化抽象通胀）：
  1. 认知复杂度：Sonar 认知复杂度口径的简化静态启发（署名：SonarQube cognitive
     complexity；因素清单启发自 CodeScene——嵌套深度/布尔组合/循环/分支）：
     控制流关键字 +1、嵌套每层再 +1、布尔序列 &&/|| 每个 +1、三元 +1；
  2. 死通用性：导出符号零跨文件引用（入口文件豁免——公共 API 面按设计存在）；
  3. Rule-of-Three 机械化：PR 新增导出符号引用点 <3 即拦（ADR-0070 决策 3）；
  4. 无逻辑 wrapper：函数体=纯转发（return other(仅参数)）即拦；
  5. 净增 LOC 预算：PR 在产品代码面的净增行数上限；
  6. 抑制标记零增长：新增抑制标记（@ts-ignore 等；org 全集见 CI-Workflows
     policy/suppressions.yaml）必须带 ADR/issue 引用，否则拦（带引用=走
     豁免审计通道，计入豁免行数指标）；
  7. 豁免审计：quality/exemptions.yaml 每条豁免必须带可追溯 ref（ADR/issue）。
退出码：0=过 1=违规 3=infra。零网络零新依赖；AST-lite 局限见 tidy_scan 头注。
"""
import json
import os
import re
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import gate_common as gc
import tidy_scan as ts

GATE = 'g040-complexity'
# 抑制标记字面量拆形构造：org 抑制预算（ADR-0036）对全树计 <标记>——本关卡的
# 检测器自己不得自计入（CI-Workflows suppression-budget.sh 头部同纪律）。
RE_SUPPRESS = re.compile('|'.join([
    'eslint-' 'disable', '@ts-ignore', '@ts-expect-error', '@ts-nocheck',
    'prettier-ignore', 'biome-ignore', 'stylelint-disable']))
RE_REF = re.compile(r'ADR-[0-9]+|#[0-9]+')
RE_REF_FULL = re.compile(r'^(ADR-[0-9]+|\.github#[0-9]+|#[0-9]+)$')
CTRL = re.compile(r'\b(if|for|while|case|catch|else)\b')
BOOL = re.compile(r'&&|\|\|')
TERNARY = re.compile(r'\s\?\s')
FN_KEYWORDS = ('if', 'for', 'while', 'switch', 'catch', 'function', 'return',
               'typeof', 'new', 'else', 'do', 'await', 'yield', 'delete', 'in', 'of')
RE_FN_DECL = re.compile(r'\bfunction\s*\*?\s+([A-Za-z_$][\w$]*)\s*\(([^)]*)\)')
RE_FN_ASSIGN = re.compile(r'\b(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*(?::[^=;]+)?='
                          r'\s*(?:async\s+)?(?:\(([^)]*)\)|[A-Za-z_$][\w$]*)\s*=>')
RE_IDENT_TOKEN = re.compile(r'[A-Za-z_$][\w$]*')


def finding(file, line, rule, message, fix):
    return {'file': file, 'line': line, 'rule': rule, 'message': message,
            'fixHint': '%s（ruleId=%s）' % (fix, rule)}


def mute(text):
    """字符串/模板/注释内容置空格（保换行保长度）——防字符串里的关键字/花括号污染计数。"""
    st = {'str': None, 'block': False}
    out = []
    for line in text.split('\n'):
        buf = []
        i = 0
        while i < len(line):
            c, nxt = line[i], (line[i + 1] if i + 1 < len(line) else '')
            if st['block']:
                if c == '*' and nxt == '/':
                    st['block'] = False
                    buf.append('  ')
                    i += 2
                else:
                    buf.append(' ')
                    i += 1
            elif st['str']:
                if c == '\\':
                    buf.append('  ')
                    i += 2
                elif c == st['str']:
                    st['str'] = None
                    buf.append(c)
                    i += 1
                else:
                    buf.append(' ')
                    i += 1
            elif c == '/' and nxt == '/':
                buf.append(' ' * (len(line) - i))
                i = len(line)
            elif c == '/' and nxt == '*':
                st['block'] = True
                buf.append('  ')
                i += 2
            elif c in ('"', "'", '`'):
                st['str'] = c
                buf.append(c)
                i += 1
            else:
                buf.append(c)
                i += 1
        out.append(''.join(buf))
    return '\n'.join(out)


def functions(text):
    """函数区域 [(name, params, start_line, body_lines)]——具名 function/赋值箭头。"""
    lines = mute(text).split('\n')
    out = []
    for idx, line in enumerate(lines):
        m = RE_FN_DECL.search(line) or RE_FN_ASSIGN.search(line)
        if not m:
            continue
        name, params = m.group(1), m.group(2) or ''
        pre, post = line[:m.start()], line[m.end():]
        if '=>' in line and '{' not in post.split('=>', 1)[1]:
            out.append((name, params, idx, [line]))  # 单表达式箭头：区域=本行
            continue
        depth, seen, j = 0, False, idx
        while j < len(lines):
            seg = lines[idx][m.end():] if j == idx else lines[j]
            for ch in seg:
                if ch == '{':
                    depth += 1
                    seen = True
                elif ch == '}':
                    depth -= 1
            if seen and depth <= 0:
                break
            j += 1
        out.append((name, params, idx, lines[idx:j + 1]))
    return out


def cognitive(body_lines):
    """简化 Sonar 认知复杂度：控制流 +1、每层嵌套再 +1、布尔/三元每个 +1。
    depth=函数体内嵌套层（声明行的 "{" 使 k>=1 行处于体内第 1 层）；
    关键字前的本行括号修正（如 "} else if" 的闭括号）让嵌套归属正确。"""
    score, depth = 0, 0
    for _k, line in enumerate(body_lines):
        m = CTRL.search(line)
        pre = line[:m.start()] if m else ''
        rel = max(0, depth - 1 + pre.count('{') - pre.count('}'))
        score += len(CTRL.findall(line)) * (1 + rel)
        score += len(BOOL.findall(line)) + len(TERNARY.findall(line))
        depth += line.count('{') - line.count('}')
    return score


def is_wrapper(params, body_lines):
    """纯转发判定：函数体归一化后= return other(<仅参数/参数展开>) 形态。"""
    pids = set(RE_IDENT_TOKEN.findall(params))
    body = ' '.join(' '.join(body_lines).split())
    m = re.match(r'^(?:export\s+)?(?:async\s+)?(?:function\s+[\w$]+\s*\([^)]*\)\s*)?'
                 r'(?:\{|=>)?\s*return\s+([A-Za-z_$][\w$.]*)\s*\((.*)\)\s*;?\s*\}?$'
                 r'|^(?:\([^()]*\)|[A-Za-z_$][\w$]*)\s*=>\s*([A-Za-z_$][\w$.]*)\s*\((.*)\)\s*;?$',
                 body)
    if not m:
        return False
    args = m.group(2) if m.group(2) is not None else m.group(4)
    if not args.strip():
        return True  # return other() 无参转发
    for tok in args.split(','):
        tok = tok.strip()
        if tok.startswith('...'):
            tok = tok[3:]
        if tok and tok not in pids:
            return False
    return True


def scan(root, th):
    F = []
    files = ts.list_files(root, th['code-paths'], th.get('ignore-paths', ''))
    ts_files = [f for f in files if f.endswith(('.ts', '.tsx', '.js'))]
    entries = set(ts.split_paths(th['entry-paths']))

    # 1. 认知复杂度 + 4. wrapper 套娃
    max_cx, wrappers, fns = 0, 0, 0
    for rel in ts_files:
        text = ts.read(root, rel)
        for name, params, start, body in functions(text):
            fns += 1
            score = cognitive(body)
            max_cx = max(max_cx, score)
            if score > int(th['max-cognitive-complexity']):
                F.append(finding(rel, start + 1, 'cx-cognitive-complexity',
                                 '函数 %s 认知复杂度 %d > 上限 %d（嵌套/布尔组合/分支过多）'
                                 % (name, score, int(th['max-cognitive-complexity'])),
                                 '拆函数：每层嵌套抽独立函数、布尔链命名化（提前 return 降嵌套）'))
            if is_wrapper(params, body):
                wrappers += 1
                F.append(finding(rel, start + 1, 'cx-logicless-wrapper',
                                 '函数 %s 是纯转发层（无逻辑 wrapper）' % name,
                                 '删除该层让调用方直连，或在此层加真实逻辑（校验/聚合/适配）'))

    # 2. 死通用性（入口文件豁免——公共 API 面按设计存在）
    universe = ts.make_symbols(root, files)
    refs, occ = ts.ref_counts(root, files, universe)
    dead = 0
    for name, ent in sorted(universe.items()):
        if ent['kind'] != 'export':
            continue
        if ent['files'] & entries:
            continue
        if refs.get(name, 0) == 0:
            dead += 1
            F.append(finding(sorted(ent['files'])[0], 1, 'cx-dead-export',
                             '导出符号 %s 零跨文件引用（死通用性）' % name,
                             '删除导出（YAGNI）；确属预埋扩展点走 quality/exemptions.yaml 登记（带 ref）'))

    # 5. 净增 LOC 预算 + 6. 抑制零增长 + 3. Rule-of-Three（PR 语境；本地模式跳过）
    net, suppress_growth, premature, exempted_sup = 0, 0, 0, 0
    base = os.environ.get('GATE_BASE', '')
    budget_paths = ts.split_paths(th['loc-budget-paths'])
    sup_paths = ts.split_paths(th['suppression-scan-paths'])
    if base:
        head = os.environ.get('GATE_HEAD', 'HEAD')
        numstat = ts.git(root, 'diff', '--numstat', '%s...%s' % (base, head))
        for row in numstat.splitlines():
            parts = row.split('\t')
            if len(parts) != 3 or '-' in (parts[0], parts[1]):
                continue
            rel = parts[2]
            if any(rel == p or rel.startswith(p + '/') for p in budget_paths):
                net += int(parts[0]) - int(parts[1])
        if net > int(th['max-net-loc']):
            F.append(finding('<diff>', 1, 'cx-loc-budget',
                             'PR 净增 %d 行 > 预算 %d（产品代码面）' % (net, int(th['max-net-loc'])),
                             '拆 PR：一个 PR 一件事；或删除死代码对冲净增'))
        changed = ts.diff_files(root, base, head)
        new_syms = {}
        for rel, line in ts.diff_added_lines(root, base, head, budget_paths):
            if RE_SUPPRESS.search(line):
                if RE_REF.search(line):
                    exempted_sup += 1  # 带引用=豁免通道（行数入指标，见豁免审计）
                else:
                    suppress_growth += 1
                    F.append(finding(rel, 1, 'cx-suppression-growth',
                                     '新增抑制标记无 ADR/issue 引用：%s' % line.strip()[:80],
                                     '删掉抑制标记修根因；确需豁免：同行加 ADR-NNNN 或 #issue 引用'))
            for pat in (RE_FN_DECL, RE_FN_ASSIGN):
                m = pat.search(line)
                if m and 'export' in line:
                    new_syms.setdefault(m.group(1), rel)
        for name, rel in sorted(new_syms.items()):
            r = occ.get(name, 0)  # 调用点=出现次数（Rule-of-Three 的“三次”）
            if 1 <= r < int(th['rule-of-three-min-refs']):
                premature += 1
                F.append(finding(rel, 1, 'cx-premature-abstraction',
                                 '新增导出 %s 引用点仅 %d < 3（Rule-of-Three：三次重复才值得抽象）'
                                 % (name, r),
                                 '先内联，出现第 3 个使用点再抽公共层（ADR-0070 决策 3）'))

    # 抑制总量（本地也计——棘轮指标面）
    suppress_total = 0
    for rel in files:
        if any(rel == p or rel.startswith(p + '/') for p in sup_paths):
            suppress_total += len(RE_SUPPRESS.findall(ts.read(root, rel)))

    # 7. 豁免审计
    exemptions, exempt_count = [], 0
    ex_path = os.path.normpath(os.path.join(root, th['exemptions-file']))
    if os.path.isfile(ex_path):
        with open(ex_path, encoding='utf-8') as f:
            exemptions = gc.load_yaml(f.read()).get('exemptions') or []
    for e in exemptions:
        exempt_count += 1
        ref = str(e.get('ref', ''))
        if not (e.get('gate') and e.get('path') and e.get('reason') and RE_REF_FULL.match(ref)):
            F.append(finding(th['exemptions-file'], 1, 'cx-exemption-unref',
                             '豁免条目缺 gate/path/reason 或 ref 不可追溯：%r' % e,
                             '补全字段；ref 必须是 ADR-NNNN / .github#NN / #NN（可追溯）'))
    m = {'cx.functions': fns, 'cx.maxCognitive': max_cx,
         'cx.deadExports': dead, 'cx.prematureAbstractions': premature,
         'cx.logiclessWrappers': wrappers, 'cx.suppressionTotal': suppress_total,
         'cx.exemptionCount': exempt_count + exempted_sup, 'cx.netLoc': net}
    return m, F


def run(argv):
    t0 = time.time()
    root = os.getcwd()
    th = ts.load_section(GATE)
    metrics, findings = scan(root, th)
    code = gc.EXIT_FAIL_FIXABLE if findings else gc.EXIT_PASS
    report = {'gate': GATE, 'severity': 'block', 'ownerRole': 'implementer',
              'status': 'fail' if findings else 'pass',
              'metrics': dict(metrics, violationCount=len(findings), exitCode=code),
              'ratchetKeys': [], 'violations': findings,
              'durationMs': int((time.time() - t0) * 1000),
              'summary': '抗复杂度：maxCognitive=%d 死导出=%d 过早抽象=%d wrapper=%d 抑制=%d 豁免=%d 净增LOC=%d'
                         % (metrics['cx.maxCognitive'], metrics['cx.deadExports'],
                            metrics['cx.prematureAbstractions'], metrics['cx.logiclessWrappers'],
                            metrics['cx.suppressionTotal'], metrics['cx.exemptionCount'], metrics['cx.netLoc']),
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
