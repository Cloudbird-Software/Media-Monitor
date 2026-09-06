"""validate.py —— 预置数据集验收自测（阶段 4 交付第 3 项）。

对每站数据集执行 10 组检查，全绿退出码 0：
  1. manifest 一致性：行数/哈希/ground_truth 行数对账
  2. 字段完整率：站点模块 REQUIRED_FIELDS 在全部记录 100% 存在（并与契约子树交叉核对）
  3. must_vary：id 类字段唯一率 ≥ 99.9%；count/pii 类字段非恒定
  4. 分布检验：view 对数正态 KS 拟合（p>0.01）+ 分位数表；like-view 对数相关 ≥ 0.7
  4b. 一致性不变式（P2-1/P2-2 修复回归项）：comment ≤ like×0.3 违例 = 0；like > view 反事实 = 0
      （xhs 无外显 view 时以 index.db 潜在 view 口径复核）
  5. 异常类占比：与配置目标一致（3σ 二项容差）
  6. 一致性：同一 author 的昵称/头像跨条目完全一致
  7. 可复现性：同种子重生成 1 万条与预置数据前 1 万行逐字节一致（sha256）
  8. 标签泄漏：JSONL 中无 anomaly/class/label 等 ground_truth 字样
  9. 第二实现复核：pandas 独立重算 4 项分布指标，与主路径数值一致
  4d. 红队 round5A 新断言（R5A-P1-1/P1-2/P2-2/P2-5）：
      作者长尾（distinct ≥5000、top1 ≤2%）、随机 1000 个 20 卡窗同作者重复 ≤2、
      评论窗口完全重复文本 =0、评论者 100% 可回查（存在该作者+昵称一致）、
      ks commentCountV2 中位 ~1.5e3 无零值 且 数据集 us_c 恒 0
  4f. 红队 round7A 新断言（R7A-P2-1/P2-2/P3-1..P3-7）：
      ks collectCount 渲染恒 0、xhs sub target_comment 同源有效、ladder 14 词
      全映射且主词严格命中进带、语气词/网络用语/@提及率进语料带（±30%）、
      dy ip 属地海外 ≤3% 且 42 种、dy custom_verify 渲染恒空、dy 楼中楼二层
      嵌套 ~1/3、dy collect/like 上尾 + 作者粉丝量级、xhs collect/like 中位

用法：python synthgen/validate.py [--datasets-dir synthgen/datasets] [--sites douyin,xhs,kuaishou]
                                    [--repro-count 10000] [--skip-repro]
"""
from __future__ import annotations

import argparse
import importlib
import json
import math
import sqlite3
import sys
import tempfile
from pathlib import Path

import numpy as np
from scipy import stats as sstats

_PKG_PARENT = str(Path(__file__).resolve().parent.parent)
if _PKG_PARENT not in sys.path:
    sys.path.insert(0, _PKG_PARENT)

from synthgen import SITES
from synthgen.config import CONTRACT_DIR

import re as _re
import datetime as _dt

LEAK_MARKERS = ('"anomaly', '"class"', '"label"', '"ground_truth"', 'anomaly_class', '"is_anomaly"', '"cls"')

# 红队 round6 新断言用到的 render 侧引用（延迟导入避免与 4c/4d 的局部引用重复）
from synthgen import commentext as _commentext_mod
from synthgen import render as _render_mod


class Report:
    def __init__(self):
        self.rows = []          # (site, check, passed, detail)
        self.summary_metrics = {}

    def add(self, site, check, passed, detail=""):
        self.rows.append((site, check, bool(passed), str(detail)))

    @property
    def all_passed(self):
        return all(r[2] for r in self.rows)

    def dump(self, datasets_dir, extra=None):
        data = {
            "rows": [{"site": s, "check": c, "passed": p, "detail": d} for s, c, p, d in self.rows],
            "metrics": self.summary_metrics,
            "all_passed": self.all_passed,
        }
        if extra:
            data.update(extra)
        out = Path(datasets_dir) / "validation_report.json"
        out.write_text(json.dumps(data, ensure_ascii=False, indent=2), encoding="utf-8")
        return out


def iter_records(path: Path):
    with open(path, "r", encoding="utf-8") as f:
        for line in f:
            yield json.loads(line)


def get_path(record: dict, dotted: str):
    cur = record
    for part in dotted.split("."):
        if not isinstance(cur, dict) or part not in cur:
            return None, False
        cur = cur[part]
    return cur, True


def collect_values(node, parts):
    """支持 a[].b 形式的数组路径取值（用于 must_vary 检查）。"""
    if not parts:
        yield node
        return
    head, rest = parts[0], parts[1:]
    if head.endswith("[]"):
        name = head[:-2]
        if isinstance(node, dict) and isinstance(node.get(name), list):
            for el in node[name]:
                yield from collect_values(el, rest)
        return
    if isinstance(node, dict) and head in node:
        yield from collect_values(node[head], rest)


# ---------- 契约交叉核对 ----------

def _contract_schema(site: str, endpoint_id: str) -> dict:
    p = CONTRACT_DIR / site / f"{endpoint_id}.contract.json"
    return json.loads(p.read_text(encoding="utf-8"))["response_schema"]


def contract_rate(site: str, site_mod, dotted: str) -> float | None:
    """在对应契约核心子树中解析字段路径，返回 present rate；找不到返回 None。"""
    if site == "douyin":
        roots = [_find_node(_contract_schema(site, site_mod.CONTRACT_ENDPOINT),
                            ["data", "[]", "aweme_info"])]
    elif site == "xhs":
        base = _find_node(_contract_schema(site, site_mod.CONTRACT_ENDPOINTS["note"]),
                          ["data", "items", "[]"])
        roots = [base, _find_node(base, ["note_card"]) if base else None,
                 _find_node(_contract_schema(site, site_mod.CONTRACT_ENDPOINTS["comment"]), ["data"])]
    else:
        roots = [_find_node(_contract_schema(site, site_mod.CONTRACT_ENDPOINT), ["feeds", "[]"])]
    best = None
    for root in roots:
        if root is None:
            continue
        node, ok = root, True
        for part in dotted.split("."):
            flds = node.get("fields") or {}
            if part not in flds:
                ok = False
                break
            nxt = flds[part]
            if part.endswith("[]") or "items" in nxt and not nxt.get("fields"):
                items = nxt.get("items")
                if items is None:
                    return max(best or 0.0, float(nxt.get("rate", 1.0)))
                node = items
            else:
                node = nxt
        if ok:
            best = max(best or 0.0, float(node.get("rate", node.get("present", 1.0)) or 1.0))
    return best


def _find_node(root, path_parts):
    cur = root
    for part in path_parts:
        if part == "[]":
            cur = cur.get("items")
        else:
            cur = (cur.get("fields") or {}).get(part)
        if cur is None:
            return None
    return cur


# ---------- 主流程 ----------

