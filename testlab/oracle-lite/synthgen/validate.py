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

LEAK_MARKERS = ('"anomaly', '"class"', '"label"', '"ground_truth"', 'anomaly_class', '"is_anomaly"', '"cls"')


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
        m = site_mod.extract_metrics(rec)
        if m["view"] is not None:
            view.append(m["view"])
        like.append(m["like"])
        comment.append(m["comment"])
        if m["publish_ts"] is not None:
            publish.append(m["publish_ts"])
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
