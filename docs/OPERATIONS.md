# OPERATIONS

## Dependency policy (enforced in CI)

- Go stdlib only; any `require` in go.mod fails the push-side audit and
  needs org-level dependency approval before landing (proposal format:
  name / purpose / license / stdlib_alternative).
- Forbidden licenses: AGPL-3.0, GPL-3.0, SSPL (also forbids vendoring
  GPL-family code; watching them via upstream registry is allowed).

## Governance alignment (org Cloudbird-Software)

- Branch model: PR + squash into `main`, linear history, delete-branch-on
  -merge, auto-merge enabled (org BP-1/BP-4; settings applied by org
  governance).
- Required checks on PR: `gate` (this repo), `org-gate`, `adversary`
  (org-level required workflows). `adversary` auto-passes PRs that do not
  touch `specs/**` — do not create a `specs/` directory in this repo.
- C1 paths (`.github/`, `AGENTS.md`, `CODEOWNERS`, `Makefile`, `docs/`,
  `quality/`, `governance/`, `go.mod`): PRs must reference a real `ADR-NNNN`
  living in `Cloudbird-Software/archive` `adr/`. New ADRs for this repo are
  added as `adr/ADR-NNNN-*.md` within the same PR and cited (existence check
  passes via PR-head files), then mirrored per the org ADR home rules.
- Actions whitelist: GitHub-official, verified, org-owned, and the org
  allow-list patterns only; all `uses:` pins are 40-hex SHAs.
- Code scanning: org code-security default ("GitHub recommended") applies;
  CodeQL gates PR merges at medium+ security alerts.
- Suppression markers in tests: budget-gated (≤3 net per PR); avoid them.

## Write identity

All commits/PRs/merges to this repo are performed by the org GitHub App
`cloudbrid-agent` via installation-scoped tokens (1h TTL). No human PAT
should ever be used for routine writes. Token minting script lives in
deployment infra; agents obtain tokens through the org credential path.

## Release process

1. `make gates-pr` locally green; PR green on all three required checks.
2. Squash-merge; bump `adapt/contracts` versions in the same PR when
   contracts change (HARNESS.md rules).
3. Delivery artifacts are produced by the external delivery pipeline
   implementing docs/HARDENING.md (this repo is source-of-truth only).

## Secrets

- Repo secrets (Actions): `AGENT_APP_SECRET` (App private key for
  drift issue filing and dependabot automerge; app-id 4632704 fixed),
  and the live-canary cookie
  secrets documented in docs/CANARY.md.
- Never commit: `.env`, `.pem`, `.key`, live cookies, device serials.
  Hygiene gate runs gitleaks over the full history on every PR.

## Issue/PR conventions

- Issues carry `type:*` labels (drift/upstream/bug/feature); drift and
  upstream watch alerts are machine-filed.
- PR body follows AGENTS.md template; `Card:` metadata line when
  card-driven; `ADR-NNNN` reference when C1 paths touched.