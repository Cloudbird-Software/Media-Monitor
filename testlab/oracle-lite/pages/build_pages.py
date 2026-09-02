# -*- coding: utf-8 -*-
"""build_pages —— 从脱敏录制 DOM 模板 + 契约字段映射生成三站合成站页面骨架。

产物：pages/out/<site>/{home,search,search_user,detail,profile}.html（synth_api 托管）。

结构参考 sanitized_corpus/<site>/<scenario>/sample_0001/page_dom.html 的关键容器
（搜索框 / 卡片容器 / 卡片 / 标题 / 点赞数），数据由页面 JS fetch 本地 synth_api
填充——不追求像素级，追求 harness 任务可走的 DOM 路径。

生成时对脱敏 DOM 做选择器存在性校验（卡片类名、搜索框特征），把「结构来源」
写进产物 HTML 的注释里；契约字段映射（响应 JSON 路径 → 卡片字段）按
oracle/contracts/ 实证路径硬编码在 SITE_CONFIG。

红队第 5 轮（5B）页面修复：
  - P2-1 详情页评论翻页：三站滚动续载 +「加载更多评论」按钮（dy cursor 数值链 /
    xhs cursor id 链 / ks pcursor 链），面板计数用 total 声称值（不再=已加载条数）；
  - P2-2 楼中楼 UI：「展开N条回复」（dy/xhs）/「查看更多回复」（ks）控件接线
    reply/sub 端点（API 早已就绪，纯前端接线）；
  - P2-3 搜索 tab 层：ks 视频/用户 tab + /search/user 页（并发 /rest/v/search/user）；
    dy 频道 tab（综合/视频/用户/直播，?type= 分流：video 走 search/item 端点、
    user 走用户卡渲染）；xhs 筛选 tab（综合/地域）；
  - P2-4 xhs：/explore 分类入口（channel tab 层）+ 搜索卡改 /search_result/<id>
    模态形态（点击模态展示、pushState/back 关模态不重拉 page1）；
  - P3-2 title 形态表：dy 搜索静态「发现更多精彩视频 - 抖音搜索」、xhs 搜索
    「<kw> - 小红书搜索」、ks 搜索「<kw> - 快手」；详情/作者页 title 由
    synth_api 按实体注入；
  - P3-3 删「卡片数：N / 已加载 N 条」调试文案；加载失败站点化「加载失败，点击
    重试」（状态行可点重试）；P3-5 翻尽后「加载更多」按钮禁用态；
  - P3-8 卡片图补 width/height 属性。
  - P1 附带：dy 作者主页「作品/喜欢」tab（data-e2e=user-work-tab/user-like-tab），
    喜欢 tab 用同人作品确定性轮转（synth_api 注入 like_works）。

运行（纯 stdlib）：
  python pages/build_pages.py
  python pages/build_pages.py --sanitized <dir> --out pages/out
"""
from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
REPLAY = HERE.parent
DEFAULT_SANITIZED = REPLAY / "sanitized_corpus"
DEFAULT_OUT = HERE / "out"

# 每站取一个搜索结果页样本做 DOM 结构对照（脱敏语料，只读）
DOM_SAMPLES = {
    "douyin": "douyin/dy_search_general/sample_0001/page_dom.html",
    "xhs": "xhs/xhs_search_notes/sample_0001/page_dom.html",
    "kuaishou": "kuaishou/ks_search_video/sample_0001/page_dom.html",
}

# 脱敏 DOM 中实证的关键选择器（生成骨架必须携带同款特征，harness 才能走 DOM 路径）
DOM_SELECTORS = {
    "douyin": {
        "搜索框": ('data-e2e="searchbar-input"', 'searchbar-input'),
        "搜索按钮": ('data-e2e="searchbar-button"', 'searchbar-button'),
        "卡片": ('class="search-result-card"', "search-result-card"),
        "瀑布流容器": ('id="waterFallScrollContainer"', "waterFallScrollContainer"),
    },
    "xhs": {
        "搜索框": ('class="search-input"', "search-input"),
        "卡片": ('class="note-item"', "note-item"),
        "标题": ('class="title"', "title"),
        "feed 容器": ('id="exploreFeeds"', "exploreFeeds"),
    },
    "kuaishou": {
        "搜索框": ('class="input-mini"', "input-mini"),
        "卡片": ('class="card-container"', "card-container"),
        "卡片列表": ('class="cards"', "cards"),
    },
}

DEFAULT_KEYWORDS = {"douyin": "美食教程", "xhs": "美食探店", "kuaishou": "美食探店"}
# 红队 R3-P1-3：页面 title 对齐真站品牌形态（不再出现「合成站」字样）
SITE_TITLES = {"douyin": "抖音 - 记录美好生活", "xhs": "小红书 - 标记我的生活",
               "kuaishou": "快手-看见每一种生活"}
SITE_BRANDS = {"douyin": "抖音", "xhs": "小红书", "kuaishou": "快手"}
# 红队 R5B-P3-2 title 形态表（对照语料/真站）：
#   dy 搜索=静态品牌串（「发现更多精彩视频 - 抖音搜索」）；xhs 搜索=<kw> - 小红书搜索；
#   ks 搜索=<kw> - 快手（live 复核一致）；首页/explore=品牌串。doSearch 仅 xhs/ks 改写。
SITE_SEARCH_TITLES = {"douyin": "发现更多精彩视频 - 抖音搜索",
                      "xhs": "__KW__ - 小红书搜索",
                      "kuaishou": "__KW__ - 快手"}
# 真站搜索页 URL 约定（红队 R3-P3-4：首页搜索改导航式，URL 同步真站）
SITE_SEARCH_URL = {
    "douyin": "'/search/'+enc(kw)+'?type=general'",
    "xhs": "'/search_result?keyword='+enc(kw)",
    "kuaishou": "'/search/video?searchKey='+enc(kw)",
}
# 卡片/详情真站 URL 约定（红队 R3-P1-2/R3-P2-3 + R5B-P2-4：xhs 搜索卡改
# /search_result/<id>?xsec_token=<tok> 模态承载形态，语料搜索页卡 href 实证）
SITE_DETAIL_URL = {
    "douyin": "'/video/'+enc(f.id)",
    "xhs": "('/search_result/'+enc(f.id)+'?xsec_token='+enc(f.xsec||'')+'&xsec_source=')",
    "kuaishou": "'/short-video/'+enc(f.id)",
}# 站内封面占位路径（红队 R3-P1-3：对齐各站 CDN 对象路径形态；本机同源 SVG）
SITE_COVER_PREFIX = {"douyin": "/obj/tos-cn-i-0813/", "xhs": "/sns-webpic/",
                     "kuaishou": "/kos/nlav111422/pc-vision/"}


# ---------------------------------------------------------------------------
# 契约字段映射（响应 JSON 路径 → 页面字段；来源 oracle/contracts/ 实证子树）
# ---------------------------------------------------------------------------
SITE_CONFIG = {
    "douyin": {
        "first_url": '"/aweme/v1/web/general/search/stream/?keyword="+enc(kw)+"&offset=0&count="+PS',
        "next_url": ('"/aweme/v1/web/general/search/single/?keyword="+enc(kw)'
                     '+"&offset="+state.cursor+"&count="+PS+"&search_id="+state.searchId'),
        "method": "GET",
        "items": "(resp.data||[]).map(function(d){return d.aweme_info;})",
        "has_more": "resp.has_more",
        # 红队 R2-P1-1 修复：after_* 必须推进 state.page——旧版只更新 cursor/searchId，
        # fetchPage 恒判 state.page===1 → 「加载更多」永远重复请求 stream?offset=0 首页。
        "after_first": ("state.cursor = resp.cursor; state.searchId = resp.extra.logid; "
                        "state.page = 2;"),
        "after_next": ("state.cursor = resp.cursor; state.page += 1; "
                       "if(resp.extra && resp.extra.logid)"
                       "{ state.searchId = resp.extra.logid; }"),
        "fields": ("function fields(a){return {"
                   "id:a.aweme_id, title:a.desc||'(无描述)', "
                   "like:a.statistics.digg_count, play:a.statistics.play_count, "
                   "comment:a.statistics.comment_count, "
                   "author:a.author&&a.author.nickname||'', "
                   "sec:(a.author&&a.author.sec_uid)||'', "
                   "sig:(a.author&&a.author.signature)||'', "
                   "fans:(a.author&&a.author.follower_count)||0, "
                   "verified:!!(a.author&&a.author.custom_verify), "
                   "dur:a.video&&a.video.duration||0};}"),
    },
    "xhs": {
        "first_url": "API_NOTES",
        "next_url": "API_NOTES",
        "method": "POST",
        "items": ("(resp.data.items||[]).filter(function(it){return it.model_type==='note';})"
                  ".map(function(it){return {note:it.note_card, id:it.id, "
                  "xsec:it.xsec_token||''};})"),
        "has_more": "resp.data.has_more",
        "after_first": "state.page = 2;",
        "after_next": "state.page += 1;",
        "fields": ("function fields(w){var n=w.note;var ii=n.interact_info||{};return {"
                   "id:w.id, title:n.display_title||'(无标题)', "
                   "like:str(ii.liked_count), play:'', comment:str(ii.comment_count), "
                   "collect:str(ii.collected_count), "
                   "author:(n.user&&n.user.nickname)||'', "
                   "uid:(n.user&&n.user.user_id)||'', "
                   "images:(n.image_list||[]).length, "
                   "xsec:w.xsec, dur:0};}"),
        "body": ("var body={keyword:kw, page:state.page, page_size:PS, "
                 "search_id:state.searchId||'', sort:'general', note_type:0, "
                 "ext_flags:[], geo:'', image_formats:['jpg','webp','avif'], "
                 "message_id:'sending', session_id:uuid(), "
                 "filters:[{tags:['time_descending'],type:'sort_type'},"
                 "{tags:['不限'],type:'filter_note_type'}]};"
                 "// 12 必填项全量（含 filters，QA P3-R2-1 兜底口径）"),
    },
    "kuaishou": {
        "first_url": "API_FEED",
        "next_url": "API_FEED",
        "method": "POST",
        "items": "resp.feeds",
        "has_more": ("resp.feeds && resp.feeds.length > 0 && resp.pcursor "
                     "&& resp.pcursor !== 'no_more'"),
        "after_first": "state.pcursor = resp.pcursor; state.session = resp.searchSessionId;",
        "after_next": "state.pcursor = resp.pcursor; state.session = resp.searchSessionId;",
        "fields": ("function fields(f){var p=f.photo||{};return {"
                   "id:p.id, title:p.caption||'(无描述)', "
                   "like:p.likeCount, play:p.viewCount, "
                   "comment:(f.comment&&f.comment.us_c)||0, "
                   "author:(f.author&&f.author.name)||'', "
                   "dur:p.duration||0};}"),
        "body": ("var body={keyword:kw, page:'search', pcursor:state.pcursor||'', "
                 "searchSessionId:state.session||'', webPageArea:''};"),
    },
}

