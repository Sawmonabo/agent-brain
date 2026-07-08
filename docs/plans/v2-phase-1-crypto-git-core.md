# agent-brain v2 — Phase 1: Foundation, Crypto & Git Core — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reset the repo to a greenfield Go module and build the riskiest core first: the Tink AES-SIV codec, the git clean/smudge/textconv filters, and the always-resolving merge driver — proven end-to-end through *real git* with two simulated machines.

**Architecture:** Single Go module (`github.com/Sawmonabo/agent-brain`), everything under `internal/`, one thin `cmd/agent-brain`. This phase delivers the plumbing git invokes (`git-clean`, `git-smudge`, `git-textconv`, `git-merge`) plus the packages they stand on (`config`, `keys`, `crypto`, `gitx`). The deliverable test: two clones of a bare repo diverge on the same encrypted file and both converge to a retain-both block with zero plaintext ever stored in git objects.

**Tech Stack:** Go 1.26 (toolchain go1.26.5) · tink-go/v2 v2.7.0 (Deterministic AEAD, AES256_SIV) · cobra v1.10.2 + fang v2.0.1 (`charm.land/fang/v2`) · renameio/v2 · pelletier/go-toml/v2 · go-cmp · system git · golangci-lint v2.12.2 · gofumpt · lefthook v2.1.9.

**Phase roadmap** (this is plan 1 of 4; later plans are written after the prior phase's code exists, per the spec's just-in-time convention):

1. **Phase 1 (this plan):** greenfield reset, module + CI/tooling, config paths, keys, crypto codec, filter/merge plumbing, real-git integration proof.
2. **Phase 2:** repo layout/registry/manifests, mirror in/out, sync engine loop, watch manager, daemon + UDS API + service install.
3. **Phase 3:** provider adapters (claude, codex), index reconciliation, full CLI/TUI (init wizard, dashboard, track/status/conflicts/doctor), testscript e2e.
4. **Phase 4:** `migrate`, retirement checks, GoReleaser + tap, onboarding/WSL2 runbooks.

Spec: `docs/00-design-spec.md` (§ references below). ADRs: `docs/decisions/`.

## Global Constraints

Every task implicitly includes these. Exact values are from the spec appendix (verified 2026-07-07).

