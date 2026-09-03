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
红队第 6 轮修复（R6A/R6B/R6C，服务线）：
  - R5A-P2-3：ks GraphQL commentListQuery 的 visionCommentList 结构对齐契约/语料
    （补 __typename/rootCommentsV2/pcursorV2 键；续页游标从 pcursor 移到 pcursorV2，
    pcursor 恒 null——28/28 语料样本实证）；
  - R6C-P3-1：空关键词/缺 keyword/BOM body 三站统一 H10 空态族（dy data=[]、
    xhs 无 items 键、ks feeds=[]+no_more；xhs 语料 37/37 空关键词实证）；
  - R6C-P3-2：ks llsid 每请求唯一（search/feed 与 graphql 信封，secrets 重掷）；
  - R6C-P3-3：listen backlog 512（SynthHTTPServer.request_queue_size，50 并发 0 拒连）；
  - R6B-P3-2：xhs /search_result SSR title 按关键词/笔记实体注入（模态态页面 JS 同口径）；
  - R6A C-2：补 12 个契约差集端点（dy suggest_words/emoji/list/mix/series/multi/
    profile/self + xhs search/recommend|filter|onebox、homefeed/category、feed、
    v2/widgets——形态按 contracts + 语料实响应体，静态站常量在
    fixtures/round6_static_payloads.json，数据面从数据集确定性派生）。
红队第 7 轮修复（R7B/R7C，服务/页面收口轮）：
  - R7C-P2-1：dy suggest_words 带 query 形态对齐语料（40/40）——source/type=
    related_search、10 词、词面从关键词→类目映射派生（与 query 强相关）、
    channel_id=94349538563、extra.time_cost 9 键非空；无 query 保持 inbox 9 词；
  - R7C-P3-1：emoji uri / widgets icon 弃 %032x（64 位哈希恒 16 前导零）——emoji
    改双段稠密 32-hex，widgets icon 静态化语料常量路径段（本机同源前缀）；
  - R7C-P3-2：xhs search/recommend 后缀池按关键词类目分池、首词恒为扩展词
    （去空后缀裸词）、highlight_flags 前 len(kw) 位 true；
  - R7C-P3-3：dy 新 5 端点（suggest/mix/multi/series/profile_self）CT 去 charset；
  - R7C-P3-4：ks 站响应补 Connection: keep-alive（语料 903/903；dy/xhs 走 h2 不补）；
  - R7C-P3-5：send_error 站点化——未知方法/畸形请求行不再落 CPython 默认英文页
    （API 前缀回站点 JSON 族、其余回站点 404 页形态；单词请求行强制 HTTP/1.1
    状态行 400）；
  - R7B-P3-3：/search_result/<id> 直开 SSR 注入按笔记派生的背景列表词
    （state.keyword，页面 INIT_KW 以 URL 参数优先）。
红队第 8 轮修复（R8A/R8C，服务/数据合并小修）：
  - R8C-P3-1：JSON 序列化全面紧凑化——_json/_jsonld/SSR state 注入/send_error 体
    统一 separators=(",",":")（语料主域 API 体 7,580/7,580 无空格分隔）；
  - R8C-P3-6：头部级畸形前置 400（parse_request 校验：TE+CL 共存 / HTTP/1.1 缺
    Host / 头行缺冒号——email 解析 defect 检测，过时折行不受影响）；
  - R8C-P3-2：ks profile/feed 信封并入 search/feed 同款（host-name/llsid 重掷/
    webPageArea + feeds 内 comment.us_c/danmakuSwitch/photo.profileUserTopPhoto）；
  - R8C-P3-5：xhs v1/feed note_card 改详情流上下文模板（render_xhs_feed_note_card）；
  - R8A-P2-1（render 侧）：ks 评论 id/游标掺 photo_id 种子（跨照片全局唯一）。
