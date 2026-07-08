# CLAUDE.md

Guidance for Claude Code sessions in this repository.

## What this repo is

agent-brain v2: a single Go binary that syncs AI coding agents' per-project
memory across machines through an encrypted private GitHub repo
(`agent-brain-memories`). A resident user daemon watches provider-native
memory paths (no wrapper command), a single-goroutine sync engine is the only
writer, and git filters encrypt content transparently (Tink AES-SIV).

`docs/00-design-spec.md` is the canonical spec — section references (§4, §5…)
appear in code comments and are load-bearing. Every design decision has an ADR
in `docs/decisions/`. The active implementation plan is in `docs/plans/`.

**Branch discipline (ADR 11):** all v2 work happens on `develop`. `main`
still holds the retired bash-era system until v2 merges; never commit there.

## Commands

```bash
go build ./...                                  # build
go test ./...                                   # all tests
go test ./internal/crypto/ -run TestName -v     # one test
go test ./... -race                             # what pre-push runs
go test ./internal/crypto/ -fuzz FuzzRoundtrip -fuzztime 30s  # fuzz (-fuzz takes exactly ONE package)
golangci-lint run                               # lint (config: .golangci.yml)
gofumpt -l -w .                                 # format
lefthook install                                # once per clone: git hooks
```

## Conventions

- Go 1.26, `toolchain go1.26.5` — Go auto-downloads the pinned toolchain.
- Everything under `internal/`; `cmd/agent-brain` stays thin.
- Package boundaries (spec §8): `engine` depends on `gitx`/`crypto`/
  `provider`/`repo` interfaces — never on `cli` or `daemon`. `daemon/api`
  types are the only daemon↔CLI shared surface.
- Tests: stdlib `testing` + `go-cmp` ONLY (no assertion frameworks, ADR 15);
  table-driven; `t.Parallel()`; `t.TempDir()`; integration tests use real
  system git with a `git init --bare` fake remote.
- Conventional Commits. Lint/format enforced by lefthook + CI.
- Safety: the Tink keyset (`~/.config/agent-brain/keyset.json`) never enters
  any repo. Plaintext memory content must never reach a git object — the
  integration suite asserts ciphertext on the wire.
