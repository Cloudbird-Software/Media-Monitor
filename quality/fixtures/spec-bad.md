---
taskId: SPEC-0202
specVersion: 1
title: 坏样例 spec（故意违规，供 run-all.sh 断言）
irRef: IR-9999
acceptanceCriteria:
  - id: AC-1
    given: lint 输入为本文件
    when: g010 运行
    then: 返回 exit 1 并列出全部违规规则
  - id: AC-3
    given: 本条无卡引用且无测试覆盖
    when: 追溯闭合检查运行
    then: 被合理地判为孤儿条款（此处『合理』即禁词违规样例）
---

# SPEC-0202 坏样例（g010 判定物：每一种违规各一处，负控制）

## BEH 行为

- BEH-1: 当 lint 运行时，本条款引用 TEST-42（该测试未登记索引，断链）
- BEH-2: 本条款是一句散文，不含任何条件句式（EARS 不匹配样例）
- BEH-3: 当检查禁词时，本条款含『尽可能』并绑定 BUDGET-9（未定义，断链样例）

## BUDGET 预算

- BUDGET-1: 坏样例自身不设预算
