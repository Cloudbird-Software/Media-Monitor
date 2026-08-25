# -*- coding: utf-8 -*-
"""spec_lint.py —— g010-spec-schema 的判定核心（ADR-0060 决策 3/4/5）。

为什么拆出 lib：关卡入口（gates/g010-spec-schema.sh）只做解释器/环境解析，
判定逻辑全部在这里，可被 run-all.sh 与后续 g160 复用。

三类检查（每类一条 ADR 决策，fail 一律 exit 1 + UID + 行号 fixHint）：
  结构：H1 唯一 / frontmatter 元数据块 / AC 编号连续（重复编号=篡改，exit 2）
  句型：BEH 条款必须匹配 EARS 六句型（正则来自 contract.yaml，版本化）
  禁词：模糊词出现在 AC/BEH 即 fail；唯一豁免=同条款绑定已定义的 BUDGET 编号
  追溯：孤儿条款（AC 无卡且无测试）/ 镀金（测试引用不存在的 AC）/ 断链
        （IR/BUDGET/TEST 引用不存在）——宪法 §4E 追溯闭合
零网络零 LLM：IR 账本/卡内容在线闭合由 org-gate 负责，这里只吃本地索引。
"""
import json
import os
import re
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import gate_common as gc  # 同目录导入（import 位置在 sys.path 修改之后，属预期）


def finding(file, line, rule, message, fix_hint, symbol='', evidence='', escalate=False):
    """一条违规。escalate=True 表示只人类可解（exit 2），落盘前剥离该内部键。"""
    f = dict(file=file, line=line, rule=rule, message=message, fixHint=fix_hint,
             symbol=symbol, evidence=evidence)
    if escalate:
        f['escalate'] = True
    return f


def parse_spec(path, text):
    """返回 (meta, clauses, h1_lines, fm_range)。clauses=[(uid, text, line, section)]。"""
    lines = text.splitlines()
    start = next((i for i, l in enumerate(lines[:5]) if l.strip() == '---'), None)
    meta, fm_end = {}, None
    if start is not None:
        fm_end = next((i for i in range(start + 1, len(lines)) if lines[i].strip() == '---'), None)
        if fm_end is None:
            fm_end = None  # 不闭合：交给结构检查报错，这里不吞
        else:
            meta = gc.load_yaml('\n'.join(lines[start + 1:fm_end]))
    clauses, section = [], ''
    body_from = 0 if fm_end is None else fm_end + 1
    for i in range(body_from, len(lines)):
        l = lines[i]
        m = re.match(r'^##\s+(\S+)', l)
        if m:
            section = m.group(1)
            continue
        m = re.match(r'^-\s+([A-Za-z]+-[0-9]+):\s*(.*)$', l)
        if m and re.match(r'^(INV|BEH|IFACE|BUDGET|DECISION|ASSUMPTION)$', m.group(1).split('-')[0]):
            clauses.append((m.group(1), m.group(2), i + 1, section))
    h1_lines = [i + 1 for i, l in enumerate(lines) if re.match(r'^#\s+\S', l)]
    return meta, clauses, h1_lines, (start, fm_end)


