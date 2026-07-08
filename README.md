# agent-brain

Invisible cross-machine sync for AI coding agents' per-project memory (Claude
Code, Codex), through an encrypted private GitHub repo. Single Go binary: a
resident daemon watches providers' native memory directories and syncs
continuously; a CLI (`agent-brain`) is the management surface.

**Status: v2 rebuild in progress on `develop`.** The bash/chezmoi/age v1
system lives on `main` until v2 is proven (ADR 11).

- Design spec: [docs/00-design-spec.md](docs/00-design-spec.md)
- Decisions (ADRs): [docs/decisions/](docs/decisions/)
- Plans: [docs/plans/](docs/plans/)
