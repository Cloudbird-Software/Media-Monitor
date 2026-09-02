# -*- coding: utf-8 -*-
"""synth_api —— 合成模式 API 服务（阶段 3 收尾：双模式回放的「合成通道」）。

按端点路由调用 synthgen.render，把预置数据集吐成契约形态响应，并托管
build_pages.py 生成的三站合成站页面骨架。全部监听 127.0.0.1，不访问任何真站。

端口规划（oracle-lite 独立段；完整 oracle 实测台架为 866x/855x，互不冲突）：
  抖音 douyin    8761
  小红书 xhs     8762
  快手 kuaishou  8763
  如需对齐完整 oracle 的 harness HOST_REWRITE 855x 改写约定，可显式
  --also-bind 8551,8552,8553 额外绑定（同一 handler、同一路由、同一份数据，
  被占用则 WARN 跳过，绝不强占）。

契约端点路由（path 匹配、不校验 Host，与 Hoverfly webserver 行为一致）：
  douyin  GET  /aweme/v1/web/general/search/stream/   首屏 offset=0
  douyin  GET  /aweme/v1/web/general/search/single/   ?offset=&count=&search_id=
          （翻页链：search_id=上页 extra.logid 回传，cursor 数值递增）
  douyin  GET  /aweme/v1/web/aweme/detail/            ?aweme_id=（红队 P1-D1；miss→200+null）
  douyin  GET  /aweme/v1/web/comment/list/            ?aweme_id=&cursor=&count=（数值游标切片）
  douyin  GET  /aweme/v1/web/comment/list/reply/      ?comment_id=（楼中楼，8 键信封）
  douyin  GET  /aweme/v1/web/search/item/             视频频道搜索（12 键信封）
  xhs     POST /api/sns/web/v2/search/notes           body 12 必填项；缺 filters 时兜底注入
          （verification_fix_round2.md P3-R2-1；空/缺 keyword 或超末页 → 真站空信封，P1-X1）
  xhs     GET  /api/sns/web/v2/comment/page           ?note_id=&cursor=（comment_page 亦路由）
          （游标=上页末条评论 id，切片推进，取尽 has_more=false，P1-X2）
  kuaishou POST /rest/v/search/feed                   body pcursor=""（首屏）→"1"→"2"
          （+ searchSessionId 回传）

真站 URL 路径别名（红队 P2-D6/P3-4/R3-P1-2）：/search/<kw>、/video/<id>（dy）、
  /search_result?keyword=、/explore/<id>（xhs，不存在笔记 302 → /404 真站形态）、
  /search/video?searchKey=、/short-video/<id>、/profile/<id>（ks；/search 对齐真站 404）。
  xhs/ks 详情与作者页数据 SSR 内嵌（__INITIAL_STATE__ 注入，真站形态）；
  dy 详情页走真路径 /aweme/detail XHR。

红队第 4 轮修复（R4）：dy 评论确定性派生 + 短 TTL 响应缓存（P2-1）、count=0 → 0 条
  与 page_size 钳制/越界 cursor 收敛（P3-1/2/3）、/favicon.ico + /robots.txt 200 与
  页面 link icon（P3-6/7）、Server 头去尾随空格（P3-8）。
红队第 5 轮修复（R5B/R5C，服务线）：
  - R5B-P1 dy 作者主页：sec_uid 空间与 author 实体打通（_page_alias 按行号聚合，
    SSR works/like_works 非空，「作品/喜欢」tab 可用）；
  - R5B-P3 meta/OG/JSON-LD 注入层（xhs Article/og、dy+ks BreadcrumbList、
    canonical/keywords/theme-color）、dy /jingxuan/search/<kw> 路由、xhs /explore
    与 /search_result/<id> 路由、ks /search/user 页；
  - R5C-P2-1 全局错误卫生：未捕获异常按站点错误族回 200 信封（不 500、不泄
    traceback）；POST body 类型非法逐端点容错（page:"abc" 等 → 缺省值）；
  - R5C-P2-2 关键词语义门移除（R4-P3-4 前提被 live 证伪）：任意非空关键词按
    keyword 轮转窗口出结果；仅空关键词/越界/count=0 走真站空态族；
  - R5C-P3 TTL 缓存命中时刷新时变字段（extra.now/logid/impr_id）、Accept-Encoding
    q 值解析（q=0 不回该编码；dy 优先 br、不支持时退 identity）、主域 API 响应
    不再附 Vary:Origin/Cache-Control:no-store（dy 压缩响应按语料回
    Vary: Accept-Encoding）、xhs 同源 OPTIONS 200+ACAM 五方法、ks pcursor=
    "no_more" 回传不回绕。
用户增强端点（Media-Monitor adapt_synth 契约）：dy /aweme/v1/web/user/profile/other
  兼容 sec_uid 调用形 + user_list 绑定（语料原调用形不变）；ks /api/user/info
  （sec_uid → $.user_list）。xhs /comment/sub/page 主参数 root_comment_id（语料 64/64），
  comment_id 仅作 MM 契约错误期间的过渡别名。

本地调试端点（127.0.0.1 专用，页面/harness 不引用——红队 R3-P1-3 调试白名单形态）：
  GET /_synth/health     健康检查（含 records 总数）
  GET /_synth/entity?id= 按 record_id 取整条实体
  GET /_synth/cover?seed=&w=&h=  确定性渐变 SVG 封面（页面实际走各站 CDN 形态路径）
  GET /  /search  /detail  /profile  合成站页面骨架（pages/out/<site>/）

keyword 轮转：render_page 的数据窗口按 (site, keyword) 稳定哈希整体平移
（窗口页数自适应：min(400, 数据集页数)），保证「换关键词 → 结果集确实刷新」，
同时保持确定性——e2e 用同一公式即可核对「页面数据 == synthgen 数据集」。

运行（任意含 numpy 的 Python 3.11+）：
  python synth_api.py --site all
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import sqlite3
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, quote, unquote, urlparse

_HERE = Path(__file__).resolve().parent
# oracle-lite：自包含打包——synthgen 包与数据集都在本目录内（_HERE 即包根），
# 不依赖 oracle 全库（原始版本 _ORACLE_ROOT 指向 oracle/ 顶层）。
if str(_HERE) not in sys.path:
    sys.path.insert(0, str(_HERE))

from synthgen import render as synth_render  # noqa: E402
from synthgen import ids as synth_ids  # noqa: E402

SITES = ("douyin", "xhs", "kuaishou")
# oracle-lite 端口独立段：876x（完整 oracle 实测台架占用 866x/855x，互不冲突）
SITE_PORTS = {"douyin": 8761, "xhs": 8762, "kuaishou": 8763}  # 站点→端口
# 完整 oracle 的 synth 模式 855x 兼容绑定（harness HOST_REWRITE）——oracle-lite
# 默认不绑；如需对齐 harness 改写，可 --also-bind 8551,8552,8553 显式指定。
SITE_COMPAT_PORTS = {"douyin": 8551, "xhs": 8552, "kuaishou": 8553}

_CFG = {"datasets": _HERE / "synthgen" / "datasets",
        "pages": _HERE / "pages" / "out"}

DEFAULT_PAGE_SIZE = 20
KEYWORD_ROTATION_PAGES = 400  # keyword 轮转窗口上限（页）
XHS_PAGE_SIZE_MAX = 20  # xhs search page_size 语料实证上限（37/37 请求全为 20）


def stable_hash(s: str) -> int:
    return int.from_bytes(hashlib.blake2b(s.encode("utf-8"), digest_size=8).digest(), "big")


class SiteState:
    """站点级共享状态：DatasetReader（懒加载行偏移）+ index.db 只读连接。"""

    def __init__(self, site: str):
        self.site = site
        self.ds: synth_render.DatasetReader | None = None
        self._ds_lock = threading.Lock()
        self._db_lock = threading.Lock()
        self._db: sqlite3.Connection | None = None
        self._sec_map_lock = threading.Lock()
        self._sec_uid_map: dict | None = None  # dy 专用：sec_uid -> (uid, 首个 line_no)

    def dataset(self) -> synth_render.DatasetReader:
        if self.ds is None:
            with self._ds_lock:
                if self.ds is None:
                    ds = synth_render.DatasetReader(_CFG["datasets"] / self.site)
                    n = len(ds)  # 触发行偏移构建（一次性扫描 JSONL）
                    print("[synth_api] %s dataset ready: %d records (%s)"
                          % (self.site, n, ds.entity), flush=True)
                    self.ds = ds
        return self.ds

    def line_no_of(self, record_id: str) -> int | None:
        with self._db_lock:
            if self._db is None:
                self._db = sqlite3.connect(
                    "file:%s?mode=ro" % (_CFG["datasets"] / self.site / "index.db"),
                    uri=True, check_same_thread=False)
            row = self._db.execute(
                "SELECT line_no FROM records WHERE record_id = ?", (record_id,)).fetchone()
            return row[0] if row else None

    def author_line_nos(self, author_key: str) -> list:
        """按 index.db author_id 取作者全部作品行号（xhs user_id / ks author.id / dy uid）。"""
        with self._db_lock:
            if self._db is None:
                self._db = sqlite3.connect(
                    "file:%s?mode=ro" % (_CFG["datasets"] / self.site / "index.db"),
                    uri=True, check_same_thread=False)
            rows = self._db.execute(
                "SELECT line_no FROM records WHERE author_id = ? ORDER BY line_no",
                (author_key,)).fetchall()
        return [r[0] for r in rows]

    def _build_sec_uid_map(self) -> dict:
        """dy：sec_uid -> (uid, line_nos)。index.db 只索引 uid，反向映射一次性扫描构建。"""
        import time as _t
        t0 = _t.time()
        m: dict = {}
        ds = self.dataset()
        for i in range(len(ds)):
            try:
                a = ds.read(i).get("author") or {}
            except Exception:
                continue
            sec = a.get("sec_uid")
            if sec:
                ent = m.setdefault(sec, (str(a.get("uid") or ""), []))
                ent[1].append(i)
        print("[synth_api] douyin sec_uid map ready: %d authors (%.1fs)"
              % (len(m), _t.time() - t0), flush=True)
        return m

    def sec_uid_map(self) -> dict:
        if self._sec_uid_map is None:
            with self._sec_map_lock:
                if self._sec_uid_map is None:
                    self._sec_uid_map = self._build_sec_uid_map()
        return self._sec_uid_map

    def sec_uid_line_no(self, sec_uid: str) -> int | None:
        ent = self.sec_uid_map().get(sec_uid)
        return ent[1][0] if ent and ent[1] else None

    def author_line_nos_by_sec_uid(self, sec_uid: str) -> list:
        ent = self.sec_uid_map().get(sec_uid)
        return list(ent[1]) if ent else []


STATES = {s: SiteState(s) for s in SITES}

# 评论 cid → 行号索引（红队 R3-P1-1「数据与端点全链接通」）：
# comment/list 渲染出的每条评论 cid 登记其所属实体行号，reply/sub 端点在调用方
# 只带 comment_id（不带 item_id/note_id）时也能定位数据——「拿到 cid 就能翻楼中楼」。
_CID_INDEX: dict = {"douyin": {}, "xhs": {}, "kuaishou": {}}
_CID_INDEX_LOCK = threading.Lock()
_CID_INDEX_CAP = 200000


def _cid_remember(site: str, comments, line_no: int):
    if not comments:
        return
    with _CID_INDEX_LOCK:
        idx = _CID_INDEX[site]
        if len(idx) > _CID_INDEX_CAP:
            idx.clear()
        for c in comments:
            cid = c.get("cid") if isinstance(c, dict) else None
            if cid:
                idx[str(cid)] = line_no


def _cid_lookup(site: str, cid: str) -> int | None:
    if not cid:
        return None
    with _CID_INDEX_LOCK:
        return _CID_INDEX[site].get(str(cid))


def rotation_pages(site: str, keyword: str | None) -> int:
    """keyword → 稳定数据窗口平移（0..N-1 页）；e2e 用同公式核对数据一致性。

    oracle-lite：窗口上限按数据集大小自适应（模数 = min(400, 数据集页数)）。
    完整 oracle 数据集 ≥8 万条/站，固定 400 页（8000 条）窗口永不出界；
    迷你数据集（300 条/站 → 15 页）用同公式缩模数，保证任意关键词都能命中
    数据窗口——确定性不变（模数由数据集记录数唯一确定）。"""
    kw = (keyword or "").strip() or "<default>"
    n_pages = max(1, len(STATES[site].dataset()) // DEFAULT_PAGE_SIZE)
    return stable_hash("%s::%s" % (site, kw)) % min(KEYWORD_ROTATION_PAGES, n_pages)


# ---------------------------------------------------------------------------
# 短 TTL 响应缓存（红队 R4-P2-1：dy 评论端点 4s 内重复请求确定性一致）
#
# 渲染层已确定性派生（cid/create_time/reply_comment_total 跨请求恒定），本缓存对
# 「同一 (端点, id, 游标, count)」的响应 dict 做 8s TTL 内复用（≥4s 窗口）。
#
# 红队 R5C-P3-1：缓存命中改为「内容复用 + 时变字段按请求重掷」——真站公理
# （R2-P3-1）：logid 每请求唯一、extra.now 恒为响应时点。旧实现原对象整只复用，
# A→B→A 交错请求时 extra.now 回退、logid 跨请求同值，可被「logid 去重 / now
# 单调」型检测器识别。现在命中时浅拷贝并刷新 extra.now / extra.logid /
# log_pb.impr_id（数据内容字节不变，仅时变字段每请求唯一）。
# ---------------------------------------------------------------------------
_RESP_CACHE: dict = {}
_RESP_CACHE_LOCK = threading.Lock()
_RESP_CACHE_TTL = 8.0
_RESP_CACHE_MAX = 512


def _refresh_dy_volatile(val: dict) -> dict:
    """dy 信封时变字段重掷（R5C-P3-1）：返回浅拷贝，不动缓存原对象。

    只刷新已存在的键（comment 族信封 extra 无 logid 键——不得因刷新引入新键，
    否则 TTL 内外响应键集漂移）；extra.now 恒为响应时点、log_pb.impr_id 每请求
    唯一（与 x-tt-logid 头同源）。
    """
    out = dict(val)
    now_ms = int(time.time() * 1000)
    logid = synth_ids.dy_logid(None)
    if isinstance(val.get("extra"), dict):
        extra = dict(val["extra"], now=now_ms)
        if "logid" in extra:
            extra["logid"] = logid
        out["extra"] = extra
    if isinstance(val.get("log_pb"), dict):
        out["log_pb"] = dict(val["log_pb"], impr_id=logid)
    return out


def _cached_response(key: tuple, fn):
    now = time.time()
    with _RESP_CACHE_LOCK:
        ent = _RESP_CACHE.get(key)
        if ent is not None and now - ent[0] < _RESP_CACHE_TTL:
            return _refresh_dy_volatile(ent[1])  # 命中：内容一致 + 时变字段新鲜
    val = fn()
    if not isinstance(val, dict):
        return val
    with _RESP_CACHE_LOCK:
        if len(_RESP_CACHE) > _RESP_CACHE_MAX:
            cutoff = now - _RESP_CACHE_TTL
            for k in [k for k, v in _RESP_CACHE.items() if v[0] < cutoff] or list(_RESP_CACHE)[:1]:
                _RESP_CACHE.pop(k, None)
        _RESP_CACHE[key] = (now, val)
    return dict(val)


# ---------------------------------------------------------------------------
# POST body 容错取值（红队 R5C-P2-1）
#
# 旧版三处裸 int(body.get(...))（xhs page/page_size、ks page_size/count）在
# page:"abc" / page:[2] / keyword:["美食"] 等类型非法 body 下落未捕获异常，
# 回 500 + CPython 异常信封——语料 68,405 请求 0 个 500。GET 侧 parse_qs 恒字符串
# （_int_param 已容错），本组助手把同款容错补到 POST body：数字/数字串/浮点按
# 数值解析，其余类型（list/dict/不可解析串）回缺省值，绝不上抛。
# ---------------------------------------------------------------------------


def _body_int(body: dict, key: str, default: int, lo: int | None = None,
              hi: int | None = None) -> int:
    v = (body or {}).get(key)
    if v is None:
        v = default
    if isinstance(v, bool):
        v = int(v)
    elif isinstance(v, (int, float)):
        v = int(v)
    elif isinstance(v, str):
        try:
            v = int(float(v)) if ("." in v or "e" in v.lower()) else int(v)
        except ValueError:
            v = default
    else:  # list/dict/None 等复合类型 → 缺省
        v = default
    if lo is not None:
        v = max(lo, v)
    if hi is not None:
        v = min(hi, v)
    return v


def _body_str(body: dict, key: str, default: str = "") -> str:
    v = (body or {}).get(key)
    if v is None:
        return default
    if isinstance(v, str):
        return v
    if isinstance(v, bool):
        return default
    if isinstance(v, (int, float)):
        return str(v)
    return default  # list/dict → 当缺失（真站对非字符串关键词走空态族）


def _json_body(handler_response: tuple | dict):
    """render 返回 (request, response) 或纯 response → 取响应部分。"""
    if isinstance(handler_response, tuple):
        return handler_response[-1]
    return handler_response


def _clip_page(state: SiteState, page_no: int) -> int:
    """页号钳制到数据集范围内（100k 条 / 20 条页 = 5000 页，正常不会触界）。"""
    max_page = max(1, len(state.dataset()) // DEFAULT_PAGE_SIZE)
    return max(1, min(page_no, max_page))


# ---------------------------------------------------------------------------
# 契约端点实现
# ---------------------------------------------------------------------------
def _int_param(params: dict, key: str, default: int, lo: int | None = None,
               hi: int | None = None) -> int:
    try:
        v = int((params.get(key) or [str(default)])[0] or default)
    except (TypeError, ValueError):
        v = default
    if lo is not None:
        v = max(lo, v)
    if hi is not None:
        v = min(hi, v)
    return v


def _dy_abs_start(state: SiteState, keyword: str, offset: int) -> int:
    """dy 搜索窗口绝对起始下标（红队 R2-P2-1）：rotation×20 + offset。

    offset 语义与 count 彻底解耦——同一 offset 换不同 count 命中同一起始记录
    （真站 offset 为结果集内绝对位置；旧实现 page=rotation+offset//count+1
    使同一 offset 在 count=10/20 下定位到完全不同记录）。"""
    return rotation_pages("douyin", keyword) * DEFAULT_PAGE_SIZE + offset


def dy_search_stream(state: SiteState, params: dict) -> dict:
    keyword = (params.get("keyword") or [""])[0]
    offset = _int_param(params, "offset", 0)
    count = _int_param(params, "count", DEFAULT_PAGE_SIZE)
    if offset < 0:  # 红队 P2 修复 10：负 offset 视为 0
        offset = 0
    if count <= 0:  # count=0 → 空集（不再钳到 1）
        return synth_render.render_douyin_search_empty(keyword, offset, stream=True)
    n_total = len(state.dataset())
    start = _dy_abs_start(state, keyword, offset)
    if start >= n_total:  # 超末页 → 空集 + cursor 原样回显（不回绕）
        return synth_render.render_douyin_search_empty(keyword, offset, stream=True)
    resp = _json_body(synth_render.render_douyin_stream(
        state.dataset(), 1, count, keyword, start=start))
    # 游标为「keyword 窗口内的相对 offset」（契约实证：首屏 offset=0，cursor=下页 offset）
    resp["cursor"] = offset + count
    return resp


def dy_search_single(state: SiteState, params: dict) -> dict:
    keyword = (params.get("keyword") or [""])[0]
    offset = _int_param(params, "offset", 0)
    count = _int_param(params, "count", DEFAULT_PAGE_SIZE)
    search_id = (params.get("search_id") or [""])[0] or None
    if offset < 0:
        offset = 0
    if count <= 0:
        return synth_render.render_douyin_search_empty(keyword, offset, stream=False)
    n_total = len(state.dataset())
    start = _dy_abs_start(state, keyword, offset)
    if start >= n_total:
        return synth_render.render_douyin_search_empty(keyword, offset, stream=False)
    req, resp = synth_render.render_douyin_single(
        state.dataset(), 1, count, search_id, keyword, start=start)
    # 游标链语义：cursor 相对 keyword 窗口递增（offset 0→20→40…），e2e 以响应值翻页
    resp["cursor"] = offset + count
    return resp


def dy_search_item(state: SiteState, params: dict) -> dict:
    """/aweme/v1/web/search/item（视频频道搜索，红队 P1-D1 附带项）：与 single 同游标语义。"""
    keyword = (params.get("keyword") or [""])[0]
    offset = _int_param(params, "offset", 0)
    count = _int_param(params, "count", DEFAULT_PAGE_SIZE)
    search_id = (params.get("search_id") or [""])[0] or None
    if offset < 0:
        offset = 0
    if count <= 0:
        return synth_render.render_douyin_search_empty(keyword, offset, stream=False)
    n_total = len(state.dataset())
    start = _dy_abs_start(state, keyword, offset)
    if start >= n_total:
        return synth_render.render_douyin_search_empty(keyword, offset, stream=False)
    req, resp = synth_render.render_douyin_search_item(
        state.dataset(), 1, count, search_id, keyword, start=start)
    resp["cursor"] = offset + count
    return resp


def dy_aweme_detail(state: SiteState, params: dict) -> dict:
    """/aweme/v1/web/aweme/detail（红队 P1-D1）：存在 → 3 键信封；不存在 → 200 + aweme_detail:null。"""
    aweme_id = (params.get("aweme_id") or [""])[0]
    line_no = state.line_no_of(aweme_id) if aweme_id else None
    if line_no is None:
        return synth_render.render_douyin_detail_missing()
    return synth_render.render_douyin_detail(state.dataset(), line_no)


def dy_comment_list(state: SiteState, params: dict) -> dict:
    """/aweme/v1/web/comment/list（红队 P1-D1）：数值游标切片；不存在 → 200 + comments:null。

    红队 R4-P2-1：渲染按 (aweme_id, 序号) 确定性派生 + 本层 8s TTL 响应缓存
    （覆盖 extra.now/impr_id 时变字段）→ 同 (aweme_id, cursor, count) 4s 内重复
    请求逐字节一致。count<=0 → 0 条（R4-P3-1，不再钳到 1）；越界 cursor 由
    渲染层收敛到 total（R4-P3-3）。
    """
    aweme_id = (params.get("aweme_id") or [""])[0]
    cursor = _int_param(params, "cursor", 0, lo=0)
    count = _int_param(params, "count", 20, lo=0, hi=50)
    line_no = state.line_no_of(aweme_id) if aweme_id else None
    if line_no is None:
        return synth_render.render_douyin_comment_list_missing()

    def _render() -> dict:
        resp = synth_render.render_douyin_comment_list(state.dataset(), line_no, cursor, count)
        _cid_remember("douyin", resp.get("comments"), line_no)
        return resp

    return _cached_response(("douyin", "comment_list", aweme_id, cursor, count), _render)


def dy_comment_list_reply(state: SiteState, params: dict) -> dict:
    """/aweme/v1/web/comment/list/reply（红队 P1-D1「含 reply」）：8 键信封。

    红队 R3-P1-1：item_id 可缺——comment/list 侧登记过 cid → 行号即可定位
    （真站契约 item_id 必带，但「拿到 cid 就能翻楼中楼」的探针/agent 路径不断链）。
    R4-P2-1：同 comment/list 的确定性派生 + TTL 缓存（楼中楼对象身份跨请求稳定）。
    """
    aweme_id = (params.get("aweme_id") or params.get("item_id") or [""])[0]
    comment_id = (params.get("comment_id") or ["0"])[0]
    cursor = _int_param(params, "cursor", 0, lo=0)
    count = _int_param(params, "count", 20, lo=0, hi=50)
    line_no = state.line_no_of(aweme_id) if aweme_id else None
    if line_no is None:
        line_no = _cid_lookup("douyin", comment_id)
    if line_no is None:
        return synth_render.render_douyin_comment_list_missing()

    def _render() -> dict:
        resp = synth_render.render_douyin_comment_list_reply(
            state.dataset(), line_no, comment_id, cursor, count)
        _cid_remember("douyin", resp.get("comments"), line_no)
        return resp

    return _cached_response(("douyin", "comment_reply", aweme_id, comment_id, cursor, count),
                            _render)


def dy_related(state: SiteState, params: dict) -> dict:
    """/aweme/v1/web/aweme/related（红队 R3-P2-2：详情页第二数据面，6 键信封 n=10）。"""
    aweme_id = (params.get("aweme_id") or [""])[0]
    line_no = state.line_no_of(aweme_id) if aweme_id else None
    if line_no is None:
        return {"status_code": 8, "status_msg": "Not Found",
                "aweme_list": None, "chime_video_list": None, "filter_infos": None,
                "has_more": 0, "log_pb": {"impr_id": ""}}
    return synth_render.render_douyin_related(state.dataset(), line_no)


def dy_user_profile(state: SiteState, params: dict) -> dict:
    """/aweme/v1/web/user/profile/other（红队 R3-P2-3 作者链）。

    语料实证（dy_user_profile_tabs 14 请求）：定位参数名 sec_user_id、响应顶层
    {extra, log_pb, status_code, status_msg, user}（user 135 键，无 user_list）。
    Media-Monitor collect 引擎的 UserProfile() 固定发 sec_uid 参数并绑定 $.user_list
    （adapt_synth/douyin-user v1）——本端点做调用方兼容：sec_uid 作定位别名；
    带 sec_uid 调用形（MM 签名）额外附 user_list 平铺数组（字段覆盖 MM bindUser
    默认路径），语料原调用形（sec_user_id）响应保持语料键集不变。
    """
    sec_uid = (params.get("sec_user_id") or [""])[0]
    mm_call = False
    if not sec_uid:  # MM 兼容：sec_uid 参数名（engine.UserProfile 固定发该名）
        sec_uid = (params.get("sec_uid") or [""])[0]
        mm_call = bool(sec_uid)
    line_no = state.sec_uid_line_no(sec_uid) if sec_uid else None
    if line_no is None:
        return {"status_code": 8, "status_msg": "Not Found", "user": None,
                "extra": {"fatal_item_ids": [], "logid": "", "now": _now_ms()},
                "log_pb": {"impr_id": ""}}
    resp = synth_render.render_douyin_user_profile(state.dataset().read(line_no))
    if mm_call:
        u = resp.get("user") or {}
        resp["user_list"] = [{
            "uid": u.get("uid"), "sec_uid": u.get("sec_uid"),
            "short_id": str(u.get("uid") or "")[:11],
            "nickname": u.get("nickname"), "signature": u.get("signature") or "",
            "avatar_url": ((u.get("avatar_thumb") or {}).get("url_list") or [""])[0],
            "ip_label": "", "gender": 0,
            "follower_count": u.get("follower_count", 0),
            "following_count": u.get("following_count", 0),
            "aweme_count": u.get("aweme_count", 0),
            "total_favorited": u.get("total_favorited", 0),
        }]
    return resp


def dy_user_posted(state: SiteState, params: dict) -> dict:
    """/aweme/v1/web/aweme/post（作者作品列表：min/max_cursor 游标）。"""
    sec_uid = (params.get("sec_user_id") or [""])[0]
    max_cursor = _int_param(params, "max_cursor", 0, lo=0)
    count = _int_param(params, "count", 18, lo=1, hi=50)
    line_nos = state.author_line_nos_by_sec_uid(sec_uid) if sec_uid else []
    if not line_nos:
        return {"status_code": 8, "status_msg": "Not Found", "aweme_list": [],
                "min_cursor": 0, "max_cursor": 0, "has_more": 0,
                "log_pb": {"impr_id": ""}}
    return synth_render.render_douyin_user_posted(
        state.dataset(), line_nos, max_cursor, count)


def _now_ms() -> int:
    import time
    return int(time.time() * 1000)


def xhs_search_notes(state: SiteState, body: dict) -> tuple[dict, dict]:
    """POST body：12 必填项；缺 filters 兜底注入（QA verification_fix_round2 P3-R2-1）。

    红队 P1-X1：空/缺 keyword、page_size<=0、page 超出数据末页 → 真站空信封
    （data 无 items 键 + has_more:false），不再把空查询当默认窗口吐全量、不回绕末页。
    R5C-P2-1：page/page_size/keyword 类型非法（"abc"/数组等）按缺省/空处理
    （_body_int/_body_str 容错），不再上抛 500。
    """
    body = dict(body or {})
    if "filters" not in body or body.get("filters") in (None, [], {}):
        body["filters"] = [
            {"tags": ["time_descending"], "type": "sort_type"},
            {"tags": ["不限"], "type": "filter_note_type"},
        ]
    keyword = _body_str(body, "keyword")
    if not keyword.strip():
        return None, synth_render.xhs_empty_search_response()
    page = _body_int(body, "page", 1, lo=1)
    size = _body_int(body, "page_size", DEFAULT_PAGE_SIZE)
    if size <= 0:  # 真站按空结果信封处理（红队 P2 修复 10）
        return None, synth_render.xhs_empty_search_response()
    # R4-P3-2：page_size 钳制到语料实证上限 20（37/37 请求全为 20，真站硬上限），
    # 且如实返回钳制后条数（hot_query 卡计入配额，不超发）
    size = min(size, XHS_PAGE_SIZE_MAX)
    # 红队 R2-P2-1：绝对定位 = 轮转基×20 + (page-1)×size——page_size 混用时窗口不漂移
    start = rotation_pages("xhs", keyword) * DEFAULT_PAGE_SIZE + (page - 1) * size
    if start >= len(state.dataset()):  # 超出数据集末页 → 空信封（替代 _clip_page 回绕，P2-X1）
        return None, synth_render.xhs_empty_search_response()
    return synth_render.render_xhs_search_notes(
        state.dataset(), page, size, _body_str(body, "search_id") or None, keyword, start=start)


def xhs_comment_page(state: SiteState, params: dict) -> dict:
    note_id = (params.get("note_id") or [""])[0]
    cursor = (params.get("cursor") or [""])[0]
    if not note_id:
        # 红队 P2-X3 族：错误信封用真站 code/success/msg 族，不自造 -1/-2 码
        return {"code": -100, "msg": "参数缺失：note_id", "success": False}
    line_no = state.line_no_of(note_id)
    if line_no is None:
        return {"code": -100, "msg": "笔记不存在", "success": False}
    req, resp = synth_render.render_xhs_comment_page(state.dataset(), line_no, cursor)
    # 二期集成（C 线）修复：xhs 评论信封条目主键是 id（render.py 语料形态），
    # 而 _cid_remember 提取 cid——此前 xhs cid 索引恒空，sub/page 只带
    # root_comment_id 时 _cid_lookup 必 miss →「笔记不存在」（一期 t10 死在
    # 参数名，从未走到这条路径，故未暴露）。按 ks 同款映射归一。
    _cid_remember("xhs",
                  [{"cid": c.get("id")} for c in (resp.get("data", {}).get("comments") or [])],
                  line_no)
    return resp


def xhs_comment_sub_page(state: SiteState, params: dict) -> dict:
    """GET /api/sns/web/v2/comment/sub/page（红队 R3-P2-1：子评论链接线）。

    游标链与 comment/page 同语义（cursor=上页末条 id、取尽 has_more=false）；
    可得总数 = 顶层 sub_comment_count 声称值（claim=可得，语料 '9' 翻尽 9）。

    参数名语料裁决（2026-09 复核）：xhs_note_detail_comments 语料 64/64 个
    sub/page 请求全部用 root_comment_id（零个裸 comment_id）——真站参数即
    root_comment_id，合成站主参数与语料一致。Media-Monitor adapt_synth
    xhs-comments-replies v1 契约发 comment_id 属契约错误（B 线修 MM）；
    本端点保留 comment_id 作过渡兼容别名（语料调用形不受影响）。
    """
    note_id = (params.get("note_id") or [""])[0]
    root_comment_id = (params.get("root_comment_id") or [""])[0]
    if not root_comment_id:  # MM 兼容别名（其契约 placeholder 误写 comment_id）
        root_comment_id = (params.get("comment_id") or [""])[0]
    cursor = (params.get("cursor") or [""])[0]
    num = _int_param(params, "num", 10, lo=1, hi=30)
    if not root_comment_id:
        return {"code": -100, "msg": "参数缺失：root_comment_id", "success": False}
    line_no = state.line_no_of(note_id) if note_id else None
    if line_no is None:
        line_no = _cid_lookup("xhs", root_comment_id)  # 只带 root_comment_id 也能翻子评论
    if line_no is None:
        return {"code": -100, "msg": "笔记不存在", "success": False}
    req, resp = synth_render.render_xhs_comment_sub_page(
        state.dataset(), line_no, root_comment_id, cursor, num)
    _cid_remember("xhs", resp.get("data", {}).get("comments"), line_no)
    return resp


def xhs_user_posted(state: SiteState, params: dict) -> dict:
    """GET /api/sns/web/v1/user_posted（红队 R3-P2-3 作者链）。"""
    user_id = (params.get("user_id") or [""])[0]
    cursor = (params.get("cursor") or [""])[0]
    num = _int_param(params, "num", 30, lo=1, hi=50)
    line_nos = state.author_line_nos(user_id) if user_id else []
    if not line_nos:
        return {"code": -100, "msg": "用户不存在", "success": False}
    req, resp = synth_render.render_xhs_user_posted(
        state.dataset(), line_nos, user_id, cursor, num)
    return resp


def ks_search_feed(state: SiteState, body: dict) -> dict:
    body = dict(body or {})
    pcursor = _body_str(body, "pcursor")
    keyword = _body_str(body, "keyword")
    rot = rotation_pages("kuaishou", keyword)
    session_id = _body_str(body, "searchSessionId") or synth_render.ks_session_id_for(keyword)
    # 红队 R5C-P3-5：终止游标 no_more 回传 → 保持 no_more + 空页（不回绕第 1 页，
    # 重试/回放型 agent 重发末游标不会陷入翻页死循环）
    if pcursor == "no_more":
        return {"result": 1, "pcursor": "no_more", "searchSessionId": session_id,
                "llsid": "", "host-name": "", "webPageArea": "searchxxnull", "feeds": []}
    # 红队 R3-P3-2：关键词结果窗口有界（8-15 页确定性），翻尽 pcursor="no_more"
    max_rel = 8 + stable_hash("ks-window::%s" % ((keyword or "").strip() or "<default>")) % 8
    rel = int(pcursor) + 1 if pcursor.isdigit() and pcursor != "" else 1
    if rel > max_rel:
        return {"result": 1, "pcursor": "no_more", "searchSessionId": session_id,
                "llsid": "", "host-name": "", "webPageArea": "searchxxnull", "feeds": []}
    page = _clip_page(state, rot + rel)
    size = _body_int(body, "page_size", DEFAULT_PAGE_SIZE, lo=1, hi=50)  # R5C-P2-1 容错
    req, resp = synth_render.render_ks_search_feed(
        state.dataset(), page, size, pcursor, session_id, keyword)
    # 契约形态：pcursor "1"→"2" 递增回传（窗口内相对页号）；末页回 no_more（R3-P3-2）
    resp["pcursor"] = "no_more" if rel >= max_rel else str(rel)
    resp["searchSessionId"] = session_id
    return resp


# ---------------------------------------------------------------------------
# 快手补面（红队 R3-P1-2）：GraphQL 主面 + REST 评论/作者/用户搜索
# ---------------------------------------------------------------------------
def ks_graphql(state: SiteState, body: dict) -> dict:
    """POST /graphql：operationName 分发（语料 12 operation；data.* 包装；miss 回 JSON）。"""
    body = dict(body or {})
    op = str(body.get("operationName") or "")
    variables = body.get("variables") or {}
    if not isinstance(variables, dict):
        variables = {}
    variables = dict(variables)
    if "count" in variables:  # R5C-P2-1：count 类型非法容错（render 侧裸 int）
        variables["count"] = _body_int(variables, "count", 20, lo=0)
    if "photoId" in variables:
        variables["photoId"] = _body_str(variables, "photoId") or None
    if "rootCommentId" in variables:
        variables["rootCommentId"] = _body_str(variables, "rootCommentId") or None
    line_no = None
    photo_id = variables.get("photoId")
    if photo_id:
        line_no = state.line_no_of(str(photo_id))
    if line_no is None:
        rc = variables.get("rootCommentId")
        if rc:
            line_no = _cid_lookup("kuaishou", str(rc))  # 只带 rootCommentId 也能翻子评论
    resp = synth_render.render_ks_graphql(state.dataset(), op, variables, line_no)
    if op == "commentListQuery" and line_no is not None:
        roots = ((resp.get("data") or {}).get("visionCommentList") or {}).get("rootCommentsV2") or []
        _cid_remember("kuaishou", [{"cid": r.get("commentId")} for r in roots], line_no)
    return resp


def ks_photo_comment_list(state: SiteState, body: dict) -> dict:
    """POST /rest/v/photo/comment/list（rootCommentsV2/commentCountV2/pcursorV2 信封）。"""
    body = dict(body or {})
    photo_id = _body_str(body, "photoId") or _body_str(body, "photo_id")
    pcursor = _body_str(body, "pcursor")
    count = _body_int(body, "count", 20, lo=0)  # R5C-P2-1：count:"abc" 容错
    line_no = state.line_no_of(photo_id) if photo_id else None
    if line_no is None:
        return {"result": 1, "host-name": "", "pcursorV2": "no_more",
                "commentCountV2": 0, "rootCommentsV2": []}
    resp = synth_render.render_ks_comment_list(state.dataset(), line_no, pcursor, count)
    _cid_remember("kuaishou",
                  [{"cid": r.get("comment_id")} for r in resp.get("rootCommentsV2") or []],
                  line_no)
    return resp


def ks_profile_get(state: SiteState, params: dict) -> dict:
    """GET /rest/v/profile/get（作者头部：eid/userName/fans/follows…）。"""
    user_id = (params.get("userId") or [""])[0]
    line_nos = state.author_line_nos(user_id) if user_id else []
    if not line_nos:
        return {"result": 1, "eid": user_id, "like": 0, "userTex": "", "sex": "M",
                "mobile": "", "follows": 0, "host-name": "", "userName": "快手用户",
                "userId": 0, "userDefineId": "0", "fans": 0,
                "userHead": "http://p5.a.yximgs.com/s1/i/def/head_m.png"}
    return synth_render.render_ks_profile_get(state.dataset(), line_nos, user_id)


def ks_profile_feed(state: SiteState, body: dict) -> dict:
    """POST /rest/v/profile/feed（作者作品页：type/tags/authorStatement/author/photo）。"""
    body = dict(body or {})
    user_id = _body_str(body, "user_id") or _body_str(body, "userId")
    pcursor = _body_str(body, "pcursor")
    line_nos = state.author_line_nos(user_id) if user_id else []
    if not line_nos:
        return {"result": 1, "pcursor": "no_more", "feeds": []}
    return synth_render.render_ks_profile_feed(state.dataset(), line_nos, user_id, pcursor)


def ks_user_info(state: SiteState, params: dict) -> dict:
    """GET /api/user/info（Media-Monitor 用户增强契约 kuaishou-user v1：sec_uid 定位、
    $.user_list 绑定；engine.UserProfile 固定发 sec_uid 参数）。语料无该路径真值
    （MM 契约自述 reconstructed），数据从 synthgen 作者池派生、跨请求确定性。
    ks 数据集作者主键为 author.id（3x…，搜索/作者面回传的都是它）——sec_uid 参数
    值即按该主键解析，同时兼容 userId 参数名；未知 id（如评论面合成的 author_id）
    按 f(id) 确定性合成档案（与 /rest/v/profile/get 对未知 id 的兜底口径一致），
    保证 $.user_list 恒可绑定。"""
    sec_uid = (params.get("sec_uid") or params.get("userId") or [""])[0]
    if not sec_uid:
        return {"result": 1, "host-name": "", "user_list": []}
    line_nos = state.author_line_nos(sec_uid) if sec_uid else []
    return synth_render.render_ks_user_info(state.dataset(), line_nos, sec_uid)


def ks_search_user(state: SiteState, body: dict) -> dict:
    """POST /rest/v/search/user（users n=30 信封）。"""
    body = dict(body or {})
    keyword = _body_str(body, "keyword")
    pcursor = _body_str(body, "pcursor")
    session_id = _body_str(body, "searchSessionId") or synth_render.ks_session_id_for(keyword)
    return synth_render.render_ks_search_user(state.dataset(), keyword, pcursor, session_id)


def ks_kconf_get(state: SiteState, body: dict) -> dict:
    """POST /rest/v/kconf/get（页面前置配置：loginConfig/pcMenuConfig 静态键集）。"""
    body = dict(body or {})
    return synth_render.render_ks_kconf_get(state.dataset(), _body_str(body, "kconfKey"))


# ---------------------------------------------------------------------------
# 本地辅助端点
# ---------------------------------------------------------------------------
def synth_entity(state: SiteState, params: dict):
    rid = (params.get("id") or [""])[0]
    if not rid:
        return 400, {"error": "missing id"}
    line_no = state.line_no_of(rid)
    if line_no is None:
        return 404, {"error": "record not found: %s" % rid}
    rec = state.dataset().read(line_no)
    return 200, {"site": state.site, "line_no": line_no, "record_id": rid, "entity": rec}


_SVG_PALETTES = [
    ("#ff6034", "#ee0a24"), ("#364fc7", "#3b5bdb"), ("#0ca678", "#099268"),
    ("#f08c00", "#e8590c"), ("#9c36b5", "#7048e8"), ("#0b7285", "#1098ad"),
]


def synth_cover(params: dict) -> bytes:
    seed = (params.get("seed") or ["synth"])[0]
    w = max(40, min(int((params.get("w") or ["323"])[0] or 323), 1920))
    h = max(40, min(int((params.get("h") or ["430"])[0] or 430), 1920))
    label = (params.get("label") or [""])[0][:2]
    c1, c2 = _SVG_PALETTES[stable_hash(seed) % len(_SVG_PALETTES)]
    return (
        '<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d">'
        '<defs><linearGradient id="g" x1="0" y1="0" x2="1" y2="1">'
        '<stop offset="0" stop-color="%s"/><stop offset="1" stop-color="%s"/>'
        "</linearGradient></defs>"
        '<rect width="100%%" height="100%%" fill="url(#g)"/>'
        '<text x="50%%" y="52%%" fill="rgba(255,255,255,.85)" font-family="sans-serif" '
        'font-size="%d" text-anchor="middle" dominant-baseline="middle">%s</text>'
        "</svg>" % (w, h, c1, c2, max(14, min(w, h) // 8), label)
    ).encode("utf-8")


# ---------------------------------------------------------------------------
# /favicon.ico 与 /robots.txt（红队 R4-P3-6/P3-7：真浏览器每会话必请求 favicon，
# 语料 dy 会话对 www.douyin.com/favicon.ico 发起 55 次请求——此前三站 404 HTML）。
# favicon：16×16 32bpp 自绘 ICO（圆底 + 播放三角，站点品牌色，确定性生成）；
# robots：语料 0 次请求、无语料真值（红队按平台惯例推定真站应有）——按
# robots.txt 通行最小形态（User-agent: * / Disallow: 空 = 允许全部）提供。
# ---------------------------------------------------------------------------
def _build_ico(color: tuple) -> bytes:
    import struct
    w = h = 16
    cx = cy = 7.5
    r = 7.75
    rows = []
    for y in range(h):
        row = bytearray()
        for x in range(w):
            inside = (x - cx) ** 2 + (y - cy) ** 2 <= r * r
            tri = False
            if inside:  # 播放三角（白）
                tx = (x - 5.0) / 6.0
                if 0.0 <= tx <= 1.0:
                    tri = abs(y - cy) <= 4.6 * (1.0 - abs(2.0 * tx - 1.0))
            if tri:
                row += bytes((255, 255, 255, 255))  # BGRA
            elif inside:
                row += bytes((color[2], color[1], color[0], 255))
            else:
                row += bytes((0, 0, 0, 0))
        rows.append(bytes(row))
    xor = b"".join(reversed(rows))  # BMP 自底向上
    and_mask = b"\x00" * (h * 4)
    size = 40 + len(xor) + len(and_mask)
    hdr = struct.pack("<HHH", 0, 1, 1)
    entry = struct.pack("<BBBBHHII", w, h, 0, 0, 1, 32, size, 22)
    bmp = struct.pack("<IiiHHIIiiII", 40, w, h * 2, 1, 32, 0,
                      len(xor) + len(and_mask), 0, 0, 0, 0)
    return hdr + entry + bmp + xor + and_mask


_FAVICON_ICO = {  # 站点品牌色自绘小图标（启动前一次性生成，确定性字节）
    "douyin": _build_ico((0xFE, 0x2C, 0x55)),
    "xhs": _build_ico((0xFF, 0x24, 0x42)),
    "kuaishou": _build_ico((0xFF, 0x60, 0x00)),
}

_ROBOTS_TXT = "User-agent: *\nDisallow:\n"


# ---------------------------------------------------------------------------
# 安全：路径校验（P2-1 修复，verification_dualmode.md 复核 6）
#
# 漏洞形态：`GET /C:/Windows/win.ini`——path.lstrip("/") 后成 `C:/...`，pathlib
# join 时被盘符绝对路径整体替换，绕过旧的 `".." not in path` 守卫 → 本机任意文件
# 可读。防御策略（纵深两层）：
#   1) 路由层：原始 path 及其 ≤2 次 percent-decode 结果逐一检查，命中任一特征
#      即 404（拒绝特征：反斜杠、任一 path 段为 `..`、任一段以盘符 `字母:` 开头、
#      `//?/`/`\\?\` 前缀）。双重编码（%252f → %2f → /）在第二轮 decode 后现形。
#   2) 静态分支：即使通过第 1 层，文件 realpath 必须落在 pages/out/<site>/ 内。
# ---------------------------------------------------------------------------
_DRIVE_SEG_RE = re.compile(r"^[A-Za-z]:")  # 段首盘符（C:/ C:\ …）


def _path_reject_reason_once(p: str) -> str | None:
    if "\\" in p:
        return "backslash"
    stripped = p.lstrip("/")
    if stripped.startswith("?/"):  # //?/C:/…、\\?\…（verbatim 前缀的斜杠写法）
        return "verbatim-prefix"
    for seg in stripped.split("/"):
        if seg == "..":
            return "dotdot-segment"
        if _DRIVE_SEG_RE.match(seg):
            return "drive-letter"
    return None


def path_reject_reason(raw_path: str) -> str | None:
    """原始 path + 最多 2 次 percent-decode 后仍出现穿越特征 → 返回原因（None=放行）。"""
    p = raw_path
    for _ in range(3):  # 原始 + 2 轮 decode，防双重编码
        reason = _path_reject_reason_once(p)
        if reason:
            return reason
        try:
            decoded = unquote(p)
        except Exception:
            return "undecodable"
        if decoded == p:
            break
        p = decoded
    return None


# ---------------------------------------------------------------------------
# HTTP handler
# ---------------------------------------------------------------------------
API_ROUTES = {
    "douyin": {
        ("GET", "/aweme/v1/web/general/search/stream/"): dy_search_stream,
        ("GET", "/aweme/v1/web/general/search/stream"): dy_search_stream,
        ("GET", "/aweme/v1/web/general/search/single/"): dy_search_single,
        ("GET", "/aweme/v1/web/general/search/single"): dy_search_single,
        # 红队 P1-D1：补齐 detail/comment(list+reply)/search(item) 契约端点
        ("GET", "/aweme/v1/web/aweme/detail/"): dy_aweme_detail,
        ("GET", "/aweme/v1/web/aweme/detail"): dy_aweme_detail,
        ("GET", "/aweme/v1/web/comment/list/"): dy_comment_list,
        ("GET", "/aweme/v1/web/comment/list"): dy_comment_list,
        ("GET", "/aweme/v1/web/comment/list/reply/"): dy_comment_list_reply,
        ("GET", "/aweme/v1/web/comment/list/reply"): dy_comment_list_reply,
        ("GET", "/aweme/v1/web/search/item/"): dy_search_item,
        ("GET", "/aweme/v1/web/search/item"): dy_search_item,
        # 红队 R3-P2-2/R3-P2-3：related 相关推荐 + 作者主页/作品最小面
        ("GET", "/aweme/v1/web/aweme/related/"): dy_related,
        ("GET", "/aweme/v1/web/aweme/related"): dy_related,
        ("GET", "/aweme/v1/web/user/profile/other/"): dy_user_profile,
        ("GET", "/aweme/v1/web/user/profile/other"): dy_user_profile,
        ("GET", "/aweme/v1/web/aweme/post/"): dy_user_posted,
        ("GET", "/aweme/v1/web/aweme/post"): dy_user_posted,    },
    "xhs": {
        ("POST", "/api/sns/web/v2/search/notes"): xhs_search_notes,
        # 契约 path_template=/api/sns/web/v2/comment/page（render 端点 ID 写作 comment_page，
        # 两种形式都路由，页面/调用方不踩坑）
        ("GET", "/api/sns/web/v2/comment/page"): xhs_comment_page,
        ("GET", "/api/sns/web/v2/comment/page/"): xhs_comment_page,
        ("GET", "/api/sns/web/v2/comment_page"): xhs_comment_page,
        # 红队 R3-P2-1：子评论链 + R3-P2-3 作者链
        ("GET", "/api/sns/web/v2/comment/sub/page"): xhs_comment_sub_page,
        ("GET", "/api/sns/web/v2/comment/sub/page/"): xhs_comment_sub_page,
        ("GET", "/api/sns/web/v1/user_posted"): xhs_user_posted,
    },
    "kuaishou": {
        ("POST", "/rest/v/search/feed"): ks_search_feed,
        # 红队 R3-P1-2 快手补面：GraphQL 主面 + REST 评论/作者/用户搜索/kconf
        ("POST", "/graphql"): ks_graphql,
        ("POST", "/graphql/"): ks_graphql,
        ("POST", "/rest/v/photo/comment/list"): ks_photo_comment_list,
        ("GET", "/rest/v/profile/get"): ks_profile_get,
        ("POST", "/rest/v/profile/feed"): ks_profile_feed,
        ("POST", "/rest/v/search/user"): ks_search_user,
        ("POST", "/rest/v/kconf/get"): ks_kconf_get,
        # 用户增强（Media-Monitor kuaishou-user v1：sec_uid 定位、$.user_list 绑定）
        ("GET", "/api/user/info"): ks_user_info,
        ("GET", "/api/user/info/"): ks_user_info,
    },
}

PAGE_FILES = {  # 路由 → pages/out/<site>/ 文件
    "/": "home.html",
    "/index.html": "home.html",
    "/search": "search.html",
    "/search/": "search.html",
    "/detail": "detail.html",
    "/detail/": "detail.html",
    "/profile": "profile.html",  # 作者主页骨架（红队 R3-P2-3；数据 SSR 注入）
    "/profile/": "profile.html",
    "/404": "404.html",  # xhs 真站错误页形态（红队 P2-X5：302 落点）
    "/404/": "404.html",
}

# 真站 URL 路径别名（红队 P2-D6/P3-4 + R3-P1-2：/search/<kw>、/video/<id>（dy）、
# /search_result?keyword=、/explore/<id>（xhs）、/search/video?searchKey=、
# /short-video/<id>、/profile/<id>（ks））
# 红队 R5B-P3-7/R5B-P2-3/R5B-P2-4 增补：dy /jingxuan/search/<kw>（录制实录 URL 形态）、
# ks /search/user（用户 tab 页）、xhs /search_result/<id>（模态承载路径）。
_DY_SEARCH_PATH_RE = re.compile(r"^/search/(?P<kw>.+)$")
_DY_JINGXUAN_RE = re.compile(r"^/jingxuan/search/(?P<kw>.+)$")
_DY_VIDEO_PATH_RE = re.compile(r"^/video/(?P<id>[0-9A-Za-z_-]+)$")
_DY_USER_PATH_RE = re.compile(r"^/user/(?P<sec>[0-9A-Za-z_._-]+)$")
_XHS_SEARCH_RESULT_RE = re.compile(r"^/search_result/?$")
_XHS_SEARCH_RESULT_NOTE_RE = re.compile(r"^/search_result/(?P<id>[0-9a-fA-F]+)$")
_XHS_EXPLORE_PATH_RE = re.compile(r"^/explore/(?P<id>[0-9a-fA-F]+)$")
_XHS_USER_PATH_RE = re.compile(r"^/user/profile/(?P<uid>[0-9a-fA-F]+)$")
_KS_SEARCH_VIDEO_RE = re.compile(r"^/search/video/?$")
_KS_SEARCH_USER_RE = re.compile(r"^/search/user/?$")
_KS_SHORTVIDEO_PATH_RE = re.compile(r"^/short-video/(?P<id>[0-9A-Za-z_-]+)$")
_KS_PROFILE_PATH_RE = re.compile(r"^/profile/(?P<id>[0-9A-Za-z_-]+)$")

# 详情/作者页 SSR 注入点（真站详情数据 SSR 内嵌，红队 R3-P1-3：页面不再走内部取数端点）
_SSR_TOKEN = "/*__STATE__*/null"


# ---------------------------------------------------------------------------
# meta / OG / JSON-LD 注入块（红队 R5B-P3-1：对照语料三站形态）
#
# 语料真值：xhs 笔记详情 og:type=article/og:site_name/og:title/og:url/og:image×N +
#   JSON-LD Article（headline/description/image[]/author Person/publisher/
#   mainEntityOfPage/interactionStatistic）；xhs 作者页 og:url+og:description（粉丝数
#   文案）+keywords；dy 详情/作者页 JSON-LD BreadcrumbList + keywords（话题串+
#   `,抖音,抖音短视频,抖音官网`）+description（文案）+theme-color #161823；
#   ks 搜索/作者页 og:title/og:description/og:url + canonical + JSON-LD BreadcrumbList。
# ---------------------------------------------------------------------------
_SITE_ORIGIN = {"douyin": "https://www.douyin.com",
                "xhs": "https://www.xiaohongshu.com",
                "kuaishou": "https://www.kuaishou.com"}
_BRAND_NAME = {"douyin": "抖音", "xhs": "小红书", "kuaishou": "快手"}


def _attr_esc(v) -> str:
    return (str(v if v is not None else "")
            .replace("&", "&amp;").replace('"', "&quot;")
            .replace("<", "&lt;").replace(">", "&gt;"))


def _text_esc(v) -> str:
    return (str(v if v is not None else "")
            .replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;"))


def _replace_title(html: str, title: str) -> str:
    """覆写 <title>（R5B-P3-2：真站详情/作者页 title 随内容而非恒品牌串）。"""
    return re.sub(r"<title>[^<]*</title>", "<title>%s</title>" % _text_esc(title),
                  html, count=1)


def _meta_prop(prop: str, content) -> str:
    return '<meta property="%s" content="%s">' % (prop, _attr_esc(content))


def _meta_name(name: str, content) -> str:
    return '<meta name="%s" content="%s">' % (name, _attr_esc(content))


def _link_rel(rel: str, href) -> str:
    return '<link rel="%s" href="%s">' % (rel, _attr_esc(href))


def _jsonld(obj) -> str:
    return ('<script type="application/ld+json">%s</script>'
            % json.dumps(obj, ensure_ascii=False))


def _breadcrumb(items: list) -> str:
    """dy/ks BreadcrumbList（语料形态：ListItem position/name/item）。"""
    return _jsonld({
        "@context": "https://schema.org", "@type": "BreadcrumbList",
        "itemListElement": [
            {"@type": "ListItem", "position": i + 1, "name": n, "item": u}
            for i, (n, u) in enumerate(items)],
    })


def _dy_topic_str(desc: str) -> str:
    """dy keywords 话题串：desc 内 #话题 词元（无则空），后接品牌固定尾串。"""
    topics = re.findall(r"#([^\s#，。！？.,!?]{1,24})", desc or "")
    seen, out = set(), []
    for t in topics:
        if t not in seen:
            seen.add(t)
            out.append(t)
    return ",".join(out + ["抖音", "抖音短视频", "抖音官网"])


