# AGENTS.md — Media-Monitor

Agents working in this repository must read this file first. It is machine-enforceable where stated.

<!-- entry-protocol v2 -->

### 入口协议（陌生 agent 从这里开始——宪法 §11 / ADR-0055/0095）

0. **按意图定角色**（指引=.github 仓 `docs/agent/ROLE-*.md`，ADR-0095）：开新意图→ROLE-IR · 把已签署 IR 写成 spec→ROLE-SPEC · 实现卡片→ROLE-IMPLEMENT · 验收/人类让你处理 issues→ROLE-ACCEPT
1. 取 ghcb（钉 SHA，禁浮动 main）：`curl -fsS -o ghcb https://raw.githubusercontent.com/Cloudbird-Software/.github/f72d9520706c8fca974d92456f65cae5c1412bb7/scripts/ghcb && chmod +x ghcb`（凭据用你自己的：`gh auth login` 或 `export GH_TOKEN=<PAT>`；`-f` 必带——404 时 curl 无 -f 仍退出 0，会把错误页当脚本落盘）
2. 找活：`bash ghcb next [owner/repo]` → 列 state:ready 卡（卡 issue 是唯一工作凭证，无卡不开工）
3. 认领：`bash ghcb claim <n> [owner/repo]` → 评论 /claim——conductor 转介 arbiter 原子 CAS 租约，先到先得；败者换下一张（`bash ghcb status <n>` 看持有者）
4. 开工：`make card-test CARD=<n>`（读卡 AC、测试先行）→ `make gates-pr`（本地复现 CI 关卡）
5. 提 PR：body 必带一行卡元数据 `Card: <owner>/<repo>#<n>`（`bash ghcb card-meta <n>` 生成；缺失=后续关卡 exit 3）
6. front-desk 命令（卡 issue 评论，conductor 转介 arbiter 处理）：/claim 认领 · /release 释放租约 · /retry 隔离回流

<!-- /entry-protocol -->

## 角色路由（按你的意图选路——ADR-0095；指引文件在 .github 治理仓 docs/agent/）

