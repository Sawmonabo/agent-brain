# agent-brain v2 — Phase 4: Cutover, Distribution & Product Completion — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Tasks 9–12 are **user-gated runbooks** (destructive and/or outward-facing): an agent may PREPARE them but must not execute a single step without the user's explicit go-ahead for that task, in that session.

**Goal:** Take v2 from "complete product on develop" (Phase 3, exit criteria stamped at `a2ad68b` incl. the real-gh smoke) to **the only system, everywhere**: close every ledgered engineering handoff, ship the deferred product surfaces (dashboard, `key rotate`, secrets scanning), stand up the release pipeline, cut over every machine to the real `agent-brain-memories` repo, retire the bash system per machine, scrub this repo's history (ADR 13), and merge develop→main (ADR 11).

**Architecture:** No new subsystems — Phase 4 grows existing seams. The daemon keeps the single-writer invariant (ADR 03): the two new mutating surfaces (`/v0/reencrypt`, quiesce) live behind the same UDS API + engine-goroutine discipline as track/untrack/migrate. The dashboard is a pure UDS/API client (plus the same read-only file surfaces the `conflicts`/`doctor` commands already use) — it introduces zero new daemon endpoints. Distribution is pure configuration (ADR 16): GoReleaser v2 on tag push, personal Homebrew tap, `go install` fallback.

**Tech Stack:** Go 1.26 (toolchain go1.26.5) · all Phase-1/2/3 packages · **charm.land/bubbletea/v2 v2.0.2 + charm.land/bubbles/v2 v2.0.0 + charm.land/lipgloss/v2 v2.0.1** (already in the module graph via huh; become direct deps in Task 6 ONLY) · GoReleaser v2.16.0 · gitleaks v8.30.1 (runtime + CI tool, never vendored) · git-filter-repo v2.47.0 (scrub runbook only) · gh CLI ≥ 2.40 at runtime.

**Phase roadmap** (this is plan 4 of 4 — the last v2 plan):

1. **Phase 1 (done):** greenfield reset, module + CI/tooling, config, keys, crypto codec, filter/merge plumbing, real-git integration proof.
2. **Phase 2 (done):** repo layout/registries/manifests, mirror in/out, sync engine, watch manager, daemon + UDS API + service install.
3. **Phase 3 (done, develop=`a2ad68b`, pushed):** claude/codex adapters, ghx, daemon API growth (track/untrack/migrate), doctor + safety gate, init wizard, product CLI, testscript e2e + 11-row adversarial corpus, real-gh smoke (found + fixed slug-encoding interop, enrollment-hint, honest-report bugs).
4. **Phase 4 (this plan):** engineering handoffs (Tasks 1–3) → deferred product (Tasks 4–6) → distribution (Tasks 7–8) → **final whole-branch review** → user-gated cutover/retirement/merge/scrub (Tasks 9–12) → epilogue (Task 13). **After this phase there is no "next plan" — leftovers go to `docs/post-v2-backlog.md` (Task 13), each with a recorded reason it is genuinely post-v2.**

