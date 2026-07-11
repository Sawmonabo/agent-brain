# Phantom Seam Wiring Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Resolve all 13 findings of the 2026-07-10 phantom review (`.superpowers/sdd/phantom-findings-2026-07-10.md`) by wiring every dormant seam into a real, used capability — not by deleting or merely documenting them — plus a recurrence gate.

**Architecture:** Six tasks. A installs a single-source-of-truth keymap in the dashboard (bubbles `key.Binding` matched by dispatch AND rendered by the footer). B gives the dead `dashboardData.Track` seam its consumer: an `a` (add) flow on the Projects tab — discovery closure injected from the cli root exactly like `runDoctor`, identity resolution via a second closure, the `named/` contract extracted to `provider.NamedIdentity` so cli and dashboard share one implementation. C makes `repo.Unit.ProjectID` load-bearing with a doctor check comparing local enrollment against the shared `.agent-brain/projects.toml`. D propagates `integrateOutcome.Offline` through `engine.Report` → `api.SyncSummary` → status/dashboard rendering. E sweeps the stale docs/comments (spec §8 tree, §7 flags, two doc comments, `GitMetaNames` unexport). F pins `deadcode` via a go.mod `tool` directive and adds a zero-baseline CI gate.

**Tech Stack:** Go 1.26, cobra, bubbletea v2 + bubbles v2 (`table`, `key`, `textinput`) + lipgloss v2 (dashboard package only), stdlib `testing` + go-cmp, real system git in engine tests, GitHub Actions.

## Global Constraints

Copied from `CLAUDE.md`, `docs/00-design-spec.md` §8, and standing session directives — every task's requirements implicitly include these:

