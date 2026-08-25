# HARDENING — delivery/packaging hardening specification

This document is the contract for the future delivery packaging pipeline. It
specifies the required protections, the mechanism for each, and the
verification gate. The repository ships source; the hardened build is
produced by the delivery pipeline implementing this spec. Nothing here
relies on obscurity as the only defense; the layered model is: make static
extraction useless, make logic exclusion the norm, make cloning/repacking
expensive and traceable, and measure all of it in CI.

## Threat model (scope)

- Attacker with the shipped binary + full OS-level access (can debug, dump
  memory, intercept its own traffic).
- Goal state, in order of importance:
  1. shipped artifact contains no readable behavioral contracts and no
     signing algorithms (logic excluded by design);
  2. an extracted/copied artifact is not practically reusable (server-bound);
  3. static extraction yields < 5% of the contract/signature surface
     (measured);
  4. leaks are attributable (per-build watermark).

Note on the un-protectable part: runtime behavior (what hosts are contacted,
that collection happens) is observable by definition. The hardening target
is logic secrecy + clone cost + traceability, not invisibility.

## Required controls (numbered; the pipeline must implement M1-M8)

- **M1 static extraction resistance** — Go binaries built with the
  supported obfuscating build (`garble -literals -tiny`), `-ldflags "-s -w"`,
  no debug symbols, no plaintext contract assets embedded（contracts loaded
  from the encrypted envelope below）.
- **M2 contract envelope** — `adapt/contracts` and flow scripts shipped as
  AES-256-GCM envelopes; key derived at first run from a license-bound
  secret exchanged with the license server (never stored on disk unsealed
  beyond process memory).
- **M3 logic exclusion** — signing/token bootstrap（signature computation,
  room handshake parameters, IP pool rotation）run server-side. Client
  receives only short-TTL signed request material bound to device+account.
- **M4 replay protection** — every server-issued signed material carries
  TTL + nonce + recipient binding; server rejects reuse; device-side rate
  envelope as second layer.
- **M5 licensing & watermarking** — Ed25519-signed license payloads bound
  to a machine fingerprint; each delivered build embeds a unique watermark
  derived from the licensee id（binary-level, survives re-obfuscation）so
  leaked copies trace to their source.
- **M6 update channel** — signed update manifests (Ed25519), rollback
  protection via version floor, update artifacts encrypted with per-version
  keys rotated on compromise.
- **M7 anti-analysis tripwires** — runtime presence checks for common
  debugger/procmon-class tools (deny by default to *degrade service*, never
  to self-destruct destructively), self-integrity checksum reported to the
  server for attestation (tamper ⇒ license revoke escalation).
- **M8 red-team scoring gate** — `.github/workflows/redteam.yml` executes
  the audit battery (binary leakage scan: strings/entropy sweep for
  contract keywords; envelope-secret search; naive unpack attempts) and
  publishes a hardening scorecard. Delivery pipeline treats the scorecard
  as a release gate, with the thresholds defined in this file's parent
  policy.

## Audit battery (implemented by redteam.yml)

1. static string sweep for declared contract endpoints/signature param names;
2. asset scan: unsealed contract JSON / PEM / key material in the artifact;
3. symbol/table scan (go tool nm) for exported sensitive symbols;
4. envelope-decryption attempt with empty/malformed keys (must fail closed);
5. replay probe against the recorded server exchange (must be rejected).

## Release gate

A delivery build is releasable only when: M1-M7 implemented, M8 scorecard
green, the license server + signer service are reachable, and an offline DR
run of that exact artifact succeeded. The pipeline itself（build machines,
signing keys, envelope keys）lives in deployment infrastructure, outside
this repository; this file is its interface spec.