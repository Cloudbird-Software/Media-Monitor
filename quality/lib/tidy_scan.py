# -*- coding: utf-8 -*-
"""tidy_scan.py —— W5-C1 整洁关卡共享扫描器（.github#224 / ADR-0070）。

署名（宪法 §9#5 署名规则，引用条款旁注明源头）：
  - aider repo-map（github.com/paul-gauthier/aider）：符号索引 + 按引用度排序
    装配、token 预算封顶的 repo-map 方法——本仓为其确定性简化实现；
  - CodeScene（codescene.com）因素清单：嵌套深度/布尔组合/循环结构作为
    复杂度主因素的启发来源（cognitive 打分见 complexity_lint.py）；
  - 判定引擎不自研（宪法 §9 原则）：结构架构判定用 dependency-cruiser 结果。

职责（arch/nav/complexity/ratchet 四组关卡 lib 复用，零网络零新依赖）：
  1. 契约子树读取：GATE_TH_<段名>_<键> 叶子级覆盖——gate_common._env_name 对
     连字符段名（g020-arch）拼出的 env 名 bash 无法注入，这里把段/键的连字符
     归一为下划线，统一注入口径；
  2. 扫描面枚举：scan-paths 前缀集 − ignore-paths 前缀集；
  3. 符号宇宙（"AST-lite"正则提取）：TS 导出/导入、py 顶层 def/class、
     sh 函数、make 目标、npm scripts——repo-map 与文档验真的符号来源；
  4. 文件级导入图（相对导入解析到文件）——引用跳数/环数。
局限声明：AST-lite 只覆盖本仓语法子集，覆盖不了的语法会漏报，所以它只承担
辅助度量/lint，阻断性架构判定一律交给 depcruise（不拿自研正则当安全网）。
"""
import os
import re
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import gate_common as gc  # 同目录导入（import 位置在 sys.path 修改之后，属预期）

SKIP_DIRS = {'node_modules', 'dist', 'coverage', '__pycache__', '.git'}


class ScanError(Exception):
    """扫描面基础设施故障（关卡层折算 exit 3）。"""


# ---------- 契约子树 + GATE_TH_ 覆盖 ----------

def load_section(section):
    node = gc.contract_get(gc.load_contract(), section)
    return _override(node, section.replace('-', '_').upper())


def _override(node, prefix):
    if isinstance(node, dict):
        return {k: _override(v, prefix + '_' + str(k).replace('-', '_').upper())
                for k, v in node.items()}
    env = os.environ.get('GATE_TH_' + prefix)
    if env is not None:
        if isinstance(node, list):
            return [gc._scalar(p, 'env GATE_TH_' + prefix) for p in env.split()]
        return gc._scalar(env, 'env GATE_TH_' + prefix)
    return node


# ---------- 扫描面枚举 ----------

def split_paths(value):
    return [p.strip().rstrip('/') for p in (value or '').split('|') if p.strip()]


def _hit(rel, prefixes):
    return any(rel == p or rel.startswith(p + '/') for p in prefixes)


def list_files(root, scan_paths, ignore_paths=''):
    """枚举扫描面文件 → 相对路径列表（POSIX 分隔、排序稳定——ratchet 可复现）。"""
    ig = split_paths(ignore_paths)
    out = []
    for base in split_paths(scan_paths):
        top = os.path.normpath(os.path.join(root, base))
        if not os.path.isdir(top):
            continue
        for dirpath, dirnames, filenames in os.walk(top):
            dirnames[:] = sorted(d for d in dirnames if d not in SKIP_DIRS and not _hit(
                os.path.relpath(os.path.join(dirpath, d), root).replace('\\', '/'), ig))
            for fn in sorted(filenames):
                rel = os.path.relpath(os.path.join(dirpath, fn), root).replace('\\', '/')
                if not _hit(rel, ig):
                    out.append(rel)
    return sorted(out)


_CACHE = {}


def read(root, rel):
    if rel not in _CACHE:
        try:
            with open(os.path.join(root, rel), encoding='utf-8', errors='replace') as f:
                _CACHE[rel] = f.read()
        except OSError as e:
            raise ScanError('文件读不了（infra）%s: %s' % (rel, e))
    return _CACHE[rel]


def code_lines(text):
    """代码行数（非空非注释行）——depthRatio 分母/分子用。"""
    n = 0
    for line in text.splitlines():
        s = line.strip()
        if s and not s.startswith('//') and not s.startswith('*') and not s.startswith('/*'):
            n += 1
    return n


# ---------- 符号宇宙（AST-lite）----------

_IDENT = r'[A-Za-z_$][\w$]*'
RE_EXPORT_DECL = re.compile(r'\bexport\s+(?:default\s+)?(?:abstract\s+)?(?:async\s+)?'
                            r'(?:function\s*\*?|class|enum|const|let|var|type|interface)\s+(' + _IDENT + r')')
