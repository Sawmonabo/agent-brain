# agent-brain v2 — Phase 3: Provider Adapters, Product CLI & Init — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn Phase 2's engine/daemon into a usable product: real `claude` and `codex` adapters, `agent-brain init` (gh provisioning + keyset + service + enrollment), `track`/`untrack`, `migrate`, `doctor`, `key export|import`, `conflicts`, `--json` read surfaces — plus the engine/daemon hardening the Phase-2 final review handed off (post-integrate scrub, watcher rebuild, log/conflict-log bounds).

**Architecture:** The single-writer rule (ADR 03) now becomes an API-shape constraint: **every mutation of the memories checkout — enrollment's `projects.toml` write, migrate's seed commits, untrack --purge — happens inside the daemon's engine goroutine**, reached via new UDS endpoints (`POST /v0/track|untrack|migrate`). The CLI is a thin client: it runs discovery and interactive pickers, then submits a resolved plan to the daemon. Read-only work (doctor checks, key export, discovery scans) stays CLI-side. `internal/ghx` isolates every gh subprocess behind a fakeable runner; `internal/doctor` is a package (not just a command) so the daemon reuses its safety-critical subset as the sync gate (spec §5: "the daemon refuses to sync until doctor passes").

**Tech Stack:** Go 1.26 (toolchain go1.26.5) · Phase-1/2 packages (`crypto`, `gitx`, `keys`, `config`, `provider`, `repo`, `engine`, `watch`, `daemon`, `service`, `cli`) · **charm.land/huh/v2 v2.0.3** (forms; pulls bubbletea v2 + lipgloss v2 transitively) · rogpeppe/go-internal (testscript) · gh CLI ≥ v2.40 at runtime (verified against v2.96.0 flags) · system git.

**Phase roadmap** (this is plan 3 of 4; scope split approved 2026-07-08: P3 = product, P4 = cutover):

1. **Phase 1 (done):** greenfield reset, module + CI/tooling, config paths, keys, crypto codec, filter/merge plumbing, real-git integration proof.
2. **Phase 2 (done, develop=aec99c9):** repo layout/registries/manifests, mirror in/out, sync engine, watch manager, daemon + UDS API + service install.
3. **Phase 3 (this plan):** claude/codex adapters + index reconcile, post-integrate scrub hardening, ghx provisioning, daemon API growth (track/untrack/migrate), doctor package + sync gate, init wizard, track/migrate/key/conflicts CLI, testscript e2e + standing adversarial probe.
4. **Phase 4 (cutover, planned just-in-time):** per-machine ab-claude retirement + verified migration everywhere, GoReleaser + Homebrew tap, onboarding/WSL2 runbooks, ADR-13 history scrub, develop→main merge. **The `dashboard` command (bubbletea multi-view app) is deliberately NOT in Phase 3** — every API/data seam it needs (controller endpoints, conflicts log, unit health) ships here; whether it lands in Phase 4 or v1.1 is a Phase-4 planning decision. `key rotate` remains v1.1 (spec §5).

Spec: `docs/00-design-spec.md` (§ references below). ADRs: `docs/decisions/`. Phase-2 handoff items (final review + C1 re-review) are folded into Tasks 1, 6, 7, 9, 12 — none are re-deferred.

## Global Constraints

Every task implicitly includes these. Version pins verified against primary sources 2026-07-08.

