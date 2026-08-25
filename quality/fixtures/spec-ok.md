---
taskId: SPEC-0201
specVersion: 1
title: 健康探针在进程存活且依赖可达时返回就绪
irRef: IR-0201
card: "Cloudbird-Software/.github#214"
acceptanceCriteria:
  - id: AC-1
    given: 进程已启动且依赖数据库可达
    when: GET /healthz 被调用
    then: 返回 200 且 body 含 status 等于 ready
  - id: AC-2
    given: 依赖数据库不可达
    when: GET /healthz 被调用
    then: 返回 503 且 body 含 status 等于 degraded，响应时间低于 2 秒
  - id: AC-3
    given: g010 以本文件为输入运行
    when: lint 完成
    then: exit 0 且 gate-report status 为 pass
nonGoals:
  - 不做指标聚合面板
blastRadius:
  - src/health/**
  - quality/fixtures/**
---

# SPEC-0201 服务健康探针（g010 判定物：合法样例）

## BEH 行为

- BEH-1: 当提交包含 quality/** 变更时，CI 必须运行 g010 并落盘 gate-report
- BEH-2: 在依赖不可达的情况下，探针必须在 2 秒内返回 degraded 而非挂起
- BEH-3: 当 CI 收到 PR 事件时，g010 用 TEST-101 对应的用例校验本条款引用闭合
- BEH-4: 当探针配置未覆盖新依赖时，允许合理降级为部分检查（预算 BUDGET-1）

## IFACE 契约

- IFACE-1: GET /healthz 的响应体必须是 JSON 且键名固定

## BUDGET 预算

- BUDGET-1: 探针实现不超过 120 行，降级行为必须在本预算内声明