CSS = """
:root{--bg:#f5f6f7;--card:#fff;--txt:#222;--sub:#888;--brand:#fe2c55}
*{box-sizing:border-box;margin:0;padding:0}
body{font:14px/1.6 -apple-system,"Segoe UI","Microsoft YaHei",sans-serif;background:var(--bg);color:var(--txt)}
header.site-header{display:flex;align-items:center;gap:16px;padding:14px 28px;background:#fff;border-bottom:1px solid #e8e8e8;position:sticky;top:0;z-index:9}
.brand{font-size:20px;font-weight:700;color:var(--brand)}
.nav a{margin-right:14px;color:#555;text-decoration:none}
.searchbar{display:flex;flex-items:center;gap:8px;flex:1;max-width:560px}
.searchbar .input-box{flex:1;border:1px solid #ddd;border-radius:18px;padding:8px 16px;background:#fafafa;display:flex;align-items:center;gap:8px}
.searchbar input{border:0;outline:0;background:transparent;width:100%;font-size:14px}
.searchbar .search-button{border:0;background:var(--brand);color:#fff;border-radius:18px;padding:8px 22px;cursor:pointer}
main{max-width:1180px;margin:18px auto;padding:0 20px}
.page-desc{color:var(--sub);margin:6px 2px 14px;font-size:12px}
.cards{display:flex;flex-wrap:wrap;gap:14px;list-style:none}
.cards .search-result-card,.cards .note-item,.cards .card-container{width:216px;background:var(--card);border-radius:10px;overflow:hidden;box-shadow:0 1px 4px rgba(0,0,0,.08);transition:transform .15s}
.cards>*:hover{transform:translateY(-3px)}
.card-link{display:block;text-decoration:none;color:inherit}
.cover{position:relative;background:#eee}
.cover img{width:100%;display:block;aspect-ratio:3/4;object-fit:cover}
.cover .duration{position:absolute;right:6px;bottom:6px;background:rgba(0,0,0,.6);color:#fff;font-size:11px;padding:1px 6px;border-radius:4px}
.card-info{padding:8px 10px 10px}
.card-info .title{font-size:13px;line-height:1.45;height:2.9em;overflow:hidden;display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical}
.card-info .author-wrapper{color:var(--sub);font-size:12px;margin-top:6px;display:flex;align-items:center;gap:6px}
.metrics{display:flex;gap:12px;color:var(--sub);font-size:12px;margin-top:6px}
.metrics .like-count{color:#e02040}
.toolbar{display:flex;gap:10px;align-items:center;margin:4px 0 14px}
.toolbar button.load-more{border:1px solid #ddd;background:#fff;border-radius:16px;padding:6px 18px;cursor:pointer}
.toolbar button.load-more:disabled{color:#bbb;cursor:default;border-color:#eee}
.toolbar .status{color:var(--sub);font-size:12px}
.toolbar .status.retry{color:var(--brand);cursor:pointer;text-decoration:underline}
.tabs{display:flex;align-items:center;gap:22px;margin:2px 0 14px;border-bottom:1px solid #eee;background:var(--card);border-radius:10px 10px 0 0;padding:0 14px}
.tabs .tab-item{position:relative;padding:10px 2px;color:#555;cursor:pointer;font-size:14px}
.tabs .tab-item.active{color:var(--brand);font-weight:600}
.tabs .tab-item-border{position:absolute;bottom:-1px;left:0;right:0;height:2px;background:var(--brand);border-radius:2px}
.filter-tabs{display:flex;gap:10px;margin:2px 0 14px;flex-wrap:wrap}
.filter-tabs .tab{border:1px solid #e8e8e8;background:#fff;border-radius:14px;padding:4px 14px;font-size:12px;color:#666;cursor:pointer}
.filter-tabs .tab.active{color:var(--brand);border-color:var(--brand)}
.channel-list{display:flex;gap:18px;margin:2px 0 14px;list-style:none;flex-wrap:wrap;padding:6px 2px}
.channel-list li.channel a{color:#666;text-decoration:none;font-size:13px;padding:6px 4px;display:inline-block}
.channel-list li.channel a.active{color:var(--brand);font-weight:600;border-bottom:2px solid var(--brand)}
.detail-wrap{display:flex;gap:26px;background:#fff;border-radius:12px;padding:22px;margin-top:6px}
.detail-cover{width:340px;border-radius:10px;overflow:hidden;flex:none}
.detail-cover img{width:100%;display:block}
.detail-main{flex:1}
.detail-main h1.title{font-size:20px;line-height:1.5;margin-bottom:10px}
.detail-main .author-wrapper{color:#666;margin-bottom:12px;text-decoration:none;display:inline-block}
.detail-main .metrics{font-size:15px;color:#444;display:flex;gap:22px;margin:8px 0 14px}
.detail-main .metrics .like-count{color:#e02040;font-weight:600}
.comments{margin-top:22px}
.comments h3{margin:10px 0}
.comments .comments-count{color:var(--sub);font-size:12px;margin:2px 0 10px}
.comment-item{background:#fff;border-radius:10px;padding:10px 14px;margin-bottom:10px}
.comment-item .c-user{color:var(--sub);font-size:12px}
.comment-item .c-content{margin-top:2px;white-space:pre-wrap}
.comment-item .c-like{color:var(--sub);font-size:12px;margin-top:4px}
.sub-comments{margin:8px 0 4px 18px;padding-left:12px;border-left:2px solid #f0f0f0}
.sub-comments .comment-item{background:#fafafa;padding:8px 12px}
button.expand-replies{border:0;background:none;color:#1c6ed8;font-size:12px;cursor:pointer;padding:6px 0;margin-top:2px}
button.load-more-comments{border:1px solid #ddd;background:#fff;border-radius:16px;padding:6px 18px;cursor:pointer;margin:8px 0}
button.load-more-comments:disabled{color:#bbb;cursor:default;border-color:#eee}
.user-card{width:100%;background:var(--card);border-radius:10px;padding:14px;display:flex;gap:14px;align-items:center;box-shadow:0 1px 4px rgba(0,0,0,.06);text-decoration:none;color:inherit;margin-bottom:12px}
.user-card .u-avatar{width:64px;height:64px;border-radius:50%;object-fit:cover;background:#eee}
.user-card .u-name{font-size:16px;font-weight:600}
.user-card .u-meta{color:var(--sub);font-size:12px;margin-top:4px}
.user-cards{list-style:none;display:block}
footer{color:#aaa;text-align:center;padding:26px 0;font-size:12px}
.note-modal-mask{position:fixed;inset:0;background:rgba(0,0,0,.45);z-index:50;display:flex;align-items:flex-start;justify-content:center;overflow:auto;padding:4vh 0}
.note-modal{background:#fff;border-radius:14px;max-width:920px;width:92%;display:flex;overflow:hidden;position:relative}
.note-modal .m-media{width:420px;flex:none;overflow:auto;background:#111}
.note-modal .m-media img{width:100%;display:block}
.note-modal .m-main{flex:1;padding:20px 24px;min-width:0}
.note-modal .m-close{position:absolute;right:12px;top:10px;border:0;background:rgba(0,0,0,.05);border-radius:50%;width:30px;height:30px;cursor:pointer;font-size:14px;color:#666}
.note-modal .m-title{font-size:19px;line-height:1.5;margin:6px 30px 10px 0}
.note-modal .m-author{color:#666;font-size:13px;text-decoration:none;display:inline-block;margin-bottom:8px}
.note-modal .m-metrics{display:flex;gap:18px;color:var(--sub);font-size:13px;margin-bottom:10px}
.note-modal .m-metrics .like-count{color:#e02040}
.profile-tabs{display:flex;gap:34px;border-bottom:1px solid #eee;margin:6px 0 16px}
.profile-tabs .p-tab{padding:10px 2px;color:#555;cursor:pointer;font-size:15px;position:relative}
.profile-tabs .p-tab.active{color:var(--brand);font-weight:600}
.profile-tabs .p-tab .p-tab-count{color:var(--sub);font-size:12px;margin-left:4px}
.profile-tabs .p-tab.active:after{content:'';position:absolute;left:0;right:0;bottom:-1px;height:2px;background:var(--brand)}
"""

# 页面 JS：公共库（enc/fmt/coverUrl/el/setStatus/基础 state）
JS_LIB = """
function enc(s){return encodeURIComponent(s||'')}
function str(v){return String(v==null?0:v)}
function uuid(){return 'xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx'.replace(/[xy]/g,function(c){var r=Math.random()*16|0;return (c=='x'?r:(r&0x3|0x8)).toString(16)})}
function fmt(n){n=parseInt(n||0,10);if(isNaN(n))return '0';if(n>=100000000)return (n/100000000).toFixed(1)+'亿';if(n>=10000)return (n/10000).toFixed(1)+'万';return String(n)}
function fmtDur(ms){var s=Math.round((ms||0)/1000);var m=Math.floor(s/60);s=s%60;return m+':'+(s<10?'0':'')+s}
function coverUrl(seed,label,w,h){return '__COVER__'+enc(seed)+'~c5_'+(w||323)+'x'+(h||430)+'?label='+enc(label||'')}
function el(tag,cls,text){var e=document.createElement(tag);if(cls)e.className=cls;if(text!=null)e.textContent=text;return e}
function contEl(){return document.querySelector('.cards');}
/* setStatus 收口到 JS_LIB（原仅 home/search 定义——detail 页调用会 ReferenceError）。
   红队 R5B-P3-3：失败态可点重试（class=retry + role=button，不再渲染 JS 异常原文）。 */
function setStatus(t,retry){var e=document.getElementById('loadStatus');if(!e)return;
  e.textContent=t||'';e.className='status'+(retry?' retry':'');e.setAttribute('role',retry?'button':'');
  if(retry){e.onclick=function(){loadMore&&loadMore();};}else{e.onclick=null;}}
var state={page:1,cursor:0,searchId:'',pcursor:'',session:'',keyword:DEFAULT_KW,loading:false,done:false,seq:0,type:'general'};
var SEARCH_TYPE='general';   /* dy 搜索页按 ?type= 覆写（R5B-P2-3 频道分流） */
""".replace("__COVER__", "__COVER_PREFIX__")

