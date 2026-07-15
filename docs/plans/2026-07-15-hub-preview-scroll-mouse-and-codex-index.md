# Hub Preview Scroll, Mouse, and Provider Index Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the memory browser's long-memory preview scroll the modern, feature-full way — an explicit preview-focus mode (Tab / click), mouse-wheel scrolling, and an OSC 52 "copy memory" affordance so nothing is lost when mouse mode suppresses native drag-select — and make every provider's *primary index file* (Claude `MEMORY.md`, Codex `memories/MEMORY.md`) sort first, not just Claude's.

**Architecture:** Three orthogonal changes to the dashboard hub, each behind a clean contract. (1) A new provider identity method `PrimaryIndexPath()` decouples "which file is the human-facing index to show first" from the merge-policy `Class` the browser was overloading. (2) A preview-focus state on the `Browser` screen reuses the reading view's exact viewport keymap, gated so list actions can never fire while scrolling. (3) The root enables bubbletea v2's per-frame `MouseMode` **only while the browser preview is on screen**, routes `MouseWheelMsg`/`MouseClickMsg` to the browser by column, and an OSC 52 copy-memory action (mirroring the shipping copy-path pattern) restores clipboard capability inside the browser.

**Tech Stack:** Go; `charm.land/bubbletea/v2 v2.0.8`, `charm.land/bubbles/v2 v2.1.1` (viewport, key), `charm.land/lipgloss/v2 v2.0.5`, `github.com/charmbracelet/x/ansi`.

## Global Constraints

- **Cross-OS parity is a hard requirement.** The app must behave on the agreed targets: **macOS** (zsh), **Linux/Ubuntu** (bash), and **WSL2 / Windows Terminal** (bash). No OS-specific syscalls or build tags are introduced. Mouse tracking and clipboard are **terminal escape sequences**, uniform across all three: mouse via xterm SGR/cell-motion, clipboard via OSC 52. Scrolling uses **explicit `tea.MouseWheelMsg`** (deterministic on all three), **never** terminal-dependent alternate-scroll mode 1007 (Alacritty ships it off). OSC 52 is best-effort — it may be silently dropped by a terminal that disables it or by tmux without `allow-passthrough`; that is acceptable and already how copy-path behaves.
- **bubbletea v2.0.8 API (verified against the module source):** mouse mode is the `MouseMode` field on the `tea.View` returned by the root `View()` — one of `tea.MouseModeNone` (default), `tea.MouseModeCellMotion`, `tea.MouseModeAllMotion`. There are **no** `WithMouseCellMotion`/`EnableMouseCellMotion` v1 options. Mouse events arrive as `tea.MouseWheelMsg` and `tea.MouseClickMsg` (both satisfy `tea.MouseMsg`); read coordinates/button via `msg.Mouse()` → `.X`, `.Y`, `.Button` (`tea.MouseWheelUp`/`tea.MouseWheelDown`/`tea.MouseLeft`). Clipboard: `tea.SetClipboard(string)` emits OSC 52 (best-effort, no ack).
- **Merge gate (must pass before any merge to develop):** `golangci-lint cache clean` → `go build ./...` → `(ulimit -u 1400; go test ./... -race -count=1)` FOREGROUND tee'd to a log (expect every package `ok`, 0 FAIL/panic/DATA RACE) → `gofumpt -l .` empty → `golangci-lint run` 0 issues → `go tool deadcode -test ./...` empty → boundaries: engine imports no cli/daemon; no charmbracelet/charm.land in engine/daemon/provider/repo/crypto/gitx non-test.
- **Test hygiene (ADR 15):** stdlib `testing` + `go-cmp` only; table-driven; `t.Parallel()`; `t.TempDir()`. No new `//nolint`. No PR numbers / reviewer names / review timestamps / dates in code comments or test names — WHY in comments, WHO/WHEN in commit messages.
- **Actions registry is the single source of truth (spec §14):** every user-visible key gets an `actions.Action` row; the footer, `ctrl+k` palette, and `?` help all render from `actions.Registry()`. A key must never be advertised on one surface and silent on another.
- **Never commit to `main` (ADR 11).** Work happens on a branch off `develop`.

---

## File Structure