Spec: `docs/00-design-spec.md` (§ refs below). ADRs: `docs/decisions/`. Every Phase-3 handoff item (plan §"Phase-4 handoff" + final-gate ledger additions) appears below as a task — none re-deferred: daemon quiesce (T2), recoverState staged-index reset (T1), chezmoi preflight timeout (T3), service typed sentinels (T3), dashboard decision (T6 — decided: BUILD), key rotate (T4 — spec §5's "v1.1" pulled forward per the standing no-deferral directive), gitleaks (T5), GoReleaser/Homebrew/WSL2/onboarding (T7–T8), retirement→scrub→merge (T9–T12). The two Phase-3 items already closed early: modernize/intrange enforcement (landed `bca827c`) and dotted-path slug confirmation (landed `e43669f`).

## Global Constraints

Every task implicitly includes these. Version pins re-verified against primary sources 2026-07-09 (spec Appendix; re-confirm any pin you consume at implementation time with `go list -m` / the release page and record the resolved version in the commit body).

- Branch: ALL code work lands on `develop`. Never commit to `main` — until Task 11, whose entire job is the one merge commit (user-gated).
- Module: `module github.com/Sawmonabo/agent-brain`; `go 1.26`; `toolchain go1.26.5`.
- Package boundaries (spec §8) unchanged and grep-enforced (exit criteria): `engine` imports `gitx`/`provider`/`repo` (+ stdlib/renameio) ONLY; `daemon/api` imports nothing internal; `doctor` never imports `daemon`/`cli`; adapters import `provider`+`gitx`+renameio+stdlib only. NEW: `internal/cli/dashboard` may import `charm.land/bubbletea/v2`, `charm.land/bubbles/v2`, `charm.land/lipgloss/v2`, `internal/daemon/api`, `internal/config`, `internal/doctor`, `internal/repo`, `internal/service` — and is the ONLY package outside `cli` root allowed direct bubbletea/lipgloss imports. Everything else keeps the Phase-3 rule: huh/fang only.
- **Single-writer invariant (ADR 03):** the CLI process never writes inside the memories checkout. New mutations follow the pattern: `key rotate`'s re-encrypt commit is an engine admin op behind `POST /v0/reencrypt`; quiesce is daemon state. The only CLI-side checkout exceptions remain `init`'s initial clone and `doctor --fix`'s `.git/config`/`.gitattributes` re-wiring.
- Tests: stdlib `testing` + `go-cmp` ONLY (ADR 15). Table-driven, `t.Parallel()`, `t.TempDir()`. Real system git with `git init --bare` fake remotes; **no network, ever**; gh and gitleaks are faked at unit level (runner seams) and shimmed with PATH scripts in testscript e2e. No live service installs in tests. All paths via `AGENT_BRAIN_CONFIG_DIR`/`AGENT_BRAIN_DATA_DIR`/`AGENT_BRAIN_RUNTIME_DIR`.
- **Fork-bomb rule (standing, incident 2026-07-08):** never point git filter/merge wiring at a test binary; build the real binary once in `TestMain` (pattern: `test/e2e/harness_test.go`). Run suites foreground with `(ulimit -u 1400; go test ./... -race -count=1)`.
- Safety invariants (spec §5, §11): keyset never enters any repo; plaintext memory content never reaches a git object (e2e asserts ciphertext on the wire — Task 4's rotate e2e re-asserts it under the NEW primary); `filter.agentbrain.required=true` fail-closed; git-meta scrub contract binding; codex secret-adjacency (only `memories/` + `memories_extensions/chronicle/`, never `$CODEX_HOME`).
- **Release/secrets hygiene (new this phase):** release artifacts contain the binary + docs only — `goreleaser release --snapshot` output is inspected in Task 7 to prove no testdata/keyset material rides along. The tap-push PAT lives ONLY in GitHub Actions secrets (fine-grained, contents:write on `homebrew-tap` only). Tags are immutable once pushed (GoReleaser immutable-releases policy) — never retag.
- **User-gate rule (Tasks 9–12):** anything that creates/deletes GitHub repos, pushes to `main`, force-pushes, installs on other machines, or deletes user files executes only on the user's explicit instruction, task by task. Prepared commands are presented first, verbatim.
- Formatting/lint unchanged: gofumpt; golangci-lint v2.12.2 strict-0; every `//nolint` carries linter + reason. Conventional Commits; one commit per task minimum. lefthook pre-push runs the race suite — budget for it.
- The age key (`~/.config/agent-brain/key.txt`) and `main`'s bash system stay untouched until their scripted retirement steps inside Tasks 9/10/12.

---

### Orientation: what Phases 1–3 already provide

Phase 4 consumes these existing surfaces — import them, never re-implement:

- `internal/engine` — `Sync(ctx, units) (Report, error)`; admin ops `RegisterProject(ctx, providerName, id, preferredFolder) (string, error)`, `PurgeProject(ctx, folder) error`, `SeedProject(ctx, folder, providerName, slug, srcDir) (SeedReport, error)` — all run `prepareCheckout` (recover → whole-checkout scrub → heal-commit) as preamble; `recoverState` (recover.go — Task 1 extends it); consts `remoteName`, `defaultBranch`.
- `internal/gitx` — `Run(ctx, dir, args...) (Result, error)` (non-zero exit = error), `RunStatus(ctx, dir, args...) (Result, error)` (exit code as data in `Result`).
- `internal/keys` — `Generate(path)`, `Export`, `Import`, `Primitive(path) (tink.DeterministicAEAD, error)`, `ErrKeysetExists`; atomic writes via renameio; Tink `keyset.NewManagerFromHandle` is available for Task 4's `Rotate`.
- `internal/daemon` — engine goroutine `loop`; UDS server (`server.go`) with `mux.HandleFunc("/v0/...")` + `postHandler(ctrl.X)` pattern; per-cycle registry reload; busy guard ("daemon busy with a sync cycle — try again", server.go:675 area).
- `internal/daemon/api` — typed `Client` (`Status`/`Sync`/`Projects`/`Track`/`Untrack`/`Migrate`, private `do(ctx, method, path, in, out)`); request/response types are the ONLY daemon↔CLI shared surface. Task 2/4 grow both sides.
- `internal/config` — `Paths` + env overrides; `Settings` strict TOML (`SyncSettings`, `ProviderSettings`; unknown key = error) — Task 3 adds `MigrateSettings`.
- `internal/service` — `Controller` interface via `NewController(binaryPath)`: install/uninstall/start/stop/status; `IsWSL2()`. Task 3 adds typed sentinels.
- `internal/doctor` — checks package + `SafetyGate` subset (membership rule documented in gate.go: a check gates only if a cycle cannot safely run while it fails AND cannot repair it); `Report` types the dashboard's Doctor view renders.
- `internal/cli` — cobra tree (fang-wrapped); `newAPIClient`, `reportWriter`, `explainDown`, `isAccessible()` (EOF-keeps-prefill contract documented); `conflicts` command's conflict-log loader (dashboard Conflicts view reuses it); `Version = "dev"` at root.go:7, stamped via `-ldflags "-X github.com/Sawmonabo/agent-brain/internal/cli.Version=..."` (cmd/agent-brain/main.go wires `fang.WithVersion(cli.Version)`).
- `internal/cli/migrate.go` — `preflightTimeout` const (line ~26, 30s) bounding the `chezmoi --config ~/.config/agent-brain/chezmoi.toml diff` subprocess; Task 3 makes it a setting.
- `test/e2e` — real-git two-machine harness (`newBareRepo`, `newMachine`, `binPath`, `TestMain` builds the binary once); 5 testscript flows; `adversarial_test.go` standing corpus (11 rows, APPEND-only, every row ends on `assertNoPlaintextOnWire`, every defense needs RED proof).
- Ciphertext discriminator: magic prefix `agb1\x00` (test/e2e/roundtrip_test.go:11) — wire assertions in Tasks 4/9 reuse it.

Exact signatures for anything you touch: read the package source first; the briefs quote the load-bearing ones.

---

### Task 1: Engine — `recoverState` resets a crash-staged index (Phase-3 final-review F3 / t12-c1)

**The bug being closed:** `recoverState` aborts interrupted rebases/merges but ignores the INDEX. A crash (SIGKILL, power loss) between a cycle's `git add`/`git rm --cached` and its `git commit` leaves staged entries behind. The next cycle's conservative deletion propagation (`git rm` without `--force` — deliberate, protects uncommitted user edits) then sees index≠HEAD and refuses with "local modifications", wedging every subsequent cycle for that path. The reviewer traced this in the Q4 gate; the fix was deferred to Phase 4 because it needs its own design + tests, and this is that task.

**Design:** after the abort steps, if `HEAD` resolves and the index differs from it, run `git reset --quiet` (mixed). A mixed reset never touches worktree files — and the engine treats the index as wholly derived state (every cycle re-stages exactly what mirror-in/scrub decide), so unconditionally clearing staged residue is safe at every entry point (`prepareCheckout` calls `recoverState` first in Sync AND all admin ops — that blast radius is the point: the wedge dies everywhere at once). The `rev-parse --verify HEAD` gate covers the unborn-branch window during a brand-new checkout, where `git reset` would fail and there is nothing to wedge anyway.

**Files:**
- Modify: `internal/engine/recover.go`
- Test: `internal/engine/recover_test.go` (extend — read the existing rebase-abort test first and follow its harness idioms)

**Interfaces:**
- Consumes: `gitx.Run`, `gitx.RunStatus` (exit code as data).
- Produces: no signature change — `recoverState(ctx) error` gains the reset behavior; Tasks 2/4 and every existing caller inherit it via `prepareCheckout`.

- [ ] **Step 1: Write the failing tests** — append to `internal/engine/recover_test.go`, reusing its existing checkout-builder helpers:

```go
// TestRecoverStateResetsStagedDeletion reproduces the F3 wedge shape: a
// crash left a staged deletion (git rm --cached) with the worktree file
// still present. recoverState must clear the staged entry and leave the
// worktree file untouched — a mixed reset, never --hard.
func TestRecoverStateResetsStagedDeletion(t *testing.T) { ... }

// TestRecoverStateResetsStagedModification stages a content change
// (git add after editing) and asserts recoverState unstages it while the
// EDITED bytes stay in the worktree (mirror-in owns reconciling them).
func TestRecoverStateResetsStagedModification(t *testing.T) { ... }

// TestRecoverStateNoopOnCleanIndex asserts a clean checkout stays
// byte-for-byte clean: git status --porcelain empty before and after,
// and no error — recoverState runs at the top of EVERY cycle, so the
// happy path must be free of churn.
func TestRecoverStateNoopOnCleanIndex(t *testing.T) { ... }

// TestRecoverStateSurvivesUnbornHead runs recoverState in a git init'd
// checkout with no commits (init's window before the first skeleton
// commit) and asserts it returns nil — the rev-parse HEAD gate.
func TestRecoverStateSurvivesUnbornHead(t *testing.T) { ... }
```

Write them fully: build the checkout with the package's existing `mustGit` helper, commit a file, stage the crash shape (`git rm --cached <file>` / edit + `git add`), call `e.recoverState(ctx)`, assert with `git diff --cached --quiet` exit code (via `gitx.RunStatus`) + `os.ReadFile` on the worktree copy.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/engine/ -run TestRecoverState -v`
Expected: the two staged-state tests FAIL (staged entries survive recoverState today); noop + unborn tests may pass — that asymmetry IS the RED proof. Record the failure output in the task report.

- [ ] **Step 3: Implement** — extend `recoverState` in `internal/engine/recover.go` after the abort loop:

```go
	// A crash between a cycle's `git add`/`git rm --cached` and its commit
	// leaves a staged index the aborts above never touch; the next cycle's
	// conservative deletion propagation then refuses ("local modifications")
	// and the folder wedges (Phase-3 final review F3). The index is wholly
	// derived state — every entry point re-stages what it needs — so clear
	// residue with a MIXED reset (worktree untouched). Unborn HEAD (a brand
	// new checkout before its first commit) has nothing to reset or wedge.
	if _, err := gitx.Run(ctx, e.checkout, "rev-parse", "--verify", "HEAD"); err == nil {
		staged, err := gitx.RunStatus(ctx, e.checkout, "diff", "--cached", "--quiet")
		if err != nil {
			return fmt.Errorf("recover: git diff --cached: %w", err)
		}
		if staged.ExitCode != 0 {
			if _, err := gitx.Run(ctx, e.checkout, "reset", "--quiet"); err != nil {
				return fmt.Errorf("recover: git reset: %w", err)
			}
		}
	}
	return nil
```

(`gitx.Result{Stdout, Stderr string; ExitCode int}` — verified against gitx.go:24 while writing this plan; `RunStatus` returns the code as data, `Run` errors on non-zero.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/engine/ -race -count=1`
Expected: PASS, whole package (the existing rebase/merge abort tests prove ordering didn't regress).

- [ ] **Step 5: Blast-radius check + commit**

The deletion-propagation wedge test from Phase 3 (`mirror_in` conservative `git rm` behavior) must still pass — the conservative semantics STAY; only crash residue is cleared. Run `go test ./internal/engine/ ./internal/daemon/ -race -count=1`, then:

```bash
git add internal/engine/recover.go internal/engine/recover_test.go
git commit -m "fix(engine): recoverState resets crash-staged index (mixed reset, unborn-HEAD safe)"
```

### Task 2: Daemon quiesce API + init/doctor-fix integration (Phase-3 final-review F2)

**The bug being closed:** re-running `agent-brain init` (or `doctor --fix`) beside a live daemon races the engine goroutine: init's repo-state heal-push and the daemon's cycle contend on git locks — transient, loud, no corruption, but exactly the kind of flake that erodes trust in the wizard. The accepted Phase-3 workaround was "the error is transient"; the proper fix, designed here, is a **quiesce primitive**: the CLI asks the daemon to hold cycles for a bounded window, does its checkout surgery, then releases.

**Design decisions (locked):**
- Quiesce is **TTL-bounded** (max 600s, min 1s; requested TTL clamped) with **auto-release** — a crashed init can never wedge the daemon permanently.
- While quiesced: tick/watch-triggered cycles are SKIPPED (one log line each); explicit `/v0/sync` and the mutating ops (`/v0/track|untrack|migrate|reencrypt`) are REFUSED with an error naming the expiry — silently queueing them would defeat the point.
- Release is idempotent; a fresh `POST /v0/quiesce` while quiesced REPLACES the deadline (last writer wins — same CLI retrying).
- `/v0/status` gains `QuiescedUntil *time.Time` so `status` and the dashboard render it honestly.
- `init`/`doctor --fix` quiesce (120s) when the daemon socket answers, and release (best-effort, deferred) when done. Daemon down → behavior unchanged.

**Files:**
- Modify: `internal/daemon/daemon.go` (loop skip + state), `internal/daemon/server.go` (endpoints), `internal/daemon/api/types.go` + `internal/daemon/api/client.go` (types + `Quiesce`/`Resume` methods), `internal/cli/initsteps.go` (repo-state step), `internal/cli/doctor.go` (--fix path), `internal/cli/status.go` (render)
- Test: `internal/daemon/daemon_test.go`, `internal/daemon/api/client_test.go`, `internal/cli/init_test.go` (fake-daemon records quiesce calls), `internal/cli/status_test.go`

**Interfaces:**
- Consumes: daemon loop select + busy-guard pattern; `postHandler` helper in server.go.
- Produces (Task 6's dashboard and Task 4's rotate rely on these):
  - `POST /v0/quiesce` body `api.QuiesceRequest{Seconds int}` → `api.QuiesceResponse{Until time.Time}`
  - `DELETE /v0/quiesce` → `api.QuiesceResponse{Until time.Time}` (zero time = released)
  - `func (c *Client) Quiesce(ctx context.Context, seconds int) (QuiesceResponse, error)`
  - `func (c *Client) Resume(ctx context.Context) (QuiesceResponse, error)`
  - `StatusResponse.QuiescedUntil *time.Time` (nil when not quiesced)

- [ ] **Step 1: Write the failing daemon tests** — follow `TestDaemonWatchesSyncsAndReports`'s harness (real daemon, temp dirs, generous flake-ceiling deadlines — that test's 30s comment explains the calibration):

```go
// TestDaemonQuiesceSkipsCycles quiesces via the API, writes a memory
// file (which would normally debounce-trigger a cycle), waits past the
// debounce window, and asserts NO cycle ran (status cycle counter /
// summary unchanged). Then resumes and asserts the next trigger DOES run.
func TestDaemonQuiesceSkipsCycles(t *testing.T) { ... }

// TestDaemonQuiesceExpires quiesces with Seconds=1, waits past expiry,
// triggers, and asserts a cycle runs WITHOUT an explicit resume.
func TestDaemonQuiesceExpires(t *testing.T) { ... }

// TestDaemonQuiesceRefusesMutations quiesces, then asserts /v0/sync and
// /v0/track return errors naming the quiesce expiry (errors.As on the
// api error type; substring "quiesced until").
func TestDaemonQuiesceRefusesMutations(t *testing.T) { ... }
```

- [ ] **Step 2: Run to verify RED** — `go test ./internal/daemon/ -run TestDaemonQuiesce -v` → FAIL: unknown endpoint (404 from mux) / undefined client methods (compile error). Either is a valid RED.

- [ ] **Step 3: Implement daemon-side** — quiesce state lives beside the busy guard (same mutex discipline); the loop checks the deadline before starting any cycle:

```go
// quiesced is the daemon-side hold: zero deadline = not quiesced.
// It shares the busy guard's mutex — "may a cycle start now?" must be
// ONE atomic read across busy+quiesced, or a cycle can slip through
// between a quiesce write and the loop's check.
//
// Sketch (adapt names to the daemon's actual guard fields — read
// daemon.go's loop + server.go's busy check first):
//
//	d.mu.Lock()
//	blocked := d.busy || d.now().Before(d.quiescedUntil)
//	d.mu.Unlock()
//
// The loop's tick/trigger arms skip when blocked-by-quiesce (one
// slog line: "cycle skipped: quiesced until %s"); they do NOT
// reschedule — the next tick fires normally after expiry, so
// auto-release needs no timer of its own.
```

Endpoints in server.go via the existing `postHandler` pattern (the DELETE verb shares the `/v0/quiesce` route through a method switch). Clamp `Seconds` to [1,600] server-side; reply with the computed `Until` (use the daemon's injected clock, never `time.Now` directly, so the expiry test can fake time if the daemon already has that seam — if it does not, a 1-second real TTL keeps the test honest and fast). Mutating handlers and the sync handler check the deadline FIRST and return `fmt.Errorf("daemon quiesced until %s — retry after, or release with the CLI that requested it", until.Format(time.RFC3339))`.

- [ ] **Step 4: Implement client + status render** — `Quiesce`/`Resume` on `api.Client` via `do()`; `StatusResponse.QuiescedUntil`; `status` prints `quiesced until <t> (<remaining>)` in yellow-free plain text (NO_COLOR-safe).

- [ ] **Step 5: Run daemon+api tests GREEN** — `go test ./internal/daemon/... -race -count=1` → PASS.

- [ ] **Step 6: Wire init/doctor-fix (test-first)** — extend the fake daemon in `internal/cli/init_test.go` (pattern: `startFakeDaemonForEnrollment`, init_test.go:901) to record quiesce/resume hits; assert the repo-state step quiesces when the socket answers and resumes after; assert init with NO daemon socket behaves exactly as before (no error). Same for `doctor --fix`. Implement: in the repo-state step and doctor --fix, `if client := tryAPIClient(); client != nil { client.Quiesce(ctx, 120); defer client.Resume(ctx) }` — best-effort: quiesce errors are logged, never fatal (the daemon may be mid-shutdown; the old transient-error behavior is the fallback, not a failure).

- [ ] **Step 7: Full-package GREEN + commit**

```bash
go test ./internal/daemon/... ./internal/cli/ -race -count=1
git add -A internal/daemon internal/cli
git commit -m "feat(daemon): TTL-bounded quiesce API; init/doctor --fix hold cycles during checkout surgery"
```

### Task 3: Polish cluster — configurable migrate preflight timeout + service typed sentinels (t11-c2, t10-1)

Two small, unrelated hardening items batched as one reviewable unit (each has its own commit).

**3a — chezmoi preflight timeout becomes a setting.** `internal/cli/migrate.go:26` hardcodes `preflightTimeout = 30 * time.Second` around the `chezmoi diff` subprocess. A cold NFS home or a huge legacy tree can exceed it, and the operator has no recourse. Move it to strict-TOML settings with the current value as default.

**Files:** modify `internal/config/settings.go` (+ its test), `internal/cli/migrate.go` (+ test).

**Interfaces produced:**
```go
// config.MigrateSettings — [migrate] table.
type MigrateSettings struct {
	PreflightTimeout Duration `toml:"preflight_timeout"` // default 30s; must be >0 and ≤10m
}
```
(Follow however `SyncSettings` already handles durations — reuse its `Duration` type/validation idiom EXACTLY; read settings.go first. Unknown keys stay errors — strict decoding is a Phase-2 contract.)

- [ ] **Step 1 (RED):** settings test rows: `[migrate] preflight_timeout = "2m"` parses; `"0s"` and `"11m"` are validation errors naming the bounds; absent table yields the 30s default. Migrate test: inject the setting via the test config file and assert the subprocess context deadline honors it (observable via a fake `chezmoi` PATH shim that sleeps — the e2e PATH-shim pattern from Phase 3, never a real chezmoi).
- [ ] **Step 2:** verify RED (`go test ./internal/config/ ./internal/cli/ -run 'Settings|Preflight' -v`).
- [ ] **Step 3:** implement both sides; delete the const.
- [ ] **Step 4:** GREEN + commit `feat(config): [migrate] preflight_timeout setting (default 30s) replaces hardcoded const`.

**3b — service package typed sentinels.** `internal/service` reports "already installed/not installed" conditions as bare formatted errors; callers (init's service step, `service install|uninstall`) can only string-match. Export sentinels and map kardianos/service's error shapes to them at the package boundary — string inspection happens ONCE, inside the package, under test.

**Interfaces produced:**
```go
// internal/service sentinels — callers branch with errors.Is.
var (
	ErrAlreadyInstalled = errors.New("service already installed")
	ErrNotInstalled     = errors.New("service not installed")
)
```

- [ ] **Step 1 (RED):** service package test: a Controller whose kardianos layer reports the already-exists shape yields `errors.Is(err, ErrAlreadyInstalled)`. CLI test: `service install` twice → second run prints "already installed — nothing to do" and exits 0 (idempotent UX; init's service step relies on the same branch).
- [ ] **Step 2:** verify RED.
- [ ] **Step 3:** implement the mapping in `internal/service/service.go`; update `internal/cli/service.go` + init's service step to `errors.Is`.
- [ ] **Step 4:** GREEN + commit `feat(service): typed ErrAlreadyInstalled/ErrNotInstalled sentinels; idempotent install UX`.

**Q1 REVIEW GATE** after Task 3: dispatch a reviewer over the diff range from the phase's base commit (derive from develop's log — NEVER from remembered worktree SHAs; cherry-pick rewrites them). Scope: Tasks 1–3 vs this plan + spec §4/§5. Gate closes on PASS or all findings fixed/accept-documented in the ledger.

### Task 4: `key rotate` — new primary + daemon-side full re-encrypt (spec §5, pulled forward from v1.1)

Spec §5 designed rotation into the key model from day one ("Tink keysets are natively multi-key... Rotation costs one full re-encrypt commit") and parked the command at v1.1. Per the standing no-deferral directive it lands now: the keyset format needs zero changes, and a compromised-key response with no tooling is not a capability, it's a promise.

**Design (locked):**
- `keys.Rotate(path)` adds a fresh AES256_SIV key via `keyset.NewManagerFromHandle` + `manager.Add(daead.AESSIVKeyTemplate())` + `manager.SetPrimary(newID)`, then atomically rewrites the file (renameio, same as Generate). **Old keys are never removed or disabled in v2**: history blobs (old commits) and not-yet-reencrypted peers still need them to smudge; a destroy/disable lifecycle is post-v2 backlog (recorded with reasoning in Task 13).
- The re-encrypt is an **engine admin op** (`ReencryptAll`) behind **`POST /v0/reencrypt`** — single-writer discipline (ADR 03). It runs `prepareCheckout`, then `git add --renormalize .` (re-runs the clean filter over every filter-subject file → fresh ciphertext under the new primary — deterministic AEAD means EVERY blob changes exactly once), commits `chore(key): rotate primary key`, and pushes through the existing push path.
- **Ordering contract with the fleet:** the moment machine A rotates and pushes, machines without the new key fail closed on smudge (degraded unit; doctor names `key import`). That is correct fail-closed behavior, not a bug — `key rotate` prints the required next step ("run `agent-brain key export` here, `key import --force` on every other machine NOW") before touching anything.
- `key rotate` REFUSES when the daemon is down: rotating the keyset without the immediate re-encrypt leaves the repo mixed-primary indefinitely, which silently defers the security value the user just asked for. Error names the fix (`agent-brain service start`).
- CLI flow: confirm (huh, EOF-safe: the prefilled answer is ABORT — an unattended pipe must not rotate keys; this is the same accessible-mode EOF contract documented on `isAccessible()`), rotate the file, print the new armored export + password-manager prompt, call `/v0/reencrypt`, render the summary (files re-encrypted, pushed/queued).

**Files:**
- Modify: `internal/keys/keys.go` (+`Rotate`), `internal/engine/admin.go` (+`ReencryptAll`), `internal/daemon/server.go` + `internal/daemon/api/types.go` + `client.go` (endpoint + `Reencrypt` method), `internal/cli/key.go` (+`rotate` subcommand)
- Test: `internal/keys/keys_test.go`, `internal/engine/admin_test.go`, `internal/daemon/daemon_test.go`, `internal/cli/key_test.go`, `test/e2e/rotate_test.go` (two-machine wire proof)

**Interfaces:**
- Consumes: `keyset.NewManagerFromHandle` / `Manager.Add` / `Manager.SetPrimary` / `Manager.Handle()` (tink-go v2.7.0 — verify exact signatures with `go doc` before writing), `prepareCheckout`, the existing commit/push helpers in engine, `postHandler`, Task 2's quiesce-refusal check (reencrypt is a mutation: refused while quiesced).
- Produces:
  - `func Rotate(path string) error` (keys) — error if keyset missing (`fs.ErrNotExist` wrapped: rotation without a keyset is `key import`'s job, not Generate's).
  - `func (e *Engine) ReencryptAll(ctx context.Context) (ReencryptReport, error)` with `ReencryptReport{Files int, Pushed bool, PushQueued bool}`.
  - `POST /v0/reencrypt` → `api.ReencryptResponse{Files int, Pushed, PushQueued bool, Error string}`; `func (c *Client) Reencrypt(ctx context.Context) (ReencryptResponse, error)`.

- [ ] **Step 1 (RED, keys):** `TestRotateAddsPrimaryKeepsOldKeys` — Generate; capture primary key ID + `Primitive` roundtrip of a sample; `Rotate`; assert: keyset now has 2 keys, primary CHANGED, old ciphertext still decrypts (old key retained), new encryptions differ from pre-rotation ciphertext for identical plaintext (primary switch observable via the deterministic property). `TestRotateRefusesMissingKeyset` — errors mentioning `key import`. Run → FAIL undefined `Rotate`.
- [ ] **Step 2 (keys impl + GREEN):** implement `Rotate` (manager add + set-primary + atomic `write`); `go test ./internal/keys/ -race -count=1` → PASS. Commit `feat(keys): Rotate — new AES256_SIV primary, old keys retained for history`.
- [ ] **Step 3 (RED, engine):** `TestReencryptAllRenormalizesCommitsPushes` — harness checkout with the REAL binary's filters wired (TestMain-built binary — NEVER the test binary, fork-bomb rule) and two committed memory files; rotate the keyset fixture; `ReencryptAll`; assert: exactly one new commit, BOTH blobs' ciphertext changed on the fake remote, `Files == 2`, worktree plaintext byte-identical. `TestReencryptAllNoopWedgeFree` — running it twice: second run commits nothing (renormalize under unchanged primary is a no-op) and reports `Files == 0`. Run → FAIL undefined.
- [ ] **Step 4 (engine impl + GREEN):** implement via `prepareCheckout` → `gitx.Run(ctx, checkout, "add", "--renormalize", ".")` → reuse the existing commit helper (skip commit when index clean) → push path. Count files from `git diff --cached --name-only` before commit. GREEN + commit `feat(engine): ReencryptAll admin op — renormalize under new primary, one commit, push`.
- [ ] **Step 5 (daemon+api+cli, RED→GREEN):** endpoint wiring test (busy-guard + quiesce-refusal rows follow Task 2's pattern); CLI test rows: daemon-down refusal message names `service start`; EOF'd stdin ABORTS before any file change (assert keyset mtime unchanged); happy path prints export + calls Reencrypt (fake daemon records the hit). Commit `feat(cli,daemon): key rotate — confirm, rotate, print export, daemon re-encrypt`.
- [ ] **Step 6 (e2e wire proof — APPEND to the standing corpus contract):** `test/e2e/rotate_test.go`, two-machine harness: A writes memory → sync → B syncs (sees plaintext). A rotates (via the real binary: `key rotate --yes`) → assert every memory blob on the bare remote CHANGED and still starts `agb1\x00`, sentinel plaintext absent (reuse `assertNoPlaintextOnWire`). B syncs WITHOUT the new key → assert degraded + doctor names key import (fail-closed proof). B `key import --force` (new export) → sync → plaintext restored. RED proof for the fail-closed row: temporarily neutering the rotate (no SetPrimary) must flip the "blobs changed" assertion — record the RED output.
- [ ] **Step 7:** full suite `(ulimit -u 1400; go test ./... -race -count=1)` foreground → PASS. Commit `test(e2e): rotate wire proof — re-encrypt on wire, stale-key fail-closed, key import recovery`.

### Task 5: gitleaks — repo CI/hook scanning + `agent-brain scan` for memory plaintext (ADRs 10/14)

Two deliverables with one tool (gitleaks v8.30.1 — runtime/CI dependency, NEVER vendored, same posture as gh):

**5a — this repo scans itself.** CI + lefthook gain gitleaks so the code repo can never accumulate committed secrets (relevant NOW: post-scrub the repo may go public, and Task 12's scrub verification uses the identical scan).

**5b — `agent-brain scan`.** The memories repo's threat model (spec §5) protects GitHub at rest — but memories THEMSELVES accumulate pasted secrets (API keys in a "how I configured X" note), which ride encrypted today and leak the day the plaintext is exported/shared. `agent-brain scan [--project <folder>] [--json]` runs the user's installed gitleaks binary (`gitleaks dir <plaintext-dir> --no-banner --report-format json --report-path -`) over enrolled units' provider dirs and renders findings. The `git`/`dir` mode family is the ADR-14-verified surface (`detect`/`protect` were deprecated at v8.19.0 — ADR 14's table row records it). **Decided non-goal, with reasoning:** the daemon does NOT scan during sync cycles — a per-cycle subprocess adds latency and false-positive fatigue to every save, for zero wire-exposure reduction (the wire is ciphertext regardless); on-demand + doctor visibility is the right cost/benefit for a single-user tool. This reshapes ADR 14's use-(1) framing ("scan before memories-repo commits") — Task 13.1 amends ADR 14 accordingly. Recorded here so it reads as a decision, not a deferral.

**Files:**
- Create: `.gitleaks.toml`, `internal/cli/scan.go`, `internal/cli/scan_test.go`
- Modify: `lefthook.yml` (pre-commit job), `.github/workflows/ci.yml` (job), `internal/doctor/checks.go` (+advisory `secrets-scan` check: reports whether gitleaks is installed, StatusInfo/StatusWarn only — NEVER SafetyGate; gate.go's membership rule forbids it), `internal/doctor/checks_test.go`

**Interfaces:**
- Consumes: the ghx runner-seam PATTERN (an injected exec func — copy the shape, not the package: scan shells out to gitleaks the way ghx shells to gh); `repo.LocalRegistry` for enrolled units; `reportWriter`.
- Produces: `scan` cobra command; doctor check name `secrets-scan`.

- [ ] **Step 1 (repo scan config):** ground truth already in hand — `gitleaks git --no-banner .` over develop's full history (226 commits, 6.4 MB) ran CLEAN on 2026-07-09 with the local brew gitleaks 8.30.1. So `.gitleaks.toml` STARTS EMPTY of allowlists and only ever grows justified entries; if a Phase-4 change introduces a legitimate high-entropy fixture, allowlist it path+rule-scoped with a one-line justification, using the documented schema: global `[[allowlists]]` (with `paths = ['''...''']`) or rule-scoped `[[rules.allowlists]]` — never a blanket regex. A REAL finding at any point → STOP, escalate to the user.
- [ ] **Step 2 (hooks + CI):** lefthook pre-commit job `gitleaks git --staged --no-banner` — the `--staged` flag is v8.30.1's own recommendation ("scan staged commits (good for pre-commit)", from `gitleaks git --help`, verified locally 2026-07-09; `protect` is deprecated since v8.19.0 per ADR 14). CI job runs the full-history scan on every push with a checksum-pinned v8.30.1 binary — decided over the official gitleaks-action (v3.0.0, 2026-05-30) even though personal accounts need no license key ("If you are scanning repos that belong to a personal account, then no license key is required" — its README): the plain binary keeps CI behavior byte-identical to the local hook and adds zero third-party action surface (ADR 12's SHA-pinning posture applies either way). Verify both fire: commit a canary AWS-style test key in a scratch branch → hook blocks → delete branch.
- [ ] **Step 3 (RED, scan command):** CLI tests with a PATH-shim `gitleaks` script (e2e pattern): happy path (shim emits a JSON finding → table row + exit 1), clean path (empty JSON → "no findings" + exit 0), gitleaks-missing path (actionable install hint, exit 1, message names `brew install gitleaks`), `--project` filters to one unit. Run → FAIL (no command).
- [ ] **Step 4 (impl + GREEN):** implement scan.go (registry → unit plaintext dirs → one gitleaks invocation per dir → merge findings); doctor advisory check + its test (installed/not-installed rows). GREEN.
- [ ] **Step 5:** full suite + lint; commit `feat(cli): agent-brain scan + repo gitleaks CI/hook scanning (ADR 10/14)`.

**Q2 REVIEW GATE** after Task 5: reviewer over Tasks 4–5 diff. Special attention: rotate's fail-closed e2e RED proof is real (revert-verified), scan never joins SafetyGate, `.gitleaks.toml` allowlists are path+rule-scoped with justifications.

### Task 6: `agent-brain dashboard` — bubbletea v2 client over existing seams (spec §7; decision: BUILD in P4)

**The decision this task records:** spec §7 deferred the dashboard out of Phase 3 with "Phase-4 planning decides whether it lands in P4 or v1.1". Decided 2026-07-09: **P4, read-first scope.** Reasoning: every seam it needs shipped in P3 (that was the deferral's premise); the standing directive rejects deferring buildable capability; and the cutover (Tasks 9–10) gets an immediate payoff — a live fleet-health view while the daemon takes over real memories. Scope discipline: the dashboard consumes EXISTING surfaces only (UDS API + the same read-only file loaders `conflicts` and `doctor` already use). If a view seems to need a new daemon endpoint, STOP — that is a scope change to raise, not build.

**Views (spec §7 names them):** Projects · Conflicts · Activity · Doctor.
- **Projects**: table (bubbles/v2 table) — folder, provider, watch state, last-cycle result, degraded flag; keys: `s` = sync now (`client.Sync`), `t` = track/untrack toggle with an inline y/N confirm state (`client.Track`/`Untrack` — the spec §6 codex global pseudo-project toggle is exactly this row's `t`).
- **Conflicts**: list of retained-conflict records via the `conflicts` command's existing loader (read-only file surface).
- **Activity**: status snapshot — uptime, state detail, quiesced-until (Task 2), last SyncSummary (commits/pushed/degraded/scrubbed), watch trigger counts.
- **Doctor**: run the doctor package read-only (`--offline` semantics) and render its `Report` with per-check status glyphs.
- **Daemon down**: full-screen notice offering `s` = start the service (same `service.Controller` path the CLI uses) then re-poll, `q` = quit. This is spec §7's "offering to start the daemon when it is down".

**Mechanics:** root model owns a tab bar + active view; every view refreshes on a shared `tea.Tick(2*time.Second, ...)` poll (tick-based polling is idiomatic bubbletea for a local daemon; no push channel exists and inventing one violates the no-new-seams rule). bubbletea **v2** API (verified from the resolved module 2026-07-09): `Model.Init() Cmd`, `Update(Msg) (Model, Cmd)`, `View() View` — views are built with `tea.NewView(string)`; program via `tea.NewProgram(model)`. Non-TTY → refuse: `dashboard requires an interactive terminal` (tested); there is no accessible-mode dashboard — `status`/`projects --json` are the scriptable equivalents (help text says so).

**Testing strategy (the reason the models stay logic-heavy/pure):** construct models directly, feed typed msgs (`statusMsg`, `projectsMsg`, `tickMsg`, `tea.KeyPressMsg`), assert on rendered strings with styling forced plain (lipgloss profile/NO_COLOR in tests). The API client behind the views is an interface (`dashboardData`) so tests inject a fake; no PTY, no golden files with ANSI.

**Files:**
- Create: `internal/cli/dashboard/dashboard.go` (root model + tabs + tick), `internal/cli/dashboard/projects.go`, `conflicts.go`, `activity.go`, `doctorview.go`, `data.go` (the `dashboardData` interface + api.Client adapter), matching `*_test.go` per view, `internal/cli/dashboard.go` (cobra command: TTY check, client construction, `tea.NewProgram(...).Run()`)
- Modify: `go.mod` (bubbletea/bubbles/lipgloss v2 become DIRECT — record resolved versions in the commit body), `internal/cli/root.go` (register), `docs/decisions/05-adr-cli-tui-stack.md` (amendment: direct bubbletea scope = dashboard package only; huh/fang everywhere else)

**Interfaces:**
- Consumes: `api.Client` (Status/Projects/Sync/Track/Untrack), Task 2's `StatusResponse.QuiescedUntil`, conflicts loader, `doctor` package Report, `service.NewController`.
- Produces: `dashboard` command. No new daemon surface (enforced by the Q3 reviewer).

- [ ] **Step 1 (RED, root model):** tests: tab cycling (`tab`/`1`–`4` switch active view, rendered tab bar marks it), tick triggers a data reload Cmd, `q` quits, daemon-down snapshot renders the start-offer screen. Run → FAIL (package does not exist).
- [ ] **Step 2 (root impl + GREEN).**
- [ ] **Step 3 (RED per view, then GREEN per view):** one commit per view, each: table/list renders the fake snapshot; Projects: `s` emits Sync call on the fake, `t` walks confirm→Track/Untrack; Doctor: check glyphs match Report statuses; Activity: quiesced state renders when set. Follow the strict RED→GREEN cycle per view; commit granularity `feat(dashboard): <view> view`.
- [ ] **Step 4 (command wiring):** TTY refusal test (pipe stdin/stdout → error message); help text cross-references `status --json`/`projects --json`. Commit.
- [ ] **Step 5 (ADR 05 amendment):** record the direct-dependency scope decision + resolved versions + this plan as the trigger; date and sources (module cache verification). Commit `docs(adr): 05 amendment — bubbletea v2 direct for dashboard only`.
- [ ] **Step 6:** full suite foreground + lint. `go build ./...` also cross-compiles linux (dashboard must not import anything darwin-only). Commit.

**Q3 REVIEW GATE** after Task 6: reviewer over the dashboard diff. Special attention: zero new daemon endpoints, boundary greps still clean (dashboard package import allowlist), model purity (no I/O in Update paths except via Cmd), EOF/TTY contracts.

### Task 7: Release pipeline — GoReleaser v2, tag-push workflow, Homebrew tap (ADR 16, spec §13)

Distribution is pure configuration (ADR 16) — but configuration with two sharp edges this task handles explicitly: (1) **the code repo is PRIVATE until the ADR-13 scrub**, and Homebrew fetches cask artifacts anonymously, so tap publishing must not go live against private release assets; (2) **unsigned Go binaries + macOS Gatekeeper** need the documented cask handling. Both resolutions are recorded in the Decision record & sources section.

**Design (locked):**
- `.goreleaser.yaml` version 2: darwin/arm64+amd64 + linux/arm64+amd64, CGO off, `-s -w -X github.com/Sawmonabo/agent-brain/internal/cli.Version={{.Version}}` (root.go:7 is built for exactly this), tar.gz archives, checksums, Conventional-Commits changelog groups.
- `homebrew_casks` (the sanctioned path since v2.16 — `brews` is deprecated) targeting `Sawmonabo/homebrew-tap`, with **`skip_upload: auto`** — prerelease tags (`v2.0.0-rc.*`) publish GitHub release assets but do NOT push a cask, so the cutover RCs never publish a public cask pointing at private assets. The final `v2.0.0` (tagged post-scrub, Task 12, when the repo goes public) is what activates `brew install sawmonabo/tap/agent-brain`.
- Cask includes the GoReleaser-documented quarantine handling for unsigned CLI binaries, verified against the homebrew_casks docs 2026-07-09 — the documented hook, adapted to this binary:

```yaml
    hooks:
      post:
        install: |
          if OS.mac?
            system_command "/usr/bin/xattr", args: ["-dr", "com.apple.quarantine", "#{staged_path}/agent-brain"]
          end
```

  The docs carry an explicit caveat that this bypasses Gatekeeper and Apple may disable the method in a future macOS — acceptable for a single-consumer tool; codesigning/notarization stays out per ADR 16 (checksums + immutable releases; the revisit-if-public note stands, and this caveat is part of what it revisits).
- Workflow `.github/workflows/release.yml`: on `push: tags: ['v*']`, `permissions: contents: write`, SHA-pinned actions (ADR 12), `fetch-depth: 0` (changelog wants history), `go-version-file: go.mod`. Tap push authenticates with `HOMEBREW_TAP_GITHUB_TOKEN` — a fine-grained PAT, contents:write, scoped to the tap repo ONLY (GITHUB_TOKEN cannot push cross-repo). Creating that secret is a user step in the runbook below.
- Interim install path while the repo is private (Tasks 9–10 use it): `gh release download <tag> -p '<pattern>'` (authenticated) or `go install` (owner has git access). Documented in Task 8's onboarding doc as the pre-public path.

**Files:**
- Create: `.goreleaser.yaml`, `.github/workflows/release.yml`
- Modify: `docs/decisions/16-adr-distribution-release.md` (amendment: skip_upload:auto rationale, private-assets finding, quarantine handling — with sources). Dependabot needs NO change — the existing `github-actions` ecosystem entry at `directory: /` covers every workflow file (verified 2026-07-09); the task verifies this holds rather than editing it.
- Test: `goreleaser check` + `goreleaser release --snapshot --clean` locally (brew-installed GoReleaser, record its version) + artifact inspection

**Interfaces:**
- Consumes: `cli.Version` ldflags seam; Conventional Commits history.
- Produces: tag-triggered releases; the tap cask (post-public). Tasks 9/10/12 consume the release artifacts.

- [ ] **Step 1: Write `.goreleaser.yaml`** — full file, version 2 schema (builds/archives/checksum/changelog/homebrew_casks exactly as designed above; cask `name: agent-brain`, homepage + description from the repo, `repository: {owner: Sawmonabo, name: homebrew-tap, token: "{{ .Env.HOMEBREW_TAP_GITHUB_TOKEN }}"}`).
- [ ] **Step 2: Validate + snapshot** — `goreleaser check` → 0 issues; `goreleaser release --snapshot --clean` → builds all four targets; then INSPECT: `tar -tzf dist/*darwin_arm64*.tar.gz` lists the binary + license/readme only (no testdata, no keyset material — release/secrets hygiene constraint); `dist/agent-brain_*_darwin_arm64/agent-brain --version` (or the extracted binary) prints the snapshot version (ldflags proof).
- [ ] **Step 3: Write `.github/workflows/release.yml`** — as designed; SHA-pin every action at its current release (record tag→SHA mapping in the commit body); extend dependabot to the new workflow file's ecosystem if not already covered by the directory glob.
- [ ] **Step 4: Tap repo + secret (USER-GATED — outward-facing):** present, verbatim, and wait for go-ahead: `gh repo create Sawmonabo/homebrew-tap --public --description "Homebrew tap for agent-brain"` (a tap must be public to be `brew tap`-able anonymously; it will hold only generated casks — empty until the first post-public release) and the PAT creation steps (GitHub → Settings → Developer settings → fine-grained PAT, repository access = homebrew-tap only, permissions = Contents: Read and write; then `gh secret set HOMEBREW_TAP_GITHUB_TOKEN --repo Sawmonabo/agent-brain`).
- [ ] **Step 5: Commit** — `git add .goreleaser.yaml .github/ && git commit -m "feat(release): GoReleaser v2 pipeline — casks with skip_upload:auto, SHA-pinned tag workflow"`. (The pipeline's live proof is Task 9's rc tag — a deliberate runbook step, not a test-time side effect.)

### Task 8: Documentation — README v2, onboarding + WSL2 runbook, spec §13/§10 truth pass

The last docs-truth pass before code freeze (Phase-3's Task 13 precedent: surgical, truthful, code-is-truth).

**Files:**
- Create: `docs/onboarding.md`
- Modify: `README.md` (full v2 rewrite — the current one describes whatever survived the greenfield reset), `docs/00-design-spec.md` (§7 dashboard "deferred" note → as-built; §13 interim-private install path; Appendix pin refresh with resolved go.mod versions), `CLAUDE.md` (commands: dashboard/scan/`key rotate`/quiesce-aware notes; keep branch discipline until Task 11 lands, then Task 13 retires it)

**Content requirements:**
- README: what it is (one paragraph), install (brew post-public; `gh release download` + `go install` while private — dated note), quickstart (`init` → `track` → done), command tour (for the dashboard, a fenced text block of the rendered TUI — no image assets), security model summary (ciphertext-on-wire, fail-closed, keyset never in any repo, threat model pointer to spec §5), uninstall.
- `docs/onboarding.md`: the §13 under-5-minute new-machine runbook, expanded per-OS — macOS (brew/gh-download), Linux (tarball/go install, systemd user unit), **WSL2** (Linux binary; `systemctl --user` requires systemd=true in /etc/wsl.conf — include the check `systemctl --user is-system-running` and the `loginctl enable-linger $USER` step so the daemon outlives the last session; both verified live in Task 10 on the real WSL machine and corrected here if reality disagrees).
- Truth rule: every claimed command/flag exists in `--help` output at the current tip (grep-verify, the Phase-3 T13 method); no future tense for shipped work.

- [ ] **Step 1:** write all three docs; run the truth greps (`go run ./cmd/agent-brain --help` tree vs claims).
- [ ] **Step 2:** commit `docs: README v2, onboarding/WSL2 runbook, spec §13 as-built truth pass`.

**Q4 REVIEW GATE** after Task 8: reviewer over Tasks 7–8 diff (config + docs truth: every pinned SHA resolvable, snapshot artifact contents as claimed, docs claims vs `--help` reality).

---

## FINAL WHOLE-BRANCH REVIEW GATE (code freeze — before any cutover step)

Dispatch the final reviewer over the whole Phase-4 range (derive base from develop's log: the commit before Task 1's first commit; NEVER remembered worktree SHAs). Then run every automated exit criterion (below) at the tip and stamp the ledger. **No rc tag until this gate closes.** Findings: fix-now per the standing directive; accept-document only what is genuinely out of code's reach, in the ledger, with reasoning.

---

### Task 9: RC release + THIS-Mac production cutover (USER-GATED RUNBOOK)

**Gate:** final review closed, develop pushed, and the user says go — this creates a real release, enrolls real projects, and (at its end) deletes bash-era files. Present each block, get the go-ahead, run, verify, then proceed.

**Preconditions checklist:**
- [ ] Final gate + all automated exit criteria stamped at the tip.
- [ ] Keyset backup confirmed IN the password manager (the armored export printed 2026-07-09 during the smoke wrap-up; re-print anytime: `agent-brain key export`). The production cutover uses THIS keyset (decided 2026-07-09).
- [ ] Legacy preflight still clean: `chezmoi --config ~/.config/agent-brain/chezmoi.toml diff` → empty (orphan adjudication was executed 2026-07-08: 30 orphans → 28 forgotten, 2 restored; a non-empty diff here means new bash-era writes happened since — re-adjudicate before migrate).

**Runbook:**
- [ ] **9.1 Tag the RC:** `git tag v2.0.0-rc.1 && git push origin v2.0.0-rc.1` → watch the release workflow (`gh run watch`); assert four archives + checksums on the release page; assert NO cask was pushed to the tap (skip_upload:auto on prerelease).
- [ ] **9.2 Install from the release artifact** (exercises the pipeline; the repo is private so use authenticated download):
```bash
mkdir -p ~/.local/bin
gh release download v2.0.0-rc.1 -R Sawmonabo/agent-brain -p '*darwin_arm64*' -O - | tar -xz -C ~/.local/bin agent-brain
agent-brain --version   # must print v2.0.0-rc.1 — the ldflags proof, live
```
- [ ] **9.3 `agent-brain init`** (interactive, real gh): expects the existing keyset → keep-flow; creates/clones `agent-brain-memories` (the REAL repo this time); wiring; service install; **enrollment picker: enroll the real projects deliberately** (ai-sidekicks, agent-brain, dotfiles, …). Live claude sessions are safe by design (smoke-verified: mirror-out snapshot gate + manifest-gated deletions) — but enroll during a quiet moment anyway; the first mirror-in snapshots current plaintext.
- [ ] **9.4 `agent-brain migrate`:** preflight runs automatically (30s default; now tunable per Task 3); the picker maps `~/.agent-brain/<slug>/` dirs → projects (GuessPath + session-cwd assists from `e43669f`); confirm each mapping. Verify the layering afterwards: `git -C "$(agent-brain status --json | jq -r .checkout)" log --oneline -20` shows seed commits + overlay commits per §10 (adjust the checkout-path extraction to the actual JSON shape — read `status --json` first).
- [ ] **9.5 Verify:** `agent-brain doctor` → all ok + the legacy-leftovers warn (expected until 9.6); wire spot-check via `gh api` on 2–3 blobs incl. a derived MEMORY.md → `agb1\x00` prefix, no plaintext (the smoke's method — remember to QUOTE URLs with `?` against zsh globbing); `agent-brain dashboard` → projects healthy, activity sane; write a real memory in a real session → watch the cycle land it (dashboard Activity or `service logs`).
- [ ] **9.6 Retire bash on this Mac** (spec §10 checklist, verbatim — ONLY after 9.5 is green): remove the SessionStart healthcheck hook; delete `~/.local/bin/ab-claude` + the healthcheck script; strip `autoMemoryDirectory` from per-project `.claude/settings.local.json` files; remove `~/.config/agent-brain/chezmoi.toml`; delete `~/.agent-brain/`. **The age key (`key.txt`) STAYS until Task 12.**
- [ ] **9.7 Re-verify:** `agent-brain doctor` → legacy-leftovers warn GONE, everything ok. Soak: leave the daemon running through normal work for at least a day before Task 10; note anomalies in the ledger.

### Task 10: Fleet cutover — every other machine (USER-GATED RUNBOOK, per machine)

**USER INPUT REQUIRED at execution:** the machine inventory. The dotfiles repo targets macOS + Linux/Ubuntu + WSL2 — enumerate the real hosts with the user and fill the table in the ledger:

| Host | OS | bash-era state? (`~/.agent-brain` exists) | cutover date | verified |
|---|---|---|---|---|
| Sawmons-MacBook-Pro | macOS | yes → migrated in Task 9 | (Task 9) | |
| _user fills_ | | | | |

Per machine (order: install → key → init → migrate → verify → retire):
- [ ] **10.1 Install:** macOS/Linux: `gh release download v2.0.0-rc.1 -p '*<os>_<arch>*' ...` (as 9.2) or `go install github.com/Sawmonabo/agent-brain/cmd/agent-brain@v2.0.0-rc.1` (owner git access; GOPRIVATE if needed). WSL2: the Linux binary; first check `systemctl --user is-system-running` (needs `systemd=true` in `/etc/wsl.conf` + restart), then `loginctl enable-linger $USER` so the daemon survives session close — correct `docs/onboarding.md` here if live WSL2 reality differs from Task 8's text.
- [ ] **10.2 Keyset:** `agent-brain key import` — paste the armored export from the password manager. NEVER transfer the keyset file through any repo.
- [ ] **10.3 `agent-brain init`:** join-flow (repo exists → clone); service install; enrollment picker for THIS machine's projects.
- [ ] **10.4 `agent-brain migrate`** if the table says bash-era state exists (preflight per machine: the chezmoi orphan adjudication from spec §10 has only been executed on Sawmons-MacBook-Pro — other machines run their own preflight and adjudication BEFORE migrate).
- [ ] **10.5 Two-machine proof (the v2 point):** write a memory in an enrolled project here → watch it arrive on the Mac (and vice versa); `doctor` ok; dashboard healthy on both.
- [ ] **10.6 Retire bash** (same §10 checklist; age key stays). Fill the table row; per-machine ledger note.
- [ ] **10.7 WSL2 idle-posture decision (only on the WSL2 host):** spec §8's tree comment promises daemon "idle-exit (WSL2)" and NO phase shipped it — a resident daemon holds the WSL utility VM alive (RAM/battery on the Windows host). Measure it live here (`wsl --list --running` from Windows; VM memory with the daemon resident vs stopped). Decide with the user: (a) resident-with-linger is acceptable → fix spec §8's comment in Task 13 (truth) + backlog entry carrying the measurement; (b) it is not → implement idle-exit as a scoped follow-up task ON THIS PLAN (WSL2-only via the existing `service.IsWSL2()`; exit after N idle minutes; systemd restart semantics + the missed-events trade documented) before calling Task 10 done. Either way the decision + numbers land in the ledger and the Decision record.

### Task 11: develop → main merge (USER-GATED — ADR 11's gate)

**Preconditions (ALL):** Tasks 9–10 verified on every inventoried machine; soak clean; ledger's cutover table complete. ADR 11: "develop merges into main only when v2 demonstrably works end-to-end" — the cutover IS that demonstration.

- [ ] **11.1** `git checkout main && git pull && git merge --no-ff develop` — merge commit (history with its 60+ ADR-linked commits is the audit trail; never squash). Message: summarize v2 + point at the spec/plans.
- [ ] **11.2** Full suite foreground on main + lint + linux cross-compile (same commands as exit criteria C1–C3).
- [ ] **11.3** `git push origin main`. Keep `develop` (house integration branch). **No final release tag yet** — v2.0.0 is tagged AFTER the scrub (Task 12), because delete-and-recreate destroys releases/tags on the old repo instance; rc tags are accepted casualties.
- [ ] **11.4** CI green on main; ledger entry.

### Task 12: ADR-13 history scrub + age-key retirement + v2.0.0 (USER-GATED, DESTRUCTIVE — the point of no return)

**Preconditions (ALL, re-verified in-session):** main merged (11); migrate verified on EVERY machine (10.5 rows all checked — the scrub destroys the last copy of anything unadjudicated); **local mirror archive exists**: `git clone --mirror git@github.com:Sawmonabo/agent-brain.git ~/archives/agent-brain-pre-scrub-$(date +%Y%m%d).git` on an external/backed-up location — kept through the first post-scrub weeks (ADR 13 gate c). The memories repo (`agent-brain-memories`) is UNTOUCHED by all of this.

- [ ] **12.1 Fresh clone + filter:** `git clone git@github.com:Sawmonabo/agent-brain.git /tmp/scrub && cd /tmp/scrub && git filter-repo --sensitive-data-removal --invert-paths --path home/dot_agent-brain` (git-filter-repo v2.47.0 via brew/pipx; it refuses non-fresh clones by design). Blob removal empties the bash-era `memory: <host> <timestamp>` commits and filter-repo prunes them — hostname/timing metadata goes too.
- [ ] **12.2 Verify BEFORE any push (ADR 13):** `gitleaks git --no-banner --log-opts=--all .` over the scrubbed clone → clean (**`--log-opts=--all` is load-bearing**: a bare scan covers only the current branch's log — verified 2026-07-09 when develop-only scanning came back clean while main's bash-era `.age` blobs sat unscanned on another ref); `git log --all --oneline -- home/dot_agent-brain` → EMPTY; `git log --all --format=%s | grep -c '^memory:'` → 0; spot-inspect `git log --all --stat | head -100`; size sanity (`git count-objects -vH` shrunk vs the archive).
- [ ] **12.3 Delete-and-recreate (the chosen finish — GitHub retains cached views + unreachable objects past force-pushes):** `gh repo delete Sawmonabo/agent-brain --yes` (the `delete_repo` scope was added 2026-07-09) → `gh repo create Sawmonabo/agent-brain --private --description ...` (**or `--public`** — the zero-cost option is live the moment the scrub verifies; USER DECIDES here, recorded in the ledger) → from the scrubbed clone: `git remote add origin git@github.com:Sawmonabo/agent-brain.git && git push origin --all && git push origin --tags`. Re-point/re-clone every machine's working copy (`git remote set-url` is NOT enough — SHAs changed; fresh clone, carry over uncommitted work manually if any).
- [ ] **12.4 Age key retirement (everywhere):** after 12.2's verification, nothing the age key guards exists anymore — delete `~/.config/agent-brain/key.txt` on every machine and retire the password-manager entry (archive-tag it rather than hard-delete for the first weeks, matching the archive-retention posture). Ledger the completion; this closes the bash era.
- [ ] **12.5 v2.0.0 final:** `git tag v2.0.0 && git push origin v2.0.0` from main on the NEW repo instance → release workflow → four archives + checksums; if 12.3 chose public, the cask publishes and `brew install sawmonabo/tap/agent-brain` goes live — verify on a machine (`brew install` + `agent-brain --version`); if private, the gh-download path remains and the cask activates whenever the repo flips public (re-run the tag's workflow or cut v2.0.1).

### Task 13: Epilogue — decision records, backlog, close-out (the last v2 task)

**Branch note:** Task 13 runs on the POST-SCRUB repo instance. `develop` remains the integration branch: recreate it from the new `main` (`git switch -c develop && git push -u origin develop` on the fresh clone), land these commits there, and finish with a `--no-ff` merge to main — the same discipline, new history.

- [ ] **13.1 ADR execution records:** amend ADR 13 (scrub executed: date, verification outputs, public/private decision), ADR 16 (as-executed: skip_upload:auto, private-assets finding, quarantine handling, workflow SHAs), and ADR 14 (use-(1) memory-content scan landed as on-demand `agent-brain scan` + doctor advisory, not commit-time — Task 5's recorded reasoning) — each with sources per the persist-research-links practice. ADR 08 gains the device-flow VERDICT paragraph (see Decision record below: build vs record-as-post-v2, with the reasoning that gh remains required for provisioning either way).
- [ ] **13.2 `docs/post-v2-backlog.md`:** every surviving idea, each with WHY it is post-v2 (not a bare list): gh device-flow auth fallback (capability analysis), key destroy/disable lifecycle (needs fleet-wide re-encrypt coordination + history rewrite of the MEMORIES repo — out of v2's blast radius), cosign/provenance if public adoption appears, Gemini CLI adapter (ADR 02's revisit trigger), dashboard push-refresh (daemon event stream — new seam), `main`-branch protection rules.
- [ ] **13.3 Spec + CLAUDE.md close-out:** spec front-matter notes v2 SHIPPED (date, v2.0.0); CLAUDE.md drops the ADR-11 "never commit to main" branch-discipline block (obsolete post-merge), documents the current branch model, adds dashboard/scan/rotate to the command list.
- [ ] **13.4 Ledger + memory:** final progress.md entry (cutover table, scrub record, release links); update the session-memory go-rebuild file to "v2 SHIPPED".

---

## Exit criteria (Phase 4 done means ALL of these)

Automated, at the final code tip (stamped in the ledger before Task 9; re-run anything invalidated by later doc commits):

1. `(ulimit -u 1400; go test ./... -race -count=1)` foreground, uncached — EXIT 0, zero FAIL lines (grep the log with `-a`; race logs can carry NUL bytes that make BSD grep go silent).
2. `golangci-lint run` — 0 issues.
3. `GOOS=linux GOARCH=amd64 go build ./...` — OK (dashboard included).
4. `govulncheck ./...` — clean at symbol AND module level.
5. `go test ./test/e2e/ -race -v` — all testscript flows + the FULL adversarial corpus (11 rows + any Phase-4 appends) + the rotate wire proof.
6. Boundary greps EMPTY: the four Phase-3 greps PLUS `grep -rn 'charm.land/bubbletea\|charm.land/bubbles\|charm.land/lipgloss' internal/ --include='*.go' | grep -v 'internal/cli/dashboard' | grep -v _test` (dashboard-only rule) and the ADR-08 tripwires in comment-excluding form.
7. `goreleaser check` — 0 issues; `goreleaser release --snapshot --clean` — four targets build; archive inspection shows binary+docs only; snapshot binary prints its version.
8. `gitleaks git --no-banner` (full history) and `gitleaks git --staged --no-banner` — clean under `.gitleaks.toml` whose every allowlist entry is path+rule-scoped and justified.
9. Zero TODO/FIXME/XXX/HACK in non-test source; zero user-facing "Phase 3/4" strings.

Human (user-gated, sequential):

10. v2.0.0-rc.1 released by the tag workflow; installed from the artifact on this Mac; real cutover + migrate + bash retirement verified here (Task 9 checklist all green).
11. Fleet table complete: every inventoried machine cut over, two-machine sync observed both directions, bash retired everywhere (Task 10).
12. develop merged to main --no-ff; suite green on main (Task 11).
13. History scrub executed + verified (gitleaks-clean, zero bash-era paths/commits); delete-and-recreate done; every clone refreshed; age key retired everywhere (Task 12).
14. v2.0.0 final released from the new repo instance; install path proven (brew if public, gh-download if private) (Task 12.5).
15. Epilogue docs landed: ADR execution records, post-v2 backlog with reasons, spec/CLAUDE.md close-out (Task 13).

## Decision record & sources (Phase-4 planning, 2026-07-09)

Decisions made while writing this plan, each with its trail (persist-research practice — links live here and in the ADR amendments they feed):

1. **Dashboard: BUILD in P4, read-first scope, zero new daemon endpoints** (Task 6). Trigger: spec §7's parked decision + the standing no-deferral directive + P3 shipped every seam. Rejected: v1.1 deferral (no concrete trigger would ever force it), building push-refresh seams now (new daemon surface during a cutover phase — post-v2 backlog). API shapes verified from the RESOLVED modules on disk 2026-07-09 (`go doc charm.land/bubbletea/v2.Model|View|NewProgram|Tick`, `charm.land/bubbles/v2/table`): v2 `View() tea.View` via `tea.NewView(string)` — a v1→v2 breaking change the task text pins so implementers don't write v1 idioms.
2. **`key rotate` pulled forward from v1.1** (Task 4): keyset format already multi-key (spec §5); rotation without tooling is not a capability. Old keys retained forever in v2 (history smudge + fleet lag); destroy/disable lifecycle recorded as post-v2 with reasons (Task 13.2).
3. **Re-encrypt is a daemon admin op** (`/v0/reencrypt`), not CLI-side: single-writer invariant (ADR 03) is an API-shape constraint since P3 — a CLI-side renormalize would be the first regression of it.
4. **Quiesce is the general primitive** for F2 (Task 2) — rejected: dropping init's heal-push (loses a real repair), advisory retry-loops (leaves the race). TTL-bounded with auto-release so a crashed CLI can't wedge the daemon.
5. **gitleaks: on-demand `scan` + CI/hooks; NO per-cycle daemon scanning** (Task 5) — wire exposure is ciphertext regardless; per-save subprocess latency + false-positive fatigue buys nothing. Recorded as a decision, not a deferral.
6. **Final v2.0.0 is tagged AFTER the scrub** (Tasks 11/12): ADR 13's delete-and-recreate destroys the old repo instance's releases/tags — discovered while sequencing; rc tags are deliberate casualties. GoReleaser `skip_upload: auto` keeps prerelease casks out of the public tap while release assets sit on a private repo.
7. **Interim distribution while private:** `gh release download` (authed) / `go install` for the cutover; `brew` activates when the repo goes public post-scrub. The tap repo itself is created public + empty in Task 7 (taps must be public to tap anonymously).
8. **Device-flow gh fallback (ADR 08): analysis, not build** — gh remains REQUIRED for provisioning/clone/credential-helper regardless (ADR 08's core), so device-flow auth alone removes no dependency and adds an auth path to maintain. The verdict is version-independent of the cli/oauth library's state; Task 13.1 fetches the library's current state when writing the ADR 08 amendment and records it there with sources.
9. **Cutover uses the smoke-proven keyset** generated 2026-07-09 (armored export re-printed at the smoke wrap-up; password-manager storage is a Task 9 precondition).
10. **Homebrew-from-private rejected concretely** (Task 7): the homebrew_casks docs DO document a private-repo path (a custom `GitHubPrivateRepositoryReleaseDownloadStrategy` cask class + every installing machine exporting `HOMEBREW_GITHUB_API_TOKEN`) — rejected because it trades a two-command interim (`gh release download`) for token distribution + a nonstandard cask on every machine; `skip_upload: auto` keeps the tap clean until the repo is public.
11. **CI secret-scanning via pinned binary, not gitleaks-action** (Task 5): the action (v3.0.0) is license-free for personal accounts, but the binary keeps hook/CI behavior byte-identical and adds no third-party action surface.

Sources (primary, fetched/verified 2026-07-09 unless noted):
- GoReleaser homebrew_casks docs — skip_upload "auto" prerelease semantics (quoted in Task 7), the unsigned-binary xattr hook + Apple caveat, repository/token shape, private-repo download strategy: https://goreleaser.com/customization/homebrew_casks/
- gitleaks repo — v8.30.1 current (2026-03-21), allowlist TOML schema (`[[allowlists]]` / `[[rules.allowlists]]`): https://github.com/gitleaks/gitleaks · staged-scan flag semantics from the LOCAL binary's own help (`gitleaks git --help`, brew gitleaks 8.30.1): "--staged: scan staged commits (good for pre-commit)" · live scans this session: staged clean, develop full history clean (226 commits, 6.4 MB, 0 leaks)
- gitleaks-action — v3.0.0 (2026-05-30), license key required for org accounts only (README quoted in Task 5): https://github.com/gitleaks/gitleaks-action
- charm.land v2 TUI API — verified from the RESOLVED modules on disk via `go doc` (bubbletea v2.0.2, bubbles v2.0.0, lipgloss v2.0.1 per go.mod): `Model.Init() Cmd`, `View() tea.View` via `tea.NewView`, `tea.Tick`, `bubbles/v2/table.New(opts...)`
- Spec Appendix pins verified 2026-07-09 (Phase-3 T13: GoReleaser v2.16.0, git-filter-repo v2.47.0, golangci-lint v2.12.2, gh flag surface at v2.96.0); ADR 13/16 search trails (2026-07-07): https://github.com/goreleaser/goreleaser/releases · https://goreleaser.com/blog/goreleaser-v2.16/ · https://github.com/newren/git-filter-repo · https://git-scm.com/docs/git-filter-branch (deprecation pointer)
- Live machine verifications this session: gh 2.92.0 token scopes + `delete_repo` device-flow refresh; huh v2.0.3 accessible-EOF keeps-prefills (smoke, live-reproduced); real Claude Code v2.1.205 slug probes (`e43669f`); `gitx.Result` fields (gitx.go:24); dependabot `github-actions@/` covers new workflows.

## Review-gate rhythm (Phase-3 precedent)

- Q1 after Task 3 (hardening cluster) · Q2 after Task 5 (rotate + gitleaks) · Q3 after Task 6 (dashboard) · Q4 after Task 8 (release config + docs) · FINAL whole-branch before Task 9. Reviewers get: this plan, the diff range derived from develop's log, the ledger triage list. Every RED proof is revert-verified. Runbook Tasks 9–12 are reviewed as executed (ledger entries with command outputs), not as diffs.
