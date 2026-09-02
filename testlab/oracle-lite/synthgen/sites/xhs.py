"""小红书核心对象合成器：note（含 interact_info/user）+ 评论样本。

契约依据：
- note:  contracts/xhs/so.xiaohongshu.com_api_sns_web_v2_search_notes.contract.json（$.data.items[].note_card）
- 评论:  contracts/xhs/edith.xiaohongshu.com_api_sns_web_v2_comment_page.contract.json（$.data.comments[]）

契约实证要点：interact_info 计数全为字符串；note 无外显播放量/时间戳，
发布日期仅以 corner_tag_info 的 "MM-DD" 文本外显（本生成器如实遵循）。
"""
from __future__ import annotations

from datetime import datetime

import numpy as np

from synthgen import ids
from synthgen.distengine import clamp_comment

ENTITY = "note"
CONTRACT_ENDPOINTS = {
    "note": "so.xiaohongshu.com_api_sns_web_v2_search_notes",
    "comment": "edith.xiaohongshu.com_api_sns_web_v2_comment_page",
}
CONTRACT_CORE_PATH = "$.data.items[].note_card"

REQUIRED_FIELDS = [
    "id", "model_type", "xsec_token",
    "note_card.type", "note_card.display_title", "note_card.cover.width",
    "note_card.cover.height", "note_card.cover.url_default", "note_card.image_list",
    "note_card.interact_info.liked_count", "note_card.interact_info.collected_count",
    "note_card.interact_info.comment_count", "note_card.interact_info.shared_count",
    "note_card.user.user_id", "note_card.user.nickname", "note_card.user.avatar",
    "comments",
]

ID_UNIQUE_FIELDS = ["id", "comments[].id"]  # user_id 跨笔记复用属一致性要求
VARY_FIELDS = [
    "note_card.display_title", "note_card.interact_info.liked_count",
    "note_card.interact_info.collected_count", "note_card.interact_info.comment_count",
    "note_card.user.nickname", "note_card.user.avatar",
    "comments[].content", "comments[].like_count",
]


def _img_url(rng, kind: str) -> str:
    return (
        f"http://sns-webpic-qc.xhscdn.com/{ids.hex_id(rng, 10)}/{ids.hex_id(rng, 5)}/"
        f"{kind}/{ids.hex_id(rng, 10)}"
    )


def _image_item(rng) -> dict:
    w = int(ids.pick(rng, [1080, 1242, 1440, 2478]))
    h = int(w * rng.uniform(1.0, 1.6))
    return {
        "width": w,
        "height": h,
        "info_list": [
            {"image_scene": "WB_DFT", "url": _img_url(rng, "notes_pre_post")},
            {"image_scene": "WB_PRV", "url": _img_url(rng, "notes_pre_post")},
        ],
    }


def _comment_user(rng, ctx) -> dict:
    u = ctx.authors[int(rng.integers(0, len(ctx.authors)))]
    return {
        "user_id": u["user_id"],
        "nickname": u["nickname"],
        "image": u["avatar"],
        "xsec_token": ids.xhs_xsec_token(rng),
        "ai_agent": False,
    }


