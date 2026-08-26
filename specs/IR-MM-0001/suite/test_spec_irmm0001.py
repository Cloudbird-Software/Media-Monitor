#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""IR-MM-0001 套件——原子采集 MCP 化 spec 的结构+语义锚断言（adversary 目标目录契约）。

被审"实现" = impl-dir 下的 spec.md（文档对形态：本 IR 的交付物首件是条款级
规格本身）。断言四层（对齐 specs/IR-0001 Viral_Radar / QW_Arena1 套件口径）：
  L1 结构：frontmatter 字段、AC-1..AC-21 完备、GWT 三段、条款段齐备
  L2 语义锚：真实原子采集平台（MCP 工具面×契约×账号池×lab）才含的机制短语
             + 条款级锚绑定（42 项异质锚——同义模板句无法同时命中）
  L3 负向锚：偷懒改写最易缺的深水位标志（义务降级/弱化词/时态后移/逃生舱）
  L4 一致性：AC 数、编号连续、卡绑定与 IR-MM-0001 期望对齐；防模板句复用
防"最偷懒实现"（judge-deep）口径：S1' 摆拍式 AC、S2 义务降级、S3 义务转嫁、
S4 时态后移、S5 逃生舱、S6 前置堆叠。
"""
import os
import re
import sys
import unittest

_cwd = os.path.abspath(os.getcwd())
IMPL = None
if os.environ.get("IMPL_DIR"):
    IMPL = os.path.normpath(os.environ["IMPL_DIR"])
elif os.path.isfile(os.path.join(_cwd, "spec.md")):
    IMPL = _cwd
elif os.path.isfile(os.path.join(_cwd, "..", "spec.md")):
    IMPL = os.path.normpath(os.path.join(_cwd, ".."))
if IMPL is None:
    raise AssertionError("无法定位 impl 目录（IMPL_DIR 未设且 cwd 上下文无 spec.md）")
SPEC = os.path.join(IMPL, "spec.md")


def read(path):
    if not os.path.isfile(path):
        raise AssertionError(f"缺文件: {path}")
    with open(path, encoding="utf-8") as f:
        return f.read()


def frontmatter(text):
    m = re.match(r"^---\n(.*?)\n---\n", text, re.S)
    if not m:
        raise AssertionError("缺 frontmatter（--- 包裹的元数据块）")
    return m.group(1)


class L1Structure(unittest.TestCase):
    def test_frontmatter_keys(self):
        fm = frontmatter(read(SPEC))
        for k in ("taskId: IR-MM-0001", "specVersion:", "title:", "irRef:", "card:"):
            self.assertIn(k, fm, f"frontmatter 缺 {k}")

    def test_card_binding(self):
        fm = frontmatter(read(SPEC))
        self.assertIn("Cloudbird-Software/Media-Monitor#16", fm,
                      "card 字段必须绑定父意图 IR issue Media-Monitor#16")

    def test_ac_complete(self):
        s = read(SPEC)
        for i in range(1, 22):
            self.assertIn(f"- id: AC-{i}", s, f"缺 AC-{i}")
        self.assertEqual(len(re.findall(r"^\s*- id: AC-\d+", s, re.M)), 21,
                         "AC 总数应为 21（编号连续且无影子条款）")

    def test_ac_gwt(self):
        s = read(SPEC)
        for i in range(1, 22):
            m = re.search(rf"- id: AC-{i}\n\s+given: (.+)\n\s+when: (.+)\n\s+then: (.+)", s)
            self.assertIsNotNone(m, f"AC-{i} 缺 given/when/then 三段")
            g, w, t = (p.strip() for p in m.groups())
            self.assertGreaterEqual(len(g), 12, f"AC-{i} given 过短（<12 字）")
            self.assertGreaterEqual(len(w), 8, f"AC-{i} when 过短（<8 字）")
            self.assertGreaterEqual(len(t), 16, f"AC-{i} then 过短（<16 字）")

    def test_clause_sections(self):
        s = read(SPEC)
        for sec in ("## INV 不变量", "## BEH 行为", "## IFACE 契约",
                    "## BUDGET 预算", "## DECISION 决策", "## ASSUMPTION 假设"):
            self.assertIn(sec, s, f"缺条款段 {sec}")
        for uid in ("INV-1", "INV-7", "BEH-1", "BEH-16",
                    "IFACE-1", "IFACE-5", "DECISION-1", "DECISION-8",
                    "ASSUMPTION-1"):
            self.assertIn(uid, s, f"缺条款 {uid}")

    def test_nongoals(self):
        s = read(SPEC)
        self.assertIn("nonGoals:", s, "缺 nonGoals 段")
        for ng in ("编排引擎", "shipinhao", "MediaCrawler"):
            self.assertIn(ng, s, f"nonGoals 缺 {ng} 边界")


class L2SemanticAnchors(unittest.TestCase):
    """语义锚：真实原子采集平台才含的机制短语。42 项异质锚。"""

    ANCHORS = [
        # MCP 工具面（原子名）
        "search_items", "get_comments", "get_replies", "get_user_posts",
        "download_video", "accounts_list",
        # 契约端点与参数
        "/aweme/v1/web/aweme/post/", "sec_user_id", "max_cursor", "a_bogus",
        "window_months", "min_engagement", "stop_after_consecutive",
        "MEDIAMON_VISION_ENDPOINT",
        # 账号健康三态与轮换
        "healthy", "degraded", "expired", "banned",
        # 数值水位
        "≤2", "16MiB", "≥3 页", "≥90%", "6h", "2h", "24h",
        "diff<400",
        # 评论作者 12 字段
        "uid", "sec_uid", "short_id", "nickname", "avatar_url",
        "signature", "ip_label", "gender", "follower_count",
        "following_count", "aweme_count", "total_favorited",
        # 机制短语
        "fail-closed", "skipped≠success", "tmp+rename", "sha256",
        "type:drift", "ghcb", "/metrics", "arch-check", "gitleaks",
        "zizmor", "time-to-detect", "time-to-repair",
    ]

    def test_semantic_anchors(self):
        s = read(SPEC)
        missing = [a for a in self.ANCHORS if a not in s]
        self.assertEqual(missing, [],
                         f"缺语义锚 {missing}（真实原子采集平台 spec 必含）")

    def test_clause_anchor_binding(self):
        """条款级锚绑定：关键 AC 与其深水位机制短语共同出现。"""
        s = read(SPEC)
        pairs = [
            ("AC-2", "翻页深度 ≥3 页"),
            ("AC-4", "stats 缺失条目不参与连续计数"),
            ("AC-5", "入参与返回对称"),
            ("AC-7", "tmp+rename"),
            ("AC-9", "连续 3 次失败"),
            ("AC-15", "落盘前剥离"),
            ("AC-17", "1 个 canary 周期 + 1 个工作日"),
            ("AC-19", "连续两轮"),
            ("BEH-1", "has_more=false"),
            ("BEH-3", "既不清零也不累加"),
            ("IFACE-1", "cursor_param=max_cursor"),
            ("IFACE-3", "artifacts/<platform>/<item_id>.mp4"),
        ]
        for ac_id, anchor in pairs:
            # 锚必须出现在对应 AC/条款附近（±6 行窗口内共同出现）
            lines = s.splitlines()
            idx = [n for n, l in enumerate(lines) if ac_id in l]
            self.assertTrue(idx, f"{ac_id} 不存在")
            window = "\n".join(lines[max(0, idx[0] - 2): idx[0] + 7])
            self.assertIn(anchor, window, f"{ac_id} 附近缺锚「{anchor}」")


class L3NegativeAnchors(unittest.TestCase):
    """负向锚：偷懒改写最易产生的弱化标志不得出现。"""

    WEAKENING = ["尽力", "适当", "如可能", "必要时可", "暂不", "后续可",
                 "应尽量", "在条件允许时", "尽可能", "酌情"]

    def test_no_weakening_words(self):
        s = read(SPEC)
        hits = [w for w in self.WEAKENING if w in s]
        self.assertEqual(hits, [], f"出现义务弱化词 {hits}（S2 义务降级攻击面）")

    def test_no_escape_hatch(self):
        s = read(SPEC)
        for esc in ("可跳过", "可豁免", "可忽略", "特殊情况下不受"):
            self.assertNotIn(esc, s, f"出现逃生舱短语「{esc}」（S5 攻击面）")

    def test_no_future_tense_dodge(self):
        s = read(SPEC)
        # spec 是当下交付承诺，禁"未来将/后续将/计划将"时态后移（S4）
        for pat in ("未来将", "后续将", "计划将", "将择机"):
            self.assertNotIn(pat, s, f"出现时态后移短语「{pat}」")

    def test_obligation_strength(self):
        """义务强度：BEH 条款必须用「必须」承载义务（EARS 句型），禁降级为「可以/建议」。"""
        s = read(SPEC)
        beh_section = re.search(r"## BEH 行为\n(.*?)\n## ", s, re.S)
        self.assertIsNotNone(beh_section, "缺 BEH 段")
        behs = re.findall(r"- BEH-\d+（[^）]*）(.+)", beh_section.group(1))
        self.assertGreaterEqual(len(behs), 16, f"BEH 条款不足 16 条（实际 {len(behs)}）")
        weak = [b[:20] for b in behs if "必须" not in b]
        self.assertEqual(weak, [], f"BEH 条款缺「必须」义务动词: {weak}")

    def test_stats_completeness_no_shrink(self):
        """AC-19 的 12 字段清单不得缩水（数字与字段名双锚）。"""
        s = read(SPEC)
        m = re.search(r"AC-19.*?then: (.+)", s, re.S)
        self.assertIsNotNone(m, "缺 AC-19")
        then_text = m.group(1)[:600]
        self.assertIn("12 字段", then_text, "AC-19 丢失「12 字段」计数锚")
        self.assertIn("≥90%", then_text, "AC-19 丢失「≥90%」完备率锚")


class L4Consistency(unittest.TestCase):
    def test_ac_count_matches_ir(self):
        """AC 总数与 IR-MM-0001 期望（21 条）一致。"""
        s = read(SPEC)
        self.assertEqual(len(re.findall(r"^\s*- id: AC-\d+", s, re.M)), 21)

    def test_ac_numbering_contiguous(self):
        s = read(SPEC)
        ids = sorted(int(m) for m in re.findall(r"- id: AC-(\d+)", s))
        self.assertEqual(ids, list(range(1, 22)), f"AC 编号不连续: {ids}")

    def test_taskid_consistency(self):
        s = read(SPEC)
        self.assertEqual(len(re.findall(r"IR-MM-0001", s)) >= 2, True,
                         "taskId 与正文引用不一致")

    def test_no_template_reuse_from_other_specs(self):
        """防模板句复用：不得出现 VR 仓 spec 特有措辞（跨 IR 抄袭 = 摆拍）。"""
        s = read(SPEC)
        for vr_phrase in ("爆款对标", "拉片", "意图标签集", "拍摄 SOP",
                          "脚本草稿", "对标组"):
            self.assertNotIn(vr_phrase, s,
                             f"出现 VR 仓 spec 特有措辞「{vr_phrase}」（模板复用攻击面）")

    def test_cross_refs_resolvable(self):
        """跨引用可解析：spec 正文提及的外部锚（ENV-REQ 准备面、跨仓 IR 引用）须成对出现。"""
        s = read(SPEC)
        # ENV-REQ-1/2/3 准备面引用存在（ASSUMPTION-1 依赖它）
        self.assertIn("ENV-REQ-1", s, "缺 ENV-REQ-1 准备面引用")
        # 跨仓引用形态：DECISION-5 引用 VR 仓 IR-0001（承接口径）
        self.assertIn("IR-0001", s, "缺跨仓 IR-0001 引用（DECISION-5 承接口径）")


if __name__ == "__main__":
    unittest.main(verbosity=2)
