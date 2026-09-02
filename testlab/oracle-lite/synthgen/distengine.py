"""分布引擎：view(对数正态·长尾) → like(view×Beta互动率×类目) → comment/collect/share(与 like 一致性约束)
→ publish_time(幂律近期加权) + 异常类变换。

数值关系（契约一致性约束）：
    like    = view × like_rate，like_rate ~ Beta(a,b) × 类目互动率乘子（异常类再乘/除倍率）
    comment = like × ratio_c，且 comment ≤ like × cap_comment（默认 0.30）
    collect = like × ratio_k，且 collect ≤ like × cap_collect
    share   = like × ratio_s，且 share   ≤ like × cap_share
"""
from __future__ import annotations

import numpy as np
from scipy.stats import norm

from synthgen import anomalies as anom
from synthgen.rngutil import inverse_cdf_pick


def _ratio_cfg(cfg: dict, site_cfg: dict, name: str) -> dict:
    base = cfg["defaults"]["ratios"][name]
    override = (site_cfg.get("ratios") or {}).get(name)
    return {**base, **override} if override else dict(base)


def clamp_comment(comment: int, like: int, cap_frac: float = 0.30) -> int:
    """最终落盘不变式钳制：comment ≤ floor(like × cap_frac)。

    所有倍率 / 池采样 / 取整之后、写出之前统一调用（含快手 us_c 独立池采样路径）。
    like×cap_frac < 1 时钳制上限为 0 —— 小赞量内容（like<4）评论取 0 属真实形态，
    三站统一该口径，保证不变式 comment ≤ like×0.3 严格成立（QA P2-1 修复）。
    """
    cap = int(like * cap_frac)  # floor（like、cap_frac 均 ≥ 0）
    return max(0, min(int(comment), cap))


