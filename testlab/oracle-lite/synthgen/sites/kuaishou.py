"""快手核心对象合成器：feed 项（photo + author + comment）。

契约依据：contracts/kuaishou/www.kuaishou.com_rest_v_search_feed.contract.json（$.feeds[]）
契约实证要点：
- photoUrl/photoH265Url 为数组 [{cdn,url}]
- manifest 为对象（adaptationSet[].representation[]）
- comment 维度即 feeds[].comment.us_c（实证多为 0，按参考值池采样）
- photo.timestamp 为毫秒；viewCount/likeCount/collectCount 为整数
"""
from __future__ import annotations

import numpy as np

from synthgen import ids
from synthgen.distengine import clamp_comment

ENTITY = "feed"
CONTRACT_ENDPOINT = "www.kuaishou.com_rest_v_search_feed"
CONTRACT_CORE_PATH = "$.feeds[]"

REQUIRED_FIELDS = [
    "author.id", "author.name", "author.headerUrl", "author.following",
    "author.livingInfo", "comment.us_c", "danmakuSwitch", "type",
    "photo.id", "photo.caption", "photo.timestamp", "photo.viewCount",
    "photo.likeCount", "photo.collectCount", "photo.liked", "photo.collected",
    "photo.duration", "photo.width", "photo.height", "photo.coverUrl",
    "photo.photoUrls", "photo.expTag", "photo.riskTagContent", "photo.riskTagUrl",
    "photo.stereoType", "photo.disableSensitivePhoto", "photo.manifest",
    "photo.manifest.adaptationSet", "photo.manifest.audioFeature",
    "photo.manifest.videoFeature", "photo.manifest.playInfo",
    "photo.manifest.videoId",
]

ID_UNIQUE_FIELDS = ["photo.id", "photo.manifest.videoId"]  # author.id 跨条目复用属一致性要求
VARY_FIELDS = [
    "photo.caption", "photo.timestamp", "photo.viewCount", "photo.likeCount",
    "photo.collectCount", "author.name", "photo.coverUrl",
]


def _ks_video_url(rng) -> str:
    return f"https://{ids.opaque(rng, 24)}.djvod.ndcimg.com/ksc2/{ids.opaque(rng, 12)}"


def _cdn_code(rng) -> str:
    return ids.dy_uri(rng, 56)


def _manifest(rng, duration_ms: int, w: int, h: int, h265: bool) -> dict:
    """h265=True 生成精简镜像（真实接口 manifestH265 与 manifest 同构，此处按磁盘预算裁剪重复特征块）。

    P2-4 修复：representation[] 补齐契约必填特征字段
    （comment / disableAdaptive / featureP2sp / kvqScore{FR,FRPost,NR,NRPost,nnvcScore[,blur,sharpness]}
    / makeupGain / normalizeGain / oriLoudness / p2spCode / realLoudness / realNormalizeGain），
    形态对齐契约实证与语料真值。
    """
    qt = ids.pick(rng, ["480p", "720p", "1080p"])
    ql = {"480p": "标清", "720p": "高清", "1080p": "超清"}[qt]
    avg = int(rng.integers(1200, 4500))
    maxb = int(avg * rng.uniform(1.1, 1.5))
    kvq = {
        "FR": -1.0,
        "NR": float(np.round(rng.uniform(2.0, 4.0), 7)),
        "FRPost": -1.0,
        "NRPost": -1.0,
        "nnvcScore": -1.0,
    }
    if rng.random() < 0.96:  # 契约实证：blur/sharpness 出现率 0.9603
        kvq["sharpness"] = float(np.round(rng.uniform(0.0, 1.0), 8))
        kvq["blur"] = float(np.round(rng.uniform(0.0, 1.0), 8))
    rep = {
        "id": 1,
        "qualityType": qt,
        "qualityLabel": ql,
        "width": w,
        "height": h,
        "frameRate": float(ids.pick(rng, [30.0, 60.0])),
        "avgBitrate": avg,
        "maxBitrate": maxb,
        "fileSize": int(duration_ms / 1000 * avg * 1024 / 8),
        "url": _ks_video_url(rng),
        "backupUrl": [_ks_video_url(rng)],
        "defaultSelect": False,
        "hidden": False,
        "mute": False,
        "agc": h265,
        "hdrType": 0,
        "quality": 1.5,
        # ---- 契约必填 representation 特征字段（P2-4）----
        "comment": f"videoId={ids.hex_id(rng, 16)}/ttExplain=AVC_VeryFast_{qt.replace('p', 'P')}_高码率_Basic/tt=b",
        "disableAdaptive": False,
        "featureP2sp": False,
        "kvqScore": kvq,
        "makeupGain": 0.0,
        "normalizeGain": 0.0,
        "oriLoudness": 0.0,
        "p2spCode": '{"fRsn":0,"fixOpt":-1,"schTask":"","schCode":-1,"schRes":"",'
                   '"pushTask":"v=0&p=0&s=0&d=0","pushCode":-1}',  # 契约实证常量形态（103 字符）
        "realLoudness": float(np.round(rng.uniform(-16.3, -9.0), 3)),
        "realNormalizeGain": float(np.round(rng.uniform(1.0, 1.5), 3)),
    }
    out = {
        "adaptationSet": [{"id": 1, "duration": duration_ms + int(rng.integers(-80, 120)),
                           "representation": [rep]}],
        "playInfo": {"bizType": 0, "cdnTimeRangeLevel": 0, "strategyBus": "{ }"},
        "businessType": 2,
        "mediaType": 2,
        "hideAuto": False,
        "manualDefaultSelect": False,
        "stereoType": 0,
        "version": "1.0.0",
        "videoId": ids.hex_id(rng, 16),
    }
    if h265:
        return out
    out["audioFeature"] = {
        "audioClip": round(float(rng.uniform(0.0, 0.01)), 4),
        "audioQuality": round(float(rng.uniform(60, 95)), 4),
        "audioSnr": round(float(rng.uniform(0.5, 8)), 3),
        "backgroundSoundProbability": round(float(rng.uniform(0.0, 0.3)), 4),
        "dialogProbability": round(float(rng.uniform(0.2, 0.8)), 4),
        "effectiveBandwidthInHz": round(float(rng.uniform(8000, 18000)), 3),
        "musicProbability": round(float(rng.uniform(0.1, 0.8)), 3),
        "stereophonicRichness": 100.0,
    }
    out["videoFeature"] = {
        "avgEntropy": round(float(rng.uniform(4, 8)), 4),
        "blockyProbability": float(rng.uniform(0, 0.001)),
        "blurProbability": float(rng.uniform(0, 0.001)),
        "contrast": round(float(rng.uniform(2, 5)), 4),
        "mosScore": round(float(rng.uniform(0.6, 0.95)), 6),
        "yMean": round(float(rng.uniform(90, 140)), 3),
        "yMeanMax": round(float(rng.uniform(120, 150)), 3),
        "yMeanMin": round(float(rng.uniform(50, 110)), 3),
    }
    return out


