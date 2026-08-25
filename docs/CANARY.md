# CANARY

## Purpose

Continuous verification that every declared platform contract still matches
platform reality. Two modes:

- **offline** (default, no credentials): run `adapt/canaries/*` against
  `adapt/fixtures/` goldens. Verifies schema/binding integrity of the
  repository itself — the guard against our own regressions and against
  fixtures drifting from contracts. Implemented.
- **live** (opt-in): execute real requests against platforms using
  secrets provided by the deployment. Verifies the outside world still
  matches the contracts — the guard against platform changes. The CLI
  flag exists (`mediactl adapt canary --live`); the live driver lands
  with the collector/live-monitor PRs and is inert (documented skip)
  until then.

## Workflows

- `.github/workflows/canary.yml` — scheduled (daily) + `workflow_dispatch`.
  Offline mode always runs. Live mode activates only when the repo secrets
  listed below are present; otherwise the job posts `live: skipped
  (no secrets)` and succeeds (documented expected skip, not silent).
- Canary failures produce a drift report artifact under `adapt-reports` and
  a structured job summary; when `MEDIAMON_CANARY_ALERT_ISSUE_LABEL` is set,
  an issue is filed via the repo App token (secret `AGENT_APP_SECRET`, app-id 4632704 fixed) with label `type:drift`.

## Live-mode secrets (deployment-owned, never committed)

| secret | meaning |
|---|---|
| `MEDIAMON_CANARY_COOKIES_DOUYIN` | browser cookies string for the douyin canary account |
| `MEDIAMON_CANARY_COOKIES_KUAISHOU` | same for kuaishou |
| `MEDIAMON_CANARY_COOKIES_XHS` | same for xhs |
| `MEDIAMON_LIVE_ROOM_URL` | a stable/example room URL for the live-monitor canary |
| `MEDIAMON_CANARY_AWEME_ID` | item id with comments for the comment canary |
| `AGENT_APP_SECRET` (repo App private key, same secret used by the org automerge workflow; app-id 4632704 is fixed) | repo App token for drift-issue filing |

## Adding a canary

1. Add fixture under `adapt/fixtures/`.
2. Register a canary case under `adapt/canaries/offline.json` (and the live
   descriptor next to it for live-mode execution in later PRs: `canaries/live.json`).
3. `make adapt-offline` must stay green (offline first; live follows when the
   deployment adds secrets).

## Failure semantics

skipped≠success: a canary that cannot run for a reason other than the
documented missing-secrets skip is a failure (fail-closed).