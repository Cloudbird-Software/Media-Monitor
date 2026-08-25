# -*- coding: utf-8 -*-
"""gate_common.py —— quality 关卡公共库（ADR-0060 决策 1/2）。

为什么自研而不用 PyYAML/jsonschema：AGENTS.md 硬规则 3（新依赖须人批）+
宪法 §9 原则（公共知识已有的判定轮子不自研，但本仓只解析自己定义的语法子集，
不需要通用 YAML 引擎）。解析不了的语法一律抛错（fail-closed），绝不静默跳过。

三个职责：
  1. contract.yaml 加载 + 点路径取值 + GATE_TH_* 环境变量覆盖（阈值唯一来源的读取面）
  2. gate-report 的最小 JSON Schema 校验器（#127 §3.2 的子集：type/required/
     properties/enum/items/additionalProperties/minimum）
  3. 关卡报告落盘 + exit 码语义（IFACE-04：0=过 1=fail-fixable 2=fail-escalate 3=infra-error）
"""
import json
import os
import re
import sys

EXIT_PASS, EXIT_FAIL_FIXABLE, EXIT_FAIL_ESCALATE, EXIT_INFRA = 0, 1, 2, 3


class ContractError(Exception):
    """contract.yaml 读不了/键缺失/语法超出子集——属基础设施故障（exit 3）。"""


# ---------- YAML 语法子集解析（本文件只解析 contract.yaml 与 spec frontmatter） ----------

def _tokens(text):
    out = []
    for n, raw in enumerate(text.splitlines(), 1):
        line = raw.rstrip('\n')
        if not line.strip() or line.lstrip().startswith('#'):
            continue  # 只支持整行注释；行内 # 会按裸标量报错，防止静默吞值
        indent = len(line) - len(line.lstrip(' '))
        if indent % 2:
            raise ContractError('contract 第 %d 行缩进不是 2 的倍数: %r' % (n, line))
        out.append((indent, line.strip(), n))
    return out


def _split_top(s, sep):
    """按分隔符切分，引号内与嵌套 []/{} 内的分隔符不算（供行内 [..]/{..} 用）。"""
    parts, buf, q, depth = [], '', '', 0
    for ch in s:
        if q:
            if ch == q:
                q = ''
            buf += ch
            continue
        if ch in ('"', "'"):
            q = ch
        elif ch in '[{':
            depth += 1
        elif ch in ']}':
            depth -= 1
        elif ch == sep and depth == 0:
            parts.append(buf.strip())
            buf = ''
            continue
        buf += ch
    if buf.strip():
        parts.append(buf.strip())
    return parts


def _scalar(s, where):
    if len(s) >= 2 and s[0] == s[-1] and s[0] in ('"', "'"):
        # 引号标量：本子集不做双引号转义展开（prettier 会把 YAML 引号统一成双引号，
        # 所以两种都收；值里出现引号本身属于超子集，让 regex/键定义避开即可）。
        return s[1:-1]
    if re.fullmatch(r'-?[0-9]+', s):
        return int(s)
    if re.fullmatch(r'-?[0-9]+\.[0-9]+', s):
        return float(s)
    if s in ('true', 'false'):
        return s == 'true'
    if s == '{}':
        return {}
    if s == '[]':
        return []
    if s == 'null':
        return None
    if '#' in s:
        raise ContractError('%s 行内注释不支持（# 会被当值的一部分）: %r' % (where, s))
    return s


def _inline(s, where):
    if s.startswith('[') and s.endswith(']'):
        return [_scalar(p, where) for p in _split_top(s[1:-1], ',')]
    if s.startswith('{') and s.endswith('}'):
        out = {}
        for pair in _split_top(s[1:-1], ','):
            k, _, v = pair.partition(':')
            if not _ or not k.strip():
                raise ContractError('%s 行内映射缺少冒号: %r' % (where, pair))
            out[k.strip()] = _inline(v.strip(), where) if v.strip().startswith(('[', '{')) else _scalar(v.strip(), where)
        return out
    return _scalar(s, where)


