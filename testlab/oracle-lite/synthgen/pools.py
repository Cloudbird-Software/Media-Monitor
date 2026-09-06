"""参考值池 + 实体一致性池（Faker zh_CN 基座）。

- 作者/用户池在数据集内一次性确定性生成：同一 author_id 的昵称、头像、sec_uid 跨条目完全一致
- 标题/话题/标签来自 data/*.json（快照类字段从参考值池采样）
- 红队 round5A R5A-P1-1 修复：
  * 作者作品数服从对数正态长尾（绝大多数作者 1~3 条、少数高产），
    并以「份额上限注水」保证 top1 作者占全库 ≤ share_cap（旧 Zipf(1.8) 幂律
    引用权重使 top1 占 ~54%、有效作者仅 ~1e2，与语料 94% 卡片作者唯一严重背离）；
  * 昵称组合生成 + 池内唯一化（不同 author_id 不同昵称，作者维度统计不因
    昵称撞车而合并）；
  * 窗口级约束：任意 40 连续 slot 内同作者 ≤2（「指定重复对」每 40 slot 恰一对
    相邻同作者 + 其余 slot 对最近 39 slot 拒绝重抽）——对齐语料
    「20 卡窗口 94% 作者唯一（至多一对重复）」的窗口生态。
"""
from __future__ import annotations

import hashlib
import math
from collections import deque

import numpy as np
from faker import Faker

from synthgen import ids

# 窗口级作者调度参数（R5A-P1-1）
_PAIR_PERIOD = 40   # 指定重复对周期：每 40 个 slot 恰有一对相邻同作者
_PAIR_SPAN = 19     # 对首位置 ∈ [0, _PAIR_SPAN)：对占位 ≤ 19 ⇒ 任意 20/40 对齐窗口
                    # （起点为 20 的倍数）恰含一对完整重复 → 作者聚合 argmax 恒唯一
_LOOKBACK = 39      # 非对 slot 的新作者不得出现在最近 39 个 slot

# 池内昵称唯一化的兜底后缀（组合空间足够大，仅极端撞车时启用）
_NICK_FALLBACK = ["二号", "真身", "本尊", "Pro", "Plus", "日常版", "在线", "呀"]

# ---------------------------------------------------------------------------
# R6A-P2-3③：hashtag 池扩容（语料 dy 600 文本 1525 个不同 tag ≈2.5/文本、
# ks 1080 个；旧池每类目仅 2-6 个 → 单位文本 tag 多样性低 ~8 倍）+
# 平台活动/运营 tag（语料 top tag 为「抖音AI创作大赛/开放赛道/喜爱度激励计划/
# 快成长计划」等活动运营族，旧池全缺，top 全是题材词）。
# 扩容确定性：题材词 × 后缀 组合 + 类目词 × 后缀，按稳定顺序截断。
# ---------------------------------------------------------------------------
# R7A-P3-2：去掉「避雷/天花板」后缀（话题替换把 hashtag 注入评论文本，这两个
# 网络用语后缀使 dy 评论网络用语密度 +1.1%，超出语料带 ±30%）
_TAG_SUFFIXES = ["打卡", "日常", "合集", "攻略", "分享", "教程", "日记", "推荐",
                 "记录", "测评", "清单", "上手", "vlog", "指南"]

# 语料/任务线高频搜索词（关键词→类目命中供给：render 搜索相关性用）
# R7A-P3-1：harness 关键词 ladder 14 词全部纳入映射（美食教程/沙雕日常/变美/手机摄影/
# 蓝牙耳机测评/夜景人像/露营装备 原未映射——评测主词「美食教程」在 xhs 6 个、
# ks 7 个任务的搜索面拿到的是全无关背景，严格命中 0.0 vs 真站 17-47%）；
# 另并入红队探针词 赶海/汉服/汽车测评/母婴好物。
_KEYWORD_TOPICS = {
    "美食探店": ["美食探店", "美食教程", "咖啡拉花", "家常菜", "探店"],
    "旅行": ["旅行攻略", "说走就走的旅行", "露营装备", "赶海"],
    "穿搭": ["穿搭分享", "穿搭", "汉服"],
    "美妆护肤": ["美妆教程", "护肤分享", "变美"],
    "健身": ["健身打卡", "健身"],
    "萌宠": ["萌宠日常"],
    "数码科技": ["数码测评", "数码好物", "手机摄影", "夜景人像",
                 "蓝牙耳机测评", "汽车测评"],
    "家居装修": ["家居好物", "家居"],
    "职场": ["职场干货"],
    "学习干货": ["学习方法"],
    "搞笑剧情": ["搞笑视频", "沙雕日常"],
    "母婴育儿": ["育儿日常", "母婴好物"],
}

