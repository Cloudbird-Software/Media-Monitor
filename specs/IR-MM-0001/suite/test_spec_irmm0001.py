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

class ACDecidability(unittest.TestCase):
    """S1' 摆拍式 AC 防御（红队 run 32972226384 钻洞归因补强）：
    AC 的 then 必须含可机检谓词锚——否则任何实现都能被宣称满足。"""

    # then 段合法的谓词锚类别（工具名/数值/枚举/错误码/路径/协议形态）
    THEN_ANCHOR_POOL = [
        "internal/license", "wiring", "hygiene", "zizmor", "gitleaks",
        "/aweme/v1/web/aweme/post/", "sec_user_id", "max_cursor", "a_bogus",
        "stats", "digg", "comment", "share", "collect", "play", "create_time",
        "≥3 页", "canary", "fixture",
        "window_months", "min_engagement", "stop_after_consecutive", "默认 5",
        "连续", "stats 缺失", "逐字节一致",
        "model.Cursor", "JSON", "limit",
        "platform", "sec_uid", "cursor", "account_id", "IF-1",
        "artifacts/", "path", "bytes", "sha256", "16MiB", "tmp+rename",
        "healthy", "degraded", "expired", "accounts_list", "HTTP 200",
        "≤2", "3 次", "banned", "/metrics", "auto",
        "type:drift", "幂等",
        "submodule", "arch-check", "internal/", "upstream/",
        "diff", "hunk", "tracked_paths",
        "成功率/新鲜度/许可", "C1 PR",
        "UI-TARS", "MEDIAMON_VISION_ENDPOINT", "tap", "swipe", "screencap",
        "uidump", "fail-closed", "adb",
        "HAR", "cookie/token", "JSON patch", "issue",
        "skipped≠success", "drift JSON", "脱敏",
        "ghcb", "1 个 canary 周期 + 1 个工作日", "time-to-detect",
        "time-to-repair", "dashboard", "/metrics", "SLA",
        "TESTING.md", "干净成功", "文档化跳过", "错误码", "12 字段", "≥90%",
        "uid", "short_id", "nickname", "avatar_url", "signature", "ip_label",
        "gender", "follower_count", "following_count", "aweme_count",
        "total_favorited", "24h",
        "MCP", "VR", "证据",
        "verdict", "survived", "AC-1 至 AC-20",
    ]

    def _ac_then_blocks(self, text):
        """抽取每条 AC 的 then 段文本（frontmatter 内 AC 值均单行）。"""
        blocks = re.findall(
            r"- id: AC-(\d+)\n\s+given: [^\n]+\n\s+when: [^\n]+\n\s+then: ([^\n]+)", text)
        return [(int(n), t.strip()) for n, t in blocks]

    def test_every_ac_then_has_predicate_anchor(self):
        """每条 AC 的 then 至少含 1 个谓词锚——无锚 then = 摆拍面。"""
        s = read(SPEC)
        blocks = self._ac_then_blocks(s)
        self.assertEqual(len(blocks), 21, f"AC then 块应为 21（实际 {len(blocks)}）")
        hollow = [n for n, t in blocks
                  if not any(a in t for a in self.THEN_ANCHOR_POOL)]
        self.assertEqual(hollow, [],
                         f"AC-{hollow} 的 then 无任何谓词锚（摆拍式 AC——任何实现都能被宣称满足）")

    def test_then_not_pure_restatement(self):
        """then 不得是 when 的重述（空洞回环）。"""
        s = read(SPEC)
        m = re.findall(r"- id: AC-(\d+)\n\s+given: (.+)\n\s+when: (.+)\n\s+then: (.+)", s)
        for n, g, w, t in m:
            # 归一化比较：then 与 when 完全同文 = 空洞
            self.assertNotEqual(t.strip(), w.strip(),
                                f"AC-{n} then 与 when 同文（空洞回环）")