- 开 IR：feature 意图=本仓 issue（issue 即 IR，无需 PR）；治理意图=.github 仓 → [ROLE-IR.md](https://github.com/Cloudbird-Software/.github/blob/main/docs/agent/ROLE-IR.md)
- IR→spec：spec PR 必带测试设计逐类讨论（差分/属性/模糊…）+ holdout；**spec agent 不得直接实现** → [ROLE-SPEC.md](https://github.com/Cloudbird-Software/.github/blob/main/docs/agent/ROLE-SPEC.md)
- 实现卡片（PM 职责）：弱模型优先（子 agent / CNB 池）· fan-out=工具非流程 · 边做边推 PR · 3 次熔断自己接手 → [ROLE-IMPLEMENT.md](https://github.com/Cloudbird-Software/.github/blob/main/docs/agent/ROLE-IMPLEMENT.md)
- 验收 / 人类让你处理 issues：卡/IR 完成度检查 · bug 复现三值判定 → [ROLE-ACCEPT.md](https://github.com/Cloudbird-Software/.github/blob/main/docs/agent/ROLE-ACCEPT.md)

## Repo-specific onboarding (read after the entry protocol)

### What this repo is

Three divisions, per README.md: software body (`cmd/`+`internal/`), adaptation harness (`adapt/`), open-source monitoring (`upstream/` + `docs/UPSTREAM.md`). Language: Go, stdlib-only. Zero external module requires is a hard invariant (docs/OPERATIONS.md).

### Commands

- `make setup` — toolchain sanity (go version, no external deps).
- `make check` — the CI check target: gofmt zero-diff, go vet, arch check, `go test -race ./...`, report upload dirs.
- `make gates-pr` — local equivalent of the PR quality gates (fast subset + card metadata parse).
- `make card-test CARD=<n>` — print issue AC list for a work card.
- `make adapt-offline` — run the adapt canary suite against bundled fixtures (no network).

### Change discipline (machine-enforced)

- **C1 paths** (`.github/`, `AGENTS.md`, `CODEOWNERS`, `Makefile`, `docs/`, `quality/`, `governance/`, `scripts/`): any PR touching them MUST reference a real `ADR-NNNN` in title or body; the ADR must exist in `Cloudbird-Software/archive` at `adr/` (org-gate enforces). If your change needs a new ADR, add it to `adr/ADR-NNNN-*.md` in the same PR and cite it.
- **No `specs/**` directory** in this repo: `specs/` PRs require the org adversary audit (W4-C3). Product specs owned elsewhere; do not create `specs/` here without an org-level decision.
- **No new third-party Actions**, no new Go dependencies: whitelist and approval flow in org `governance/GOVERNANCE.yaml` + `docs/OPERATIONS.md`.
- **Branch model**: squash-only merges into `main`, linear history, delete branch on merge, auto-merge allowed. Write identity: GitHub App `cloudbrid-agent`.
- **Gate semantics**: required checks on PRs are `gate` (this repo's ci.yml aggregation), `org-gate`, `adversary` (auto-passed for PRs without `specs/**`). skipped≠success.
- **Go package rules**: entry binaries only under `cmd/`; `internal/` must not be imported outside this module; each public package exposes one entry file (`doc.go` or `<pkg>.go`); no circular imports between `internal/` packages (enforced by `go build` + `quality/arch-check.sh`).
- **Suppression markers** (`t.Skip`, `!TODO-fixme`, `lint:ignore` style markers in Go tests/annotations): budget-gated by the org suppression-gate (max +3 net per PR). Avoid them entirely.
- **Secrets**: never commit `.env`, `.pem`, `.key`, token-like fixtures. gitleaks runs over the full history on every PR (hygiene gate).

### Data & contract model

- All platform behavior is declared in `adapt/contracts/*.json`. Code must never hardcode endpoint paths/params; a version adaptation is a contract patch + fixture update + canary green. Playbook: `adapt/playbook/AGENTS.md`.
- Field completeness contract for comment authors: `uid`, `sec_uid` (MS4 form), `short_id`, `nickname`, `avatar_url`, `signature`, `ip_label`, `gender`, `follower_count`, `following_count`, `aweme_count`, `total_favorited` — see `internal/model` and canary assertions.

### Testing policy

- Golden fixtures in `adapt/fixtures/` are the source of truth for parser/binder tests; regenerating them is a C1-adjacent change (must cite ADR and keep both old+new behavior asserted in `adapt/canaries/`).
- Property tests (`internal/testkit/prop.go`) are required for: JSONPath walker, protobuf reader/writer, ws frame codec, store append/scan. Extend, don't delete.
- Live-network canaries are opt-in via secrets (docs/CANARY.md). Never commit live cookies.

### PR body template

```
## What
<one paragraph>
## Why
<one paragraph>
## Contracts
- <adapted contract names + version bumps, or none>
## Tests
- <what was executed>
Card: <owner>/<repo>#<n>   (when driven by a work card)
ADR-XXXX (<reason>)        (required when C1 paths touched)
```

## Docs (read on demand)

docs/ARCHITECTURE.md · docs/HARNESS.md · docs/CANARY.md · docs/UPSTREAM.md · docs/HARDENING.md · docs/TESTING.md · docs/OPERATIONS.md
<!-- synth-test-guide v1 (2026-09-06) -->

## 合成站测试指南（冷上下文 agent 按此操作）

### A. 环境准备（一次性）

```bash
cd D:/Projects/temp2/oracle/mediamonitor/Media-Monitor
go build -o bin/mediactl.exe ./cmd/mediactl
go build -o bin/signsvc.exe ./cmd/signsvc
```

Python venv 已在 `D:/Projects/temp2/oracle/env/`。Chrome 已装。

### B. 启动合成站并跑原子能力

```bash
# 1. 启动合成站（三站，预载数据 ~30s）
D:/Projects/temp2/oracle/env/Scripts/python.exe \
    D:/Projects/temp2/oracle/replay/synth_api.py --site all --base-port 8751 --preload &
sleep 35
# 验证
curl -s http://127.0.0.1:8751/_synth/health | grep -q ok && echo READY

# 2. 运行全量 A–H e2e（10 个子测试）
MEDIAMON_SYNTH_PORTS=8751,8752,8753 \
    go test ./internal/collect -run TestSynthE2ENewCapabilities -v -count=1
# 预期：全部 PASS（~170s）

# 3. 也可用 CLI 直连测试（需先建指向合成站的契约副本）
python3 -c "
import json, pathlib
src = pathlib.Path('adapt/contracts')
dst = pathlib.Path('/tmp/adapt_synth'); dst.mkdir(parents=True, exist_ok=True)
for f in src.glob('*.json'):
    c = json.load(open(f))
    t = c.get('transport') or {}
    if 'douyin' in t.get('base_url',''): t['base_url'] = 'http://127.0.0.1:8751'
    if 'kuaishou' in t.get('base_url',''): t['base_url'] = 'http://127.0.0.1:8753'
    if 'xiaohongshu' in t.get('base_url',''): t['base_url'] = 'http://127.0.0.1:8752'
    json.dump(c, open(dst/f.name, 'w'), ensure_ascii=False)
"
export MEDIAMON_ADAPT_DIR=/tmp/adapt_synth
printf 'ttwid=test' > /tmp/ck.txt
./bin/mediactl.exe collect search --platform douyin --keyword "美食" --limit 20 --cookies /tmp/ck.txt
```

### C. 离线回归（不需要网络/合成站）

```bash
go test ./internal/collect ./internal/httpclient ./internal/contracts -count=1
```

### D. 真站测试（需登录态 + 签名器）

```bash
# 启动签名器
DY_SIGN_PY=D:/Projects/temp2/oracle/env/Scripts/python.exe \
    ./bin/signsvc.exe --addr 127.0.0.1:9701 --provider node \
    --node-js D:/Projects/temp2/oracle/mediamonitor/signer_live/sign_dy.js &

# 跑真站原子测试
export MEDIAMON_REAL_LIVE=1
export MEDIAMON_ACCOUNTS_DIR=D:/Projects/temp2/oracle/mediamonitor/signer_live/accounts_dir
export MEDIAMON_TLS_IMPERSONATE=chrome152
export MEDIAMON_REAL_KW=海南瑾公子呀
go test ./internal/collect -run TestRealLiveDouyin -v -count=1
```

### E. 桥采集（签名阻拦时的备选路径）

```bash
D:/Projects/temp2/oracle/env/Scripts/python.exe \
    D:/Projects/temp2/oracle/mediamonitor/signer_live/bridge_adapter.py
```

### F. 排查

| 症状 | 解法 |
|---|---|
| 合成站 curl 000 | 等 35s 预载；日志看 "dataset ready" |
| search 0 条 | 检查 MEDIAMON_ADAPT_DIR 是否指向合成站端口 |
| 真站 403 Argus | 正常随机门控（~50%），等几秒重试 |
| go test 卡住 | netstat -ano \| grep 875 查端口占用 |
