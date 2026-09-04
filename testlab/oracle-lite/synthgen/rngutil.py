"""全链路种子化工具。

核心原则：第 i 条记录的随机流只由 (base_seed, site_code, i) 决定，
与 --count、--filter、生成顺序无关 —— 因此同种子重生成任意前缀逐字节一致。
"""
from __future__ import annotations

import hashlib
import struct

import numpy as np


def record_rng(seed: int, site_code: int, index: int) -> np.random.Generator:
    """第 index 条记录的独立随机流。"""
    return np.random.default_rng([int(seed), int(site_code), int(index)])


def hash_unit(seed: int, site_code: int, index: int, salt: str = "") -> float:
    """稳定哈希 → [0,1) 均匀分布，用于与记录随机流解耦的判定（如异常类指派）。"""
    h = hashlib.blake2b(
        f"{salt}|{int(seed)}|{int(site_code)}|{int(index)}".encode("utf-8"), digest_size=8
    ).digest()
    (v,) = struct.unpack(">Q", h)
    return v / 2**64


def inverse_cdf_pick(u: float, cum: np.ndarray) -> int:
    """预计算累计权重上的 O(log n) 采样。"""
    return int(np.searchsorted(cum, u, side="right"))