- All work lands on `develop` (ADR 11). Never commit to `main`.
- Tests: stdlib `testing` + `go-cmp` ONLY (ADR 15). No assertion frameworks. Table-driven where multiple cases exist; `t.Parallel()`; `t.TempDir()`. Integration tests use real system git with a `git init --bare` fake remote.
- **Never point git filter/merge wiring at a test binary** (fork-bomb hazard, 2026-07-08). Run full suites in the foreground as `(ulimit -u 1400; go test ./... -race -count=1)` — never as background jobs.
- Package boundaries (spec §8): `engine` depends only on `gitx`/`crypto`/`provider`/`repo` interfaces — never on `cli` or `daemon`. `daemon/api` types are the only daemon↔CLI shared surface. `internal/cli/dashboard` (plus the `cli` root command that launches it) is the ONLY place allowed direct bubbletea/bubbles/lipgloss imports (ADR 05 amendment).
- CLI never writes inside the memories checkout. Task C's doctor check READS `.agent-brain/projects.toml` only — reads never violate the single-writer invariant (spec §5/§11, same posture as the conflict-log reader).
- The Tink keyset never enters any repo; plaintext memory content never reaches a git object. Nothing in this plan touches the crypto path — do not modify `internal/crypto`, filter wiring, or `.gitattributes` generation.
- Conventional Commits. `gofumpt -l -w .` clean. `golangci-lint run` clean — fix findings structurally, never add `//nolint` directives.
- Never cite PR numbers, reviewer names, review dates, or session artifacts in code comments or test names. Code comments explain WHY; commit messages carry context. (Referencing spec §N, ADR N, or package paths in comments is the repo's established convention and is required.)
- Docs ripple with the feature: a task that changes user-visible behavior updates spec §7 / README / CLAUDE.md in the SAME task, not in Task E (Task E owns only the pre-existing drift).
- New exported identifiers use full descriptive names (no abbreviations).

**Execution order:** Task A before Task B (B extends A's keymap and modal routing). Tasks C, D, E, F are independent of A/B and each other; execute in numbered order for a clean ledger.

**Verification commands used throughout** (from repo root `/Users/sawmonabo/dev/agent-brain`):

```bash
go build ./...
go test ./internal/cli/dashboard/ -race -count=1
go test ./internal/doctor/ -race -count=1
go test ./internal/engine/ -race -count=1
(ulimit -u 1400; go test ./... -race -count=1)   # full suite, foreground
golangci-lint run
gofumpt -l .
```

---

### Task A: Dashboard keymap contract (finding 2)

Replace the hardcoded footer string with a per-tab keymap: bubbles `key.Binding` values are the single source of truth — `handleKey`/`projectsView.update` MATCH against them and `footer()` RENDERS them, so "keys that work" and "keys we advertise" cannot drift again. The Projects-only keys (`s`, `t`) stop being advertised on Conflicts/Activity/Doctor, where they have always been silent no-ops.

**Files:**
- Create: `internal/cli/dashboard/keymap.go`
- Modify: `internal/cli/dashboard/dashboard.go` (`handleKey` at :291-339, `footer()` at :385-387)
- Modify: `internal/cli/dashboard/projects.go` (`update` at :119-159)
- Test: `internal/cli/dashboard/dashboard_test.go` (new footer/no-op tests)
- Modify: `README.md` (footer example line in the dashboard section, currently line ~98)

**Interfaces:**
- Consumes: `charm.land/bubbles/v2/key` — verified against the resolved module `bubbles/v2@v2.1.1`: `key.NewBinding(key.WithKeys(...), key.WithHelp(key, desc))`, `key.Matches[Key fmt.Stringer](k, b...) bool`, `binding.Help().Key` / `.Desc`.
- Produces: package-level `var dashboardKeys dashboardKeymap` with fields `TabSwitch, Select, Sync, Untrack, Quit key.Binding` and method `forTab(t tab) []key.Binding`. Task B adds an `Add key.Binding` field to this exact struct and widens the method to `forTab(t tab, addAvailable bool)` (updating `footer()`, its sole caller, in the same step).

- [ ] **Step A1: Write the failing footer test**

Append to `internal/cli/dashboard/dashboard_test.go`:

```go
func TestFooterAdvertisesOnlyActiveTabKeys(t *testing.T) {
	t.Parallel()
	m := New(Config{Data: &fakeData{}})

	m.active = tabProjects
	projectsFooter := plain(m.footer())
	for _, want := range []string{"tab/1–4 switch", "↑/↓ select", "s sync", "t untrack", "q quit"} {
		if !strings.Contains(projectsFooter, want) {
			t.Errorf("Projects footer %q missing %q", projectsFooter, want)
		}
	}

	for _, other := range []tab{tabConflicts, tabActivity, tabDoctor} {
		m.active = other
		otherFooter := plain(m.footer())
		if strings.Contains(otherFooter, "sync") || strings.Contains(otherFooter, "untrack") {
			t.Errorf("%s footer advertises Projects-only keys: %q", other.title(), otherFooter)
		}
		for _, want := range []string{"tab/1–4 switch", "q quit"} {
			if !strings.Contains(otherFooter, want) {
				t.Errorf("%s footer %q missing %q", other.title(), otherFooter, want)
			}
		}
	}
}

// TestProjectsKeysStayDeadOffProjectsTab pins the behavior the old footer
// lied about: s/t on a non-Projects tab dispatch nothing and mutate nothing.
func TestProjectsKeysStayDeadOffProjectsTab(t *testing.T) {
	t.Parallel()
	fake := &fakeData{}
	m := New(Config{Data: fake})
	m.active = tabConflicts

	m2, cmd := step(m, key("s"))
	if cmd != nil {
		t.Fatal("s on Conflicts produced a Cmd; want none")
	}
	_, cmd = step(m2, key("t"))
	if cmd != nil {
		t.Fatal("t on Conflicts produced a Cmd; want none")
	}
	if len(fake.syncCalls) != 0 || len(fake.untrackCalls) != 0 {
		t.Fatalf("keys off the Projects tab reached the daemon: sync=%v untrack=%v",
			fake.syncCalls, fake.untrackCalls)
	}
}
```

- [ ] **Step A2: Run the tests to verify they fail**

Run: `go test ./internal/cli/dashboard/ -run 'TestFooter|TestProjectsKeysStayDead' -v`
Expected: `TestFooterAdvertisesOnlyActiveTabKeys` FAILS — the current `footer()` returns the same `s sync · t untrack` string on every tab (and lacks `↑/↓ select`). `TestProjectsKeysStayDeadOffProjectsTab` already passes (the dispatch was always gated); it is the regression pin.

- [ ] **Step A3: Create the keymap**

Create `internal/cli/dashboard/keymap.go`:

```go
package dashboard

import "charm.land/bubbles/v2/key"

// dashboardKeymap is the dashboard's single keymap: every key the root reducer and
// the views dispatch, with the help text the footer advertises. handleKey and
// projectsView.update MATCH against these bindings and footer() RENDERS them
// through forTab — so an advertised key the active tab ignores is structurally
// impossible, and the inverse (a working key left unadvertised) is pinned by
// TestProjectsKeysStayDeadOffProjectsTab.
type dashboardKeymap struct {
	// TabSwitch bundles every tab-navigation key under one advertised hint.
	// handleKey matches the binding for membership, then picks the direction
	// from the concrete key — the binding stays the single gate for whether
	// the key does anything at all.
	TabSwitch key.Binding
	// Select advertises the bubbles table's own ↑/↓/k/j navigation on the
	// Projects tab; the table consumes the keys itself in update's
	// passthrough.
	Select  key.Binding
	Sync    key.Binding // Projects tab only
	Untrack key.Binding // Projects tab only
	Quit    key.Binding
}

// dashboardKeys is package-level because the keymap is static configuration —
// bindings never change at runtime; per-tab availability is forTab's job.
var dashboardKeys = dashboardKeymap{
	TabSwitch: key.NewBinding(
		key.WithKeys("tab", "shift+tab", "right", "left", "l", "h", "1", "2", "3", "4"),
		key.WithHelp("tab/1–4", "switch"),
	),
	Select:  key.NewBinding(key.WithKeys("up", "down", "k", "j"), key.WithHelp("↑/↓", "select")),
	Sync:    key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "sync")),
	Untrack: key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "untrack")),
	Quit:    key.NewBinding(key.WithKeys("q"), key.WithHelp("q", "quit")),
}

// forTab returns the bindings the footer advertises on t, in render order —
// mirroring the availability rule handleKey enforces by routing view keys
// to the Projects view only while it is active.
func (k dashboardKeymap) forTab(t tab) []key.Binding {
	bindings := []key.Binding{k.TabSwitch}
	if t == tabProjects {
		bindings = append(bindings, k.Select, k.Sync, k.Untrack)
	}
	return append(bindings, k.Quit)
}
```

- [ ] **Step A4: Render the footer from the keymap**

In `internal/cli/dashboard/dashboard.go`, replace:

```go
func (m Model) footer() string {
	return dimStyle.Render("tab/1–4 switch · s sync · t untrack · q quit")
}
```

with:

```go
// footer advertises exactly the keys that dispatch on the active tab,
// rendered from the same bindings handleKey matches (keymap.go).
func (m Model) footer() string {
	bindings := dashboardKeys.forTab(m.active)
	parts := make([]string, len(bindings))
	for i, binding := range bindings {
		help := binding.Help()
		parts[i] = help.Key + " " + help.Desc
	}
	return dimStyle.Render(strings.Join(parts, " · "))
}
```

- [ ] **Step A5: Dispatch through the keymap**

In `internal/cli/dashboard/dashboard.go`, add `"charm.land/bubbles/v2/key"` to the imports, then replace the global-keys switch inside `handleKey` (currently lines 319-332):

```go
	switch msg.String() {
	case "q":
		m.quitting = true
		return m, tea.Quit
	case "tab", "right", "l":
		m.active = (m.active + 1) % tabCount
		return m, m.switchCmd()
	case "shift+tab", "left", "h":
		m.active = (m.active + tabCount - 1) % tabCount
		return m, m.switchCmd()
	case "1", "2", "3", "4":
		m.active = tab(msg.String()[0] - '1')
		return m, m.switchCmd()
	}
```

with:

```go
	switch {
	case key.Matches(msg, dashboardKeys.Quit):
		m.quitting = true
		return m, tea.Quit
	case key.Matches(msg, dashboardKeys.TabSwitch):
		// The binding is the membership gate; the concrete key picks the
		// direction. "1"–"4" are the only single-rune members left after the
		// named cases, so the default is exact, not a catch-all.
		switch msg.String() {
		case "tab", "right", "l":
			m.active = (m.active + 1) % tabCount
		case "shift+tab", "left", "h":
			m.active = (m.active + tabCount - 1) % tabCount
		default:
			m.active = tab(msg.String()[0] - '1')
		}
		return m, m.switchCmd()
	}
```

In `internal/cli/dashboard/projects.go`, add `"charm.land/bubbles/v2/key"` to the imports and replace the two action cases in `update` (currently `case "s":` and `case "t":` inside the `switch msg.String()` at lines 137-154):

```go
	switch {
	case key.Matches(msg, dashboardKeys.Sync):
		unit, ok := v.selectedUnit()
		if !ok {
			return nil
		}
		v.notice = fmt.Sprintf("syncing %s…", unit.Folder)
		return syncCmd(data, unit.Folder)
	case key.Matches(msg, dashboardKeys.Untrack):
		unit, ok := v.selectedUnit()
		if !ok {
			return nil
		}
		v.confirming = true
		v.confirmUnit = unit
		v.notice = ""
		return nil
	}
```

The confirm-modal branch (`y`/`Y`/`n`/`N`/`esc` at lines 120-135) stays literal string matching — those keys are advertised inline by the `untrack %s? (y/N)` prompt itself, not by the footer, and are live only while the modal owns the keyboard.

- [ ] **Step A6: Run the tests to verify they pass**

Run: `go test ./internal/cli/dashboard/ -race -count=1`
Expected: PASS, including all pre-existing tests (`TestQuitKeys`, `TestTabCycling`, `TestProjectsSyncKey`, `TestProjectsUntrackToggleConfirmsThenCalls`, …) — the dispatch behavior is unchanged, only its source of truth moved.

- [ ] **Step A7: Mutation-proof the contract**

Temporarily remove `k.Sync,` from `forTab` — run `go test ./internal/cli/dashboard/ -run TestFooter -v` and confirm `TestFooterAdvertisesOnlyActiveTabKeys` FAILS (footer no longer advertises `s sync`). Restore the line byte-identical. This proves the test pins the keymap↔footer contract.

- [ ] **Step A8: Update the README footer example**

In `README.md`, the dashboard example block currently ends with the line:

```text
tab/1–4 switch · s sync · t untrack · q quit
```

Replace it with:

```text
tab/1–4 switch · ↑/↓ select · s sync · t untrack · q quit
```

(Task B updates this line again when it adds `a add` — each task keeps the README true at its own commit.)

- [ ] **Step A9: Lint, format, commit**

Run: `gofumpt -l -w . && golangci-lint run && go build ./...`
Expected: no output from gofumpt, no lint findings.

```bash
git add internal/cli/dashboard/keymap.go internal/cli/dashboard/dashboard.go internal/cli/dashboard/projects.go internal/cli/dashboard/dashboard_test.go README.md
git commit -m "fix(dashboard): derive the footer from the keymap so hints cannot drift from live keys"
```

---

### Task B: Dashboard Track action (finding 6)

Give `dashboardData.Track` its consumer: `a` (add) on the Projects tab discovers untracked memory roots, offers a picker, confirms the project path (prefilled from the lossy `PathGuess`), names remoteless projects with live validation, calls `data.Track`, then triggers a fleet sync and refresh — management parity with `agent-brain track` for single-root enrollment (repeat `a` to enroll more; the CLI picker remains the multi-select surface). Discovery and identity resolution are injected as closures from the cli root (the `offlineDoctorRunner` pattern), and the `named/` identity contract moves to `provider.NamedIdentity` so cli and dashboard share one implementation.

**Files:**
- Create: `internal/provider/named.go`
- Create: `internal/provider/named_test.go`
- Create: `internal/cli/dashboard/track.go`
- Modify: `internal/cli/dashboard/keymap.go` (add `Add` binding)
- Modify: `internal/cli/dashboard/dashboard.go` (Config, Model, message handling, modal routing)
- Modify: `internal/cli/dashboard/projects.go` (add-flow state machine, `update` signature)
- Modify: `internal/cli/enroll.go` (use `provider.NamedIdentity` at :161-163)
- Modify: `internal/cli/dashboard.go` (inject `Discover`/`Identify` closures)
- Test: `internal/cli/dashboard/projects_test.go`, `internal/cli/dashboard/dashboard_test.go`
- Modify: `docs/00-design-spec.md` (§7 dashboard bullet), `README.md` (dashboard section + footer example)

**Interfaces:**
- Consumes: `dashboardKeys`/`dashboardKeymap` from Task A; `dashboardData.Track(ctx, api.TrackRequest) (api.TrackResponse, error)` (already on the seam); `provider.Identity{ProjectID, PreferredFolder string}`; `provider.Discovered{LocalDir, RepoSubdir, Label, PathGuess string}`; `p.Identify(ctx, d provider.Discovered, projectPath string) (provider.Identity, error)`; cli's `buildTrackDeps()`, `buildEnrollCandidates(ctx, registry, enrolled)`, `enrolledSet(units)`; `repo.ValidateFolderName(name string) error`; `charm.land/bubbles/v2/textinput` — verified against `bubbles/v2@v2.1.1`: `textinput.New() Model`, `(*Model).SetValue(string)`, `(Model).Value() string`, `(*Model).Focus() tea.Cmd`, `(Model).Update(tea.Msg) (Model, tea.Cmd)`, `(Model).View() string`.
- Produces:
  - `provider.NamedIdentity(folderName string) Identity` — the shared `named/` contract.
  - Exported dashboard types the cli root constructs: `dashboard.TrackRoot{LocalDir, RepoSubdir string}`, `dashboard.TrackCandidate{Provider, Label, PathGuess string, Global bool, Roots []TrackRoot}`.
  - `dashboard.Config` gains `Discover func(context.Context) ([]TrackCandidate, error)` and `Identify func(ctx context.Context, providerName string, root TrackRoot, projectPath string) (provider.Identity, error)`.

- [ ] **Step B1: Write the failing provider.NamedIdentity test**

Create `internal/provider/named_test.go`:

```go
package provider_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/provider"
)

func TestNamedIdentity(t *testing.T) {
	t.Parallel()
	got := provider.NamedIdentity("notes")
	want := provider.Identity{ProjectID: "named/notes", PreferredFolder: "notes"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("NamedIdentity mismatch (-want +got):\n%s", diff)
	}
}

// TestNamedIdentityStaysTwoSegments pins the collision contract: a named id
// has exactly 2 slash-separated segments, so it can never collide with a
// remote-derived id (always ≥ 3 segments: host/owner/repo...). The folder
// name itself contains no "/" — repo.ValidateFolderName guarantees that at
// every prompt and the daemon re-checks it on Track.
func TestNamedIdentityStaysTwoSegments(t *testing.T) {
	t.Parallel()
	for _, folderName := range []string{"notes", "my-project", "a_b.c"} {
		id := provider.NamedIdentity(folderName).ProjectID
		if got := strings.Count(id, "/"); got != 1 {
			t.Errorf("NamedIdentity(%q).ProjectID = %q has %d slashes, want exactly 1", folderName, id, got)
		}
	}
}
```

- [ ] **Step B2: Run it to verify it fails**

Run: `go test ./internal/provider/ -run TestNamedIdentity -v`
Expected: FAIL — `undefined: provider.NamedIdentity`.

- [ ] **Step B3: Implement provider.NamedIdentity and rewire enrollOne**

Create `internal/provider/named.go`:

```go
package provider

// NamedIdentity is the identity for a per-project root that has no git
// remote: the human-chosen folder name under the reserved named/ namespace.
// It is THE contract shared by every enrollment surface (cli's enrollOne,
// the dashboard add flow), so the collision argument lives exactly once.
//
// named/<folderName> can never collide with a canonical remote-derived id:
// joinRemoteID (remote.go) requires a non-empty host AND a path containing
// "/", so every remote-derived id has at least 3 slash-separated segments
// (host/owner/repo, ...). named/<folderName> has exactly 2 — provided
// folderName itself is a single segment, which is what
// repo.ValidateFolderName's charset (no "/") guarantees at every prompt and
// what the daemon re-checks fail-closed on Track.
func NamedIdentity(folderName string) Identity {
	return Identity{ProjectID: "named/" + folderName, PreferredFolder: folderName}
}
```

In `internal/cli/enroll.go`, inside `enrollOne`, replace the inline assignment (currently lines ~151-163 — the big collision comment plus the two assignments):

```go
			// named/<folderName> can never collide with a canonical
			// remote-derived id: joinRemoteID (provider/remote.go) requires
			// a non-empty host AND a path containing "/", so every
			// remote-derived id has at least 3 slash-separated segments
			// (host/owner/repo, ...). named/<folderName> has exactly 2 —
			// provided folderName itself is a single segment, which is
			// exactly what repo.ValidateFolderName's charset (no "/")
			// guarantees at the prompt (nameRemotelessFolderInteractive)
			// and what the daemon re-checks fail-closed on Track.
			projectID = "named/" + folderName
			preferredFolder = folderName
```

with:

```go
			// The named/ shape and its collision-safety argument live in
			// provider.NamedIdentity — one contract for every enrollment
			// surface (this flow and the dashboard's add flow).
			named := provider.NamedIdentity(folderName)
			projectID, preferredFolder = named.ProjectID, named.PreferredFolder
```

- [ ] **Step B4: Run provider + cli tests to verify green**

Run: `go test ./internal/provider/ ./internal/cli/ -race -count=1`
Expected: PASS (enrollOne behavior is byte-identical; only the contract's home moved).

- [ ] **Step B5: Write the failing dashboard add-flow tests**

Append to `internal/cli/dashboard/projects_test.go`:

```go
// addConfig builds a Config whose discovery/identity closures return the
// given candidates/identity without any real provider composition.
func addConfig(fake *fakeData, candidates []TrackCandidate, identity provider.Identity, identifyErr error) Config {
	return Config{
		Data: fake,
		Discover: func(context.Context) ([]TrackCandidate, error) {
			return candidates, nil
		},
		Identify: func(_ context.Context, _ string, _ TrackRoot, _ string) (provider.Identity, error) {
			return identity, identifyErr
		},
	}
}

// drive feeds one message through the root model and executes any returned
// Cmd synchronously — flattening tea.Batch the way a running program would —
// feeding every produced message back in until the model goes quiet. It lets
// a test walk the full a → discover → pick → confirm → identify → track
// chain without a running program.
func drive(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	queue := []tea.Msg{msg}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		if next == nil {
			continue
		}
		if batch, ok := next.(tea.BatchMsg); ok {
			for _, cmd := range batch {
				if cmd != nil {
					queue = append(queue, cmd())
				}
			}
			continue
		}
		model, cmd := m.Update(next)
		m = model.(Model)
		if cmd != nil {
			queue = append(queue, cmd())
		}
	}
	return m
}

func TestProjectsAddDiscoverEmpty(t *testing.T) {
	t.Parallel()
	fake := &fakeData{}
	m := New(addConfig(fake, nil, provider.Identity{}, nil))
	m.active = tabProjects

	m = drive(t, m, key("a"))
	if got := plain(m.projects.view()); !strings.Contains(got, "no new memory roots") {
		t.Fatalf("empty discovery view = %q, want a 'no new memory roots' notice", got)
	}
	if len(fake.trackCalls) != 0 {
		t.Fatalf("empty discovery must not track: %v", fake.trackCalls)
	}
}

func TestProjectsAddGlobalTracksAllRootsAndSyncs(t *testing.T) {
	t.Parallel()
	fake := &fakeData{trackResp: api.TrackResponse{Folder: "_global"}}
	candidates := []TrackCandidate{{
		Provider: "codex",
		Label:    "codex  global memories",
		Global:   true,
		Roots: []TrackRoot{
			{LocalDir: "/home/u/.codex/memories"},
			{LocalDir: "/home/u/.codex/notes", RepoSubdir: "notes"},
		},
	}}
	m := New(addConfig(fake, candidates, provider.Identity{}, nil))
	m.active = tabProjects

	m = drive(t, m, key("a"))     // discover → picker (one row)
	m = drive(t, m, key("enter")) // global: track directly, then fleet sync

	if len(fake.trackCalls) != 2 {
		t.Fatalf("trackCalls = %d, want 2 (both roots of the grouped global candidate)", len(fake.trackCalls))
	}
	for i, call := range fake.trackCalls {
		if call.Provider != "codex" || call.ProjectID != "" {
			t.Fatalf("trackCalls[%d] = %+v, want codex with empty ProjectID (global scope)", i, call)
		}
	}
	if fake.trackCalls[1].RepoSubdir != "notes" {
		t.Fatalf("trackCalls[1].RepoSubdir = %q, want %q", fake.trackCalls[1].RepoSubdir, "notes")
	}
	if len(fake.syncCalls) != 1 || fake.syncCalls[0] != "" {
		t.Fatalf("syncCalls = %v, want one whole-fleet sync after a successful track", fake.syncCalls)
	}
}

func TestProjectsAddRemoteProjectFlow(t *testing.T) {
	t.Parallel()
	fake := &fakeData{trackResp: api.TrackResponse{Folder: "myrepo"}}
	candidates := []TrackCandidate{{
		Provider:  "claude",
		Label:     "claude  myrepo  → /g/myrepo",
		PathGuess: "/g/myrepo",
		Roots:     []TrackRoot{{LocalDir: "/home/u/.claude/projects/-g-myrepo/memory"}},
	}}
	identity := provider.Identity{ProjectID: "github.com/owner/myrepo", PreferredFolder: "myrepo"}
	m := New(addConfig(fake, candidates, identity, nil))
	m.active = tabProjects

	m = drive(t, m, key("a"))     // discover → picker
	m = drive(t, m, key("enter")) // pick → path-confirm input, prefilled with PathGuess
	if got := plain(m.projects.view()); !strings.Contains(got, "/g/myrepo") {
		t.Fatalf("path-confirm view = %q, want the PathGuess prefill visible", got)
	}
	m = drive(t, m, key("enter")) // accept path → identify → track → fleet sync

	if len(fake.trackCalls) != 1 {
		t.Fatalf("trackCalls = %v, want exactly one", fake.trackCalls)
	}
	call := fake.trackCalls[0]
	if call.ProjectID != "github.com/owner/myrepo" || call.PreferredFolder != "myrepo" ||
		call.LocalDir != "/home/u/.claude/projects/-g-myrepo/memory" {
		t.Fatalf("track request = %+v, want the identified project", call)
	}
}

func TestProjectsAddRemotelessNamesFolder(t *testing.T) {
	t.Parallel()
	fake := &fakeData{trackResp: api.TrackResponse{Folder: "scratch"}}
	candidates := []TrackCandidate{{
		Provider:  "claude",
		Label:     "claude  scratch  → /g/scratch",
		PathGuess: "/g/scratch",
		Roots:     []TrackRoot{{LocalDir: "/home/u/.claude/projects/-g-scratch/memory"}},
	}}
	// Identify resolves no remote: empty ProjectID, PreferredFolder as hint.
	identity := provider.Identity{PreferredFolder: "scratch"}
	m := New(addConfig(fake, candidates, identity, nil))
	m.active = tabProjects

	m = drive(t, m, key("a"))
	m = drive(t, m, key("enter")) // pick
	m = drive(t, m, key("enter")) // accept path → identify → remoteless → naming input

	// An invalid name must be refused locally (repo.ValidateFolderName),
	// before any wire call.
	m.projects.addInput.SetValue("bad/name")
	m = drive(t, m, key("enter"))
	if len(fake.trackCalls) != 0 {
		t.Fatalf("invalid folder name reached the daemon: %v", fake.trackCalls)
	}

	m.projects.addInput.SetValue("scratch")
	m = drive(t, m, key("enter"))
	if len(fake.trackCalls) != 1 {
		t.Fatalf("trackCalls = %v, want exactly one after a valid name", fake.trackCalls)
	}
	if got := fake.trackCalls[0].ProjectID; got != "named/scratch" {
		t.Fatalf("ProjectID = %q, want %q (provider.NamedIdentity contract)", got, "named/scratch")
	}
}

func TestProjectsAddEscCancelsEachStage(t *testing.T) {
	t.Parallel()
	fake := &fakeData{}
	candidates := []TrackCandidate{{
		Provider:  "claude",
		Label:     "claude  myrepo  → /g/myrepo",
		PathGuess: "/g/myrepo",
		Roots:     []TrackRoot{{LocalDir: "/x/memory"}},
	}}
	identity := provider.Identity{PreferredFolder: "myrepo"}

	// Stage 1: cancel from the picker.
	m := New(addConfig(fake, candidates, identity, nil))
	m.active = tabProjects
	m = drive(t, m, key("a"))
	m = drive(t, m, key("esc"))
	if m.projects.adding != addNone {
		t.Fatal("esc in the picker did not reset the add flow")
	}

	// Stage 2: cancel from the path confirm.
	m = drive(t, m, key("a"))
	m = drive(t, m, key("enter"))
	m = drive(t, m, key("esc"))
	if m.projects.adding != addNone {
		t.Fatal("esc in the path confirm did not reset the add flow")
	}

	// Stage 3: cancel from the folder naming input.
	m = drive(t, m, key("a"))
	m = drive(t, m, key("enter"))
	m = drive(t, m, key("enter"))
	m = drive(t, m, key("esc"))
	if m.projects.adding != addNone {
		t.Fatal("esc in the naming input did not reset the add flow")
	}
	if len(fake.trackCalls) != 0 {
		t.Fatalf("cancelled flows must never track: %v", fake.trackCalls)
	}
}

func TestFooterAdvertisesAddOnlyWhenWired(t *testing.T) {
	t.Parallel()
	wired := New(addConfig(&fakeData{}, nil, provider.Identity{}, nil))
	wired.active = tabProjects
	if got := plain(wired.footer()); !strings.Contains(got, "a add") {
		t.Fatalf("Projects footer %q missing %q with discovery wired", got, "a add")
	}
	wired.active = tabDoctor
	if got := plain(wired.footer()); strings.Contains(got, "a add") {
		t.Fatalf("Doctor footer %q must not advertise add", got)
	}
	unwired := New(Config{Data: &fakeData{}})
	unwired.active = tabProjects
	if got := plain(unwired.footer()); strings.Contains(got, "a add") {
		t.Fatalf("footer %q advertises add with no discovery closure wired", got)
	}
}
```

Add the imports the new tests need to `projects_test.go`'s import block if absent: `"context"`, `"strings"`, `tea "charm.land/bubbletea/v2"`, `"github.com/Sawmonabo/agent-brain/internal/daemon/api"`, `"github.com/Sawmonabo/agent-brain/internal/provider"`.

- [ ] **Step B6: Run them to verify they fail**

Run: `go test ./internal/cli/dashboard/ -run TestProjectsAdd -v`
Expected: FAIL to compile — `undefined: TrackCandidate`, `undefined: TrackRoot`, `Config` has no `Discover`/`Identify` fields, `m.projects.adding` undefined.

- [ ] **Step B7: Implement the add flow**

Create `internal/cli/dashboard/track.go`:

```go
package dashboard

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// TrackRoot is one memory root of a track candidate — the (LocalDir,
// RepoSubdir) pair a TrackRequest enrolls.
type TrackRoot struct {
	LocalDir   string
	RepoSubdir string
}

// TrackCandidate is one row the add picker offers: a discovered-but-
// unenrolled memory root, or — for a global-scope provider — ALL of its
// unenrolled roots grouped as one row, mirroring the cli enrollment picker's
// semantics (picking it enrolls them together under _global).
type TrackCandidate struct {
	Provider  string
	Label     string
	PathGuess string // per-project only: the adapter's lossy project-path guess
	Global    bool
	Roots     []TrackRoot // len ≥ 1; > 1 only for a grouped global candidate
}

// trackActions bundles the two closures the cli root injects (the
// offlineDoctorRunner pattern): discovery of untracked roots and identity
// resolution for a confirmed project path. The dashboard package cannot
// import cli, and providers/registry composition lives outside its import
// allowlist — the closures carry exactly the capability, nothing else.
type trackActions struct {
	discover func(context.Context) ([]TrackCandidate, error)
	identify func(ctx context.Context, providerName string, root TrackRoot, projectPath string) (provider.Identity, error)
}

// addStage is the add flow's modal state machine, owned by projectsView.
type addStage int

const (
	addNone addStage = iota
	addDiscovering
	addPicking
	addConfirmPath
	addIdentifying
	addNamingFolder
	addTracking
)

type (
	discoverMsg struct {
		candidates []TrackCandidate
		err        error
	}
	identifyMsg struct {
		identity provider.Identity
		err      error
	}
	trackResultMsg struct {
		folders []string
		err     error
	}
)

func discoverCmd(actions trackActions) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		candidates, err := actions.discover(ctx)
		return discoverMsg{candidates: candidates, err: err}
	}
}

func identifyCmd(actions trackActions, candidate TrackCandidate, projectPath string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		identity, err := actions.identify(ctx, candidate.Provider, candidate.Roots[0], projectPath)
		return identifyMsg{identity: identity, err: err}
	}
}

// trackCmd enrolls every root of the chosen candidate. A grouped global
// candidate is several TrackRequests by design (the daemon enrolls one root
// per request); the timeout scales so a slow daemon cannot strand a
// multi-root enrollment halfway through its budget.
func trackCmd(data dashboardData, candidate TrackCandidate, identity provider.Identity) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout*time.Duration(len(candidate.Roots)))
		defer cancel()
		folders := make([]string, 0, len(candidate.Roots))
		for _, root := range candidate.Roots {
			response, err := data.Track(ctx, api.TrackRequest{
				Provider:        candidate.Provider,
				ProjectID:       identity.ProjectID,
				PreferredFolder: identity.PreferredFolder,
				LocalDir:        root.LocalDir,
				RepoSubdir:      root.RepoSubdir,
			})
			if err != nil {
				return trackResultMsg{folders: folders, err: err}
			}
			folders = append(folders, response.Folder)
		}
		return trackResultMsg{folders: folders}
	}
}

// updateAdd routes keys while the add flow owns the keyboard. It returns
// (handled, cmd); handled is true whenever the flow is active so the caller
// swallows everything else, exactly like the untrack confirm.
func (v *projectsView) updateAdd(msg tea.KeyPressMsg, data dashboardData, actions trackActions) (bool, tea.Cmd) {
	if v.adding == addNone {
		return false, nil
	}
	if msg.String() == "esc" {
		v.resetAdd()
		v.notice = "add cancelled"
		return true, nil
	}
	switch v.adding {
	case addPicking:
		switch msg.String() {
		case "up", "k":
			if v.addCursor > 0 {
				v.addCursor--
			}
		case "down", "j":
			if v.addCursor < len(v.addCandidates)-1 {
				v.addCursor++
			}
		case "enter":
			choice := v.addCandidates[v.addCursor]
			v.addChoice = choice
			if choice.Global {
				v.adding = addTracking
				return true, trackCmd(data, choice, provider.Identity{})
			}
			v.adding = addConfirmPath
			v.addInput.SetValue(choice.PathGuess)
			return true, v.addInput.Focus()
		}
		return true, nil

	case addConfirmPath:
		if msg.String() == "enter" {
			projectPath := strings.TrimSpace(v.addInput.Value())
			if projectPath == "" {
				v.notice = "project path cannot be empty"
				return true, nil
			}
			v.adding = addIdentifying
			return true, identifyCmd(actions, v.addChoice, projectPath)
		}
		var cmd tea.Cmd
		v.addInput, cmd = v.addInput.Update(msg)
		return true, cmd

	case addNamingFolder:
		if msg.String() == "enter" {
			folderName := strings.TrimSpace(v.addInput.Value())
			// The daemon re-validates fail-closed on Track; validating here
			// too keeps a bad name a local correction, not a wire error.
			if err := repo.ValidateFolderName(folderName); err != nil {
				v.notice = err.Error()
				return true, nil
			}
			v.adding = addTracking
			return true, trackCmd(data, v.addChoice, provider.NamedIdentity(folderName))
		}
		var cmd tea.Cmd
		v.addInput, cmd = v.addInput.Update(msg)
		return true, cmd

	default: // addDiscovering, addIdentifying, addTracking: waiting on a Cmd
		return true, nil
	}
}

func (v *projectsView) resetAdd() {
	v.adding = addNone
	v.addCandidates = nil
	v.addCursor = 0
	v.addChoice = TrackCandidate{}
}

func (v *projectsView) onDiscover(msg discoverMsg) {
	if v.adding != addDiscovering {
		return // a stale answer after esc must not resurrect the flow
	}
	if msg.err != nil {
		v.resetAdd()
		v.notice = fmt.Sprintf("discover failed: %v", msg.err)
		return
	}
	if len(msg.candidates) == 0 {
		v.resetAdd()
		v.notice = "no new memory roots discovered"
		return
	}
	v.adding = addPicking
	v.addCandidates = msg.candidates
	v.addCursor = 0
}

// onIdentify advances the flow once identity resolution answers: a canonical
// id tracks immediately; an empty one (remoteless project) opens the folder
// naming input, prefilled with Identify's PreferredFolder — the same prefill
// contract the cli flow uses, since an accepted empty answer must be a value
// we are willing to enroll under.
func (v *projectsView) onIdentify(msg identifyMsg, data dashboardData) tea.Cmd {
	if v.adding != addIdentifying {
		return nil // a stale answer after esc must not resurrect the flow
	}
	if msg.err != nil {
		v.resetAdd()
		v.notice = fmt.Sprintf("identify failed: %v", msg.err)
		return nil
	}
	if msg.identity.ProjectID != "" {
		v.adding = addTracking
		return trackCmd(data, v.addChoice, msg.identity)
	}
	v.adding = addNamingFolder
	v.addInput.SetValue(msg.identity.PreferredFolder)
	return v.addInput.Focus()
}

func (v *projectsView) onTrackResult(msg trackResultMsg) {
	v.resetAdd()
	if msg.err != nil {
		v.notice = fmt.Sprintf("track failed: %v", msg.err)
		if len(msg.folders) > 0 {
			v.notice = fmt.Sprintf("track failed after enrolling %s: %v", strings.Join(msg.folders, ", "), msg.err)
		}
		return
	}
	v.notice = fmt.Sprintf("tracked %s — syncing…", strings.Join(msg.folders, ", "))
}

// addView renders the add flow in place of the projects table while active.
func (v projectsView) addView() string {
	var b strings.Builder
	switch v.adding {
	case addDiscovering:
		b.WriteString(dimStyle.Render("discovering memory roots…"))
	case addPicking:
		b.WriteString("Select a memory root to enroll\n\n")
		for i, candidate := range v.addCandidates {
			cursor := "  "
			if i == v.addCursor {
				cursor = "→ "
			}
			b.WriteString(cursor + candidate.Label + "\n")
		}
		b.WriteString("\n")
		b.WriteString(dimStyle.Render("↑/↓ move · enter select · esc cancel"))
	case addConfirmPath:
		b.WriteString("Confirm this project's path\n\n")
		b.WriteString(v.addInput.View())
		b.WriteString("\n\n")
		b.WriteString(dimStyle.Render("enter confirm · esc cancel"))
	case addIdentifying:
		b.WriteString(dimStyle.Render("resolving project identity…"))
	case addNamingFolder:
		b.WriteString("This project has no git remote — choose a folder name\n\n")
		b.WriteString(v.addInput.View())
		b.WriteString("\n\n")
		b.WriteString(dimStyle.Render("enter confirm · esc cancel"))
	case addTracking:
		b.WriteString(dimStyle.Render("enrolling…"))
	}
	if v.adding != addDiscovering && v.notice != "" {
		b.WriteString("\n")
		b.WriteString(warnStyle.Render(v.notice))
	}
	return b.String()
}
```

Note: `Focus()`'s Cmd emits cursor-blink messages that no `Update` case routes back into the input, so the modal renders a static (non-blinking) cursor. That is deliberate — routing blink would mean a `bubbles/cursor` import and a new message case for pure cosmetics. Do not add it.

- [ ] **Step B8: Wire the flow into the existing models**

In `internal/cli/dashboard/keymap.go`: add to `dashboardKeymap`:

```go
	Add     key.Binding // Projects tab only
```

add to `dashboardKeys`:

```go
	Add:     key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "add")),
```

and replace `forTab` wholesale (signature change: whether `a` answers depends on the injected closures, so the footer must gate on availability to keep advertising only keys that work):

```go
// forTab returns the bindings the footer advertises on t, in render order —
// the SAME availability rule handleKey enforces. addAvailable gates the Add
// binding: a build with no discovery closure must not advertise a key that
// answers "unavailable". (dashboardKeys is shared package state — never toggle
// availability by mutating a binding's Enabled flag; filter here instead.)
func (k dashboardKeymap) forTab(t tab, addAvailable bool) []key.Binding {
	bindings := []key.Binding{k.TabSwitch}
	if t == tabProjects {
		bindings = append(bindings, k.Select, k.Sync, k.Untrack)
		if addAvailable {
			bindings = append(bindings, k.Add)
		}
	}
	return append(bindings, k.Quit)
}
```

and update `footer()`'s first line (Task A wrote it as `dashboardKeys.forTab(m.active)`) to:

```go
	bindings := dashboardKeys.forTab(m.active, m.actions.discover != nil)
```

In `internal/cli/dashboard/projects.go`:

1. Add to the `projectsView` struct (after the `confirmUnit` field):

```go
	// Add flow (track.go): a modal state machine over discovery → picker →
	// path confirm → identity → optional naming → Track.
	adding        addStage
	addCandidates []TrackCandidate
	addCursor     int
	addChoice     TrackCandidate
	addInput      textinput.Model
```

2. In `newProjectsView`, initialize the input:

```go
	view.addInput = textinput.New()
```

3. Change `update`'s signature and head to route the add flow first:

```go
func (v *projectsView) update(msg tea.KeyPressMsg, data dashboardData, actions trackActions) tea.Cmd {
	if handled, cmd := v.updateAdd(msg, data, actions); handled {
		return cmd
	}
	if v.confirming {
```

4. Add the `a` case to the action switch (after the `Untrack` case):

```go
	case key.Matches(msg, dashboardKeys.Add):
		if actions.discover == nil {
			v.notice = "add is unavailable in this build"
			return nil
		}
		v.adding = addDiscovering
		v.notice = ""
		return discoverCmd(actions)
```

5. Replace `view()` wholesale: the add flow must own the body AHEAD of the `loadErr`/`!loaded`/empty guards (it must render on an empty, unloaded, or even load-errored fleet — an empty fleet is exactly when `a` matters most), and the confirm/notice trailer must render in EVERY non-add state so a post-cancel or empty-discovery notice is visible even when no table is on screen:

```go
func (v projectsView) view() string {
	var b strings.Builder
	b.WriteString(sectionTitle("Projects"))
	b.WriteString("\n\n")

	switch {
	case v.adding != addNone:
		b.WriteString(v.addView())
		return strings.TrimRight(b.String(), "\n")
	case v.loadErr != nil:
		fmt.Fprintf(&b, "projects unavailable: %v", v.loadErr)
	case !v.loaded:
		b.WriteString(dimStyle.Render("loading projects…"))
	case len(v.units) == 0:
		b.WriteString(dimStyle.Render("no projects enrolled — run `agent-brain track` or press a"))
	default:
		b.WriteString(v.table.View())
	}
	b.WriteString("\n")

	switch {
	case v.confirming:
		b.WriteString(warnStyle.Render(fmt.Sprintf("untrack %s? (y/N)", v.confirmUnit.Folder)))
	case v.notice != "":
		b.WriteString(dimStyle.Render(v.notice))
	}
	return strings.TrimRight(b.String(), "\n")
}
```

Keep the body copy identical to the current branches where they already exist (`projects unavailable`, `loading projects…`, the table) — the only new copy is the add-flow branch, the ` or press a` suffix on the empty state, and the trailer applying to all states. `TestProjectsEmptyState`'s "no projects enrolled" substring still matches.

6. Add the imports `"charm.land/bubbles/v2/textinput"` to `projects.go`.

In `internal/cli/dashboard/dashboard.go`:

1. Add to `Config`:

```go
	// Discover lists discovered-but-unenrolled memory roots; Identify
	// resolves a confirmed project path to its cross-machine identity. Both
	// are injected by the cli root command (the same composition-at-the-edge
	// pattern as the doctor runner) because provider/registry composition
	// lives outside this package's import allowlist. nil disables the
	// Projects tab's add action.
	Discover func(context.Context) ([]TrackCandidate, error)
	Identify func(ctx context.Context, providerName string, root TrackRoot, projectPath string) (provider.Identity, error)
```

2. Add `actions trackActions` to `Model` and populate it in `New`:

```go
		actions:      trackActions{discover: cfg.Discover, identify: cfg.Identify},
```

3. Update both `m.projects.update(...)` call sites (the confirm-modal guard and the active-view forwarding) to pass the actions:

```go
	if m.active == tabProjects && m.projects.modalOpen() {
		return m, m.projects.update(msg, m.data, m.actions)
	}
```

and

```go
	if m.active == tabProjects {
		return m, m.projects.update(msg, m.data, m.actions)
	}
```

4. Add the modal predicate to `projects.go`:

```go
// modalOpen reports whether a Projects-view modal (untrack confirm or the
// add flow) owns the keyboard: while true, the root must route keys here
// BEFORE its own tab/quit globals, so typing a path containing "1" or "q"
// edits the input instead of switching tabs or quitting.
func (v projectsView) modalOpen() bool {
	return v.confirming || v.adding != addNone
}
```

(The existing guard at dashboard.go:315 checks `m.projects.confirming` — replace that expression with `m.projects.modalOpen()` as shown above; the comment above it about `y`/`n` extends naturally to the add inputs.)

5. Add the three message cases to `Update` (after `untrackResultMsg`):

```go
	case discoverMsg:
		m.projects.onDiscover(msg)
		return m, nil

	case identifyMsg:
		return m, m.projects.onIdentify(msg, m.data)

	case trackResultMsg:
		failed := msg.err != nil
		m.projects.onTrackResult(msg)
		if failed {
			return m, m.projectsCmd()
		}
		// Track's HTTP reply returns BEFORE the daemon's post-admin cycle
		// (the same lesson track.go's syncAfterTrack records): an explicit
		// whole-fleet sync is what makes the enrollment's first mirror-in
		// visible here rather than landing silently later.
		return m, tea.Batch(m.projectsCmd(), m.statusCmd(), syncCmd(m.data, ""))
	}
```

6. In `projects.go`'s `onSyncResult`, render the whole-fleet label: replace the two `msg.folder` uses in notices with a resolved label —

```go
func (v *projectsView) onSyncResult(msg syncResultMsg) {
	label := msg.folder
	if label == "" {
		label = "fleet"
	}
	switch {
	case msg.err != nil:
		v.notice = fmt.Sprintf("sync %s failed: %v", label, msg.err)
	case msg.resp.Status == "running":
		v.notice = fmt.Sprintf("sync %s still running — check Activity", label)
	default:
		v.notice = fmt.Sprintf("synced %s", label)
	}
}
```

- [ ] **Step B9: Inject the closures from the cli root**

In `internal/cli/dashboard.go`, extend the `dashboard.Config` literal:

```go
			model := dashboard.New(dashboard.Config{
				Data:     dashboard.NewData(client, offlineDoctorRunner()),
				Discover: dashboardDiscover(),
				Identify: dashboardIdentify(),
```

and add at the bottom of the file:

```go
// dashboardDiscover mirrors track's discovery flow (runTrackDiscover): the
// same buildTrackDeps composition and buildEnrollCandidates filter, mapped to
// the dashboard's provider-name candidate shape — the dashboard package
// cannot import cli or compose providers itself (ADR 05 amendment). Deps are
// rebuilt per call so every `a` press sees the current registry and
// enrollment; a root tracked since the last press disappears from the offer.
func dashboardDiscover() func(context.Context) ([]dashboard.TrackCandidate, error) {
	return func(ctx context.Context) ([]dashboard.TrackCandidate, error) {
		deps, err := buildTrackDeps()
		if err != nil {
			return nil, err
		}
		local, err := repo.LoadLocalRegistry(deps.paths.LocalRegistryFile())
		if err != nil {
			return nil, err
		}
		candidates, err := buildEnrollCandidates(ctx, deps.registry, enrolledSet(local.Units))
		if err != nil {
			return nil, err
		}
		out := make([]dashboard.TrackCandidate, 0, len(candidates))
		for _, candidate := range candidates {
			roots := make([]dashboard.TrackRoot, len(candidate.discovered))
			for i, discovered := range candidate.discovered {
				roots[i] = dashboard.TrackRoot{LocalDir: discovered.LocalDir, RepoSubdir: discovered.RepoSubdir}
			}
			global := candidate.provider.Scope() == provider.ScopeGlobal
			pathGuess := ""
			if !global {
				pathGuess = candidate.discovered[0].PathGuess
			}
			out = append(out, dashboard.TrackCandidate{
				Provider:  candidate.provider.Name(),
				Label:     candidate.label,
				PathGuess: pathGuess,
				Global:    global,
				Roots:     roots,
			})
		}
		return out, nil
	}
}

// dashboardIdentify resolves one candidate root's cross-machine identity for
// a human-confirmed project path — the enrollOne Identify step, reached
// through the registry so the dashboard names providers by string only.
func dashboardIdentify() func(context.Context, string, dashboard.TrackRoot, string) (provider.Identity, error) {
	return func(ctx context.Context, providerName string, root dashboard.TrackRoot, projectPath string) (provider.Identity, error) {
		deps, err := buildTrackDeps()
		if err != nil {
			return provider.Identity{}, err
		}
		registered, ok := deps.registry.Get(providerName)
		if !ok {
			return provider.Identity{}, fmt.Errorf("provider %q is not registered", providerName)
		}
		discovered := provider.Discovered{LocalDir: root.LocalDir, RepoSubdir: root.RepoSubdir}
		return registered.Identify(ctx, discovered, projectPath)
	}
}
```

Add the needed imports to `internal/cli/dashboard.go`: `"fmt"`, `"github.com/Sawmonabo/agent-brain/internal/provider"`, `"github.com/Sawmonabo/agent-brain/internal/repo"`.

- [ ] **Step B10: Run the tests to verify they pass**

Run: `go test ./internal/cli/dashboard/ ./internal/cli/ ./internal/provider/ -race -count=1`
Expected: PASS — all new add-flow tests plus every pre-existing dashboard test (the `update` signature change touches the two root call sites only).

- [ ] **Step B11: Mutation-proof the shared contract**

Temporarily change `provider.NamedIdentity` to return `Identity{ProjectID: folderName, PreferredFolder: folderName}` (drop the prefix). Run: `go test ./internal/provider/ ./internal/cli/dashboard/ -run 'TestNamedIdentity|TestProjectsAddRemoteless' -v` — expect BOTH packages' tests to fail (proving cli and dashboard genuinely share the one contract). Restore byte-identical.

- [ ] **Step B12: Docs ripple**

1. `docs/00-design-spec.md` §7 dashboard bullet — replace:

```
  terminals ≥120 cols; `s` syncs the selected unit, `t` untracks it behind a y/N
  confirm), **Conflicts** (retained retain-both records via the `conflicts`
```

with:

```
  terminals ≥120 cols; `s` syncs the selected unit, `t` untracks it behind a y/N
  confirm, `a` discovers untracked memory roots and enrolls one — path confirm,
  remoteless naming, and the `named/` contract shared with `track` via
  `provider.NamedIdentity`), **Conflicts** (retained retain-both records via the `conflicts`
```

2. `README.md` dashboard section — in the prose listing the keys, insert `` `a` adds (tracks) a discovered memory root, `` after the untrack clause. CAUTION: that prose wraps across two physical source lines ("…`t` untracks it behind a" / "`y/N` confirm…") — match the wrapped form, not one long line. Update the example footer line to:

```text
tab/1–4 switch · ↑/↓ select · s sync · t untrack · a add · q quit
```

3. `/Users/sawmonabo/dev/agent-brain/CLAUDE.md` — in the product-CLI sentence, change "`dashboard` (bubbletea v2 TUI over the daemon" to "`dashboard` (bubbletea v2 TUI over the daemon — in-TUI track/untrack/sync".

- [ ] **Step B13: Full suite, lint, commit**

Run: `(ulimit -u 1400; go test ./... -race -count=1)` then `gofumpt -l -w . && golangci-lint run`
Expected: full suite PASS, no lint findings.

```bash
git add internal/provider/named.go internal/provider/named_test.go internal/cli/dashboard/ internal/cli/enroll.go internal/cli/dashboard.go docs/00-design-spec.md README.md CLAUDE.md
git commit -m "feat(dashboard): add-to-track flow — a key discovers and enrolls memory roots

Gives dashboardData.Track its consumer: discovery/identity closures injected
from the cli root, the named/ identity contract extracted to
provider.NamedIdentity and shared with enrollOne, path confirm + remoteless
naming as bubbletea inputs, and a post-track fleet sync mirroring track's
own visibility lesson.

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task C: Doctor check — project-identity mapping (finding 5)

Make `repo.Unit.ProjectID` load-bearing: a new advisory doctor check compares every per-project enrollment against the shared `.agent-brain/projects.toml` in the memories checkout and warns when a folder's registered canonical id no longer matches what this machine enrolled under. Battery goes 18 → 19 checks.

**Files:**
- Modify: `internal/doctor/checks.go` (new `checkProjectIdentity`, after `checkRegistryLocal` at :367)
- Modify: `internal/doctor/doctor.go` (battery slice + battery-order doc comment at :138-160)
- Modify: `internal/repo/local.go` (`Unit.ProjectID` doc comment at :20)
- Modify: `internal/cli/scan.go` (stale field comment at :137, conditional — see step C6)
- Create: `docs/plans/backlog-project-identity-engine-guard.md` (durable engine-guard follow-up — see step C7)
- Test: `internal/doctor/checks_test.go`

**Interfaces:**
- Consumes: `doctor.Deps.Enrolled []repo.Unit` (already populated by `buildDoctorDeps`, `internal/cli/doctor.go:165`); `repo.LoadProjects(path) (*repo.Projects, error)`; `repo.NewLayout(deps.Paths.MemoriesDir()).ProjectsFile()`; `repo.Projects.Entries map[string]repo.ProjectEntry` (`ProjectEntry.ID string`).
- Produces: check name `"project-identity"` in `Report.Results`, `StatusOK`/`StatusWarn` only (advisory — never `StatusFail`, never in `SafetyGate`).

- [ ] **Step C1: Write the failing tests**

Append to `internal/doctor/checks_test.go` (external test package `doctor_test`; reuse the existing `result(t, report, name)` helper and `minimalDeps` style):

```go
// writeProjectsRegistry drops a projects.toml into deps' memories checkout
// path so checkProjectIdentity has a shared registry to compare against.
func writeProjectsRegistry(t *testing.T, deps doctor.Deps, body string) {
	t.Helper()
	path := repo.NewLayout(deps.Paths.MemoriesDir()).ProjectsFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProjectIdentityMatches(t *testing.T) {
	t.Parallel()
	deps := minimalDeps(t)
	deps.Enrolled = []repo.Unit{{Provider: "claude", Folder: "myrepo", LocalDir: "/x", ProjectID: "github.com/owner/myrepo"}}
	writeProjectsRegistry(t, deps, "version = 1\n\n[projects.myrepo]\nid = \"github.com/owner/myrepo\"\n")

	got := result(t, doctor.Run(context.Background(), deps), "project-identity")
	if got.Status != doctor.StatusOK {
		t.Fatalf("project-identity = %+v, want ok", got)
	}
}

func TestProjectIdentityDriftedMapping(t *testing.T) {
	t.Parallel()
	deps := minimalDeps(t)
	deps.Enrolled = []repo.Unit{{Provider: "claude", Folder: "myrepo", LocalDir: "/x", ProjectID: "github.com/owner/myrepo"}}
	writeProjectsRegistry(t, deps, "version = 1\n\n[projects.myrepo]\nid = \"github.com/other/myrepo\"\n")

	got := result(t, doctor.Run(context.Background(), deps), "project-identity")
	if got.Status != doctor.StatusWarn {
		t.Fatalf("project-identity = %+v, want warn on a reassigned folder", got)
	}
	for _, want := range []string{"github.com/owner/myrepo", "github.com/other/myrepo", "myrepo", "crosses projects"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("Detail %q missing %q", got.Detail, want)
		}
	}
	if !strings.Contains(got.Fix, "untrack") {
		t.Errorf("Fix %q must name the untrack/re-track remediation", got.Fix)
	}
}

func TestProjectIdentityFolderMissingFromRegistry(t *testing.T) {
	t.Parallel()
	deps := minimalDeps(t)
	deps.Enrolled = []repo.Unit{{Provider: "claude", Folder: "myrepo", LocalDir: "/x", ProjectID: "github.com/owner/myrepo"}}
	writeProjectsRegistry(t, deps, "version = 1\n")

	got := result(t, doctor.Run(context.Background(), deps), "project-identity")
	if got.Status != doctor.StatusWarn {
		t.Fatalf("project-identity = %+v, want warn when the folder vanished from the registry", got)
	}
	if !strings.Contains(got.Detail, "missing") {
		t.Errorf("Detail %q should say the folder is missing from the shared registry", got.Detail)
	}
}

func TestProjectIdentityUnreadableRegistryWarns(t *testing.T) {
	t.Parallel()
	deps := minimalDeps(t)
	deps.Enrolled = []repo.Unit{{Provider: "claude", Folder: "myrepo", LocalDir: "/x", ProjectID: "github.com/owner/myrepo"}}
	writeProjectsRegistry(t, deps, "version = 99\n") // unsupported version → LoadProjects error

	got := result(t, doctor.Run(context.Background(), deps), "project-identity")
	if got.Status != doctor.StatusWarn {
		t.Fatalf("project-identity = %+v, want warn on an unreadable registry", got)
	}
}

// TestProjectIdentitySkipsWithoutPerProjectUnits: global-scope units carry no
// ProjectID, so the check must not apply at all (mirrors the prereq checks'
// enrolled-scoping) — absent from the report, not a vacuous ok.
func TestProjectIdentitySkipsWithoutPerProjectUnits(t *testing.T) {
	t.Parallel()
	deps := minimalDeps(t)
	deps.Enrolled = []repo.Unit{{Provider: "codex", Folder: "_global", LocalDir: "/y"}}

	report := doctor.Run(context.Background(), deps)
	for _, res := range report.Results {
		if res.Name == "project-identity" {
			t.Fatalf("project-identity should be inapplicable with no per-project units, got %+v", res)
		}
	}
}
```

If `checks_test.go`'s import block lacks any of `"strings"`, `"os"`, `"path/filepath"`, `"github.com/Sawmonabo/agent-brain/internal/repo"`, add them.

- [ ] **Step C2: Run them to verify they fail**

Run: `go test ./internal/doctor/ -run TestProjectIdentity -v`
Expected: FAIL — every test's `result(...)` lookup finds no `"project-identity"` check (the skip test passes vacuously; the other four fail).

- [ ] **Step C3: Implement the check**

In `internal/doctor/checks.go`, insert after `checkRegistryLocal` (`checks.go` already imports `slices` — no new imports needed):

```go
// checkProjectIdentity verifies the cross-machine linchpin (spec §3): every
// per-project unit this machine enrolled must still be the project the
// SHARED registry (.agent-brain/projects.toml in the memories checkout) maps
// its folder to. The drift is real: machine A `untrack --purge`s folder F
// (the last tracker deletes the registry row), machine C later tracks a
// DIFFERENT project whose preferred folder lands back on F — a stale machine
// still enrolled under F would from then on mirror its memories into a
// folder the fleet has reassigned. Unit.ProjectID is recorded at Track
// exactly so this comparison needs no lossy re-derivation (slug reversal)
// and no network: local enrollment vs the checkout's registry file.
//
// Advisory (StatusWarn), deliberately NOT in SafetyGate: the gate's
// membership rule (gate.go) blocks the WHOLE fleet's cycles, and one
// drifted folder does not make the other units' cycles unsafe. Per-unit
// engine withholding on drift is recorded follow-up work in
// docs/plans/backlog-project-identity-engine-guard.md, not silently skipped.
func checkProjectIdentity(_ context.Context, deps Deps) (CheckResult, bool) {
	const name = "project-identity"
	perProject := make([]repo.Unit, 0, len(deps.Enrolled))
	for _, unit := range deps.Enrolled {
		if unit.ProjectID != "" { // global-scope units carry no project identity
			perProject = append(perProject, unit)
		}
	}
	if len(perProject) == 0 {
		return CheckResult{}, false
	}
	projects, err := repo.LoadProjects(repo.NewLayout(deps.Paths.MemoriesDir()).ProjectsFile())
	if err != nil {
		return CheckResult{
			Name: name, Status: StatusWarn,
			Detail: "cannot read the shared project registry: " + err.Error(),
			Fix:    "run `agent-brain sync`, then `agent-brain doctor` again",
		}, true
	}
	var drifted []string
	for _, unit := range perProject {
		entry, ok := projects.Entries[unit.Folder]
		switch {
		case !ok:
			drifted = append(drifted, fmt.Sprintf("%s (%s): folder missing from the shared registry", unit.Folder, unit.Provider))
		case entry.ID != unit.ProjectID:
			drifted = append(drifted, fmt.Sprintf("%s (%s): registry maps it to %q, this machine enrolled %q — mirroring crosses projects until re-tracked",
				unit.Folder, unit.Provider, entry.ID, unit.ProjectID))
		}
	}
	if len(drifted) > 0 {
		slices.Sort(drifted)
		return CheckResult{
			Name: name, Status: StatusWarn,
			Detail: "project identity drift: " + strings.Join(drifted, "; "),
			Fix:    "untrack the listed folders and re-track their local dirs (`agent-brain untrack <folder>`, then `agent-brain track <path>`)",
		}, true
	}
	return CheckResult{
		Name: name, Status: StatusOK,
		Detail: fmt.Sprintf("%d enrolled folder(s) match the shared registry", len(perProject)),
	}, true
}
```

In `internal/doctor/doctor.go`, insert `checkProjectIdentity,` into `battery` immediately after `checkRegistryLocal,`, and update the battery-order doc comment's list from

```
// dashboard renders it (spec: settings · keyset · checkout · filters ·
// attributes · git-meta · credential-helper · remote · gh · daemon ·
// service · registry-local · conflict-log · claude-prereqs ·
// codex-prereqs · legacy-leftovers · secrets-scan · keyset-decrypt).
```

to

```
// dashboard renders it (spec: settings · keyset · checkout · filters ·
// attributes · git-meta · credential-helper · remote · gh · daemon ·
// service · registry-local · project-identity · conflict-log ·
// claude-prereqs · codex-prereqs · legacy-leftovers · secrets-scan ·
// keyset-decrypt).
```

- [ ] **Step C4: Run the tests to verify they pass**

Run: `go test ./internal/doctor/ -race -count=1`
Expected: PASS — all five new tests plus the whole existing doctor suite (no existing test enumerates check names or counts; verified 2026-07-10).

- [ ] **Step C5: Document the field's consumer**

In `internal/repo/local.go`, replace line 20:

```go
	ProjectID string `toml:"project_id"` // canonical id; empty for global scope
```

with:

```go
	// ProjectID is the canonical machine-independent id this machine
	// enrolled under (empty for global scope). Written by the daemon's
	// Track; read back by doctor's project-identity check, which compares
	// it against the shared registry's current mapping for Folder to catch
	// cross-machine folder reassignment.
	ProjectID string `toml:"project_id"`
```

- [ ] **Step C6: True up the scan.go field comment (conditional)**

Run: `grep -n "ProjectID" internal/cli/scan.go` and read the surrounding comment (around :137). If it still groups `ProjectID` with `RepoSubdir` as fields carried "for toml tags only" / not consumed, extend that sentence with: `(ProjectID is consumed by doctor's project-identity check; RepoSubdir by the engine's path mapping)`. If the comment already reads accurately, change nothing and note that in the task report.

- [ ] **Step C7: Record the engine-guard follow-up durably**

Create `docs/plans/backlog-project-identity-engine-guard.md` with exactly this content:

```markdown
# Backlog: engine-level withholding on project-identity drift

Doctor's `project-identity` check (advisory) detects the drift; it does not
stop it — a headless machine never runs the battery and keeps mirroring
bidirectionally into a folder the fleet reassigned (cross-project
contamination) until someone runs `doctor` or opens the dashboard.

The stronger guarantee is engine-side: verify `unit.ProjectID` against the
shared registry POST-INTEGRATE each cycle (the reassignment arrives in the
same fetch) and withhold that unit's mirror-in/mirror-out as a new degrade
class. That is a spec §4/§5 semantics change (a per-cycle registry read and
new degrade vocabulary) and needs its own ADR + design pass — do not bolt
it onto another wave.
```

- [ ] **Step C8: Full suite, lint, commit**

Run: `(ulimit -u 1400; go test ./... -race -count=1)` then `gofumpt -l -w . && golangci-lint run`
Expected: PASS / clean. (The e2e doctor flows assert specific checks' outcomes, not the battery size; a new advisory check must not break them — if any txtar golden fails, STOP and report, do not edit goldens to force green.)

```bash
git add internal/doctor/checks.go internal/doctor/doctor.go internal/doctor/checks_test.go internal/repo/local.go internal/cli/scan.go docs/plans/backlog-project-identity-engine-guard.md
git commit -m "feat(doctor): project-identity check — enrolled folders must match the shared registry

Makes Unit.ProjectID load-bearing: compares each per-project enrollment
against .agent-brain/projects.toml to catch cross-machine folder
reassignment (purge-then-reuse). Advisory by the SafetyGate membership
rule; per-unit engine withholding is recorded follow-up work.

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task D: Offline cycle telemetry (finding 7)

Propagate `integrateOutcome.Offline` (set on fetch failure, currently collapsed into `!Integrated`) through `engine.Report` → `api.SyncSummary` → every human surface: `status`, the dashboard header, and the Activity view render **offline** distinctly from **degraded** — offline on a laptop is benign; degraded means conflicts need attention.

**Files:**
- Modify: `internal/engine/engine.go` (`Report` struct at :76-87)
- Modify: `internal/engine/sync.go` (capture at the `integrate` call, :62-65)
- Modify: `internal/daemon/api/types.go` (`SyncSummary` at :15-28)
- Modify: `internal/daemon/daemon.go` (`toSummary` at :688-699)
- Modify: `internal/cli/dashboard/dashboard.go` (`lastCycle` at :426-440)
- Modify: `internal/cli/dashboard/activity.go` (`writeSyncSummary` at :74+)
- Modify: `internal/cli/client_commands.go` (`printSummary` at :104-130)
- Test: `internal/engine/sync_test.go`, `internal/daemon/server_test.go` (internal `package daemon` — see D4), `internal/cli/dashboard/dashboard_test.go` (`TestLastCycle` table), `internal/cli/dashboard/activity_test.go`, `internal/cli/client_commands_test.go` (exists — append)

**Interfaces:**
- Consumes: `integrateOutcome.Offline bool` (`internal/engine/integrate.go:33`, set at :51).
- Produces: `engine.Report.Offline bool`; `api.SyncSummary.Offline bool \`json:"offline,omitempty"\`` (strictly additive — absent/false from older daemons, ignored by older clients); header state string `"offline"`.

- [ ] **Step D1: Write the failing engine test**

Append to `internal/engine/sync_test.go` (same fixture vocabulary as `TestSyncFullCycleLocalToRemote` and `TestIntegrateOfflineIsNotAnError`):

```go
func TestSyncOfflineCycleReportsOffline(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	// The vanished-remote trick from TestIntegrateOfflineIsNotAnError, one
	// level up: the whole cycle must classify the fetch failure as offline.
	mustGit(t, checkout, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "vanished.git"))
	u := unit(t, "alpha")
	writeLocal(t, u, "memories/fact.md", "a fact\n")

	report, err := engine.Sync(context.Background(), []repo.Unit{u})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Offline {
		t.Fatalf("report = %+v, want Offline=true on a fetch failure", report)
	}
	if report.Pushed {
		t.Fatal("an offline cycle cannot have pushed")
	}
	if !report.PushQueued {
		t.Fatal("an offline cycle that committed local work must queue the push")
	}
	if len(report.Degraded) != 0 {
		t.Fatalf("offline is not degraded: %v", report.Degraded)
	}
}
```

- [ ] **Step D2: Run it to verify it fails**

Run: `go test ./internal/engine/ -run TestSyncOfflineCycleReportsOffline -v`
Expected: FAIL to compile — `report.Offline` undefined.

- [ ] **Step D3: Thread the field through engine, api, daemon**

1. `internal/engine/engine.go` — add to `Report` after `Degraded`:

```go
	// Offline reports a fetch failure this cycle: the remote was
	// unreachable, integrate was skipped, and any local commits were queued
	// (PushQueued) rather than pushed. Distinct from Degraded on purpose —
	// offline is the benign off-network axis; degraded means a folder was
	// withheld over a merge/conflict failure and needs a human look.
	Offline bool
```

2. `internal/engine/sync.go` — immediately after the `integrate` call's error check (`integ, err := e.integrate(ctx); if err != nil { return report, err }`), add:

```go
	report.Offline = integ.Offline
```

3. `internal/daemon/api/types.go` — add to `SyncSummary` after `Degraded`:

```go
	// Offline means this cycle could not reach the remote (fetch failed):
	// integrate was skipped and any local commits were queued. Additive
	// field — absent/false from older daemons.
	Offline bool `json:"offline,omitempty"`
```

4. `internal/daemon/daemon.go` — in `toSummary`, add `Offline: report.Offline,` after the `Degraded:` line.

Per-unit outcomes (`recordOutcome`) are deliberately unchanged: offline is a fleet-level axis (one fetch per cycle), and a unit whose local mirror-in/commit succeeded is honestly `"ok"` — its queued push is already visible via `PushQueued`. Add no `"offline"` value to `UnitCycleResult.Outcome`'s vocabulary.

- [ ] **Step D4: Write the failing daemon mapping test**

Append to `internal/daemon/server_test.go`. CAUTION: `internal/daemon/daemon_test.go` is the EXTERNAL `package daemon_test` and cannot see the unexported `toSummary` — the internal (`package daemon`) test files are `logging_test.go`, `quiesce_test.go`, `server_test.go`, `telemetry_test.go`, `watch_rebuild_test.go`; use `server_test.go` (add the `engine` import if it is not already there):

```go
func TestToSummaryCarriesOffline(t *testing.T) {
	t.Parallel()
	summary := toSummary(engine.Report{Offline: true, PushQueued: true})
	if !summary.Offline || !summary.PushQueued {
		t.Fatalf("summary = %+v, want Offline and PushQueued carried through", summary)
	}
}
```

Run: `go test ./internal/daemon/ -run TestToSummaryCarriesOffline -v` — expected PASS immediately after D3 (this pins the mapping against future field additions being forgotten; if `toSummary` had been left unmapped it would fail).

- [ ] **Step D5: Render offline on all three human surfaces**

1. `internal/cli/dashboard/dashboard.go` — in `lastCycle`, insert the offline case directly after the `Scrubbed` case, NOT after `Error`. `Degraded` is provably empty on an offline cycle, but `Scrubbed` is not: `prepareCheckout` scrubs BEFORE integrate (`sync.go:31-35`), so an offline cycle can carry the repo's loudest security signal, and the benign state must never mask it. Resulting precedence: `error > degraded > scrubbed > offline > ok`:

```go
	case len(status.LastSync.Scrubbed) > 0:
		return "scrubbed"
	case status.LastSync.Offline:
		return "offline"
```

(The `Scrubbed` case already exists — add ONLY the `Offline` case, placed immediately after it.)

2. `internal/cli/dashboard/activity.go` — in `writeSyncSummary`, after the `summary.Error` block:

```go
	if summary.Offline {
		b.WriteString("  offline: remote unreachable this cycle — local commits queued\n")
	}
```

3. `internal/cli/client_commands.go` — in `printSummary`, after the `summary.Error` block:

```go
	if summary.Offline {
		report.println("  offline: remote unreachable this cycle — local commits queued")
	}
```

(`writeSyncSummary`'s doc comment says it mirrors `printSummary` — keep the two lines textually identical.)

- [ ] **Step D6: Write the failing render tests**

1. `internal/cli/dashboard/dashboard_test.go` — extend the `TestLastCycle` table (at :291) with three cases:

```go
		{name: "offline", status: api.StatusResponse{LastSync: &api.SyncSummary{Offline: true}}, want: "offline"},
		{name: "error outranks offline", status: api.StatusResponse{LastSync: &api.SyncSummary{Offline: true, Error: "boom"}}, want: "error"},
		{name: "scrubbed outranks offline", status: api.StatusResponse{LastSync: &api.SyncSummary{Offline: true, Scrubbed: []string{"x"}}}, want: "scrubbed"},
```

(Match the table's actual field names when editing — the implementer adapts to the existing struct literal shape in that test.)

2. `internal/cli/dashboard/activity_test.go` — add:

```go
func TestActivityShowsOfflineCycle(t *testing.T) {
	t.Parallel()
	status := api.StatusResponse{
		State: "ready", Version: "test", PID: 1,
		LastSync: &api.SyncSummary{Offline: true, PushQueued: true},
	}
	got := plain(activityView{}.view(status, nil, nil, time.Now()))
	if !strings.Contains(got, "offline: remote unreachable") {
		t.Fatalf("Activity view %q missing the offline line", got)
	}
}
```

3. `internal/cli/client_commands_test.go` — the file EXISTS (`package cli`; `bytes`, `strings`, `testing`, and the `api` import are all already present). APPEND the test — do not recreate the file:

```go
func TestPrintSummaryOffline(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	printSummary(&reportWriter{w: &buf}, &api.SyncSummary{Offline: true, PushQueued: true})
	if !strings.Contains(buf.String(), "offline: remote unreachable") {
		t.Fatalf("printSummary output %q missing the offline line", buf.String())
	}
}
```

- [ ] **Step D7: Run everything to verify green**

Run: `go test ./internal/engine/ ./internal/daemon/ ./internal/cli/ ./internal/cli/dashboard/ -race -count=1`
Expected: PASS across all four packages.

- [ ] **Step D8: Mutation-proof the thread**

Temporarily delete the `report.Offline = integ.Offline` line in `sync.go`; run `go test ./internal/engine/ -run TestSyncOfflineCycleReportsOffline` — expect FAIL. Restore byte-identical.

- [ ] **Step D9: Full suite, lint, commit**

Run: `(ulimit -u 1400; go test ./... -race -count=1)` then `gofumpt -l -w . && golangci-lint run`

```bash
git add internal/engine/engine.go internal/engine/sync.go internal/engine/sync_test.go internal/daemon/api/types.go internal/daemon/daemon.go internal/daemon/daemon_test.go internal/cli/dashboard/dashboard.go internal/cli/dashboard/dashboard_test.go internal/cli/dashboard/activity.go internal/cli/dashboard/activity_test.go internal/cli/client_commands.go internal/cli/client_commands_test.go
git commit -m "feat(telemetry): surface offline cycles distinctly from degraded

integrateOutcome.Offline now rides engine.Report and api.SyncSummary
(additive field) so status, the dashboard header, and Activity say
'offline' — the benign off-network axis — instead of leaving it
indistinguishable from a conflict degrade.

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task E: Stale docs/comments sweep + gitMetaNames unexport (findings 1, 3, 4, 9, 8)

Pure truth restoration for pre-existing drift. Two commits: the docs/comments sweep, then the unexport.

**Files:**
- Modify: `docs/00-design-spec.md` (§8 tree at :376-402; §7 flag lines at :330 and :335-338)
- Modify: `internal/ghx/ghx.go` (:70-71)
- Modify: `internal/selfupdate/selfupdate.go` (:32-33)
- Modify: `internal/repo/gitmeta.go` (:8-11, :44-46)

**Interfaces:** none — no behavior changes. The unexport is provably safe: `grep -rn "GitMetaNames" --include="*.go" .` returns only `internal/repo/gitmeta.go` (verified 2026-07-10; re-verify in step E5).

- [ ] **Step E1: Spec §8 — true up the package tree**

In `docs/00-design-spec.md`, the tree block currently ends its `internal/` listing with:

```
│   ├── config/                # config.toml, platform paths (XDG / macOS)
│   ├── service/               # kardianos install/uninstall, WSL2 spawn mode
│   └── provision/             # gh detection, repo creation
```

Replace those three lines with:

```
│   ├── config/                # config.toml, platform paths (XDG / macOS)
│   ├── service/               # kardianos install/uninstall, WSL2 spawn mode
│   ├── ghx/                   # gh CLI exec wrapper: auth, provisioning, releases
│   ├── doctor/                # check battery + the daemon's sync SafetyGate
│   └── selfupdate/            # gh-native self-update pipeline (ADR 18)
```

And after the tree's trailing line `(\`testdata/\` directories sit inside each package as needed.)`, append a new paragraph:

```
(The once-planned `internal/provision` package was folded into `internal/ghx`
plus `internal/cli`'s init steps — ADR 08 records the provisioning design.)
```

- [ ] **Step E2: Spec §7 — enumerate the missing flags**

Two exact replacements in `docs/00-design-spec.md`:

1. Replace:

```
- **`track [path] | track --all`**, **`untrack <path|folder> [--purge]`** —
```

with:

```
- **`track [path] | track --all`**, **`untrack <path|folder> [--purge | --yes]`** —
```

2. Replace:

```
  scan — advisory, §5/§11), **`service install|uninstall|start|stop|status|logs`**,
  **`key export`** / **`key import [--force]`** / **`key rotate`** (fail-closed
  fleet re-encrypt, §5), **`migrate`** (§10), **`daemon run`** (foreground).
```

with:

```
  scan — advisory, §5/§11), **`service install|uninstall|start|stop|status|logs [-n]`**,
  **`key export`** / **`key import [--force]`** / **`key rotate [--yes]`** (fail-closed
  fleet re-encrypt, §5), **`migrate [--skip-preflight | --yes]`** (§10),
  **`daemon run`** (foreground).
```

- [ ] **Step E3: ghx.go — comment states the real call graph**

Replace (at `internal/ghx/ghx.go:70-71`):

```go
// NewClientWithRunner wires an explicit Runner and binary path — the seam
// tests (ghxtest.Fake) and init/doctor (a path already resolved once) use.
```

with:

```go
// NewClientWithRunner wires an explicit Runner and binary path. Production
// code reaches it only through NewClient (which resolves gh and wires the
// exec-backed Runner); tests call it directly to inject ghxtest.Fake without
// a real gh on PATH.
```

- [ ] **Step E4: selfupdate.go — comment states the real consumers**

Replace (at `internal/selfupdate/selfupdate.go:32-33`):

```go
// Typed sentinels the CLI branches on with errors.Is — mirrors
// internal/service's sentinel discipline (never string-match error text).
```

with:

```go
// Typed sentinels, identity-assertable with errors.Is through every %w wrap
// — mirrors internal/service's sentinel discipline (never string-match error
// text). runUpdate surfaces their self-remediating text verbatim rather than
// branching per sentinel; the test suite (and any scripted caller) asserts
// on them with errors.Is.
```

- [ ] **Step E5: Verify, test, commit the sweep**

Run: `grep -rn "internal/provision" docs/ && grep -n "logs \[-n\]" docs/00-design-spec.md` — first grep must return ONLY the new folded-note line (no other doc names that path: ADR 08 describes the provisioning design without ever spelling `internal/provision`), second must hit.
Run: `go build ./... && go test ./internal/ghx/ ./internal/selfupdate/ -race -count=1`
Expected: PASS (comment-only Go changes).

```bash
git add docs/00-design-spec.md internal/ghx/ghx.go internal/selfupdate/selfupdate.go
git commit -m "docs: true up spec §7 flags and §8 package tree; fix two stale doc comments

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

- [ ] **Step E6: Unexport gitMetaNames**

First re-verify: `grep -rn "GitMetaNames" --include="*.go" .` → must list only `internal/repo/gitmeta.go`. If anything else appears (Task B/C landed new code since the review), STOP and report instead of renaming.

In `internal/repo/gitmeta.go`, apply three edits:

1. Declaration comment + name (lines 8-11):

```go
// gitMetaNames are the path segments that carry git's own semantics and so
// must never originate from provider content or survive in the checkout
// below its root: `.gitattributes`, `.gitignore`, `.git`. Unexported: the
// list is IsGitMetaPath's implementation detail, and exporting a mutable
// package-level slice would let any importer edit a security predicate.
var gitMetaNames = []string{".gitattributes", ".gitignore", ".git"}
```

2. The sole use inside `IsGitMetaPath`:

```go
		if slices.ContainsFunc(gitMetaNames, func(meta string) bool {
```

- [ ] **Step E7: Test, lint, commit the unexport**

Run: `go build ./... && go test ./internal/repo/ ./internal/doctor/ ./internal/engine/ -race -count=1 && golangci-lint run`
Expected: PASS / clean (`repo/gitmeta_test.go` and `doctor/gitmeta_test.go` both use literal strings, not the slice).

```bash
git add internal/repo/gitmeta.go
git commit -m "refactor(repo): unexport gitMetaNames — a security predicate's list is not API

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task F: CI dead-code gate (recurrence backstop for one class)

Pin `golang.org/x/tools/cmd/deadcode` through a go.mod `tool` directive (Go 1.24+; this repo is 1.26) and add a CI job that fails on ANY unreachable function — the baseline is zero across all 21 packages (established by the 2026-07-10 review, three invocations). The tool directive makes the pin dependabot-bumpable, which run-line `@version` pins are not (the exact pain `ci.yml`'s govulncheck comment records).

Scope stated honestly: RTA-based deadcode pins the unreachable-FUNCTION class at zero. It cannot see an interface-dispatched method that loses its last caller (finding 6's exact shape — `apiData.Track` reports clean today), unread struct fields (findings 5, 7), or doc drift; for those classes the wiring tasks' own tests are the defense. The gate keeps the cheapest phantom class from ever accumulating again — no more, no less.

**Files:**
- Modify: `go.mod` / `go.sum` (tool directive; `golang.org/x/tools` becomes a direct dependency at v0.48.0)
- Modify: `.github/workflows/ci.yml` (new `deadcode` job)
- Modify: `CLAUDE.md` (commands block)

**Interfaces:**
- Consumes: `go tool deadcode` (resolves through go.mod), `-test ./...` flags. Deadcode exits 0 even when it finds dead functions — the gate must fail on non-empty OUTPUT, not exit code.
- Produces: CI job `deadcode`; local command `go tool deadcode -test ./...`.

- [ ] **Step F1: Add the tool dependency**

```bash
go get -tool golang.org/x/tools/cmd/deadcode@v0.48.0
go mod tidy
```

Expected: `go.mod` gains a `tool golang.org/x/tools/cmd/deadcode` directive and `golang.org/x/tools v0.48.0` moves from the indirect block to the direct `require` block; `go mod tidy` changes nothing further.

- [ ] **Step F2: Verify the zero baseline locally**

```bash
go tool deadcode -test ./...
```

Expected: EMPTY output (exit 0). Then verify the analyzer really ran: `go tool deadcode -json -test ./... ` prints `null` (the tool's empty-result JSON), not an error.

- [ ] **Step F3: Mutation-proof the gate**

Append a temporary dead function to `internal/repo/gitmeta.go`:

```go
func deadcodeCanaryNeverCalled() {}
```

Run `go tool deadcode -test ./...` — expected output names `deadcodeCanaryNeverCalled` in `internal/repo/gitmeta.go`. Delete the canary, re-run, confirm empty again. (This proves the gate detects the unreachable-function class before CI ever runs it.)

- [ ] **Step F4: Add the CI job**

In `.github/workflows/ci.yml`, append after the `govulncheck` job (same SHA-pinned actions as every other job):

```yaml
  deadcode:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@93cb6efe18208431cddfb8368fd83d5badbf9bfd # v5.0.1
      - uses: actions/setup-go@924ae3a1cded613372ab5595356fb5720e22ba16 # v6.5.0
        with:
          go-version-file: go.mod
      # deadcode is version-pinned via go.mod's tool directive (dependabot
      # bumps it there, unlike run-line @version pins). The tool exits 0
      # even when it finds dead functions, so the gate fails on output.
      # Baseline: zero unreachable functions across all packages (2026-07-10).
      # Scope: unreachable functions only — interface-dispatched methods stay
      # live under RTA, and struct fields are invisible to it by construction.
      - name: dead-code gate (zero baseline)
        run: |
          output=$(go tool deadcode -test ./...)
          if [ -n "$output" ]; then
            echo "$output"
            echo "::error::dead code detected — the baseline is zero; delete it or wire it"
            exit 1
          fi
```

- [ ] **Step F5: Document the local command**

In `CLAUDE.md`'s Commands block, add after the `gofumpt -l -w .` line:

```
go tool deadcode -test ./...                    # dead-code gate (CI-enforced, zero baseline)
```

- [ ] **Step F6: Full suite (the x/tools bump touched go.sum), lint, commit**

Run: `(ulimit -u 1400; go test ./... -race -count=1)` then `golangci-lint run`
Expected: PASS / clean.

```bash
git add go.mod go.sum .github/workflows/ci.yml CLAUDE.md
git commit -m "ci: enforce the zero dead-code baseline with a go.mod-pinned deadcode gate

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

## Deliberately not in this wave (recorded, not silently skipped)

- **Per-sentinel `errors.Is` branching / distinct exit codes in `runUpdate`** (finding 4's "richer" alternative): the sentinel errors already carry their remediation text verbatim to the user, and no scripted consumer exists today — building branch ceremony now would create exactly the unused surface this plan exists to eliminate. Task E's comment reword states the true contract instead.
- **Extracting `internal/provision` to match the old spec diagram** (finding 1's inverse resolution): `ghx` already IS the gh boundary and init's steps are cohesive where they live; the spec follows as-built truth (Task E), with ADR 08 as the provisioning record.
- **Engine-side per-unit withholding on project-identity drift** (finding 5 follow-up): Task C's doctor check makes drift visible with an untrack/re-track remediation, and its Detail names the real consequence (silent bidirectional cross-project contamination — a headless machine never runs the battery). Blocking a drifted unit inside the engine cycle is the stronger guarantee, but the correct enforcement point is post-integrate (the reassignment arrives in the same fetch), which means a per-cycle registry read and a new degrade class — a spec §4/§5 semantics change requiring its own ADR. Task C step C7 records that follow-up durably in `docs/plans/backlog-project-identity-engine-guard.md` (a finished plan's appendix is where follow-ups go to die), deliberately not bolted onto this wave.
- **Per-unit `"offline"` outcome in `UnitCycleResult`** (finding 7 scope edge): offline is fleet-level (one fetch per cycle); Task D's comment records the reasoning.

## Self-review (writing-plans checklist)

- **Spec coverage:** finding 1 → E1; 2 → A; 3 → E3; 4 → E4; 5 → C; 6 → B; 7 → D; 8 → E6; 9 → E2; 10-13 were verified non-issues (no task, per the findings report's dispositions); recurrence → F. Every actionable disposition in the findings report maps to a task.
- **Placeholders:** none — every step carries exact code, exact old→new text, exact commands with expected outcomes. The two deliberately conditional steps (C6 scan.go comment, E6 pre-grep) specify the exact decision rule and both outcomes.
- **Type consistency:** `dashboardKeys`/`dashboardKeymap`/`forTab` (A) are extended, not redefined, by B (B changes `forTab`'s signature and updates its sole caller, `footer()`, in the same step); `TrackCandidate`/`TrackRoot`/`trackActions` names match across track.go, projects.go, dashboard.go, and the cli closures; `provider.NamedIdentity` signature matches its uses in enroll.go and track.go; `Report.Offline`/`SyncSummary.Offline` names match across engine/daemon/api/cli/dashboard.

## Review deltas (independent staff review, 2026-07-10 — applied)

An independent reviewer read the live tree against every claim in this plan (~40 API/line-number spot-checks, all accurate), empirically confirmed the two riskiest premises (bubbles/v2@v2.1.1 `key`/`textinput` signatures verbatim from the module cache; `deadcode@v0.48.0 -test ./...` returning zero at tip), and endorsed every contested architecture call: closure injection over daemon endpoints (the ZERO-endpoints constraint holds — the CLI itself does discovery/identify in-process; only mutation needs the single writer), the doctor check's SafetyGate exclusion, fleet-level offline with no per-unit outcome, the keymap as single source, and all four recorded deferrals. Its findings (2 blockers, 2 majors, 12 minors) are applied above:

- **Task B (both blockers + one major):** `drive` now flattens `tea.BatchMsg` (the post-track fleet sync was unreachable under the old helper, failing a correct implementation); `view()` is replaced wholesale so the add flow renders ahead of the `loadErr`/`!loaded`/empty guards and the notice trailer renders in every non-add state (three of six tests would otherwise fail against spec-correct code); `onDiscover` gained the stale-after-esc guard `onIdentify` already had (a late discovery answer must not resurrect a cancelled flow); the footer now gates `a add` on the closures actually being injected, preserving Task A's advertise-only-what-works contract.
- **Task D (major):** header precedence corrected to `error > degraded > scrubbed > offline > ok` — the original order rested on a false premise (`prepareCheckout` scrubs BEFORE integrate, so an offline cycle CAN carry `Scrubbed`) and would have masked the loudest security signal behind "offline" for an entire offline stretch. A third table case pins it.
- **Task C:** the engine-guard follow-up is recorded in a durable backlog stub (step C7) and the drift Detail names the cross-project contamination.
- **Honesty fixes:** deadcode gate scope narrowed to the unreachable-function class it actually pins (verified empirically: it does NOT flag finding 6's interface-method shape); the "full parity" claim narrowed to single-root enrollment; the keymap comment softened to the direction that is structural vs. test-pinned; exact-text corrections (daemon internal test file is `server_test.go`, `client_commands_test.go` already exists, the README key line wraps, ADR 08 never names `internal/provision`).