CARD_RENDERERS = {
    "douyin": """
function cardNode(f){
  var li=el('li','search-result-card'); li.setAttribute('data-id',f.id);
  var a=el('a','card-link'); a.href=detailUrl(f);
  var cov=el('div','cover');
  var img=new Image(); img.alt=f.title; img.loading='lazy';
  img.width=323; img.height=430;               /* R5B-P3-8：尺寸属性（CSS 仍控布局） */
  img.src=coverUrl(f.id,f.title.slice(0,2));
  cov.appendChild(img);
  if(f.dur>0){cov.appendChild(el('span','duration',fmtDur(f.dur)))}
  var info=el('div','card-info');
  info.appendChild(el('div','title',f.title));
  var aw=el('div','author-wrapper'); aw.textContent=f.author; info.appendChild(aw);
  var m=el('div','metrics');
  m.appendChild(el('span','like-count','赞 '+fmt(f.like)));
  m.appendChild(el('span','play-count','播放 '+fmt(f.play)));
  m.appendChild(el('span','comment-count','评 '+fmt(f.comment)));
  info.appendChild(m);
  a.appendChild(cov); a.appendChild(info); li.appendChild(a); return li;
}
/* 红队 R5B-P2-3：dy 用户频道卡（?type=user）——作者卡派生自搜索响应作者字段 */
function userCardNode(f){
  var li=el('li','search-result-card user-result-card'); li.setAttribute('data-sec-uid',f.sec||'');
  var a=el('a','user-card'); a.href='/user/'+enc(f.sec||'');
  var img=new Image(); img.alt=f.author; img.loading='lazy'; img.className='u-avatar';
  img.src=coverUrl(f.sec||f.author,f.author.slice(0,2),128,128);
  var box=el('div');
  var nm=el('div','u-name'); nm.textContent=(f.author||'抖音用户')+(f.verified?' ✓':''); box.appendChild(nm);
  var mt=el('div','u-meta');
  mt.textContent='粉丝 '+fmt(f.fans)+(f.sig?(' · '+String(f.sig).slice(0,24)):'');
  box.appendChild(mt);
  a.appendChild(img); a.appendChild(box); li.appendChild(a); return li;
}""",
    "xhs": """
function cardNode(f){
  var sec=el('section','note-item'); sec.setAttribute('data-id',f.id);
  if(f.xsec){sec.setAttribute('data-xsec-token',f.xsec)}
  var a=el('a','card-link'); a.href=detailUrl(f);
  var cov=el('div','cover');
  var img=new Image(); img.alt=f.title; img.loading='lazy';
  img.width=323; img.height=430;
  img.src=coverUrl(f.id,f.title.slice(0,2));
  cov.appendChild(img);
  var foot=el('div','card-info footer-box');
  var t=el('a','title'); t.textContent=f.title; t.href=detailUrl(f); foot.appendChild(t);
  var aw=el('div','author-wrapper'); aw.textContent=f.author; foot.appendChild(aw);
  var m=el('div','metrics like-wrapper');
  m.appendChild(el('span','count like-count','点赞 '+fmt(f.like)));
  m.appendChild(el('span','count comment-count','评论 '+fmt(f.comment)));
  m.appendChild(el('span','count collected-count','收藏 '+fmt(f.collect)));
  foot.appendChild(m);
  a.appendChild(cov); a.appendChild(foot); sec.appendChild(a); return sec;
}""",
    "kuaishou": """
function cardNode(f){
  var d=el('div','card-container video-card'); d.setAttribute('data-id',f.id);
  var a=el('a','card-link'); a.href=detailUrl(f);
  var cov=el('div','cover');
  var img=new Image(); img.alt=f.title; img.loading='lazy';
  img.width=323; img.height=430;
  img.src=coverUrl(f.id,f.title.slice(0,2));
  cov.appendChild(img);
  if(f.dur>0){cov.appendChild(el('span','duration',fmtDur(f.dur*1000)))}
  var info=el('div','card-info');
  info.appendChild(el('div','title video-title',f.title));
  var aw=el('div','author-wrapper'); aw.textContent=f.author; info.appendChild(aw);
  var m=el('div','metrics');
  m.appendChild(el('span','like-count','赞 '+fmt(f.like)));
  m.appendChild(el('span','play-count','看 '+fmt(f.play)));
  m.appendChild(el('span','comment-count','评 '+fmt(f.comment)));
  info.appendChild(m);
  a.appendChild(cov); a.appendChild(info); d.appendChild(a); return d;
}
/* 红队 R5B-P2-3：ks 用户 tab 卡（/search/user 页 + /rest/v/search/user 数据） */
function ksUserCardNode(u){
  var d=el('div','card-container user-result-card'); d.setAttribute('data-user-id',u.user_id||'');
  var a=el('a','user-card'); a.href='/profile/'+enc(u.user_id||'');
  var img=new Image(); img.alt=u.user_name; img.loading='lazy'; img.className='u-avatar';
  img.src=coverUrl(u.user_id||u.user_name,(u.user_name||'').slice(0,2),128,128);
  var box=el('div');
  box.appendChild(el('div','u-name',u.user_name||'快手用户'));
  box.appendChild(el('div','u-meta',u.user_text||''));
  a.appendChild(img); a.appendChild(box); d.appendChild(a); return d;
}""",
}

CONTAINERS = {
    "douyin": '<ul id="waterFallScrollContainer" class="cards waterfall" data-keyword=""></ul>',
    "xhs": '<div id="exploreFeeds" class="feeds-container cards" data-keyword=""></div>',
    "kuaishou": '<div class="cards search-content" data-keyword=""></div>',
}

SEARCH_INPUTS = {
    # 统一 id=searchInput（页面 JS 用），并保留脱敏 DOM 实证的选择器特征（data-e2e / class）
    "douyin": ('<input id="searchInput" data-e2e="searchbar-input" type="text" maxlength="100" '
               'placeholder="搜索你感兴趣的内容" value="{kw}" aria-label="搜索">'),
    "xhs": ('<input id="searchInput" class="search-input" type="text" placeholder="搜索你感兴趣的内容" '
            'value="{kw}" aria-label="搜索">'),
    "kuaishou": ('<input id="searchInput" class="input input-mini" type="text" placeholder="搜索你感兴趣的内容" '
                 'value="{kw}" aria-label="搜索">'),
}

# dy 频道 tab（语料：data-key=general/video/user/live；R5B-P2-3）
DY_CHANNEL_TABS = [("general", "综合"), ("video", "视频"), ("user", "用户"), ("live", "直播")]
# ks 搜索 tab（语料：role=button class=tab-item 视频/用户 + tab-item-border 滑块；R5B-P2-3）
KS_SEARCH_TABS = [("video", "视频", "/search/video?searchKey="), ("user", "用户", "/search/user?searchKey=")]
# xhs 筛选 tab（语料：button.tab aria-details=综合/地域；R5B-P2-3）
XHS_FILTER_TABS = ["综合", "北京", "上海", "广东", "浙江", "江苏", "四川", "湖北"]
# xhs /explore 分类 channel（语料 channel_id=homefeed.*；R5B-P2-4）
XHS_CHANNELS = [("首页", "homefeed.fashion_v3"), ("穿搭", "homefeed.fashion_v3"),
                ("美食", "homefeed.food_v3"), ("影视", "homefeed.movie_and_tv_v3"),
                ("家居", "homefeed.household_product_v3"), ("游戏", "homefeed.gaming_v3"),
                ("旅行", "homefeed.travel_v3"), ("健身", "homefeed.fitness_v3"),
                ("职场", "homefeed.career_v3")]


def build_tabs_html(site: str) -> str:
    """搜索页 tab 层（红队 R5B-P2-3）：dy 频道 tab / ks 视频·用户 tab / xhs 筛选 tab。"""
    if site == "douyin":
        spans = []
        for key, name in DY_CHANNEL_TABS:
            spans.append(
                '<span role="button" class="tab-item%s" data-key="%s" data-type="%s">%s%s</span>'
                % (" active" if key == "general" else "", key, key, name,
                   '<i class="tab-item-border"></i>' if key == "general" else ""))
        return ('<div class="tabs channel-tabs" id="channelTabs" data-e2e="search-channel-tabs">'
                + "".join(spans) + "</div>")
    if site == "kuaishou":
        spans = []
        for key, name, href in KS_SEARCH_TABS:
            spans.append(
                '<span role="button" class="tab-item%s" data-href-prefix="%s">%s%s</span>'
                % (" tab-item-active" if key == "video" else "", href, name,
                   '<i class="tab-item-border"></i>' if key == "video" else ""))
        return '<div class="tabs tab-nav" id="searchTabs">' + "".join(spans) + "</div>"
    # xhs：筛选 tab（综合/地域），aria-details 对齐语料
    btns = ['<button type="button" class="tab%s" aria-details="%s">%s</button>'
            % (" active" if i == 0 else "", name, name)
            for i, name in enumerate(XHS_FILTER_TABS)]
    return '<div class="filter-tabs" id="filterTabs">' + "".join(btns) + "</div>"


def build_channel_html() -> str:
    """xhs /explore 分类入口（channel tab 静态层，R5B-P2-4；href 语料形态）。"""
    lis = ['<li class="channel"><a href="/explore?channel_id=%s"%s>%s</a></li>'
           % (cid, " active" if i == 0 else "", name)
           for i, (name, cid) in enumerate(XHS_CHANNELS)]
    return '<ul class="channel-list" id="channelList">' + "".join(lis) + "</ul>"


