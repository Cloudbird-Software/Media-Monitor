"""配置与参考值池加载。"""
from __future__ import annotations

import json
from datetime import datetime
from pathlib import Path

import yaml

BASE_DIR = Path(__file__).resolve().parent
ORACLE_DIR = BASE_DIR.parent
CONTRACT_DIR = ORACLE_DIR / "contracts"
DATA_DIR = BASE_DIR / "data"
DEFAULT_DIST_CONFIG = BASE_DIR / "distributions.yaml"


def load_dist_config(path=None) -> dict:
    p = Path(path) if path else DEFAULT_DIST_CONFIG
    with open(p, "r", encoding="utf-8") as f:
        return yaml.safe_load(f)


def anchor_epoch(cfg: dict) -> int:
    iso = cfg["meta"]["anchor_time"]
    dt = datetime.fromisoformat(iso)
    return int(dt.timestamp())


def load_json(path) -> dict:
    with open(path, "r", encoding="utf-8") as f:
        return json.load(f)


def load_categories() -> list[dict]:
    return load_json(DATA_DIR / "categories.json")["categories"]


def load_site_pools(site: str) -> dict:
    return load_json(DATA_DIR / f"{site}_pools.json")


def contract_path(site: str, endpoint_id: str) -> Path:
    return CONTRACT_DIR / site / f"{endpoint_id}.contract.json"