- `internal/provider/provider.go` — add `PrimaryIndexPath()` to the `Provider` interface (contract).
- **Four** `Provider` implementers must gain the method or `go test ./...` fails to compile (`go build ./...` alone will look green — the fourth is a test fake): `internal/provider/claude/claude.go`, `internal/provider/codex/codex.go`, `internal/provider/providertest/fake.go`, **and `internal/cli/init_test.go`'s `fakeProvider`** (~line 1141-1163).
- `internal/cli/dashboard/memoryfs/memoryfs.go` — add `Memory.IsIndex`, set at enumeration.
- `internal/cli/dashboard/views/browser.go` — sort on `IsIndex`; preview-focus state + focused keymap; `WantsMouse()`; `MouseWheelMsg`/`MouseClickMsg` handling; copy-memory emit.
- `internal/cli/dashboard/views/reading.go` — copy-memory-body emit (mirror copy-path).
- `internal/cli/dashboard/views/keymap.go` — new `DashboardKeymap` bindings.
- `internal/cli/dashboard/actions/actions.go` — new registry rows.
- `internal/cli/dashboard/dashboard.go` — root `MouseMode` gating; `MouseWheelMsg`/`MouseClickMsg` routing to the stack; `CopyMemoryMsg` handler; **and the `available(id)` whitelist (~line 1855) — the three new runner-less IDs must be added to the unconditional-`true` branch, or `stackFooterRows` renders them struck-through (spec §14 honesty violation).**
- Tests: `internal/provider/claude/claude_test.go` + `internal/provider/codex/codex_test.go` (real-adapter `PrimaryIndexPath` values), `memoryfs_test.go`, `browser_test.go`, `reading_test.go`, `dashboard_test.go`.

---

## Task 1: Provider `PrimaryIndexPath()` contract + Codex index sorts first

**Problem:** `browser.go`'s `isDerivedIndex` keys the "sort the index first" rule on `provider.ClassDerivedIndex`, a **merge-policy** class only Claude's `MEMORY.md` carries. Codex's index (`memories/MEMORY.md`) is legitimately `ClassRegenerated` (agent-brain does not reconcile it; it is newest-wins), so it never floats to the top. The display concern must not ride a merge-policy class.

**Files:**
- Modify: `internal/provider/provider.go` (interface + doc)
- Modify: `internal/provider/claude/claude.go`, `internal/provider/codex/codex.go`, `internal/provider/providertest/fake.go`
- Modify: `internal/cli/init_test.go` (~line 1141-1163) — the `fakeProvider` type is a fourth `Provider` implementer; add the method or `go test ./internal/cli/...` won't compile
- Modify: `internal/cli/dashboard/memoryfs/memoryfs.go:107-145` (`listUnit`), `:43-55` (`Memory`)
- Modify: `internal/cli/dashboard/views/browser.go:740-770`
- Test: `internal/provider/claude/claude_test.go`, `internal/provider/codex/codex_test.go` (real adapters), `internal/cli/dashboard/memoryfs/memoryfs_test.go`, `internal/cli/dashboard/views/browser_test.go`

**Interfaces:**
- Produces: `provider.Provider.PrimaryIndexPath() string` — the provider's human-facing index file, expressed in **pattern-glob space** (the same `classifyRel(RepoSubdir, rel)` namespace the `Patterns()` globs live in), or `""` if the provider has none. Claude → `"MEMORY.md"`; Codex → `"memories/MEMORY.md"`.
- Produces: `memoryfs.Memory.IsIndex bool` — true when this file is its provider's `PrimaryIndexPath()`.

- [ ] **Step 1: Add the contract method to the interface.** In `provider.go`, add to the `Provider` interface (after `Patterns()`):

```go
	// PrimaryIndexPath is the provider's human-facing index/entry-point
	// memory, expressed in the same path space as Patterns() globs
	// (classifyRel(RepoSubdir, rel)): the file a reader should see first
	// and open first. Claude's "MEMORY.md", Codex's "memories/MEMORY.md".
	// Distinct from Class (merge policy) on purpose — an index rides
	// whatever merge class its provider assigns; being the index is a
	// display/identity fact, not a merge fact. "" when the provider has
	// no distinguished index.
	PrimaryIndexPath() string
```

