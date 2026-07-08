# ADR 12: Go engineering standards — latest-everything toolchain, gofumpt, golangci-lint v2, lefthook, CI gates

- **Status:** Accepted
- **Date:** 2026-07-07
- **Deciders:** Sawmon (directive: "shiny new, latest versions of everything"); specifics researched
- **Related:** ADR 05 (Go 1.26 floor), ADR 11 (develop-gated workflow)

## Context

The user mandated current best/modern Go practice across formatting, line length,
typing rigor, pre-commit, and CI/CD, with the latest versions of everything —
including the local toolchain. Verified state as of 2026-07-07:

- **Go 1.26.5** is the latest stable (released 2026-07-07; support policy: each
  major supported until two newer majors exist). Local machine had 1.26.0 via
  Homebrew — upgraded as part of this decision.
- **golangci-lint v2.12.2** (2026-05-06) is current; v2 YAML config format.
- **lefthook v2.1.9** (2026-05-29) is the current fast, Go-native git-hooks manager
  (single binary, parallel execution, `{staged_files}` templating).
- **Line length:** Google's official Go style guide has **no fixed line length**;
  gofmt/gofumpt deliberately do not wrap lines. golines (100-col wrapper) exists —
  original segmentio repo archived 2025-12-19; golangci maintains the fork.

## Decision

1. **Toolchain currency:** `go.mod` declares `go 1.26` + **`toolchain go1.26.5`** —
   Go's automatic toolchain management (GOTOOLCHAIN=auto) then builds with the
   pinned latest everywhere, regardless of package-manager lag. Patch bumps arrive
   via automated dependency PRs (below). Local Homebrew go kept current.
2. **Formatting:** gofmt + **gofumpt** (stricter superset), CI-enforced (diff =
   failure). **Line length: no hard limit**, per the official style guide; ~100
   columns as soft review guidance; golines available in golangci-lint if a hard
   wrap is ever wanted — not adopted (not ecosystem norm).
3. **Static analysis ("types"):** Go's compiler enforces the type system natively;
   the deep-semantic layer (the Go analog of tools like Astral's `ty` for Python)
   is **staticcheck + govet**, run via golangci-lint. `any` avoided in exported
   surfaces; generics only where they delete real duplication.
4. **Linting:** golangci-lint v2.12.2, v2 YAML config. Curated set (not
   everything-on): govet, staticcheck, errcheck, revive, gosec, errorlint,
   misspell, unconvert, unparam, nolintlint — every `//nolint` must carry a linter
   name and reason (nolintlint enforces). Exact config lands with the first code
   commit and is itself reviewed.
5. **Pre-commit (lefthook v2.1.9):** fast hooks only on pre-commit — gofumpt check
   on `{staged_files}`, golangci-lint on the whole module (see amendment below),
   `go mod tidy` drift check; heavier gates on pre-push — `go test ./... -race`.
   Commit messages: Conventional Commits (the repo already uses `docs:`/`feat:`
   informally; formalized now).

   *Amendment (2026-07-07, Phase 1 planning):* golangci-lint cannot take
   `{staged_files}` — file arguments spanning packages fail typechecking
   ("named files must all be in one directory") and v2.12.2 then reports
   "0 issues" with exit 0, silently neutering the hook (verified empirically).
   The pre-commit job therefore runs `golangci-lint run` module-wide, gated by
   a `*.go` glob so non-Go commits skip it; the module stays small enough that
   this remains within the fast-hook budget.
6. **CI/CD (GitHub Actions):** on every PR to `develop`/`main` — lint job
   (golangci-lint), test matrix (macos-latest + ubuntu-latest, `-race`, coverage),
   and **govulncheck** (official Go vulnerability scanner). Actions pinned by
   commit SHA (supply-chain hygiene) with Dependabot/Renovate bumping gomod,
   actions, and the Go toolchain weekly. Releases: GoReleaser on tag (details in
   the distribution section of the design).

## Consequences

- The repo is always on the newest supported Go within days of release, hands-free.
- A contributor (or future machine) needs only git + go + lefthook; everything else
  is pinned and self-installing.
- WSL2 cannot run in GitHub-hosted CI — its daemon-activation branch is covered by
  the testing section's manual runbook instead (design Section 12).

## Buy vs build

All tooling bought: gofumpt, golangci-lint, lefthook, govulncheck, GoReleaser,
Dependabot — zero custom scripts beyond configuration files.

## Sources

Search trail (WebSearch/WebFetch, 2026-07-07):

Query: latest Go release —
- https://go.dev/doc/devel/release (go1.26.5 + go1.25.12 released 2026-07-07;
  support policy quoted)

Query: `golangci-lint v2 latest version July 2026 recommended linters configuration`
- https://github.com/golangci/golangci-lint/releases
- https://golangci-lint.run/docs/product/changelog/
- https://github.com/golangci/golangci-lint
- https://golangci-lint.run/docs/welcome/install/local/
- https://golangci-lint.run/
- https://pkg.go.dev/github.com/golangci/golangci-lint/v2
- https://golangci-lint.run/docs/product/changelog-v1/
- https://golangci-lint.run/docs/welcome/faq/
- https://repology.org/project/golangci-lint/versions
- https://golangci-lint.run/docs/welcome/integrations/
- https://golangci-lint.run/docs/linters/configuration/
- https://olegk.dev/go-linters-configuration-the-right-version
- https://golangci-lint.run/docs/configuration/
- https://freshman.tech/linting-golang/
- https://dev.to/olegkovalov/go-linters-configuration-the-right-version-3jeo
- https://www.glukhov.org/post/2025/11/linters-for-go/
- https://golangci-lint.run/docs/linters/
- https://golangci-lint.run/docs/configuration/file/
- https://rostislaved.medium.com/understanding-golangci-lint-configuration-de530f023ef6

Query: `lefthook git hooks manager latest version 2026 Go projects pre-commit`
- https://github.com/evilmartians/lefthook
- https://pypi.org/project/lefthook/
- https://lefthook.dev/
- https://github.com/evilmartians/lefthook/releases
- https://recca0120.github.io/en/2026/03/08/lefthook-git-hooks/
- https://www.npmjs.com/package/lefthook
- https://lobehub.com/skills/agentskillexchange-skills-lefthook-git-hooks-manager
- https://sourceforge.net/projects/lefthook.mirror/
- https://dev.to/recca0120/ditch-husky-speed-up-git-hooks-with-lefthook-hkm
- https://www.pkgpulse.com/guides/husky-vs-lefthook-vs-lint-staged-git-hooks-nodejs-2026

Query: `Go code style line length limit official guidance gofumpt golines 2026`
- https://golangci-lint.run/docs/formatters/configuration/
- https://github.com/segmentio/golines (archived 2025-12-19)
- https://google.github.io/styleguide/go/guide.html (no fixed line length)
- https://pkg.go.dev/github.com/wrype/golines
- https://pkg.go.dev/github.com/golangci/golines (maintained fork)
- https://github.com/golangci/golines
- https://pkg.go.dev/github.com/matthewhughes934/golines
