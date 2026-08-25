# ARCHITECTURE

## Boundary at a glance

```
cmd/mediad          ─┐ (daemon: health/metrics/dashboard/task API)
cmd/mediactl        ─┤ (CLI: collect / live / adapt / tasks)
cmd/mediad-mcp      ─┘ (MCP server, stdio)
        │ Go stdlib only · module-internal imports only
internal/
  model        canonical data types (no behavior)
  contracts    contract loader / JSONPath binder / drift diff
  collect      contract-driven generic collection engine
  platforms    per-platform glue: defaults, cookie/signing policies
  live         live-room event monitors (websocket + protobuf wire)
  adb          Android device control protocol client
  vision       GUI-model providers + semantic actions + flow distilling
  store        JSONL append-only storage + id maps
  core         task lifecycle
  obs          counters + metrics text + health
  sign         signature provider interface (pluggable: local / remote svc)
  testkit      property-test runner + fixture helpers
adapt/  contracts/ · canaries/ · fixtures/ · flows/ · playbook/ · reports/
upstream/  open-source ecosystem registry (git pin pointers + policy)
quality/  local gate orchestration
```

## Design invariants (enforced by CI)

- Zero external Go modules; everything on the stdlib (dependency policy).
- Endpoint URLs / params / bindings never hardcoded in .go files — they live
  in `adapt/contracts/*.json`. Version adaptation = contract patch.
- All I/O is injectable (http transport, signer, store dir, clock) so tests
  run offline against `adapt/fixtures/` goldens.
- `internal/` packages expose one entry file each; entrypoints only under
  `cmd/`; no package cycles.

## Data flow

1. Task created via CLI/MCP/daemon API (`core.Runner.Submit`) → persisted as
   JSONL (`store`).
2. Runner executes a `step` closure. Collect steps are produced by the
   generic engine (`collect`) from a Contract: build URL (path placeholders +
   static query) → optional Signer enriches params (a_bogus/msToken/X-Bogus
   proxies) → request via `httpclient` (UA rotation, retry) → JSON → binding
   via `contracts.Path` → normalized `model.*` records → store + cursor
   update → loop until `HasMore=false`.
3. Live monitors (`live`) subscribe to the room websocket; the wire protocol
   reader (`protoio`) decodes PushFrame/Response envelopes; per-message-type
   mappers emit `model.LiveEvent` streams; ack+heartbeat keep the session
   alive; control messages end the stream.
4. Mobile automation (`adb` + `vision`): semantic actions are executed either
   deterministically from `adapt/flows/*.json` (accessibility/input scripts)
   or through a vision provider (`vision.Provider`) that maps screenshots +
   task description to the same semantic-action vocabulary; successful
   vision-driven runs can be distilled back into flow scripts.
5. Observability (`obs`): counters for requests, retries, records collected,
   task states, drift issues; `/metrics` text endpoint on the daemon.

## Contract model

Contract = transport (base/path/method/static query/headers/placeholders) +
signature requirements + response binding (JSONPath-lite) + pagination spec +
cookie requirements. The same struct drives collection AND drift detection
(`contracts.Diff`), so a platform change shows up twice: once as a canary
failure, once as a machine-readable diff report for the adapting agent.

## Extension points

- New platform: add `adapt/contracts/<platform>-*.json` + platform defaults in
  `internal/platforms` (UA/cookie names/host normalization) + fixture +
  canary entry. No engine code changes.
- New live event type: extend the message-type mapper in `internal/live` for
  the new wire field; canary fixture must carry it.
- New signer: implement `httpclient.Signer` (local VM port / remote sign
  service client) and set it in the collector config.