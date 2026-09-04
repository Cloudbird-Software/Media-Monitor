"""异常类注册与指派。

异常类真实标签只写入独立 ground_truth.db，绝不进入站点数据本体（JSONL）。
指派是 (seed, site, index) 的纯函数（哈希分桶），因此：
- 与 --count 无关（重生成前缀逐字节一致）
- 占比为二项分布口径，10 万条下与目标占比偏差 < ±0.2%
"""
from __future__ import annotations

from synthgen.rngutil import hash_unit

# 英文键（机器用）↔ 中文名（CLI/文档用）
CLASS_NAMES = {
    "high_like_low_view": "高赞低播放",
    "low_like_high_view": "低赞高播放",
    "new_hot": "新发布高互动",
    "old_hot": "老内容高互动",
    "high_collect_low_like": "高收藏低赞",
    "normal": "正常",
}
ZH_TO_EN = {v: k for k, v in CLASS_NAMES.items()}
# 固定遍历顺序，保证同配置下指派确定性
ORDER = ["high_collect_low_like", "high_like_low_view", "low_like_high_view", "new_hot", "old_hot"]
NORMAL = "normal"


def normalize_class_key(name: str) -> str:
    name = name.strip()
    if name in CLASS_NAMES:
        return name
    if name in ZH_TO_EN:
        return ZH_TO_EN[name]
    raise ValueError(f"未知异常类: {name!r}（可用: {', '.join(CLASS_NAMES.values())}）")


def parse_anomaly_spec(spec: str | None, defaults: dict) -> dict[str, float]:
    """解析 CLI 形如 "高赞低播放:2%,低赞高播放:1.5%" 为 {英文键: frac}。

    None/空/none/off → 使用 distributions.yaml 中 anomalies.*.frac 默认值。
    """
    if not spec or spec.strip().lower() in ("none", "off", "-"):
        out = {}
        for key, val in defaults.items():
            if key == NORMAL:
                continue
            frac = val.get("frac", 0.0)
            if frac > 0:
                out[key] = float(frac)
        return out
    out: dict[str, float] = {}
    total = 0.0
    for part in spec.split(","):
        part = part.strip()
        if not part:
            continue
        if ":" not in part:
            raise ValueError(f"异常类格式错误（应为 类名:占比）: {part!r}")
        name, frac_s = part.rsplit(":", 1)
        key = normalize_class_key(name)
        if key == NORMAL:
            raise ValueError("normal 为兜底类，不可指定占比")
        frac_s = frac_s.strip().rstrip("%")
        frac = float(frac_s) / 100.0
        if not (0 <= frac <= 1):
            raise ValueError(f"占比越界: {part!r}")
        out[key] = frac
        total += frac
    if total > 1.0:
        raise ValueError(f"异常类占比之和 {total:.3f} > 1")
    return out


def class_for_index(seed: int, site_code: int, index: int, fracs: dict[str, float]) -> str:
    """哈希分桶指派：u 落入 [累计阈值) 区间则属于该类，否则 normal。"""
    if not fracs:
        return NORMAL
    u = hash_unit(seed, site_code, index, salt="anomaly-class")
    acc = 0.0
    for key in ORDER:
        f = fracs.get(key, 0.0)
        if f <= 0:
            continue
        acc += f
        if u < acc:
            return key
    return NORMAL


def target_fractions(fracs: dict[str, float]) -> dict[str, float]:
    """完整占比视图（含 normal 兜底），供校验。"""
    out = dict(fracs)
    out[NORMAL] = max(0.0, 1.0 - sum(fracs.values()))
    return out