- Branch: ALL work lands on `develop`. Never commit to `main`.
- Module: `module github.com/Sawmonabo/agent-brain`; `go 1.26`; `toolchain go1.26.5`.
- Dependency pins: tink-go/v2 v2.7.0 · cobra v1.10.2 · fang v2.0.1 (module path `charm.land/fang/v2` — Charm's vanity domain; the `github.com/charmbracelet/fang` path ends at v1) · renameio/v2 · pelletier/go-toml/v2 · go-cmp (latest minor of each unless pinned here). Dependabot keeps them current afterward.
- All packages under `internal/` — no public API surface. No `pkg/` directory.
- Formatting: gofmt + gofumpt (CI-enforced). No hard line-length limit; ~100 cols soft.
- Lint set (golangci-lint v2.12.2): govet, staticcheck, errcheck, revive, gosec, errorlint, misspell, unconvert, unparam, nolintlint. Every `//nolint` needs linter name + reason.
- Tests: stdlib `testing` + `go-cmp` only — NO assertion frameworks (ADR 15). Table-driven, `t.Parallel()`, `t.TempDir()`.
- Commits: Conventional Commits (`feat:`, `fix:`, `chore:`, `test:`, `docs:`, `ci:`). One commit per task minimum; `!` on breaking.
- Safety invariants (spec §5, §11): the keyset NEVER enters any repo; plaintext memory content NEVER reaches a git object (tests assert this); `filter.agentbrain.required = true` fail-closed; the merge driver ALWAYS exits resolved (0) on merge-able input.
- The age key and `main`'s bash system stay untouched — migration/scrub are Phase 4+ concerns (ADR 13 gate).

---

### Task 1: Greenfield reset on develop (ADR 11, spec §8)

Deletes the bash/chezmoi system from `develop` and rewrites the repo-level meta files for the Go world. `main` keeps the bash system until v2 merges; the encrypted memories also still exist in every machine's runtime dirs and on the private GitHub remote, so deleting `home/` here loses nothing.

**Files:**
- Delete: `home/` (entire tree, incl. `dot_agent-brain/` ciphertext), `tools/` (installer + bats tests), `.chezmoiroot`
- Move: `docs/plans/sparkling-wiggling-curry.md`, `docs/plans/v3-cli-settings-flag.md` → `docs/archive/plans/`
- Rewrite: `.gitattributes`, `.gitignore`, `README.md`, `CLAUDE.md`

**Interfaces:**
- Consumes: nothing (first task).
- Produces: a repo root ready for `go mod init`; later tasks assume `home/`, `tools/`, chezmoi files are gone.

- [ ] **Step 1: Confirm you are on `develop` and clean**

Run: `git branch --show-current && git status --porcelain`
Expected: `develop`, empty status. If not on develop: STOP — do not proceed on any other branch.

- [ ] **Step 2: Delete the legacy trees**

```bash
git rm -r -q home tools .chezmoiroot
mkdir -p docs/archive/plans
git mv docs/plans/sparkling-wiggling-curry.md docs/archive/plans/
git mv docs/plans/v3-cli-settings-flag.md docs/archive/plans/
```

- [ ] **Step 3: Rewrite `.gitattributes`** (the `*.age binary` rule dies with the ciphertext; nothing binary remains)

```gitattributes
* text=auto eol=lf
*.md text eol=lf
go.mod text eol=lf
go.sum text eol=lf
```

- [ ] **Step 4: Rewrite `.gitignore`**

The age-key patterns STAY (the key is live until the ADR 13 scrub completes); Tink keyset and Go artifacts are added.

```gitignore
# Never commit private keys — age (legacy, live until ADR 13 scrub) or Tink.
*key.txt
*age-key*
**/private_*
keyset.json

# Go build/test artifacts
/agent-brain
dist/
coverage.out
*.test

# Transient agent scratch (surface-forward-then-delete convention)
.agents/

# Per-user / per-machine Claude Code state
.claude/settings.local.json
.claude/tmp/

# Editor / OS
.DS_Store
*.swp
*~
```

- [ ] **Step 5: Replace `README.md`** with this exact content:

```markdown
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
```

- [ ] **Step 6: Replace `CLAUDE.md`** with this exact content (note the four-backtick fence — the body embeds a ```bash block):

````markdown
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
````

- [ ] **Step 7: Commit**

```bash
git add -A
git commit -m "chore!: greenfield reset — remove bash/chezmoi system on develop (ADR 11)

home/ (incl. ciphertext leaves), tools/ (installer + bats), and chezmoi
scaffolding are deleted; memories remain safe in runtime dirs, on the
private remote, and in main's history until the ADR 13 scrub.

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

Run: `git status --porcelain` → empty; `ls` → no `home/`, no `tools/`.

---

### Task 2: Go module + CLI skeleton

Creates the module, the thin `main`, and the cobra root that every later command hangs off. Plumbing subcommands (Task 7/9) need cobra now, so the CLI framework lands first.

**Files:**
- Create: `go.mod`, `go.sum` (generated), `cmd/agent-brain/main.go`, `internal/cli/root.go`, `internal/cli/root_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `cli.Root() *cobra.Command` — every later task registers subcommands on it via `root.AddCommand(...)` inside `Root()`. `main` is frozen after this task.

- [ ] **Step 1: Init the module and pin the toolchain**

```bash
go mod init github.com/Sawmonabo/agent-brain
go mod edit -go=1.26 -toolchain=go1.26.5
go get github.com/spf13/cobra@v1.10.2
go get charm.land/fang/v2@v2.0.1
```

Expected: `go.mod` shows `go 1.26` and `toolchain go1.26.5`; the first `go get` may download go1.26.5 (Go's automatic toolchain management — normal). fang v2 lives under Charm's vanity import path — its go.mod declares `module charm.land/fang/v2` (verified against the module proxy 2026-07-07); `github.com/charmbracelet/fang` ends at v1.0.0 and must NOT be used. Verify: `go list -m github.com/spf13/cobra charm.land/fang/v2`.

- [ ] **Step 2: Write the failing test** — `internal/cli/root_test.go`:

```go
package cli

import "testing"

func TestRoot(t *testing.T) {
	t.Parallel()
	root := Root()
	if root.Use != "agent-brain" {
		t.Fatalf("root.Use = %q, want %q", root.Use, "agent-brain")
	}
	if root.Version == "" {
		t.Fatal("root.Version is empty; want a default like \"dev\"")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/cli/ -v`
Expected: FAIL — `undefined: Root`.

- [ ] **Step 4: Implement** — `internal/cli/root.go`:

```go
// Package cli assembles the agent-brain command tree (spec §7).
package cli

import "github.com/spf13/cobra"

// Version is stamped by the release build (-ldflags "-X ...cli.Version=v1.2.3").
var Version = "dev"

// Root returns the fully wired command tree. Later packages add subcommands
// here — main never changes.
func Root() *cobra.Command {
	root := &cobra.Command{
		Use:           "agent-brain",
		Short:         "Invisible cross-machine sync for AI coding agents' memory",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	return root
}
```

And `cmd/agent-brain/main.go`:

```go
// Package main is the thin entry point for the agent-brain CLI binary
// (spec §8): it wires fang's runtime around the cobra command tree
// assembled in internal/cli.
package main

import (
	"context"
	"os"

	"charm.land/fang/v2"

	"github.com/Sawmonabo/agent-brain/internal/cli"
)

func main() {
	if err := fang.Execute(context.Background(), cli.Root(), fang.WithVersion(cli.Version)); err != nil {
		os.Exit(1)
	}
}
```

(Signature verified against fang v2.0.1: `func Execute(ctx context.Context, root *cobra.Command, options ...Option) error`. If a later fang differs, `go doc charm.land/fang/v2 Execute` is the authority. `fang.WithVersion(cli.Version)` is required — fang.Execute unconditionally overwrites `root.Version` with its own build-info fallback otherwise (verified in fang v2.0.1 source, fang.go:137-139), so neither `cli.Version`'s default "dev" nor a release ldflags stamp would ever reach `--version` without it. The package doc comment is required by the revive package-comments rule in the Task 3 lint gate.)

- [ ] **Step 5: Run tests and build**

Run: `go test ./... && go build ./... && go run ./cmd/agent-brain --version`
Expected: PASS; build clean; version prints `dev`.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum cmd/ internal/
git commit -m "feat: Go module scaffold — cobra root + fang main (spec §8)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 3: Tooling — lint, hooks, CI (ADR 12)

**Files:**
- Create: `.golangci.yml`, `lefthook.yml`, `.github/workflows/ci.yml`, `.github/dependabot.yml`

**Interfaces:**
- Consumes: the module from Task 2.
- Produces: `golangci-lint run` and `lefthook install` working locally; CI gating pushes/PRs on develop+main.

- [ ] **Step 1: Write `.golangci.yml`** (v2 config schema, curated set from ADR 12):

```yaml
version: "2"
linters:
  default: none
  enable:
    - govet
    - staticcheck
    - errcheck
    - revive
    - gosec
    - errorlint
    - misspell
    - unconvert
    - unparam
    - nolintlint
  settings:
    nolintlint:
      require-explanation: true
      require-specific: true
  exclusions:
    rules:
      - path: '_test\.go'
        linters:
          - gosec # test fixtures use 0o644 files and variable paths by design
formatters:
  enable:
    - gofmt
    - gofumpt
```

Run: `golangci-lint run` → no issues (skeleton code is clean). If the config schema errors, run `golangci-lint config verify` and fix per its message — the tool documents its own v2 schema. (This exact config passed `golangci-lint config verify` on v2.12.2, 2026-07-07.) Global gosec excludes are prohibited (owner decision 2026-07-08, Q1 review). When a later task's by-design pattern fires a gosec rule (keyset paths in keys, filter argv paths in cli, git exec in gitx, merge labels), suppress it at the narrowest scope: a path-scoped rule under `linters.exclusions.rules` (path + rule-ID text match) or `//nolint:gosec // <reason>` at the site — every waiver carries a written justification (nolintlint enforces the nolint form).

- [ ] **Step 2: Write `lefthook.yml`** (pre-commit fast, pre-push heavy — ADR 12):

```yaml
pre-commit:
  parallel: true
  jobs:
    - name: gofumpt
      glob: "*.go"
      run: test -z "$(gofumpt -l {staged_files})" || { gofumpt -l {staged_files}; exit 1; }
    - name: golangci-lint
      glob: "*.go"
      run: golangci-lint run
    - name: mod-tidy
      glob: "go.{mod,sum}"
      run: go mod tidy -diff
pre-push:
  jobs:
    - name: test-race
      run: go test ./... -race
```

Run: `lefthook install && lefthook run pre-commit --all-files`
Expected: all jobs pass (gofumpt may list files on first run — fix with `gofumpt -w .` and re-run).

The golangci-lint job deliberately runs the whole module rather than ADR 12's original `{staged_files}`: golangci-lint file arguments must all live in ONE directory — a staged set spanning packages fails typechecking and then exits 0 with "0 issues", silently neutering the hook (verified against v2.12.2). The `*.go` glob still skips the job entirely for non-Go commits, which keeps pre-commit fast; ADR 12 carries an amendment note recording this.

- [ ] **Step 3: Write `.github/dependabot.yml`**:

```yaml
version: 2
updates:
  - package-ecosystem: gomod
    directory: /
    schedule:
      interval: weekly
  - package-ecosystem: github-actions
    directory: /
    schedule:
      interval: weekly
```

- [ ] **Step 4: Write `.github/workflows/ci.yml`** — then SHA-pin it (ADR 12):

```yaml
name: ci
on:
  push:
    branches: [develop, main]
  pull_request:
    branches: [develop, main]
permissions:
  contents: read
jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: actions/setup-go@v6
        with:
          go-version-file: go.mod
      - uses: golangci/golangci-lint-action@v8
        with:
          version: v2.12.2
  test:
    strategy:
      matrix:
        os: [ubuntu-latest, macos-latest]
    runs-on: ${{ matrix.os }}
    steps:
      - uses: actions/checkout@v5
      - uses: actions/setup-go@v6
        with:
          go-version-file: go.mod
      - run: go test ./... -race -coverprofile=coverage.out
      - run: go tool cover -func=coverage.out | tail -1  # spec §12: coverage tracked, no hard gate in v1
  govulncheck:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: actions/setup-go@v6
        with:
          go-version-file: go.mod
      - run: go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

Now replace every `@vN` tag with its commit SHA (immutable pin), keeping the tag as a comment. For each action:

```bash
gh api repos/actions/checkout/git/matching-refs/tags/v5 --jq '.[-1] | .ref, .object.sha'
gh api repos/actions/setup-go/git/matching-refs/tags/v6 --jq '.[-1] | .ref, .object.sha'
gh api repos/golangci/golangci-lint-action/git/matching-refs/tags/v8 --jq '.[-1] | .ref, .object.sha'
```

Edit each `uses:` to `owner/repo@<sha> # <tag>`. (If a `.object.sha` points at a tag object rather than a commit, dereference it: `gh api repos/<owner>/<repo>/git/tags/<sha> --jq .object.sha`.)

- [ ] **Step 5: Commit, push develop, verify CI**

```bash
git add .golangci.yml lefthook.yml .github/
git commit -m "ci: lint + macos/ubuntu race matrix + govulncheck; lefthook hooks; dependabot (ADR 12)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
git push -u origin develop
gh run watch --exit-status
```

Expected: the `ci` workflow passes all three jobs. If a job fails, fix before proceeding — every later task assumes green CI.

---

### Task 4: `internal/config` — platform paths

Resolves where things live (spec §3/§5): config dir `~/.config/agent-brain` on BOTH OSes (keyset + config.toml), data dir `~/Library/Application Support/agent-brain` (macOS) / XDG (`~/.local/share/agent-brain`) on Linux. Env overrides exist so tests and git-spawned filter processes can inject paths. TOML config-file parsing is Phase 2 (nothing in Phase 1 reads settings).

**Files:**
- Create: `internal/config/paths.go`, `internal/config/paths_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `config.DefaultPaths() (config.Paths, error)`; `type Paths struct { ConfigDir, DataDir string }`; method `(Paths) Keyset() string`. Env overrides: `AGENT_BRAIN_CONFIG_DIR`, `AGENT_BRAIN_DATA_DIR`.

- [ ] **Step 1: Write the failing test** — `internal/config/paths_test.go`:

```go
package config

import (
	"path/filepath"
	"runtime"
	"testing"
)

// t.Setenv forbids t.Parallel — these tests stay serial.
func TestDefaultPathsEnvOverride(t *testing.T) {
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", "/tmp/cfg")
	t.Setenv("AGENT_BRAIN_DATA_DIR", "/tmp/data")
	paths, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	if paths.ConfigDir != "/tmp/cfg" || paths.DataDir != "/tmp/data" {
		t.Fatalf("got %+v, want env-injected dirs", paths)
	}
	if got, want := paths.Keyset(), filepath.Join("/tmp/cfg", "keyset.json"); got != want {
		t.Fatalf("Keyset() = %q, want %q", got, want)
	}
}

func TestDefaultPathsPerOS(t *testing.T) {
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", "")
	t.Setenv("AGENT_BRAIN_DATA_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "/home/u")
	paths, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/home/u", ".config", "agent-brain"); paths.ConfigDir != want {
		t.Fatalf("ConfigDir = %q, want %q", paths.ConfigDir, want)
	}
	wantData := filepath.Join("/home/u", ".local", "share", "agent-brain")
	if runtime.GOOS == "darwin" {
		wantData = filepath.Join("/home/u", "Library", "Application Support", "agent-brain")
	}
	if paths.DataDir != wantData {
		t.Fatalf("DataDir = %q, want %q", paths.DataDir, wantData)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -v`
Expected: FAIL — `undefined: DefaultPaths`.

- [ ] **Step 3: Implement** — `internal/config/paths.go`:

```go
// Package config resolves agent-brain's on-disk locations (spec §3, §5).
package config

import (
	"os"
	"path/filepath"
	"runtime"
)

// Paths locates agent-brain's local state. ConfigDir holds keyset.json and
// config.toml on every OS; DataDir holds the memories checkout, local
// registry, and logs.
type Paths struct {
	ConfigDir string
	DataDir   string
}

// DefaultPaths resolves per-OS defaults. AGENT_BRAIN_CONFIG_DIR and
// AGENT_BRAIN_DATA_DIR override — required by tests and by filter processes
// git spawns without our process environment conventions.
func DefaultPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	paths := Paths{
		ConfigDir: filepath.Join(xdgDir("XDG_CONFIG_HOME", filepath.Join(home, ".config")), "agent-brain"),
	}
	if runtime.GOOS == "darwin" {
		paths.DataDir = filepath.Join(home, "Library", "Application Support", "agent-brain")
	} else {
		paths.DataDir = filepath.Join(xdgDir("XDG_DATA_HOME", filepath.Join(home, ".local", "share")), "agent-brain")
	}
	if dir := os.Getenv("AGENT_BRAIN_CONFIG_DIR"); dir != "" {
		paths.ConfigDir = dir
	}
	if dir := os.Getenv("AGENT_BRAIN_DATA_DIR"); dir != "" {
		paths.DataDir = dir
	}
	return paths, nil
}

// Keyset returns the Tink keyset location (spec §5: beside config.toml).
func (p Paths) Keyset() string {
	return filepath.Join(p.ConfigDir, "keyset.json")
}

func xdgDir(env, fallback string) string {
	if dir := os.Getenv(env); dir != "" {
		return dir
	}
	return fallback
}
```

- [ ] **Step 4: Run tests** — `go test ./internal/config/ -v` → PASS (both).

- [ ] **Step 5: Commit**

```bash
git add internal/config/
git commit -m "feat: config package — per-OS paths with env overrides (spec §3, §5)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 5: `internal/keys` — keyset generate / load / export / import (spec §5)

One shared Tink keyset across hosts, AES256_SIV, plaintext-keyset workflow, 0600, atomic writes.

**Files:**
- Create: `internal/keys/keys.go`, `internal/keys/keys_test.go`

**Interfaces:**
- Consumes: nothing (takes explicit paths; callers get them from `config.Paths.Keyset()`).
- Produces:
  - `keys.Generate(path string) error` — errors if the file already exists
  - `keys.Primitive(path string) (tink.DeterministicAEAD, error)`
  - `keys.Export(path string) (string, error)` — std-base64 armored keyset JSON
  - `keys.Import(path, armored string) error` — validates before writing; errors if file exists

- [ ] **Step 1: Add dependencies**

```bash
go get github.com/tink-crypto/tink-go/v2@v2.7.0
go get github.com/google/renameio/v2@latest
```

- [ ] **Step 2: Write the failing test** — `internal/keys/keys_test.go`:

```go
package keys

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateLoadRoundtrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "keyset.json")
	if err := Generate(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("keyset perm = %o, want 600", perm)
	}
	if err := Generate(path); err == nil {
		t.Fatal("second Generate succeeded; want refuse-to-overwrite error")
	}
	primitive, err := Primitive(path)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := primitive.EncryptDeterministically([]byte("hello"), nil)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := primitive.DecryptDeterministically(ciphertext, nil)
	if err != nil || string(plaintext) != "hello" {
		t.Fatalf("roundtrip failed: %v %q", err, plaintext)
	}
}

func TestExportImport(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	original := filepath.Join(dir, "keyset.json")
	if err := Generate(original); err != nil {
		t.Fatal(err)
	}
	armored, err := Export(original)
	if err != nil {
		t.Fatal(err)
	}
	imported := filepath.Join(dir, "imported.json")
	if err := Import(imported, armored); err != nil {
		t.Fatal(err)
	}
	// The imported keyset must decrypt what the original encrypted.
	primitiveA, err := Primitive(original)
	if err != nil {
		t.Fatal(err)
	}
	primitiveB, err := Primitive(imported)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := primitiveA.EncryptDeterministically([]byte("shared identity"), nil)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := primitiveB.DecryptDeterministically(ciphertext, nil)
	if err != nil || string(plaintext) != "shared identity" {
		t.Fatalf("cross-keyset decrypt failed: %v %q", err, plaintext)
	}
	if err := Import(imported, armored); err == nil {
		t.Fatal("Import over existing file succeeded; want error")
	}
	if err := Import(filepath.Join(dir, "bad.json"), "!!!not-base64!!!"); err == nil {
		t.Fatal("Import of garbage succeeded; want error")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/keys/ -v`
Expected: FAIL — `undefined: Generate` (and friends).

- [ ] **Step 4: Implement** — `internal/keys/keys.go`:

```go
// Package keys manages the shared Tink keyset (spec §5): one AES256_SIV
// Deterministic-AEAD keyset across all hosts, stored plaintext at 0600 —
// the documented no-KMS posture for a local dev tool (ADR 06).
package keys

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/renameio/v2"
	"github.com/tink-crypto/tink-go/v2/daead"
	"github.com/tink-crypto/tink-go/v2/insecurecleartextkeyset"
	"github.com/tink-crypto/tink-go/v2/keyset"
	"github.com/tink-crypto/tink-go/v2/tink"
)

// Generate creates a fresh AES256_SIV keyset at path (0600). It refuses to
// overwrite: losing a keyset means losing every memory encrypted under it.
func Generate(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("keyset already exists at %s (use key import/export to move keys)", path)
	}
	// AESSIVKeyTemplate is daead's only template; it generates the
	// AES256_SIV key type (64-byte key, RFC 5297) the spec pins.
	handle, err := keyset.NewHandle(daead.AESSIVKeyTemplate())
	if err != nil {
		return fmt.Errorf("generate keyset: %w", err)
	}
	return write(path, handle)
}

// Primitive loads the keyset and returns the Deterministic AEAD primitive.
func Primitive(path string) (tink.DeterministicAEAD, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read keyset: %w", err)
	}
	handle, err := insecurecleartextkeyset.Read(keyset.NewJSONReader(bytes.NewReader(data)))
	if err != nil {
		return nil, fmt.Errorf("parse keyset: %w", err)
	}
	primitive, err := daead.New(handle)
	if err != nil {
		return nil, fmt.Errorf("keyset is not a Deterministic AEAD keyset: %w", err)
	}
	return primitive, nil
}

// Export returns the keyset as std-base64 for transfer over a user-chosen
// channel; the export IS the recovery artifact (spec §5).
func Export(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read keyset: %w", err)
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// Import validates an armored keyset and installs it at path (0600).
func Import(path, armored string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("keyset already exists at %s; refusing to overwrite", path)
	}
	data, err := base64.StdEncoding.DecodeString(armored)
	if err != nil {
		return fmt.Errorf("decode armored keyset: %w", err)
	}
	handle, err := insecurecleartextkeyset.Read(keyset.NewJSONReader(bytes.NewReader(data)))
	if err != nil {
		return fmt.Errorf("parse keyset: %w", err)
	}
	if _, err := daead.New(handle); err != nil {
		return fmt.Errorf("keyset is not a Deterministic AEAD keyset: %w", err)
	}
	return write(path, handle)
}

func write(path string, handle *keyset.Handle) error {
	var buf bytes.Buffer
	if err := insecurecleartextkeyset.Write(handle, keyset.NewJSONWriter(&buf)); err != nil {
		return fmt.Errorf("serialize keyset: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return renameio.WriteFile(path, buf.Bytes(), 0o600)
}
```

- [ ] **Step 5: Run tests** — `go test ./internal/keys/ -v` → PASS. (The `daead` symbols above — `AESSIVKeyTemplate`, `New` — were verified against tink-go v2.7.0 on 2026-07-07; there is no `AES256SIVKeyTemplate`. If any other symbol differs, `go doc github.com/tink-crypto/tink-go/v2/<pkg>` is the authority — adjust the call sites, not the behavior.)

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/keys/
git commit -m "feat: keys package — Tink AES256_SIV keyset generate/export/import (spec §5, ADR 06)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 6: `internal/crypto` — the codec (magic header + deterministic AEAD)

**Files:**
- Create: `internal/crypto/codec.go`, `internal/crypto/codec_test.go`, `internal/crypto/fuzz_test.go`

**Interfaces:**
- Consumes: `tink.DeterministicAEAD` (from `keys.Primitive`).
- Produces:
  - `crypto.NewCodec(d tink.DeterministicAEAD) *Codec`
  - `(*Codec) Encrypt(plaintext []byte) ([]byte, error)` — output = `agb1\x00` magic + Tink ciphertext
  - `(*Codec) Decrypt(data []byte) ([]byte, error)` — errors unless data carries the magic
  - `(*Codec) Clean(data []byte) ([]byte, error)` — the clean-filter endpoint: on magic-prefixed input, verify-decrypt then pass the original bytes through byte-identical (idempotent) on success or fail closed with `ErrCleanVerifyFailed` if it does not decrypt; else Encrypt
  - `(*Codec) Smudge(data []byte) ([]byte, error)` — the smudge/textconv endpoint: Decrypt if ciphertext, else passthrough (never-encrypted content)
  - `crypto.IsEncrypted(data []byte) bool`

Per spec §8, `internal/crypto` owns the clean/smudge/textconv/merge endpoint *logic*; `internal/cli` wraps it in thin cobra commands (Tasks 7, 9).

Design constraints locked here: **associated data is always nil** — the merge driver and textconv operate on pathless temp blobs git hands them, so a path-bound AD would break decryption there; and equal-plaintext ⇒ equal-ciphertext is the accepted determinism trade (spec §5). The magic header lets `IsEncrypted` distinguish never-filtered plaintext; Clean's idempotency is a **verified passthrough** — magic-prefixed input is returned byte-identical only after it decrypts, so re-cleaning genuine stored ciphertext is stable while lookalike plaintext and foreign-keyset ciphertext fail closed (spec §5, Q2-ratified).

- [ ] **Step 1: Write the failing test** — `internal/crypto/codec_test.go`:

```go
package crypto

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/keys"
)

// testing.TB so both tests (*testing.T) and fuzz targets (*testing.F) can use it.
func newTestCodec(tb testing.TB) *Codec {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "keyset.json")
	if err := keys.Generate(path); err != nil {
		tb.Fatal(err)
	}
	primitive, err := keys.Primitive(path)
	if err != nil {
		tb.Fatal(err)
	}
	return NewCodec(primitive)
}

func TestCodec(t *testing.T) {
	t.Parallel()
	codec := newTestCodec(t)
	plaintext := []byte("# memory\n\nsecret fact\n")

	ciphertext, err := codec.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if !IsEncrypted(ciphertext) {
		t.Fatal("Encrypt output not recognized by IsEncrypted")
	}
	if bytes.Contains(ciphertext, []byte("secret fact")) {
		t.Fatal("ciphertext contains plaintext")
	}

	again, err := codec.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ciphertext, again) {
		t.Fatal("determinism violated: equal plaintext produced different ciphertext")
	}

	decrypted, err := codec.Decrypt(ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("roundtrip mismatch: %q", decrypted)
	}

	if _, err := codec.Decrypt(plaintext); err == nil {
		t.Fatal("Decrypt of plaintext succeeded; want no-magic error")
	}

	cleaned, err := codec.Clean(ciphertext)
	if err != nil || !bytes.Equal(cleaned, ciphertext) {
		t.Fatalf("Clean not idempotent on ciphertext: %v", err)
	}
	cleaned, err = codec.Clean(plaintext)
	if err != nil || !bytes.Equal(cleaned, ciphertext) {
		t.Fatalf("Clean(plaintext) != Encrypt(plaintext): %v", err)
	}
	smudged, err := codec.Smudge(ciphertext)
	if err != nil || !bytes.Equal(smudged, plaintext) {
		t.Fatalf("Smudge(ciphertext) failed: %v", err)
	}
	smudged, err = codec.Smudge(plaintext)
	if err != nil || !bytes.Equal(smudged, plaintext) {
		t.Fatalf("Smudge must pass plaintext through: %v", err)
	}
	tampered := append([]byte{}, ciphertext...)
	tampered[len(tampered)-1] ^= 0xFF
	if _, err := codec.Decrypt(tampered); err == nil {
		t.Fatal("Decrypt of tampered ciphertext succeeded; want auth error")
	}
	if IsEncrypted([]byte("agb")) || IsEncrypted(nil) {
		t.Fatal("IsEncrypted false positives on short input")
	}
}

// TestCleanFailsClosed pins the Q2-ratified verify-decrypt contract: Clean
// must reject magic-prefixed input it cannot decrypt rather than pass it
// through, so plaintext that merely mimics the header never reaches a git
// object and ciphertext from a foreign keyset is not silently committed.
func TestCleanFailsClosed(t *testing.T) {
	t.Parallel()
	codec := newTestCodec(t)

	// An independent keyset stands in for another machine's ciphertext.
	foreign := newTestCodec(t)
	foreignCiphertext, err := foreign.Encrypt([]byte("secret from another machine"))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		input []byte
	}{
		{
			name:  "lookalike plaintext carrying the magic header",
			input: append(append([]byte{}, magic...), "not valid ciphertext"...),
		},
		{
			name:  "genuine ciphertext under a different keyset",
			input: foreignCiphertext,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, err := codec.Clean(testCase.input)
			if !errors.Is(err, ErrCleanVerifyFailed) {
				t.Fatalf("Clean error = %v; want ErrCleanVerifyFailed", err)
			}
			if got != nil {
				t.Fatalf("Clean returned %q on failure; want nil output", got)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/crypto/ -v`
Expected: FAIL — `undefined: Codec`.

- [ ] **Step 3: Implement** — `internal/crypto/codec.go`:

```go
// Package crypto is the storage codec (spec §5): deterministic AEAD
// (Tink AES-SIV) behind a magic header that marks agent-brain ciphertext.
package crypto

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/tink-crypto/tink-go/v2/tink"
)

// magic prefixes every stored ciphertext. Version bump = new magic.
var magic = []byte("agb1\x00")

// ErrNotEncrypted reports input without the agent-brain magic header.
var ErrNotEncrypted = errors.New("data is not agent-brain ciphertext (missing magic header)")

// ErrCleanVerifyFailed reports that Clean received magic-prefixed input it
// could not decrypt: the bytes carry the agent-brain header but are not valid
// ciphertext under this keyset (keyset mismatch or corrupted content). Clean
// fails closed on it rather than commit lookalike plaintext or double-wrap.
var ErrCleanVerifyFailed = errors.New("clean: magic-prefixed input is not valid ciphertext under this keyset (keyset mismatch or corrupted content)")

// Codec encrypts/decrypts memory content. Associated data is always nil:
// the merge driver and textconv receive pathless temp blobs from git, and
// equal-plaintext ⇒ equal-ciphertext is the accepted determinism trade
// (spec §5) — so nothing can be bound into AD.
type Codec struct {
	daead tink.DeterministicAEAD
}

// NewCodec wraps a Deterministic AEAD primitive (from keys.Primitive).
func NewCodec(d tink.DeterministicAEAD) *Codec {
	return &Codec{daead: d}
}

// Encrypt seals plaintext and prefixes the magic header.
func (c *Codec) Encrypt(plaintext []byte) ([]byte, error) {
	ciphertext, err := c.daead.EncryptDeterministically(plaintext, nil)
	if err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}
	return append(append(make([]byte, 0, len(magic)+len(ciphertext)), magic...), ciphertext...), nil
}

// Decrypt unseals Encrypt output; ErrNotEncrypted if the magic is absent.
func (c *Codec) Decrypt(data []byte) ([]byte, error) {
	if !IsEncrypted(data) {
		return nil, ErrNotEncrypted
	}
	plaintext, err := c.daead.DecryptDeterministically(data[len(magic):], nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt (wrong keyset or corrupted data): %w", err)
	}
	return plaintext, nil
}

// Clean is the clean-filter endpoint (spec §5, §8). It upholds the absolute
// invariant that plaintext memory content never reaches a git object.
//
// Non-magic input is encrypted. Magic-prefixed input is verify-decrypted: on
// success the ORIGINAL input passes through byte-identical (idempotent — git
// may re-clean already-stored ciphertext), on failure Clean fails closed with
// ErrCleanVerifyFailed. Verification is what makes passthrough safe: it proves
// the bytes are genuine ciphertext under this keyset, so lookalike plaintext
// (content that merely begins with the magic header) and foreign-keyset or
// corrupted ciphertext are rejected at commit time rather than stored or
// double-wrapped.
func (c *Codec) Clean(data []byte) ([]byte, error) {
	if IsEncrypted(data) {
		if _, err := c.Decrypt(data); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrCleanVerifyFailed, err)
		}
		return data, nil
	}
	return c.Encrypt(data)
}

// Smudge is the smudge/textconv endpoint (spec §5, §8): ciphertext is
// decrypted, never-encrypted content passes through.
func (c *Codec) Smudge(data []byte) ([]byte, error) {
	if !IsEncrypted(data) {
		return data, nil
	}
	return c.Decrypt(data)
}

// IsEncrypted reports whether data carries the agent-brain magic header.
func IsEncrypted(data []byte) bool {
	return bytes.HasPrefix(data, magic)
}
```

- [ ] **Step 4: Run tests** — `go test ./internal/crypto/ -v` → PASS.

- [ ] **Step 5: Add the fuzz target** — `internal/crypto/fuzz_test.go`:

```go
package crypto

import (
	"bytes"
	"testing"
)

func FuzzRoundtrip(f *testing.F) {
	codec := newTestCodec(f)
	f.Add([]byte(""))
	f.Add([]byte("# memory\nfact\n"))
	f.Add([]byte{0x00, 0xFF, 0x61, 0x67, 0x62, 0x31, 0x00})
	f.Add([]byte("agb1\x00looks like ciphertext but is plaintext"))
	f.Fuzz(func(t *testing.T, plaintext []byte) {
		first, err := codec.Encrypt(plaintext)
		if err != nil {
			t.Fatal(err)
		}
		second, err := codec.Encrypt(plaintext)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, second) {
			t.Fatal("determinism violated")
		}
		decrypted, err := codec.Decrypt(first)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(decrypted, plaintext) {
			t.Fatal("roundtrip mismatch")
		}
		// Clean must verify-decrypt genuine ciphertext and pass the exact
		// bytes through (Q2 verify-decrypt contract): whatever the plaintext,
		// its ciphertext decrypts under this keyset, so Clean neither rejects
		// nor alters it.
		cleaned, err := codec.Clean(first)
		if err != nil {
			t.Fatalf("Clean rejected genuine ciphertext: %v", err)
		}
		if !bytes.Equal(cleaned, first) {
			t.Fatal("Clean altered genuine ciphertext; want byte-identical passthrough")
		}
	})
}
```

Run: `go test ./internal/crypto/ -run FuzzRoundtrip -v` (seed corpus only) → PASS.
Then: `go test ./internal/crypto/ -fuzz FuzzRoundtrip -fuzztime 20s` → no crashers.

- [ ] **Step 6: Commit**

```bash
git add internal/crypto/
git commit -m "feat: crypto codec — magic-prefixed deterministic AEAD + fuzzed roundtrip (spec §5)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 7: Filter plumbing commands — `git-clean`, `git-smudge`, `git-textconv` (spec §5, §7)

Hidden cobra subcommands git invokes per file. Fail-closed semantics: any error → non-zero exit → git (with `filter.agentbrain.required=true`) refuses the operation. Clean/smudge are byte pipes on stdin/stdout; textconv takes a path argument.

**Files:**
- Create: `internal/cli/filter.go`, `internal/cli/filter_test.go`
- Modify: `internal/cli/root.go` (register the three commands)

**Interfaces:**
- Consumes: `config.DefaultPaths`, `keys.Primitive`, `crypto.NewCodec/IsEncrypted`.
- Produces: CLI commands `git-clean`, `git-smudge`, `git-textconv <file>`; internal helper `loadCodec() (*crypto.Codec, error)` reused by Task 9.

Behavior table (implement exactly):

| Command | Input has magic | Input plain | Keyset missing |
|---|---|---|---|
| `git-clean` (stdin→stdout) | verify-decrypt → byte-identical passthrough; ERROR exit 1 if decrypt fails (blocks commit) | encrypt | ERROR exit 1 (blocks commit — never push plaintext) |
| `git-smudge` (stdin→stdout) | decrypt | passthrough (never-encrypted file) | ERROR exit 1 only if input has magic; plain passthrough still works |
| `git-textconv <file>` | decrypt file→stdout | cat file→stdout | ERROR exit 1 only if file has magic |

- [ ] **Step 1: Write the failing test** — `internal/cli/filter_test.go`:

```go
package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/crypto"
	"github.com/Sawmonabo/agent-brain/internal/keys"
)

// runCmd executes the root tree in-process. t.Setenv (keyset injection)
// forbids t.Parallel in callers.
func runCmd(t *testing.T, stdin []byte, args ...string) (stdout []byte, err error) {
	t.Helper()
	root := Root()
	root.SetIn(bytes.NewReader(stdin))
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs(args)
	err = root.Execute()
	if err != nil {
		t.Logf("stderr: %s (err: %v)", errBuf.String(), err)
	}
	return out.Bytes(), err
}

func setupKeyset(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", dir)
	if err := keys.Generate(filepath.Join(dir, "keyset.json")); err != nil {
		t.Fatal(err)
	}
}

func TestCleanSmudgeRoundtrip(t *testing.T) {
	setupKeyset(t)
	plaintext := []byte("# memory\nfact one\n")

	ciphertext, err := runCmd(t, plaintext, "git-clean")
	if err != nil {
		t.Fatal(err)
	}
	if !crypto.IsEncrypted(ciphertext) || bytes.Contains(ciphertext, []byte("fact one")) {
		t.Fatal("git-clean did not encrypt")
	}

	again, err := runCmd(t, ciphertext, "git-clean")
	if err != nil || !bytes.Equal(again, ciphertext) {
		t.Fatalf("git-clean not idempotent on ciphertext: %v", err)
	}

	back, err := runCmd(t, ciphertext, "git-smudge")
	if err != nil || !bytes.Equal(back, plaintext) {
		t.Fatalf("git-smudge roundtrip failed: %v %q", err, back)
	}

	passthrough, err := runCmd(t, plaintext, "git-smudge")
	if err != nil || !bytes.Equal(passthrough, plaintext) {
		t.Fatalf("git-smudge should pass plaintext through: %v", err)
	}
}

func TestCleanFailsClosedWithoutKeyset(t *testing.T) {
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", t.TempDir()) // empty dir: no keyset
	if _, err := runCmd(t, []byte("secret"), "git-clean"); err == nil {
		t.Fatal("git-clean without keyset succeeded; must fail closed")
	}
}

func TestTextconv(t *testing.T) {
	setupKeyset(t)
	plaintext := []byte("readable\n")
	ciphertext, err := runCmd(t, plaintext, "git-clean")
	if err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(blob, ciphertext, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd(t, nil, "git-textconv", blob)
	if err != nil || !bytes.Equal(out, plaintext) {
		t.Fatalf("git-textconv failed: %v %q", err, out)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -v`
Expected: FAIL — `unknown command "git-clean"`.

- [ ] **Step 3: Implement** — `internal/cli/filter.go`:

```go
package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/crypto"
	"github.com/Sawmonabo/agent-brain/internal/keys"
)

// loadCodec builds the storage codec from the configured keyset. Every
// plumbing command shares it; a missing keyset must surface as an error so
// filter.agentbrain.required=true fails closed (spec §5).
func loadCodec() (*crypto.Codec, error) {
	paths, err := config.DefaultPaths()
	if err != nil {
		return nil, err
	}
	primitive, err := keys.Primitive(paths.Keyset())
	if err != nil {
		return nil, fmt.Errorf("keyset unavailable (run `agent-brain init` or `agent-brain key import`): %w", err)
	}
	return crypto.NewCodec(primitive), nil
}

// The endpoint logic lives on crypto.Codec (Clean/Smudge — spec §8). git-clean
// always needs the codec now (Clean verify-decrypts magic input before it may
// pass through), so it has no keyset-less path; git-smudge/textconv keep their
// keyset-less passthrough for never-encrypted plaintext, so a clone without a
// keyset can still read never-filtered files.

func newGitCleanCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "git-clean",
		Short:  "Filter: encrypt stdin to stdout (invoked by git)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// nil passthrough: git-clean has no keyset-less short-circuit —
			// Clean verify-decrypts magic input, so both branches (verify
			// existing ciphertext, encrypt plaintext) need the codec.
			return pipeFilter(cmd, func(codec *crypto.Codec, data []byte) ([]byte, error) {
				return codec.Clean(data)
			}, nil)
		},
	}
}

func newGitSmudgeCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "git-smudge",
		Short:  "Filter: decrypt stdin to stdout (invoked by git)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return pipeFilter(cmd, func(codec *crypto.Codec, data []byte) ([]byte, error) {
				return codec.Smudge(data)
			}, func(data []byte) bool { return !crypto.IsEncrypted(data) }) // plaintext passes through without a keyset
		},
	}
}

func newGitTextconvCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "git-textconv <file>",
		Short:  "Diff textconv: print decrypted file (invoked by git)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			if !crypto.IsEncrypted(data) {
				_, err = cmd.OutOrStdout().Write(data)
				return err
			}
			codec, err := loadCodec()
			if err != nil {
				return err
			}
			plaintext, err := codec.Smudge(data)
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(plaintext)
			return err
		},
	}
}

// pipeFilter reads stdin and applies the codec endpoint. A nil passthrough
// predicate means the endpoint always needs the keyset (git-clean must
// verify-decrypt magic input, so it can never short-circuit keyset-less). A
// non-nil predicate short-circuits to raw passthrough when it reports true —
// git-smudge/textconv pass never-encrypted plaintext through without a keyset,
// so a clone lacking one can still read never-filtered files.
func pipeFilter(cmd *cobra.Command, endpoint func(*crypto.Codec, []byte) ([]byte, error), passthrough func([]byte) bool) error {
	input, err := io.ReadAll(cmd.InOrStdin())
	if err != nil {
		return err
	}
	if passthrough != nil && passthrough(input) {
		_, err = cmd.OutOrStdout().Write(input)
		return err
	}
	codec, err := loadCodec()
	if err != nil {
		return err
	}
	output, err := endpoint(codec, input)
	if err != nil {
		return err
	}
	_, err = cmd.OutOrStdout().Write(output)
	return err
}
```

In `internal/cli/root.go`, register them before `return root`:

```go
	root.AddCommand(newGitCleanCmd(), newGitSmudgeCmd(), newGitTextconvCmd())