class DistEngine:
    """单站点分布引擎（不可变配置，记录级无共享状态，可安全顺序调用）。"""

    def __init__(self, cfg: dict, site: str, site_code: int, seed: int, categories: list[dict]):
        self.site = site
        self.site_code = site_code
        self.seed = seed
        self.sc = cfg["sites"][site]
        self.categories = categories
        # 类目加权采样（逆 CDF）
        w = np.array([c["weight"] for c in categories], dtype=float)
        self._cat_cum = np.cumsum(w) / w.sum()
        # 异常类配置
        self.anomaly_cfg = cfg.get("anomalies", {})
        # 锚定时间
        from synthgen.config import anchor_epoch
        self.anchor = anchor_epoch(cfg)
        # 幂律时间参数
        pl = cfg["time"]["powerlaw"]
        self._pl_scale = float(pl["scale_days"])
        self._pl_shape = float(pl["shape"])
        self._pl_max_age = float(pl["max_age_days"])
        # 比率配置（站点级覆盖）
        self._ratios = {n: _ratio_cfg(cfg, self.sc, n) for n in ("comment", "collect", "share")}
        self.view_cfg = self.sc["view"]
        self.like_cfg = self.sc["like"]

    # ---- 基础采样 ----
    def sample_category(self, rng: np.random.Generator) -> dict:
        return self.categories[inverse_cdf_pick(rng.random(), self._cat_cum)]

    def sample_age_days(self, rng: np.random.Generator) -> float:
        """幂律（Pareto 型）发布年龄：近期加权、长尾到 max_age_days。"""
        u = rng.random()
        while u <= 1e-12:
            u = rng.random()
        age = self._pl_scale * (u ** (-1.0 / self._pl_shape) - 1.0)
        return float(min(age, self._pl_max_age))

    def _lognormal_view(self, rng: np.random.Generator, qrange: tuple[float, float] | None) -> float:
        mu, sigma = float(self.view_cfg["mu"]), float(self.view_cfg["sigma"])
        if qrange is None:
            v = float(rng.lognormal(mu, sigma))
        else:
            q0, q1 = qrange
            u = q0 + (q1 - q0) * rng.random()
            v = float(np.exp(mu + sigma * norm.ppf(u)))
        return max(v, float(self.view_cfg.get("min", 1)))

    # ---- 记录级统计量（含异常变换） ----
    def sample_stats(
        self,
        rng: np.random.Generator,
        index: int,
        fracs: dict[str, float],
    ) -> dict:
        category = self.sample_category(rng)
        cat_name = category["name"]
        view_mult = float(category["view_mult"][self.site])
        engage_mult = float(category["engage_mult"][self.site])
        cls = anom.class_for_index(self.seed, self.site_code, index, fracs)
        acfg = self.anomaly_cfg.get(cls, {}) if cls != anom.NORMAL else {}

        # 1) view：对数正态（长尾）× 类目乘子 ×（异常类分位区间/倍率）
        qrange = None
        view_mult_anom = 1.0
        if "view_quantile" in acfg:
            qrange = tuple(acfg["view_quantile"])
        if "view_mult" in acfg:
            lo, hi = acfg["view_mult"]
            view_mult_anom = float(rng.uniform(lo, hi))
        view = self._lognormal_view(rng, qrange) * view_mult * view_mult_anom

        # 2) 发布时间：幂律近期加权（new_hot/old_hot 覆盖年龄区间）
        if cls == "new_hot":
            age_hours = float(rng.uniform(0.5, float(acfg["max_age_hours"])))
            age_days = age_hours / 24.0
        elif cls == "old_hot":
            age_days = float(rng.uniform(float(acfg["min_age_days"]), self._pl_max_age))
            age_hours = age_days * 24.0
        else:
            age_days = self.sample_age_days(rng)
            age_hours = age_days * 24.0
        publish_ts = int(self.anchor - round(age_days * 86400))

        # 3) 互动率：Beta × 类目互动乘子 ×（异常类倍率）
        a, b = float(self.like_cfg["beta_a"]), float(self.like_cfg["beta_b"])
        like_rate = float(rng.beta(a, b)) * engage_mult
        engage_mult_anom = 1.0
        if cls == "high_like_low_view":
            lo, hi = acfg["like_mult"]
            like_rate *= float(rng.uniform(lo, hi))
            like_rate = min(like_rate, float(acfg.get("like_rate_cap", 0.6)))
        elif cls == "low_like_high_view":
            lo, hi = acfg["like_div"]
            like_rate /= float(rng.uniform(lo, hi))
        elif cls in ("new_hot", "old_hot"):
            lo, hi = acfg["engage_mult"]
            engage_mult_anom = float(rng.uniform(lo, hi))
            like_rate *= engage_mult_anom
        elif cls == "high_collect_low_like":
            lo, hi = acfg["like_mult"]
            like_rate *= float(rng.uniform(lo, hi))
        # P2-2 修复：like_rate 上限（1.0）截断发生在 like 计算之前——
        # 赞不可能超过播放，反事实（like > view）从采样阶段即被排除。
        # 注意 like 基于取整后的整数 view 计算（int 截断 + round 组合可产生 like=view+1）。
        view_i = int(max(view, 1))
        like_rate = min(like_rate, 1.0)
        like = int(round(view_i * like_rate))

        # 4) 派生互动（与 like 一致性约束）
        comment = self._derive_count(rng, like, "comment")
        collect = self._derive_count(rng, like, "collect")
        share = self._derive_count(rng, like, "share")
        if cls == "high_collect_low_like":
            lo, hi = acfg["collect_mult"]
            collect = int(round(collect * float(rng.uniform(lo, hi))))
            collect = max(collect, like * 3)  # 该类的定义性特征：收藏显著高于赞
        if cls in ("new_hot", "old_hot"):
            comment = int(round(comment * min(engage_mult_anom, 3.0)))
            share = int(round(share * min(engage_mult_anom, 2.5)))

        # 5) 最终落盘不变式钳制（P2-1 修复）：倍率 / 取整之后统一保证
        #    comment ≤ like × 0.30（new_hot/old_hot 的 comment×3 倍率与
        #    cap 处 round 上取此前可绕过该约束）。上限取整用 floor，
        #    like×0.3 < 1 时 comment 归 0（三站一致口径）。
        comment = clamp_comment(comment, like)

        return {
            "index": index,
            "category": cat_name,
            "anomaly_class": cls,  # 仅进 ground_truth.db，绝不写入站点 JSONL
            "view": view_i,
            "like": max(like, 0),
            "comment": max(comment, 0),
            "collect": max(collect, 0),
            "share": max(share, 0),
            "like_rate": like_rate,
            "age_days": age_days,
            "age_hours": age_hours,
            "publish_ts": publish_ts,
        }

    def _derive_count(self, rng: np.random.Generator, like: int, name: str) -> int:
        rc = self._ratios[name]
        ratio = float(rng.beta(float(rc["beta_a"]), float(rc["beta_b"])))
        ratio = min(ratio, float(rc["cap_like_frac"]))
        return int(round(like * ratio))