# 平台活动/运营 tag（按语料话题池形态造；标题 tag 尾缀簇）
_ACTIVITY_TAGS = {
    "douyin": ["抖音AI创作大赛", "开放赛道", "喜爱度激励计划", "快成长计划",
               "全民任务", "春日灵感计划", "抖音小助手", "创作灵感", "上热门",
               "每日打卡挑战", "新人报道", "Dou来上新菜"],
    "kuaishou": ["光合计划", "快手创作者中心", "万能生活指南", "快手小剧场",
                 "磁力万象", "快手百分百", "老铁共创计划", "村口大舞台",
                 "每日一更", "快手美食季", "人间烟火气", "家乡好物"],
    "xhs": [],   # xhs 无 hashtag 池（笔记标题形态，语料未捕获 tag 尾缀簇）
}
_ACTIVITY_TAG_FRAC = 0.30   # 每条记录附带 1 个活动 tag 的概率（语料 top tag ≈7%/tag）

# 标题 unicode emoji（R6A-P2-3：dy desc 8.8% / ks caption 4.0% 含 emoji，
# 语料样例「乡村厨房🏠…🍳」；xhs 标题 emoji 已由模板自带 42.5%）
_TITLE_EMOJI = ["🏠", "🍳", "✨", "🔥", "😍", "😭", "🥹", "🐶", "🐱", "🌈",
                "🍜", "⛰️", "📸", "💪", "🌞", "🍂", "🧋", "🎒", "🎣", "灶"]


def _expand_tag_pool(base: list, topics: list, cat: str) -> list:
    """类目 tag 池扩容：base ∪ 关键词供给 ∪ 题材×后缀 ∪ 类目词×后缀（确定性）。"""
    out, seen = [], set()

    def add(t: str):
        if t and t not in seen:
            seen.add(t)
            out.append(t)

    for t in base:
        add(t)
    for t in _KEYWORD_TOPICS.get(cat, []):
        add(t)
    for topic in topics:
        for suf in _TAG_SUFFIXES:
            add(topic + suf)
    for suf in _TAG_SUFFIXES:
        add(cat + suf)
    return out[:52]   # 每类目 ~50（旧 2-6 → 8 倍以上）


def _stable_hash(s: str) -> int:
    return int.from_bytes(hashlib.blake2b(s.encode("utf-8"), digest_size=8).digest(), "big")


