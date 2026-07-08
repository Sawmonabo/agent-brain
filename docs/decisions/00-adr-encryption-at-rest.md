# ADR 00: Encryption at rest — transparent deterministic git-filter crypto

- **Status:** Accepted
- **Date:** 2026-07-07
- **Deciders:** Sawmon (options researched and framed by Claude)
- **Related:** ADR 03 (conflict resolution requires plaintext merges)

## Context

Memories sync through a private GitHub repo (`agent-brain-memories`). The current bash
system encrypts with chezmoi + age before commit, which has proven hostile in practice:
the working tree is ciphertext-only (no diffs, no hand-edits, no tooling), age encryption
is nondeterministic (fresh nonce per encryption), so unchanged plaintext still produces
changed ciphertext — defeating change detection — and git-level merging is impossible,
forcing whole-file conflict resolution. Whole-file overwrite is exactly the data-loss
failure mode driving this rebuild. The rebuild must keep memory contents unreadable to
GitHub while making the local working copy behave like ordinary markdown.

## Options considered

1. **Transparent clean/smudge git-filter encryption, deterministic (git-crypt model)** —
   working copy is plaintext; git stores ciphertext; same plaintext always produces the
   same ciphertext, so unchanged files produce no spurious diffs.
2. **Plaintext in a private repo** — trust GitHub repo ACLs; simplest possible sync;
   memories readable by anyone with repo access or a leaked token.
3. **Explicit age encryption before commit (status quo)** — proven, but retains every
   pain listed in Context.

## Decision

Option 1. The local tree is plaintext (diff/merge/edit/tooling all natural); GitHub only
ever stores ciphertext via a deterministic clean/smudge filter; one shared symmetric key
across hosts (same operational model as today's shared age identity).

## Consequences

- Three-way merges can run on plaintext — but git merges the *clean* (stored) form by
  default, so a **custom merge driver** (decrypt → merge → re-encrypt) is required.
  This is a hard dependency of ADR 03.
- Deterministic encryption leaks file-equality metadata (same content → same ciphertext,
  observable across history). Accepted trade-off, identical to git-crypt's.
- Key loss makes the repo unreadable. Mitigation: carry forward the recovery-key pattern
  from the current installer.
- Filenames and repo structure remain visible (as today).

## Buy vs build

Deferred to a follow-up ADR. The *model* is decided here; the *mechanism* (git-crypt
binary vs transcrypt vs git-agecrypt vs an in-binary Go filter using a deterministic
AEAD such as AES-GCM-SIV) is under active research, including each candidate's
maintenance health and — critically — its merge-driver story.

## Sources

- Current-system pain: `docs/00-design-spec.md` (age nonce churn, ciphertext-only tree)
- git-crypt deterministic-encryption design rationale (same-plaintext → same-ciphertext
  to keep git deltas meaningful)

Research trail: no websearch was conducted for this model-level decision (it predates
the research-before-options rule adopted later the same day; the trade-offs draw on the
current system's documented failure modes). The mechanism follow-up ADR — selecting
among git-crypt / transcrypt / git-agecrypt / in-binary Go filter, including
merge-driver behavior — will carry its full search trail.
