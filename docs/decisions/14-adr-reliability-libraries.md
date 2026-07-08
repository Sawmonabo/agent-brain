# ADR 14: Reliability & hygiene library choices — backoff, atomic writes, logging, secret scanning

- **Status:** Accepted
- **Date:** 2026-07-07
- **Deciders:** Sawmon (buy-vs-build standing rule); specifics researched after Section 11 challenge
- **Related:** ADR 12 (engineering standards), ADR 13 (scrub verification), design Section 11

## Context

Design Section 11 (failure modes) implies several small but load-bearing mechanisms:
retry-with-backoff for push/pull cycles, atomic file replacement for mirror-out,
structured daemon logging, and secret detection (memory-content risk + ADR 13 scrub
verification). Each is a classic wheel — the buy-vs-build rule requires checking the
shelf before rolling any of them. Dedicated websearch conducted 2026-07-07.

## Decision

| Concern | Choice | Verified state |
|---|---|---|
| Retry/backoff | **cenkalti/backoff/v5** (v5.0.3) | THE Go exponential-backoff library; context-aware `Retry`; `backoff.Permanent(err)` for non-retriable errors; `RetryAfter` for rate-limit style waits; note `WithMaxElapsedTime` defaults to 15 min — set explicitly per loop |
| Atomic file writes (mirror-out) | **google/renameio/v2** | Temp-file + rename atomic replace; deliberately exports nothing on native Windows — irrelevant here (targets are macOS/Linux/WSL2, all POSIX). Eyes-open note: feature-complete and quiet (little activity since ~2021); if dependency hygiene ever flags it, the fallback is ~20 lines of stdlib (CreateTemp → write → Sync → Rename → dir fsync) |
| Structured logging | **log/slog** (stdlib) | Standard library since Go 1.21 — the modern default; zero dependency; JSON handler for daemon logs, text for CLI verbose mode |
| Secret scanning | **gitleaks** (v8.30.1) | The de-facto standard (16M+ docker pulls, 17k stars). Three uses: (1) v1.1 memory-content scan before memories-repo commits (the claude-brain harvest, ADR 10); (2) full-history verification of the ADR 13 scrub (`gitleaks git`); (3) ordinary CI hygiene on this code repo. Note v8.19.0 deprecated `detect`/`protect` subcommands — use the `git`/`dir`/`stdin` modes |

## Consequences

- Zero hand-rolled retry loops, temp-file dances, or regex secret heuristics in v1
  code; each concern is one well-known dependency (or stdlib).
- gitleaks is a *tool* dependency (CI + scrub verification), not a Go module import,
  until the v1.1 memories-scan feature decides embed-vs-exec.

## Buy vs build

All bought; the only build-adjacent item is the renameio stdlib fallback, documented
above and used only if the dependency is ever retired.

## Sources

Search trail (WebSearch, 2026-07-07):

Query: `Go retry backoff library cenkalti/backoff v5 2026 best practice`
- https://github.com/cenkalti/backoff
- https://pkg.go.dev/github.com/cenkalti/backoff/v5
- https://github.com/cenkalti/backoff/blob/v5/backoff.go
- https://github.com/cenkalti/backoff/blob/v5/example_test.go
- https://github.com/cenkalti/backoff/blob/v5/README.md
- https://github.com/cenkalti/backoff/blob/v5/retry.go
- https://github.com/cenkalti/backoff/tree/v5.0.3
- https://pkg.go.dev/github.com/cenkalti/backoff
- https://chromium.googlesource.com/external/github.com/cenkalti/backoff/+/refs/heads/v5%5E!/
- https://gopkg.in/cenkalti/backoff.v5

Query: `Go atomic file write library renameio maintained 2026`
- https://pkg.go.dev/github.com/google/renameio
- https://github.com/google/renameio
- https://pkg.go.dev/github.com/nater0000/renameio
- https://pkg.go.dev/github.com/sashka/atomicfile
- https://github.com/natefinch/atomic
- https://alexwlchan.net/notes/2026/go-atomicfile/
- https://github.com/zserge/atomicwriter
- https://michael.stapelberg.ch/posts/2017-01-28-golang_atomically_writing/
- https://pkg.go.dev/github.com/facebookgo/atomicfile

Query: `gitleaks latest version 2026 secret scanning git repository pre-commit`
- https://github.com/gitleaks/gitleaks
- https://gitleaks.io/
- https://gitleaks.org/
- https://pickuma.com/for-dev/gitleaks-open-source-secret-scanning-2026/
- https://stevekinney.com/courses/self-testing-ai-agents/secret-scanning-with-gitleaks
- https://oneuptime.com/blog/post/2026-01-25-secret-scanning-gitleaks/view
- https://dev.to/pickuma/gitleaks-open-source-secret-scanning-for-git-repos-in-2026-4ceb
- https://github.com/gitleaks/gitleaks-action
- https://appsecsanta.com/gitleaks
- https://www.turbogeek.co.uk/gitleaks-secret-scanning-devsecops/

log/slog: standard library (no search — stdlib since Go 1.21).
