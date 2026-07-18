# ADR 11: Development workflow — develop-gated main, greenfield reset

- **Status:** Accepted (transitional `main` arrangement concluded 2026-07-17 — see
  "Amendment: cutover executed" below)
- **Date:** 2026-07-07
- **Deciders:** Sawmon (directive)
- **Related:** ADR 10 (build verdict), Section 10 of the design (migration)

## Context

v2 is a ground-up rebuild in a repo that currently ships the working bash system.
The sole user is the author — there are no external consumers, so backward
compatibility has exactly one obligation: a one-time import of existing memories
into the new structure so nothing is lost.

## Decision

- All v2 work lands on the **`develop`** branch (created 2026-07-07 from `main` at
  62e20a7). `main` continues to represent the working bash system until v2 is
  proven; `develop` merges into `main` only when v2 demonstrably works end-to-end.
- **Greenfield posture on `develop`:** legacy code and files not used by v2 (the
  chezmoi source-state tree `home/`, the bash installer/tests under `tools/`, and
  related scaffolding) are deleted outright rather than kept for compatibility.
  Deleting them is safe because migration does not read the legacy *repo* — the
  `migrate` command imports from the machine-local runtime state
  (`~/.agent-brain/`, already-decrypted plaintext) plus the local slug mappings,
  none of which live in this repository.
- The only backward-compat surface in v2 is `agent-brain migrate`: one-time,
  idempotent, import-only.

## Consequences

- No dual-maintenance window: bash fixes (if any) happen on `main`; v2 never
  carries legacy shims.
- History preserves everything — deletion on `develop` loses nothing that
  `git log`/`main` can't recover.
- The design's migration section shrinks to a single import command plus a
  retirement checklist (uninstall bash artifacts, archive the old sync repo state).

## Buy vs build

n/a (process decision).

## Sources

No websearch conducted — user-directed workflow decision, not a technology
selection. Branch state verified locally (only `main` and a stale
`feat/install-upgrade-path` existed prior).

## Amendment: cutover executed (2026-07-17, Sawmon)

The transitional arrangement above — `main` representing the working bash
system until the Go rebuild proved out — ended at the public launch:

- The Go system demonstrably worked end-to-end, ADR 13's history scrub was
  executed, and `main` was reset to the scrubbed greenfield line
  (`develop` = `main` at cutover). The repo went public the same day, with
  `v1.0.0` as the first tag on the new line.
- **What persists:** the develop-first flow. All work still lands on
  `develop`; `main` is the released public line, advanced only by
  fast-forward from `develop`. Direct commits to `main` remain forbidden.
- **Superseded consequence:** "History preserves everything — deletion on
  `develop` loses nothing that `git log`/`main` can't recover" was true
  during the transition but is deliberately false since the scrub: the bash
  era is no longer recoverable from this repository's history (that was the
  point — ADR 13 records what was erased and why).
