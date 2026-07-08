# ADR 02: Provider scope for v1 — Claude Code + Codex adapters

- **Status:** Accepted
- **Date:** 2026-07-07
- **Deciders:** Sawmon (options researched and framed by Claude)
- **Related:** ADR 01 (discovery), ADR 03 (per-file-class merge policy)

## Context

The rebuild must sync memories across providers, not just Claude Code. Research
(2026-07-07, official docs) established that provider memory models differ
fundamentally:

- **Claude Code:** per-project memory at `~/.claude/projects/<slug>/memory/`
  (`MEMORY.md` + topic files, plain markdown), zero-config since v2.1.59; slug derived
  from the git repository (worktrees share memory); machine-local.
- **OpenAI Codex:** real persistent memory ("Memories" + Chronicle extension), plain
  markdown — but **user-global** under `~/.codex/memories/`, no per-project keying at
  all; background consolidation *regenerates* summary files; enabled via
  `features.memories = true` in `~/.codex/config.toml`; machine-local; storage
  relocatable only via `CODEX_HOME`.
- **Gemini CLI:** tiered memory since v0.40.0 (April 2026) including a per-project
  private memory folder (exact path partially unverified).
- **GitHub Copilot:** memory is cloud/account-scoped, synced server-side by GitHub —
  no local file to sync.

## Options considered

1. **Claude Code + Codex in v1 (chosen)** — the adapter interface is proven against
   the two most *different* real topologies (per-project vs user-global) from day one.
2. **Claude Code first, Codex in v1.1** — ships sooner; risks baking Claude-shaped
   assumptions into the abstraction.
3. **Wider net (also Gemini, others)** — broadest coverage, slowest v1, some targets
   only partially documented.

## Decision

Option 1. Claude Code is the reference adapter; the Codex adapter ships in v1 and may
carry an "experimental" label if its (partly third-party-documented) on-disk layout
proves unstable. Copilot is explicitly out of scope — GitHub already syncs it.
Gemini CLI is the first post-v1 adapter candidate.

## Consequences

- The adapter interface must model **memory scope** (per-project vs user-global) as a
  first-class property, not assume project keying.
- Codex memories map to a dedicated global space in the sync repo (working name
  `_codex-global/`) rather than per-project folders.
- Codex's regenerated summaries need class-aware merge policy (see ADR 03):
  newest-wins is *correct* for rebuildable artifacts.
- Both providers use a file literally named `MEMORY.md` for different things — all
  logic keys on full paths, never basenames.

## Buy vs build

n/a at this level (scope decision). Per-adapter parsing/watching libraries go through
buy-vs-build in implementation ADRs.

## Sources

All links below were gathered and reviewed via delegated web research (WebSearch +
WebFetch, accessed 2026-07-07).

Primary (load-bearing for the decision):

- Claude Code memory (official): https://code.claude.com/docs/en/memory
- Codex Memories (official): https://developers.openai.com/codex/memories
- Codex Chronicle (official): https://developers.openai.com/codex/memories/chronicle
- Codex config reference (official): https://developers.openai.com/codex/config-reference
- Codex changelog (official): https://developers.openai.com/codex/changelog

Investigated — third-party detail and exclusion rationale:

- Codex on-disk file layout (third-party, UNVERIFIED by OpenAI):
  https://mem0.ai/blog/how-memory-works-in-codex-cli
- Codex instruction-layer detail only — source conflates AGENTS.md with Memories, used
  cautiously: https://mer.vin/2025/12/openai-codex-cli-memory-deep-dive/
- Gemini CLI memory tool: https://geminicli.com/docs/tools/memory/
- Gemini CLI v0.40.0 tiered memory:
  https://github.com/google-gemini/gemini-cli/discussions/26216
- Copilot Memory concepts (grounds the Copilot exclusion — cloud-synced by GitHub):
  https://docs.github.com/en/copilot/concepts/agents/copilot-memory
- Copilot Memory CLI/deletion controls changelog:
  https://github.blog/changelog/2026-05-26-copilot-memory-has-more-controls-for-deletion-scope-and-the-copilot-cli/
- Cursor persistent-memory ecosystem (grounds the Cursor exclusion — no first-party
  memory files): https://hindsight.vectorize.io/blog/2026/06/12/cursor-persistent-memory
- Cursor third-party memory bank example: https://github.com/jayzeng/agentmemory
