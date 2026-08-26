# IR-MM-0001 波次计划（spec PR 落地，adversary 审计待跑；本文件为开卡依据）

> 波次边界按"何时能接触现实"切（组织 PLAYBOOK §3 先例），不按工作量切。
> 每波次结束产出一个人类 5 分钟内可验收的东西。
> 平台顺序按 spec DECISION：抖音先行（W2-C1）、小红书次之（W2-C2）；
> vision/shipinhao 专属通道后置（W6）；真机实验室收口（W7-W8）。
> 每卡纪律（BUDGET-1）：一个 PR 一件事、diff < 400 行、测试先于实现（fail-before）。

## W0 治理前置 —— "ADR 族落档 + license 门禁拆除"

| 卡    | issue | 内容                                                                                         | 关键 AC           | 测试要求                          |
| ----- | ----- | -------------------------------------------------------------------------------------------- | ----------------- | --------------------------------- |
| W0-C1 | #17   | ADR 族落 archive 仓（license 拆除 / 上游实体化 / vision 解冻，编号取 archive INDEX.yaml 空闲号） | DECISION-1/2/4    | 文档卡：ADR 落档 + INDEX.yaml 登记 |
| W1-C1 | #18   | license 门禁全量拆除：internal/license 删除、三 cmd wiring/env/docs 清除                        | AC-1 / INV-1      | fail-before：拆除后零 license 配置下全部工具可调用 |

**W0 人类验收**：`make check` 全绿 + 合成调用一个采集类工具无 license 拦截。

## W2 账号历史回溯基础 —— "契约 ×2 + 早停谓词"

| 卡    | issue | 内容                                                                                                    | 关键 AC                     | 测试要求                                                |
| ----- | ----- | ------------------------------------------------------------------------------------------------------- | --------------------------- | ------------------------------------------------------- |
| W2-C1 | #19   | 抖音 user-posts 契约：/aweme/v1/web/aweme/post/ + sec_user_id + max_cursor + a_bogus 签名 + 三页 fixture | AC-2 / IFACE-1 / BEH-1      | fail-before：契约解析测试先红 + canary stats 完备性先红 |
| W2-C2 | #20   | 小红书 user-notes 契约同规格                                                                             | AC-3 / IFACE-1              | fail-before：同 W2-C1 口径                               |
| W2-C3 | #21   | 回溯早停谓词：engine fetchPages 终止条件扩展（window/阈值/连续 N 条）                                    | AC-4 / BEH-1..4 / DECISION-6 | fail-before：零参数逐字节等价断言 + 早停边界用例         |

**W2 人类验收**：`make adapt-offline` 全绿，三页 fixture 翻页+终止条件正确。

## W3 MCP 工具面 —— "cursor 透传 ×2 工具注册"

| 卡    | issue | 内容                                                             | 关键 AC                | 测试要求                                    |
| ----- | ----- | ---------------------------------------------------------------- | ---------------------- | ------------------------------------------- |
| W3-C1 | #22   | MCP 工具分页 cursor 透传：search_items/get_comments/get_replies | AC-5 / IFACE-2         | fail-before：传 cursor 断言 engine 收到    |
| W3-C2 | #23   | get_user_posts MCP 工具注册（账号历史回溯原子面）                  | AC-6 / INV-5           | fail-before：全参数 schema + 零样本描述断言  |
| W3-C3 | #24   | download_video MCP 工具（artifact 落盘，绕 16MiB 行上限）          | AC-7 / IFACE-3         | fail-before：落盘原子写 + sha256 断言       |

**W3 人类验收**：MCP 客户端零样本完成一次「回溯→评论翻页→下载」竖切。

## W4 账号池韧性 —— "健康探测 → 自动轮换 → 水位告警"

| 卡    | issue | 内容                                                            | 关键 AC                    | 测试要求                                    |
| ----- | ----- | --------------------------------------------------------------- | -------------------------- | ------------------------------------------- |
| W4-C1 | #25   | 账号健康探测（cookie 常态化持有·其一）                           | AC-8 / BEH-5..7            | fail-before：三态判定 + HTTP 200 空页 expired |
| W4-C2 | #26   | 引擎账号自动轮换（cookie 常态化持有·其二）                       | AC-9 / IFACE-4             | fail-before：auto 选号 + 有界重试 + 同 cursor 续采 |
| W4-C3 | #27   | 账号池水位告警                                                    | AC-10 / IFACE-4            | fail-before：阈值触发 issue + 幂等去重       |