def lint_spec(path, text, th, ctx):
    """单个 spec 的全部检查，返回 findings（可能为空）。"""
    out = []
    meta, clauses, h1_lines, (fm_s, fm_e) = parse_spec(path, text)
    spec = th['spec']
    # -- 结构：H1 唯一（ADR 决策 3 的三项之一）--
    if len(h1_lines) != 1:
        out.append(finding(path, h1_lines[0] if h1_lines else 1, 'structure-h1',
                           'H1 标题必须恰好 1 个，实际 %d 个' % len(h1_lines),
                           '第%s行：保留一个 "# 标题"，其余降为 ##/' % (h1_lines or ['无'])))
    # -- 结构：frontmatter 元数据块 --
    if fm_s is None or fm_e is None:
        out.append(finding(path, 1, 'structure-frontmatter',
                           '缺少 --- 包裹的 frontmatter 元数据块（或不闭合）',
                           '第1行起：文件开头用 --- ... --- 包住元数据键值'))
        meta = meta or {}
    for key in spec['required_meta_keys']:
        if key not in meta or meta[key] in (None, '', [], {}):
            out.append(finding(path, 1, 'meta-required', '元数据缺键 %r' % key,
                               'frontmatter 补 %s:（结构断言键来自 contract.yaml）' % key))
    if meta.get('taskId') and not re.match(spec['task_id_pattern'], str(meta['taskId'])):
        out.append(finding(path, 1, 'meta-pattern', 'taskId %r 不匹配 %s' % (meta['taskId'], spec['task_id_pattern']),
                           'taskId 改为 PREFIX-数字 形（如 SPEC-0201）'))
    if meta.get('irRef') and not re.match(spec['ir_pattern'], str(meta['irRef'])):
        out.append(finding(path, 1, 'meta-pattern', 'irRef %r 不匹配 %s' % (meta['irRef'], spec['ir_pattern']),
                           'irRef 改为 IR-数字 形'))
    title = str(meta.get('title') or '')
    if title and len(title) > spec['max_title_length']:
        out.append(finding(path, 1, 'meta-title-length', '标题 %d 字超上限 %d' % (len(title), spec['max_title_length']),
                           'title 压缩到一句话（可观测行为），细节进正文'))
    # -- 结构：AC 编号连续（重复=篡改影子，exit 2）--
    acs = meta.get('acceptanceCriteria') or []
    ac_ids = [str(a.get('id')) for a in acs if isinstance(a, dict)]
    ctx['ac_count'] += len(ac_ids)
    if len(acs) < spec['min_ac_clauses']:
        out.append(finding(path, 1, 'ac-min-clauses', 'AC 条款 %d 条 < 下限 %d' % (len(acs), spec['min_ac_clauses']),
                           'frontmatter 补 acceptanceCriteria（无验收面=不是 spec）'))
    for aid in ac_ids:
        if not re.match(spec['ac_id_pattern'], aid):
            out.append(finding(path, 1, 'ac-id-pattern', 'AC id %r 不匹配 %s' % (aid, spec['ac_id_pattern']),
                               'id 改为 AC-数字 形'))
    nums = sorted(int(re.sub(r'^AC-', '', a)) for a in ac_ids if re.match(r'^AC-[0-9]+$', a))
    dups = [n for n in set(nums) if nums.count(n) > 1]
    missing = [n for n in range(1, len(nums) + 1) if n not in nums]
    if dups:
        out.append(finding(path, 1, 'ac-duplicate-uid',
                           'AC 编号重复 %s（重复定义=影子条款，篡改类只人类可解）' % dups,
                           '删除重复的 AC-%d 定义，保留正本' % dups[0], escalate=True))
    elif missing:
        # 重复存在时编号检查无意义（以篡改信号为准），只在无重复时报缺号
        out.append(finding(path, 1, 'ac-numbering', 'AC 编号不连续，缺 %s' % missing,
                           '按 AC-1..AC-N 连续重排（删条款也要重排编号）'))
    # -- 句型 + 禁词：BEH 条款 --
    beh = [(u, t, ln) for (u, t, ln, sec) in clauses if u.startswith('BEH-')]
    ctx['beh_count'] += len(beh)
    ears = [re.compile(r) for r in th['ears'].values()]
    ears_names = '/'.join(th['ears'].keys())
    vague = th['vague_terms']
    budget_pat = re.compile(th['trace']['budget_id_pattern'])
    test_pat = re.compile(th['trace']['test_id_pattern'])
    defined_budgets = {u for (u, _t, _l, _s) in clauses if u.startswith('BUDGET-')}
    ac_texts = [(a.get('id'), ' '.join(str(a.get(k) or '') for k in ('given', 'when', 'then')))
                for a in acs if isinstance(a, dict)]
    for uid, txt, ln in beh:
        if not any(p.search(txt) for p in ears):
            out.append(finding(path, ln, 'ears-pattern', '%s 不匹配 EARS 六句型' % uid,
                               '第%d行：改写为 当…时，…/若…则…/在…期间…/系统必须… 之一（句型=%s）' % (ln, ears_names),
                               symbol=uid, evidence=txt[:60]))
    for uid, txt, ln in beh + [(i, t, None) for i, t in ac_texts]:
        ln_s = ln if ln is not None else 1
        # 禁词：模糊词命中且未绑定 BUDGET 才 fail；绑定不存在=断链（豁免必须可 grep 复核）
        hit = [w for w in vague if w in txt]
        if hit:
            bound = budget_pat.findall(txt)
            if bound and all(b in defined_budgets for b in bound):
                ctx['exempted'] += len(hit)  # 豁免成立：绑定关系留在条款文本里，可 grep 复核
            elif bound:
                miss = [b for b in bound if b not in defined_budgets]
                out.append(finding(path, ln_s, 'trace-broken-link',
                                   '%s 绑定的 %s 在本 spec BUDGET 节未定义（断链）' % (uid, miss),
                                   '第%d行：BUDGET 节补定义 %s，或删掉条款里的绑定' % (ln_s, miss), symbol=uid))
            else:
                out.append(finding(path, ln_s, 'vague-term', '%s 含模糊词 %s 且未绑定 BUDGET' % (uid, hit),
                                   '第%d行：换成可判定表述；确属预算裁量则写明「BUDGET-数字」绑定' % ln_s,
                                   symbol=uid, evidence=txt[:60]))
        # 断链：条款里引用的 TEST-uid 必须在测试索引（无索引时此项降级，run() 里置 None）
        for t_ref in test_pat.findall(txt):
            if ctx['tests'] is not None and t_ref not in ctx['tests']:
                out.append(finding(path, ln_s, 'trace-broken-link',
                                   '%s 引用的 %s 不在测试索引（断链）' % (uid, t_ref),
                                   '第%d行：补测试并登记索引，或删引用' % ln_s, symbol=uid))
    # -- 追溯闭合：孤儿/镀金/断链（决策 5）--
    task = str(meta.get('taskId') or '')
    covered = {ref.split(':')[-1] for ref in ctx['tests_refs'] if ref.startswith(task + ':')}
    card_bound = bool(meta.get('card')) or bool(ctx['gate_card'])
    if not card_bound and th['trace']['require_card_binding']:
        for aid in ac_ids:
            if aid not in covered:
                out.append(finding(path, 1, 'trace-orphan-clause',
                                   '%s 无卡无测试（孤儿条款）' % aid,
                                   'frontmatter 补 card: <owner>/<repo>#<n>，或补测试索引覆盖 %s' % aid, symbol=aid))
    for t_uid, ref in ctx['tests_items']:
        if ref.startswith(task + ':') and ref.split(':')[-1] not in ac_ids:
            out.append(finding(path, 1, 'trace-gilding-scope',
                               '%s 覆盖不存在的 %s（镀金范围）' % (t_uid, ref),
                               '测试 %s 改绑真实 AC，或 spec 补该 AC' % t_uid, symbol=t_uid))
    if meta.get('irRef') and ctx['ir_index'] is not None and str(meta['irRef']) not in ctx['ir_index']:
        out.append(finding(path, 1, 'trace-broken-link', 'irRef %s 不在 IR 索引（断链）' % meta['irRef'],
                           'irRef 改为真实存在的 IR 编号'))
    if meta.get('card') and ctx['gate_card'] and str(meta['card']).strip() != str(ctx['gate_card']).strip():
        out.append(finding(path, 1, 'trace-card-conflict',
                           'spec 卡引用 %r 与运行上下文 GATE_CARD=%r 冲突（篡改嫌疑，只人类可解）'
                           % (meta['card'], ctx['gate_card']),
                           '人核对：以卡 issue 为准改 spec 或改调用上下文', escalate=True))
    return out