def _iso_ms(ms) -> str:
    import datetime
    try:
        return datetime.datetime.fromtimestamp(
            int(ms) / 1000.0, datetime.timezone.utc).isoformat().replace("+00:00", "Z")
    except Exception:
        return ""


def _dy_detail_meta(state: SiteState, rec: dict) -> tuple[str, str]:
    """dy 视频详情：title=文案全文；keywords/description/theme-color/BreadcrumbList。"""
    desc = (rec.get("desc") or "").strip() or "抖音视频"
    a = rec.get("author") or {}
    nickname = a.get("nickname") or "抖音用户"
    sec = a.get("sec_uid") or ""
    vid = rec.get("aweme_id") or ""
    origin = _SITE_ORIGIN["douyin"]
    head = "\n".join([
        _meta_name("keywords", _dy_topic_str(desc)),
        _meta_name("description", desc.split("\n")[0][:120]),
        _meta_name("theme-color", "#161823"),
        _breadcrumb([(nickname, "%s/user/%s" % (origin, sec)) if sec
                     else (_BRAND_NAME["douyin"], origin),
                     ("视频作品", "%s/video/%s" % (origin, vid))]),
    ])
    return desc, head


def _dy_user_meta(state: SiteState, rec: dict, sec_uid: str) -> tuple[str, str]:
    """dy 作者主页：title=<昵称>的抖音 - 抖音；BreadcrumbList（抖音→作者）。"""
    nickname = ((rec.get("author") or {}).get("nickname")) or "抖音用户"
    origin = _SITE_ORIGIN["douyin"]
    head = "\n".join([
        _meta_name("theme-color", "#161823"),
        _breadcrumb([(_BRAND_NAME["douyin"], origin),
                     (nickname, "%s/user/%s" % (origin, sec_uid))]),
    ])
    return "%s的抖音 - 抖音" % nickname, head


