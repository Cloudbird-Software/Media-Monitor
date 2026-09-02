"""ground_truth.db（sqlite）：唯一存放异常类真实标签的地方。

站点数据本体（JSONL）中绝无 anomaly/class/label 等字段（validate 泄漏检查兜底）。
阶段 5 评分器以 (site, record_id) 关联标签。
"""
from __future__ import annotations

import json
import sqlite3
from pathlib import Path

SCHEMA = """
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE IF NOT EXISTS labels (
    site TEXT NOT NULL,
    record_id TEXT NOT NULL,
    line_no INTEGER NOT NULL,
    anomaly_class TEXT NOT NULL,
    is_anomaly INTEGER NOT NULL,
    class_zh TEXT NOT NULL,
    PRIMARY KEY (site, record_id)
);
CREATE INDEX IF NOT EXISTS idx_labels_class ON labels(site, anomaly_class);
"""


class GroundTruthWriter:
    def __init__(self, path: Path, site: str, meta: dict):
        path.parent.mkdir(parents=True, exist_ok=True)
        if path.exists():
            path.unlink()
        self.conn = sqlite3.connect(path)
        self.conn.executescript(SCHEMA)
        rows = [(k, json.dumps(v, ensure_ascii=False) if isinstance(v, (dict, list)) else str(v))
                for k, v in meta.items()]
        self.conn.executemany("INSERT OR REPLACE INTO meta(key,value) VALUES(?,?)", rows)
        self.site = site
        self._buf: list[tuple] = []
        self._n = 0

    def add(self, record_id: str, line_no: int, anomaly_class: str, class_zh: str):
        self._buf.append((self.site, record_id, line_no, anomaly_class,
                          0 if anomaly_class == "normal" else 1, class_zh))
        self._n += 1
        if len(self._buf) >= 10000:
            self.flush()

    def flush(self):
        if self._buf:
            self.conn.executemany(
                "INSERT OR REPLACE INTO labels(site,record_id,line_no,anomaly_class,is_anomaly,class_zh)"
                " VALUES(?,?,?,?,?,?)", self._buf)
            self._buf.clear()
            self.conn.commit()

    def close(self):
        self.flush()
        self.conn.commit()
        self.conn.close()

    @property
    def count(self) -> int:
        return self._n


def open_reader(path: Path) -> sqlite3.Connection:
    conn = sqlite3.connect(f"file:{path}?mode=ro", uri=True)
    return conn
