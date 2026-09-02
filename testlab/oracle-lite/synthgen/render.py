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


def render_aweme(rec: dict, detail: bool = False) -> dict:
    """aweme_info / aweme_detail：真值模板物化（S2 修复主路径）。

    模板（sites/templates/dy_search_aweme|dy_detail_aweme.json）定义结构与静态值，
    数据集记录同路径值优先（aweme_id/desc/statistics/author/video/music 核心全走记录），
    must_vary 叶子形态保持合成；同一实体跨端点渲染一致。
    """
    fam = "dy_detail_aweme" if detail else "dy_search_aweme"
    if fam not in _templates():
        return augment_aweme(rec, detail)
    return materialize(rec, fam, base=rec)


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


def _dy_search_item_wrap(rec: dict, rng: np.random.Generator) -> dict:
    """搜索 data[i]：对齐真站 16 键形态（type/aweme_info/doc_type/...，红队 P1-D2）。

    type 分布按语料/live 实证：视频 1 为主（~86%），混排 6（~10%）与 77（~4%），
    保证 agent 的 `it.type===1` 过滤有结果。
    """
    r = rng.random()
    item_type = 1 if r < 0.86 else (6 if r < 0.96 else 77)
    aid = str(rec.get("aweme_id") or "")
    return {
        "type": item_type,
        "aweme_info": render_aweme(rec),
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

    start（R2-P2-1）：绝对起始下标（keyword 轮转基×20 + offset），与 count 解耦。"""
    records = dataset.abs_slice(start, page_size) if start is not None \
        else dataset.page_slice(page_no, page_size)
    rng = _page_rng(dataset, f"dy-stream-{page_no}" if start is None else f"dy-stream-a{start}")
    data = [_dy_search_item_wrap(rec, rng) for rec in records]
    return _dy_envelope(
        rng, ids.dy_logid(rng), page_no, page_size, len(dataset),
        data, stream=True, keyword=keyword, abs_start=start,
    )


def render_douyin_single(dataset: DatasetReader, page_no: int, page_size: int,
                         search_id: str | None = None,
                         keyword: str | None = None,
                         start: int | None = None) -> tuple[dict, dict]:
    """single 翻页形态：请求 search_id=上页 extra.logid、offset 递增；返回 (请求参数, 响应)。"""
    records = dataset.abs_slice(start, page_size) if start is not None \
        else dataset.page_slice(page_no, page_size)
    rng = _page_rng(dataset, f"dy-single-{page_no}" if start is None else f"dy-single-a{start}")
    request = {
        "search_id": search_id or "",
        "offset": (page_no - 1) * page_size,
        "count": page_size,
    }
    data = [_dy_search_item_wrap(rec, rng) for rec in records]
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
    records = dataset.abs_slice(start, page_size) if start is not None \
        else dataset.page_slice(page_no, page_size)
    rng = _page_rng(dataset, f"dy-item-{page_no}" if start is None else f"dy-item-a{start}")
    logid = ids.dy_logid(rng)
    data = [_dy_search_item_wrap(rec, rng) for rec in records]
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
_DY_CMT_TEXTS = [
    "这个{topic}也太绝了，收藏了", "刷到三次了，必须来评论一下", "跟着做了，全家都说好吃",
    "博主太用心了，文案和画面都在线", "请问{topic}用的什么工具呀", "已三连，蹲一个后续",
    "这才是我想看的{topic}，没有废话", "看了三遍，每一遍都有新收获", "评论区也太热闹了吧",
    "感谢分享，正好需要这个{topic}", "拍得真好，bgm 也选得妙", "第一天来打卡，以后天天看",
]
_DY_CMT_IP = ["四川", "广东", "浙江", "江苏", "北京", "上海", "山东", "湖北", "福建", "河南",
              "湖南", "安徽", "重庆", "陕西", "辽宁", "中国香港", "中国台湾", "日本", "美国", "新加坡"]


def _dy_comment_user(rng: np.random.Generator, rec: dict) -> dict:
    """评论用户：以作品作者结构为模板换身份（nickname 池确定性采样，uid/sec_uid 重派生）。"""
    user = dict(rec.get("author") or {})
    nickname = ids.pick(rng, ("爱吃", "爱看", "爱拍", "爱逛", "大碗", "元气", "快乐", "人间")
                        ) + ids.pick(rng, ("小王", "阿慧", "大乔", "小李", "老张", "汤圆", "栗子", "椰果")
                                     ) + ids.pick(rng, ("日常", "日记", "不吃香菜", "在线", "星球", "观察员"))
    uid = ids.dy_uid(rng)  # R2-P2-2：长度/首位按语料分布（恒 19 位 → 众数 16）
    user.update({
        "uid": uid,
        "short_id": ids.digits_str(rng, 11),
        "unique_id": ids.digits_str(rng, 11),
        "sec_uid": ids.dy_sec_uid(rng),
        "nickname": nickname,
        "signature": None,
        "follower_count": int(rng.integers(0, 20000)),
        "total_favorited": int(rng.integers(0, 200000)),
    })
    return user


def _dy_comment(rng: np.random.Generator, rec: dict, idx: int, total: int,
                now_ts: int | None = None) -> dict:
    """单条评论。红队 R2-P2-3：create_time = 发布后 [0, min(30d, now-发布)] 延迟
    （旧实现为发布前 60s~30d 的时间倒置）；cid 用真站雪花结构 (ctime-δ)<<32|rand32
    （R2-P2-2，语料 cid 首位 '7' 1175/1175、时间可编码）。"""
    topic = ""
    for te in rec.get("text_extra") or []:
        if isinstance(te, dict) and te.get("hashtag_name"):
            topic = te["hashtag_name"]
            break
    text = _DY_CMT_TEXTS[idx % len(_DY_CMT_TEXTS)].format(topic=topic or "视频")
    pub = int(rec.get("create_time") or _now_ms() // 1000)
    now = int(now_ts if now_ts is not None else _now_ms() // 1000)
    delay_span = max(1, min(30 * 86400, now - pub))  # 评论只能晚于发布（R2-P2-3）
    ctime = pub + int(rng.integers(0, delay_span))
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
        "text_extra": [],
        "text_music_info": None,
        "user": _dy_comment_user(rng, rec),
        "user_buried": False,
        "user_digged": 0,
        "video_list": None,
    }


def _dy_comment_item(rng: np.random.Generator, rec: dict, idx: int, total: int,
                     now_ts: int | None = None) -> dict:
    """评论条目：语义核心（cid/时间序/user）+ 真值模板物化（S2：comment user 69 键、
    text_extra 元素、can_create_item 等高频结构由 sites/templates/dy_comment_item 补齐）。"""
    base = _dy_comment(rng, rec, idx, total, now_ts=now_ts)
    if "dy_comment_item" not in _templates():
        return base
    return materialize({"aweme_id": rec.get("aweme_id"), "cid": base["cid"],
                        "create_time": base["create_time"]},
                       "dy_comment_item", base=base)


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


def _dy_comment_envelope(rng: np.random.Generator, comments, cursor: int, has_more: int,
                         total: int) -> dict:
    """comment/list 17 键信封（语料实证：comment_config/total/extra/.../sort_tags_report_map；
    无 comment_common_data——真值 113/131 无该键，S2 多余键删除）。"""
    return {
        "status_code": 0,
        "comments": comments,
        "cursor": cursor,
        "has_more": has_more,
        "reply_style": 2,
        "total": total,
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
                                 rec, cursor + i, total, now_ts=now) for i in range(n)]
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
    comments = []
    for i in range(n):
        c = _dy_comment_item(_page_rng(dataset, f"dy-rpl2-{comment_id}-{cursor + i}"),
                             rec, cursor + i, total, now_ts=now)
        c["reply_id"] = comment_id
        c["level"] = 2  # 楼中楼子评论层级（真站 reply 条目 level=2）
        if "dy_reply_item" in _templates():  # reply 专属高频键（root_comment_id 等，S2）
            c = materialize({"aweme_id": rec.get("aweme_id"), "cid": c["cid"],
                             "reply_id": comment_id, "create_time": c["create_time"]},
                            "dy_reply_item", base=c)
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

# xhs 评论文案池（数据集内嵌评论是「样例集」，总量以 interact_info.comment_count
# 为准——超出内嵌部分按 (note_id, 序号) 确定性合成，与 dy 评论同策略）
_XHS_CMT_TEXTS = [
    "太实用了，马上安排", "收藏了，周末就去试", "博主拍照也太好看了吧",
    "这个思路真的绝", "蹲一个详细教程", "已下单，回来汇报",
    "评论区也好有爱", "同款体验，确实不错", "感谢分享，正好需要",
    "看完了，受益匪浅", "风格好喜欢", "这也太治愈了",
]
_XHS_CMT_IP = ["江苏", "上海", "北京", "广东", "浙江", "四川", "山东",
               "湖北", "福建", "中国香港", "日本", "新加坡"]
_XHS_CMT_NICKS = ["奶茶三分甜", "晚风与猫", "山雾漫步", "一只柚子", "北岛有风",
                  "拾贝壳的人", "半夏微凉", "橘子汽水", "步履不停", "慢煮时光"]


def _xhs_sub_claim_for(comment_id: str) -> int:
    """xhs 子评论声称值 = f(comment_id) 确定性派生（0-9，语料实证 claim '9'）。"""
    return _stable_hash("xhs-sub-total::%s" % comment_id) % 10


def _xhs_synth_comment(rec: dict, idx: int) -> dict:
    """第 idx（≥内嵌数）条评论：确定性合成（同 note 同序号永远一致）。

    红队 R3-P2-1/R3-P1-1：sub_comment_count 与 /comment/sub/page 可得数同源
    （_xhs_sub_claim_for）；claim>0 时内嵌 1 条 + 游标续读（语料形态：
    claim '9' 字符串、内嵌 1、sub/page 翻尽 9）。
    """
    note_id = str(rec.get("id") or "")
    seed = _stable_hash("xhs-cmt::%s::%d" % (note_id, idx))
    rng = np.random.default_rng([seed & 0x7FFFFFFF, idx])
    pub_ms = int(rec.get("create_time") or 0) or _now_ms()
    ctime = int(pub_ms + (seed % (30 * 86400)) * 1000)
    author = (rec.get("note_card") or {}).get("user") or {}
    cid = ids.xhs_ts_hex_id(rng, ctime // 1000)
    sub_claim = _xhs_sub_claim_for(cid)
    c = {
        "id": cid,
        "note_id": note_id,
        "content": _XHS_CMT_TEXTS[idx % len(_XHS_CMT_TEXTS)],
        "create_time": ctime,
        "like_count": str(int(rng.integers(0, 900))),
        "liked": False,
        "invalid": False,
        "ip_location": ids.pick(rng, _XHS_CMT_IP),
        "pictures": [],
        "at_users": [],
        "show_tags": [],
        "status": 0,
        "sub_comment_count": str(sub_claim),
        "sub_comment_cursor": "",
        "sub_comment_has_more": False,
        "user_info": {
            "user_id": ids.hex_id(rng, 24),
            "nickname": ids.pick(rng, _XHS_CMT_NICKS),
            "image": "https://sns-avatar-qc.xhscdn.com/avatar/%s?imageView2/2/w/120/format/jpg"
                     % ids.hex_id(rng, 32),
            "xsec_token": ids.xhs_xsec_token(rng),
            "ai_agent": False,
        },
    }
    subs = _xhs_sub_comments_full(rec, c, sub_claim)
    if subs:
        c["sub_comments"] = subs[:1]  # 语料形态：大计数内嵌 1 条
        c["sub_comment_has_more"] = sub_claim > 1
        if sub_claim > 1:
            c["sub_comment_cursor"] = subs[0]["id"]
    return c


def _xhs_synth_sub_comment(rec: dict, root: dict, idx: int) -> dict:
    """root 的第 idx 条子评论：确定性合成（sub/page 翻页数据源）。"""
    note_id = str(rec.get("id") or "")
    root_id = str(root.get("id") or "")
    seed = _stable_hash("xhs-sub::%s::%s::%d" % (note_id, root_id, idx))
    rng = np.random.default_rng([seed & 0x7FFFFFFF, idx + 7])
    root_ct = int(root.get("create_time") or 0) or _now_ms()
    ctime = int(root_ct + (seed % (14 * 86400)) * 1000)  # 子评论晚于父评论
    return {
        "id": ids.xhs_ts_hex_id(rng, ctime // 1000),
        "note_id": note_id,
        "content": _XHS_CMT_TEXTS[(idx * 3 + 1) % len(_XHS_CMT_TEXTS)],
        "create_time": ctime,
        "like_count": str(int(rng.integers(0, 300))),
        "liked": False,
        "invalid": False,
        "ip_location": ids.pick(rng, _XHS_CMT_IP),
        "pictures": [],
        "at_users": [],
        "show_tags": [],
        "status": 0,
        "user_info": {
            "user_id": ids.hex_id(rng, 24),
            "nickname": ids.pick(rng, _XHS_CMT_NICKS),
            "image": "https://sns-avatar-qc.xhscdn.com/avatar/%s?imageView2/2/w/120/format/jpg"
                     % ids.hex_id(rng, 32),
            "xsec_token": ids.xhs_xsec_token(rng),
            "ai_agent": False,
        },
    }


def _xhs_sub_comments_full(rec: dict, root: dict, claim: int) -> list:
    """root 评论的全量子评论（内嵌在前 + 确定性合成补足到 claim，语料 claim=可得）。"""
    if claim <= 0:
        return []
    subs = [dict(s) for s in (root.get("sub_comments") or []) if isinstance(s, dict)]
    for idx in range(len(subs), claim):
        subs.append(_xhs_synth_sub_comment(rec, root, idx))
    return subs[:claim]


def _xhs_note_declared_comments(rec: dict) -> int:
    try:
        return int((((rec.get("note_card") or {}).get("interact_info") or {})
                    .get("comment_count")) or 0)
    except (TypeError, ValueError):
        return 0


def _xhs_note_comments(rec: dict) -> list:
    """笔记全量顶层评论：内嵌样例集 + 超出部分按 (note_id, 序号) 确定性合成。

    comment/page 与 comment/sub/page 共用同一构建（红队 R3-P2-1：
    两端点必须看到同一份评论列表，sub 计数/游标才能对得上）。
    """
    comments = [dict(c) for c in (rec.get("comments") or []) if isinstance(c, dict)]
    declared = _xhs_note_declared_comments(rec)
    for idx in range(len(comments), max(len(comments), declared)):
        comments.append(_xhs_synth_comment(rec, idx))
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
    page_size 混用时窗口不再漂移（页号模式为兼容保留）。"""
    records = dataset.abs_slice(start, page_size) if start is not None \
        else dataset.page_slice(page_no, page_size)
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
    comments = _xhs_note_comments(rec)
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
    if "xhs_comment_item" in _templates():  # S2：评论条目按真值模板物化（user_info 等对齐）
        note_ref = {"id": rec.get("id"), "note_id": rec.get("id"),
                    "create_time": rec.get("create_time")}
        page = [materialize(note_ref, "xhs_comment_item", base=c) for c in page]
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
    for c in _xhs_note_comments(rec):
        if str(c.get("id") or "") == str(root_comment_id):
            root = c
            break
    subs: list = []
    if root is not None:
        try:
            claim = int(str(root.get("sub_comment_count") or "0"))
        except (TypeError, ValueError):
            claim = len(root.get("sub_comments") or [])
        subs = _xhs_sub_comments_full(rec, root, claim)
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
    out = []
    for s in page:
        s = dict(s)
        s["target_comment"] = {"id": str((root or {}).get("id") or ""), "user_info": root_user}
        out.append(s)
    if "xhs_comment_item" in _templates():
        note_ref = {"id": rec.get("id"), "note_id": rec.get("id"),
                    "create_time": rec.get("create_time")}
        out = [materialize(note_ref, "xhs_comment_item", base=s) for s in out]
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
def render_ks_search_feed(dataset: DatasetReader, page_no: int, page_size: int,
                          pcursor: str = "", search_session_id: str | None = None,
                          keyword: str | None = None) -> tuple[dict, dict]:
    """快手搜索 feed：请求 body pcursor（首屏 ""），响应 pcursor "1"→"2" 递增 + searchSessionId 回传。"""
    records = dataset.page_slice(page_no, page_size)
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
    response = {
        "result": 1,
        "pcursor": str(page_no),
        "searchSessionId": search_session_id,
        "llsid": ids.digits_str(rng, 19),
        "host-name": ids.opaque(rng, 52),
        "webPageArea": "searchxxnull",
        "feeds": [materialize(rec, "ks_feed_item", base=rec) for rec in records],
    }
    return request, response


# ---------------------------------------------------------------------------
# 快手补面（红队 R3-P1-2）：GraphQL 12 operation + REST 评论/作者/用户搜索。
# 信封/键集全部按语料实证形态（ks_corpus_baseline3.json）：
#   - GraphQL 统一 {"data": {...}} 包装，vision* 键名 + __typename；
#   - REST photo/comment/list：rootCommentsV2/commentCountV2/pcursorV2 信封（snake_case 键）；
#   - 评论计数与 search feed 的 comment.us_c 同源（跨端点计数一致）。
# ---------------------------------------------------------------------------
_KS_CMT_TEXTS = [
    "太真实了，看完直接愣住", "这就是生活啊", "拍得真好，支持一下", "评论区都是懂行的",
    "第一次见这么拍的", "跟着拍同款去了", "这个必须收藏", "主播也太有才了",
    "看完了，意犹未尽", "这波操作满分", "家乡味一下子就来了", "满满的回忆",
]
_KS_CMT_NAMES = ["山城小面", "北巷", "大力的日常", "爱吃辣的猫", "晚风拾遗", "半亩方塘",
                 "奔跑的柚子", "老陈说事", "小城故事", "拾光记", "南方以南", "一颗白菜"]
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


def _ks_sub_total_for(comment_id: str) -> int:
    return _stable_hash("ks-sub-total::%s" % comment_id) % 4


def _ks_comment_total(rec: dict) -> int:
    """评论总数与 search feed 的 comment.us_c 同源（跨端点计数一致）。"""
    try:
        return int((rec.get("comment") or {}).get("us_c") or 0)
    except (TypeError, ValueError):
        return 0


def _ks_comments_core(dataset: DatasetReader, line_no: int, pcursor: str, count: int):
    """快手评论页核心数据（REST/GraphQL 共用）：返回 (条目核心列表, total, start, more)。"""
    rec = dataset.read(line_no)
    total = _ks_comment_total(rec)
    rng = _page_rng(dataset, f"ks-cmt-{line_no}-{pcursor or 'p0'}")
    start = 0
    if pcursor and pcursor not in ("", "no_more") and pcursor.isdigit():
        start = max(0, min(int(pcursor) - _KS_CMT_CURSOR_BASE, total))
    n = max(0, min(count, total - start))
    items = []
    for i in range(n):
        idx = start + i
        pub_ms = int((rec.get("photo") or {}).get("timestamp") or _now_ms())
        span = max(1, min(90 * 86400 * 1000, _now_ms() - pub_ms))
        ts = int(pub_ms + rng.integers(0, span))
        cid = str(_KS_CMT_CURSOR_BASE + idx)
        items.append({
            "cid": cid,
            "author_id": ids.ks_id(rng),
            "author_name": _KS_CMT_NAMES[int(rng.integers(0, len(_KS_CMT_NAMES)))],
            "content": _KS_CMT_TEXTS[idx % len(_KS_CMT_TEXTS)],
            "headurl": ("http://p66-plat.wsbkwai.com/uhead/AB/2026/03/04/02/BMjAyNjAzMDQwMjUyMjRf"
                        "%s_1_hd%d_%d_s.jpg?x-kcdn-pid=112530"
                        % (ids.opaque(rng, 20), int(rng.integers(600, 900)), int(rng.integers(100, 999)))),
            "timestamp": ts,
            "liked": False,
            "status": "done",
            "liked_count": int(rng.integers(0, 5000)),
            "sub_total": _ks_sub_total_for(cid),
        })
    more = start + n < total
    return items, total, start, more, rng


def render_ks_comment_list(dataset: DatasetReader, line_no: int, pcursor: str = "",
                           count: int = 20) -> dict:
    """POST /rest/v/photo/comment/list（REST 信封：rootCommentsV2/commentCountV2/pcursorV2）。"""
    items, total, start, more, rng = _ks_comments_core(dataset, line_no, pcursor, count)
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
        "pcursorV2": str(_KS_CMT_CURSOR_BASE + start + len(items)) if more else "no_more",
        "commentCountV2": total,
        "rootCommentsV2": roots,
    }


