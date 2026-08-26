---
taskId: IR-MM-0001
specVersion: 1
title: 原子采集 MCP 化·账号历史回溯·上游双轨适配·真机自优化实验室
irRef: IR-MM-0001
card: "Cloudbird-Software/Media-Monitor#16"
acceptanceCriteria:
  - id: AC-1
    given: 仓库存在 internal/license 门禁且全部采集与动作类 MCP 工具被 license fail-closed 包裹
    when: license 拆除合并后在无任何 license 配置的环境下调用全部采集与动作类 MCP 工具
    then: internal/license 包删除、三个 cmd 的 wiring/env/docs 同步清除，全部工具可调用且 hygiene/zizmor/gitleaks 关卡零降级
  - id: AC-2
    given: 抖音 user-posts 契约以 /aweme/v1/web/aweme/post/ 端点注册且 query 含 sec_user_id 与 max_cursor 翻页参数、a_bogus 经 signsvc 签名
    when: offline canary 以三页合成 fixture 驱动该契约
    then: 契约+fixture+offline canary 全绿，每条 item 绑定 stats{digg,comment,share,collect}（play 若返回则一并绑定）与 create_time，翻页深度 ≥3 页进入 canary 断言
  - id: AC-3
    given: 小红书 user-notes 契约按抖音同规格注册
    when: offline canary 以多页合成 fixture 驱动该契约
    then: stats 绑定与翻页深度断言同 AC-2 规格全绿
  - id: AC-4
    given: engine fetchPages 已支持可选终止谓词
    when: 采集方传入 window_months 时间窗或 min_engagement{metric,threshold} 连续早停参数
    then: 终止条件为时间窗截止或连续 stop_after_consecutive（默认 5）条低于阈值早停且 stats 缺失条目不参与连续计数，零参数时行为与既有契约逐字节一致（既有测试不改一字全绿）
  - id: AC-5
    given: MCP 工具面存在 search_items/get_comments/get_replies/get_user_posts 工具
    when: 调用方在入参携带 cursor 且工具返回结果
    then: cursor 以 model.Cursor 的 JSON 形态接受并返回（入参与返回对称），limit 不再构成翻页天花板
  - id: AC-6
    given: get_user_posts MCP 工具已注册
    when: agent 零样本调用该工具
    then: 暴露 platform/sec_uid/window_months/min_engagement/stop_after_consecutive/limit/cursor/account_id 全参数且描述字段满足 agent 零样本调用（IF-1 口径）
  - id: AC-7
    given: 下载目标视频已可经 resolve 定位
    when: 调用 download_video MCP 工具
    then: 流式落盘 artifacts/<platform>/<item_id>.mp4 并返回 {path, bytes, sha256}，字节不经 MCP 行通道（16MiB 上限绕开），落盘为原子写（tmp+rename）
  - id: AC-8
    given: 账号池持有常态化 cookie 的各平台账号
    when: 周期性健康探测执行（per-platform 最廉价契约探测 + 翻页深度异常检测，HTTP 200 + 空 body 判 expired）
    then: health ∈ {healthy, degraded, expired} 持久化进 accounts 模型并经 accounts_list 工具可见
  - id: AC-9
    given: 引擎以 pool auto 模式选号执行采集
    when: 选中账号失效或采集失败
    then: 按 health 自动选号、失效换号有界重试 ≤2（同 cursor 续采）、连续 3 次失败标记 banned，轮换与封禁事件进 /metrics
  - id: AC-10
    given: 账号池内各平台的可用账号数随 cookie 失效持续下降
    when: 可用数低于水位阈值
    then: 自动开 type:drift issue 且幂等去重
  - id: AC-11
    given: upstream/vendor/ 以 submodule pin 落 f2/wx_channels_download/MediaCrawler/UI-TARS
    when: registry 六条目 pin 全部落实且 arch-check 执行
    then: internal/ 永不 import upstream/（arch-check 守卫 + 违规样例测试在位）
  - id: AC-12
    given: 上游 submodule 有新 commit 且 watcher 触发报警
    when: 报警 issue 生成
    then: 附 tracked_paths 的上游 commit diff 摘要（文件级 + 关键 hunk），agent 可直接据此修契约
  - id: AC-13
    given: swap-test bench 以 mediactl 子命令对指定上游执行
    when: 同套 canary 对上游运行
    then: 输出评分（成功率/新鲜度/许可），采纳走 C1 PR 附评分
  - id: AC-14
    given: vision provider 接 UI-TARS OpenAI 兼容端点（MEDIAMON_VISION_ENDPOINT）
    when: 经 adb 语义动作（tap/swipe/text/screencap/uidump）驱动真机完成采集路径
    then: 端点未配置时 fail-closed 且不产出半成品数据
  - id: AC-15
    given: 设备代理形态录得 HAR
    when: netcapture→fixture 转换链执行
    then: 候选 fixture 经 adapt diff 产出契约补丁提案（JSON patch + issue 草稿）且 HAR 内 cookie/token 落盘前剥离，全程无人工复制
  - id: AC-16
    given: lab secrets 就绪
    when: canary live 模式执行
    then: 真实执行契约探测（skipped≠success 语义保持），失败自动开 type:drift issue 附 drift JSON + HAR 摘要 + 脱敏账号 id
  - id: AC-17
    given: 检出契约漂移后进入自优化闭环
    when: 月度 drill 以人为种子契约破坏执行
    then: 检出→issue→ghcb 认领→修复 PR→lab 复跑绿在 1 个 canary 周期 + 1 个工作日内完成，time-to-detect 与 time-to-repair 进 /metrics
  - id: AC-18
    given: mediad dashboard 页面运行
    when: 观测契约健康与账号运营状态
    then: 呈现契约健康时间线、账号健康与轮换事件、time-to-detect/time-to-repair 两项 SLA 指标，且面板数据与 /metrics 交叉一致
  - id: AC-19
    given: 真机矩阵验收按 docs/TESTING.md A（搜索）/B（评论+字段）/E（ADB/vision）组执行
    when: golden 用例连续两轮运行（间隔 24h）
    then: 全部通过且极端行结局 ∈ {干净成功, 文档化跳过, fail-closed 错误码}，评论作者 12 字段（uid, sec_uid, short_id, nickname, avatar_url, signature, ip_label, gender, follower_count, following_count, aweme_count, total_favorited）≥90% 完备
  - id: AC-20
    given: VR 仓 MCP transport 适配器以真实 MCP 调用消费本仓工具面
    when: 执行 user_posts（含 window+阈值回溯）→ comments（cursor 链翻页）→ download 竖切
    then: 三段竖切全部经真实 MCP 调用完成且证据落 VR 仓侧
  - id: AC-21
    given: 本 spec 与套件已合并
    when: 红队审计运行并产出判定
    then: 存在 verdict 为 survived 的审计记录，且后续工作卡的验收标准逐条派生自本 spec 的 AC-1 至 AC-20
