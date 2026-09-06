"""分布引擎：view(对数正态·长尾) → like(view×Beta互动率×类目) → comment/collect/share(与 like 一致性约束)
→ publish_time(幂律近期加权) + 异常类变换。

数值关系（契约一致性约束）：
    like    = view × like_rate，like_rate ~ Beta(a,b) × 类目互动率乘子（异常类再乘/除倍率）
    comment = like × ratio_c，且 comment ≤ like × cap_comment（默认 0.30）
    collect = like × ratio_k，且 collect ≤ like × cap_collect
    share   = like × ratio_s，且 share   ≤ like × cap_share

红队 round6A 修复（R6A-P2-2/P3-1/P3-2）：
  - 发布年龄改为「新鲜切片 + 存量长尾」双组分混合（旧 powerlaw(scale=2d) 使
    全库中位年龄 1.7 天——评论延迟长尾无从谈起、星期分布退化为锚点前 1-3 天
    的 Mon+Tue 58.7% 尖峰）；
  - 发布时刻加日内节律（语料直方图形状：早 9-11 时峰、晚 19-20 时谷）与
    星期均衡（day≥7 的内容按目标星期 ±3 天重放置）；
  - 评论延迟（comment_delay_seconds）为独立重尾混合，分桶对齐语料
    （<1h 2.7% / 1h-1d 20.9% / 1d-1w 23.9% / 1w-1mo 30.3% / >1mo 22.1%），
    供 render 层三站共用。
"""
from __future__ import annotations

import numpy as np
from scipy.stats import norm

from synthgen import anomalies as anom
from synthgen.rngutil import inverse_cdf_pick

# ---------------------------------------------------------------------------
# R6A-P2-2：评论延迟分桶（语料 dy 详情页评论链 1184 条实测分桶，
# xhs/ks 共用同一形状口径；render 层与 sites/xhs 内嵌评论统一调用）
# ---------------------------------------------------------------------------
_COMMENT_DELAY_BUCKETS = (
    # (权重, 下界秒, 上界秒)
    (0.027, 60, 3600),            # <1h
    (0.209, 3600, 86400),         # 1h - 1d
    (0.239, 86400, 604800),       # 1d - 1w
    (0.303, 604800, 2592000),     # 1w - 1mo
    (0.222, 2592000, 365 * 86400),  # >1mo（长尾到一年）
)
_DELAY_CUM = []
_acc = 0.0
for _w, _lo, _hi in _COMMENT_DELAY_BUCKETS:
    _acc += _w
    _DELAY_CUM.append(_acc)
_DELAY_CUM[-1] = 1.0

# R6A-P3-1：发布小时节律（语料 dy 搜索结果日内分布形状：9-11 时
# 13.4/13.7/10.2%、19-20 时 0.2/0.2%，CV≈0.9、峰谷比≈68——取「强结构」
# 复刻形状，峰位/谷位与语料一致）
_PUBLISH_HOUR_WEIGHTS = (
    0.9, 0.7, 0.5, 0.4, 0.4, 0.6, 1.5, 3.2, 7.5, 13.4, 13.7, 10.2,
    6.6, 5.4, 4.6, 4.0, 3.4, 2.2, 1.0, 0.2, 0.2, 0.6, 0.9, 1.0,
)
_HOUR_CUM = np.cumsum(_PUBLISH_HOUR_WEIGHTS) / sum(_PUBLISH_HOUR_WEIGHTS)

_TZ_OFFSET = 8 * 3600   # 数据集锚点为东八区；日内节律按本地时放置


def comment_delay_seconds(rng) -> float:
    """评论相对发布时间的延迟（秒）：重尾混合分桶（R6A-P2-2）。

    p50≈8-9 天、p90≈180-215 天、>1 月≈22%（语料 dy 1184 条实测形状）；
    桶内均匀（顶桶跨 [30d, 365d]，与语料 p90=183d 对齐）。
    """
    u = rng.random()
    for (w, lo, hi), c in zip(_COMMENT_DELAY_BUCKETS, _DELAY_CUM):
        if u <= c:
            return float(lo + (hi - lo) * rng.random())
    return float(365 * 86400 * rng.random())