```

- [ ] **Step 4: Run tests** — `go test ./internal/cli/ -v` → PASS (all four).

- [ ] **Step 5: Commit**

```bash
git add internal/cli/
git commit -m "feat: git-clean/git-smudge/git-textconv plumbing, fail-closed (spec §5)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 8: `internal/gitx` — system-git wrapper + filter wiring installer

**Files:**
- Create: `internal/gitx/gitx.go`, `internal/gitx/install.go`, `internal/gitx/gitx_test.go`

**Interfaces:**
- Consumes: nothing internal.
- Produces:
  - `type Result struct { Stdout, Stderr string; ExitCode int }`
  - `gitx.Run(ctx, dir string, args ...string) (Result, error)` — error on non-zero exit (stderr in the error) or spawn failure
  - `gitx.RunStatus(ctx, dir string, args ...string) (Result, error)` — non-zero exit is data (merge-file reports conflict count as exit code); errors only when git cannot run to a trustworthy completion: spawn failure, empty dir, canceled/expired context, or a signal-terminated child (whose -1 would otherwise masquerade as a conflict count)
  - `gitx.InstallFilters(ctx, repoDir, binPath string) error` — writes the `.git/config` wiring (spec §5)

- [ ] **Step 1: Write the failing test** — `internal/gitx/gitx_test.go`:

```go
package gitx

import (
	"context"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()

	if _, err := Run(ctx, dir, "init", "--quiet"); err != nil {
		t.Fatal(err)
	}
	result, err := Run(ctx, dir, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(result.Stdout) != "true" {
		t.Fatalf("rev-parse: %v %q", err, result.Stdout)
	}
	if _, err := Run(ctx, dir, "no-such-subcommand"); err == nil {
		t.Fatal("Run of invalid subcommand succeeded; want error")
	}
	result, err = RunStatus(ctx, dir, "no-such-subcommand")
	if err != nil {
		t.Fatalf("RunStatus must not error on non-zero exit: %v", err)
	}
	if result.ExitCode == 0 {
		t.Fatal("RunStatus.ExitCode = 0 for failing command")
	}
}

func TestInstallFilters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := Run(ctx, dir, "init", "--quiet"); err != nil {
		t.Fatal(err)
	}
	if err := InstallFilters(ctx, dir, "/usr/local/bin/agent-brain"); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"filter.agentbrain.required":  "true",
		"filter.agentbrain.clean":     `"/usr/local/bin/agent-brain" git-clean`,
		"filter.agentbrain.smudge":    `"/usr/local/bin/agent-brain" git-smudge`,
		"diff.agentbrain.textconv":    `"/usr/local/bin/agent-brain" git-textconv`,
		"merge.agentbrain.driver":     `"/usr/local/bin/agent-brain" git-merge --mode fact -- %O %A %B %P`,
		"merge.agentbrain-lww.driver": `"/usr/local/bin/agent-brain" git-merge --mode lww -- %O %A %B %P`,
		"merge.renormalize":           "true",
	} {
		result, err := Run(ctx, dir, "config", "--get", key)
		if err != nil {
			t.Fatalf("%s not set: %v", key, err)
		}
		if got := strings.TrimSpace(result.Stdout); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/gitx/ -v`
Expected: FAIL — `undefined: Run`.

- [ ] **Step 3: Implement** — `internal/gitx/gitx.go`:

```go
// Package gitx wraps system git — the engine's git backend (ADR 06: go-git
// cannot run filters or merge drivers, so v2 shells out).
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
)

// Result carries a finished git invocation's output and exit code.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Run executes git in dir and errors on any non-zero exit.
func Run(ctx context.Context, dir string, args ...string) (Result, error) {
	result, err := RunStatus(ctx, dir, args...)
	if err != nil {
		return result, err
	}
	if result.ExitCode != 0 {
		return result, fmt.Errorf("git %v exited %d: %s", args, result.ExitCode, result.Stderr)
	}
	return result, nil
}

// RunStatus executes git and reports the exit code as data — needed for
// commands like merge-file whose exit code is a count, not a failure.
func RunStatus(ctx context.Context, dir string, args ...string) (Result, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return result, nil
	case errors.As(err, &exitErr):
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	default:
		return result, fmt.Errorf("spawn git %v: %w", args, err)
	}
}
```

And `internal/gitx/install.go`:

```go
package gitx

import (
	"context"
	"fmt"
)

// InstallFilters writes the local .git/config wiring (spec §5). It runs on
// init/doctor on every machine and after every clone — .git/config is not
// versioned, so this is the only way the filter chain exists.
func InstallFilters(ctx context.Context, repoDir, binPath string) error {
	quoted := fmt.Sprintf("%q", binPath)
	settings := [][2]string{
		{"filter.agentbrain.clean", quoted + " git-clean"},
		{"filter.agentbrain.smudge", quoted + " git-smudge"},
		{"filter.agentbrain.required", "true"},
		{"diff.agentbrain.textconv", quoted + " git-textconv"},
		{"merge.agentbrain.name", "agent-brain fact merge (3-way + retain-both)"},
		{"merge.agentbrain.driver", quoted + " git-merge --mode fact -- %O %A %B %P"},
		{"merge.agentbrain-lww.name", "agent-brain newest-wins merge"},
		{"merge.agentbrain-lww.driver", quoted + " git-merge --mode lww -- %O %A %B %P"},
		{"merge.renormalize", "true"},
	}
	for _, setting := range settings {
		if _, err := Run(ctx, repoDir, "config", setting[0], setting[1]); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Run tests** — `go test ./internal/gitx/ -v` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gitx/
git commit -m "feat: gitx — system-git exec wrapper + filter/merge-driver config installer (spec §5)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 9: The merge driver — `git-merge --mode fact|lww` (spec §4)

The heart of concurrent-session safety. Git hands the driver STORED (ciphertext) base/current/other files; it decrypts all three, three-way merges the plaintext with `git merge-file`, rewrites any conflict hunks as retain-both blocks, re-encrypts into `%A`, and **exits 0 — always resolved** (spec §4: a rebase can never strand mid-conflict). `lww` mode keeps `%A` unchanged (under the engine's rebase flow `%A` is the upstream side; the regenerated class converges either way — spec §11 clock-skew row). Caveat, acceptable by design: if a merge ever runs outside the rebase flow (e.g. the spec §4 step 3 merge-commit fallback), `%A` is the LOCAL side and the lww winner flips — harmless for this class, because the provider regenerates the file anyway (spec §11).

Driver modes are `fact` and `lww` only: derived-index files merge as facts at the driver level, and the engine reconciles them post-merge (spec §4 step 4) — reconciliation is an engine step in Phase 2, not a driver mode.

**Files:**
- Create: `internal/crypto/retain.go`, `internal/crypto/retain_test.go` (pure conflict-hunk rewriter)
- Create: `internal/crypto/merge.go` (the merge endpoint — spec §8 puts it beside clean/smudge)
- Create: `internal/cli/merge.go`, `internal/cli/merge_test.go` (thin driver command: env + wiring only)
- Modify: `internal/cli/root.go` (register `git-merge`)

**Interfaces:**
- Consumes: `crypto.Codec` (Task 6), `gitx.RunStatus` (Task 8), `loadCodec` (Task 7).
- Produces:
  - `crypto.RewriteRetainBoth(merged []byte, labelA, labelB, timestamp string) (out []byte, hadConflicts bool)` — both labels are sanitized at this boundary (CR/LF and the marker/`-->` characters `<` `=` `>` → U+FFFD) so a hostile label cannot forge the block's parse anchors; well-behaved labels (hostnames, the defaults) pass through byte-for-byte (Q3 mandate).
  - `crypto.MergeFact(ctx context.Context, codec *Codec, basePath, currentPath, otherPath, pathname, labelA, labelB string) (hadConflicts bool, err error)` — Phase 2's engine reuses this directly
  - CLI command `git-merge --mode fact|lww -- <base> <current> <other> <pathname>`
  - Retain-both block format (STABLE — Phase 3's conflicts view parses it):

```markdown
<!-- agent-brain conflict <RFC3339-UTC>: both versions retained — keep what is right, then delete these comment lines (spec §4) -->
<!-- agent-brain version: <labelA> -->
...lines from side A...
<!-- agent-brain version: <labelB> -->
...lines from side B...
<!-- agent-brain conflict end -->
```

  - Labels default to `version A` / `version B`; env `AGENT_BRAIN_MERGE_LABEL_A` / `AGENT_BRAIN_MERGE_LABEL_B` override (the Phase 2 engine sets these to host names). Both are sanitized at the format boundary — inside `RewriteRetainBoth` and before git's `-L` markers in `MergeFact` — so a hostile override cannot forge conflict/anchor lines (Q3 mandate). Optional env `AGENT_BRAIN_CONFLICT_LOG`: when set, the driver appends one JSON line `{"time":...,"path":...,"mode":...}` per conflicted merge (best-effort; never fails the merge).

- [ ] **Step 1: Write the failing rewriter test** — `internal/crypto/retain_test.go`:

```go
package crypto

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestRewriteRetainBoth(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		input         string
		wantConflicts bool
		want          string
	}{
		{
			name:          "clean merge untouched",
			input:         "line 1\nline 2\n",
			wantConflicts: false,
			want:          "line 1\nline 2\n",
		},
		{
			name: "single hunk becomes retain-both block",
			input: "intro\n" +
				"<<<<<<< version A\n" +
				"fact from machine A\n" +
				"=======\n" +
				"fact from machine B\n" +
				">>>>>>> version B\n" +
				"outro\n",
			wantConflicts: true,
			want: "intro\n" +
				"<!-- agent-brain conflict 2026-07-07T00:00:00Z: both versions retained — keep what is right, then delete these comment lines (spec §4) -->\n" +
				"<!-- agent-brain version: version A -->\n" +
				"fact from machine A\n" +
				"<!-- agent-brain version: version B -->\n" +
				"fact from machine B\n" +
				"<!-- agent-brain conflict end -->\n" +
				"outro\n",
		},
		{
			name: "two hunks both rewritten",
			input: "<<<<<<< version A\na1\n=======\nb1\n>>>>>>> version B\n" +
				"mid\n" +
				"<<<<<<< version A\na2\n=======\nb2\n>>>>>>> version B\n",
			wantConflicts: true,
			want: "<!-- agent-brain conflict 2026-07-07T00:00:00Z: both versions retained — keep what is right, then delete these comment lines (spec §4) -->\n" +
				"<!-- agent-brain version: version A -->\na1\n" +
				"<!-- agent-brain version: version B -->\nb1\n" +
				"<!-- agent-brain conflict end -->\n" +
				"mid\n" +
				"<!-- agent-brain conflict 2026-07-07T00:00:00Z: both versions retained — keep what is right, then delete these comment lines (spec §4) -->\n" +
				"<!-- agent-brain version: version A -->\na2\n" +
				"<!-- agent-brain version: version B -->\nb2\n" +
				"<!-- agent-brain conflict end -->\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, hadConflicts := RewriteRetainBoth([]byte(test.input), "version A", "version B", "2026-07-07T00:00:00Z")
			if hadConflicts != test.wantConflicts {
				t.Fatalf("hadConflicts = %v, want %v", hadConflicts, test.wantConflicts)
			}
			if diff := cmp.Diff(test.want, string(got)); diff != "" {
				t.Fatalf("output mismatch (-want +got):\n%s", diff)
			}
			if strings.Contains(string(got), "<<<<<<<") {
				t.Fatal("git conflict markers leaked into output")
			}
		})
	}
}
```

Run `go get github.com/google/go-cmp@latest` first. Then `go test ./internal/crypto/ -run TestRewriteRetainBoth -v` → FAIL (`undefined: RewriteRetainBoth`).

- [ ] **Step 2: Implement the rewriter** — `internal/crypto/retain.go`:

```go
package crypto

