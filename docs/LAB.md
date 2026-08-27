# LAB — 自优化实验室运行手册（W7-C2/C3）

## 本地调度载体（本仓已落地）

`mediad` 内建实验室双频循环（`startLabLoop`，随守护进程启动）：

| 循环 | 环境变量 | 默认 | 行为 |
| --- | --- | --- | --- |
| canary 刷新 | `MEDIAMON_LAB_CANARY_INTERVAL` | 6h | 重跑 offline canary 快照并记录当日契约健康（dashboard 时间线数据源） |
| 账号探测 | `MEDIAMON_LAB_PROBE_INTERVAL` | 2h | 对池内每账号跑健康探测（W4-C1 `ProbeAndStore`），health 供自动轮换与水位告警消费 |

非法间隔值 → 循环禁用并显式日志（fail-closed）。xhs 探测目标经账号 tag
`probe_user_id:<id>` 提供（无 tag 时该账号跳过并计数）。

## GitHub 侧调度（owner 接线；App 无 workflow 权限）

组织规则：本仓 App（cloudbrid-agent）不得触碰 `.github/workflows/**`。真机
runner 的调度接线（IR AC-16/17 的 workflow 侧）由 owner 执行：

1. 自托管 runner 挂真机 adb + 账号 secrets（`MEDIAMON_CANARY_COOKIES_*`、
   `AGENT_APP_SECRET`、`MEDIAMON_SIGNER_URL`）。
2. 定时任务每 6h：`mediactl adapt canary --live`（失败自动开 type:drift
   issue，见 docs/CANARY.md）。
3. 定时任务每 2h：`mediactl accounts probe --id <id>`（全池轮询）。

## drift issue 认领与闭环（BEH-14..16）

drift issue 开出后（W7-C1 driver），修复 agent 按 front-desk 流程认领：

```
bash ghcb claim <issue#>     # arbiter CAS 租约，先到先得
# ...修复 → PR（Card: Cloudbird-Software/Media-Monitor#<卡号>）→ 合并
bash ghcb release <issue#>   # 或由 conductor 自动
```

修复合并后，下一个 canary 周期复跑绿 → 结果以评论回填对应 drift issue
（`lab canary cycle green — closing`），issue 自动关闭。复跑评论由 owner 的
定时任务附 `-comment-issue <n>` 参数触发（`mediactl adapt canary --live
--comment-issue <n>` 面）。

## SLA 计量（W7-C3 已落地工具面）

- **drill**：`mediactl lab drill [--contract <name>]` —— 沙箱副本内种子破坏
  （主 binding → `$.drilled_seed_break`）→ offline canary 必红 → 报告
  {`seed/seeded_at/detected_at/detect_seconds/green_again`} 落
  `adapt/reports/drill-*.json`；活树零触碰（rollback by construction）。
- time-to-detect：种子破坏时刻 → drift issue 开出时刻（drill 与真实事件
  分列计数器 `sla.time_to_detect.drill` / `.real`，dashboard 呈现）。
- time-to-repair：issue 开出 → 复跑绿评论时刻。
- 月度 drill 验收口径：1 个 canary 周期 + 1 个工作日内自愈（IR BUDGET）。