def _block(toks, i, indent):
    """解析 indent 层的映射或列表，返回 (值, 下一 token 下标)。"""
    if i < len(toks) and toks[i][0] == indent and toks[i][1].startswith('- '):
        out = []
        while i < len(toks) and toks[i][0] == indent and toks[i][1].startswith('- '):
            _, item, n = toks[i]
            item = item[2:].strip()
            if not item:  # "- " 后空：子块整体为该项
                val, i = _block(toks, i + 1, indent + 2)
                out.append(val)
                continue
            head, colon, rest = item.partition(':')
            if colon and rest.strip():  # 列表项是映射首键：- { k: v, ... } 或 - k: v
                cur = {}
                if rest.strip().startswith('{'):
                    cur = _inline(rest.strip(), '第 %d 行' % n)
                else:
                    cur[head.strip()] = _inline(rest.strip(), '第 %d 行' % n)
                    while i + 1 < len(toks) and toks[i + 1][0] > indent and not toks[i + 1][1].startswith('- '):
                        i += 1
                        k2, c2, r2 = toks[i][1].partition(':')
                        if not c2:
                            raise ContractError('第 %d 行续行缺少冒号: %r' % (toks[i][2], toks[i][1]))
                        cur[k2.strip()] = _inline(r2.strip(), '第 %d 行' % toks[i][2])
                out.append(cur)
            else:
                out.append(_inline(item, '第 %d 行' % n))
            i += 1
        return out, i
    out = {}
    while i < len(toks) and toks[i][0] == indent and not toks[i][1].startswith('- '):
        _, line, n = toks[i]
        key, colon, rest = line.partition(':')
        if not colon or not key.strip():
            raise ContractError('第 %d 行缺少 key: 结构: %r' % (n, line))
        key = key.strip()
        if key in out:
            raise ContractError('第 %d 行重复键 %r（重复键=潜在篡改，fail-closed）' % (n, key))
        rest = rest.strip()
        if rest:
            out[key] = _inline(rest, '第 %d 行' % n)
            i += 1
        elif i + 1 < len(toks) and toks[i + 1][0] > indent:
            out[key], i = _block(toks, i + 1, toks[i + 1][0])
        else:
            out[key] = None
            i += 1
    return out, i


def load_yaml(text):
    toks = _tokens(text)
    if not toks or toks[0][0] != 0:
        raise ContractError('contract 为空或顶层缩进不为 0')
    val, _ = _block(toks, 0, 0)
    return val


# ---------- contract 取值 + GATE_TH_* 覆盖（AC-1：阈值唯一来源的唯一读取面） ----------

def _env_name(dotted):
    parts = dotted.split('.')
    if parts and parts[0] == 'thresholds':
        parts = parts[1:]  # 覆盖名不带 thresholds 前缀：GATE_TH_G010_SPEC_MIN_AC_CLAUSES
    return 'GATE_TH_' + '_'.join(p.upper() for p in parts)


def contract_get(root, dotted):
    env = os.environ.get(_env_name(dotted))
    if env is not None:  # 测试注入通道：证明脚本读的是 contract/环境，不是硬编码
        return _scalar(env, '环境变量 ' + _env_name(dotted))
    cur = root
    for part in dotted.split('.'):
        if not isinstance(cur, dict) or part not in cur:
            raise ContractError('contract 缺键 %s（fail-closed：阈值不落脚本，缺键=配错）' % dotted)
        cur = cur[part]
    return cur


def apply_env_overrides(node, prefix=''):
    """对阈值子树逐叶子应用 GATE_TH_* 覆盖（qc_get 的子树版）。
    为什么需要：关卡一次取整棵 thresholds.<gate> 子树，若只在 qc_get 点路径上
    生效，叶级注入会静默失效（注入测试假阴）——所以取子树后必须再走一遍覆盖。"""
    if isinstance(node, dict):
        return {k: apply_env_overrides(v, '%s.%s' % (prefix, k) if prefix else k)
                for k, v in node.items()}
    if prefix:
        env = os.environ.get(_env_name(prefix))
        if env is not None:
            if isinstance(node, list):
                return [_scalar(p, '环境变量 ' + _env_name(prefix)) for p in env.split()]
            return _scalar(env, '环境变量 ' + _env_name(prefix))
    return node