**W4 人类验收**：杀掉一个账号的 cookie，采集在 ≤2 次重试内自动换号完成。

## W5 上游双轨其一 —— "submodule 实体化 + diff 摘要 + swap-test"

| 卡    | issue | 内容                                                                     | 关键 AC               | 测试要求                                     |
| ----- | ----- | ------------------------------------------------------------------------ | --------------------- | -------------------------------------------- |
| W5-C1 | #28   | 上游 submodule 实体化 + registry pin 落实                                 | AC-11 / INV-3         | fail-before：arch-check 违规样例测试          |
| W5-C2 | #29   | watcher 报警 diff 摘要增强                                                | AC-12 / BEH-8         | fail-before：fixture 驱动 diff 摘要结构断言   |
| W5-C3 | #30   | swap-test bench 可执行化                                                  | AC-13 / BEH-9..10     | fail-before：评分输出 + 采纳记录断言          |

**W5 人类验收**：一次上游 commit 造改报警，issue 里直接读到可修契约的 diff 摘要。

## W6 上游双轨其二 —— "vision 解冻 + netcapture 转换链"

| 卡    | issue | 内容                                                              | 关键 AC                 | 测试要求                                    |
| ----- | ----- | ----------------------------------------------------------------- | ----------------------- | ------------------------------------------- |
| W6-C1 | #31   | vision provider 解冻接 UI-TARS（改版适配轨道 B）                    | AC-14 / DECISION-4      | fail-before：端点未配置 fail-closed 断言     |
| W6-C2 | #32   | netcapture→fixture 转换器（HAR → 契约补丁提案）                     | AC-15 / BEH-11..13      | fail-before：脱敏前置 + JSON patch 结构断言  |

**W6 人类验收**：录一段 HAR，跑出一份带脱敏的候选 fixture 与补丁提案草稿。

## W7 真机自优化实验室 —— "live canary → runner → drill/SLA → dashboard"

| 卡    | issue | 内容                                    | 关键 AC                  | 测试要求                                     |
| ----- | ----- | --------------------------------------- | ------------------------ | -------------------------------------------- |
| W7-C1 | #33   | live canary driver 落地                 | AC-16 / INV-2            | fail-before：skipped≠success + drift issue 断言 |
| W7-C2 | #34   | 真机实验室 runner（自优化闭环载体）     | AC-17 / BEH-14..15       | fail-before：调度与幂等测试先红                |
| W7-C3 | #35   | drill 机制 + 闭环 SLA 计量              | AC-17 / BEH-16           | fail-before：SLA 指标断言 + drill 演练冒烟     |
| W7-C4 | #36   | mediad dashboard 扩展（实验室观测面）   | AC-18 / IFACE-4          | fail-before：面板与 /metrics 交叉一致断言      |

**W7 人类验收**：种子一次契约破坏，实验室在一个 canary 周期内自愈且 SLA 可读。

## W8 收口 —— "真机矩阵验收 + VR 消费竖切"

| 卡    | issue | 内容                                            | 关键 AC                    | 测试要求                        |
| ----- | ----- | ----------------------------------------------- | -------------------------- | ------------------------------- |
| W8-C1 | #37   | 真机矩阵验收（TESTING.md A/B/E 组）              | AC-19 / INV-2              | golden 连续两轮 + 极端行结局断言 |
| W8-C2 | #38   | VR 消费竖切打通（跨仓协调）                       | AC-20                      | 联调收口：四段证据齐备           |

**W8 人类验收**：TESTING.md A/B/E 组 golden 连续两轮通过（间隔 24h），VR 仓竖切证据落档。

依赖序：W0→W1 可并行于 W2 契约线；W3-C2 依赖 W2-C1/C3 与 W3-C1；W4-C2 依赖 W4-C1；W6 依赖 ENV-REQ-3；W7 依赖 W1+W4+W6；W8 收口全部。