def _ks_comment_list_graphql(dataset: DatasetReader, line_no: int, pcursor: str = "",
                             count: int = 20) -> dict:
    """commentListQuery 的 visionCommentList 形态（camelCase 字符串 id；尾页 pcursor=null）。"""
    items, total, start, more, _ = _ks_comments_core(dataset, line_no, pcursor, count)
    roots = [{
        "commentId": c["cid"], "authorId": c["author_id"], "authorName": c["author_name"],
        "content": c["content"], "headurl": c["headurl"], "timestamp": c["timestamp"],
        "hasSubComments": c["sub_total"] > 0, "likedCount": str(c["liked_count"]),
        "liked": c["liked"], "status": c["status"], "__typename": "VisionRootCommentItem",
    } for c in items]
    return {
        "commentCount": None,
        "commentCountV2": total,
        "pcursor": str(_KS_CMT_CURSOR_BASE + start + len(items)) if more else None,
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
        return {"data": {"visionConfig": _KS_VISION_CONFIG_STATIC}}
    if operation == "visionBaseEmoticons":
        return {"data": {"visionBaseEmoticons": {"iconUrls": _KS_EMOTICON_URLS}}}
    if operation == "visionShortVideoReco":
        feeds = []
        if line_no is not None:
            n = len(dataset)
            for i in range(1, 21):
                r = dataset.read((line_no + i) % n)
                photo = dict(r.get("photo") or {})
                photo["__typename"] = "PhotoEntity"
                photo.setdefault("originCaption", photo.get("caption") or "")
                photo["realLikeCount"] = int(photo.get("likeCount") or 0)
                photo["commentCount"] = None
                author = dict(r.get("author") or {})
                author["__typename"] = "VisionShortVideoAuthor"
                feeds.append({"type": 1, "author": author, "photo": photo})
        return {"data": {"visionShortVideoReco": {"llsid": ids.digits_str(rng, 19), "feeds": feeds}}}
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


def render_ks_sub_comments(dataset: DatasetReader, line_no: int, root_comment_id: str,
                           pcursor: str = "") -> dict:
    """visionSubCommentList：{pcursor:"", subComments:[], pcursorV2, subCommentsV2}。"""
    rec = dataset.read(line_no)
    rng = _page_rng(dataset, f"ks-sub-{line_no}-{root_comment_id}-{pcursor or 'p0'}")
    total = _ks_sub_total_for(root_comment_id)
    start = 0
    if pcursor and pcursor.isdigit():
        start = max(0, min(int(pcursor) - _KS_CMT_CURSOR_BASE, total))
    n = max(0, min(10, total - start))
    root_name = _KS_CMT_NAMES[_stable_hash("ks-root-name::%s" % root_comment_id)
                              % len(_KS_CMT_NAMES)]
    subs = []
    for i in range(n):
        idx = start + i
        pub_ms = int((rec.get("photo") or {}).get("timestamp") or _now_ms())
        span = max(1, min(90 * 86400 * 1000, _now_ms() - pub_ms))
        ts = int(pub_ms + rng.integers(0, span))
        subs.append({
            "commentId": str(_KS_CMT_CURSOR_BASE + idx + 1), "authorId": ids.ks_id(rng),
            "authorName": _KS_CMT_NAMES[int(rng.integers(0, len(_KS_CMT_NAMES)))],
            "content": _KS_CMT_TEXTS[(idx * 5 + 2) % len(_KS_CMT_TEXTS)],
            "headurl": ("http://p66-plat.wsbkwai.com/uhead/AB/2026/05/11/07/BMjAyNjA1MTEw"
                        "NzI1MjRf%s_2_hd%d_%d_s.jpg?x-kcdn-pid=112530"
                        % (ids.opaque(rng, 20), int(rng.integers(600, 900)), int(rng.integers(100, 999)))),
            "timestamp": ts, "hasSubComments": False,
            "likedCount": str(int(rng.integers(0, 200))), "liked": False, "status": "done",
            "replyToUserName": root_name, "replyTo": ids.ks_id(rng),
            "__typename": "VisionSubCommentItem",
        })
    more = start + n < total
    return {"data": {"visionSubCommentList": {
        "pcursor": "", "subComments": [],
        "pcursorV2": str(_KS_CMT_CURSOR_BASE + start + n) if more else "no_more",
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
    "[惊讶]": "NzY3NTYwMDM2dGhpcmRfcGFydHlfczEyOTY3NDMwNjEucG5nEIfN1y8",
    "[ slime ]": "NzY3NTk3ODA0N3RoaXJkX3BhcnR5X3MxMjk2NzQ3MDUucG5nEIfN1y8",
}.items()}


def render_ks_profile_get(dataset: DatasetReader, line_nos: list, user_id: str) -> dict:
    """GET /rest/v/profile/get（语料键集：eid/like/userTex/sex/mobile/follows/host-name/
    userName/userId/userDefineId/fans/result/userHead）。"""
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
        "userName": name, "userId": uid_num,
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


def render_ks_profile_feed(dataset: DatasetReader, line_nos: list, user_id: str,
                           pcursor: str = "") -> dict:
    """POST /rest/v/profile/feed（作者作品页：type/tags/authorStatement/author/photo）。"""
    rng = _page_rng(dataset, f"ks-pfeed-{user_id}-{pcursor or 'p0'}")
    per_page = 18
    start = 0
    if pcursor and pcursor not in ("", "no_more"):
        # 语料形态 pcursor 为科学计数法毫秒（"1.785484910875E12"）：确定性映射回页号
        start = (_stable_hash("ks-pfeed-cur::%s" % pcursor) % 12) * per_page
    recs = [dataset.read(i) for i in line_nos]
    page_recs = recs[start:start + per_page]
    feeds = []
    for r in page_recs:
        item = materialize(r, "ks_feed_item", base=r)
        feeds.append({
            "type": r.get("type") or 1,
            "tags": r.get("tags") or [],
            "authorStatement": {"content": "作者声明：含AI生成内容", "type": 1},
            "author": item.get("author") or r.get("author"),
            "photo": item.get("photo") or r.get("photo"),
        })
    more = start + per_page < len(recs)
    next_cur = "no_more" if not more else ("%.12f" % (_now_ms() / 1e12)).rstrip("0") + "E12"
    return {"result": 1, "pcursor": next_cur, "feeds": feeds}


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
def render_douyin_related(dataset: DatasetReader, line_no: int, count: int = 10) -> dict:
    """/aweme/v1/web/aweme/related/（语料 6 键信封：aweme_list/chime_video_list/
    filter_infos/has_more/log_pb/status_code，n=10）——同数据窗口邻域切片。"""
    rng = _page_rng(dataset, f"dy-related-{line_no}")
    n = len(dataset)
    recs = [dataset.read((line_no + 1 + i) % n) for i in range(count)]
    logid = ids.dy_logid(rng)
    return {
        "status_code": 0,
        "aweme_list": [render_aweme(rec) for rec in recs],
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
        "custom_verify": a.get("custom_verify") or "",
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
    """/aweme/v1/web/aweme/post/（作者作品列表：min/max_cursor + aweme_list + has_more）。"""
    rng = _page_rng(dataset, "dy-uposted-%d" % (len(line_nos)))
    start = 0
    if max_cursor:
        for i, ln in enumerate(line_nos):
            if dataset.read(ln).get("create_time", 0) * 1000 <= max_cursor:
                start = i
                break
    page = line_nos[start:start + count]
    recs = [dataset.read(i) for i in page]
    logid = ids.dy_logid(rng)
    next_cursor = (recs[-1]["create_time"] * 1000 - 1) if recs else 0
    return {
        "status_code": 0,
        "min_cursor": 0,
        "max_cursor": next_cursor,
        "has_more": 1 if start + count < len(line_nos) else 0,
        "aweme_list": [render_aweme(r) for r in recs],
        "log_pb": {"impr_id": logid},
        "extra": {"fatal_item_ids": [], "logid": logid, "now": _now_ms()},
    }
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