class SiteContext:
    """站点生成上下文：池 + 引擎 + 配置。"""

    NICK_FIELD = "nickname"   # ks 为 "name"

    def __init__(self, site: str, site_code: int, seed: int, dist_cfg: dict, categories: list[dict]):
        from synthgen.config import load_site_pools
        from synthgen.distengine import DistEngine

        self.site = site
        self.site_code = site_code
        self.seed = seed
        self.pools = load_site_pools(site)
        self.categories = categories
        self.engine = DistEngine(dist_cfg, site, site_code, seed, categories)
        self.sc = dist_cfg["sites"][site]
        self._expand_pools_r6a()
        self._build_author_pool()
        self._build_title_cums()
        # 窗口调度状态（顺序生成时确定性演进 → 同种子逐字节可复现）
        self._recent: deque = deque(maxlen=_LOOKBACK)
        self._recent_set: set = set()
        self._slot = 0
        self._last_j = 0
        self._emitted: list = []   # 已落盘记录的作者池下标（评论者实体可回查用）

    # ---------- R6A-P2-3：池扩容（hashtag ×8+ / 活动 tag / 关键词供给） ----------
    def _expand_pools_r6a(self):
        p = self.pools
        # 话题池补关键词供给（搜索严格命中的标题供给，render 相关性采样用）
        for cat, words in _KEYWORD_TOPICS.items():
            lst = p["topics"].get(cat)
            if lst is None:
                continue
            for w in words:
                if w not in lst and w not in (p.get("hashtags") or p.get("tags_pool") or {}).get(cat, []):
                    lst.append(w)
        # hashtag/tags_pool 每类目扩到 ~50
        key = "hashtags" if "hashtags" in p else ("tags_pool" if "tags_pool" in p else None)
        if key is not None:
            for cat in list(p[key]):
                p[key][cat] = _expand_tag_pool(p[key][cat], p["topics"].get(cat, []), cat)
        self.activity_tags = list(_ACTIVITY_TAGS.get(self.site, []))

    def pick_activity_tag(self, rng: np.random.Generator) -> str | None:
        """平台活动 tag（R6A-P2-3③）：每条记录 ~30% 附 1 个（语料 top tag 形态）。"""
        pool = getattr(self, "activity_tags", None) or _ACTIVITY_TAGS.get(self.site) or []
        if not pool or rng.random() >= _ACTIVITY_TAG_FRAC:
            return None
        return ids.pick(rng, pool)

    def title_emoji(self, rng: np.random.Generator, rate: float) -> str:
        """标题 unicode emoji 后缀（R6A-P2-3：dy 8.8% / ks 4.0% 语料率）。"""
        if rate <= 0 or rng.random() >= rate:
            return ""
        e = ids.pick(rng, _TITLE_EMOJI)
        return e + (ids.pick(rng, _TITLE_EMOJI) if rng.random() < 0.25 else "")

    # ---------- 作者池 ----------
    def _build_author_pool(self):
        n = int(self.sc.get("author_pool", 30000))
        rng = np.random.default_rng([self.seed, 9000 + self.site_code])
        fk = Faker(locale="zh_CN")
        Faker.seed(self.seed * 7 + self.site_code)
        fcfg = self.sc.get("follower", {"mu": 8.0, "sigma": 1.8, "min": 0})
        # R5A-P1-1：作品数长尾（对数正态）+ 份额上限注水，替代旧 Zipf(1.8) 引用幂律
        wcfg = self.sc.get("author_works", {"mean_works": 3.4, "sigma": 2.2, "share_cap": 0.016})
        sigma = float(wcfg.get("sigma", 2.2))
        mu = math.log(float(wcfg.get("mean_works", 3.4))) - 0.5 * sigma * sigma
        w = rng.lognormal(mu, sigma, size=n)
        w = w / w.sum()
        cap = float(wcfg.get("share_cap", 0.016))
        for _ in range(24):   # 注水迭代：截断头部份额 → 归一 → 收敛（和恒 1，max ≤ cap(1+ε)）
            w = np.minimum(w, cap)
            s = float(w.sum())
            if s <= 0:
                break
            w = w / s
            if float(w.max()) <= cap * 1.0005:
                w = np.minimum(w, cap)
                break
        w = w / w.sum()
        self._author_cum = np.cumsum(w)
        authors = []
        seen_nick: set = set()
        for j in range(n):
            followers = int(max(rng.lognormal(fcfg["mu"], fcfg["sigma"]), fcfg.get("min", 0)))
            # uid 长度分层抽样位：池内下标 × 黄金比例（低差异，相位 0.75 落 16 位众数段）——
            # 记录级分布贴合语料全局分布（{11:27.6,12:9.4,14:0.7,15:10.1,16:44.8,19:7.4}%）
            u_len = (0.75 + j * 0.6180339887498949) % 1.0
            a = self._make_author(rng, fk, followers, uid_stratum=u_len)
            # 昵称池内唯一化（R5A-P1-1）：不同 author_id 不同昵称，作者维度统计不被撞名合并
            nick = str(a.get(self.NICK_FIELD) or "")
            if nick in seen_nick:
                nick = self._unique_nickname(rng, fk, seen_nick)
                a[self.NICK_FIELD] = nick
            seen_nick.add(nick)
            authors.append(a)
        self.authors = authors

    def _make_author(self, rng: np.random.Generator, fk: Faker, followers: int) -> dict:
        raise NotImplementedError

    def _make_nickname(self, rng: np.random.Generator, fk: Faker) -> str:
        raise NotImplementedError

    def _unique_nickname(self, rng: np.random.Generator, fk: Faker, seen: set) -> str:
        """昵称池内唯一：重试组合生成，极端撞车加自然后缀（后缀结果同样查重）。"""
        for _ in range(10):
            cand = self._make_nickname(rng, fk)
            if cand not in seen:
                return cand
        base = self._make_nickname(rng, fk)
        for suf in _NICK_FALLBACK + [str(k) for k in range(2026, 2126)]:
            cand = base + suf
            if cand not in seen:
                return cand
        return base + str(len(seen))

    # ---------- 作者调度（R5A-P1-1 窗口约束） ----------
    def _pair_head_pos(self, block: int) -> int:
        return _stable_hash("pairpos::%d::%d::%d" % (self.seed, self.site_code, block)) % _PAIR_SPAN

    def _draw_fresh(self, rng: np.random.Generator) -> int:
        """从长尾权重采样且不得落入最近 39 slot（确定性拒绝重抽）。"""
        for _ in range(96):
            j = ids.pick_weighted_index(rng, self._author_cum)
            if j not in self._recent_set:
                return j
        for j in range(len(self.authors)):   # 兜底：池中段确定性扫描
            if j not in self._recent_set:
                return j
        return 0

    def _push_recent(self, j: int):
        if len(self._recent) == _LOOKBACK:
            old = self._recent[0]
            self._recent_set.discard(old)
        self._recent.append(j)
        self._recent_set.add(j)

    def pick_author(self, rng: np.random.Generator, index: int | None = None) -> dict:
        """第 index 条记录的作者（index 缺省时按内部 slot 推进）。

        调度：每 40 slot 恰一对相邻同作者（对首位置由 hash(block) ∈ [0,20) 决定）；
        其余 slot 拒绝最近 39 slot 内出现过的作者 ⇒ 任意 40 连续记录内同作者 ≤2、
        任意 20 连续窗口（页窗）内同作者 ≤2，且 40 对齐窗口恰有一对作者重复
        （作者聚合类任务的 argmax 严格唯一在数据层面得到保证）。
        """
        i = self._slot if index is None else int(index)
        self._slot = max(self._slot, i + 1)
        pos = i % _PAIR_PERIOD
        head = self._pair_head_pos(i // _PAIR_PERIOD)
        if pos == head + 1 and len(self._recent) > 0:
            j = self._recent[-1]        # 指定对第二张卡：与对首同作者
            self._last_j = j
            return self.authors[j]
        j = self._draw_fresh(rng)
        self._push_recent(j)
        self._last_j = j
        return self.authors[j]

    def mark_emitted(self):
        """记录「已通过过滤、确定落盘」的上一条作者（评论者从已落盘实体中取）。"""
        self._emitted.append(self._last_j)

    def pick_commenter(self, rng: np.random.Generator) -> dict:
        """评论者实体：从已落盘作品的作者池中确定性采样（保证跨端点可回查）。"""
        pool = self._emitted or [self._last_j]
        return self.authors[pool[int(rng.integers(0, len(pool)))]]

    # ---------- 标题池 ----------
    def _build_title_cums(self):
        topics = self.pools["topics"]
        self._topic_lists = {cat: lst for cat, lst in topics.items()}
        self._topic_cums = {}
        for cat, lst in topics.items():
            w = np.ones(len(lst), dtype=float)
            self._topic_cums[cat] = np.cumsum(w) / w.sum()

    def pick_topic(self, rng: np.random.Generator, category: str) -> str:
        lst = self._topic_lists[category]
        cum = self._topic_cums[category]
        return lst[int(np.searchsorted(cum, rng.random(), side="right"))]

    def make_title(self, rng: np.random.Generator, category: str) -> str:
        tpl = ids.pick(rng, self.pools["title_templates"][category])
        topic = self.pick_topic(rng, category)
        return tpl.replace("{topic}", topic)

    def make_tags(self, rng: np.random.Generator, category: str, k: int) -> list[str]:
        key = "hashtags" if "hashtags" in self.pools else "tags_pool"
        pool = list(self.pools[key][category])
        k = min(k, len(pool))
        idx = rng.permutation(len(pool))[:k]
        return [pool[i] for i in idx]


class DouyinContext(SiteContext):
    def _make_nickname(self, rng: np.random.Generator, fk: Faker) -> str:
        p = self.pools
        style = rng.integers(0, 4)
        if style == 0:
            return fk.name()  # 真名型昵称
        if style == 1:
            return ids.pick(rng, p["nickname_prefix"]) + ids.pick(rng, p["nickname_main"])
        if style == 2:
            return ids.pick(rng, p["nickname_main"]) + ids.pick(rng, p["nickname_suffix"])
        return (
            ids.pick(rng, p["nickname_prefix"])
            + ids.pick(rng, p["nickname_main"])
            + ids.pick(rng, p["nickname_suffix"])
            + ids.pick(rng, p["nickname_emoji"])
        )

    def _make_author(self, rng, fk, followers, uid_stratum=None):
        p = self.pools
        nickname = self._make_nickname(rng, fk)
        # R2-P2-2：uid 长度/首位按语料分布（435 样本：众数 16 位，首位 1/2/3 偏多）
        uid = ids.dy_uid(rng, u=uid_stratum)
        uri = ids.dy_uri(rng, 69)
        hosts = ["p3-pc.douyinpic.com", "p3.douyinpic.com"]
        url = f"https://{ids.pick(rng, hosts)}/aweme/100x100/aweme-avatar/{uri}"
        custom_verify = ids.pick(rng, p["custom_verify_pool"])
        enterprise = (
            ids.pick(rng, p["enterprise_verify_reason_pool"])
            if custom_verify == "" and rng.random() < 0.6
            else ""
        )
        return {
            "uid": uid,
            "sec_uid": ids.dy_sec_uid(rng),
            "nickname": nickname,
            "avatar_uri": uri,
            "avatar_urls": [url],
            "followers": followers,
            "total_favorited": int(followers * rng.uniform(2, 40)),
            "custom_verify": custom_verify,
            "enterprise_verify_reason": enterprise,
        }


class XhsContext(SiteContext):
    def _make_nickname(self, rng: np.random.Generator, fk: Faker) -> str:
        p = self.pools["nickname_styles"]
        style = rng.integers(0, 5)
        if style == 0:
            return ids.pick(rng, p["english"])
        if style == 1:
            return fk.name()
        if style == 2:
            return ids.pick(rng, p["prefix"]) + ids.pick(rng, p["main"])
        if style == 3:
            return ids.pick(rng, p["main"]) + ids.pick(rng, p["suffix"])
        return (
            ids.pick(rng, p["prefix"])
            + ids.pick(rng, p["main"])
            + ids.pick(rng, p["emoji"])
        )

    def _make_author(self, rng, fk, followers, uid_stratum=None):
        nickname = self._make_nickname(rng, fk)
        uid = ids.hex_id(rng, 24)
        return {
            "user_id": uid,
            "nickname": nickname,
            "avatar": f"https://sns-avatar-qc.xhscdn.com/avatar/{ids.opaque(rng, 24)}",
            "followers": followers,
        }


class KuaishouContext(SiteContext):
    NICK_FIELD = "name"

    def _make_nickname(self, rng: np.random.Generator, fk: Faker) -> str:
        p = self.pools
        style = rng.integers(0, 3)
        if style == 0:
            return fk.name()
        if style == 1:
            return ids.pick(rng, p["nickname_prefix"]) + ids.pick(rng, p["nickname_main"])
        return (
            ids.pick(rng, p["nickname_prefix"])
            + ids.pick(rng, p["nickname_main"])
            + ids.pick(rng, p["nickname_suffix"])
        )

    def _make_author(self, rng, fk, followers, uid_stratum=None):
        nickname = self._make_nickname(rng, fk)
        head = (
            f"https://p{int(rng.integers(60, 90))}.a.kwimgs.com/uhead/"
            f"{ids.hex_id(rng, 2).upper()}/2026/{int(rng.integers(1,13)):02d}/"
            f"{int(rng.integers(1,29)):02d}/{int(rng.integers(0,24)):02d}/{ids.dy_uri(rng, 16)}"
        )
        return {
            "id": ids.ks_id(rng),
            "name": nickname,
            "header_url": head,
            "followers": followers,
        }


_CONTEXTS = {"douyin": DouyinContext, "xhs": XhsContext, "kuaishou": KuaishouContext}


def build_context(site: str, site_code: int, seed: int, dist_cfg: dict, categories: list[dict]) -> SiteContext:
    return _CONTEXTS[site](site, site_code, seed, dist_cfg, categories)
