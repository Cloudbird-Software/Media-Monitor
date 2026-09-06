"""--filter 条件过滤：对输出子集按生成统计量筛选。

语法（大小写不敏感，AND 优先于 OR，可用括号）：
    like>100000 AND published_within_24h
    view>=50000 AND (like<1000 OR comment>500)
    category==美食探店 AND like_rate>0.1

字段：view like comment collect share like_rate age_hours age_days
      category followers published_within_{n}h / published_within_{n}d
注意：小红书契约不外显播放量，view 为潜在曝光（驱动 liked_count 生成），过滤仍可用。
"""
from __future__ import annotations

import re

TOKEN_RE = re.compile(
    r'\s*(\(|\)|&&|\|\||==|!=|>=|<=|>|<|published_within_\d+[hd]|[A-Za-z_][A-Za-z0-9_]*|"[^"]*"|\'[^\']*\'|[^\s()=!<>]+)'
)

NUMERIC_FIELDS = {
    "view", "like", "comment", "collect", "share", "like_rate",
    "age_hours", "age_days", "followers", "duration_ms",
}
WITHIN_RE = re.compile(r"^published_within_(\d+)([hd])$")


class FilterParser:
    def __init__(self, expr: str):
        self.tokens = self._tokenize(expr)
        self.pos = 0

    def _tokenize(self, expr: str) -> list[str]:
        out, pos = [], 0
        while pos < len(expr):
            m = TOKEN_RE.match(expr, pos)
            if not m:
                if expr[pos].isspace():
                    pos += 1
                    continue
                raise ValueError(f"过滤表达式无法解析: {expr!r} @ {expr[pos:]!r}")
            tok = m.group(1)
            pos = m.end()
            if tok.upper() in ("AND", "&&"):
                tok = "AND"
            elif tok.upper() in ("OR", "||"):
                tok = "OR"
            out.append(tok)
        return out

    def parse(self):
        node = self._or_expr()
        if self.pos != len(self.tokens):
            raise ValueError(f"过滤表达式存在多余 token: {self.tokens[self.pos:]}")
        return node

    def _peek(self):
        return self.tokens[self.pos] if self.pos < len(self.tokens) else None

    def _next(self):
        tok = self._peek()
        self.pos += 1
        return tok

    def _or_expr(self):
        node = self._and_expr()
        while self._peek() == "OR":
            self._next()
            node = ("or", node, self._and_expr())
        return node

    def _and_expr(self):
        node = self._clause()
        while self._peek() == "AND":
            self._next()
            node = ("and", node, self._clause())
        return node

    def _clause(self):
        tok = self._next()
        if tok == "(":
            node = self._or_expr()
            if self._next() != ")":
                raise ValueError("过滤表达式括号不匹配")
            return node
        if tok is None:
            raise ValueError("过滤表达式意外结束")
        m = WITHIN_RE.match(tok)
        if m:  # 布尔短句：published_within_24h
            n, unit = int(m.group(1)), m.group(2)
            hours = n * 24 if unit == "d" else n
            return ("within", hours)
        op = self._next()
        if op in (">", ">=", "<", "<=", "==", "!="):
            val = self._next().strip("'\"")
            return ("cmp", tok, op, val)
        raise ValueError(f"子句格式错误（期望 比较符）: {tok!r} {op!r}")


def compile_filter(expr: str | None):
    """返回 eval_stats(stats)->bool；None 表达式 → 恒 True。"""
    if not expr or not expr.strip():
        return None
    node = FilterParser(expr).parse()

    def ev(node, s: dict) -> bool:
        kind = node[0]
        if kind == "and":
            return ev(node[1], s) and ev(node[2], s)
        if kind == "or":
            return ev(node[1], s) or ev(node[2], s)
        if kind == "within":
            return s["age_hours"] <= node[1]
        if kind == "cmp":
            _, field, op, raw = node
            if field == "category":
                cur = s["category"]
                val = raw
                return {"==": cur == val, "!=": cur != val}.get(op, False)
            if field not in NUMERIC_FIELDS:
                raise ValueError(f"未知过滤字段: {field!r}（可用: {sorted(NUMERIC_FIELDS)}）")
            cur = float(s.get(field, 0.0))
            val = float(raw)
            return {
                ">": cur > val, ">=": cur >= val, "<": cur < val,
                "<=": cur <= val, "==": cur == val, "!=": cur != val,
            }[op]
        raise ValueError(f"未知节点: {node!r}")

    return lambda s: ev(node, s)