红队第 10 轮修复（R10A/R10C，服务线序列化/读体收口）：
  - R10A-P3-1：序列化出口转义表——dy 全 JSON 面 &<> / U+2028 → 反斜杠uXXXX（语料
    204 体 raw&=0）、ks REST /rest/v/* emoji → UTF-16 代理对（\\ud83d\\udcXX 形态）；
    xhs/ks-graphql 保持原生（wire_json_escape，解析后完全等价）；
  - R10A-P3-4：媒体 URL 按次签发盐——dy 封面/头像签名 query 段掺 (端点,url)
    派生（同端点恒同串、跨端点必异；media_url_pass(salt=)）；
  - R10C-P3-1：单头逗号同值 CL（7,7）按单值受理；读体 ValueError 兜底进站点
    400 族（不再零响应直断）；
  - R10C-P3-2：GET/HEAD 携带 CL/chunked body 时排空再复用连接（_drain_
    request_body，keep-alive 零污染，后续请求不再被残留字节打成 501）。
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
完整 oracle 数据集 ≥8 万条/站，固定 400 页（8000 条）窗口永不出界；迷你
数据集（300 条/站 → 15 页）用同公式缩模数，确定性不变（模数由数据集
记录数唯一确定）。

运行（主 venv，synthgen 依赖 numpy）：
  D:/Projects/temp2/oracle/env/Scripts/python.exe synth_api.py \
      --site all --base-port 8661 [--also-bind 8551,8552,8553]
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
# oracle-lite：自包含打包——synthgen 包/数据集/页面/fixtures 都在本目录内
# （_HERE 即包根），不依赖 oracle 全库（原始版本 _ORACLE_ROOT 指向 oracle/ 顶层）。
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


# ---------------------------------------------------------------------------
# 红队 R9A-P3-1：媒体 URL 值形态规整（响应序列化前统一过一遍）
#
# 数据集侧（pools.py/sites/*.py，R2~R6 生成）的 URL 每表面用单一简化模板：
# dy avatar 69 位 base62 无扩展名、cover 两段随机、play 无 query；xhs avatar
# 24 位随机、webpic 10hex/5hex 前缀；ks uhead/upic 无 B<base64> 文件名、djvod
# 域错拼 ndcimg.com（语料 ndcimgs.com）且主机名可含 "_"（DNS 非法字符，语料
# 0/21,628）。语料真值（21,628 URL 实测）：dy avatar tos-cn-avt-%04d_<32hex>.jpeg
# ?from=、cover image-cut-tos-priv/<32hex>~tplv-*.jpeg?lk3s=…、play 15c000-ce 段
# + 全带 query；xhs avatar 1040g<39> 族、webpic <12位时间戳>/<32hex>/notes_pre_post/
# <44字符>!nc_n_webp_mw_1；ks uhead/upic B<base64>_…(.jpg|.mp4)?tag=1-…&clientCacheKey=…。
#
# 修法取显示/传输层派生（不动数据集，避免重建级联）：_json/_serve_state_page
# 序列化前对响应树做确定性改写——仅命中上述「合成简化形」的 URL 被改写为语料
# 模板形态（按 URL 字符串稳定 hash 派生各段，同一 URL 两次渲染逐字节一致；
# 语料/模板自带的正确形态 URL 不满足任何规则，原样通过）。与 R7C-P3-1 emoji
# URI 模板池同法。
# ---------------------------------------------------------------------------
def _url_hex(url: str, n: int, salt: str = "") -> str:
    return hashlib.md5(("%s\x00%s" % (salt, url)).encode("utf-8")).hexdigest()[:n]


_URL_LOWER = "abcdefghijklmnopqrstuvwxyz0123456789"


def _url_tok(url: str, n: int, salt: str = "", alpha: str = _URL_LOWER) -> str:
    d = hashlib.sha256(("%s\x00%s" % (salt, url)).encode("utf-8")).digest()
    return "".join(alpha[b % len(alpha)] for b in (d * 4)[:n])


def _url_digits(url: str, n: int, salt: str = "", no_leading_zero: bool = False) -> str:
    d = hashlib.sha256(("%s\x00%s" % (salt, url)).encode("utf-8")).digest()
    out = [str(b % 10) for b in (d * 4)[:n]]
    if no_leading_zero and out and out[0] == "0":
        out[0] = "1"
    return "".join(out)


_MU_DY_AVATAR_RX = re.compile(
    r"^(https://p\d+(?:-pc)?\.douyinpic\.com)/(aweme/\d+x\d+|img)/aweme-avatar/([^/?]+)$")
# pc-sign 全量规整：数据集简化形（两段随机）与模板 scramble 形（字符类保持但
# 模板词元被扰动，如 ~tplv-→~tpyg-）都不再命中任何语料封面模板——统一改写为
# 语料 image-cut-tos-priv 模板（响应内不存在逐字节语料封面值：模板 URL 叶子
# 恒经 _scramble_url，数据集值为简化形，见上注）。
_MU_DY_COVER_RX = re.compile(r"^https://p\d+-pc-sign\.douyinpic\.com/.+$")
# douyinvod：无 query 的数据集简化形 + 首段非 32hex 的 scramble 形都改写；
# 语料模板形（32hex/8hex/video/tos/cn/…，含 media-audio-und-mp4a 音频族）原样。
_MU_DY_PLAY_RX = re.compile(r"^(https://v\d+-weba?\.douyinvod\.com)/([^/?]+)(/.*)?$")
_MU_XHS_AVATAR_RX = re.compile(
    r"^https://sns-avatar-qc\.xhscdn\.com/avatar/[A-Za-z0-9\-_]{24}$")
_MU_XHS_WEBPIC_RX = re.compile(
    r"^(https?://sns-webpic-qc\.xhscdn\.com)/[0-9a-f]{10}/[0-9a-f]{5}/((?:[^/?]+/)*[^/?]+)$")
_MU_KS_UHEAD_RX = re.compile(
    r"^(https?://p[\da-z]+\.a\.(?:kwimgs|yximgs)\.com)/uhead/([0-9A-Fa-f]{2})/"
    r"(\d{4})/(\d{2})/(\d{2})/(\d{2})/[A-Za-z0-9\-_]+$")
_MU_KS_UPIC_RX = re.compile(
    r"^(https?://p[\da-z]+\.a\.yximgs\.com)/upic/(\d{4})/(\d{2})/(\d{2})/(\d{2})/"
    r"[A-Za-z0-9\-_]+$")
_MU_KS_DJVOD_RX = re.compile(r"^(https?://)([A-Za-z0-9\-_]+)\.djvod\.ndcimg\.com(/.*)$")

_DY_COVER_TPLVS = ("tsj2vxp0zn-gaosi:40", "dy-360p", "dy-resize-origshort-autoq-75:330")


def _dy_pack_source(salt: str) -> str:
    """dy s=PackSourceEnum_* 端点谱（红队 R11A-P3-4，按语料全量分面真值）。

    旧版按「路径含 /search/」二值化 {SEARCH, AWEME_DETAIL}，外推到 related/post/
    mix/series 与 search/item 五个未验证面时与语料相反/缺值。语料真值（B6 分面）：
    search(general) SEARCH 12,847 dominant；detail AWEME_DETAIL 4,940；related
    WEBPC_RELATED_AWEME 3,286（AWEME_DETAIL 0 例）；post PUBLISH 8,428；
    mix MIX_AWEME 108；series SERIES_AWEME 2,048。SSR/未知面维持 AWEME_DETAIL。"""
    p = salt.split("?", 1)[0]
    if "/related" in p:
        return "WEBPC_RELATED_AWEME"
    if "/aweme/post" in p:
        return "PUBLISH"
    if "/mix/aweme" in p:
        return "MIX_AWEME"
    if "/series/aweme" in p:
        return "SERIES_AWEME"
    if "/search/" in p:
        return "SEARCH"
    return "AWEME_DETAIL"


def _mu_rewrite(url: str, salt: str = "") -> str:
    """单条 URL → 语料模板形态（不命中任何合成简化形则原样返回）。

    salt（R10A-P3-4）：端点级「按次签发」盐。真站签名 CDN URL 按次签发、跨
    端点永不复用同串（语料 search∩detail 同 aweme：封面 url_list 50/50 全不
    相交——42/50 仅签名 query 不同、头像 avatar_thumb 50/50 全不同）。路径段
    （对象身份 32hex 等）仍按 URL 稳定派生（语料 42/50 path 相同），仅签名
    query 段掺 (salt,url) 派生——同端点重复请求恒同串（R9A A3c 确定性保持），
    跨端点必异串。dy avatar 的 query 参数集按端点语义分化：card 形态仅
    search(general) 面（语料 191 例），search/item 视频频道面 294/294 裸
    from=（R11A-P3-4 勘误）；s= 族按 _dy_pack_source 端点谱（R11A-P3-4）。
    """
    m = _MU_DY_AVATAR_RX.match(url)
    if m:
        # 语料形态：…/aweme/100x100/aweme-avatar/tos-cn-avt-0015_<32hex>.jpeg?from=<digits>
        # （搜索卡上下文额外带 card_type=153&column_n=0，语料 191 例实证；
        # R11A-P3-4：仅 search(general)——search/item 视频频道面 294/294 裸 from）
        h = stable_hash("mu-avt::%s\x00%s" % (salt, url))
        frm = (("2064092626", "3782654143", "2956013662")[(h >> 4) % 3]
               if h % 16 == 0 else "327834062")  # 语料 from 值池（327834062 占 96%）
        _p = salt.split("?", 1)[0]
        q = ("card_type=153&column_n=0&from=%s"
             if "/search/" in _p and "/search/item" not in _p else "from=%s") % frm
        return "%s/%s/aweme-avatar/tos-cn-avt-0015_%s.jpeg?%s" % (
            m.group(1), m.group(2), _url_hex(url, 32, "dy-avt"), q)
    if _MU_DY_COVER_RX.match(url):
        # 语料形态：image-cut-tos-priv/<32hex>~tplv-<模板>.jpeg?lk3s=…&x-expires=…
        # &x-signature=<b64>%3D&from=…&s=PackSourceEnum_…
        tplv = _DY_COVER_TPLVS[stable_hash("mu-cov::%s" % url) % len(_DY_COVER_TPLVS)]
        sig = "%s\x00%s" % (salt, url)
        return ("https://p%d-pc-sign.douyinpic.com/image-cut-tos-priv/%s~tplv-%s.jpeg"
                "?lk3s=138a59ce&x-expires=%s&x-signature=%s%%3D&from=327834062"
                "&s=PackSourceEnum_%s&se=false&sc=cover&biz_tag=aweme_search"
                % (3 + stable_hash("mu-covh::%s" % url) % 7, _url_hex(url, 32, "dy-cov"),
                   tplv, 1788278463 + stable_hash("mu-covx::%s" % sig) % 10**7,
                   _url_tok(sig, 27, "dy-covsig",
                            "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"),
                   _dy_pack_source(salt)))
    m = _MU_DY_PLAY_RX.match(url)
    if m and ("?" not in url or not re.match(r"^[0-9a-f]{32}$", m.group(2))):
        # 语料形态：/<32hex>/<8hex>/video/tos/cn/tos-cn-ve-15c000-ce/<16+alnum>/
        # ?a=6383&ch=…&cr=3&…（语料 play URL 100% 带 query）
        br = 700 + stable_hash("mu-br::%s" % url) % 3400
        return ("%s/%s/%s/video/tos/cn/tos-cn-ve-15c000-ce/%s/?a=6383&ch=%d&cr=3&dr=0"
                "&lr=all&cd=0%%7C0%%7C0%%7C3&cv=1&br=%d&bt=%d&cs=%d&ds=3"
                "&ft=AJkekFlZ8XBQ2%%3D&bfs=%d&drs=1&tk=%s&e=video_ts"
                % (m.group(1), _url_hex(url, 32, "dy-play"), _url_hex(url, 8, "dy-play2"),
                   _url_tok(url, 20, "dy-play3",
                            "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"),
                   (11, 26, 148, 224, 10010)[stable_hash("mu-ch::%s" % url) % 5],
                   br, br, stable_hash("mu-cs::%s" % url) % 3,
                   1 + stable_hash("mu-bfs::%s" % url) % 4,
                   _url_tok(url, 12, "dy-tk",
                            "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789")))
    if _MU_XHS_AVATAR_RX.match(url):
        # 语料形态：/avatar/1040g<39 小写字母数字>?imageView2/2/w/{80,120}/format/{jpg,webp}
        h = stable_hash("mu-xa::%s" % url)
        return "https://sns-avatar-qc.xhscdn.com/avatar/1040g%s?imageView2/2/w/%d/format/%s" % (
            _url_tok(url, 39, "xhs-avt"), (120 if h % 2 else 80),
            ("jpg" if h % 3 else "webp"))
    m = _MU_XHS_WEBPIC_RX.match(url)
    if m:
        # 语料形态：/<12位时间戳>/<32hex>/<kind>/<44字符>!nc_n_webp_{mw,prv}_1
        # 红队 R11A-P3-2：分钟级换签 + ts 段真时间语义——语料同笔记跨抓取
        # 跨分钟 ts+hash 双换（202609012226/2c0360e4… → 202609012227/c3c59d6e…）、
        # 同分钟同串，ts 段 29,039/29,039 合法日期（YYYYMMDDHHMM 真时间）；
        # 旧版 "202609"+随机6位 仅 4/150 合法且恒同串。hash 段掺「分钟桶+端点」
        # 盐：同端点同分钟确定（确定性保持）、跨分钟必换、跨端点必异；对象
        # 身份段（44 字符）仍按 URL 稳定派生。
        segs = m.group(2).split("/")
        kind = segs[0] if segs[0] in ("notes_pre_post", "comment", "spectrum") \
            else "notes_pre_post"
        bucket = time.strftime("%Y%m%d%H%M")  # 服务当前时间（录制真时间同语义）
        return "%s/%s/%s/%s/%s!nc_n_webp_%s_1" % (
            m.group(1), bucket,
            _url_hex("%s\x00%s\x00%s" % (bucket, salt, url), 32, "xhs-wp"),
            kind, _url_tok(url, 44, "xhs-wp-id"),
            ("mw" if stable_hash("mu-xw::%s" % url) % 2 else "prv"))
    m = _MU_KS_UHEAD_RX.match(url)
    if m:
        # 语料形态：/uhead/<2字符>/<日期>/<时>/B<base64(时间串_数字_…)>_s.jpg
        import base64 as _b64
        h = stable_hash("mu-ku::%s" % url)
        payload = "%s-%s-%s %s:%02d:%02d_%d_%d_hd%d_%d" % (
            m.group(3), m.group(4), m.group(5), m.group(6),
            h % 60, (h >> 6) % 60, h % 10**9, (h >> 8) % 10**9,
            100 + (h >> 10) % 900, 1 + (h >> 12) % 999)
        return "%s/uhead/%s/%s/%s/%s/%s/B%s_s.jpg" % (
            m.group(1), m.group(2), m.group(3), m.group(4), m.group(5), m.group(6),
            _b64.b64encode(payload.encode()).decode())
    m = _MU_KS_UPIC_RX.match(url)
    if m:
        # 语料形态：/upic/<日期>/<时>/B<base64>_B<32hex>.jpg?tag=1-…&clientCacheKey=…
        import base64 as _b64
        h = stable_hash("mu-kp::%s" % url)
        payload = "%s-%s-%s %s:%02d:%02d_%d_%d_%d" % (
            m.group(2), m.group(3), m.group(4), m.group(5),
            h % 60, (h >> 6) % 60, h % 10**10, (h >> 8) % 10**9, (h >> 10) % 10)
        area = ("xpcwebsearch", "xpcwebprofile", "xpcwebdetail")[h % 3]
        return ("%s/upic/%s/%s/%s/%s/B%s_B%s.jpg?tag=1-%d-%s-0-%s-%s"
                "&clientCacheKey=3x%s_%s"
                % (m.group(1), m.group(2), m.group(3), m.group(4), m.group(5),
                   _b64.b64encode(payload.encode()).decode(), _url_hex(url, 32, "ks-upic"),
                   1788275713 + h % 10**6, area, _url_tok(url, 10, "ks-upic-t"),
                   _url_hex(url, 16, "ks-upic-s"), _url_tok(url, 13, "ks-upic-c"),
                   _url_hex(url, 8, "ks-upic-k")))
    m = _MU_KS_DJVOD_RX.match(url)
    if m:
        # 语料形态：<alnum 主机>.djvod.ndcimgs.com（语料正确域，多一个 s；主机
        # 字符集 [0-9a-z]——旧版 base62 可含 "_" 为 DNS 非法字符）+
        # /bs2/photo-video-mz/<19位>_<16hex>_<4位>_hd15.mp4?tag=1-…&provider=self…
        h = stable_hash("mu-kd::%s" % url)
        return ("https://%s.djvod.ndcimgs.com/bs2/photo-video-mz/%s_%s_%d_hd15.mp4"
                "?tag=1-%d-unknown-0-%s-%s&provider=self&clientCacheKey=3x%s_%s"
                % (_url_tok(url, 8 + h % 17, "ks-djvod"),
                   _url_digits(url, 19, "ks-djvod-id", no_leading_zero=True),
                   _url_hex(url, 16, "ks-djvod-h"), 1000 + h % 9000,
                   1788275713 + h % 10**6, _url_tok(url, 10, "ks-djvod-t"),
                   _url_hex(url, 16, "ks-djvod-s"), _url_tok(url, 13, "ks-djvod-c"),
                   _url_hex(url, 8, "ks-djvod-k")))
    return url


def media_url_pass(obj, salt: str = ""):
    """响应树确定性改写（不动缓存原对象）：仅字符串叶子命中合成简化形才改写。

    salt（R10A-P3-4）＝端点级按次签发盐（"api:<路径>" / "ssr:<骨架名>"）：
    同端点内同 URL 恒同串（确定性），跨端点签名 query 必异（真站按次签发语义）。
    """
    if isinstance(obj, str):
        if "://" in obj:
            return _mu_rewrite(obj, salt)
        return obj
    if isinstance(obj, dict):
        return {k: media_url_pass(v, salt) for k, v in obj.items()}
    if isinstance(obj, list):
        return [media_url_pass(v, salt) for v in obj]
    return obj


# ---------------------------------------------------------------------------
# 红队 R10A-P3-1：JSON 字符串转义表（站点序列器线格式指纹，序列化层派生）。
#
# 语料逐字节扫描（round10A B1 + 本轮收口复核全端点）：
#   - dy 主域 JSON 体 raw `&`=0（204 体；`>`59,134、`<`3,802、`\u2028`=45 次全走
#     `\uXXXX` 转义——fastjson/Jackson HTML-safe 序列器行为；aweme/post 的 raw&
#     体全部为 spaced 脱敏占位重建体，非真站字节）；dy 页面内嵌 JSON 同表。
#   - ks REST /rest/v/* 面 emoji 恒 `\ud83d\udcXX` 代理对转义（search/feed 119、
#     photo/comment 213、profile/feed 90、search/user 311、feed/hot 52 处，raw
#     非 BMP 全 0——Java 系序列器）；ks graphql（172 处原生）与 xhs（全原生）保持。
# JSON 解析后完全等价（零功能破坏）；&<>、U+2028 与非 BMP 字符只可能出现在
# 字符串字面量内（非 JSON 结构字符），对序列化后文本整串替换不触碰结构位，
# 也不引入空格分隔（R8C-P3-1 紧凑化保持）。
# ---------------------------------------------------------------------------
_DY_JSON_ESC = (("&", "\\u0026"), ("<", "\\u003c"), (">", "\\u003e"), ("\u2028", "\\u2028"))
_NONBMP_RX = re.compile("[\U00010000-\U0010FFFF]")


def dy_json_escape(text: str) -> str:
    """dy 出口：字符串值转义表（& / < / > / U+2028 → \\uXXXX）。"""
    for a, b in _DY_JSON_ESC:
        if a in text:
            text = text.replace(a, b)
    return text


def ks_rest_json_escape(text: str) -> str:
    """ks REST /rest/v/* 出口：非 BMP emoji → UTF-16 代理对转义（\\ud83d\\udcXX）。"""
    if not _NONBMP_RX.search(text):
        return text

    def _pair(m):
        cp = ord(m.group(0)) - 0x10000
        return "\\u%04x\\u%04x" % (0xD800 + (cp >> 10), 0xDC00 + (cp & 0x3FF))

    return _NONBMP_RX.sub(_pair, text)


def wire_json_escape(site: str, path: str, text: str) -> str:
    """序列化出口按站点/端点族套转义表（R10A-P3-1；不命中族保持原生）。"""
    if site == "douyin":
        return dy_json_escape(text)
    if site == "kuaishou" and path.split("?", 1)[0].startswith("/rest/"):
        return ks_rest_json_escape(text)
    return text


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
    完整 oracle 数据集 ≥8 万条/站固定 400 页永不出界；迷你集 300 条 → 15 页。"""
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


def ks_llsid_fresh() -> str:
    """ks llsid 每请求重掷（红队 R6C-P3-2）：19 位数字（首位非 0）。

    语料真值：ks_search_video 同一会话内 2 次 search/feed 的 llsid 两两不同
    （sample_0004/0006 均 2/2 不同、跨 sample 亦全不同）——逐请求掷出的会话
    标识。与 dy logid（R5C-P3-1）同族的响应时变字段处理：内容确定性字段不动，
    仅 llsid 按请求唯一（secrets 随机源，进程内任意两次调用必不同）。
    """
    import secrets
    return secrets.choice("123456789") + "".join(
        secrets.choice("0123456789") for _ in range(18))


def _refresh_ks_volatile(val):
    """递归重掷响应内全部非空字符串 llsid 键（R6C-P3-2，同 dy logid 家族）。

    search/feed、graphql likeDataQuery / visionShortVideoReco 等信封均带
    llsid；None 值（visionProfilePhotoList）保持 None——只刷新已是字符串的键，
    不引入/删除键。返回新结构，不动入参。
    """
    if isinstance(val, dict):
        return {k: (ks_llsid_fresh() if k == "llsid" and isinstance(v, str)
                    and v else _refresh_ks_volatile(v))
                for k, v in val.items()}
    if isinstance(val, list):
        return [_refresh_ks_volatile(v) for v in val]
    return val


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
    if not keyword.strip():
        # 红队 R6C-P3-1：空/缺关键词 → 真站空态族（H10 口径三站统一；
        # xhs 语料 37/37 空关键词实证空信封，dy/ks 语料无空关键词样本——
        # 按 H10 声明口径统一为 data=[] 空集，不再回默认窗口满页）
        return synth_render.render_douyin_search_empty(keyword, offset, stream=True)
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
    if not keyword.strip():  # 红队 R6C-P3-1：空/缺关键词 → H10 空态族
        return synth_render.render_douyin_search_empty(keyword, offset, stream=False)
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
    if not keyword.strip():  # 红队 R6C-P3-1：空/缺关键词 → H10 空态族
        return synth_render.render_douyin_search_empty(keyword, offset, stream=False)
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
    # 红队 R6C-P3-1：空/缺关键词（含 BOM body 整体解析失败 → body={} 场景）→
    # H10 空态族 feeds=[]+no_more（xhs 语料 37/37 空关键词实证同族空信封；
    # ks 语料无空关键词样本，按 H10 声明口径三站统一）
    if not keyword.strip():
        return {"result": 1, "pcursor": "no_more", "searchSessionId": session_id,
                "llsid": ks_llsid_fresh(), "host-name": "", "webPageArea": "searchxxnull",
                "feeds": []}
    # 红队 R5C-P3-5：终止游标 no_more 回传 → 保持 no_more + 空页（不回绕第 1 页，
    # 重试/回放型 agent 重发末游标不会陷入翻页死循环）
    if pcursor == "no_more":
        return {"result": 1, "pcursor": "no_more", "searchSessionId": session_id,
                "llsid": ks_llsid_fresh(), "host-name": "", "webPageArea": "searchxxnull",
                "feeds": []}
    # 红队 R3-P3-2：关键词结果窗口有界（8-15 页确定性），翻尽 pcursor="no_more"
    max_rel = 8 + stable_hash("ks-window::%s" % ((keyword or "").strip() or "<default>")) % 8
    rel = int(pcursor) + 1 if pcursor.isdigit() and pcursor != "" else 1
    if rel > max_rel:
        return {"result": 1, "pcursor": "no_more", "searchSessionId": session_id,
                "llsid": ks_llsid_fresh(), "host-name": "", "webPageArea": "searchxxnull",
                "feeds": []}
    page = _clip_page(state, rot + rel)
    size = _body_int(body, "page_size", DEFAULT_PAGE_SIZE, lo=1, hi=50)  # R5C-P2-1 容错
    req, resp = synth_render.render_ks_search_feed(
        state.dataset(), page, size, pcursor, session_id, keyword)
    # 契约形态：pcursor "1"→"2" 递增回传（窗口内相对页号）；末页回 no_more（R3-P3-2）
    resp["pcursor"] = "no_more" if rel >= max_rel else str(rel)
    resp["searchSessionId"] = session_id
    # 红队 R6C-P3-2：llsid 每请求唯一（语料真值同会话两两不同；render 侧
    # keyword/页级确定性派生 → 同词 3 连发恒定，可被「llsid 去重」识别）
    resp["llsid"] = ks_llsid_fresh()
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
    if op == "commentListQuery":
        # 红队 R5A-P2-3（服务线修复层）：visionCommentList 信封对齐契约/语料真值——
        # 28/28 语料样本键集恒为 {__typename, commentCount(null), commentCountV2,
        # pcursor(null), pcursorV2(13 位数字串), rootComments([]), rootCommentsV2}：
        # 续页游标在 pcursorV2（客户端把它回传进下页请求 variables.pcursor），
        # pcursor 恒 null、rootComments 恒空数组（列表数据在 rootCommentsV2）。
        # render 侧旧形态把续页游标放 pcursor 且缺 __typename/rootCommentsV2 键——
        # 本层做结构归一（数据面来自 synthgen，键位/键集在服务层修齐）。
        data = resp.get("data") if isinstance(resp.get("data"), dict) else None
        vcl = (data or {}).get("visionCommentList")
        if isinstance(vcl, dict):
            cont = vcl.get("pcursor")  # render 旧键位：more 时 13 位数字串、尾页 None
            roots = vcl.get("rootCommentsV2") or []
            normalized = {
                "__typename": "VisionRootCommentFeed",
                "commentCount": None,
                "commentCountV2": vcl.get("commentCountV2", 0),
                "pcursor": None,
                "pcursorV2": str(cont) if cont else "no_more",
                "rootComments": [],
                "rootCommentsV2": roots,
            }
            # 保留归一集之外的未知键（对 synthgen 演进鲁棒）
            for k, v in vcl.items():
                if k not in normalized:
                    normalized[k] = v
            resp["data"]["visionCommentList"] = normalized
            if line_no is not None:
                _cid_remember("kuaishou",
                              [{"cid": r.get("commentId")} for r in roots], line_no)
    # 红队 R6C-P3-2：graphql 信封内 llsid（likeDataQuery / visionShortVideoReco）
    # 每请求重掷（同 search/feed 处理；None 保持 None）
    return _refresh_ks_volatile(resp)


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
    """GET /rest/v/profile/get（作者头部：eid/userName/fans/follows…）。

    红队 R12A-P3-2：未知/缺失 userId → 语料唯一错误形态 `{"result":109}` 单键体
    （语料 3/3，搜索场景点击已注销作者）。旧版兜底伪造完整实体（result=1 +
    userId=0 + 「快手用户」 + 默认头像）与语料错误形态相反，且假实体本身是
    探测用户存在性的 agent 的自指纹——按语料回 109，不再伪造。"""
    user_id = (params.get("userId") or [""])[0]
    line_nos = state.author_line_nos(user_id) if user_id else []
    if not line_nos:
        return {"result": 109}
    return synth_render.render_ks_profile_get(state.dataset(), line_nos, user_id)


def ks_profile_feed(state: SiteState, body: dict) -> dict:
    """POST /rest/v/profile/feed（作者作品页：type/tags/authorStatement/author/photo）。

    红队 R8C-P3-2：信封族并入 search/feed 同款（host-name/llsid/webPageArea）——
    llsid 每请求重掷（R6C-P3-2 同法，render 侧确定性派生会被「llsid 去重」识别）。"""
    body = dict(body or {})
    user_id = _body_str(body, "user_id") or _body_str(body, "userId")
    pcursor = _body_str(body, "pcursor")
    line_nos = state.author_line_nos(user_id) if user_id else []
    if not line_nos:
        return {"result": 1, "pcursor": "no_more", "feeds": [], "host-name": "",
                "llsid": ks_llsid_fresh(), "webPageArea": "profilexxnull"}
    resp = synth_render.render_ks_profile_feed(state.dataset(), line_nos, user_id, pcursor)
    resp["llsid"] = ks_llsid_fresh()
    return resp


def ks_user_info(state: SiteState, params: dict) -> dict:
    """GET /api/user/info（Media-Monitor 用户增强契约 kuaishou-user v1：sec_uid 定位、
    $.user_list 绑定；engine.UserProfile 固定发 sec_uid 参数）。语料无该路径真值
    （MM 契约自述 reconstructed），数据从 synthgen 作者池派生、跨请求确定性。
    ks 数据集作者主键为 author.id（3x…，搜索/作者面回传的都是它）——sec_uid 参数
    值即按该主键解析，同时兼容 userId 参数名；未知 id（如评论面合成的 author_id）
    按 f(id) 确定性合成档案（本端点语料无错误形态真值、MM 契约要求 $.user_list
    恒可绑定，故保留合成兜底；REST /rest/v/profile/get 的未知 id 面按语料回
    {"result":109}，两端口径不同——R12A-P3-2），
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
# 契约端点补面（红队 round 6A 报告 C 段差集 C-2 组 12 个新数据面端点）：
#   dy  suggest_words（搜索联想，88 req）/ emoji/list（评论表情面板，178）/
#       multi/aweme/detail（批量详情，14）/ mix/aweme（合集，3）/ series/aweme
#       （短剧系列，12）/ user/profile/self（自档案，120）
#   xhs search/recommend（搜索发现词，111）/ search/filter（筛选层，23）/
#       search/onebox（23）/ homefeed/category（2）/ feed（笔记详情流，1）/
#       v2/widgets（笔记相关搜索组件，39）
# 形态全部按 contracts/*.contract.json + 语料 sanitized_corpus 实响应体；静态站
# 常量（emoji 名录/筛选层/分类表）落在 fixtures/round6_static_payloads.json，
# 数据面（联想词/合集条目/详情流）从 synthgen 数据集确定性派生（对数据集内容
# 变化鲁棒：字段缺失回静态池，绝不 500）。
# ---------------------------------------------------------------------------
_ROUND6_FIXTURE = {"loaded": False, "data": {}}


def _round6_fixture() -> dict:
    if not _ROUND6_FIXTURE["loaded"]:
        try:
            f = _HERE / "fixtures" / "round6_static_payloads.json"
            _ROUND6_FIXTURE["data"] = json.loads(f.read_text(encoding="utf-8"))
        except Exception:
            _ROUND6_FIXTURE["data"] = {}
        _ROUND6_FIXTURE["loaded"] = True
    return _ROUND6_FIXTURE["data"]


def _dy_env() -> dict:
    """dy 详情族公共时变信封（extra/log_pb 同源 logid，R5C-P3-1 口径）。"""
    logid = synth_ids.dy_logid(None)
    return {"extra": {"fatal_item_ids": [], "logid": logid, "now": _now_ms()},
            "log_pb": {"impr_id": logid}}


def _uuid4_str() -> str:
    import uuid
    return str(uuid.uuid4())


# dy suggest_words 兜底词池（数据集话题词元不足 9 个时补位；语料 rsp_source=inbox 形态）
_DY_SUGGEST_POOL = ["美食探店", "旅行攻略", "家常菜", "萌宠日常", "健身打卡", "手机摄影",
                    "露营装备", "咖啡拉花", "穿搭分享", "职场干货", "农村生活", "赶集",
                    "街头采访", "家居收纳", "夜市小吃", "自驾游"]

# 红队 R7C-P2-1：带 query 形态词面必须与 query 强相关——按关键词→类目映射派生
# 前缀扩展后缀池（对照语料真值形态：街头采访→街头采访搞笑/街头采访外国人/街头挑战/
# 采访类视频；无人机航拍→大疆neo2/高清航拍无人机/无人机航拍视频；滑板教学→滑板教学
# 新手入门/附近滑板培训班）。未命中类目回通用池（语料 40/40 带 query 样本词面全部
# 与 query 强相关；旧版对任意 query 回 inbox 轮转窗口话题标签，9 词含 query 的 0 个）。
_DY_RELATED_SUFFIX_POOLS = [
    (("美食", "菜", "厨", "小吃", "夜市", "探店", "烘焙", "辅食", "吃货", "食谱"),
     ("做法", "探店", "教程", "推荐", "避雷", "家常菜", "视频", "博主", "团购", "合集")),
    (("采访",), ("搞笑", "外国人", "热门", "名场面", "视频", "博主", "挑战", "合集",
                 "高能", "翻车")),
    (("航拍", "无人机", "摄影", "拍照", "相机",),
     ("航拍视频", "多少钱", "大片", "教程", "入门", "推荐", "视频", "技巧", "风景", "剪辑")),
    (("健身", "减肥", "增肌", "瑜伽", "拉伸", "腹肌",),
     ("教程", "跟练", "入门", "计划", "动作", "视频", "干货", "合集", "打卡", "饮食")),
    (("旅行", "旅游", "攻略", "自驾", "露营", "徒步",),
     ("攻略", "路线", "必备", "清单", "避雷", "vlog", "花费", "景点", "装备", "跟拍")),
    (("穿搭", "美妆", "护肤", "化妆", "显白", "发型",),
     ("推荐", "教程", "避雷", "合集", "显瘦", "日常", "平价", "通勤", "干货", "好物")),
    (("教学", "教程", "入门", "新手", "课程", "培训", "滑板", "滑雪", "游泳",),
     ("新手入门", "培训班", "免费课程", "附近", "教程", "入门", "视频", "动作", "技巧",
      "装备")),
    (("电视剧", "电影", "综艺", "动漫", "影视", "短剧", "剧场", "食堂",),
     ("全集", "第二季", "在线观看", "解说", "结局", "演员表", "高清", "免费", "剧情",
      "幕后")),
    (("考研", "考试", "复习", "学习", "数学", "英语", "公考",),
     ("经验", "分享", "课程", "资料", "时间表", "攻略", "上岸", "题库", "计划", "干货")),
    (("装修", "家居", "收纳", "布置", "打扫", "整理",),
     ("风格效果图", "步骤流程", "预算", "风格", "避雷", "清单", "设计", "攻略", "日记",
      "干货")),
]
_DY_RELATED_SUFFIX_DEFAULT = ("推荐", "教程", "视频", "合集", "热门", "博主", "入门",
                              "避雷", "附近", "干货")

# 语料真值：query 形态 3 值分布 94349535277×28 / 94349538563×40 / 94349563971×20，
# 带 query 样本多为 94349538563；无 query（inbox）恒 94349535277。
_DY_SUG_CHANNEL_QUERY = 94349538563
_DY_SUG_CHANNEL_INBOX = 94349535277


def _dy_related_suffixes(query: str) -> tuple:
    for pats, sfx in _DY_RELATED_SUFFIX_POOLS:
        if any(p in query for p in pats):
            return sfx
    return _DY_RELATED_SUFFIX_DEFAULT


def _dy_suggest_time_cost() -> dict:
    """extra.time_cost 9 键非空字符串（语料 inbox/related_search 两形态同键集）。

    旧版恒 {}——R7C 伴随项：networkServer* 为响应时点毫秒、其余为耗时毫秒，
    确定性小值 + 时点字段（不参与任何断言的稳定性面）。
    """
    now = _now_ms()
    cost = 40 + stable_hash("dy-sug-cost::%d" % (now // 1000)) % 160
    return {"call_extra_time": "0", "call_rpc_time": str(cost), "init_time": "3",
            "networkServerEngineRequestTime": str(now - 9),
            "networkServerEngineResponseTime": str(now),
            "networkServerRequestTime": str(now - 12),
            "networkServerResponseTime": str(now),
            "server_engine_cost": str(cost), "stream_inner": str(cost + 20)}


def dy_suggest_words(state: SiteState, params: dict) -> dict:
    """GET /aweme/v1/web/api/suggest_words（搜索联想）。

    语料真值两形态（R7C A4 复核，88 req）：
    - 带 query（聚焦搜索框逐字输入）：data[0] source/type="related_search"、**10 词**、
      词面为 query 的强相关扩展/联想、channel_id=94349538563；
    - 无 query：source/type="inbox"、9 词（热搜词）、channel_id=94349535277。
    顶层 7 键（StabilityStatistics/data/errno/extra/log_id/msg/real_log_id），
    word 项 {id(19 位), params, word}。无 query 词面从数据集 rotation 窗口的 desc
    话题词元派生（确定性），不足回静态池。
    """
    query = (params.get("query") or [""])[0].strip()
    env_logid = synth_ids.dy_logid(None)
    if query:
        # R7C-P2-1：带 query → related_search 10 词（词面与 query 强相关）
        sfx = _dy_related_suffixes(query)
        words: list = []
        for s in sfx:
            w = query + s
            if w not in words:
                words.append(w)
            if len(words) >= 10:
                break
        core = query[:-2] if len(query) >= 4 else ""   # 语料样本含核心短词（滑板教学→滑板）
        if core and core not in words and len(words) < 10:
            words.insert(min(6, len(words)), core)
        while len(words) < 10:
            w = query + sfx[len(words) % len(sfx)]
            if w not in words:
                words.append(w)
        source = "related_search"
        channel = _DY_SUG_CHANNEL_QUERY
    else:
        words = []
        try:
            n = len(state.dataset())
            start = rotation_pages("douyin", "<default>") * DEFAULT_PAGE_SIZE
            i = start
            while len(words) < 9 and i < min(start + 200, n):
                desc = str((state.dataset().read(i) or {}).get("desc") or "")
                for t in re.findall(r"#([^\s#，。！？.,!?]{1,24})", desc):
                    w = t.strip()
                    if w and w not in words:
                        words.append(w)
                        if len(words) >= 9:
                            break
                i += 1
        except Exception:
            words = []
        for w in _DY_SUGGEST_POOL:
            if len(words) >= 9:
                break
            if w not in words:
                words.append(w)
        source = "inbox"
        channel = _DY_SUG_CHANNEL_INBOX
    group_words = [{
        "id": str(1000000000000000000 + stable_hash("dy-sug::%s" % w) % 8000000000000000000),
        "params": {"challenge_id": "0",
                   "extra_info": {"enable_prefetch": "0", "hotboard_label": "0",
                                  "is_trending_hotboard_source": "0",
                                  "sentence_id": "0", "sim_id": "0"},
                   "info": "{}", "reason": ""},
        "word": w,
    } for w in words]
    return {
        "StabilityStatistics": {"1": "1"},
        "data": [{"params": {"channel_id": channel,
                             "extra_info": {"qrec_channel": "AWEME_RECOMMEND_GUESS",
                                            "qrec_channel_is_aweme": "1"}},
                  "source": source, "type": source, "words": group_words}],
        "errno": "0",
        "extra": {"RespFrom": "do_search", "call_per_refresh": "",
                  "qrec_extra": "", "time_cost": _dy_suggest_time_cost()},
        "log_id": env_logid, "msg": "success", "real_log_id": env_logid,
    }


def _dense_hex32(key: str) -> str:
    """稠密 32-hex（红队 R7C-P3-1：64 位哈希 %032x 恒带 16 个前导零，整族 URI 可
    编程一辨——双段拼接后均匀覆盖全宽；语料真值 emoji uri 为稠密 32-hex）。"""
    return "%016x%016x" % (stable_hash("a::%s" % key), stable_hash("b::%s" % key))


def dy_emoji_list(state: SiteState, params: dict) -> dict:
    """GET /aweme/v1/web/emoji/list（评论表情面板，371 项语料名录）。

    语料真值：{status_code:0, version:13 位, emoji_list:[{origin_uri,
    display_name, hide, emoji_url:{uri, url_list}}]}。名录为站静态常量
    （fixtures）；emoji_url 按名确定性派生，url_list 用本机同源 CDN 形态路径
    （R3-P1-3 口径，synth cover 路由可服务）。
    """
    pairs = _round6_fixture().get("dy_emoji_list") or []
    emoji_list = []
    for e in pairs:
        uri = "tos-cn-i-tsj2vxp0zn/%s" % _dense_hex32("dy-emoji::%s" % e["origin_uri"])
        emoji_list.append({
            "origin_uri": e["origin_uri"], "display_name": e["display_name"],
            "hide": int(e.get("hide", 1)),
            "emoji_url": {"uri": uri,
                          "url_list": ["/obj/%s?from=876277922" % uri,
                                       "/obj/%s?from=876277922" % uri]},
        })
    return {"emoji_list": emoji_list, "status_code": 0, "version": 1766572369433}


def _dy_id_list(raw) -> list:
    """multi/aweme/detail 的 aweme_ids 参数（JSON 数组串，form 体 percent 编码）。"""
    if isinstance(raw, list):
        return [str(x) for x in raw]
    s = str(raw or "")
    try:
        v = json.loads(s)
        if isinstance(v, list):
            return [str(x) for x in v]
    except Exception:
        pass
    return [m for m in re.findall(r"\d{15,21}", s)]


def dy_multi_aweme_detail(state: SiteState, body: dict) -> dict:
    """POST /aweme/v1/web/multi/aweme/detail（批量详情，form 体 aweme_ids=[id,…]）。

    语料真值：{aweme_details:[aweme…], emoji_list:null, extra, filter_list:null,
    log_pb, status_code:0, verification_filter_list:null}；条目复用 detail 端点
    的 render_aweme(detail=True) 全量 aweme 形态，未命中 id 静默省略。
    """
    ids = _dy_id_list(body.get("aweme_ids"))
    details = []
    for aid in ids[:20]:
        line_no = state.line_no_of(aid) if aid else None
        if line_no is None:
            continue
        details.append(synth_render.render_aweme(state.dataset().read(line_no), detail=True))
    env = _dy_env()
    return {"aweme_details": details, "emoji_list": None, "extra": env["extra"],
            "filter_list": None, "log_pb": env["log_pb"], "status_code": 0,
            "verification_filter_list": None}


def _dy_series_slice(state: SiteState, series_key: str, cursor: int, count: int):
    """mix/series 共用：确定性合集窗口（起点/总集数由 series_key 派生）。"""
    n = len(state.dataset())
    total = 12 + stable_hash("series-total::%s" % series_key) % 38
    start = stable_hash("series-start::%s" % series_key) % max(1, n - 64)
    items, episode = [], {}
    for idx in range(cursor, min(cursor + count, total)):
        rec = state.dataset().read((start + idx) % n)
        items.append(synth_render.render_aweme(rec, detail=True))
        episode[str(rec.get("aweme_id") or "")] = idx + 1
    has_more = 1 if cursor + len(items) < total else 0
    return items, episode, cursor + len(items), has_more


def dy_mix_aweme(state: SiteState, params: dict) -> dict:
    """GET /aweme/v1/web/mix/aweme（合集：?mix_id=&cursor=&count=）。

    语料真值：{aweme_list, cursor:<下页起点>, has_more, item_id_to_episode:
    "<json str id→集号>", log_pb, status_code}（cursor 49/count 6 → 6 条、
    cursor 55、has_more 1）。
    """
    mix_id = (params.get("mix_id") or [""])[0]
    cursor = _int_param(params, "cursor", 0, lo=0)
    count = _int_param(params, "count", 10, lo=1, hi=20)
    env = _dy_env()
    if not mix_id:
        return {"aweme_list": [], "cursor": cursor, "has_more": 0,
                "item_id_to_episode": "{}", "log_pb": env["log_pb"], "status_code": 0}
    items, episode, next_cursor, has_more = _dy_series_slice(state, "mix::%s" % mix_id,
                                                             cursor, count)
    return {"aweme_list": items, "cursor": next_cursor, "has_more": has_more,
            "item_id_to_episode": json.dumps(episode, ensure_ascii=False,
                                             separators=(",", ":")),
            "log_pb": env["log_pb"], "status_code": 0}


def dy_series_aweme(state: SiteState, params: dict) -> dict:
    """GET /aweme/v1/web/series/aweme（短剧系列：?series_id=&pull_type=&cursor=&count=）。

    语料真值：{aweme_list, extra, has_more, log_pb, max_cursor, min_cursor,
    related_item_tag:-1, selection_panel_conf:{has_segment:false}, status_code:0,
    status_msg:"", watch_histories:null}。
    """
    series_id = (params.get("series_id") or [""])[0]
    cursor = _int_param(params, "cursor", 0, lo=0)
    count = _int_param(params, "count", 10, lo=1, hi=20)
    env = _dy_env()
    if not series_id:
        return {"aweme_list": [], "extra": env["extra"], "has_more": 0,
                "log_pb": env["log_pb"], "max_cursor": 0, "min_cursor": 0,
                "related_item_tag": -1, "selection_panel_conf": {"has_segment": False},
                "status_code": 0, "status_msg": "", "watch_histories": None}
    items, _episode, next_cursor, has_more = _dy_series_slice(
        state, "series::%s" % series_id, cursor, count)
    return {"aweme_list": items, "extra": env["extra"], "has_more": has_more,
            "log_pb": env["log_pb"], "max_cursor": next_cursor,
            "min_cursor": cursor or 1, "related_item_tag": -1,
            "selection_panel_conf": {"has_segment": False}, "status_code": 0,
            "status_msg": "", "watch_histories": None}


def dy_user_profile_self(state: SiteState, params: dict) -> dict:
    """GET /aweme/v1/web/user/profile/self（自档案，120 req 全最小 4 键 user）。

    语料真值（sanitized corpus 120/120）：{extra, status_code:0, status_msg:"",
    user:{uid, sec_uid, nickname, short_id}}——录制账号脱敏后即此形；合成站无
    登录态，按同形返回稳定档案（uid/sec_uid 与语料脱敏值同形）。
    """
    env = _dy_env()
    return {"extra": env["extra"], "status_code": 0, "status_msg": "",
            "user": {"uid": "219430709953",
                     "sec_uid": "GT6yKpSN5719416626661872150602267720742142353697",
                     "nickname": "replay_user", "short_id": "0000000000"}}


# xhs search/recommend 后缀池（红队 R7C-P3-2：语料 96 组渐进输入实证——后缀随关键词
# 类目强变化（美食探→店/店博主/图文/拍摄技巧、考研经验→贴/分享/PPT、装修→风格效果图/
# 步骤流程），且 96/96 首词恒为扩展词、无一返回输入本身；旧版跨关键词共享固定池 +
# 首词空后缀裸词）。按类目分池，全部非空后缀（首词必为扩展）。
_XHS_SUG_POOLS = [
    (("美食", "菜", "小吃", "探店", "吃货", "烘焙", "食谱", "做菜"),
     ("店", "店博主", "探店攻略", "家常菜做法大全", "推荐", "图片", "视频", "避雷",
      "教程", "附近")),
    (("考研", "考公", "考试", "复习", "学习", "英语", "数学", "四六级"),
     ("贴", "分享", "上岸", "复习资料", "课程", "攻略", "时间表", "经验", "书单", "提醒")),
    (("装修", "家居", "收纳", "布置", "软装", "家电"),
     ("风格效果图", "步骤流程", "预算", "风格", "避雷", "清单", "设计", "攻略", "日记",
      "干货")),
    (("旅行", "旅游", "攻略", "自由行", "行程", "签证", "徒步"),
     ("攻略", "自由行", "路线", "必去景点", "避雷", "花费", "跟团", "拍照机位", "文案",
      "清单")),
    (("健身", "减肥", "增肌", "瑜伽", "拉伸", "瘦腿", "腹肌"),
     ("计划", "动作", "减脂", "跟练", "食谱", "打卡", "入门", "器械", "一周", "效果")),
    (("穿搭", "美妆", "护肤", "化妆", "显白", "发型", "平价"),
     ("推荐", "攻略", "视频", "图片", "教程", "显瘦", "日常", "通勤", "好物", "避雷")),
    (("摄影", "拍照", "相机", "修图", "调色", "构图"),
     ("技巧", "教程", "参数", "构图", "手机", "入门", "调色", "风格", "攻略", "后期")),
    (("萌宠", "猫", "狗", "养猫", "养狗", "宠物"),
     ("日常", "攻略", "用品", "健康", "训练", "掉毛", "吃什么", "疫苗", "避雷", "日记")),
]
_XHS_SUG_DEFAULT = ("推荐", "攻略", "视频", "图片", "教程", "避雷", "合集", "日常",
                    "清单", "怎么拍")


def _xhs_sug_suffixes(kw: str) -> tuple:
    for pats, sfx in _XHS_SUG_POOLS:
        if any(p in kw for p in pats):
            return sfx
    return _XHS_SUG_DEFAULT


def xhs_search_recommend(state: SiteState, params: dict) -> dict:
    """GET /api/sns/web/v1/search/recommend（搜索发现词，?keyword= 渐进输入）。

    语料真值：{success:true, msg:"成功", code:1000, data:{word_request_id:
    "<uuid>#<ms>", sug_items:[10×{highlight_flags(逐字符), search_type:"notes",
    type:"top_note", text}], search_cpl_id:<32hex>}}——联想词 = 输入前缀+补全
    （首词恒为扩展词、后缀随类目分池，R7C-P3-2）；highlight_flags 前 len(kw) 位
    为 true（前缀命中段），余位 false。
    """
    kw = (params.get("keyword") or [""])[0].strip()
    words: list = []
    if kw:
        for suf in _xhs_sug_suffixes(kw):
            w = kw + suf
            if w not in words:
                words.append(w)
            if len(words) >= 10:
                break
    else:  # 空关键词 → 静态热词池（无语料真值，站级兜底口径）
        words = list(_DY_SUGGEST_POOL[:10])
    sug = [{"highlight_flags": [True] * min(len(kw), len(w))
            + [False] * max(0, len(w) - len(kw)),
            "search_type": "notes", "type": "top_note", "text": w} for w in words]
    return {"success": True, "msg": "成功", "code": 1000,
            "data": {"word_request_id": "%s#%d" % (_uuid4_str(), _now_ms()),
                     "sug_items": sug, "search_cpl_id": _hex(32)}}


def xhs_search_filter(state: SiteState, params: dict) -> dict:
    """GET /api/sns/web/v1/search/filter（筛选层，站静态常量 + 逐请求 word_request_id）。"""
    import copy
    payload = copy.deepcopy(_round6_fixture().get("xhs_search_filter") or
                            {"data": {"filters": []}, "code": 0, "success": True,
                             "msg": "成功"})
    rid = "%s#%d" % (_uuid4_str(), _now_ms())
    for g in (payload.get("data") or {}).get("filters") or []:
        g["word_request_id"] = rid
    return payload


def xhs_search_onebox(state: SiteState, body: dict) -> dict:
    """POST /api/sns/web/v1/search/onebox（语料真值：空 onebox 信封 success=false）。"""
    return {"msg": "成功", "data": {}, "code": 0, "success": False}


def xhs_homefeed_category(state: SiteState, params: dict) -> dict:
    """GET /api/sns/web/v1/homefeed/category（/explore 分类表，站静态常量）。"""
    payload = _round6_fixture().get("xhs_homefeed_category")
    if payload:
        return payload
    return {"code": 0, "success": True, "msg": "成功",
            "data": {"categories": [{"id": "homefeed.fashion_v3", "name": "穿搭"},
                                    {"id": "homefeed.food_v3", "name": "美食"}]}}


def xhs_feed(state: SiteState, body: dict) -> dict:
    """POST /api/sns/web/v1/feed（笔记详情流：source_note_id → 该笔记单条流）。

    语料真值：{code:0, success:true, msg:"成功", data:{cursor_score:"", items:
    [{model_type:"note", note_card, ignore:false, id}], current_time:<ms>}}。
    """
    body = dict(body or {})
    note_id = _body_str(body, "source_note_id") or _body_str(body, "note_id")
    data = {"cursor_score": "", "items": [], "current_time": _now_ms()}
    line_no = state.line_no_of(note_id) if note_id else None
    if line_no is not None:
        rec = state.dataset().read(line_no)
        # R8C-P3-5：note_card 用 v1/feed 上下文模板物化（详情流形态，非搜索卡模板）
        data["items"] = [{"model_type": "note",
                          "note_card": synth_render.render_xhs_feed_note_card(rec),
                          "ignore": False, "id": rec.get("id")}]
    return {"code": 0, "success": True, "msg": "成功", "data": data}


def xhs_widgets(state: SiteState, body: dict) -> dict:
    """POST /api/sns/web/v2/widgets（笔记相关搜索组件：猜你想搜胶囊）。

    语料真值：{code:0, success:true, msg:"成功", data:{result:{code,message,
    success}, widgets:{widget_list:[{track, model, biz_type, ui, position}]}}}；
    联想词从笔记标题派生（数据面），icon 用本机同源 /sns-webpic 形态。
    """
    body = dict(body or {})
    note_id = _body_str(body, "note_id")
    word = ""
    line_no = state.line_no_of(note_id) if note_id else None
    if line_no is not None:
        nc = (state.dataset().read(line_no).get("note_card")) or {}
        title = str(nc.get("display_title") or "").strip()
        m = re.match(r"^[^#\s，。！？]{2,8}", title)
        word = m.group(0) if m else ""
    if not word:
        word = _DY_SUGGEST_POOL[stable_hash("xhs-wdg::%s" % (note_id or ""))
                                % len(_DY_SUGGEST_POOL)]
    rid = _hex(32)
    kw_q = quote(word, safe="")
    nid = note_id or ""
    api_extra = quote('{"source_note_id":"%s"}' % nid, safe="")
    # 红队 R7C-P3-1：icon 静态化语料常量（6/6 样本同一图标；路径段取语料真值、
    # 主机段换本机同源 /sns-webpic —— 旧版 %032x 派生恒带 16 前导零可编程一辨）
    icon = "/sns-webpic/fe-platform/09c136c01bac91a3eb7284b6e107e4714d7c06da.png"
    icon_dark = "/sns-webpic/fe-platform/104101l031ul732v92a06ftel2030k000000000blfjmfc"
    return {"code": 0, "success": True, "msg": "成功",
            "data": {"result": {"code": 0, "message": "success", "success": True},
                     "widgets": {"widget_list": [{
                         "track": {"track_id": word, "name": "WEB_RELATED_SEARCH"},
                         "model": {
                             "sub_title": word,
                             "icon_dark": icon_dark,
                             "icon": icon,
                             "link": ("xhsdiscover://search/result?keyword=%s"
                                      "&target_search=notes&mode=note_text_recommend"
                                      "&word_from=SEARCH_WORD_FROM_NOTE_TEXT_RECOMMEND"
                                      "&source=normal_note&noteId=%s&word_request_id=%s"
                                      "&api_extra=%s&add_to_history=true"
                                      % (kw_q, nid, rid, api_extra)),
                             "title": "猜你想搜",
                             "biz_extra": json.dumps(
                                 {"word_request_id": rid, "keyword": word},
                                 ensure_ascii=False, separators=(",", ":"))},
                         "biz_type": "image_related_search",
                         "ui": {"type": "native", "name": "capsule"},
                         "position": {"container": "content_under_capsule",
                                      "order": 100, "show_type": 0},
                     }]}}}


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
        ("GET", "/aweme/v1/web/aweme/post"): dy_user_posted,
        # 红队 round 6A C-2 组：新数据面端点（suggest/emoji/multi/mix/series/self）
        ("GET", "/aweme/v1/web/api/suggest_words/"): dy_suggest_words,
        ("GET", "/aweme/v1/web/api/suggest_words"): dy_suggest_words,
        ("GET", "/aweme/v1/web/emoji/list/"): dy_emoji_list,
        ("GET", "/aweme/v1/web/emoji/list"): dy_emoji_list,
        ("POST", "/aweme/v1/web/multi/aweme/detail/"): dy_multi_aweme_detail,
        ("POST", "/aweme/v1/web/multi/aweme/detail"): dy_multi_aweme_detail,
        ("GET", "/aweme/v1/web/mix/aweme/"): dy_mix_aweme,
        ("GET", "/aweme/v1/web/mix/aweme"): dy_mix_aweme,
        ("GET", "/aweme/v1/web/series/aweme/"): dy_series_aweme,
        ("GET", "/aweme/v1/web/series/aweme"): dy_series_aweme,
        ("GET", "/aweme/v1/web/user/profile/self/"): dy_user_profile_self,
        ("GET", "/aweme/v1/web/user/profile/self"): dy_user_profile_self,
    },
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
        # 红队 round 6A C-2 组：新数据面端点（recommend/filter/onebox/category/feed/widgets）
        ("GET", "/api/sns/web/v1/search/recommend"): xhs_search_recommend,
        ("GET", "/api/sns/web/v1/search/recommend/"): xhs_search_recommend,
        ("GET", "/api/sns/web/v1/search/filter"): xhs_search_filter,
        ("GET", "/api/sns/web/v1/search/filter/"): xhs_search_filter,
        ("POST", "/api/sns/web/v1/search/onebox"): xhs_search_onebox,
        ("POST", "/api/sns/web/v1/search/onebox/"): xhs_search_onebox,
        ("GET", "/api/sns/web/v1/homefeed/category"): xhs_homefeed_category,
        ("GET", "/api/sns/web/v1/homefeed/category/"): xhs_homefeed_category,
        ("POST", "/api/sns/web/v1/feed"): xhs_feed,
        ("POST", "/api/sns/web/v1/feed/"): xhs_feed,
        ("POST", "/api/sns/web/v2/widgets"): xhs_widgets,
        ("POST", "/api/sns/web/v2/widgets/"): xhs_widgets,
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
# 红队 R12C-P3-2：xhs `/` → 302 /explore 带体（语料 25/25 CL:47 text/html;
# charset=utf-8）——体为语料等长小页（47 字节，内容无 CJK，形态对齐）。
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

# xhs `/` 302 体（R12C-P3-2/3：语料 CL:47 带体；`<html><head></head><body>/explore</body></html>` 恰 47 字节）
_XHS_ROOT_302_BODY = b"<html><head></head><body>/explore</body></html>"
assert len(_XHS_ROOT_302_BODY) == 47

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
    # R8C-P3-1：JSON-LD 注入同紧凑分隔符（语料 ld+json 无空格形态）
    return ('<script type="application/ld+json">%s</script>'
            % json.dumps(obj, ensure_ascii=False, separators=(",", ":")))


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
    return "%s - %s" % (desc, _BRAND_NAME["douyin"]), head   # R9B-P3-3：语料 caption+「 - 抖音」后缀（3/3）


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
    """note_card.image_list → og:image URL 列表（WB_DFT 场景；数据集字段缺失则空）。

    R9A-P3-1：og:image 走同一 URL 值形态规整（语料 webpic 模板形态）。"""
    out = []
    for img in ((entity.get("note_card") or {}).get("image_list") or [])[:limit]:
        for info in (img.get("info_list") or []):
            if info.get("image_scene") == "WB_DFT" and info.get("url"):
                out.append(_mu_rewrite(str(info["url"])))
                break
    return out


def _xhs_note_bg_keyword(entity: dict) -> str:
    """红队 R7B-P3-3：/search_result/<id> 直开页背景列表派生词。

    URL 无 keyword 参数——背景列表不再回退构建期默认词（旧版与 URL/内容/title
    三者脱节），改按笔记实体 display_title 派生（SSR 注入 state.keyword，页面
    JS INIT_KW 以 URL 参数优先、此处次之）。
    """
    title = str(((entity.get("note_card") or {}).get("display_title")) or "").strip()
    m = re.match(r"^[^#\s，。！？!?.,{}()（）【】\[\]]{2,8}", title)
    return m.group(0) if m else (title[:4] or "小红书")


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
    # R9B-P3-3：ks 搜索页 <title> 恒静态「快手」（语料 20/20，关键词不入 title）
    return "快手", head


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
    # R9B-P3-3：ks 作者页 <title> 恒静态「快手」（语料 20/20，作者名不入 title）
    return "快手", head


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


# 真站 Content-Type 实证（红队 P2-D4）：dy stream/detail/comment 无 charset、single 带 charset；
# R7C-P3-3：新补 5 端点（suggest_words 88 / mix 2 / multi 28 / series 12 / profile/self
# 120 语料请求全 application/json 无 charset）并入 dy 无 charset 族。
# 红队 R11C-P3-6（全路由 × 语料逐路径 CT 真值对照，rt11c_ct）：search/item 语料
# 49/49 **带** charset（移出无 charset 族）；related/（39）、user/profile/other/
# （20）、aweme/post/（25）语料**无** charset（并入）；ks /graphql 语料 295/295
# application/json 无 charset（_json 单独分支）。
_DY_NO_CHARSET_PATHS = (
    "/aweme/v1/web/general/search/stream",
    "/aweme/v1/web/aweme/detail",
    "/aweme/v1/web/comment/list",
    "/aweme/v1/web/api/suggest_words",
    "/aweme/v1/web/aweme/related",
    "/aweme/v1/web/user/profile/other",
    "/aweme/v1/web/aweme/post",
    "/aweme/v1/web/mix/aweme",
    "/aweme/v1/web/multi/aweme/detail",
    "/aweme/v1/web/series/aweme",
    "/aweme/v1/web/user/profile/self",
)


class SynthHandler(BaseHTTPRequestHandler):
    server_version = "synth_api/1.0"  # make_handler 按站点覆盖（TLB / nginx / 空）
    sys_version = ""  # 去掉 "Python/3.x" 指纹（红队 P2-D4）
    protocol_version = "HTTP/1.1"

    # 由 make_handler 注入（每端口一个 site）
    site: str = "douyin"

    def handle_one_request(self):
        # 红队 R11C-P1-1：每请求重置「响应已发送」标记（_send/send_error 置位，
        # _serve_page 入口防御——一请求至多一响应，RFC 7230 §5.1 消息成帧语义）
        self._response_sent = False
        return super().handle_one_request()

    def version_string(self):
        # 红队 R4-P3-8：基类 version_string = server_version + ' ' + sys_version，
        # sys_version 置空后会拼出尾随空格（"TLB "）；语料真值无空格——先拼后 strip。
        v = self.server_version
        if self.sys_version:
            v = "%s %s" % (v, self.sys_version)
        return v.rstrip()

    def parse_request(self):
        """红队 R8C-P3-6：头部级畸形前置 400（请求走私/坏头行向量）。

        语料无此类请求真值（录制会话从未发过 TE+CL 共存/缺 Host/无冒号头行），
        按 RFC 7230 §5.4/§5.5.3 与 nginx 族的保守 400（站点错误族体由 send_error
        站点化，不泄解释器特征）。三入口：
          ① Transfer-Encoding 与 Content-Length 共存（ smuggling 消歧向量）；
          ② HTTP/1.1+ 请求缺 Host 头；
          ③ 头行缺冒号（email 解析器吸收该行并记 MissingHeaderBodySeparatorDefect，
            过时折行 continuation 不产生 defect——合法折叠不受影响）。
        其余头部级形态（重复 Host/NUL 值/原始 UTF-8 头值/300 头）维持可预期受理。

        红队 R9C-P3-2（第④入口）：重复 Content-Length 且值不一致 → 400
        （RFC 7230 §3.3.2「多个 Content-Length 头或单值含逗号列表且值不同，
        必须视为无效消息」；与 TE+CL 入口同族。值完全一致的重复按单值受理）。
        红队 R11C-P3-4（第⑤入口）：重复 Host（多 header 或单值逗号列表形态，
        不分同值）→ 400（RFC 7230 §5.4「server MUST respond 400 to any request
        that contains more than one Host header field」；nginx/Go 族同款）。
        红队 R11C-P3-5（第⑥入口）：非数字单值 Content-Length → 400（旧版只在
        POST 读体入口有 ValueError 兜底，GET/HEAD/OPTIONS 排空入口 int() 抛
        被吞、不排空 → 连接污染；前置到 parse_request 后全方法一致 400）。"""
        ok = super().parse_request()
        if not ok:
            return False
        try:
            hdrs = self.headers
            bad = None
            if "transfer-encoding" in hdrs and "content-length" in hdrs:
                bad = "te+cl"
            elif self.request_version not in ("HTTP/0.9", "HTTP/1.0") \
                    and "host" not in hdrs:
                bad = "missing-host"
            elif any(getattr(d, "__class__", type(d)).__name__
                     == "MissingHeaderBodySeparatorDefect" for d in hdrs.defects):
                bad = "bad-header-line"
            if bad is None:
                hosts = [str(h) for h in (hdrs.get_all("Host") or [])]
                if len(hosts) > 1 or any("," in h for h in hosts):
                    bad = "dup-host"
            if bad is None:
                cl_vals = []
                for v in (hdrs.get_all("Content-Length") or []):
                    cl_vals.extend(p.strip() for p in str(v).split(",") if p.strip())
                if len(set(cl_vals)) > 1:
                    bad = "dup-cl"
                elif any(not re.fullmatch(r"\d+", v) for v in cl_vals):
                    bad = "invalid-cl"
            if bad:
                self.send_error(400)
                return False
        except Exception:
            pass
        return True

    def log_message(self, fmt, *args):  # 单行访问日志
        print("[synth_api] %s %s" % (self.site, fmt % args), flush=True)

    def send_header(self, keyword, value):
        # 红队 R3-P2-4：快手真站无 Server 头——站点 server 置空时整个头不发
        if keyword.lower() == "server" and not str(value or "").strip():
            return
        return super().send_header(keyword, value)

    def send_error(self, code, message=None, explain=None):
        """红队 R7C-P3-5：错误族的最后一层站点化。

        BaseHTTPRequestHandler 对未知方法（PUT/PATCH/DELETE）与畸形/坏版本/超长
        请求行走 send_error → CPython 默认英文 HTML（「Error response…Error code
        explanation: 501 - …」），站级 Server 头正确但 body 为解释器模板——实现
        指纹。现按站点错误族回体：API 前缀 → 站点 JSON 族（dy status_code 族 /
        xhs code 族 / ks result 族），其余 → 站点 404 页形态；畸形请求行
        （HTTP/0.9 单词回退形态）强制以 HTTP/1.1 状态行回 400（不再裸 body），
        不支持的超大版本号（HTTP/9.9 → 基类 505）按「畸形行统一 400」口径折为
        400（nginx 同款处置，消 CPython 505 指纹），不回显请求行原文、不泄露
        解释器特征。
        """
        try:
            self.log_error('"%s" %s', getattr(self, "requestline", "") or "-", code)
            if code == 505:  # 版本号不支持 → 畸形行统一 400（R7C-P3-5）
                code = 400
            self.close_connection = True
            if getattr(self, "request_version", "HTTP/0.9") == "HTTP/0.9":
                self.request_version = "HTTP/1.1"  # 单词请求行回退 0.9 语义 → 强制状态行
            path = (getattr(self, "path", "") or "").split("?", 1)[0]
            if path and any(path.startswith(p) for p in API_PREFIXES[self.site]):
                body = wire_json_escape(
                    self.site, path,
                    json.dumps(_miss_body(self.site, path, self.command or ""),
                               ensure_ascii=False,
                               separators=(",", ":"))).encode("utf-8")
                ctype = ("application/json" if self.site == "douyin"
                         else "application/json;charset=UTF-8" if self.site == "kuaishou"
                         else "application/json; charset=utf-8")
            else:
                title, text = self._PAGE_MISS_TITLES.get(
                    self.site, self._PAGE_MISS_TITLES["douyin"])
                body = ("<!DOCTYPE html><html lang=\"zh-CN\"><head><meta charset=\"utf-8\">"
                        "<title>%s</title></head><body style=\"font:14px/1.8 sans-serif;"
                        "text-align:center;padding-top:12vh\">"
                        "<h1 style=\"font-size:20px;color:#333\">%s</h1>"
                        "<p style=\"color:#888\"><a href=\"/\">返回首页</a></p></body></html>"
                        % (title, text)).encode("utf-8")
                ctype = "text/html; charset=utf-8"
            self.send_response(code)   # 标准理由短语（不透传 message，避免回显请求行原文）
            self.send_header("Content-Type", ctype)
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Connection", "close")
            self.end_headers()
            if self.command != "HEAD" and self.request_version != "HTTP/0.9":
                self.wfile.write(body)
            self._response_sent = True  # R11C-P1-1：send_error 路径同样置位
        except Exception:
            pass

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

    def _send(self, code: int, body: bytes, ctype: str | None, extra: dict | None = None,
              api: bool = False) -> bool:
        """发响应（红队 R5C-P3-2/P3-3：压缩协商按 q 值；主域 API 不附 Vary:Origin/
        Cache-Control:no-store——语料 dy 3625 主域 API 响应 cache-control 0 次、xhs
        edith /api/sns 2074 响应两头均 0 次；dy 压缩响应按语料主形态回
        Vary: Accept-Encoding，xhs/ks 不回）。

        红队 R11C-P1-1：返回哨兵 True（旧版无 return 恒 None → _page_alias 命中
        分支返回 None，do_GET `if alias is not None` 判 None 穿透到 _serve_page
        二次发包——同连接追加幻影 404 站点页，连接复用型客户端下一请求必吃到）。
        ctype=None 不发 Content-Type（R11C-P3-3：语料 edith OPTIONS 3,072 例无 CT）。
        红队 R11C-P3-2：204 不发 Content-Length（RFC 7230 §3.3.2 明文 MUST NOT；
        nginx 族同款）。"""
        ce = None
        if code != 204 and body:
            ce = self._pick_content_encoding()
        if ce:
            body = _COMPRESS[ce](body)
            self.send_response(code)
            if ctype:
                self.send_header("Content-Type", ctype)
            if code != 204:
                self.send_header("Content-Length", str(len(body)))
            self.send_header("Content-Encoding", ce)
            if api and self.site == "douyin":
                self.send_header("Vary", "Accept-Encoding")  # dy 语料主形态（2264/3625）
        else:
            self.send_response(code)
            if ctype:
                self.send_header("Content-Type", ctype)
            if code != 204:
                self.send_header("Content-Length", str(len(body)))
        origin = self._allowed_origin()
        extra_keys = {k.lower() for k in (extra or {})}
        # 红队 R7C-P3-4：ks 主域 http/1.1 响应全带 Connection: keep-alive（语料
        # 903/903）；dy/xhs 主域走 h2 无此头（不补）。请求明示 Connection: close
        # （close_connection 已置位）时不声称 keep-alive。
        if self.site == "kuaishou" and not self.close_connection:
            self.send_header("Connection", "keep-alive")
        if origin:  # 不匹配/无 Origin → 不回任何 CORS 头
            self.send_header("Access-Control-Allow-Origin", origin)
            if "access-control-allow-methods" not in extra_keys:  # 调用方已给 ACAM 时不重复
                self.send_header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
            if "access-control-allow-headers" not in extra_keys:  # 调用方已给 ACAH 时不重复
                self.send_header("Access-Control-Allow-Headers", "Content-Type, X-S, X-T")
        for k, v in (extra or {}).items():
            self.send_header(k, v)
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)
        self._response_sent = True  # R11C-P1-1：本请求已有响应落盘（_serve_page 防御用）
        return True

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
        """api=True 时补站级头集 + 真站 Content-Type 形态（P2-D4/P2-X2）。

        红队 R8C-P3-1：序列化全面紧凑化（separators=(",",":")）——语料三站主域
        API JSON 体 7,580/7,580 无空格分隔（唯一 spaced 的 240 个为 vmok-manifest
        静态文件）；旧版 Python 默认（", "/": "）是单正则可辨的最宽格式指纹。
        红队 R9A-P3-1：序列化前媒体 URL 值形态规整（见 media_url_pass 注，盐=
        "api:<路径>"——R10A-P3-4 跨端点按次签发）。
        红队 R10A-P3-1：dy 出口 &<> / U+2028 转义表；ks REST /rest/v/* 出口
        emoji 代理对转义（序列化后套表，不触碰结构位/紧凑分隔）。
        红队 R11A-P3-1：响应体尾换行按站/端点族分流——xhs 全 JSON 面（语料
        3,949/3,949 体以 \n 结尾）与 ks graphql（295/295）追加 \n；dy 主域与
        ks REST（逐验无尾换行）不追加（size==byte_len 字节级口径）。"""
        obj = media_url_pass(obj, "api:" + self.path.split("?", 1)[0])
        text = json.dumps(obj, ensure_ascii=False, separators=(",", ":"))
        text = wire_json_escape(self.site, self.path, text)
        path0 = self.path.split("?", 1)[0]
        if self.site == "xhs" or \
                (self.site == "kuaishou" and path0.rstrip("/") == "/graphql"):
            text += "\n"  # R11A-P3-1：语料 xhs/ks-graphql 体末 \n
        extra = _site_api_headers(self.site, obj) if api else None
        ctype = "application/json; charset=utf-8"
        if api and self.site == "douyin":
            p = path0.rstrip("/")
            if any(p.startswith(q.rstrip("/")) for q in _DY_NO_CHARSET_PATHS):
                ctype = "application/json"  # 真站 stream/detail/comment/search_item 无 charset
        elif api and self.site == "kuaishou":
            if path0.rstrip("/") == "/graphql":
                ctype = "application/json"  # R11C-P3-6：语料 295/295 无 charset
            else:
                ctype = "application/json;charset=UTF-8"  # 真站无空格大写形态（R3-P3-5）
        return self._send(code, text.encode("utf-8"), ctype, extra, api=api)

    def _read_chunked_body(self) -> bytes:
        """红队 R9C-P3-1：Transfer-Encoding: chunked 请求体逐帧解码（RFC 7230 §4.1）。

        读至 0 长度帧（含 trailer 行）；chunk-ext 忽略；8MB 上限防钉连接。
        旧版仅按 Content-Length 读体——合法 chunked 请求的帧字节全部滞留不读：
        体被静默丢弃（同 body 用 CL 发送则正常出数）且 keep-alive 连接被滞留
        字节污染（紧随其后的合法请求得 400）。"""
        out = bytearray()
        try:
            while len(out) <= 8 * 1024 * 1024:
                line = self.rfile.readline(65536)
                if not line:
                    break
                try:
                    sz = int(line.strip().split(b";")[0] or b"0", 16)
                except ValueError:
                    break
                if sz <= 0:
                    while True:  # trailer 至空行
                        t = self.rfile.readline(65536)
                        if not t or t in (b"\r\n", b"\n"):
                            break
                    break
                out += self.rfile.read(sz)
                self.rfile.read(2)  # 帧尾 CRLF
        except Exception:
            pass
        return bytes(out)

    def _read_body(self) -> dict:
        te = (self.headers.get("Transfer-Encoding") or "").strip().lower()
        if "chunked" in te:   # R9C-P3-1：TE 末编码 chunked → 按 chunked 帧读体
            raw = self._read_chunked_body()
        else:
            # R10C-P3-1：单头逗号同值 CL（`7,7` / `7, 7`）按单值受理（RFC 7230
            # §3.3.2——R9C-P3-2「值完全一致的重复按单值受理」宣称的形态兑现；
            # 旧版 int('7,7') 直接 ValueError 穿透到 socketserver handle_error，
            # 连接零响应直断）。不同值逗号在 parse_request 第④入口已 400，此处
            # 兜底再拦（ValueError 由 do_POST 的站点 400 族接住，绝不无响应断连）。
            vals = [p.strip() for p in
                    str(self.headers.get("Content-Length") or "").split(",") if p.strip()]
            uniq = sorted(set(vals))
            if len(uniq) > 1:
                raise ValueError("dup-cl: %r" % ",".join(uniq))
            if not vals:
                return {}
            n = int(uniq[0])
            if n <= 0:
                return {}
            raw = self.rfile.read(n)
        try:
            return json.loads(raw.decode("utf-8"))
        except Exception:
            pass
        # 红队 round 6A C-2：dy multi/aweme/detail 语料调用形为
        # application/x-www-form-urlencoded（aweme_ids=%5B…%5D&origin_type=…）——
        # JSON 解析失败时按 form 体取值（值保持字符串，调用方自行容错），
        # 其余非法形态仍回 {}（R5C-P2-1 容错口径不变）。
        try:
            form = parse_qs(raw.decode("utf-8"))
            if form and any(k.strip() for k in form):
                return {k: v[0] if len(v) == 1 else v for k, v in form.items()}
        except Exception:
            pass
        return {}

    def _drain_request_body(self):
        """红队 R10C-P3-2：GET/HEAD 携带请求体时排空再复用连接（nginx discard 语义）。

        旧版 do_GET 全程不读体：`GET <API>` + `Content-Length: 5` + body 的 5 字节
        滞留 rfile，同一 keep-alive 连接紧随的合法请求 request line 被残留字节破坏
        （读成 body 首词 → 501 站点页，三站全复现）。语料 68,405 请求 0 例
        GET+body（浏览器不发），实现指纹面修复——对无 body 方法在连接复用前
        丢弃请求体（RFC 7230 §3.3 允许 GET 带体；nginx 族 ngx_http_discard_
        request_body 同款）。HEAD 经 do_HEAD→do_GET 同路；chunked 体读尽帧链。
        """
        try:
            te = (self.headers.get("Transfer-Encoding") or "").strip().lower()
            if "chunked" in te:
                self._read_chunked_body()
                return
            vals = [p.strip() for p in
                    str(self.headers.get("Content-Length") or "").split(",") if p.strip()]
            uniq = sorted(set(vals))
            if len(uniq) > 1 or not uniq:
                return  # 畸形 CL：parse_request 前置 400 已关连接，无体可排
            n = int(uniq[0])
            while n > 0:
                chunk = self.rfile.read(min(n, 65536))
                if not chunk:
                    break
                n -= len(chunk)
        except Exception:
            pass

    def do_OPTIONS(self):
        # 红队 R5C-P3-4：xhs 同源 OPTIONS 真站形态 = 200 + ACAM 五方法、无 Allow 头
        # （edith /api/sns 语料 3,074 次同源 OPTIONS，4 样本同形）；dy/ks 主域同源
        # OPTIONS 语料 0 次（无真值）——维持 204 + Allow 形态（R3-P3-5）。
        # 红队 R11C-P3-1：OPTIONS 携带请求体时排空再复用连接（R10C-P3-2 只挂
        # do_GET/do_HEAD，本入口同款——无 body 方法族排空覆盖面补齐）。
        self._drain_request_body()
        if self.site == "xhs":
            # 红队 R11C-P3-3：头集全量对齐语料 edith 3,072/3,072——无 Content-Type、
            # CL:0、ACAM 五方法、ACAC:true、ACMA:7200、ACAH 六项列表（旧版多 CT、
            # 缺 ACAC/ACMA、ACAH 为通用三项）。
            return self._send(200, b"", None, {
                "access-control-allow-methods": "POST, GET, OPTIONS, PUT, DELETE",
                "access-control-allow-credentials": "true",
                "access-control-max-age": "7200",
                "access-control-allow-headers":
                    "content-type,x-b3-traceid,x-s,x-s-common,x-t,x-xray-traceid",
            })
        # 红队 R11C-P3-2：204 不携带 Content-Length（RFC 7230 §3.3.2 MUST NOT）
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
        self._drain_request_body()   # R10C-P3-2：GET/HEAD 带体请求先排空（keep-alive 零污染）
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
        # R8C-P3-1：SSR state 注入同紧凑分隔符（与 API 体线格式一致）；
        # R9A-P3-1：state 内媒体 URL 同过值形态规整（盐="ssr:<骨架名>"，与 API
        # 面跨端点必异——R10A-P3-4）；R10A-P3-1：dy 内嵌 JSON 同转义表（语料 dy
        # 页面内嵌 JSON 体 \u0026 族实证；ks/xhs SSR 保持原生）
        payload = wire_json_escape(
            self.site, fname,
            json.dumps(media_url_pass(state_obj, "ssr:" + fname),
                       ensure_ascii=False, separators=(",", ":")))
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
            da = author_rec.get("author") or {}
            author["followers"] = da.get("follower_count")
            # 红队 R11B-P3-6：作者页 header 统计块显示字段补齐（数据面
            # follower_count/total_favorited 在；following_count/抖音号/IP 属地
            # 语料显示面真值，数据集无源——显示层确定性派生，同请求恒定）
            author["favorited"] = da.get("total_favorited")
            h = stable_hash("dy-prof::%s" % author_key)
            author["following"] = 1 + h % 480
            author["number"] = str(10 ** 9 + stable_hash("dy-no::%s" % author_key)
                                   % 9 * 10 ** 8 + h % 10 ** 8)
            author["ip_location"] = (
                "广东", "河南", "河北", "四川", "山东", "江苏", "浙江", "湖北",
                "湖南", "福建", "陕西", "安徽", "上海", "北京", "重庆", "辽宁"
            )[stable_hash("dy-ip::%s" % author_key) % 16]
        elif self.site == "xhs":
            # 红队 R11B-P3-6：xhs 作者页 header「关注 N 粉丝 M 获赞与收藏」
            # （语料显示面真值；xhs user 实体无计数字段——显示层确定性派生）
            author["following"] = 5 + stable_hash("xhs-fo::%s" % author_key) % 1900
            author["fans"] = 80 + stable_hash("xhs-fans::%s" % author_key) % 46000
            author["likes"] = 300 + stable_hash("xhs-lk::%s" % author_key) % 970000
        elif self.site == "kuaishou":
            # 红队 R11B-P3-6：ks 作者页「快手号」+ 统计块（数据集 author 无计数
            # 源，显示层确定性派生）
            author["kwai_number"] = str(10 ** 9 + stable_hash("ks-no::%s" % author_key) % 9 * 10 ** 8
                                        + stable_hash("ks-no2::%s" % author_key) % 10 ** 8)
            author["following"] = 1 + stable_hash("ks-fo::%s" % author_key) % 900
            author["followers"] = 100 + stable_hash("ks-fans::%s" % author_key) % 900000
            author["favorited"] = 500 + stable_hash("ks-lk::%s" % author_key) % 9500000
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
            if path == "/":
                # 红队 R12C-P3-2：语料 25/25 `/` → 302 /explore（text/html;
                # charset=utf-8，CL:47 带体小页——302 带体为 R12C-P3-3 语料全量
                # 形态 20/20；首页本体挂 /explore 不变，跳转后内容等价）
                return self._send(302, _XHS_ROOT_302_BODY, "text/html; charset=utf-8",
                                  {"Location": "/explore"})
            if _XHS_SEARCH_RESULT_RE.match(path):
                # 红队 R6B-P3-2（SSR 侧）：?keyword= 直开时静态 <title> 注入关键词态
                # （与 doSearch 运行时同口径，消「fetch 型 agent 读原始 HTML 只见
                # 品牌串」的观察 3 之 xhs 部分）
                kw = (params.get("keyword") or [None])[0]
                if kw:
                    return self._serve_state_page(
                        "search.html", {}, title="%s - 小红书搜索" % kw)
                return self._serve_file("search.html")
            m = _XHS_SEARCH_RESULT_NOTE_RE.match(path)
            if m:  # R5B-P2-4：真站搜索卡 href=/search_result/<id>（模态承载）——
                # 不存在 id 与 /explore/<id> 同走 302 /404；存在则搜索页 + 笔记 SSR
                item_id = m.group("id")
                line_no = state.line_no_of(item_id)
                if line_no is None:
                    return self._xhs_note_302(path)
                entity = state.dataset().read(line_no)
                # 红队 R6B-P3-2：直开/刷新 /search_result/<id> 的 SSR title =
                # 笔记标题形态（语料 sample_0011 开笔记态 DOM title 实证），
                # 页面 JS 开模态后同口径覆写运行时 title
                title = "%s - %s" % ((((entity.get("note_card") or {})
                                       .get("display_title")) or "").strip()
                                     or "小红书笔记", _BRAND_NAME["xhs"])
                # 红队 R7B-P3-3：背景列表词按笔记实体派生注入（URL 无 keyword 参数，
                # 不再回退默认词——URL↔列表↔title 三者语义一致）
                return self._serve_state_page(
                    "search.html",
                    {"entity": entity, "keyword": _xhs_note_bg_keyword(entity)},
                    title=title)
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
        if path in ("/new-reco", "/new-reco/"):
            # 红队 R12C-P3-4：语料 20/20 `/new-reco` = ks PC 首页真路径名
            # （200 text/html; charset=utf-8）；旧版仅挂 `/` → 404
            return self._serve_file("home.html")
        if path in ("/404", "/404/"):
            # 红队 R12C-P3-5：语料 78/78 `/404` → 200 状态 404 页（ks 旅程的
            # 404 落点直达形态；xhs 同为 200）；旧版折成 page-miss 404
            return self._serve_file("404.html")
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
        """真站实证（live）：302 → /404?source=/404/sec_…&error_code=300031。
        R12C-P3-3：302 带体——语料 20/20 全部 CL>0（笔记 404 落点 302 携完整
        404 页，84,924B 样本在档）；旧版发空体 CL:0 为整族形态断裂。"""
        src = "/404/sec_pVswaPpO?redirectPath=%s" % quote(
            "https://www.xiaohongshu.com%s" % path, safe="")
        loc = "/404?source=%s&error_code=300031&error_msg=%s&uuid=%s" % (
            quote(src, safe=""), quote("当前笔记暂时无法浏览", safe=""),
            _hex(8) + "-" + _hex(4) + "-4" + _hex(3) + "-a" + _hex(3) + "-" + _hex(12))
        body = b""
        f = _CFG["pages"] / self.site / "404.html"
        if f.is_file():
            body = f.read_bytes()   # 302 落点页整页随体（语料形态）
        return self._send(302, body, "text/html; charset=utf-8", {"Location": loc})

    def do_POST(self):
        state = STATES[self.site]
        parsed = urlparse(self.path)
        path, params = parsed.path, parse_qs(parsed.query)
        bad = path_reject_reason(self._request_target()) or path_reject_reason(path)
        if bad:
            return self._api_or_page_miss("POST", path)
        try:
            body = self._read_body()
        except ValueError:
            # 红队 R10C-P3-1：读体入口的 ValueError（非数字 CL、逗号不同值等）
            # → 站点 400 族有响应（旧版异常穿透 socketserver handle_error，
            # 连接零响应直断；nginx/Go 族或合并受理或 400，均必有响应）
            return self.send_error(400)
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
        # 红队 R11C-P1-1 防御：本请求已有响应落盘（别名命中/前置 400 等）时不再
        # 二次发包——杜绝同连接幻影响应（一请求至多一响应的协议公理）
        if getattr(self, "_response_sent", False):
            return True
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
        if fname == "404.html" and self.site not in ("xhs", "kuaishou"):
            # 404 页 xhs/ks 生成（真站形态；ks /404 落点 200——R12C-P3-5，别名
            # 层已接，此处兜底放宽）；dy 语料 0 样本，维持 page-miss 形态
            return self._page_miss()
        if fname == "search.html" and self.site == "kuaishou" and path.rstrip("/") == "/search":
            # 红队 R3-P1-2（live 反向验证）：快手真站无 /search（404），搜索页在 /search/video
            return self._page_miss()
        return self._serve_file(fname)


def make_handler(site: str):
    return type("Handler_%s" % site, (SynthHandler,),
                {"site": site, "server_version": SITE_SERVER[site]})


class SynthHTTPServer(ThreadingHTTPServer):
    """红队 R6C-P3-3：listen backlog 提升到 512。

    socketserver 默认 request_queue_size=5（Windows 上 listen backlog 受限更明显），
    50 并发连接实测 25/50 被 ConnectionRefusedError 10061 拒绝（真站形态：语料
    68,405 请求 0 拒连、0 ratelimit 头）——线程模型本身可承载（ThreadingHTTPServer
    一连接一线程），瓶颈只在 accept 队列；调大 backlog 后 50 并发 0 拒绝。
    """

    request_queue_size = 512
    daemon_threads = True


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
        httpd = SynthHTTPServer((args.bind, port), make_handler(site))
        httpd.daemon_threads = True
        servers.append((site, port, httpd, "primary"))
        if i < len(compat):
            cport = compat[i]
            if cport == port:
                continue
            if port_free(cport):
                ch = SynthHTTPServer((args.bind, cport), make_handler(site))
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
