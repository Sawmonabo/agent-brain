# ADR 01: Project tracking — automatic discovery, deliberate enrollment via interactive CLI

- **Status:** Accepted
- **Date:** 2026-07-07
- **Deciders:** Sawmon (options researched and framed by Claude)
- **Related:** ADR 02 (provider scope determines what discovery scans)

## Context

With no wrapper command, the daemon must decide *which* projects' memories sync to
GitHub. This is a real privacy boundary: providers create memory for every project a
user merely opens, and auto-uploading client/work context to a personal GitHub account
is unacceptable. At the same time, per-project setup friction is one of the diseases
this rebuild cures (the bash system's per-project `.claude/settings.local.json` opt-in
plus wrapper invocation).

## Options considered

1. **Auto-track everything + ignore rules** — zero friction, default-open; surprise
   uploads possible until an ignore rule exists.
2. **Explicit opt-in command per project** — default-closed, strongest control;
   re-introduces per-project setup friction.
3. **Allowlist roots** (e.g. auto under `~/dev`, never elsewhere) — middle ground;
   more configuration surface to understand.
4. **Interactive CLI dashboard (chosen, user-specified):** discovery is automatic and
   continuous across all providers; *enrollment* is a deliberate act inside a pretty
   interactive CLI that lists every project with memories and offers per-project
   opt-in/opt-out, bulk opt-in-all, untrack (with optional purge from the repo), and
   force-sync actions.

## Decision

Option 4 — default-closed enrollment with a discovery UI that makes opting in a single
keystroke. The CLI is a first-class product surface, not an afterthought admin tool.

## Consequences

- New projects never sync silently; they surface as "discovered, untracked" in the
  dashboard.
- A future "track all current and future" toggle can opt into default-open behavior
  without changing the architecture.
- The daemon must maintain a registry of discovered projects (per provider) separate
  from the enrolled set.
- TUI stack selection is a buy-vs-build decision handled in a follow-up ADR (research
  in progress on the mid-2026 Go TUI ecosystem).

## Buy vs build

The interactive UI will be built on an existing, well-maintained Go TUI stack rather
than hand-rolled terminal handling; candidate selection (charmbracelet ecosystem vs
alternatives) deferred to the follow-up ADR with maintenance/adoption evidence.

## Sources

Research trail: no websearch was conducted for this decision — the enrollment model is
a user-specified product preference (privacy boundary + UX), not a technology-selection
question. The TUI-stack follow-up ADR will carry the full search trail for the
interactive-CLI implementation.
