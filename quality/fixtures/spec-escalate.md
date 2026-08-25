---
taskId: SPEC-0203
specVersion: 1
title: 重复 AC 编号的篡改样例
irRef: IR-0203
card: "Cloudbird-Software/.github#214"
acceptanceCriteria:
  - id: AC-1
    given: lint 输入为本文件
    when: g010 运行
    then: 因 AC-1 重复定义返回 exit 2
  - id: AC-1
    given: 影子条款试图覆盖正本
    when: g010 运行
    then: 仍被判为篡改类违规（fail-escalate）
---

# SPEC-0203 篡改样例（g010 判定物：AC 重复定义 → exit 2）

## BEH 行为

- BEH-1: 当出现重复 AC 编号时，g010 必须以 exit 2 升级给人
