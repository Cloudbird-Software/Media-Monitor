"""参考值池 + 实体一致性池（Faker zh_CN 基座）。

- 作者/用户池在数据集内一次性确定性生成：同一 author_id 的昵称、头像、sec_uid 跨条目完全一致
- 标题/话题/标签/评论模板来自 data/*.json（快照类字段从参考值池采样）
"""
from __future__ import annotations

import numpy as np
from faker import Faker

from synthgen import ids


class SiteContext:
    """站点生成上下文：池 + 引擎 + 配置。"""

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
        self._build_author_pool()
        self._build_title_cums()

    # ---------- 作者池 ----------
    def _build_author_pool(self):
        n = int(self.sc.get("author_pool", 30000))
        rng = np.random.default_rng([self.seed, 9000 + self.site_code])
        fk = Faker(locale="zh_CN")
        Faker.seed(self.seed * 7 + self.site_code)
        fcfg = self.sc.get("follower", {"mu": 8.0, "sigma": 1.8, "min": 0})
        authors = []
        for j in range(n):
            followers = int(max(rng.lognormal(fcfg["mu"], fcfg["sigma"]), fcfg.get("min", 0)))
            # uid 长度分层抽样位：池内下标 × 黄金比例（低差异，相位 0.75 落 16 位众数段）——
            # 记录级分布被作者热度幂律加权（头部作者占比高），分层后池级分布严格贴合
            # 语料全局分布（{11:27.6,12:9.4,14:0.7,15:10.1,16:44.8,19:7.4}%），
            # 记录级保持众数 16 且各长度均有出现
            u_len = (0.75 + j * 0.6180339887498949) % 1.0
            a = self._make_author(rng, fk, followers, uid_stratum=u_len)
            authors.append(a)
        self.authors = authors
        # 幂律热度：作者被作品引用的分布（少数高产作者占比高 → 跨条目一致性可被抽查）
        shape = float(self.sc.get("author_popularity_shape", 1.8))
        w = 1.0 / np.power(np.arange(1, n + 1, dtype=float), shape)
        self._author_cum = np.cumsum(w) / w.sum()

    def _make_author(self, rng: np.random.Generator, fk: Faker, followers: int) -> dict:
        raise NotImplementedError

    def pick_author(self, rng: np.random.Generator) -> dict:
        i = ids.pick_weighted_index(rng, self._author_cum)
        return self.authors[i]

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
    def _make_author(self, rng, fk, followers, uid_stratum=None):
        p = self.pools
        style = rng.integers(0, 4)
        if style == 0:
            nickname = fk.name()  # 真名型昵称
        elif style == 1:
            nickname = ids.pick(rng, p["nickname_prefix"]) + ids.pick(rng, p["nickname_main"])
        elif style == 2:
            nickname = ids.pick(rng, p["nickname_main"]) + ids.pick(rng, p["nickname_suffix"])
        else:
            nickname = (
                ids.pick(rng, p["nickname_prefix"])
                + ids.pick(rng, p["nickname_main"])
                + ids.pick(rng, p["nickname_suffix"])
                + ids.pick(rng, p["nickname_emoji"])
            )
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
    def _make_author(self, rng, fk, followers, uid_stratum=None):
        p = self.pools["nickname_styles"]
        style = rng.integers(0, 5)
        if style == 0:
            nickname = ids.pick(rng, p["english"])
        elif style == 1:
            nickname = fk.name()
        elif style == 2:
            nickname = ids.pick(rng, p["prefix"]) + ids.pick(rng, p["main"])
        elif style == 3:
            nickname = ids.pick(rng, p["main"]) + ids.pick(rng, p["suffix"])
        else:
            nickname = (
                ids.pick(rng, p["prefix"]) + ids.pick(rng, p["main"]) + ids.pick(rng, p["emoji"])
            )
        uid = ids.hex_id(rng, 24)
        return {
            "user_id": uid,
            "nickname": nickname,
            "avatar": f"https://sns-avatar-qc.xhscdn.com/avatar/{ids.opaque(rng, 24)}",
            "followers": followers,
        }


class KuaishouContext(SiteContext):
    def _make_author(self, rng, fk, followers, uid_stratum=None):
        p = self.pools
        style = rng.integers(0, 3)
        if style == 0:
            nickname = fk.name()
        elif style == 1:
            nickname = ids.pick(rng, p["nickname_prefix"]) + ids.pick(rng, p["nickname_main"])
        else:
            nickname = (
                ids.pick(rng, p["nickname_prefix"])
                + ids.pick(rng, p["nickname_main"])
                + ids.pick(rng, p["nickname_suffix"])
            )
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
