# ADR 06: Encryption mechanism — in-binary deterministic AEAD filter + custom merge driver

- **Status:** Accepted
- **Date:** 2026-07-07
- **Deciders:** Sawmon (accepted within Approach A / buy-vs-build table)
- **Related:** Follow-up to ADR 00 (model); hard dependency of ADR 03 (plaintext merges)

## Context

ADR 00 chose transparent git-filter encryption; this ADR selects the mechanism. The
governing fact from research: git's three-way merge operates on the stored
(post-clean = **ciphertext**) bytes by default, and a whole-file AEAD blob has no line
structure — so *no* encryption tool makes files mergeable without a **custom merge
driver** that decrypts base/ours/theirs, merges plaintext, and re-encrypts.
Determinism matters for a different reason: a fresh random nonce per `clean` makes
identical plaintext produce different ciphertext per commit and per machine — churn,
spurious whole-file conflicts, broken rename detection. **Convergent encryption**
(synthetic nonce derived from plaintext) yields byte-identical ciphertext on every
machine. Its known cost: an observer learns when two files are identical, plus length
and change timing — acceptable for a single-user private repo (same trade git-crypt
documents).

## Options considered

1. **Bespoke Go clean/smudge + `diff.textconv` + custom merge driver over a
   deterministic AEAD (chosen).** Zero external runtime dependencies; ships inside the
   binary we already distribute; the merge driver delivers the one capability nothing
   else has.
2. **git-crypt 0.8.0** — deterministic (AES-256-CTR, synthetic IV) but **has no merge
   driver and never has** (PRs #180/#141 open for years; issue #140 documents silent
   overwrite of remote changes on conflict); low velocity (no 2026 commits); external
   C++ binary.
3. **transcrypt v2.3.2** — actively maintained, deterministic, but bash+OpenSSL
   subprocess and still no merge driver.
4. **SOPS v3.13.2 + age v1.3.1** — very healthy (CNCF), but built for structured
   secrets: freeform markdown falls to binary mode (JSON/YAML envelope — the working
   file is not readable markdown, breaking transparency) and encryption is
   non-deterministic.
5. **git-agecrypt** — stabilizes ciphertext via a local cache in `.git/` that is never
   pushed, so two machines produce *different* ciphertext for identical plaintext —
   defeats the multi-machine requirement outright.

## Decision

Option 1. Construction (**amended 2026-07-07** after staff-level pushback review):
**Google Tink's Deterministic AEAD — AES-SIV, RFC 5297** (`tink-crypto/tink-go`,
`daead` primitive) — deterministic and misuse-resistant *by design*, maintained by
Google, purpose-built API for exactly this use (Tink deliberately blocks
caller-controlled nonces on its regular AEADs, steering deterministic encryption to
`daead`). No hand-composed crypto anywhere in the codebase. Context: AES-GCM-SIV is
not in Go std/x-crypto (golang/go#54364 open), and the initially-picked leaner
alternative — XChaCha20-Poly1305 (`x/crypto`) with a keyed-hash synthetic nonce
(git-crypt's SIV pattern, modernized) — was flipped because hand-composing crypto is
precisely what a staff security review rejects when a standardized construction
exists. **Review gate (retained, now a formality):** key handling, key separation,
and pinned RFC 5297 test vectors verified before v1.

Git wiring (installed by `init`/`doctor` on every machine and after every clone, since
`.git/config` is not versioned): `*.md filter=agentbrain diff=agentbrain
merge=agentbrain` in `.gitattributes`; `filter.agentbrain.clean/smudge` (ciphertext in
repo, plaintext in worktree); `diff.agentbrain.textconv` (readable diffs/logs);
`merge.agentbrain.driver = agent-brain git-merge %O %A %B %P` (decrypt → `git
merge-file` → re-encrypt; on true conflict, write conflict-marked **plaintext** so
resolution happens on readable markers); `merge.renormalize = true`.

## Consequences

- **System git is the engine** — filters and merge drivers are git features; go-git
  executes neither. All repo operations shell out to git (chezmoi-style exec wrapper).
- A clone without filters installed shows ciphertext — `doctor` detects and repairs;
  the daemon refuses to sync until filters are installed.
- One shared symmetric key across hosts (ADR 00); key distribution/recovery carried
  forward from the current system's model.

## Buy vs build

**Buy the crypto** (tink-go Deterministic AEAD / AES-SIV; the x/crypto synthetic-nonce
composition remains the documented fallback only if the Tink dependency ever becomes
untenable), **build the small filter/driver layer** (~hundreds of lines). Build is
justified under the buy-first rule because every existing tool fails a hard
requirement: no merge driver (git-crypt, transcrypt), not transparent for markdown
(SOPS), broken cross-machine determinism (git-agecrypt).

## Sources

Research delegated to a parallel research team (accessed 2026-07-07); links below are
the sources cited in its Topic C report.

- https://github.com/AGWA/git-crypt/issues/140 (silent overwrite on conflict)
- https://github.com/AGWA/git-crypt/pull/180, https://github.com/AGWA/git-crypt/pull/141
  (merge-driver PRs, open for years)
- https://github.com/elasticdog/transcrypt
- https://github.com/getsops/sops, https://github.com/FiloSottile/age
- https://github.com/vlaci/git-agecrypt (dormant; active fork noted by researcher)
- https://github.com/golang/go/issues/54364 (AES-GCM-SIV not in std/x-crypto)
- https://github.com/tink-crypto/tink-go
- https://github.com/secure-io/siv-go (dormant since 2018 — rejected)

Post-decision verification (2026-07-07, direct doc checks after the Tink amendment):

- tink-go **v2.7.0** (published 2026-06-10) confirmed current with `daead`,
  `daead/aessiv` (RFC 5297), `keyset`, and `insecurecleartextkeyset` packages:
  https://pkg.go.dev/github.com/tink-crypto/tink-go/v2
- Tink officially recommends the **AES256_SIV** key type for deterministic
  encryption: https://developers.google.com/tink/deterministic-encryption
- Plaintext keyset storage is an officially documented workflow
  (https://developers.google.com/tink/generate-plaintext-keyset) via the
  deliberately-scary-named `insecurecleartextkeyset` API — Tink steers server users
  to KMS-wrapped keysets; for a local dev tool with no KMS, a 0600 plaintext keyset
  plus password-manager copy is the accepted posture (same class as age identities
  and git-crypt keys). Documented in the design's threat model.
- git merge drivers receive the **in-repository (post-clean = ciphertext) content**,
  and `filter.<driver>.required = true` is confirmed fail-closed on both add/commit
  (clean failure = not staged) and checkout (smudge failure = checkout fails), while
  the default `required = false` is a silent no-op passthru — exactly the dangerous
  mode this design avoids: https://git-scm.com/docs/gitattributes
- Bonus placeholders for the driver: `%L` (marker size) and `%X`/`%Y` (ours/theirs
  conflict labels) — useful for labeling retain-both blocks.
