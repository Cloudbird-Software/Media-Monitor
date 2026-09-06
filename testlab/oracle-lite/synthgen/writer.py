"""JSONL 输出 + 可选 sqlite 索引（便于阶段 3/5 快速分页/过滤定位）。"""
from __future__ import annotations

import json
import sqlite3
from pathlib import Path

INDEX_SCHEMA = """
CREATE TABLE IF NOT EXISTS records (
    line_no INTEGER PRIMARY KEY,
    record_id TEXT NOT NULL,
    category TEXT NOT NULL,
    author_id TEXT NOT NULL,
    view INTEGER NOT NULL,
    like INTEGER NOT NULL,
    comment INTEGER NOT NULL,
    collect INTEGER NOT NULL,
    share INTEGER NOT NULL,
    publish_ts INTEGER NOT NULL,
    followers INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_rec_view ON records(view);
CREATE INDEX IF NOT EXISTS idx_rec_like ON records(like);
CREATE INDEX IF NOT EXISTS idx_rec_pub ON records(publish_ts);
CREATE INDEX IF NOT EXISTS idx_rec_author ON records(author_id);
CREATE INDEX IF NOT EXISTS idx_rec_cat ON records(category);
"""


class JsonlWriter:
    def __init__(self, path: Path):
        path.parent.mkdir(parents=True, exist_ok=True)
        if path.exists():
            path.unlink()
        self.f = open(path, "w", encoding="utf-8", newline="\n")
        self.n = 0

    def write(self, record: dict):
        self.f.write(json.dumps(record, ensure_ascii=False, separators=(",", ":")))
        self.f.write("\n")
        self.n += 1

    def close(self):
        self.f.close()


class IndexWriter:
    def __init__(self, path: Path):
        path.parent.mkdir(parents=True, exist_ok=True)
        if path.exists():
            path.unlink()
        self.conn = sqlite3.connect(path)
        self.conn.executescript(INDEX_SCHEMA)
        self._buf: list[tuple] = []

    def add(self, line_no: int, record_id: str, stats: dict, author_id: str):
        self._buf.append((line_no, record_id, stats["category"], author_id,
                          stats["view"], stats["like"], stats["comment"],
                          stats["collect"], stats["share"], stats["publish_ts"],
                          stats["followers"]))
        if len(self._buf) >= 10000:
            self.flush()

    def flush(self):
        if self._buf:
            self.conn.executemany(
                "INSERT INTO records(line_no,record_id,category,author_id,view,like,comment,"
                "collect,share,publish_ts,followers) VALUES(?,?,?,?,?,?,?,?,?,?,?)", self._buf)
            self._buf.clear()
            self.conn.commit()

    def close(self):
        self.flush()
        self.conn.commit()
        self.conn.close()


def sha256_of(path: Path, bufsize: int = 1 << 20) -> str:
    import hashlib
    h = hashlib.sha256()
    with open(path, "rb") as f:
        while True:
            b = f.read(bufsize)
            if not b:
                break
            h.update(b)
    return h.hexdigest()