import (
	"bytes"
	"fmt"
	"strings"
)

// RewriteRetainBoth converts `git merge-file` conflict hunks into
// retain-both blocks (spec §4): HTML-comment markers so the block is inert
// in rendered markdown, both versions in full, labels + timestamp for the
// conflicts view. Marker prefixes match merge-file's 7-char default style.
func RewriteRetainBoth(merged []byte, labelA, labelB, timestamp string) ([]byte, bool) {
	lines := strings.SplitAfter(string(merged), "\n")
	var out bytes.Buffer
	hadConflicts := false
	for i := 0; i < len(lines); i++ {
		if !strings.HasPrefix(lines[i], "<<<<<<< ") {
			out.WriteString(lines[i])
			continue
		}
		// Collect the hunk: ours until =======, theirs until >>>>>>>.
		var ours, theirs []string
		j := i + 1
		for ; j < len(lines) && !strings.HasPrefix(lines[j], "======="); j++ {
			ours = append(ours, lines[j])
		}
		k := j + 1
		for ; k < len(lines) && !strings.HasPrefix(lines[k], ">>>>>>> "); k++ {
			theirs = append(theirs, lines[k])
		}
		if j >= len(lines) || k >= len(lines) {
			// Malformed hunk (marker-like content): emit unchanged.
			out.WriteString(lines[i])
			continue
		}
		hadConflicts = true
		fmt.Fprintf(&out, "<!-- agent-brain conflict %s: both versions retained — keep what is right, then delete these comment lines (spec §4) -->\n", timestamp)
		fmt.Fprintf(&out, "<!-- agent-brain version: %s -->\n", labelA)
		out.WriteString(strings.Join(ours, ""))
		ensureNewline(&out)
		fmt.Fprintf(&out, "<!-- agent-brain version: %s -->\n", labelB)
		out.WriteString(strings.Join(theirs, ""))
		ensureNewline(&out)
		out.WriteString("<!-- agent-brain conflict end -->\n")
		i = k
	}
	return out.Bytes(), hadConflicts
}

func ensureNewline(buf *bytes.Buffer) {
	if buf.Len() > 0 && buf.Bytes()[buf.Len()-1] != '\n' {
		buf.WriteByte('\n')
	}
}
```

Run: `go test ./internal/crypto/ -v` → PASS (all three subtests + existing).

- [ ] **Step 3: Write the failing driver test** — `internal/cli/merge_test.go`:

```go
package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// writeEncrypted runs content through git-clean and writes it where the
// driver expects a stored (post-clean) version — exactly what git hands us.
func writeEncrypted(t *testing.T, path string, content []byte) {
	t.Helper()
	ciphertext, err := runCmd(t, content, "git-clean")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, ciphertext, 0o644); err != nil {
		t.Fatal(err)
	}
}

func decryptFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := runCmd(t, data, "git-smudge")
	if err != nil {
		t.Fatal(err)
	}
	return plaintext
}

func TestGitMergeFactClean(t *testing.T) {
	setupKeyset(t)
	dir := t.TempDir()
	base, current, other := filepath.Join(dir, "O"), filepath.Join(dir, "A"), filepath.Join(dir, "B")
	writeEncrypted(t, base, []byte("top\nmiddle\nbottom\n"))
	writeEncrypted(t, current, []byte("top EDITED\nmiddle\nbottom\n"))
	writeEncrypted(t, other, []byte("top\nmiddle\nbottom EDITED\n"))

	if _, err := runCmd(t, nil, "git-merge", "--mode", "fact", "--", base, current, other, "notes.md"); err != nil {
		t.Fatalf("driver must exit resolved on mergeable input: %v", err)
	}
	got := decryptFile(t, current)
	want := []byte("top EDITED\nmiddle\nbottom EDITED\n")
	if !bytes.Equal(got, want) {
		t.Fatalf("clean 3-way merge wrong:\n%s", got)
	}
}

func TestGitMergeFactOverlap(t *testing.T) {
	setupKeyset(t)
	dir := t.TempDir()
	base, current, other := filepath.Join(dir, "O"), filepath.Join(dir, "A"), filepath.Join(dir, "B")
	writeEncrypted(t, base, []byte("fact: original\n"))
	writeEncrypted(t, current, []byte("fact: version from machine A\n"))
	writeEncrypted(t, other, []byte("fact: version from machine B\n"))

	if _, err := runCmd(t, nil, "git-merge", "--mode", "fact", "--", base, current, other, "notes.md"); err != nil {
		t.Fatalf("driver must exit resolved even on overlap: %v", err)
	}
	got := string(decryptFile(t, current))
	for _, must := range []string{
		"agent-brain conflict",
		"fact: version from machine A",
		"fact: version from machine B",
		"agent-brain conflict end",
	} {
		if !bytes.Contains([]byte(got), []byte(must)) {
			t.Fatalf("retain-both output missing %q:\n%s", must, got)
		}
	}
	if bytes.Contains([]byte(got), []byte("<<<<<<<")) {
		t.Fatalf("git conflict markers leaked:\n%s", got)
	}
}

func TestGitMergeLwwKeepsCurrent(t *testing.T) {
	setupKeyset(t)
	dir := t.TempDir()
	base, current, other := filepath.Join(dir, "O"), filepath.Join(dir, "A"), filepath.Join(dir, "B")
	writeEncrypted(t, base, []byte("regenerated v1\n"))
	writeEncrypted(t, current, []byte("regenerated v2 (upstream)\n"))
	writeEncrypted(t, other, []byte("regenerated v2 (local replay)\n"))
	before, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, nil, "git-merge", "--mode", "lww", "--", base, current, other, "memory_summary.md"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("lww mode must leave %A unchanged")
	}
}
```

Run: `go test ./internal/cli/ -run TestGitMerge -v` → FAIL (`unknown command "git-merge"`).

- [ ] **Step 4: Implement the merge endpoint** — `internal/crypto/merge.go` (spec §8: crypto owns the endpoint; the CLI command in Step 5 is wiring only):

```go
package crypto

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/renameio/v2"

	"github.com/Sawmonabo/agent-brain/internal/gitx"
)

// MergeFact three-way merges STORED (post-clean, ciphertext) base/current/
// other files on their plaintext, rewrites true overlaps as retain-both
// blocks, and re-encrypts the result into currentPath (%A). On merge-able
// input it always succeeds — the driver must exit resolved so a rebase can
// never strand (spec §4). It returns an error ONLY for unexpected failure
// (unreadable input, wrong keyset, merge-file error), where the engine's
// fallback ladder takes over (spec §4 step 3) — and it never writes
// currentPath on that path, so no data is lost.
func MergeFact(ctx context.Context, codec *Codec, basePath, currentPath, otherPath, pathname, labelA, labelB string) (bool, error) {
	tempDir, err := os.MkdirTemp("", "agent-brain-merge-*")
	if err != nil {
		return false, err
	}
	defer func() { _ = os.RemoveAll(tempDir) }() // best-effort temp cleanup

	plaintextPaths := make([]string, 3)
	for i, storedPath := range []string{currentPath, basePath, otherPath} {
		plaintext, err := decryptLoose(codec, storedPath)
		if err != nil {
			return false, fmt.Errorf("decrypt %s side of %s: %w", []string{"current", "base", "other"}[i], pathname, err)
		}
		plaintextPaths[i] = filepath.Join(tempDir, fmt.Sprintf("side-%d", i))
		if err := os.WriteFile(plaintextPaths[i], plaintext, 0o600); err != nil {
			return false, err
		}
	}

	result, err := gitx.RunStatus(ctx, tempDir, "merge-file", "-p",
		"-L", labelA, "-L", "base", "-L", labelB,
		plaintextPaths[0], plaintextPaths[1], plaintextPaths[2])
	if err != nil {
		return false, err
	}
	// merge-file's exit value is the conflict count (bounded to 127) — but
	// negative-on-error surfaces through exec as >127 (e.g. 255 for binary
	// input). Treating that as "conflicts" would encrypt an EMPTY stdout
	// over %A and lose the file, so it must be an error instead.
	if result.ExitCode > 127 {
		return false, fmt.Errorf("git merge-file failed on %s: %s", pathname, result.Stderr)
	}

	merged := []byte(result.Stdout)
	hadConflicts := false
	if result.ExitCode > 0 {
		merged, hadConflicts = RewriteRetainBoth(merged, labelA, labelB,
			time.Now().UTC().Format(time.RFC3339))
	}
	ciphertext, err := codec.Encrypt(merged)
	if err != nil {
		return false, err
	}
	if err := renameio.WriteFile(currentPath, ciphertext, 0o644); err != nil {
		return false, err
	}
	return hadConflicts, nil
}

// decryptLoose tolerates what git actually hands a merge driver: empty temp
// files (add/add) and post-clean ciphertext; plain content passes through.
func decryptLoose(codec *Codec, path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || !IsEncrypted(data) {
		return data, nil
	}
	return codec.Decrypt(data)
}
```

- [ ] **Step 5: Implement the driver command** — `internal/cli/merge.go`:

```go
package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/Sawmonabo/agent-brain/internal/crypto"
)

