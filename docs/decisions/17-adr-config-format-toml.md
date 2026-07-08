# ADR 17: Configuration format — TOML via pelletier/go-toml/v2

- **Status:** Accepted
- **Date:** 2026-07-07
- **Deciders:** Sawmon (TOML adopted within approved design Section 3; ADR recorded to close the per-decision gap)
- **Related:** Section 3 (projects.toml registry, config.toml), ADR 05 (CLI stack)

## Context

Section 3 of the approved design uses TOML in two places: the user-editable
`~/.config/agent-brain/config.toml` and the shared plaintext registry
`.agent-brain/projects.toml` inside the memories repo. Precedent alignment: Codex
configures via `~/.codex/config.toml`, and the gh/chezmoi ecosystem the user already
lives in is TOML/INI-shaped. YAML (indentation footguns, implicit typing) and JSON
(no comments) were passed over for human-edited config.

## Decision

**TOML**, parsed/emitted with **pelletier/go-toml/v2** — the actively maintained Go
TOML library (v1.0.0-spec compliant, encoding/json-like API, ~2k importing projects,
release as recent as 2026-04; BurntSushi/toml is described as no longer supported,
with users migrating off it).

Known limitation, designed around: go-toml/v2 does not preserve comments on
round-trip (training-knowledge claim — verify at implementation). Therefore:
`config.toml` is **read-only** to the program (only the user edits it; `init` writes
it once from a template), and `projects.toml` is **machine-owned** (written
programmatically, kept comment-free). No file is both user-commented and
program-rewritten, so the limitation never bites.

## Consequences

- One config language across user config, repo registry, and the Codex adapter's
  doctor checks (it reads `~/.codex/config.toml` with the same parser).
- Manifests stay JSON (`manifests/<host>.json`, ADR 03/Section 3) — machine-only
  state where jq-ability and stdlib encoding/json suffice; no format churn there.

## Buy vs build

Buy: pelletier/go-toml/v2. No custom parsing anywhere.

## Sources

Search trail (WebSearch, 2026-07-07), query: `Go TOML library 2026 pelletier go-toml
v2 BurntSushi maintained comparison`

- https://github.com/pelletier/go-toml
- https://pkg.go.dev/github.com/pelletier/go-toml/v2
- https://github.com/pelletier/go-toml/discussions/506
- https://github.com/pelletier/go-toml/discussions/471
- https://pkg.go.dev/github.com/BurntSushi/toml (unsupported per go-toml README claim)
- https://packages.debian.org/sid/golang-github-pelletier-go-toml.v2-dev
- https://github.com/pelletier/go-toml/blob/v2/README.md
- https://github.com/pelletier/go-toml/tree/v2.0.6
- https://github.com/pelletier/go-toml/compare/v2.0.1...v2.0.2
