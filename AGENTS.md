# AGENTS.md — Media-Monitor

Agents working in this repository must read this file first. It is machine-enforceable where stated.

<!-- entry-protocol v1 -->

### 入口协议（陌生 agent 从这里开始——宪法 §11 / ADR-0055）

1. 取 ghcb（钉 SHA，禁浮动 main）：`curl -fsS -o ghcb https://raw.githubusercontent.com/Cloudbird-Software/.github/f72d9520706c8fca974d92456f65cae5c1412bb7/scripts/ghcb && chmod +x ghcb`（凭据用你自己的：`gh auth login` 或 `export GH_TOKEN=<PAT>`；`-f` 必带）
2. 找活：`bash ghcb next [owner/repo]` → 列 state:ready 卡（卡 issue 是唯一工作凭证，无卡不开工）
3. 认领：`bash ghcb claim <n> [owner/repo]`
4. 开工：`make card-test CARD=<n>` → `make gates-pr`（本地复现 CI 关卡）
5. 提 PR：body 必带一行卡元数据 `Card: <owner>/<repo>#<n>`
6. front-desk 命令（卡 issue 评论）：/claim 认领 · /release 释放租约 · /retry 隔离回流

<!-- /entry-protocol -->

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