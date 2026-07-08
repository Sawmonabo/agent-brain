# ADR 08: GitHub provisioning + auth — borrow gh, fall back to device flow

- **Status:** Accepted
- **Date:** 2026-07-07
- **Deciders:** Sawmon (accepted within Approach A / buy-vs-build table)
- **Related:** ADR 04 (first-run wizard drives this)

## Context

agent-brain auto-provisions a private `agent-brain-memories` repo under the user's
GitHub account and needs non-interactive push/pull auth thereafter. Research confirmed
`gh` is authenticated on the primary host with `repo` scope (sufficient to create
private repos and push). The ecosystem pattern: downstream tools either borrow gh's
token or reimplement gh's own OAuth device flow.

## Options considered

1. **Two-tier: borrow gh when present, device flow when absent (chosen).**
   - Happy path: `gh auth token` for programmatic token reuse; `gh repo create <name>
     --private` for provisioning. **Never run `gh auth setup-git`** — it writes the
     absolute path of the gh binary into gitconfig, which breaks a gitconfig synced
     across machines with different install paths (cli/cli discussion #9438) — a
     direct hazard for a chezmoi-managed dotfiles user. Instead inject the token
     ephemerally per operation (env-passed one-shot credential helper, or
     `x-access-token` basic auth).
   - Fallback: our own OAuth **device flow** via `github.com/cli/oauth` v1.2.2 (the
     library gh itself is built on; no client secret needed; ship only the OAuth app
     client_id) → non-expiring `repo`-scoped token → create repo via
     `github.com/google/go-github` v89 (`Repositories.Create` with empty org ⇒
     authenticated user) → store in `github.com/zalando/go-keyring` with a 0600
     plaintext file fallback.
2. **Device flow as the only path** — one code path, but re-solves auth+keyring gh
   already solved for most users, and requires registering/shipping the OAuth app on
   day one.
3. **Fine-grained PAT paste** — best least-privilege, worst UX; kept as the tertiary
   manual/headless path (Administration:write + Contents:write).

## Decision

Option 1 — **amended 2026-07-07 (v1 scope trim, staff-level pushback review): v1
requires gh.** The `init` wizard checks for gh, guiding install + `gh auth login`
when missing; `doctor` verifies it thereafter. The device-flow fallback (and the PAT
tertiary path) are deferred to v1.1 as documented follow-ups — they remain the design
for gh-less machines, just not v1 surface. Rationale: removes an OAuth-app
registration and a chunk of code from v1's riskiest area, and every current host
already runs gh authenticated. OAuth-app tokens being non-expiring (vs GitHub App
user tokens: 8h + refresh) keeps the v1.1 fallback the right shape for "authorize
once, sync forever."

## Consequences

- SSH-remote users: gh's `repo` scope covers HTTPS; when the user's git uses SSH
  remotes (as on the primary host), plain `git push` over SSH already works — token
  injection is only needed for HTTPS remotes.
- The OAuth app (for the fallback) is registered once; its client_id is not a secret.
- Deploy keys/SSH alone cannot *create* repos, so they never satisfy provisioning.

## Buy vs build

**Buy throughout:** gh CLI (subprocess), cli/oauth v1.2.2, go-github v89.0.0,
zalando/go-keyring. Nothing custom beyond glue.

## Sources

Research delegated to a parallel research team (accessed 2026-07-07); links below are
the sources cited in its Topic E report.

- https://cli.github.com/manual/gh_auth_token
- https://cli.github.com/manual/gh_repo_create
- https://github.com/cli/cli/discussions/9438 (setup-git absolute-path hazard)
- https://github.com/cli/oauth
- https://github.com/google/go-github
- https://github.com/zalando/go-keyring
- UNVERIFIED (flagged by researcher): "gh uses zalando/go-keyring" attribution is
  community-sourced (https://github.com/cli/cli/issues/8980); the
  keyring-with-plaintext-fallback *behavior* is confirmed by first-party issues.