def build_js(site: str, page: str) -> str:
    """组装页面 JS：状态机 + fetch 链 + 渲染/交互（home 与 search 共用列表逻辑）。"""
    cfg = SITE_CONFIG[site]
    js = [JS_LIB.replace("DEFAULT_KW", json.dumps(DEFAULT_KEYWORDS[site], ensure_ascii=False))
             .replace("__COVER_PREFIX__", SITE_COVER_PREFIX[site]),
          "var PS=20;", "function detailUrl(f){return %s;}" % SITE_DETAIL_URL[site],
          "var BRAND=%s;" % json.dumps(SITE_BRANDS[site], ensure_ascii=False),
          cfg["fields"]]

    if page in ("home", "search"):
        if cfg["method"] == "GET":
            # R5B-P2-3：dy ?type=video 走 search/item 端点（语料视频频道搜索面）
            if site == "douyin":
                js.append("""
var SEARCH_TYPE=(function(){var q=new URLSearchParams(location.search).get('type');
  return ['general','video','user','live'].indexOf(q)>=0?q:'general';})();
function searchBase(kw){
  if(SEARCH_TYPE==='video'){return {first:'/aweme/v1/web/search/item/?keyword='+enc(state.keyword)+'&offset=0&count='+PS,
    next:'/aweme/v1/web/search/item/?keyword='+enc(state.keyword)+'&offset='+state.cursor+'&count='+PS+'&search_id='+(state.searchId||'')};}
  return {first:FIRST_URL, next:NEXT_URL};
}
function fetchPage(kw){
  var url = state.page===1 ? searchBase(kw).first : searchBase(kw).next;
  return fetch(url,{headers:{'Accept':'application/json'}}).then(function(r){
    if(!r.ok) throw new Error('HTTP '+r.status);
    return r.json();
  });
}""".replace("FIRST_URL", cfg["first_url"]).replace("NEXT_URL", cfg["next_url"]))
            else:
                js.append("""
function fetchPage(kw){
  var url = state.page===1 ? FIRST_URL : NEXT_URL;
  return fetch(url,{headers:{'Accept':'application/json'}}).then(function(r){
    if(!r.ok) throw new Error('HTTP '+r.status);
    return r.json();
  });
}""".replace("FIRST_URL", cfg["first_url"]).replace("NEXT_URL", cfg["next_url"]))
        else:
            js.append(("""
function fetchPage(kw){
  var url = %s;
  BODY_TMPL
  return fetch(url,{method:'POST',headers:{'Content-Type':'application/json','Accept':'application/json'},
    body:JSON.stringify(body)}).then(function(r){
    if(!r.ok) throw new Error('HTTP '+r.status);
    return r.json();
  });
}""" % ("'/api/sns/web/v2/search/notes'" if site == "xhs" else "'/rest/v/search/feed'")
                  ).replace("BODY_TMPL", cfg["body"]))
        js.append(("""
/* R5B-P3-3：删除调试计数文案；R5B-P3-5：翻尽后按钮禁用。 */
function setDone(done){
  state.done=!!done;
  ['loadMoreBtn','loadMoreBtn2'].forEach(function(id){
    var b=document.getElementById(id); if(!b)return; b.disabled=!!done;
  });
}
function renderCards(items, append){
  var cont=contEl();
  if(!append){cont.innerHTML='';}
  cont.setAttribute('data-keyword', state.keyword);
  items.forEach(function(w){
    var f=fields(w);
    if(state.type==='user'&&DY_USER_MODE){cont.appendChild(userCardNode(f));}  /* dy 用户频道 */
    else{cont.appendChild(cardNode(f));}
  });
}
var DY_USER_MODE=__DY_USER_MODE__;
var _seenSec={};   /* dy 用户 tab：按 sec_uid 去重（跨页聚合作者） */
function renderItems(raw, append){
  var cont=contEl();
  if(typeof rememberNotes==='function'){rememberNotes(raw);}  /* xhs：条目数据入模态缓存 */
  if(DY_USER_MODE&&state.type==='user'){
    if(!append){cont.innerHTML='';_seenSec={};}
    raw.forEach(function(a){
      var f=fields(a); if(!f.sec||_seenSec[f.sec])return; _seenSec[f.sec]=1;
      cont.appendChild(userCardNode(f));
    });
    return;
  }
  renderCards(raw, append);
}
function loadMore(){
  if(state.loading||state.done) return;
  var myseq=state.seq;               /* 代际守卫：过期响应（换词瞬间）直接丢弃 */
  state.loading=true; setStatus('加载中…');
  fetchPage(state.keyword).then(function(resp){
    if(myseq!==state.seq){return;}   /* 已发起新搜索，本页作废 */
    var items=ITEMS_EXPR;
    renderItems(items, true);
    var more=HAS_MORE_EXPR;
    AFTER;
    if(!more){setDone(true);setStatus('没有更多了');}
    else setStatus('');
  }).catch(function(e){if(myseq===state.seq)setStatus('加载失败，点击重试',true)})
    .then(function(){if(myseq===state.seq)state.loading=false});
}
function doSearch(kw){
  state={page:1,cursor:0,searchId:'',pcursor:'',session:'',keyword:kw,loading:false,done:false,seq:(state.seq||0)+1,type:SEARCH_TYPE||'general'};
  document.title=TITLE_EXPR;   /* R5B-P3-2 title 形态表：dy 静态、xhs/ks 关键词态 */
  setDone(false);
  var cont=contEl(); cont.innerHTML=''; cont.setAttribute('data-keyword',kw);
  loadMore();
}
/* 红队 R2-P2-6：滚动到底自动加载下一页（真站无限滚动；按钮保留兜底）。
   节流 1200ms + loading/done 守卫；仅视口接近底部时触发。 */
var _scrollTick=0;
window.addEventListener('scroll', function(){
  var doc=document.documentElement;
  var h=Math.max(doc.scrollHeight||0, document.body?document.body.scrollHeight||0:0);
  if(!h) return;
  if(window.scrollY + window.innerHeight <= h - 700) return;
  var now=Date.now();
  if(now-_scrollTick<1200) return;
  _scrollTick=now;
  loadMore();
}, {passive:true});
function initSearch(){
  var form=document.getElementById('searchForm');
  form.addEventListener('submit',function(ev){ev.preventDefault();
    var v=(document.getElementById('searchInput')||document.querySelector('input')).value.trim()||DEFAULT_KW_JS;
    location.href=SEARCH_URL_JS;});
  document.querySelector('.search-button,button[data-e2e="searchbar-button"]').addEventListener('click',function(){
    var v=(document.getElementById('searchInput')||document.querySelector('input')).value.trim()||DEFAULT_KW_JS;
    location.href=SEARCH_URL_JS;});
  var kw0=INIT_KW;
  doSearch(kw0);
}""")
            .replace("ITEMS_EXPR", cfg["items"])
            .replace("HAS_MORE_EXPR", cfg["has_more"])
            .replace("AFTER", cfg["after_next"])
            .replace("SEARCH_URL_JS", SITE_SEARCH_URL[site].replace("enc(kw)", "enc(v)"))
            .replace("TITLE_EXPR",
                     ("TITLE_STATIC_JS" if site == "douyin"
                      else "kw+' - 小红书搜索'" if site == "xhs"
                      else "kw+' - 快手'"))
            .replace("INIT_KW",
                     ("(function(){var q=new URLSearchParams(location.search);"
                      "if(q.has('keyword'))return q.get('keyword');"
                      "if(q.has('searchKey'))return q.get('searchKey');"
                      "var m=location.pathname.match(/^\\/(?:search|jingxuan\\/search)\\/(.+)$/);"
                      "if(m){try{return decodeURIComponent(m[1])}catch(e){return m[1]}}"
                      "return DEFAULT_KW_JS;})()")
                     if page == "search" else "DEFAULT_KW_JS")
            .replace("DEFAULT_KW_JS", json.dumps(DEFAULT_KEYWORDS[site], ensure_ascii=False))
            .replace("__DY_USER_MODE__", "true" if site == "douyin" else "false"))
        # 站点专属 post-init（tab 层激活 / ks 并发 search/user / xhs 模态）
        if site == "douyin":
            js.append(("""
var TITLE_STATIC_JS=__DY_TITLE__;
/* R5B-P2-3：频道 tab 激活态 + 点击分流（type=user 走用户卡渲染、video 走 search/item） */
(function(){
  var t=SEARCH_TYPE||'general';
  var tabs=document.querySelectorAll('#channelTabs .tab-item');
  tabs.forEach(function(x){
    var k=x.getAttribute('data-key')||x.getAttribute('data-type');
    var on=(k===t);
    x.className='tab-item'+(on?' active':'');
    x.addEventListener('click',function(){
      location.href='/search/'+enc(state.keyword)+'?type='+k;
    });
  });
})();""").replace("__DY_TITLE__", json.dumps(
                SITE_SEARCH_TITLES["douyin"], ensure_ascii=False)))
        elif site == "kuaishou":
            js.append("""
/* R5B-P2-3：视频/用户 tab 激活态（用户 tab → /search/user?searchKey=）；
   搜索加载并发 /rest/v/search/user（语料 req_0007：search/feed 与 search/user 同发） */
(function(){
  var tabs=document.querySelectorAll('#searchTabs .tab-item');
  tabs.forEach(function(x){
    x.addEventListener('click',function(){
      location.href=x.getAttribute('data-href-prefix')+enc(state.keyword);
    });
  });
})();
function fetchSearchUsers(kw,pcursor){
  return fetch('/rest/v/search/user',{method:'POST',
    headers:{'Content-Type':'application/json','Accept':'application/json'},
    body:JSON.stringify({keyword:kw,pcursor:pcursor||'',searchSessionId:state.session||'',webPageArea:''})}
  ).then(function(r){return r.ok?r.json():null}).catch(function(){return null});
}
function onSearchUsers(resp){ window.__ksSearchUsers=resp; }""")
        elif site == "xhs":
            js.append("""
/* R5B-P2-3：筛选 tab（综合/地域）激活态切换 */
(function(){
  var tabs=document.querySelectorAll('#filterTabs .tab');
  tabs.forEach(function(x){
    x.addEventListener('click',function(){
      tabs.forEach(function(y){y.className='tab'});
      x.className='tab active';
    });
  });
})();
/* R5B-P2-4：搜索卡 /search_result/<id> 模态承载——点击开模态（pushState），
   back 关模态（popstate），不重拉 page1（无导航 = 无重请求）。 */
var _modalOpen=null;
function closeModal(){
  var m=document.getElementById('noteModalMask');
  if(m)m.parentNode.removeChild(m);
  _modalOpen=null;
}
window.addEventListener('popstate',function(){ closeModal(); });
function noteModalHtml(f){
  var n=f.note||{};
  var mask=el('div','note-modal-mask'); mask.id='noteModalMask';
  var modal=el('div','note-modal'); modal.id='noteModal';
  var media=el('div','m-media');
  ((n.image_list||[]).length?n.image_list:[{}]).forEach(function(im){
    var u='';
    ((im.info_list||[]).forEach(function(info){if(info.image_scene==='WB_DFT')u=info.url||u;}));
    var img=new Image(); img.alt=n.display_title||'';
    img.src=u||coverUrl(f.id,(n.display_title||'').slice(0,2),660,880);
    media.appendChild(img);
  });
  var main=el('div','m-main');
  var close=el('button','m-close','×'); close.setAttribute('aria-label','关闭');
  close.onclick=function(){ if(history.state&&history.state.noteModal){history.back();} else {closeModal();} };
  main.appendChild(close);
  main.appendChild(el('div','m-title',n.display_title||'(无标题)'));
  var au=el('a','m-author',((n.user&&n.user.nickname)||'')+' · 关注');
  au.href='/user/profile/'+enc((n.user&&n.user.user_id)||'');
  main.appendChild(au);
  var ii=n.interact_info||{};
  var m=el('div','m-metrics');
  m.appendChild(el('span','like-count','点赞 '+fmt(ii.liked_count)));
  m.appendChild(el('span','comment-count','评论 '+fmt(ii.comment_count)));
  m.appendChild(el('span','collected-count','收藏 '+fmt(ii.collected_count)));
  main.appendChild(m);
  var cw=el('div','comments');
  cw.appendChild(el('h3',null,'全部评论'));
  var cc=el('div','comments-count','共 <span class="m-cmt-total">'+fmt(ii.comment_count)+'</span> 条评论');
  cw.appendChild(cc);
  var list=el('div','comment-list'); cw.appendChild(list);
  var moreBtn=el('button','load-more-comments','加载更多评论');
  cw.appendChild(moreBtn);
  main.appendChild(cw);
  modal.appendChild(media); modal.appendChild(main); mask.appendChild(modal);
  mask.addEventListener('click',function(ev){if(ev.target===mask)close.onclick();});
  return mask;
}
function openNoteModal(f, fromEntity){
  closeModal();
  var mask=noteModalHtml(f);
  document.body.appendChild(mask);
  _modalOpen=f.id;
  /* 评论翻页链（xhs cursor id 链）+ 楼中楼展开（R5B-P2-1/P2-2 同详情页口径） */
  var mc={cursor:'',done:false,loading:false};
  var list=mask.querySelector('.comment-list');
  var moreBtn=mask.querySelector('.load-more-comments');
  function loadCmts(){
    if(mc.done||mc.loading)return; mc.loading=true; moreBtn.disabled=true;
    fetch('/api/sns/web/v2/comment/page?note_id='+enc(f.id)+'&cursor='+enc(mc.cursor)
      +'&top_comment_id=&image_formats=jpg,webp,avif&xsec_token='+enc(f.xsec||''))
      .then(function(r){return r.ok?r.json():Promise.reject(new Error('HTTP '+r.status))})
      .then(function(resp){
        var d=(resp&&resp.data)||{};
        (d.comments||[]).forEach(function(c){list.appendChild(xhsCommentRow(c,f.id,f.xsec));});
        mc.cursor=d.cursor||''; mc.done=!d.has_more;
        moreBtn.disabled=mc.done; moreBtn.textContent=mc.done?'没有更多评论':'加载更多评论';
        mc.loading=false;
      }).catch(function(){mc.loading=false;moreBtn.disabled=false;});
  }
  moreBtn.onclick=loadCmts;
  loadCmts();
  if(!fromEntity){
    try{ history.pushState({noteModal:1},'',detailUrl(f)); }catch(e){}  /* 协议受限环境降级：仅内存开模态 */
  }
}
/* 卡片点击 → 模态（拦截整页跳转；href 保持真站 /search_result/<id> 形态） */
function bindCardModal(){
  document.addEventListener('click',function(ev){
    var a=ev.target.closest&&ev.target.closest('a.card-link, a.title');
    if(!a)return;
    var m=(a.getAttribute('href')||'').match(/^\\/search_result\\/([0-9a-fA-F]+)/);
    if(!m)return;
    var id=m[1];
    var sec=a.closest('[data-id]');
    var f=window.__noteCache&&window.__noteCache[id];
    if(f){ev.preventDefault(); openNoteModal(f,false); }
  });
}
window.__noteCache={};
function rememberNotes(raw){raw.forEach(function(w){if(w&&w.id)window.__noteCache[w.id]=w;});}
/* 直开 /search_result/<id>（synth_api SSR entity）→ 搜索列表加载后自动开模态 */
function maybeAutoOpenModal(){
  var m=location.pathname.match(/^\\/search_result\\/([0-9a-fA-F]+)$/);
  var st=window.__INITIAL_STATE__;
  if(!m||!(st&&st.entity))return;
  var e=st.entity, nc=e.note_card||{};
  var f={id:e.id, xsec:(nc.xsec_token)||'', note:nc};
  openNoteModal(f,true);
}""")
    else:  # detail
        # 红队 P2-D6/P3-4 + R3-P1-2：id 同时支持 ?id= 与真站路径形态 /video/<id>（dy）、
        # /explore/<id>（xhs）、/short-video/<id>（ks）
        id_pat = ('/^\\/video\\/([0-9A-Za-z_-]+)$/' if site == "douyin"
                  else '/^\\/explore\\/([0-9a-fA-F]+)$/' if site == "xhs"
                  else '/^\\/short-video\\/([0-9A-Za-z_-]+)$/')
        if site == "douyin":
            # dy 详情数据走真路径 XHR /aweme/detail（语料实证：详情页网络即此端点）
            # R5B-P2-1/P2-2：评论面板滚动/按钮续载（cursor 链）+ 展开N条回复（reply 端点）
            detail_fetch = """
  var id=detailId();
  if(id){
    fetch('/aweme/v1/web/aweme/detail/?aweme_id='+enc(id)+'&aid=6383&version_code=170400'
      +'&device_platform=webapp&channel=channel_pc_web')
      .then(function(r){return r.ok?r.json():Promise.reject(new Error('HTTP '+r.status))})
      .then(function(j){ if(j&&j.aweme_detail){renderDetail(j.aweme_detail);} else {renderDetailError();} })
      .catch(function(e){renderDetailError(e)});
    fetch('/aweme/v1/web/aweme/related/?aweme_id='+enc(id)+'&count=10&refresh=1'
      +'&device_platform=webapp&aid=6383')
      .then(function(r){return r.ok?r.json():Promise.reject(new Error('HTTP '+r.status))})
      .catch(function(e){console.warn('aweme/related:', e)});
    initDyComments(id);
  }"""
        elif site == "xhs":
            # xhs 详情数据 SSR 内嵌（语料实证：详情页无 note XHR，仅 comment/page）
            detail_fetch = """
  var id=detailId();
  var st=window.__INITIAL_STATE__;
  if(st&&st.entity){ renderDetail(st.entity); }
  else if(id){ renderDetailError(); }
  if(id){ initXhsComments(id, st&&st.entity&&st.entity.note_card); }"""
        else:
            # ks 详情数据 SSR 内嵌 + 评论走 GraphQL commentListQuery（真站主面）
            detail_fetch = """
  var st=window.__INITIAL_STATE__;
  if(st&&st.entity){ renderDetail(st.entity); }
  else { renderDetailError(); }
  var pid=detailId()||(st&&st.entity&&st.entity.photo&&st.entity.photo.id);
  if(pid){ initKsComments(String(pid)); }"""
        js.append("""
function detailId(){
  var id=new URLSearchParams(location.search).get('id');
  if(id)return id;
  var m=location.pathname.match(%s);
  return m?m[1]:null;
}
function initDetail(){
%s
}""" % (id_pat, detail_fetch))
        js.append(COMMENT_ENGINES[site])
    if site == "xhs" and page in ("home", "search"):
        # xhs 搜索页模态（R5B-P2-4）内嵌评论列表 + 楼中楼展开——与详情页共用
        # 评论行渲染/子评论引擎（xhsCommentRow/xhsExpandReplies）。
        js.append(COMMENT_ENGINES["xhs"])
    return "\n".join(js)