class NegativeControl(unittest.TestCase):
    """负控制（红队 run 32972226384 补强）：程序化构造偷懒 spec 变体，
    断言套件锚逻辑必然拒绝——证明锚"杀得死"摆拍，而非仅"摆在那里"。
    每个变体对应一类已知攻击（S1' 摆拍/S2 义务降级/S4 数值漂移/S5 字段缩水/空洞化）。"""

    def _anchors_hit(self, text, anchors):
        return [a for a in anchors if a not in text]

    def _ac_then_of(self, text, ac_id):
        m = re.search(rf"- id: {ac_id}\n\s+given: [^\n]+\n\s+when: [^\n]+\n\s+then: ([^\n]+)", text)
        return m.group(1) if m else ""

    def test_variant_hollow_then_caught(self):
        """T1 摆拍变体：把 then 空洞化（删机制短语）→ 谓词锚断言必红。"""
        s = read(SPEC)
        variant = re.sub(r"(- id: AC-7\n\s+given: [^\n]+\n\s+when: [^\n]+\n\s+then: )[^\n]+",
                         r"\1工具正确处理下载请求并返回结果", s)
        dec = ACDecidability()
        blocks = dec._ac_then_blocks(variant)
        hollow = [n for n, t in blocks
                  if not any(a in t for a in ACDecidability.THEN_ANCHOR_POOL)]
        self.assertIn(7, hollow, "空洞化 then 未被谓词锚断言抓到（套件杀不死摆拍）")

    def test_variant_obligation_weakening_caught(self):
        """T2 义务降级变体：BEH 的「必须」→「可以」→ 义务强度断言必红。"""
        s = read(SPEC)
        variant = s.replace("系统必须", "系统可以", 5)
        beh_section = re.search(r"## BEH 行为\n(.*?)\n## ", variant, re.S)
        behs = re.findall(r"- BEH-\d+（[^）]*）(.+)", beh_section.group(1))
        weak = [b[:20] for b in behs if "必须" not in b]
        self.assertTrue(weak, "义务降级变体未被「必须」断言抓到")

    def test_variant_numeric_drift_caught(self):
        """T4 数值漂移变体：≤2→≤8、16MiB→64KiB → 语义锚断言必红。"""
        s = read(SPEC)
        variant = s.replace("≤2", "≤8").replace("16MiB", "64KiB")
        # 变体上锚丢失（套件锚逻辑对变体必红）
        missing_on_variant = self._anchors_hit(variant, ["≤2", "16MiB"])
        self.assertTrue(missing_on_variant,
                        "数值漂移变体未丢锚（变形器失效）")
        # 正控：原 spec 锚在位（套件接受原文）
        self.assertEqual(self._anchors_hit(s, ["≤2", "16MiB"]), [])

    def test_variant_field_shrink_caught(self):
        """T5 字段缩水变体：12 字段清单删 3 个 → AC-19 防缩水断言必红。"""
        s = read(SPEC)
        variant = s.replace(
            "uid, sec_uid, short_id, nickname, avatar_url, signature, ip_label, "
            "gender, follower_count, following_count, aweme_count, total_favorited",
            "uid, sec_uid, nickname, avatar_url, gender, follower_count, "
            "following_count, aweme_count, total_favorited")
        self.assertNotIn("short_id", variant, "变形器未删字段（负控制失效）")
        # 12 字段计数锚对变体必红：变体 then 段字段名 < 12
        then_text = self._ac_then_of(variant, "AC-19")
        field_names = ["uid", "sec_uid", "short_id", "nickname", "avatar_url",
                       "signature", "ip_label", "gender", "follower_count",
                       "following_count", "aweme_count", "total_favorited"]
        hits = [f for f in field_names if f in then_text]
        self.assertLess(len(hits), 12, "字段缩水变体仍通过 12 字段断言（套件杀不死缩水）")

    def test_variant_gwt_hollow_caught(self):
        """T3 空洞 then（重述 when）→ 非重述断言必红。"""
        s = read(SPEC)
        variant = re.sub(r"(- id: AC-3\n\s+given: [^\n]+\n\s+when: )([^\n]+)(\n\s+then: )[^\n]+",
                         r"\1\2\3\2", s)
        m = re.search(r"- id: AC-3\n\s+given: [^\n]+\n\s+when: ([^\n]+)\n\s+then: ([^\n]+)", variant)
        self.assertEqual(m.group(1).strip(), m.group(2).strip(),
                         "变形器未构造重述（负控制失效）")

