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
   - `cmd/mediad` — daemon: REST health+metrics+dashboard, task/collect/send/accounts REST + dashboard.
   - `cmd/mediactl` — CLI: collectors, live monitor, DM broadcast, trace run, accounts, export, webhook, netcapture, toolbox (encrypt/stylize/wechat-multi), self-update, adapt harness, task ops.
   - `cmd/mediad-mcp` — MCP server (stdio, 20 tools) exposing the same capabilities as tool calls.
   - `internal/collect` — contract-driven collection engine (search/comments/replies/users/group/video/collects/im-unread).
   - `internal/platforms`, `internal/live` — platform adapters: douyin/kuaishou/xhs; live monitoring with pluggable decoders (douyin protobuf, kuaishou/xhs gunzip+base64 JSON).
   - `internal/tasks` — DM broadcast orchestration (templates, delay, send caps, retry).
   - `internal/trace` — probabilistic trace/engagement engine (device-equalized, adb executor; flows: douyin + shipinhao).
   - `internal/accounts` — multi-account pool (cookies/proxy/UA, Netscape/JSON import/export).
   - `internal/datacenter` — lead data hub (dedup, cap, keyword filter, webhook push w/ retry, CSV export).
   - `internal/license` — client-side license/activation/device-binding (Ed25519 offline verify).
   - `internal/selfupdate` — update skeleton (manifest check + SHA256 download to data/updates/).
   - `internal/netcapture` — network-capture tool (session + HAR export).
   - `internal/adb` — Android device control over the ADB protocol (shell, screencap, input, ui dump).
   - `internal/vision` — GUI visual-agent layer (provider abstraction, flow-script distilling; frozen this phase).

2. **Adaptation harness** (`adapt/`) — fast version-adaptation machinery. Contracts as data, canary goldens, fixtures, agent playbook, drift reports. See docs/HARNESS.md. Also driven by GitHub Actions (canary.yml).

3. **Open-source monitoring** (`upstream/` + `docs/UPSTREAM.md`) — pinned third-party ecosystem radar: pointer registry (submodules/git pins), watcher workflow, swap-test bench. Version-adaptation early-warning sensors.

## Capability matrix (final)

| capability | component | status marker |
|---|---|---|
| keyword search (video/image posts, 3 platforms) | collect engine + platform contracts | implemented (douyin/kuaishou/xhs) |
| comment + reply collection with 12-field author profile | collect engine | implemented (model.UserProfile) |
| live-room monitoring (enter/like/chat/gift/follow/fansclub/rank/room-stat) | internal/live | implemented (douyin protobuf + kuaishou/xhs JSON decoders) |
| group member silent enumeration | collect engine IM endpoints | implemented (douyin/kuaishou/xhs) |
| direct-message broadcast (two-message flow, caps, retry) | internal/tasks | implemented |
| trace/engagement engine (probabilistic gestures, device equalization, adb) | internal/trace + adapt/flows/{douyin,shipinhao}-trace.json | implemented (douyin + shipinhao flows) |
| toolbox: WeChat multi-open | internal/toolbox/wechat + `mediactl toolbox wechat-multi` | implemented |
| toolbox: content encryption (zero-width steganography + phone stylization) | internal/toolbox/encrypt + `mediactl toolbox encrypt/stylize` | implemented |
| watermark-free video download + collects folder enumeration | collect engine | implemented |
| IM unread-count polling | collect engine | implemented |
| multi-account pool (cookies/proxy/UA, import/export, rotation) | internal/accounts | implemented |
| data center: dedup / cap / keyword filter / webhook push / CSV export | internal/datacenter | implemented |
| license/activation/device-binding (Ed25519 offline verify) | internal/license | implemented |
| self-update skeleton (manifest check + SHA256 download) | internal/selfupdate | implemented |
| network-capture tool (session + HAR export) | internal/netcapture | implemented |
| MCP/CLI/daemon interfaces | cmd/mediad-mcp (20 tools) + mediad REST + mediactl | implemented |
| open-source radar + canary | .github/workflows (upstream-watch/canary) | implemented |
| ADB device control (shell/screencap/input/ui-dump) | internal/adb | implemented |

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