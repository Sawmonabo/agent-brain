# agent-brain v2 — Phase 5: Dashboard Hub — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Every task is ordinary code work on `develop` — no user-gated runbooks in this phase; the release cut that ships it (rc.7+) is user-gated as always.

**Goal:** Turn the read-mostly dashboard into the hub `docs/01-dashboard-hub-spec.md` specifies: bare `agent-brain` opens it (TTY- and agent-gated announced auto-init), every project's memories are browsable/readable/editable with automatic capture+push, per-memory history with diff/restore rides two new read-only daemon endpoints, plus global search, wiki-links, lint, insights, conflict center, one-key update/doctor/scan, and full enrollment parity.

**Architecture:** ADR 20's three trust-domain rules govern everything: (1) the hub reads **provider dirs** directly and writes them only via write-temp+rename — the watcher/engine captures, the CLI/TUI never touches the memories checkout (ADR 03 intact); (2) the **memories checkout** is reached only through two new read-only daemon endpoints (`GET /v0/history`, `GET /v0/blob`) served on the single engine goroutine; (3) editing is a hardened `$EDITOR` handoff on a scratch copy (`tea.ExecProcess`) — no embedded editor. `internal/cli/dashboard` splits into focused subpackages and remains the only TUI-importing tree.

**Tech Stack:** Go 1.26 (toolchain go1.26.5) · existing Phase 1–4 packages · charm.land/{bubbletea,bubbles,lipgloss}/v2 (present) · **charm.land/glamour/v2 v2.0.1** (markdown render — Task 11) · **mvdan.cc/sh/v3 v3.13.1** (`shell.Fields` POSIX word-split — Task 10) · **github.com/aymanbagabas/go-udiff v0.4.1** (unified diff — Task 14) · **github.com/catppuccin/go v0.2.0** (promoted indirect→direct — Task 4). Versions resolved against the module proxy 2026-07-12; re-confirm with `go list -m` at implementation and record the resolved version in the commit body.

