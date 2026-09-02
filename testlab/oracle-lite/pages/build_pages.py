# -*- coding: utf-8 -*-
"""build_pages —— 从脱敏录制 DOM 模板 + 契约字段映射生成三站合成站页面骨架。

产物：pages/out/<site>/{home,search,detail}.html（synth_api 托管在 8661/8662/8663）。

结构参考 sanitized_corpus/<site>/<scenario>/sample_0001/page_dom.html 的关键容器
（搜索框 / 卡片容器 / 卡片 / 标题 / 点赞数），数据由页面 JS fetch 本地 synth_api
填充——不追求像素级，追求 harness 任务可走的 DOM 路径。

生成时对脱敏 DOM 做选择器存在性校验（卡片类名、搜索框特征），把「结构来源」
写进产物 HTML 的注释里；契约字段映射（响应 JSON 路径 → 卡片字段）按
oracle/contracts/ 实证路径硬编码在 SITE_CONFIG。

运行（任一 venv，纯 stdlib）：
  D:/Projects/temp2/oracle/env/Scripts/python.exe pages/build_pages.py
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
# 真站搜索页 URL 约定（红队 R3-P3-4：首页搜索改导航式，URL 同步真站）
SITE_SEARCH_URL = {
    "douyin": "'/search/'+enc(kw)+'?type=general'",
    "xhs": "'/search_result?keyword='+enc(kw)",
    "kuaishou": "'/search/video?searchKey='+enc(kw)",
}
# 卡片/详情真站 URL 约定（红队 R3-P1-2/R3-P2-3：链接对齐真站路径；f=卡片字段对象）
SITE_DETAIL_URL = {
    "douyin": "'/video/'+enc(f.id)",
    "xhs": "'/explore/'+enc(f.id)",
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
.toolbar .status{color:var(--sub);font-size:12px}
.detail-wrap{display:flex;gap:26px;background:#fff;border-radius:12px;padding:22px;margin-top:6px}
.detail-cover{width:340px;border-radius:10px;overflow:hidden;flex:none}
.detail-cover img{width:100%;display:block}
.detail-main{flex:1}
.detail-main h1.title{font-size:20px;line-height:1.5;margin-bottom:10px}
.detail-main .author-wrapper{color:#666;margin-bottom:12px}
.detail-main .metrics{font-size:15px;color:#444;display:flex;gap:22px;margin:8px 0 14px}
.detail-main .metrics .like-count{color:#e02040;font-weight:600}
.comments{margin-top:22px}
.comments h3{margin:10px 0}
.comment-item{background:#fff;border-radius:10px;padding:10px 14px;margin-bottom:10px}
.comment-item .c-user{color:var(--sub);font-size:12px}
.comment-item .c-content{margin-top:2px}
footer{color:#aaa;text-align:center;padding:26px 0;font-size:12px}
"""

# 页面 JS：分页链状态机（dy search_id / xhs page / ks pcursor）+ 卡片渲染
JS_LIB = """
function enc(s){return encodeURIComponent(s||'')}
function str(v){return String(v==null?0:v)}
function uuid(){return 'xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx'.replace(/[xy]/g,function(c){var r=Math.random()*16|0;return (c=='x'?r:(r&0x3|0x8)).toString(16)})}
function fmt(n){n=parseInt(n||0,10);if(isNaN(n))return '0';if(n>=100000000)return (n/100000000).toFixed(1)+'亿';if(n>=10000)return (n/10000).toFixed(1)+'万';return String(n)}
function fmtDur(ms){var s=Math.round((ms||0)/1000);var m=Math.floor(s/60);s=s%60;return m+':'+(s<10?'0':'')+s}
function coverUrl(seed,label,w,h){return '__COVER__'+enc(seed)+'~c5_'+(w||323)+'x'+(h||430)+'?label='+enc(label||'')}
function el(tag,cls,text){var e=document.createElement(tag);if(cls)e.className=cls;if(text!=null)e.textContent=text;return e}
function contEl(){return document.querySelector('.cards');}
/* setStatus 收口到 JS_LIB（原仅 home/search 定义——detail 页调用会 ReferenceError，红队修复复测暴露） */
function setStatus(t){var e=document.getElementById('loadStatus');if(e)e.textContent=t;}
var state={page:1,cursor:0,searchId:'',pcursor:'',session:'',keyword:DEFAULT_KW,loading:false,done:false,seq:0};
""".replace("__COVER__", "__COVER_PREFIX__")