- [ ] **Step 2: Implement on all four implementers.** Claude (`claude.go`, near `Patterns`): `func (a *Adapter) PrimaryIndexPath() string { return "MEMORY.md" }`. Codex (`codex.go`, near `Patterns`): `func (a *Adapter) PrimaryIndexPath() string { return "memories/MEMORY.md" }`. Fake (`fake.go`): its constructor is `New(name, scope, patterns)` (3-arg) with ~28 existing callers — do **not** change that signature. Add an unexported field `primaryIndexPath string`, `func (f *Fake) PrimaryIndexPath() string { return f.primaryIndexPath }`, and a chainable non-breaking setter `func (f *Fake) WithPrimaryIndex(p string) *Fake { f.primaryIndexPath = p; return f }` so a test opts in with `providertest.New(...).WithPrimaryIndex("MEMORY.md")`. Default `""` keeps every existing fake-based test unchanged. **`fakeProvider`** in `internal/cli/init_test.go` (~line 1152, after its `Patterns`): `func (f *fakeProvider) PrimaryIndexPath() string { return "" }` — required for the cli package tests to compile.

- [ ] **Step 3: Run provider tests to verify they compile/fail as expected**, then add `PrimaryIndexPath` assertions in the **real-adapter** test files: `internal/provider/claude/claude_test.go` (`== "MEMORY.md"`) and `internal/provider/codex/codex_test.go` (`== "memories/MEMORY.md"`). Note `internal/provider/provider_test.go` exercises `providertest` fakes, not the real adapters, so the constant assertions belong in the adapter packages. Run: `go test ./internal/provider/...`

- [ ] **Step 4: Enumerate `IsIndex` in memoryfs.** In `listUnit`, factor the classify path into a local so it is computed once and reused (it currently inlines `classifyRel(unit.RepoSubdir, rel)` at the `Classify` call):

```go
		rel = filepath.ToSlash(rel)
		classifyPath := classifyRel(unit.RepoSubdir, rel)
		class := provider.Classify(prov, classifyPath)
		if class == provider.ClassIgnore {
			return nil
		}
		// ...
		out = append(out, Memory{
			// ...existing fields...
			Class:   class,
			IsIndex: prov.PrimaryIndexPath() != "" && classifyPath == prov.PrimaryIndexPath(),
			// ...
		})
```

Add `IsIndex bool` to the `Memory` struct with a doc line: `// IsIndex marks the provider's PrimaryIndexPath file — the browser sorts it first within its provider group (a display fact, independent of Class).`

- [ ] **Step 5: Write the failing memoryfs test.** In `memoryfs_test.go`, a table test with a fake per-project provider (`PrimaryIndexPath()=="MEMORY.md"`) enumerating a dir with `MEMORY.md` + two other files, asserting exactly the `MEMORY.md` row has `IsIndex==true`. Add a second case with `RepoSubdir=="memories"` and `PrimaryIndexPath()=="memories/MEMORY.md"` proving the RepoSubdir-prefixed match. Run: `go test ./internal/cli/dashboard/memoryfs/ -run IsIndex -v` — expect FAIL before Step 4 is wired, PASS after.

- [ ] **Step 6: Switch the browser sort to `IsIndex`.** In `browser.go`, rename `isDerivedIndex` → `isIndex`, body `return m.IsIndex`, drop the now-unneeded `provider` reference in that helper (the `provider` import stays — `Memory.Class` still carries `provider.Class` elsewhere). Update the `visibleRows` sort comment to state the rule keys on the provider-declared index (`IsIndex`), so it holds for **every** provider that declares one — Claude and Codex today — not just `ClassDerivedIndex`.

- [ ] **Step 7: Migrate + extend the browser sort test.** In `browser_test.go`, `TestBrowserIndexMemorySortsFirst` (~line 1680) currently builds its index rows with `Class: provider.ClassDerivedIndex` (~1686 claude, ~1689 codex) and asserts on `Class` (~1706). After Step 6 keys the sort on `IsIndex`, those rows sort like any other unless migrated. **Migrate:** set `IsIndex: true` on the index rows; give the **codex** index row the realistic `Class: provider.ClassRegenerated` (not `ClassDerivedIndex`) so the fixture actively proves the decoupling — a `ClassRegenerated` file still sorts first purely on `IsIndex`; update the ~1706 assertion to check `IsIndex`/order rather than `Class`. Keep both order modes (`o` toggle) in the table. Run: `go test ./internal/cli/dashboard/views/ -run IndexMemorySortsFirst -v` — expect PASS.