def build_comments(rng, stats, ctx, note_id: str) -> tuple[list[dict], int]:
    """评论样本 + comment_count 计数（红队 R2-P1-2：计数与可得评论同源）。

    真站参照（corpus xhs_note_comments）：note 卡「共 N 条评论」与 comment/page
    实际可翻条数量级一致（3→3 尽、6→4 尽、16→6 尽、29→p1=10+has_more），
    label ≥ 可得、label/可得 ∈ [1.0, ~2.7]。

    生成口径：
      - 可得条数 k = min(engine 评论量 × 比例因子, 内嵌上限 comments_per_note.max)；
        比例因子按语料形态混合采样 {1.0:35%, 0.7:25%, 0.4:30%, 0.25:10%}；
      - comment_count = k + 右偏噪声（U·U·2k，label ≥ k、≤ 3k），
        再钳制 comment ≤ like×0.3 不变式（clamp_comment）——计数与可翻条数同源；
      - 评论按 like_count 降序内嵌（真站评论区默认热度排序：最高赞评论必在第 1 页）；
      - 评论 id / 子评论 id 用真站结构 hex(ts)+00000000+hex8（R2-P2-2），
        ts 取评论自身 create_time（晚于笔记发布，R2-P2-3 同族口径）。

    返回 (comments, comment_count)：build_record 把计数写进
    interact_info.comment_count 并同步 stats["comment"]（index.db 同源）。
    """
    pools = ctx.pools
    n_comment = stats["comment"]
    if n_comment <= 0:
        return [], 0
    cap = int(ctx.sc.get("comments_per_note", {}).get("max", 40))
    u = rng.random()
    ratio = 1.0 if u < 0.35 else (0.7 if u < 0.60 else (0.4 if u < 0.90 else 0.25))
    k = max(0, min(round(n_comment * ratio), n_comment, cap))
    label = k + int(rng.random() * rng.random() * 2.0 * k)  # 右偏噪声：label ≥ 可得
    label = clamp_comment(label, stats["like"])
    if k <= 0:
        return [], label
    comments = []
    note_dt = stats["publish_ts"]
    # 评论时间窗：发布后 [~2min, min(14d, 距 anchor)]（R2-P2-3：评论晚于发布）
    span_s = max(120.0, min(14.0 * 86400, ctx.engine.anchor - note_dt - 60))
    for i in range(k):
        cat_pool = pools["comment_templates"].get(stats["category"], pools["comment_templates"]["generic"])
        content = ids.pick(rng, cat_pool) if rng.random() < 0.8 else ids.pick(rng, pools["comment_templates"]["generic"])
        c_user = _comment_user(rng, ctx)
        created = int((note_dt + rng.uniform(0.02, 1.0) * span_s) * 1000)
        created = min(created, ctx.engine.anchor * 1000)
        cid = ids.xhs_ts_hex_id(rng, created // 1000)
        # 评论点赞：首条热度显著高于其余（真站头部评论形态；保证 top 唯一可辨）
        base = label * (rng.uniform(0.02, 0.30) if i == 0 else rng.uniform(0.0005, 0.05))
        like_count = max(0, int(base))
        n_subs = 1 if rng.random() < 0.18 else 0  # 契约实证：sub_comments len=0..1
        sub_comment_cursor = ids.hex_id(rng, 24) if n_subs > 0 else ""
        sub_comments = []
        for _ in range(n_subs):
            s_user = _comment_user(rng, ctx)
            sub_created = min(int((created / 1000 + rng.uniform(0.01, 5) * 86400) * 1000), ctx.engine.anchor * 1000)
            sub_comments.append({
                "id": ids.xhs_ts_hex_id(rng, sub_created // 1000),
                "note_id": note_id,
                "content": ids.pick(rng, pools["sub_comment_pool"]),
                "create_time": sub_created,
                "like_count": str(int(rng.integers(0, max(2, like_count)))),
                "liked": False,
                "invalid": False,
                "ip_location": ids.pick(rng, pools["ip_locations"]),
                "pictures": [],
                "at_users": [],
                "show_tags": [],
                "status": 0,
                "user_info": s_user,
                "target_comment": {
                    "id": cid,  # R2 跨字段自洽：指向真实父评论 id（原为随机 hex）
                    "user_info": {k2: c_user[k2] for k2 in ("user_id", "nickname", "image")},
                },
            })
        pictures = []
        if rng.random() < 0.04:
            pic = _image_item(rng)
            pictures.append({
                "width": pic["width"], "height": pic["height"],
                "url_default": _img_url(rng, "comment"),
                "url_pre": _img_url(rng, "comment"),
                "info_list": pic["info_list"],
            })
        comments.append({
            "id": cid,
            "note_id": note_id,
            "content": content,
            "create_time": created,
            "like_count": str(like_count),
            "liked": False,
            "invalid": False,
            "ip_location": ids.pick(rng, pools["ip_locations"]),
            "pictures": pictures,
            "at_users": [],
            "show_tags": [],
            "status": 0,
            "sub_comment_count": str(len(sub_comments) + (int(rng.integers(1, 40)) if sub_comments else 0)),
            "sub_comment_cursor": sub_comment_cursor,
            "sub_comment_has_more": len(sub_comments) > 0,
            "sub_comments": sub_comments,
            "user_info": c_user,
        })
    # 热度排序（真站默认）：最高赞评论在前 → 首页即可翻到全局最高赞评论
    comments.sort(key=lambda c: int(c["like_count"]), reverse=True)
    return comments, label


def build_record(rng: np.random.Generator, stats: dict, author: dict, ctx) -> dict:
    title = ctx.make_title(rng, stats["category"])
    if rng.random() < 0.02:
        title = ""  # 契约实证：display_title 约 2% 为空串
    imgcfg = ctx.sc.get("images_per_note", {"min": 1, "max": 6})
    n_images = int(np.clip(int(rng.poisson(1.9)) + 1, imgcfg["min"], imgcfg["max"]))
    # R2-P2-2：note_id 真站结构 hex(publish_ts)+00000000+hex8（语料 738/738 mid-8-zero）
    note_id = ids.xhs_ts_hex_id(rng, stats["publish_ts"])
    comments, comment_count = build_comments(rng, stats, ctx, note_id)
    stats["comment"] = comment_count  # index.db 与 JSONL 计数同源（R2-P1-2）
    cover = _image_item(rng)
    pub = datetime.fromtimestamp(stats["publish_ts"])
    record = {
        "id": note_id,
        "model_type": "note",
        "xsec_token": ids.xhs_xsec_token(rng),
        "note_card": {
            "type": "normal",
            "display_title": title,
            "cover": {
                "width": cover["width"],
                "height": cover["height"],
                "url_default": cover["info_list"][0]["url"],
                "url_pre": cover["info_list"][1]["url"],
            },
            "image_list": [_image_item(rng) for _ in range(n_images)],
            "interact_info": {
                "liked_count": str(stats["like"]),
                "collected_count": str(stats["collect"]),
                "comment_count": str(comment_count),
                "shared_count": str(stats["share"]),
                "liked": False,
                "collected": False,
            },
            "user": {
                "user_id": author["user_id"],
                "nickname": author["nickname"],
                "nick_name": author["nickname"],
                "avatar": author["avatar"],
                "xsec_token": ids.xhs_xsec_token(rng),
            },
            "corner_tag_info": [{"type": "publish_time", "text": pub.strftime("%m-%d")}],
        },
        "comments": comments,
    }
    return record


def record_identity(record: dict) -> tuple[str, str]:
    return record["id"], record["note_card"]["user"]["user_id"]

def extract_metrics(record: dict) -> dict:
    """小红书无外显播放量（契约实证）；liked_count 为字符串。发布日期仅有 MM-DD 文本。"""
    nc = record["note_card"]
    inter, user = nc["interact_info"], nc["user"]
    return {
        "record_id": record["id"],
        "view": None,
        "like": int(inter["liked_count"]),
        "comment": int(inter["comment_count"]),
        "collect": int(inter["collected_count"]),
        "share": int(inter["shared_count"]),
        "publish_text": nc["corner_tag_info"][0]["text"],
        "publish_ts": None,
        "author_id": user["user_id"],
        "author_nickname": user["nickname"],
        "author_avatar": user["avatar"],
    }