func newGitMergeCmd() *cobra.Command {
	var mode string
	cmd := &cobra.Command{
		Use:    "git-merge --mode fact|lww -- <base> <current> <other> <pathname>",
		Short:  "Merge driver: 3-way merge on plaintext, retain-both on overlap (invoked by git)",
		Hidden: true,
		Args:   cobra.ExactArgs(4),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch mode {
			case "lww":
				return nil // keep %A: upstream side under the engine's rebase (spec §4, §11)
			case "fact":
				codec, err := loadCodec()
				if err != nil {
					return err
				}
				hadConflicts, err := crypto.MergeFact(cmd.Context(), codec,
					args[0], args[1], args[2], args[3],
					envOr("AGENT_BRAIN_MERGE_LABEL_A", "version A"),
					envOr("AGENT_BRAIN_MERGE_LABEL_B", "version B"))
				if err != nil {
					return err
				}
				if hadConflicts {
					logConflict(args[3])
				}
				return nil
			default:
				return fmt.Errorf("unknown --mode %q (want fact or lww)", mode)
			}
		},
	}
	cmd.Flags().StringVar(&mode, "mode", "fact", "merge policy: fact (3-way + retain-both) or lww (keep current)")
	return cmd
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// logConflict best-effort appends a JSON line for the Phase 3 conflicts
// view; it must never fail the merge, so errors are discarded.
func logConflict(pathname string) {
	logPath := os.Getenv("AGENT_BRAIN_CONFLICT_LOG")
	if logPath == "" {
		return
	}
	file, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = file.Close() }()
	line, err := json.Marshal(map[string]string{
		"time": time.Now().UTC().Format(time.RFC3339),
		"path": pathname,
		"mode": "fact",
	})
	if err != nil {
		return
	}
	_, _ = file.Write(append(line, '\n'))
}
```

In `internal/cli/root.go`, extend the registration line:

```go
	root.AddCommand(newGitCleanCmd(), newGitSmudgeCmd(), newGitTextconvCmd(), newGitMergeCmd())
```

- [ ] **Step 6: Run tests** — `go test ./internal/... -v` → ALL PASS.

- [ ] **Step 7: Add the merge fuzz targets** (spec §12 requires fuzzing the merge-driver three-way inputs) — append to `internal/crypto/fuzz_test.go`, extending its imports to `bytes`, `context`, `os`, `path/filepath`, `testing`:

```go
func FuzzRewriteRetainBoth(f *testing.F) {
	f.Add([]byte("plain\n"))
	f.Add([]byte("<<<<<<< A\nx\n=======\ny\n>>>>>>> B\n"))
	f.Add([]byte("<<<<<<< A\nunterminated"))
	f.Fuzz(func(t *testing.T, merged []byte) {
		out, _ := RewriteRetainBoth(merged, "A", "B", "2026-07-07T00:00:00Z")
		_ = out // must not panic; malformed hunks pass through unchanged
	})
}

// FuzzMergeFact drives the full driver path with arbitrary three-way inputs.
// Invariants: success ⇒ %A holds decryptable ciphertext with no leaked git
// markers; failure ⇒ %A is byte-identical to before (no data loss, spec §4).
func FuzzMergeFact(f *testing.F) {
	codec := newTestCodec(f)
	f.Add([]byte("base\n"), []byte("ours\n"), []byte("theirs\n"))
	f.Add([]byte(""), []byte("a\n"), []byte("b\n"))
	f.Add([]byte("x\ny\nz\n"), []byte("x\nY\nz\n"), []byte("x\ny\nZZ\n"))
	f.Fuzz(func(t *testing.T, base, ours, theirs []byte) {
		dir := t.TempDir()
		for name, plaintext := range map[string][]byte{"O": base, "A": ours, "B": theirs} {
			ciphertext, err := codec.Encrypt(plaintext)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), ciphertext, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		currentPath := filepath.Join(dir, "A")
		before, err := os.ReadFile(currentPath)
		if err != nil {
			t.Fatal(err)
		}
		_, mergeErr := MergeFact(context.Background(), codec,
			filepath.Join(dir, "O"), currentPath, filepath.Join(dir, "B"), "fuzz.md", "A", "B")
		after, err := os.ReadFile(currentPath)
		if err != nil {
			t.Fatal(err)
		}
		if mergeErr != nil {
			if !bytes.Equal(before, after) {
				t.Fatal("MergeFact errored AND modified %A — data-loss path")
			}
			return // e.g. merge-file rejects binary input; fallback ladder owns this
		}
		if !IsEncrypted(after) {
			t.Fatal("merge result is not ciphertext")
		}
		plaintext, err := codec.Decrypt(after)
		if err != nil {
			t.Fatal(err)
		}
		marker := []byte("<<<<<<<")
		inputsHaveMarkers := bytes.Contains(base, marker) ||
			bytes.Contains(ours, marker) || bytes.Contains(theirs, marker)
		if !inputsHaveMarkers && bytes.Contains(plaintext, marker) {
			t.Fatal("raw git conflict markers leaked from merge")
		}
	})
}
```

Run: `go test ./internal/crypto/ -fuzz FuzzRewriteRetainBoth -fuzztime 20s` → no crashers.
Run: `go test ./internal/crypto/ -fuzz FuzzMergeFact -fuzztime 30s` → no crashers (slower — each iteration execs git).

- [ ] **Step 8: Commit**

```bash
git add internal/crypto/ internal/cli/ go.mod go.sum
git commit -m "feat: merge driver — plaintext 3-way + retain-both blocks, always resolved (spec §4)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 10: Real-git integration harness + roundtrip & fail-closed proof (spec §12, ADR 15)

Everything so far tested our code calling our code. Now git itself drives the chain: a bare repo as the fake remote, clones as "machines", the built binary wired via `.git/config`. Zero network.

**Files:**
- Create: `test/e2e/harness_test.go`, `test/e2e/roundtrip_test.go`

**Interfaces:**
- Consumes: the built `agent-brain` binary (TestMain builds it), `gitx.InstallFilters`, `keys.Generate` (test/e2e imports `internal/` — same module).
- Produces (harness helpers Task 11 reuses): `newBareRepo(t) string`, `newMachine(t, name, bareURL string) string` (clone + git identity + filters + re-smudged worktree), `gitRun(t, dir string, args ...string) string` (fails test on error), `gitRunEnv(t, dir string, extraEnv []string, args ...string) (string, error)`, the `gitAttributes` const (memories-repo wiring incl. the `*.lww.md` newest-wins class), package globals `binPath string` (set in TestMain), `suiteCtx = context.Background()`.

- [ ] **Step 1: Write the harness** — `test/e2e/harness_test.go`:

```go
// Package e2e proves the filter/merge-driver chain through real git —
// the only way to test code that git invokes, not us (ADR 15).
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/keys"
)

var (
	binPath  string
	suiteCtx = context.Background()
)

// TestMain builds the binary once and creates the suite-wide shared keyset —
// one keyset across all "machines" is the shared-identity model (spec §5).
// os.Exit skips defers, so the real work (and the deferred cleanup) lives in
// testMain and the exit happens at top level.
func TestMain(m *testing.M) {
	os.Exit(testMain(m))
}

func testMain(m *testing.M) int {
	root, err := os.MkdirTemp("", "agent-brain-e2e-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(root) }()

	binPath = filepath.Join(root, "agent-brain")
	build := exec.Command("go", "build", "-o", binPath, "../../cmd/agent-brain")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build: %v\n%s", err, out)
		return 1
	}

	keysetDir := filepath.Join(root, "config")
	if err := keys.Generate(filepath.Join(keysetDir, "keyset.json")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// Process-wide: git-spawned filter processes inherit it.
	if err := os.Setenv("AGENT_BRAIN_CONFIG_DIR", keysetDir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	return m.Run()
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitRunEnv(t, dir, nil, args...)
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return out
}

func gitRunEnv(t *testing.T, dir string, extraEnv []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(suiteCtx, "git", args...)
	cmd.Dir = dir
	// Hermetic git (ADR 15): the developer's global/system config must not
	// leak in — commit.gpgsign would hang commits, hooksPath/defaultRemoteName
	// would corrupt the two-machine simulation. extraEnv comes last so tests
	// can still override anything (Go 1.19+: last duplicate wins).
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func newBareRepo(t *testing.T) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), "remote.git")
	gitRun(t, filepath.Dir(bare), "init", "--bare", "--initial-branch=main", bare)
	return bare
}

// gitAttributes is the memories-repo wiring (spec §5): everything filtered
// and binary-safe; `*.lww.md` is this harness's stand-in for the regenerated
// newest-wins class (spec §4 — Phase 2/3 generate the real per-provider
// patterns); the attributes file and .agent-brain/** metadata are excluded.
// Phase 2's repo package becomes the canonical home of this content.
const gitAttributes = "* filter=agentbrain diff=agentbrain merge=agentbrain -text\n" +
	"*.lww.md merge=agentbrain-lww\n" +
	".gitattributes -filter -diff -merge text eol=lf\n" +
	".agent-brain/** -filter -diff -merge text eol=lf\n"

func newMachine(t *testing.T, name, bareURL string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	gitRun(t, filepath.Dir(dir), "clone", "--quiet", bareURL, dir)
	gitRun(t, dir, "config", "user.name", name)
	gitRun(t, dir, "config", "user.email", name+"@test.invalid")
	if err := gitx.InstallFilters(suiteCtx, dir, binPath); err != nil {
		t.Fatal(err)
	}
	refreshWorktree(t, dir)
	return dir
}

// refreshWorktree forces a fresh smudge of every tracked file — required
// after InstallFilters, because clone checked files out before the filter
// wiring existed in .git/config.
func refreshWorktree(t *testing.T, dir string) {
	t.Helper()
	listed := strings.TrimSpace(gitRun(t, dir, "ls-files"))
	if listed == "" {
		return
	}
	for _, file := range strings.Split(listed, "\n") {
		if err := os.Remove(filepath.Join(dir, file)); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, dir, "checkout", "--", ".")
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// remoteBlob returns the STORED bytes of a file on the remote — what an
// attacker with GitHub access would see.
func remoteBlob(t *testing.T, bare, path string) string {
	t.Helper()
	return gitRun(t, bare, "cat-file", "blob", "main:"+path)
}
```

- [ ] **Step 2: Write the roundtrip + fail-closed tests** — `test/e2e/roundtrip_test.go`:

