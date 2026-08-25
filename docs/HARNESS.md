# HARNESS — fast version adaptation

The adaptation harness exists so that a platform breaking change is absorbed
as a small, reviewable data patch instead of a code scramble. Components:

```
adapt/contracts/     one JSON per collection operation (version field)
adapt/canaries/      golden cases: contract × fixture × assertions
adapt/fixtures/      golden responses (synthetic samples of public API shapes)
adapt/flows/         UI flow scripts for mobile automation (per app version)
adapt/playbook/      agent playbook for the repair loop
adapt/reports/       drift reports (gitignored, generated)
```

## CLI verbs (mediactl adapt ...)

```
adapt canary --offline [name]          run canary suite against fixtures
adapt canary --live [name]             live mode (secrets required, see CANARY)
adapt diff --contract <n> --fixture <f.json> --kind items|comments|users|members
adapt snapshot --accept <name>         after a repair: regenerate fixture from
                                       the new captured payload (review as diff)
```

## Adaptation loop (agent workflow — see adapt/playbook/AGENTS.md)

1. **Detect** — scheduled canary (workflow canary.yml) or a live-run failure
   flags a contract; the drift report pinpoints the failing binding path.
2. **Diagnose** — capture fresh traffic/fixture, run `adapt diff`; classify:
   endpoint moved | params changed | schema changed | auth/signature gate.
3. **Patch** — edit the contract JSON (new `version`), add/replace the
   fixture, add a canary entry that asserts the previously-failing path.
4. **Verify** — `make adapt-offline` locally + CI; both the new fixture and
   the OLD fixture (kept) must remain describable by the versioned contract
   family — regressions are visible as diffs, never silent.
5. **Release** — merge; consumers pin the contract version; the daemon/CLI
   read contracts from the repo state at build/runtime.

## Contract versioning rules

- Breaking binding/path change ⇒ bump `version` and keep the old contract
  file as `<name>-vN.json` (renaming is a C1-visible change; cite ADR).
- Additive field mapping ⇒ same version, new line in `binding.fields`.
- Removed endpoints ⇒ contract gets `"deprecated": true` + `"superseded_by"`.

## Fixture policy

- Fixtures are synthetic goldens: structure-equivalent to public API shapes,
  never real PII, never credentials, gitleaks-clean (hygiene gate scans the
  full history).
- `adapt snapshot --accept` is the only sanctioned way to regenerate a
  fixture from captured traffic, and it prints a schema diff first.

## Machine-readable guarantees

- A canary is green only if: contract parses against the internal schema,
  fixture exists, every binding path resolves in the fixture document
  (`contracts.Diff`), and the canary's `expect` keys are present.
- Drift reports are JSON (`adapt/reports/*.json`) with severity/error codes —
  downstream agents act on codes, not prose.