- Branch: ALL work lands on `develop`. Never commit to `main`.
- Module: `module github.com/Sawmonabo/agent-brain`; `go 1.26`; `toolchain go1.26.5`.
- New dependency pins: **charm.land/huh/v2 v2.0.3** (module path is charm.land, NOT github.com/charmbracelet — same vanity move as fang v2; huh self-hosts its forms via `form.Run()`, no manual bubbletea program). testscript comes from **github.com/rogpeppe/go-internal** — pin the latest release at `go get` time and record the resolved version in the task's commit body (`go list -m github.com/rogpeppe/go-internal`). No direct lipgloss/bubbletea imports — if you find yourself importing them, stop (YAGNI; fang already styles help/errors).
- gh CLI: runtime dependency, NEVER vendored, NEVER `gh auth setup-git` (ADR 08 — it writes an absolute gh path into the user's *global* gitconfig, which breaks synced dotfiles; our repo-LOCAL config in the hidden checkout may hold an absolute path because it never leaves the machine). Flag surface used: `gh --version`, `gh auth status`, `gh api user --jq .login`, `gh repo view OWNER/NAME --json name`, `gh repo create NAME --private --description ...` (prints the repo URL on stdout), `gh repo clone OWNER/NAME DIR -- ...` (git passthrough args after `--`), `gh auth git-credential` (gh's own credential helper, invoked BY git). All verified against gh v2.96.0 (2026-07-02).
- All packages under `internal/`; `cmd/agent-brain` stays thin. No `pkg/`.
- Package boundaries (spec §8): `engine` imports `gitx`/`provider`/`repo` (+ stdlib/renameio) ONLY — never `cli`, `daemon`, `watch`, `crypto`, `ghx`, or `doctor`. `daemon/api` is the only daemon↔CLI shared surface and imports nothing internal. New boundaries: `ghx` imports stdlib only (it shells out); `doctor` may import `config`/`keys`/`repo`/`provider`/`gitx`/`ghx` but NEVER `daemon` or `cli` (the daemon imports doctor, not vice versa); provider adapters (`provider/claude`, `provider/codex`) import `provider` + `gitx` + renameio + stdlib only (amended during Task 3 to match the briefs' binding boundary amendment: `Identify` invokes git through `gitx.RunStatus`, `ReconcileIndex` writes atomically through renameio).
- **Single-writer invariant (ADR 03), Phase-3 form: the CLI process NEVER writes inside the memories checkout.** Enrollment, seeding, purging — anything that touches `projects.toml`, project folders, or git state — is submitted over the UDS API and executed by the daemon's one engine goroutine. The ONLY exception is `init` creating the checkout before any daemon could know it exists (clone into a temp sibling + atomic rename, Task 10) and `doctor --fix` re-wiring `.git/config` + `.gitattributes` (idempotent, byte-deterministic repairs, safe against a concurrent cycle because git config writes are atomic and WriteAttributes is renameio).
- Tests: stdlib `testing` + `go-cmp` ONLY (ADR 15). Table-driven, `t.Parallel()`, `t.TempDir()`. Real system git with `git init --bare` fake remotes; **no network, ever** — gh is faked (`ghx.Runner` test double at unit level; an executable `gh` shim script on a test-owned PATH in testscript e2e). **No live service installs** in any test. All paths via `AGENT_BRAIN_CONFIG_DIR`/`AGENT_BRAIN_DATA_DIR`/`AGENT_BRAIN_RUNTIME_DIR` env overrides — never real home paths. TUI code paths are exercised in huh's accessible mode (`WithAccessible(true)` → plain sequential stdin/stdout — CI-safe); interactive-only rendering is NOT unit-tested, so keep it logic-free.
- Safety invariants (spec §5, §11): the Tink keyset NEVER enters any repo; plaintext memory content NEVER reaches a git object (e2e asserts ciphertext on the wire); `filter.agentbrain.required=true` fail-closed; **git-meta scrub contract** — any code path that writes files into the checkout scrubs `.gitattributes`/`.gitignore`/`.git` from its target subtree first (the SECURITY CONTRACT comment at sync.go's post-integrate block is binding; Tasks 1, 3, 7 implement/honor it); **codex secret-adjacency** — the adapter watches/mirrors ONLY `$CODEX_HOME/memories/` and `$CODEX_HOME/memories_extensions/chronicle/`, never `$CODEX_HOME` itself (`auth.json` lives there).
- Formatting/lint unchanged: gofumpt; golangci-lint v2.12.2; every `//nolint` carries linter + reason. Conventional Commits; one commit per task minimum. lefthook pre-push runs `go test ./... -race` — budget for it.
- The age key, `~/.config/agent-brain/key.txt`, and `main`'s bash system stay untouched. `agent-brain migrate` READS `~/.agent-brain/<slug>/` and never writes there.

---

### Orientation: what Phases 1–2 already provide

Phase 3 consumes these existing surfaces — import them, never re-implement:

- `internal/provider` — `Provider` interface (`Name`, `Scope`, `Patterns`, `ReconcileIndex`), `Classify(p, rel) Class` (first match wins, default `ClassFact`), `Match`/`ValidateGlob` (segment globs; `**` middle = zero+ segments, trailing = one+), `NewRegistry`, `providertest.Fake`.
- `internal/repo` — `Layout` (`UnitDir(folder, provider)`, `ProjectsFile`, `ManifestFile`, `AttributesFile`), `Projects` registry (`Add` disambiguates `folder-2`, `folder-3`…; `FolderFor`), `LocalRegistry` (`Enroll`/`Remove`/`Save`, strict load), `Unit{Provider, ProjectID, Folder, LocalDir}`, `Manifest` (per-host JSON ledger; keys are checkout-root-relative paths), `ValidateFolderName` (rejects `_`/`.` starts, reserved names), `ValidateRelPath`, `SanitizeHostname`, `GenerateAttributes`/`WriteAttributes` (canonical `.gitattributes`; emits `**/<provider>/<glob> merge=agentbrain-lww` for `ClassRegenerated` rows), `GlobalFolder` + `MetaDirName` consts.
- `internal/engine` — `New(checkout, host, registry, now)`, `Sync(ctx, units)` (recover → mirror-in → commit → integrate → reconcile → mirror-out → commit meta → push), `isGitMetaPath(rel)` + `scrubCheckoutFile` (mirror_in.go — the C1 defense), consts `remoteName="origin"`, `defaultBranch="main"`, `upstreamRef`. Degraded units are per-folder; mirror-out is withheld for degraded folders.
- `internal/daemon` — one engine goroutine (`loop`), `runCycle` **reloads `LocalRegistry` every cycle** (enrollment applies without restart — only the *watcher* lags; Task 6 fixes that), `checkoutState()` (bare `.git` stat — Task 8 replaces it with the doctor safety gate), UDS server with peer-UID enforcement, `SocketPathForClient()`.
- `internal/daemon/api` — typed `Client` (`Status`/`Sync`/`Projects`, private `do(ctx, method, path, out)`), response types. Task 7 grows both sides.
- `internal/config` — `Paths` + env overrides, `Settings` (strict TOML; unknown key = error), `RuntimeDir`, `ValidateSocketPath`.
- `internal/keys` — `Generate`, `Export` (armored std-base64), `Import`, `Primitive`, `ErrKeysetExists`.
- `internal/gitx` — `Run`/`RunStatus` (exit code as data), `InstallFilters(ctx, repoDir, binPath)` (clean/smudge/textconv/merge + `required=true`).
- `internal/service` — `Controller` interface (`NewController(binaryPath)`), install/start/stop/status; `IsWSL2()`.
- `internal/watch` — `Manager` (`New(Config{Debounce, Poll})`, `Add(root)` **before** `Run` only — the pump goroutine owns all watch state once running; rebuilding = new Manager), `Triggers()`.
- `test/e2e` — real-git two-machine harness: `newBareRepo`, `newMachine`, `gitRun`/`gitRunEnv` (hermetic `GIT_CONFIG_GLOBAL/SYSTEM=/dev/null`), `remoteBlob`, `binPath`, `TestMain` (builds the binary once).
- `internal/cli` — cobra root (fang-wrapped in `cmd/agent-brain`), hidden git plumbing commands, `daemon`/`service`/`status`/`sync`/`projects`, `newAPIClient`, `explainDown`, `reportWriter`.

Exact signatures for anything you touch: read the package source first; the briefs quote the load-bearing ones.

---

### Task 1: Engine hardening — post-integrate checkout scrub + root-attributes heal (M1+M2 one design)

The Phase-2 C1 fix scrubs git-meta from **unit dirs during mirror-in**. Two residues were accepted then and become urgent now: (M2) integrate can deliver remote-poisoned git-meta into the tree, and the moment reconcile *writes* files (Task 3's claude `ReconcileIndex`), the post-integrate `git add` would consult that poisoned worktree `.gitattributes` — the SECURITY CONTRACT comment at sync.go's post-integrate block demands a scrub first; (M1) the mirror-in scrub is unit-scoped, so a *folder-level* `.gitattributes` (e.g. `<project>/.gitattributes`, one level above the unit dir) delivered by a hostile push survives. One design closes both: after every successful integrate, walk the whole checkout and (a) delete every git-meta path outside the repo root, (b) verify the ROOT `.gitattributes` is byte-identical to `GenerateAttributes(registry)` and heal it if not (a hostile push can rewrite the root file to unscope the encryption filter — the worktree copy is what `git add` consults).

**Files:**
- Create: `internal/engine/scrub.go`
- Modify: `internal/engine/sync.go` (call the scrub inside the `if integ.Integrated {` block, BEFORE reconcile; update the SECURITY CONTRACT comment to point at the implementation)
- Test: `internal/engine/scrub_test.go`

**Interfaces:**
- Consumes: `isGitMetaPath(rel)`, `scrubCheckoutFile(ctx, checkout, rel)` (both in mirror_in.go), `repo.GenerateAttributes(reg)`, `repo.NewLayout`, `gitx.Run`.
- Produces: `func (e *Engine) scrubIntegrated(ctx context.Context) (healed []string, err error)` — called only from `Sync`; returns the repo-relative paths it removed/healed so the cycle log names them. Task 3's `ReconcileIndex` and Task 7's `SeedProject` rely on this running before any post-integrate write+add.

- [ ] **Step 1: Write the failing tests** — `internal/engine/scrub_test.go`. Reuse the package's existing test helpers (`mustGit`, engine construction with `providertest.Fake` — read `mirror_in_test.go` first and follow its idioms exactly):

```go
package engine

// TestScrubIntegratedRemovesDeepGitMeta plants git-meta at three depths a
// hostile PUSH could deliver (folder level, unit level, nested) plus a
// legitimate memory file, simulates their arrival via a tracked commit,
// and asserts scrubIntegrated deletes every git-meta path — including the
// folder-level one mirror-in's unit-scoped pass cannot see — while the
// memory file and the repo's own root .gitattributes survive.
func TestScrubIntegratedRemovesDeepGitMeta(t *testing.T) { ... }

// TestScrubIntegratedHealsRootAttributes overwrites the checkout's root
// .gitattributes with a hostile unscoped version (no filter lines),
// commits it as if integrate delivered it, and asserts scrubIntegrated
// rewrites it byte-identical to repo.GenerateAttributes(registry) and
// stages the heal.
func TestScrubIntegratedHealsRootAttributes(t *testing.T) { ... }

// TestScrubIntegratedNoopOnCleanTree asserts a clean checkout yields
// healed == nil and leaves `git status --porcelain` empty (no commit
// churn on the happy path — determinism matters: every cycle runs this).
func TestScrubIntegratedNoopOnCleanTree(t *testing.T) { ... }
```

Write them fully (no stubs): each test builds a real git checkout in `t.TempDir()` the way `mirror_in_test.go` does, plants files with `mustGit` add/commit so they are TRACKED (integrate-delivered state, not untracked scratch), calls `scrubIntegrated`, then asserts with `git ls-files` + `os.Stat` + go-cmp. The root-attributes test must compare healed bytes with `repo.GenerateAttributes` output exactly, not a substring.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/engine/ -run TestScrubIntegrated -v`
Expected: FAIL — `undefined: (*Engine).scrubIntegrated` (compile error is the RED state here).

- [ ] **Step 3: Implement `internal/engine/scrub.go`**

```go
package engine

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// scrubIntegrated enforces the git-meta invariant over the WHOLE checkout
// after integrate (SECURITY CONTRACT, sync.go): a hostile push can deliver
// .gitattributes/.gitignore at any depth — including folder level, one
// above the unit dirs mirror-in's scrub covers — and the worktree copy is
// what `git add` consults for filter attributes. It also re-canonicalizes
// the ROOT .gitattributes: that file is ours (generated), and a pushed
// mutation of it could unscope the encryption filter for every later add.
//
// Deletions and the root heal are staged via scrubCheckoutFile /
// `git add`; the caller's existing post-integrate commitProjects commits
// them, so healed state propagates to other machines like any other fix.
func (e *Engine) scrubIntegrated(ctx context.Context) ([]string, error) {
	var healed []string
	root := e.layout.Root()
	err := filepath.WalkDir(root, func(abs string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(root, abs)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		// The checkout's own git dir is not repo content.
		if rel == ".git" {
			return filepath.SkipDir
		}
		// The root .gitattributes is legitimate (generated); its content
		// is verified below. Everything git-meta ANYWHERE else goes.
		if rel == ".gitattributes" {
			return nil
		}
		if !isGitMetaPath(rel) {
			return nil
		}
		if entry.IsDir() {
			// A directory named .git (nested repo smuggling) — remove the
			// whole tree, then skip walking into what no longer exists.
			if err := os.RemoveAll(abs); err != nil {
				return fmt.Errorf("scrub dir %s: %w", rel, err)
			}
			if err := stageRemoval(ctx, e.checkout, rel); err != nil {
				return err
			}
			healed = append(healed, rel)
			return filepath.SkipDir
		}
		if err := scrubCheckoutFile(ctx, e.checkout, rel); err != nil {
			return err
		}
		healed = append(healed, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("post-integrate scrub: %w", err)
	}

	canonical := repo.GenerateAttributes(e.registry)
	attrsPath := e.layout.AttributesFile()
	current, readErr := os.ReadFile(attrsPath) //nolint:gosec // G304: path derives from the engine's own Layout, not untrusted input
	if readErr != nil || string(current) != canonical {
		if err := repo.WriteAttributes(e.layout, e.registry); err != nil {
			return nil, fmt.Errorf("post-integrate scrub: heal root attributes: %w", err)
		}
		if _, err := gitxAdd(ctx, e.checkout, ".gitattributes"); err != nil {
			return nil, err
		}
		healed = append(healed, ".gitattributes")
	}
	return healed, nil
}
```

Notes for the implementer (bind these into the code, not comments-to-reviewer):
- `scrubCheckoutFile` already handles the tracked/untracked split (`git rm` vs plain remove) — reuse it verbatim for files. Add the small helpers this file needs (`stageRemoval` for directories via `git rm -r --cached --ignore-unmatch -- <rel>` + `gitxAdd` wrapping `gitx.Run(ctx, dir, "add", "--", rel)`) in scrub.go if mirror_in.go doesn't already export equivalents — read mirror_in.go FIRST and reuse whatever exists; do not duplicate an idiom that is already there.
- `e.registry`, `e.layout`, `e.checkout` are the existing Engine fields (engine.go) — no new state.
- Determinism: on a clean tree the function must not touch the index at all (the no-op test pins this — `git status --porcelain` stays empty).

- [ ] **Step 4: Wire into `Sync`** — in `internal/engine/sync.go`, inside the `if integ.Integrated {` block and BEFORE the reconcile call, insert:

```go
		healed, err := e.scrubIntegrated(ctx)
		if err != nil {
			return report, err
		}
		report.Scrubbed = append(report.Scrubbed, healed...)
```

Add `Scrubbed []string` to `engine.Report` (engine.go) with a doc comment: `// Scrubbed lists git-meta paths removed (or the root .gitattributes healed) after integrate — nonzero means a remote pushed something hostile or corrupted.` Rewrite the SECURITY CONTRACT comment at the block to state the contract is now ENFORCED by scrubIntegrated and that any new post-integrate writer (reconcile, seed) is covered only if it stays inside this block's ordering (scrub → write → commit).

- [ ] **Step 5: Run the engine suite**

Run: `go test ./internal/engine/ -race`
Expected: PASS, including the three new tests and every existing mirror/sync test (the no-op guarantee keeps existing commit-subject assertions intact — if any existing test fails, the scrub is not idempotent: fix the scrub, not the test).

- [ ] **Step 6: Commit**

```bash
git add internal/engine/scrub.go internal/engine/scrub_test.go internal/engine/sync.go internal/engine/engine.go
git commit -m "feat(engine): post-integrate checkout-wide git-meta scrub + root attributes heal

Closes Phase-2 handoff M1+M2 with one design: hostile git-meta at ANY
depth (incl. folder level) is deleted after every integrate, and the
root .gitattributes is healed byte-identical to the canonical
generation before any post-integrate write can git-add against it."
```

---

### Task 2: Provider contract v3 — discovery/identity methods + `Unit.RepoSubdir` plumbing

Enrollment needs the two interface methods deferred from Phase 2 (spec §6: `DiscoverProjects`, `ResolveIdentity`), and the codex adapter needs a way to bind TWO local roots to subdirectories of one provider dir (`_global/codex/memories`, `_global/codex/chronicle` — the spec §3 layout) without violating the one-LocalDir-per-Unit model. This task extends the contract and threads `RepoSubdir` through `repo.Unit` and the engine; adapters land in Tasks 3–4.

**The pattern-namespace contract (load-bearing, repeated in Tasks 3/4/12):** a provider's `Patterns()` globs are relative to the provider's REPO dir (`<folder>/<provider>/`), NOT to a unit's local root. `GenerateAttributes` already emits `**/<provider>/<glob>` — with subdir units, mirror-time classification must agree with those attribute rows, so the engine classifies `path.Join(unit.RepoSubdir, relFromLocalRoot)`. For claude (`RepoSubdir == ""`) this is the Phase-2 behavior unchanged.

**Files:**
- Modify: `internal/provider/provider.go` (interface + new types)
- Modify: `internal/provider/providertest/fake.go` (implement new methods, recording calls)
- Modify: `internal/repo/local.go` (`Unit.RepoSubdir` + validation)
- Modify: `internal/engine/mirror_in.go`, `internal/engine/mirror_out.go`, `internal/engine/reconcile.go` (unit dir + classify-rel resolution)
- Create: `internal/engine/unitpath.go`
- Test: `internal/provider/provider_test.go`, `internal/repo/local_test.go`, `internal/engine/unitpath_test.go` (additions to existing files where they exist)

**Interfaces:**
- Consumes: existing `provider.Provider`, `repo.Unit`, `repo.Layout.UnitDir`, `provider.Classify`.
- Produces (Tasks 3, 4, 7, 10, 11 rely on these exact shapes):

```go
// In internal/provider/provider.go:

// Discovered is one enrollable memory root an adapter found on this
// machine. For per-project providers each project yields one entry; a
// global provider may yield several (one per RepoSubdir root).
type Discovered struct {
	// LocalDir is the absolute local memory root to mirror/watch.
	LocalDir string
	// RepoSubdir is the slash-separated subdir under <folder>/<provider>/
	// this root maps to ("" = the provider dir itself). Mirrors
	// repo.Unit.RepoSubdir.
	RepoSubdir string
	// Label is what the enrollment picker shows (a slug, "codex memories", …).
	Label string
	// PathGuess is the adapter's best guess at the PROJECT path the memory
	// belongs to (per-project scope; "" for global). The picker shows it
	// for confirmation — it is a GUESS (slug reversal is lossy).
	PathGuess string
}

// Identity is the cross-machine binding for one discovered root
// (spec §3 "Project identity"). Global-scope providers return the zero
// Identity — their folder is repo.GlobalFolder by construction.
type Identity struct {
	// ProjectID is the canonical machine-independent id
	// (host/owner/repo from the normalized git remote), or "" when the
	// project has no remote — the caller must then ask the user to name
	// the folder and uses name:<folder> as the id.
	ProjectID string
	// PreferredFolder is the repo folder to propose (repo basename).
	PreferredFolder string
}

type Provider interface {
	Name() string
	Scope() Scope
	Patterns() []Pattern
	ReconcileIndex(ctx context.Context, dir string) error
	// Discover enumerates this machine's enrollable memory roots. Roots
	// already enrolled are included — the caller filters against its
	// registry (the adapter is stateless).
	Discover(ctx context.Context) ([]Discovered, error)
	// Identify resolves a Discovered root to its cross-machine identity,
	// confirming/deriving the project path (reads the git remote for
	// per-project scope). projectPath is the user-confirmed project
	// directory (equal to d.PathGuess unless the user corrected it).
	Identify(ctx context.Context, d Discovered, projectPath string) (Identity, error)
}
```

```go
// In internal/repo/local.go — Unit grows one field:
type Unit struct {
	Provider   string `toml:"provider"`
	ProjectID  string `toml:"project_id"`
	Folder     string `toml:"folder"`
	LocalDir   string `toml:"local_dir"`
	// RepoSubdir maps this unit under a subdirectory of the provider dir
	// (<folder>/<provider>/<repo_subdir>). Empty for providers with one
	// root (claude). Validated by ValidateRelPath when set.
	RepoSubdir string `toml:"repo_subdir,omitempty"`
}
```

```go
// In internal/engine/unitpath.go:

// unitDir resolves the checkout directory one unit mirrors to.
func (e *Engine) unitDir(u repo.Unit) string

// classifyRel returns the provider-dir-relative path used for
// classification: path.Join(u.RepoSubdir, relFromLocalRoot). This is THE
// namespace Patterns() globs are written against, and it matches the
// paths GenerateAttributes emits attribute rows for.
func classifyRel(u repo.Unit, rel string) string
```

- [ ] **Step 1: Write the failing tests.** Provider side (`internal/provider/provider_test.go` additions): a compile-level test that `providertest.Fake` still satisfies `Provider` (`var _ provider.Provider = (*providertest.Fake)(nil)` — this exists; the new methods make it fail until implemented) plus behavior tests that the fake records `Discover`/`Identify` calls and returns configured values. Repo side (`internal/repo/local_test.go` additions): table rows — `RepoSubdir: "memories"` valid; `RepoSubdir: "../x"` and `RepoSubdir: "/abs"` rejected via `ValidateRelPath`; empty stays valid; round-trips through `Save`/`LoadLocalRegistry` (assert the TOML omits the key when empty — `omitempty` — so Phase-2 registries load unchanged). Engine side (`internal/engine/unitpath_test.go`):

```go
package engine

import (
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

func TestUnitDirAndClassifyRel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		unit        repo.Unit
		rel         string
		wantDirTail string // relative to checkout root, OS-joined
		wantRel     string
	}{
		{
			name:        "claude project unit (no subdir) — Phase-2 behavior",
			unit:        repo.Unit{Provider: "claude", Folder: "agent-brain", LocalDir: "/x"},
			rel:         "MEMORY.md",
			wantDirTail: filepath.Join("agent-brain", "claude"),
			wantRel:     "MEMORY.md",
		},
		{
			name:        "codex memories unit",
			unit:        repo.Unit{Provider: "codex", Folder: repo.GlobalFolder, LocalDir: "/x", RepoSubdir: "memories"},
			rel:         "memory_summary.md",
			wantDirTail: filepath.Join("_global", "codex", "memories"),
			wantRel:     "memories/memory_summary.md",
		},
		{
			name:        "codex chronicle unit, nested file",
			unit:        repo.Unit{Provider: "codex", Folder: repo.GlobalFolder, LocalDir: "/x", RepoSubdir: "chronicle"},
			rel:         "2026/07/log.md",
			wantDirTail: filepath.Join("_global", "codex", "chronicle"),
			wantRel:     "chronicle/2026/07/log.md",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newTestEngine(t) // reuse/extract the package's existing constructor helper
			gotDir := e.unitDir(tt.unit)
			if want := filepath.Join(e.checkout, tt.wantDirTail); gotDir != want {
				t.Errorf("unitDir = %q, want %q", gotDir, want)
			}
			if got := classifyRel(tt.unit, tt.rel); got != tt.wantRel {
				t.Errorf("classifyRel = %q, want %q", got, tt.wantRel)
			}
		})
	}
}
```

(If the engine tests have no reusable constructor helper, extract one — `newTestEngine(t *testing.T) *Engine` building a real temp checkout the way `mirror_in_test.go` does — as part of this step; do NOT copy-paste engine construction a fourth time.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/provider/... ./internal/repo/ ./internal/engine/ 2>&1 | head -30`
Expected: compile FAILs — `Fake` missing `Discover`/`Identify`; `unitDir`/`classifyRel` undefined; `RepoSubdir` unknown field.

- [ ] **Step 3: Implement.** Interface + types in provider.go exactly as the Produces block. Fake: add configurable `DiscoverResult []provider.Discovered`, `IdentifyResult provider.Identity`, error fields, and recorded call slices, following the fake's existing style. `Unit.RepoSubdir` + extend `Unit.validate()`:

```go
	if u.RepoSubdir != "" {
		if err := ValidateRelPath(u.RepoSubdir); err != nil {
			return fmt.Errorf("unit repo_subdir: %w", err)
		}
	}
```

`internal/engine/unitpath.go`:

```go
package engine

import (
	"path"
	"path/filepath"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// unitDir resolves the checkout directory one unit mirrors to:
// <folder>/<provider>[/<repo_subdir>]. RepoSubdir is validated
// slash-relative at registry load; FromSlash makes it OS-correct here.
func (e *Engine) unitDir(u repo.Unit) string {
	dir := e.layout.UnitDir(u.Folder, u.Provider)
	if u.RepoSubdir != "" {
		dir = filepath.Join(dir, filepath.FromSlash(u.RepoSubdir))
	}
	return dir
}

// classifyRel is the provider-dir-relative name for classification —
// the namespace Patterns() globs and generated attribute rows share.
func classifyRel(u repo.Unit, rel string) string {
	return path.Join(u.RepoSubdir, rel)
}
```

Replace the three `e.layout.UnitDir(u.Folder, u.Provider)` call sites (mirror_in.go:89, mirror_out.go:38, reconcile.go:22) with `e.unitDir(u)`, and route every `provider.Classify(prov, rel)` call in mirror-in/mirror-out through `provider.Classify(prov, classifyRel(u, rel))` — find them all: `grep -n 'Classify' internal/engine/*.go`. In reconcile.go, keep handing `ReconcileIndex` the PROVIDER dir (`e.layout.UnitDir(u.Folder, u.Provider)`), not the unit subdir — the index spans the provider dir; add a dedupe so a provider with several units in one folder reconciles once per (folder, provider) pair per cycle:

```go
	seen := map[string]bool{}
	for _, u := range units {
		key := u.Folder + "/" + u.Provider
		if skip[u.Folder] || seen[key] {
			continue
		}
		seen[key] = true
		// ... existing Get + ReconcileIndex(ctx, e.layout.UnitDir(u.Folder, u.Provider))
	}
```

- [ ] **Step 4: Run the full suite** (interface changes ripple):

Run: `go test ./... -race`
Expected: PASS everywhere. Compile errors in daemon/e2e tests mean a fake or helper constructs `Provider`/`Unit` literally — update those constructions, changing no behavior.

- [ ] **Step 5: Commit**

```bash
git add internal/provider/ internal/repo/ internal/engine/
git commit -m "feat(provider,repo,engine): discovery/identity contract + Unit.RepoSubdir

Provider gains Discover/Identify (spec §6 — enrollment is their first
consumer); Unit gains repo_subdir so one global provider maps several
local roots under its provider dir (codex: memories + chronicle, the
spec §3 layout). Classification namespace pinned: Patterns() globs are
provider-dir-relative; engine classifies path.Join(RepoSubdir, rel)."
```

### Task 3: Claude adapter — classification, discovery, identity, index reconcile (spec §6)

**Files:**
- Create: `internal/provider/claude/claude.go`, `internal/provider/claude/slug.go`, `internal/provider/claude/reconcile.go`
- Create: `internal/provider/remote.go` (shared remote-URL normalization — exported; migrate and future adapters reuse it)
- Test: `internal/provider/claude/claude_test.go`, `internal/provider/claude/slug_test.go`, `internal/provider/claude/reconcile_test.go`, `internal/provider/remote_test.go`

**Boundary amendment (binding):** adapters may import `provider`, `gitx`, `renameio`, and stdlib — nothing else. (`gitx` is needed to read git remotes; it is itself a leaf package.)

**Interfaces:**
- Consumes: Task 2's `provider.Provider` (with `Discover`/`Identify`), `provider.Pattern`/classes, `gitx.RunStatus`, `renameio.WriteFile`.
- Produces:
  - `func New(home string) *Adapter` — `home` is the user home dir, injected (composition root passes `os.UserHomeDir()`; tests pass `t.TempDir()`). `Name() == "claude"`, `Scope() == provider.ScopePerProject`.
  - `func GuessPath(slug string, dirExists func(string) bool) string` — exported: `migrate` (Task 11) reuses it on `~/.agent-brain/<slug>` names (same slug format).
  - `func provider.NormalizeRemoteURL(raw string) (string, error)` — canonical `host/owner/repo` from ssh/https/scp-style remotes.
  - The identity contract for remoteless projects: `Identify` returns `Identity{ProjectID: "", PreferredFolder: base}`; the CALLER (track/init flows, Task 11) prompts for a name and uses `named/<folder>` as the canonical id. Adapters never invent ids.

- [ ] **Step 1: Write the failing tests.**

`internal/provider/remote_test.go` — table over `NormalizeRemoteURL`:

```go
func TestNormalizeRemoteURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		raw, want string
		wantErr   bool
	}{
		{raw: "git@github.com:Sawmonabo/agent-brain.git", want: "github.com/Sawmonabo/agent-brain"},
		{raw: "https://github.com/Sawmonabo/agent-brain.git", want: "github.com/Sawmonabo/agent-brain"},
		{raw: "https://github.com/Sawmonabo/agent-brain", want: "github.com/Sawmonabo/agent-brain"},
		{raw: "ssh://git@github.com/Sawmonabo/agent-brain.git", want: "github.com/Sawmonabo/agent-brain"},
		{raw: "https://gitlab.com/group/sub/project.git", want: "gitlab.com/group/sub/project"},
		{raw: "https://user:tok@github.com/o/r.git", want: "github.com/o/r"}, // credentials stripped, never stored
		{raw: "", wantErr: true},
		{raw: "not a url", wantErr: true},
		{raw: "file:///local/path", wantErr: true}, // machine-local — not a cross-machine identity
	}
	// ... standard table body asserting got/err
}
```

`internal/provider/claude/slug_test.go` — `GuessPath` with a fake `dirExists` (a `map[string]bool` lookup): naive reversal when it exists (`-Users-u-dev-proj` + existing `/Users/u/dev/proj` → that path); dash-preserving reconstruction when only `/Users/u/dev/agent-brain` exists for slug `-Users-u-dev-agent-brain` (the greedy filesystem-guided walk); naive reversal returned as last resort when nothing exists.

`internal/provider/claude/claude_test.go` — `Discover` on a fabricated `home` tree (`.claude/projects/<slugA>/memory/`, `<slugB>/` WITHOUT memory dir, a stray file): exactly slugA yields a `Discovered{LocalDir: home/.claude/projects/<slugA>/memory, RepoSubdir: "", Label: slugA, PathGuess: <guess>}`. `Identify` against a real `t.TempDir()` git repo (`git init` + `git remote add origin git@github.com:o/r.git` via `gitx.Run`) → `Identity{ProjectID: "github.com/o/r", PreferredFolder: "r"}`; a repo with no remote → zero-ProjectID identity with `PreferredFolder = filepath.Base(projectPath)`; a projectPath that is not a git repo → same remoteless result (not an error — the picker warned already). Patterns: `provider.Classify(a, "MEMORY.md") == ClassDerivedIndex`, `"topic.md"` → Fact, `".DS_Store"` and `"sub/.DS_Store"` → ClassIgnore.

`internal/provider/claude/reconcile_test.go` — golden-content tests:

```go
// Three topic files exercise the three extraction tiers:
//   with-frontmatter.md: "---\nname: alpha-rule\ndescription: the hook text\n---\nbody"
//   heading-only.md:     "# Heading Title\nbody"
//   bare.md:             "just prose"
// Expected MEMORY.md (byte-exact, trailing newline included):
//
// # Memory index
//
// - [alpha-rule](with-frontmatter.md) — the hook text
// - [Heading Title](heading-only.md)
// - [bare](bare.md)
func TestReconcileIndexRebuild(t *testing.T) { ... }

// Deterministic + idempotent: two runs, byte-identical output; file order
// is sorted by filename regardless of creation order.
func TestReconcileIndexDeterministic(t *testing.T) { ... }

// Empty dir with no MEMORY.md → no file created (no-op).
// Existing MEMORY.md but zero topic files → rewritten to header only.
// MEMORY.md itself and non-.md files are never indexed.
func TestReconcileIndexEdges(t *testing.T) { ... }
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/provider/... -run 'Normalize|Guess|Claude|Reconcile' -v 2>&1 | head -20`
Expected: compile FAIL (packages don't exist).

- [ ] **Step 3: Implement.**

`internal/provider/remote.go`:

```go
package provider

import (
	"fmt"
	"net/url"
	"strings"
)

// NormalizeRemoteURL canonicalizes a git remote to the machine-independent
// project id "host/owner/repo" (spec §3). Credentials embedded in https
// URLs are stripped and never appear in the id. Local-only remotes
// (file://, plain paths) are rejected — they cannot identify a project
// across machines.
func NormalizeRemoteURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("empty remote url")
	}
	// scp-like syntax: git@host:owner/repo(.git)
	if !strings.Contains(raw, "://") {
		at := strings.Index(raw, "@")
		colon := strings.Index(raw, ":")
		if at >= 0 && colon > at {
			host, path := raw[at+1:colon], raw[colon+1:]
			return joinRemoteID(host, path)
		}
		return "", fmt.Errorf("remote %q is not a recognizable git url", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse remote url: %w", err)
	}
	switch u.Scheme {
	case "https", "http", "ssh", "git":
		return joinRemoteID(u.Hostname(), u.Path)
	default:
		return "", fmt.Errorf("remote scheme %q cannot identify a project across machines", u.Scheme)
	}
}

func joinRemoteID(host, path string) (string, error) {
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if host == "" || path == "" || !strings.Contains(path, "/") {
		return "", fmt.Errorf("remote host/path %q/%q incomplete", host, path)
	}
	return strings.ToLower(host) + "/" + path, nil
}
```

(Note the deliberate asymmetry: host lowercased — DNS is case-insensitive; owner/repo case preserved — GitHub redirects but preserves case, and lowercasing could merge two genuinely distinct projects on case-sensitive hosts.)

`internal/provider/claude/slug.go`:

```go
// Package claude adapts Claude Code's per-project memory
// (~/.claude/projects/<slug>/memory/, zero-config since v2.1.59) to the
// agent-brain provider contract (spec §6).
package claude

import (
	"path/filepath"
	"strings"
)

// GuessPath reverses Claude's project slug (absolute path with '/'
// replaced by '-') into a best-guess project directory. The encoding is
// lossy — a '-' in a real path component is indistinguishable from a
// separator — so the reconstruction is filesystem-guided: walk the slug
// segments, preferring '/' when the resulting directory exists and
// falling back to extending the current component with '-'. dirExists is
// injected for testability (production passes a stat closure).
//
// Exported because migrate (spec §10) maps identical slugs under
// ~/.agent-brain/.
func GuessPath(slug string, dirExists func(string) bool) string {
	segments := strings.Split(strings.TrimPrefix(slug, "-"), "-")
	if len(segments) == 0 {
		return ""
	}
	naive := "/" + strings.Join(segments, "/")
	if dirExists(naive) {
		return naive
	}
	current := "/" + segments[0]
	for _, segment := range segments[1:] {
		asChild := current + "/" + segment
		if dirExists(asChild) {
			current = asChild
			continue
		}
		current += "-" + segment
	}
	if dirExists(current) {
		return current
	}
	return filepath.ToSlash(naive)
}
```

`internal/provider/claude/claude.go` — `Adapter` struct holding `home`; `Patterns()` returns

```go
	[]provider.Pattern{
		{Glob: "MEMORY.md", Class: provider.ClassDerivedIndex},
		// macOS Finder droppings are not memory data. Everything else
		// unmatched falls through to ClassFact (spec §6: retain-both is
		// the safest default for unknown files).
		{Glob: ".DS_Store", Class: provider.ClassIgnore},
		{Glob: "**/.DS_Store", Class: provider.ClassIgnore},
	}
```

`Discover` reads `filepath.Join(a.home, ".claude", "projects")` with `os.ReadDir` (a missing root returns `nil, nil` — Claude not installed is not an error), keeps entries whose `<entry>/memory` is a directory, and builds `Discovered{LocalDir: <root>/<slug>/memory, Label: slug, PathGuess: GuessPath(slug, statDir)}` sorted by Label (deterministic picker order). `Identify(ctx, d, projectPath)`:

```go
func (a *Adapter) Identify(ctx context.Context, _ provider.Discovered, projectPath string) (provider.Identity, error) {
	fallback := provider.Identity{PreferredFolder: filepath.Base(projectPath)}
	if projectPath == "" {
		return provider.Identity{}, fmt.Errorf("claude identify: empty project path")
	}
	res, err := gitx.RunStatus(ctx, projectPath, "remote", "get-url", "origin")
	if err != nil || res.ExitCode != 0 {
		// Not a git repo, or no origin: a nameable local project, not an
		// error — the enrollment flow asks the user for a folder name.
		return fallback, nil //nolint:nilerr // absence of a remote is the documented remoteless path, not a failure
	}
	id, err := provider.NormalizeRemoteURL(strings.TrimSpace(res.Stdout))
	if err != nil {
		return fallback, nil //nolint:nilerr // unparseable remote → same remoteless path
	}
	return provider.Identity{ProjectID: id, PreferredFolder: path.Base(id)}, nil
}
```

`ReconcileIndex` (`reconcile.go`): read dir entries; collect non-`MEMORY.md` `*.md` files sorted by name; for each extract (title, hook): frontmatter block (first line `---`, scan to closing `---`, parse `name:` / `description:` values with `strings.TrimSpace`, tolerate quoted values by trimming matching `"`), else first `# ` heading text, else filename without `.md`; hook = description or "". Render exactly:

```go
	var b strings.Builder
	b.WriteString("# Memory index\n\n")
	for _, entry := range entries {
		if entry.hook != "" {
			fmt.Fprintf(&b, "- [%s](%s) — %s\n", entry.title, entry.file, entry.hook)
		} else {
			fmt.Fprintf(&b, "- [%s](%s)\n", entry.title, entry.file)
		}
	}
```

No topic files: if `MEMORY.md` absent → return nil (never create); present → rewrite header-only. Write via `renameio.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte(b.String()), 0o644)` — atomic; a crashed reconcile never leaves a torn index. Skip writing when existing bytes already equal the rendering (no mtime churn → no spurious watch triggers when the file mirrors back out). Doc comment on the method (binding): *writes exactly one file, `MEMORY.md`, never a git-meta name; safe under the sync.go scrub contract because Sync orders scrubIntegrated before reconcile.*

A note on what reconcile costs (put this in the package doc, verbatim intent): regeneration replaces any hand-curation Claude's own sessions did to MEMORY.md wording — the frontmatter `description` IS the hook text by construction, so curated descriptions survive in the topic files themselves; this is the spec §3 derived-index trade, decided (files are the source of truth, the index is a view).

- [ ] **Step 4: Run the adapter tests**

Run: `go test ./internal/provider/... -race -v 2>&1 | tail -20`
Expected: PASS (all new + existing provider tests).

- [ ] **Step 5: Commit**

```bash
git add internal/provider/
git commit -m "feat(provider): claude adapter — discovery, identity, MEMORY.md reconcile

Slug reversal is filesystem-guided (lossy '-' encoding); identity is the
normalized origin remote (host/owner/repo, credentials stripped);
remoteless projects fall back to user-named folders. ReconcileIndex
rebuilds MEMORY.md deterministically from topic-file frontmatter
(name/description → title/hook), atomic + churn-free."
```

---

### Task 4: Codex adapter (experimental) + config-overridable classification

Codex memory is user-global (`$CODEX_HOME`, default `~/.codex`) with two rescue-worthy roots: `memories/` and `memories_extensions/chronicle/`. The adapter NEVER touches `$CODEX_HOME` itself — `auth.json` lives there (Global Constraints: secret-adjacency). Each root becomes one unit via Task 2's `RepoSubdir` (`memories`, `chronicle`), mapping to `_global/codex/memories` and `_global/codex/chronicle` — the spec §3 layout, byte-for-byte. Its classification table is config-overridable (spec §6: upstream format drift absorbed without a release); claude's is deliberately NOT.

**Files:**
- Create: `internal/provider/codex/codex.go`
- Modify: `internal/config/settings.go` (provider override section)
- Modify: `internal/cli/daemon.go` (composition root: build the real registry — read the file first; Phase 2 wires an empty registry there)
- Create: `internal/cli/registry.go` (shared registry construction — daemon, doctor, init, track all need the same one)
- Test: `internal/provider/codex/codex_test.go`, `internal/config/settings_test.go` (additions), `internal/cli/registry_test.go`

**Interfaces:**
- Consumes: Task 2 contract; `config.Settings`.
- Produces:
  - `func codex.New(codexHome string, overrides []provider.Pattern) *Adapter` — nil/empty `overrides` → built-in table; non-empty REPLACES it wholesale (documented: overriding means owning the whole table).
  - `config.Settings` gains: `Providers map[string]config.ProviderSettings` (`toml:"providers"`), `type ProviderSettings struct { Classify []ClassifyRule }`, `type ClassifyRule struct { Glob string; Class string }` (`toml:"glob"`, `toml:"class"`). Class strings: `fact`, `derived-index`, `regenerated`, `ignore` (exactly `provider.Class.String()` values). `LoadSettings` validates every rule (glob via `provider.ValidateGlob`, class via a parse map) — an invalid rule is a load error, strict like the rest of ADR 17.
  - `func ClassFromString(s string) (Class, error)` added to `internal/provider` (inverse of `String()`).
  - `cli.buildRegistry(settings config.Settings, home string) (*provider.Registry, error)` — claude + codex, overrides applied; THE registry constructor for daemon/doctor/init/track. `$CODEX_HOME` env honored here (defaults to `home/.codex`).

- [ ] **Step 1: Write the failing tests.** `codex_test.go`: built-in classification table rows (provider-dir-relative! — `Classify(a, "memories/raw_memories.md")` → Fact; `"memories/memory_summary.md"`, `"memories/MEMORY.md"` → Regenerated; `"memories/rollout_summaries/2026/x.md"` → Regenerated; `"memories/skills/foo/SKILL.md"` → Fact; `"chronicle/2026/07/log.md"` → Fact; `".DS_Store"`/`"memories/.DS_Store"` → Ignore); `Discover` on a fabricated codexHome — both roots present → exactly two `Discovered` (`RepoSubdir: "memories"` then `"chronicle"`, LocalDirs absolute, Labels `"codex memories"`/`"codex chronicle"`); chronicle missing → one; neither → nil, nil; `Identify` → zero Identity, nil error; `ReconcileIndex` → nil (no-op) and creates nothing. Overrides: `New(home, []provider.Pattern{{Glob: "**", Class: provider.ClassIgnore}})` classifies everything Ignore. `settings_test.go` additions: a TOML fixture with `[providers.codex]` + two classify rules round-trips; bad class string and bad glob each fail `LoadSettings` with the offending rule named. `registry_test.go`: `buildRegistry` returns both providers; codex overrides from settings are applied; `CODEX_HOME` env override respected (set via `t.Setenv` — note: no `t.Parallel()` in that one test).

- [ ] **Step 2: Run to verify failure** — `go test ./internal/provider/codex/ ./internal/config/ ./internal/cli/ 2>&1 | head -15` → compile FAIL.

- [ ] **Step 3: Implement.** `codex.go` core:

```go
// Package codex adapts OpenAI Codex CLI's user-global memory to the
// provider contract (spec §6). Ships experimental (ADR 02): the layout
// is partly third-party-documented, so the classification table is
// config-overridable and the adapter mirrors ONLY the two memory roots —
// never $CODEX_HOME itself, which holds credentials (auth.json).
package codex

func builtinPatterns() []provider.Pattern {
	return []provider.Pattern{
		{Glob: "memories/raw_memories.md", Class: provider.ClassFact}, // append-mostly log
		{Glob: "memories/memory_summary.md", Class: provider.ClassRegenerated},
		{Glob: "memories/MEMORY.md", Class: provider.ClassRegenerated},
		{Glob: "memories/rollout_summaries/**", Class: provider.ClassRegenerated},
		{Glob: "memories/skills/**/SKILL.md", Class: provider.ClassFact},
		{Glob: "chronicle/**", Class: provider.ClassFact},
		{Glob: ".DS_Store", Class: provider.ClassIgnore},
		{Glob: "**/.DS_Store", Class: provider.ClassIgnore},
		// Unmatched → ClassFact via Classify's default: correct here
		// because units scope mirroring to the two roots above.
	}
}

func (a *Adapter) Discover(_ context.Context) ([]provider.Discovered, error) {
	var found []provider.Discovered
	roots := []struct{ local, sub, label string }{
		{filepath.Join(a.codexHome, "memories"), "memories", "codex memories"},
		{filepath.Join(a.codexHome, "memories_extensions", "chronicle"), "chronicle", "codex chronicle"},
	}
	for _, r := range roots {
		if info, err := os.Stat(r.local); err == nil && info.IsDir() {
			found = append(found, provider.Discovered{LocalDir: r.local, RepoSubdir: r.sub, Label: r.label})
		}
	}
	return found, nil
}
```

`Identify` returns `provider.Identity{}, nil` (global scope; folder is `repo.GlobalFolder` by construction at enrollment). `ReconcileIndex` returns nil with a doc comment (codex's own consolidator owns its indexes; ours are class-Regenerated). `provider.ClassFromString`: exact inverse map of `String()`, error naming the bad value and listing valid ones. Settings: strict parse + validation loop in `LoadSettings` after the existing floor checks:

```go
	for providerName, ps := range settings.Providers {
		for i, rule := range ps.Classify {
			if err := provider.ValidateGlob(rule.Glob); err != nil {
				return Settings{}, fmt.Errorf("providers.%s.classify[%d]: %w", providerName, i, err)
			}
			if _, err := provider.ClassFromString(rule.Class); err != nil {
				return Settings{}, fmt.Errorf("providers.%s.classify[%d]: %w", providerName, i, err)
			}
		}
	}
```

(Import direction check: `config` → `provider` is new; `provider` imports nothing internal, so no cycle. Record it in the package comment.)

`internal/cli/registry.go`:

```go
// buildRegistry is THE composition point for provider adapters — daemon,
// doctor, init, and track must all see the identical registry, or
// generated attributes and classification drift apart.
func buildRegistry(settings config.Settings, home string) (*provider.Registry, error) {
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	var codexOverrides []provider.Pattern
	if ps, ok := settings.Providers["codex"]; ok {
		for _, rule := range ps.Classify {
			class, err := provider.ClassFromString(rule.Class) // validated at load; defensive here
			if err != nil {
				return nil, err
			}
			codexOverrides = append(codexOverrides, provider.Pattern{Glob: rule.Glob, Class: class})
		}
	}
	return provider.NewRegistry(claude.New(home), codex.New(codexHome, codexOverrides))
}
```

Wire it in `internal/cli/daemon.go` where the daemon's `Config.Registry` is built (read the current construction; replace the placeholder registry with `buildRegistry(settings, home)`, sourcing `home` from `os.UserHomeDir()` and settings from the existing load).

- [ ] **Step 4: Full suite** — `go test ./... -race` → PASS. The daemon package's tests construct registries with fakes; they must keep compiling untouched (they inject `Config.Registry` directly — composition change is cli-side only).

- [ ] **Step 5: Commit**

```bash
git add internal/provider/ internal/config/ internal/cli/
git commit -m "feat(provider,config,cli): codex adapter (experimental) + overridable classification

Two RepoSubdir units (memories, chronicle) — never \$CODEX_HOME itself
(auth.json adjacency). Classification table replaceable via
[providers.codex] classify rules in config.toml, validated strictly at
load (ADR 17). buildRegistry becomes the single composition point."
```

### Task 5: `internal/ghx` — gh CLI integration + repo-local credential wiring (ADR 08)

Everything gh-shaped lives behind one fakeable seam. Runtime posture (ADR 08, amended): v1 REQUIRES gh; we borrow its auth per operation and persist nothing. The daemon's ongoing HTTPS pushes authenticate through git's credential-helper mechanism pointed at `gh auth git-credential` — wired REPO-LOCALLY in the hidden memories checkout with gh's ABSOLUTE path (per-machine file, so the cli/cli#9438 synced-gitconfig hazard cannot apply; `doctor --fix` re-wires if gh moves). SSH remotes never invoke the helper; it is inert there.

**Files:**
- Create: `internal/ghx/ghx.go`, `internal/ghx/ghxtest/fake.go`
- Modify: `internal/gitx/install.go` (add `InstallCredentialHelper`)
- Test: `internal/ghx/ghx_test.go`, `internal/gitx/install_test.go` (additions)

**Interfaces:**
- Consumes: `os/exec`, `gitx.Run`.
- Produces (Tasks 8, 10 rely on these):

```go
package ghx

// ErrMissing means no gh binary is on PATH. Every message names the fix.
var ErrMissing = errors.New("gh CLI not found — install it (https://cli.github.com) and run `gh auth login`")

// Result carries a finished gh invocation (same shape idiom as gitx).
type Result struct {
	Stdout, Stderr string
	ExitCode       int
}

// Runner executes gh. The exec implementation is process-global reality;
// ghxtest.Fake scripts it for tests.
type Runner interface {
	Run(ctx context.Context, args ...string) (Result, error)
}

// Client wraps gh operations agent-brain needs. Zero persistence: no
// token ever leaves gh's own storage (ADR 08).
type Client struct{ /* runner Runner; binaryPath string */ }

func NewClient() (*Client, error)                  // exec.LookPath("gh") → ErrMissing
func NewClientWithRunner(r Runner, binaryPath string) *Client // tests + init wiring
func (c *Client) BinaryPath() string
func (c *Client) AuthOK(ctx context.Context) error // `gh auth status`; nonzero → error incl. stderr + "run `gh auth login`"
func (c *Client) Login(ctx context.Context) (string, error) // `gh api user --jq .login`, trimmed
func (c *Client) RepoExists(ctx context.Context, owner, name string) (bool, error)
func (c *Client) CreateRepo(ctx context.Context, name, description string) (url string, err error) // --private always
func (c *Client) Clone(ctx context.Context, ownerRepo, dir string, gitArgs ...string) error
```

- `gitx.InstallCredentialHelper(ctx context.Context, repoDir, ghPath string) error` — repo-local config: first `credential.helper` set to the empty string (clears inherited helpers — a stale global osxkeychain PAT must not shadow gh), then `--add credential.helper "!<single-quoted ghPath> auth git-credential"`. Idempotent: re-running replaces, never duplicates (use `--replace-all` for the empty entry, then `--add`).
- `ghxtest.Fake` — scripted `Runner`: ordered expectations of arg-slices → `Result`/error, recording actual calls; unexpected call = `t.Fatal` via injected `testing.TB`.

- [ ] **Step 1: Write the failing tests.** `ghx_test.go` (fake-runner driven, table style):
  - `AuthOK`: exit 0 → nil; exit 1 + stderr "You are not logged in" → error containing both the stderr and `gh auth login`.
  - `Login`: stdout `"Sawmonabo\n"` → `"Sawmonabo"`; empty stdout → error.
  - `RepoExists`: exit 0 → (true, nil); exit 1 + stderr containing `Could not resolve to a Repository` → (false, nil); exit 1 + stderr `connect: network is unreachable` → (false, error) — a network failure must NOT read as "repo missing" (init would try to create over an existing repo).
  - `CreateRepo`: asserts args exactly `repo create agent-brain-memories --private --description <desc>`; stdout URL trimmed and returned.
  - `Clone`: args `repo clone o/r <dir> -- --no-checkout` when gitArgs passed through.
  - Exec runner (no fake): put an executable `gh` stub script in `t.TempDir()` (`#!/bin/sh\necho out; echo err >&2; exit 3`), build the client against that absolute path, assert `Result{Stdout: "out\n", Stderr: "err\n", ExitCode: 3}` with nil error, and that a killed/unstartable binary returns err (mirror gitx.RunStatus's contract: exit codes are data, spawn failures are errors).
  - `install_test.go` additions: `InstallCredentialHelper` on a `git init` temp repo → `git config --local --get-all credential.helper` yields exactly `["", "!'/fake path/gh' auth git-credential"]` (quoting proven by a ghPath containing a space); re-run → identical (idempotent).

- [ ] **Step 2: Run to verify failure** — `go test ./internal/ghx/... ./internal/gitx/ 2>&1 | head -10` → compile FAIL.

- [ ] **Step 3: Implement.** Notes beyond the signatures:
  - exec runner: `exec.CommandContext(ctx, c.binaryPath, args...)`, capture both pipes, translate `*exec.ExitError` to `Result.ExitCode` (data, nil error); any other error (spawn, ctx) is an error. Never pass user-controlled strings through a shell — args go as a slice.
  - `RepoExists` runs `repo view owner/name --json name`; the "Could not resolve" match is on stderr, case-sensitive as gh emits it; document that any OTHER nonzero is surfaced as an error naming the stderr.
  - `InstallCredentialHelper` quoting helper: `func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }` — git runs helper values through `/bin/sh`.
  - Doc comment on the package (binding): NEVER add a call to `gh auth setup-git` or `gh auth token` — the first writes global gitconfig (ADR 08 hazard), the second would put a live token in our process memory/argv for no benefit over the helper indirection.

- [ ] **Step 4: Run** — `go test ./internal/ghx/... ./internal/gitx/ -race` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ghx/ internal/gitx/
git commit -m "feat(ghx,gitx): gh client behind fakeable runner + repo-local credential helper

ADR 08 as-built: gh required, auth borrowed per operation, zero token
persistence. HTTPS pushes ride \`gh auth git-credential\` wired with an
absolute path in the hidden checkout's LOCAL git config (never the
synced global gitconfig), inherited helpers cleared so a stale keychain
PAT can't shadow it. Network failure vs missing repo disambiguated."
```

---

### Task 6: Daemon reliability — watcher rebuild, mid-run log rotation, conflicts.jsonl bound (Phase-2 handoff items 2, 3, 5)

Three residents-die-slowly bugs from the Phase-2 handoff, one task. (a) `daemon.log` rotates only at startup — a long-lived daemon grows without bound; rotate mid-run. (b) `conflicts.jsonl` has no bound at all. (c) Enrollment applies on the next cycle (`runCycle` reloads the registry) but the WATCHER never learns new roots until restart, and a died fsnotify watcher (fd exhaustion, WSL2 quirks) is logged and abandoned — rebuild it in both cases.

**Files:**
- Modify: `internal/daemon/logging.go` (rotating writer + shared rotate helper)
- Modify: `internal/daemon/daemon.go` (loop restructure: triggers as a variable channel, rebuild on unit-set change and on watcher death; conflict-log rotation at cycle start; refuse-message update)
- Test: `internal/daemon/logging_test.go`, `internal/daemon/daemon_test.go` (additions — follow the package's existing real-time eventually-assert idioms; no fake clocks, per the Phase-2 convention recorded in its Task 9)

**Interfaces:**
- Consumes: `watch.Manager` (`New`/`Add`/`Run`/`Triggers`/`Close` — Add-before-Run contract makes rebuild = new Manager), existing `loop`/`runCycle`.
- Produces:
  - `type rotatingWriter struct{ ... }` implementing `io.Writer` — mutex-guarded (slog handlers are called from multiple goroutines); on crossing `maxLogSize` it closes, renames to `<path>.1` (one generation), reopens, resets. `func newRotatingWriter(path string) (*rotatingWriter, error)`; `Close() error`. `openLogger` now builds the slog JSON handler on it.
  - `func rotateIfOversized(path string, limit int64) error` — the startup-style rotation, reused for `conflicts.jsonl` (called at the top of every `runCycle`; safe because merge drivers only append DURING a cycle, and the single engine goroutine is not in a cycle yet — note this ordering in the comment).
  - `const maxConflictLogSize = 5 << 20`.
  - `runCycle` returns the units it synced (`[]repo.Unit`) so `loop` can diff watch roots; `loop` owns `triggers <-chan watch.Trigger` + `watchDied chan error` and a `rebuildWatcher(roots []string)` closure.

- [ ] **Step 1: Write the failing tests.**
  - `logging_test.go`: `rotatingWriter` — write past a tiny injected limit (make the limit a field, defaulted to `maxLogSize`, settable in tests) → `.1` exists with the old bytes, live file restarts small; concurrent writers (`go` + `sync.WaitGroup`, `-race` is the assertion) never lose the writer invariant; `rotateIfOversized` rotates an oversized file, no-ops on small/missing.
  - `daemon_test.go` — the load-bearing one: start a daemon (existing test harness idioms: temp dirs via env-override `config.Paths`, fake registry, real socket), with ZERO units enrolled; then (simulating `track`) append a unit to `registry-local.toml` pointing at a fresh temp dir, trigger one manual sync over the API (`client.Sync`) so the cycle observes the new unit; then touch a file inside the new unit's LocalDir and eventually-assert (deadline-bounded poll of `client.Status`) that a NEW cycle ran with reason `watch` — proving the rebuilt watcher covers the enrolled root without a daemon restart.
  - Direct rebuild test: call the extracted rebuild path twice with different root sets; assert the old manager is closed (its `Triggers()` channel is drained/dead) and events under the new roots trigger, events under removed roots don't. (Forcing a REAL fsnotify fd death portably isn't feasible; death and enrollment share the same rebuild code path — pin the path, document the limitation in the test comment.)

- [ ] **Step 2: Run to verify failure** — `go test ./internal/daemon/ -run 'Rotat|Rebuild|Enroll' -v` → FAIL (missing symbols / watcher never rebuilt so the eventually-assert times out).

- [ ] **Step 3: Implement.** Loop restructure sketch (the implementer adapts to the real file, preserving every existing behavior — backoff, manual-sync reply, ticker):

```go
	var (
		triggers  <-chan watch.Trigger
		watchDied = make(chan error, 1)
		manager   *watch.Manager
		watched   []string // sorted roots currently attached
	)
	startWatcher := func(roots []string) {
		if manager != nil {
			_ = manager.Close()
		}
		m, err := watch.New(watch.Config{Debounce: ..., Poll: ...})
		if err != nil {
			logger.Error("watch rebuild failed", "error", err)
			triggers, manager = nil, nil // ticker/poll backstop keeps cycles alive
			return
		}
		for _, root := range roots {
			if err := m.Add(root); err != nil {
				logger.Warn("watch root not attached", "dir", root, "error", err)
			}
		}
		go func(m *watch.Manager) {
			if err := m.Run(ctx); err != nil && ctx.Err() == nil {
				select { case watchDied <- err: default: }
			}
		}(m)
		manager, triggers, watched = m, m.Triggers(), roots
	}
```

`loop` calls `startWatcher(rootsOf(initialUnits))` once (replacing the current pre-loop Add calls in `Run` — move them), then in the select: `case trigger := <-triggers:` (nil channel = blocked = fine); `case err := <-watchDied: logger.Error("watch manager died — rebuilding", "error", err); startWatcher(watched)`; and after EVERY `runCycle` return, diff `rootsOf(units)` (sorted, go-cmp-free compare) against `watched` → `startWatcher` on change. `rotateIfOversized(d.cfg.Paths.ConflictLogFile(), maxConflictLogSize)` at the top of `runCycle`, warn-level log on error (a full disk must not stop sync attempts). Update the `TriggerSync` refusal string to `"memories repo not initialized on this machine — run `agent-brain init` first"` (Task 8 refines the condition itself; only the message changes here) and keep the daemon-side text in sync with what Task 9 asserts in CLI tests.

- [ ] **Step 4: Run the daemon suite shaken** — `go test ./internal/daemon/ -race -count=3` → PASS consistently (timing tests are eventually-asserted; flakes here are bugs, not noise — fix the code or the deadline, never sleep-and-pray).

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/
git commit -m "feat(daemon): watcher rebuild on enrollment/death + mid-run log rotation + conflict-log bound

Closes Phase-2 handoff items: enrolled units are watched without a
restart (root-set diff after every cycle rebuilds the manager — the
Add-before-Run contract makes rebuild-by-replacement the only correct
shape), a died watcher is rebuilt instead of abandoned, daemon.log
rotates mid-run via a mutex-guarded rotating writer, conflicts.jsonl
is bounded at cycle start (5 MiB, one .1 generation)."
```

### Task 7: Enrollment, purge & seed as engine operations + daemon API growth (`/v0/track`, `/v0/untrack`, `/v0/migrate`)

The single-writer rule made concrete: `projects.toml` writes, folder creation, purge commits, and migrate's seed commits all happen inside the daemon's ONE engine goroutine. The loop gains an admin-request channel beside `syncRequests`; UDS handlers submit typed operations and wait bounded. The CLI (Tasks 10–11) is purely a client.

**Files:**
- Create: `internal/engine/admin.go` (RegisterProject, PurgeProject, SeedProject)
- Modify: `internal/repo/manifest.go` (`ImportedFrom` marker map)
- Modify: `internal/daemon/daemon.go` (admin channel in `loop`; handlers), `internal/daemon/server.go` (routes), `internal/daemon/api/types.go` + `client.go` (request/response types, body-carrying `do`)
- Test: `internal/engine/admin_test.go`, `internal/repo/manifest_test.go` (additions), `internal/daemon/daemon_test.go` + `internal/daemon/api/client_test.go` (additions)

**Interfaces:**
- Consumes: Task 2 (`unitDir`, `RepoSubdir`), Task 1 (`isGitMetaPath`), existing `Projects`/`LocalRegistry`/`Manifest`, commit helpers in `internal/engine/commit.go` (read it first; REUSE its commit idiom and subject conventions — the git-author/stamp mechanics are already there).
- Produces:

```go
// internal/engine/admin.go — all three run ONLY on the daemon's engine
// goroutine (same rule as Sync; the busy-guard idiom applies).

// RegisterProject records id in the shared projects registry, creates
// the project/provider dir, commits the registration (meta commit,
// subject convention from commit.go), and returns the folder actually
// recorded (collision-disambiguated). Idempotent: an already-registered
// id returns its existing folder with no new commit. Global scope never
// calls this (folder is repo.GlobalFolder by construction).
func (e *Engine) RegisterProject(ctx context.Context, id, preferredFolder string) (string, error)

// PurgeProject removes the project folder from the checkout AND its
// projects.toml entry, in one commit ("history retains it", spec §7).
// Honest semantics, documented on the method: another machine still
// tracking this project will re-seed the folder on its next cycle —
// purge is a this-machine-was-the-last-tracker operation.
func (e *Engine) PurgeProject(ctx context.Context, folder string) error

// SeedReport says what a seed did.
type SeedReport struct {
	Folder  string
	Files   int  // files copied into the seed commit
	Skipped bool // imported-from marker already present → no-op
}

// SeedProject imports a bash-era memory tree as the SEED layer
// (spec §10 steps 3–5): copies srcDir's files into <folder>/<provider>/,
// scrubbing git-meta and skipping .lock/.sync-pending droppings, commits
// them together with the host manifest's imported-from marker
// (slug → folder) in ONE commit, and no-ops forever after via that
// marker. The daemon composes it with enrollment ORDER-SENSITIVELY:
// register → seed → enroll → cycle, so the live overlay lands as the
// second layer (spec §10 step 4).
func (e *Engine) SeedProject(ctx context.Context, folder, providerName, slug, srcDir string) (SeedReport, error)
```

```go
// internal/repo/manifest.go — Manifest grows the marker (additive,
// omitempty; version stays 1 — old files load unchanged):
type Manifest struct {
	Version int                      `json:"version"`
	Files   map[string]ManifestEntry `json:"files"`
	// ImportedFrom records completed migrate seeds on this host:
	// bash-era slug → repo folder (spec §10 step 5). Presence of a slug
	// makes SeedProject a no-op.
	ImportedFrom map[string]string `json:"imported_from,omitempty"`
}
```

```go
// internal/daemon/api/types.go:
type TrackRequest struct {
	Provider        string `json:"provider"`
	ProjectID       string `json:"project_id"`        // "" for global scope
	PreferredFolder string `json:"preferred_folder"`  // ignored for global scope
	LocalDir        string `json:"local_dir"`
	RepoSubdir      string `json:"repo_subdir,omitempty"`
}
type TrackResponse struct{ Folder string `json:"folder"` }

type UntrackRequest struct {
	Provider string `json:"provider"`
	LocalDir string `json:"local_dir"`
	Purge    bool   `json:"purge"`
}
type UntrackResponse struct {
	Removed bool `json:"removed"`
	Purged  bool `json:"purged"`
}

type MigrateRequest struct {
	Provider        string `json:"provider"`
	ProjectID       string `json:"project_id"`
	PreferredFolder string `json:"preferred_folder"`
	LocalDir        string `json:"local_dir"` // live memory dir to ENROLL (may not exist yet — enrollment still valid)
	Slug            string `json:"slug"`      // bash-era slug (marker key)
	SeedDir         string `json:"seed_dir"`  // ~/.agent-brain/<slug> tree to import
}
type MigrateResponse struct {
	Folder  string `json:"folder"`
	Files   int    `json:"files"`
	Skipped bool   `json:"skipped"`
}
```

- `POST /v0/sync` additionally accepts an optional body `SyncRequest{Project string `json:"project,omitempty"`}` (spec §7: `sync [--project X]`): non-empty → the triggered cycle filters units to that repo folder (unknown folder = 400 naming the enrolled folders). `Client.Sync` gains the parameter (`Sync(ctx context.Context, project string)`); existing callers pass `""`. Implementation: the loop's `syncRequest` carries the filter; `runCycle` applies it AFTER loading the registry (`units = filterUnits(units, folder)`), so watch/ticker cycles stay whole-fleet.
- Client methods: `Track(ctx, TrackRequest) (TrackResponse, error)`, `Untrack(...)`, `Migrate(...)` — extend the private `do` to `do(ctx, method, path string, in, out any)` (nil `in` = no body; existing calls pass nil). Server routes `POST /v0/track|untrack|migrate` with the same peer-UID enforcement and JSON error envelope as existing handlers (read server.go and mirror its exact idiom).
- Daemon: `adminRequests chan adminRequest` where `type adminRequest struct { run func(context.Context, *engine.Engine) (any, error); reply chan adminReply }`; `loop` gains `case request := <-d.adminRequests:` executing on the engine goroutine. Handlers build closures: track = `RegisterProject` (skip for `repo.GlobalFolder`) → `LocalRegistry` load/`Enroll`/`Save` → reply, then trigger a cycle; untrack = registry `Remove`/`Save` (+ `PurgeProject` when `Purge`); migrate = register → seed → enroll → cycle, per the ordering contract. Admin ops require state "ready" (same refusal as `TriggerSync`).

- [ ] **Step 1: Write the failing engine + manifest tests** (`admin_test.go` — reuse `newTestEngine`; real git):
  - `TestRegisterProjectIdempotent`: first call → folder recorded in a NEW commit whose subject matches commit.go's meta convention, `<folder>/<provider>` dir exists; second call same id → same folder, `git log` count unchanged.
  - `TestRegisterProjectCollision`: two ids sharing a preferred folder → `folder`, `folder-2` (delegating to `Projects.Add` — assert through the engine, not by re-testing repo internals).
  - `TestPurgeProject`: seeded folder + registry entry → one commit removing both; `git ls-files <folder>` empty; entry gone from a fresh `LoadProjects`.
  - `TestSeedProject`: srcDir with `keep.md`, `sub/topic.md`, `.lock`, `x.sync-pending`, hostile `.gitattributes`, nested `evil/.gitignore` → exactly the two `.md` files land under `<folder>/claude/`, ONE commit containing them AND the manifest marker (`ImportedFrom[slug] == folder`); rerun → `SeedReport{Skipped: true}`, no new commit.
  - Manifest: marker round-trips; a Phase-2 manifest JSON without the key loads (backward compat pinned).

- [ ] **Step 2: Run to verify failure** — `go test ./internal/engine/ ./internal/repo/ -run 'Register|Purge|Seed|Manifest' -v 2>&1 | head` → FAIL.

- [ ] **Step 3: Implement engine + manifest.** Copy loop for seed (inside `SeedProject`):

```go
	err := filepath.WalkDir(srcDir, func(abs string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(srcDir, abs)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		name := path.Base(rel)
		if isGitMetaPath(rel) || name == ".lock" || strings.HasSuffix(name, ".sync-pending") {
			return nil // bash-era droppings and git-meta never enter the seed
		}
		if err := repo.ValidateRelPath(rel); err != nil {
			return fmt.Errorf("seed %s: hostile path: %w", slug, err)
		}
		// copy abs → filepath.Join(destDir, filepath.FromSlash(rel)) via
		// os.MkdirAll(parent, 0o755) + renameio.WriteFile(0o644)
		copied++
		return nil
	})
```

then a single `git add <folder> .agent-brain/manifests/<host>.json` + commit `migrate: seed <folder> from <host>:<slug>` (adjust to commit.go's author/stamp mechanics). All three methods must respect the engine's existing busy-guard so a mid-Sync admin call fails loudly rather than interleaving — read engine.go's guard and apply the same pattern.

- [ ] **Step 4: Write failing daemon/API tests, then implement the plumbing.** Daemon test (real socket, fake registry with a claude-named `providertest.Fake`): `client.Track` a temp dir → response folder; assert `projects.toml` committed inside the checkout (`git -C checkout log --oneline` grows; `LoadProjects` sees the id), `registry-local.toml` gained the unit, and a subsequent touch in the tracked dir eventually syncs (composition with Task 6's rebuild). `client.Untrack{Purge: true}` → folder gone + registry entry gone. `client.Migrate` (seedDir fixture) → git log shows seed commit BEFORE the live-overlay memory commit (assert relative order by subjects — the layering contract of spec §10). Uninitialized daemon → all three return the actionable 500. api client tests: request bodies marshal, error envelope decodes (mirror existing client_test idioms).

- [ ] **Step 5: Full suite** — `go test ./... -race` → PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/engine/ internal/repo/ internal/daemon/
git commit -m "feat(engine,daemon,api): track/untrack/migrate as engine-goroutine operations

Enrollment (projects.toml + folder + local registry), purge, and
migrate's seed layer all execute on the single engine goroutine via an
admin channel — the CLI stays a pure UDS client (ADR 03 as an API
shape). Seed commits carry the imported-from manifest marker in the
same commit (spec §10: idempotent, ordered register→seed→enroll→cycle
so live state overlays the seed layer)."
```

---

### Task 8: `internal/doctor` package + daemon safety gate + `doctor` command (spec §5, §7)

Spec §5: "The daemon refuses to sync until `doctor` passes." That makes doctor a PACKAGE with the daemon as a consumer, not just a command: `SafetyGate` is the sync-blocking subset (encryption wiring intact), evaluated by the daemon before every cycle, replacing Phase 2's bare `.git`-stat `checkoutState`. The full check battery (gh, service, provider prerequisites, legacy leftovers) is CLI/dashboard surface.

**Files:**
- Create: `internal/doctor/doctor.go`, `internal/doctor/checks.go`, `internal/doctor/gate.go`
- Modify: `internal/daemon/daemon.go` (`checkoutState` → gate; refusal carries the gate's reason), `internal/daemon/api/types.go` (`StatusResponse.StateDetail string` — additive)
- Create: `internal/cli/doctor.go`
- Test: `internal/doctor/doctor_test.go`, `internal/doctor/gate_test.go`, `internal/daemon/daemon_test.go` (additions), `internal/cli/doctor_test.go`

**Boundary (binding):** `doctor` imports `config`/`keys`/`repo`/`provider`/`gitx`/`ghx`/`service` + stdlib. NEVER `daemon`, `daemon/api`, or `cli` — the daemon imports doctor; CLI-only probes (daemon ping) are injected as closures.

**Interfaces:**
- Consumes: `keys.Primitive`, `gitx.Run`/`RunStatus`, `repo.GenerateAttributes`, `ghx.Client`, `service.NewController`, `config.Paths`/`Settings`.
- Produces:

```go
package doctor

type Status int // StatusOK, StatusWarn, StatusFail (String(): "ok","warn","fail")

type CheckResult struct {
	Name   string // stable machine key, e.g. "keyset", "filters", "attributes"
	Status Status
	Detail string // human sentence: what was found
	Fix    string // the exact next action ("run `gh auth login`"), "" when ok
	Fixed  bool   // set by --fix when a repair was applied
}

type Report struct{ Results []CheckResult }
func (r Report) Failed() bool // any StatusFail

// Deps carries everything checks probe. Nil-able fields degrade to a
// named fail/warn, never a panic (a half-initialized machine is doctor's
// PRIMARY audience).
type Deps struct {
	Paths      config.Paths
	Settings   config.Settings
	SettingsErr error            // LoadSettings error, surfaced as its own check
	Registry   *provider.Registry
	GH         *ghx.Client       // nil → "gh" check fails with install guidance
	BinaryPath string            // what filter/helper wiring must point at
	DaemonPing func(ctx context.Context) error // nil → "daemon" check skipped (daemon-side use)
	Enrolled   []repo.Unit       // provider-prerequisite checks scope to what's in use
	Home       string            // provider prerequisite file locations
	Offline    bool              // skip the network check (ls-remote)
}

func Run(ctx context.Context, deps Deps) Report
func Fix(ctx context.Context, deps Deps) (Report, error) // re-Run after applying the three idempotent repairs

// SafetyGate is the sync-blocking subset (spec §5): checkout is a git
// repo; keyset loads; filter/merge wiring present with required=true and
// pointing at binaryPath; root .gitattributes byte-canonical. Cheap
// enough for every cycle (a few config reads + one keyset parse).
func SafetyGate(ctx context.Context, paths config.Paths, registry *provider.Registry, binaryPath string) error
```

**The check battery** (`checks.go`; each check one function, table-registered, deterministic order):
`settings` (SettingsErr) · `keyset` (exists + `keys.Primitive` loads) · `checkout` (dir is a git work tree: `git rev-parse --is-inside-work-tree`) · `filters` (`git config --local --get filter.agentbrain.clean` contains BinaryPath; `required=true`; merge/textconv entries present — reuse the exact keys `gitx.InstallFilters` writes: read install.go, assert against its constants) · `attributes` (checkout root `.gitattributes` == `repo.GenerateAttributes(Registry)`) · `credential-helper` (only when `remote.origin.url` is https: `--get-all credential.helper` includes `auth git-credential`) · `remote` (unless Offline: `git ls-remote --exit-code origin HEAD` with a 5s context timeout; fail names the push consequence: "commits will queue locally") · `gh` (present + `AuthOK`) · `daemon` (DaemonPing when non-nil) · `service` (Controller status; warn-not-fail when not installed — foreground `daemon run` is legitimate) · `registry-local` (loads) · `conflict-log` (warn > 4 MiB: "approaching rotation bound") · `claude-prereqs` (warn if `CLAUDE_CODE_DISABLE_AUTO_MEMORY` set; warn if `~/.claude/settings.json` parses with `"autoMemoryEnabled": false`) · `codex-prereqs` (only when a codex unit is Enrolled: `features.memories = true` in `$CODEX_HOME/config.toml`) · `legacy-leftovers` (warn when `~/.local/bin/ab-claude`, `~/.agent-brain/`, or `~/.config/agent-brain/chezmoi.toml` exists: "bash-era system still present — see spec §10 retirement checklist").
`Fix` applies exactly three repairs, each already idempotent: `gitx.InstallFilters`, `repo.WriteAttributes`, `gitx.InstallCredentialHelper` — then re-runs. Nothing else is auto-fixed (a wrong keyset or dead gh is a human decision).

- [ ] **Step 1: Write the failing tests.** `doctor_test.go`: fabricate machines in `t.TempDir()` — a HEALTHY one (init-shaped: git checkout + real keyset via `keys.Generate` + `InstallFilters` + canonical attributes) asserts every applicable check ok; then one table row per broken axis (delete keyset → `keyset` fail with fix text; corrupt attributes → `attributes` fail; unset required → `filters` fail; add fake `ab-claude` to a PATH-shadowing temp dir + point Home at a tree with `.agent-brain/` → `legacy-leftovers` warn; etc.). `Fix` on a machine with broken filters+attributes → both `Fixed: true` and the re-run ok. `gate_test.go`: healthy → nil; each safety axis broken → error naming that axis. Daemon addition: break attributes in a running test daemon's checkout → next `client.Status` shows `State: "uninitialized"` with `StateDetail` naming attributes; heal → `"ready"` (this REPLACES the bare `.git` stat everywhere `checkoutState` was used — grep it).
- [ ] **Step 2: Run to verify failure** — `go test ./internal/doctor/ 2>&1 | head` → compile FAIL.
- [ ] **Step 3: Implement** package + daemon gate swap (`checkoutState()` body becomes a `doctor.SafetyGate` call — daemon caches `os.Executable()` once at Run start for BinaryPath; on gate error, `state = "uninitialized"`, `StateDetail = err.Error()`, and `TriggerSync`/admin refusals embed it).
- [ ] **Step 4: Implement `internal/cli/doctor.go`** — `doctor [--fix] [--json] [--offline]`: builds `Deps` (paths, settings, `buildRegistry`, `ghx.NewClient()` tolerating ErrMissing → nil GH, `os.Executable()`, DaemonPing = `client.Status` via `newAPIClient`, Enrolled from local registry, Home from `os.UserHomeDir()`), renders aligned rows `ok/warn/FAIL  name  detail` + fix lines, exit code 1 when `Failed()`. `--json` marshals the Report. Test with a fabricated healthy machine + one broken axis (assert exit code + row rendering through cobra's `SetOut`/`SetArgs` execution — follow existing cli test idioms).
- [ ] **Step 5: Full suite** — `go test ./... -race` → PASS.
- [ ] **Step 6: Commit**

```bash
git add internal/doctor/ internal/daemon/ internal/cli/
git commit -m "feat(doctor,daemon,cli): doctor package with daemon safety gate + doctor command

SafetyGate (checkout+keyset+filters+attributes) now IS the daemon's
readiness condition — spec §5's 'refuses to sync until doctor passes'
enforced per cycle, with the reason surfaced through status. Full check
battery covers gh/service/providers/legacy leftovers; --fix applies the
three idempotent wiring repairs only."
```

### Task 9: CLI read-surface polish — `key export|import`, `conflicts`, `--json`, socket pre-check (spec §5, §7; handoff item 4)

**Files:**
- Create: `internal/cli/key.go`, `internal/cli/conflicts.go`
- Modify: `internal/cli/client_commands.go` (`--json` on status/projects; `sync --project`; socket pre-check; projects empty-state message), `internal/cli/service.go` (`service logs`), `internal/cli/merge.go` (typed conflict record), `internal/cli/root.go` (register commands)
- Test: `internal/cli/key_test.go`, `internal/cli/conflicts_test.go`, `internal/cli/client_commands_test.go`, `internal/cli/service_test.go` (additions)

**Interfaces:**
- Consumes: `keys.Export`/`Import`/`ErrKeysetExists`, `config.ValidateSocketPath`, `config.DefaultPaths`, api response types.
- Produces:
  - `agent-brain key export` — prints the armored keyset to stdout (NOTHING else on stdout — it must pipe cleanly); the password-manager reminder goes to stderr: `"This armored keyset IS the recovery artifact — store a copy in your password manager now."`
  - `agent-brain key import [--force]` — reads armored from stdin (trimmed); `keys.Import` refusal (`ErrKeysetExists`) surfaces as `"keyset already exists at <path> — pass --force to replace it (the old keyset can no longer decrypt new commits once replaced)"`; `--force` moves the existing keyset to `keyset.json.bak-<unixts>` before importing (never silently destroys key material).
  - `status --json` / `projects --json` — marshal the api response structs verbatim (indent two spaces); the JSON shape IS `daemon/api`'s types, documented as such in the flag help.
  - `conflicts list [--limit N]` (default 50, newest first, columns: time · path · mode) and `conflicts show <path>` — prints the retained blocks currently present in the checkout copy of `<path>` (read-only; readers don't violate single-writer). Read `internal/crypto/retain.go` FIRST for the exact retain-both block delimiters `MergeFact` emits and scan with those markers — do not invent marker strings. No blocks → `"no retained blocks in <path> — already tidied"` and exit 0.
  - `type conflictRecord struct { Time string `json:"time"`; Path string `json:"path"`; Mode string `json:"mode"` }` in the cli package; `logConflict` (merge.go) refactors to marshal it — writer and reader now share one type, pinned by a round-trip test.
  - `newAPIClient` pre-checks `config.ValidateSocketPath(socketPath)` BEFORE dialing and returns its named-fix error (pointing at `AGENT_BRAIN_RUNTIME_DIR`) instead of letting the dial die with a bare EINVAL (handoff item 4).
  - `sync --project <folder>` — passes Task 7's filter through `Client.Sync` (help text names `agent-brain projects` as the folder source).
  - `service logs [-n N]` — spec §7 surface that Phase 2 never shipped: prints the last N lines (default 100) of `paths.DaemonLogFile()` plus a trailer naming the file path and its `.1` rotation sibling when present. Pure file read — works with the daemon down (that is precisely when logs matter); missing file → `"no daemon log yet at <path>"` exit 0. No follow mode in v1.
  - Projects empty-state message becomes: `"no projects enrolled — run `agent-brain track`"`.

- [ ] **Step 1: Write the failing tests.** key: export on a `keys.Generate`d temp config dir → stdout is EXACTLY the armored string + newline (pipe-clean), reminder on stderr; import round-trips into an empty dir (`keys.Primitive` loads after); import onto existing → refusal naming `--force`; `--force` → `.bak-` file exists AND new keyset decrypts. conflicts: write three records via the refactored `logConflict` (env pointed at a temp file), `list --limit 2` shows newest two; round-trip test `logConflict`→`conflictRecord` unmarshal; `show` against a fixture file containing a real `crypto.MergeFact` retain-both output (produce it by CALLING MergeFact in the test on divergent inputs — never a hand-typed imitation) prints the block; a clean file → tidied message. client_commands: `--json` outputs decode back into the api types; socket pre-check: `t.Setenv("AGENT_BRAIN_RUNTIME_DIR", strings.Repeat("x", 200))` → error mentions the env var, no dial attempted; `sync --project x` sends `SyncRequest{Project: "x"}` (stub server asserts the body). service: `logs -n 2` on a fabricated 5-line daemon.log prints the last two + the path trailer; missing log → friendly exit 0.
- [ ] **Step 2: Run to verify failure** — `go test ./internal/cli/ -run 'Key|Conflict|JSON|Socket' -v 2>&1 | head` → FAIL.
- [ ] **Step 3: Implement** (key commands grouped under one `key` parent command; `conflicts` likewise with `list` the default when no subcommand).
- [ ] **Step 4: Run** — `go test ./internal/cli/ -race` → PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/cli/
git commit -m "feat(cli): key export/import, conflicts list/show, --json surfaces, socket pre-check

Export is pipe-clean (armor on stdout, recovery reminder on stderr);
import refuses to clobber without --force and always .bak's replaced
key material. Conflict records get one shared typed schema between the
merge driver's writer and the new reader."
```

---

### Task 10: `agent-brain init` — the first-run wizard (spec §7; ADRs 04, 08)

Composition of everything above into the onboarding flow: gh → repo → keyset → wiring → config → service → enrollment → first sync. Every step is IDEMPOTENT and separately re-runnable — `init` on a half-initialized machine repairs forward (same philosophy as doctor, which shares the wiring functions). The wizard is huh-driven interactively but **fully scriptable**: every decision has a flag, and huh's accessible mode keeps prompts working without a TTY.

**Files:**
- Create: `internal/cli/init.go`, `internal/cli/initsteps.go` (step functions — logic separated from form rendering so tests never need a TTY)
- Test: `internal/cli/init_test.go`

**Interfaces:**
- Consumes: `ghx.Client`, `keys` (Generate/Import/Primitive/ErrKeysetExists), `gitx` (Run/InstallFilters/InstallCredentialHelper), `repo` (NewLayout/WriteAttributes/NewProjects/Save), `buildRegistry`, `service.NewController`, `api.Client` (Track — enrollment goes through the daemon, Task 7), `huh` forms.
- Produces: `newInitCmd()` registered on root. Flags (each skips its prompt): `--non-interactive` (fail loudly wherever a prompt would be needed instead of asking), `--generate-key` / `--import-key` (mutually exclusive; import reads armored from stdin), `--skip-service`, `--enroll all|none` (default: interactive picker), `--repo-name NAME` (default `agent-brain-memories`). Step functions in initsteps.go, each `func(ctx, *initState) error` with `initState{paths, settings, registry, gh, binaryPath, login, out io.Writer}` — Task 12's testscript drives the whole flow non-interactively.

**The steps, in order (each prints what it did + what's next — spec §7 UX rule):**

1. **Resolve identity**: `os.Executable()` (absolute, symlink-resolved via `filepath.EvalSymlinks`) — everything wired into git config uses THIS path. `config.DefaultPaths`, `LoadSettings`.
2. **gh**: `ghx.NewClient()` → `AuthOK` → `Login`. Failure = actionable stop (`ErrMissing` text / `gh auth login`).
3. **Keyset**: exists → validate via `keys.Primitive` and continue (say so). Missing → prompt generate (first machine) vs import (joining) [flags override]; generate calls `keys.Generate` then PRINTS the export (`keys.Export`) with the password-manager instruction and a huh `Confirm` gate — `"I have stored the keyset in my password manager"` — that must be affirmed (spec §5: the export IS the recovery artifact; `--non-interactive` + `--generate-key` prints it with the instruction and proceeds); import reads armored from stdin via the Task-9 logic.
4. **Repo**: `RepoExists(login, repoName)`; missing → `CreateRepo` (`--private`, description `"agent-brain encrypted memories (github.com/Sawmonabo/agent-brain)"`). `paths.MemoriesDir()` already a git repo → verify `remote.origin.url` points at `<login>/<repoName>` (mismatch = FAIL naming both — never silently adopt a foreign checkout) and skip cloning. Otherwise **clone into a temp sibling and atomically rename** — the daemon may already be running and must never observe a partial checkout:

```go
	partial := memories + ".partial"
	if err := os.RemoveAll(partial); err != nil { return err }
	if err := gh.Clone(ctx, login+"/"+repoName, partial, "--no-checkout"); err != nil { return err }
	// Deterministic branch regardless of the user's init.defaultBranch:
	if _, err := gitx.Run(ctx, partial, "symbolic-ref", "HEAD", "refs/heads/main"); err != nil { return err }
	if err := os.Rename(partial, memories); err != nil { return err }
```

(A daemon cycle racing between the rename and step 5 fails CLOSED — `required=true` filters aren't wired yet, so git refuses plaintext; the cycle degrades and heals on the next tick. Note this in the code comment.)
5. **Wiring**: `gitx.InstallFilters(ctx, memories, binaryPath)` + `gitx.InstallCredentialHelper(ctx, memories, gh.BinaryPath())` + identity for commits if unset (`user.name`/`user.email` — repo-local, `agent-brain daemon` / `agent-brain@<host>` — read commit.go first: if the engine already sets author env per commit, SKIP this and say so in the plan-deviation note).
6. **Repo state**: EMPTY clone (no HEAD commit — `gitx.RunStatus(... "rev-parse", "--verify", "HEAD")` nonzero) → first machine: `repo.WriteAttributes` + `repo.NewProjects().Save(layout.ProjectsFile())` + `mkdir .agent-brain/manifests` + commit `"meta: initialize memories repo"` + `git push -u origin main`. NON-empty → joining machine: `git checkout main` (materializes the tree through the now-wired smudge), then verify root attributes canonical (heal + commit if not, same as scrubIntegrated's rule).
7. **config.toml**: missing → write the commented template (ADR 17: written ONCE, never rewritten — if present, leave untouched and say so):

```toml
# agent-brain configuration (TOML). Deleting this file restores defaults.
# The daemon reads it at startup: `agent-brain service restart` to apply.

[sync]
# ticker: idle fetch/integrate interval.
ticker = "5m"
# debounce: trailing quiet window after a file event before syncing.
debounce = "2s"
# poll: backstop rescan interval for filesystems fsnotify misses.
poll = "45s"

# Per-provider classification overrides (advanced; spec §6). Overriding
# a provider REPLACES its whole table. Classes: fact | derived-index |
# regenerated | ignore.
# [providers.codex]
# classify = [
#   { glob = "memories/raw_memories.md", class = "fact" },
# ]
```

Write via `renameio.WriteFile(paths.SettingsFile(), template, 0o600)`.
8. **Service**: unless `--skip-service`: `service.NewController(binaryPath)` install + start (idempotent: already-installed → report status). WSL2 (`service.IsWSL2()`): print the manual-unit guidance instead of failing (mirror what Phase 2's service command does — read service.go first).
9. **Enrollment**: wait for the daemon socket (bounded: poll `client.Status` up to 15s — service start is async); `registry.All()` → `Discover` each → filter already-enrolled (local registry) → huh `MultiSelect[int]` picker (options labeled `<provider>  <Label>  → <PathGuess>`; global-scope providers grouped as one toggle enrolling all their roots) → per selection: per-project → confirm/edit `PathGuess` (huh `Input` prefilled), `Identify`, remoteless → `Input` for the folder name → id `named/<folder>`; submit `client.Track` per unit. `--enroll all` takes every discovery with PathGuess as-is (remoteless skipped with a printed warning — they need a human name); `--enroll none` skips. Daemon unreachable → print `"daemon not reachable — start it, then run `agent-brain track`"` and continue (init still succeeds; enrollment is resumable).
10. **First sync + summary**: `client.Sync` when anything was enrolled; print the summary (existing `printSummary`), then the next-steps block (`agent-brain status` / `track` / `migrate` when `~/.agent-brain` exists — tie-in to spec §10).

- [ ] **Step 1: Write the failing tests** (`init_test.go` — everything through fakes; NO network, NO real service, NO TTY):
  - Step functions against a `ghxtest.Fake` scripting: auth ok → login → repo missing → create → clone (the fake's Clone "runs" by `git init --bare` at a test remote and real `git clone` — build the fake's clone behavior on real local git so steps 4–6 exercise REAL git; only gh itself is fake).
  - First-machine repo state: after steps 4–6 the checkout has main, canonical `.gitattributes`, committed `projects.toml`, and the bare remote received the push (`git -C bare rev-parse main` succeeds).
  - Joining machine: seed the bare remote first (run first-machine flow into checkout A with keyset K; push), then join with checkout B + imported K → tree materializes, files decrypt (assert a seeded memory file's plaintext in B).
  - Idempotency: run the full non-interactive step sequence TWICE on the same machine → second run reports skip/verify on every step, zero new commits.
  - Foreign-checkout refusal: pre-place a git repo with a different origin at MemoriesDir → step 4 fails naming both URLs.
  - config.toml write-once: pre-write a user-modified settings file → untouched byte-for-byte after init.
  - Keyset confirm gate: `--non-interactive --generate-key` → export printed once with the password-manager line.
  - (Service step: `--skip-service` in ALL tests — Global Constraints forbid live installs; the step function's WSL2/branching is covered by Phase 2's service tests.)
- [ ] **Step 2: Run to verify failure** — `go test ./internal/cli/ -run Init -v 2>&1 | head` → FAIL.
- [ ] **Step 3: Implement** initsteps.go then init.go (huh forms only in init.go; a `--non-interactive` run never constructs a form). Use `huh.NewForm(...).WithAccessible(accessible)` where `accessible = os.Getenv("ACCESSIBLE") != "" || !term.IsTerminal(...)` — `golang.org/x/term` is already in the dependency graph via Charm; verify with `go mod graph | grep x/term` and note the resolved version in the commit body.
- [ ] **Step 4: Run** — `go test ./internal/cli/ -race` → PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/cli/ go.mod go.sum
git commit -m "feat(cli): agent-brain init — gh provisioning, keyset, wiring, service, enrollment

Ten idempotent steps; every prompt has a flag override so init is
fully scriptable (--non-interactive, --generate-key/--import-key,
--skip-service, --enroll). Clone lands via temp-sibling + atomic rename
so a running daemon never sees a partial checkout; branch pinned to
main regardless of user init.defaultBranch; keyset generation gates on
the password-manager confirmation (spec §5: the export IS recovery)."
```

---

### Task 11: `track`, `untrack`, `migrate` commands (spec §7, §10)

The daily-driver enrollment surface plus the one-time importer. All three are thin: discovery + pickers CLI-side, every mutation over Task 7's endpoints.

**Files:**
- Create: `internal/cli/track.go`, `internal/cli/migrate.go`
- Create: `internal/cli/enroll.go` (the shared discovery→confirm→Track flow init's step 9 also calls — extract it THERE if Task 10 inlined it; one implementation, two callers)
- Test: `internal/cli/track_test.go`, `internal/cli/migrate_test.go`
- Modify: `internal/cli/root.go` (register)

**Interfaces:**
- Consumes: `buildRegistry`, provider `Discover`/`Identify`, `api.Client.Track/Untrack/Migrate`, `claude.GuessPath`, `repo.ValidateFolderName`, huh.
- Produces:
  - `track [path]` — no arg: full discovery picker (the Task-10 step-9 flow, shared via enroll.go); with a path argument: resolve THAT project directly (match a discovery whose PathGuess/confirmed path == abs(path), or for claude derive the slug from the path and find its memory dir; nothing found → actionable error naming `~/.claude/projects`). `track --all` = `--enroll all` semantics.
  - `untrack <path|folder> [--purge]` — resolves the enrolled unit by LocalDir prefix-match or repo folder name (ambiguity = error listing candidates); `--purge` requires typed confirmation of the folder name (huh `Input`, or `--yes` flag) before sending `Purge: true` — spec's history-retains note printed either way.
  - `migrate` — spec §10 as-built:
    1. **Pre-flight gate** (spec §10 pre-flight, amended 2026-07-08): if `~/.config/agent-brain/chezmoi.toml` exists, run `chezmoi --config ~/.config/agent-brain/chezmoi.toml diff` (via exec, 30s timeout); NON-EMPTY output or missing chezmoi binary → REFUSE with the adjudication instructions (restore keepers / `chezmoi forget` confirmed deletions / commit+push legacy) and exit 1. Config absent → gate passes silently (machine never ran the bash system, or already retired). `--skip-preflight` exists but prints a red warning citing the spec section (the scrub is the point of no return).
    2. Enumerate `~/.agent-brain/<slug>/` dirs, skipping non-dirs and (inside each) `.lock`/`*.sync-pending` (the seed copy re-filters anyway — belt and suspenders); empty enumeration → `"nothing to migrate"` exit 0.
    3. Per slug: `claude.GuessPath(slug, statDir)` → confirm/edit via the shared picker idiom → `Identify` at the confirmed path → remoteless → prompt name (id `named/<folder>`).
    4. Submit `api.MigrateRequest{Provider: "claude", ProjectID, PreferredFolder, LocalDir: <claude memory dir for the slug — enrollment target>, Slug, SeedDir: ~/.agent-brain/<slug>}` — note `LocalDir` is the LIVE dir (`~/.claude/projects/<slug>/memory`, existing or not-yet-existing), NOT the seed dir; the daemon enrolls it so the overlay layer lands per spec §10 step 4.
    5. Print per-project outcome (`seeded N files` / `already imported — skipped`) and the retirement pointer (spec §10 checklist) once ALL slugs are imported.
- [ ] **Step 1: Write the failing tests.** track: fake registry (adapter fakes with scripted discoveries) + a stub API server on a real temp socket (reuse the daemon test-server helper if one exists — read the daemon tests; else spin `httptest.Server`-over-UDS the way client tests do) asserting exact TrackRequests; path-arg resolution; ambiguous untrack error. migrate: temp `~/.agent-brain` with two slugs (one with `.lock` + `.sync-pending` droppings) + fake chezmoi scenarios — preflight refusal on non-empty diff (fake `chezmoi` script on test PATH echoing a diff), pass on absent config; assert MigrateRequests carry SeedDir≠LocalDir correctly; `--skip-preflight` warning text pinned.
- [ ] **Step 2: Run to verify failure.**
- [ ] **Step 3: Implement** (all prompts through the shared enroll.go helpers with accessible-mode support; `--yes`/`--non-interactive` respected).
- [ ] **Step 4: Run** — `go test ./internal/cli/ -race` → PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/cli/
git commit -m "feat(cli): track/untrack/migrate — enrollment picker + spec §10 importer

Discovery and confirmation stay CLI-side; every mutation rides the
daemon API. migrate enforces the §10 pre-flight (chezmoi delta must be
EMPTY before the seed is read), maps slugs with the shared
filesystem-guided reversal, and enrolls the LIVE memory dir while
seeding from the bash-era tree so layering lands seed-then-overlay."
```

### Task 12: testscript e2e for CLI flows + standing adversarial probe (spec §12; handoff item 6)

Two additions to `test/e2e`: (a) **testscript** coverage of the product flows — txtar scripts running the REAL binary with a fake `gh` on PATH and real local git remotes (spec §12 names testscript as the CLI/e2e harness); (b) a **standing adversarial corpus** — a Go test table of hostile inputs delivered through raw git (bypassing agent-brain, as an attacker with repo write access would), asserting the engine's containment invariants. "Standing" is the contract: later phases APPEND rows, never delete.

**Files:**
- Create: `test/e2e/testscript_test.go`, `test/e2e/scripts/init_first_machine.txt`, `test/e2e/scripts/track_and_sync.txt`, `test/e2e/scripts/migrate.txt`, `test/e2e/scripts/doctor_fix.txt`, `test/e2e/scripts/key_roundtrip.txt`
- Create: `test/e2e/fakegh_test.go` (writes the shim), `test/e2e/adversarial_test.go`
- Modify: `go.mod` (add `github.com/rogpeppe/go-internal` — latest release at `go get` time; record the resolved version in the commit body)

**Interfaces:**
- Consumes: the built binary (`binPath` from the existing `TestMain`), existing harness helpers (`newBareRepo`, `remoteBlob`, `gitRunEnv`), Tasks 1–11 surfaces.
- Produces: `testscript.Params` setup that later scripts extend; custom testscript commands `waitdaemon` (bounded socket poll), `stopdaemon`, `remoteblob` (assert a remote blob for a path is ciphertext / plaintext-free); the fake-gh shim contract below.

**Fake gh shim** (a shell script `fakegh_test.go` writes into each script's PATH dir; behavior driven by env the Setup provides):
- `gh --version` → `gh version 2.96.0 (fake)`; `gh auth status` → exit 0; `gh api user --jq .login` → `fakeuser`.
- `gh repo view fakeuser/<name> --json name` → exit 0 iff `$GH_FAKE_REMOTE` (a bare repo path) exists, else exit 1 + `Could not resolve to a Repository` on stderr.
- `gh repo create <name> --private --description <d>` → `git init --bare "$GH_FAKE_REMOTE"` + echo a fake URL.
- `gh repo clone fakeuser/<name> <dir> -- <args...>` → `exec git clone <args...> "$GH_FAKE_REMOTE" <dir>`.
- `gh auth git-credential` → exit 0 reading stdin (never needed — file-path remotes bypass credentials; the credential WIRING is asserted by unit tests in Task 5, not here — note this in the shim comment so nobody "fixes" it).

**The scripts** (each starts from `Setup`-provided hermetic env: fresh `HOME`, `AGENT_BRAIN_{CONFIG,DATA,RUNTIME}_DIR` under the script's `WORK`, `ACCESSIBLE=1`, `NO_COLOR=1`, `GIT_CONFIG_GLOBAL/SYSTEM=/dev/null`, PATH = shim dir + real git + system):

```txtar
# track_and_sync.txt — the daily-driver loop end-to-end
exec agent-brain init --non-interactive --generate-key --skip-service --enroll none
exec agent-brain daemon run &daemon&
waitdaemon
mkdir $HOME/.claude/projects/-work-proj/memory
exec git init $WORK/proj
exec git -C $WORK/proj remote add origin git@github.com:fakeuser/proj.git
cp memory-fixture.md $HOME/.claude/projects/-work-proj/memory/topic.md
exec agent-brain track --all
exec agent-brain sync
exec agent-brain projects
stdout 'claude .*proj'
remoteblob proj/claude/topic.md !plaintext 'the sentinel memory line'
stopdaemon

-- memory-fixture.md --
the sentinel memory line
```

(The txtar above is the SHAPE — the implementer writes the five scripts with exact stdout assertions against the real command output, adjusting the track path-confirmation to `--all`/non-interactive forms. `migrate.txt` seeds `$HOME/.agent-brain/-work-proj/` and asserts the seed commit lands BEFORE the overlay commit via `exec git -C $AGENT_BRAIN_DATA_DIR/memories log --format=%s` + ordered stdout matches. `init_first_machine.txt` asserts idempotency by running init twice. `doctor_fix.txt` corrupts `.gitattributes` in the checkout, expects doctor exit 1 with `attributes` FAIL, then `doctor --fix` → exit 0. `key_roundtrip.txt` pipes `key export` into `key import --force` under a second config dir and asserts `keys` parity by decrypting a smudged file.)

**Adversarial corpus** (`adversarial_test.go` — two REAL machines via the existing harness; attacker = raw `gitRunEnv` pushes to the bare remote):

| # | Hostile input (pushed raw) | Asserted containment |
|---|---|---|
| 1 | nested `<proj>/claude/.gitattributes` disabling filters | scrubbed from B's checkout after one cycle; never mirrored to B's provider dir; heal commit propagates |
| 2 | folder-level `<proj>/.gitattributes` (above unit dir) | same — Task 1's whole-checkout walk catches it |
| 3 | mutated ROOT `.gitattributes` (filter lines stripped) | healed byte-canonical + committed; subsequent adds still encrypt (remoteblob ciphertext) |
| 4 | `<proj>/claude/.gitignore` + nested `.git` dir | both scrubbed |
| 5 | hostile `projects.toml` (folder `../escape`, duplicate ids) | B's cycle FAILS LOUDLY (degraded/error in status), no file written outside the checkout |
| 6 | manifest JSON with unsafe path keys | load error named, cycle fails loudly, no traversal |
| 7 | memory file whose content starts with the codec magic prefix | commit fails closed at the clean filter (Phase-1 pin, re-asserted here at engine level) |
| 8 | 5k-file burst in one enrolled dir | one coalesced cycle completes; every remote blob ciphertext |

Every row ends with the universal invariant: `remoteBlob` proves NO plaintext sentinel ever appears in ANY object reachable from the bare remote (`git -C bare cat-file --batch-all-objects --batch-check` + content scan helper — write it once as `assertNoPlaintextOnWire(t, bare, sentinels...)`).

- [ ] **Step 1: Write `fakegh_test.go` + `testscript_test.go` harness + ONE script (`init_first_machine.txt`), run, iterate until green.** (Harness-first here: the RED/GREEN unit is the script, and the harness cannot be "failing-test-first" meaningfully.)
- [ ] **Step 2: Write the remaining four scripts; run each** — `go test ./test/e2e/ -run TestScripts -v`.
- [ ] **Step 3: Write `adversarial_test.go` rows 1–8 as a table; run** — `go test ./test/e2e/ -run TestAdversarial -race -v`. Rows 1–4 MUST fail if Task 1's scrub is reverted — prove it once by `git stash`ing the scrub call locally (manual RED check, note the result in the ledger, unstash).
- [ ] **Step 4: Full suite** — `go test ./... -race` → PASS.
- [ ] **Step 5: Commit**

```bash
git add test/e2e/ go.mod go.sum
git commit -m "test(e2e): testscript CLI flows + standing adversarial corpus

Five txtar flows (init/track+sync/migrate/doctor/key) run the real
binary against fake-gh + real local git. Adversarial table delivers
hostile git-meta, registry, manifest, magic-prefix, and burst inputs
through raw pushes and pins containment: scrub+heal, fail-loudly, and
no-plaintext-on-wire across every reachable object."
```

---

### Task 13: Documentation truth pass — spec §5/§6/§7 as-built deltas, pins, CLAUDE.md

Code and spec must not drift (the spec is canonical and section-referenced from code). Record what Phase 3 DECIDED while building, in the spec, with the same one-concern-per-edit discipline as the §10 pre-flight amendment.

**Files:**
- Modify: `docs/00-design-spec.md`, `CLAUDE.md`

**Edits (each its own reviewable hunk):**
1. **§5 filter wiring paragraph** — append the credential mechanism sentence: repo-local `credential.helper` cleared-then-set to `!<absolute gh path> auth git-credential` in the hidden checkout only (never global gitconfig — ADR 08's #9438 hazard); SSH remotes never invoke it; `doctor --fix` re-wires.
2. **§6 adapter interface sentence** — align to as-built: `Discover`/`Identify` (names as shipped); note `WatchRoots` was folded into enrollment (each enrolled unit's LocalDir IS a watch root; discovery-time watching of provider parents is dashboard-era work, not v1); note codex ships as two `repo_subdir` units.
3. **§3 local-state paragraph** — one sentence: `registry-local.toml` units may carry `repo_subdir` mapping a local root under `<folder>/<provider>/<subdir>` (codex: `memories`, `chronicle` — the layout drawn above).
4. **§7 command tree** — update to as-built surface: `track [path] | track --all`, `untrack <path|folder> [--purge]`, `key export|import [--force]`, `conflicts [list|show <path>]`, `doctor [--fix|--json|--offline]`, `init` flags (`--non-interactive`, `--generate-key/--import-key`, `--skip-service`, `--enroll`, `--repo-name`), `--json` on status/projects. Add one line: **dashboard: deferred out of Phase 3; Phase-4 planning decides P4-vs-v1.1** (every API seam it needs exists).
5. **§12 CLI/e2e bullet** — mark testscript as implemented (scripts named), adversarial corpus as a standing suite.
6. **Appendix pins** — add: huh v2.0.3 (`charm.land/huh/v2`) · bubbletea v2.0.8 + lipgloss v2.0.5 (transitive) · rogpeppe/go-internal (resolved version from Task 12's commit) · gh ≥ 2.40 runtime (flags verified at v2.96.0). Refresh the "as of" date.
7. **CLAUDE.md** — Commands section gains the two new test entry points (`go test ./test/e2e/ -run TestScripts` / `-run TestAdversarial`); Conventions gains one line: "CLI never writes inside the memories checkout — mutations go through the daemon API (ADR 03 as-built, Phase 3)."

- [ ] **Step 1: Make the edits** (read each spec section fresh before editing — line numbers in this plan may have drifted).
- [ ] **Step 2: Verify internal consistency** — `grep -n 'WatchRoots\|DiscoverProjects\|ResolveIdentity' docs/00-design-spec.md` shows only the as-built names/notes; `grep -n 'dashboard' docs/00-design-spec.md` shows the deferral note in §7.
- [ ] **Step 3: Commit**

```bash
git add docs/00-design-spec.md CLAUDE.md
git commit -m "docs(spec): record Phase-3 as-built decisions — credential wiring, adapter contract, command surface, pins"
```

---

## Exit criteria (Phase 3 done means ALL of these)

Automated — run from a clean tree at the branch head:

1. `go test ./... -race` — full uncached pass (`go clean -testcache` first), every package.
2. `golangci-lint run` — zero findings.
3. `GOOS=linux GOARCH=amd64 go build ./...` — cross-compiles (WSL2 target stays honest).
4. Boundary greps ALL empty:
   - `grep -rn '"github.com/Sawmonabo/agent-brain/internal/\(cli\|daemon\|doctor\|ghx\|watch\)"' internal/engine/` (engine purity)
   - `grep -rn '"github.com/Sawmonabo/agent-brain/internal/\(daemon\|cli\)"' internal/doctor/`
   - `grep -rn '"github.com/Sawmonabo/agent-brain/internal/' internal/daemon/api/ | grep -v _test` (api imports nothing internal)
   - `grep -rn '"github.com/Sawmonabo/agent-brain/internal/\(cli\|daemon\|engine\|watch\|repo\|config\|keys\|crypto\|doctor\|ghx\)"' internal/provider/` (adapters: provider+gitx only)
   - `grep -rn 'gh auth setup-git\|gh auth token' --include='*.go' internal/ cmd/` (ADR 08 tripwires)
5. `go test ./test/e2e/ -run 'TestScripts|TestAdversarial' -race -v` — all five flows + all eight adversarial rows green.
6. `grep -rn 'Phase 3' internal/ cmd/ --include='*.go'` — no stale "arrives in Phase 3" user-facing strings survive.

Human (user-gated, NOT blocking the branch):

7. Real-Mac smoke: `agent-brain init --repo-name agent-brain-memories-smoketest` end-to-end against real gh (creates a real private repo — user decides when; delete the repo after). Track this repo's own project, write a memory, `agent-brain sync`, verify ciphertext on GitHub via the web UI, `agent-brain doctor` all-ok, then `untrack --purge` + repo delete. **The real `agent-brain-memories` cutover is Phase 4, not this smoke.**

## Phase-4 handoff (carry forward, do not lose)

- `dashboard` P4-vs-v1.1 decision (seams ready: controller endpoints, conflicts log/typed record, unit health, doctor Report).
- `key rotate` (v1.1 per spec §5; keyset format already multi-key).
- Device-flow gh fallback (v1.1 per amended ADR 08).
- gitleaks pre-commit scan on memories repo (v1.1, ADRs 10/14).
- GoReleaser + Homebrew tap + WSL2 runbook + onboarding doc (§13) — Phase 4.
- Retirement execution per machine (spec §10 checklist; `doctor` already detects leftovers) → verified migrate everywhere → ADR-13 scrub → develop→main merge.
