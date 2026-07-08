# ADR 16: Distribution & release — GoReleaser v2, homebrew_casks tap, go install

- **Status:** Accepted
- **Date:** 2026-07-07
- **Deciders:** Sawmon (pending Section 13 approval; recorded on presentation per ADR-per-decision rule)
- **Related:** ADR 12 (CI, SHA-pinned actions), ADR 13 (public-repo option post-scrub), Section 8 (cmd/ layout)

## Context

v2 must install on new machines in minutes (macOS, Linux, WSL2) and stay current.
Verified state 2026-07-07: **GoReleaser v2.16.0** (2026-05-24) is current; its
release notes carry two decision-relevant changes: releases are now published under
an **immutable-releases policy** (tag bytes can never be replaced), and the legacy
`brews` formula config is **fully deprecated in favor of `homebrew_casks`** — casks
are now the sanctioned way to ship pre-compiled binaries via Homebrew.

## Decision

1. **GoReleaser v2.16.0** runs on tag push in GitHub Actions (SHA-pinned, ADR 12):
   darwin/arm64+amd64 and linux/arm64+amd64 archives, checksums file, changelog
   generated from Conventional Commits.
2. **Homebrew via `homebrew_casks`** (not the deprecated `brews`), published to a
   personal tap repo `Sawmonabo/homebrew-tap`:
   `brew install sawmonabo/tap/agent-brain`.
3. **`go install github.com/Sawmonabo/agent-brain/cmd/agent-brain@latest`** works
   by construction (single root module + cmd/ layout, Section 8) — the no-brew
   fallback for Linux/WSL2.
4. **Signing:** checksums + GitHub immutable releases only in v1. cosign /
   provenance attestations are YAGNI for a single-consumer personal tool —
   documented here and revisited if the repo goes public after the ADR 13 scrub.
5. **New-machine onboarding runbook** (target: under 5 minutes):
   brew/go install → `agent-brain init` (wizard: gh auth check → clone
   `agent-brain-memories` → `key import` from password manager → service install →
   enrollment picker) → `agent-brain migrate` if the machine has bash-era state.

## Consequences

- Self-updating is delegated to the package manager (`brew upgrade` /
  `go install @latest`) — no self-update code in the binary (YAGNI; single user).
- The tap repo is one more repo to provision; `init`'s wizard does not manage it —
  it is a release-time artifact only, created once.
- WSL2 uses the Linux binary via `go install` or linuxbrew; no Windows-native
  build is shipped (all targets are POSIX — consistent with ADR 14's renameio
  constraint).

## Buy vs build

Buy entirely: GoReleaser, Homebrew tap mechanism, Dependabot. Build: nothing —
distribution is pure configuration.

## Sources

Search trail (WebSearch, 2026-07-07), query: `GoReleaser v2 latest version 2026
homebrew tap Go binary release`

- https://github.com/goreleaser/goreleaser/releases (v2.16.0, 2026-05-24)
- https://goreleaser.com/blog/goreleaser-v2.16/ (immutable releases; brews →
  homebrew_casks deprecation; dockers_v2 GA)
- https://goreleaser.com/blog/goreleaser-v2/
- https://goreleaser.com/
- https://github.com/goreleaser/goreleaser
- https://pkg.go.dev/github.com/goreleaser/goreleaser/v2
- https://github.com/goreleaser/goreleaser-pro/releases
- https://repology.org/project/goreleaser/versions
- https://goreleaser.com/blog/goreleaser-v2.12/
- https://goreleaser.com/blog/goreleaser-v2.14/