def build_record(rng: np.random.Generator, stats: dict, author: dict, ctx) -> dict:
    pools = ctx.pools
    title = ctx.make_title(rng, stats["category"])
    tags = ctx.make_tags(rng, stats["category"], int(rng.integers(1, 5)))
    caption = title + " " + "".join(f"#{t}" for t in tags)
    dcfg = ctx.sc["durations_ms"]
    duration = int(min(max(rng.lognormal(dcfg["mu"], dcfg["sigma"]), dcfg["min"]), dcfg["max"]))
    w, h = ids.pick(rng, [(720, 1280), (1080, 1920), (720, 960)])

    def cover_url():
        yy, mm, dd = 2026, int(rng.integers(1, 9)), int(rng.integers(1, 29))
        host = f"p{int(rng.integers(2, 30))}.a.yximgs.com"
        return f"https://{host}/upic/{yy}/{mm:02d}/{dd:02d}/{int(rng.integers(10, 20)):02d}/{ids.dy_uri(rng, 14)}"

    record = {
        "author": {
            "id": author["id"],
            "name": author["name"],
            "headerUrl": author["header_url"],
            "following": False,
            "livingInfo": {"iconType": 0, "living": False, "livingId": None},
        },
        "comment": {
            # P2-1 修复：us_c 参考池独立采样后，落盘前统一做不变式钳制 us_c ≤ like×0.3
            "us_c": clamp_comment(ids.pick(rng, pools["us_c_pool"]), stats["like"])
        },
        "danmakuSwitch": True,
        "type": 1,
        "photo": {
            "id": ids.ks_id(rng),
            "caption": caption,
            "timestamp": stats["publish_ts"] * 1000,
            "viewCount": stats["view"],
            "likeCount": stats["like"],
            "collectCount": stats["collect"],
            "liked": False,
            "collected": False,
            "duration": duration,
            "width": w,
            "height": h,
            "coverUrl": cover_url(),
            "animatedCoverUrl": cover_url(),
            "photoUrls": [
                {"cdn": _cdn_code(rng), "url": _ks_video_url(rng)},
                {"cdn": _cdn_code(rng), "url": _ks_video_url(rng)},
            ],
            "photoH265Urls": [
                {"cdn": _cdn_code(rng), "url": _ks_video_url(rng)},
            ],
            "expTag": ids.opaque(rng, 43),
            "stereoType": 0,
            "disableSensitivePhoto": False,
            "riskTagContent": None,
            "riskTagUrl": None,
            "manifest": _manifest(rng, duration, w, h, h265=False),
            "manifestH265": _manifest(rng, duration, w, h, h265=True),
        },
        "tags": [{"name": t, "type": 1} for t in tags],
    }
    if rng.random() < 0.27:
        record["authorStatement"] = {
            "content": ids.pick(rng, pools["author_statement_pool"]),
            "type": 1,
        }
    return record


def record_identity(record: dict) -> tuple[str, str]:
    return record["photo"]["id"], record["author"]["id"]

def extract_metrics(record: dict) -> dict:
    ph, au = record["photo"], record["author"]
    return {
        "record_id": ph["id"],
        "view": ph["viewCount"],
        "like": ph["likeCount"],
        "comment": record["comment"]["us_c"],
        "collect": ph["collectCount"],
        "share": None,
        "publish_ts": ph["timestamp"] // 1000,
        "author_id": au["id"],
        "author_nickname": au["name"],
        "author_avatar": au["headerUrl"],
    }