def _xhs_image_urls(entity: dict, limit: int = 4) -> list:
    """note_card.image_list → og:image URL 列表（WB_DFT 场景；数据集字段缺失则空）。"""
    out = []
    for img in ((entity.get("note_card") or {}).get("image_list") or [])[:limit]:
        for info in (img.get("info_list") or []):
            if info.get("image_scene") == "WB_DFT" and info.get("url"):
                out.append(info["url"])
                break
    return out


def _xhs_detail_meta(state: SiteState, entity: dict) -> tuple[str, str]:
    """xhs 笔记详情：og 全家 + JSON-LD Article（语料形态）。"""
    nc = entity.get("note_card") or {}
    title = (nc.get("display_title") or "").strip() or "小红书笔记"
    note_id = entity.get("id") or ""
    origin = _SITE_ORIGIN["xhs"]
    url = "%s/explore/%s" % (origin, note_id)
    imgs = _xhs_image_urls(entity)
    ii = nc.get("interact_info") or {}
    try:
        likes = int(str(ii.get("liked_count") or 0))
    except ValueError:
        likes = 0
    published = _iso_ms((state.dataset().anchor_ts or 0) * 1000)
    og = [
        _meta_prop("og:type", "article"),
        _meta_prop("og:site_name", _BRAND_NAME["xhs"]),
        _meta_prop("og:title", "%s - %s" % (title, _BRAND_NAME["xhs"])),
        _meta_prop("og:description", title),
        _meta_prop("og:url", url),
    ] + [_meta_prop("og:image", u) for u in imgs]
    article = {
        "@context": "https://schema.org", "@type": "Article",
        "headline": "%s - %s" % (title, _BRAND_NAME["xhs"]),
        "description": title,
        "author": {"@type": "Person",
                   "name": ((nc.get("user") or {}).get("nickname")) or "小红书用户"},
        "publisher": {"@type": "Organization", "name": _BRAND_NAME["xhs"], "url": origin,
                      "logo": {"@type": "ImageObject", "url": origin + "/favicon.ico"}},
        "mainEntityOfPage": {"@type": "WebPage", "@id": url},
        "interactionStatistic": {"@type": "InteractionCounter",
                                  "interactionType": "https://schema.org/LikeAction",
                                  "userInteractionCount": likes},
    }
    if imgs:
        article["image"] = imgs
    if published:
        article["datePublished"] = published
        article["dateModified"] = published
    head = "\n".join(og + [_jsonld(article)])
    return "%s - %s" % (title, _BRAND_NAME["xhs"]), head


