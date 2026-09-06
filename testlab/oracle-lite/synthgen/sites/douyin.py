"""抖音核心对象合成器：aweme（含 statistics/author/video/music）。

契约依据：contracts/douyin/www.douyin.com_aweme_v1_web_general_search_single.contract.json
核心子树：$.data[].aweme_info（search/stream 契约同构）。
说明：抖音语料未捕获独立评论端点，评论维度即 statistics.comment_count（契约实证），
     comment_list 字段按实证形态保持 null。
P2-4 修复：契约必填（rate=1.0）字段 100% 补齐——
  - 50 个顶层 + 45 个 author.* 恒 null 快照字段显式保留 null 字面量；
  - text_extra[] 补 end/start/hashtag_name/is_commerce（契约必填 number/实证形态）；
  - rawdata / author.room_data（契约实证 98%+ 空串，enum [""]）；
  - video.bit_rate[] 补 HDR_bit/HDR_type/play_addr{8 字段}/video_extra；
  - video.bit_rate_audio[]（约半数记录非空，实证 121/189 数组）；
  - video.big_thumbs[]（79% 数组 / 21% null，实证 150/189）；
  - video.tags（98.4% null，实证 3/189 数组）。
"""
from __future__ import annotations

import json as _json

import numpy as np

from synthgen import ids

ENTITY = "aweme"
CONTRACT_ENDPOINT = "www.douyin.com_aweme_v1_web_general_search_single"
CONTRACT_CORE_PATH = "$.data[].aweme_info"

# 契约恒 null 快照字段（types={"null"}，rate=1.0）：显式保留 null 字面量（P2-4）
NULL_FIELDS_TOP = [
    "ai_follow_images", "anchors", "challenge_position", "chapter_bar_color",
    "chapter_list", "commerce_config_data", "common_left_top_labels", "cover_labels",
    "create_scale_type", "dislike_dimension_list", "dislike_dimension_list_v2",
    "diversion_bar_info", "effect_inflow_effects", "encrypt_interest_point_list",
    "encrypt_key_phrase_list", "follow_shot_assets", "geofencing", "geofencing_regions",
    "hybrid_label", "image_follow_shot_assets", "image_infos", "img_bitrate",
    "interaction_stickers", "interest_points", "jump_tab_info_list", "label_top_text",
    "mv_info", "nearby_hot_comment", "nickname_position", "origin_comment_ids",
    "origin_text_extra", "original_images", "packed_clips", "position", "promotions",
    "ref_tts_id_list", "ref_voice_modify_id_list", "relation_labels",
    "reply_smart_emojis", "slides_music_beats", "social_tag_list",
    "standard_bar_info_list", "trends_infos", "tts_id_list", "uniqid_position",
    "video_labels", "video_tag", "video_text", "voice_modify_id_list", "yumme_recreason",
]
NULL_FIELDS_AUTHOR = [
    "ad_cover_url", "ban_user_functions", "batch_unfollow_contain_tabs",
    "batch_unfollow_relation_desc", "can_set_geofencing", "card_entries",
    "card_entries_not_display", "card_sort_priority", "cf_list", "contrail_list",
    "cover_url", "creator_tag_list", "data_label_list", "display_info",
    "endorsement_info_list", "familiar_visitor_user",
    "follower_list_secondary_information_struct", "followers_detail",
    "geofencing", "homepage_bottom_toast", "identity_labels", "im_role_ids", "item_list",
    "link_item_list", "need_points", "new_story_cover", "not_seen_item_id_list",
    "not_seen_item_id_list_v2", "offline_info_list", "personal_tag_list",
    "platform_sync_info", "private_relation_list", "profile_component_disabled",
    "profile_mob_params", "profile_signature_components", "relative_users",
    "signature_extra", "special_people_labels", "text_extra", "type_label",
    "user_permissions", "user_tags", "verification_permission_ids",
    "webcast_preview_labels", "white_cover_url",
]

