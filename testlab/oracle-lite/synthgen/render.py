"""render：把 JSONL 实体数据按「端点 + 分页游标」组装成契约形态的响应页（阶段 3 回放 / 阶段 5 评分对接）。

游标形态对齐契约实证（DRAFT_SUMMARY §分页游标本批汇总）：
- douyin general_search_stream：首屏 offset=0；响应 cursor=<下页offset>, has_more=1, extra.logid=<34位hex>
- douyin general_search_single：请求 search_id=<上页 extra.logid>、offset 递增 10；响应 cursor 数值
- xhs    so /api/sns/web/v2/search/notes：请求 body（keyword/page/page_size/search_id/...12 必填项）；响应 data.has_more
- xhs    comment_page：请求 cursor=""（首屏）；响应 data.cursor=<24hex>
- ks     /rest/v/search/feed：请求 body pcursor=""（首屏）；响应 pcursor="1"→"2" 递增 + searchSessionId=<60位>
抖音响应外层信封按契约必填（rate=1.0）全量补齐（status_code/log_pb/.../time_cost，P2-3 修复）。

CLI：
  python synthgen/render.py --dataset synthgen/datasets/douyin --endpoint search --page 2 --page-size 10
"""
from __future__ import annotations

import argparse
import json
import re
import sys
import time
from pathlib import Path

import numpy as np

_PKG_PARENT = str(Path(__file__).resolve().parent.parent)
if _PKG_PARENT not in sys.path:
    sys.path.insert(0, _PKG_PARENT)

from synthgen import commentext
from synthgen import ids

ENDPOINTS = {
    "douyin": ["search_stream", "search_single", "search_item", "aweme_detail",
               "comment_list", "comment_list_reply"],
    "xhs": ["search_notes", "comment_page"],
    "kuaishou": ["search_feed"],
}


def _stable_hash(s: str) -> int:
    import hashlib
    return int.from_bytes(hashlib.blake2b(s.encode("utf-8"), digest_size=8).digest(), "big")


class DatasetReader:
    """按行号随机访问 JSONL 实体（可选用 index.db 加速过滤定位）。"""

    def __init__(self, site_dir: Path):
        self.site_dir = Path(site_dir)
        manifest = json.loads((self.site_dir / "MANIFEST.json").read_text(encoding="utf-8"))
        self.manifest = manifest
        self.entity = manifest["entity"]
        self.jsonl = self.site_dir / f"{self.entity}.jsonl"
        self._f = None
        self._line_offsets = None
        self._seed = int(manifest["seed"])
        self.anchor_ts = self._parse_anchor(manifest.get("anchor_time"))

    @staticmethod
    def _parse_anchor(val) -> int | None:
        """数据集生成锚点（MANIFEST.anchor_time）→ epoch 秒。

        红队 R4-P2-1：dy 评论 create_time 的「现在」基准改用该锚——数据集内所有
        实体都发布于锚点之前，评论时间落在 [发布, min(发布+30d, 锚点)] 内既满足
        语料时间序公理，又与墙钟彻底解耦（跨请求/跨进程渲染完全一致）。
        """
        if not val:
            return None
        try:
            import datetime
            dt = datetime.datetime.fromisoformat(str(val))
            return int(dt.timestamp())
        except Exception:
            return None

    def _offsets(self):
        if self._line_offsets is None:
            offs = [0]
            with open(self.jsonl, "rb") as f:
                for line in f:
                    offs.append(offs[-1] + len(line))
            self._line_offsets = offs[:-1] if offs[-1] == 0 else offs[:-1]
        return self._line_offsets

    def __len__(self):
        self._offsets()
        return len(self._line_offsets)

    def read(self, line_no: int) -> dict:
        offs = self._offsets()
        if line_no < 0 or line_no >= len(offs):
            raise IndexError(f"line_no {line_no} out of range (0..{len(offs)-1})")
        with open(self.jsonl, "r", encoding="utf-8") as f:
            f.seek(offs[line_no])
            return json.loads(f.readline())

    def page_slice(self, page_no: int, page_size: int) -> list[dict]:
        start = (page_no - 1) * page_size
        return [self.read(i) for i in range(start, min(start + page_size, len(self)))]

    def abs_slice(self, start: int, count: int) -> list[dict]:
        """绝对下标切片（红队 R2-P2-1：offset 语义与 count 解耦——同一 offset 换 count
        必须命中同一条记录，真站 offset 是结果集内绝对位置）。"""
        return [self.read(i) for i in range(max(0, start), min(start + count, len(self)))]


def _page_rng(dataset: DatasetReader, salt: str):
    return np.random.default_rng([dataset._seed, 777, _stable_hash(salt) % (2**31)])


def _now_ms() -> int:
    """extra.now / data.time 用「当前时间」毫秒（红队 P3-2：真站为请求时点，合成原为随机时刻）。"""
    return int(time.time() * 1000)


# ---------------------------------------------------------------------------
# 红队 round6A R6A-P2-1：搜索相关性（关键词→类目映射 + 类目偏置采样）
#
# 差距：合成搜索结果与关键词零耦合（严格命中 0.8-3.3% vs 语料 17.7-47.1%，
# 「咖啡拉花」返回穿搭/搞笑/萌宠均匀混合）——关键词仅决定轮转窗口起点。
# 方案（红队 E.2）：关键词池本身带类目标签（data/*.json topics/hashtags 按类目
# 组织），关键词 → 命中类目集合；搜索窗口按「严格命中池 + 命中类目池 + 背景
# 跨类目池」三池配比采样，严格命中率落进语料区间；无映射词（乱码词等）退化
# 为背景混合（对齐真站模糊匹配，乱码词 24/24 出结果的 R5C 口径保持）。
# 确定性：纯 (site, keyword, 绝对位置) 函数——同一关键词任意页码/翻页路径
# 返回同一结果序列；三池互不相交 + 池内单调推进 ⇒ 页内/跨页零重复。
# ---------------------------------------------------------------------------
_RELEVANCE_MIX = {   # 每 20 卡的三池配比（语料严格命中 dy 47.1/xhs 17.7/ks 37.4）
    "douyin": (6, 7, 7),     # 30% 严格命中
    "xhs": (4, 8, 8),        # 20%
    "kuaishou": (5, 8, 7),   # 25%
}
_SITE_SCAN_CACHE: dict = {}   # site_dir -> (categories[list], texts[list])
_KW_VIEW_CACHE: dict = {}     # (site_dir, keyword) -> _KeywordView / None
_RELEVANCE_MIN_TOKEN = 2      # CJK 子串匹配最小长度（单字不判相关）


class _KeywordView:
    """单关键词的三池视图（严格命中 / 命中类目 / 背景）。"""

    __slots__ = ("cats", "hits", "cat_lines", "bg_lines", "weak")

    def __init__(self, cats, hits, cat_lines, bg_lines, weak=False):
        self.cats = cats
        self.hits = hits
        self.cat_lines = cat_lines
        self.bg_lines = bg_lines
        self.weak = weak


def _has_cjk(s: str) -> bool:
    return any("\u4e00" <= ch <= "\u9fff" for ch in (s or ""))


def _kw_categories(site: str, keyword: str) -> list:
    """关键词 → 命中类目（data/*.json 池带类目标签；确定性、无副作用）。

    匹配规则（分高者胜）：类目名精确 = 3；池内词条精确 = 2；包含关系
    （类目名/词条 ⊆ 关键词或反之，≥2 字）= 1。词条含生成侧扩容注入的
    关键词供给词（pools._KEYWORD_TOPICS，如「咖啡拉花→美食探店」）。
    """
    kw = (keyword or "").strip()
    if len(kw) < _RELEVANCE_MIN_TOKEN:
        return []
    from synthgen.config import load_site_pools
    from synthgen.pools import _KEYWORD_TOPICS
    pools = load_site_pools(site)
    topics = pools.get("topics") or {}
    tags = pools.get("hashtags") or pools.get("tags_pool") or {}
    scored: dict[str, int] = {}
    for cat in topics.keys() | tags.keys():
        score = 0
        if kw == cat:
            score = 3
        else:
            tlist = list(topics.get(cat, [])) + list(tags.get(cat, [])) \
                + list(_KEYWORD_TOPICS.get(cat, []))
            if kw in tlist:
                score = 2
            else:
                if len(cat) >= _RELEVANCE_MIN_TOKEN and (cat in kw or kw in cat):
                    score = 1
                for t in tlist:
                    if len(t) >= _RELEVANCE_MIN_TOKEN and (t in kw or kw in t):
                        score = max(score, 1)
        if score:
            scored[cat] = score
    if not scored:
        return []
    top = max(scored.values())
    cats = [c for c, s in scored.items() if s == top]
    return sorted(cats)


def _index_rows(idx: Path):
    """读 index.db 的 (line_no, record_id, category) 全表；不可读/损坏返回 None。"""
    import sqlite3
    try:
        conn = sqlite3.connect("file:%s?mode=ro" % idx, uri=True)
        rows = conn.execute("SELECT line_no, record_id, category FROM records "
                            "ORDER BY line_no").fetchall()
        conn.close()
    except Exception:
        return None
    return rows