def load_index(path, want):
    """读 GATE_TESTS_INDEX / GATE_IR_INDEX；给不了路径返回 None（该项检查降级为格式校验）。"""
    if not path:
        return None
    try:
        with open(path, encoding='utf-8') as f:
            raw = [l.strip() for l in f if l.strip() and not l.startswith('#')]
    except OSError as e:
        raise gc.ContractError('索引 %s 读不了（infra）: %s' % (path, e))
    if want == 'ir':
        return {l for l in raw if re.match(r'^IR-[0-9]+$', l)}
    items, bad = [], []
    for l in raw:
        m = re.match(r'^(TEST-[0-9]+)\s+([A-Za-z]+-[0-9]+:AC-[0-9]+)$', l)
        if m:
            items.append((m.group(1), m.group(2)))
        else:
            bad.append(l)
    return items, bad


def collect_specs(argv, env):
    if argv:
        return list(argv)
    if env.get('GATE_SPEC'):
        return [p for p in re.split(r'[\n:]+', env['GATE_SPEC']) if p]
    if env.get('GATE_TASK_ID'):
        return [os.path.join('specs', env['GATE_TASK_ID'], 'spec.md')]
    import glob
    return sorted(glob.glob(os.path.join(gc.repo_root(), 'specs', '**', 'spec.md'), recursive=True))