# 输出记录内必须 100% 存在的契约常驻字段（validate 逐条核对）
REQUIRED_FIELDS = [
    "aweme_id", "desc", "create_time", "aweme_type", "media_type", "group_id",
    "is_top", "user_digged", "collect_stat", "author_user_id", "comment_list",
    "rawdata",
    "author.uid", "author.sec_uid", "author.nickname", "author.avatar_thumb.uri",
    "author.avatar_thumb.url_list", "author.follower_count", "author.follow_status",
    "author.custom_verify", "author.enterprise_verify_reason", "author.room_data",
    "statistics.digg_count", "statistics.collect_count", "statistics.comment_count",
    "statistics.download_count", "statistics.forward_count", "statistics.live_watch_count",
    "statistics.play_count", "statistics.share_count",
    "video.duration", "video.width", "video.height", "video.ratio",
    "video.cover.uri", "video.cover.url_list", "video.play_addr.uri",
    "video.play_addr.url_list", "video.play_addr.data_size",
    "video.bit_rate", "video.big_thumbs", "video.bit_rate_audio", "video.tags",
    "music.id", "music.id_str", "music.mid", "music.owner_id", "music.owner_nickname",
    "status.allow_share", "status.in_reviewing", "status.is_delete",
]

# 契约 must_vary 口径：id 类字段须全局近似唯一；count/pii 类字段须非恒定
ID_UNIQUE_FIELDS = ["aweme_id", "group_id"]  # 作者 id 刻意跨条目复用（一致性要求），不做唯一性检查
VARY_FIELDS = [
    "desc", "create_time", "statistics.play_count", "statistics.digg_count",
    "statistics.comment_count", "statistics.collect_count", "statistics.share_count",
    "author.nickname", "author.avatar_thumb.uri", "author.follower_count",
]

_RESOLUTIONS = [("720p", 720, 1280), ("1080p", 1080, 1920), ("720p", 720, 960)]

_VIDEO_TAG_TITLES = ["每周精选", "抖音热榜", "精选推荐", "热榜精选"]


def _video_extra_str(rng: np.random.Generator, ratio: str, data_size: int) -> str:
    """bit_rate[].video_extra：契约实证为 ~522 字符 ASCII JSON（PktOffsetMap 等）。"""
    offsets = []
    off = float(data_size) * rng.uniform(0.05, 0.15)
    step = float(data_size) * rng.uniform(0.3, 0.5)
    for t in (1, 2, 3, 4, 5, 10):
        offsets.append({"time": t, "offset": int(off + step * (t - 1) * rng.uniform(0.7, 1.0))})
    pkt = _json.dumps(offsets, separators=(", ", ": "))
    obj = {
        "PktOffsetMap": pkt,
        "format": "mp4",
        "definition": ratio,
        "quality": "normal",
        "file_id": ids.hex_id(rng, 32),
        "applog_map": {"feature_id": ids.hex_id(rng, 32), "manual_only": 1},
        "manual_only": 1,
        "audio_channels": "2.0",
        "audio_sample_rate": "44100",
        "audio_metrics": None,
        "audio_score": None,
    }
    return _json.dumps(obj, separators=(",", ":"))


def _audio_sub_info(rng: np.random.Generator, uri: str) -> str:
    """bit_rate_audio[].audio_meta.sub_info：契约实证 ASCII JSON（~409 字符）。"""
    obj = {
        "audio_bitrate_target": 64,
        "audio_channels": "2.0",
        "audio_layout": "C",
        "audio_profile": "aac_he_v2",
        "audio_sample_rate": "44100",
        "base_range_info": {"index_range": "866-1161", "init_range": "0-865"},
        "check_info": {
            "check_info": f"a:{uri}|b:0-1161-{ids.hex_id(rng, 32)}|c:0-1161-{ids.hex_id(rng, 8)}"
        },
        "first_segment_range": "1162-45291",
        "gear_des_key": "0:dash|1:normal|2:bytevc1|5:normal|10000:105",
    }
    return _json.dumps(obj, separators=(",", ":"))


def _audio_url(rng: np.random.Generator) -> str:
    return (
        f"https://v26-web.douyinvod.com/{ids.hex_id(rng, 32)}/6a981e0f/video/tos/cn/"
        f"tos-cn-ve-15c00-ce/{ids.dy_uri(rng, 32)}/media-audio-und-mp4a/"
        f"?a=6383&ch=0&cr=8&dr=0&er=1&lr=default&cv=1&br=49&bt=49&cs=4"
        f"&mime_type=video_mp4&qs=0&rc={ids.dy_uri(rng, 12)}%3D%3D"
        f"&btag=80000e00028000&cquery=101n_100o&dy_q=1788267535"
    )