CARD_RENDERERS = {
    "douyin": """
function cardNode(f){
  var li=el('li','search-result-card'); li.setAttribute('data-id',f.id);
  var a=el('a','card-link'); a.href=detailUrl(f);
  var cov=el('div','cover');
  var img=new Image(); img.alt=f.title; img.loading='lazy'; img.src=coverUrl(f.id,f.title.slice(0,2));
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
}""",
    "xhs": """
function cardNode(f){
  var sec=el('section','note-item'); sec.setAttribute('data-id',f.id);
  if(f.xsec){sec.setAttribute('data-xsec-token',f.xsec)}
  var a=el('a','card-link'); a.href=detailUrl(f);
  var cov=el('div','cover');
  var img=new Image(); img.alt=f.title; img.loading='lazy'; img.src=coverUrl(f.id,f.title.slice(0,2));
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
  var img=new Image(); img.alt=f.title; img.loading='lazy'; img.src=coverUrl(f.id,f.title.slice(0,2));
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
            js.append("""
function fetchPage(kw){
  var url = state.page===1 ? FIRST_URL : NEXT_URL;
  return fetch(url,{headers:{'Accept':'application/json'}}).then(function(r){
    if(!r.ok) throw new Error('HTTP '+r.status+' @ '+url);
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
    if(!r.ok) throw new Error('HTTP '+r.status+' @ '+url);
    return r.json();
  });
}""" % ("'/api/sns/web/v2/search/notes'" if site == "xhs" else "'/rest/v/search/feed'")
                  ).replace("BODY_TMPL", cfg["body"]))
        js.append(("""
function renderCards(items, append){
  var cont=contEl();
  if(!append){cont.innerHTML='';}
  cont.setAttribute('data-keyword', state.keyword);
  items.forEach(function(w){ cont.appendChild(cardNode(fields(w))); });
  document.getElementById('cardCount').textContent=cont.children.length;
}
function loadMore(){
  if(state.loading||state.done) return;
  var myseq=state.seq;               // 代际守卫：过期响应（换词瞬间）直接丢弃
  state.loading=true; setStatus('加载中…');
  fetchPage(state.keyword).then(function(resp){
    if(myseq!==state.seq){return;}   // 已发起新搜索，本页作废
    var items=ITEMS_EXPR;
    renderCards(items, true);
    var more=HAS_MORE_EXPR;
    AFTER;
    if(!more){state.done=true;setStatus('没有更多了');}
    else setStatus('已加载 '+contEl().children.length+' 条');
  }).catch(function(e){if(myseq===state.seq)setStatus('加载失败: '+e.message)})
    .then(function(){if(myseq===state.seq)state.loading=false});
}
function doSearch(kw){
  state={page:1,cursor:0,searchId:'',pcursor:'',session:'',keyword:kw,loading:false,done:false,seq:(state.seq||0)+1};
  document.title=kw+' - '+BRAND;   /* 红队 R3-P1-3：搜索页 title 对齐真站「<kw> - <品牌>」形态 */
  var cont=contEl(); cont.innerHTML=''; cont.setAttribute('data-keyword',kw);
  document.getElementById('cardCount').textContent='0';
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
            .replace("INIT_KW",
                     ("(function(){var q=new URLSearchParams(location.search);"
                      "if(q.has('keyword'))return q.get('keyword');"
                      "if(q.has('searchKey'))return q.get('searchKey');"
                      "var m=location.pathname.match(/^\\/search\\/(.+)$/);"
                      "if(m){try{return decodeURIComponent(m[1])}catch(e){return m[1]}}"
                      "return DEFAULT_KW_JS;})()")
                     if page == "search" else "DEFAULT_KW_JS")
            .replace("DEFAULT_KW_JS", json.dumps(DEFAULT_KEYWORDS[site], ensure_ascii=False)))
    else:  # detail
        # 红队 P2-D6/P3-4 + R3-P1-2：id 同时支持 ?id= 与真站路径形态 /video/<id>（dy）、
        # /explore/<id>（xhs）、/short-video/<id>（ks）
        id_pat = ('/^\\/video\\/([0-9A-Za-z_-]+)$/' if site == "douyin"
                  else '/^\\/explore\\/([0-9a-fA-F]+)$/' if site == "xhs"
                  else '/^\\/short-video\\/([0-9A-Za-z_-]+)$/')
        if site == "douyin":
            # dy 详情数据走真路径 XHR /aweme/detail（语料实证：详情页网络即此端点）
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
    fetch('/aweme/v1/web/comment/list/?aweme_id='+enc(id)+'&cursor=0&count=5&item_type=0&cut_version=1')
      .then(function(r){return r.ok?r.json():Promise.reject(new Error('HTTP '+r.status))})
      .then(function(j){renderDyComments(j.comments||[])})
      .catch(function(e){console.warn('comment/list:', e)});
  }"""
        elif site == "xhs":
            # xhs 详情数据 SSR 内嵌（语料实证：详情页无 note XHR，仅 comment/page）
            detail_fetch = """
  var id=detailId();
  var st=window.__INITIAL_STATE__;
  if(st&&st.entity){ renderDetail(st.entity); }
  else if(id){ renderDetailError(); }
  if(id){
    fetch('/api/sns/web/v2/comment/page?note_id='+enc(id)+'&cursor=&top_comment_id=&image_formats=jpg,webp,avif&xsec_token='
          +enc((st&&st.entity&&st.entity.xsec_token)||''))
      .then(function(r){return r.ok?r.json():Promise.reject(new Error('HTTP '+r.status))})
      .then(function(resp){ if(resp.data&&resp.data.comments){renderComments(resp.data.comments);} })
      .catch(function(e){console.warn('comment_page:', e)});
  }"""
        else:
            # ks 详情数据 SSR 内嵌 + 评论走 GraphQL commentListQuery（真站主面）
            detail_fetch = """
  var st=window.__INITIAL_STATE__;
  if(st&&st.entity){ renderDetail(st.entity); }
  else { renderDetailError(); }
  var pid=detailId()||(st&&st.entity&&st.entity.photo&&st.entity.photo.id);
  if(pid){
    fetch('/graphql',{method:'POST',headers:{'Content-Type':'application/json'},
      body:JSON.stringify({operationName:'commentListQuery',
        query:'query commentListQuery($photoId:String,$pcursor:String){visionCommentList(photoId:$photoId,pcursor:$pcursor){commentCount commentCountV2 pcursor rootCommentsV2{commentId authorId authorName content timestamp likedCount}}}',
        variables:{photoId:String(pid),pcursor:''}})})
      .then(function(r){return r.ok?r.json():Promise.reject(new Error('HTTP '+r.status))})
      .then(function(j){ var l=(j&&j.data&&j.data.visionCommentList)||{};
        renderKsComments(l.rootCommentsV2||[]); })
      .catch(function(e){console.warn('commentListQuery:', e)});
  }"""
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
    return "\n".join(js)


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
  renderComments(e.comments||[]);
  setStatus('note_id='+e.id);
}
/* 红队 P2-X5：真站对不存在笔记 302 → /404；服务端已 302，此处为页面侧兜底 */
function renderDetailError(){
  location.replace('/404?source='+enc('/404/sec_pVswaPpO?redirectPath='+location.pathname)
    +'&error_code=300031&error_msg='+enc('当前笔记暂时无法浏览'));
}
function renderComments(list){
  var box=document.getElementById('commentList'); if(!box)return;
  box.innerHTML='';
  (list||[]).slice(0,10).forEach(function(c){
    var it=el('div','comment-item');
    it.appendChild(el('div','c-user',((c.user_info&&c.user_info.nickname)||'匿名')+(c.ip_location?(' · '+c.ip_location):'')));
    it.appendChild(el('div','c-content',c.content||''));
    it.appendChild(el('div','c-like like-count','赞 '+fmt(c.like_count)));
    box.appendChild(it);
  });
  document.getElementById('commentCount').textContent=(list||[]).length;
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
}
function renderKsComments(list){
  var box=document.getElementById('commentList'); if(!box)return;
  box.innerHTML='';
  (list||[]).slice(0,10).forEach(function(c){
    var it=el('div','comment-item');
    it.appendChild(el('div','c-user',(c.authorName||'匿名')+(c.timestamp?(' · '+new Date(c.timestamp).toLocaleDateString()):'')));
    it.appendChild(el('div','c-content',c.content||''));
    it.appendChild(el('div','c-like like-count','赞 '+fmt(c.likedCount)));
    box.appendChild(it);
  });
  document.getElementById('commentCount').textContent=(list||[]).length;
}"""


