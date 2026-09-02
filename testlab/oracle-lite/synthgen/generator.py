"""合成数据生成器 CLI（阶段 4 主入口）。

用法示例：
  python synthgen/generator.py --site all --count 100000 --seed 20260901 --with-index
  python synthgen/generator.py --site douyin --count 10000 --seed 7 \
      --anomaly-classes "高赞低播放:5%,新发布高互动:3%" \
      --filter "like>100000 AND published_within_24h" --out-dir synthgen/datasets/smoke
"""
from __future__ import annotations

import argparse
import importlib
import json
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

_PKG_PARENT = str(Path(__file__).resolve().parent.parent)
if _PKG_PARENT not in sys.path:
    sys.path.insert(0, _PKG_PARENT)

from synthgen import SITE_CODES, SITES, __version__
from synthgen.anomalies import CLASS_NAMES, parse_anomaly_spec, target_fractions
from synthgen.config import DEFAULT_DIST_CONFIG, anchor_epoch, load_categories, load_dist_config
from synthgen.filters import compile_filter
from synthgen.groundtruth import GroundTruthWriter
from synthgen.pools import build_context
from synthgen.rngutil import record_rng
from synthgen.writer import IndexWriter, JsonlWriter, sha256_of


def generate_site(
    site: str,
    count: int,
    seed: int,
    dist_cfg: dict,
    anomaly_spec: str | None,
    out_dir: Path,
    filter_expr: str | None = None,
    with_index: bool = False,
    quiet: bool = False,
) -> dict:
    """生成单站数据集：JSONL + ground_truth.db (+ index.db) + MANIFEST.json。"""
    site_code = SITE_CODES[site]
    categories = load_categories()
    ctx = build_context(site, site_code, seed, dist_cfg, categories)
    fracs = parse_anomaly_spec(anomaly_spec, dist_cfg.get("anomalies", {}))
    filt = compile_filter(filter_expr)
    mod = importlib.import_module(f"synthgen.sites.{site}")

    site_dir = Path(out_dir) / site
    site_dir.mkdir(parents=True, exist_ok=True)
    jsonl_path = site_dir / f"{mod.ENTITY}.jsonl"
    gt_path = site_dir / "ground_truth.db"
    idx_path = site_dir / "index.db"

    meta = {
        "tool": f"synthgen/{__version__}",
        "site": site,
        "entity": mod.ENTITY,
        "seed": seed,
        "anchor_time": dist_cfg["meta"]["anchor_time"],
        "anomaly_classes": {k: v for k, v in sorted(fracs.items())},
        "anomaly_class_names_zh": {k: CLASS_NAMES[k] for k in fracs},
        "filter": filter_expr or "",
    }
    gt = GroundTruthWriter(gt_path, site, meta)
    jw = JsonlWriter(jsonl_path)
    iw = IndexWriter(idx_path) if with_index else None

    t0 = time.time()
    line_no = 0
    class_counts: dict[str, int] = {}
    try:
        for i in range(count):
            rng = record_rng(seed, site_code, i)
            stats = ctx.engine.sample_stats(rng, i, fracs)
            author = ctx.pick_author(rng)
            stats["followers"] = author["followers"]
            if filt is not None and not filt(stats):
                continue
            record = mod.build_record(rng, stats, author, ctx)
            record_id, author_id = mod.record_identity(record)
            cls = stats["anomaly_class"]
            jw.write(record)
            gt.add(record_id, line_no, cls, CLASS_NAMES[cls])
            if iw is not None:
                iw.add(line_no, record_id, stats, author_id)
            class_counts[cls] = class_counts.get(cls, 0) + 1
            line_no += 1
            if not quiet and line_no % 20000 == 0:
                print(f"  [{site}] {line_no} records ({time.time()-t0:.1f}s)", flush=True)
    finally:
        jw.close()
        gt.close()
        if iw is not None:
            iw.close()

    manifest = {
        **meta,
        "count_requested": count,
        "count_emitted": jw.n,
        "gt_rows": gt.count,
        "class_counts": dict(sorted(class_counts.items())),
        "target_fractions": {k: v for k, v in sorted(target_fractions(fracs).items())},
        "generated_at_utc": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "distribution_config": dist_cfg,
        "files": {
            "jsonl": {"name": jsonl_path.name, "lines": jw.n, "bytes": jsonl_path.stat().st_size},
            "ground_truth_db": {"name": gt_path.name},
            "index_db": {"name": idx_path.name, "present": iw is not None},
        },
    }
    with open(site_dir / "MANIFEST.json", "w", encoding="utf-8") as f:
        json.dump(manifest, f, ensure_ascii=False, indent=2)
    # JSONL 哈希最后计算并补写（MANIFEST 单独文件，不影响数据可复现性口径）
    manifest["files"]["jsonl"]["sha256"] = sha256_of(jsonl_path)
    with open(site_dir / "MANIFEST.json", "w", encoding="utf-8") as f:
        json.dump(manifest, f, ensure_ascii=False, indent=2)

    if not quiet:
        size_mb = manifest["files"]["jsonl"]["bytes"] / 1e6
        print(
            f"[{site}] done: {jw.n} records, {size_mb:.1f} MB, "
            f"{time.time()-t0:.1f}s, classes={class_counts}", flush=True
        )
    return manifest


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description="三站合成数据生成器（Faker zh_CN 基座，全链路种子化）")
    ap.add_argument("--site", default="all", choices=[*SITES, "all"], help="站点（默认 all）")
    ap.add_argument("--count", type=int, default=100000, help="生成条数（默认 100000）")
    ap.add_argument("--seed", type=int, default=20260901, help="全局种子（默认 20260901）")
    ap.add_argument("--anomaly-classes", default=None,
                    help='异常类占比，如 "高赞低播放:2%%,低赞高播放:1.5%%"；缺省用 distributions.yaml')
    ap.add_argument("--filter", default=None,
                    help='输出子集过滤，如 "like>100000 AND published_within_24h"')
    ap.add_argument("--out-dir", default=str(Path(__file__).resolve().parent / "datasets"))
    ap.add_argument("--with-index", action="store_true", help="附带 sqlite 索引 index.db")
    ap.add_argument("--dist-config", default=str(DEFAULT_DIST_CONFIG))
    ap.add_argument("--quiet", action="store_true")
    args = ap.parse_args(argv)

    dist_cfg = load_dist_config(args.dist_config)
    _ = anchor_epoch(dist_cfg)  # 提前校验 anchor_time 合法
    sites = list(SITES) if args.site == "all" else [args.site]
    out_dir = Path(args.out_dir)

    manifests = {}
    for site in sites:
        manifests[site] = generate_site(
            site, args.count, args.seed, dist_cfg,
            args.anomaly_classes, out_dir, args.filter, args.with_index, args.quiet,
        )
    total = sum(m["count_emitted"] for m in manifests.values())
    print(f"OK: {len(sites)} site(s), {total} records -> {out_dir}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
