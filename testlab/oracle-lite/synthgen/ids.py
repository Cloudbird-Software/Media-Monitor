"""各站 ID/Token 形态生成器（形态对齐契约 example：<hex:len=N>/<digits>/<opaque:len=N>）。"""
from __future__ import annotations

import itertools
import threading

import numpy as np

B64URL = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
LOWER_ALNUM = "abcdefghijklmnopqrstuvwxyz0123456789"


def _chars(rng: np.random.Generator, alphabet: str, n: int) -> str:
    idx = rng.integers(0, len(alphabet), size=n)
    return "".join(alphabet[i] for i in idx)


def hex_id(rng: np.random.Generator, n: int = 24) -> str:
    return _chars(rng, "0123456789abcdef", n)


def digits_str(rng: np.random.Generator, n: int) -> str:
    digits = rng.integers(0, 10, size=n)
    digits[0] = rng.integers(1, 10)
    return "".join(str(int(d)) for d in digits)


# ---------------------------------------------------------------------------
# 真站 id 结构形态（红队 round2 R2-P2-2：id 形态学对齐，证据 corpus_morphology.json）
# ---------------------------------------------------------------------------
def dy_snowflake_id(rng: np.random.Generator, create_time: int) -> str:
    """抖音 aweme_id/cid 真站结构：id = (create_time - δ) << 32 | rand32（δ∈[0,4)）。

    语料实证（5 组 cid/create_time + 1 组 aweme/detail）：id >> 32 恰为
    create_time - (0..3)s；19 位数字；2021-08 之后发布内容首位 '7'（语料
    382/385，其余 3 条 '6' 为更早旧作）——时间可编码性与时代前缀同时成立。
    """
    ct = int(create_time) - int(rng.integers(0, 4))
    return str((ct << 32) | int(rng.integers(0, 2**32)))


# dy 用户 uid 长度分布（语料 435 样本，record 级频次）：
# {11:27.6%, 12:9.4%, 14:0.7%, 15:10.1%, 16:44.8%, 19:7.4%}
_DY_UID_LENGTH_CDF = []
for _l, _c in ((11, 120), (12, 41), (14, 3), (15, 44), (16, 195), (19, 32)):
    _DY_UID_LENGTH_CDF.append((_l, _c))
_DY_UID_LEN_TOTAL = sum(_c for _, _c in _DY_UID_LENGTH_CDF)
# dy uid 首位频次（语料 435 样本）：1:113 2:54 3:69 4:19 5:19 6:37 7:57 8:19 9:48
_DY_UID_FIRST = (1, 1, 1, 1, 1, 1, 1, 2, 2, 2, 3, 3, 3, 3, 4, 5, 6, 6, 7, 7, 7, 8, 9, 9)


def dy_uid(rng: np.random.Generator, u: float | None = None) -> str:
    """抖音用户 uid：长度按语料分布（众数 16，11~19 位，无 13/17/18），首位 1/2/3 偏多。

    u：外部低差异分层抽样位（作者池按下标黄金分割分层）——记录级 uid 长度分布
    会被作者热度幂律加权，iid 采样时头部 ~800 作者的随机偏差被放大；分层后
    池内任意前缀（= 热门作者段）都贴合全局语料分布。
    """
    if u is None:
        u = float(rng.random())
    acc = 0.0
    n_len = 16
    for length, count in _DY_UID_LENGTH_CDF:
        acc += count / _DY_UID_LEN_TOTAL
        if u < acc:
            n_len = length
            break
    first = _DY_UID_FIRST[int(rng.integers(0, len(_DY_UID_FIRST)))]
    if n_len == 1:
        return str(first)
    return str(first) + "".join(str(int(d)) for d in rng.integers(0, 10, size=n_len - 1))


def xhs_ts_hex_id(rng: np.random.Generator, create_time: int) -> str:
    """小红书 note/评论 id 真站结构：hex(unix_ts)(8) + '0'*8 + hex(8)（24hex）。

    语料实证：note_id 738/738、评论 id 183/183 均为该结构（mid-8-zero）；
    近两年内容 ts ∈ [0x62.., 0x6a..] → 首位 '6'（7 条 18 位旧笔记首位 '5'）。
    """
    return "%08x00000000%08x" % (int(create_time), int(rng.integers(0, 2**32)))


def digits_int(rng: np.random.Generator, n: int) -> int:
    return int(digits_str(rng, n))


def opaque(rng: np.random.Generator, n: int, alphabet: str = B64URL) -> str:
    return _chars(rng, alphabet, n)


def dy_sec_uid(rng: np.random.Generator) -> str:
    """抖音 sec_uid：MS4wLjABAAAA 前缀 + 43 位 b64url，总长 55。"""
    return "MS4wLjABAAAA" + _chars(rng, B64URL, 43)


def dy_logid(rng: np.random.Generator) -> str:
    """抖音 extra.logid / x-tt-logid：34 位「日期前缀」形态（红队 P2-D1 修复）。

    真站实证（live 头 + corpus extra.logid）：
      20260902085523853723D52453D11DDDA0 = YYYYMMDDHHMMSS(14) + 20 位大写 hex。
    旧实现为纯小写 hex 34 位，与真站形态不符。

    红队 round2 R2-P3-1：20 位 hex 部分原由请求级确定性 rng 派生，同秒内多个
    请求 logid 完全相同（真站每请求唯一）。现混入进程级计数器 + 随机源，
    保证同一进程内任意两次调用唯一（logid/cid 为响应时变字段，不参与
    数据集字节级可复现性口径）。
    """
    import datetime
    import secrets
    with _LOGID_LOCK:
        seq = next(_LOGID_SEQ)
    rand = secrets.token_hex(6).upper()  # 12 位大写 hex 随机
    return (datetime.datetime.now().strftime("%Y%m%d%H%M%S")
            + ("%08X" % (seq & 0xFFFFFFFF)) + rand)  # 14 + 8 + 12 = 34 位


_LOGID_SEQ = itertools.count()
_LOGID_LOCK = threading.Lock()


def ks_id(rng: np.random.Generator) -> str:
    """快手 photo/author id：'3x' + 13 位小写字母数字（15 位）。

    语料实证：脱敏录制 short-video/profile URL 契约 45+ 样本 id 全部为
    '3x' 前缀 15 位小写字母数字（如 3x2swmbawxahuuu / 3x3ch2br49s4cpc）。
    """
    return "3x" + _chars(rng, LOWER_ALNUM, 13)


def ks_session_id(rng: np.random.Generator) -> str:
    """快手 searchSessionId：60 位 opaque。"""
    return _chars(rng, B64URL, 60)


def xhs_xsec_token(rng: np.random.Generator) -> str:
    """小红书 xsec_token：50 位 opaque。"""
    return _chars(rng, B64URL, 50)


def pick(rng: np.random.Generator, seq):
    return seq[int(rng.integers(0, len(seq)))]


def pick_weighted_index(rng: np.random.Generator, cum: np.ndarray) -> int:
    return int(np.searchsorted(cum, rng.random(), side="right"))


def dy_uri(rng: np.random.Generator, n: int) -> str:
    return _chars(rng, B64URL, n)
