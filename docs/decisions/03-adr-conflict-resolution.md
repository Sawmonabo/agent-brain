# ADR 03: Conflict resolution — git merge + retain-both, per file class

- **Status:** Accepted
- **Date:** 2026-07-07
- **Deciders:** Sawmon (options researched and framed by Claude)
- **Related:** ADR 00 (requires the plaintext merge driver), ADR 02 (Codex file classes)

## Context

The core pain driving the rebuild: concurrent sessions (same project, or two machines)
force whole-file overwrites in the current system, sometimes silently undoing the most
recent changes. Requirements: an accepted memory change is never silently lost, and
sync never stalls waiting for a human. Constraints: the writers (Claude Code, Codex)
write plain files directly — this tool does not own the write path; files are small
markdown, contention is low, and git is already in the stack.

## Research (production-system evidence, gathered 2026-07-07)

- **Syncthing:** newest-modification-wins, but the loser is never destroyed — it becomes
  a propagated `*.sync-conflict-<date>-<host>` copy. (docs.syncthing.net/users/syncing.html)
- **Jujutsu:** conflicts are first-class objects that live in history without blocking
  work; resolution is deferrable. (docs.jj-vcs.dev/latest/conflicts/)
- **CRDTs (Automerge):** merges never fail, but markdown does not preserve intent under
  concurrent merges (Ink & Switch, Peritext), state grows with operation history, and
  the guarantees assume the CRDT owns the write path — ours doesn't; we would diff
  files into synthetic ops, forfeiting the theory while keeping the complexity.
- **Atuin:** conflict-freedom via append-only records — again only possible by owning
  the write path (shell hook). Same disqualifier.

## Options considered

1. **Git three-way merge + per-file-class policy + inline retain-both (chosen).**
2. **Syncthing model verbatim** — no merge logic at all; but conflict siblings pile up
   outside `MEMORY.md`'s index where no agent session reads or cleans them, and cleanly
   mergeable cases needlessly produce copies.
3. **CRDT engine** — disqualified per research above.

## Decision

Per file class:

| File class | Policy |
|---|---|
| Memory fact files (append-mostly markdown) | Plaintext three-way git merge; clean majority merges silently |
| Derived indexes (`MEMORY.md`) | **Regenerated deterministically from fact files — never merged** (build-artifact principle; eliminates the hottest conflict source) |
| Provider-regenerated artifacts (Codex summaries/rollouts) | Newest-wins — they are rebuildable by the provider |
| True overlapping edits | **Both versions retained inline** under clear markers; sync continues |

Same-machine concurrency is eliminated by construction: the daemon is the single
serialized writer for all local git operations, so concurrent local sessions cannot
race the repo.

Inline retention (not sibling conflict files) is deliberate: the file's consumer is an
LLM, so the next session reads the marked duplicates and tidies them naturally —
self-healing. A sibling `.conflict` file would sit unindexed and rot.

## Consequences

- The "undo my recent changes" failure mode becomes impossible; worst case is a
  temporarily duplicated fact.
- No user-blocking conflict prompts, ever; the dashboard may *surface* retained
  conflicts passively.
- Hard dependency on ADR 00's custom merge driver (merges must see plaintext).
- MEMORY.md regeneration must be deterministic and provider-format-aware.

## Buy vs build

Merging leverages git itself (merge machinery, custom merge driver hook) rather than
any bespoke merge engine. The merge-driver mechanism choice lands with the ADR 00
follow-up.

## Sources

Primary (load-bearing for the decision):

- Syncthing conflict handling: https://docs.syncthing.net/users/syncing.html
- Jujutsu first-class conflicts: https://docs.jj-vcs.dev/latest/conflicts/
- Peritext — markdown loses intent under concurrent edits: https://www.inkandswitch.com/peritext/
- Automerge: https://automerge.org/
- Atuin: https://github.com/atuinsh/atuin

Search trail (WebSearch, 2026-07-07 — full result sets reviewed per query):

Query: `CRDT vs git three-way merge syncing markdown notes across machines best practice 2026`
- https://udata.company/blog/future-of-version-control-crdt-git-2026
- https://www.inkandswitch.com/peritext/
- https://en.wikipedia.org/wiki/Conflict-free_replicated_data_type
- https://automerge.org/docs/hello/
- https://news.ycombinator.com/item?id=24619103
- https://posit-dev.github.io/automerge-r/articles/crdt-concepts.html
- https://crdt.tech/implementations
- https://vlcn.io/blog/intro-to-crdts
- https://www.iankduncan.com/engineering/2025-11-27-crdt-dictionary/
- https://automerge.org/

Query: `jujutsu jj first-class conflicts merge design why better`
- https://ofcr.se/jujutsu-merge-workflow
- https://dev.to/nyctef/automatically-resolve-formatting-conflicts-with-jj-fix-b92
- https://news.ycombinator.com/item?id=41895702
- https://isaaccorbrey.com/notes/jujutsu-megamerges-for-fun-and-profit
- https://v5.chriskrycho.com/essays/jj-init/
- https://www.kunalganglani.com/blog/jujutsu-jj-git-version-control
- https://dev.to/kunal_d6a8fea2309e1571ee7/jujutsu-jj-the-git-compatible-version-control-tool-that-might-actually-fix-gits-worst-problems-4m3c
- https://docs.jj-vcs.dev/latest/conflicts/
- https://steveklabnik.github.io/jujutsu-tutorial/branching-merging-and-conflicts/conflicts.html
- https://github.com/rafikdraoui/jj-diffconflicts

Query: `syncthing conflict resolution sync-conflict file design last writer wins`
- https://docs.syncthing.net/users/syncing.html
- https://forum.syncthing.net/t/how-does-conflict-resolution-work/15113
- https://forum.syncthing.net/t/cause-and-can-i-delete-sync-conflict-files/22605
- https://blog.mavnn.eu/2025/08/15/conflict_free_syncthing_notes.html
- https://forum.syncthing.net/t/how-to-resolve-syncthing-conflict/20584
- https://github.com/syncthing/syncthing/issues/9280
- https://forum.syncthing.net/t/sync-conflicts/14400
- https://docs.syncthing.net/users/faq.html
- https://forum.syncthing.net/t/better-conflict-resolution-in-ui/23689

Query: `atuin sync record store append-only conflict-free design shell history`
- https://jpk.io/dev-tools/atuin-shell-history-review/
- https://how2.sh/posts/how-to-search-shell-history-with-atuin/
- https://www.x-cmd.com/pkg/atuin/
- https://www.commandinline.com/atuin-shell-history-sync/
- https://crates.io/crates/atuin-history
- https://medium.com/design-bootcamp/sync-search-and-backup-shell-history-with-atuin-ab2bbcd38e79
- https://sumguy.com/atuin-shell-history-sync/
- https://github.com/atuinsh/atuin