# ---------------------------------------------------------------------------
# 评论引擎（红队 R5B-P2-1 翻页 + P2-2 楼中楼）：三站各自续载链 + 展开控件
# ---------------------------------------------------------------------------
COMMENT_ENGINES = {
    "douyin": """
/* dy：comment/list 数值 cursor 链（滚动续载 + 按钮），面板计数=total 声称值 */
var dyCmt={cursor:0,total:null,done:false,loading:false};
function dyCommentRow(c,awemeId){
  var it=el('div','comment-item'); it.setAttribute('data-cid',c.cid||'');
  it.appendChild(el('div','c-user',((c.user&&c.user.nickname)||'匿名')
    +(c.ip_location?(' · '+c.ip_location):'')));
  it.appendChild(el('div','c-content',c.text||''));
  it.appendChild(el('div','c-like like-count','赞 '+fmt(c.digg_count)));
  var n=parseInt(c.reply_comment_total||0,10);
  if(n>0){   /* R5B-P2-2：展开N条回复（语料形态文案） */
    var btn=el('button','expand-replies','展开'+n+'条回复');
    btn.setAttribute('data-cid',c.cid||''); btn.setAttribute('data-left',n);
    btn.onclick=function(){dyExpandReplies(btn,awemeId);};
    it.appendChild(btn);
  }
  return it;
}
function dyExpandReplies(btn,awemeId){
  var cid=btn.getAttribute('data-cid');
  var box=el('div','sub-comments');
  btn.parentNode.insertBefore(box,btn.nextSibling);
  btn.disabled=true; btn.textContent='展开中…';
  function page(cursor){
    fetch('/aweme/v1/web/comment/list/reply/?item_id='+enc(awemeId)
      +'&comment_id='+enc(cid)+'&cursor='+cursor+'&count=20')
      .then(function(r){return r.ok?r.json():Promise.reject(new Error('HTTP '+r.status))})
      .then(function(j){
        (j.comments||[]).forEach(function(rc){box.appendChild(dyCommentRow(rc,awemeId));});
        if(j.has_more){page(j.cursor);}
        else{btn.parentNode&&btn.parentNode.removeChild(btn);}
      }).catch(function(){btn.disabled=false;btn.textContent='加载失败，点击重试';
        btn.onclick=function(){dyExpandReplies(btn,awemeId);};});
  }
  page(0);
}
function initDyComments(awemeId){
  var box=document.getElementById('commentList'); if(!box)return;
  var btn=document.getElementById('loadCmtBtn');
  function load(){
    if(dyCmt.done||dyCmt.loading)return; dyCmt.loading=true; if(btn)btn.disabled=true;
    fetch('/aweme/v1/web/comment/list/?aweme_id='+enc(awemeId)
      +'&cursor='+dyCmt.cursor+'&count=10&item_type=0&cut_version=1')
      .then(function(r){return r.ok?r.json():Promise.reject(new Error('HTTP '+r.status))})
      .then(function(j){
        (j.comments||[]).forEach(function(c){box.appendChild(dyCommentRow(c,awemeId));});
        dyCmt.total=(j.total!=null)?j.total:dyCmt.total;
        var n=document.getElementById('commentCount'); if(n&&dyCmt.total!=null)n.textContent=dyCmt.total;
        dyCmt.cursor=j.cursor||dyCmt.cursor; dyCmt.done=!j.has_more;
        if(btn){btn.disabled=dyCmt.done;btn.textContent=dyCmt.done?'没有更多评论':'加载更多评论';}
        dyCmt.loading=false;
      }).catch(function(){dyCmt.loading=false;if(btn)btn.disabled=false;});
  }
  if(btn)btn.onclick=load;
  load();
  /* 滚动续载（语料：每样本 8-20 次 comment/list，滚动触发） */
  var _t=0;
  window.addEventListener('scroll',function(){
    var doc=document.documentElement;
    var h=Math.max(doc.scrollHeight||0,document.body?document.body.scrollHeight||0:0);
    if(!h||window.scrollY+window.innerHeight<=h-600)return;
    var now=Date.now(); if(now-_t<1200)return; _t=now;
    if(!dyCmt.done)load();
  },{passive:true});
}""",
    "xhs": """
/* xhs：comment/page cursor id 链；面板「共 loaded/total 条评论」（total=interact 声称） */
function xhsCommentRow(c,noteId,xsec){
  var it=el('div','comment-item'); it.setAttribute('data-cid',c.id||'');
  it.appendChild(el('div','c-user',((c.user_info&&c.user_info.nickname)||'匿名')
    +(c.ip_location?(' · '+c.ip_location):'')));
  it.appendChild(el('div','c-content',c.content||''));
  it.appendChild(el('div','c-like like-count','赞 '+fmt(c.like_count)));
  var n=parseInt(c.sub_comment_count||0,10);
  if(n>0){   /* R5B-P2-2：展开 N 条回复（sub/page 端点） */
    var btn=el('button','expand-replies','展开'+n+'条回复');
    btn.setAttribute('data-cid',c.id||'');
    btn.onclick=function(){xhsExpandReplies(btn,noteId,xsec);};
    it.appendChild(btn);
  }
  return it;
}
function xhsExpandReplies(btn,noteId,xsec){
  var rid=btn.getAttribute('data-cid');
  var box=el('div','sub-comments');
  btn.parentNode.insertBefore(box,btn.nextSibling);
  btn.disabled=true; btn.textContent='展开中…';
  function page(cursor){
    fetch('/api/sns/web/v2/comment/sub/page?note_id='+enc(noteId)
      +'&root_comment_id='+enc(rid)+'&num=10&cursor='+enc(cursor||'')
      +'&image_formats=jpg,webp,avif&top_comment_id=&xsec_token='+enc(xsec||''))
      .then(function(r){return r.ok?r.json():Promise.reject(new Error('HTTP '+r.status))})
      .then(function(resp){
        var d=(resp&&resp.data)||{};
        (d.comments||[]).forEach(function(sc){
          var it=el('div','comment-item');
          var who=((sc.user_info&&sc.user_info.nickname)||'匿名');
          var t=(sc.target_comment&&sc.target_comment.user_info&&sc.target_comment.user_info.nickname);
          it.appendChild(el('div','c-user',who+(t?(' 回复 '+t):'')));
          it.appendChild(el('div','c-content',sc.content||''));
          it.appendChild(el('div','c-like like-count','赞 '+fmt(sc.like_count)));
          box.appendChild(it);
        });
        if(d.has_more&&d.cursor){page(d.cursor);}
        else{btn.parentNode&&btn.parentNode.removeChild(btn);}
      }).catch(function(){btn.disabled=false;btn.textContent='加载失败，点击重试';
        btn.onclick=function(){xhsExpandReplies(btn,noteId,xsec);};});
  }
  page('');
}
function initXhsComments(noteId,noteCard){
  var box=document.getElementById('commentList'); if(!box)return;
  var btn=document.getElementById('loadCmtBtn');
  var total=null;
  try{total=parseInt(((noteCard||{}).interact_info||{}).comment_count,10);}catch(e){}
  if(total!=null&&!isNaN(total)){
    var t=document.getElementById('commentCount'); if(t)t.textContent=total;
  }
  var c={cursor:'',done:false,loading:false};
  function load(){
    if(c.done||c.loading)return; c.loading=true; if(btn)btn.disabled=true;
    fetch('/api/sns/web/v2/comment/page?note_id='+enc(noteId)+'&cursor='+enc(c.cursor)
      +'&top_comment_id=&image_formats=jpg,webp,avif&xsec_token=')
      .then(function(r){return r.ok?r.json():Promise.reject(new Error('HTTP '+r.status))})
      .then(function(resp){
        var d=(resp&&resp.data)||{};
        (d.comments||[]).forEach(function(cm){box.appendChild(xhsCommentRow(cm,noteId,''));});
        var l=document.getElementById('commentLoaded');
        if(l)l.textContent=box.children.length;
        if(total==null||isNaN(total)){
          var t=document.getElementById('commentCount'); if(t)t.textContent=box.children.length;
        }
        c.cursor=d.cursor||''; c.done=!d.has_more;
        if(btn){btn.disabled=c.done;btn.textContent=c.done?'没有更多评论':'加载更多评论';}
        c.loading=false;
      }).catch(function(){c.loading=false;if(btn)btn.disabled=false;});
  }
  if(btn)btn.onclick=load;
  load();
  var _t=0;
  window.addEventListener('scroll',function(){
    var doc=document.documentElement;
    var h=Math.max(doc.scrollHeight||0,document.body?document.body.scrollHeight||0:0);
    if(!h||window.scrollY+window.innerHeight<=h-600)return;
    var now=Date.now(); if(now-_t<1200)return; _t=now;
    if(!c.done)load();
  },{passive:true});
}""",
    "kuaishou": """
/* ks：GraphQL commentListQuery pcursor 链 + 「查看更多回复」（visionSubCommentList） */
function ksCommentRow(c,photoId){
  var it=el('div','comment-item'); it.setAttribute('data-cid',c.commentId||'');
  it.appendChild(el('div','c-user',(c.authorName||'匿名')
    +(c.timestamp?(' · '+new Date(c.timestamp).toLocaleDateString()):'')));
  it.appendChild(el('div','c-content',c.content||''));
  it.appendChild(el('div','c-like like-count','赞 '+fmt(c.likedCount)));
  if(c.hasSubComments){   /* R5B-P2-2：查看更多回复（语料形态文案） */
    var btn=el('button','expand-replies','查看更多回复');
    btn.setAttribute('data-cid',c.commentId||'');
    btn.onclick=function(){ksExpandReplies(btn,photoId);};
    it.appendChild(btn);
  }
  return it;
}
function ksExpandReplies(btn,photoId){
  var rid=btn.getAttribute('data-cid');
  var box=el('div','sub-comments');
  btn.parentNode.insertBefore(box,btn.nextSibling);
  btn.disabled=true; btn.textContent='展开中…';
  function page(pcursor){
    fetch('/graphql',{method:'POST',headers:{'Content-Type':'application/json'},
      body:JSON.stringify({operationName:'visionSubCommentList',
        query:'query visionSubCommentList($photoId:String,$rootCommentId:String,$pcursor:String){visionSubCommentList(photoId:$photoId,rootCommentId:$rootCommentId,pcursor:$pcursor){pcursor pcursorV2 subCommentsV2{commentId authorName content timestamp likedCount replyToUserName}}}',
        variables:{photoId:String(photoId),rootCommentId:String(rid),pcursor:String(pcursor||'')}})})
      .then(function(r){return r.ok?r.json():Promise.reject(new Error('HTTP '+r.status))})
      .then(function(j){
        var d=(j&&j.data&&j.data.visionSubCommentList)||{};
        (d.subCommentsV2||[]).forEach(function(sc){
          var it=el('div','comment-item');
          it.appendChild(el('div','c-user',(sc.authorName||'匿名')
            +(sc.replyToUserName?(' 回复 '+sc.replyToUserName):'')));
          it.appendChild(el('div','c-content',sc.content||''));
          it.appendChild(el('div','c-like like-count','赞 '+fmt(sc.likedCount)));
          box.appendChild(it);
        });
        if(d.pcursorV2&&d.pcursorV2!=='no_more'){page(d.pcursorV2);}
        else{btn.textContent='收起回复';
          btn.disabled=false; btn.onclick=function(){
            var vis=box.style.display==='none';
            box.style.display=vis?'':'none'; btn.textContent=vis?'收起回复':'查看更多回复';};
        }
      }).catch(function(){btn.disabled=false;btn.textContent='加载失败，点击重试';
        btn.onclick=function(){ksExpandReplies(btn,photoId);};});
  }
  page('');
}
function initKsComments(photoId){
  var box=document.getElementById('commentList'); if(!box)return;
  var btn=document.getElementById('loadCmtBtn');
  var c={pcursor:'',done:false,loading:false};
  function gql(pcursor){
    return fetch('/graphql',{method:'POST',headers:{'Content-Type':'application/json'},
      body:JSON.stringify({operationName:'commentListQuery',
        query:'query commentListQuery($photoId:String,$pcursor:String){visionCommentList(photoId:$photoId,pcursor:$pcursor){commentCount commentCountV2 pcursor rootCommentsV2{commentId authorId authorName content timestamp likedCount hasSubComments}}}',
        variables:{photoId:String(photoId),pcursor:String(pcursor||'')}})})
      .then(function(r){return r.ok?r.json():Promise.reject(new Error('HTTP '+r.status))});
  }
  function load(){
    if(c.done||c.loading)return; c.loading=true; if(btn)btn.disabled=true;
    gql(c.pcursor).then(function(j){
      var l=(j&&j.data&&j.data.visionCommentList)||{};
      (l.rootCommentsV2||[]).forEach(function(rc){box.appendChild(ksCommentRow(rc,photoId));});
      var t=document.getElementById('commentCount');
      if(t&&l.commentCountV2!=null)t.textContent=l.commentCountV2;
      c.pcursor=l.pcursor||''; c.done=!l.pcursor;
      if(btn){btn.disabled=c.done;btn.textContent=c.done?'没有更多评论':'加载更多评论';}
      c.loading=false;
    }).catch(function(){c.loading=false;if(btn)btn.disabled=false;});
  }
  if(btn)btn.onclick=load;
  load();
  var _t=0;
  window.addEventListener('scroll',function(){
    var doc=document.documentElement;
    var h=Math.max(doc.scrollHeight||0,document.body?document.body.scrollHeight||0:0);
    if(!h||window.scrollY+window.innerHeight<=h-600)return;
    var now=Date.now(); if(now-_t<1200)return; _t=now;
    if(!c.done)load();
  },{passive:true});
}""",
}