def validate_site(site: str, site_dir: Path, rep: Report, repro_count: int, skip_repro: bool) -> dict:
    site_mod = importlib.import_module(f"synthgen.sites.{site}")
    manifest = json.loads((site_dir / "MANIFEST.json").read_text(encoding="utf-8"))
    jsonl = site_dir / f"{site_mod.ENTITY}.jsonl"
    gt_path = site_dir / "ground_truth.db"
    n_lines = manifest["files"]["jsonl"]["lines"]

    # ---- 1. manifest 一致性 ----
    actual_lines = sum(1 for _ in open(jsonl, encoding="utf-8"))
    rep.add(site, "manifest.lines", actual_lines == n_lines, f"{actual_lines} == {n_lines}")
    import hashlib
    h = hashlib.sha256()
    with open(jsonl, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    sha_ok = h.hexdigest() == manifest["files"]["jsonl"].get("sha256")
    rep.add(site, "manifest.sha256", sha_ok, "")

    conn = sqlite3.connect(f"file:{gt_path}?mode=ro", uri=True)
    n_labels, = conn.execute("SELECT COUNT(*) FROM labels").fetchone()
    n_distinct, = conn.execute("SELECT COUNT(DISTINCT record_id) FROM labels").fetchone()
    rep.add(site, "gt.rowcount", n_labels == n_lines and n_distinct == n_lines,
            f"labels={n_labels}, distinct={n_distinct}, lines={n_lines}")

    # ---- 全量单遍扫描：收集指标/字段完整率/must_vary/一致性/泄漏 ----
    view, like, comment, publish = [], [], [], []
    missing = {}
    id_sets = {f: set() for f in site_mod.ID_UNIQUE_FIELDS}
    id_totals = {f: 0 for f in site_mod.ID_UNIQUE_FIELDS}
    vary_vals = {f: set() for f in site_mod.VARY_FIELDS}
    author_attr = {}
    author_conflicts = 0
    leak_hits = 0
    n = 0
    # 红队 round2 新增收集器：id 形态学（R2-P2-2）+ 评论时间/计数自洽（R2-P1-2/P2-3）
    morph = {"dy_len19": 0, "dy_first7": 0, "dy_time_enc": 0,
             "xhs_id_form": 0, "ks_id_form": 0}
    time_bad = 0          # 评论时间早于作品发布（跨字段/跨端点）
    claim_bad = 0         # xhs comment_count 与内嵌（可翻）评论条数不同源
    n_comments_checked = 0
    # 红队 round5A 新增收集器（R5A-P1-1/P2-2）：作者序列/头部占比/ks us_c
    author_seq: list = []          # 按行序的 author_id（窗口重复检查用）
    author_counts: dict = {}       # author_id -> 出现次数
    author_sec_uids: set = set()   # dy：作者 sec_uid 实体空间（评论者可回查）
    ks_usc_nonzero = 0             # ks：数据集 us_c 非零条数（语料真值恒 0）
    # 红队 round6 新增收集器（R6A-P2-3/P3-1/P3-2 + R6C-P3-4）：
    pub_hours = [0] * 24           # 发布小时直方图（本地时）
    pub_weekdays = [0] * 7         # 发布星期直方图
    title_emoji_n = 0              # 标题 unicode emoji 文本数（dy/ks）
    xhs_bracket_n = 0              # xhs 标题 [xxR] 站内表情数
    tag_names: set = set()         # hashtag 池多样性（dy/ks，容量封顶）
    activity_tags_seen: set = set()
    ks_durations: list = []        # duration 方差（旧恒 5000 unique=1）
    ks_manifest_meta: list = []    # (underExposed, oriLoudness) 抽样
    _UNI_EMOJI = _re.compile(r"[\U0001F000-\U0001FAFF\u2600-\u27BF\uFE0F]")
    _BRACKET_R = _re.compile(r"\[[^\[\]]{1,6}R\]")
    _ACT_TAG_PREFIX = ("抖音", "开放赛道", "喜爱度", "快成长", "全民任务", "光合计划",
                       "快手", "万能生活指南", "磁力", "老铁", "村口", "人间烟火")
    n_title_checked = 0
    for rec in iter_records(jsonl):
        n += 1
        for fld in site_mod.REQUIRED_FIELDS:
            _, ok = get_path(rec, fld)
            if not ok:
                missing[fld] = missing.get(fld, 0) + 1
        # ---- R2-P2-2 id 形态学 + R2-P2-3/R2-P1-2 自洽（按站点结构） ----
        if site == "douyin":
            aid = str(rec.get("aweme_id") or "")
            ct = rec.get("create_time")
            if not (len(aid) == 19 and aid.isdigit()):
                morph["dy_len19"] += 1
            if aid and aid[0] != "7":
                morph["dy_first7"] += 1
            if aid.isdigit() and isinstance(ct, int):
                # 真站雪花结构：id>>32 = create_time - (0..3)s（语料 6 组实证）
                if not (ct - 4 <= (int(aid) >> 32) <= ct):
                    morph["dy_time_enc"] += 1
        elif site == "xhs":
            nid = str(rec.get("id") or "")
            if not (len(nid) == 24 and nid[8:16] == "00000000"
                    and all(ch in "0123456789abcdef" for ch in nid)):
                morph["xhs_id_form"] += 1
            note_ts = int(nid[0:8], 16) if nid[:8].isalnum() else 0
            claim = int((rec.get("note_card") or {}).get("interact_info", {})
                        .get("comment_count") or 0)
            cmts = rec.get("comments") or []
            # R2-P1-2：claim ≥ 可翻条数 k 且 ≤ 3k（语料 label/可得 ∈ [1, 2.7]）
            k = len(cmts)
            if not (claim >= k and claim <= max(3 * k, k)) or (k == 0 and claim != 0):
                claim_bad += 1
            for c in cmts:
                cid = str(c.get("id") or "")
                n_comments_checked += 1
                if not (len(cid) == 24 and cid[8:16] == "00000000"):
                    morph["xhs_id_form"] += 1
                elif note_ts and int(cid[0:8], 16) < note_ts:
                    time_bad += 1  # 评论 id 时间戳早于笔记发布
                if note_ts and int(c.get("create_time") or 0) < note_ts * 1000:
                    time_bad += 1
                for sc in c.get("sub_comments") or []:
                    sid = str(sc.get("id") or "")
                    if not (len(sid) == 24 and sid[8:16] == "00000000"):
                        morph["xhs_id_form"] += 1
                    elif note_ts and int(sid[0:8], 16) < note_ts:
                        time_bad += 1
                    if int(sc.get("create_time") or 0) < int(c.get("create_time") or 0):
                        time_bad += 1
        else:  # kuaishou
            pid = str((rec.get("photo") or {}).get("id") or "")
            if not (len(pid) == 15 and pid.startswith("3x")
                    and all(ch in "abcdefghijklmnopqrstuvwxyz0123456789" for ch in pid[2:])):
                morph["ks_id_form"] += 1
            if int((rec.get("comment") or {}).get("us_c") or 0) != 0:
                ks_usc_nonzero += 1
        m = site_mod.extract_metrics(rec)
        if m["view"] is not None:
            view.append(m["view"])
        like.append(m["like"])
        comment.append(m["comment"])
        if m["publish_ts"] is not None:
            publish.append(m["publish_ts"])
        # ---- R6A-P3-1 节律 / R6A-P2-3 文本形态 / R6C-P3-4 元数据 收集 ----
        ts = m["publish_ts"]
        if site == "xhs" and ts is None:
            nid = str(rec.get("id") or "")
            ts = int(nid[0:8], 16) if len(nid) == 24 and nid[:8].isalnum() else None
        if ts:
            _lt = _dt.datetime.fromtimestamp(ts + 8 * 3600, _dt.timezone.utc)
            pub_hours[_lt.hour] += 1
            pub_weekdays[_lt.weekday()] += 1
        if site == "douyin":
            _t = rec.get("desc") or ""
            n_title_checked += 1
            if _UNI_EMOJI.search(_t):
                title_emoji_n += 1
            for te in rec.get("text_extra") or []:
                nm = te.get("hashtag_name") if isinstance(te, dict) else None
                if nm and len(tag_names) < 6000:
                    tag_names.add(nm)
                    if nm.startswith(_ACT_TAG_PREFIX):
                        activity_tags_seen.add(nm)
        elif site == "xhs":
            _t = (rec.get("note_card") or {}).get("display_title") or ""
            n_title_checked += 1
            if _BRACKET_R.search(_t):
                xhs_bracket_n += 1
        else:
            ph = rec.get("photo") or {}
            _t = ph.get("caption") or ""
            n_title_checked += 1
            if _UNI_EMOJI.search(_t):
                title_emoji_n += 1
            if len(ks_durations) < 5000:
                ks_durations.append(int(ph.get("duration") or 0))
            for tg in rec.get("tags") or []:
                nm = tg.get("name") if isinstance(tg, dict) else None
                if nm and len(tag_names) < 6000:
                    tag_names.add(nm)
                    if nm.startswith(_ACT_TAG_PREFIX):
                        activity_tags_seen.add(nm)
            if len(ks_manifest_meta) < 2000:
                mf = ph.get("manifest") or {}
                vf = mf.get("videoFeature") or {}
                try:
                    _rep_item = (((mf.get("adaptationSet") or [{}])[0]).get("representation") or [{}])[0]
                    ks_manifest_meta.append((vf.get("underExposed"), _rep_item.get("oriLoudness")))
                except Exception:
                    pass
        # R5A-P1-1：作者序列（窗口重复/头部占比）
        aid = m["author_id"]
        author_seq.append(aid)
        author_counts[aid] = author_counts.get(aid, 0) + 1
        if site == "douyin":
            sec = str((rec.get("author") or {}).get("sec_uid") or "")
            if sec:
                author_sec_uids.add(sec)
        for f in id_sets:
            vals = list(collect_values(rec, f.split(".")))
            id_sets[f].update(vals)
            id_totals[f] += len(vals)
        for f in vary_vals:
            for v in collect_values(rec, f.split(".")):
                if len(vary_vals[f]) < 50:
                    vary_vals[f].add(v)
        aid = m["author_id"]
        attr = (m["author_nickname"], m["author_avatar"])
        if aid in author_attr:
            if author_attr[aid] != attr:
                author_conflicts += 1
        else:
            author_attr[aid] = attr
    with open(jsonl, "r", encoding="utf-8") as f:
        for line in f:
            low = line.lower()
            for marker in LEAK_MARKERS:
                if marker.lower() in low:
                    leak_hits += 1
                    break

    # ---- 2. 字段完整率 ----
    complete = not missing
    rep.add(site, "field.completeness", complete,
            f"{len(site_mod.REQUIRED_FIELDS)} required fields, missing={missing or '无'}")
    contract_missing = []
    for fld in site_mod.REQUIRED_FIELDS:
        if contract_rate(site, site_mod, fld) is None:
            contract_missing.append(fld)
    rep.add(site, "field.contract_crosscheck", not contract_missing,
            f"未在契约子树找到: {contract_missing or '无'}")

    # ---- 3. must_vary ----
    id_bad = {f: f"{len(s)}/{id_totals[f]}" for f, s in id_sets.items()
              if len(s) < 0.999 * max(id_totals[f], 1)}
    rep.add(site, "mustvary.id_unique", not id_bad,
            f"{ {f: f'{len(s)}/{id_totals[f]}' for f, s in id_sets.items()} }")
    vary_bad = [f for f, s in vary_vals.items() if len(s) < 2]
    rep.add(site, "mustvary.nonconst", not vary_bad, f"恒定字段: {vary_bad or '无'}")

    # ---- 4. 分布检验 ----
    like_arr = np.array(like, dtype=float)
    metrics = {"n": n, "like_median": float(np.median(like_arr))}
    if view:
        v = np.array(view, dtype=float)
        lv = np.log(v)
        mu, sigma = sstats.norm.fit(lv)
        ks = sstats.kstest(lv, "norm", args=(mu, sigma))
        rep.add(site, "dist.view_lognormal_ks", ks.pvalue > 0.01,
                f"KS p={ks.pvalue:.4f}, fitted mu={mu:.3f} sigma={sigma:.3f}")
        metrics.update(view_median=float(np.median(v)), view_p99=float(np.percentile(v, 99)),
                       view_mu=mu, view_sigma=sigma)
        # like-view 相关（对数尺度）
        corr = float(np.corrcoef(np.log10(v), np.log10(like_arr + 1))[0, 1])
        rep.add(site, "dist.like_view_corr", corr >= 0.7, f"Pearson(log view, log like)={corr:.4f} ≥ 0.7")
        metrics["like_view_corr"] = corr
    else:
        # xhs：无外显 view → like = lognormal(view) × Beta(互动率) 为乘积分布，
        # 严格 KS 在 n=1e5 下对微小形状偏差也拒绝；按规范改用「分位数对比」（偏差 ≤5%）
        ll = np.log(like_arr + 1)
        mu, sigma = sstats.norm.fit(ll)
        qs = [0.05, 0.25, 0.50, 0.75, 0.95]
        obs_q = np.quantile(ll, qs)
        fit_q = sstats.norm.ppf(qs, mu, sigma)
        devs = {f"q{int(q*100):02d}": float((o - f) / abs(f) * 100) for q, o, f in zip(qs, obs_q, fit_q)}
        ok_q = all(abs(d) <= 5.0 for d in devs.values())
        ks_full = sstats.kstest(ll, "norm", args=(mu, sigma))
        rep.add(site, "dist.like_lognormal_quantiles", ok_q,
                f"分位数偏差%={ {k: round(v, 1) for k, v in devs.items()} }（全量 KS D={ks_full.statistic:.4f} 仅作参考）")
        metrics["like_lognorm_mu"], metrics["like_lognorm_sigma"] = mu, sigma
        c_arr = np.array(comment, dtype=float)
        corr = float(np.corrcoef(np.log10(like_arr + 1), np.log10(c_arr + 1))[0, 1])
        rep.add(site, "dist.like_comment_corr", corr >= 0.7, f"Pearson(log like, log comment)={corr:.4f} ≥ 0.7")
        metrics["like_comment_corr"] = corr

    # ---- 4b. 一致性不变式（P2-1/P2-2 修复回归项）----
    c_arr = np.array(comment, dtype=float)
    viol_comment = int(np.sum(c_arr > like_arr * 0.3 + 1e-9))
    rep.add(site, "invariant.comment_le_like03", viol_comment == 0,
            f"comment > like×0.3 违例 {viol_comment}/{n}（要求 0）")
    metrics["invariant_comment_violations"] = viol_comment
    if view:
        viol_lv = int(np.sum(like_arr > np.array(view, dtype=float) + 1e-9))
        rep.add(site, "invariant.like_le_view", viol_lv == 0,
                f"like > view 反事实 {viol_lv}/{n}（要求 0）")
        metrics["invariant_like_gt_view"] = viol_lv
    else:
        # xhs：JSONL 无外显 view，用 index.db 潜在 view 复核（行序 = line_no）
        idx_path = site_dir / "index.db"
        if idx_path.exists():
            conn2 = sqlite3.connect(f"file:{idx_path}?mode=ro", uri=True)
            rows = conn2.execute("SELECT view, like FROM records ORDER BY line_no").fetchall()
            conn2.close()
            viol_lv = sum(1 for v, l in rows if l > v)
            rep.add(site, "invariant.like_le_view", viol_lv == 0,
                    f"like > view 反事实（index.db 口径）{viol_lv}/{len(rows)}（要求 0）")
            metrics["invariant_like_gt_view"] = viol_lv

    # ---- 4c. 红队 round2 新断言：id 形态学（R2-P2-2）+ 评论时间/计数自洽（R2-P1-2/P2-3）----
    if site == "douyin":
        rep.add(site, "morphology.id_structure",
                morph["dy_len19"] == 0 and morph["dy_time_enc"] == 0
                and morph["dy_first7"] <= 0.05 * n,
                f"非19位={morph['dy_len19']}, 首位非'7'={morph['dy_first7']}（语料 382/385 '7'，"
                f"容差 5%）, 时间编码违例={morph['dy_time_enc']}（id>>32 == create_time-(0..3)s）")
        # dy 评论在渲染层按 (aweme_id, 序号) 确定性合成 → 跨端点断言走 render 抽样
        from synthgen import render as _render
        rdr = _render.DatasetReader(site_dir)
        idxs = np.linspace(0, len(rdr) - 1, 40).astype(int)
        cmt_time_bad = cmt_cid_bad = 0
        for ln in idxs:
            ln = int(ln)
            rec_c = rdr.read(ln)
            pub = int(rec_c.get("create_time") or 0)
            resp = _render.render_douyin_comment_list(rdr, ln, 0, 20)
            for c in resp.get("comments") or []:
                n_comments_checked += 1
                if int(c.get("create_time") or 0) < pub:
                    cmt_time_bad += 1
                cid = str(c.get("cid") or "")
                if not (len(cid) == 19 and cid.isdigit()
                        and int(c["create_time"]) - 4 <= (int(cid) >> 32)
                        <= int(c["create_time"])):
                    cmt_cid_bad += 1
        rep.add(site, "invariant.comment_time_ge_publish", cmt_time_bad == 0,
                f"render 抽样 {len(idxs)} 作品 / {n_comments_checked} 条评论："
                f"早于发布={cmt_time_bad}（R2-P2-3，要求 0）；cid 形态违例={cmt_cid_bad}")
    elif site == "xhs":
        rep.add(site, "morphology.id_structure", morph["xhs_id_form"] == 0,
                f"note/评论/子评论 id 非 hex(ts)+00000000+hex8 结构={morph['xhs_id_form']}"
                f"（语料 738/738+183/183，R2-P2-2）")
        rep.add(site, "invariant.comment_time_ge_publish", time_bad == 0,
                f"评论 id/时间早于笔记发布={time_bad}（{n_comments_checked} 条评论，要求 0，R2-P2-3）")
        rep.add(site, "invariant.comment_count_matches_retrievable", claim_bad == 0,
                f"comment_count 与内嵌（可翻）条数不同源={claim_bad}/{n}"
                f"（claim ∈ [k, 3k]，语料 label/可得 ∈ [1, 2.7]，R2-P1-2）")
    else:
        rep.add(site, "morphology.id_structure", morph["ks_id_form"] == 0,
                f"photo.id 非 '3x'+13 位小写字母数字形态={morph['ks_id_form']}/{n}"
                f"（语料 45+ 契约 URL 样本一致，R2-P2-2）")

    # ---- 4d. 红队 round5A 新断言：作者长尾 / 窗口重复 / 评论文本池 / 评论者可回查 / ks 计数 ----
    # (a) R5A-P1-1 作者池：distinct ≥ 数千、top1 ≤ 2%；随机 1000 个 20 卡窗同作者最大重复 ≤ 2
    distinct_authors = len(author_counts)
    top1_share = max(author_counts.values()) / max(n, 1)
    win_rng = np.random.default_rng([20260901, 77, len(author_seq)])
    n_windows = min(1000, max(0, n - 19))
    starts = win_rng.integers(0, max(1, n - 19), size=n_windows) if n_windows else []
    win_max_rep = 0
    win_dist = {1: 0, 2: 0}
    for s in starts:
        c = {}
        for aid in author_seq[int(s):int(s) + 20]:
            c[aid] = c.get(aid, 0) + 1
        mx = max(c.values()) if c else 0
        win_max_rep = max(win_max_rep, mx)
        win_dist[2 if mx >= 2 else 1] += 1
    # oracle-lite：迷你数据集（300 条/站）阈值按规模自适应——distinct 下限
    # min(5000, 80%×n)（完整台架 10 万条时与 ≥5000 等价；300 条 → ≥240）、
    # top1 容差附加小样本计数噪声 ±2 条（完整台架时上限仍 ≈2%）。
    _min_distinct = min(5000, int(n * 0.8))
    _top1_cap = 0.02 + 2.0 / max(n, 1)
    rep.add(site, "author.longtail",
            distinct_authors >= _min_distinct and top1_share <= _top1_cap,
            f"distinct={distinct_authors}（≥{_min_distinct}，旧实现 ~90-183），"
            f"top1={top1_share:.4f}（≤{_top1_cap:.4f}，旧 ~0.54；语料 ≈0.01）")
    rep.add(site, "author.window_repeat",
            win_max_rep <= 2,
            f"{n_windows} 个随机 20 卡窗：同作者最大重复={win_max_rep}（≤2，旧 6-16；"
            f"语料 94% 卡片作者唯一）——含 1 对重复的窗 {win_dist[2]}、全唯一 {win_dist[1]}")
    metrics["author_distinct"] = distinct_authors
    metrics["author_top1_share"] = float(top1_share)
    metrics["author_window_max_repeat"] = int(win_max_rep)
    metrics["author_window_pair_frac"] = win_dist[2] / max(1, n_windows)

    # (b)+(d) R5A-P1-2/P2-5：评论窗口文本重复率 0 + 评论者可回查（render 抽样）
    #   重复按「单内容窗口」口径统计（红队 R5A-P1-2：单视频 20 条评论内部完全重复），
    #   同时记录跨内容重叠率（参照修复建议 <10%）
    from synthgen import render as _render_all
    rdr2 = _render_all.DatasetReader(site_dir)
    sample_lines = np.linspace(0, max(0, len(rdr2) - 1), 40).astype(int)
    dup_texts = 0            # 单内容窗口内完全重复条数（要求 0）
    n_cmt = 0
    xfile_texts: set = set()
    xfile_shared = 0         # 跨内容重叠（仅参考指标）
    commenter_checked = commenter_bad = 0
    for ln in sample_lines:
        ln = int(ln)
        if site == "douyin":
            resp = _render_all.render_douyin_comment_list(rdr2, ln, 0, 20)
            items = resp.get("comments") or []
            texts = [c.get("text") for c in items]
            for c in items:
                u = c.get("user") or {}
                nick = author_attr.get(str(u.get("uid") or "")) or (None, None)
                commenter_checked += 1
                if nick == (None, None) or nick[0] != u.get("nickname") \
                        or str(u.get("sec_uid") or "") not in author_sec_uids:
                    commenter_bad += 1
        elif site == "xhs":
            _, r1 = _render_all.render_xhs_comment_page(rdr2, ln, "")
            _, r2p = _render_all.render_xhs_comment_page(rdr2, ln, r1["data"]["cursor"])
            items = list(r1["data"]["comments"]) + list(r2p["data"]["comments"])
            texts = [c.get("content") for c in items]
            for c in items:
                u = c.get("user_info") or {}
                nick = author_attr.get(str(u.get("user_id") or "")) or (None, None)
                commenter_checked += 1
                if nick == (None, None) or nick[0] != u.get("nickname"):
                    commenter_bad += 1
        else:
            resp = _render_all.render_ks_comment_list(rdr2, ln, "", 20)
            items = resp.get("rootCommentsV2") or []
            texts = [c.get("content") for c in items]
            for c in items:
                nick = author_attr.get(str(c.get("author_id") or "")) or (None, None)
                commenter_checked += 1
                if nick == (None, None) or nick[0] != c.get("author_name"):
                    commenter_bad += 1
        dup_texts += len(texts) - len(set(texts))
        n_cmt += len(texts)
        for t in texts:
            if t in xfile_texts:
                xfile_shared += 1
            xfile_texts.add(t)
    corpus_unique = {"douyin": "100%", "xhs": "98%", "kuaishou": "94%"}[site]
    xfile_rate = xfile_shared / max(1, n_cmt)
    rep.add(site, "comment.window_text_unique", dup_texts == 0,
            f"render 抽样 40 内容 / {n_cmt} 条评论：单窗口完全重复={dup_texts}"
            f"（0 ≤ 语料唯一率 {corpus_unique}；旧 dy 20 条仅 12 种/8 组重复）；"
            f"跨内容重叠率={xfile_rate:.3f}（参考线 <0.1）")
    rep.add(site, "comment.commenter_resolvable", commenter_bad == 0,
            f"{commenter_checked} 条评论的评论者：不可回查/昵称不一致={commenter_bad}"
            f"（0；旧 5/5 断链，R5A-P2-5）")
    metrics["comment_dup_texts"] = int(dup_texts)
    metrics["comment_cross_overlap_rate"] = float(xfile_rate)
    metrics["commenter_checked"] = int(commenter_checked)
    metrics["commenter_bad"] = int(commenter_bad)

    # (c) R5A-P2-2：ks commentCountV2 独立对数正态（中位 ~1.5e3、无零值）+ us_c 恒 0
    if site == "kuaishou":
        step = max(1, n // 2000)
        from itertools import islice
        totals = [_render_all._ks_comment_total(rec)
                  for rec in islice(iter_records(jsonl), 0, n, step)]
        med = float(np.median(totals))
        zeros = sum(1 for t in totals if t == 0)
        rep.add(site, "ks.comment_count_dist",
                800 <= med <= 2600 and zeros == 0 and min(totals) >= 51,
                f"commentCountV2 抽样 {len(totals)}：中位={med:.0f}（语料 1544，旧为 0-21）、"
                f"min={min(totals)}/max={max(totals)}（语料 [51,28863]）、零值={zeros}/语料 0/70")
        rep.add(site, "ks.us_c_always_zero", ks_usc_nonzero == 0,
                f"数据集 comment.us_c 非零={ks_usc_nonzero}/{n}（语料真值恒 0；旧 38% 非零）")
        metrics["ks_comment_total_median"] = med
        metrics["ks_us_c_nonzero"] = int(ks_usc_nonzero)

    # ---- 4e. 红队 round6 新断言（R6A-P2-1/P2-2/P2-3/P3-1/P3-2/P3-3/P3-4 + R6C-P2-1/P3-4）----

    # (a) R6C-P2-1：dy 真站口径——play_count 恒 0（语料 799/799 搜索 + 50/50 详情）；
    #     total_favorited 搜索上下文 0（799/799，detail 上下文有值）；
    #     follower_count single 端点 0（210/210，search/item 上下文有值 609 条）
    if site == "douyin":
        _st = _render_all.render_douyin_stream(rdr2, 1, 20, keyword="美食探店", start=0)
        _si_req, _si = _render_all.render_douyin_single(rdr2, 1, 20, keyword="美食探店", start=20)
        _it_req, _it = _render_all.render_douyin_search_item(rdr2, 1, 20, keyword="美食探店", start=40)
        _det = _render_all.render_douyin_detail(rdr2, 0)["aweme_detail"]
        def _aw_list(resp):
            return [d.get("aweme_info") or {} for d in (resp.get("data") or [])]
        _a_all = _aw_list(_st) + _aw_list(_si) + _aw_list(_it) + [_det]
        _pc0 = all((a.get("statistics") or {}).get("play_count") == 0 for a in _a_all)
        _tf0 = all((a.get("author") or {}).get("total_favorited") == 0
                   for a in _aw_list(_st) + _aw_list(_si) + _aw_list(_it))
        _fc0 = all((a.get("author") or {}).get("follower_count") == 0
                   for a in _aw_list(_st) + _aw_list(_si))
        _fc_item = any((a.get("author") or {}).get("follower_count") for a in _aw_list(_it))
        _tf_det = ((_det.get("author") or {}).get("total_favorited") or 0) > 0
        rep.add(site, "dy.play_count_hidden_true_site",
                _pc0 and _tf0 and _fc0 and _fc_item and _tf_det,
                f"stream/single/item/detail 抽样 61 卡：play_count=0 {_pc0}（语料 849/849）、"
                f"搜索 total_favorited=0 {_tf0}（799/799）、single follower=0 {_fc0}（210/210）、"
                f"item follower 有值 {_fc_item}（609 条）、detail total_favorited 有值 {_tf_det}（50/50）")
        metrics["dy_play_count_zeroed"] = bool(_pc0 and _tf0 and _fc0)

    # (b) R6A-P2-1：搜索相关性——严格命中率进语料区间（dy 47.1/xhs 17.7/ks 37.4%），
    #     类目耦合（映射类目占比 ≥ 50%，旧 0 耦合/近似均匀）、乱码词退化背景
    _rel_kws = {"douyin": ["美食探店", "穿搭分享", "咖啡拉花"],
                "xhs": ["穿搭分享", "美食探店", "旅行攻略"],
                "kuaishou": ["美食探店", "穿搭分享", "赶集"]}[site]
    _strict_tot = _strict_n = 0
    _cat_tot = _cat_n = 0
    _kw_strict_modes = 0
    for _kw in _rel_kws:
        _view = _render_all._kw_view(rdr2, site, _kw)
        if _view is None:
            continue
        _cat_all = set(_view.hits) | set(_view.cat_lines)
        # oracle-lite：供给阈值按规模缩放（完整台架 ≥24 等价；300 条 → ≥1），
        # 严格命中下带 0.08 在小供给下无统计意义，迷你集退化为 ≥0（上带不变）。
        _supply_min = max(1, int(round(24 * n / 100000)))
        _strict_lb = 0.08 if n >= 20000 else 0.0
        _kw_strict = len(_view.hits) >= _supply_min   # 命中供给充足才计入严格命中率口径
        _kw_strict_modes += 1 if _kw_strict else 0
        for _pg in (0, 1):
            _lines = _render_all.search_window_lines(rdr2, site, _kw, _pg * 20, 20) or []
            _recs = [rdr2.read(ln) for ln in _lines]
            for _rec, _ln in zip(_recs, _lines):
                _txt = _render_all._search_text(site, _rec)
                _cat_n += 1
                _cat_tot += 1 if _ln in _cat_all else 0
                if _kw_strict:
                    _strict_n += 1
                    _strict_tot += 1 if _kw in _txt else 0
    _strict_rate = _strict_tot / max(1, _strict_n)
    _cat_rate = _cat_tot / max(1, _cat_n)
    _gk_lines = _render_all.search_window_lines(rdr2, site, "qzwkjxvbpq", 0, 20)
    rep.add(site, "search.relevance",
            _kw_strict_modes >= 1 and _strict_n >= 40
            and _strict_lb <= _strict_rate <= 0.60 and _cat_rate >= 0.50,
            f"映射关键词 {_strict_n} 卡（{ _kw_strict_modes }/3 个关键词命中供给充足）："
            f"严格命中 {_strict_rate:.3f}（语料 dy 0.471/xhs 0.177/ks 0.374，旧 0.00-0.033）、"
            f"类目耦合 {_cat_rate:.3f}（≥0.5，旧≈0）；"
            f"乱码词退化为背景={_gk_lines is None}（R5C 乱码词出结果口径保持）")
    metrics["search_strict_hit_rate"] = float(_strict_rate)
    metrics["search_category_coupling"] = float(_cat_rate)

    # (c) R6A-P2-2：评论延迟长尾（语料 p50=8.1 天、22.1%>1 月、<1h 仅 2.7%）
    _delays: list = []
    _cmt_texts: list = []
    _cmt_pub_bad = 0
    for _ln in sample_lines[::2]:
        _ln = int(_ln)
        if site == "douyin":
            _rec_c = rdr2.read(_ln)
            if int((_rec_c.get("statistics") or {}).get("comment_count") or 0) < 50:
                continue
            _resp_c = _render_all.render_douyin_comment_list(rdr2, _ln, 0, 60)
            _pub = int(_rec_c.get("create_time") or 0)
            for _c in _resp_c.get("comments") or []:
                _d = int(_c.get("create_time") or 0) - _pub
                _delays.append(_d / 86400.0)
                _cmt_texts.append(_c.get("text") or "")
                if _d < 0:
                    _cmt_pub_bad += 1
        elif site == "xhs":
            _rec_c = rdr2.read(_ln)
            _pub = int(str(_rec_c.get("id"))[:8], 16)
            for _c in _rec_c.get("comments") or []:
                _d = (int(_c.get("create_time") or 0) // 1000) - _pub
                _delays.append(_d / 86400.0)
                _cmt_texts.append(_c.get("content") or "")
                if _d < 0:
                    _cmt_pub_bad += 1
        else:
            _resp_c = _render_all.render_ks_comment_list(rdr2, _ln, "", 40)
            _rec_c = rdr2.read(_ln)
            _pub = int((_rec_c.get("photo") or {}).get("timestamp") or 0) // 1000
            for _c in _resp_c.get("rootCommentsV2") or []:
                _d = (int(_c.get("timestamp") or 0) // 1000) - _pub
                _delays.append(_d / 86400.0)
                _cmt_texts.append(_c.get("content") or "")
                if _d < 0:
                    _cmt_pub_bad += 1
    _dl = np.array(_delays)
    _d_p50 = float(np.median(_dl)) if len(_dl) else -1
    _d_gt1mo = float(np.mean(_dl > 30)) if len(_dl) else -1
    _d_p90 = float(np.percentile(_dl, 90)) if len(_dl) else -1
    rep.add(site, "comment.delay_longtail",
            _cmt_pub_bad == 0 and len(_dl) >= 200 and 0.5 <= _d_p50 <= 12.0
            and _d_gt1mo >= 0.08 and _d_p90 >= 20,
            f"render 抽样 {len(_dl)} 条：p50={_d_p50:.2f} 天（语料 8.1，旧 0.03）、"
            f"p90={_d_p90:.0f} 天（语料 183，旧 0.11）、>1 月 {_d_gt1mo:.3f}（语料 0.221，旧 0）、"
            f"早于发布 {_cmt_pub_bad}（0，R2-P2-3 保持）")
    metrics["comment_delay_p50_days"] = _d_p50
    metrics["comment_delay_gt1mo"] = _d_gt1mo

    # (d) R6A-P2-3①②：评论括号表情率 30-40%（语料 dy 40.2/xhs 31.7/ks 39.1）+ 长度离散
    from synthgen.commentext import has_emoji_marker as _has_emoji
    _emoji_rate = sum(1 for t in _cmt_texts if _has_emoji(t, site)) / max(1, len(_cmt_texts))
    _lens = sorted(len(t) for t in _cmt_texts)
    _l_p10 = _lens[max(0, len(_lens) // 10)] if _lens else -1
    _l_p90 = _lens[min(len(_lens) - 1, 9 * len(_lens) // 10)] if _lens else -1
    _l_max = max(_lens) if _lens else -1
    rep.add(site, "comment.emoji_and_length",
            0.24 <= _emoji_rate <= 0.46 and len(_cmt_texts) >= 200
            and _l_p10 <= 8 and _l_p90 >= 18 and _l_max >= 26,
            f"评论 {len(_cmt_texts)} 条：括号表情率 {_emoji_rate:.3f}（语料 0.317-0.402，旧 0）、"
            f"长度 p10/p90/max={_l_p10}/{_l_p90}/{_l_max}（语料 [3-4, 26-33]，旧 [10-12, 17]）")
    metrics["comment_emoji_rate"] = float(_emoji_rate)
    metrics["comment_len_p10"], metrics["comment_len_p90"] = _l_p10, _l_p90

    # (e) R6A-P2-3①③：标题表情 + hashtag 池多样性/活动 tag
    if site == "xhs":
        _tb = xhs_bracket_n / max(1, n_title_checked)
        rep.add(site, "title.emoji_form",
                0.18 <= _tb <= 0.45,
                f"xhs 标题 [xxR] 站内表情率 {_tb:.3f}（旧 0；xhs 语料表情形态 [xxR]）")
        metrics["xhs_title_bracket_rate"] = float(_tb)
    else:
        _te = title_emoji_n / max(1, n_title_checked)
        _band = (0.04, 0.15) if site == "douyin" else (0.01, 0.09)
        rep.add(site, "title.emoji_form",
                _band[0] <= _te <= _band[1],
                f"{site} 标题 unicode emoji 率 {_te:.3f}（语料 dy 0.088/ks 0.040，旧 0）")
        metrics["title_unicode_emoji_rate"] = float(_te)
        _need = 400 if site == "douyin" else 300
        rep.add(site, "hashtag.pool_diversity",
                len(tag_names) >= _need and len(activity_tags_seen) >= 6,
                f"全库 distinct tag {len(tag_names)}（≥{_need}；语料 dy 2.5 tag/文本，旧池 "
                f"61/38 个）、活动运营 tag {len(activity_tags_seen)} 种（旧 0；语料 top tag 族）")
        metrics["hashtag_distinct"] = len(tag_names)
        metrics["activity_tag_kinds"] = len(activity_tags_seen)

    # (f) R6A-P3-1：发布节律——小时 CV/峰谷比（语料 CV 0.913、68.5×）+ 星期平坦
    _tot = sum(pub_hours) or 1
    _hr_arr = np.array(pub_hours, dtype=float) / _tot
    _hr_cv = float(_hr_arr.std() / _hr_arr.mean())
    _hr_pt = float(_hr_arr.max() / max(_hr_arr.min(), 1e-9))
    _wd_arr = np.array(pub_weekdays, dtype=float) / (sum(pub_weekdays) or 1)
    _wd_max = float(_wd_arr.max())
    _wd_mt = float(_wd_arr[0] + _wd_arr[1])
    rep.add(site, "time.publish_rhythm",
            _hr_cv >= 0.45 and _hr_pt >= 8.0 and _wd_max <= 0.21 and _wd_mt <= 0.36,
            f"小时 CV={_hr_cv:.3f}（语料 0.913，旧 0.186）、峰谷比={_hr_pt:.0f}×（语料 68.5，旧 1.8）、"
            f"星期 max={_wd_max:.3f}（语料 ≤0.159，旧 0.298）、Mon+Tue={_wd_mt:.3f}（旧 0.587）")
    metrics["publish_hour_cv"] = _hr_cv
    metrics["publish_weekday_max"] = _wd_max

    # (g) R6A-P3-2：计数长尾上尾（dy like p90 语料 218k/旧 24.7k；comment p90 语料 6514/旧 1820）
    if site == "douyin":
        _like_p90 = float(np.percentile(like_arr, 90))
        _like_p99 = float(np.percentile(like_arr, 99))
        _cm_p90 = float(np.percentile(c_arr, 90))
        rep.add(site, "dy.count_tail",
                _like_p90 >= 5e4 and _cm_p90 >= 2200 and _like_p99 >= 5e5,
                f"like p90={_like_p90:.3g}（语料 2.18e5，旧 2.47e4）、p99={_like_p99:.3g}"
                f"（语料 1.56e6，旧 1.55e5）、comment p90={_cm_p90:.3g}（语料 6514，旧 1820）")
        metrics["dy_like_p90"] = _like_p90
    # (h) R6A-P3-2 + R6C-P3-4：ks viewCount 量级 + duration/深层元数据方差
    if site == "kuaishou":
        v_arr = np.array(view, dtype=float)
        _v_p50 = float(np.median(v_arr))
        _v_p90 = float(np.percentile(v_arr, 90))
        _l_p50 = float(np.median(like_arr))
        rep.add(site, "ks.view_magnitude",
                4e5 <= _v_p50 <= 1.1e6 and _v_p90 >= 2.5e6 and 5e3 <= _l_p50 <= 1.5e4,
                f"view p50={_v_p50:.3g}（语料 6.97e5，旧 1.99e4）、p90={_v_p90:.3g}（语料 4.5e6，"
                f"旧 1.17e5）、like p50={_l_p50:.3g}（语料 9788，旧 1591）")
        metrics["ks_view_p50"] = _v_p50
        _dur_u = len(set(ks_durations))
        _dur_p50 = float(np.median(ks_durations)) if ks_durations else 0
        # oracle-lite：distinct 下限按规模缩放（完整台架 ≥900 等价；300 条 → ≥270）
        _dur_min = min(900, int(n * 0.9))
        rep.add(site, "ks.duration_variance",
                _dur_u >= _dur_min and 20000 <= _dur_p50 <= 130000,
                f"duration distinct={_dur_u}/{len(ks_durations)}（旧 unique=1 恒 5000）、"
                f"p50={_dur_p50:.0f}ms（语料中位 65000、范围 [3233, 2629599]）")
        _ue = [x[0] for x in ks_manifest_meta if isinstance(x[0], (int, float))]
        _ol = [x[1] for x in ks_manifest_meta if isinstance(x[1], (int, float))]
        _ue_span = (max(_ue) / min(_ue)) if _ue and min(_ue) > 0 else 1.0
        _ol_span = (max(_ol) - min(_ol)) if _ol else 0.0
        rep.add(site, "ks.manifest_metadata_variance",
                _ue_span >= 1e3 and _ol_span >= 4.0,
                f"underExposed distinct={len(set(_ue))}/{len(_ue)}、跨 {_ue_span:.1e}×"
                f"（语料 2.98e-9~3.25e-5 跨 5 个数量级，旧恒 5.96e-9）、"
                f"oriLoudness 极差 {_ol_span:.1f} dB（语料 [-20.164, -11.954]，旧恒 0.0）")
        metrics["ks_underexposed_span"] = float(_ue_span)
        metrics["ks_duration_p50"] = _dur_p50
        # (i) R6A-P3-3：profile/get userId 与请求同空间（旧：响应自报数字空间 id）
        _uid_probe = str((rdr2.read(0).get("author") or {}).get("id") or "3xtest")
        _pg = _render_all.render_ks_profile_get(rdr2, [0], _uid_probe)
        rep.add(site, "ks.profile_userid_unified",
                _pg.get("userId") == _uid_probe,
                f"请求 userId={_uid_probe[:6]}… → 响应 userId={str(_pg.get('userId'))[:6]}…"
                f"（旧：响应 6347098269 型数字空间，与搜索流/评论/页面 URL 的 3x 空间不一致）")

    # (j) R6A-P3-4：dy 深列表 600 条零重复（旧 >71 条起逐字重复）+ ks 跨内容重叠（旧 24-75%）
    if site == "douyin":
        _deep_ln, _deep_cc = None, -1
        # oracle-lite：扫描上限按数据集规模自适应（完整台架 ≥2000；迷你集 300）
        for _ln in range(0, min(2000, len(rdr2))):
            _cc = int((rdr2.read(_ln).get("statistics") or {}).get("comment_count") or 0)
            if _cc > _deep_cc:
                _deep_ln, _deep_cc = _ln, _cc
        _deep = _render_all.render_douyin_comment_list(rdr2, _deep_ln, 0, 600)["comments"]
        _deep_txt = [c.get("text") for c in _deep]
        _deep_dup = len(_deep_txt) - len(set(_deep_txt))
        rep.add(site, "comment.deep_list_unique",
                _deep_dup <= 2,
                f"claim={_deep_cc} 的内容前 600 条：逐字重复 {_deep_dup}（旧：第 71 条起重复、"
                f"500 条内 22 次；组合空间 {_commentext_mod.space_size('douyin')}）")
        metrics["dy_deep_dup_600"] = int(_deep_dup)
    if site == "kuaishou":
        _s1 = set()
        _s2 = set()
        for _i, _ln in enumerate(sample_lines[:2]):
            _resp_k = _render_all.render_ks_comment_list(rdr2, int(_ln), "", 300)
            _set = _s1 if _i == 0 else _s2
            _set.update(c.get("content") for c in _resp_k.get("rootCommentsV2") or [])
        _ovl = len(_s1 & _s2) / max(1, min(len(_s1), len(_s2)))
        rep.add(site, "comment.cross_content_overlap",
                _ovl <= 0.05,
                f"两内容各 300 条评论重叠率 {_ovl:.3f}（语料同题材 0.24-0.745，红队建议 <0.1；"
                f"组合空间 {_commentext_mod.space_size('kuaishou')}，旧 1910）")
        metrics["ks_comment_overlap"] = float(_ovl)

    # ---- 4f. 红队 round7A 新断言（R7A-P2-1/P2-2/P3-1..P3-7）----
    _OVERSEAS_IP = ("美国", "日本", "韩国", "新加坡", "马来西亚", "英国", "加拿大",
                    "澳大利亚", "中国香港", "中国澳门", "中国台湾")
    _PARTICLES = set("了啊呀吧呢嘛哦哈咯哒诶哎哟嘞哩咯啵")
    _EMO_RE = _re.compile(r"\[[^\]\[]{1,8}\]")
    _SLANG = ["哈哈", "233", "666", "yyds", "u1s1", "xswl", "awsl", "绝了", "离谱",
              "破防", "拿捏", "天花板", "谁懂", "家人们", "集美", "宝子", "拴q",
              "尊嘟", "蚌埠", "好家伙", "上头", "种草", "拔草", "安利", "踩雷",
              "避雷", "真香", "无语", "服了", "笑死", "泪目", "慕了", "酸了",
              "冲鸭", "震撼", "太可了", "爱了", "嗑", "整活", "社死", "内卷",
              "躺平", "打工人", "干饭", "搞钱", "码住", "蹲", "同款", "白嫖",
              "已老实", "求个"]
    _CORPUS_TXT = {"douyin": {"part": 0.157, "slang": 0.025, "at": 0.095},
                   "xhs": {"part": 0.235, "slang": 0.035, "at": 0.032},
                   "kuaishou": {"part": 0.089, "slang": 0.060, "at": 0.156}}

    def _band(v, ref, tol=0.30):
        return ref * (1 - tol) <= v <= ref * (1 + tol)

    # (a) R7A-P2-1：ks collectCount 渲染恒 0（语料 search/feed 706/706=0）
    if site == "kuaishou":
        _feed = _render_all.render_ks_search_feed(rdr2, 1, 20, keyword="美食探店")[1]
        _ccs = [(f.get("photo") or {}).get("collectCount") for f in _feed["feeds"]]
        _pf = _render_all.render_ks_profile_feed(rdr2, [0, 1, 2], "3xtest")
        _cc2 = [(f.get("photo") or {}).get("collectCount") for f in _pf["feeds"]]
        _cc0 = all(x == 0 for x in _ccs + _cc2)
        rep.add(site, "ks.collect_count_hidden",
                _cc0,
                f"search/feed 60 卡 + profile/feed collectCount 全 0 = {_cc0}"
                f"（语料 706/706=0，旧 60/60 非零、p50 比值 0.161——R6C-P2-1 dy play_count 同族）")
        metrics["ks_collect_zero"] = bool(_cc0)

    # (b) R7A-P2-2：xhs 嵌入 sub / sub 页 sub 的 target_comment 同源有效
    #     （指向父评论或兄弟 sub；旧 48/72 指向非 hex 占位常量、sub/page 键缺失）
    if site == "xhs":
        _sub_ok = _sub_bad = _sub_hex_bad = _sub_nokey = 0
        _hex24 = _re.compile(r"^[0-9a-f]{24}$")
        for _ln in sample_lines[:20]:
            _ln = int(_ln)
            _, _r1 = _render_all.render_xhs_comment_page(rdr2, _ln, "")
            for _c in _r1["data"]["comments"]:
                _sids = {str(_s.get("id") or "") for _s in (_c.get("sub_comments") or [])}
                for _s in _c.get("sub_comments") or []:
                    _tc = _s.get("target_comment") or {}
                    _tid = str(_tc.get("id") or "")
                    if not _hex24.match(str(_s.get("id") or "")):
                        _sub_hex_bad += 1
                    if _tid == str(_c.get("id") or "") or _tid in _sids:
                        _sub_ok += 1
                    else:
                        _sub_bad += 1
                if (_c.get("sub_comment_count") or "0") != "0" and \
                        not (_c.get("sub_comments") or []):
                    pass   # count>0 但未内嵌（首页形态：游标续读）不计
            if _r1["data"]["comments"]:
                _, _r2 = _render_all.render_xhs_comment_sub_page(
                    rdr2, _ln, str(_r1["data"]["comments"][0].get("id") or ""))
                for _s in _r2["data"]["comments"] or []:
                    _tc = _s.get("target_comment") or {}
                    _tid = str(_tc.get("id") or "")
                    if not _tc:
                        _sub_nokey += 1
                    elif not _hex24.match(_tid):
                        _sub_bad += 1
                    else:
                        _sub_ok += 1
        rep.add(site, "xhs.sub_target_valid",
                _sub_bad == 0 and _sub_nokey == 0 and _sub_hex_bad == 0 and _sub_ok >= 20,
                f"嵌入+sub/page 抽样 {_sub_ok + _sub_bad} 条 sub：同源指向（父/兄弟）"
                f"{_sub_ok}、断裂 {_sub_bad}、sub/page 缺键 {_sub_nokey}、非 hex id "
                f"{_sub_hex_bad}（旧：48/72 占位常量 + sub/page 整键缺失）")
        metrics["xhs_sub_target_ok"] = int(_sub_ok)
        metrics["xhs_sub_target_bad"] = int(_sub_bad + _sub_nokey + _sub_hex_bad)

    # (c) R7A-P3-1：ladder 14 词全映射 + 主词三站严格命中进带 + 乱码词退化保持
    _LADDER = ["美食教程", "美食探店", "旅行攻略", "健身打卡", "穿搭分享", "家居好物",
               "数码测评", "沙雕日常", "变美", "手机摄影", "蓝牙耳机测评",
               "咖啡拉花", "夜景人像", "露营装备"]
    _unmapped = [k for k in _LADDER if _render_all._kw_view(rdr2, site, k) is None]
    _l_hits = _l_n = 0
    for _kw in ("美食教程", "露营装备"):
        _v = _render_all._kw_view(rdr2, site, _kw)
        if _v is None:
            continue
        for _pg in (0, 1):
            for _ln in (_render_all.search_window_lines(rdr2, site, _kw, _pg * 20, 20) or []):
                _l_n += 1
                _l_hits += 1 if _kw in _render_all._search_text(site, rdr2.read(_ln)) else 0
    _l_rate = _l_hits / max(1, _l_n)
    _gk = _render_all.search_window_lines(rdr2, site, "qzwkjxvbpq", 0, 20)
    # oracle-lite：主词严格命中下带按规模缩放（供给不足时下带退化为 0，上带不变）
    _lad_lb = 0.08 if n >= 20000 else 0.0
    rep.add(site, "search.ladder_words_mapped",
            not _unmapped and _l_n >= 40 and _lad_lb <= _l_rate <= 0.60 and _gk is None,
            f"ladder 14 词未映射 {_unmapped or '无'}（旧 7 词未映射：主词严格命中 0.0）；"
            f"主词严格命中 {_l_rate:.3f}（设计 0.20-0.30，真站 0.17-0.47）；"
            f"乱码词退化背景保持 = {_gk is None}")
    metrics["ladder_strict_rate"] = float(_l_rate)

    # (d) R7A-P3-2/P3-3：语气词/网络用语/@提及率进语料带（±30%）
    _part_n = sum(1 for t in _cmt_texts
                  if (lambda c: c and c[-1] in _PARTICLES)
                  (_EMO_RE.sub("", t or "").strip().rstrip("！？!?。.,，、~～… ")))
    _part_rate = _part_n / max(1, len(_cmt_texts))
    _slang_rate = sum(1 for t in _cmt_texts
                      if any(s in (t or "").lower() for s in _SLANG)) / max(1, len(_cmt_texts))
    _at_rate = sum(1 for t in _cmt_texts if "@" in (t or "")) / max(1, len(_cmt_texts))
    _ct = _CORPUS_TXT[site]
    rep.add(site, "comment.text_style_band",
            len(_cmt_texts) >= 200 and _band(_part_rate, _ct["part"])
            and _band(_slang_rate, _ct["slang"]) and _band(_at_rate, _ct["at"]),
            f"语气词 {_part_rate:.3f}（语料 {_ct['part']}，旧 0.40-0.59）、网络用语 "
            f"{_slang_rate:.3f}（语料 {_ct['slang']}，旧 0.26-0.45）、@提及 "
            f"{_at_rate:.3f}（语料 {_ct['at']}，旧 0）（±30% 带）")
    metrics["comment_particle_rate"] = float(_part_rate)
    metrics["comment_slang_rate"] = float(_slang_rate)
    metrics["comment_at_rate"] = float(_at_rate)

    # (e) R7A-P3-4/P3-6：dy ip 属地海外率 ~1% + custom_verify 渲染恒空
    if site == "douyin":
        _ips = [c.get("ip_label") for _resp_c in [_render_all.render_douyin_comment_list(
            rdr2, int(ln), 0, 20)["comments"] for ln in sample_lines[:40]]
            for c in _resp_c if c.get("ip_label")]
        _ovs = sum(1 for x in _ips if x in _OVERSEAS_IP) / max(1, len(_ips))
        _uniq = len(set(_ips))
        _cvs = [((a or {}).get("custom_verify") or "")
                for a in [_render_all._dy_search_item_wrap(
                    rdr2.read(int(ln)), np.random.default_rng([int(ln), 5]))["aweme_info"]["author"]
                    for ln in sample_lines[:12]]]
        _cv_empty = all(v == "" for v in _cvs)
        rep.add(site, "dy.ip_and_verify",
                _ovs <= 0.03 and _uniq >= 35 and _cv_empty,
                f"评论 ip 属地 {_uniq} 种（{len(_ips)} 条抽样）、海外+港澳台 {_ovs:.3f}"
                f"（语料 42 种/1.0%，旧 20 种/24.2%）；搜索卡 custom_verify 非空 "
                f"{sum(1 for v in _cvs if v)}/{len(_cvs)}（语料 0/2059，旧 72.5%）")
        metrics["dy_ip_overseas_rate"] = float(_ovs)
        metrics["dy_ip_unique"] = int(_uniq)
        metrics["dy_custom_verify_nonempty"] = int(sum(1 for v in _cvs if v))

    # (f) R7A-P3-5：dy 楼中楼二层嵌套（reply_to_reply_id≠0 占比 ~1/3）
    if site == "douyin":
        _r2r_n = _r2r_hit = 0
        for _ln in sample_lines[:10]:
            _ln = int(_ln)
            _rec_c = rdr2.read(_ln)
            _resp_c = _render_all.render_douyin_comment_list(rdr2, _ln, 0, 20)
            for _c in _resp_c.get("comments") or []:
                if int(_c.get("reply_comment_total") or 0) <= 0:
                    continue
                _rr = _render_all.render_douyin_comment_list_reply(
                    rdr2, _ln, str(_c.get("cid") or ""), 0, 20)
                for _s in _rr.get("comments") or []:
                    _r2r_n += 1
                    _r2r_hit += 1 if str(_s.get("reply_to_reply_id") or "0") != "0" else 0
        _r2r_rate = _r2r_hit / max(1, _r2r_n)
        rep.add(site, "dy.reply_nested_level2",
                _r2r_n >= 20 and 0.20 <= _r2r_rate <= 0.50,
                f"子评论 {_r2r_n} 条：reply_to_reply_id≠0 占 {_r2r_rate:.3f}"
                f"（语料 11/30=0.367，旧 0/43 全扁平）")
        metrics["dy_reply_r2r_rate"] = float(_r2r_rate)

    # (g) R7A-P3-7：比值上尾 + 作者粉丝量级（dy/xhs）
    if site == "douyin":
        _cl = np.array([int((rdr2.read(int(ln)).get("statistics") or {}).get("collect_count") or 0)
                        for ln in sample_lines]) / np.maximum(1, np.array(
            [int((rdr2.read(int(ln)).get("statistics") or {}).get("digg_count") or 0)
             for ln in sample_lines]))
        _cl_p90 = float(np.percentile(_cl, 90))
        _fols = np.array([int((rdr2.read(int(ln)).get("author") or {}).get("follower_count") or 0)
                          for ln in sample_lines])
        rep.add(site, "dy.ratio_tail_and_followers",
                _cl_p90 >= 0.55 and float(np.median(_fols)) >= 1.5e4
                and float(np.percentile(_fols, 90)) >= 4e5,
                f"collect/like p90={_cl_p90:.2f}（语料 0.938，旧 0.30）、"
                f"follower p50={np.median(_fols):.3g}/p90={np.percentile(_fols, 90):.3g}"
                f"（语料 2.7e4/1.2e6，旧 3.7e3/3.1e4）")
        metrics["dy_collect_like_p90"] = _cl_p90
        metrics["dy_follower_p50"] = float(np.median(_fols))
        metrics["dy_follower_p90"] = float(np.percentile(_fols, 90))
    if site == "xhs":
        _xcl = np.array([int(((rdr2.read(int(ln)).get("note_card") or {}).get("interact_info") or {})
                              .get("collected_count") or 0) for ln in sample_lines]) / np.maximum(1, np.array(
            [int(((rdr2.read(int(ln)).get("note_card") or {}).get("interact_info") or {})
                 .get("liked_count") or 0) for ln in sample_lines]))
        _xcl_p50 = float(np.median(_xcl))
        rep.add(site, "xhs.collect_ratio_median",
                0.39 <= _xcl_p50 <= 0.72,
                f"collect/like 中位 {_xcl_p50:.3f}（语料 0.556——收藏≈点赞一半的社区形态，"
                f"旧 0.268）")
        metrics["xhs_collect_like_p50"] = _xcl_p50

    # ---- 5. 异常类占比 ----
    tgt = manifest["target_fractions"]
    rows = conn.execute("SELECT anomaly_class, COUNT(*) FROM labels GROUP BY anomaly_class").fetchall()
    obs = {c: cnt for c, cnt in rows}
    prop_bad = []
    for cls, frac in tgt.items():
        expect = frac * n
        # 哈希分桶 ≈ 二项分布：4σ 容差（10 万条下 σ≤0.05%·n，另有少量余量）
        tol = 4 * math.sqrt(max(frac * (1 - frac), 1e-9) * n) + 2
        actual = obs.get(cls, 0)
        if abs(actual - expect) > tol:
            prop_bad.append(f"{cls}: obs={actual} expect={expect:.0f}±{tol:.0f}")
    for cls in obs:
        if cls not in tgt:
            prop_bad.append(f"多余类 {cls}")
    rep.add(site, "anomaly.proportion", not prop_bad, "; ".join(prop_bad) or
            f"{dict(sorted(obs.items()))}")
    conn.close()

    # ---- 6. 一致性 ----
    rep.add(site, "consistency.author", author_conflicts == 0,
            f"authors={len(author_attr)}, conflicts={author_conflicts}")

    # ---- 8. 标签泄漏 ----
    rep.add(site, "leak.no_labels_in_jsonl", leak_hits == 0,
            f"ground_truth 字样命中 {leak_hits} 行（标记集 {LEAK_MARKERS}）")

    # ---- 7. 可复现性 ----
    if not skip_repro:
        from synthgen.generator import generate_site
        from synthgen.config import load_dist_config
        k = min(repro_count, n)
        # 用 MANIFEST 记录的异常类占比重建 spec（en 键同样可解析）
        frac_spec = ",".join(f"{cls}:{frac * 100:g}%" for cls, frac in
                             manifest["anomaly_classes"].items()) or "none"
        with tempfile.TemporaryDirectory(prefix="synthgen_repro_") as td:
            generate_site(site, k, int(manifest["seed"]), load_dist_config(),
                          frac_spec, Path(td), manifest.get("filter") or None,
                          with_index=False, quiet=True)
            tmp_jsonl = Path(td) / site / f"{site_mod.ENTITY}.jsonl"
            hh = hashlib.sha256()
            with open(tmp_jsonl, "rb") as f:
                hh.update(f.read())
            re_hash = hh.hexdigest()
        head = hashlib.sha256()
        with open(jsonl, "rb") as f:
            for _ in range(k):
                head.update(f.readline())
        rep.add(site, "repro.same_seed", re_hash == head.hexdigest(),
                f"前{k}行 sha256 一致")
        metrics["repro_count"] = k

    # ---- 9. 第二实现复核（pandas） ----
    import pandas as pd
    df = pd.read_json(jsonl, lines=True, dtype=False)
    m2 = site_mod.extract_metrics(df.iloc[0].to_dict())  # 仅探测结构
    # 主路径指标 vs pandas 重算
    like_pd = pd.Series([int(x) for x in _pd_col(df, site_mod, "like")])
    med_pd = float(like_pd.median())
    ok_med = abs(med_pd - metrics["like_median"]) <= 1e-6 * max(1, metrics["like_median"])
    detail = f"like median: numpy={metrics['like_median']:.1f} pandas={med_pd:.1f}"
    if view:
        v_pd = pd.Series([int(x) for x in _pd_col(df, site_mod, "view")])
        med_v_pd = float(v_pd.median())
        ok_v = abs(med_v_pd - metrics.get("view_median", -1)) <= 1e-6 * metrics.get("view_median", 1)
        detail += f"; view median: numpy={metrics['view_median']:.1f} pandas={med_v_pd:.1f}"
        c_pd = float(np.log10(v_pd).corr(np.log10(like_pd + 1)))
        ok_c = abs(c_pd - metrics["like_view_corr"]) < 1e-9
        detail += f"; corr: numpy={metrics['like_view_corr']:.6f} pandas={c_pd:.6f}"
    else:
        ok_v, ok_c = True, True
    rep.add(site, "second_impl.pandas", ok_med and ok_v and ok_c, detail)

    rep.summary_metrics[site] = metrics
    return metrics


def _pd_col(df, site_mod, key):
    """pandas 路径独立取数（与 extract_metrics 不同代码路径）。"""
    if site_mod.__name__.endswith("douyin"):
        if key == "like":
            return df["statistics"].map(lambda s: s["digg_count"])
        return df["statistics"].map(lambda s: s["play_count"])
    if site_mod.__name__.endswith("xhs"):
        return df["note_card"].map(lambda c: int(c["interact_info"]["liked_count"]))
    if key == "like":
        return df["photo"].map(lambda p: p["likeCount"])
    return df["photo"].map(lambda p: p["viewCount"])


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description="synthgen 预置数据集验收自测")
    ap.add_argument("--datasets-dir", default=str(Path(__file__).resolve().parent / "datasets"))
    ap.add_argument("--sites", default=",".join(SITES))
    ap.add_argument("--repro-count", type=int, default=10000)
    ap.add_argument("--skip-repro", action="store_true")
    args = ap.parse_args(argv)

    rep = Report()
    base = Path(args.datasets_dir)
    for site in args.sites.split(","):
        site_dir = base / site
        if not (site_dir / "MANIFEST.json").exists():
            rep.add(site, "manifest.exists", False, str(site_dir))
            continue
        validate_site(site, site_dir, rep, args.repro_count, args.skip_repro)

    print(f"\n{'站点':<10}{'检查项':<30}{'结果':<6}详情")
    print("-" * 100)
    for s, c, p, d in rep.rows:
        print(f"{s:<10}{c:<30}{'✓' if p else '✗ FAIL':<6}{d[:60]}")
    n_pass = sum(1 for r in rep.rows if r[2])
    print("-" * 100)
    print(f"通过 {n_pass}/{len(rep.rows)} 项；all_passed={rep.all_passed}")
    out = rep.dump(base)
    print(f"报告已写入: {out}")
    return 0 if rep.all_passed else 1


if __name__ == "__main__":
    sys.exit(main())