```go
package e2e

import (
	"strings"
	"testing"
)

func TestEncryptedRoundtripThroughRealGit(t *testing.T) {
	bare := newBareRepo(t)
	machineA := newMachine(t, "machine-a", bare)

	writeFile(t, machineA, ".gitattributes", gitAttributes)
	writeFile(t, machineA, "notes.md", "# memory\n\nthe launch code is swordfish\n")
	gitRun(t, machineA, "add", ".")
	gitRun(t, machineA, "commit", "--quiet", "-m", "memory: machine-a test 2026-07-07")
	gitRun(t, machineA, "push", "--quiet", "origin", "main")

	stored := remoteBlob(t, bare, "notes.md")
	if !strings.HasPrefix(stored, "agb1\x00") {
		t.Fatalf("remote blob lacks agent-brain magic; stored plaintext? %q", stored[:min(16, len(stored))])
	}
	if strings.Contains(stored, "swordfish") {
		t.Fatal("PLAINTEXT LEAKED TO REMOTE — filter chain broken")
	}

	machineB := newMachine(t, "machine-b", bare)
	if got := readFile(t, machineB, "notes.md"); !strings.Contains(got, "swordfish") {
		t.Fatalf("machine-b smudge failed; worktree content: %q", got)
	}
}

func TestFailClosedWithoutKeyset(t *testing.T) {
	bare := newBareRepo(t)
	machineA := newMachine(t, "machine-a", bare)
	writeFile(t, machineA, ".gitattributes", gitAttributes)
	writeFile(t, machineA, "notes.md", "secret\n")
	gitRun(t, machineA, "add", ".gitattributes")
	gitRun(t, machineA, "commit", "--quiet", "-m", "attributes only")

	// Point the filter at an empty config dir: no keyset.
	noKeyset := []string{"AGENT_BRAIN_CONFIG_DIR=" + t.TempDir()}

	// Clean must fail closed: required=true means git refuses the add.
	if out, err := gitRunEnv(t, machineA, noKeyset, "add", "notes.md"); err == nil {
		t.Fatalf("git add without keyset succeeded — plaintext could reach the repo:\n%s", out)
	}

	// Smudge must fail closed: checking out ciphertext without a keyset errors.
	gitRun(t, machineA, "add", "notes.md")
	gitRun(t, machineA, "commit", "--quiet", "-m", "memory: machine-a test 2026-07-07")
	if err := removeAndCheckout(t, machineA, "notes.md", noKeyset); err == nil {
		t.Fatal("checkout of ciphertext without keyset succeeded; want fail-closed error")
	}
}
```

Add the small helper to `harness_test.go`:

```go
func removeAndCheckout(t *testing.T, dir, name string, extraEnv []string) error {
	t.Helper()
	if err := os.Remove(filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
	_, err := gitRunEnv(t, dir, extraEnv, "checkout", "--", name)
	if err != nil {
		// Restore for subsequent assertions in the same test.
		gitRun(t, dir, "checkout", "--", name)
	}
	return err
}
```

- [ ] **Step 3: Run** — `go test ./test/e2e/ -v -run 'TestEncryptedRoundtrip|TestFailClosed'`
Expected: PASS. Debug tips if not: `GIT_TRACE=1` via `gitRunEnv`, and remember `min` needs Go 1.21+ (we are on 1.26 — fine).

- [ ] **Step 4: Commit**

```bash
git add test/
git commit -m "test: real-git integration — encrypted roundtrip + fail-closed proof (spec §12)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 11: The money test — two machines diverge and converge (spec §4, §12)

The reason v2 exists: concurrent edits on two machines never lose data. Real git, real rebase, real driver.

**Files:**
- Create: `test/e2e/divergence_test.go`

**Interfaces:**
- Consumes: every harness helper from Task 10.
- Produces: the executable proof of ADR 03's guarantee; Phase 2's engine tests extend this file's scenarios.

- [ ] **Step 1: Write the failing test** — `test/e2e/divergence_test.go`:

```go
package e2e

import (
	"strings"
	"testing"
)

// seedTwoMachines pushes an initial memory file from machine A and clones
// machine B at that state. Returns bare, machineA, machineB.
func seedTwoMachines(t *testing.T, seedContent string) (string, string, string) {
	t.Helper()
	bare := newBareRepo(t)
	machineA := newMachine(t, "machine-a", bare)
	writeFile(t, machineA, ".gitattributes", gitAttributes)
	writeFile(t, machineA, "notes.md", seedContent)
	gitRun(t, machineA, "add", ".")
	gitRun(t, machineA, "commit", "--quiet", "-m", "memory: machine-a seed 2026-07-07")
	gitRun(t, machineA, "push", "--quiet", "origin", "main")
	machineB := newMachine(t, "machine-b", bare)
	return bare, machineA, machineB
}

func TestDivergentOverlapRetainsBoth(t *testing.T) {
	bare, machineA, machineB := seedTwoMachines(t, "fact: original\n")

	writeFile(t, machineA, "notes.md", "fact: edited on machine A\n")
	gitRun(t, machineA, "add", "notes.md")
	gitRun(t, machineA, "commit", "--quiet", "-m", "memory: machine-a edit 2026-07-07")
	gitRun(t, machineA, "push", "--quiet", "origin", "main")

	writeFile(t, machineB, "notes.md", "fact: edited on machine B\n")
	gitRun(t, machineB, "add", "notes.md")
	gitRun(t, machineB, "commit", "--quiet", "-m", "memory: machine-b edit 2026-07-07")

	// The moment of truth: rebase must complete WITHOUT stranding — the
	// driver always resolves (spec §4).
	gitRun(t, machineB, "pull", "--rebase", "--quiet", "origin", "main")

	merged := readFile(t, machineB, "notes.md")
	for _, must := range []string{
		"agent-brain conflict",
		"fact: edited on machine A",
		"fact: edited on machine B",
		"agent-brain conflict end",
	} {
		if !strings.Contains(merged, must) {
			t.Fatalf("retain-both missing %q; merged file:\n%s", must, merged)
		}
	}
	if strings.Contains(merged, "<<<<<<<") {
		t.Fatalf("raw git conflict markers leaked:\n%s", merged)
	}

	// Converge machine A onto the retained result and verify equality.
	gitRun(t, machineB, "push", "--quiet", "origin", "main")
	gitRun(t, machineA, "pull", "--rebase", "--quiet", "origin", "main")
	if got := readFile(t, machineA, "notes.md"); got != merged {
		t.Fatalf("machines diverge after sync:\nA: %q\nB: %q", got, merged)
	}

	// And the wire never saw plaintext, even through the driver's re-encrypt.
	stored := remoteBlob(t, bare, "notes.md")
	if strings.Contains(stored, "edited on machine") {
		t.Fatal("PLAINTEXT LEAKED TO REMOTE after merge-driver rewrite")
	}
}

func TestDivergentNonOverlapMergesClean(t *testing.T) {
	_, machineA, machineB := seedTwoMachines(t, "top\n\nmiddle\n\nbottom\n")

	writeFile(t, machineA, "notes.md", "top EDITED A\n\nmiddle\n\nbottom\n")
	gitRun(t, machineA, "add", "notes.md")
	gitRun(t, machineA, "commit", "--quiet", "-m", "memory: machine-a top 2026-07-07")
	gitRun(t, machineA, "push", "--quiet", "origin", "main")

	writeFile(t, machineB, "notes.md", "top\n\nmiddle\n\nbottom EDITED B\n")
	gitRun(t, machineB, "add", "notes.md")
	gitRun(t, machineB, "commit", "--quiet", "-m", "memory: machine-b bottom 2026-07-07")
	gitRun(t, machineB, "pull", "--rebase", "--quiet", "origin", "main")

	merged := readFile(t, machineB, "notes.md")
	want := "top EDITED A\n\nmiddle\n\nbottom EDITED B\n"
	if merged != want {
		t.Fatalf("clean 3-way through real git wrong:\n%q\nwant:\n%q", merged, want)
	}
	if strings.Contains(merged, "agent-brain conflict") {
		t.Fatal("non-overlapping edits produced a retain-both block")
	}
}

// TestDivergentLwwKeepsUpstream drives the newest-wins class through a real
// rebase (spec §12): `*.lww.md` maps to merge.agentbrain-lww, whose driver
// keeps %A — the upstream side under pull --rebase. Machine B's replayed
// edit dissolves (its commit becomes empty and the rebase drops it), so B
// converges to A's regenerated copy with no conflict block.
func TestDivergentLwwKeepsUpstream(t *testing.T) {
	bare := newBareRepo(t)
	machineA := newMachine(t, "machine-a", bare)
	writeFile(t, machineA, ".gitattributes", gitAttributes)
	writeFile(t, machineA, "summary.lww.md", "regenerated v1\n")
	gitRun(t, machineA, "add", ".")
	gitRun(t, machineA, "commit", "--quiet", "-m", "memory: machine-a seed 2026-07-07")
	gitRun(t, machineA, "push", "--quiet", "origin", "main")
	machineB := newMachine(t, "machine-b", bare)

	writeFile(t, machineA, "summary.lww.md", "regenerated on machine A\n")
	gitRun(t, machineA, "add", "summary.lww.md")
	gitRun(t, machineA, "commit", "--quiet", "-m", "memory: machine-a regen 2026-07-07")
	gitRun(t, machineA, "push", "--quiet", "origin", "main")

	writeFile(t, machineB, "summary.lww.md", "regenerated on machine B\n")
	gitRun(t, machineB, "add", "summary.lww.md")
	gitRun(t, machineB, "commit", "--quiet", "-m", "memory: machine-b regen 2026-07-07")
	gitRun(t, machineB, "pull", "--rebase", "--quiet", "origin", "main")

	merged := readFile(t, machineB, "summary.lww.md")
	if merged != "regenerated on machine A\n" {
		t.Fatalf("lww through a real rebase must keep the upstream side, got %q", merged)
	}
	if strings.Contains(merged, "agent-brain conflict") || strings.Contains(merged, "<<<<<<<") {
		t.Fatalf("lww must never produce conflict blocks:\n%s", merged)
	}
	stored := remoteBlob(t, bare, "summary.lww.md")
	if strings.Contains(stored, "regenerated") {
		t.Fatal("PLAINTEXT LEAKED TO REMOTE on the lww path")
	}
}
```

- [ ] **Step 2: Run** — `go test ./test/e2e/ -v`
Expected: ALL PASS. If the rebase strands in conflict (exit into `REBASE_HEAD` state), the driver wiring is wrong — check `git config --get merge.agentbrain.driver` inside the machine dir and that `.gitattributes` reached the clone; the harness's `refreshWorktree` must have run.

- [ ] **Step 3: Run the full suite exactly as CI does**

Run: `go test ./... -race && golangci-lint run && test -z "$(gofumpt -l .)"`
Expected: clean. Fix anything here, not in CI.

- [ ] **Step 4: Commit and push**

```bash
git add test/
git commit -m "test: two-machine divergence converges via retain-both — the v2 core guarantee (spec §4, ADR 03)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
git push origin develop
gh run watch --exit-status
```

---

## Phase 1 exit criteria

All must hold before writing the Phase 2 plan:

1. `go test ./... -race` green on macOS locally AND in the macos+ubuntu CI matrix.
2. `TestDivergentOverlapRetainsBoth` proves: rebase never strands, both versions retained, machines converge, zero plaintext in any git object.
3. `TestDivergentLwwKeepsUpstream` proves the newest-wins class resolves to the upstream side through a real rebase (spec §12's "newest-wins classes" leg).
4. `TestFailClosedWithoutKeyset` proves both directions of `filter.required` fail-closed.
5. All three fuzz targets — `FuzzRoundtrip`, `FuzzRewriteRetainBoth`, `FuzzMergeFact` — ran ≥20s with no crashers.
6. Legacy `home/`, `tools/`, chezmoi files gone from `develop`; `main` untouched.

**Phase 2 preview** (plan written after this phase lands): `internal/repo` (layout, `projects.toml` registry, per-host manifests, canonical `.gitattributes` generation), `internal/engine` (mirror-in/commit/integrate/reconcile/mirror-out/push loop), `internal/watch`, `internal/daemon` + UDS API, `internal/service`. The harness from Tasks 10–11 grows engine-driven scenarios.