def detail_renderer(site: str) -> str:
    if site == "douyin":
        return """
function renderDetail(a){
  document.querySelector('.detail-main .title').textContent=a.desc||'(无描述)';
  var aw=document.querySelector('.detail-main a.author-wrapper');
  aw.textContent=(a.author&&a.author.nickname)||'';
  aw.href='/user/'+enc((a.author&&a.author.sec_uid)||'');
  var m=document.querySelector('.detail-main .metrics');
  m.innerHTML='';
  [['like-count','赞 '+fmt(a.statistics.digg_count)],['play-count','播放 '+fmt(a.statistics.play_count)],
   ['comment-count','评论 '+fmt(a.statistics.comment_count)],['collect-count','收藏 '+fmt(a.statistics.collect_count)],
   ['share-count','分享 '+fmt(a.statistics.share_count)]].forEach(function(p){
    var s=el('span',p[0],p[1]); m.appendChild(s);});
  document.querySelector('.detail-cover img').src=coverUrl(a.aweme_id,(a.desc||'').slice(0,2),660,880);
  document.querySelector('.duration-badge').textContent=fmtDur(a.video&&a.video.duration);
  setStatus('aweme_id='+a.aweme_id);
}
/* 红队 P2-D6：详情不存在 → 真站错误态（「你要观看的视频不存在」+ data-e2e="error-page"） */
function renderDetailError(){
  var w=document.querySelector('.detail-wrap');
  w.innerHTML='';
  var d=el('div','error-page');
  d.setAttribute('data-e2e','error-page');
  d.style.cssText='text-align:center;padding:72px 0;color:#555';
  var h=el('div'); h.style.cssText='font-size:22px;font-weight:600;margin-bottom:10px';
  h.textContent='你要观看的视频不存在';
  var p=el('div'); p.style.cssText='font-size:13px;color:#999';
  p.textContent='视频可能已被删除，或暂时无法观看';
  var b=el('a'); b.href='/'; b.textContent='去首页逛逛';
  b.style.cssText='display:inline-block;margin-top:18px;color:#fe2c55;text-decoration:none';
  d.appendChild(h); d.appendChild(p); d.appendChild(b);
  w.appendChild(d);
  setStatus('视频不存在');
}"""
    if site == "xhs":
        return """
function renderDetail(e){
  var n=e.note_card||{};
  document.querySelector('.detail-main .title').textContent=n.display_title||'(无标题)';
  var aw=document.querySelector('.detail-main a.author-wrapper');
  aw.textContent=(n.user&&n.user.nickname)||'';
  aw.href='/user/profile/'+enc((n.user&&n.user.user_id)||'');
  var ii=n.interact_info||{}; var m=document.querySelector('.detail-main .metrics'); m.innerHTML='';
  [['like-count','点赞 '+fmt(ii.liked_count)],['comment-count','评论 '+fmt(ii.comment_count)],
   ['collected-count','收藏 '+fmt(ii.collected_count)],['shared-count','分享 '+fmt(ii.shared_count)]]
   .forEach(function(p){m.appendChild(el('span',p[0],p[1]))});
  document.querySelector('.detail-cover img').src=coverUrl(e.id,(n.display_title||'').slice(0,2),660,880);
  setStatus('note_id='+e.id);
}
/* 红队 P2-X5：真站对不存在笔记 302 → /404；服务端已 302，此处为页面侧兜底 */
function renderDetailError(){
  location.replace('/404?source='+enc('/404/sec_pVswaPpO?redirectPath='+location.pathname)
    +'&error_code=300031&error_msg='+enc('当前笔记暂时无法浏览'));
}"""
    return """
function renderDetail(e){
  var p=e.photo||{};
  document.querySelector('.detail-main .title').textContent=p.caption||'(无描述)';
  var aw=document.querySelector('.detail-main a.author-wrapper');
  aw.textContent=(e.author&&e.author.name)||'';
  aw.href='/profile/'+enc((e.author&&e.author.id)||'');
  var m=document.querySelector('.detail-main .metrics'); m.innerHTML='';
  [['like-count','赞 '+fmt(p.likeCount)],['play-count','播放 '+fmt(p.viewCount)],
   ['comment-count','评论 '+fmt((e.comment&&e.comment.us_c)||0)],['collect-count','收藏 '+fmt(p.collectCount)]]
   .forEach(function(x){m.appendChild(el('span',x[0],x[1]))});
  document.querySelector('.detail-cover img').src=coverUrl(p.id,(p.caption||'').slice(0,2),660,880);
  document.querySelector('.duration-badge').textContent=fmtDur((p.duration||0)*1000);
  setStatus('photo.id='+p.id);
}
function renderDetailError(){
  var w=document.querySelector('.detail-wrap');
  w.innerHTML='';
  var d=el('div','error-page');
  d.setAttribute('data-e2e','error-page');
  d.style.cssText='text-align:center;padding:72px 0;color:#555';
  var h=el('div'); h.style.cssText='font-size:22px;font-weight:600;margin-bottom:10px';
  h.textContent='内容不存在';
  var b=el('a'); b.href='/'; b.textContent='去首页逛逛';
  b.style.cssText='display:inline-block;margin-top:18px;color:#fe2c55;text-decoration:none';
  d.appendChild(h); d.appendChild(b);
  w.appendChild(d);
  setStatus('内容不存在');
}"""