def run(argv):
    env = os.environ
    t0 = time.time()
    th = gc.apply_env_overrides(gc.contract_get(gc.load_contract(), 'thresholds.g010'), 'g010')
    idx = load_index(env.get('GATE_TESTS_INDEX', ''), 'tests')
    # 无索引（env 未给）→ tests 置 None：TEST 引用断链检查降级（离线没有索引可对），
    # 孤儿/镀金仍按"无测试覆盖"执法——空索引与无索引语义不同，不混。
    if idx is None:
        tests_items, tests_bad, tests_set = [], [], None
    else:
        tests_items, tests_bad = idx
        tests_set = {t for t, _ in tests_items}
    ctx = dict(gate_card=env.get('GATE_CARD', ''),
               tests=tests_set,
               tests_refs=[r for _, r in tests_items], tests_items=tests_items,
               ir_index=load_index(env.get('GATE_IR_INDEX', ''), 'ir'),
               ac_count=0, beh_count=0, exempted=0)
    paths = collect_specs(argv, env)
    findings = [finding('tests-index', 1, 'trace-index-malformed',
                        '测试索引行格式应为 TEST-n TASK:AC-n，实为 %r' % b, '改这一行') for b in tests_bad]
    for p in paths:
        try:
            with open(p, encoding='utf-8') as f:
                text = f.read()
        except OSError as e:
            raise gc.ContractError('spec 读不了（infra）: %s' % e)
        findings += lint_spec(p, text, th, ctx)
    escalate = any(f.get('escalate') for f in findings)
    public = [{k: v for k, v in f.items() if k != 'escalate'} for f in findings]
    code = gc.EXIT_FAIL_ESCALATE if escalate else (gc.EXIT_FAIL_FIXABLE if findings else gc.EXIT_PASS)
    report = {'gate': 'g010-spec-schema', 'severity': 'block', 'ownerRole': 'spec-author',
              'status': 'fail' if findings else 'pass',
              'metrics': {'specs': len(paths), 'acCount': ctx['ac_count'], 'behCount': ctx['beh_count'],
                          'violationCount': len(public), 'exemptedVague': ctx['exempted'], 'exitCode': code},
              'ratchetKeys': [], 'violations': public, 'durationMs': int((time.time() - t0) * 1000)}
    emit(report, findings, code, env.get('GATE_REPORT_OUT', ''))
    return code


def emit(report, findings, code, out_path):
    """落盘 + schema 校验（不过 schema 即 exit 3，ADR 决策 2）+ 人类摘要（<=20 行）。"""
    out = out_path or os.path.join(gc.repo_root(), 'quality', 'reports', report['gate'] + '.json')
    os.makedirs(os.path.dirname(out), exist_ok=True)
    with open(out, 'w', encoding='utf-8') as f:
        json.dump(report, f, ensure_ascii=False, indent=2)
        f.write('\n')
    with open(gc.gate_report_schema_path(), encoding='utf-8') as f:
        errs = gc.validate_schema(report, json.load(f))
    if errs:
        print('infra: gate-report 不过 schema 校验：%s' % '; '.join(errs), file=sys.stderr)
        sys.exit(gc.EXIT_INFRA)
    label = {0: 'pass', 1: 'fail-fixable', 2: 'fail-escalate', 3: 'infra-error'}[code]
    print('g010-spec-schema: %s（%d violation，report=%s）' % (label, len(findings), out))
    for f in findings[:19]:
        print('  - %s:%s %s %s' % (f.get('file', '?'), f.get('line', '?'), f.get('rule', ''), f.get('message', '')))


def main(argv):
    try:
        return run(argv)
    except gc.ContractError as e:
        print('infra-error: %s' % e, file=sys.stderr)
        return gc.EXIT_INFRA


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