def build_record(rng: np.random.Generator, stats: dict, author: dict, ctx) -> dict:
    pools = ctx.pools
    title = ctx.make_title(rng, stats["category"])
    title = (title + ctx.title_emoji(rng, 0.088)).strip()   # R6A-P2-3：dy 语料 8.8% desc 含 emoji
    # 标签数按语料实证分布（detail/search 真值 text_extra 长度：众数 5、3-7 覆盖 65/65，
    # 旧 1-2 使 text_extra 长度倍率带超 2 → L3 分页层失分；保真修复轮）
    n_tags = int(rng.choice(np.array([3, 4, 5, 6, 7]),
                            p=np.array([0.06, 0.15, 0.68, 0.02, 0.09])))
    tags = ctx.make_tags(rng, stats["category"], n_tags)
    act = ctx.pick_activity_tag(rng)   # R6A-P2-3③：平台活动 tag（语料 top tag 族）
    if act:
        tags.append(act)
    desc = title + " " + " ".join(f"#{t}" for t in tags)

    ratio, w, h = _RESOLUTIONS[int(rng.integers(0, len(_RESOLUTIONS)))]
    dcfg = ctx.sc["durations_ms"]
    duration = int(min(max(rng.lognormal(dcfg["mu"], dcfg["sigma"]), dcfg["min"]), dcfg["max"]))

    like, view = stats["like"], stats["view"]
    share = stats["share"]
    # R2-P2-2：aweme_id 按真站雪花结构 = (create_time-δ)<<32 | rand32（时间可编码 + 首位 '7' 时代段）
    aweme_id = ids.dy_snowflake_id(rng, stats["publish_ts"])
    data_size = int(duration / 1000 * rng.uniform(0.4, 1.2) * 1024 * 1024)

    def vid_url():
        return (
            f"https://v26-web.douyinvod.com/{ids.hex_id(rng, 12)}/6a96f6bf/video/tos/cn/"
            f"tos-cn-ve-15/{ids.dy_uri(rng, 24)}/"
        )

    cover_uri = ids.dy_uri(rng, 51)
    play_uri = ids.dy_uri(rng, 32)

    # text_extra：契约实证 98.4% 为数组（1.6% null）；元素含 start/end 偏移（必填 number）
    text_extra = None
    if rng.random() < 0.984:
        pos = len(title) + 1  # desc = "<title> #tag1 #tag2 ..."，首 '#' 位于 title+1
        text_extra = []
        for t in tags:
            end = pos + len(t) + 1  # 含 '#' 号的区间终点（开区间口径，实证 start<end）
            text_extra.append({
                "start": pos,
                "end": end,
                "type": 1,
                "hashtag_name": t,
                "hashtag_id": ids.dy_snowflake_id(rng, stats["publish_ts"]),
                "is_commerce": False,
            })
            pos = end + 1

    # bit_rate 元素补齐契约必填：HDR_bit/HDR_type/play_addr{8 字段}/video_extra
    br_uri = ids.dy_uri(rng, 32)
    br_bitrate = int(rng.integers(800000, 4500000))
    bit_rate_item = {
        "gear_name": ids.pick(rng, pools["gear_names"]),
        "quality_type": 1,
        "bit_rate": br_bitrate,
        "format": "mp4",
        "FPS": ids.pick(rng, [30, 30, 60]),
        "is_bytevc1": 0,
        "is_h265": 0,
        "HDR_bit": "",
        "HDR_type": "",
        "play_addr": {
            "uri": br_uri,
            "url_list": [vid_url(), vid_url()],
            "url_key": f"{br_uri}_h264_{ratio}_{br_bitrate}",
            "width": w,
            "height": h,
            "data_size": data_size,
            "file_cs": f"c:0-{int(rng.integers(10000, 200000))}-{ids.hex_id(rng, 4)}",
            "file_hash": ids.hex_id(rng, 32),
        },
        "video_extra": _video_extra_str(rng, ratio, data_size),
    }

    # bit_rate_audio：契约实证 95/189 数组、94/189 null → 约半数记录 1 元素
    bit_rate_audio = None
    if rng.random() < 0.5:
        audio_size = int(duration / 1000 * rng.uniform(20, 90) * 1024 / 8)
        bit_rate_audio = [{
            "audio_extra": f'{{"real_bitrate":{int(rng.integers(30000, 90000))}}}',
            "audio_quality": 5,
            "audio_meta": {
                "bitrate": int(rng.integers(40000, 80000)),
                "codec_type": "bytevc1",
                "encoded_type": "normal",
                "file_hash": ids.hex_id(rng, 32),
                "file_id": ids.hex_id(rng, 32),
                "format": "dash",
                "fps": 0,
                "logo_type": "",
                "media_type": "audio",
                "quality": "normal",
                "quality_desc": "",
                "size": audio_size,
                "sub_info": _audio_sub_info(rng, play_uri),
                "url_list": {
                    "main_url": _audio_url(rng),
                    "backup_url": _audio_url(rng).replace("v26-web", "v11-weba"),
                    "fallback_url": (
                        f"https://www.douyin.com/aweme/v1/play/?video_id={play_uri}"
                        f"&line=0&file_id={ids.hex_id(rng, 32)}&sign={ids.hex_id(rng, 32)}"
                        f"&is_play_url=1&source=PackSourceEnum_SEARCH"
                    ),
                },
            },
        }]

    # big_thumbs：契约实证 150/189 数组、39/189 null
    big_thumbs = None
    if rng.random() < 0.79:
        bt_uri = f"tos-cn-p-0015c000-ce/{ids.dy_uri(rng, 39)}"

        def sprite_url(u: str) -> str:
            return (
                f"https://p3-sign.douyinpic.com/{u}~tplv-noop.image"
                f"?cquery=100z_100o_101n_100B_100x&dy_q=1788267535"
                f"&x-expires=1788278463&x-signature={ids.dy_uri(rng, 28)}%3D"
            )

        big_thumbs = [{
            "img_num": int(rng.integers(36, 100)),
            "uri": bt_uri,
            "img_url": sprite_url(bt_uri),
            "img_x_size": 136,
            "img_y_size": 240,
            "img_x_len": 5,
            "img_y_len": 5,
            "duration": round(duration / 1000 * rng.uniform(0.3, 1.0), 3),
            "interval": 2,
            "fext": "jpg",
            "uris": [bt_uri, f"tos-cn-p-0015c000-ce/{ids.dy_uri(rng, 39)}",
                     f"tos-cn-p-0015c000-ce/{ids.dy_uri(rng, 39)}"],
            "img_urls": [sprite_url(bt_uri)],
        }]

    # video.tags：契约实证 3/189 数组、186/189 null
    video_tags = None
    if rng.random() < 0.016:
        video_tags = [{
            "title": ids.pick(rng, _VIDEO_TAG_TITLES),
            "tag_type": 1,
            "left_right_padding": 3,
            "background_color": "#56292929",
            "font_color": "#E5FFFFFF",
            "url": {"url_list": [f"https://www.douyin.com/hot/{ids.digits_str(rng, 10)}"]},
        }]

    author_obj = {
        "uid": author["uid"],
        "sec_uid": author["sec_uid"],
        "nickname": author["nickname"],
        "avatar_thumb": {
            "uri": author["avatar_uri"],
            "url_list": list(author["avatar_urls"]),
            "height": 720,
            "width": 720,
        },
        "avatar_schema_list": None,
        "follower_count": author["followers"],
        "follower_status": 0,
        "follow_status": 0,
        "total_favorited": author["total_favorited"],
        "custom_verify": author["custom_verify"],
        "enterprise_verify_reason": author["enterprise_verify_reason"],
        "room_id": 0,
        "room_id_str": "0",
        "room_data": "",  # 契约必填（P2-4）：实证 98.4% 空串，enum [""]
        "secret": 0,
        "account_cert_info": "{}",
        "cha_list": None,
        "interest_tags": None,
    }
    author_obj.update({k: None for k in NULL_FIELDS_AUTHOR})

    record = {
        "aweme_id": aweme_id,
        "desc": desc,
        "create_time": stats["publish_ts"],
        "group_id": aweme_id,
        "aweme_type": 0,
        "media_type": 4,
        "is_top": 0,
        "user_digged": 0,
        "collect_stat": 0,
        "author_user_id": int(author["uid"]),
        "comment_list": None,
        "cha_list": None,
        "images": None,
        "image_list": None,
        "long_video": None,
        "text_extra": text_extra,
        "rawdata": "",  # 契约必填（P2-4）：实证 99.5% 空串，enum [""]
        "author": author_obj,
        "statistics": {
            "digg_count": like,
            "collect_count": stats["collect"],
            "comment_count": stats["comment"],
            "download_count": int(like * rng.uniform(0.0, 0.02)),
            "forward_count": int(share * rng.uniform(0.05, 0.4)),
            "live_watch_count": int(view * 0.01) if rng.random() < 0.02 else 0,
            "play_count": view,
            "share_count": share,
        },
        "video": {
            "duration": duration,
            "width": w,
            "height": h,
            "ratio": ratio,
            "cover": {
                "uri": cover_uri,
                "url_list": [
                    f"https://p9-pc-sign.douyinpic.com/{ids.dy_uri(rng, 10)}/{ids.dy_uri(rng, 12)}",
                    f"https://p3-pc-sign.douyinpic.com/{ids.dy_uri(rng, 10)}/{ids.dy_uri(rng, 12)}",
                ],
                "width": 720,
                "height": 720,
            },
            "origin_cover": {
                "uri": ids.dy_uri(rng, 51),
                "url_list": [
                    f"https://p3-pc-sign.douyinpic.com/{ids.dy_uri(rng, 10)}/{ids.dy_uri(rng, 12)}"
                ],
                "width": 720,
                "height": 720,
            },
            "play_addr": {
                "uri": play_uri,
                "url_list": [vid_url(), vid_url()],
                "width": w,
                "height": h,
                "data_size": data_size,
                "file_cs": f"c:0-{int(rng.integers(10000, 200000))}-{ids.hex_id(rng, 4)}",
                "file_hash": ids.hex_id(rng, 32),
            },
            "download_addr": {
                "uri": ids.dy_uri(rng, 32),
                "url_list": [vid_url()],
                "width": w,
                "height": h,
                "data_size": data_size,
            },
            "bit_rate": [bit_rate_item],
            "bit_rate_audio": bit_rate_audio,
            "big_thumbs": big_thumbs,
            "tags": video_tags,
        },
        "music": {
            "id": ids.digits_int(rng, 18),
            "id_str": ids.digits_str(rng, 18),
            "mid": ids.dy_uri(rng, 24),
            "owner_id": ids.digits_str(rng, 12),
            "owner_nickname": author["nickname"],
            "user_count": int(rng.integers(0, 500000)),
            "duration": duration + int(rng.integers(-2000, 5000)),
            "author": ids.pick(rng, pools["music_pool"]),
        },
        "status": {
            "allow_share": True,
            "in_reviewing": False,
            "is_delete": False,
            "is_private": False,
            "is_prohibited": False,
            "part_see": 0,
            "private_status": 0,
            "review_result": {"review_status": 0},
        },
    }
    record.update({k: None for k in NULL_FIELDS_TOP})
    return record


def record_identity(record: dict) -> tuple[str, str]:
    """(record_id, author_id) —— ground_truth/一致性索引用。"""
    return record["aweme_id"], record["author"]["uid"]

def extract_metrics(record: dict) -> dict:
    """供 validate/阶段5 抽取规范化指标（播放/点赞/评论/收藏/分享/发布时间秒/作者）。"""
    st, au = record["statistics"], record["author"]
    return {
        "record_id": record["aweme_id"],
        "view": st["play_count"],
        "like": st["digg_count"],
        "comment": st["comment_count"],
        "collect": st["collect_count"],
        "share": st["share_count"],
        "publish_ts": record["create_time"],
        "author_id": au["uid"],
        "author_nickname": au["nickname"],
        "author_avatar": au["avatar_thumb"]["uri"],
    }