def error404_page(site: str, provenance: str) -> str:
    """xhs 真站 404 页形态（红队 P2-X5：302 落点，title「小红书 - 你访问的页面不见了」）。"""
    assert site == "xhs"
    return """<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<link rel="icon" href="/favicon.ico" type="image/x-icon">
<title>小红书 - 你访问的页面不见了</title>
<style>
body{font:14px/1.7 -apple-system,"Segoe UI","Microsoft YaHei",sans-serif;color:#333;background:#fff}
header{display:flex;align-items:center;gap:20px;padding:14px 32px;border-bottom:1px solid #eee}
header .brand{color:#fe2c55;font-weight:700;font-size:19px}
header nav a{margin-right:16px;color:#555;text-decoration:none;font-size:13px}
main{max-width:640px;margin:0 auto;padding:90px 20px;text-align:center}
main .code{font-size:64px;font-weight:700;color:#fe2c55;letter-spacing:4px}
main h1{font-size:20px;font-weight:500;margin:14px 0 8px}
main p{color:#999;font-size:13px}
main a.btn{display:inline-block;margin-top:22px;padding:9px 30px;border-radius:18px;
 background:#fe2c55;color:#fff;text-decoration:none;font-size:14px}
footer{position:fixed;bottom:0;left:0;right:0;padding:14px 0;text-align:center;color:#bbb;font-size:11px;
 border-top:1px solid #f2f2f2;background:#fafafa}
</style>
<!-- __PROVENANCE__ -->
</head>
<body>
<header>
  <span class="brand">小红书</span>
  <nav><a href="/">首页</a><a href="/search_result">发现</a></nav>
</header>
<main>
  <div class="code">404</div>
  <h1>你访问的页面不见了</h1>
  <p>页面可能已被删除、名称已更改或暂时不可用</p>
  <a class="btn" href="/">返回首页</a>
</main>
<footer>沪ICP备13030189号 | 营业执照 | © 2014-2026 行吟信息科技（上海）有限公司</footer>
</body>
</html>
""".replace("__PROVENANCE__", provenance)


def profile_page(site: str, provenance: str) -> str:
    """作者主页骨架（红队 R3-P2-3 + R5B-P1）：SSR 数据注入 __INITIAL_STATE__。

    dy：「作品/喜欢」tab（data-e2e=user-work-tab/user-like-tab + user-tab-count，
    语料 dy_user_profile_tabs 形态）；喜欢 tab 数据 = synth_api 注入 like_works。"""
    detail_href = {"douyin": "'/video/'+enc(w.id)", "xhs": "'/explore/'+enc(w.id)",
                   "kuaishou": "'/short-video/'+enc(w.id)"}[site]
    js = """
function workNode(w){
  var d=el('div','card-container work-card');
  var link=el('a','card-link'); link.href=%s;
  var cov=el('div','cover');
  var img=new Image(); img.alt=w.title; img.loading='lazy';
  img.width=323; img.height=430;
  img.src=coverUrl(w.id,(w.title||'').slice(0,2));
  cov.appendChild(img);
  var info=el('div','card-info');
  info.appendChild(el('div','title',w.title||'(无描述)'));
  var m=el('div','metrics');
  if(w.like!=null)m.appendChild(el('span','like-count','赞 '+fmt(w.like)));
  if(w.view)m.appendChild(el('span','play-count',w.view));
  info.appendChild(m);
  link.appendChild(cov); link.appendChild(info); d.appendChild(link); return d;
}
function renderWorks(list){
  var box=document.querySelector('.works'); box.innerHTML='';
  (list||[]).forEach(function(w){box.appendChild(workNode(w));});
  document.getElementById('profileStatus').textContent='共 '+((list||[]).length)+' 个作品';
}
function initProfile(){
  var st=window.__INITIAL_STATE__;
  var box=document.querySelector('.works');
  if(!box)return;
  if(!(st&&st.author)){ document.getElementById('profileStatus').textContent='用户不存在'; return; }
  var a=st.author;
  document.querySelector('.p-name').textContent=a.name||'用户';
  if(a.followers!=null){document.querySelector('.p-meta').textContent=
    '作品 '+((st.works||[]).length)+' · 粉丝 '+fmt(a.followers);}
  else{document.querySelector('.p-meta').textContent='作品 '+((st.works||[]).length);}
  renderWorks(st.works||[]);
  %s
}""" % (detail_href,
            """  /* R5B-P1：作品/喜欢 tab（语料 data-e2e 形态 + user-tab-count） */
  var tabs=document.querySelectorAll('.profile-tabs .p-tab');
  if(tabs.length){
    var counts={'post':(st.works||[]).length,'like':(st.like_works||st.works||[]).length};
    tabs.forEach(function(t){
      var k=t.getAttribute('data-tab');
      var c=t.querySelector('.p-tab-count'); if(c)c.textContent=counts[k]||0;
      t.addEventListener('click',function(){
        tabs.forEach(function(x){x.className='p-tab';});
        t.className='p-tab active';
        renderWorks(k==='like'?(st.like_works||st.works||[]):(st.works||[]));
      });
    });
  }""" if site == "douyin" else "")
    dy_tabs = """
  <div class="profile-tabs" data-e2e="user-tabs">
    <div class="p-tab active" data-tab="post" data-e2e="user-work-tab" role="tab" aria-selected="true">作品<span class="p-tab-count" data-e2e="user-tab-count">0</span></div>
    <div class="p-tab" data-tab="like" data-e2e="user-like-tab" role="tab" aria-selected="false">喜欢<span class="p-tab-count" data-e2e="user-tab-count">0</span></div>
  </div>""" if site == "douyin" else ""
    html = """<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<link rel="icon" href="/favicon.ico" type="image/x-icon">
<title>__TITLE__</title>
<style>__CSS__
.profile-head{display:flex;align-items:center;gap:18px;background:#fff;border-radius:12px;padding:22px;margin-bottom:16px}
.profile-head .p-name{font-size:20px;font-weight:600}
.profile-head .p-meta{color:#888;font-size:13px;margin-top:6px}
.works{display:flex;flex-wrap:wrap;gap:14px}
</style>
<!-- __PROVENANCE__ -->
<script>window.__INITIAL_STATE__=/*__STATE__*/null;</script>
</head>
<body>
<header class="site-header">
  <span class="brand">__BRAND__</span>
  <nav class="nav"><a href="/">首页</a><a href="__SEARCH_PATH__">搜索</a></nav>
</header>
<main>
  <div class="profile-head">
    <div>
      <div class="p-name">加载中…</div>
      <div class="p-meta"></div>
    </div>
  </div>
__DY_TABS__
  <p class="page-desc status" id="profileStatus"></p>
  <div class="works cards"></div>
</main>
<footer>__FOOTER__</footer>
<script>
__JS__
document.addEventListener('DOMContentLoaded', function(){ initProfile(); });
</script>
</body>
</html>
"""
    for k, v in {
        "__TITLE__": SITE_TITLES[site], "__BRAND__": SITE_BRANDS[site],
        "__SEARCH_PATH__": {"douyin": "/search", "xhs": "/search_result",
                            "kuaishou": "/search/video"}[site],
        "__FOOTER__": SITE_TITLES[site], "__CSS__": CSS,
        "__DY_TABS__": dy_tabs,
        "__PROVENANCE__": provenance, "__JS__": JS_LIB.replace(
            "DEFAULT_KW", json.dumps(DEFAULT_KEYWORDS[site], ensure_ascii=False)
        ).replace("__COVER_PREFIX__", SITE_COVER_PREFIX[site]) + js,
    }.items():
        html = html.replace(k, v)
    return html