def dy_comments_renderer() -> str:
    return """
function renderDyComments(list){
  var box=document.getElementById('commentList'); if(!box)return;
  box.innerHTML='';
  (list||[]).slice(0,10).forEach(function(c){
    var it=el('div','comment-item');
    it.appendChild(el('div','c-user',((c.user&&c.user.nickname)||'匿名')+(c.ip_location?(' · '+c.ip_location):'')));
    it.appendChild(el('div','c-content',c.text||''));
    it.appendChild(el('div','c-like like-count','赞 '+fmt(c.digg_count)));
    box.appendChild(it);
  });
  var n=document.getElementById('commentCount'); if(n)n.textContent=(list||[]).length;
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
    """作者主页骨架（红队 R3-P2-3）：SSR 数据注入 __INITIAL_STATE__（synth_api 侧替换）。"""
    detail_href = {"douyin": "'/video/'+enc(w.id)", "xhs": "'/explore/'+enc(w.id)",
                   "kuaishou": "'/short-video/'+enc(w.id)"}[site]
    js = """
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
  box.innerHTML='';
  (st.works||[]).forEach(function(w){
    var d=el('div','card-container work-card');
    var link=el('a','card-link'); link.href=%s;
    var cov=el('div','cover');
    var img=new Image(); img.alt=w.title; img.loading='lazy'; img.src=coverUrl(w.id,(w.title||'').slice(0,2));
    cov.appendChild(img);
    var info=el('div','card-info');
    info.appendChild(el('div','title',w.title||'(无描述)'));
    var m=el('div','metrics');
    if(w.like!=null)m.appendChild(el('span','like-count','赞 '+fmt(w.like)));
    if(w.view)m.appendChild(el('span','play-count',w.view));
    info.appendChild(m);
    link.appendChild(cov); link.appendChild(info); d.appendChild(link); box.appendChild(d);
  });
  document.getElementById('profileStatus').textContent='共 '+((st.works||[]).length)+' 个作品';
}""" % detail_href
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
    <span class="status">卡片数：<span id="cardCount">0</span></span>
  </div>
  {container}
  <div class="toolbar"><button class="load-more" id="loadMoreBtn2" type="button">加载更多</button></div>
""".replace("{container}", CONTAINERS[site])
        init = "initSearch();document.getElementById('loadMoreBtn').onclick=loadMore;document.getElementById('loadMoreBtn2').onclick=loadMore;"
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
        comments = ('<div class="comments"><h3>评论（<span id="commentCount">0</span>）'
                    '</h3><div id="commentList"></div></div>')
        body = body.replace("{comments}", comments)
        init = "initDetail();"

    js = build_js(site, page)
    if page == "detail":
        js += "\n" + detail_renderer(site)
        if site == "douyin":
            js += "\n" + dy_comments_renderer()
    elif page in ("home", "search"):
        js += "\n" + CARD_RENDERERS[site]

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
        "__TITLE__": SITE_TITLES[site],
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
        # 三站生成作者主页骨架（红队 R3-P2-3：/user/<sec_uid>、/user/profile/<uid>、/profile/<id>）
        pages = ("home", "search", "detail", "profile", "error404") if site == "xhs" \
            else ("home", "search", "detail", "profile")
        for page in pages:
            dest = out_dir / site / ("404.html" if page == "error404" else "%s.html" % page)
            dest.parent.mkdir(parents=True, exist_ok=True)
            content = error404_page(site, provenance) if page == "error404" \
                else (profile_page(site, provenance) if page == "profile"
                      else build_page(site, page, provenance))
            dest.write_text(content, encoding="utf-8")
            print("written: %s (%d bytes)" % (dest, dest.stat().st_size))
        print("[%s] 脱敏 DOM 对照：录制卡片 %d 张；骨架卡片结构 = %s" % (site, n_dom_cards,
                                                              CONTAINERS[site].split()[1]))
    print("DONE %s（选择器校验%s）" % (out_dir, "通过" if ok else "有缺失项，见上方 0 次警告"))
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