class StrictClauseAnchors(unittest.TestCase):
    """S1' 深防御（红队 32972226384/32975505230 两轮同因归因补强）：
    21 条 AC 全覆盖的强承诺矩阵——每条 then 必含其**全部**强承诺短语（AND 语义，
    非词池 OR）。任何摆拍改写必须保留全部短语 = 保留承诺本身。"""

    # AC → then 段必含强承诺短语（取自 spec 原文核心承诺：动词短语+数值+专名）
    STRICT_ANCHORS = {
        1: ["internal/license 包删除", "hygiene/zizmor/gitleaks 关卡零降级"],
        2: ["offline canary 全绿", "create_time", "翻页深度 ≥3 页进入 canary 断言"],
        3: ["同 AC-2 规格全绿"],
        4: ["连续 stop_after_consecutive（默认 5）条低于阈值早停",
            "stats 缺失条目不参与连续计数", "逐字节一致"],
        5: ["入参与返回对称", "limit 不再构成翻页天花板"],
        6: ["platform/sec_uid/window_months/min_engagement/stop_after_consecutive/limit/cursor/account_id",
            "零样本调用（IF-1 口径）"],
        7: ["artifacts/<platform>/<item_id>.mp4", "{path, bytes, sha256}",
            "tmp+rename"],
        8: ["{healthy, degraded, expired}", "accounts_list 工具可见"],
        9: ["重试 ≤2（同 cursor 续采）", "连续 3 次失败标记 banned",
            "轮换与封禁事件进 /metrics"],
        10: ["自动开 type:drift issue 且幂等去重"],
        11: ["internal/ 永不 import upstream/",
             "arch-check 守卫 + 违规样例测试在位"],
        12: ["文件级 + 关键 hunk", "tracked_paths"],
        13: ["成功率/新鲜度/许可", "C1 PR 附评分"],
        14: ["fail-closed 且不产出半成品数据"],
        15: ["JSON patch + issue 草稿", "落盘前剥离", "全程无人工复制"],
        16: ["skipped≠success 语义保持", "drift JSON + HAR 摘要 + 脱敏账号 id"],
        17: ["1 个 canary 周期 + 1 个工作日",
             "time-to-detect 与 time-to-repair 进 /metrics"],
        18: ["契约健康时间线", "time-to-detect/time-to-repair",
             "面板数据与 /metrics 交叉一致"],
        19: ["全部通过且极端行结局", "12 字段", "≥90% 完备"],
        20: ["真实 MCP 调用完成", "证据落 VR 仓侧"],
        21: ["verdict 为 survived 的审计记录",
             "逐条派生自本 spec 的 AC-1 至 AC-20"],
    }

    def _then_of(self, text, ac_id):
        m = re.search(
            rf"- id: AC-{ac_id}\n\s+given: [^\n]+\n\s+when: [^\n]+\n\s+then: ([^\n]+)", text)
        return m.group(1) if m else ""

    def test_strict_anchor_matrix(self):
        """每条 AC 的 then 必含其全部强承诺短语（AND 语义）。"""
        s = read(SPEC)
        for ac_id, phrases in self.STRICT_ANCHORS.items():
            then_text = self._then_of(s, ac_id)
            self.assertTrue(then_text, f"AC-{ac_id} then 段缺失")
            missing = [p for p in phrases if p not in then_text]
            self.assertEqual(missing, [],
                             f"AC-{ac_id} then 丢强承诺 {missing}（摆拍面：改写掏空语义）")

    def test_strict_anchors_kill_rewrites(self):
        """负控制：同义改写变体（保留语义骨架但抽走一个强短语）必被矩阵拒绝。"""
        s = read(SPEC)
        # 构造变体：AC-4 抽走「逐字节一致」承诺（换成"行为一致"）
        variant = s.replace("零参数时行为与既有契约逐字节一致（既有测试不改一字全绿）",
                           "零参数时行为与既有契约保持一致（既有测试全绿）")
        then_text = self._then_of(variant, 4)
        self.assertNotIn("逐字节一致", then_text, "变形器未抽走承诺（负控制失效）")
        missing = [p for p in self.STRICT_ANCHORS[4] if p not in then_text]
        self.assertTrue(missing, "弱化变体未被矩阵拒绝（套件杀不死改写）")

    def test_ac_ir_lineage(self):
        """AC 与父意图 IR #16 的血缘：spec 21 条 AC 与 IR 期望变化逐条对应（数量锚）。"""
        s = read(SPEC)
        fm = frontmatter(s)
        self.assertIn("card: \"Cloudbird-Software/Media-Monitor#16\"", fm,
                      "frontmatter card 绑定必须是 IR #16（血缘防偷换）")


if __name__ == "__main__":
    unittest.main(verbosity=2)




