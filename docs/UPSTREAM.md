# UPSTREAM — open-source ecosystem monitoring

`upstream/registry.json` is the machine-readable registry of external
open-source projects this repo tracks (request/protocol/signature/device
automation ecosystems). Entries are pointers (repo slug + git pin + tracked
paths + license verdict), not vendored code. Webhook-free; polling based.

## Watcher (workflow: upstream-watch.yml)

- Scheduled (every 6h) + manual dispatch.
- For each entry: fetch latest commits/releases via the GitHub public API
  (no clone), filter filenames against `tracked_paths` patterns (e.g.
  `*sign*`, `*bogus*`, `*proto*`, `*version_code*`, `*xh*s*`).
- New matching upstream activity ⇒ drift alert issue in this repo with label
  `type:upstream` (via repo App token secrets) + job summary. This is the
  early-warning layer: ecosystem repos usually adapt to platform changes
  before this repo's own canaries notice them.

## Vendor submodules (materialized, ADR-0099)

`upstream/vendor/` holds four observation-copy submodules (f2 /
wx_channels_download / MediaCrawler / UI-TARS), each pinned to the SHA
recorded in the registry (clone with `--recurse-submodules`; CI never needs
them — the arch guard reads source text only). They exist so the watcher's
diff summaries and the swap-test bench have a local, diffable baseline
(track A of the dual-track adaptation strategy, IR-MM-0001 D-3).
`internal/` and `cmd/` never import `upstream/` — enforced fail-closed by
`quality/arch-check.sh` (INV-3). Moving a pin is a PR (data change).
Borrowing boundaries per IR D-2: f2 / wx_channels_download / UI-TARS
(permissive) may be referenced and ported; MediaCrawler (non-commercial) is
watch-only — parameters and binding knowledge, never code.

## Swap-test bench (local, agent-driven)

`upstream/swap-test.md` workflow (agent-only; not automated yet):
pin upstream → adapter shim under `internal/platforms` or standalone script
→ run the same canary suite against the upstream implementation → score
(success rate / freshness / license). Adoption requires a C1 PR with the
score attached. Removal mirrors it. The vendor submodules above provide the
pinned local baseline the bench scores against.

## Policy

- License: forbidden AGPL-3.0 / GPL-3.0 / SSPL everywhere inside the repo
  (org dependency policy); GPL-family projects may be watched (metadata
  only) but never linked or vendored into this module's code.
- Pins: entries pin to a release ref where available (`pin.type=tag`),
  otherwise to a commit SHA. Moving a pin is a reviewable data change.
- tracked_paths are the only paths that can raise an alert; everything else
  in the upstream repo is ignored by the watcher.

## Schema (upstream/registry.json)

```json
{
  "version": 1,
  "entries": [
    {
      "slug": "owner/repo",
      "role": "signature|collector|live|device|vision",
      "pin": {"type": "tag|sha", "ref": "..."},
      "license": {"spdx": "Apache-2.0", "verdict": "allowed"},
      "tracked_paths": ["*.proto"],
      "notes": "one line"
    }
  ]
}
```