- [ ] **Step 8: Commit.** `git add` the provider, memoryfs, browser, and test files; `git commit -m "feat(provider): declare each provider's primary index; sort it first for codex too"` (+ Co-Authored-By trailer).

---

## Task 2: Preview-focus mode (keyboard)

**Problem:** In the two-pane browser, `j/k` drive the list, so the preview can only be scrolled with `ctrl+d/u` — non-obvious, and there is no way to give the preview the full reading toolkit. Modern list+preview TUIs (lazygit) solve this with an explicit **focus the preview pane** action; once focused, the ordinary scroll keys scroll it.

**Files:**
- Modify: `internal/cli/dashboard/views/browser.go` — `previewFocused` state, `browserPreviewFocusedKeyMap()`, `updateKey` routing, focus reset points, focus cue in `renderPreviewPane`, `Title`/footer wiring.
- Modify: `internal/cli/dashboard/actions/actions.go` — `browser-focus-preview` row; update `browser-scroll-preview` copy.
- Modify: `internal/cli/dashboard/views/keymap.go` — `BrowserFocusPreview` binding.
- Test: `internal/cli/dashboard/views/browser_test.go`

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces: `Browser.previewFocused bool` (read by Task 3's click handler); focused-preview keymap.

- [ ] **Step 1: Add the focused keymap.** In `browser.go`, add a keymap mirroring `reading.go`'s `readingViewportKeyMap()` exactly (`up/k`, `down/j`, `ctrl+u`, `ctrl+d`, `pgup`, `pgdown`; `Left`/`Right` unbound). Name it `browserPreviewFocusedKeyMap()`. This is the set installed on `previewViewport` **while focused**; the existing `browserPreviewKeyMap()` (ctrl+d/u + pgup/pgdown only, no j/k) stays the **unfocused** keymap so `j/k` remain the list cursor when the list is focused.

- [ ] **Step 2: Add `previewFocused` state + Tab binding.** Add field `previewFocused bool` to `Browser`. Add `BrowserFocusPreview keybinding.Binding` to `DashboardKeymap` resolved from a new registry row (Step 5). Tab toggles it, but **only when a preview is shown**.

- [ ] **Step 3: Route keys by focus in `updateKey`.** Before the list-action switch, gate on **both** `previewFocused` and `previewShown` — a narrow-terminal resize sets `previewShown=false` while `previewFocused` stays `true`, and without the `&& b.previewShown` guard the focused block would keep swallowing `j/k/Tab/Esc` into an off-screen viewport, leaving the list unresponsive:

```go
	if b.previewFocused && b.previewShown {
		switch {
		case keybinding.Matches(msg, DashboardKeys.BrowserFocusPreview), keybinding.Matches(msg, DashboardKeys.BrowserBack):
			b.previewFocused = false
			return b, nil
		case msg.String() == "g":
			b.previewViewport.GotoTop()
			return b, nil
		case msg.String() == "G":
			b.previewViewport.GotoBottom()
			return b, nil
		}
		b.previewViewport.KeyMap = browserPreviewFocusedKeyMap()
		var cmd tea.Cmd
		b.previewViewport, cmd = b.previewViewport.Update(msg)
		return b, cmd
	}
```

In the normal (list-focused) branch, handle `BrowserFocusPreview`: `if b.previewShown { b.previewFocused = true }; return b, nil` — and if `!previewShown`, Tab is inert. Keep the existing unfocused `ctrl+d/u` passthrough (`if b.previewShown { previewViewport.Update }`) so quick half-page scroll from the list still works. Ensure the unfocused keymap is (re)installed on the viewport when not focused (set it in `NewBrowser` as today; re-assert in the unfocused passthrough is unnecessary if focus toggling always restores it — restore it when clearing focus).

- [ ] **Step 4: Reset focus on every exit from the normal body.** Set `b.previewFocused = false` when: entering filter mode (`updateKey`'s `BrowserFilter` case), entering deleted mode (`enterDeleted`), and whenever `View` runs with `previewShown==false` (a narrow resize that drops the preview must not leave a dangling focus — clear it at the top of `renderList`/the no-preview path, or guard reads on `previewShown && previewFocused`). Simplest correct rule: treat effective focus as `previewFocused && previewShown` everywhere it is read, and clear the bool in the filter/deleted transitions.

- [ ] **Step 5: Register the action + keep it lit.** In `actions.go` add, in the ScopeBrowser block:

```go
	{ID: "browser-focus-preview", Title: "focus preview", Keys: []string{"tab"}, KeyHint: "tab", Scope: ScopeBrowser},
```

Wire `BrowserFocusPreview: bindingFor("browser-focus-preview")` in `keymap.go`. **Add `"browser-focus-preview"` to the unconditional-`true` branch of `available(id)` in `dashboard.go` (~line 1855, beside `browser-scroll-preview`)** — otherwise `stackFooterRows` (which computes `disabled := !m.available(action.ID)`) renders it struck-through even though the key works (spec §14 honesty). For the existing `browser-scroll-preview` row, change **only its code comment** to reflect that `ctrl+d/u`+`pgup/pgdn` scroll from the list and `tab` enters full-key scrolling — do **not** change its `Title` ("scroll") or `KeyHint` ("ctrl+d/u"): `TestStackFooterAdvertisesScopedKeys` (`dashboard_test.go`) pins the exact footer substring `"ctrl+d/u scroll"`.

- [ ] **Step 6: Focus cue.** In `renderPreviewPane`, when `b.previewFocused`, render the preview with a visible focused affordance — a highlighted/bordered title or an indicator in the scroll hint line (reuse `b.deps.Styles`; do not invent a new palette entry if an emphasis style exists). The unfocused pane renders as today. Keep it within the existing height budget (no extra line that would break the wave-2 footer guarantee).

- [ ] **Step 7: Tests.** In `browser_test.go`: (a) Tab with a preview shown sets focus and `j`/`k` then move the viewport (assert `YOffset` or that "row 001" scrolls out), not the list cursor; (b) Tab again / Esc clears focus and `j`/`k` move the list cursor again; (c) Tab with no preview (narrow width) is inert; (d) entering filter or deleted mode clears focus. Drive via `tea.KeyPressMsg` as the suite already does. In `dashboard_test.go`, add a footer assertion that `browser-focus-preview` renders **lit, not struck** in the browser stack footer (the existing scoped-footer tests are subset checks that would miss a struck row — this closes the `available()` gap the reviewer flagged). Run: `go test ./internal/cli/dashboard/... -run 'Preview|Focus|Footer' -v`.

- [ ] **Step 8: Commit.** `git commit -m "feat(dashboard): focus the browser preview pane with tab for full-key scrolling"`.

---

## Task 3: Mouse wheel + click-to-focus (scoped to the browser)

**Problem:** Mouse reporting is never enabled (`tea.NewProgram` at `internal/cli/dashboard.go:153` — note: the CLI command file, **not** the `internal/cli/dashboard/dashboard.go` model file edited below — passes no mouse option, and v2 sets mode per-frame anyway), so the wheel does nothing. Enabling it globally would suppress native drag-select everywhere; v2's per-frame `MouseMode` lets us scope it to the one screen that needs it. No change to `internal/cli/dashboard.go` is required — the mode is set on the `tea.View` the model returns.

**Files:**
- Modify: `internal/cli/dashboard/dashboard.go:686` (`Update` — add `MouseWheelMsg`/`MouseClickMsg` cases) and `:2030-2072` (`View` — gate `MouseMode`).
- Modify: `internal/cli/dashboard/views/browser.go` — `WantsMouse()`, `updateMouse` (wheel scroll + click focus by column).
- Test: `internal/cli/dashboard/views/browser_test.go`, `internal/cli/dashboard/dashboard_test.go`

**Interfaces:**
- Consumes: `Browser.previewShown`, `Browser.previewFocused` (Task 2).
- Produces: `Browser.WantsMouse() bool`.

- [ ] **Step 1: `WantsMouse` on the browser.** `func (b *Browser) WantsMouse() bool { return b.previewShown }` — with a doc noting it reflects the **last** `View` (previewShown is set during render), which is exactly when the root reads it (root calls `top.View` before constructing the `tea.View`).

- [ ] **Step 2: Gate `MouseMode` in the root `View`.** In the `default` branch, only on the pushed-stack path, compute whether mouse is wanted, then apply after `tea.NewView`:

```go
	mouseWanted := false
	// ...inside default, where `top, ok := m.stackTop()` is in scope and top.View already ran:
		if browser, isBrowser := top.(*views.Browser); isBrowser && browser.WantsMouse() {
			mouseWanted = true
		}
	// ...after view := tea.NewView(body):
	view.AltScreen = true
	view.WindowTitle = "agent-brain dashboard"
	if mouseWanted {
		view.MouseMode = tea.MouseModeCellMotion
	}
```

Mouse stays `MouseModeNone` for the daemon-down / help / palette / search-overlay branches and for every tab and non-browser screen — native selection intact everywhere but the browser preview.

- [ ] **Step 3: Route mouse messages to the stack.** In the root `Update` type switch, add:

```go
	case tea.MouseWheelMsg, tea.MouseClickMsg:
		// Mouse is enabled only while a browser preview is on screen (View's
		// MouseMode gate); forward to the stack top, which is that browser.
		// Other screens never receive mouse because the mode is off there.
		if _, ok := m.stackTop(); ok {
			return m.forwardToStack(msg)
		}
		return m, nil
```

- [ ] **Step 4: Handle mouse in the browser.** Add a `case tea.MouseWheelMsg` and `case tea.MouseClickMsg` to `Browser.Update` (guarded to the normal body: ignore while `filtering` or `showDeleted`). Implement `updateMouse`:

```go
	// Column geometry mirrors View: list occupies [0,listPaneWidth); the
	// preview starts after the two-space gap at listPaneWidth+2. X-origin is
	// 0 for both root and browser (the root joins vertically), so column
	// routing needs no absolute offset.
	func (b *Browser) overPreview(x int) bool { return b.previewShown && x >= listPaneWidth+2 }
```

Wheel: if `overPreview(mouse.X)`, scroll the preview a few lines per notch (`b.previewViewport.ScrollDown(3)` / `ScrollUp(3)` on `tea.MouseWheelDown`/`Up`; use the viewport's line-scroll method available in bubbles v2.1.1 — verify the exact name and use it, do not hand-roll offset math). Else (wheel over the list) move the list cursor one row per notch (`b.moveCursor("down")`/`"up"`). Click (`tea.MouseLeft`): `b.previewFocused = b.overPreview(mouse.X)` — clicking the preview focuses it, clicking the list returns focus to it. (Row-precise click-selection is intentionally out of scope: the browser is composed by the root and does not know its absolute Y origin, so mapping a click to a specific list row would require a new root→screen offset seam. Note this as a follow-up, do not fake it.)

- [ ] **Step 5: Tests.** `MouseWheelMsg`/`MouseClickMsg` are `type … Mouse` with exported `X, Y int; Button MouseButton` fields — construct directly: `tea.MouseWheelMsg{X: <col>, Button: tea.MouseWheelDown}` and `tea.MouseClickMsg{X: <col>, Button: tea.MouseLeft}`; the handler reads them via `msg.Mouse()`. Browser: a wheel-down in the preview column (`X >= listPaneWidth+2`) scrolls the preview; the same over the list column (`X < listPaneWidth`) moves the cursor; a left click in the preview column sets `previewFocused`, in the list column clears it. Root: a wheel message with a browser pushed forwards to the stack; with no stack it is a no-op. Add a root `View` assertion that `MouseMode == MouseModeCellMotion` when a browser with a preview is on top and `MouseModeNone` on a bare tab / with help open. Run: `go test ./internal/cli/dashboard/... -run 'Mouse' -v`.

- [ ] **Step 6: Verify terminal teardown.** Confirm (by reading `program.Run`'s restore path, already relied on for AltScreen) that a clean quit resets `MouseMode` to none before `maybeReExec` runs `syscall.Exec` — the renderer diffs `MouseMode`→`None` on the final frame/close, so no explicit disable is needed. Note the finding in the task report; add no code unless the reset is absent.

- [ ] **Step 7: Commit.** `git commit -m "feat(dashboard): wheel-scroll and click-to-focus the browser preview; mouse scoped to that screen"`.

---

## Task 4: OSC 52 "copy memory" — the feature-full answer to selection loss

**Problem:** While mouse mode is on in the browser, native drag-select is suppressed there (an xterm-protocol constraint, not ours). The modern feature-full remedy (what Claude Code CLI does) is app-level clipboard: give the user a key that copies the memory to the system clipboard via OSC 52 — which also works over SSH/tmux/WSL2, better than drag-select. We already ship this exact pattern for copy-path (`CopyPathMsg` → `tea.SetClipboard`, `dashboard.go:881-890`).

**Files:**
- Modify: `internal/cli/dashboard/views/browser.go` — `browser-copy` key → emit `CopyMemoryMsg` for the selected memory's body.
- Modify: `internal/cli/dashboard/views/reading.go` — `reading-copy-body` key → same, for the open memory.
- Modify: `internal/cli/dashboard/views/reading.go` (or a shared `messages` location) — declare `CopyMemoryMsg{Body string, Label string}` beside `CopyPathMsg`.
- Modify: `internal/cli/dashboard/dashboard.go` — `case views.CopyMemoryMsg` mirroring the `CopyPathMsg` handler (toast + `tea.SetClipboard`).
- Modify: `internal/cli/dashboard/actions/actions.go`, `internal/cli/dashboard/views/keymap.go` — `browser-copy` (`y`) and `reading-copy-body` (`Y`) rows/bindings.
- Modify docs: `docs/01-dashboard-hub-spec.md` §3/§14 (or the doc that lists browser keys) — document copy-memory and the mouse-selection bypass (Option-drag macOS / Shift-drag Linux+Windows Terminal, or Enter into the reading view).
- Test: `internal/cli/dashboard/views/browser_test.go`, `reading_test.go`, `internal/cli/dashboard/dashboard_test.go`.

**Interfaces:**
- Consumes: the copy-path pattern (`CopyPathMsg`), `Browser.Selected()`, `Browser.deps.ReadBody`.
- Produces: `views.CopyMemoryMsg`.

- [ ] **Step 1: Declare the message.** Beside `CopyPathMsg`: `type CopyMemoryMsg struct{ Body string; Label string }` with a doc mirroring `CopyPathMsg`'s (best-effort OSC 52; `Label` is the memory name for the toast).

- [ ] **Step 2: Emit from the browser.** Add a `browser-copy` (`y`) case in `updateKey` (list-focused branch): read the selected memory's body via `b.deps.ReadBody(memory)`; on success emit `CopyMemoryMsg{Body: body, Label: memory.Name}`; on error emit an error toast the same way other browser read errors surface. Use the existing `selectedRequest` helper shape where it fits, or a small dedicated method (`ReadBody` returns `(string, error)`, so a dedicated `copyRequest` cmd is cleanest).

- [ ] **Step 3: Emit from the reading view.** Add a `reading-copy-body` (`Y`) case in `Reading.updateKey`: emit `CopyMemoryMsg{Body: r.body, Label: r.deps.Memory.Name}` — `r.body` (reading.go:86, set by `adoptBody`) is the raw source it renders from, not the glamour-styled string.

- [ ] **Step 4: Handle in the root.** Add `case views.CopyMemoryMsg` next to `CopyPathMsg`: push a confirmation toast (`copied "<Label>" to clipboard`) and return `tea.SetClipboard(msg.Body)`. Best-effort, exactly like copy-path.

- [ ] **Step 5: Register keys + keep them lit.** `actions.go`: `{ID: "browser-copy", Title: "copy", Keys: []string{"y"}, KeyHint: "y", Scope: ScopeBrowser}` and `{ID: "reading-copy-body", Title: "copy body", Keys: []string{"Y"}, KeyHint: "Y", Scope: ScopeReading}` (reading already has `reading-copy-path` on `y`; body is `Y`). Wire both bindings in `keymap.go`. Confirm no collision: `y` is free in ScopeBrowser; `Y` is free in ScopeReading. **Add both IDs to the unconditional-`true` branch of `available(id)` in `dashboard.go` (~line 1855, beside `reading-copy-path`)** so the footer renders them lit, not struck.

- [ ] **Step 6: Tests.** Browser `y` emits `CopyMemoryMsg` with the selected memory's body; reading `Y` emits it for the open memory; the root handler returns a non-nil cmd and pushes a toast. (Do not assert OSC 52 bytes reach a real clipboard — `tea.SetClipboard` is best-effort and untestable in unit scope; assert the cmd is issued, mirroring `dashboard_test.go`'s copy-path test which checks `slices.Contains(drain(cmd), tea.SetClipboard(want)())`, and `reading_test.go`'s `cmd().(CopyPathMsg)` shape.) Add a footer assertion that `browser-copy` and `reading-copy-body` render lit (not struck) in their scopes. Run: `go test ./internal/cli/dashboard/... -run 'Copy|Footer' -v`.

- [ ] **Step 7: Docs + cross-platform manual-test matrix.** Update the spec/doc key list. Record a manual test matrix in the task report to be run on the dev build: **macOS** (Terminal.app + iTerm2), **Linux** (gnome-terminal + one of alacritty/kitty), **WSL2/Windows Terminal** — for each: wheel scrolls the preview; `y` copy lands in the system clipboard (note tmux needs `allow-passthrough`); Option-drag (macOS) / Shift-drag (Linux, Windows Terminal) still selects text inside the browser; native selection is untouched on the Projects tab and the reading view.

- [ ] **Step 8: Commit.** `git commit -m "feat(dashboard): copy a memory to the clipboard via OSC 52 from the browser and reading view"`.

---

## Self-Review

**Spec coverage:** Item 1 (Codex index-first) → Task 1. Item 2 (intuitive preview scroll keys) → Task 2 (focus mode; the modern lazygit pattern; `w/a/s/d` deliberately not used — non-standard for a TUI and `d`=delete). Item 3 (mouse wheel like Claude Code) → Task 3, with the selection-loss remedy in Task 4. Cross-OS constraint → Global Constraints + Task 4 Step 7 matrix.

**Placeholder scan:** every code step carries concrete code or an exact symbol to add; the two "verify the exact method/field name against the module" notes (viewport line-scroll method; `tea.Mouse` field names) are explicit instructions to the implementer to confirm against bubbles/bubbletea v2 source, not vague hand-waving — the implementer has the module cache.

**Type consistency:** `PrimaryIndexPath() string` used identically across interface + 3 impls + memoryfs. `Memory.IsIndex bool` set in Task 1, read in Task 1 Step 6. `previewFocused` produced in Task 2, consumed in Task 3 Step 4. `WantsMouse()` produced in Task 3 Step 1, consumed Step 2. `CopyMemoryMsg` produced in Task 4 Step 1, consumed Steps 2-4.

**Ordering:** Task 3 depends on Task 2 (`previewFocused`); Task 4 is independent of 2/3 but shares the actions registry — execute 1 → 2 → 3 → 4.

**Blast radius / bugs resolved beyond the three asks:** the inaccurate `isDerivedIndex` comment (claimed generality it did not have) is corrected in Task 1; focus state is defended against filter/deleted/narrow-resize leaks (Task 2 Step 3-4); mouse is defended against firing on non-browser screens and under overlays (Task 3 Step 2); terminal teardown across re-exec is verified (Task 3 Step 6).

**Adversarial review incorporated (against live code, before commit):** (1) all three new runner-less action IDs added to the `available()` whitelist so the footer renders them lit, not struck (spec §14) — File Structure, Task 2 Step 5, Task 4 Step 5, plus lit-not-struck footer assertions in Task 2/4 tests; (2) the fourth `Provider` implementer `fakeProvider` in `internal/cli/init_test.go` added to Task 1 (else `go test ./...` fails to compile); (3) the focused-key block gated on `previewFocused && previewShown` to survive a narrow resize (Task 2 Step 3); (4) `TestBrowserIndexMemorySortsFirst` fixture migration from `Class`-keyed to `IsIndex`-keyed, with the codex row given a realistic `ClassRegenerated` to actively prove the decoupling (Task 1 Step 7); (5) the pinned `"ctrl+d/u scroll"` footer substring preserved by touching only the `browser-scroll-preview` comment (Task 2 Step 5); (6) real-adapter `PrimaryIndexPath` assertions placed in `claude_test.go`/`codex_test.go`, not `provider_test.go` (Task 1 Step 3); (7) the `tea.NewProgram` file reference disambiguated (`internal/cli/dashboard.go`, not the model file) (Task 3 Problem).