nonGoals:
  - 不建编排引擎——原子经 MCP 暴露后由消费侧 agent 自由组合
  - shipinhao 不进契约体系（无稳定端点）：走 netcapture+vision 专属通道，单独立项
  - 不做定时全量巡检类产品化（lab 是验证环，不是监控产品）
  - 不重建 license（未来 HARDENING 交付管线再议）
  - 不 vendored MediaCrawler 代码（非商用 license；参数与绑定知识可学）
  - 不动 .github/workflows/** 与 Makefile 的 check 目标（App 无此权限，人类专属）
---

# IR-MM-0001 spec：原子采集 MCP 化·账号历史回溯·上游双轨适配·真机自优化实验室

> 父意图：[Cloudbird-Software/Media-Monitor#16](https://github.com/Cloudbird-Software/Media-Monitor/issues/16)（已签署）。
> 本仓 AGENTS.md 原约定不建 specs/ 目录——owner 2026-08-26 会话升级决策：spec 以 PR 形态落地并接受组织 adversary 红队审计（本 PR 即该决策的执行）。

## INV 不变量

- INV-1 fail-closed 全程保持：cookie.required / signature.required / 绑定缺失即错；任何轨道（含 vision 重捕获产物）不得削弱该语义
- INV-2 无静默错数据：所有采集路径的失败必须以显式错误码暴露，永不 hang，永不产出半成品数据（TESTING.md 判据 5）
- INV-3 internal/ 不得 import upstream/（submodule 是可 diff 的观测副本，不是依赖）
- INV-4 证据可回查：lab 一切判定挂 GitHub 侧运行时证据（drift JSON / HAR / issue 链接），沙箱自报数字不采信（承 VR 仓 IR-0001 DECISION-2 口径）
- INV-5 MCP 工具 schema 即原子契约：能力只经工具面暴露，描述字段对 agent 零样本可用
- INV-6 凭据永不入库：cookies/devices 只存在于 secrets 与 lab 环境（gitleaks 全历史扫描）
- INV-7 一卡一 PR、diff<400 行；新依赖先报批（名称/用途/许可证/标准库可否替代），禁 AGPL/GPL-3.0/SSPL

## BEH 行为

- BEH-1（回溯原子）当给定 platform 与 sec_uid 发起回溯时，系统必须从最新向最旧翻页（max_cursor 链）；终止条件任一先满足：(a) has_more=false（作品耗尽）(b) 条目 create_time 早于 window 截止（window_months 默认 6）(c) 连续 stop_after_consecutive（默认 5）条低于 min_engagement (d) limit 达成
- BEH-2（回溯语义）当回溯执行时，系统必须只做列举：返回条目携带完整 stats + create_time + media_type + author 摘要 + next_cursor；具体视频下载与评论采集由消费侧另调 download/comments 原子
- BEH-3（阈值语义）当 min_engagement 传入时，metric 必须取值 ∈ {digg, comment, share, collect, play}；stats 缺失条目不参与连续计数（既不清零也不累加）；时间窗与 limit 兜底；评测时点=采集时刻
- BEH-4（游标续采）当回溯中断后经 next_cursor 续采时，系统必须不重头且 cursor JSON 形态版本化
- BEH-5（账号 401/风控墙）当账号遭遇 401 或风控墙时，系统必须判 expired 并触发轮换
- BEH-6（账号空页）当账号出现 HTTP 200 + 空页时，系统必须经翻页深度检测判 expired 并触发轮换
- BEH-7（账号部分成功）当账号部分请求成功时，系统必须判 degraded 并触发告警
- BEH-8（上游报警）当上游 submodule 有新 commit 时，watcher 必须报警并附 diff 摘要
- BEH-9（swap-test）当对指定上游跑同套 canary 时，系统必须输出评分（成功率/新鲜度/许可）
- BEH-10（采纳决策）当 swap-test 评分产出后，采纳或忽略决策必须走 C1 PR 并留记录
- BEH-11（vision 驱动）当契约红时，系统必须经 vision 驱动真机走采集路径
- BEH-12（重捕获）当 vision 路径执行时，系统必须经 netcapture 录 HAR 产出候选 fixture
- BEH-13（补丁提案）当候选 fixture 产出后，系统必须经 adapt diff 生成契约补丁提案（提案非变更，审后走 PR）
- BEH-14（lab canary）当 lab 运行时，系统必须每 6h 执行 live canary 且每 2h 执行账号探测
- BEH-15（闭环修复）当 drift issue 开出后，修复必须经 ghcb 认领→修复 PR→复跑绿评论回填
- BEH-16（月度 drill）当月度 drill 执行时，系统必须以人为种子契约破坏验收闭环 SLA

## IFACE 契约

- IFACE-1 user_posts 族契约：paging.cursor_param=max_cursor；回溯参数（window/阈值/N）在 collect 选项层，不进契约 schema——契约声明端点，谓词是采集时行为
- IFACE-2 MCP cursor 形态：model.Cursor 的 JSON 序列化规范（版本化，入参与返回对称）
- IFACE-3 download_video artifact 布局：artifacts/<platform>/<item_id>.mp4 + 返回 {path, bytes, sha256}
- IFACE-4 账号 health 字段进 accounts 模型与 accounts_list 工具；rotation/banned/水位指标进 /metrics
- IFACE-5 vision Provider 接口（OpenAI 兼容端点）与 HAR→fixture 转换器签名（脱敏前置）

## BUDGET 预算

- 一卡一 PR、diff<400 行（组织硬规则逐卡拆）
- 轮换重试上界：单请求换号重试 ≤2；连续失败 ban 阈值=3
- lab 频率：live canary 每 6h；账号探测每 2h；drill 每月 1 次
- drill 自愈时限：1 个 canary 周期 + 1 个工作日

## DECISION 决策

- DECISION-1 license 全拆（owner 2026-08-26 明示）；未来经 HARDENING 交付管线以打包形态重建
- DECISION-2 借鉴边界：f2（Apache-2.0）/ wx_channels_download（MIT）/ UI-TARS（Apache-2.0）可参照可搬；MediaCrawler（非商用 license）学参数表与绑定知识，不搬代码
- DECISION-3 上游双轨：GitHub API 轮询预警（已有 workflow）+ submodule 本地 diff（swap-test 素材）并行
- DECISION-4 vision 选型 UI-TARS（registry 已登记 Apache-2.0；ENV-REQ-3 既定端点方案）
- DECISION-5 shipinhao 降级为 netcapture+vision 专属通道（与 VR 仓 IR-0001 DECISION-1「wx_channel 参考不依赖」一致）
- DECISION-6 回溯阈值语义：连续 N 条低于阈值早停（平台无服务端过滤；创作者历史互动量非单调，单条阈值会误截断）
- DECISION-7 spec 以 PR 形态落地并接受 adversary 审计（owner 2026-08-26 升级决策，取代 IR #16 引言「正文承载 spec 条款」的 D-7 过渡安排；IR #16 正文其余条款不变）
- DECISION-8 编排不建引擎（owner 2026-08-26：原子暴露后由 agent 自行编排）

## ASSUMPTION 假设

- ASSUMPTION-1 owner 提供：各平台登录态账号（≥5/平台，分桶见 docs/TESTING.md §2）、Android 真机（≥2 台）、住宅代理池、signsvc 部署、UI-TARS 端点（ENV-REQ-1/2/3 已登记准备面）
- ASSUMPTION-2 自托管 runner（挂设备与账号 secrets）可接入本仓 workflow
- ASSUMPTION-3 平台公开接口在实现窗口内可经频控+账号池访问；平台反爬升级属运维事件，不构成 spec 违约（承 VR IR-0001 ASSUMPTION-1）
- ASSUMPTION-4 user_posts 端点参数表以 f2 #435 日志与 MediaCrawler creator 模式为初始依据，落契约时以真实抓包校准