def _site_categories(dataset: DatasetReader) -> list | None:
    """全库类目数组（line_no 序）：优先 index.db（生成器 --with-index 落盘），
    缺失时返回 None（调用方退化为 hashtag 反查 / 背景模式）。

    R7A 数据线加固：index 与 JSONL 内容校验（抽样 4 行 record_id 对账）——
    数据集换代时 index.db 可能与 JSONL 失配（8090 栈进程持有旧 index 只读
    句柄导致文件不可替换，需重启后换位）；失配时依次尝试伴生 index.db.new
    （收尾换位前的落位名），仍失配才退化为 hashtag 反查。保证换代窗口期
    render（服务/precompute 共用）与重启后使用同一份类目视图。
    """
    n = len(dataset)
    for name in ("index.db", "index.db.new"):
        idx = dataset.site_dir / name
        if not idx.exists():
            continue
        rows = _index_rows(idx)
        if not rows or len(rows) != n:
            continue
        ok = True
        for probe in (0, n // 3, 2 * n // 3, n - 1):
            try:
                if str(rows[probe][1]) != str(_record_identity(dataset, probe)):
                    ok = False
                    break
            except Exception:
                ok = False
                break
        if ok:
            return [c for _, _, c in rows]
    return None


def _record_identity(dataset: DatasetReader, line_no: int):
    rec = dataset.read(line_no)
    return rec.get("aweme_id") or rec.get("id") or (rec.get("photo") or {}).get("id")


def _site_scan(dataset: DatasetReader, site: str):
    """全库单遍扫描缓存：(类目数组, 标题数组)。首扫 ~10-20s，之后零成本。

    类目优先取 index.db；缺失时按站点标题/tag 结构反查池（raw 池 + 扩容池
    重建 tag→类目 映射）。标题数组用于关键词严格命中判定：
    dy=desc（含 #tag，语料样例「…#美食探店」即严格命中）、xhs=display_title、
    ks=caption（含 #tag）。
    """
    key = str(dataset.site_dir)
    if key in _SITE_SCAN_CACHE:
        return _SITE_SCAN_CACHE[key]
    cats = _site_categories(dataset)
    if cats is None:
        from synthgen.config import load_site_pools
        from synthgen.pools import _expand_tag_pool
        pools = load_site_pools(site)
        topics = pools.get("topics") or {}
        raw_tags = pools.get("hashtags") or pools.get("tags_pool") or {}
        rev: dict = {}
        for cat, lst in topics.items():
            rev.setdefault(cat, cat)
            for t in lst:
                rev.setdefault(t, cat)
        for cat, lst in raw_tags.items():
            for t in _expand_tag_pool(lst, topics.get(cat, []), cat):
                rev.setdefault(t, cat)
        cats = []
        texts = []
        with open(dataset.jsonl, "r", encoding="utf-8") as f:
            for line in f:
                rec = json.loads(line)
                texts.append(_search_text(site, rec))
                cats.append(_search_category(site, rec, rev))
        _SITE_SCAN_CACHE[key] = (cats, texts)
        return _SITE_SCAN_CACHE[key]
    texts: list = []
    with open(dataset.jsonl, "r", encoding="utf-8") as f:
        for line in f:
            texts.append(_search_text(site, json.loads(line)))
    _SITE_SCAN_CACHE[key] = (cats, texts)
    return _SITE_SCAN_CACHE[key]


def _search_text(site: str, rec: dict) -> str:
    """站点标题口径（严格命中判定用）。"""
    if site == "douyin":
        return rec.get("desc") or ""
    if site == "xhs":
        return (rec.get("note_card") or {}).get("display_title") or ""
    return (rec.get("photo") or {}).get("caption") or ""


def _search_category(site: str, rec: dict, rev: dict) -> str | None:
    """站点类目反查（index.db 缺失时的兜底路径）。"""
    if site == "douyin":
        te = rec.get("text_extra") or []
        tag = next((t.get("hashtag_name") for t in te
                    if isinstance(t, dict) and t.get("hashtag_name")), None)
        return rev.get(tag)
    if site == "xhs":
        return None   # xhs 无 tag 结构，兜底路径退化为纯背景
    tag = next((t.get("name") for t in (rec.get("tags") or [])
                if isinstance(t, dict) and t.get("name")), None)
    return rev.get(tag)


def _kw_view(dataset: DatasetReader, site: str, keyword: str):
    """关键词视图（None = 无映射，走背景混合）。

    R7A-P3-1 弱相关兜底：未映射的 CJK 常见词（不在 _KEYWORD_TOPICS、但标题池
    存在字面供给 ≥8 条）不再退化为纯背景——严格命中 0.0 vs 真站 17-47% 是
    R6A-P2-1 修复的池依赖残余。弱视图给 4/20（20% ≥ 语料下限 17.7%）的
    字面命中 + 背景混合；无字面供给或非 CJK（乱码词）保持纯背景（R5C 口径）。
    """
    kw = (keyword or "").strip()
    key = (str(dataset.site_dir), kw)
    if key in _KW_VIEW_CACHE:
        return _KW_VIEW_CACHE[key]
    view = None
    cats = _kw_categories(site, kw)
    if cats:
        cat_set = set(cats)
        cats_arr, texts = _site_scan(dataset, site)
        hits = [i for i, t in enumerate(texts) if kw in t]
        cat_lines = [i for i, c in enumerate(cats_arr)
                     if c in cat_set and not (kw in texts[i])]
        bg_lines = [i for i, c in enumerate(cats_arr) if c not in cat_set]
        # 命中供给不足时降级（关键词映射到类目但无字面命中 → 全走类目池，宽口径相关）
        view = _KeywordView(cats, hits, cat_lines, bg_lines)
    elif kw and _has_cjk(kw):
        cats_arr, texts = _site_scan(dataset, site)
        hits = [i for i, t in enumerate(texts) if kw in t]
        if len(hits) >= 8:
            hit_set = set(hits)
            bg_lines = [i for i in range(len(texts)) if i not in hit_set]
            view = _KeywordView([], hits, [], bg_lines, weak=True)
    _KW_VIEW_CACHE[key] = view
    return view


_WEAK_MIX = (4, 0, 16)   # 未映射 CJK 词的弱相关配比（20% 严格命中，≥ 语料下限 17.7%）


def _mix_layout(site: str, keyword: str, weak: bool = False) -> list:
    """20 位页面槽位布局（'h'/'c'/'b'），按 (site, keyword) 确定性洗牌。"""
    n_h, n_c, n_b = _WEAK_MIX if weak else _RELEVANCE_MIX[site]
    slots = ["h"] * n_h + ["c"] * n_c + ["b"] * n_b
    rng = np.random.default_rng([_stable_hash("mix::%s::%s" % (site, keyword)) % (2**31), 21])
    rng.shuffle(slots)
    return slots


def search_window_lines(dataset: DatasetReader, site: str, keyword: str | None,
                        start: int, count: int) -> list[int] | None:
    """R6A-P2-1 搜索数据窗口：关键词→类目偏置采样（公开接口，供
    precompute/离线核对复用同一选择逻辑）。

    返回 line_no 列表（长度 min(count, len(dataset))）；无映射关键词返回 None
    （调用方退化为背景混合 = 原 abs_slice 行为）。绝对位置 i 的槽位类型由
    (i mod 20) 布局决定，池内索引随 i 单调推进（O(1) 计算，分页稳定）。
    R7A-P3-1：未映射 CJK 常见词的弱视图走 _WEAK_MIX（4 命中 + 16 背景）。
    """
    view = _kw_view(dataset, site, keyword or "")
    if view is None:
        return None
    n = len(dataset)
    count = max(0, min(count, n - min(start, n)))
    if count <= 0:
        return []
    layout = _mix_layout(site, (keyword or "").strip(), weak=getattr(view, "weak", False))
    n_h = layout.count("h")
    n_c = layout.count("c")
    # 命中供给不足：命中槽位让渡给类目池（宽口径相关仍成立）；弱视图无类目池 → 背景
    if len(view.hits) < 24:
        layout = [(("c" if not getattr(view, "weak", False) else "b") if s == "h" else s)
                  for s in layout]
        n_h = 0
        n_c = layout.count("c")
    base_h = _stable_hash("pool-h::%s::%s" % (site, keyword)) % max(1, len(view.hits) or 1)
    base_c = _stable_hash("pool-c::%s::%s" % (site, keyword)) % max(1, len(view.cat_lines) or 1)
    base_b = _stable_hash("pool-b::%s::%s" % (site, keyword)) % max(1, len(view.bg_lines) or 1)
    out = []
    for i in range(max(0, start), max(0, start) + count):
        slot = layout[i % 20]
        page, off = divmod(i, 20)
        if slot == "h" and view.hits:
            idx = page * n_h + layout[:off].count("h")
            out.append(view.hits[(base_h + idx) % len(view.hits)])
        elif slot == "c" and view.cat_lines:
            idx = page * n_c + layout[:off].count("c")
            out.append(view.cat_lines[(base_c + idx) % len(view.cat_lines)])
        elif view.bg_lines:
            idx = page * (20 - n_h - n_c) + layout[:off].count("b")
            out.append(view.bg_lines[(base_b + idx) % len(view.bg_lines)])
        elif view.cat_lines:
            idx = page * n_c + layout[:off].count("c")
            out.append(view.cat_lines[(base_c + idx) % len(view.cat_lines)])
        else:
            out.append(i % n)
    return out


def _search_records(dataset: DatasetReader, site: str, keyword: str | None,
                    start: int | None, page_no: int, page_size: int) -> list[dict]:
    """搜索页记录选择：映射关键词走类目偏置采样，否则背景窗口切片。"""
    if keyword and (keyword or "").strip():
        lines = search_window_lines(dataset, site, keyword,
                                    page_no * page_size if start is None else start,
                                    page_size)
        if lines is not None:
            return [dataset.read(ln) for ln in lines]
    if start is not None:
        return dataset.abs_slice(start, page_size)
    return dataset.page_slice(page_no, page_size)


# ---------------------------------------------------------------------------
# 真值结构模板物化（保真修复轮，fidelity_report §3-S2）
#
# 差距：synth 渲染器字段树薄于录制真值（dy music.*/video.* 子树、detail 150 键
# 超集、comment user 69 键、ks manifest representation 族…，hit-path L2 仅
# 0.29-0.89）。逐键手补不可维护，改为「模板定结构」：
#   - sites/templates/*.json：extract_templates.py 按父相对率 ≥50% 从录制真值
#     提取的结构模板（含静态快照值，与红队 P2-D3「静态形态取自语料」先例一致）；
#   - graft：模板遍历，同路径的数据集记录值优先（实体空间保持合成），
#     must_vary 类叶子按「形态保持扰动」合成（digits/hex/b64/url 逐字符同字符集
#     重掷——形态与真值一致、内容不复 制真站 id/token/pii），静态值原样保留。
# ---------------------------------------------------------------------------
_TPL_DIR = Path(__file__).resolve().parent / "sites" / "templates"
_TPL_CACHE = None
_MV_RULES = None

_FORM_DIGITS = re.compile(r"^\d+$")
_FORM_HEX = re.compile(r"^(0x)?[0-9a-fA-F]{16,}$")
_FORM_URL = re.compile(r"^[a-z][a-z0-9+.-]*://", re.I)
_FORM_B64 = re.compile(r"^[A-Za-z0-9_\-+/=]{20,}={0,2}$")
_FORM_DT = re.compile(r"^\d{4}-\d{2}-\d{2}[ T]")


def _templates() -> dict:
    global _TPL_CACHE
    if _TPL_CACHE is None:
        _TPL_CACHE = {}
        if _TPL_DIR.is_dir():
            for p in sorted(_TPL_DIR.glob("*.json")):
                try:
                    d = json.loads(p.read_text(encoding="utf-8"))
                except Exception:
                    continue
                if isinstance(d, dict) and isinstance(d.get("template"), (dict, list)):
                    _TPL_CACHE[p.stem] = d["template"]
    return _TPL_CACHE


def _classify_path(path: str) -> str:
    """must_vary 分类（与 contracts/rules.json 同源规则，synthgen 侧独立加载）。"""
    global _MV_RULES
    if _MV_RULES is None:
        _MV_RULES = []
        try:
            rp = Path(__file__).resolve().parent.parent / "contracts" / "rules.json"
            rules = json.loads(rp.read_text(encoding="utf-8"))["must_vary_rules"]["ordered"]
            _MV_RULES = [(r["class"], re.compile(r["path_re"])) for r in rules]
        except Exception:
            pass
    for cls, rx in _MV_RULES:
        if rx.search(path):
            return cls
    return "snapshot"


def _jtype(v) -> str:
    if v is None:
        return "null"
    if isinstance(v, bool):
        return "bool"
    if isinstance(v, (int, float)):
        return "num"
    if isinstance(v, str):
        return "str"
    if isinstance(v, list):
        return "list"
    if isinstance(v, dict):
        return "dict"
    return "other"


def _scramble(text: str, rng: np.random.Generator) -> str:
    """形态保持扰动：数字→数字、字母→同大小写字母、其余原样（长度/字符集/形态不变）。"""
    out = []
    for ch in text:
        if "0" <= ch <= "9":
            out.append(str(int(rng.integers(0, 10))))
        elif "a" <= ch <= "z":
            out.append(chr(97 + int(rng.integers(0, 26))))
        elif "A" <= ch <= "Z":
            out.append(chr(65 + int(rng.integers(0, 26))))
        else:
            out.append(ch)
    return "".join(out)


def _scramble_url(url: str, rng: np.random.Generator) -> str:
    m = re.match(r"^([a-zA-Z][a-zA-Z0-9+.-]*://[^/?#]+)", url)
    if not m:
        return _scramble(url, rng)
    return m.group(1) + _scramble(url[len(m.group(1)):], rng)


_GENERIC_NAMES = ("爱吃白菜的猫", "山间的风", "半糖去冰", "北方小土豆", "晚风轻拾",
                  "一颗青梅", "跑步的树懒", "南方小满", "深海打盹", "拾光者")


def _synth_leaf(tv, cls: str, rng: np.random.Generator, rec: dict):
    """模板叶子 → 合成值（形态对齐模板值；静态快照值原样）。"""
    if isinstance(tv, bool) or tv is None:
        return tv
    if isinstance(tv, (int, float)):
        if cls == "timestamp" and tv >= 10**9:
            base = int((rec or {}).get("create_time") or _now_ms() // 1000)
            unit = 1000 if tv >= 10**12 else 1
            span = 86400 * 7 * unit
            return base * unit + int(rng.integers(-span, span))
        if cls == "count" and tv:
            v = int(tv * rng.uniform(0.5, 1.5))
            return float(v) if isinstance(tv, float) else v
        return tv
    if isinstance(tv, str):
        if _FORM_URL.match(tv):
            return _scramble_url(tv, rng)
        if _FORM_DT.match(tv):
            return time.strftime("%Y-%m-%d %H:%M:%S", time.gmtime(
                int((rec or {}).get("create_time") or time.time())))
        if cls == "pii":
            return ids.pick(rng, _GENERIC_NAMES)
        if cls in ("id", "token") or _FORM_HEX.match(tv) \
                or _FORM_B64.match(tv) or _FORM_DIGITS.match(tv):
            return _scramble(tv, rng)
        return tv  # 静态文本（枚举/模板文案）
    return tv


def _leaf_rng(seed: int, path: str, idx: int = 0) -> np.random.Generator:
    return np.random.default_rng([seed & 0x7FFFFFFF,
                                  _stable_hash(path) % (2 ** 31), idx])


def _share_binding(rec, rng, m):
    return _dy_share_info(rec).get(m.group(2))


def _music_title_binding(rec, rng, m):
    nick = (rec.get("author") or {}).get("nickname") or "作者"
    return "%s创作的原声" % nick


# 跨实体引用绑定（同路径记录值覆盖不了的回显/派生字段）
_BINDINGS = [
    (re.compile(r"(^|\.)share_info\.(?P<k>[A-Za-z0-9_]+)$"), _share_binding),
    (re.compile(r"(^|\.)music\.title$"), _music_title_binding),
    (re.compile(r"(^|\.)(aweme_id|note_id)$"),
     lambda rec, rng, m: str(rec.get("aweme_id") or rec.get("id") or "")),
]


def graft(tnode, bnode, rec: dict, seed: int, path: str = ""):
    """模板节点 → 物化值。

    tnode：结构模板叶子/容器；bnode：同路径的「语义优先值」（数据集记录或
    已构建评论——类型相容即整体采用，保证 e2e/ground_truth 的实体一致性）；
    rec：数据集实体（跨引用绑定与时间基准）；seed：实体级确定性种子
    （同一实体在任何端点渲染出完全相同的字段值，真站一致性口径）。
    """
    if isinstance(tnode, dict):
        b = bnode if isinstance(bnode, dict) else None
        out = {}
        for k, tv in tnode.items():
            cp = (path + "." + k) if path else k
            out[k] = _graft_leaf_or_node(tv, (b or {}).get(k), rec, seed, cp)
        return out
    if isinstance(tnode, list):
        if not tnode:
            return []  # 真值高频形态为空数组（如 reply 的 text_extra）→ 不带出 base 元素
        if isinstance(bnode, list):
            if isinstance(tnode[0], dict):
                # 对象数组（text_extra/adaptationSet/sub_comments…）：
                # 逐元素按模板物化（补齐 representation.colorInfo 等结构），基数尊重 base
                return [_graft_leaf_or_node(tnode[0],
                                            bel if isinstance(bel, dict) else None,
                                            rec, seed, "%s[]#%d" % (path, i))
                        for i, bel in enumerate(bnode[:8])]
            return list(bnode)  # 标量数组（url_list 等）：base 值原样
        return [_graft_leaf_or_node(tnode[0], None, rec, seed, "%s[]#%d" % (path, i))
                for i in range(min(len(tnode), 8))]
    return _graft_leaf_or_node(tnode, bnode, rec, seed, path)


def _graft_leaf_or_node(tv, bv, rec, seed, path):
    if isinstance(tv, (dict, list)):
        return graft(tv, bv, rec, seed, path)
    cls = _classify_path(path)
    for rx, fn in _BINDINGS:
        m = rx.search(path)
        if m:
            v = fn(rec, _leaf_rng(seed, path), m)
            if v is not None:
                return v
    if bv is not None and _jtype(bv) == _jtype(tv):
        return bv
    return _synth_leaf(tv, cls, _leaf_rng(seed, path), rec)


def materialize(rec: dict, family: str, base=None) -> dict:
    """按真值模板物化一个实体/评论对象（模板缺失时返回 base 兜底）。"""
    tpl = _templates().get(family)
    if tpl is None or not isinstance(tpl, (dict, list)):
        return base if base is not None else dict(rec)
    key = (rec.get("aweme_id") or rec.get("id") or rec.get("cid")
           or (rec.get("photo") or {}).get("id")
           or (base or {}).get("id") or "")
    seed = _stable_hash("%s::%s" % (family, key))
    return graft(tpl, base if base is not None else rec, rec, seed)


# ---------------------------------------------------------------------------
# 抖音静态模板（红队 round1：真站录制语料静态化，来源见 verify/redteam_round1）
#   - _DY_GDC_FILTER_SETTINGS_JSON：dy_search_general stream 首帧 global_doodle_config.filter_settings
#     （真站为固定配置 + keyword 回显；旧合成硬编码 keyword=美食教程 且缺 filter_settings，P2-D2）
#   - _AWEME_EXTRA_STATIC / _AWEME_EXTRA_NULL：aweme_info 补 13 键的静态形态（P2-D3）
# ---------------------------------------------------------------------------
_DY_GDC_FILTER_SETTINGS_JSON = (
    "[{\"title\":\"排序依据\",\"name\":\"sort_type\",\"log_name\":\"by_top\",\"btm\":\"a1128.b51319.c58997.d0\","
    "\"default_index\":0,\"android_version\":170300,\"ios_version\":170300,\"harmony_version\":0,"
    "\"lite_android_version\":170300,\"lite_ios_version\":170300,\"lite_harmony_version\":0,\"enable_lite\":true,"
    "\"huoshan_android_version\":220300,\"huoshan_ios_version\":220300,\"enable_huo_shan\":true,"
    "\"items\":[{\"title\":\"综合排序\",\"value\":\"0\",\"log_value\":\"top_all\"},"
    "{\"title\":\"最新发布\",\"value\":\"2\",\"log_value\":\"top_time\"},"
    "{\"title\":\"最多点赞\",\"value\":\"1\",\"log_value\":\"top_likes\"}]},"
    "{\"title\":\"发布时间\",\"name\":\"publish_time\",\"log_name\":\"by_time\",\"btm\":\"a1128.b51319.c96272.d0\","
    "\"default_index\":0,\"android_version\":170300,\"ios_version\":170300,\"harmony_version\":0,"
    "\"lite_android_version\":170300,\"lite_ios_version\":170300,\"lite_harmony_version\":0,\"enable_lite\":true,"
    "\"huoshan_android_version\":220300,\"huoshan_ios_version\":220300,\"enable_huo_shan\":true,"
    "\"items\":[{\"title\":\"不限\",\"value\":\"0\",\"log_value\":\"time_all\"},"
    "{\"title\":\"一天内\",\"value\":\"1\",\"log_value\":\"within_day\"},"
    "{\"title\":\"一周内\",\"value\":\"7\",\"log_value\":\"within_week\"},"
    "{\"title\":\"半年内\",\"value\":\"180\",\"log_value\":\"within_half_year\"}]},"
    "{\"title\":\"视频时长\",\"name\":\"filter_duration\",\"log_name\":\"by_duration\",\"btm\":\"a1128.b51319.c40833.d0\","
    "\"default_index\":0,\"android_version\":190200,\"ios_version\":190200,\"harmony_version\":0,"
    "\"lite_android_version\":0,\"lite_ios_version\":0,\"lite_harmony_version\":0,\"enable_lite\":true,"
    "\"huoshan_android_version\":220300,\"huoshan_ios_version\":220300,\"enable_huo_shan\":true,"
    "\"items\":[{\"title\":\"不限\",\"value\":\"\",\"log_value\":\"不限\"},"
    "{\"title\":\"1分钟以下\",\"value\":\"0-1\",\"log_value\":\"1分钟以下\"},"
    "{\"title\":\"1-5分钟\",\"value\":\"1-5\",\"log_value\":\"1-5分钟\"},"
    "{\"title\":\"5分钟以上\",\"value\":\"5-10000\",\"log_value\":\"5分钟以上\"}],"
    "\"search_nil_text\":{\"info\":\"暂无更多，\",\"jump_text\":\"查看所有内容\"},"
    "\"search_less_text\":{\"info\":\"暂无更多，\",\"jump_text\":\"查看所有内容\"}},"
    "{\"title\":\"搜索范围\",\"name\":\"search_range\",\"log_name\":\"by_history\",\"btm\":\"a1128.b51319.c46803.d0\","
    "\"default_index\":0,\"android_version\":190400,\"ios_version\":190400,\"harmony_version\":0,"
    "\"lite_android_version\":0,\"lite_ios_version\":0,\"lite_harmony_version\":0,\"enable_lite\":true,"
    "\"huoshan_android_version\":220300,\"huoshan_ios_version\":220300,\"enable_huo_shan\":true,"
    "\"items\":[{\"title\":\"不限\",\"value\":\"0\",\"log_value\":\"view_all\"},"
    "{\"title\":\"关注的人\",\"value\":\"3\",\"log_value\":\"follow_user\"},"
    "{\"title\":\"最近看过\",\"value\":\"1\",\"show_dot\":1638288000,\"log_value\":\"recent_played\"},"
    "{\"title\":\"还未看过\",\"value\":\"2\",\"log_value\":\"unplayed\"}],"
    "\"search_nil_text\":{\"info\":\"暂无更多，\",\"jump_text\":\"查看所有内容\"},"
    "\"search_less_text\":{\"jump_text\":\"查看所有内容\",\"info\":\"暂无更多，\"}},"
    "{\"title\":\"内容形式\",\"name\":\"content_type\",\"log_name\":\"by_aweme_type\",\"btm\":\"a1128.b51319.c26154.d0\","
    "\"default_index\":0,\"filter_style\":0,\"android_version\":200900,\"ios_version\":200900,\"harmony_version\":0,"
    "\"lite_android_version\":0,\"lite_ios_version\":0,\"lite_harmony_version\":0,\"enable_lite\":true,"
    "\"huoshan_android_version\":220300,\"huoshan_ios_version\":220300,\"enable_huo_shan\":true,"
    "\"items\":[{\"title\":\"不限\",\"value\":\"0\",\"log_value\":\"type_all\"},"
    "{\"title\":\"视频\",\"value\":\"1\",\"log_value\":\"video\"},"
    "{\"title\":\"图文\",\"value\":\"2\",\"show_dot\":1964793600,\"log_value\":\"picture\"}],"
    "\"search_nil_text\":{\"info\":\"暂无更多，\",\"jump_text\":\"查看所有内容\"},"
    "\"search_less_text\":{\"info\":\"暂无更多，\",\"jump_text\":\"查看所有内容\"}}]"
)

_DY_GDC_FILTER_SETTINGS = None  # 懒解析缓存


def _dy_gdc(keyword: str | None, with_filters: bool) -> dict:
    """global_doodle_config：keyword 按请求回显（P2-D2）；stream/search_item 带 filter_settings。"""
    global _DY_GDC_FILTER_SETTINGS
    gdc: dict = {"keyword": keyword or ""}
    if with_filters:
        if _DY_GDC_FILTER_SETTINGS is None:
            _DY_GDC_FILTER_SETTINGS = json.loads(_DY_GDC_FILTER_SETTINGS_JSON)
        gdc = {"filter_settings": _DY_GDC_FILTER_SETTINGS, "keyword": keyword or ""}
    return gdc


# aweme_info 补 13 键（红队 P2-D3：真站 single 85 键 vs 合成 72 键）——静态形态取自录制语料
_AWEME_EXTRA_NULL_KEYS = ("mix_info", "douyin_p_c_video_extra", "fake_horizontal_info",
                          "related_music_anchor", "series_info")  # 语料该版本未回传 → null 补位

_AWEME_EXTRA_STATIC = {
    "risk_infos": {"vote": False, "warn": False, "risk_sink": False, "type": 0, "content": ""},
    "video_control": {
        "allow_download": False, "share_type": 0, "show_progress_bar": 1, "draft_progress_bar": 1,
        "allow_duet": True, "allow_react": False, "prevent_download_type": 3,
        "allow_dynamic_wallpaper": False, "timer_status": 1, "allow_music": True,
        "allow_stitch": False, "allow_douplus": True, "allow_share": True, "share_grayed": False,
        "download_ignore_visibility": True, "duet_ignore_visibility": True,
        "share_ignore_visibility": True,
        "download_info": {"level": 1, "fail_info": {"code": 200021, "reason": "only_self",
                                                    "msg": "作者已关闭下载功能"}},
        "duet_info": {"level": 0}, "allow_record": True, "disable_record_reason": "",
        "timer_info": {"timer_status": 0}, "show_ai_corner": False, "show_watermark": False},
    "prevent_download": False,
    "impression_data": {"group_id_list_a": None, "group_id_list_b": None, "similar_id_list_a": None,
                        "similar_id_list_b": None, "group_id_list_c": None, "group_id_list_d": None},
    "danmaku_control": {"enable_danmaku": True, "post_privilege_level": 0, "is_post_denied": False,
                        "post_denied_reason": "", "activities": None},
    "entertainment_product_info": {"sub_title": None, "market_info": {"limit_free": {"in_free": False},
                                                                     "marketing_tag": None}, "biz": 0},
    "suggest_words": {"suggest_words": [{"words": [], "scene": "comment_top_rec", "icon_url": "",
                                         "hint_text": "大家都在搜：",
                                         "extra_info": "{\"is_life_intent\":1,\"resp_from\":\"hit_cache\"}"}]},
}


def _dy_share_info(rec: dict) -> dict:
    """share_info：结构取自语料样板，share_url/share_title 按本条 aweme 派生。"""
    desc = rec.get("desc") or ""
    return {
        "share_url": "https://www.iesdouyin.com/share/video/%s/?region=CN&titleType=title&share_version=190600"
                     "&from_aid=6383&from_ssr=1" % rec.get("aweme_id", ""),
        "share_desc": "在抖音，记录美好生活",
        "share_title": desc,
        "share_link_desc": "6.46 b@A.GV 10/15 eOX:/ :7pm %s  %%s 复制此链接，打开Dou音搜索，直接观看视频！" % desc,
        "share_quote": "",
        "share_desc_info": "#在抖音，记录美好生活#%s" % desc,
    }


def augment_aweme(rec: dict, detail: bool = False) -> dict:
    """aweme_info 键集补齐：+13 键（85 形态）；detail=True 时按语料 150 键超集再补默认键。

    红队 P2-D3：真站 single aweme_info 85 键 vs 合成 72 键（差 13）；真站 detail 150 键。
    保真修复轮：结构对齐改由真值模板 materialize() 主导（S2），本函数保留为
    模板缺失时的兜底路径。
    """
    out = dict(rec)
    out.update(_AWEME_EXTRA_STATIC)
    out["share_info"] = _dy_share_info(rec)
    for k in _AWEME_EXTRA_NULL_KEYS:
        out[k] = None
    if detail:
        v = rec.get("video") or {}
        for k, dv in (
            ("duration", v.get("duration", 0)), ("region", "CN"), ("rate", 1),
            ("is_ads", False), ("activity_video_type", -1), ("boost_status", 0),
            ("author_mask_tag", 0), ("aweme_type_tags", ""), ("caption", ""),
            ("authentication_token", ""), ("can_be_oc_cover", False), ("can_cache_to_local", True),
            ("original", False), ("is_story", False), ("pc_need_login", False),
        ):
            out.setdefault(k, dv)
        for k in _DETAIL_EXTRA_KEYS:
            out.setdefault(k, None)
    return out


def _utf16_len(s: str) -> int:
    """UTF-16 码元长度（JS 字符串索引语义）。

    红队 R10A-P3-3：text_extra 偏移单位标准——语料 1,004/1,004 条目（773
    hashtag + 231 @）按 UTF-16 码元 100% 精确对齐（含 emoji 文案：非 BMP
    字符 1 码点 = 2 码元）；数据集生成侧为 Python 码点口径，渲染层统一换算，
    三站共用同一口径（xhs/ks 面 by at_users/headurl 无偏移结构，天然不受影响）。
    """
    return len(s.encode("utf-16-le", "surrogatepass")) // 2


def _dy_text_extra_utf16(desc, entries):
    """desc 面 text_extra 偏移：码点口径 → UTF-16 码元口径（R10A-P3-3）。

    仅当条目码点切片校验通过（desc[start:end] == "#"+hashtag_name，数据集
    生成公式保证）时换算；未命中形态原样保留（不破坏其他面）。
    emoji 前置文案（如「…说清🍂 #桌面改造上手」）换算后 JS 切片精确对齐。
    """
    if not isinstance(desc, str) or not isinstance(entries, list):
        return entries
    out = []
    for e in entries:
        if isinstance(e, dict) and e.get("type") == 1 \
                and isinstance(e.get("hashtag_name"), str) \
                and not isinstance(e.get("start"), bool) \
                and not isinstance(e.get("end"), bool):
            try:
                s0, e0 = int(e["start"]), int(e["end"])
            except (TypeError, ValueError):
                out.append(e)
                continue
            if 0 <= s0 < e0 <= len(desc) and desc[s0:e0] == "#" + e["hashtag_name"] \
                    and (s0, e0) != (_utf16_len(desc[:s0]), _utf16_len(desc[:e0])):
                e = dict(e)
                e["start"] = _utf16_len(desc[:s0])
                e["end"] = _utf16_len(desc[:e0])
        out.append(e)
    return out


# ---------------------------------------------------------------------------
# 红队 R12A-P3-3：aweme 实体可选对象键族按语料携带率渲染（纯渲染层，数据面零改动）。
#
# 语料三态口径（dy 五端点面全量普查：single 189 / item 610 / detail 50 /
# related 385 / post 477 实体）：携带 → 键在（真值子树）；不携带 → 键整缺
# （chapter_list 例外：null 补位，语料两面 null 形态在档）。携带判定 = 实体
# aweme_id × 族稳定 hash 的 latent u < 面携带率（千分率表 = 语料实测率；确定性：
# 同一实体跨请求/翻页/端点恒定；detail⊇search 的 mix/series 嵌套由同一 u 的
# 阈值序天然满足）。子树样板取自语料单实体直提（sites/templates/dy_optfam.json），
# 经 graft 物化：id/URL 按实体种子形态保持合成、时间戳锚定记录 create_time。
# ---------------------------------------------------------------------------
_DY_OPTFAM_PERMILLE = {
    "search": {"mix_info": 275, "series_info": 185, "related_music_anchor": 365,
               "douyin_p_c_video_extra": 275, "video_abstract": 79, "anchor_info": 74,
               "misc_download_addrs": 407, "dl_fail": 460, "du_fail": 328},
    "item": {"mix_info": 259, "series_info": 151, "related_music_anchor": 254,
             "douyin_p_c_video_extra": 285, "anchor_info": 195,
             "misc_download_addrs": 449, "dl_fail": 516, "du_fail": 587},
    "detail": {"mix_info": 360, "series_info": 300, "related_music_anchor": 260,
               "douyin_p_c_video_extra": 140, "anchor_info": 300,
               "misc_download_addrs": 380, "chapter_list": 140,
               "dl_fail": 460, "du_fail": 380},
    "related": {"mix_info": 265, "series_info": 174, "related_music_anchor": 249,
                "douyin_p_c_video_extra": 244, "poi_info": 132, "anchor_info": 130,
                "chapter_list": 200, "dl_fail": 426, "du_fail": 169},
    "post": {"mix_info": 291, "series_info": 184, "related_music_anchor": 224,
             "douyin_p_c_video_extra": 121, "anchor_info": 195, "chapter_list": 111,
             "misc_download_addrs": 394, "dl_fail": 463, "du_fail": 174},
}


def _optfam_tpl() -> dict:
    """可选族样板/变体池（dy_optfam.json；缺文件时机制整体旁路，零影响）。"""
    return _templates().get("dy_optfam") or {}


def _optfam_u(key: str, fam: str) -> float:
    """实体×族 latent u∈[0,1)（跨请求/翻页/端点恒定；族间独立）。"""
    return (_stable_hash("dy-optfam::%s::%s" % (key, fam)) % (2 ** 30)) / float(2 ** 30)


def _optfam_weighted(key: str, fam: str, pool):
    """语料频次加权变体池确定性取样（pool 行末位为权重）。"""
    total = sum(int(p[-1]) for p in pool)
    h = _stable_hash("dy-optfam-var::%s::%s" % (key, fam)) % max(1, total)
    for p in pool:
        h -= int(p[-1])
        if h < 0:
            return p
    return pool[0]


def _dy_optional_families(a: dict, rec: dict, face: str) -> None:
    """R12A-P3-3：按面携带率注入可选对象族（就地修改物化产物 a；
    不携带面保持模板基线：键整缺 / chapter_list=null）。"""
    rates = _DY_OPTFAM_PERMILLE.get(face) or {}
    aid = str(rec.get("aweme_id") or rec.get("id") or "")
    if not aid or not rates:
        return
    tpl = _optfam_tpl()
    if not tpl:
        return
    seed = _stable_hash("dy-optfam-obj::%s" % aid)

    def carry(fam: str) -> bool:
        r = rates.get(fam, 0)
        return r > 0 and _optfam_u(aid, fam) < r / 1000.0

    if carry("mix_info"):
        t = tpl.get("mix_%s" % face) or tpl.get("mix_search")
        a["mix_info"] = graft(t, None, rec, seed, "mix_info")
        st = (a["mix_info"] or {}).get("statis")
        if isinstance(st, dict) and isinstance(st.get("current_episode"), int) \
                and isinstance(st.get("updated_to_episode"), int) \
                and st["current_episode"] > st["updated_to_episode"]:
            st["updated_to_episode"] = st["current_episode"]   # 集数序不变量
    if carry("series_info"):
        # series_info 按面取变体（语料键数分面：single/item 15、related 16、
        # detail 30（含 series_ui_config）、post 33——ui 配置仅 detail/post 面）
        stpl = tpl.get("series_%s" % face) or tpl.get("series_detail")
        a["series_info"] = graft(stpl, None, rec, seed, "series_info")
    if carry("related_music_anchor"):
        a["related_music_anchor"] = graft(tpl["related_music_anchor"], None, rec, seed,
                                          "related_music_anchor")
    if carry("video_abstract"):
        a["video_abstract"] = graft(tpl["video_abstract"], None, rec, seed,
                                    "video_abstract")
    if carry("poi_info"):
        a["poi_info"] = graft(tpl["poi_info"], None, rec, seed, "poi_info")
    if carry("anchor_info"):
        a["anchor_info"] = graft(tpl["anchor_info"], None, rec, seed, "anchor_info")
    if carry("douyin_p_c_video_extra"):
        # 语料形态：JSON 编码字符串（ai_summary 族标志位），12 变体按语料频次
        a["douyin_p_c_video_extra"] = _optfam_weighted(
            aid, "pc_extra", tpl["pc_extra"])[0]
    if carry("chapter_list"):
        # 章节 2-17 项（语料众数 3-5）；首「引言」尾「结语」，时间戳沿时长递增
        rng = _leaf_rng(seed, "chapter_list")
        n = int(rng.integers(3, 6))
        dur = int(((rec.get("video") or {}).get("duration")) or 0) or 30000
        descs = tpl.get("chapter_desc") or ["引言", "结语"]
        item_tpl = tpl["chapter_item"]
        a["chapter_list"] = [
            graft(dict(item_tpl,
                       desc=("引言" if i == 0 else "结语" if i == n - 1 else
                             descs[int(rng.integers(0, len(descs)))]),
                       timestamp=max(1000, int(dur * (i + 1) / (n + 1)) // 1000 * 1000)),
                  None, rec, seed, "chapter_list[]#%d" % i)
            for i in range(n)]
    v = a.get("video")
    if isinstance(v, dict) and carry("misc_download_addrs"):
        # 语料形态：JSON 编码字符串（suffix_scene 单键，96/96 同构）；
        # 出口 & 转义由 synth_api._json 站级口径统一处理
        v["misc_download_addrs"] = json.dumps(
            graft(tpl["misc_download_addrs"], None, rec, seed,
                  "video.misc_download_addrs"),
            ensure_ascii=False, separators=(",", ":"))
    vc = a.get("video_control")
    if isinstance(vc, dict):
        di = vc.get("download_info")
        if isinstance(di, dict) and carry("dl_fail"):
            lvl, code, reason, msg, _w = _optfam_weighted(aid, "dl_fail", tpl["dl_fail"])
            di["level"] = lvl   # 语料配对：fail 携带 ⇒ level∈{1,2}；不携带 ⇒ level=0
            di["fail_info"] = {"code": code, "reason": reason, "msg": msg}
        du = vc.get("duet_info")
        if isinstance(du, dict) and carry("du_fail"):
            lvl, code, reason, _w = _optfam_weighted(aid, "du_fail", tpl["du_fail"])
            du["level"] = lvl
            du["fail_info"] = {"code": code, "reason": reason}


# ---------------------------------------------------------------------------
# 红队 R11A-P3-3：dy video.bit_rate 多档码率阶梯（渲染层派生，数据集零改动）。
#
# 语料真值（rt11_b_drill3/rt11_drill6 全量）：每 aweme 3-29 档（众数 17-23），
# 每档 11 键（gear_name/quality_type/bit_rate/play_addr/is_h265/is_bytevc1/
# HDR_type/HDR_bit/FPS/video_extra/format——gear_name×quality_type 恒耦合），
# mp4+dash 成对（同 gear_name、dash 码率≈mp4×0.987）；gear play_addr.url_list
# 3 条（13953/13953）+ video.play_addr.url_list 3 条（819/819：v26-web +
# v11-weba douyinvod + www.douyin.com play 代理）。旧版恒 1 档 adapt_720_0、
# url_list 恒 2——按清晰度/gear 选流的播放器型 agent 只见单档。
# ---------------------------------------------------------------------------
_DY_BR_LADDER = (
    # (gear_name, quality_type, is_h265, is_bytevc1, FPS)——语料 gear_name×quality_type 耦合表
    ("normal_1080_0", 1, 0, 0, 30),
    ("adapt_lowest_1440_1", 7, 1, 1, 60),
    ("adapt_lowest_4_1", 72, 1, 1, 60),
    ("720_1_1", 11, 1, 1, 60),
    ("normal_720_0", 10, 0, 0, 30),
    ("low_720_0", 211, 0, 0, 30),
    ("adapt_lowest_1080_1", 5, 1, 1, 60),
    ("720_2_1", 12, 1, 1, 60),
    ("normal_540_0", 20, 0, 0, 30),
    ("low_540_0", 292, 0, 0, 30),
    ("lower_540_0", 224, 0, 0, 30),
    ("adapt_low_540_0", 291, 1, 1, 60),
    ("540_2_1", 23, 1, 1, 60),
    ("adapt_lower_540_1", 21, 1, 1, 60),
    ("540_3_1", 13, 1, 1, 60),
)
_DY_URL_ALNUM = "0123456789abcdefghijklmnopqrstuvwxyz"


def _dy_url_tok(seed: str, n: int) -> str:
    out = []
    h = _stable_hash(seed)
    while len(out) < n:
        out.append(_DY_URL_ALNUM[h % 36])
        h = ((h // 36) ^ (h << 13)) & 0xFFFFFFFFFFFFFFFF
    return "".join(out)


def _dy_vuri(seed: str) -> str:
    """douyin 视频 uri 形态：v<4digits>fgi0000<20 lowercase alnum>（语料 32 字符）。"""
    return "v%04dfgi0000%s" % (2000 + _stable_hash("dy-vuri-h::" + seed) % 9999,
                               _dy_url_tok("dy-vuri::" + seed, 20))


def _dy_play_urls3(base_url: str, uri: str, seed: str) -> list:
    """play_addr.url_list 3 条（语料形态）：v26-web + v11-weba douyinvod 简化形
    （出口经 synth_api _mu_rewrite 规整为语料模板形）+ www.douyin.com play 代理。"""
    m = re.match(r"^(https://v\d+-weba?\.douyinvod\.com)/([^/]+)(/.*)$", base_url or "")
    if not m:
        return None
    out = []
    for j, host in enumerate(("https://v26-web.douyinvod.com",
                              "https://v11-weba.douyinvod.com")):
        h = _stable_hash("dy-p3::%s\x00%d" % (seed, j))
        out.append("%s/%010x%s" % (host, h & 0xFFFFFFFFFF, m.group(3)))
    out.append("https://www.douyin.com/aweme/v1/play/?video_id=%s&line=0&file_id=%032x"
               % (uri, _stable_hash("dy-p3f::" + seed) & 0xFFFFFFFFFFFFFFFF))
    return out


def _dy_bitrate_ladder(video: dict, seed_key: str, drop_keys: tuple = ()) -> None:
    """单 gear bit_rate → 语料多档阶梯（确定性：同实体跨端点一致、跨请求恒定）。

    drop_keys：按端点族剔除键（语料 related 面 gear 键集 10 键——无
    is_bytevc1；search/detail/post 面 11 键含之，rt8fix R8C-P3-3 键树口径）。"""
    br = video.get("bit_rate")
    if not isinstance(br, list) or not br or not isinstance(br[0], dict):
        return
    base = br[0]
    h = _stable_hash("dy-br::%s" % seed_key)
    n = 3 + h % 27                      # 3-29 档（语料档数域）
    top = 2700000 + (h >> 8) % 1800001  # 顶档码率 2.7M-4.5M（语料主带）
    out = []
    for i in range(n):
        name, qt, h265, bvc1, fps = _DY_BR_LADDER[min(i // 2, len(_DY_BR_LADDER) - 1)]
        fmt = "mp4" if i % 2 == 0 else "dash"
        rate = int(top * (0.82 ** (i // 2)) * (0.987 if fmt == "dash" else 1.0)) + 10007
        g = dict(base)
        g["gear_name"] = name
        g["quality_type"] = qt
        g["bit_rate"] = rate
        g["is_h265"] = h265
        g["is_bytevc1"] = bvc1
        g["FPS"] = fps
        g["format"] = fmt
        pa = base.get("play_addr")
        if isinstance(pa, dict):
            uri = _dy_vuri("%s\x00%d" % (seed_key, i))
            urls = pa.get("url_list")
            b0 = next((u for u in (urls if isinstance(urls, list) else [])
                       if isinstance(u, str) and "douyinvod.com" in u), None)
            pa2 = dict(pa)
            pa2["uri"] = uri
            if b0:
                pa2["url_list"] = _dy_play_urls3(b0, uri, "%s\x00%d" % (seed_key, i))
            if isinstance(pa.get("data_size"), int):
                pa2["data_size"] = max(1024, int(pa["data_size"] * (0.82 ** (i // 2))))
            g["play_addr"] = pa2
        for dk in drop_keys:
            g.pop(dk, None)
        out.append(g)
    video["bit_rate"] = out
    vpa = video.get("play_addr")
    if isinstance(vpa, dict) and isinstance(vpa.get("url_list"), list):
        b0 = next((u for u in vpa["url_list"]
                   if isinstance(u, str) and "douyinvod.com" in u), None)
        if b0:
            vuri = _dy_vuri("%s\x00top" % seed_key)
            vpa["uri"] = vuri  # 语料 v2800fgi0000<20> 形态（与 video_id 参数一致）
            vpa["url_list"] = _dy_play_urls3(b0, vuri, "%s\x00top" % seed_key) \
                or vpa["url_list"]


def render_aweme(rec: dict, detail: bool = False, face: str | None = None) -> dict:
    """aweme_info / aweme_detail：真值模板物化（S2 修复主路径）。

    模板（sites/templates/dy_search_aweme|dy_detail_aweme.json）定义结构与静态值，
    数据集记录同路径值优先（aweme_id/desc/statistics/author/video/music 核心全走记录），
    must_vary 叶子形态保持合成；同一实体跨端点渲染一致。
    face（R12A-P3-3 可选族携带率分面）：search/item/detail（related/post 走
    _dy_related_or_post_aweme 专属模板与面注入），缺省按 detail 推导。
    R6C-P2-1：dy web 从不暴露播放量——statistics.play_count 恒置 0
    （语料搜索 799/799 + 详情 50/50 全 0；数据集底层 view 保留，仅信封输出置 0）。
    R7A-P3-6：author.custom_verify 同法恒置空——语料搜索卡 0/2059、
    profile/other 0/20、评论用户 0/1175 非空（web 表面不暴露认证文案，
    数据集底层保留）。
    """
    fam = "dy_detail_aweme" if detail else "dy_search_aweme"
    if fam not in _templates():
        out = augment_aweme(rec, detail)
    else:
        out = materialize(rec, fam, base=rec)
    face = face or ("detail" if detail else "search")
    st = out.get("statistics")
    if isinstance(st, dict):
        st["play_count"] = 0
    a = out.get("author")
    if isinstance(a, dict):
        a["custom_verify"] = ""
    # R10A-P3-3：desc 面 text_extra 偏移换算 UTF-16 码元口径（JS 切片语义）。
    # 附带（R10 收口）：数据集 text_extra=None 的记录（语料 1.6% null 形态），
    # graft 会用模板静态样本合成一条与 desc 无关的错位条目（hashtag_name 恒为
    # 模板快照词）——按语料 null 形态回 null，不落任何错位条目（语料
    # 1,004/1,004 条目全对齐的不变量要求零错位供给）。
    if rec.get("text_extra") is None:
        out["text_extra"] = None
    elif isinstance(out.get("text_extra"), list):
        out["text_extra"] = _dy_text_extra_utf16(out.get("desc"), out["text_extra"])
    # R11A-P3-3：bit_rate 多档码率阶梯 + play_addr.url_list 3 条（渲染层确定性派生）
    if isinstance(out.get("video"), dict):
        _dy_bitrate_ladder(out["video"], str(rec.get("aweme_id") or ""))
    # R12A-P3-3：可选对象族按面携带率注入（mix/series/rma/pc_extra/abstract/
    # poi/anchor/chapter/misc_download_addrs/dl·duet fail_info）
    _dy_optional_families(out, rec, face)
    return out


# 真站 detail 信封独有键（corpus 150 键 − 合成基础键），缺省补 null（超集策略，P2-D3 detail 侧）
_DETAIL_EXTRA_KEYS = [
    "activity_video_type", "authentication_token", "author_mask_tag", "aweme_control",
    "aweme_listen_struct", "aweme_type_tags", "boost_status", "can_be_oc_cover",
    "can_cache_to_local", "caption", "cf_assets_type", "cf_recheck_ts", "collection_corner_mark",
    "comment_gid", "comment_permission_info", "component_control", "component_info_v2",
    "disable_relation_bar", "distribute_circle", "douplus_user_type",
    "douyin_pc_video_extra_seo", "duet_aggregate_in_music_tab", "duration",
    "ecom_comment_atmosphere_type", "enable_comment_sticker_rec", "enable_decorated_emoji",
    "ent_log_extra", "entertainment_recommend_info", "entertainment_video_paid_way",
    "entertainment_video_type", "f_s_grouth_property", "fall_card_struct",
    "feed_comment_config", "flash_mob_trends", "follow_shoot_clip_info", "follow_shoot_property",
    "friend_recommend_info", "galileo_pad_textcrop", "game_tag_info", "guide_scene_info",
    "image_album_music_info", "image_comment", "image_crop_ctrl", "incentive_item_type",
    "is_24_story", "is_25_story", "is_ads", "is_aigc_media", "is_collects_selected",
    "is_duet_sing", "is_from_ad_auth", "is_image_beat", "is_life_item", "is_moment_history",
    "is_moment_story", "is_new_text_mode", "is_share_post", "is_story", "is_subtitled",
    "is_use_music", "item_aigc_follow_shot", "item_title", "item_warn_notification",
    "libfinsert_task_id", "mark_largely_following", "origin_duet_resource_uri", "original",
    "pack_usage_scene_by_req_path", "pc_need_login",
    "personal_page_botton_diagnose_style", "photo_search_entrance", "play_progress",
    "preview_title", "preview_video_status", "product_genre_info", "publish_plus_alienation",
    "rate", "region", "sec_item_id", "select_anchor_expanded_content", "seo_info",
    "series_basic_info", "series_paid_info", "share_rec_extra", "share_url", "shoot_way",
    "should_open_ad_report", "show_follow_button", "trends_event_track",
    "user_recommend_status", "video_game_data_channel_config", "video_share_edit_status",
    "visual_search_info", "vtag_search", "xigua_base_info",
]


def _dy_search_item_wrap(rec: dict, rng: np.random.Generator, ctx: str = "general") -> dict:
    """搜索 data[i]：对齐真站 16 键形态（type/aweme_info/doc_type/...，红队 P1-D2）。

    type 分布按语料/live 实证：视频 1 为主（~86%），混排 6（~10%）与 77（~4%），
    保证 agent 的 `it.type===1` 过滤有结果。
    R6C-P2-1：按端点上下文隐藏真站不暴露的计数字段——
      general/search/single（stream+single）：author.follower_count=0（语料 210/210）、
      total_favorited=0（搜索上下文 799/799=0，detail 上下文有值）；
      search/item：保留 follower_count（语料 609 条有值）、total_favorited=0。
    play_count 在 render_aweme 内恒置 0（三上下文语料全 0）。
    """
    r = rng.random()
    item_type = 1 if r < 0.86 else (6 if r < 0.96 else 77)
    aid = str(rec.get("aweme_id") or "")
    # R12A-P3-3：single(general) 与 search/item 两面携带率分表注入
    aweme = render_aweme(rec, face=("item" if ctx == "item" else "search"))
    st = aweme.get("statistics")
    if isinstance(st, dict):
        st["play_count"] = 0   # 双保险（render_aweme 已置 0，模板兜底路径再压一次）
    a = aweme.get("author")
    if isinstance(a, dict):
        a["total_favorited"] = 0
        a["custom_verify"] = ""   # R7A-P3-6 双保险（语料搜索卡 0/2059 非空）
        if ctx == "general":
            a["follower_count"] = 0
    return {
        "type": item_type,
        "aweme_info": aweme,
        "doc_type": 153,
        "sub_card_list": None,
        "provider_doc_id": int(aid) if aid.isdigit() else aid,
        "provider_doc_id_str": aid,
        "tab": None,
        "show_tab": None,
        "debug_diff_info": {},
        "aweme_list": None,
        "ecom_goods_list": None,
        "music_info_list": None,
        "card_unique_name": "video",
        "ops": None,
        "qishui_music_list": None,
        "shoot_position_list": None,
    }


def _dy_extra(rng: np.random.Generator, logid: str, search_id: str | None = None) -> dict:
    """抖音响应 extra：契约必填 5 字段（fatal_item_ids/logid/now/scenes/search_request_id）。

    logid 为 34 位「日期前缀」形态（红队 P2-D1：YYYYMMDDHHMMSS+20 位大写 hex）；
    now 用当前时间毫秒（P3-2）；search_id 为翻页链回显键（实证）。
    """
    extra = {
        "fatal_item_ids": [],
        "logid": logid,
        "now": _now_ms(),
        "scenes": None,
        "search_request_id": "",
    }
    if search_id is not None:
        extra["search_id"] = search_id  # 翻页链实证：single 响应回显请求 search_id
    return extra


def _dy_envelope(rng: np.random.Generator, logid: str, page_no: int, page_size: int,
                 n_total: int, data: list, search_id: str | None = None,
                 stream: bool = False, keyword: str | None = None,
                 abs_start: int | None = None) -> dict:
    """抖音响应外层：补齐契约必填信封字段（P2-3 修复：14 个顶层必填）。

    红队 round1 对齐：stream 帧 15 键（无 time_cost/maokai_extra/path/mock_recall_path，P3-1）、
    single 18 键；global_doodle_config.keyword 按请求回显（P2-D2）。
    abs_start（红队 R2-P2-1）：绝对起始下标模式——cursor/has_more 按绝对位置计算，
    offset 语义与 count 解耦（页号模式为兼容保留：CLI/离线渲染仍用 page_no*page_size）。
    """
    if abs_start is None:
        next_cursor = page_no * page_size
    else:
        next_cursor = abs_start + page_size
    resp = {
        "status_code": 0,
        "cursor": next_cursor,
        "has_more": 1 if next_cursor < n_total else 0,
        "ad_info": {},
        "data": data,
        "douyin_ai_search_info": {
            "ai_search_req_patch": {},
            "is_hit_high_risk": False,
            "is_simple_qa_intent": False,
        },
        "extra": _dy_extra(rng, logid, search_id),
        "global_doodle_config": _dy_gdc(keyword, with_filters=stream),
        "guide_search_words": None,
        "log_pb": {"impr_id": logid},  # 实证 impr_id 与 logid 同为 34 位形态
        "multi_columns_info": {"group_tag": "", "is_multi_columns": True},
        "ops": None,
        "polling_time": 3,
        "qc": "",
    }
    if stream:
        resp["result_status"] = 2  # stream 契约实证：数据块含 result_status=2
    else:  # single 专有 4 键（真站 stream 帧无，P3-1）
        resp["maokai_extra"] = {}
        resp["mock_recall_path"] = ids.opaque(rng, 36)
        resp["path"] = ids.opaque(rng, 36)
        resp["time_cost"] = {"stream_inner": int(rng.integers(200, 1500))}
    return resp


def render_douyin_stream(dataset: DatasetReader, page_no: int, page_size: int,
                         keyword: str | None = None,
                         start: int | None = None) -> dict:
    """stream 首页形态：cursor=下页 offset，has_more=1，extra.logid 作为翻页 search_id 回传。

    start（R2-P2-1）：绝对起始下标（keyword 轮转基×20 + offset），与 count 解耦。
    R6A-P2-1：映射关键词的数据窗口改为类目偏置采样（_search_records）。"""
    records = _search_records(dataset, "douyin", keyword, start, page_no, page_size)
    rng = _page_rng(dataset, f"dy-stream-{page_no}" if start is None else f"dy-stream-a{start}")
    data = [_dy_search_item_wrap(rec, rng, ctx="general") for rec in records]
    return _dy_envelope(
        rng, ids.dy_logid(rng), page_no, page_size, len(dataset),
        data, stream=True, keyword=keyword, abs_start=start,
    )


def render_douyin_single(dataset: DatasetReader, page_no: int, page_size: int,
                         search_id: str | None = None,
                         keyword: str | None = None,
                         start: int | None = None) -> tuple[dict, dict]:
    """single 翻页形态：请求 search_id=上页 extra.logid、offset 递增；返回 (请求参数, 响应)。"""
    records = _search_records(dataset, "douyin", keyword, start, page_no, page_size)
    rng = _page_rng(dataset, f"dy-single-{page_no}" if start is None else f"dy-single-a{start}")
    request = {
        "search_id": search_id or "",
        "offset": (page_no - 1) * page_size,
        "count": page_size,
    }
    data = [_dy_search_item_wrap(rec, rng, ctx="general") for rec in records]
    response = _dy_envelope(
        rng, ids.dy_logid(rng), page_no, page_size, len(dataset),
        data, search_id=request["search_id"], keyword=keyword, abs_start=start,
    )
    return request, response


def render_douyin_search_item(dataset: DatasetReader, page_no: int, page_size: int,
                              search_id: str | None = None,
                              keyword: str | None = None,
                              start: int | None = None) -> tuple[dict, dict]:
    """/aweme/v1/web/search/item（视频频道搜索，红队 P1-D1 附带项）。

    语料实证信封 12 键：status_code/aweme_list/has_more/cursor/guide_search_words/extra/
    log_pb/backtrace/data/global_doodle_config/path/mock_recall_path；item 11 键（type 在首位）。
    """
    records = _search_records(dataset, "douyin", keyword, start, page_no, page_size)
    rng = _page_rng(dataset, f"dy-item-{page_no}" if start is None else f"dy-item-a{start}")
    logid = ids.dy_logid(rng)
    data = [_dy_search_item_wrap(rec, rng, ctx="item") for rec in records]
    for it in data:  # search/item 的 item 无 doc_type/provider_* 等 5 键（语料实证 11 键）
        for k in ("doc_type", "provider_doc_id", "provider_doc_id_str",
                  "debug_diff_info", "card_unique_name"):
            it.pop(k, None)
    request = {"keyword": keyword or "", "search_source": "normal_search",
               "offset": (page_no - 1) * page_size, "count": page_size,
               "search_id": search_id or ""}
    hashtags = []
    for rec in records:
        for te in rec.get("text_extra") or []:
            if isinstance(te, dict) and te.get("hashtag_name"):
                hashtags.append(te["hashtag_name"])
    guide = [{"id": ids.digits_str(rng, 19), "word": w, "type": "recom",
              "query_id": ids.digits_str(rng, 19), "attached_text": None}
             for w in dict.fromkeys(hashtags[:3])]
    next_cursor = (start if start is not None else (page_no - 1) * page_size) + page_size
    response = {
        "status_code": 0,
        "aweme_list": None,
        "has_more": 1 if next_cursor < len(dataset) else 0,
        "cursor": next_cursor,
        "guide_search_words": guide or None,
        "extra": _dy_extra(rng, logid),
        "log_pb": {"impr_id": logid},
        "backtrace": ids.opaque(rng, 22) + "==",
        "data": data,
        "global_doodle_config": _dy_gdc(keyword, with_filters=True),
        "path": ids.opaque(rng, 36),
        "mock_recall_path": ids.opaque(rng, 36),
    }
    return request, response


def render_douyin_search_empty(keyword: str | None = None, offset: int = 0,
                               stream: bool = True) -> dict:
    """越界/空参游标的空结果信封（红队 P2-X1 同族）：data=[] + has_more=0 + cursor 原样回显。"""
    rng = np.random.default_rng([int(time.time()) & 0xFFFF, 71, 73])
    resp = _dy_envelope(rng, ids.dy_logid(rng), 1, 0, 0, [], stream=stream, keyword=keyword)
    resp["cursor"] = offset
    resp["has_more"] = 0
    return resp


def render_douyin_detail(dataset: DatasetReader, line_no: int) -> dict:
    """/aweme/v1/web/aweme/detail 存在 id：{aweme_detail, log_pb, status_code}（语料实证 3 顶层键）。"""
    rec = dataset.read(line_no)
    rng = _page_rng(dataset, f"dy-detail-{line_no}")
    logid = ids.dy_logid(rng)
    detail = render_aweme(rec, detail=True)
    # 跨端点一致性兜底：author 核心键（uid/sec_uid/nickname/follower_count）以数据集为准，
    # 模板缺失时也必须保留（harness dy_entity_detail 的 author_followers 依赖此键）
    a = detail.get("author")
    if isinstance(a, dict):
        src = rec.get("author") or {}
        for k in ("uid", "sec_uid", "nickname", "follower_count", "total_favorited"):
            if k in src:
                a[k] = src[k]
    return {
        "aweme_detail": detail,
        "log_pb": {"impr_id": logid},
        "status_code": 0,
    }


def render_douyin_detail_missing() -> dict:
    """/aweme/v1/web/aweme/detail 不存在 id：HTTP 200 + aweme_detail:null（红队 P1-D1，绝不 404）。"""
    rng = np.random.default_rng([int(time.time()) & 0xFFFF, 31, 41])
    return {
        "aweme_detail": None,
        "log_pb": {"impr_id": ids.dy_logid(rng)},
        "status_code": 0,
    }


# 抖音评论（数据集实体不含评论列表 → 按 aweme_id 确定性合成；同一 id 同一页永远一致）
# R5A-P1-2：文本由 commentext 模板×槽位组合生成（旧 12 条固定池 idx 轮转，
# 单视频 20 条仅 12 种、8 组一字不差重复；语料唯一率 100%）
# R7A-P3-4：ip 属地池对齐语料 1,175 条评论的 42 属地频次（旧 20 属地均匀池
# 海外+港澳台 24.2% vs 语料 1.0%）——按语料计数近似权重展开（/2 取整、≥1）。
_DY_CMT_IP_WEIGHTS = [
    ("广东", 163), ("浙江", 80), ("重庆", 80), ("江苏", 71), ("福建", 68),
    ("四川", 61), ("河南", 59), ("山东", 57), ("安徽", 48), ("湖北", 47),
    ("湖南", 44), ("广西", 42), ("上海", 34), ("河北", 31), ("北京", 30),
    ("陕西", 27), ("贵州", 26), ("江西", 24), ("辽宁", 23), ("山西", 20),
    ("云南", 18), ("天津", 18), ("吉林", 14), ("黑龙江", 14), ("新疆", 13),
    ("内蒙古", 12), ("海南", 10), ("甘肃", 9), ("IP未知", 9), ("中国香港", 6),
    ("宁夏", 5), ("加拿大", 2), ("西藏", 1), ("英国", 1), ("阿联酋", 1),
    ("尼日利亚", 1), ("坦桑尼亚", 1), ("加蓬", 1), ("中国台湾", 1), ("中国澳门", 1),
    ("新加坡", 1), ("德国", 1),
]
_DY_CMT_IP = [loc for loc, cnt in _DY_CMT_IP_WEIGHTS
              for _ in range(max(1, cnt // 2))]

# R7A-P3-3 @提及出现率（语料：dy 顶层评论 9.7%（114/1175，子评论 0/30 不加））
_DY_AT_PERMILLE = 97


def _maybe_at_prefix(salt: str, permille: int) -> bool:
    """@提及确定性判定（与各站评论 rng 流解耦：纯 hash，翻页/换 count 稳定）。"""
    return _stable_hash("at::%s" % salt) % 1000 < permille


def _dataset_commenter(dataset: "DatasetReader", site: str, salt: str, seq: int) -> dict:
    """评论者 = 数据集内某条作品的作者实体（R5A-P2-5：跨端点可回查）。

    行号由 (site, salt, seq) 确定性派生 ⇒ 同一评论的评论者在任何翻页路径/
    任何端点下恒为同一实体；sec_uid/user_id 落在站点作者实体空间，
    profile/other、user_posted、profile/get 反查均可解析且昵称一致。
    """
    if dataset is None:
        return {}
    try:
        ln = _stable_hash("%s-cuser::%s::%d" % (site, salt, seq)) % len(dataset)
        rec = dataset.read(ln)
    except Exception:
        return {}
    if site == "xhs":   # xhs 作者实体位于 note_card.user
        a = (rec.get("note_card") or {}).get("user") or {}
        if not a.get("user_id"):
            return {}
        return {"user_id": a.get("user_id"), "nickname": a.get("nickname"),
                "avatar": a.get("avatar")}
    a = rec.get("author") or {}
    return a if (a.get("uid") or a.get("id")) else {}


def _dy_comment_user(rng: np.random.Generator, rec: dict, dataset=None, seq: int = 0,
                     aweme_id: str = "") -> dict:
    """评论用户：R5A-P2-5 取数据集作者实体（sec_uid 可经 profile/other 回查、昵称一致）；
    派生字段（short_id/unique_id 等）按 rng 确定性生成。dataset 缺失时退回旧合成形态。"""
    src = _dataset_commenter(dataset, "douyin", str(aweme_id or rec.get("aweme_id") or ""), seq)
    if src:
        user = dict(src)
    else:
        user = dict(rec.get("author") or {})
        user["uid"] = ids.dy_uid(rng)
        user["sec_uid"] = ids.dy_sec_uid(rng)
        user["nickname"] = ids.pick(rng, ("爱吃", "爱看", "爱拍", "爱逛")) + \
            ids.pick(rng, ("小王", "阿慧", "大乔", "小李", "汤圆", "栗子")) + \
            ids.pick(rng, ("日常", "日记", "不吃香菜", "在线", "星球", "观察员"))
    user.update({
        "short_id": ids.digits_str(rng, 11),
        "unique_id": ids.digits_str(rng, 11),
        "signature": None,
        "custom_verify": "",   # R7A-P3-6：语料评论用户 0/1175 非空（web 不暴露认证文案）
    })
    return user


def _dy_comment(rng: np.random.Generator, rec: dict, idx: int, total: int,
                now_ts: int | None = None, dataset=None, text_salt: str | None = None,
                min_ts: int | None = None) -> dict:
    """单条评论。R2-P2-3：评论晚于发布（0 倒置）；R6A-P2-2：延迟改为重尾混合
    （语料 p50≈8.1 天、22%>1 月——旧「发布后 [0, min(30d, 年龄)] 均匀」压缩为
    p50≈43 分钟的即时爆发形态）；cid 用真站雪花结构 (ctime-δ)<<32|rand32
    （R2-P2-2，语料 cid 首位 '7' 1175/1175、时间可编码）。
    min_ts（R9A-P2-2）：楼中楼子评论的根评论时间下界——create_time 采样窗口
    改为 [min_ts, now]，保证子评论严格晚于其回复的根评论（语料 0/168 违例）。"""
    from synthgen.distengine import comment_ctime
    topic = ""
    for te in rec.get("text_extra") or []:
        if isinstance(te, dict) and te.get("hashtag_name"):
            topic = te["hashtag_name"]
            break
    aweme_id = str(rec.get("aweme_id") or "")
    # 楼中楼（text_salt=comment_id）与根评论分命名空间：两列表内部 0 重复且互不撞句
    text = commentext.comment_text("douyin", idx, text_salt or aweme_id, topic=topic or None)
    # R7A-P3-3：@作者昵称前缀（仅顶层评论；语料子评论 0/30 含 @，楼中楼由
    # render_douyin_comment_list_reply 的二层嵌套另配语境）
    # R10A-P3-2：@ 注入与 text_extra 结构化标注同源耦合——语料 231 个 type=0
    # @ 条目（start/end/user_id/sec_uid 全套、hashtag_name/hashtag_id 空串；
    # 键序 start,end,user_id,type,hashtag_name,hashtag_id,sec_uid），正文含 @
    # 的评论必带对应条目；start/end 按 UTF-16 码元口径（昵称可含非 BMP emoji，
    # 与 R10A-P3-3 同一标准）
    at_extra = None
    if text_salt is None and _maybe_at_prefix("dy::%s::%d" % (aweme_id, idx),
                                              _DY_AT_PERMILLE):
        au = rec.get("author") or {}
        nick = au.get("nickname") or ""
        if nick:
            text = "@%s %s" % (nick, text)
            at_extra = [{
                "start": 0,
                "end": _utf16_len("@" + nick),
                "user_id": str(au.get("uid") or ""),
                "type": 0,
                "hashtag_name": "",
                "hashtag_id": "",
                "sec_uid": str(au.get("sec_uid") or ""),
            }]
    pub = int(rec.get("create_time") or _now_ms() // 1000)
    if min_ts and int(min_ts) > pub:   # R9A-P2-2：根评论时间下界（楼中楼）
        pub = int(min_ts)
    now = int(now_ts if now_ts is not None else _now_ms() // 1000)
    ctime = comment_ctime(rng, pub, now)   # R6A-P2-2 重尾延迟 + 边界钳制
    cid = ids.dy_snowflake_id(rng, ctime)
    return {
        "aweme_id": str(rec.get("aweme_id") or ""),
        "can_collect": False,
        "can_share": True,
        "cid": cid,
        "content_type": 1,
        "create_time": ctime,
        "decorated_emoji_info": None,
        "digg_count": int(rng.integers(0, 5000)),
        "ent_parachute_tip": 0,  # 契约实证 number（旧合成误为 null，S2 类型差）
        "enter_from": "homepage_hot",
        "image_list": None,
        "ip_label": ids.pick(rng, _DY_CMT_IP),
        "is_author_digged": False,
        "is_folded": False,
        "is_hot": idx < 2,
        "is_note_comment": 0,
        "is_user_tend_to_reply": False,
        "item_comment_total": total,
        "label_list": None,
        "label_text": "",
        "label_type": -1,
        "level": 1,
        "merge_comment_label": None,
        "reply_comment": None,
        "reply_comment_total": _dy_reply_claim_for(cid),
        "reply_id": "0",
        "reply_to_reply_id": "0",
        "sort_tags": "{\"eco_level_10\":1,\"top_list\":1}" if idx < 2 else "{\"eco_level_3\":1}",
        "status": 1,
        "stick_position": 0,
        "text": text,
        "text_extra": at_extra or [],
        "text_music_info": None,
        "user": _dy_comment_user(rng, rec, dataset=dataset, seq=idx,
                                 aweme_id=str(text_salt or aweme_id)),
        "user_buried": False,
        "user_digged": 0,
        "video_list": None,
    }


def _dy_comment_item(rng: np.random.Generator, rec: dict, idx: int, total: int,
                     now_ts: int | None = None, dataset=None, text_salt: str = None,
                     min_ts: int | None = None) -> dict:
    """评论条目：语义核心（cid/时间序/user）+ 真值模板物化（S2：comment user 69 键、
    text_extra 元素等高频结构由 sites/templates/dy_comment_item 补齐）。

    红队 R9A-P3-3：can_create_item 键存在性语义——语料仅 true 时携带键（list 面
    475 带/700 缺 = 40.4%、reply 面 10/30，无 present-null 形态；R8C-P3-3 的
    null-for-absent 与语料「缺键」不同）。按 cid 确定性派生，false 分支删键。"""
    base = _dy_comment(rng, rec, idx, total, now_ts=now_ts, dataset=dataset,
                       text_salt=text_salt, min_ts=min_ts)
    if "dy_comment_item" not in _templates():
        base_set = base
    else:
        base_set = materialize({"aweme_id": rec.get("aweme_id"), "cid": base["cid"],
                                "create_time": base["create_time"]},
                               "dy_comment_item", base=base)
    if _stable_hash("dy-cci::%s" % base["cid"]) % 1000 < 404:
        base_set["can_create_item"] = True   # 仅 true 时带键（语料形态）
    return base_set


def _dy_reply_claim_for(comment_id: str) -> int:
    """楼中楼声称值 = f(comment_id) 确定性派生（0-8，语料 90% 首评带 1~8）。"""
    return _stable_hash("dy-reply-total::%s" % comment_id) % 9


def _dy_reply_total_for(comment_id: str) -> int:
    """楼中楼真实可得总数（reply 端点 total）。

    红队 R3-P1-1：list 侧 reply_comment_total 与 reply 端点 total 必须同源
    （此前两个独立随机源，「有回复的评论」点进楼中楼却 0 条）。同源之上再叠
    语料噪声比例：语料实证 claim 5 → reply total 4（声称略高于可得），
    claim<=2 时精确相等——「有回复的评论」点进去保证非 0，claim/可得 ∈ [1.0, 2.0]。
    """
    claim = _dy_reply_claim_for(comment_id)
    if claim >= 3 and _stable_hash("dy-reply-noise::%s" % comment_id) % 4 == 0:
        return claim - 1
    return claim


def _dy_comment_log_pb(rng: np.random.Generator) -> str:
    return ids.dy_logid(rng)


# 红队 R8C-P3-3：comment_common_data——评论公共配置 blob（base64url 形态用户
# 侧令牌，语料 20/133 样本携带、契约 rate=1.0 在录）。按语料形态静态化（语料
# 20 个样本两两不同、长度 3148-4108——合成取一个语料样本常量，静态快照口径
# 与 R7C-P3-1 widgets icon 同先例）。
_DY_COMMENT_COMMON_DATA = (
    "MS4wLjAAAAAA1xdV1azAS_FgR3uUDCATQfO678NzLQVtX7BYw1wbHNalkr0IPH2wyHSm3lMEFRZFipb83Y7jETUDISQdEh-A"
    "chFfmbcxM73XG0_mo2S5WzlWF_XYz6wTFErpUWTaZmLIRcxUBUA7FczEbORGEHy8SrVXB2OFLvlH-xmw0OuSJ_0_6nOFjmYz"
    "PSnaGqkwlhyVUyLApN3Sn_19wqO3Y4uInEcTm1tbw0RxRZf4LY1jncx7xJoxz_-HzO8vTZu_wPh-mNn6N1HcTtEU5bA30IBu"
    "TKSuze9ev1oT0xCvpOn3o948IEs3TJnP90NR59e-m0MWzYZ-c0R2ZVw0bsDlQ7LWFof1n-HidSDMcN-fsP9L8P95rKZY4fil"
    "NxETmwiO7WWLH3SCOLnJEoCE8AOWLthW23GmqLKmL8Ns-5uouQBp9wuL7-X00tDCX1Hu9DQuQGYaq_T59Oumw04au3o6pCcY"
    "2Cro4F3bSUmULqAhbhraCc4y0OS8KJlI_s1pTImnHS018oB22eU2rUIPxJevBsQb43PUmfZq5Mw7Q5WgSOOsGy81gu1vy69D"
    "yIcb1nJJd8hvWfFKX7nLDYNDEGQRTCDzj5fTo3v95e6-M8YghxfIBDWXT1E9rqMiXCXGSGItF-tozEISrttE73TrIPhegp__"
    "5H9WPReMMvZS4410hLTQ08hXIshIAMufqTtWTc6MebrmbC9ZdJpEbJDjr_lQ7LBEE5JcA-12pvYaKZIxBdYwYoTFU7XmzDkF"
    "RrCHJTiOozAGGESl9ylaWbSRdRMp9D5-fVDO256hbPtd7rnPLuFGiiqz9tUK3QjkL4BKZ9_50UKcc9edpedyeOaUUOoyZFrF"
    "WNzUUjaGrmDEeG0eeVLr2MutbtaYZ-80W0NnQK7zI9MtR0M0aVjuo42ydn1OonkVEYyVUgupc2-eBgxHZu4XY2IsGbRy4UK_"
    "P9E5L0vpnV4NCp7NMxOXl6KMbybP3jl1yftcQMARWLriiOQnWEbNPNvBlPfGUODQSsPs1OuAEdy-ECkp3-F1EvHFd3rWDZ99"
    "9hFSu-adka8bkrRNOhooLJ20a69UVgE8tnBbQRK38uEuaHFp0OpRSbCO4ai8X1UwTnDYbeKSiG1jbS_67hWWFapa7ZOAwhlt"
    "dgYoJ9f8lQ5eNH6l6pRp0IDB-aRHm15UcnQN7p_SYEEl0vphSWUS7NMV5YlrLhIze6uMwj7bhmSu8ecVkR5phaSdJEWUXDin"
    "IiVMAKy3Y_-ABTvm0Pq5-w5KGHYZpQpwbyTtte_P_UGY_Rwcb74FuXoQktslxjzFZsIQkg5Th6N2N7RCGakWfF3YOBr6khlk"
    "VQaQkp_FlSdIySzgYaP1ErVTQlzK6OmUeVrL1fUB6xxbxwbGYvFBxD23aCxBhchXsKfPn61mi-8DmiT-Cv2pzJK2PXf-vyeF"
    "k0rlR-B4WWEm9cnJUcKfKm_PP1erQJQblgKrTQa10Clz99YoHM_z3WriiosUaJxOD8EeHzl_KUqZ8Y-4IuL1PSd_yAaWsdmw"
    "I-2yxpIWhBrB6urxYIqxP9orUIt1W4e14L-cM_RVXpuh3I7JWg-GwsUDej2qiW_a214_kvH9pNIeFZj7B1kcQwVACcWXqsw0"
    "ba3iWpEJqZSDWFjfjt9E6lUnqm6bg7IB_fbVgq8jKIMqEuJwLrP2yXiWv6ZXBH5BFm2K3v51rjTh5eEKjBguyI9nm-CDaVl2"
    "IK53GbvxnEplc8ZK-0SI2rasL2yptUUnvhrcuhWA919E5YJzYqz6rOpG91aE2J0eflaefuq21HJpsPQ7mV4ZZm647iHE6oxJ"
    "jxfZ8SAl7kxoxFton_iPSWekedHOQKMaMIwFYkk2r89tXHM31vtTdU5SOdQpAkFI11U0w5Y_RlRWyhPmHxK6NXrziYHjFb7D"
    "VGBjOGvfMLRSu241DCyS-_bFrn4L-77QvOpc5-UEur7F5H547fFNqsmZxg9N1jIr9NHaCsuoOqRVzMfpnTgV851EjBD8JqhI"
    "zR0rRs_tbSEaWIB9pr1NuAf6-kdWQgSbgXzRmWQFHuHRiKP7wRnSWZxdrda5uM8gjFR-b6N4hii1Fxyv_ERqY3sxz_wdP_xU"
    "dTYQpKA04_eBhPgSEz4sJ77o_W8827KMLaAf1ufH8TEnAMEQQ-G8rB6d9YC-S5G4V8OZtEqWwNpCJ-nm8-TOxSPUDDiBORsu"
    "21Q1RanXrTdLTt6eI-Sfyl2HJ-Gg8OLggtChY5vJ11lkWQxJC3yEdlNwcY3J3PJXyCTmEGQrncNRrCf8E197LLIlM2117pFn"
    "GBpxNhq3cIMCZ4py1-bwHQrbj0MmGPMb233SJ2p3KnZ-ePTVcav3xJuvX1iHLS_NDAcFkjIz80n8KO7Sz-h3sbEnAvUT3EVe"
    "hkEsfvfl_gIpQkKGi9Cz7Lji31qKe-XC-DJkqyIibyRl6kijsO1uUqWY3D4rmYPuDHteicbwTj-78Ssg7GTWFbPawrcffoYQ"
    "RQwKTnyb5KpugIreJOjSlmEJC40DWlYjUy6qW5vJkfyHdO-OC3hSYnkIjZnTeHhzpNXPuqnHlOA5lOViP5OQDVORJwcKCHTo"
    "sQKq2CP2CRVATD_Kb_puLBiE3hMNNJHrTISghCOZXkduIy6JuGDQBNbNx1vyDKBw9qzbNupw6DqrMT9bojhSO7h5KmmBtrf8"
    "FWZgj7zVrtLWmxlf1boPG0i05QbA_fMyO1TPl4H5E1fwDeVsouPRBhnhWRXvZuIXPue-fvjDCsXFfkRS3HveAVrFzU4UX7mU"
    "2v_KQ5NuDfWpWRji5PqdOpk1bREKP6aujqTP_FRpDNQkcVpiXld4r_g-lZRCi5f13Y7amv3hGxHPCW0f0SuMhQbWVwyJ-7Gg"
    "6TlQXuMdw3Htn-fQIbE7Bm0f-o0dqF3J6ljHPd4h1UvY9wxNJmDrTZ8UfqAG2X6GyW9liJ7TflBqOTRpydc7UCDiBP8zTHfe"
    "vRxk_c_bZxPE5fj9TooZcL81NMDRbOaAFdOHrXFpN3KSTwdbC08uZmiFFtBzKcPn_T4q_U0-tcYoo10RkAD-q36FIegzuuiF"
    "I90jSGKXKtDN8wAcZfU0wz8ra1OOkC4LMTrayym3d7KXk5wlI1_oAR9d1niRu9wFZNkfgszWSk9j"
)


def _dy_comment_envelope(rng: np.random.Generator, comments, cursor: int, has_more: int,
                         total: int) -> dict:
    """comment/list 18 键信封（语料实证：comment_config/total/extra/.../sort_tags_report_map；
    R8C-P3-3 补 comment_common_data——契约 rate=1.0 在录、语料 20/133 样本携带）。"""
    return {
        "status_code": 0,
        "comments": comments,
        "cursor": cursor,
        "has_more": has_more,
        "reply_style": 2,
        "total": total,
        "comment_common_data": _DY_COMMENT_COMMON_DATA,
        "extra": {"now": _now_ms(), "fatal_item_ids": None, "scenes": None},
        "log_pb": {"impr_id": _dy_comment_log_pb(rng)},
        "hotsoon_filtered_count": 0,
        "user_commented": -1,
        "fast_response_comment": {
            "constant_response_words": ["赞", "比心", "加油"],
            "timed_response_words": ["早上好", "下午好", "晚上好"],
        },
        "comment_config": {},
        "general_comment_config": {},
        "show_management_entry_point": 0,
        "folded_comment_count": 0,
        "comment_insert_bars": None,
        "sort_tags_report_map": "{\"eco_level_10\":\"interest_comment_v4_view\","
                                "\"eco_level_8\":\"interest_comment_v3_view\"}",
    }


def render_douyin_comment_list(dataset: DatasetReader, line_no: int, cursor: int,
                               count: int) -> dict:
    """/aweme/v1/web/comment/list：数值游标切片（cursor 0→10→20 推进，红队 P1-D1）。

    语料实证：resp cursor = req cursor + max(count,10)；has_more = cursor+count < total；
    total = 作品 comment_count。评论按 (aweme_id, 序号) 确定性合成（数据集实体不含评论）。

    红队 R4-P2-1：评论生成种子改由 (aweme_id, 序号) 确定性派生（此前为页级 rng +
    墙钟 now——同 (aweme_id,cursor) 相隔数秒两次请求 cid 大面积重掷、create_time
    随墙钟漂移、reply_comment_total 逐次变）。时间基准改用数据集 anchor_time，
    与 search 侧同口径：同一 (aweme_id, cursor, count) 任意两次渲染逐字段一致；
    同一评论序号在任何翻页路径（换 count/起点）下 cid 恒定。
    红队 R4-P3-1：count<=0 → 0 条（cursor 不前进）；R4-P3-3：cursor 越界 → 空页
    且 cursor 收敛到 total（不回显 cursor+count）。
    """
    rec = dataset.read(line_no)
    total = int((rec.get("statistics") or {}).get("comment_count") or 0)
    aweme_id = str(rec.get("aweme_id") or "")
    now = dataset.anchor_ts or int(time.time())
    rng = _page_rng(dataset, f"dy-cmt-env-{aweme_id}-{cursor // max(count, 1)}")
    if cursor >= total:  # 越界游标：空页 + cursor 收敛（真站尾页 cursor 不超过 total）
        return _dy_comment_envelope(rng, [], min(cursor, total), 0, total)
    n = max(0, min(count, total - cursor))
    comments = [_dy_comment_item(_page_rng(dataset, f"dy-cmt2-{aweme_id}-{cursor + i}"),
                                 rec, cursor + i, total, now_ts=now, dataset=dataset)
                for i in range(n)]
    if count <= 0:  # R4-P3-1：count=0/-1 → 0 条，游标原地
        next_cursor, has_more = cursor, (1 if cursor < total else 0)
    else:
        next_cursor = cursor + max(count, 10)
        has_more = 1 if next_cursor < total else 0
    return _dy_comment_envelope(rng, comments, next_cursor, has_more, total)


def render_douyin_comment_list_missing() -> dict:
    """aweme_id 不存在：HTTP 200 + comments:null + status_code:0（真站信封族，P1-D1）。"""
    rng = np.random.default_rng([int(time.time()) & 0xFFFF, 37, 43])
    env = _dy_comment_envelope(rng, None, 0, 0, 0)
    return env


def render_douyin_comment_list_reply(dataset: DatasetReader, line_no: int, comment_id: str,
                                     cursor: int, count: int) -> dict:
    """/aweme/v1/web/comment/list/reply：8 键信封（契约实证 comments/cursor/extra/has_more/
    log_pb/merge_cursor/status_code/total）。

    红队 R4-P2-1：与 comment/list 同口径确定性派生——子评论种子 = f(comment_id, 序号)、
    时间基准 = 数据集 anchor_time，跨请求/跨翻页路径 cid 与 create_time 恒定；
    count<=0 → 0 条、cursor 越界 → 空页收敛（R4-P3-1/P3-3 同族口径）。
    """
    rec = dataset.read(line_no)
    aweme_id = str(rec.get("aweme_id") or "")
    rng = _page_rng(dataset, f"dy-rpl-env-{aweme_id}-{comment_id}-{cursor // max(count, 1)}")
    total = _dy_reply_total_for(comment_id)  # 与 list 侧 reply_comment_total 同源（保真修复轮）
    now = dataset.anchor_ts or int(time.time())
    if cursor >= total:
        cursor = min(cursor, total)
        return {
            "status_code": 0, "comments": [], "cursor": cursor,
            "extra": {"now": _now_ms(), "fatal_item_ids": None, "scenes": None},
            "has_more": 0, "log_pb": {"impr_id": _dy_comment_log_pb(rng)},
            "merge_cursor": "", "total": total,
        }
    n = max(0, min(count, total - cursor))

    def _build_sub(idx: int) -> dict:
        # R9A-P2-2：子评论时间 = 根评论时间 + 正偏移。根评论 create_time 从其
        # 雪花 cid 解码：cid = (ctime-δ)<<32 | rand32（δ∈[0,4)，ids.dy_snowflake_id）
        # → decoded = ctime-δ ≤ ctime < decoded+4，取 decoded+4 作采样下界即严格
        # 晚于根实际时间；comment_ctime 的 [下界+30, anchor] 钳制给出正偏移分布。
        try:
            root_lo = (int(comment_id) >> 32) + 4
        except (TypeError, ValueError):
            root_lo = 0
        c = _dy_comment_item(_page_rng(dataset, f"dy-rpl2-{comment_id}-{idx}"),
                             rec, idx, total, now_ts=now, dataset=dataset,
                             text_salt="reply::%s" % comment_id,
                             min_ts=root_lo if root_lo > 0 else None)
        c["reply_id"] = comment_id
        c["level"] = 2  # 楼中楼子评论层级（真站 reply 条目 level=2）
        if "dy_reply_item" in _templates():  # reply 专属高频键（root_comment_id 等，S2）
            c = materialize({"aweme_id": rec.get("aweme_id"), "cid": c["cid"],
                             "reply_id": comment_id, "create_time": c["create_time"]},
                            "dy_reply_item", base=c)
        # R9A-P3-3：reply 面同语义——语料 10 带/20 缺（无 present-null），
        # 仅 true 时携带键（~33% 按 cid 派生）；reply_to_user_* 三键保持（语料 null 形态）
        if _stable_hash("dy-cci::%s" % c.get("cid", "")) % 1000 < 333:
            c["can_create_item"] = True
        c.setdefault("reply_to_user_sec_id", None)
        c.setdefault("reply_to_userid", None)
        c.setdefault("reply_to_username", None)
        return c

    # R7A-P3-5 楼中楼二层嵌套：语料 30 条 sub 中 11 条（37%）reply_to_reply_id≠0
    # （「回复中的回复」——reply_id 仍为线程锚（父评论），reply_to_reply_id 指向
    # 同线程前序 sub）。翻页首条的前序评论用同种子重建（cid 跨页恒定，R4-P2-1）。
    prev = _build_sub(cursor - 1) if cursor > 0 else None
    comments = []
    for i in range(n):
        idx = cursor + i
        c = _build_sub(idx)
        if prev is not None and \
                _stable_hash("dy-r2r::%s::%d" % (comment_id, idx)) % 1000 < 450:
            c["reply_to_reply_id"] = str(prev.get("cid") or "0")
        prev = c
        comments.append(c)
    next_cursor = cursor + max(count, 3) if count > 0 else cursor
    return {
        "status_code": 0,
        "comments": comments,
        "cursor": next_cursor,
        "extra": {"now": _now_ms(), "fatal_item_ids": None, "scenes": None},
        "has_more": 1 if next_cursor < total else 0,
        "log_pb": {"impr_id": _dy_comment_log_pb(rng)},
        "merge_cursor": "",
        "total": total,
    }


_XHS_KEYWORDS = ["美食探店", "旅行攻略", "健身打卡", "穿搭分享", "家居好物", "数码测评"]

XHS_COMMENT_PAGE_SIZE = 10  # 语料实证：comment/page 每页 10 条

# xhs 评论文案（R5A-P1-2）：数据集内嵌评论是「样例集」，总量以 interact_info.comment_count
# 为准——超出内嵌部分按 (note_id, 序号) 确定性合成；文本全部走 commentext
# 组合引擎（与 sites/xhs.py 内嵌评论共享 salt=note_id 的 seq 空间，同笔记 0 重复）
_XHS_CMT_IP = ["江苏", "上海", "北京", "广东", "浙江", "四川", "山东",
               "湖北", "福建", "中国香港", "日本", "新加坡"]


# xhs @提及出现率（R7A-P3-3：语料顶层评论 @ 3.2%（at_users 非空 4.6%——提及列表
# 与正文 @ 配套）；子评论 ~2%）
_XHS_AT_PERMILLE = 35
_XHS_SUB_AT_PERMILLE = 20


def _xhs_at_user_entry(rng: np.random.Generator, user: dict) -> dict:
    """at_users 元素形态（语料/模板实证：user_id/nickname/ai_agent/xsec_token）。"""
    return {
        "user_id": str(user.get("user_id") or ""),
        "nickname": str(user.get("nickname") or ""),
        "ai_agent": False,
        "xsec_token": ids.xhs_xsec_token(rng),
    }


def _xhs_sub_claim_for(comment_id: str) -> int:
    """xhs 子评论声称值 = f(comment_id) 确定性派生（0-9，语料实证 claim '9'）。"""
    return _stable_hash("xhs-sub-total::%s" % comment_id) % 10


def _xhs_synth_comment(dataset, rec: dict, idx: int) -> dict:
    """第 idx（≥内嵌数）条评论：确定性合成（同 note 同序号永远一致）。

    红队 R3-P2-1/R3-P1-1：sub_comment_count 与 /comment/sub/page 可得数同源
    （_xhs_sub_claim_for）；claim>0 时内嵌 1 条 + 游标续读（语料形态：
    claim '9' 字符串、内嵌 1、sub/page 翻尽 9）。
    R5A-P1-2：文本走 commentext（salt=note_id，与内嵌评论同一 seq 空间）；
    R5A-P2-5：评论者取数据集作者实体（user_posted 可回查）。
    """
    note_id = str(rec.get("id") or "")
    seed = _stable_hash("xhs-cmt::%s::%d" % (note_id, idx))
    rng = np.random.default_rng([seed & 0x7FFFFFFF, idx])
    # 发布时间基准：note_id 自带 hex(ts)（记录无外显 create_time，避免墙钟漂移）
    pub_ms = int(rec.get("create_time") or 0) or (
        int(note_id[0:8], 16) * 1000 if len(note_id) == 24 else _now_ms())
    # R6A-P2-2：延迟重尾混合（旧 (seed % 30d) 均匀 → p50≈1.5h、0%>1 周）
    from synthgen.distengine import comment_ctime
    ctime = comment_ctime(rng, pub_ms // 1000,
                          dataset.anchor_ts if dataset is not None and dataset.anchor_ts
                          else pub_ms // 1000 + 30 * 86400) * 1000
    cid = ids.xhs_ts_hex_id(rng, ctime // 1000)
    sub_claim = _xhs_sub_claim_for(cid)
    commenter = _dataset_commenter(dataset, "xhs", "%s::%d" % (note_id, idx), idx)
    if not commenter:
        commenter = {"user_id": ids.hex_id(rng, 24),
                     "nickname": "小红书用户", "avatar": ""}
    content = commentext.comment_text("xhs", idx, note_id)
    at_users = []
    # R7A-P3-3：@提及（指向笔记作者，语料形态「@昵称 正文」+ at_users 配套）
    if _maybe_at_prefix("xhs::%s::%d" % (note_id, idx), _XHS_AT_PERMILLE):
        note_author = ((rec.get("note_card") or {}).get("user") or {})
        target = note_author if note_author.get("nickname") else commenter
        if target.get("nickname"):
            content = "@%s %s" % (target["nickname"], content)
            at_users = [_xhs_at_user_entry(rng, target)]
    c = {
        "id": cid,
        "note_id": note_id,
        "content": content,
        "create_time": ctime,
        "like_count": str(int(rng.integers(0, 900))),
        "liked": False,
        "invalid": False,
        "ip_location": ids.pick(rng, _XHS_CMT_IP),
        "pictures": [],
        "at_users": at_users,
        "show_tags": [],
        "status": 0,
        "sub_comment_count": str(sub_claim),
        "sub_comment_cursor": "",
        "sub_comment_has_more": False,
        "user_info": {
            "user_id": commenter["user_id"],
            "nickname": commenter["nickname"],
            "image": commenter.get("avatar") or commenter.get("image")
                     or "https://sns-avatar-qc.xhscdn.com/avatar/%s"
                        % ids.hex_id(rng, 24),
            "xsec_token": ids.xhs_xsec_token(rng),
            "ai_agent": False,
        },
    }
    subs = _xhs_sub_comments_full(dataset, rec, c, sub_claim)
    if subs:
        c["sub_comments"] = subs[:1]  # 语料形态：大计数内嵌 1 条
        c["sub_comment_has_more"] = sub_claim > 1
        if sub_claim > 1:
            c["sub_comment_cursor"] = subs[0]["id"]
    return c


def _xhs_synth_sub_comment(dataset, rec: dict, root: dict, idx: int,
                           prev: dict | None = None) -> dict:
    """root 的第 idx 条子评论：确定性合成（sub/page 翻页数据源）。

    R7A-P2-2：target_comment 按同源派生——默认指向真实父评论（root）id+user_info；
    ~2.3% 概率（语料 130 个嵌入 sub 中 3 个）挂二层嵌套指向同线程前序 sub
    （prev）。旧实现无该键 → comment_page 嵌入 sub 的 target_comment 落到
    模板静态占位（非 hex 常量、跨笔记复现），sub/page 则键缺失。
    """
    note_id = str(rec.get("id") or "")
    root_id = str(root.get("id") or "")
    seed = _stable_hash("xhs-sub::%s::%s::%d" % (note_id, root_id, idx))
    rng = np.random.default_rng([seed & 0x7FFFFFFF, idx + 7])
    root_ct = int(root.get("create_time") or 0) or _now_ms()
    ctime = int(root_ct + (seed % (14 * 86400)) * 1000)  # 子评论晚于父评论
    if dataset is not None and dataset.anchor_ts:
        ctime = min(ctime, int(dataset.anchor_ts) * 1000)
    commenter = _dataset_commenter(dataset, "xhs", "sub::%s::%s" % (note_id, root_id), idx)
    if not commenter:
        commenter = {"user_id": ids.hex_id(rng, 24),
                     "nickname": "小红书用户", "avatar": ""}
    content = commentext.comment_text("xhs", idx, "sub::%s::%s" % (note_id, root_id))
    at_users = []
    target = root
    if prev is not None and \
            _stable_hash("xhs-sub-nest::%s::%s::%d" % (note_id, root_id, idx)) % 1000 < 25:
        target = prev   # 二层嵌套：指向前序 sub（语料 3/130 兄弟指向形态）
    if _maybe_at_prefix("xhs-sub::%s::%s::%d" % (note_id, root_id, idx),
                        _XHS_SUB_AT_PERMILLE):
        tu = (target.get("user_info") or {})
        if tu.get("nickname"):
            content = "@%s %s" % (tu["nickname"], content)
            at_users = [_xhs_at_user_entry(rng, tu)]
    tgt_user = dict(target.get("user_info") or {})
    tgt_user = {k: tgt_user[k] for k in ("user_id", "nickname", "image",
                                         "xsec_token", "ai_agent") if k in tgt_user}
    return {
        "id": ids.xhs_ts_hex_id(rng, ctime // 1000),
        "note_id": note_id,
        "content": content,
        "create_time": ctime,
        "like_count": str(int(rng.integers(0, 300))),
        "liked": False,
        "invalid": False,
        "ip_location": ids.pick(rng, _XHS_CMT_IP),
        "pictures": [],
        "at_users": at_users,
        "show_tags": [],
        "status": 0,
        "target_comment": {"id": str(target.get("id") or root_id),
                           "user_info": tgt_user or dict(root.get("user_info") or {})},
        "user_info": {
            "user_id": commenter["user_id"],
            "nickname": commenter["nickname"],
            "image": commenter.get("avatar") or commenter.get("image")
                     or "https://sns-avatar-qc.xhscdn.com/avatar/%s"
                        % ids.hex_id(rng, 24),
            "xsec_token": ids.xhs_xsec_token(rng),
            "ai_agent": False,
        },
    }


def _xhs_sub_comments_full(dataset, rec: dict, root: dict, claim: int) -> list:
    """root 评论的全量子评论（内嵌在前 + 确定性合成补足到 claim，语料 claim=可得）。"""
    if claim <= 0:
        return []
    subs = [dict(s) for s in (root.get("sub_comments") or []) if isinstance(s, dict)]
    for idx in range(len(subs), claim):
        subs.append(_xhs_synth_sub_comment(dataset, rec, root, idx,
                                           prev=(subs[idx - 1] if idx > 0 else None)))
    return subs[:claim]


def _xhs_ensure_at_users(c: dict, note_id: str) -> dict:
    """R9A-P3-4：正文 @提及 ↔ at_users 强耦合（语料 text_only=0/22 样本）。

    泄漏面：xhs_comment_item 模板 sub_comments[].at_users=[]（众数剪枝产物）
    在 graft() 空模板分支会把数据集已耦合的 at_users 条目抹成空——正文仍带
    @昵称 前缀而列表为空（19% 断裂）。此处物化后兜底：按正文 @词元与现有
    条目昵称求差集，缺项确定性回补（user_id=24hex / nickname / ai_agent=false /
    xsec_token=50 opaque，与语料元素形态一致）。根/子评论与两个端点统一过检。"""
    txt = str(c.get("content") or "")
    ats = re.findall(r"@([^\s@，。！？,]+)", txt)
    if not ats:
        return c
    have = {str((a or {}).get("nickname") or "") for a in (c.get("at_users") or [])}
    miss = [a for a in ats if a and a not in have]
    if not miss:
        return c
    rng = np.random.default_rng(
        [_stable_hash("xhs-at::%s::%s" % (note_id, c.get("id"))) & 0x7FFFFFFF, 7])
    out = [a for a in (c.get("at_users") or []) if isinstance(a, dict)]
    for nick in miss:
        h = _stable_hash("xhs-at-uid::%s" % nick)
        out.append({"user_id": "%024x" % (h % (2 ** 96)), "nickname": nick,
                    "ai_agent": False, "xsec_token": ids.xhs_xsec_token(rng)})
    c["at_users"] = out
    return c


def _xhs_note_declared_comments(rec: dict) -> int:
    try:
        return int((((rec.get("note_card") or {}).get("interact_info") or {})
                    .get("comment_count")) or 0)
    except (TypeError, ValueError):
        return 0


def _xhs_note_comments(dataset, rec: dict) -> list:
    """笔记全量顶层评论：内嵌样例集 + 超出部分按 (note_id, 序号) 确定性合成。

    comment/page 与 comment/sub/page 共用同一构建（红队 R3-P2-1：
    两端点必须看到同一份评论列表，sub 计数/游标才能对得上）。
    """
    comments = [dict(c) for c in (rec.get("comments") or []) if isinstance(c, dict)]
    declared = _xhs_note_declared_comments(rec)
    for idx in range(len(comments), max(len(comments), declared)):
        comments.append(_xhs_synth_comment(dataset, rec, idx))
    return comments


def xhs_empty_search_response() -> dict:
    """真站空结果信封（红队 P1-X1）：data 无 items 键 + has_more:false（95 字节实证）。"""
    return {"code": 0, "msg": "成功", "success": True,
            "data": {"has_more": False, "request_dqa_instant": True}}


def _xhs_session_id(rng: np.random.Generator) -> str:
    """session_id：契约实证 36 位 uuid4 形态（确定性 4×32 位采样拼装）。"""
    import uuid
    w = [int(x) for x in rng.integers(0, 2**32, size=4)]
    val = (w[0] << 96) | (w[1] << 64) | (w[2] << 32) | w[3]
    return str(uuid.UUID(int=val, version=4))


def render_xhs_search_notes(dataset: DatasetReader, page_no: int, page_size: int,
                            search_id: str | None = None,
                            keyword: str | None = None,
                            start: int | None = None) -> tuple[dict, dict]:
    """start（R2-P2-1）：绝对起始下标 = 轮转基×DEFAULT_PAGE_SIZE + (page-1)×page_size，
    page_size 混用时窗口不再漂移（页号模式为兼容保留）。
    R6A-P2-1：映射关键词的数据窗口改为类目偏置采样（_search_records）。"""
    records = _search_records(dataset, "xhs", keyword, start, page_no, page_size)
    rng = _page_rng(dataset, f"xhs-notes-{page_no}" if start is None else f"xhs-notes-a{start}")
    if search_id is None:
        # search_id 形态（红队 P2-X4，语料实证）：p1=21 位；p2+ = <21位>@<22位> 拼接
        search_id = ids.opaque(rng, 21 if page_no == 1 else 22)
        if page_no > 1:
            search_id = ids.opaque(_page_rng(dataset, f"xhs-notes-sid1-{page_no}"), 21) \
                + "@" + search_id
    if keyword is None:
        keyword = _XHS_KEYWORDS[int(rng.integers(0, len(_XHS_KEYWORDS)))]
    # 请求描述：契约 body_params 必填 12 项全量（P3 修复：keyword/session_id/message_id/ext_flags/geo 等）
    request = {
        "keyword": keyword,
        "page": page_no,
        "page_size": page_size,
        "search_id": search_id,
        "sort": "general",
        "note_type": 0,
        "ext_flags": [],
        "geo": "",
        "image_formats": ["jpg", "webp", "avif"],
        "message_id": "sending",
        "session_id": _xhs_session_id(rng),
    }
    items = []
    for rec in records:
        item = {k: v for k, v in rec.items() if k != "comments"}  # 评论走 comment_page
        items.append(item)
        if rng.random() < 0.09:  # 契约实证：items 中混排 hot_query 卡（~9%）
            items.append(_xhs_hot_query(rng, rec))
    # R4-P3-2：条数口径 =「note 条数 = page_size（调用方已钳到语料上限 20），
    # hot_query 卡按语料形态追加（语料实证 page_size=20 → items 21-26）」——
    # note 集恒等于数据窗口切片（e2e note_ids 逐一对齐），卡片不占 note 配额。
    next_pos = (start if start is not None else (page_no - 1) * page_size) + page_size
    response = {
        "code": 0,
        "msg": "成功",
        "success": True,
        "data": {
            "has_more": next_pos < len(dataset),
            "items": items,
            "request_dqa_instant": True,
        },
    }
    return request, response


def _xhs_hot_query(rng, ref_rec: dict) -> dict:
    word = ref_rec["note_card"].get("display_title") or "大家都在搜"
    queries = []
    for i in range(4):
        w = word if i == 0 else f"{word[:4]}{i}"
        queries.append({
            "cover": f"https://sns-na-i1.xhscdn.com/{ids.hex_id(rng, 16)}",  # 红队 P3-6：https 形态
            "id": w,
            "name": w,
            "search_word": w,
        })
    return {"hot_query": {"queries": queries, "source": 2, "title": "大家都在搜",
                          "word_request_id": ids.digits_str(rng, 50)},
            "model_type": "hot_query"}


def render_xhs_comment_page(dataset: DatasetReader, note_line_no: int, cursor: str = "") -> tuple[dict, dict]:
    """对某条笔记吐评论页：游标切片 + 确定性推进（红队 P1-X2 修复）。

    旧实现：每页全量回同一份评论、cursor 每次随机、has_more 恒真 → 无限重复采集。
    现按语料真站形态：cursor = 上页末条评论 id（24hex，依合成数据派生、稳定）；
    按游标对评论列表切片（每页 ≤10 条）；取尽 has_more=false；未知游标视为取尽（不回绕）。
    保真修复轮：评论总量以 note_card.interact_info.comment_count 为准（内嵌评论
    是样例集，超出部分按 (note_id, 序号) 确定性合成）——内嵌 6 条/计数 21 的笔记
    原只回 6 条且 p2 空（truth 语料页页 10 条 → L3 长度带失分）。
    """
    rec = dataset.read(note_line_no)
    comments = _xhs_note_comments(dataset, rec)
    if cursor:
        # 游标定位：本页从 cursor 所指评论的下一条开始；未命中（乱传/跨笔记）→ 取尽
        start = None
        for i, c in enumerate(comments):
            if str(c.get("id") or "") == cursor:
                start = i + 1
                break
        if start is None:
            comments = []
        else:
            comments = comments[start:]
    page = comments[:XHS_COMMENT_PAGE_SIZE]
    has_more = len(comments) > len(page)
    next_cursor = str(page[-1].get("id") or "") if page else ""
    rng = _page_rng(dataset, f"xhs-cmt-{note_line_no}-{cursor or 'p0'}")
    # R7A-P2-2：先捕获 base 的 sub 级 target_comment（数据集内嵌/合成均为同源
    # 派生——父评论或兄弟 sub），物化后原样回接（模板静态占位不覆盖真实引用）；
    # base 无 sub_comments 的评论（claim=0）物化后压回空列表——否则模板数组
    # 分支会凭空物化出一条「 scrambling id + 占位 target」的假子评论
    _base_sub_targets = [
        [dict(s.get("target_comment")) if isinstance(s.get("target_comment"), dict) else None
         for s in (c.get("sub_comments") or [])] for c in page
    ]
    _base_no_subs = [not (c.get("sub_comments") or []) for c in page]
    if "xhs_comment_item" in _templates():  # S2：评论条目按真值模板物化（user_info 等对齐）
        note_ref = {"id": rec.get("id"), "note_id": rec.get("id"),
                    "create_time": rec.get("create_time")}
        page = [materialize(note_ref, "xhs_comment_item", base=c) for c in page]
    for c, tgts, no_subs in zip(page, _base_sub_targets, _base_no_subs):
        if no_subs:
            c["sub_comments"] = []   # 语料形态之一：key 在、列表空、count '0'（53 样本）
            continue
        for s, tgt in zip(c.get("sub_comments") or [], tgts):
            s["target_comment"] = tgt or {
                "id": str(c.get("id") or ""),
                "user_info": dict(c.get("user_info") or {})}
    # R9A-P3-4：正文 @ ↔ at_users 强耦合（根 + 内嵌子统一过检，见 helper 注）
    for c in page:
        _xhs_ensure_at_users(c, str(rec.get("id") or ""))
        for s in (c.get("sub_comments") or []):
            if isinstance(s, dict):
                _xhs_ensure_at_users(s, str(rec.get("id") or ""))
    # 请求描述：契约 query_params 必填 5 项全量（P3 修复：image_formats/xsec_token）
    request = {
        "note_id": rec["id"],
        "cursor": cursor,
        "top_comment_id": "",
        "image_formats": "jpg,webp,avif",
        "xsec_token": ids.opaque(rng, 46),  # 契约实证 46 位（区别于 note 卡的 50 位 token）
    }
    response = {
        "code": 0,
        "msg": "成功",
        "success": True,
        "data": {
            "comments": page,
            "cursor": next_cursor,
            "has_more": has_more,
            "time": _now_ms(),
            "user_id": ids.hex_id(rng, 24),
            "xsec_token": ids.xhs_xsec_token(rng),
        },
    }
    return request, response


def render_xhs_comment_sub_page(dataset: DatasetReader, note_line_no: int,
                                root_comment_id: str, cursor: str = "",
                                num: int = 10) -> tuple[dict, dict]:
    """GET /api/sns/web/v2/comment/sub/page（红队 R3-P2-1：子评论链接线）。

    语料形态：query note_id/root_comment_id/num=10/cursor/image_formats/top_comment_id；
    data{comments, cursor(24hex), has_more, time, user_id, xsec_token}；子评论条目带
    target_comment（父评论引用）。可得总数 = 顶层 sub_comment_count 声称值
    （语料 claim '9' → sub/page 翻尽 9，claim=可得）；内嵌 1 条在前、
    超出部分按 (note_id, root_id, 序号) 确定性合成；游标同 comment/page 语义。
    """
    rec = dataset.read(note_line_no)
    rng = _page_rng(dataset, f"xhs-sub-{note_line_no}-{root_comment_id}-{cursor or 'p0'}")
    root = None
    for c in _xhs_note_comments(dataset, rec):
        if str(c.get("id") or "") == str(root_comment_id):
            root = c
            break
    subs: list = []
    if root is not None:
        try:
            claim = int(str(root.get("sub_comment_count") or "0"))
        except (TypeError, ValueError):
            claim = len(root.get("sub_comments") or [])
        subs = _xhs_sub_comments_full(dataset, rec, root, claim)
    if cursor and subs:
        start = None
        for i, s in enumerate(subs):
            if str(s.get("id") or "") == cursor:
                start = i + 1
                break
        subs = subs[start:] if start is not None else []
    page = subs[:max(1, min(num, 30))]
    has_more = len(subs) > len(page)
    next_cursor = str(page[-1].get("id") or "") if page else ""
    root_user = (root or {}).get("user_info") or {}
    # R7A-P2-2：base 的 sub 级 target_comment 为同源派生（root 或兄弟 sub），
    # 物化后原样回接——旧实现物化（模板无顶层 target_comment 键）把该键丢掉，
    # sub/page 端点整键缺失（语料每个 sub 都有）
    _base_targets = [dict(s.get("target_comment")) if isinstance(s.get("target_comment"), dict)
                     else None for s in page]
    out = []
    for s in page:
        s = dict(s)
        s["target_comment"] = {"id": str((root or {}).get("id") or ""), "user_info": root_user}
        out.append(s)
    if "xhs_comment_item" in _templates():
        note_ref = {"id": rec.get("id"), "note_id": rec.get("id"),
                    "create_time": rec.get("create_time")}
        out = [materialize(note_ref, "xhs_comment_item", base=s) for s in out]
    for s, tgt in zip(out, _base_targets):
        s["target_comment"] = tgt or {
            "id": str((root or {}).get("id") or ""), "user_info": root_user}
    # R8C-P3-5：sub/page 评论项为纯评论对象——真站该项无嵌套游标族
    # （sub_comment_count/sub_comment_cursor/sub_comment_has_more/sub_comments
    # 属顶层 comment/page 形态；语料 sub/page 项 0 携带，模板按 comment/page 提取
    # 而被 base 值带回，此处剥除）
    for s in out:
        for k in ("sub_comment_count", "sub_comment_cursor",
                  "sub_comment_has_more", "sub_comments"):
            s.pop(k, None)
        # R9A-P3-4：sub/page 面同检（正文 @ ↔ at_users 强耦合）
        _xhs_ensure_at_users(s, str(rec.get("id") or ""))
    request = {
        "note_id": rec["id"],
        "root_comment_id": str(root_comment_id),
        "num": str(max(1, min(num, 30))),
        "cursor": cursor,
        "image_formats": "jpg,webp,avif",
        "top_comment_id": "",
        "xsec_token": ids.xhs_xsec_token(rng),
    }
    response = {
        "code": 0,
        "msg": "成功",
        "success": True,
        "data": {
            "comments": out,
            "cursor": next_cursor,
            "has_more": has_more,
            "time": _now_ms(),
            "user_id": ids.hex_id(rng, 24),
            "xsec_token": ids.xhs_xsec_token(rng),
        },
    }
    return request, response


def render_xhs_feed_note_card(rec: dict) -> dict:
    """R8C-P3-5：v1/feed 的 note_card 形态（详情流上下文）。

    语料该端点 note_card 与搜索卡模板不同（逐样本差 72 路径：image_list 直出
    url/url_default/url_pre/file_id/live_photo/stream/trace_id、desc/title/note_id/
    tag_list/at_user_list/ip_location/time/last_update_time/share_info/
    interact_info.{followed,nice_count,relation,share_count}/video 族；且不带搜索卡
    的 corner_tag_info/cover/display_title/interact_info.shared_count/user.nick_name）。
    改用语料直提模板 xhs_feed_note_card 物化（数据集 note_card 同路径值优先，
    模板定键集——多余键自然消失）；模板缺失时回退数据集 note_card 原样。"""
    nc = (rec or {}).get("note_card") or {}
    if "xhs_feed_note_card" in _templates():
        return materialize(rec, "xhs_feed_note_card", base=nc)
    return nc


def render_xhs_user_posted(dataset: DatasetReader, line_nos: list,
                           user_id: str, cursor: str = "", num: int = 30
                           ) -> tuple[dict, dict]:
    """GET /api/sns/web/v1/user_posted（红队 R3-P2-3 作者链最小实现）。

    语料形态：query num/cursor/user_id/image_formats/xsec_token/xsec_source；
    data{cursor, has_more, notes:[{type, display_title, user, cover, interact_info,
    note_id, time, xsec_token}]}。数据源 = 数据集该作者的全部作品（index.author_id）。
    """
    recs = [dataset.read(i) for i in line_nos]
    notes = []
    for r in recs:
        nc = r.get("note_card") or {}
        notes.append({
            "type": "normal",
            "display_title": nc.get("display_title") or "",
            "user": {k: v for k, v in (nc.get("user") or {}).items()
                     if k in ("nick_name", "avatar", "user_id", "nickname")},
            "cover": {"width": 1920, "height": 1280, "url": "", "trace_id": "",
                      "info_list": [{"image_scene": "WB_PRV", "url": "",
                                     "width": 720, "height": 960, "image_scene_dict": {}}]},
            "interact_info": nc.get("interact_info") or {},
            "note_id": r.get("id"),
            "time": int(r.get("create_time") or 0),
            "xsec_token": r.get("xsec_token") or "",
        })
    start = 0
    if cursor:
        for i, n in enumerate(notes):
            if str(n["note_id"]) == cursor:
                start = i + 1
                break
        else:
            start = len(notes)
    page = notes[start:start + num]
    has_more = start + num < len(notes)
    next_cursor = str(page[-1]["note_id"]) if page else ""
    rng = _page_rng(dataset, f"xhs-posted-{user_id}-{cursor or 'p0'}")
    request = {
        "user_id": user_id, "num": str(num), "cursor": cursor,
        "image_formats": "jpg,webp,avif",
        "xsec_token": ids.xhs_xsec_token(rng), "xsec_source": "pc_search",
    }
    return request, {
        "code": 0, "msg": "成功", "success": True,
        "data": {"notes": page, "cursor": next_cursor, "has_more": has_more},
    }
# R12A-P3-3：ks feed 可选 authorStatement——语料携带率 search/feed 108/706=15.3%、
# profile/feed 196/403=48.6%（photo 级属性：同人作品页非 0/100 二值）；文案取语料
# 5 变体按频次加权池（AI 155 / 虚构 73 / 转载 57 / 观点 11 / 危险 8）。
_KS_AUTHOR_STATEMENT_PERMILLE = {"search": 153, "profile": 486}


def _ks_author_statement(photo_id: str, face: str):
    """携带 → {"content","type":1}；不携带 → None（键整缺，语料形态）。"""
    permille = _KS_AUTHOR_STATEMENT_PERMILLE.get(face, 0)
    if not photo_id or permille <= 0:
        return None
    if _stable_hash("ks-author-stmt::%s" % photo_id) % 1000 >= permille:
        return None
    pool = _optfam_tpl().get("ks_author_statement") or []
    if not pool:
        return {"content": "作者声明：含AI生成内容", "type": 1}
    return {"content": _optfam_weighted(photo_id, "ks_stmt", pool)[0], "type": 1}


def render_ks_search_feed(dataset: DatasetReader, page_no: int, page_size: int,
                          pcursor: str = "", search_session_id: str | None = None,
                          keyword: str | None = None) -> tuple[dict, dict]:
    """快手搜索 feed：请求 body pcursor（首屏 ""），响应 pcursor "1"→"2" 递增 + searchSessionId 回传。

    R6A-P2-1：映射关键词的数据窗口改为类目偏置采样（page_no 即绝对页号，
    由 synth_api 的轮转基 + 相对页合成）。"""
    records = _search_records(dataset, "kuaishou", keyword, None, page_no, page_size)
    rng = _page_rng(dataset, f"ks-feed-{page_no}")
    if search_session_id is None:
        search_session_id = ids.ks_session_id(rng)
    if keyword is None:
        keyword = _XHS_KEYWORDS[int(rng.integers(0, len(_XHS_KEYWORDS)))]  # 契约实证 2-4 字 CJK 搜索词
    request = {
        "keyword": keyword,
        "page": "search",
        "pcursor": pcursor,
        "searchSessionId": search_session_id,
        "webPageArea": "",
    }
    def _feed_item(r: dict) -> dict:
        item = materialize(r, "ks_feed_item", base=r)
        st = _ks_author_statement(str((r.get("photo") or {}).get("id") or ""),
                                  "search")   # R12A-P3-3：语料携带率 15.3%
        if st is not None:
            item["authorStatement"] = st
        return _ks_zero_collect(item)

    response = {
        "result": 1,
        "pcursor": str(page_no),
        "searchSessionId": search_session_id,
        "llsid": ids.digits_str(rng, 19),
        "host-name": ids.opaque(rng, 52),
        "webPageArea": "searchxxnull",
        "feeds": [_feed_item(rec) for rec in records],
    }
    return request, response


# ---------------------------------------------------------------------------
# 快手补面（红队 R3-P1-2）：GraphQL 12 operation + REST 评论/作者/用户搜索。
# 信封/键集全部按语料实证形态（ks_corpus_baseline3.json）：
#   - GraphQL 统一 {"data": {...}} 包装，vision* 键名 + __typename；
#   - REST photo/comment/list：rootCommentsV2/commentCountV2/pcursorV2 信封（snake_case 键）；
#   - R5A-P1-2：评论文本 commentext 组合生成；R5A-P2-5：评论者 = 数据集作者实体。
_KS_HOST_HEADS = ["public-bjy-c34-kce-node3595", "public-bjmt-c26-kce-node283",
                  "public-bjzey-c95-kce-node171", "public-bjy-c56-kce-node210",
                  "public-bjmt-c26-kce-node310"]
_KS_HOST_TAILS = ["idchb1az2.hb1.kwaidc.com", "idchb1az4.hb1.kwaidc.com",
                  "idchb1az3.hb1.kwaidc.com", "idc.kwaidc.com"]


def _ks_host(rng) -> str:
    head = _KS_HOST_HEADS[int(rng.integers(0, len(_KS_HOST_HEADS)))]
    return "%s.%s" % (head, _KS_HOST_TAILS[int(rng.integers(0, len(_KS_HOST_TAILS)))])


def ks_session_id_for(keyword: str | None, uid: int | None = None) -> str:
    """searchSessionId 真站形态（红队 R3-P3-1）：base64("14_<uid>_<ms_ts>_") + 尾缀，
    且同一 keyword（=同一搜索会话）跨页稳定。"""
    import base64 as _b64
    kw = (keyword or "").strip() or "<default>"
    h = _stable_hash("ks-session::%s" % kw)
    if uid is None:
        uid = 5530000000 + h % 900000000
    ms = 1780000000000 + (h >> 8) % (14 * 86400 * 1000)
    b64 = _b64.b64encode(("14_%d_%d_" % (uid, ms)).encode()).decode().rstrip("=")
    rng = np.random.default_rng([h & 0x7FFFFFFF, 9])
    return b64 + "-%s-%s" % (ids.opaque(rng, 1), ids.opaque(rng, 5))


# REST pcursorV2 / GraphQL pcursor 的「13 位毫秒时间戳」游标：低 10 位编码列表偏移
_KS_CMT_CURSOR_BASE = 1173364000000
# 红队 R8A-P2-1：评论 id/游标掺 photo_id 种子——每照片独立基址（photo_base =
# BASE + f(photo_id) 偏移，落 13 位数字内），根评论 cid=photo_base+idx、子评论
# cid=photo_base+SUB_OFFSET+idx+1（根/子命名空间分离），pcursorV2=photo_base+
# start+n 同源。旧版全库共用常量基：20 张照片首评 id 全部相同（跨照片 95% 碰撞，
# 语料 425/425 全局唯一）。偏移域 8e12：10 万照片两两碰撞期望 ~6e-4。
_KS_CMT_SUB_OFFSET = 1_000_000


def _ks_cmt_base(photo_id: str) -> int:
    """照片级评论 id 基址（R8A-P2-1）：f(photo_id) 确定性偏移，跨照片全局唯一。"""
    return _KS_CMT_CURSOR_BASE + _stable_hash("ks-cmt-base::%s" % photo_id) % 8_000_000_000_000


def _ks_root_cmt_ts(photo_id: str, idx: int, pub_ms: int, anchor_ms: int) -> int:
    """根评论时间（R9A-P2-2）：按 (photo_id, 序号) 独立种子派生（不再走页级 rng 流）。

    任何翻页路径/端点下恒定——visionSubCommentList 可精确重算根评论时间做
    子评论的因果下界（旧版子端点无法重建根时间只能独立采样 → 51% 子评论
    早于根，语料 0/168 违例；R4-P2-1 确定性口径同源）。"""
    rng = np.random.default_rng([_stable_hash("ks-cmt-ts::%s::%d" % (photo_id, idx))
                                 & 0x7FFFFFFF, idx])
    from synthgen.distengine import comment_ctime
    return comment_ctime(rng, pub_ms // 1000, anchor_ms // 1000) * 1000

# R7A-P2-1：ks web 不暴露收藏数——photo.collectCount 渲染恒 0（语料 search/feed
# 全部 76 个含该键的响应 706/706 卡 = 0，与 dy play_count 同性质；数据集底层保留）
# R7A-P3-3：ks @提及出现率（语料 root 评论 65/417 = 15.6%，形态「@昵称(O数字ID)」，
# 子评论无语料样本不加）
_KS_AT_PERMILLE = 156


def _ks_zero_collect(item: dict) -> dict:
    """R7A-P2-1：渲染层置 0（数据集 photo.collectCount 保留供约束/precompute）。"""
    ph = item.get("photo")
    if isinstance(ph, dict) and "collectCount" in ph:
        ph["collectCount"] = 0
    return item


def _ks_sub_total_for(comment_id: str) -> int:
    return _stable_hash("ks-sub-total::%s" % comment_id) % 4


def _ks_comment_total(rec: dict) -> int:
    """评论总数（commentCountV2，REST 与 GraphQL 同源）。

    红队 R5A-P2-2：旧实现取 search feed 的 comment.us_c 当计数源——但语料真值
    us_c 恒 0（100/100，另有语义），导致合成 commentCountV2 全集 {0,1,5,8,13,21}、
    62% 零评论；真站 70 样本 ∈ [51, 28863]、中位 1544、零值 0/70。现改为按
    photo.id 确定性派生的独立对数正态（中位 e^7.34≈1544，σ=1.15 对齐语料区间）。
    """
    from statistics import NormalDist
    pid = str((rec.get("photo") or {}).get("id") or "")
    u = (_stable_hash("ks-cmt-cnt::%s" % pid) % (2 ** 53)) / float(2 ** 53)
    u = min(max(u, 1e-9), 1.0 - 1e-9)
    z = NormalDist().inv_cdf(u)
    v = 1544.0 * float(np.exp(1.15 * z))   # 中位 1544（语料实证），σ=1.15
    return int(min(28863, max(51, round(v))))


def _ks_comments_core(dataset: DatasetReader, line_no: int, pcursor: str, count: int):
    """快手评论页核心数据（REST/GraphQL 共用）：返回 (条目核心列表, total, start, more)。"""
    rec = dataset.read(line_no)
    total = _ks_comment_total(rec)
    photo_id = str((rec.get("photo") or {}).get("id") or "")
    photo_base = _ks_cmt_base(photo_id)   # R8A-P2-1：照片级基址
    rng = _page_rng(dataset, f"ks-cmt-{line_no}-{pcursor or 'p0'}")
    start = 0
    if pcursor and pcursor not in ("", "no_more") and pcursor.isdigit():
        start = max(0, min(int(pcursor) - photo_base, total))
    n = max(0, min(count, total - start))
    anchor_ms = (dataset.anchor_ts or 0) * 1000 or _now_ms()
    items = []
    for i in range(n):
        idx = start + i
        pub_ms = int((rec.get("photo") or {}).get("timestamp") or _now_ms())
        # R9A-P2-2：根评论时间按 (photo_id, 序号) 独立派生（子评论端点可重算）
        ts = _ks_root_cmt_ts(photo_id, idx, pub_ms, anchor_ms)
        cid = str(photo_base + idx)
        commenter = _dataset_commenter(dataset, "kuaishou", photo_id, idx)
        content = commentext.comment_text("kuaishou", idx, photo_id)
        # R7A-P3-3：@提及（语料形态「@昵称(O数字ID)」，指向另一评论者/作者；
        # 判定用稳定 hash——与页级 rng 流解耦，count/翻页路径变化下恒定）
        if _maybe_at_prefix("ks::%s::%d" % (photo_id, idx), _KS_AT_PERMILLE):
            tagged = _dataset_commenter(dataset, "kuaishou",
                                        "at::%s::%d" % (photo_id, idx), idx)
            nick = str(tagged.get("name") or commenter.get("name") or "快手用户")
            oid = _stable_hash("ks-at-oid::%s::%d" % (photo_id, idx)) % 10**9
            content = "@%s(O%d) %s" % (nick, oid, content)
        items.append({
            "cid": cid,
            "author_id": str(commenter.get("id") or ids.ks_id(rng)),
            "author_name": str(commenter.get("name") or "快手用户"),
            "content": content,
            # R9A-P3-2：评论者头像非空（语料 0/1,476 空）——数据集作者实体键为
            # headerUrl（camel），旧版只读 header_url 恒空串
            "headurl": str(commenter.get("headerUrl")
                           or commenter.get("header_url") or ""),
            "timestamp": ts,
            "liked": False,
            "status": "done",
            "liked_count": int(rng.integers(0, 5000)),
            "sub_total": _ks_sub_total_for(cid),
        })
    more = start + n < total
    return items, total, start, more, rng, photo_base


def render_ks_comment_list(dataset: DatasetReader, line_no: int, pcursor: str = "",
                           count: int = 20) -> dict:
    """POST /rest/v/photo/comment/list（REST 信封：rootCommentsV2/commentCountV2/pcursorV2）。"""
    items, total, start, more, rng, photo_base = _ks_comments_core(dataset, line_no, pcursor, count)
    roots = [{
        "content": c["content"], "subCommentId": 0, "timestamp": c["timestamp"],
        "headurl": c["headurl"], "replyToUserName": "",
        "likeCount": c["liked_count"], "commentCount": c["sub_total"],
        "liked": c["liked"], "hasSubComments": c["sub_total"] > 0,
        "author_id": c["author_id"], "reply_to": "0",
        "comment_id": int(c["cid"]), "author_name": c["author_name"],
    } for c in items]
    return {
        "result": 1,
        "host-name": _ks_host(rng),
        "pcursorV2": str(photo_base + start + len(items)) if more else "no_more",
        "commentCountV2": total,
        "rootCommentsV2": roots,
    }


def _ks_comment_list_graphql(dataset: DatasetReader, line_no: int, pcursor: str = "",
                             count: int = 20) -> dict:
    """commentListQuery 的 visionCommentList 形态（camelCase 字符串 id；尾页 pcursor=null）。"""
    items, total, start, more, _, photo_base = _ks_comments_core(dataset, line_no, pcursor, count)
    roots = [{
        "commentId": c["cid"], "authorId": c["author_id"], "authorName": c["author_name"],
        "content": c["content"], "headurl": c["headurl"], "timestamp": c["timestamp"],
        "hasSubComments": c["sub_total"] > 0, "likedCount": str(c["liked_count"]),
        "liked": c["liked"], "status": c["status"], "__typename": "VisionRootCommentItem",
    } for c in items]
    return {
        "commentCount": None,
        "commentCountV2": total,
        "pcursor": str(photo_base + start + len(items)) if more else None,
        "rootCommentsV2": roots,
    }


def render_ks_graphql(dataset: DatasetReader, operation: str, variables: dict,
                      line_no: int | None = None) -> dict:
    """POST /graphql：按 operationName 分发（语料 12 operation 全覆盖，data.* 包装）。

    line_no：commentListQuery/visionSubCommentList/visionShortVideoReco 的目标作品
    （按 photoId 反查；查不到按 miss 形态回空）。
    """
    variables = variables or {}
    rng = _page_rng(dataset, f"ks-gql-{operation}")
    if operation == "checkLoginQuery":
        return {"data": {"checkLogin": True}}
    if operation == "visionLoginConfig":
        return {"data": {"kconf": {"domain": "/", "open": True,
                                   "showTime": 60000, "frequencyLimit": 1}}}
    if operation == "visionOttConfig":
        return {"data": {"kconf": {
            "imgUrl": "https://s2-11673.kwimgs.com/kos/nlav11673/ott-page/ott.png",
            "title": ["电视刷快手", "大屏更过瘾"],
            "subText": ["1. 点击“立即下载”按钮", "2. 下载后的app拷贝至U盘再插入电视",
                        "3. 在电视里找到U盘中的安装包打开安装"],
            "apkUrl": "https://js.a.kspkg.com/bs2/fes/KS_TV-KSPC_TV-huidu-4.2.4.2340-YRD-x32-signed_c2508a.apk"}}}
    if operation == "likeDataQuery":
        return {"data": {"likeData": {
            "result": 1, "llsid": ids.digits_str(rng, 19), "expTag": None,
            "serverExpTag": None, "pcursor": "no_more", "feeds": [],
            "webPageArea": "followxxnull", "__typename": "PhotoResult"}}}
    if operation == "visionProfileReduced":
        # R8C-P3-4：userProfile 对象形态（语料 20/40 样本携带、契约在录；profile
        # 主体与 userInfoQuery 同一录制账号静态件）
        st = _ks_gql_statics().get("ks_profile_reduced_with_user")
        if isinstance(st, dict):
            out = json.loads(json.dumps(st, ensure_ascii=False))  # 深拷贝（hostName 按站形态派生）
            out["hostName"] = _ks_host(rng)
            return {"data": {"visionProfileReduced": out}}
        return {"data": {"visionProfileReduced": {
            "result": 21, "hostName": _ks_host(rng), "userProfile": None,
            "__typename": "VisionProfileResult"}}}
    if operation == "visionProfilePhotoList":
        return {"data": {"visionProfilePhotoList": {
            "result": 50, "llsid": None, "webPageArea": None, "feeds": [],
            "hostName": None, "pcursor": None, "__typename": "VisionProfilePhotoList"}}}
    if operation == "userInfoQuery":
        return {"data": {"userInfo": {
            "id": "3xtytcu6gte7qe9", "name": "就难蜘蛛",
            "avatar": "http://p1.a.yximgs.com/s1/i/def/head_m.png",
            "eid": "3xtytcu6gte7qe9", "userId": 5537274238, "__typename": "BaseUser"}}}
    if operation == "visionConfigQuery":
        # R8C-P3-4：visionConfig 语料静态件（__typename/双 banner/subTabs 补齐）
        cfg = _ks_gql_statics().get("ks_vision_config")
        if isinstance(cfg, dict):
            return {"data": {"visionConfig": json.loads(json.dumps(cfg, ensure_ascii=False))}}
        return {"data": {"visionConfig": _KS_VISION_CONFIG_STATIC}}
    if operation == "visionBaseEmoticons":
        # R8C-P3-4：iconUrls 扩到语料全量 640 名录（20/20 样本逐字节相同）+__typename
        emo = _ks_gql_statics().get("ks_emoticons")
        if isinstance(emo, dict) and emo.get("iconUrls"):
            emo = json.loads(json.dumps(emo, ensure_ascii=False))
            emo.setdefault("__typename", "VisionEmoticonsResult")
            return {"data": {"visionBaseEmoticons": emo}}
        return {"data": {"visionBaseEmoticons": {"iconUrls": _KS_EMOTICON_URLS}}}
    if operation == "visionShortVideoReco":
        # R8C-P3-4：feed 条目改用语料直提模板 ks_reco_feed 物化（旧版手写 dict 逐样本
        # 恒缺 91 键——__typename 全节点族/authorStatement/canAddComment/tags/
        # photo.{coverUrls,videoResource,distance,profileUserTopPhoto,…}；且多出真站
        # reco 上下文不回的 author.livingInfo/photo.{collectCount,collected,height,
        # photoH265Urls,photoUrls,width} 14 键——模板定键集后多余键自然消失）。
        feeds = []
        if line_no is not None:
            n = len(dataset)
            for i in range(1, 21):
                r = dataset.read((line_no + i) % n)
                if "ks_reco_feed" in _templates():
                    item = materialize(r, "ks_reco_feed", base=r)
                    # R8C-P3-4：authorStatement 语料 73/389 ≈ 19% 逐 feed 携带（模板众数
                    # null——18.8% < min_rate 被剪）——按 photo id 确定性回补语料对象形态
                    if _stable_hash("ks-reco-stmt::%s"
                                    % ((r.get("photo") or {}).get("id") or "")) % 1000 < 190:
                        item["authorStatement"] = {
                            "content": "作者声明：含虚构演绎内容，仅供娱乐", "type": 1,
                            "riskStyleType": None,
                            "__typename": "VisionAuthorStatement"}
                    feeds.append(item)
                else:
                    photo = dict(r.get("photo") or {})
                    photo["__typename"] = "PhotoEntity"
                    photo.setdefault("originCaption", photo.get("caption") or "")
                    photo["realLikeCount"] = int(photo.get("likeCount") or 0)
                    photo["commentCount"] = None
                    photo["collectCount"] = 0   # R7A-P2-1：渲染恒 0（语料 web 不暴露）
                    author = dict(r.get("author") or {})
                    author["__typename"] = "VisionShortVideoAuthor"
                    feeds.append({"type": 1, "author": author, "photo": photo})
        return {"data": {"visionShortVideoReco": {
            "__typename": "VisionShortVideoRecoFeed",
            "llsid": ids.digits_str(rng, 19), "feeds": feeds}}}
    if operation == "commentListQuery":
        if line_no is None:
            return {"data": {"visionCommentList": {"commentCount": None, "commentCountV2": 0,
                                                  "pcursor": None, "rootCommentsV2": []}}}
        return {"data": {"visionCommentList": _ks_comment_list_graphql(
            dataset, line_no, str(variables.get("pcursor") or ""),
            int(variables.get("count") or 20))}}
    if operation == "visionSubCommentList":
        if line_no is None:
            return {"data": {"visionSubCommentList": {
                "pcursor": "", "subComments": [], "pcursorV2": "no_more",
                "subCommentsV2": [], "__typename": "VisionSubCommentFeed"}}}
        return render_ks_sub_comments(dataset, line_no,
                                      str(variables.get("rootCommentId") or ""),
                                      str(variables.get("pcursor") or ""))
    # 未知 operation：JSON 错误族（不落 HTML 404，不泄露路由表）
    return {"data": None, "errors": [{"message": "not found"}]}


# R9A-P2-1：子评论 id 槽位掺 root 维度——slot = root_idx*STRIDE + idx，同照片
# 跨 root 全局唯一（旧版 idx 按 root 重启：同照片每个 root 的第 i 条子评论 id
# 全相同，170 条仅 15 唯一、语料 1,484/1,484 全局唯一）。STRIDE=8（每根子数
# ≤3 留裕量）；子域 [photo_base+1e6, +1e6+total*8] 与根域 [photo_base, +total)
# 因 total ≤ 28863 < 1e6 确定性不交（根/子命名空间分离保持）。
_KS_SUB_STRIDE = 8


def render_ks_sub_comments(dataset: DatasetReader, line_no: int, root_comment_id: str,
                           pcursor: str = "") -> dict:
    """visionSubCommentList：{pcursor:"", subComments:[], pcursorV2, subCommentsV2}。

    R5A-P1-2/P2-5：子评论文本 commentext（salt=root cid 命名空间）；评论者与
    replyTo 指向的根评论作者均取数据集作者实体（跨端点可回查）。
    R9A-P2-1：子评论 id = photo_base + SUB_OFFSET + root_idx*STRIDE + idx + 1
    （跨 root 全局唯一）；R9A-P2-2：子评论时间 = 根评论时间 + 正偏移
    （根时间按 (photo_id, root_idx) 独立派生重算，语料 0/168 违例）。"""
    rec = dataset.read(line_no)
    photo_id = str((rec.get("photo") or {}).get("id") or "")
    photo_base = _ks_cmt_base(photo_id)   # R8A-P2-1：子评论 id 同源照片级基址
    rng = _page_rng(dataset, f"ks-sub-{line_no}-{root_comment_id}-{pcursor or 'p0'}")
    total = _ks_sub_total_for(root_comment_id)
    start = 0
    if pcursor and pcursor.isdigit():
        start = max(0, min(int(pcursor) - photo_base - _KS_CMT_SUB_OFFSET, total))
    n = max(0, min(10, total - start))
    # 根评论作者（cid = photo_base+idx 与 _ks_comments_core 同一派生 → 姓名一致）
    root_idx = max(0, int(root_comment_id) - photo_base) if root_comment_id.isdigit() else 0
    root_author = _dataset_commenter(dataset, "kuaishou", photo_id, root_idx)
    root_name = str(root_author.get("name") or "快手用户")
    root_aid = str(root_author.get("id") or "0")
    anchor_ms = (dataset.anchor_ts or 0) * 1000 or _now_ms()
    pub_ms = int((rec.get("photo") or {}).get("timestamp") or _now_ms())
    # R9A-P2-2：根评论时间（与 _ks_comments_core 同一派生重算）——子评论采样
    # 窗口 [根时间, anchor]，comment_ctime 钳制保证子 ≥ 根+30s（正偏移）
    root_ts = _ks_root_cmt_ts(photo_id, root_idx, pub_ms, anchor_ms)
    from synthgen.distengine import comment_ctime
    subs = []
    for i in range(n):
        idx = start + i
        ts = comment_ctime(rng, root_ts // 1000, anchor_ms // 1000) * 1000
        commenter = _dataset_commenter(dataset, "kuaishou",
                                       "sub::%s" % root_comment_id, idx)
        subs.append({
            "commentId": str(photo_base + _KS_CMT_SUB_OFFSET
                             + root_idx * _KS_SUB_STRIDE + idx + 1),
            "authorId": str(commenter.get("id") or ids.ks_id(rng)),
            "authorName": str(commenter.get("name") or "快手用户"),
            "content": commentext.comment_text("kuaishou", idx,
                                               "sub::%s" % root_comment_id),
            "headurl": str(commenter.get("headerUrl")
                           or commenter.get("header_url") or ""),
            "timestamp": ts, "hasSubComments": False,
            "likedCount": str(int(rng.integers(0, 200))), "liked": False, "status": "done",
            "replyToUserName": root_name, "replyTo": root_aid,
            "__typename": "VisionSubCommentItem",
        })
    more = start + n < total
    return {"data": {"visionSubCommentList": {
        "pcursor": "", "subComments": [],
        "pcursorV2": str(photo_base + _KS_CMT_SUB_OFFSET + start + n) if more else "no_more",
        "subCommentsV2": subs, "__typename": "VisionSubCommentFeed"}}}


_KS_VISION_CONFIG_STATIC = {
    "coronaTabs": [
        {"tabId": 0, "name": "推荐", "folded": False, "selected": True, "__typename": "VisionTab"},
        {"tabId": 1, "name": "影视", "folded": False, "selected": False, "__typename": "VisionTab"},
        {"tabId": 8, "name": "动漫", "folded": False, "selected": False, "__typename": "VisionTab"},
        {"tabId": 7, "name": "美食", "folded": False, "selected": False, "__typename": "VisionTab"},
        {"tabId": 5, "name": "游戏", "folded": False, "selected": False, "__typename": "VisionTab"},
        {"tabId": 3, "name": "音乐", "folded": False, "selected": False, "__typename": "VisionTab"},
    ],
    "tubeTabs": [
        {"tabId": 0, "name": "精选", "folded": False, "selected": True, "subTabs": [],
         "__typename": "VisionTab"},
        {"tabId": 10000, "name": "短剧", "folded": False, "selected": False,
         "subTabs": [{"subTabId": 10005, "subTabName": "短片", "__typename": "SubTab"}],
         "__typename": "VisionTab"},
    ],
    "banners": [
        {"id": 4, "landingUrl": "", "imageUrl": "https://s2-10623.kwimgs.com/kos/nlav10623/vision_images/topBannerx1.png",
         "checkLogin": False, "openInNewTab": None, "__typename": "Banner"},
    ],
    "movieTagTypes": [{"movieTagType": "movieType", "movieTagValues": ["喜剧", "动作", "爱情", "悬疑", "剧情"],
                       "__typename": "movieTag"}],
    "disabledModules": ["movie", "corona", "olympic", "parisOly"],
    "homeModuleOrder": [
        {"name": "brilliant", "title": "精彩短视频", "order": 1, "showMore": True,
         "checkLogin": False, "clickTarget": "/brilliant", "startTime": None, "endTime": None},
        {"name": "hotRanks", "title": "快手热榜", "order": 1, "showMore": False,
         "checkLogin": False, "clickTarget": "", "startTime": None, "endTime": None},
    ],
}

_KS_EMOTICON_URLS = {k: "//p66-plat.wskwai.com/kimg/bs2/emotion/CigxNzA0%s.webp" % v for k, v in {
    "[捂脸]": "Nzc2MDIxMjAydGhpcmRfcGFydHlfczEyOTY3NDc0NTcucG5nEIfN1y8",
    "[龇牙]": "Nzc1OTc4MTQ0dGhpcmRfcGFydHlfczEyOTY3NDY4ODIucG5nEIfN1y8",
    "[大哭]": "NzcxMDI4OTQwdGhpcmRfcGFydHlfczEyOTY2OTYwMzMucG5nEIfN1y8",
    "[赞]": "NzYyMTUzNDQ5dGhpcmRfcGFydHlfczEyOTY2NTI4ODEucG5nEIfN1y8",
    "[微笑]": "NzY3NTgwMDQ0dGhpcmRfcGFydHlfczEyOTY3NDUyNTcucG5nEIfN1y8",
    "[生气]": "NzY3NTYxMDQ4dGhpcmRfcGFydHlfczEyOTY3NDI0MTkucG5nEIfN1y8",
    "[笑哭]": "NzY3NzE1NDc4MXRoaXJkX3BhcnR5X3MxMjk2Njk5NzEucG5nEIfN1y8",
    "[色]": "NzY3NTk3ODA0N3RoaXJkX3BhcnR5X3MxMjk2NzQ3MDUucG5nEIfN1y8",
    "[惊讶]": "NzY3NTYwMDM2dGhpcmRfcGFydHlfczEyOTY3NDMwNjMucG5nEIfN1y8",
    "[ slime ]": "NzY3NTk3ODA0N3RoaXJkX3BhcnR5X3MxMjk2NzQ3MDUucG5nEIfN1y8",
}.items()}

# ---------------------------------------------------------------------------
# 红队 R8C-P3-4：ks graphql 静态件（语料直提，sanitized_corpus 只读提取到
# synthgen/data/ks_gql_statics.json——visionBaseEmoticons 全量 640 名录（语料
# 20/20 样本 iconUrls 恒 640 项且逐字节相同，旧版仅 10 项）、visionConfig
# （语料 20 样本全同形，旧版缺 __typename/双 banner/重复 disabledModules 项）、
# visionProfileReduced 的 userProfile 对象形态（语料 20/40 携带，契约在录）。
# ---------------------------------------------------------------------------
_KS_GQL_STATICS = {"loaded": False, "data": {}}


def _ks_gql_statics() -> dict:
    if not _KS_GQL_STATICS["loaded"]:
        try:
            f = Path(__file__).resolve().parent / "data" / "ks_gql_statics.json"
            _KS_GQL_STATICS["data"] = json.loads(f.read_text(encoding="utf-8"))
        except Exception:
            _KS_GQL_STATICS["data"] = {}
        _KS_GQL_STATICS["loaded"] = True
    return _KS_GQL_STATICS["data"]


def render_ks_profile_get(dataset: DatasetReader, line_nos: list, user_id: str) -> dict:
    """GET /rest/v/profile/get（语料键集：eid/like/userTex/sex/mobile/follows/host-name/
    userName/userId/userDefineId/fans/result/userHead）。

    R6A-P3-3：userId 双 id 空间统一——响应 userId 回填请求的 `3x` 形态 id
    （旧实现自报数字空间 id（6347098269 型），与搜索流/评论/页面 URL 三个表面
    使用的 `3x` 空间不一致，跨端点 id 不自洽）；userDefineId 保留数字自定义号
    形态（语料该键即用户自定义号，与 userId 不同源）。
    """
    rng = _page_rng(dataset, f"ks-profile-{user_id}")
    rec = dataset.read(line_nos[0]) if line_nos else None
    author = (rec or {}).get("author") or {}
    uid_num = 5537274238 + _stable_hash("ks-uid::%s" % user_id) % 900000000
    name = author.get("name") or "快手用户"
    return {
        "eid": user_id, "like": int(rng.integers(10, 60)),
        "userTex": "", "sex": "M", "mobile": "138****%04d" % int(rng.integers(0, 9999)),
        "follows": int(rng.integers(1, 200)),
        "host-name": _ks_host(rng),
        "userName": name, "userId": user_id,
        "userDefineId": str(uid_num),
        "fans": int(rng.integers(6, 200000)),
        "result": 1,
        "userHead": author.get("headerUrl") or "http://p5.a.yximgs.com/s1/i/def/head_m.png",
    }


def render_ks_user_info(dataset: DatasetReader, line_nos: list, sec_uid: str) -> dict:
    """GET /api/user/info（Media-Monitor 用户增强契约 kuaishou-user v1）。

    契约口径：sec_uid 定位单个作者、绑定 $.user_list；语料无该路径真值（MM 契约
    自述 reconstructed），形态 = ks REST result 族 + user_list 数组，数据全部从
    synthgen 作者池确定性派生（rng 种子 f(sec_uid)，跨请求稳定）。字段覆盖 MM
    bindUser 全部默认路径（uid/sec_uid/nickname/avatar/fans/follows/aweme_count）。
    """
    rng = _page_rng(dataset, f"ks-uinfo-{sec_uid}")
    rec = dataset.read(line_nos[0]) if line_nos else None
    author = (rec or {}).get("author") or {}
    uid = str(author.get("id") or sec_uid)
    fans = int(rng.integers(6, 200000))
    follows = int(rng.integers(1, 800))
    name = author.get("name") or "快手用户"
    user = {
        "user_id": uid, "sec_uid": sec_uid,
        "user_name": name, "nickname": name,
        "headurl": author.get("headerUrl") or "",
        "avatar_url": author.get("headerUrl") or "",
        "user_text": "感谢关注，日常更新",
        "fans": fans, "follower_count": fans,
        "follows": follows, "following_count": follows,
        "aweme_count": len(line_nos),
        "total_favorited": int(rng.integers(0, 5000000)),
        "verified": False, "gender": "M",
    }
    return {"result": 1, "host-name": _ks_host(rng), "user_list": [user]}


def _ks_pcursor_encode(ms: int) -> str:
    """毫秒整数 → 语料 pcursor 形态（科学计数法毫秒，如 1.785484910875E12）。"""
    return ("%.12f" % (ms / 1e12)).rstrip("0") + "E12"


def _ks_pcursor_decode(pcursor: str):
    """语料 pcursor → 毫秒 float；非数值形态返回 None（调用方按首页处理）。"""
    try:
        v = float(str(pcursor).replace("E", "e"))
    except ValueError:
        return None
    return v if v > 0 else None


def render_ks_profile_feed(dataset: DatasetReader, line_nos: list, user_id: str,
                           pcursor: str = "") -> dict:
    """POST /rest/v/profile/feed（作者作品页：type/tags/authorStatement/author/photo）。

    红队 R8C-P3-2：信封并入 search/feed 同款键族——L1 {feeds, host-name, llsid,
    pcursor, result, webPageArea}（语料 3/3 恒有；webPageArea='profilexxnull'）、
    feeds 项内补 comment.us_c=0（语料真值恒 0，同 R5A-P2-2 口径）/danmakuSwitch=true/
    photo.profileUserTopPhoto=true；llsid 由 synth_api 层每请求重掷（R6C-P3-2 同法）。

    价值挖掘修复（2026-09-01，与 dy aweme/post 同族）：旧版下一页 pcursor 用墙钟
    _now_ms() 派生（时变、不可复现），回翻映射 stable_hash(pcursor)%12*18（随机
    跳页——作者作品 >216 条时尾页永不可达，页间可重复）。修法（render 层）：
    作品按 (photo.timestamp 降序, photo.id) 稳定排序（语料真站形态：最新在前），
    pcursor = 末条时间戳的语料科学计数法毫秒形态（同毫秒并列时组内 -1ms 保持
    严格单调），回翻定位首个时间戳 < pcursor 的作品 → 游标单调、页间 0 重复、
    页数=ceil(作品数/18)、末页 pcursor="no_more"、复读确定性。"""
    rng = _page_rng(dataset, f"ks-pfeed-{user_id}-{pcursor or 'p0'}")
    per_page = 18
    recs = [dataset.read(i) for i in line_nos]
    recs.sort(key=lambda r: (-int(((r.get("photo") or {}).get("timestamp")) or 0),
                             str(((r.get("photo") or {}).get("id")) or "")))
    keys = []   # 每条作品的游标键（photo.timestamp 毫秒；严格单调递减）
    for r in recs:
        k = int(((r.get("photo") or {}).get("timestamp")) or 0)
        if keys and k > keys[-1] - 1:
            k = keys[-1] - 1
        keys.append(k)
    start = 0
    if pcursor and pcursor != "no_more":
        cur_ms = _ks_pcursor_decode(pcursor)
        if cur_ms is not None:
            start = len(keys)   # 游标早于全部作品 → 空页 + no_more 终止
            for i, k in enumerate(keys):
                if k < cur_ms:
                    start = i
                    break
    page_recs = recs[start:start + per_page]
    feeds = []
    for r in page_recs:
        item = materialize(r, "ks_feed_item", base=r)
        photo = item.get("photo") or r.get("photo") or {}
        photo.setdefault("profileUserTopPhoto", True)   # R8C-P3-2：语料恒 true
        # R12A-P3-3：profile/feed 面 authorStatement 按语料携带率 48.6%（旧版恒带
        # 单一文案；变体池按频次加权，photo 级确定性）
        stmt = _ks_author_statement(str((r.get("photo") or {}).get("id") or ""),
                                    "profile")
        feed = {
            "type": r.get("type") or 1,
            "tags": r.get("tags") or [],
            "author": item.get("author") or r.get("author"),
            "photo": photo,
            "comment": {"us_c": 0},
            "danmakuSwitch": True,
        }
        if stmt is not None:
            feed["authorStatement"] = stmt
        feeds.append(feed)
        _ks_zero_collect(feeds[-1])   # R7A-P2-1：作者作品页同口径（web 不暴露收藏数）
    more = start + per_page < len(recs)
    next_cur = (_ks_pcursor_encode(keys[start + per_page - 1])
                if more and page_recs else "no_more")
    return {"result": 1, "pcursor": next_cur, "feeds": feeds,
            "host-name": _ks_host(rng),
            "llsid": ids.digits_str(rng, 19),
            "webPageArea": "profilexxnull"}


def render_ks_search_user(dataset: DatasetReader, keyword: str, pcursor: str = "",
                          session_id: str | None = None) -> dict:
    """POST /rest/v/search/user（语料：users n=30、pcursor "1"、searchSessionId 回传）。"""
    rng = _page_rng(dataset, f"ks-suser-{keyword}-{pcursor or 'p0'}")
    n_total = len(dataset)
    start = _stable_hash("ks-suser::%s" % keyword) % (n_total - 400)
    users, seen = [], set()
    for off in range(start, n_total):
        if len(users) >= 30:
            break
        r = dataset.read(off)
        a = r.get("author") or {}
        aid = str(a.get("id") or "")
        if not aid or aid in seen:
            continue
        seen.add(aid)
        users.append({
            "verified": False,
            "headurl": a.get("headerUrl") or "",
            "livingInfo": {"living": False, "livingId": None, "iconType": 0},
            "user_name": a.get("name") or "快手用户",
            "isFollowing": False,
            "user_id": aid,
            "user_text": "感谢关注，日常更新 %s 相关内容" % ((r.get("tags") or [{}])[0].get("name") or "生活"),
        })
    return {
        "result": 1, "pcursor": "1", "host-name": _ks_host(rng),
        "searchSessionId": session_id or ks_session_id_for(keyword),
        "users": users,
    }


def render_ks_kconf_get(dataset: DatasetReader, kconf_key: str = "") -> dict:
    """POST /rest/v/kconf/get（语料静态键集：loginConfig/pcMenuConfig）。"""
    return {
        "result": 1,
        "loginConfig": {"domain": "/", "open": True, "showTime": 60000, "frequencyLimit": 1},
        "pcMenuConfig": {
            "user_menu": [
                {"type": "item", "text": "我的作品", "icon": "https://p2-plat.wskwai.com/kos/nlav111422/ks-web/images/webframe_icon/my_work.svg", "onClick": "jumpPage_profile"},
                {"type": "item", "text": "我的点赞", "icon": "https://p2-plat.wskwai.com/kos/nlav111422/ks-web/images/webframe_icon/my_like.svg", "onClick": "jumpPage_profile"},
                {"type": "item", "text": "退出登录", "icon": "https://p2-plat.wskwai.com/kos/nlav111422/ks-web/images/webframe_icon/logout.svg", "onClick": "logout"},
                {"type": "item", "text": "切换账号", "icon": "https://p2-plat.wskwai.com/kos/nlav111422/ks-web/images/webframe_icon/switch_user.svg", "onClick": "switchAccount"},
            ],
            "more_menu": [
                {"text": "业务合作", "type": "group"},
                {"type": "link", "text": "关于快手", "link": "https://www.kuaishou.com/about/"},
                {"type": "link", "text": "加入我们", "link": "https://zhaopin.kuaishou.cn/recruit/e/#/official/social/"},
            ],
        },
    }


# ---------------------------------------------------------------------------
# 抖音补面（红队 R3-P2-2/P2-3）：related 相关推荐 + 作者主页最小面
# ---------------------------------------------------------------------------
def _dy_related_or_post_aweme(rec: dict, family: str) -> dict:
    """R8C-P3-3：related / aweme/post 上下文的 aweme 形态。

    语料实证两上下文的 aweme 键集与搜索卡不同（related 逐样本差 184-238 键、
    且真站 related 恒不回 author.{card_entries,ban_user_functions,…} 等 117 键——
    旧版复用搜索卡模板把 profile 系 author 键整族带出）。改用语料直提模板
    dy_related_aweme / dy_post_aweme 物化（模板定键集：多余键随模板消失、缺口
    随模板补齐），数据集记录同路径值优先；play_count/custom_verify 与
    render_aweme 同口径恒置 0/空（语料三上下文一致）。"""
    if family in _templates():
        a = materialize(rec, family, base=rec)
    else:
        a = render_aweme(rec)
    st = a.get("statistics")
    if isinstance(st, dict):
        st["play_count"] = 0   # R6C-P2-1：dy web 从不暴露播放量
    au = a.get("author")
    if isinstance(au, dict):
        au["custom_verify"] = ""   # R7A-P3-6：web 表面不暴露认证文案
    # R10A-P3-3：related/post 面 desc 偏移同口径换算（与 render_aweme 一致，
    # 含数据集 null → null 的语料 1.6% 形态）
    if rec.get("text_extra") is None:
        a["text_extra"] = None
    elif isinstance(a.get("text_extra"), list):
        a["text_extra"] = _dy_text_extra_utf16(a.get("desc"), a["text_extra"])
    # R11A-P3-3：related/post 面 bit_rate 阶梯同口径（语料两上下文同 3-29 档形态；
    # related 面 gear 键集少 is_bytevc1——语料 10 键 vs search/detail/post 11 键）
    if isinstance(a.get("video"), dict):
        _dy_bitrate_ladder(a["video"], str(rec.get("aweme_id") or ""),
                           drop_keys=(("is_bytevc1",) if family == "dy_related_aweme" else ()))
    # R12A-P3-3：related/post 面可选族携带率分表注入（poi/anchor 仅 related 语料在档）
    _dy_optional_families(a, rec,
                          "related" if family == "dy_related_aweme" else "post")
    return a


def render_douyin_related(dataset: DatasetReader, line_no: int, count: int = 10) -> dict:
    """/aweme/v1/web/aweme/related/（语料 6 键信封：aweme_list/chime_video_list/
    filter_infos/has_more/log_pb/status_code，n=10）——同数据窗口邻域切片。"""
    rng = _page_rng(dataset, f"dy-related-{line_no}")
    n = len(dataset)
    recs = [dataset.read((line_no + 1 + i) % n) for i in range(count)]
    logid = ids.dy_logid(rng)
    return {
        "status_code": 0,
        "aweme_list": [_dy_related_or_post_aweme(rec, "dy_related_aweme") for rec in recs],
        "chime_video_list": None,
        "filter_infos": None,
        "has_more": 1 if (line_no + 1 + count) < n else 0,
        "log_pb": {"impr_id": logid},
    }


def render_douyin_user_profile(rec: dict) -> dict:
    """/aweme/v1/web/user/profile/other/（作者主页头部；user 块以数据集 author 为核心）。"""
    rng = np.random.default_rng([_stable_hash("dy-uprofile::%s" % (rec.get("author") or {}).get("uid", "")) & 0x7FFFFFFF, 3])
    a = rec.get("author") or {}
    logid = ids.dy_logid(rng)
    avatar_uri = ((a.get("avatar_thumb") or {}).get("url_list") or [""])[0].rsplit("/", 1)[-1]
    user = {
        "uid": a.get("uid"), "sec_uid": a.get("sec_uid"),
        "nickname": a.get("nickname"), "signature": a.get("signature") or "",
        "avatar_thumb": a.get("avatar_thumb"),
        "avatar_168x168": {"uri": avatar_uri, "url_list": (a.get("avatar_thumb") or {}).get("url_list"), "width": 720, "height": 720},
        "avatar_300x300": {"uri": avatar_uri, "url_list": (a.get("avatar_thumb") or {}).get("url_list"), "width": 720, "height": 720},
        "follower_count": a.get("follower_count", 0),
        "following_count": int(rng.integers(1, 800)),
        "total_favorited": a.get("total_favorited", 0),
        "aweme_count": int(rng.integers(1, 400)),
        "custom_verify": "",   # R7A-P3-6：语料 profile/other 0/20 非空（web 不暴露认证文案）
        "follow_status": 0, "follower_status": 0, "is_block": False,
        "is_gov_media_vip": False, "is_mix_user": False, "is_star": False,
        "room_id": 0, "verified": False, "verify_info": "",
    }
    return {
        "extra": {"fatal_item_ids": [], "logid": logid, "now": _now_ms()},
        "log_pb": {"impr_id": logid},
        "status_code": 0, "status_msg": None, "user": user,
    }


def render_douyin_user_posted(dataset: DatasetReader, line_nos: list,
                              max_cursor: int = 0, count: int = 18) -> dict:
    """/aweme/v1/web/aweme/post/（作者作品列表：min/max_cursor + aweme_list + has_more）。

    红队 R8C-P3-3：信封对齐契约/语料 10 键 L1——补 post_serial(=2)/
    replace_series_cover(=1)/request_item_cursor(=请求 max_cursor 回显)/time_list
    （作品月份筛选条「YYYY·MM」降序去重），删语料恒不存在的 extra 键
    （契约 L1 无该键）；aweme_list 改用语料直提模板 dy_post_aweme（旧版搜索卡
    模板逐样本差 300+ 键）。

    价值挖掘修复（2026-09-01，capability_proposals 建议 A「已发现坑」）：数据集
    create_time 未按时间排序（行序 ≠ 时间序），旧版在行序上扫「首个
    create_time*1000<=max_cursor」的记录作页起点——游标语义回跳（扫不到时回卷
    首页），3/5 作者 50 页不终止、页间大量重复。修法（render 层，数据集不动）：
    同作者作品按 (create_time 降序, aweme_id) 稳定排序（语料真站形态：最新在前）
    后按游标切窗；游标键 = create_time*1000（语料毫秒形态），同秒并列时组内逐条
    -1ms 保持键序列严格单调递减 → 翻页位置由游标精确恢复：页间 0 重复 0 跳过、
    页数=ceil(作品数/count)、末页 has_more=0。time_list 随排序真正降序（旧版行序
    拼出的月份条非降序，与语料形态不符）。"""
    rng = _page_rng(dataset, "dy-uposted-%d" % (len(line_nos)))
    recs = [dataset.read(i) for i in line_nos]
    recs.sort(key=lambda r: (-(int(r.get("create_time") or 0)),
                             str(r.get("aweme_id") or "")))
    keys = []   # 每条作品的游标键（毫秒形态；严格单调递减）
    for r in recs:
        k = int(r.get("create_time") or 0) * 1000
        if keys and k > keys[-1] - 1:
            k = keys[-1] - 1
        keys.append(k)
    start = 0
    if max_cursor:
        # 游标早于全部作品 → 空页终止（旧版回卷首页重发即 50 页死循环根因）
        start = len(keys)
        for i, k in enumerate(keys):
            if k <= max_cursor:
                start = i
                break
    page = recs[start:start + count]
    logid = ids.dy_logid(rng)
    next_cursor = (keys[start + len(page) - 1] - 1) if page else 0
    months = []
    for ct in (int(r.get("create_time") or 0) for r in recs):
        label = time.strftime("%Y·%m", time.gmtime(ct))
        if label not in months:
            months.append(label)
    return {
        "status_code": 0,
        "min_cursor": 0,
        "max_cursor": next_cursor,
        "has_more": 1 if start + count < len(recs) else 0,
        "aweme_list": [_dy_related_or_post_aweme(r, "dy_post_aweme") for r in page],
        "log_pb": {"impr_id": logid},
        "post_serial": 2,
        "replace_series_cover": 1,
        "request_item_cursor": int(max_cursor or 0),
        "time_list": months[:12],
    }


def render_page(dataset: DatasetReader, endpoint: str, page_no: int = 1,
                page_size: int = 10, **cursor_kwargs):
    """统一入口：返回 (请求参数或None, 响应dict)。"""
    site = dataset.manifest["site"]
    if site == "douyin" and endpoint in ("search_stream", "stream"):
        return None, render_douyin_stream(dataset, page_no, page_size,
                                          cursor_kwargs.get("keyword"))
    if site == "douyin" and endpoint in ("search_single", "single"):
        return render_douyin_single(dataset, page_no, page_size,
                                    cursor_kwargs.get("search_id"),
                                    cursor_kwargs.get("keyword"))
    if site == "douyin" and endpoint in ("search_item", "item"):
        return render_douyin_search_item(dataset, page_no, page_size,
                                         cursor_kwargs.get("search_id"),
                                         cursor_kwargs.get("keyword"))
    if site == "douyin" and endpoint in ("aweme_detail", "detail"):
        return None, render_douyin_detail(dataset, cursor_kwargs.get("line_no", 0))
    if site == "douyin" and endpoint in ("comment_list", "comments"):
        return None, render_douyin_comment_list(dataset, cursor_kwargs.get("line_no", 0),
                                                int(cursor_kwargs.get("cursor", 0) or 0),
                                                int(cursor_kwargs.get("count", 20) or 20))
    if site == "douyin" and endpoint in ("comment_list_reply", "comment_reply"):
        return None, render_douyin_comment_list_reply(
            dataset, cursor_kwargs.get("line_no", 0),
            str(cursor_kwargs.get("comment_id", "0") or "0"),
            int(cursor_kwargs.get("cursor", 0) or 0),
            int(cursor_kwargs.get("count", 20) or 20))
    if site == "xhs" and endpoint in ("search_notes", "notes"):
        return render_xhs_search_notes(dataset, page_no, page_size,
                                       cursor_kwargs.get("search_id"),
                                       cursor_kwargs.get("keyword"))
    if site == "xhs" and endpoint in ("comment_page", "comments"):
        return render_xhs_comment_page(dataset, cursor_kwargs.get("note_line_no", 0),
                                       cursor_kwargs.get("cursor", ""))
    if site == "kuaishou" and endpoint in ("search_feed", "feed"):
        return render_ks_search_feed(dataset, page_no, page_size,
                                     cursor_kwargs.get("pcursor", ""),
                                     cursor_kwargs.get("search_session_id"),
                                     cursor_kwargs.get("keyword"))
    raise ValueError(f"站点 {site} 不支持端点 {endpoint}（可用: {ENDPOINTS.get(site)}）")


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description="按端点+分页游标吐契约形态响应页")
    ap.add_argument("--dataset", required=True, help="数据集目录，如 synthgen/datasets/douyin")
    ap.add_argument("--endpoint", required=True, help="search_stream|search_single|search_notes|comment_page|search_feed")
    ap.add_argument("--page", type=int, default=1)
    ap.add_argument("--page-size", type=int, default=10)
    ap.add_argument("--note-line", type=int, default=0, help="comment_page 专用：笔记行号")
    ap.add_argument("--keyword", default=None, help="搜索请求 keyword（缺省从稳定池确定性选取）")
    ap.add_argument("--out", default=None, help="写出 JSON 文件（缺省打印首 4000 字符）")
    args = ap.parse_args(argv)

    ds = DatasetReader(Path(args.dataset))
    req, resp = render_page(ds, args.endpoint, args.page, args.page_size,
                            note_line_no=args.note_line, keyword=args.keyword)
    payload = {"request": req, "response": resp}
    text = json.dumps(payload, ensure_ascii=False, indent=2)
    if args.out:
        Path(args.out).write_text(text, encoding="utf-8")
        print(f"written: {args.out}")
    else:
        print(text[:4000])
    return 0


if __name__ == "__main__":
    sys.exit(main())
