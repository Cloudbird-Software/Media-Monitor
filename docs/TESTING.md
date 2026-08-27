# TESTING — deep real-world verification plan & required resources

Offline hermetic tests (fixtures, fakes, property checks) are already in CI;
this document defines the LIVE verification program: the resources to
provision, the matrix to run, and the success criteria that make a release
"truly scrapes what was asked". Everything here is executable by an agent
with the resources listed — the repo already carries the drivers:
`mediactl adapt canary --live`, `mediactl collect ...`, `mediactl live
monitor`, `mediad-mcp` tools, and the adapt harness playbook.

## Required resources (provision before live verification)

### 1. Network / egress
- Residential/4G proxy pool per platform region (douyin/kuaishou CN, xhs CN),
  SOCKS5+HTTP, geo/exchange-size/rotation policies; at least 20 IPs to test
  rotation, and a few stale/banned IPs on purpose (negative cases).
- A dedicated signer service deployment (`cmd/signsvc` + an upstream JS
  signer script wired via `--node-js`; see upstream/registry.json), TLS'd,
  reachable from the test runners. Set `MEDIAMON_SIGNER_URL` +
  `MEDIAMON_SIGNER_TOKEN`.
- mTLS-capped metrics/labels on the signer so burst/anomaly tests are
  observable; its /metrics must be scrapeable.

### 2. Accounts & sessions
- Per platform: >=5 real accounts in each bucket {new(0d) / aged(30d+) /
  with group memberships / live-viewer capable}; one owner account with a
  group we may enumerate (the group-member feature).
- Export per-account browser cookies to the format `mediactl --cookies`
  accepts (`k1=v1; k2=v2`); keep them in the secrets manager, never in git.
- Rotate accounts; expect captcha/risk-control walls on the new/mobile-IP
  buckets — those runs validate fail-closed handling, not success.

### 3. Devices (for the ADB/vision layer)
- 2+ physical Android devices w/ USB debugging + wireless adb; OEM/adb-key
  pre-authorized; uiautomator available; target apps installed (the versions
  under test). Cloud-phone farm acceptable for scale runs.
- An OpenAI-compatible vision endpoint (local UI-TARS-7B via vLLM or cloud)
  with `MEDIAMON_VISION_ENDPOINT`-style config for the vision layer.

### 4. Observability & storage
- mediad instance + Prometheus (or the embedded /metrics) + logs dirs;
- dedicated SQLite/store dirs per run for post-run audits;
- CI repo secrets: `MEDIAMON_CANARY_COOKIES_{DOUYIN,KUAISHOU,XHS}`,
  `MEDIAMON_LIVE_ROOM_URL`, `MEDIAMON_CANARY_AWEME_ID`,
  `MEDIAMON_CANARY_KEYWORD`, `AGENT_APP_SECRET` (provisioned), signer creds.

## Test matrix (by capability)

### A. Search (video/image) — per platform
- golden: 5 keywords x {video filter, image filter, unfiltered}, 3 pages each; assert ≥1 item/page, ID/desc/author present, media_type correct, pagination terminates and cursor monotonic.
- extremes: 0-result keywords; >10k-result keywords pagination to 300; unicode/emoji keywords; 64-char keywords; repeated identical queries (dedup/cursor stability); keyword changes mid-run.

### B. Comments + user fields
- golden: pick one hot item; walk ≥20 pages; assert the 10+ author fields (uid, sec_uid/MS4 form, short_id, nickname, avatar, signature, ip_label, gender, counters) present on ≥90% of authors; sticky comment surfaced; reply fan-out via get_replies on a top comment.
- extremes: item with 0 comments (empty-page success); comment pages during live flood (high churn — verify no duplicates/loss via store audit); deleted-comment placeholders; author whose region label is hidden; comment text with emoji/zb common chars; >1000 replies fan-out; cid continuity across cursor.

### C. Live room monitoring
- golden: target room (`MEDIAMON_LIVE_ROOM_URL`): 30min soak asserting every event family observed once (enter/chat/like/gift/follow/fansclub/rank/room_stat) + terminal control on end; hb/ack stability (obs live.hb/live.ack deltas) + zero reconnect in stable room.
- extremes: room ended mid-monitor; room restarted (new session); server close without close-frame; gzip on/off mixtures; 30 concurrent rooms (mediad + MCP lobby isolation); signature service outage mid-run (fail-closed, reconnect respects limit); clock skew >1h (event times sane per response `now`).

### D. Group member enumeration
- golden: owner-account group: enumerate to exhaustion; assert uid/sec_uid pairs and dedupe; member-count sanity vs app UI.
- extremes: pagination boundary (group size near page size); member leaves mid-run; account removed from group (auth failure surfaces clearly).

### E. ADB / vision (device lane)
- golden: tap/swipe/text/screencap/uidump on 2 devices; ui tree parsed; flow script `adapt/flows` replays on the target app version; vision provider drives one multi-step flow when accessibility alone fails (auto-repair path).
- extremes: cable unplug mid-exec; unauthorized device (ErrAuthRequired); uiautomator dump missing (fallback path); screen locked; huge XML hierarchy (>10k nodes); slow device (>2s per op).

### F. Soak & resilience
- 48h mixed workload (search+comments+2 live rooms); store audit: no duplicate (cid,uid) keys beyond policy; goroutine/heap stable (pprof snapshot diff); restart-resume of tasks (cursor persistence); clock-step + TZ changes.

## Success criteria (release-gating)

1. A/B/C/D golden rows pass on 2 consecutive runs, 24h apart.
2. Zero error-level drift findings in `adapt canary --live`; the offline canary suite stays green on the same builds.
3. Store audit shows the mandated user fields on ≥90% of comment authors (per contract).
4. Live monitor: every event family observed within the golden soak; correct terminal behavior for room-end.
5. All extreme-case rows end in one of: clean success, documented skip, or a fail-closed error with the documented code — never silent wrong data, never a hang. Any hang/incorrect-data row blocks the release.
6. redteam battery (docs/HARDENING.md M8) green on the deliverable build.

## Artifacts per run
adapt/reports/*.json (drift), store JSONL exports, obs metrics snapshot, pprof
snapshots, and a short triage note per failed row (adapt playbook format).

## Offline matrix lane (`mediactl lab`)

The groups above mix live-device rows with offline-assertable ones. The lab
commands execute the offline portion against the repository's own golden
fixtures served by an in-process mock platform (contracts remapped onto a
loopback listener — the same pattern the engine tests use), and record a
three-valued judgment per row: `clean_success`, `documented_skip`, or
`fail_closed` with its documented code (success criterion 5). Rows that
need the owner environment (live accounts ENV-REQ-1, real devices
ENV-REQ-2, vision endpoint ENV-REQ-3) end as documented skips here with
their environment code; live evidence stays owner-side (INV-4).

    mediactl lab matrix <a|b|e|user_posts>     # report to adapt/reports/matrix-<group>-<ts>.json
    mediactl lab audit-comments --store <dir>  # AC-19: comment-author 12-field completeness

The user_posts group drives the IR-new backtrack atom end to end: depth
(3 fixture pages, newest-first descending max_cursor chain),
min_engagement early stop inside the engine, window cutoff from both
sides of the live clock, BEH-4 cursor resumption without refetching page
one, and the undeclared-contract fail-closed demo (kuaishou). Real
account/device execution of A/B/E remains the release-gating owner run;
this lane keeps those builds honest between runs and catches contract or
binding regressions at PR time.