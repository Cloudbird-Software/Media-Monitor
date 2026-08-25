# adapt/playbook/AGENTS.md — platform-breaking-change repair playbook

Scope: this directory is the operational surface for version adaptation.
Entry conditions: a canary failure, a live-run drift report, or an upstream
radar alert (docs/UPSTREAM.md).

## Loop (executable, not prose)

1. **Read the signal** — `mediactl adapt canary --offline` locally, or the
   drift report under `adapt/reports/` (JSON: contract, severity-coded
   issues). Identify the failing binding path.
2. **Capture** — obtain a fresh observed payload for the failing endpoint
   (saved into a scratch fixture). Do not commit live payloads directly:
   run `mediactl adapt snapshot --accept <name>` which reduces/redacts and
   prints the schema diff for review.
3. **Classify** — endpoint moved | params changed | schema changed |
   auth/signature gate. Record the classification in the PR body.
4. **Patch** — edit the contract JSON(s); bump `version` on breaking
   changes; keep the previous contract file as `<name>-vN.json`. Add or
   update the fixture and the canary case; assertions must include the
   path that previously failed.
5. **Verify** — `make adapt-offline` green here + CI on the PR. The org
   contract/suppression/zizmor gates all apply.
6. **Release** — squash-merge; consumers pick contract version via repo
   state. Close the drift subject with the PR link.

## Rules

- Fixtures are synthetic goldens of public API shapes only: no PII, no
  credentials, gitleaks-clean (full-history scan runs on every PR).
- One adaptation unit per PR (one platform × one category).
- Changes to `adapt/contracts` version numbers must be machine-diffable:
  never reformat unrelated lines.
- Live-mode secrets (docs/CANARY.md) must never enter the repo.
- If the fix requires signing/token changes rather than contract changes,
  the PR is C1-adjacent: reference the relevant ADR and update
  docs/HARDENING.md threat notes when the mechanism itself shifts.

## Handoff to CI

- PR events re-run the offline canary (quality-gates).
- Scheduled `canary.yml` (daily) re-verifies everything merged; drift
  issues are filed automatically when App secrets are provisioned.
- `upstream-watch.yml` (6h) alerts on ecosystem activity; treat its hits
  as pre-signals, run this loop before the platform forces it.