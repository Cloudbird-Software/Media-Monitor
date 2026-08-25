# Media-Monitor

Machine-readable index. Humans: use AGENTS.md as the entry point instead.

## Repo ID

- org: Cloudbird-Software
- repo: Media-Monitor
- layer: L2 (implementation/product)
- language: go (stdlib-only, see dependency policy in OPERATIONS.md)
- license: MIT
- write identity: GitHub App `cloudbrid-agent` (installation-scoped tokens, see AGENTS.md)

## What this repository contains (three divisions)

1. **Software body** (`cmd/`, `internal/`, exported binaries)
   - `cmd/mediad` — daemon: REST health+metrics+dashboard, task runner host.
   - `cmd/mediactl` — CLI for humans/agents: collectors, live monitor, mobile control, adapt harness, task ops.
   - `cmd/mediad-mcp` — MCP server (stdio) exposing the same capabilities as tool calls.
   - `internal/collect` — contract-driven generic collection engine (search/comments/replies/users/group members).
   - `internal/platforms`, `internal/live` — platform adapters: douyin (web+live-room event stream), kuaishou, xhs; live room event monitoring over websocket+protobuf.
   - `internal/adb` — Android device control over the ADB protocol (shell, screencap, input, ui dump, scrcpy spawn).
   - `internal/vision` — GUI visual-agent layer: provider abstraction, semantic action schema, flow-script distilling (auto-repair + capability extension).

2. **Adaptation harness** (`adapt/`) — fast version-adaptation machinery. Contracts as data, canary goldens, fixtures, agent playbook, drift reports. See docs/HARNESS.md. Also driven by GitHub Actions (canary.yml).

3. **Open-source monitoring** (`upstream/` + `docs/UPSTREAM.md`) — pinned third-party ecosystem radar: pointer registry (submodules/git pins), watcher workflow, swap-test bench. Version-adaptation early-warning sensors.

## Capability matrix (target)

| capability | component | status marker |
|---|---|---|
| keyword search (video/image posts, multi-platform) | collect engine + platform contracts | see adapt/contracts |
| comment + reply collection with author profile (uid, sec_uid/MS4, short_id, nickname, avatar, signature, region ip_label, gender, stats) | collect engine | model.UserProfile |
| live-room monitoring: enter/like/chat/gift/follow/fansclub/rank/room-stat events | internal/live | websocket+protobuf |
| group member silent enumeration | collect engine IM endpoints | platform contracts |
| MCP/CLI/daemon interfaces | cmd/mediad-mcp (15 tools) + medad REST + mediactl | implemented |
| open-source radar + canary + hardening audit | .github/workflows (upstream-watch/canary/redteam) | implemented (workflows active; secrets provision on deployment) |
| PC → phone control (ADB + accessibility flows + scrcpy) | internal/adb + internal/vision flows | flow scripts in adapt/flows |
| GUI-model integration (auto-repair + capability layer) | internal/vision | provider-agnostic |

## Layout

```
cmd/                       entry points only (3 binaries)
internal/                  no cross-module cycles; boundaries enforced by quality/arch-check.sh
adapt/contracts/           runtime contracts (JSON; versioned; the adaptation unit)
adapt/canaries/            golden canary cases (offline fixtures + optional live mode)
adapt/flows/               UI flow scripts for mobile automation
adapt/playbook/AGENTS.md   adaptation playbook for agents
docs/                      ARCHITECTURE/HARNESS/CANARY/UPSTREAM/HARDENING/OPERATIONS
quality/                   local gate scripts (run-gates.sh / arch-check.sh)
upstream/                  open-source monitoring registry
.github/workflows/         ci.yml (org gate wiring), canary.yml, upstream-watch.yml, redteam.yml
```

## Docs index

- docs/ARCHITECTURE.md — module boundaries, data model, event flow.
- docs/HARNESS.md — how the adapt/ harness works, CLI verbs, agent workflow.
- docs/CANARY.md — canary semantics, workflows, how live-mode secrets are consumed.
- docs/UPSTREAM.md — upstream registry policy, watcher, swap-test bench.
- docs/HARDENING.md — delivery/packaging hardening specification (future delivery pipeline contract).
- docs/TESTING.md — live verification program: needed resources, matrix, release criteria.
- docs/OPERATIONS.md — dependency policy, repository governance alignment, release notes discipline.

## Getting started (agent)

Everything an agent needs to onboard is in AGENTS.md; do not read beyond it until instructed by it.

## Quick reference (automation)

- Run offline canary: `make adapt-offline` (or `mediactl adapt canary --offline`)
- Run live connections: `mediactl live monitor --room <url> [--signer-url $MEDIAMON_SIGNER_URL]`
- Start daemon: `mediad -dir ./data -addr 127.0.0.1:8088` (health/metrics/tasks/collect REST + dashboard)
- Start MCP server: `mediad-mcp` (stdio; wire into any MCP host)
- Adaptation playbook for breakages: `adapt/playbook/AGENTS.md`