def build_page(site: str, page: str, provenance: str) -> str:
    kw = DEFAULT_KEYWORDS[site]
    inp = SEARCH_INPUTS[site].format(kw=kw)
    body = ""
    if page in ("home", "search"):
        body = """
  <div class="toolbar">
    <button class="load-more" id="loadMoreBtn" type="button">加载更多</button>
    <span class="status" id="loadStatus">初始化…</span>
  </div>
__TABS__
{container}
  <div class="toolbar"><button class="load-more" id="loadMoreBtn2" type="button">加载更多</button></div>
""".replace("{container}", CONTAINERS[site])
        body = body.replace("__TABS__", build_tabs_html(site) if page == "search" else
                            (build_channel_html() if site == "xhs" else ""))
        # R5B-P3-2：dy 搜索页静态 title（真站形态，doSearch 不再覆写）
        title = SITE_SEARCH_TITLES["douyin"] if (site == "douyin" and page == "search") \
            else SITE_TITLES[site]
        init = ("initSearch();document.getElementById('loadMoreBtn').onclick=loadMore;"
                "document.getElementById('loadMoreBtn2').onclick=loadMore;"
                + ("bindCardModal();maybeAutoOpenModal();" if site == "xhs" else ""))
    else:
        body = """
  <div class="detail-wrap">
    <div class="detail-cover"><img alt="封面"><span class="duration-badge duration" style="position:static;display:inline-block;margin-top:6px"></span></div>
    <div class="detail-main">
      <h1 class="title">加载中…</h1>
      <a class="author-wrapper" href="#"></a>
      <div class="metrics"></div>
      {comments}
    </div>
  </div>
  <p class="page-desc status" id="loadStatus"></p>
"""
        # R5B-P2-1：评论面板标题按站对齐（dy「全部评论」/xhs「共 N/M 条评论」/ks「评论 N」），
        # 计数用 total 声称值（不再=已加载条数）；加载更多评论按钮（禁用态随翻尽）。
        if site == "douyin":
            comments = ('<div class="comments"><h3>全部评论</h3>'
                        '<div class="comments-count">共 <span id="commentCount">0</span> 条</div>'
                        '<div id="commentList"></div>'
                        '<button class="load-more-comments" id="loadCmtBtn" type="button">加载更多评论</button></div>')
        elif site == "xhs":
            comments = ('<div class="comments"><h3>全部评论</h3>'
                        '<div class="comments-count">共 <span id="commentLoaded">0</span>/<span id="commentCount">0</span> 条评论</div>'
                        '<div id="commentList"></div>'
                        '<button class="load-more-comments" id="loadCmtBtn" type="button">加载更多评论</button></div>')
        else:
            comments = ('<div class="comments"><h3>评论</h3>'
                        '<div class="comments-count">共 <span id="commentCount">0</span> 条</div>'
                        '<div id="commentList"></div>'
                        '<button class="load-more-comments" id="loadCmtBtn" type="button">加载更多评论</button></div>')
        body = body.replace("{comments}", comments)
        title = SITE_TITLES[site]  # 详情/作者页 title 由 synth_api 按实体注入（R5B-P3-2）
        init = "initDetail();"

    js = build_js(site, page)
    if page == "detail":
        js += "\n" + detail_renderer(site)
    elif page in ("home", "search"):
        js += "\n" + CARD_RENDERERS[site]
        if site == "kuaishou":
            # 真站搜索加载并发 search/user（语料 req_0007）——页面加载即双发
            js += ("\nvar _origLoadMore=loadMore;\n"
                   "loadMore=function(){var r=_origLoadMore();"
                   "if(state.page===1){fetchSearchUsers(state.keyword,'').then(onSearchUsers);}"
                   "return r;};")

    html = """<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<link rel="icon" href="/favicon.ico" type="image/x-icon">
<title>__TITLE__</title>
<style>__CSS__</style>
<!-- __PROVENANCE__ -->
<script>window.__INITIAL_STATE__=/*__STATE__*/null;</script>
</head>
<body>
<header class="site-header">
  <span class="brand">__BRAND__</span>
  <nav class="nav"><a href="/">首页</a><a href="__SEARCH_PATH__">搜索</a></nav>
  <form class="searchbar" id="searchForm" action="__SEARCH_PATH__" method="get">
    <div class="input-box">__INPUT__</div>
    <button class="search-button" data-e2e="searchbar-button" type="submit">搜索</button>
  </form>
</header>
<main>
__BODY__
</main>
<footer>__FOOTER__</footer>
<script>
__JS__
document.addEventListener('DOMContentLoaded', function(){ __INIT__ });
</script>
</body>
</html>
"""
    for k, v in {
        "__TITLE__": title,
        "__BRAND__": SITE_BRANDS[site],
        "__SEARCH_PATH__": {"douyin": "/search", "xhs": "/search_result",
                            "kuaishou": "/search/video"}[site],
        "__FOOTER__": {"douyin": "抖音 - 记录美好生活", "xhs": "小红书 - 标记我的生活",
                       "kuaishou": "快手-看见每一种生活"}[site],
        "__CSS__": CSS, "__PROVENANCE__": provenance,
        "__INPUT__": inp, "__BODY__": body, "__JS__": js, "__INIT__": init,
    }.items():
        html = html.replace(k, v)
    return html


def search_user_page(site: str, provenance: str) -> str:
    """ks /search/user?searchKey= 用户 tab 页（红队 R5B-P2-3；数据 /rest/v/search/user）。"""
    assert site == "kuaishou"
    js = JS_LIB.replace("DEFAULT_KW", json.dumps(DEFAULT_KEYWORDS[site], ensure_ascii=False)) \
               .replace("__COVER_PREFIX__", SITE_COVER_PREFIX[site]) + """
function initSearchUsers(){
  var q=new URLSearchParams(location.search);
  var kw=(q.get('searchKey')||q.get('keyword')||DEFAULT_KW);
  var cont=document.querySelector('.user-cards');
  cont.setAttribute('data-keyword',kw);
  document.title=kw+' - 快手';
  fetch('/rest/v/search/user',{method:'POST',
    headers:{'Content-Type':'application/json','Accept':'application/json'},
    body:JSON.stringify({keyword:kw,pcursor:'',searchSessionId:'',webPageArea:''})})
    .then(function(r){return r.ok?r.json():Promise.reject(new Error('HTTP '+r.status))})
    .then(function(j){
      (j.users||[]).forEach(function(u){cont.appendChild(ksUserCardNode(u));});
      var t=document.getElementById('userCount'); if(t)t.textContent=(j.users||[]).length;
      setStatus('');
    })
    .catch(function(){setStatus('加载失败，点击重试',true)});
  /* tab 激活态：本页=用户 */
  var tabs=document.querySelectorAll('#searchTabs .tab-item');
  tabs.forEach(function(x){
    var on=(x.getAttribute('data-href-prefix')||'').indexOf('/search/user')===0;
    x.className='tab-item'+(on?' tab-item-active':'');
    x.addEventListener('click',function(){
      location.href=x.getAttribute('data-href-prefix')+enc(kw);
    });
  });
}
"""
    html = """<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<link rel="icon" href="/favicon.ico" type="image/x-icon">
<title>快手</title>
<style>__CSS__</style>
<!-- __PROVENANCE__ -->
</head>
<body>
<header class="site-header">
  <span class="brand">快手</span>
  <nav class="nav"><a href="/">首页</a><a href="/search/video">搜索</a></nav>
  <form class="searchbar" id="searchForm" action="/search/video" method="get">
    <div class="input-box"><input id="searchInput" class="input input-mini" type="text"
      placeholder="搜索你感兴趣的内容" value="美食探店" aria-label="搜索" name="searchKey"></div>
    <button class="search-button" data-e2e="searchbar-button" type="submit">搜索</button>
  </form>
</header>
<main>
  <div class="tabs tab-nav" id="searchTabs">
    <span role="button" class="tab-item" data-href-prefix="/search/video?searchKey=">视频</span>
    <span role="button" class="tab-item tab-item-active" data-href-prefix="/search/user?searchKey=">用户<i class="tab-item-border"></i></span>
  </div>
  <p class="page-desc status" id="loadStatus">加载中…</p>
  <p class="page-desc">用户（<span id="userCount">0</span>）</p>
  <div class="cards user-cards"></div>
</main>
<footer>快手-看见每一种生活</footer>
<script>
__JS__
__CARDS__
document.addEventListener('DOMContentLoaded', function(){ initSearchUsers(); });
</script>
</body>
</html>
"""
    for k, v in {
        "__CSS__": CSS, "__PROVENANCE__": provenance, "__JS__": js,
        "__CARDS__": CARD_RENDERERS["kuaishou"],
    }.items():
        html = html.replace(k, v)
    return html


def verify_against_sanitized(site: str, sanitized_dir: Path) -> tuple[str, int]:
    """读取脱敏 DOM，校验关键选择器存在；返回 provenance 注释与卡片计数。"""
    dom_path = sanitized_dir / DOM_SAMPLES[site]
    if not dom_path.is_file():
        return ("结构来源：脱敏 DOM 样本缺失(%s)，骨架按契约字段生成" % DOM_SAMPLES[site], 0)
    dom = dom_path.read_text(encoding="utf-8", errors="replace")
    lines = ["DOM structure cross-check against reference sample"]
    n_cards = 0
    for name, (needle, cls) in DOM_SELECTORS[site].items():
        n = dom.count(needle)
        if name == "卡片":
            n_cards = n
        lines.append("  %s：'%s' 出现 %d 次%s" % (name, cls, n, "" if n else "（0 次！）"))
    return "\n    ".join(lines), n_cards


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description="生成三站合成站页面骨架（synth_api 托管）")
    ap.add_argument("--sanitized", default=str(DEFAULT_SANITIZED))
    ap.add_argument("--out", default=str(DEFAULT_OUT))
    ap.add_argument("--site", default="all", choices=list(SITE_TITLES) + ["all"])
    args = ap.parse_args(argv)

    sanitized_dir, out_dir = Path(args.sanitized), Path(args.out)
    # oracle-lite：脱敏录制语料不随包分发——语料目录缺失时跳过 DOM 对照
    # （不判失败），仍可重生成页面骨架；有语料时传 --sanitized <dir> 恢复完整校验。
    sanitized_missing = not sanitized_dir.is_dir()
    sites = list(SITE_TITLES) if args.site == "all" else [args.site]
    ok = True
    for site in sites:
        provenance, n_dom_cards = verify_against_sanitized(site, sanitized_dir)
        if n_dom_cards == 0 and not sanitized_missing:
            ok = False
        # xhs 额外生成真站 404 页形态（红队 P2-X5：/404?source=… 302 落点）；
        # 三站生成作者主页骨架（红队 R3-P2-3：/user/<sec_uid>、/user/profile/<uid>、/profile/<id>）；
        # ks 生成用户搜索页（红队 R5B-P2-3：/search/user?searchKey=）
        pages = ("home", "search", "detail", "profile", "error404") if site == "xhs" \
            else ("home", "search", "detail", "profile")
        if site == "kuaishou":
            pages = pages + ("search_user",)
        for page in pages:
            dest = out_dir / site / ("404.html" if page == "error404" else "%s.html" % page)
            dest.parent.mkdir(parents=True, exist_ok=True)
            content = error404_page(site, provenance) if page == "error404" \
                else (profile_page(site, provenance) if page == "profile"
                      else (search_user_page(site, provenance) if page == "search_user"
                            else build_page(site, page, provenance)))
            dest.write_text(content, encoding="utf-8")
            print("written: %s (%d bytes)" % (dest, dest.stat().st_size))
        print("[%s] 脱敏 DOM 对照：录制卡片 %d 张；骨架卡片结构 = %s" % (site, n_dom_cards,
                                                              CONTAINERS[site].split()[1]))
    print("DONE %s（选择器校验%s）" % (out_dir, "通过" if ok else "有缺失项，见上方 0 次警告"))
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