def load_contract(path=None):
    path = path or os.environ.get('GATE_CONTRACT') or os.path.join(repo_root(), 'quality', 'contract.yaml')
    try:
        with open(path, encoding='utf-8') as f:
            return load_yaml(f.read())
    except OSError as e:
        raise ContractError('contract.yaml 读不了（%s）: %s' % (path, e))


def repo_root():
    return os.path.abspath(os.path.join(os.path.dirname(__file__), '..', '..'))


# ---------- 最小 JSON Schema 校验器（#127 §3.2 用到的关键字子集） ----------

def validate_schema(inst, sch, path='$'):
    errs = []
    t = sch.get('type')
    if t == 'integer':
        ok = isinstance(inst, int) and not isinstance(inst, bool)
    elif t == 'number':
        ok = isinstance(inst, (int, float)) and not isinstance(inst, bool)
    elif t == 'object':
        ok = isinstance(inst, dict)
    elif t == 'array':
        ok = isinstance(inst, list)
    elif t == 'string':
        ok = isinstance(inst, str)
    elif t == 'boolean':
        ok = isinstance(inst, bool)
    elif t is None:
        ok = True
    else:
        return ['%s: schema 用了未支持类型 %r（先扩 gate_common.validate_schema）' % (path, t)]
    if not ok:
        return ['%s: 期望 %s，实际 %s' % (path, t, type(inst).__name__)]
    if 'enum' in sch and inst not in sch['enum']:
        errs.append('%s: %r 不在 enum %r 内' % (path, inst, sch['enum']))
    if 'minimum' in sch and isinstance(inst, (int, float)) and inst < sch['minimum']:
        errs.append('%s: %s < minimum %s' % (path, inst, sch['minimum']))
    if isinstance(inst, dict):
        for k in sch.get('required', []):
            if k not in inst:
                errs.append('%s: 缺 required 键 %r' % (path, k))
        props = sch.get('properties', {})
        for k, v in inst.items():
            if k in props:
                errs += validate_schema(v, props[k], '%s.%s' % (path, k))
            elif 'additionalProperties' in sch and isinstance(sch['additionalProperties'], dict):
                errs += validate_schema(v, sch['additionalProperties'], '%s.%s' % (path, k))
    if isinstance(inst, list) and 'items' in sch:
        for n, v in enumerate(inst):
            errs += validate_schema(v, sch['items'], '%s[%d]' % (path, n))
    return errs


def gate_report_schema_path():
    return os.path.join(os.path.dirname(__file__), '..', 'schema', 'gate-report.schema.json')


# ---------- bash 侧 CLI：qc_get 走这里（python gate_common.py get <dotted.key>） ----------

def _main(argv):
    try:
        return _dispatch(argv)
    except ContractError as e:  # 一行干净报错 + exit 3：contract 属基础设施（infra-error）
        print('contract-error: %s' % e, file=sys.stderr)
        return EXIT_INFRA


def _dispatch(argv):
    if len(argv) >= 2 and argv[1] == 'get':
        val = contract_get(load_contract(), argv[2])
        if isinstance(val, list):
            print(' '.join(str(v) for v in val))
        elif isinstance(val, bool):
            print('true' if val else 'false')
        else:
            print(val)
        return 0
    if len(argv) >= 3 and argv[1] == 'validate-report':
        with open(argv[2], encoding='utf-8') as f:
            rep = json.load(f)
        with open(argv[3] if len(argv) > 3 else gate_report_schema_path(), encoding='utf-8') as f:
            sch = json.load(f)
        errs = validate_schema(rep, sch)
        for e in errs:
            print('SCHEMA-ERROR %s' % e, file=sys.stderr)
        return 1 if errs else 0
    print('用法: gate_common.py get <dotted.key> | validate-report <report.json> [schema.json]', file=sys.stderr)
    return 2


if __name__ == '__main__':
    sys.exit(_main(sys.argv))
