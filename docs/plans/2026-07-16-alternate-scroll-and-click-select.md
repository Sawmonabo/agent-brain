# Alternate-Scroll Wheel + Browser Click-to-Select Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the mouse wheel scroll hub content on every screen (reading view included) without sacrificing native drag-select, by enabling the terminal's alternate-scroll mode (DECSET 1007) for the hub session — and close the previously-deferred browser contract so a click on a list row selects that memory.

**Architecture:** DECSET 1007 makes the terminal translate wheel notches into arrow-key sequences while an app is in the alternate screen with no mouse tracking active — the wheel scrolls content, and because no mouse tracking is enabled, native drag-select keeps working. Mouse tracking (the browser preview's existing cell-motion capture) takes precedence over alternate scroll by xterm-documented rule, so the two mechanisms compose with no arbitration code. The mode is emitted through bubbletea's supported raw-escape seam (`tea.Raw` → `RawMsg` → in-loop `p.execute`), constructed from x/ansi's generic mode machinery (`ansi.SetMode(ansi.DECMode(1007))`) — no dependency change. Codex ships this exact posture (binary carries `[?1007h/l`, no mouse-capture escapes; openai/codex#2836 confirms runtime emission). Click-to-select lands via a root→screen mouse coordinate translation plus a render-time line→row hit-map recorded by the browser's own `renderList`, so mapping and pixels cannot drift.

**Tech Stack:** Go, charm.land/bubbletea/v2 v2.0.8, charm.land/bubbles/v2 v2.1.1, github.com/charmbracelet/x/ansi v0.11.7 (all already pinned; latest published — verified 2026-07-16).

## Global Constraints

- Never commit to `main` (ADR 11); work lands on a feature branch, merged to `develop`.
- Tests: stdlib `testing` + `google/go-cmp` only; table-driven; `t.Parallel()`; `t.TempDir()` for any filesystem needs (ADR 15).
- No `//nolint` directives; fix lint findings structurally.
- No PR numbers, reviewer names, review timestamps, dates, or commit hashes in code comments or test names — WHY in comments, WHO/WHEN in commit messages.
- Comment density and idiom must match the surrounding files (this codebase carries heavy rationale comments; every non-obvious decision gets its WHY).
- `internal/cli/dashboard` remains the only TUI-importing package tree (spec §15). `github.com/charmbracelet/x/ansi` is already a direct module dependency; importing it inside `internal/cli/dashboard` is in-boundary.
- Full gate before merge: `go build ./...`, `(ulimit -u 1400; go test ./... -race -count=1)` run FOREGROUND, `gofumpt -l .` empty, `golangci-lint run` 0 issues, `go tool deadcode -test ./...` empty.
- Cross-OS: everything here is terminal escape sequences and pure Go — no OS-specific APIs. Must not regress macOS/Linux/WSL2.
- Commit messages end with the `Co-Authored-By:` trailer your harness specifies.

## Decision Records (adjudicated during planning — do not re-litigate in-task)

1. **DECSET 1007 over hub-wide mouse capture.** Mouse capture anywhere beyond the browser preview would suppress native drag-select there — 1007 achieves wheel-scroll with drag-select intact. The previously-discussed `dashboard.mouse` full-capture toggle is REJECTED as superseded, not deferred: with 1007, its only residual value would be click-to-focus in multi-pane views, and the browser (the only multi-pane screen) already has scoped capture.
2. **Config kill-switch, default on.** `dashboard.alternate_scroll = true` (k9s `ui.enableMouse` precedent, inverted default because 1007 is non-destructive). A user who wants the terminal's raw wheel behavior in the hub (e.g. to check shell scrollback mid-session, Claude-Code-style) sets `false`. Plain `bool` is safe: `LoadSettings` decodes over `DefaultSettings()` (settings.go:125-137), so an absent key keeps `true`.
3. **No DECRQM feature detection.** The mode is set-and-forget: terminals that ignore 1007 (kitty, tmux) are unaffected, and no hub behavior branches on support. Querying would add async state for zero behavioral difference.
4. **kitty/tmux inertness accepted and documented.** kitty translates wheel→arrows in alt-screen unconditionally (right behavior, no 1007 parsing); tmux swallows the inner app's 1007 and needs a user `WheelUpPane` binding — documented beside the existing OSC52 `allow-passthrough` note.
5. **Activity/Doctor tab scrolling stays out** — NOT a silent deferral: those tabs have no cursor/viewport machinery by deliberate design (doctorview.go caps pathological sweeps), and their overflow UX is the pre-existing tracked follow-up ("doctor/activity tab scroll UX design"). Wheel-as-arrows is inert there until that follow-up lands its design.
6. **Wheel-per-notch bursts are acceptable.** Terminals send 1–6 arrow presses per notch (VTE ~3, alacritty 1/line, macOS momentum floods) — each press is one small cursor/viewport step, identical to holding `j`. No clamping layer.

---

### Task 1: `dashboard.alternate_scroll` config setting

**Files:**
- Modify: `internal/config/settings.go` (struct at :91, defaults at :102, no validation needed — bool)
- Test: `internal/config/settings_test.go`

**Interfaces:**
- Consumes: existing `Settings`/`DefaultSettings`/`LoadSettings` (settings.go:91-137).
- Produces: `config.DashboardSettings{AlternateScroll bool}` reachable as `settings.Dashboard.AlternateScroll` — Task 2 reads it via the dashboard `Config.Settings` plumbing (dashboard.go:194→:372) and at the command layer (internal/cli/dashboard.go).

- [ ] **Step 1: Write the failing test**

Add to `internal/config/settings_test.go`, matching the file's existing table style:

```go
// TestLoadSettingsAlternateScroll pins the dashboard.alternate_scroll
// contract: absent file and absent section both keep the default (on), an
// explicit false wins, an explicit true round-trips.
func TestLoadSettingsAlternateScroll(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name string
		toml string // "" = no file written
		want bool
	}{
		{name: "no config file", toml: "", want: true},
		{name: "section absent", toml: "[lint]\nstale_after_days = 30\n", want: true},
		{name: "explicit false", toml: "[dashboard]\nalternate_scroll = false\n", want: false},
		{name: "explicit true", toml: "[dashboard]\nalternate_scroll = true\n", want: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.toml")
			if testCase.toml != "" {
				if err := os.WriteFile(path, []byte(testCase.toml), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			settings, err := LoadSettings(path)
			if err != nil {
				t.Fatalf("LoadSettings: %v", err)
			}
			if got := settings.Dashboard.AlternateScroll; got != testCase.want {
				t.Errorf("Dashboard.AlternateScroll = %v, want %v", got, testCase.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run it, verify it fails**

Run: `go test ./internal/config/ -run TestLoadSettingsAlternateScroll -count=1`
Expected: FAIL — `settings.Dashboard undefined`.

- [ ] **Step 3: Implement**

In `internal/config/settings.go`, beside the other section types (after `LintSettings`):

```go
// DashboardSettings tunes the hub TUI.
type DashboardSettings struct {
	// AlternateScroll asks the terminal for alternate-scroll mode (DECSET
	// 1007) while the hub runs: the wheel scrolls hub content (delivered as
	// arrow keys) instead of the terminal's own scrollback, without mouse
	// tracking — so native drag-select stays intact everywhere. False
	// restores the terminal's raw wheel behavior. Terminals that do not
	// implement the mode ignore it either way.
	AlternateScroll bool `toml:"alternate_scroll"`
}
```

Add `Dashboard DashboardSettings \`toml:"dashboard"\`` to `Settings` (beside `Lint`), and to `DefaultSettings()`:

```go
Dashboard: DashboardSettings{
	AlternateScroll: true,
},
```

- [ ] **Step 4: Run the test, verify green; run the package suite**

Run: `go test ./internal/config/ -race -count=1`
Expected: PASS (all — the new field must not break existing round-trip/validation tests).

- [ ] **Step 5: Commit**

```bash
git add internal/config/settings.go internal/config/settings_test.go
git commit -m "feat(config): dashboard.alternate_scroll setting (default on)"
```

---

### Task 2: DECSET 1007 lifecycle — set on init, re-assert after editor, reset on exit

**Files:**
- Create: `internal/cli/dashboard/altscroll.go`
- Modify: `internal/cli/dashboard/dashboard.go` (`Init` at :515)
- Modify: `internal/cli/dashboard/editflow.go` (`editorFinishedMsg` handler — find the root `Update` case that consumes it)
- Modify: `internal/cli/dashboard.go` (command layer; `program.Run()` at :159, before `maybeReExec` at :173)
- Test: `internal/cli/dashboard/altscroll_test.go`, `internal/cli/dashboard/dashboard_test.go`

**Interfaces:**
- Consumes: `config.Settings.Dashboard.AlternateScroll` (Task 1); `tea.Raw`/`tea.RawMsg` (bubbletea raw.go — in-loop `p.execute`, race-free by construction); `ansi.SetMode`/`ansi.ResetMode`/`ansi.DECMode` (x/ansi mode.go:66/:95/:219).
- Produces: `dashboard.RestoreAlternateScroll(w io.Writer, enabled bool)` called by the command layer; package-internal `setAlternateScroll`/`resetAlternateScroll` string constants; `Model.alternateScrollCmd() tea.Cmd`.

**Why each lifecycle point (verified against bubbletea v2.0.8 source):**
- *Init:* `tea.Raw` flows through the program loop (`tea.go:858` → `p.execute`), so ordering against the renderer's own mode writes is serialized. The renderer sets/resets ONLY its enumerated modes (cursed_renderer.go:116-127, :168-186, :328-367) and never touches 1007 — no blanket reset exists that could clobber ours.
- *editorFinishedMsg:* the in-terminal `$EDITOR` handoff (`tea.ExecProcess`, editflow.go:371) hands the terminal to a child that may reset modes; re-asserting on its completion message is deterministic and idempotent (4 bytes when redundant). The GUI-editor path never releases the terminal — the redundant re-assert is harmless there too.
- *post-Run, command layer:* covers EVERY exit path (q, ctrl+c, context cancel, ErrProgramKilled) with one choke point. It must be an explicit call, not a defer: `maybeReExec` (internal/cli/dashboard.go:173) may `syscall.Exec`, which replaces the process image — deferred functions never run. A SIGKILL leaks the mode; that is the same leak class as bubbletea's own mouse/altscreen modes and is benign (1007 is dormant outside the alternate screen).

- [ ] **Step 1: Write the failing tests**

`internal/cli/dashboard/altscroll_test.go`:

```go
package dashboard

import (
	"strings"
	"testing"
)

// TestAlternateScrollSequences pins the exact escapes: private mode 1007
// set/reset. Hand-typed literals, not the ansi helpers, so a helper
// regression cannot silently rewrite both sides of the comparison.
func TestAlternateScrollSequences(t *testing.T) {
	t.Parallel()
	if setAlternateScroll != "\x1b[?1007h" {
		t.Errorf("setAlternateScroll = %q, want %q", setAlternateScroll, "\x1b[?1007h")
	}
	if resetAlternateScroll != "\x1b[?1007l" {
		t.Errorf("resetAlternateScroll = %q, want %q", resetAlternateScroll, "\x1b[?1007l")
	}
}

// TestRestoreAlternateScroll pins the command-layer teardown: enabled writes
// exactly the reset sequence, disabled writes nothing (the mode was never
// set, so resetting would flip a state the user's own tooling may have set).
func TestRestoreAlternateScroll(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		enabled bool
		want    string
	}{
		{name: "enabled resets", enabled: true, want: "\x1b[?1007l"},
		{name: "disabled writes nothing", enabled: false, want: ""},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			var out strings.Builder
			RestoreAlternateScroll(&out, testCase.enabled)
			if got := out.String(); got != testCase.want {
				t.Errorf("wrote %q, want %q", got, testCase.want)
			}
		})
	}
}
```

In `internal/cli/dashboard/dashboard_test.go`, a drain-based Init pin (use the file's existing helpers for draining batched Cmds; follow the local fixture style for building a Model with settings):

```go
// TestInitEmitsAlternateScroll pins the enable half of the 1007 lifecycle:
// Init's batch carries the set sequence as a RawMsg exactly when the setting
// is on. Table covers both setting states so the config gate is proven
// load-bearing, not decorative.
func TestInitEmitsAlternateScroll(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name    string
		enabled bool
		want    bool
	}{
		{name: "enabled emits set", enabled: true, want: true},
		{name: "disabled emits nothing", enabled: false, want: false},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			model := newTestModel(t) // adapt to the file's actual fixture constructor
			model.settings.Dashboard.AlternateScroll = testCase.enabled
			messages := drainCmd(t, model.Init()) // adapt to the file's actual drain helper
			found := false
			for _, message := range messages {
				if raw, ok := message.(tea.RawMsg); ok && fmt.Sprint(raw.Msg) == setAlternateScroll {
					found = true
				}
			}
			if found != testCase.want {
				t.Errorf("Init emitted set-1007 RawMsg = %v, want %v", found, testCase.want)
			}
		})
	}
}
```

Plus the editor re-assert pin: find the existing editflow test that drives `editorFinishedMsg` through the root `Update` (editflow tests live in the dashboard package) and add a case asserting the returned Cmd's drained messages include the same `tea.RawMsg` when the setting is on, and do not when off.

- [ ] **Step 2: Run them, verify they fail**

Run: `go test ./internal/cli/dashboard/ -run 'TestAlternateScroll|TestRestoreAlternateScroll|TestInitEmitsAlternateScroll' -count=1`
Expected: FAIL — `setAlternateScroll` undefined.

- [ ] **Step 3: Implement `altscroll.go`**

```go
package dashboard

import (
	"io"

	"github.com/charmbracelet/x/ansi"
)

// Alternate-scroll mode (DECSET 1007): while the hub owns the alternate
// screen and no mouse tracking is armed, the terminal translates wheel
// notches into arrow-key presses — every list and viewport scrolls under
// the wheel, and because no tracking is enabled, the terminal's native
// drag-select keeps working everywhere. The browser preview's cell-motion
// capture takes precedence over this mode while armed (xterm's documented
// rule), so the two compose without arbitration. bubbletea v2.0.8 has no
// named API for mode 1007; the sequences are built from x/ansi's generic
// DECMode machinery and delivered through tea.Raw — the supported raw-escape
// seam — so no renderer change or dependency bump is involved. ADR 21 holds
// the full decision trail.
var (
	setAlternateScroll   = ansi.SetMode(ansi.DECMode(1007))
	resetAlternateScroll = ansi.ResetMode(ansi.DECMode(1007))
)

// RestoreAlternateScroll writes the mode reset after the program has
// returned, whatever the exit path — the one choke point every quit shares.
// It must run before any re-exec: syscall.Exec replaces the process image,
// so nothing deferred survives it. No-op when the mode was never set:
// resetting unconditionally could flip a 1007 the user's own tooling armed.
func RestoreAlternateScroll(w io.Writer, enabled bool) {
	if !enabled {
		return
	}
	_, _ = io.WriteString(w, resetAlternateScroll)
}
```

- [ ] **Step 4: Wire Init (dashboard.go:515)**

```go
func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.reloadCmd(), m.tickCmd(), tea.RequestBackgroundColor}
	if m.settings.Dashboard.AlternateScroll {
		// Set once for the whole session; the terminal keeps the mode across
		// the editor handoff's altscreen exit/re-entry (it is dormant outside
		// the alternate screen), and editorFinishedMsg re-asserts it in case
		// the child editor reset it.
		cmds = append(cmds, tea.Raw(setAlternateScroll))
	}
	return tea.Batch(cmds...)
}
```

- [ ] **Step 5: Wire the editor re-assert (editflow.go)**

In the root's `editorFinishedMsg` handling, batch `tea.Raw(setAlternateScroll)` alongside the existing continuation when `m.settings.Dashboard.AlternateScroll` — with a comment naming WHY (child editors may reset terminal modes; idempotent when they did not).

- [ ] **Step 6: Wire the command-layer teardown (internal/cli/dashboard.go)**

Immediately after `program.Run()` returns (before the error handling at :160, so every path is covered):

```go
finalModel, err := program.Run()
// Whatever the exit path — clean quit, ctrl+c, context cancel, kill — the
// terminal must not keep translating the wheel to arrows for the shell.
// Explicit call, not deferred: maybeReExec below may syscall.Exec, which
// replaces the process image before any defer could run.
dashboard.RestoreAlternateScroll(cmd.OutOrStdout(), settings.Dashboard.AlternateScroll)
```

(`settings` is already in scope from `loadDashboardSettings()`; verify the variable name at the call site.)

- [ ] **Step 7: Run the full dashboard + cli suites**

Run: `go test ./internal/cli/... -race -count=1`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/cli/dashboard/altscroll.go internal/cli/dashboard/altscroll_test.go internal/cli/dashboard/dashboard.go internal/cli/dashboard/dashboard_test.go internal/cli/dashboard/editflow.go internal/cli/dashboard.go
git commit -m "feat(dashboard): alternate-scroll wheel across the hub (DECSET 1007)

Set on init, re-asserted after the editor handoff, reset on every exit
path before any re-exec. Wheel scrolls hub content as arrow keys with no
mouse tracking, so native drag-select stays intact on every screen; the
browser preview's cell-motion capture takes precedence while armed."
```

---

### Task 3: Root mouse-coordinate translation + browser click-to-select-row

This closes the contract `updateMouseClick`'s comment explicitly deferred ("Click does NOT select a specific list row: the browser … never learns its absolute Y origin").

**Files:**
- Modify: `internal/cli/dashboard/dashboard.go` (mouse case at :977; new `mousePrefixLines`/`translateStackMouse` beside `forwardToStack` at :1447)
- Modify: `internal/cli/dashboard/views/browser.go` (`renderList` at :1191, `View` at :1024, `updateMouseClick` at :792; new fields + `clickRow`)
- Test: `internal/cli/dashboard/dashboard_test.go`, `internal/cli/dashboard/views/browser_test.go`

**Interfaces:**
- Consumes: `tea.MouseClickMsg`/`tea.MouseWheelMsg` (struct `Mouse{X, Y int; Button MouseButton}`); the root `View`'s join shape (`strings.Join([]string{header, breadcrumb, screen, footer}, "\n\n")` at :2098).
- Produces: root-side `translateStackMouse(msg tea.Msg) tea.Msg` (screen-local coordinates for every stack-bound mouse event); browser-side render-time hit-map `listLineRows []int` + `listTopLines int`; `Selected()` reflects a clicked row.

**Contract:** the hit-map is recorded by `renderList` at render time — the same staleness class as `previewShown` (browser.go:564-574 documents it): the map describes the LAST rendered frame, which is exactly what the user clicked on.

- [ ] **Step 1: Write the failing root-translation test**

In `dashboard_test.go` (reuse the fixture machinery `TestRootViewMouseModeGate` at :4169 uses to compose a Model with a browser on the stack):

```go
// TestStackMouseTranslation pins the root→screen coordinate contract: a
// stack-bound mouse event reaches the screen with Y shifted by exactly the
// chrome the root composes above it — header (with and without a toast
// line), the blank join line, the breadcrumb, and its blank join line. The
// translation must mirror View's actual join shape, so the test derives the
// expected offset from the rendered frame rather than hard-coding a number:
// the screen's first body line is located in View output, and a click on
// absolute row N must arrive at the browser as N minus that line index.
func TestStackMouseTranslation(t *testing.T) {
	// table: {name, withToast bool}
	// - compose model + browser stack exactly as the mouse-gate test does
	// - render m.View(), find the line index where the browser body begins
	//   (first line of the browser's rendered View in the joined frame)
	// - send tea.MouseClickMsg{Mouse{X: 0, Y: bodyStart}} through m.Update
	// - assert the browser observed Y == 0 (instrument via the clicked row:
	//   a click on the first list line must select the row the hit-map puts
	//   there, proven through browser.Selected())
}
```

(The implementer writes the concrete body following the file's existing fixture style; the assertion path via `Selected()` makes the pin end-to-end rather than testing a private offset function against itself.)

- [ ] **Step 2: Write the failing browser hit-map tests**

In `browser_test.go`, with a fixture spanning two providers (so group headers interleave) and enough memories to force windowing:

```go
// TestBrowserClickSelectsRow pins the click→row contract over the recorded
// hit-map: a click on a memory line moves the cursor to that memory (proven
// via Selected), a click on a group-header line changes nothing, a click
// below the last rendered line changes nothing, a click over the preview
// still focuses it (the pane-focus contract is unchanged), and clicks are
// inert while filtering and in the deleted list.
```

Table cases (each drives `View` first so the hit-map reflects a real frame, then `Update(tea.MouseClickMsg{...})`, then asserts `Selected()` / focus state):
1. click first memory row of provider A → selects it
2. click a row of provider B (after a header line) → selects it
3. click the provider-B header line → selection unchanged
4. click one line below the last rendered row → unchanged
5. click in the preview band → `previewFocused` true, selection unchanged
6. `filtering` true → click inert; `showDeleted` true → click inert
7. windowed list (cursor deep in a long list): click maps to the ABSOLUTE row the window shows on that line, not `start`-relative

- [ ] **Step 3: Run both, verify they fail**

Run: `go test ./internal/cli/dashboard/... -run 'TestStackMouseTranslation|TestBrowserClickSelectsRow' -count=1`
Expected: FAIL (no translation; clicks only flip pane focus).

- [ ] **Step 4: Implement the root translation (dashboard.go)**

```go
// mousePrefixLines is how many terminal rows the root composes above a stack
// screen's first line: the header block (toast line included when present),
// one blank line from the "\n\n" join, the breadcrumb, and its blank join
// line. Built from the same strings View joins so the offset and the pixels
// cannot drift; mouse reporting is only ever armed on the stack-top browser
// frame (the View MouseMode gate), so the default branch's shape is the only
// one a mouse event can arrive under.
func (m Model) mousePrefixLines() int {
	header := m.statusHeader()
	if toastLine := m.toastLine(); toastLine != "" {
		header = strings.Join([]string{header, toastLine}, "\n\n")
	}
	return lipgloss.Height(header) + 1 + lipgloss.Height(m.breadcrumb()) + 1
}

// translateStackMouse rebases a mouse event from terminal-absolute rows to
// screen-local rows before it is forwarded down the stack — the seam the
// browser's click-to-select contract needs (updateMouseClick's row mapping),
// left unbuilt when hover-scroll landed. X passes through untouched: the
// root adds no horizontal chrome (overPreview's documented invariant).
func (m Model) translateStackMouse(msg tea.Msg) tea.Msg {
	offset := m.mousePrefixLines()
	switch mouse := msg.(type) {
	case tea.MouseClickMsg:
		mouse.Y -= offset
		return mouse
	case tea.MouseWheelMsg:
		mouse.Y -= offset
		return mouse
	}
	return msg
}
```

Change the Update case at :977 to forward `m.translateStackMouse(msg)`.

- [ ] **Step 5: Implement the browser hit-map (browser.go)**

Fields on `Browser`:

```go
// listLineRows maps each rendered list-block line to the visibleRows index
// it displays (-1 for provider-header and notice lines), and listTopLines is
// how many browser-body lines sit above the list block. Both are recorded
// during View — the same last-frame contract as previewShown — so the click
// mapping and the pixels the user clicked can never disagree.
listLineRows []int
listTopLines int
```

`renderList` appends to `listLineRows` exactly where it writes each line (header lines and notices append -1; memory rows append their absolute `visibleRows` index — the same index space `b.cursor`/`Selected` use). `View` records `listTopLines` where it assembles the body above the list block (both the single-pane and split-pane branches). `updateMouseClick` gains the mapping, replacing the deferral comment:

```go
if b.overPreview(mouse.X) {
	b.previewFocused = true
	return
}
b.blurPreview()
// Screen-local Y (the root rebases stack mouse events) minus the browser's
// own chrome lands in the recorded hit-map; -1 entries (group headers,
// notices) and out-of-range clicks select nothing.
line := mouse.Y - b.listTopLines
if line < 0 || line >= len(b.listLineRows) {
	return
}
if row := b.listLineRows[line]; row >= 0 {
	b.cursor = row
}
```

(Verify against `moveCursor`'s cursor-space before landing: the hit-map must store indices in exactly the space `Selected()` reads.)

- [ ] **Step 6: Run the suites, verify green + no wheel regressions**

Run: `go test ./internal/cli/dashboard/... -race -count=1`
Expected: PASS, including the untouched wheel/gate pins (`TestRootViewMouseModeGate`, browser wheel tests).

- [ ] **Step 7: Commit**

```bash
git add internal/cli/dashboard/dashboard.go internal/cli/dashboard/dashboard_test.go internal/cli/dashboard/views/browser.go internal/cli/dashboard/views/browser_test.go
git commit -m "feat(dashboard): click a browser row to select it

Root rebases stack-bound mouse events to screen-local coordinates; the
browser records a render-time line-to-row hit-map inside renderList, so
the mapping and the rendered pixels cannot drift. Header and notice lines
select nothing; pane-focus clicks are unchanged."
```

---

### Task 4: Discoverability, comment truth-fix, spec, and ADR 21

**Files:**
- Modify: `internal/cli/dashboard/actions/actions.go` (registry, reading rows at :196-210)
- Modify: `internal/cli/dashboard/dashboard.go` (`available()` at :1853 — the reading-scope arm)
- Modify: `internal/cli/dashboard/views/reading.go` (CopyMemoryMsg doc at :37-44)
- Modify: `docs/01-dashboard-hub-spec.md` (§3 :62-69, §4 :77-86, §16 :249)
- Create: `docs/decisions/21-adr-alternate-scroll.md`
- Test: `internal/cli/dashboard/actions/actions_test.go` (if a registry-shape test exists, extend it), `internal/cli/dashboard/dashboard_test.go` (footer advertises the row)

**Interfaces:**
- Consumes: `actions.Action` struct (actions.go:79-86); `stackFooterRows` filtering (`dashboard.go:1679-1694`: scope match + `m.available(id)`); help overlay renders every registry row (help.go).
- Produces: registry row `reading-scroll`; corrected reading.go doc comment; spec §3/§4/§16 text; ADR 21.

- [ ] **Step 1: Registry row + availability (with test first)**

Test: extend the existing registry/footer test pattern — with a reading screen on the stack, the footer line must contain `j/k scroll`; the help overlay must list it under the reading scope. Registry order is render order, and scroll is the reading view's primary interaction, so insert the row FIRST among the ScopeReading rows (before `reading-links` at :196):

```go
// scroll documents the viewport keys (spec §4) in the footer and help; the
// viewport's own keymap matches them (readingViewportKeyMap), no root
// runner. The wheel reaches the same path: alternate scroll (ADR 21)
// delivers wheel notches as these arrow keys.
{ID: "reading-scroll", Title: "scroll", Keys: []string{"j", "k"}, KeyHint: "j/k", Scope: ScopeReading},
```

Add `"reading-scroll"` to `available()`'s always-true reading arm (beside `reading-links`; find the exact switch arm). The palette hides it automatically (no dispatch runner — the established mechanism for in-screen rows; verify with the palette's existing test pattern if one covers runnerless rows).

- [ ] **Step 2: Comment truth-fix (reading.go:37-44)**

The current doc claims the OSC52 copy is "the same thing Claude Code's CLI does" — binary inspection disproved that (Claude Code renders inline in the primary buffer, captures no mouse, and relies on native terminal selection; no app-level copy). Replace the clause:

```go
// CopyMemoryMsg asks the root to copy Body — a memory's RAW markdown source —
// to the system clipboard. It is the feature-full answer to native drag-select
// being suppressed while the browser holds mouse mode (an xterm-protocol
// constraint, not ours): an app-level OSC52 copy that carries over SSH, tmux
// (allow-passthrough), and WSL2 — places a pointer selection cannot reach.
// Label is the memory's display name, printed verbatim in the confirmation
// toast. Like CopyPathMsg the clipboard write is bubbletea's OSC52
```

(Keep the rest of the original comment intact from "Label is" onward — only the Claude Code attribution goes.)

- [ ] **Step 3: Spec updates (docs/01-dashboard-hub-spec.md)**

§3, after the existing mouse bullet (:62-69), new bullet:

```markdown
- Everywhere else the hub captures no mouse. Instead it sets the terminal's
  alternate-scroll mode (DECSET 1007) for the session, so the wheel scrolls
  hub content — delivered as arrow keys — with native drag-select intact on
  every screen (ADR 21; Codex's posture). Clicking a browser list row selects
  it. Terminals that ignore the mode: kitty already translates wheel to
  arrows in the alternate screen unconditionally; tmux swallows it unless the
  user binds `WheelUpPane`/`WheelDownPane` with `#{alternate_on}` send-keys
  (documented beside the OSC52 `allow-passthrough` note). Config:
  `dashboard.alternate_scroll = false` restores the terminal's raw wheel.
```

§4 (:77-79): extend the scroll sentence: `(viewport; j/k, ctrl+d/u, g/G — and the wheel via alternate scroll)`.

§16 (:249): replace `No remote/SSH serving of the TUI; no mouse-first interactions.` with `No remote/SSH serving of the TUI. Keyboard-first: the wheel (alternate scroll) and the browser's scoped mouse reporting are conveniences layered over complete keyboard paths, never the only path.`

- [ ] **Step 4: ADR 21 (docs/decisions/21-adr-alternate-scroll.md)**

Follow the shape of ADR 20 (status/context/decision/consequences). Content requirements — decision: DECSET 1007 hub-wide via `tea.Raw` + generic `ansi.DECMode(1007)`, config-gated default-on, reset on every exit path, re-assert after editor handoff; alternatives rejected: (a) mouse capture in the reader (kills native drag-select — the lazygit trade), (c) keyboard-only status quo (wheel surprise on iTerm2-class terminals); precedence rule (mouse tracking beats alternate scroll — terminalguide p1007, iTerm2 feature-reporting, microsoft/terminal PR #16535); terminal matrix summary (xterm/iTerm2/VTE/alacritty/WT honor it; kitty translates unconditionally; tmux needs the user binding; Terminal.app modern default sends arrows; xterm.js/Cursor partial — smoke-test cell); peer evidence (Codex binary carries `[?1007h/l` + no mouse-capture escapes, openai/codex#2836; Claude Code is inline/primary-buffer via Ink, no mouse, native selection). **Sources section** (the planning research trail, persisted per repo practice):

```markdown
## Sources

- xterm ctlseqs, private mode 1007 / alternateScroll: https://invisible-island.net/xterm/ctlseqs/ctlseqs.html
- terminalguide mode p1007 (precedence: mouse reporting suppresses alternate scroll): https://terminalguide.namepad.de/mode/p1007/
- jvns, "Two ways the mouse wheel works in the terminal": https://jvns.ca/til/two-ways-the-mouse-wheel-works-in-the-terminal/
- iTerm2 supported escapes + precedence note: https://iterm2.com/feature-reporting and https://iterm2.com/documentation-preferences-profiles-terminal.html
- Windows Terminal alternate-scroll default-on + precedence fix: https://github.com/microsoft/terminal/pull/16535
- kitty translates unconditionally (no 1007 in screen.c): https://github.com/kovidgoyal/kitty
- tmux ignores inner 1007; WheelUpPane + #{alternate_on} pattern: https://github.com/tmux/tmux/issues/1302
- xterm.js (Cursor/VS Code) alternate-scroll status: https://github.com/xtermjs/xterm.js/issues/5194 and https://github.com/xtermjs/xterm.js/discussions/5063
- Codex emits 1007 at runtime: https://github.com/openai/codex/issues/2836 (plus local binary sweep: `[?1007h/l` present, no 1000/1002/1003/1006 mouse-capture escapes)
- bubbletea v2.0.8 raw-escape seam: raw.go (tea.Raw/RawMsg → in-loop execute), cursed_renderer.go mode lifecycle (enumerated set/reset only)
- x/ansi v0.11.7 generic mode machinery: mode.go SetMode/ResetMode over DECMode(int)
```

- [ ] **Step 5: Run the affected suites**

Run: `go test ./internal/cli/dashboard/... -race -count=1`
Expected: PASS (registry-shape tests, footer test, help render tests).

- [ ] **Step 6: Commit**

```bash
git add internal/cli/dashboard/actions/actions.go internal/cli/dashboard/dashboard.go internal/cli/dashboard/dashboard_test.go internal/cli/dashboard/views/reading.go docs/01-dashboard-hub-spec.md docs/decisions/21-adr-alternate-scroll.md
git commit -m "docs(dashboard): scroll-keys registry row, ADR 21, spec + comment truth pass

The reading footer/help advertise the viewport scroll keys; ADR 21 records
the alternate-scroll decision with the research trail; the spec documents
the hub-wide wheel posture, the tmux caveat, and the config kill-switch;
the OSC52 comment drops its disproved Claude Code attribution."
```

---

## Verification (whole-branch, before merge)

- [ ] `go build ./...`
- [ ] `(ulimit -u 1400; go test ./... -race -count=1)` — foreground
- [ ] `gofumpt -l .` → empty; `golangci-lint run` → 0 issues; `go tool deadcode -test ./...` → empty
- [ ] Boundary greps: engine/provider stay charm-free and UI-free
- [ ] Manual smoke handoff note (cannot be automated — no real tty in tests): wheel scrolls the reading view on iTerm2 with the setting on; wheel still hover-scrolls the browser preview; drag-select works in the reading view WITHOUT modifier keys on 1007-honoring terminals; `dashboard.alternate_scroll = false` restores old behavior; quitting leaves the shell's wheel normal (mode reset); Cursor/xterm.js cell verified as part of the cross-OS matrix.

## Explicitly recorded non-goals (with reasons — not silent deferrals)

- Activity/Doctor tab scrolling: pre-existing tracked follow-up with its own overflow-design questions (doctorview.go's deliberate sweep cap); wheel-as-arrows becomes useful there the moment that lands.
- Double-click-to-open: bubbletea v2.0.8 exposes no click-count; single click selects, `enter` opens.
- DECRQM support detection: no behavior branches on it; set-and-forget is strictly simpler and equally safe.
- Hub-wide mouse capture toggle: superseded by 1007 (Decision Record 1).

## Execution deltas (review adjudications, recorded post-merge)

Every finding from the four task reviews and the final whole-branch review landed in exactly
one of three states — fixed/built, adjudicated KEEP with the reviewer concurring, or
inherently manual. Nothing was dropped without a ruling.

**Fixed or built:**
- T4 round 1 Important — the brief's conditional "extend the registry-shape test" was skipped;
  `reading-scroll` entry added to `TestReadingRegistryRowsShape` (5f4a668).
- T4 Minors — `ForScope(ScopeReading)[0]` lead-position pin added; ADR 21 path corrected to
  `internal/config/settings.go` with the `toml:"alternate_scroll"` span unbroken (5f4a668).
- Plan-author adjudication — this plan's own §3 spec text placed "Clicking a browser list row
  selects it." inside the 1007 bullet; clicks only arrive while the preview's mouse capture is
  armed, so the sentence moved into the capture bullet as a conditional (5f4a668). The plan's
  draft wording was wrong; the landed spec is more accurate.
- Final-review Minor 1 — plain exit DECRST could clobber a 1007 the user's own terminal config
  pre-armed. Built rather than deferred: XTSAVE before arming (save+set as ONE `tea.Raw`;
  `tea.Batch` guarantees no ordering), exit writes DECRST-then-XTRESTORE so non-supporting
  terminals land on the plain-reset posture and supporting ones round-trip the user's state;
  the editor re-assert deliberately never re-saves, pinned by exact-payload equality (1245968).
- Final-review Minor 2 — `mousePrefixLines` comment overstated its frame invariant; reworded to
  name the one-cycle in-flight window and why it is benign (1245968).
- Increment-verify Minor — the teardown comment conflated "terminal ignores XTRESTORE" with the
  early-exit case where a supporting terminal restores its startup value; split (d838b01).
- Task 2 plan-text correction — the Interfaces block named `Model.alternateScrollCmd() tea.Cmd`
  while Step 4 showed the inline `Init` batch; the step body governed and the sketch was never
  built. Plan internal inconsistency, implementation correct.

**Adjudicated KEEP (reviewer concurred, not merge-blocking):**
- `reading-scroll` advertises `j/k` only: registry rows carry primary bindings by idiom; the
  full viewport set (ctrl+d/u, pgup/pgdn, g/G) is spec §4's contract. A schema change to carry
  a help-only elaboration for one row would be over-engineering.
- Twin per-row footer lit tests (`…ReadingCopyBodyLit` / `…ReadingScrollLit`): the file's idiom
  is per-row lit functions (four siblings); consolidating only the new pair would split the
  idiom. Whole-family table consolidation is legitimate future cleanup, out of this branch.
- Save/restore constant pins are literal-vs-literal by nature (the constants are hand-built —
  there is no derivation to guard, unlike the x/ansi-derived set/reset pair); the test comment
  says so honestly.

**Accepted residuals (no code answer exists; documented where they live):**
- A mouse event already in flight when an overlay opens lands one frame late and may move the
  browser cursor invisibly — benign, recoverable, and the class predates this branch (code
  comment at `mousePrefixLines` carries it).
- A child `$EDITOR` that itself emits XTSAVE for mode 1007 during the handoff could poison the
  saved slot; there is no terminal-honest defense, and never re-saving remains strictly better
  than re-saving.
- The cross-OS wheel/drag-select/kill-switch/click behavior is irreducibly manual — the smoke
  matrix in ADR 21 is the human gate's checklist.