RE_EXPORT_BRACE = re.compile(r'\bexport\s*\{([^}]*)\}')
RE_SPEC_FROM = re.compile(r'''\b(?:import|export)\s[^;'"\n]*?\bfrom\s*["']([^"']+)["']''')
RE_BARE_IMPORT = re.compile(r'''\b(?:import\s*\(\s*|require\s*\(\s*|import\s+)["']([^"']+)["']''')
RE_PY_DEF = re.compile(r'^(?:def|class)\s+(' + _IDENT + r')', re.M)
RE_SH_FN = re.compile(r'^(' + _IDENT + r')\s*\(\)\s*\{', re.M)
RE_MAKE_TARGET = re.compile(r'^([A-Za-z][\w.%-]*):(?!=)', re.M)


def ts_exports(text):
    """TS/JS 导出符号 [(name, line)]。"""
    out = []
    for i, line in enumerate(text.splitlines(), 1):
        for m in RE_EXPORT_DECL.finditer(line):
            out.append((m.group(1), i))
        for m in RE_EXPORT_BRACE.finditer(line):
            for item in m.group(1).split(','):
                name = item.split(' as ')[-1].strip()
                if re.fullmatch(_IDENT, name):
                    out.append((name, i))
    return out


def ts_imports(text):
    """导入说明符 [(specifier, line)]（import/export-from/import()/require）。"""
    out = []
    for i, line in enumerate(text.splitlines(), 1):
        for m in RE_SPEC_FROM.finditer(line):
            out.append((m.group(1), i))
        for m in RE_BARE_IMPORT.finditer(line):
            out.append((m.group(1), i))  # 动态 import()/require()/副作用 import "x"
    return out


def make_symbols(root, files):
    """符号宇宙 {name: {files:set, kind:str}}——TS 导出 + py 顶层 def/class + sh 函数。"""
    sym = {}

    def add(name, rel, kind):
        ent = sym.setdefault(name, {'files': set(), 'kind': kind})
        ent['files'].add(rel)
        if kind == 'export':
            ent['kind'] = 'export'

    for rel in files:
        text = read(root, rel)
        if rel.endswith(('.ts', '.tsx', '.js', '.cjs', '.mjs')):
            for name, _line in ts_exports(text):
                add(name, rel, 'export')
        elif rel.endswith('.py'):
            for m in RE_PY_DEF.finditer(text):
                add(m.group(1), rel, 'pydef')
        elif rel.endswith('.sh'):
            for m in RE_SH_FN.finditer(text):
                add(m.group(1), rel, 'shfn')
    return sym


def ref_counts(root, files, sym):
    """每个符号被“其它文件”引用的文件数（repo-map in-degree——aider 的 PageRank
    在本仓简化为跨文件引用计数：确定性、可离线复现）。"""
    pats = {name: re.compile(r'\b%s\b' % re.escape(name)) for name in sym}
    hits = {name: set() for name in sym}
    occ = {name: 0 for name in sym}
    for rel in files:
        text = read(root, rel)
        for name, ent in sym.items():
            if rel in ent['files']:
                continue
            n = len(pats[name].findall(text))
            if n:
                hits[name].add(rel)
                occ[name] += n
    return {name: len(v) for name, v in hits.items()}, occ


def import_graph(root, files):
    """文件级导入图 {file: set(file)}（相对导入解析；包内相对路径尝试后缀/目录 index）。"""
    fs = set(files)
    graph = {f: set() for f in files}
    for rel in files:
        if not rel.endswith(('.ts', '.tsx', '.js', '.cjs', '.mjs')):
            continue
        for spec, _line in ts_imports(read(root, rel)):
            if not spec.startswith('.'):
                continue  # 裸说明符=外部包/Node 内建，不入仓内图
            base = os.path.normpath(os.path.join(os.path.dirname(rel), spec)).replace('\\', '/')
            # Node ESM 习惯写 .js 后缀指向 .ts 源——两种后缀都尝试解析
            cands = [base, re.sub(r'\.js$', '.ts', base)]
            cands += [base + '.ts', base + '.js', base + '.tsx',
                      base + '/index.ts', base + '/index.js']
            for cand in cands:
                if cand in fs and cand != rel:
                    graph[rel].add(cand)
                    break
    return graph


# ---------- git 上下文（PR 范围；无 GATE_BASE 时调用方自行降级为本地模式）----------

def git(root, *args):
    p = subprocess.run(['git', '-C', root] + list(args), capture_output=True, text=True)
    if p.returncode != 0:
        raise ScanError('git %s 失败: %s' % (' '.join(args[:2]), p.stderr.strip()[:200]))
    return p.stdout


def diff_files(root, base, head='HEAD'):
    return [l for l in git(root, 'diff', '--name-only', '%s...%s' % (base, head)).splitlines() if l]


def diff_added_lines(root, base, head, paths):
    """范围内指定前缀下“新增的行”（逐行内容）——抑制标记零增长/新导出符号用。"""
    out = []
    for rel in diff_files(root, base, head):
        if not any(rel == p or rel.startswith(p + '/') for p in paths):
            continue
        text = git(root, 'diff', '-U0', '%s...%s' % (base, head), '--', rel)
        out.extend((rel, l[1:]) for l in text.splitlines() if l.startswith('+') and not l.startswith('+++'))
    return out