def _xhs_user_meta(state: SiteState, uid: str, author: dict) -> tuple[str, str]:
    """xhs 作者主页：og:url+og:description（粉丝数文案）+keywords（语料形态）。"""
    origin = _SITE_ORIGIN["xhs"]
    name = (author.get("nickname") or author.get("name")) or "小红书用户"
    fans = author.get("fans") or 0
    url = "%s/user/profile/%s" % (origin.replace("://www.", "://"), uid)
    head = "\n".join([
        _meta_prop("og:url", url),
        _meta_prop("og:description", "小红书博主%s，拥有%s位粉丝" % (name, fans)),
        _meta_name("keywords", "%s,小红书,博主" % name),
    ])
    return "%s - %s" % (name, _BRAND_NAME["xhs"]), head


def _ks_search_meta(kw: str) -> tuple[str, str]:
    """ks 搜索页：og:title/description/url + canonical（语料形态，URL 用 /search/<kw>）。"""
    origin = _SITE_ORIGIN["kuaishou"]
    url = "%s/search/%s" % (origin, quote(kw, safe=""))
    head = "\n".join([
        _meta_prop("og:title", "%s - %s" % (kw, _BRAND_NAME["kuaishou"])),
        _meta_prop("og:description",
                   "您在查找%s吗？快手综合搜索帮你找到更多相关视频内容，支持在线观看。"
                   "更有海量高清视频、相关直播、用户，满足您的在线观看需求。" % kw),
        _meta_prop("og:url", url),
        _link_rel("canonical", url),
    ])
    return "%s - %s" % (kw, _BRAND_NAME["kuaishou"]), head


