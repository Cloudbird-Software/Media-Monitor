# VR Integration — consuming Media-Monitor atoms over MCP

Viral_Radar is the first MCP consumer of Media-Monitor's collection atoms
(IR-MM-0001 AC-20). This document is the joint slice playbook: the three
segments VR needs — **user_posts backtrack → comments cursor chain →
download** — expressed as real `tools/call` payloads, the cross-process
artifact contract, and the egress rules that keep the split compliant.

Status legend: ✅ = tool on MM's current MCP surface; ⚠️ = see note.

| Atom            | Tool             | Status |
| --------------- | ---------------- | ------ |
| search          | `search_items`   | ✅     |
| creator history | `get_user_posts` | ✅     |
| comments        | `get_comments`   | ✅     |
| replies         | `get_replies`    | ✅     |
| profile         | `get_user`       | ✅     |
| play address    | `resolve_video`  | ✅     |
| media bytes     | `download_video` | ✅ restored — the W3-C3 surface landed again with the exact IFACE-3 shape below (incident recovery, byte-identical to its accepted merge) |

## 0. Transport

VR's adapter lives in the VR repo (`src/viral_radar/adapters/mcp/`,
python-sdk `ClientSession` + asyncio bridge) and speaks JSON-RPC to a
running `mediad-mcp`. Every payload below is what goes over the wire after
the handshake (`initialize`, then `notifications/initialized`).

## 1. Segment one — user_posts (window + threshold backtrack)

Contract: newest-first listing; stop conditions fire in-behavior, not as
errors. `min_engagement` never truncates on a single low item (creators are
not monotonic); items older than `window_months` are not emitted; the
returned `cursor` is resumable, early-stop included.

```json
{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{
  "name":"get_user_posts",
  "arguments":{
    "platform":"douyin",
    "sec_uid":"MS4wLjABAAAA...",
    "window_months":6,
    "min_engagement":{"metric":"digg","threshold":10000},
    "stop_after_consecutive":5,
    "limit":200
  }
}}
```

Result (shape): `content[0].text` carries NDJSON/JSON with `items[]`
(`id`, `desc`, `media_type`, `create_time`, full `stats`:
digg/comment/share/collect[/play], `author` summary) plus `cursor`
(`{"page":N,"has_more":bool,"source":{"max_cursor":...}}`). Continue by
passing that cursor object back verbatim:

```json
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{
  "name":"get_user_posts",
  "arguments":{"platform":"douyin","sec_uid":"MS4wLjABAAAA...","limit":200,
               "cursor":{"page":3,"has_more":true,"source":{"max_cursor":1780499996000}}}
}}
```

MM-side behavioral proof (offline, mock-driven): `mediactl lab matrix
user_posts` walks the golden fixture pages and proves depth ≥ 3, the
descending max_cursor chain, the consecutive-low early stop inside the
engine, window cutoff from both sides of the clock, and resumption without
re-fetching page one.

## 2. Segment two — comments across a cursor chain

Pagination is cursored: pass back exactly the `cursor` each result returns.
The per-call `limit` stops being a depth ceiling once you keep chaining.

```json
{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{
  "name":"get_comments",
  "arguments":{"platform":"douyin","item_id":"7660000000000000001","limit":50}
}}
```

Continue:

```json
{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{
  "name":"get_comments",
  "arguments":{"platform":"douyin","item_id":"7660000000000000001","limit":50,
               "cursor":{"page":2,"has_more":true,"source":{"cursor":2}}}
}}
```

Rules the consumer must honor:

- dedupe on `cid` across legs (deleted-comment placeholders may appear mid-
  stream and parse cleanly; they count as data, not errors);
- treat author fields as best-effort per model.go's completeness contract —
  the owner-side audit (`mediactl lab audit-comments --store <dir>`) holds
  the ≥90% AC-19 line;
- replies fan out via `get_replies` on `{platform,item_id,cid}`.

MM-side proof: `mediactl lab vr-slice --sec-uid <id>` segment 2 drives two
continuation legs over the mock chain and asserts zero duplicate cids.

## 3. Segment three — download (bytes off the MCP channel)

`resolve_video` returns the address only; media bytes intentionally do not
ride MCP (mcpio row cap). Consumers pick them up from disk:

```json
{"jsonrpc":"2.0","id":20,"method":"tools/call","params":{
  "name":"resolve_video",
  "arguments":{"platform":"douyin","item_id":"7660000000000000001"}
}}
```

Artifact convention (IFACE-3): files land at

```
<artifacts_root>/<platform>/<item_id>.mp4
```

with the delivery record `{path, bytes, sha256}`. Same-host deployment
(default): both processes read/write the same volume, so VR receives the
absolute path and verifies `sha256(local_file) == record.sha256` before
ingest. Split hosts need a shared mount pointed at MM's artifacts root —
there is deliberately no HTTP side-channel between the two services.

Writer-side guarantees: tmp-file + atomic rename publish (a half-written
mp4 is never visible under the final name), streaming write (no whole-file
buffering), hash computed while streaming.

The restored `download_video` tool returns exactly this `{path, bytes,
sha256}` shape; nothing above changes for VR.

MM-side proof: `mediactl lab vr-slice` segment 3 resolves through the mock
contract, streams 64 KiB to `artifacts/douyin/<item>.mp4` in the run dir,
and asserts byte-count and sha256 equality against the served payload.

## 4. INV-3 compliance (VR never egresses directly)

- VR performs **zero direct platform network access**: every acquisition
  call enters through MCP tools; VR's HostGuard allowlist contains only the
  MM service addresses (`mediad-mcp` endpoint; signer/metrics stay MM-
  internal and are reachable solely from MM's process).
- The transport adapter maps one logical function to one atom
  (`transport(cursor)->{items,next_cursor}`, `fetch_profile→get_user`) and
  owns no scraping logic — upstream rewrites land only on MM's side.
- Evidence trail (INV-4): live joint-run records reference the run report
  paths (`adapt/reports/vr-slice-evidence-*.json`, matrix reports) rather
  than self-reported numbers.

## 5. Joint acceptance checklist (AC-20)

| AC   | What                                                                    | Where the evidence comes from                              |
| ---- | ----------------------------------------------------------------------- | ---------------------------------------------------------- |
| AC-1 | user_posts threshold backtrack reaches the engine                       | `lab vr-slice` seg1 metrics + owner live run record        |
| AC-2 | comments ≥2 cursor pages merged, > single-page limit                    | `lab vr-slice` seg2 + owner live run record                |
| AC-3 | download path readable + sha256 matches                                 | `lab vr-slice` seg3 (+ same check from VR container)       |
| AC-4 | end-to-end run archived                                                 | issue-linked evidence JSONs                                |
| AC-5 | VR INV-3 compliance                                                     | VR repo card's HostGuard test assertions                   |

Mock-slice green covers MM's tool face; VR-repo transport tests belong to
the VR card, and the final live joint run belongs to the owner.