def comment_ctime(rng, pub_ts: int, now_ts: int, _salt: int = 0) -> int:
    """评论 create_time = pub + 重尾延迟，钳制在 (pub, now]。

    「发布前不可能」边界（R2-P2-3 口径保持）：延迟下界 60s；发布后不足
    一分钟的内容（new_hot 48h 内的最边缘）钳到 pub+30s。长延迟超出内容
    年龄时（新鲜内容的 >1mo 桶），回退为内容年龄的后半段（新鲜内容的评论
    集中在临近 now——语料新鲜内容形态），避免把评论堆到同一时刻。
    """
    delay = comment_delay_seconds(rng)
    ctime = int(pub_ts + delay)
    if ctime > now_ts:
        age = max(1, now_ts - pub_ts)
        ctime = int(pub_ts + age * rng.uniform(0.5, 1.0) - 30)
    return max(int(pub_ts) + 30, min(ctime, int(now_ts)))


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
        # 幂律时间参数（R6A-P2-2/P3-1：双组分混合 + 日内节律 + 星期均衡）
        pl = cfg["time"]["powerlaw"]
        self._pl_scale = float(pl["scale_days"])
        self._pl_shape = float(pl["shape"])
        self._pl_max_age = float(pl["max_age_days"])
        tm = cfg["time"].get("mixture", {}) or {}
        self._fresh_frac = float(tm.get("fresh_frac", 0.22))
        self._fresh_cap_days = float(tm.get("fresh_cap_days", 30))
        self._old_lo_days = float(tm.get("old_lo_days", 45))
        self._old_hi_days = float(tm.get("old_hi_days", 730))
        rhythm = cfg["time"].get("rhythm", {}) or {}
        self._rhythm_on = bool(rhythm.get("enabled", True))
        self._weekday_flatten_min_day = int(rhythm.get("weekday_flatten_min_day", 1))
        # 比率配置（站点级覆盖）
        self._ratios = {n: _ratio_cfg(cfg, self.sc, n) for n in ("comment", "collect", "share")}
        self.view_cfg = self.sc["view"]
        self.like_cfg = self.sc["like"]

    # ---- 基础采样 ----
    def sample_category(self, rng: np.random.Generator) -> dict:
        return self.categories[inverse_cdf_pick(rng.random(), self._cat_cum)]

    def sample_age_days(self, rng: np.random.Generator) -> float:
        """发布年龄（天）：双组分混合（R6A-P2-2/P3-1）。

        fresh_frac × Pareto(scale, shape, 截断 fresh_cap_days)：近期加权的新鲜切片
        （搜索新鲜度偏置 + 阈值类任务的 24h 供给）；其余为存量长尾
        Uniform(old_lo, max_age)——语料详情页评论链的延迟 p90=183 天意味着
        相当比例内容为数月存量。旧 powerlaw(scale=2d) 全库中位 1.7 天，
        使 >1 月评论不可能存在、星期分布退化为 Mon+Tue 58.7% 尖峰。
        """
        if rng.random() < self._fresh_frac:
            u = rng.random()
            while u <= 1e-12:
                u = rng.random()
            age = self._pl_scale * (u ** (-1.0 / self._pl_shape) - 1.0)
            return float(min(age, self._fresh_cap_days))
        return float(rng.uniform(self._old_lo_days, self._pl_max_age))

    def _place_publish_ts(self, rng: np.random.Generator, age_days: float) -> int:
        """把「年龄」放置为具体时刻：日内节律 + 星期均衡（R6A-P3-1）。

        - 日内：小时按语料直方图形状加权（9-11 时峰、19-20 时谷），分秒均匀；
        - 星期：day ≥ weekday_flatten_min_day 的内容按目标星期 ±3 天重放置
          （语料周一至周日 11.7-15.9% 平坦；窄年龄窗叠加锚点为周二曾造成
          Mon+Tue 58.7% 尖峰）；近 7 天内容保持原位（新鲜度不被扰动）。
        """
        anchor = self.anchor
        a_local = anchor + _TZ_OFFSET
        day_start_local = a_local - (a_local % 86400)
        day_idx = int(age_days)
        anchor_hour = int((a_local % 86400) // 3600)
        if day_idx == 0 and anchor_hour <= 1:
            day_idx = 1   # 锚点在凌晨：当日无可放置时段，整体退到前一日
        if day_idx >= self._weekday_flatten_min_day:
            # 目标星期（0=周一..6=周日），epoch day0(1970-01-01)=周四 → +3
            target_w = int(rng.integers(0, 7))
            cur_day = int(day_start_local // 86400) - day_idx
            cur_w = (cur_day + 3) % 7
            delta = (target_w - cur_w) % 7
            if delta > 3:
                delta -= 7
            day_idx = max(0, day_idx + delta)   # day0 不越界（内容不得晚于锚点）
        if self._rhythm_on:
            # 当日（day_idx==0）只能落在锚点小时之前（内容不得晚于锚点）；
            # 锚点 12:00 → 0-11 时，与语料早峰形状一致，几乎无损
            cum = _HOUR_CUM if day_idx > 0 else _HOUR_CUM[:max(anchor_hour, 1)]
            cum = cum / cum[-1]
            hour = int(np.searchsorted(cum, rng.random(), side="right"))
            hour = min(max(hour, 0), 23)
        else:
            hour = int(rng.integers(0, 24))
        hms = hour * 3600 + int(rng.integers(0, 60)) * 60 + int(rng.integers(0, 60))
        pub_local = day_start_local - day_idx * 86400 + hms
        return int(pub_local - _TZ_OFFSET)

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

        # 2) 发布时间：双组分混合年龄 + 日内节律/星期均衡放置
        #    （new_hot/old_hot 覆盖年龄区间，保持精确窗口、不走节律重放置）
        if cls == "new_hot":
            age_hours = float(rng.uniform(0.5, float(acfg["max_age_hours"])))
            age_days = age_hours / 24.0
            publish_ts = int(self.anchor - round(age_hours * 3600))
        elif cls == "old_hot":
            age_days = float(rng.uniform(float(acfg["min_age_days"]), self._pl_max_age))
            publish_ts = self._place_publish_ts(rng, age_days)
        else:
            age_days = self.sample_age_days(rng)
            publish_ts = self._place_publish_ts(rng, age_days)
        age_hours = age_days * 24.0

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
        cap = float(rc["cap_like_frac"])
        # R7A-P3-7 比值上尾重尾分量：frac 比例的记录取 U(lo, hi)×cap
        # （β 主体中位不动，p90 加厚进语料带；语料 dy collect/like p90 0.938、
        # comment/like p90 0.203——教程型/讨论型内容）
        tail = rc.get("tail") or {}
        if tail and rng.random() < float(tail.get("frac", 0.0)):
            ratio = float(rng.uniform(float(tail.get("lo", 0.5)),
                                      float(tail.get("hi", 1.0)))) * cap
        else:
            ratio = float(rng.beta(float(rc["beta_a"]), float(rc["beta_b"])))
        ratio = min(ratio, cap)
        return int(round(like * ratio))