def _ks_user_meta(state: SiteState, uid: str, author: dict) -> tuple[str, str]:
    """ks 作者页：og + canonical + BreadcrumbList（快手→昵称）。"""
    origin = _SITE_ORIGIN["kuaishou"]
    name = (author.get("name") or author.get("nickname")) or "快手用户"
    url = "%s/profile/%s" % (origin, uid)
    head = "\n".join([
        _meta_prop("og:title", "%s - %s" % (name, _BRAND_NAME["kuaishou"])),
        _meta_prop("og:description", "快手作者%s的主页，快来围观吧" % name),
        _meta_prop("og:url", url),
        _link_rel("canonical", url),
        _breadcrumb([(_BRAND_NAME["kuaishou"], origin), (name, url)]),
    ])
    return "%s - %s" % (name, _BRAND_NAME["kuaishou"]), head


def _ks_detail_meta(rec: dict) -> tuple[str, str]:
    """ks 作品详情：title=<caption>-快手（语料无空格形态）。"""
    caption = ((rec.get("photo") or {}).get("caption") or "").strip() or "快手作品"
    return "%s-%s" % (caption, _BRAND_NAME["kuaishou"]), ""


# ---------------------------------------------------------------------------
# 站级响应头 / 404 信封（红队 P2-D4 / P2-X2 / P2-X3：去掉 synth 指纹与路由表泄露，
# 只补真站会回的头，不伪造安全语义）
# ---------------------------------------------------------------------------
SITE_SERVER = {"douyin": "TLB", "xhs": "nginx", "kuaishou": ""}  # ks：真站无 Server 头（R3-P2-4）

