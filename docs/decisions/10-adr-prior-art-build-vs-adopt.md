# ADR 10: Prior art — build new, harvest claude-memsync's proven designs

- **Status:** Accepted
- **Date:** 2026-07-07
- **Deciders:** Sawmon (buy-vs-build rule applied at whole-product level)
- **Related:** ADR 02 (provider scope), ADR 03 (merge policy), ADR 06 (encryption)

## Context

The buy-first rule applies to the product itself: if a maintained tool already syncs
AI-agent memory across machines, adopt or extend it instead of building. Ecosystem
scan (2026-07-07):

- **claude-memsync** (MarimerLLC/claude-utils, Go, MIT, v0.1.8 2026-05-09, ~6★): the
  architectural twin — fsnotify daemon (3s debounce) mirroring
  `~/.claude/projects/<hash>/memory/` into a git work-tree, private GitHub repo as
  transport, custom merge driver that unions MEMORY.md section blocks, and a per-PC
  `manifest.json` distinguishing deleted-vs-incoming files. **No encryption, no
  Codex, pre-1.0, near-zero adoption.**
- **claude-brain** (toroleapinc, 67★, v0.2.0, active): SessionStart/End hooks + git +
  LLM-powered semantic merge, optional age encryption, secret-stripping. Hook-scoped
  (no daemon/watching), global rather than per-project focus, ongoing API cost for
  merges.
- **claude-sync** (renefichtmueller, 19★): multi-backend sync of the **global**
  `~/.claude`, not per-project memory.
- Chezmoi+age blog pattern (arun.blog): our current stack's twin — commands/hooks
  only, not memory; documents the same pains (manual key distribution, non-diffable
  ciphertext) we are designing away.
- **Codex:** no native cross-machine sync (openai/codex#19307 closed); third-party is
  MCP-server-shaped. **Claude Code first-party:** cross-device memory sync not planned
  (#35985, #38519 closed as not-planned; #25739 open).

## Options considered

1. **Build agent-brain v2; study and harvest prior art (chosen).**
2. **Adopt/extend claude-memsync** — closest core, but lacks encryption (a hard
   requirement, ADR 00), lacks multi-provider (ADR 02), is pre-1.0 with ~6 stars
   (fails the "well maintained and liked" bar), and its union-merge of MEMORY.md
   conflicts with ADR 03's regenerate-the-index policy.
3. **Adopt claude-brain** — hook lifecycle conflicts with ADR 04 (resident daemon,
   continuous watching), LLM-merge conflicts with ADR 03 (deterministic merges,
   no per-sync API cost).

## Decision

Option 1. The combination we need — deterministic encryption + per-project memory +
Codex + daemon lifecycle + cross-OS — is genuinely unoccupied. Harvest list:

- **claude-memsync:** the per-machine manifest design (disambiguating "deleted here"
  vs "new from remote" — a sync-correctness problem we would otherwise re-derive), and
  its merge-driver experience as a reference implementation (MIT; attribute if code is
  referenced).
- **claude-brain:** the secret-stripping idea, noted as a candidate future feature
  (scan memory content for credential patterns before push).

## Consequences

- Differentiation is documented; if claude-memsync matures dramatically, revisit.
- The design doc's sync-engine section must include the per-machine manifest.

## Buy vs build

Whole-product **build**, justified against the buy-first rule: every candidate fails
at least one hard requirement, and none meets the "well maintained and liked" bar
(6–67 stars, pre-1.0).

## Sources

Research delegated to a parallel research team (accessed 2026-07-07); links below are
the sources cited in its Topic G report.

- https://github.com/MarimerLLC/claude-utils
- https://blog.lhotka.net/2026/05/08/Claude-Memory-Sync
- https://github.com/toroleapinc/claude-brain
- claude-sync (renefichtmueller) — repo URL not captured in the research report
- https://www.arun.blog/sync-claude-code-with-chezmoi-and-age/
- https://github.com/openai/codex/issues/19307
- https://github.com/anthropics/claude-code/issues/35985,
  https://github.com/anthropics/claude-code/issues/38519,
  https://github.com/anthropics/claude-code/issues/25739
- Note: the researcher's claim that Claude Code memory is keyed to the absolute
  filesystem path is superseded by direct doc verification (slug derived from the git
  repository as of July 2026; see ADR 02 sources) — retained here for the audit trail.
