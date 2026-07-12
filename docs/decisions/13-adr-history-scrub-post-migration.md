# ADR 13: Scrub this repo's history after verified migration

- **Status:** Accepted (scope amended 2026-07-12 — see "Amendment: full v1 erasure" below)
- **Date:** 2026-07-07
- **Deciders:** Sawmon (directive: once old data is safely in the new memories repo, scrub the code repo of secrets/ciphertext)
- **Related:** ADR 11 (greenfield workflow), design Section 10 (migration)

## Context

After v2, this repo is pure logic — memories live in the dedicated
`agent-brain-memories` repo. But this repo's git history still carries the bash
era's payload: age-encrypted memory blobs (`home/dot_agent-brain/**/*.age`) across
hundreds of `memory: <host> <timestamp>` commits, plus hostname/timing metadata in
those messages. Keeping that history means (a) the age key must be archived forever
to honor "data never lost," and (b) the repo can never comfortably go public.

## Decision

**After migration is verified complete on every machine**, rewrite this repo's
history with **git-filter-repo v2.47.0** (the tool git's own docs recommend over
`filter-branch`; single-pass, refuses to run on non-fresh clones):

1. Fresh clone; `git filter-repo --sensitive-data-removal --invert-paths
   --path home/dot_agent-brain` (plus any other flagged paths). Blob removal empties
   the `memory:` commits and filter-repo prunes them — the hostname/timing metadata
   goes with them.
2. **Verify** with a gitleaks full-history scan (`gitleaks git`) plus manual
   inspection before any push.
3. Force-push; then handle GitHub's server-side retention — cached views and
   unreachable objects can persist after a force-push, so for thoroughness either
   contact GitHub support to run garbage collection or delete-and-recreate the
   (private, forkless, single-user) repo. Delete-and-recreate is the staff pick
   here: simplest and total.
4. **Then retire the age key everywhere** — after the scrub nothing it guards
   exists, so the "archive the age key forever" obligation in the retirement
   checklist collapses to "keep until scrub completes."

Sequencing gate: scrub only after (a) v2 merged to main, (b) `migrate` run and
verified on every machine, (c) a local archive copy of the pre-scrub repo exists as
a belt-and-suspenders fallback until the first post-scrub weeks pass.

Going public afterward becomes a zero-cost option, not a commitment.

## Amendment: full v1 erasure (2026-07-12, Sawmon — reverses the memories-only scope)

The original decision removed only the memory-store paths, leaving the bash-era
*code* commits visible in history. Sawmon's stated goal for the cutover is
stronger: the published history should look like v1 never existed, with v2.0.0
as the repo's first release. The scrub scope is therefore extended:

1. **Graft the greenfield-reset commit as the new root.** In the fresh scrub
   clone: `git replace --graft <greenfield-reset-sha>` (parentless), then the
   same `git filter-repo` run bakes the graft into real history. Everything
   before the reset — every v1 code commit and all ~48 `memory: <host>` flush
   commits — ceases to exist; v2's full engineering history is preserved.
   Keep `--sensitive-data-removal --invert-paths --path home/dot_agent-brain`
   in the same run as belt-and-braces against any stray blob. Optional at
   execution: rewrite the root commit's message (filter-repo message
   callback) so it does not reference the system it replaced.
2. **The develop→main merge is dropped.** Merging would tie the erased v1
   graph back into main. Instead the scrubbed develop line simply becomes
   `main` on the recreated repo. ADR 11's *gate* (merge only when v2
   demonstrably works end-to-end) is unchanged — the cutover remains that
   demonstration; only the vehicle changes from merge commit to
   branch replacement. Nothing v2-valuable lives only on old main (its
   unique commits are all bash-era operations, which this amendment erases).
3. **Old tags are not re-pushed.** The rc tags reference pre-scrub history;
   the fresh instance starts tagless and `v2.0.0` becomes its first tag and
   first release.
4. **Verification gains one check:** zero commits older than the new root
   (`git log --all --reverse | head` shows the greenfield commit as root),
   alongside the existing gitleaks `--log-opts=--all`, empty-path, and
   zero-`memory:`-subject checks — all before any push.
5. **Local backups are deleted last, not at scrub time:** the quarantine
   folder (`~/.agent-brain-quarantine-20260710`) and the legacy checkout
   (`~/dev/agent-brain-legacy`) outlive the scrub and the mirror archive's
   retention window, and go only after fleet-wide migration verification
   plus a post-scrub soak. The pre-audit-cleanup tarball inside the
   quarantine is the only copy of the deliberately-pruned memories — its
   deletion is a separate, deliberate decision (vault-stash or forget).

Author identity (name/email) on v2 commits is deliberately retained — it is
the chosen public git identity; a mailmap rewrite remains possible at
execution time if that changes.

## Consequences

- All commit SHAs change; every clone must be re-cloned (trivial: one user).
- ~~Development history (ADRs, code) survives — only memory payload paths are
  removed.~~ Superseded by the 2026-07-12 amendment: v2 development history
  survives; all pre-greenfield (v1) history is erased.
- Supersedes Section 10's earlier "post-v1 options (a)/(b)/(c)" framing: option (c)
  scrub is chosen; fresh-repo (b) unnecessary.

## Buy vs build

Buy entirely: git-filter-repo (scrub) + gitleaks (verification). No custom tooling.

## Sources

Search trail (WebSearch, 2026-07-07), query: `git-filter-repo latest version 2026
remove sensitive files git history rewrite best practice GitHub`

- https://github.com/newren/git-filter-repo (v2.47.0; --sensitive-data-removal,
  --invert-paths workflow; fresh-clone safety)
- https://github.com/newren/git-filter-repo/releases
- https://www.git-tower.com/learn/git/faq/git-filter-repo
- https://git-scm.com/docs/git-filter-branch (deprecation pointer to filter-repo)
- https://www.mankier.com/1/git-filter-repo
- https://github.com/newren/git-filter-repo/blob/main/INSTALL.md
- https://manpages.debian.org/testing/git-filter-repo/git-filter-repo.1.en.html
- https://repology.org/project/git-filter-repo/versions

gitleaks verification tooling: see ADR 14 sources.