API_PREFIXES = {  # API 路径前缀（miss 时按站点错误族回信封，不泄露内部路由表）
    "douyin": ("/aweme/",),
    "xhs": ("/api/", "/edith/", "/so/"),
    "kuaishou": ("/rest/", "/api/", "/graphql"),
}

# 站内封面占位路径（红队 R3-P1-3：页面图片不再走 /_synth/* 内部路径，
# 形态对齐各站 CDN 对象路径；仍为本机同源 SVG，无任何出网请求）
SITE_COVER_PREFIX = {
    "douyin": "/obj/tos-cn-i-0813/",
    "xhs": "/sns-webpic/",
    "kuaishou": "/kos/nlav111422/pc-vision/",
}


def _miss_body(site: str, path: str, method: str) -> dict:
    """未知 API 路由的 404 信封：真站错误族（dy status_code 族 / xhs code 族）。"""
    if site == "douyin":
        return {"status_code": 8, "status_msg": "Not Found"}
    if site == "xhs":
        return {"success": False, "code": -404, "msg": "Not Found"}
    return {"result": 0, "error_msg": "not found"}


def _hex(n: int) -> str:
    import secrets
    return secrets.token_hex((n + 1) // 2)[:n] if n else ""


# ---------------------------------------------------------------------------
# Accept-Encoding 协商（红队 R5C-P3-2）：q 值解析 + 编码器表
# ---------------------------------------------------------------------------
_BR_WARNED = {"done": False}


def _warn_br_unavailable():
    """dy 明示 br 但本机无 brotli 库 → 退 identity，只记录一次（不逐请求刷屏）。"""
    if not _BR_WARNED["done"]:
        _BR_WARNED["done"] = True
        print("[synth_api] WARN: Accept-Encoding 含 br 但 brotli 不可用——dy 响应退 "
              "identity（语料 dy 主域 br 63%，如需对齐请为主 venv 安装 brotli）", flush=True)


def _parse_accept_encoding(ae: str) -> dict:
    """`gzip;q=0, br` → {"br":1.0}。q<=0 剔除；不可解析项 q 当 1.0。"""
    out: dict = {}
    for part in ae.split(","):
        part = part.strip().lower()
        if not part:
            continue
        toks = part.split(";")
        enc = toks[0].strip()
        if not enc:
            continue
        q = 1.0
        for t in toks[1:]:
            t = t.strip()
            if t.startswith("q="):
                try:
                    q = float(t[2:])
                except ValueError:
                    q = 1.0
        if q <= 0:
            out.pop(enc, None)
            continue
        out[enc] = max(q, out.get(enc, 0.0))
    return out


def _gzip_compress(data: bytes) -> bytes:
    import gzip
    return gzip.compress(data)


_COMPRESS: dict = {"gzip": _gzip_compress}
try:  # dy 语料主形态 br（63%）；可用则启用，不可用则 _pick_content_encoding 退 identity
    import brotli as _brotli_mod  # type: ignore

    _COMPRESS["br"] = _brotli_mod.compress
except ImportError:
    pass


def _site_api_headers(site: str, payload=None) -> dict:
    """契约 API 响应的站级响应头（按语料/活体实测头集的子集；不含安全语义头）。"""
    if site == "douyin":
        logid = None
        if isinstance(payload, dict):
            logid = ((payload.get("extra") or {}).get("logid")
                     if isinstance(payload.get("extra"), dict) else None)
            if not logid:
                lb = payload.get("log_pb")
                logid = lb.get("impr_id") if isinstance(lb, dict) else None
        hdr = {"bd-tt-error-code": "0", "tt_stable": "1"}
        if logid:
            hdr["x-tt-logid"] = str(logid)
        return hdr
    if site == "xhs":
        return {"request-id": _hex(16), "x-kong-sign": "0",
                "xhs-request-time": "%.3f" % (0.05 + (stable_hash(_hex(8)) % 400) / 1000.0)}
    return {}


# 真站 Content-Type 实证（红队 P2-D4）：dy stream/detail/comment 无 charset、single 带 charset
_DY_NO_CHARSET_PATHS = (
    "/aweme/v1/web/general/search/stream",
    "/aweme/v1/web/aweme/detail",
    "/aweme/v1/web/comment/list",
    "/aweme/v1/web/search/item",
)


class SynthHandler(BaseHTTPRequestHandler):
    server_version = "synth_api/1.0"  # make_handler 按站点覆盖（TLB / nginx / 空）
    sys_version = ""  # 去掉 "Python/3.x" 指纹（红队 P2-D4）
    protocol_version = "HTTP/1.1"

    # 由 make_handler 注入（每端口一个 site）
    site: str = "douyin"

    def version_string(self):
        # 红队 R4-P3-8：基类 version_string = server_version + ' ' + sys_version，
        # sys_version 置空后会拼出尾随空格（"TLB "）；语料真值无空格——先拼后 strip。
        v = self.server_version
        if self.sys_version:
            v = "%s %s" % (v, self.sys_version)
        return v.rstrip()

    def log_message(self, fmt, *args):  # 单行访问日志
        print("[synth_api] %s %s" % (self.site, fmt % args), flush=True)

    def send_header(self, keyword, value):
        # 红队 R3-P2-4：快手真站无 Server 头——站点 server 置空时整个头不发
        if keyword.lower() == "server" and not str(value or "").strip():
            return
        return super().send_header(keyword, value)

    # ---- 基础设施 ----
    def _allowed_origin(self) -> str | None:
        """ACAO 收紧（P2-1）：仅回显本服务自身端口的 localhost Origin，其余不回 ACAO。

        旧版固定 `Access-Control-Allow-Origin: *`，叠加路径穿越时任意网页可在浏览器
        内跨源读取本机文件。现按请求 Origin 匹配白名单（127.0.0.1/localhost + 实际
        监听端口，含 855x 兼容绑定——同一 handler 实例的 server 即该端口的 server）。
        """
        origin = (self.headers.get("Origin") or "").strip()
        if not origin:
            return None
        try:
            port = int(self.server.server_address[1])
        except Exception:
            return None
        if origin in ("http://127.0.0.1:%d" % port, "http://localhost:%d" % port):
            return origin
        return None

    def _send(self, code: int, body: bytes, ctype: str, extra: dict | None = None,
              api: bool = False):
        """发响应（红队 R5C-P3-2/P3-3：压缩协商按 q 值；主域 API 不附 Vary:Origin/
        Cache-Control:no-store——语料 dy 3625 主域 API 响应 cache-control 0 次、xhs
        edith /api/sns 2074 响应两头均 0 次；dy 压缩响应按语料主形态回
        Vary: Accept-Encoding，xhs/ks 不回）。"""
        ce = None
        if code != 204 and body:
            ce = self._pick_content_encoding()
        if ce:
            body = _COMPRESS[ce](body)
            self.send_response(code)
            self.send_header("Content-Type", ctype)
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Content-Encoding", ce)
            if api and self.site == "douyin":
                self.send_header("Vary", "Accept-Encoding")  # dy 语料主形态（2264/3625）
        else:
            self.send_response(code)
            self.send_header("Content-Type", ctype)
            self.send_header("Content-Length", str(len(body)))
        origin = self._allowed_origin()
        extra_keys = {k.lower() for k in (extra or {})}
        if origin:  # 不匹配/无 Origin → 不回任何 CORS 头
            self.send_header("Access-Control-Allow-Origin", origin)
            if "access-control-allow-methods" not in extra_keys:  # 调用方已给 ACAM 时不重复
                self.send_header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
            self.send_header("Access-Control-Allow-Headers", "Content-Type, X-S, X-T")
        for k, v in (extra or {}).items():
            self.send_header(k, v)
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    def _pick_content_encoding(self) -> str | None:
        """Accept-Encoding 协商（红队 R5C-P3-2）。

        - 按 q 值解析（q=0 剔除；逗号切词，不再子串匹配——`gzip;q=0` 不再回 gzip）；
        - dy：主域语料 br 63% / gzip 0 次 → 明示 br（或 *）时优先 br，brotli 不可用
          则退 identity 并记录一次；不明示 br → identity（不回 gzip）；
        - xhs/ks：明示 gzip（或 *）→ gzip，否则 identity。
        """
        ae = (self.headers.get("Accept-Encoding") or "").strip()
        if not ae:
            return None
        ok = _parse_accept_encoding(ae)
        if not ok:
            return None
        star = "*" in ok
        if self.site == "douyin":
            if "br" in ok or star:
                if "br" in _COMPRESS:
                    return "br"
                _warn_br_unavailable()
                return None
            return None
        if "gzip" in ok or star:
            return "gzip"
        return None

    def _json(self, code: int, obj, api: bool = False):
        """api=True 时补站级头集 + 真站 Content-Type 形态（P2-D4/P2-X2）。"""
        extra = _site_api_headers(self.site, obj) if api else None
        ctype = "application/json; charset=utf-8"
        if api and self.site == "douyin":
            p = self.path.split("?", 1)[0].rstrip("/")
            if any(p.startswith(q.rstrip("/")) for q in _DY_NO_CHARSET_PATHS):
                ctype = "application/json"  # 真站 stream/detail/comment/search_item 无 charset
        elif api and self.site == "kuaishou":
            ctype = "application/json;charset=UTF-8"  # 真站无空格大写形态（R3-P3-5）
        return self._send(code, json.dumps(obj, ensure_ascii=False).encode("utf-8"),
                          ctype, extra, api=api)

    def _read_body(self) -> dict:
        n = int(self.headers.get("Content-Length") or 0)
        if n <= 0:
            return {}
        raw = self.rfile.read(n)
        try:
            return json.loads(raw.decode("utf-8"))
        except Exception:
            return {}

    def do_OPTIONS(self):
        # 红队 R5C-P3-4：xhs 同源 OPTIONS 真站形态 = 200 + ACAM 五方法、无 Allow 头
        # （edith /api/sns 语料 3,074 次同源 OPTIONS，4 样本同形）；dy/ks 主域同源
        # OPTIONS 语料 0 次（无真值）——维持 204 + Allow 形态（R3-P3-5）。
        if self.site == "xhs":
            return self._send(200, b"", "text/plain",
                              {"access-control-allow-methods": "POST, GET, OPTIONS, PUT, DELETE"})
        return self._send(204, b"", "text/plain", {"Allow": "GET, POST, OPTIONS"})

    def do_HEAD(self):
        self.do_GET()

    # ---- 路由 ----
    def _request_target(self) -> str:
        """送路径校验用的原始 request-target。

        先于 CPython parse_request 对前导 `//` 的归一化（gh-81389 把 `//?/C:/x`
        改写成 `/?/C:/x`），用 self.path 之前的原始形态再校验一道。普通路径去掉
        ?query（查询串不送检、不误伤）；以 `//` 开头的 authority/verbatim 形态
        （如 `//?/C:/…`）整体送检——`?` 恰是 verbatim 前缀的一部分，先切 query
        会把前缀切碎，且此类形态本就无合法查询语义。
        """
        try:
            target = self.requestline.split(" ", 2)[1]
        except Exception:
            target = self.path
        if target.startswith("//"):
            return target
        return target.split("?", 1)[0]

    def _api_miss(self, method: str, path: str):
        """未实现/未知 API 路由：真站错误族 404（红队 P2-X3/P2-D4：不泄露路由表）。"""
        return self._json(404, _miss_body(self.site, path, method), api=True)

    _PAGE_MISS_TITLES = {
        # 红队 R3-P1-2/R3-P1-3：三站 404 页 title/文案对齐各自真站形态
        "douyin": ("404 - 抖音", "页面不存在"),
        "xhs": ("小红书 - 你访问的页面不见了", "你访问的页面不见了"),
        "kuaishou": ("404 - 快手", "页面不存在"),
    }

    def _page_miss(self):
        """未知页面路径：贴近真站的 404 页（红队 R3-P1-2：ks 不再顶着抖音的 404 title）。"""
        title, text = self._PAGE_MISS_TITLES.get(self.site, self._PAGE_MISS_TITLES["douyin"])
        body = ("<!DOCTYPE html><html lang=\"zh-CN\"><head><meta charset=\"utf-8\">"
                "<title>%s</title></head><body style=\"font:14px/1.8 sans-serif;"
                "text-align:center;padding-top:12vh\">"
                "<h1 style=\"font-size:20px;color:#333\">%s</h1>"
                "<p style=\"color:#888\"><a href=\"/\">返回首页</a></p></body></html>"
                % (title, text))
        return self._send(404, body.encode("utf-8"), "text/html; charset=utf-8")

    def do_GET(self):
        state = STATES[self.site]
        parsed = urlparse(self.path)
        path = parsed.path
        params = parse_qs(parsed.query)
        bad = path_reject_reason(self._request_target()) or path_reject_reason(path)
        if bad:  # 路径穿越特征（含 ≤2 次 decode 后现形）→ 与 miss 同形态 404，不泄露过滤存在
            return self._api_or_page_miss("GET", path)
        try:
            if path == "/_synth/health":
                return self._json(200, {
                    "site": self.site,
                    "records": len(state.dataset()),
                    "entity": state.dataset().entity})
            if path == "/_synth/entity":
                code, obj = synth_entity(state, params)
                return self._json(code, obj)
            if path == "/_synth/cover":
                return self._send(200, synth_cover(params), "image/svg+xml; charset=utf-8")
            # 最后一公里（红队 R4-P3-6/P3-7）：favicon 200 二进制 + robots 200 文本
            if path == "/favicon.ico":
                return self._send(200, _FAVICON_ICO[self.site], "image/x-icon")
            if path == "/robots.txt":
                return self._send(200, _ROBOTS_TXT.encode("utf-8"), "text/plain")
            # 站内封面占位（真站 CDN 对象路径形态，红队 R3-P1-3；同源本机 SVG）
            cover_prefix = SITE_COVER_PREFIX[self.site]
            if path.startswith(cover_prefix):
                merged = dict(params)
                merged.setdefault("seed", [unquote(path[len(cover_prefix):]) or "cover"])
                return self._send(200, synth_cover(merged), "image/svg+xml; charset=utf-8")
            fn = API_ROUTES[self.site].get(("GET", path))
            if fn:
                return self._json(200, fn(state, params), api=True)
            alias = self._page_alias(path, params)
            if alias is not None:
                return alias
            return self._serve_page(path)
        except Exception:  # 红队 R5C-P2-1：全局错误卫生——服务端日志留 traceback，
            # 客户端只见站点错误族信封（语料 68,405 请求 0 个 500；绝不回
            # Python 异常文本/解释器特征）
            import traceback
            traceback.print_exc()
            return self._family_error("GET", path)

    def _api_or_page_miss(self, method: str, path: str):
        if any(path.startswith(p) for p in API_PREFIXES[self.site]):
            return self._api_miss(method, path)
        return self._page_miss()

    def _family_error(self, method: str, path: str):
        """未捕获异常兜底（红队 R5C-P2-1）：API 路径回 200 + 站点错误族信封
        （dy status_code 族 / xhs code 族 / ks result 族，站级头集齐全）；
        页面路径回站点 404 页。不回 500、不泄任何解释器/内部字段。"""
        if any(path.startswith(p) for p in API_PREFIXES[self.site]) \
                or (method, path) in API_ROUTES[self.site]:
            return self._json(200, _miss_body(self.site, path, method), api=True)
        return self._page_miss()

    # ---- SSR 注入（红队 R3-P1-3：详情/作者数据内嵌页面，真站 SSR 形态） ----
    def _serve_state_page(self, fname: str, state_obj: dict,
                          title: str | None = None, head: str | None = None):
        """读页面骨架，把 state 注入 __SYNTH_STATE__ 占位（真站 SSR 数据内嵌形态）。

        红队 R5B-P3-1/P3-2：title 按实体内容覆写（真站详情/作者页 title 随内容）、
        head 为按语料形态注入的 meta/OG/JSON-LD 块（xhs Article、dy/ks
        BreadcrumbList、canonical/keywords/theme-color 等）。
        """
        f = _CFG["pages"] / self.site / fname
        if not f.is_file():
            return self._page_miss()  # R5C-P2-1：骨架缺失不回内部构建提示
        html = f.read_text(encoding="utf-8")
        payload = json.dumps(state_obj, ensure_ascii=False)
        if _SSR_TOKEN in html:
            html = html.replace(_SSR_TOKEN, payload)
        else:  # 兜底：无占位时注入到 <head> 尾部
            html = html.replace("</head>",
                                "<script>window.__INITIAL_STATE__=%s;</script></head>" % payload)
        if title:
            html = _replace_title(html, title)
        if head:
            html = html.replace("</head>", head + "</head>", 1)
        return self._send(200, html.encode("utf-8"), "text/html; charset=utf-8")

    def _detail_state(self, state: SiteState, item_id: str):
        """详情页 SSR 数据：整条实体（dy 由页面走真路径 aweme/detail，此路供 xhs/ks）。"""
        line_no = state.line_no_of(item_id) if item_id else None
        if line_no is None:
            return None
        return {"entity": state.dataset().read(line_no)}

    def _profile_state(self, state: SiteState, author_key: str, author_rec: dict | None,
                       line_nos: list | None = None):
        """作者主页 SSR 数据：作者卡 + 作品列表（index.author_id 聚合）。

        红队 R5B-P1：dy 调用方按 sec_uid 反查行号后经 line_nos 传入（sec_uid 空间与
        author 实体打通——旧版把 sec_uid 传给按 uid 查询的 author_line_nos，恒 0 命中，
        作者页 works 恒空）。xhs/ks 的路径主键即 author_id，默认行为不变。
        """
        if line_nos is None:
            line_nos = state.author_line_nos(author_key) if author_key else []
        works = []
        for ln in line_nos[:60]:
            try:
                r = state.dataset().read(ln)
            except Exception:
                continue
            if self.site == "douyin":
                works.append({
                    "id": r.get("aweme_id"), "title": (r.get("desc") or "").split("\n")[0],
                    "like": (r.get("statistics") or {}).get("digg_count"),
                    "view": (r.get("statistics") or {}).get("play_count"),
                    "dur": (r.get("video") or {}).get("duration") or 0,
                })
            elif self.site == "xhs":
                nc = r.get("note_card") or {}
                works.append({
                    "id": r.get("id"), "title": nc.get("display_title") or "",
                    "like": (nc.get("interact_info") or {}).get("liked_count"),
                    "view": "", "dur": 0,
                })
            else:
                p = r.get("photo") or {}
                works.append({
                    "id": p.get("id"), "title": p.get("caption") or "",
                    "like": p.get("likeCount"), "view": p.get("viewCount"),
                    "dur": p.get("duration") or 0,
                })
        a = (author_rec or {}).get("author") if self.site != "xhs" else \
            ((author_rec or {}).get("note_card") or {}).get("user")
        author = {"id": author_key, "name": (a or {}).get("nickname") or (a or {}).get("name")
                  or "作者", "avatar": (a or {}).get("headerUrl")
                  or ((a or {}).get("avatar_thumb") or {}).get("url_list", [""])[0]
                  or (a or {}).get("avatar") or ""}
        if self.site == "douyin" and author_rec:
            author["followers"] = (author_rec.get("author") or {}).get("follower_count")
        return {"author": author, "works": works}

    def _page_alias(self, path: str, params: dict):
        """真站 URL 路径别名（红队 P2-D6/P3-4 + R3-P1-2 + R5B）+ 详情/作者页 SSR 注入。

        - dy  /search/<kw>、/jingxuan/search/<kw>（R5B-P3-7 录制实录形态）→ search.html；
              /video/<id> → detail.html（不存在 id 200 + 页面错误态；存在时注入
              meta/JSON-LD，R5B-P3-1）；/user/<sec_uid> → profile.html（R5B-P1：
              sec_uid 反查行号聚合，works 非空 + 作品/喜欢 tab）
        - xhs /search_result?keyword= → search.html；/search_result/<id> → 搜索页 +
              笔记实体 SSR（页面 JS 开模态，R5B-P2-4）；/explore、/explore/<id>；
              不存在 id → 302 /404（真站形态）；/user/profile/<uid> → profile.html
        - ks  /search/video?searchKey=、/search/user?searchKey=（R5B-P2-3 用户 tab 页）
              → search.html / search_user.html；/short-video/<id> → detail.html；
              /profile/<id> → profile.html（真站 URL 约定）
        """
        state = STATES[self.site]
        if self.site == "douyin":
            m = _DY_SEARCH_PATH_RE.match(path) or _DY_JINGXUAN_RE.match(path)
            if m and path not in ("/search/", "/jingxuan/search/"):  # 裸 /search/ 走默认页
                return self._serve_file("search.html")
            m = _DY_VIDEO_PATH_RE.match(path)
            if m:  # 不存在 id：真站 200 + 页面错误组件（build_pages 详情 JS 渲染「视频不存在」）
                line_no = state.line_no_of(m.group("id"))
                title = head = None
                if line_no is not None:  # R5B-P3-1/P3-2：meta/JSON-LD + title=文案全文
                    title, head = _dy_detail_meta(state, state.dataset().read(line_no))
                return self._serve_state_page("detail.html", {}, title=title, head=head)
            m = _DY_USER_PATH_RE.match(path)
            if m:
                sec = m.group("sec")
                ent = state.sec_uid_map().get(sec)
                if not ent or not ent[1]:
                    return self._page_miss()
                uid, line_nos = ent[0], ent[1]
                first = state.dataset().read(line_nos[0])
                st = self._profile_state(state, uid, first, line_nos=line_nos)
                st["author"]["sec_uid"] = sec
                # R5B-P1：「作品/喜欢」tab 数据可用——喜欢 tab 用同人作品确定性轮转
                # （真站作者页 30 链接 + work/like tab；喜欢数据集无独立来源，同人
                # 轮转保证 tab 可用且对数据集内容变化鲁棒）
                half = max(1, len(line_nos) // 2)
                st["like_works"] = [
                    w for w in self._profile_state(
                        state, uid, first, line_nos=line_nos[half:] + line_nos[:half]
                    )["works"]][:30] or st["works"][:10]
                st["tabs"] = {"post": len(st["works"]), "like": len(st["like_works"])}
                title, head = _dy_user_meta(state, first, sec)
                return self._serve_state_page("profile.html", st, title=title, head=head)
            if path == "/detail":
                item_id = (params.get("id") or [None])[0]
                if item_id is not None:
                    return self._serve_state_page("detail.html",
                                                  self._detail_state(state, item_id) or {})
                return None  # 无 id 走 _serve_page（页面 JS 处理）
            return None
        if self.site == "xhs":
            if _XHS_SEARCH_RESULT_RE.match(path):
                return self._serve_file("search.html")
            m = _XHS_SEARCH_RESULT_NOTE_RE.match(path)
            if m:  # R5B-P2-4：真站搜索卡 href=/search_result/<id>（模态承载）——
                # 不存在 id 与 /explore/<id> 同走 302 /404；存在则搜索页 + 笔记 SSR
                item_id = m.group("id")
                if state.line_no_of(item_id) is None:
                    return self._xhs_note_302(path)
                return self._serve_state_page(
                    "search.html", {"entity": state.dataset().read(state.line_no_of(item_id))})
            if path in ("/explore", "/explore/"):  # R5B-P2-4：真站首页即 /explore（分类 feed）
                return self._serve_file("home.html")
            item_id = None
            m = _XHS_EXPLORE_PATH_RE.match(path)
            if m:
                item_id = m.group("id")
            elif path == "/detail":
                item_id = (params.get("id") or [None])[0]
            if item_id is not None:
                line_no = state.line_no_of(item_id)
                if line_no is None:
                    return self._xhs_note_302(path)
                entity = state.dataset().read(line_no)
                title, head = _xhs_detail_meta(state, entity)
                return self._serve_state_page("detail.html", {"entity": entity},
                                              title=title, head=head)
            m = _XHS_USER_PATH_RE.match(path)
            if m:
                uid = m.group("uid")
                line_nos = state.author_line_nos(uid)
                if not line_nos:
                    return self._page_miss()
                first = state.dataset().read(line_nos[0])
                author = ((first.get("note_card") or {}).get("user")) or {}
                st = self._profile_state(state, uid, first, line_nos=line_nos)
                title, head = _xhs_user_meta(state, uid, author)
                return self._serve_state_page("profile.html", st, title=title, head=head)
            return None
        # kuaishou（红队 R3-P1-2：真站 URL 约定路由）
        if _KS_SEARCH_VIDEO_RE.match(path):
            kw = (params.get("searchKey") or [None])[0]
            if kw:  # R5B-P3-1：ks 搜索页 og/canonical 按关键词注入
                title, head = _ks_search_meta(kw)
                return self._serve_state_page("search.html", {}, title=title, head=head)
            return self._serve_file("search.html")
        if _KS_SEARCH_USER_RE.match(path):  # R5B-P2-3：用户 tab 页（数据走 /rest/v/search/user）
            return self._serve_file("search_user.html")
        m = _KS_SHORTVIDEO_PATH_RE.match(path)
        if m:
            st = self._detail_state(state, m.group("id"))
            title = head = None
            if st:
                title, head = _ks_detail_meta(st.get("entity") or {})
            return self._serve_state_page("detail.html", st or {}, title=title, head=head)
        m = _KS_PROFILE_PATH_RE.match(path)
        if m:
            pid = m.group("id")
            line_nos = state.author_line_nos(pid)
            if not line_nos:
                return self._page_miss()
            first = state.dataset().read(line_nos[0])
            st = self._profile_state(state, pid, first, line_nos=line_nos)
            title, head = _ks_user_meta(state, pid, first.get("author") or {})
            return self._serve_state_page("profile.html", st, title=title, head=head)
        if path == "/detail":
            item_id = (params.get("id") or [None])[0]
            if item_id is not None:
                return self._serve_state_page("detail.html",
                                              self._detail_state(state, item_id) or {})
        return None

    def _xhs_note_302(self, path: str):
        """真站实证（live）：302 → /404?source=/404/sec_…&error_code=300031。"""
        src = "/404/sec_pVswaPpO?redirectPath=%s" % quote(
            "https://www.xiaohongshu.com%s" % path, safe="")
        loc = "/404?source=%s&error_code=300031&error_msg=%s&uuid=%s" % (
            quote(src, safe=""), quote("当前笔记暂时无法浏览", safe=""),
            _hex(8) + "-" + _hex(4) + "-4" + _hex(3) + "-a" + _hex(3) + "-" + _hex(12))
        return self._send(302, b"", "text/html", {"Location": loc})

    def do_POST(self):
        state = STATES[self.site]
        parsed = urlparse(self.path)
        path, params = parsed.path, parse_qs(parsed.query)
        bad = path_reject_reason(self._request_target()) or path_reject_reason(path)
        if bad:
            return self._api_or_page_miss("POST", path)
        body = self._read_body()
        try:
            fn = API_ROUTES[self.site].get(("POST", path))
            if fn is None:
                if any(path.startswith(p) for p in API_PREFIXES[self.site]):
                    return self._api_miss("POST", path)
                return self._page_miss()
            out = fn(state, body if path != "/api/sns/web/v2/search/notes" else body)
            # xhs search_notes 返回 (request, response)：响应里回显兜底后的请求语义
            return self._json(200, out[-1] if isinstance(out, tuple) else out, api=True)
        except Exception:
            # 红队 R5C-P2-1：POST body 类型非法等未捕获异常 → 站点错误族 200 信封
            # （xhs page:"abc"/keyword:[...] 旧版落 500+ValueError 信封，语料 0 个 500）
            import traceback
            traceback.print_exc()
            return self._family_error("POST", path)

    def _serve_file(self, fname: str):
        f = _CFG["pages"] / self.site / fname
        if not f.is_file():
            return self._page_miss()  # R5C-P2-1：骨架缺失不回内部构建提示
        return self._send(200, f.read_bytes(), "text/html; charset=utf-8")

    def _serve_page(self, path: str):
        fname = PAGE_FILES.get(path)
        if fname is None:
            # 页面骨架内引用的静态文件（如 style.css）按需透传——纵深第 2 层：
            # realpath 解析后必须仍落在 pages/out/<site>/ 内（路由层校验之外再兜一道）
            base = (_CFG["pages"] / self.site).resolve()
            cand = (base / path.lstrip("/")).resolve()
            inside = cand != base and base in cand.parents
            if inside and cand.is_file():
                data = cand.read_bytes()
                ctype = ("text/css; charset=utf-8" if path.endswith(".css")
                         else "application/javascript; charset=utf-8" if path.endswith(".js")
                         else "application/octet-stream")
                return self._send(200, data, ctype)
            return self._api_or_page_miss("GET", path)
        if fname == "404.html" and self.site != "xhs":
            return self._page_miss()  # 404 页仅 xhs 生成（真站形态）
        if fname == "search.html" and self.site == "kuaishou" and path.rstrip("/") == "/search":
            # 红队 R3-P1-2（live 反向验证）：快手真站无 /search（404），搜索页在 /search/video
            return self._page_miss()
        return self._serve_file(fname)


def make_handler(site: str):
    return type("Handler_%s" % site, (SynthHandler,),
                {"site": site, "server_version": SITE_SERVER[site]})


def port_free(port: int) -> bool:
    import socket
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        try:
            s.bind(("127.0.0.1", port))
            return True
        except OSError:
            return False


def serve_forever(server):
    try:
        server.serve_forever()
    except Exception as e:
        print("[synth_api] server exited: %s" % e, flush=True)


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description="合成模式 API 服务（synthgen 数据 → 契约形态响应 + 合成站骨架）")
    ap.add_argument("--site", default="all", choices=SITES + ("all",))
    ap.add_argument("--base-port", type=int, default=None,
                    help="8761 起（douyin/xhs/kuaishou = base/base+1/base+2），缺省按站点表")
    ap.add_argument("--also-bind", default="",
                    help="synth 模式下额外绑定的兼容端口（逗号分隔，按站点顺序 dy,xhs,ks），"
                         "如 8551,8552,8553；被占用则 WARN 跳过")
    ap.add_argument("--bind", default="127.0.0.1")
    ap.add_argument("--datasets", default=str(_CFG["datasets"]))
    ap.add_argument("--pages-dir", default=str(_CFG["pages"]))
    ap.add_argument("--preload", action="store_true", help="启动即扫描 JSONL 建行偏移")
    args = ap.parse_args(argv)

    _CFG["datasets"] = Path(args.datasets)
    _CFG["pages"] = Path(args.pages_dir)

    sites = list(SITES) if args.site == "all" else [args.site]
    base = args.base_port
    compat = [int(x) for x in args.also_bind.split(",") if x.strip().isdigit()]

    servers = []
    for i, site in enumerate(sites):
        port = base + i if base is not None else SITE_PORTS[site]
        httpd = ThreadingHTTPServer((args.bind, port), make_handler(site))
        httpd.daemon_threads = True
        servers.append((site, port, httpd, "primary"))
        if i < len(compat):
            cport = compat[i]
            if cport == port:
                continue
            if port_free(cport):
                ch = ThreadingHTTPServer((args.bind, cport), make_handler(site))
                ch.daemon_threads = True
                servers.append((site, cport, ch, "compat(hoverfly)"))
            else:
                print("[synth_api] WARN: 兼容端口 %d(%s) 被占用，跳过绑定"
                      "（harness 855x 改写在 synth 模式下由该端口提供）" % (cport, site),
                      flush=True)

    if args.preload:
        for s in sites:
            STATES[s].dataset()

    # dy 作者主页（/user/<sec_uid>）需要 sec_uid → 行号反向索引：后台预热一次
    # （约 10-20s 全量扫描；懒加载兜底仍在，预热只是消除首个作者请求的冷启动）
    if "douyin" in sites:
        threading.Thread(target=STATES["douyin"].sec_uid_map, daemon=True).start()

    print("[synth_api] listening: %s" % ", ".join(
        "%s=%s:%d(%s)" % (site, args.bind, port, tag) for site, port, _, tag in servers),
        flush=True)
    threads = []
    for j, (site, port, httpd, tag) in enumerate(servers):
        if j == 0:
            continue  # 主线程跑第一个
        t = threading.Thread(target=serve_forever, args=(httpd,), daemon=True)
        t.start()
        threads.append(t)
    serve_forever(servers[0][2])
    return 0


if __name__ == "__main__":
    sys.exit(main())