**Dependency verification result (2026-07-12, supersedes ADR 20's buy line for one item):** `charm.land/x/editor` is **dropped**. v0.2.0 (newest) declares module path `github.com/charmbracelet/x/editor` (the x/ repos never adopted the charm.land vanity prefix), and its resolution contradicts ADR 20 decision 2's own hardening: it consults `$EDITOR` only (never `$VISUAL`), splits the command with `strings.Fields` (not POSIX-aware), and silently falls back to `nano`. Task 10 builds resolution in `editorx` (~40 lines) exactly to the ADR's rules; Task 22 records this correction in ADR 20 and spec §5.

## Global Constraints

Every task implicitly includes these.

- Branch: ALL work lands on `develop`. Never commit to `main` (ADR 11).
- Module `github.com/Sawmonabo/agent-brain`; `go 1.26`; `toolchain go1.26.5`.
- Package boundaries (spec §8) unchanged and grep-enforced: `engine` imports `gitx`/`provider`/`repo` (+ stdlib/renameio) ONLY; `daemon/api` imports nothing internal; `doctor` never imports `daemon`/`cli`. The `internal/cli/dashboard` tree (root + new subpackages `views`, `theme`, `actions`, `memoryfs`, `links`, `lint`, `search`, `editorx`) may import: `charm.land/bubbletea/v2`, `charm.land/bubbles/v2`, `charm.land/lipgloss/v2`, `charm.land/glamour/v2`, `github.com/catppuccin/go`, `github.com/aymanbagabas/go-udiff`, `mvdan.cc/sh/v3/shell`, `github.com/google/renameio/v2`, `internal/daemon/api`, `internal/config`, `internal/doctor`, `internal/provider`, `internal/repo`, `internal/service` — and stays the ONLY tree outside `cli` root allowed direct bubbletea/lipgloss imports. Everything else keeps huh/fang.
- **Single-writer invariant (ADR 03):** the CLI/TUI process never writes inside the memories checkout. Hub mutations (edit/new/rename/delete/restore) are provider-dir writes via write-temp-in-same-dir + rename (`renameio/v2`), captured by the daemon. The two new endpoints are READ-ONLY and run on the engine goroutine. No new mutating daemon endpoints (spec §16).
- **Scratch rule (ADR 20 D2):** editors only ever get a disposable copy under `os.UserCacheDir()` — never a live provider file, never anything inside a watched tree.
- Safety invariants (spec §5/§11): keyset never enters any repo; plaintext memory content never reaches a git object; `/v0/blob` serves DECRYPTED content over the UDS socket only (peer-UID enforced, ADR 09) — it must never be logged.
- Tests: stdlib `testing` + `go-cmp` ONLY (ADR 15); table-driven; `t.Parallel()`; `t.TempDir()`; real system git with `git init --bare` fake remotes; no network ever; paths via `AGENT_BRAIN_CONFIG_DIR`/`AGENT_BRAIN_DATA_DIR`/`AGENT_BRAIN_RUNTIME_DIR`. TUI renders asserted through the existing CSI-strip helpers (`plain` in dashboard tests, `stripANSI` in cli tests).
- **Fork-bomb rule (standing, incident 2026-07-08):** never point git filter/merge wiring at a test binary; build the real binary once in `TestMain` (pattern: `test/e2e/harness_test.go`). Full suites FOREGROUND with `(ulimit -u 1400; go test ./... -race -count=1)` — never as a background job.
- gofumpt-clean; golangci-lint clean; zero new `//nolint`; `go tool deadcode -test ./...` stays at zero findings. Conventional Commits, one commit per task minimum; bodies explain mechanism + user-visible behavior; never cite PR numbers/reviewers/dates in code comments or test names. lefthook pre-push runs the race suite — budget for it.
- The hub never prompts or opens the TUI without a real TTY (spec §1/§15); huh forms keep the `cancellableKeyMap()`/`titleWithCancelHint(...)` cancel contract (form.go).

---

### Orientation: existing surfaces this phase consumes

Import these — never re-implement. Exact signatures verified 2026-07-12 at `develop` = `7f77073`.

- `internal/engine` — `Engine{checkout, host, layout repo.Layout, registry, now}`; `New(checkout, host, registry, now)`; commit subjects `memory: <host> <folder> <timestamp>` (commit.go:28, RFC 3339 UTC stamp) and `memory: <host> manifest <timestamp>`; `gitx.Run(ctx, dir, args...) (Result, error)` / `gitx.RunStatus` (exit code as data).
- `internal/daemon` — engine-goroutine `loop` with `syncRequests`/`adminRequests` channel arms; `submitAdmin(ctx, reason, run)` (quiesce-refusing, 60s bounded, post-op cycle); `refreshState` (SafetyGate → "ready"/"uninitialized"); `statusError{code, msg}` for non-500 envelopes; `controller` interface in server.go wired by `newServer(ctrl, peerUID)`.
- `internal/daemon/api` — `Client.do(ctx, method, path, in, out)`; add methods beside `Status`/`Sync`/`Projects`/`Track`/`Untrack`/`Migrate`/`Reencrypt`/`Quiesce`/`Resume`. Client body reads are capped at 1 MiB (`client.go:144`) — response payloads must stay under it.
- `internal/config` — `Paths{ConfigDir, DataDir}` + `SettingsFile()`/`MemoriesDir()`/`LocalRegistryFile()`/`ConflictLogFile()`; `Settings` strict TOML (unknown key = error) with `validate()` floors; `ConflictRecord{Time, Path, Mode}` + `ReadConflictLog`.
- `internal/repo` — `Unit{Provider, ProjectID, Folder, LocalDir, RepoSubdir}`; `LoadLocalRegistry`; `Layout.UnitDir(folder, provider)`; `MetaDirName = ".agent-brain"`, `GlobalFolder = "_global"`; `ValidateFolderName` (names.go:34), `ValidateRelPath` (manifest.go:125).
- `internal/provider` — `Registry.Get(name) (Provider, bool)`; `Provider.Scope()/Patterns()/Identify(...)`; `Class` (fact / derived-index / regenerated / ignore) + `MatchClass` semantics in match.go; claude adapter: per-project `~/.claude/projects/<slug>/memory/`, `MEMORY.md` = derived-index, frontmatter conventions in `claude/reconcile.go`; codex: global scope, `memories/**` tables.
- `internal/doctor` — `Run(ctx, Deps) Report`, `Fix(ctx, Deps) (Report, error)`, `Report.Failed()`, `CheckResult{Name, Status, Detail, Fix, Fixed}`; `SafetyGate(ctx, paths, registry, binaryPath) error` (gate.go:27).
- `internal/selfupdate` — `Updater{Source, Getenv}`, `Options{Repo, CurrentVersion, TargetPath, RequestedVersion, GOOS, GOARCH}`, `Check → Decision{Latest, UpdateNeeded, Downgrade}`, `Apply(ctx, opts, targetTag)`; sentinels `ErrDevBuild`, `ErrBrewManaged`, `ErrNoRelease`.
- `internal/cli` — `Root()` (root.go — the bare command changes here); init: `runInit(ctx, state, accessible)` treats a form cancel as a clean stop (`runInitSteps` swallows `errInitCancelled` and returns nil — the hub must re-probe, Task 3); `isAccessible()` (enroll.go:385: `ACCESSIBLE` env or non-TTY stdin); `isInteractiveTTY(cmd)` (dashboard.go:108); `newAPIClient`/`tryAPIClient`/`resumeQuietly`/`quiesceHoldForInit = 120` (client_commands.go); `buildDoctorDeps(offline, binaryPath)`; `offlineDoctorRunner()`; `dashboardDiscover()`/`dashboardIdentify()` (dashboard.go); update: `updateEngine` seam, `runUpdate`, `restartServiceForUpdate`, `productRepo = "Sawmonabo/agent-brain"`, `resolveBinary()`; scan: `gitleaksExecRunner`, `scanUnits(ctx, runner, units, redact)`, `filterUnitsByFolder`; migrate: `runMigrate(ctx, deps, client, callbacks, out)`, `legacyRoot(home)`, `enumerateLegacySlugs`, `runMigratePreflight`; form.go: `cancellableKeyMap()`, `titleWithCancelHint`, `formCancelled`.
- `internal/cli/dashboard` (pre-split) — `Model`/`Config{Data, StartService, Discover, Identify}`/`New`; `dashboardData` seam + `apiData`; per-view files projects/conflicts/activity/doctorview + keymap + track (add-flow modal machine); tests: `plain()` CSI-strip, `key(name)` KeyPressMsg builder, `fakeData`.
- bubbletea v2 specifics: `tea.ExecProcess(cmd *exec.Cmd, fn func(error) tea.Msg) tea.Cmd` (exec.go:50); `tea.RequestBackgroundColor` Cmd → `tea.BackgroundColorMsg` with `.IsDark()` (color.go); `program.Run() (tea.Model, error)` returns the final model (Task 18's re-exec reads it).
- `test/e2e` — `harness_test.go` TestMain builds the real binary once; testscript flows in `test/e2e/scripts/*.txt` (`exec agent-brain …`, `stdout 'regex'`, `! stdout`, env via `env K=V`); fake gh shim; ciphertext discriminator `agb1\x00`.

### File structure (locked by this plan)

```
internal/cli/dashboard/            root: Model reducer, Cmds/messages, Config, data seam, toasts, overlays wiring
internal/cli/dashboard/theme/      Catppuccin palette + shared lipgloss styles (Task 4)
internal/cli/dashboard/views/      every tab + drill-in screen model (Task 4 moves the four tabs; later tasks add screens)
internal/cli/dashboard/actions/    single-source action registry driving keymap/palette/help (Task 5)
internal/cli/dashboard/memoryfs/   provider-dir memory enumeration, frontmatter meta, atomic write/delete/rename (Task 6)
internal/cli/dashboard/links/      [[wiki-link]] parse/resolve/backlinks (Task 7)
internal/cli/dashboard/lint/       advisory memory lint rules (Task 8)
internal/cli/dashboard/search/     tiered cross-project search (Task 9)
internal/cli/dashboard/editorx/    editor resolution, scratch lifecycle, change detection (Task 10)
internal/engine/history.go         read-only history/blob git plumbing (Task 1)
internal/daemon/…                  read funnel + two GET routes (Task 2)
internal/cli/hub.go                bare-command entry: gate → announce → init → probe → launch (Task 3)
test/e2e/scripts/bare_command.txt  entry-contract testscript (Task 3)
test/e2e/hub_semantics_test.go     capture/restore/history acceptance battery (Task 21)
```

---

### Task 1: Engine — read-only `History` and `BlobAt` over the checkout

The data source for spec §6. Pure git reads in the engine package (the checkout's owner); the daemon serializes calls onto the engine goroutine in Task 2. No engine state is touched — these never stage, commit, or write.

**Files:**
- Create: `internal/engine/history.go`
- Test: `internal/engine/history_test.go`

**Interfaces:**
- Consumes: `gitx.Run`, `repo.ValidateFolderName`, `repo.ValidateRelPath`, existing test helpers in `internal/engine/helpers_test.go` (read them first; reuse the checkout-builder + `mustGit` idioms of `commit_test.go`).
- Produces (Task 2 depends on these exact shapes):

```go
// HistoryVersion is one commit touching the queried folder/path, newest first.
type HistoryVersion struct {
	Rev     string    // full commit hash
	Subject string    // verbatim commit subject
	Host    string    // parsed from a capture subject; "" otherwise
	Stamp   time.Time // parsed from a capture subject; zero otherwise
	Paths   []string  // folder-relative changed paths; populated only in folder-wide mode
	Live    bool      // path mode only: this rev's blob for path equals HEAD's
}

func (e *Engine) History(ctx context.Context, folder, path string, limit int) ([]HistoryVersion, error)
func (e *Engine) BlobAt(ctx context.Context, folder, path, rev string) ([]byte, error)

var (
	ErrBlobTooLarge = errors.New("blob exceeds the API size cap")
	ErrBlobBinary   = errors.New("blob is not valid UTF-8 text")
)
```

**Design (write these rules into doc comments):**

- Validation first, fail closed: `folder` via `repo.ValidateFolderName` (also accept `repo.GlobalFolder`, which ValidateFolderName must be checked against — if it rejects `_global`, special-case allow it and say why); `path` (when non-empty) via `repo.ValidateRelPath`; `rev` must match `^[0-9a-f]{7,64}$` (only ever revs a History reply emitted); `limit` clamped to `[1, 500]`, `0 → 50`.
- History command (one subprocess, parse-once):
  `git log --no-renames --max-count=<limit> -z --name-only --format=%x01%H%x00%s -- <folder>` (append `/<path>` to the pathspec in path mode). Records are split on `\x01`; within a record, fields split on NUL: hash, subject, then zero-or-more changed paths (merge commits simplified out by git's default history simplification yield zero paths — tolerate). Changed paths arrive repo-relative (`<folder>/<provider>/…`); strip the `<folder>/` prefix before returning. A hostile subject containing `\x01` can only garble its own listing entry, never write anything — note this bound in the parser comment.
- Capture-subject parse: `memory: <host> <folder> <timestamp>` — exactly four space-separated fields with prefix `memory: `, timestamp `time.RFC3339`; on any mismatch return the subject verbatim with empty Host/zero Stamp (manifest commits, integrate merges, and foreign subjects all render honestly in the UI).
- `Live` (path mode only): resolve `HEAD:<folder>/<path>` once via `git rev-parse`; mark each version whose `<rev>:<folder>/<path>` blob OID equals it. Content-identity semantics: after a restore, both the restored rev and its source read Live=true — truthful ("this content is live"). A deleted path (HEAD resolve fails) marks nothing live.
- `BlobAt`: `git cat-file --textconv <rev>:<folder>/<path>` — textconv is the checkout's own decrypt wiring, so the returned bytes are PLAINTEXT (acceptance: byte-equal to `git cat-file --textconv` run manually). Guard order: size probe first (`git cat-file -s <rev>:<folder>/<path>`; > `historyBlobByteCap = 256 << 10` → `ErrBlobTooLarge` — the API client caps bodies at 1 MiB, and memory files are KB-scale), then content fetch, then `utf8.Valid` + no-NUL check → `ErrBlobBinary`. A missing rev/path surfaces as the git error verbatim (the daemon maps it to a 400 in Task 2).

- [ ] **Step 1: Write the failing tests** — `internal/engine/history_test.go`, table-driven, `t.Parallel()`, real git via the package's existing helpers. Build a checkout with the engine's own commit path (stage files under `projA/claude/`, commit with real capture subjects via `e.commitProjects`) so subjects are production-shaped, then:

```go
// TestHistoryPathModeListsNewestFirstWithLiveFlag: three edits to
// projA/claude/notes.md (three capture commits) → History(ctx, "projA",
// "claude/notes.md", 0) returns 3 versions newest-first; Host/Stamp parsed
// (go-cmp against the engine's own stamp), Paths empty in path mode,
// exactly the newest version Live (and after committing a fourth edit that
// byte-restores version 2's content, BOTH that new head and version 2 are
// Live — the content-identity contract).
// TestHistoryFolderWideCarriesChangedPaths: commits touching notes.md and
// other.md → folder-wide History(ctx, "projA", "", 0) versions carry
// folder-relative Paths ("claude/notes.md"), Live false everywhere.
// TestHistoryHonorsLimitClamp: 5 commits, limit 2 → 2 versions; limit 0 →
// all 5 (default 50); limit 9999 → all 5 (clamp is a max, not a demand).
// TestHistoryForeignSubjectsVerbatim: a hand-made commit with subject
// "merge stuff" → Host "", Stamp zero, Subject verbatim.
// TestHistoryRejectsBadInputs: folder "no/slash" and path "../escape" and
// hostile "-rf" folder → error BEFORE any git run (table of bad inputs).
// TestBlobAtReturnsPlaintextAtRev: with real filter wiring from the
// package's integration harness pattern (testmain_test.go builds the real
// binary — NEVER the test binary), BlobAt at the first rev returns the
// original plaintext of version 1 while the worktree holds version 3.
// TestBlobAtRefusesOversizeAndBinary: a 300 KiB file → ErrBlobTooLarge;
// a file with NUL bytes → ErrBlobBinary (errors.Is both).
// TestBlobAtRejectsBadRev: rev "HEAD" and "abc$(rm)" → validation error,
// no git subprocess (assert via error text prefix "history:").
```

If the engine package's existing tests run without real filter wiring (plain git), keep `TestBlobAtReturnsPlaintextAtRev` faithful by wiring the filters exactly the way `testmain_test.go`/integration tests already do — read those first; if only plaintext-in/plaintext-out wiring exists there, textconv falls back to raw content and the test asserts that equivalence honestly (content at rev == committed bytes), with a comment naming the e2e task (Task 21) as the ciphertext-proving layer.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/engine/ -run 'TestHistory|TestBlobAt' -v`
Expected: every test FAILS to compile (`e.History` undefined) — record the output.

- [ ] **Step 3: Implement `internal/engine/history.go`** — the exported types/sentinels above plus:

```go
const (
	historyDefaultLimit = 50
	historyMaxLimit     = 500
	historyBlobByteCap  = 256 << 10
)

// validateHistoryInputs fails closed before any git subprocess sees an
// argument: folder and path are user-influenced wire inputs (Task 2), and
// pathspecs beginning with "-" or containing ".." must never reach git.
func validateHistoryInputs(folder, path, rev string) error { … }

// parseCaptureSubject extracts (host, stamp) from the engine's own
// capture-subject convention (commit.go); ok=false leaves the caller
// rendering the subject verbatim — foreign commits are data, not errors.
func parseCaptureSubject(subject string) (host string, stamp time.Time, ok bool) { … }

// parseHistoryRecords splits `--format=%x01%H%x00%s --name-only -z` output.
func parseHistoryRecords(raw, folder string) []HistoryVersion { … }
```

`History` composes: validate → clamp → one `gitx.Run` log call → parse → in path mode resolve `HEAD:` OID then per-version `git rev-parse <rev>:<pathspec>` for Live (skip silently on resolve failure — a deleted file has no live version). `BlobAt`: validate → size probe → cat-file --textconv → UTF-8/NUL guard.

- [ ] **Step 4: Run the package tests, then the full suite FOREGROUND**

Run: `go test ./internal/engine/ -run 'TestHistory|TestBlobAt' -v` → PASS, then `(ulimit -u 1400; go test ./... -race -count=1)` → PASS, then `golangci-lint run && gofumpt -l . && go tool deadcode -test ./...` → clean (deadcode will flag the new exports until Task 2 consumes them — if it does, note it in the report and confirm zero findings again in Task 2; do NOT suppress).

- [ ] **Step 5: Commit**

```bash
git add internal/engine/history.go internal/engine/history_test.go
git commit -m "feat(engine): read-only history and blob lookups over the checkout"
```

---

### Task 2: Daemon — read funnel + `GET /v0/history` + `GET /v0/blob` + client

Spec §6's API. Reads ride the engine goroutine (ADR 20 D3) through a NEW read funnel: like `submitAdmin` but (a) no quiesce refusal — spec §15 greys only mutations, and the quiesce window's CLI surgery (`.git/config`/`.gitattributes` re-wiring) cannot corrupt `git log`/`cat-file`; a checkout mid-`init` re-clone is caught by the state gate instead — and (b) no post-op sync cycle (a history browse must not trigger fetch/integrate).

**Files:**
- Modify: `internal/daemon/api/types.go` (history/blob types + `UnitInfo.RepoSubdir`)
- Modify: `internal/daemon/api/client.go` (+ `History`, `Blob` — the first query-string GETs)
- Modify: `internal/daemon/daemon.go` (read funnel + controller methods)
- Modify: `internal/daemon/server.go` (two GET routes + controller interface)
- Test: `internal/daemon/api/types_test.go`, `internal/daemon/server_test.go`, `internal/daemon/daemon_test.go` (extend, following each file's existing fake/harness idioms)

**Interfaces:**
- Consumes: Task 1's `engine.History`/`engine.BlobAt`/`engine.ErrBlobTooLarge`/`engine.ErrBlobBinary`; existing `submitAdmin`/`statusError`/`refreshState`/`errNotInitialized` shapes.
- Produces (every later task's wire surface):

```go
// api/types.go
type HistoryVersion struct {
	Rev       string     `json:"rev"`
	Subject   string     `json:"subject"`
	Host      string     `json:"host,omitempty"`
	Timestamp *time.Time `json:"timestamp,omitempty"` // nil = not a capture subject
	Paths     []string   `json:"paths,omitempty"`     // folder-wide mode only
	Live      bool       `json:"live"`
}
type HistoryResponse struct {
	Versions []HistoryVersion `json:"versions"`
}
type BlobResponse struct {
	Content string `json:"content"`
}
// UnitInfo gains (additive, omitempty — old daemons/clients unaffected):
//   RepoSubdir string `json:"repo_subdir,omitempty"`
// The hub needs it to map a local file to its repo path (<provider>/<repo_subdir>/<file>).

// api/client.go
func (c *Client) History(ctx context.Context, folder, path string, limit int) (HistoryResponse, error)
func (c *Client) Blob(ctx context.Context, folder, path, rev string) (BlobResponse, error)
```

Client methods build the path with `url.Values` (`net/url`): `"/v0/history?" + values.Encode()` — `do` needs no change (nil body GET).

```go
// daemon.go — controller growth (server.go's interface adds the same two):
func (d *Daemon) History(ctx context.Context, folder, path string, limit int) (api.HistoryResponse, error)
func (d *Daemon) Blob(ctx context.Context, folder, path, rev string) (api.BlobResponse, error)

// The read funnel: adminRequest's shape, its own channel, serviced by a new
// loop arm that does NOT run a post-op cycle and does NOT check quiesce.
readRequests chan adminRequest   // new Daemon field, made in New like the others
func (d *Daemon) submitRead(ctx context.Context, reason string, run func(context.Context, *engine.Engine) (any, error)) (any, error)
```

`submitRead` = `submitAdmin` minus the quiesce check (keep the `refreshState != "ready"` gate → `errNotInitialized`; keep the 60s bounded waits with the same busy/timeout messages). Loop arm:

```go
case request := <-d.readRequests:
	// Read-only history/blob work runs HERE so it can never race the
	// writer (ADR 20 D3). Unlike adminRequests: no post-op cycle (nothing
	// changed) and no quiesce refusal (spec §15 greys mutations only).
	result, err := request.run(ctx, syncEngine)
	request.reply <- adminReply{result: result, err: err}
```

Error mapping in the controller methods: `engine.ErrBlobTooLarge` → `statusError{413, err.Error()}`; `engine.ErrBlobBinary` → `statusError{415, err.Error()}`; Task 1 validation errors and unknown rev/path git errors → `statusError{400, …}` (detect validation via a sentinel: give Task 1's `validateHistoryInputs` a package-level `var ErrBadHistoryInput = errors.New("history: invalid input")` wrapped into its returns, and map `errors.Is` here — add that sentinel to Task 1's file in THIS task if Task 1 shipped without it, with its test). Server routes follow `/v0/status`'s GET shape, parsing `r.URL.Query()`; `limit` parse failure = 400.

**`Projects()` change:** populate `RepoSubdir: u.RepoSubdir` in the UnitInfo projection (daemon.go:869 area).

- [ ] **Step 1: Write the failing tests**

```go
// api/types_test.go — TestHistoryResponseRoundTrip: JSON round-trip with
// go-cmp; omitempty proof: a zero-Host/nil-Timestamp version marshals
// without "host"/"timestamp" keys; UnitInfo without RepoSubdir is
// byte-identical to the pre-change payload (additive-field contract, the
// same proof style Task 6.5's fields carry in this file).
// server_test.go — extend the fake controller with History/Blob recorders:
// TestHistoryEndpointParsesQuery: GET /v0/history?folder=projA&path=claude/n.md&limit=7
// reaches the controller with exactly those values; POST → 405; controller
// statusError{400} → HTTP 400 with the message.
// TestBlobEndpointParsesQuery: folder/path/rev threaded; 413/415 pass through.
// daemon_test.go — against the package's real-daemon harness (read its
// existing checkout-provisioning helpers first):
// TestHistoryServedThroughReadFunnel: provision a ready checkout with two
// capture commits, call d.History via the API client end-to-end (UDS), and
// assert versions match Task 1's direct engine.History output (go-cmp).
// TestReadsRefuseUninitialized: an unprovisioned daemon answers History
// with the errNotInitialized envelope (HTTP 500 containing "run `agent-brain init`").
// TestReadsAllowedWhileQuiesced: quiesce the daemon (d.Quiesce(60)), then
// History succeeds while TriggerSync is refused — pins the mutations-only
// greying contract so a future refactor cannot silently fold reads into
// submitAdmin.
```

- [ ] **Step 2: Run tests to verify they fail** — `go test ./internal/daemon/... -run 'History|Blob|ReadFunnel|Quiesced' -v` → compile failures / 404s. Record.

- [ ] **Step 3: Implement** — types, client methods, `readRequests` channel + `submitRead` + loop arm, controller methods calling `e.History`/`e.BlobAt` with the error mapping, two routes, controller-interface growth, `RepoSubdir` projection.

- [ ] **Step 4: Run** — package tests, then full suite foreground + lint/format/deadcode. Expected: PASS, deadcode zero (Task 1's exports now consumed).

- [ ] **Step 5: Commit**

```bash
git add internal/daemon internal/engine
git commit -m "feat(daemon): read-only /v0/history and /v0/blob on the engine goroutine"
```

---

### Task 3: Bare `agent-brain` opens the hub; uninitialized launch TTY- and agent-gated

Spec §1 exactly. The bare command becomes the hub (dashboard alias unchanged); an uninitialized machine gets the announced guided init on a human TTY, and the pointer everywhere else.

**Files:**
- Modify: `internal/cli/root.go` (root gains `Args: cobra.NoArgs` + `RunE`)
- Create: `internal/cli/hub.go` (entry decision + announce + init reuse + probe + launch)
- Modify: `internal/cli/dashboard.go` (extract the TUI-launching body into `launchHub(cmd)`; the `dashboard` command and the bare root both call it)
- Modify: `internal/cli/init.go` (extract `newInitCmd`'s RunE body into `runInteractiveInitFlow(cmd *cobra.Command) error` — the flag-driven struct literal stays in RunE; the hub calls the flow with all-default values: interactive, no key flags, `defaultRepoName`, service install on, interactive enrollment)
- Test: `internal/cli/hub_test.go`
- Create: `test/e2e/scripts/bare_command.txt`; Modify: `test/e2e/testscript_test.go` if scripts are enumerated explicitly (read it — if it globs `scripts/*.txt`, no change)

**Interfaces:**
- Consumes: `isInteractiveTTY`, `isAccessible`, `runInit`+`initState` composition (init.go:40–75), `buildDoctorDeps` + `doctor.SafetyGate` (the initialized probe), `launchHub` (this task's extraction of dashboard.go:37–83).
- Produces:

```go
// agentEnvVars is the coding-agent fingerprint (spec §1 exact list, ADR 20 D1).
var agentEnvVars = []string{
	"CLAUDECODE", "CURSOR_AGENT", "CODEX_SANDBOX", "CODEX_THREAD_ID",
	"CODEX_CI", "GEMINI_CLI", "CLINE_ACTIVE", "OPENCODE",
	"OPENCLAW_SHELL", "ANTIGRAVITY_CLI_ALIAS",
}

// hubEntryDecision is what a bare invocation does. Pure — the unit-testable
// core of spec §1's matrix.
type hubEntryDecision int
const (
	hubOpen        hubEntryDecision = iota // initialized + TTY
	hubGuidedInit                          // uninitialized + human TTY
	hubPointerExit                         // everything else — print pointer, exit non-zero
)
func decideHubEntry(initialized, tty, agentEnv bool) hubEntryDecision
func agentEnvDetected(getenv func(string) string) bool // any fingerprint set AND non-empty

// hubInitialized probes the machine the same way the daemon's readiness
// gate does — SafetyGate over the ambient deps. Cheap CLI-side call; the
// daemon does NOT need to be running (spec §1: daemon-unreachable still
// opens the hub with the degraded banner).
func hubInitialized(ctx context.Context) bool

const hubPointer = "agent-brain is not initialized. To get started, run: agent-brain init"
const hubAnnounce = "agent-brain is not set up on this machine — starting guided setup (esc cancels)"
```

**Behavior (runHub, the root RunE):**

1. Initialized + interactive TTY → `launchHub(cmd)`.
2. Initialized + non-TTY → the dashboard command's existing refusal text (return the same error `dashboard requires an interactive terminal …` — scripting equivalents named).
3. Uninitialized + (non-TTY OR agent env) → print `hubPointer` to stderr, return a plain error so fang exits 1 (return `errors.New(hubPointer)` and print nothing yourself — fang prints returned errors to stderr; verify against how doctor's failure exit works at doctor.go:89 and match it).
4. Uninitialized + human TTY → print `hubAnnounce` to stdout, run `runInteractiveInitFlow(cmd)`; on error return it. Then RE-PROBE `hubInitialized`: init reports a user cancel as success (`runInitSteps` swallows `errInitCancelled` — init.go:126), so the probe is the ONLY honest completion signal. Probe true → `launchHub(cmd)`; false → return `errors.New("setup was not completed — run: agent-brain init")` (non-zero, spec §1's cancel contract).

Root wiring: root gets `Args: cobra.NoArgs` and the RunE; a mistyped subcommand (`agent-brain nonsense`) must keep failing with cobra's unknown-command error, never open a TUI — pin it in the testscript. `--help`/`--version` behavior unchanged (cobra flags run before RunE).

- [ ] **Step 1: Write the failing tests**

```go
// hub_test.go (unit, t.Parallel, table-driven):
// TestDecideHubEntryMatrix — all 8 (initialized × tty × agentEnv) combos
// against spec §1: {true,true,false}→hubOpen; {true,true,true}→hubOpen
// (agent gating applies to the INIT path only — an initialized machine's
// hub refusal is the TTY gate, and agentEnv+TTY on an initialized machine
// still opens: the wizard risk ADR 20 D1 gates does not exist there);
// {false,true,false}→hubGuidedInit; {false,true,true}→hubPointerExit;
// any tty=false → initialized? hubOpen is impossible: expect pointer/refusal
// per runHub rules — encode the decision function's contract precisely.
// TestAgentEnvDetected — each of the ten vars individually set→true;
// set-but-empty→false; none→false (fake getenv map).
```

Decision-matrix nuance to encode (and comment): `decideHubEntry` is consulted only after the TTY check routes non-TTY to its refusal/pointer, so define it total anyway (non-TTY rows → hubPointerExit) and let `runHub` pick the wording per initialized-state.

```
# test/e2e/scripts/bare_command.txt
# Bare command on an uninitialized, non-interactive machine: pointer, exit
# non-zero, no prompt, no hang (spec §1; clig.dev TTY rule).
! exec agent-brain
stderr 'agent-brain is not initialized'
stderr 'run: agent-brain init'

# Agent fingerprint forces the same pointer (testscript has no TTY either
# way; the TTY+agent distinction is pinned by the unit matrix).
env CLAUDECODE=1
! exec agent-brain
stderr 'agent-brain is not initialized'
env CLAUDECODE=

# A typo must stay an unknown-command error, never a TUI.
! exec agent-brain nonsense
stderr 'unknown command'

# Initialized machine (provision non-interactively), still non-TTY: the
# dashboard's interactive-terminal refusal, naming the scriptable equivalents.
exec agent-brain init --non-interactive --generate-key --skip-service --enroll none
! exec agent-brain
stderr 'requires an interactive terminal'
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/cli/ -run TestDecideHubEntry -v` (compile fail) and `go test ./test/e2e/ -run 'TestScripts/bare_command' -v` (bare exec currently prints help with exit 0 → `! exec` assertion fails). Record.

- [ ] **Step 3: Implement** — hub.go + root.go RunE + `launchHub` extraction + `runInteractiveInitFlow` extraction. The extractions are MOVES: `git diff` should show dashboard.go's RunE body relocating verbatim; behavior-neutral for `agent-brain dashboard` and `agent-brain init` (their tests stay green untouched).

- [ ] **Step 4: Run** — cli package tests, the new testscript, then full suite foreground + lint/format/deadcode → all green.

- [ ] **Step 5: Commit**

```bash
git add internal/cli test/e2e/scripts/bare_command.txt test/e2e/testscript_test.go
git commit -m "feat(cli)!: bare agent-brain opens the hub with gated guided setup"
```

(`!` — a behavior change of the bare invocation; the body cites ADR 20 D1 and the spec §1 matrix.)

---

### Task 4: Dashboard split — `views/` + `theme/` subpackages, zero behavior change

Spec §15's structure, done BEFORE the feature wave so every new screen lands in its final home. Pure refactor: byte-identical renders, all existing tests pass (moved, not rewritten).

**Files:**
- Create: `internal/cli/dashboard/theme/theme.go` (+ `theme_test.go`)
- Create: `internal/cli/dashboard/views/` — move `projects.go`, `conflicts.go`, `activity.go`, `doctorview.go`, `track.go`, `keymap.go` (+ their tests) here
- Modify: `internal/cli/dashboard/dashboard.go`, `data.go` (root reducer stays; references become `views.*`)
- Modify: `internal/cli/dashboard/dashboard_test.go` (root-level tests stay; view-local tests move with their views)

**Interfaces:**
- Produces (later tasks build on these):

```go
// theme — package-level styles replacing dashboard.go's var block, built on
// catppuccin/go (Mocha dark / Latte light) with the SAME visible glyph/text
// contract: tests strip styling, so only names/structure matter here.
package theme
type Styles struct {
	Title, Header, Dim, OK, Warn, Fail, Info, ActiveTab, InactiveTab lipgloss.Style
	Badge, Toast, Selected                                          lipgloss.Style // consumed by later tasks
}
func Default(isDark bool) Styles // Mocha when dark, Latte when light
```

Root holds `styles theme.Styles`, defaults `theme.Default(true)`, and re-derives on `tea.BackgroundColorMsg` (`m.styles = theme.Default(msg.IsDark())`); `Init` batches `tea.RequestBackgroundColor` alongside the existing Cmds. Promote `github.com/catppuccin/go v0.2.0` direct (`go get github.com/catppuccin/go@v0.2.0` — already in the graph via huh).

```go
// views — the moved view types export what the root reducer touches:
package views
type ProjectsView struct{ … }   // was projectsView; methods SetUnits, SetLoadErr,
                                 // SetSize, Update, View, ModalOpen, OnSyncResult,
                                 // OnUntrackResult, OnDiscover, OnIdentify, OnTrackResult,
                                 // SelectedUnit() (api.UnitInfo, bool)
type ConflictsView struct{ … }  // Set, View
type ActivityView struct{}       // View(status, statusErr, units, now)
type DoctorView struct{ … }     // Set, View
// DataSource is the consumer-side seam (Go interface-at-the-consumer):
// EXACTLY the current dashboardData method set. Root's apiData implements
// it; root passes it into Update calls as today.
type DataSource interface { …status/projects/sync/track/untrack/doctor/conflicts as in data.go:34… }
// TrackCandidate, TrackRoot, TrackActions (was trackActions — exported, with
// Discover/Identify fields), the add-flow stages, and the keymap move verbatim.
```

Messages stay defined where produced: root keeps its Cmd-produced messages (statusMsg etc. stay unexported in root); view-internal messages (`discoverMsg`, `identifyMsg`, `trackResultMsg`) move with track.go and export (`DiscoverMsg`…) since root's Update switches on them. `dashboardData` becomes an alias consumed as `views.DataSource` (delete the root copy; root's `apiData` stays in data.go implementing the views interface).

- [ ] **Step 1: Move + mechanical export renames.** `git mv` the six files + their tests into `views/`, update package clauses, export the identifiers the root touches (rename call sites), extract the style vars into `theme` and thread `theme.Styles`. Where views previously reached the shared style vars directly, they now hold a `styles theme.Styles` field set via `SetStyles(theme.Styles)` called from root on construction and on background-color change (one setter, not per-render threading).
- [ ] **Step 2: Run the moved tests unchanged in intent** — `go test ./internal/cli/dashboard/... -v`. Every pre-existing test must pass with only package-qualifier/rename edits; any assertion change beyond renames is a refactor bug — stop and fix the refactor, not the test.
- [ ] **Step 3: Add the two new pins** —

```go
// theme_test.go
// TestDefaultStylesRenderPlainText: for both isDark values, Styles render
// text that plain() strips to the input verbatim (the glyphs-not-color
// contract every view test depends on survives the palette swap).
// dashboard_test.go
// TestBackgroundColorSwapsPalette: send a dark-background
// tea.BackgroundColorMsg then a light one through Update; model stays
// renderable (View non-empty, no panic) — pins the re-derive wiring.
```

- [ ] **Step 4: Full suite foreground + lint/format/deadcode** → green. `go build ./...` proves no import cycles (views must not import the dashboard root — if a cycle appears, the leaked identifier belongs in views or theme, move it).
- [ ] **Step 5: Commit**

```bash
git add internal/cli/dashboard go.mod go.sum
git commit -m "refactor(dashboard): split views and theme subpackages ahead of the hub"
```

---

### Task 5: Action registry, palette, help overlay, toasts, root chrome

Spec §14 + §2's chrome: ONE registry drives keymap, footer, `ctrl+k` palette, and `?` help so they cannot drift; `esc` at root asks before quitting; toasts + status-bar plumbing every later task reuses; untrack rebinds `t`→`u` (spec §13).

**Files:**
- Create: `internal/cli/dashboard/actions/actions.go` (+ test)
- Create: `internal/cli/dashboard/views/palette.go`, `views/help.go` (+ tests)
- Modify: `internal/cli/dashboard/dashboard.go` (overlay states, toasts, quit prompt, footer from registry)
- Modify: `internal/cli/dashboard/views/keymap.go` (rebind; keymap renders from the registry's bindings)
- Test: extend `dashboard_test.go`, `views/projects_test.go`

**Interfaces:**

```go
package actions
// Action is one user-invokable operation. The SAME rows drive the palette
// list, the per-view footer, and the help overlay (spec §14's single source).
type Action struct {
	ID      string             // stable identifier ("sync-project", "quit", …)
	Title   string             // palette/help label ("Sync selected project")
	Keys    []string           // bubbles/key keys ("s")
	KeyHint string             // footer form ("s")
	Scope   Scope              // where it applies
	Mutates bool               // greyed + refused while the daemon is quiesced (spec §15)
}
type Scope int
const (
	ScopeGlobal Scope = iota // any root view
	ScopeProjects            // Projects tab
	ScopeDoctor              // Doctor tab
	ScopeBrowser             // memory browser (Task 11+)
	ScopeReading             // reading view
	ScopeHistory             // history view
	ScopeConflicts           // conflicts tab/detail
)
func Registry() []Action                 // the full static table
func ForScope(s Scope) []Action          // render order preserved
func Fuzzy(query string) []Action        // palette filter: subsequence match on Title+ID, rank prefix>substring>subsequence
func Binding(a Action) key.Binding       // bubbles binding built from Keys/KeyHint+Title
```

Seed rows this task (later tasks append theirs): switch-tabs, select, quit, sync-project, sync-fleet, add-project (`a`), untrack (`u` — REBIND), doctor-refresh placeholderless (arrives Task 19: do NOT add rows for unbuilt features), open-palette (`ctrl+k`), help (`?`), search (`/` — row exists but its handler arrives Task 15; the palette must hide rows whose runner is nil, so give the root a `available(actionID) bool` gate and register runners per task — an Action without a registered runner is invisible everywhere, which keeps the registry honest while features land incrementally).

Root chrome:

```go
// dashboard.go additions
type toast struct{ text string; expiresAt time.Time }
func (m *Model) pushToast(text string)        // 5s TTL; expiry checked on the existing 2s tick (tickMsg) — no extra timers
// overlay state: paletteOpen bool + palette views.PaletteModel; helpOpen bool;
// quitPrompt bool (esc at root: "quit agent-brain? (y/n)" inline footer prompt;
// q still quits immediately — spec §2).
// runner registry: map[string]func() tea.Cmd — Update dispatches palette
// selections and key matches through ONE dispatch(actionID) so key and
// palette behavior cannot diverge.
// Quiesce greying: dispatch refuses Mutates actions while
// m.status.QuiescedUntil is active, toasting the existing refusal wording
// ("daemon quiesced until …") instead of calling the daemon.
```

`views.PaletteModel`: textinput + filtered action list (cursor, enter → chosen ActionID surfaced as `views.PaletteChoiceMsg{ID string}`), esc closes. `views.HelpModel`: static render of `actions.Registry()` grouped by Scope with the keymap column; any-key closes. Footer: replace `forTab`/`forModal` composition with registry-driven rows for the active scope + modal subsets (keep `forModal` for the add/confirm modals — they are input-owned states, not actions).

- [ ] **Step 1: Failing tests**

```go
// actions_test.go: TestRegistryIDsUniqueAndKeysDisjointPerScope (two actions
// in one scope must not share a key — table over ForScope × key sets);
// TestFuzzyRanksPrefixOverSubsequence ("sy" → sync-* first; "qt" still
// finds quit).
// palette_test.go: TestPaletteFiltersAndChooses — type "sync", cursor down,
// enter → PaletteChoiceMsg{ID:"sync-fleet"}; esc → closed, no choice.
// help_test.go: TestHelpListsEveryRegisteredAction — every Registry() Title
// appears in plain(view).
// dashboard_test.go: TestCtrlKOpensPaletteAndDispatches — ctrl+k, type,
// enter on sync-fleet → the fakeData records a fleet sync (proves palette
// and key share one dispatch); TestEscAtRootPromptsBeforeQuit — esc →
// prompt visible, n → stays, esc→y → quitting; TestQuiescedMutationRefusedLocally —
// status with QuiescedUntil future: pressing u toasts the refusal and the
// fakeData records NO untrack call; TestToastExpiresOnTick.
// projects_test.go: TestUntrackRebindToU — u opens the confirm (t no longer does).
```

- [ ] **Step 2: Verify failure** — `go test ./internal/cli/dashboard/... -run 'Palette|Help|Registry|Toast|Quiesced|Rebind|EscAtRoot' -v`. Record.
- [ ] **Step 3: Implement** the registry, both overlay views, root chrome, rebind. Footer/help/palette all render through `actions` — delete any now-dead keymap help plumbing (deadcode gate will catch leftovers).
- [ ] **Step 4: Full suite foreground + lint/format/deadcode** → green.
- [ ] **Step 5: Commit**

```bash
git add internal/cli/dashboard
git commit -m "feat(dashboard): action registry drives palette, help, footer, and toasts"
```

---

### Task 6: `memoryfs` — provider-dir enumeration, frontmatter meta, atomic mutations

The read/write substrate of spec §3/§5. Plain filesystem + provider classification; no TUI imports (only api/provider/repo/renameio/stdlib).

**Files:**
- Create: `internal/cli/dashboard/memoryfs/memoryfs.go`, `frontmatter.go` (+ `memoryfs_test.go`, `frontmatter_test.go`)
- Modify: `internal/cli/dashboard/dashboard.go` + `internal/cli/dashboard.go` (Config gains `Registry *provider.Registry`, wired from `buildTrackDeps().registry` in the cli command — needed for classification)

**Interfaces:**

```go
package memoryfs

// Memory is one file under an enrolled unit's provider dir.
type Memory struct {
	Provider    string         // unit identity —
	Folder      string         //   the repo folder it syncs to
	LocalDir    string         //   the unit's provider dir root
	RelPath     string         // path under LocalDir (filename, or subdir path)
	RepoPath    string         // <provider>[/<repo_subdir>]/<RelPath> — the /v0/history path key
	Name        string         // frontmatter name, else filename stem
	Description string         // frontmatter description, else ""
	Class       provider.Class // fact files are editable; derived-index/regenerated render read-only
	ModTime     time.Time
	Size        int64
}

// List walks each unit's LocalDir (units pre-filtered to one folder by the
// caller), classifies via the provider's pattern table, and returns every
// non-ignore-class regular file. Symlinks are skipped (the engine's own
// mirror-in exfiltration rule); a missing dir yields no entries, not an
// error (an enrolled-but-empty unit is normal).
func List(registry *provider.Registry, units []api.UnitInfo) ([]Memory, error)

// ReadBody returns the file's content, capped at maxBodyBytes (1 MiB) —
// same defensive posture as the blob endpoint; oversize returns ErrTooLarge.
func ReadBody(m Memory) (string, error)

// WriteFileAtomic lands content at dir/rel via renameio (write-temp in the
// SAME directory + rename): one atomic replace, one clean watcher event
// (ADR 20 D2). Creates parent dirs 0o700 as needed.
func WriteFileAtomic(dir, rel string, content []byte) error

// Delete removes the provider file (plain os.Remove — deletion IS the
// mutation the watcher captures; recoverable via history restore).
func Delete(m Memory) error

// Rename moves m.RelPath to newRel inside the same LocalDir (os.Rename —
// atomic same-volume). Validates newRel via repo.ValidateRelPath and
// requires the same extension.
func Rename(m Memory, newRel string) error

// LocalTarget maps a repo path (as /v0/history reports) back to the unit
// dir + relative path that produces it — restore's write target. ok=false
// when no enrolled unit matches (e.g. the unit was untracked).
func LocalTarget(units []api.UnitInfo, folder, repoPath string) (dir, rel string, ok bool)

// Skeleton is the provider-correct new-memory stub (spec §5 `n`): claude
// gets the frontmatter block (name/description/metadata.type) + body stub;
// every other provider gets "# <name>\n\n".
func Skeleton(providerName, name string) string

var ErrTooLarge = errors.New("memory file exceeds the read cap")
```

```go
// frontmatter.go — minimal fence parser (NOT a YAML dependency: the two
// keys are advisory display metadata; absence and malformation must render
// gracefully, and the claude adapter's own reconcile.go sets the precedent
// of line-based parsing).
// Meta reads at most the first 4 KiB: if line 1 is exactly "---", scan to
// the closing "---" collecting top-level `name:` / `description:` values
// (quotes trimmed, same tolerance as claude/reconcile.go's
// splitFrontmatterLine — mirror its rules, do not import it: it is
// unexported and adapter-internal).
func Meta(path string) (name, description string, hasFrontmatter bool)
```

- [ ] **Step 1: Failing tests** — table-driven over a `t.TempDir()` fixture tree:

```go
// TestListClassifiesAndOrders: a claude unit (notes.md fact, MEMORY.md
// derived-index, .DS_Store ignore) + a codex unit with RepoSubdir —
// ignore-class absent, RepoPath includes the subdir, Name/Description from
// frontmatter when present, stem fallback otherwise; symlink skipped.
// TestReadBodyCapsOversize: 2 MiB file → ErrTooLarge.
// TestWriteFileAtomicReplacesInOneRename: concurrent-read proof — write
// 64 KiB content A, start a goroutine loop re-reading the path, atomically
// replace with content B ×50: every read observes exactly A or exactly B,
// never a prefix (the no-partial-content acceptance row, spec §17).
// TestRenameValidatesTarget: "../escape.md" and "notes.txt"→"notes.md"
// extension mismatch → error; "renamed.md" succeeds and the old path is gone.
// TestLocalTargetRoundTrips: List → each Memory's (Folder, RepoPath) →
// LocalTarget returns its (LocalDir, RelPath) (go-cmp).
// TestSkeletonClaudeFrontmatter: contains "name: <n>", "description:",
// "metadata:", "type:" lines and a body stub.
// frontmatter_test.go: fences present/absent/unclosed/quoted values/
// crlf/first-4KiB-boundary table.
```

- [ ] **Step 2: Verify failure** (compile). Record.
- [ ] **Step 3: Implement.** `List` composes `provider.Registry.Get(unit.Provider)` + the provider's `Patterns()` through the same matching the engine uses (find the exported match entry point in `internal/provider/match.go` and use it — read it first; if matching is only reachable via an unexported helper, use `provider.MatchClass`-equivalent exported API that exists, and if none exists, add ONE exported wrapper to `internal/provider` with its own test and a doc comment naming both consumers).
- [ ] **Step 4: Full suite foreground + lint/format/deadcode** (Config.Registry wiring makes cli compile; deadcode: memoryfs exports consumed by tests only until Task 11 — if deadcode flags them, note in the report exactly as Task 1 did).
- [ ] **Step 5: Commit**

```bash
git add internal/cli/dashboard/memoryfs internal/cli/dashboard/dashboard.go internal/cli/dashboard.go
git commit -m "feat(dashboard): memoryfs enumerates and atomically mutates provider dirs"
```

---

### Task 7: `links` — wiki-link parse, resolve, backlinks

Spec §4's `[[link]]` navigation substrate.

**Files:**
- Create: `internal/cli/dashboard/links/links.go` (+ `links_test.go`)

**Interfaces:**

```go
package links

// Link is one [[target]] occurrence; offsets are byte positions in the body
// (the reading view highlights and cycles them in order).
type Link struct {
	Target     string // inner text, trimmed
	Start, End int    // byte span INCLUDING the brackets
}

// Parse scans body for non-nested [[target]] spans. Rules (each a test row):
// no newline inside; empty target ignored; unterminated "[[x" ignored;
// "[[a|b]]" uses a as target (pipe alias tolerated, alias shown by caller).
func Parse(body string) []Link

// Index resolves targets against a project's memories and answers backlinks.
// Resolution: exact filename stem match first, then frontmatter Name match,
// both case-insensitive (memory naming convention: stem == frontmatter name).
type Index struct{ /* name → memoryfs.Memory; reverse map */ }

// BuildIndex parses every memory's body once (readBody seam so tests need
// no real files and the browser can reuse cached bodies).
func BuildIndex(memories []memoryfs.Memory, readBody func(memoryfs.Memory) (string, error)) *Index
func (ix *Index) Resolve(target string) (memoryfs.Memory, bool)
func (ix *Index) Backlinks(m memoryfs.Memory) []memoryfs.Memory // sorted by Name
func (ix *Index) Dangling(m memoryfs.Memory) []Link             // links in m with no resolution
```

- [ ] **Step 1: Failing tests** — parse table (the rule rows above + multiple links + adjacent `[[a]][[b]]` + unicode targets); index: three fixture memories where A links B and C-by-frontmatter-name, B links missing "ghost" → `Resolve` hits, `Backlinks(B) == [A]`, `Dangling(B) == [ghost]`; read-error tolerance (readBody error for one memory → its outbound links absent, others intact, no panic).
- [ ] **Step 2: Verify failure.** Record.
- [ ] **Step 3: Implement** (single-pass scanner, no regex needed; keep it allocation-light — bodies are re-parsed on browser refresh).
- [ ] **Step 4: Full suite foreground + lint/format/deadcode.**
- [ ] **Step 5: Commit**

```bash
git add internal/cli/dashboard/links
git commit -m "feat(dashboard): wiki-link parsing, resolution, and backlinks"
```

---

### Task 8: `lint` — advisory memory doctor + `lint.stale_after_days` config

Spec §8 exactly: advisory, never gating (like scan).

**Files:**
- Create: `internal/cli/dashboard/lint/lint.go` (+ `lint_test.go`)
- Modify: `internal/config/settings.go` (+ `settings_test.go`)

**Interfaces:**

```go
// config: Settings gains (strict-TOML additive section)
type LintSettings struct {
	// StaleAfterDays flags memories unmodified longer than this; 0 disables.
	StaleAfterDays int `toml:"stale_after_days"`
}
// Settings.Lint LintSettings `toml:"lint"`; DefaultSettings: 90.
// validate(): reject negative values ("lint.stale_after_days = -1 must not be negative").
```

```go
package lint
type Issue struct {
	Rule   string // "frontmatter", "dangling-link", "stale", "index-drift"
	Detail string // human sentence naming the specifics
}
type Result struct {
	Memory memoryfs.Memory
	Issues []Issue
}
// Check runs every advisory rule. now is injected (staleness must be
// deterministic in tests); readBody is the same seam BuildIndex takes.
func Check(memories []memoryfs.Memory, index *links.Index, readBody func(memoryfs.Memory) (string, error), staleAfterDays int, now time.Time) []Result
```

Rules (each with the exact Detail wording as a test row):
- **frontmatter** — claude-provider fact-class `.md` files must have non-empty `name` and `description` (memoryfs.Meta): `"missing frontmatter"` / `"frontmatter missing name"` / `"frontmatter missing description"`.
- **dangling-link** — `index.Dangling` non-empty: `"[[<target>]] resolves to no memory in this project"` (one Issue per target).
- **stale** — `staleAfterDays > 0 && now.Sub(ModTime) > days`: `"unmodified for <N> days"`.
- **index-drift** — claude units only: parse the unit's MEMORY.md (a `(<file>.md)` per index line — mirror the link shape `claude/reconcile.go` renders; read reconcile.go first and match its emitted format exactly): fact `.md` files absent from the index → `"absent from MEMORY.md"`; index lines naming missing files → `"MEMORY.md links missing file <name>"` (attached to the MEMORY.md memory's Result).

Results carry only memories WITH issues; browser badges any memory present in the slice.

- [ ] **Step 1: Failing tests** — settings: parse/default/negative-reject/unknown-key-reject rows appended to the existing settings table style; lint: one fixture per rule + a clean fixture asserting an empty result + `staleAfterDays == 0` disables staleness.
- [ ] **Step 2: Verify failure.** Record.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Full suite foreground + lint/format/deadcode.** (Settings additivity note for the report: strict parsing means a config.toml carrying `[lint]` requires this binary or newer — same-release shipping, no fleet hazard; the daemon and hub read the same file.)
- [ ] **Step 5: Commit**

```bash
git add internal/cli/dashboard/lint internal/config
git commit -m "feat(dashboard): advisory memory lint with configurable staleness"
```

---

### Task 9: `search` — tiered cross-project matching

Spec §7's engine (the overlay view arrives Task 15).

**Files:**
- Create: `internal/cli/dashboard/search/search.go` (+ `search_test.go`)

**Interfaces:**

```go
package search
type Tier int
const (
	TierName Tier = iota // fuzzy (subsequence) on Memory.Name
	TierDescription      // case-insensitive substring on Description
	TierBody             // case-insensitive substring in the body
)
type Hit struct {
	Memory   memoryfs.Memory
	Tier     Tier
	Fragment string // the matched line, trimmed to ≤120 runes around the match
	Line     int    // 1-based body line (0 for name/description tiers)
}
// Query ranks: tier ascending, then name-match quality (prefix > substring
// > subsequence), then Name. limit bounds the result (0 → 50). readBody
// errors skip that memory silently — search is best-effort by design.
func Query(memories []memoryfs.Memory, readBody func(memoryfs.Memory) (string, error), query string, limit int) []Hit
```

Fuzzy = case-insensitive subsequence (hand-rolled ~15 lines; no dependency). Body scan is per-line so Fragment/Line fall out naturally; stop scanning a body at its first hit (one Hit per memory per tier; the best tier wins — a name hit suppresses that memory's body hit).

- [ ] **Step 1: Failing tests** — ranking table (name-prefix beats name-subsequence beats description beats body); fragment extraction (long line trimmed around the needle, needle preserved); one-hit-per-memory; empty query → nil; limit honored; cross-"project" set (memories from two folders) both matched — the overlay's ≥2-projects acceptance seed reuses this fixture shape.
- [ ] **Step 2: Verify failure.** Record.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Full suite foreground + lint/format/deadcode.**
- [ ] **Step 5: Commit**

```bash
git add internal/cli/dashboard/search
git commit -m "feat(dashboard): tiered fuzzy and full-text memory search"
```

---

### Task 10: `editorx` — hardened editor resolution + scratch lifecycle + `editor.*` config

ADR 20 D2's mechanics, minus the TUI (Task 13 wires it). Adds `mvdan.cc/sh/v3 v3.13.1`.

**Files:**
- Create: `internal/cli/dashboard/editorx/editorx.go` (+ `editorx_test.go`)
- Modify: `internal/config/settings.go` (+ test)
- Modify: `go.mod` (`go get mvdan.cc/sh/v3@v3.13.1`)

**Interfaces:**

```go
// config
type EditorSettings struct {
	// Command overrides $VISUAL/$EDITOR ("cursor --wait"). Parsed with a
	// POSIX word splitter, so quoting works.
	Command string `toml:"command"`
	// InTerminal false runs the editor without suspending the TUI (GUI
	// editors configured with their wait flag — lazygit's editInTerminal
	// precedent; auto-detection deliberately does not exist, ADR 20 D2).
	InTerminal bool `toml:"in_terminal"`
}
// Settings.Editor EditorSettings `toml:"editor"`; default {Command: "", InTerminal: true}.
```

```go
package editorx
type Editor struct {
	Argv       []string // resolved command words; the file path is appended by Command
	InTerminal bool
}
var ErrNoEditor = errors.New("no editor configured — set $EDITOR or editor.command in config")

// Resolve: settings.Command → $VISUAL → $EDITOR, first non-empty wins,
// parsed with shell.Fields(src, getenv-adapter) (POSIX: quotes, escapes —
// never strings.Fields). Empty everywhere → ErrNoEditor: the hub gates the
// binding on it (crush-style honest absence), NEVER a silent nano default.
// x/editor was evaluated and dropped for exactly these rules (see the
// plan's dependency-verification note + ADR 20 correction, Task 22).
func Resolve(settings config.EditorSettings, getenv func(string) string) (Editor, error)

// NewScratchDir creates a per-edit temp dir OUTSIDE any watched tree
// (os.MkdirTemp(cacheRoot, "agent-brain-edit-*")); cacheRoot "" resolves
// os.UserCacheDir(). Caller removes it (returned cleanup).
func NewScratchDir(cacheRoot string) (dir string, cleanup func(), err error)

// Stage copies content into dir under the memory's own filename (editors
// key syntax/behavior off the name — a .md scratch must stay .md).
func Stage(dir, filename string, content []byte) (scratchPath string, err error)

// Command builds the exec.Cmd (argv + scratchPath appended). ctx bounds
// nothing here — the TUI owns process lifetime via ExecProcess/Run.
func Command(ed Editor, scratchPath string) *exec.Cmd

// Changed byte-compares the scratch against the original content it was
// staged from (kubectl's no-op contract: unchanged bytes = cancelled edit).
func Changed(original []byte, scratchPath string) (changed bool, edited []byte, err error)
```

- [ ] **Step 1: Failing tests** — resolution precedence table (command set / VISUAL set / EDITOR set / all / none→ErrNoEditor; set-but-blank counts as unset); quoting rows (`FOO="my editor" --flag` → `["my editor","--flag"]`; a `$VAR` inside expands via getenv — pin whatever shell.Fields does with a test so its behavior is a recorded contract, not an assumption); scratch roundtrip (Stage preserves filename + bytes; cleanup removes the dir); Changed rows (identical → false; edited → true + new bytes; scratch deleted by a hostile editor → error, never a false "unchanged"); Command appends the path last (go-cmp on cmd.Args). Config rows: defaults, `[editor]` parse, unknown-key reject.
- [ ] **Step 2: Verify failure.** Record.
- [ ] **Step 3: Implement** (+ `go get mvdan.cc/sh/v3@v3.13.1 && go mod tidy` — record the resolved version in the commit body).
- [ ] **Step 4: Full suite foreground + lint/format/deadcode.**
- [ ] **Step 5: Commit**

```bash
git add internal/cli/dashboard/editorx internal/config go.mod go.sum
git commit -m "feat(dashboard): hardened editor resolution and scratch lifecycle"
```

---

### Task 11: Memory browser screen + navigation stack + glamour preview

Spec §3. Introduces the drill-in `Screen` stack and the glamour renderer (dep: `charm.land/glamour/v2 v2.0.1`).

**Files:**
- Create: `internal/cli/dashboard/views/screen.go`, `views/browser.go` (+ `browser_test.go`)
- Modify: `internal/cli/dashboard/dashboard.go` (stack, forwarding, breadcrumb; Config gains `Settings config.Settings` — StaleAfterDays here, Editor in Task 13), `internal/cli/dashboard.go` (wire Settings from the loaded config), `views/projects.go` (enter → open browser), `internal/cli/dashboard/actions/actions.go` (browser rows)
- Modify: `go.mod` (`go get charm.land/glamour/v2@v2.0.1`)

**Interfaces:**

```go
// views/screen.go
// Screen is one drill-in surface on the root's navigation stack. The root
// forwards every message to the top screen while the stack is non-empty;
// esc pops (spec §2). Screens return a replacement Screen (usually
// themselves) plus a Cmd — the root reducer stays a dumb forwarder.
type Screen interface {
	Update(msg tea.Msg) (Screen, tea.Cmd)
	View(width, height int) string
	Title() string // breadcrumb segment
}
// PushScreenMsg asks the root to push a screen (screens open sub-screens by
// returning it as a Cmd's message); PopScreenMsg pops one level.
type PushScreenMsg struct{ Screen Screen }
type PopScreenMsg struct{}
```

```go
// views/browser.go
// BrowserDeps is everything the browser screen needs, injected once (the
// consumer-side-seam idiom): no globals, fakeable in tests.
type BrowserDeps struct {
	Registry *provider.Registry
	Units    []api.UnitInfo // the folder's units
	Folder   string
	Styles   theme.Styles
	Now      func() time.Time
	ReadBody func(memoryfs.Memory) (string, error) // memoryfs.ReadBody in production
	List     func() ([]memoryfs.Memory, error)     // memoryfs.List over Units
	StaleAfterDays int                              // lint config
}
func NewBrowser(deps BrowserDeps) *Browser
// Browser state: memories, lint results, links index, list cursor, group-by-
// provider render, orderByRecency bool (o toggles; default recency), filter
// textinput (/ opens, esc clears — IN-BROWSER filter, spec §3), preview pane
// (glamour render of the selection, right side when width ≥ 100, hidden below),
// loading/error states. Refresh: on construction and on tickMsg (relist +
// re-lint; listing a memory dir is cheap and keeps the browser live against
// external agent writes).
```

Glamour wiring (root-owned, shared): root holds a `renderMarkdown func(md string, width int) string` built from `glamour.NewTermRenderer(glamour.WithStandardStyle(style), glamour.WithWordWrap(width))` where style is `"dark"`/`"light"` from the Task 4 background probe; rebuilt on background change and width change; render errors fall back to raw text (`View` must never fail). Passed into BrowserDeps (and later Reading) as a field `Render func(string, int) string`.

Keys this task (registry rows, ScopeBrowser): `enter` read (pushes a placeholder-free Reading screen in Task 12 — THIS task pushes nothing on enter yet; the row lands with Task 12's runner, so do not register enter-read here), `o` order toggle, `/` filter, `esc` back. Rows show: name, description (dim, truncated), relative modified time, `⚠` badge when lint flags. Projects tab: `enter` on a unit row pushes the Browser for that unit's Folder (all units sharing the folder) — root builds BrowserDeps from Config (Registry, Settings) + the row.

- [ ] **Step 1: Failing tests**

```go
// browser_test.go (fixture tree in t.TempDir via memoryfs, fake ReadBody):
// TestBrowserListsGroupedByProviderNewestFirst — two providers, mtimes
// staggered → plain(view) shows provider group headers and recency order;
// o → name order.
// TestBrowserFilterNarrows — "/", type "auth" → only matching name/
// description rows; esc restores.
// TestBrowserLintBadge — a memory with a dangling link renders ⚠ on its row.
// TestBrowserPreviewRendersSelection — Render seam returns "RENDERED:<input>";
// selection's body appears through it at width 120; at width 80 no preview pane.
// dashboard_test.go:
// TestEnterOnProjectsPushesBrowser — enter on a unit row → stack depth 1,
// breadcrumb contains the folder; esc pops back to tabs.
// TestStackForwardsTick — with a browser pushed, tickMsg reaches it (List
// call count increments on the fake).
```

- [ ] **Step 2: Verify failure.** Record.
- [ ] **Step 3: Implement** screen stack (root: `stack []views.Screen`; while non-empty: header + breadcrumb (`Projects ▸ <folder>`) + top.View + registry footer for the screen's scope; `esc` pops unless the top screen consumed it — filter-mode esc clears the filter first: the Screen returns itself and NO PopScreenMsg to signal consumption), browser, glamour plumbing, projects enter. `go get charm.land/glamour/v2@v2.0.1 && go mod tidy`.
- [ ] **Step 4: Full suite foreground + lint/format/deadcode.**
- [ ] **Step 5: Commit**

```bash
git add internal/cli/dashboard go.mod go.sum
git commit -m "feat(dashboard): memory browser with glamour preview and drill-in stack"
```

---

### Task 12: Reading screen — full render, link navigation, backlinks

Spec §4.

**Files:**
- Create: `internal/cli/dashboard/views/reading.go` (+ `reading_test.go`)
- Modify: `views/browser.go` (enter pushes Reading), `actions/actions.go` (ScopeReading rows + browser's enter row)

**Interfaces:**

```go
type ReadingDeps struct {
	Memory   memoryfs.Memory
	Index    *links.Index          // the browser's — shared, not rebuilt
	ReadBody func(memoryfs.Memory) (string, error)
	Render   func(string, int) string
	Styles   theme.Styles
}
func NewReading(deps ReadingDeps) *Reading
// State: body, parsed links, linkCursor int (-1 none), backlinksOpen bool,
// viewport (bubbles/v2 viewport for j/k, ctrl+d/u, g/G — spec §4's keys).
// Header line: name · class · modified (absolute) · size.
// Link navigation: tab/shift+tab cycle links (the ACTIVE link renders
// inverse-styled — post-glamour overlay is fragile, so render the body for
// display by substituting each [[target]] span before markdown rendering:
// active link → "▶target◀", dangling → struck-through via the theme style
// and a " (dangling)" marker; document the substitution in a comment).
// enter on a resolved link → PushScreenMsg{NewReading(target…)} (the
// navigation stack IS the history; esc returns — spec §4).
// b toggles the backlinks panel (list under the header; enter within it
// jumps: cursor shared with linkCursor while open).
// y → CopyPathMsg{Path string} (absolute provider-file path); the ROOT
// handles it: toast "path: <abs>" — OSC52 clipboard write is not exposed by
// bubbletea v2.0.8 (verified), so the toast IS the copy affordance; the
// comment says exactly that so nobody "fixes" it blind.
// e edit / h history: rows registered by Tasks 13/14 (runners nil until then).
```

- [ ] **Step 1: Failing tests** — render (header fields present; body through Render seam); link cycling (three links: tab advances, wraps, shift+tab reverses; active marker visible in plain()); enter-on-link pushes Reading for the target (assert PushScreenMsg carries the resolved memory); dangling link renders the marker and enter on it toasts (returns no push); backlinks toggle lists referrers by name; y emits CopyPathMsg with the absolute path; viewport scroll smoke (g/G change visible slice on a 100-line body at height 20).
- [ ] **Step 2: Verify failure.** Record.
- [ ] **Step 3: Implement** + browser enter wiring + registry rows.
- [ ] **Step 4: Full suite foreground + lint/format/deadcode.**
- [ ] **Step 5: Commit**

```bash
git add internal/cli/dashboard
git commit -m "feat(dashboard): reading view with wiki-link navigation and backlinks"
```

---

### Task 13: Edit / new / rename / delete — the $EDITOR handoff flow

Spec §5 end-to-end: the flagship mutation path. Everything lands in provider dirs; the daemon captures.

**Files:**
- Create: `internal/cli/dashboard/editflow.go` (+ `editflow_test.go`) — root-level: the flow needs ExecProcess and root chrome
- Modify: `views/browser.go` (e/n/r/d keys emit flow-request messages), `views/reading.go` (e), `dashboard.go` (flow states, pendingCapture, Config gains `CacheRoot string` — cli passes `os.UserCacheDir()`-derived root; `Config.Settings` exists since Task 11 and supplies `Settings.Editor` here), `actions/actions.go`
- Modify: `internal/cli/dashboard.go` (wire CacheRoot)

**Interfaces (root-level flow):**

```go
// editflow.go
// editSession is one in-flight handoff. Root holds at most one (a second
// request while active is refused with a toast — ExecProcess owns the
// terminal; concurrency is meaningless).
type editSession struct {
	kind        editKind // editExisting | editNew | editRestore (Task 14 reuses)
	memory      memoryfs.Memory // zero-value for editNew until landed
	targetDir   string          // provider dir
	targetRel   string          // filename to land
	original    []byte          // staged bytes (skeleton for new)
	scratchPath string
	cleanup     func()
	startedAt   time.Time
}
type editorFinishedMsg struct{ err error }
type editRequestMsg struct{ memory memoryfs.Memory }                 // views emit these —
type newRequestMsg struct{ folder string; units []api.UnitInfo }     //   root runs the flow
type renameRequestMsg struct{ memory memoryfs.Memory }
type deleteRequestMsg struct{ memory memoryfs.Memory }

// beginEdit resolves the editor (editorx.Resolve; ErrNoEditor → toast the
// exact spec wording "no editor configured — set $EDITOR or editor.command
// in config", binding stays visibly disabled via the actions availability
// gate), stages the scratch (editorx.NewScratchDir + Stage), and returns
// the launch Cmd:
//   InTerminal true  → tea.ExecProcess(editorx.Command(ed, scratch), wrap)
//     with the bubbletea-431 one-frame guard: root sets m.execHandoff=true
//     and View returns an EMPTY view for that frame (the documented
//     render-empty workaround; comment cites the issue number NOT dates).
//   InTerminal false → a plain tea.Cmd goroutine running cmd.Run() (the
//     TUI stays live; the GUI editor was configured with its wait flag).
// finishEdit (on editorFinishedMsg): editor error → toast + cleanup.
// Otherwise editorx.Changed(original, scratchPath):
//   unchanged → toast "edit cancelled, no changes made"; cleanup.
//   changed   → memoryfs.WriteFileAtomic(targetDir, targetRel, edited);
//               cleanup; class-gate note below; pendingCapture{folder, at}.
// Derived-class gate: e/r/d on a non-fact memory never starts a session —
// toast "derived index — regenerated by the provider; edit the memory files
// instead" (Class check at the request handler, one place).
```

Sub-flows:
- **new (`n`)**: single-line name input (browser modal, textinput; validate: non-empty, no `/`, forced `.md` for claude); skeleton = `memoryfs.Skeleton(provider, name)`; original = skeleton bytes so an unedited save is "cancelled" (kubectl rule); claude toast on land: `"saved — remember the MEMORY.md index line"` (spec §5).
- **rename (`r`)**: prefilled textinput → `memoryfs.Rename`; toast result. No editor involved.
- **delete (`d`)**: modal confirm default No naming the file (`"delete <name>? it stays recoverable from history (y/N)"`) → `memoryfs.Delete` → pendingCapture.
- **pendingCapture** (root): `{folder, since}`; each statusMsg checks `LastSync.At.After(since)` && any commit subject containing `" "+folder+" "` → toast `"✓ captured — pushed"` / `"✓ captured — push queued"` (Pushed/PushQueued); `LastSync.Error != ""` → toast the error; 90s deadline → toast `"capture not yet confirmed — daemon may be quiesced or offline (see Activity)"`. One pending at a time; a new mutation replaces it.

- [ ] **Step 1: Failing tests** (fake the exec boundary — the launch Cmd builder is a seam: tests call `finishEdit` paths directly and assert the launch decision without running an editor):

```go
// TestEditRefusedWithoutEditor — empty settings+env: toast wording exact,
// no session, no scratch dir created.
// TestEditUnchangedIsCancelled — finishEdit with untouched scratch → toast
// "edit cancelled, no changes made", target file untouched (bytes + mtime),
// scratch dir removed.
// TestEditChangedLandsAtomically — edited scratch → target holds edited
// bytes; pendingCapture set for the folder.
// TestEditDerivedClassRefused — e on MEMORY.md (derived-index) → the toast,
// no session.
// TestNewStagesSkeletonAndRemindsIndex — n "api-notes" on claude → scratch
// staged with Skeleton content; simulated edit → file lands; toast mentions
// MEMORY.md.
// TestRenameAndDeleteFlows — r validates + renames; d requires y (n/esc
// abort), file removed, pendingCapture set.
// TestPendingCaptureToasts — table: pushed / queued / error / 90s expiry →
// exact toast per row (statusMsg-driven, fake clock via injected now).
// TestInTerminalFalseKeepsUIAlive — launch decision for InTerminal=false is
// the goroutine Cmd (assert via the seam), never ExecProcess.
// TestSecondEditRefusedWhileActive.
```

- [ ] **Step 2: Verify failure.** Record.
- [ ] **Step 3: Implement** (+ the execHandoff empty-View guard with its comment; wire e/n/r/d request messages from browser/reading; registry rows with availability = editor-resolves ∧ fact-class ∧ no-active-session).
- [ ] **Step 4: Full suite foreground + lint/format/deadcode.**
- [ ] **Step 5: Commit**

```bash
git add internal/cli/dashboard internal/cli/dashboard.go
git commit -m "feat(dashboard): editor handoff with atomic write-back and capture toasts"
```

---

### Task 14: History screen — versions, diff, restore, deleted view

Spec §6 in the TUI, over Task 2's endpoints. Dep: `github.com/aymanbagabas/go-udiff v0.4.1`.

**Files:**
- Create: `internal/cli/dashboard/views/history.go` (+ `history_test.go`)
- Modify: `internal/cli/dashboard/data.go` (dashboardData/views.DataSource grow `History(ctx, folder, path string, limit int) (api.HistoryResponse, error)` + `Blob(ctx, folder, path, rev string) (api.BlobResponse, error)`, apiData delegating to the client)
- Modify: `views/browser.go` (h on selection; `deleted` filter view entry), `views/reading.go` (h), `dashboard.go`/`editflow.go` (restore rides editRestore), `actions/actions.go`
- Modify: `go.mod`

**Interfaces:**

```go
type HistoryDeps struct {
	Memory   memoryfs.Memory              // zero for the deleted-view path variant
	Folder   string
	RepoPath string
	Live     func() (string, error)       // current provider-file content ("" for deleted)
	Data     HistoryDataSource             // consumer-side: History+Blob only
	Render   func(string, int) string
	Styles   theme.Styles
	Units    []api.UnitInfo                // restore target mapping (memoryfs.LocalTarget)
}
type HistoryDataSource interface {
	History(ctx context.Context, folder, path string, limit int) (api.HistoryResponse, error)
	Blob(ctx context.Context, folder, path, rev string) (api.BlobResponse, error)
}
func NewHistory(deps HistoryDeps) *History
// List rows: short rev (12) · absolute time · relative · host · "live" tag.
// enter → fetch blob → glamour-render that version (viewport sub-state,
// esc back to the list).
// d → unified diff selected↔live; D → selected↔next-older (udiff.Unified
// with labels "<rev> (…)" / "live" — style +/- lines via theme, raw text in
// a viewport).
// R → confirm modal ("restore this version? it becomes a NEW capture —
// history only grows (y/N)") → fetch blob → RestoreRequestMsg{Folder,
// RepoPath, Content} → ROOT lands it through the Task-13 write path
// (memoryfs.LocalTarget → WriteFileAtomic → pendingCapture) — restore is
// never a git operation (ADR 20 D3).
// Loading/error states for every fetch (daemon errors render verbatim —
// quiesce/not-initialized wording included).
```

Browser additions: `h` on a memory pushes History (RepoPath from the Memory); a `deleted` toggle in the browser (key `x`, registry row "show deleted") swaps the list to deleted-memory mode: folder-wide `History(folder, "", 200)` → union of version Paths minus on-disk RepoPaths → rows named by path; enter/h pushes the History screen for that path (Live() returns ""); restore resurrects the file (spec §6's recoverable-delete acceptance).

- [ ] **Step 1: Failing tests** — fake HistoryDataSource: list render (rows, live tag, host/relative time); enter renders blob through Render; d produces a diff containing `-old`/`+new` lines for a two-version fixture; D diffs adjacent; R confirm modal → y emits RestoreRequestMsg with the blob content, n aborts; deleted-mode: fixture where history paths ⊃ disk paths → the missing path listed, restore message targets it; fetch-error row renders the daemon error text. Root: TestRestoreLandsAndPendsCapture (RestoreRequestMsg → file bytes land at the mapped local path, pendingCapture set).
- [ ] **Step 2: Verify failure.** Record.
- [ ] **Step 3: Implement** (+ `go get github.com/aymanbagabas/go-udiff@v0.4.1 && go mod tidy`; data-seam growth; fakeData growth in tests).
- [ ] **Step 4: Full suite foreground + lint/format/deadcode.**
- [ ] **Step 5: Commit**

```bash
git add internal/cli/dashboard go.mod go.sum
git commit -m "feat(dashboard): per-memory history with diff, restore, and deleted recovery"
```

---

### Task 15: Global search overlay

Spec §7's UI over Task 9's engine.

**Files:**
- Create: `internal/cli/dashboard/views/searchoverlay.go` (+ test)
- Modify: `dashboard.go` (`/` from root views opens it; result-choice pushes Reading), `actions/actions.go` (runner for the existing `/` row)

**Interfaces:**

```go
type SearchOverlayDeps struct {
	// Collect enumerates EVERY tracked project's memories fresh (root builds
	// it over Config.Registry + the latest projectsMsg units, grouped by
	// folder). Kept as a closure so the overlay never holds stale fleet state.
	Collect  func() ([]memoryfs.Memory, error)
	ReadBody func(memoryfs.Memory) (string, error)
	Styles   theme.Styles
}
func NewSearchOverlay(deps SearchOverlayDeps) *SearchOverlay
// One textinput; results re-queried ON INPUT with a 250ms debounce
// implemented the bubbletea way: each keystroke stamps a generation int and
// returns tea.Tick(250ms) carrying it; only the newest generation's tick
// runs search.Query (in a Cmd — never in Update). Result rows:
// folder · provider · name · dim fragment (tier-tagged). enter →
// SearchChoiceMsg{Memory} (root pushes Reading with a per-folder links
// Index built lazily); esc dismisses. Cap display at 30 with a
// "+N more" line (no silent truncation).
```

Root: `/` opens the overlay from any root view (stack empty or not — spec says root views; while a Screen is stacked, `/` belongs to the screen (browser filter), so gate on stack-empty and note it), routes all keys to it while open.

- [ ] **Step 1: Failing tests** — typing produces generations (stale tick ignored: two quick inputs → one Query call with the final text, via a counting fake); results render rows + fragment; enter emits SearchChoiceMsg for the cursor row; ≥2-folders fixture surfaces both (the spec §17 seed); esc closes; cap row appears at 31 hits. Root: `/` on Projects opens overlay; `/` with a Browser stacked reaches the browser filter instead.
- [ ] **Step 2: Verify failure.** Record.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Full suite foreground + lint/format/deadcode.**
- [ ] **Step 5: Commit**

```bash
git add internal/cli/dashboard
git commit -m "feat(dashboard): debounced global search overlay across projects"
```

---

### Task 16: Insights screen + Projects fleet header

Spec §9.

**Files:**
- Create: `internal/cli/dashboard/views/insights.go` (+ test)
- Modify: `views/browser.go` (`i`), `views/projects.go` + `dashboard.go` (fleet header line; Config gains `Version string`, wired from `cli.Version` in `internal/cli/dashboard.go`), `actions/actions.go`

**Interfaces:**

```go
type InsightsDeps struct {
	Folder   string
	Memories []memoryfs.Memory        // browser's current list (pass, don't re-walk)
	Lint     []lint.Result
	Data     HistoryDataSource         // folder-wide history for edit/machine stats
	Styles   theme.Styles
	Now      func() time.Time
}
func NewInsights(deps InsightsDeps) *Insights
// Sections (all computed in a Cmd from ONE folder-wide History(folder,"",500) call):
//   counts: memories per provider · total size (memoryfs sums)
//   last capture: newest capture-subject Timestamp
//   most edited: top 5 paths by version count (history Paths tally)
//   stalest: bottom 5 by ModTime (memoryfs)
//   lint summary: issue count per rule
//   machines: distinct history Hosts with per-host version counts
// History fetch failure renders the error inline; filesystem sections still
// show (the daemon being down must not blank local facts).
```

Fleet header (Projects tab, above the table — spec §9): `N units · watching M/N · last sync <outcome+relative> · vX.Y.Z` (version from Config; the "vs latest" comparison joins in Task 18 when the release check exists — leave a plain version until then, no placeholder text).

- [ ] **Step 1: Failing tests** — fixture with two providers + fake history (varied hosts/paths): every section's numbers exact in plain(view); history-error row keeps filesystem sections; fleet header string for a mixed watching/failed unit set.
- [ ] **Step 2: Verify failure.** Record.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Full suite foreground + lint/format/deadcode.**
- [ ] **Step 5: Commit**

```bash
git add internal/cli/dashboard
git commit -m "feat(dashboard): project insights and fleet header"
```

---

### Task 17: Conflict center — detail screen with edit/read actions

Spec §10.

**Files:**
- Create: `internal/cli/dashboard/views/conflictdetail.go` (+ test)
- Modify: `views/conflicts.go` (cursor + enter → detail; the flat list gains selection state), `dashboard.go`, `actions/actions.go`

**Interfaces:**

```go
type ConflictDetailDeps struct {
	Record   config.ConflictRecord // Path is repo-relative <folder>/<provider>/…
	Units    []api.UnitInfo
	Registry *provider.Registry
	ReadBody func(memoryfs.Memory) (string, error)
	Render   func(string, int) string
	Styles   theme.Styles
}
func NewConflictDetail(deps ConflictDetailDeps) *ConflictDetail
// Resolves Record.Path → folder + repoPath → memoryfs.LocalTarget → the
// live Memory (via a targeted memoryfs.List over the matching unit).
// Renders: event metadata (time/path/mode) + the CURRENT union-merged
// content (glamour). e → editRequestMsg{memory} (Task 13's flow — cleaning
// up a merge IS an edit); enter → push Reading. An unmapped path (unit
// untracked since) renders the metadata + "no longer tracked on this
// machine" and offers nothing.
```

- [ ] **Step 1: Failing tests** — conflicts list gains cursor (j/k move, plain(view) marks selection); enter pushes detail; detail renders metadata + content through Render; e emits editRequestMsg with the resolved memory; unmapped-path fixture shows the honest notice with no action rows.
- [ ] **Step 2: Verify failure.** Record.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Full suite foreground + lint/format/deadcode.**
- [ ] **Step 5: Commit**

```bash
git add internal/cli/dashboard
git commit -m "feat(dashboard): conflict detail with in-place cleanup via the edit flow"
```

---

### Task 18: Update banner + one-key self-update + re-exec

Spec §11's update half. The hub never talks to gh itself — closures from the cli root carry ADR 18's machinery.

**Files:**
- Modify: `internal/cli/dashboard/dashboard.go` (banner state, `U` confirm modal + progress, `R` outcome), `data.go`/Config, `actions/actions.go`
- Modify: `internal/cli/dashboard.go` + `internal/cli/hub.go` (closure composition; post-Run re-exec)
- Test: `dashboard_test.go`, `hub_test.go`

**Interfaces:**

```go
// dashboard.Config gains (Version string exists since Task 16 — the banner
// compares against it):
	// CheckUpdate returns the newer release tag, or "" when current. Wired
	// to selfupdate.Updater.Check with the same Options update.go builds;
	// errors are returned (the hub shows nothing on failure — the banner is
	// best-effort, never noise).
	CheckUpdate func(context.Context) (string, error)
	// ApplyUpdate runs Check→Apply→service restart for tag — the exact
	// runUpdate sequence minus its stdout prose; wired in cli via the
	// existing updateEngine seam + restartServiceForUpdate with an
	// io.Discard writer (their line-by-line reporting is CLI UX; the hub
	// reports via its modal).
	ApplyUpdate func(ctx context.Context, tag string) error
// Model: updateTag string (one CheckUpdate Cmd fired from Init AFTER the
// first successful statusMsg — "at most once per hub session", spec §11);
// banner in the status header: "vX.Y.Z available — U to update".
// U (only when updateTag != "") → confirm modal → applying state (status
// line "installing <tag>…", inputs ignored except ctrl+c) → ApplyUpdate Cmd:
//   error → toast verbatim (ErrBrewManaged/ErrDevBuild texts are already
//   self-remediating); success → banner swaps to "installed <tag> — R to
//   restart the hub on it (or restart manually)".
// R → m.reExec = true + tea.Quit. Exposed: func (m Model) ReExecRequested() bool.
// cli launchHub after program.Run():
//   if final, ok := finalModel.(dashboard.Model); ok && final.ReExecRequested() {
//       binary, err := resolveBinary(); …
//       return syscall.Exec(binary, os.Args, os.Environ()) // unix: replaces the process; hub restarts on the new binary
//   }
// (darwin/linux only — the supported matrix; comment it.)
```

- [ ] **Step 1: Failing tests** — CheckUpdate fires once (counting fake: two ticks, one call) and only after a status success; tag set → banner text in plain(view); U→confirm→y calls ApplyUpdate with the tag; apply error toasts it and clears the applying state; success shows the R offer; R → quitting model with ReExecRequested()==true; CheckUpdate error → no banner, no toast. hub_test: launchHub re-exec branch exercised via a seam (extract `maybeReExec(finalModel tea.Model, execFn func(string, []string, []string) error) error` — inject a recording execFn; production passes syscall.Exec).
- [ ] **Step 2: Verify failure.** Record.
- [ ] **Step 3: Implement** (registry row "update agent-brain", Mutates=false but availability = updateTag != "").
- [ ] **Step 4: Full suite foreground + lint/format/deadcode.**
- [ ] **Step 5: Commit**

```bash
git add internal/cli/dashboard internal/cli
git commit -m "feat(dashboard): update banner with one-key self-update and re-exec"
```

---

### Task 19: Doctor actions (`r` re-run, `f` fix, `s` scan) + scan results

Spec §11's doctor half + §12.

**Files:**
- Modify: `internal/cli/dashboard/views/doctorview.go` (keys, fixing/scanning states, scan results section), `data.go`/Config, `dashboard.go`, `actions/actions.go`
- Modify: `internal/cli/dashboard.go` (closures), `internal/cli/doctor.go` (extract the --fix quiesce orchestration into `runDoctorFixWithQuiesce(ctx context.Context, stderr io.Writer) (doctor.Report, error)` shared by the command and the closure — the command's RunE body shrinks to call it; behavior identical, its tests stay green)
- Modify: `internal/cli/scan.go` if the per-folder runner needs an exported-in-package helper (compose from `scanUnits` + `filterUnitsByFolder` — read them first)

**Interfaces:**

```go
// dashboard.Config gains:
	// RunDoctorFix is the quiesce-aware `doctor --fix` (quiesce best-effort →
	// doctor.Fix → resume) — cli's runDoctorFixWithQuiesce with stderr routed
	// to io.Discard (hub reports via the view).
	RunDoctorFix func(context.Context) (doctor.Report, error)
	// Scan runs the gitleaks scan for one folder ("" = every enrolled unit),
	// mapping cli scanFinding rows to the hub's ScanFinding. Advisory only —
	// never joins SafetyGate (spec §12).
	Scan func(ctx context.Context, folder string) ([]ScanFinding, error)
// dashboard (root package):
type ScanFinding struct {
	Folder string
	File   string // unit-relative path
	Rule   string // gitleaks rule id
	Line   int
}
// DoctorView: r → re-run (existing doctorCmd, now on demand); f → only when
// report.Failed() and a fixable row exists (Fix != ""): "fixing…" state →
// RunDoctorFix Cmd → re-render the returned report + toast "fix applied —
// re-checked"; s → "scanning…" → Scan("") → findings section under the
// checks ("N findings in M files — advisory, plaintext hygiene only" +
// per-file rows; zero → "no plaintext leaks found"). Errors render inline.
// Palette rows: doctor-fix (Mutates=true — it quiesces), scan (Mutates=false).
```

- [ ] **Step 1: Failing tests** — fakeData/Config closures recorded: r refetches; f gated (absent on a passing report), invokes the closure, renders the new report; s renders findings table + advisory wording, zero-case, error-case; palette dispatch reaches both. cli: `TestRunDoctorFixWithQuiesceQuiesces` — against the package's existing fake-daemon/client seams (mirror how doctor.go's current --fix path is tested; read doctor_test.go first) assert quiesce→fix→resume ordering survives the extraction.
- [ ] **Step 2: Verify failure.** Record.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Full suite foreground + lint/format/deadcode.**
- [ ] **Step 5: Commit**

```bash
git add internal/cli/dashboard internal/cli
git commit -m "feat(dashboard): one-key doctor fix and advisory gitleaks scan in the hub"
```

---

### Task 20: Enrollment parity — multi-select add + in-hub migrate

Spec §13: `a` becomes the full init-style multi-select enrollment; `m` drives the spec-§10 importer through the daemon.

**Files:**
- Modify: `internal/cli/dashboard/views/track.go` (picker stage → multi-select), `views/projects.go`, `data.go` (DataSource grows `Migrate(context.Context, api.MigrateRequest) (api.MigrateResponse, error)`), `dashboard.go`/Config, `actions/actions.go`
- Modify: `internal/cli/dashboard.go` (migrate closures)
- Test: `views/track_test.go` (extend), `views/projects_test.go`

**Interfaces:**

```go
// Multi-select add (parity with buildEnrollPickerForm's semantics, spec §13):
// addPicking gains selected map[int]bool; space toggles, enter confirms the
// SET (≥1 required; empty-set enter toasts "select at least one with space").
// Confirmed candidates queue; the existing per-candidate stages
// (path-confirm → identify → optional naming → track) run for each in order,
// with a progress line "enrolling 2 of 3: <label>". Row render: "[x] label"
// / "[ ] label" + cursor — same picker grammar as the cli MultiSelect.

// Migrate (m on Projects):
type MigrateCandidate struct {
	Provider  string // "claude" (the bash-era importer's domain)
	Slug      string // legacy slug (marker key)
	SeedDir   string // legacy tree to import
	PathGuess string // decoded project-path guess for the confirm input
	LiveDir   string // provider live dir for the guessed path (recomputed by
	                  // LiveDirFor after the user corrects the path)
}
// dashboard.Config gains:
	// LegacyDiscover enumerates un-imported bash-era stores — cli composes
	// legacyRoot + enumerateLegacySlugs + hasRealContent + the already-
	// migrated marker filter, EXACTLY as runMigrate's discovery does (read
	// runMigrate first; the closure reuses its helpers, never re-implements).
	LegacyDiscover func(context.Context) ([]MigrateCandidate, error)
	// LiveDirFor maps (provider, confirmed project path) → the live provider
	// dir to enroll (claude.MemoryDirFor via the cli's composition — the
	// dashboard tree does not import provider adapters).
	LiveDirFor func(providerName, projectPath string) (string, error)
	// MigratePreflight is runMigratePreflight bound to ambient config — the
	// chezmoi gate runs ONCE before the first migrate of a session.
	MigratePreflight func(context.Context) error
// Flow (modal machine mirroring add): m → preflight ("checking legacy
// store…"; failure toasts verbatim and aborts) → discovering → picking
// (single-select list of "slug → path guess") → path confirm (prefilled
// textinput) → identify (existing Identify closure) → optional naming →
// data.Migrate(api.MigrateRequest{Provider, ProjectID, PreferredFolder,
// LocalDir: LiveDirFor(...), Slug, SeedDir}) → toast
// "migrated <slug> → <folder> (<files> files)" (+ "already imported —
// enrolled only" when resp.Skipped) → fleet sync (the add flow's
// post-track idiom).
```

- [ ] **Step 1: Failing tests** — multi-select: space toggles render `[x]`; enter with none toasts; two selected → both enrolled in order (fakeData Track calls), progress line visible mid-queue; esc mid-queue abandons the REST with a toast naming what already enrolled (truthful partial outcome — same rule as onTrackResult's partial wording). Migrate: full happy path through fakes (preflight → discover → pick → confirm → identify → Migrate request field-exact via go-cmp); preflight failure aborts with its error; Skipped response wording; remoteless naming branch.
- [ ] **Step 2: Verify failure.** Record.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Full suite foreground + lint/format/deadcode.**
- [ ] **Step 5: Commit**

```bash
git add internal/cli/dashboard internal/cli/dashboard.go
git commit -m "feat(dashboard): multi-select enrollment and in-hub legacy migrate"
```

---

### Task 21: End-to-end acceptance battery

Spec §17's rows that only a real daemon + real git can prove. Uses the existing e2e harness (real binary from TestMain, bare fake remote, fake gh shim).

**Files:**
- Create: `test/e2e/hub_semantics_test.go`
- Modify: `test/e2e/scripts/bare_command.txt` only if Task 3 gaps emerged (otherwise untouched)

**Battery (each test follows the harness's provision-then-run idioms — read `sync_engine_test.go` + `rotate_test.go` first):**

```go
// TestEditRoundTripSemantics — provision a machine, enroll a claude unit,
// let the daemon capture a baseline. Then, exactly as the hub's write path
// does (renameio write-temp+rename into the provider dir — call the REAL
// memoryfs.WriteFileAtomic), land changed content; wait for the capture;
// assert EXACTLY ONE new commit on the remote whose subject matches
// `memory: <host> <folder> `, and ciphertext on the wire (agb1\x00
// discriminator, reusing assertNoPlaintextOnWire). Then write byte-identical
// content again: assert ZERO new commits after a full sync cycle (the
// no-op acceptance row).
// TestDeleteThenRestoreFromHistory — delete the provider file; wait for the
// deletion capture; query /v0/history folder-wide via the api client (UDS):
// the path appears in a version's Paths while absent on disk; fetch its
// pre-delete blob via /v0/blob; land it back through WriteFileAtomic;
// assert the restore capture exists and `git cat-file --textconv HEAD:<path>`
// on a fresh clone equals the restored content — restore grew history,
// never rewrote it (rev count strictly increases across all three phases).
// TestHistoryMatchesGitLog — after N edits, /v0/history for the path returns
// exactly the revs `git log --format=%H -- <path>` prints on the daemon's
// checkout, same order; each version's blob equals
// `git cat-file --textconv <rev>:<path>`.
```

- [ ] **Step 1: Write the tests (they are the spec — no implementation step; failures here are real bugs in Tasks 1–14).**
- [ ] **Step 2: Run** — `go test ./test/e2e/ -run 'TestEditRoundTrip|TestDeleteThenRestore|TestHistoryMatches' -v` FOREGROUND. Fix any surfaced defect in its owning package (each fix gets its own minimal commit referencing the failing test).
- [ ] **Step 3: Full suite foreground + lint/format/deadcode** → green.
- [ ] **Step 4: Commit**

```bash
git add test/e2e
git commit -m "test(e2e): hub capture, restore, and history equivalence battery"
```

---

### Task 22: Documentation truth pass

Ship the docs the behavior change demands (ADR 20 consequences list).

**Files:**
- Modify: `README.md` (bare command opens the hub; the full key/feature surface; scripting equivalents; `agent-brain dashboard` alias)
- Modify: `docs/00-design-spec.md` (§7's dashboard clause gains the pointer to `docs/01-dashboard-hub-spec.md` as the superseding surface — the note 01 already claims; make 00 agree)
- Modify: `docs/01-dashboard-hub-spec.md` §5 + `docs/decisions/20-adr-dashboard-hub.md` (the x/editor correction: v0.2.0 resolves `$EDITOR` only via `strings.Fields` with a nano fallback and declares module path `github.com/charmbracelet/x/editor`; resolution built in `editorx` instead — record it in ADR 20's Buy-vs-build with a dated verification line, and fix spec §5's parenthetical)
- Modify: `CLAUDE.md` (product-CLI paragraph: bare command = hub; dashboard subpackages; the two read-only endpoints in the daemon-API sentence)

- [ ] **Step 1: Write the edits.** Every claim checked against the shipped code (bidirectional: docs→code and the new surfaces→docs).
- [ ] **Step 2: `go test ./... ` unaffected — run lint + the docs' own proof:** grep the four files for "charm.land/x/editor" → only the corrected/annotated mentions remain.
- [ ] **Step 3: Commit**

```bash
git add README.md docs CLAUDE.md
git commit -m "docs: hub-era entry, endpoints, and editor-resolution corrections"
```

---

## FINAL WHOLE-BRANCH REVIEW GATE

After Task 22: run `scripts/review-package <pre-Task-1 base> HEAD`, dispatch the final whole-branch reviewer (most capable model) with the package + this plan's Global Constraints + the accumulated Minor-findings ledger. Fix waves per SDD rules (ONE fixer with the full findings list). Only then is the phase eligible for an rc tag (user-gated release, ADR 18 flow).

## Exit criteria (Phase 5 done means ALL of these)

1. Bare `agent-brain`: the full spec-§1 matrix proven (unit table + testscript); `agent-brain nonsense` still errors; `dashboard` alias unchanged.
2. `GET /v0/history` + `GET /v0/blob` live, engine-goroutine-serialized, quiesce-transparent for reads, statusError-mapped (400/413/415), peer-UID enforced like every route; `UnitInfo.RepoSubdir` populated.
3. Browser → reading → history drill-in works over the stack; glamour renders theme-aware; `[[links]]`, backlinks, dangling markers live.
4. Edit/new/rename/delete land atomically in provider dirs via scratch handoff; byte-equal save = zero commits + cancelled toast; each mutation = exactly one capture (e2e-proven); derived-class files refuse mutation honestly; no-editor state is visible, never a silent default.
5. Restore (live + deleted) creates a new capture whose content equals the chosen version; history equivalence vs `git log`/`cat-file --textconv` proven in e2e.
6. Search, lint (with `lint.stale_after_days`), insights, conflict detail, update banner + `U`/`R`, doctor `r`/`f`/`s`, scan, multi-select add, and in-hub migrate all reachable via keys AND palette, all registry-driven, mutations greyed while quiesced.
7. Suite green: `(ulimit -u 1400; go test ./... -race -count=1)`; golangci-lint + gofumpt clean; deadcode zero; no new `//nolint`; package-boundary greps clean (engine/daemon-api/doctor unchanged; dashboard-tree allowlist as specified).
8. Docs truthful (Task 22), including the ADR 20 x/editor correction.

## Decision record (planning-time, 2026-07-12)

- **Reads bypass quiesce, mutations don't** (Task 2): spec §15 greys mutations only; quiesce-window CLI surgery cannot corrupt log/cat-file reads, and the not-ready state gate covers re-clones. Pinned by `TestReadsAllowedWhileQuiesced`.
- **`Live` = content identity** (Task 1): blob-OID equality against HEAD — after a restore, source and restored both read live (truthful provenance).
- **`charm.land/x/editor` dropped** (Task 10): fails ADR 20 D2's own rules ($VISUAL, POSIX split, no silent default) and isn't on the vanity path. ~40 lines owned instead.
- **Diff via `github.com/aymanbagabas/go-udiff v0.4.1`** (Task 14): maintained gotextdiff port used in the charm ecosystem; hand-rolling LCS is exactly the wheel ADR 20's buy-list philosophy avoids.
- **No OSC52 copy** (Task 12): bubbletea v2.0.8 exposes clipboard reads only; `y` toasts the absolute path. Revisit if upstream adds a write API.
- **Frontmatter: line-based fence parse, no YAML dep** (Task 6): two advisory keys, malformation must degrade gracefully; matches the claude adapter's own precedent.
- **Views split with consumer-side interfaces** (Task 4): `views` defines the seams it consumes; root implements — no import cycles, spec §15's package shape honored.
