# Hub Acceptance Hotfix 4 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the four hub defects Sawmon's round-4 acceptance pass surfaced (floating footer, mute lint badges + preview misalignment, preview-focus "freeze", no native text selection) and close every remaining our-side residual in the same wave (M4 stability-gate hardening, hover-scroll battery cell, doctor/activity tab scrolling).

**Architecture:** All UI fixes land at established seams: the root View's height compose (one fill-to-budget primitive pins the footer for every screen and tab), the browser's preview pane (header zone for lint reasons + alignment), the browser's focus sub-mode (footer/cue honesty), and the root's per-frame mouse arming gate (runtime toggle). Test growth extends the existing PTY battery and unit pins; no new dependencies.

**Tech Stack:** Go, bubbletea v2 (existing), bubbles viewport (existing), existing PTY battery (x/xpty + vt10x, test-only).

## Global Constraints

- Never commit to `main` (ADR 11); work lands on `feat/hub-hotfix-4`, merged to `develop`.
- Tests: stdlib `testing` + `google/go-cmp` only; table-driven where natural; `t.Parallel()`; `t.TempDir()` (ADR 15).
- No `//nolint`; fix lint findings structurally.
- No PR numbers, reviewer names, review timestamps, dates, or commit hashes in code comments or test names.
- Comment density and idiom match the surrounding dashboard files (rationale-heavy; every non-obvious decision gets its WHY).
- Boundaries hold: engine/provider stay charm-free and UI-free; xpty/vt10x stay confined to `test/e2e`.
- Every new assertion proven load-bearing: negative control or temporary mutation shown red then reverted.
- No bare sleeps as synchronization; PTY waits are poll-until-predicate with deadlines, failing with raw-tail + screen dump.
- Full gate before merge: `go build ./...`, `(ulimit -u 1400; go test ./... -race -count=1)` FOREGROUND, `gofumpt -l .` empty, `golangci-lint run` 0 issues, `go tool deadcode -test ./...` empty, boundary greps clean, `go test ./test/e2e/ -race -count=3 -run 'TestPTY'` clean.
- Commit messages end with the `Co-Authored-By:` trailer the harness specifies.

## Orientation (verified facts, with sources — recon run at develop 04cd408)

- Root compose joins `header + breadcrumb/tabBar + body + footer` with `"\n\n"` (`internal/cli/dashboard/dashboard.go:2163` pushed-screen path, `:2166` tab path). `fitHeight` (`dashboard.go:1695-1708`) CLAMPS an over-budget body to `maxLines` but never pads a short one — so when the browser body is short (short list beside a short preview: `renderPreviewPane` deliberately sizes a fitting preview to its own content, `views/browser.go:1318-1326`), the footer floats directly under the content and jumps as the selected memory's preview height changes. That is defect 1's whole mechanism.
- Body budgets: `m.stackBodyHeight()` / `m.tabBodyHeight()` (callers at `dashboard.go:2155`, `:2165`) already define the exact line budget the body region owns; header/toast lines are grouped into `header` above (`:2140-2144`).
- Lint reasons already exist in memory: `lint.Issue{Rule, Detail}` (`internal/cli/dashboard/lint/lint.go:31-34`, Detail is a "human sentence naming the specifics"); the browser retains BOTH `lintFlags` (RepoPath→bool badge source, `views/browser.go:133`) and `lintResults` (the full `lint.Check` output the flags are derived from, `:140-142`). The ⚠ badge renders via `fitListRow` (`:1267-1271`, `:1448`); nothing anywhere displays `Detail`.
- Preview pane: `renderPreviewPane` (`views/browser.go:1300-1338`) feeds glamour output straight into `previewViewport`; the "----" rule Sawmon sees is the memory's own frontmatter rendered by glamour — the pane has NO header zone of its own. `chromeLines()` is subtracted from the browser-body budget for the pane height (`:1314`). `previewScrollHint` renders ONLY when content overflows (`:1336-1337`).
- Preview focus: Tab or a click over the pane sets `previewFocused` (`views/browser.go:499`, `updateMouseClick` `:806-821`); the focused keymap swaps in lazily; `blurPreview` (`:295`) restores. The ONLY visible cue is the scroll hint — which does not render for a fitting preview — and the root footer keeps showing the unfocused browser scope's keys (`footerBindings`, `dashboard.go:2248-2252`). Net: click a short memory's preview → zero visible change, list keys stop moving the cursor → reads as a total freeze until esc. (A prior review adjudicated the inert footer "not a violation"; live user evidence overturns that ruling — this wave implements the honest footer.)
- Mouse arming: per-frame gate at `dashboard.go:2156-2161` — `WantsMouse()` read AFTER `top.View` — sets `view.MouseMode = tea.MouseModeCellMotion` (`:2177`); every non-browser frame arms None. While CellMotion is armed the terminal does NOT do native drag-select — that is defect 4's mechanism, previously answered only by y/Y OSC52 copy + Option/Shift-drag (spec §3).
- Doctor/activity tabs render cursorless top-windows (`views/doctorview.go`, `views/activity.go`, both render through `m.activeBody()` at `dashboard.go:2182` and get root-clamped) — the documented P5 limitation this wave closes with viewport scrolling.
- PTY battery: `test/e2e/ptyharness_test.go` (hubSession: `send`, `waitScreen`, `waitRaw`, `scrollByWheel`, `quitAndDrain`, seeded `long-scroll-target.md` line-001..line-200) and `test/e2e/pty_hub_test.go` (six scenarios). `waitStableMaxVisibleLineNumber` currently requires TWO consecutive equal reads (`ptyharness_test.go`, the wheel tests' pre-notch gate).
- SGR wheel wire encodings: wheel-up `\x1b[<64;COL;ROWM`, wheel-down `\x1b[<65;COL;ROWM` (press form; wheel has no release). SGR click: press `\x1b[<0;COL;ROWM`, release `\x1b[<0;COL;ROWm` — already used by `TestPTYClickBytesSelectBrowserRow`.
- Keymap collisions: check `views/keymap.go` + `actions/actions.go` before binding any new key. `m` is believed free in ScopeBrowser; verify empirically (registry shape test enumerates every binding).

## Decision records

1. **Fill at the root, not per-view:** the footer pin belongs at the one seam every frame passes through (the root compose), exactly like the hotfix-2 clamp. Per-view padding would need N views to cooperate and any regressed view would float the footer again. The preview pane's size-to-content behavior is DELIBERATE (its doc explains why) and stays.
2. **Lint reasons in the preview header zone, not a new overlay:** the reasons belong where the eye already is when hovering a flagged memory. The pane gains an owned header zone (padding + warn lines) — this simultaneously fixes the alignment complaint (preview content starts lower, in line with the list's first row).
3. **Footer honesty over cue-only:** the focused sub-mode swaps the FOOTER bindings (the user's single source of key truth) and adds an always-visible pane cue. The prior "transient sub-mode, in-pane cue discloses it" adjudication is overturned by live evidence: the cue did not render for fitting previews.
4. **Runtime mouse toggle, not a config flag:** native selection is a moment-to-moment need (grab this text now), not an installation posture. A `dashboard.alternate_scroll`-style config exists for the wheel; the selection toggle is a session action with footer disclosure. OSC52 y/Y copy stays (SSH/tmux path).
5. **M4 built proactively:** the two-read stability gate hardens to three on the user's standing directive (the recorded trigger was "first observed flake"; the user directed building actionable hardening now). The plan-deltas note in `docs/plans/2026-07-16-pty-e2e-battery.md` gets a dated supersession line — append-only, the original ruling stays visible.
6. **Doctor/activity scroll = viewport convention:** same interaction grammar as the preview/reading panes (ctrl+d/u, pgup/pgdn, g/G), cursorless (scroll only) — no new interaction concepts.
7. **Stable footers beat per-state churn (execution adjudications, T6 review):** (a) footers keep listing scroll keys even when a tab's content fits — hiding them per-selection would recreate the position-instability T1 fixed; keys no-op harmlessly. (b) The doctor pane accepts one gratuitous scroll reset when an expired quiesce deadline clears to nil: removing it needs either real-now in the scroll identity (reintroduces time-driven yanks at the expiry moment) or dropping the deadline from the identity (loses reset-on-new-quiesce) — both strictly worse.
8. **Mouse toggle is scope-broad, mode-structural (T4 review):** the toggle works across browser surfaces (list, focused, deleted) because native selection is wanted wherever text is read — but the key match lives in the browser's mode dispatch (request-msg to the root), never a mode-blind root interception, which was proven to steal `m` from filter typing. The off-state cue leads the footer line so the disclosed state is visible at narrow widths.
9. **Footers fit, never silently clip (T4 review's wave-level finding):** the browser list-scope footer exceeds 120 columns when armed; the v2 compositor clips, so trailing rows — including the toggle's own row and cue before Task 4's fix — were invisible at the battery's canonical width. Footer lines now fit by dropping whole trailing rows (registry order = priority) behind an explicit continuation marker, with state cues always visible; the `?` help overlay remains the full-truth surface.
10. **Exit affordances never drop; fitting owns the two row-based seams (T9 fix round):** the registry tail is authored for READING order — back/quit anchor the end — so reusing that order directly as drop priority inverted importance: a narrow terminal dropped the *exit* first (esc back at the canonical 120, q quit in a narrow Projects tab) while informational rows survived. A per-row never-drop flag in the registry now separates DROP eligibility from DISPLAY order. The fitter keeps every flagged exit affordance (each scope's esc/back row, the focused pane's return-to-list row, and the global q quit) and the leading state cue at any width, drops only unflagged rows from the tail, and renders the `… ?` marker at the ELISION POINT where the dropped run was (`… ? · esc back`, the marker before the exit, never after it). The `?` help row stays droppable on purpose — help discovery rides the marker's own `?`, which points at the full-truth overlay, so pinning the help row too would be redundant. Degenerate floor: if even the cue plus the exit affordances plus the marker exceed the width, they are kept over-width anyway — hiding a state cue or an exit is the worse lie. In practice this floor reaches 49 printable columns in the browser's list scope with the mouse-capture cue lit (terminals as wide as 41–48 columns still hit it, not only sub-40 ones), 14 with the cue off; the preview-focused scope's cue-off/cue-on floors are 18/53, though 53 is effectively unreachable — the preview pane itself needs 100+ columns before tab can focus it at all. Scope: this contract governs the two ROW-BASED footer seams (the pushed-screen stack footer and the bare-tab footer, which drop whole registry rows). The modal/prompt footer states (update confirm/applying, quit prompt, flow modal, Projects modals) are single fixed sentences with no rows to drop and are deliberately outside the fitter's domain — a row-fitter would replace a too-narrow prompt with `… ?` wholesale, strictly worse than the terminal's own clip.

## File Structure

- `internal/cli/dashboard/dashboard.go` — fill-to-budget primitive + compose change (T1); footer sub-mode selection (T3); mouse-capture toggle flag + gate (T4); doctor/activity key routing (T6); width-aware footer fitting (T9)
- `internal/cli/dashboard/views/browser.go` — preview header zone w/ lint reasons (T2); focus cue + focused-scope exposure (T3)
- `internal/cli/dashboard/views/doctorview.go`, `views/activity.go` — viewport scrolling (T6)
- `internal/cli/dashboard/actions/actions.go` (+`actions_test.go` shape) — new registry rows (T3 focused scope if modeled there, T4 toggle, T6 scroll rows)
- `test/e2e/ptyharness_test.go`, `test/e2e/pty_hub_test.go` — footer-pin scenario (T1), hover-scroll scenario + 3-read gate (T5)
- `docs/01-dashboard-hub-spec.md` §2/§3 — footer pinning, selection guidance, toggle (T1/T4)
- `docs/plans/2026-07-16-pty-e2e-battery.md` — M4 supersession delta (T5)

---

### Task 1: Pin the footer to the terminal's last row (root fill-to-budget)

**Files:**
- Modify: `internal/cli/dashboard/dashboard.go` (fitHeight site `:1695-1708`, compose `:2155-2166`)
- Test: `internal/cli/dashboard/dashboard_test.go` (alongside existing root View tests), `test/e2e/pty_hub_test.go` (+harness helpers as needed)
- Modify: `docs/01-dashboard-hub-spec.md` §2 (one sentence: footer is anchored to the terminal's bottom row)

**Interfaces:**
- Produces: `fitAndFillHeight(body string, exact int) string` — clamps to at most `exact` lines AND pads with trailing empty lines to exactly `exact` lines. Replaces both `fitHeight` call sites; `fitHeight` itself is absorbed (delete it if nothing else calls it — deadcode gate will verify).

- [ ] **Step 1: Failing unit test** — root View with a deliberately short body (one seeded memory, small list, short preview) must render exactly `m.height` lines with the footer's key text on the LAST line; and cycling the browser cursor between a short-preview and a tall-preview memory must keep the footer on that same last line (two View() snapshots, same footer row index). Run: expect FAIL (footer floats today).
- [ ] **Step 2: Implement `fitAndFillHeight`** — reuse fitHeight's clamp logic, then pad `strings.Repeat("\n", missing)`; swap both call sites (`screen` and `tabBody`). Account for the `"\n\n"` joins: the body region's budget functions already do (they produced correct over-budget clamping); filling to the same number makes total frame height invariant. Verify the daemon-down and overlay frames still render correctly (they take different compose paths — check each renders ≤ height and the footer, where present, sits on the last row).
- [ ] **Step 3: Unit green + mutation** — revert the fill (clamp-only) temporarily: Step-1 test must go RED. Restore.
- [ ] **Step 4: PTY scenario** — `TestPTYFooterStaysAnchoredAcrossSelections`: 120×40 session, wait for browser-with-preview, capture screen; assert the footer key hint appears on vt10x row 39 (0-indexed last row); press `j` (selection with different preview height), wait for the new selection's preview, assert footer still on row 39. Use `waitScreen` predicates on the last grid row. Prove load-bearing by the Step-3 mutation (run scenario against the mutated build once, expect red, revert).
- [ ] **Step 5: Full dashboard package + e2e battery run** — `go test ./internal/cli/dashboard/... -race -count=1` and `go test ./test/e2e/ -race -count=1 -run 'TestPTY'`. Existing View tests that assumed unfilled short bodies may need their expectations updated — update them to the new invariant (exact-height frames), never weaken them.
- [ ] **Step 6: Commit** — `fix(dashboard): pin the footer to the terminal's bottom row on every frame`

### Task 2: Surface lint reasons in the preview pane + align its top edge

**Files:**
- Modify: `internal/cli/dashboard/views/browser.go` (`renderPreviewPane` `:1300-1338`, `chromeLines`, lint plumbing near `:133-142`)
- Test: `internal/cli/dashboard/views/browser_test.go`

**Interfaces:**
- Consumes: `lint.Issue{Rule, Detail}` from retained `lintResults`.
- Produces: preview pane header zone: line 1 = blank padding (always, so the pane's first content line aligns with the list's first row / provider header line); when the hovered memory has lint issues, one Warn-styled line per issue: `⚠ <Rule>: <Detail>`, ansi-truncated to pane width (fitListRow precedent), capped at 3 issue lines + `⚠ +N more` overflow line if more.

- [ ] **Step 1: Failing tests** — (a) hovered flagged memory: pane output contains the issue's `Detail` sentence, Warn-styled, ABOVE the rendered body; (b) unflagged memory: exactly one leading padding line, no warn lines; (c) alignment: at fixed geometry the pane's first BODY line index equals the list column's first row index (compute both from the joined browser View); (d) pane height budget still respected with header lines present (tall memory + 3 issues: total pane lines ≤ budget; scroll hint intact).
- [ ] **Step 2: Implement** — extend the lint retention so reasons are addressable by RepoPath (map[string][]lint.Issue built where lintFlags is built — same update path, cannot drift); render the header zone in `renderPreviewPane` and subtract its line count from `paneHeight` BEFORE the fits/overflows split. The blank padding line renders unconditionally (alignment is not conditional on warnings).
- [ ] **Step 3: Green + mutation** — zero the header-zone subtraction (render header but don't subtract): the budget test must go RED (pane overflow). Restore.
- [ ] **Step 4: Package run + commit** — `fix(dashboard): preview pane names lint warnings and aligns with the list top`

### Task 3: Preview-focus honesty — footer swap + always-visible cue (the "freeze")

**Files:**
- Modify: `internal/cli/dashboard/views/browser.go` (cue in preview render; expose focus state), `internal/cli/dashboard/dashboard.go` (`footerBindings` `:2248-2252` scope selection), `internal/cli/dashboard/actions/actions.go` if the focused sub-scope is modeled as a registry scope (preferred — the registry is the single key-truth source; follow the shape test's conventions in `actions/actions_test.go`)
- Test: `views/browser_test.go`, `dashboard_test.go`, `actions/actions_test.go`

**Interfaces:**
- Produces: `(*Browser).PreviewFocused() bool` (true only when `previewFocused && previewShown` — the same compound gate `:470` uses); a focused-preview binding set surfaced through the footer (esc/tab return · j/k scroll · ctrl+d/u · pgup/pgdn · g/G · y copy); an ALWAYS-rendered focus cue line on the pane while focused (e.g. accent-styled `▶ preview — esc returns to list` as the pane's cue row), present for BOTH fitting and overflowing previews (the overflow scroll hint remains additionally when overflowing).

- [ ] **Step 1: Failing tests** — (a) root footer while focused lists the focused set (contains "esc", scroll keys) and NOT the list-only actions; blurred restores the browser scope footer; (b) cue line present in the pane when focused with a SHORT (fitting) preview — this is the exact vacuity that made the freeze: today NOTHING renders; (c) cue absent when unfocused; (d) registry shape test extended if a new scope is added.
- [ ] **Step 2: Implement** — smallest honest wiring: root consults `PreviewFocused()` when choosing the footer's binding set (mirror how modal footers already swap via `forModal` — same idiom, follow it); browser renders the cue row inside the pane (count it in the header/chrome math from Task 2 so budgets hold).
- [ ] **Step 3: Green + mutations** — remove the footer swap: (a) RED. Remove the cue render: (b) RED. Restore both.
- [ ] **Step 4: Verify the freeze narrative end-to-end** — PTY: click the preview of a SHORT memory, assert the cue text appears on screen and the footer now shows "esc"; press esc, assert footer restored and list cursor moves on `j`. Add as `TestPTYPreviewClickFocusIsDisclosed` (reuses SGR click bytes from the click scenario, targeting preview-column coordinates).
- [ ] **Step 5: Package + battery run; commit** — `fix(dashboard): disclose preview focus in the footer and pane (click no longer reads as a freeze)`

### Task 4: Mouse-capture toggle for native text selection

**Files:**
- Modify: `internal/cli/dashboard/dashboard.go` (arming gate `:2156-2161`, toggle state, footer/hint disclosure), `internal/cli/dashboard/actions/actions.go` (+shape test row)
- Test: `dashboard_test.go`, `actions/actions_test.go`
- Modify: `docs/01-dashboard-hub-spec.md` §3 (selection guidance: the toggle joins Option/Shift-drag and y/Y)

**Interfaces:**
- Produces: registry action `mouse-capture-toggle` (ScopeBrowser; key `m` — verify free against the full shape enumeration first; if taken, choose the first free of `m`, `M`, `ctrl+t` and record why in the action's comment), flipping root `mouseCaptureOff bool`. While off: the `:2160` gate never sets `mouseWanted` (renderer diffs MouseMode→None next frame — teardown semantics identical to the existing every-non-browser-frame disarm, so no new escape plumbing), wheel hover-scroll and click-select simply stop arriving (disclosed), native drag-select works. Footer/hint shows the state while off (e.g. `mouse: native select (m re-arms)`) — the disclosure renders on every frame the toggle is off, not only the frame it flipped.

- [ ] **Step 1: Failing tests** — (a) armed browser-with-preview frame: `view.MouseMode` is CellMotion; after the toggle action: None on the next View; toggling again: CellMotion; (b) disclosure text present in the frame while off, absent while on; (c) registry shape row (scope/keys) + action routed.
- [ ] **Step 2: Implement** — flag lives on the root model (frame-scoped truth, same place the gate reads); the action toggles it; disclosure joins the hint/footer line the same way existing state cues do.
- [ ] **Step 3: Green + mutation** — force `mouseCaptureOff` ignored at the gate: (a) RED. Restore.
- [ ] **Step 4: Package run; commit** — `feat(dashboard): mouse-capture toggle enables native text selection`

### Task 5: Battery growth — hover-scroll cell + 3-read stability gate (M4)

**Files:**
- Modify: `test/e2e/ptyharness_test.go` (`waitStableMaxVisibleLineNumber` 2→3 consecutive equal reads; comment updated to say why three), `test/e2e/pty_hub_test.go` (new scenario)
- Modify: `docs/plans/2026-07-16-pty-e2e-battery.md` (append ONE dated line to its Execution deltas "Accepted residual" bullet: superseded — hardened to three reads proactively on the user's standing directive)
- Modify: `docs/decisions/21-adr-alternate-scroll.md` "Automated wire-contract coverage" (hover-scroll cell moves from manual bucket 1's "incl. hover-scroll" phrasing to the automated list; bucket 1 keeps emulator wheel translation itself)

**Interfaces:**
- Consumes: hubSession helpers; SGR wheel-down press `\x1b[<65;COL;ROWM` at preview-pane coordinates.

- [ ] **Step 1: New scenario (red-first against a mutation)** — `TestPTYHoverScrollWheelScrollsPreviewWithoutFocusChange`: browser-with-preview over the long memory, send 3× SGR wheel-down at a preview-column coordinate, assert the preview content scrolled (higher line numbers visible in the preview column / scroll hint changed) AND focus did NOT change (no focus cue from Task 3, footer unchanged). Prove load-bearing: temporarily route wheel to focus-change or no-op in browser.go, RED, revert.
- [ ] **Step 2: 3-read gate** — change the stability requirement to three consecutive equal reads; run the wheel tests ×3 race; update the harness comment (why: one extra 10ms sample removes the >poll-interval mid-frame stall window at negligible latency; built proactively per directive).
- [ ] **Step 3: Docs deltas** (the two files above, append-only).
- [ ] **Step 4: Full battery flake gate** — `(ulimit -u 1400; go test ./test/e2e/ -race -count=3 -run 'TestPTY')` green.
- [ ] **Step 5: Commit** — `test(e2e): hover-scroll wire scenario; harden the scroll-stability gate to three reads`

### Task 6: Doctor and Activity tab scrolling

**Files:**
- Modify: `internal/cli/dashboard/views/doctorview.go`, `internal/cli/dashboard/views/activity.go` (bounded viewports), `internal/cli/dashboard/dashboard.go` (key routing to the active tab), `internal/cli/dashboard/actions/actions.go` (+shape rows for the scroll bindings in those scopes)
- Test: `views/doctorview_test.go`, `views/activity_test.go`, `actions/actions_test.go`

**Interfaces:**
- Consumes: the preview/reading viewport convention — ctrl+d/u (half page), pgup/pgdn, g/G; cursorless (scroll only); bottom line spent on the `── N% ──` overflow hint when content overflows, no hint when it fits (reuse the existing hint helper if exported/reachable; otherwise mirror its exact format).
- Produces: both tabs render through a height-bounded viewport fed by their existing content builders; root routes the scroll keys to the active tab's viewport when that tab is active and no overlay/prompt is open.

- [ ] **Step 1: Failing tests** — overflow content (e.g. 100 doctor rows) at small height: (a) body line count ≤ budget WITH hint line; (b) ctrl+d advances the window (later rows visible, earlier gone); (c) g/G jump to top/bottom; (d) fitting content: no hint, keys no-op harmlessly; (e) registry shape rows.
- [ ] **Step 2: Implement** — same structure the browser preview uses (SetWidth/SetHeight, content set on data refresh, GotoTop on data-identity change so a refresh never yanks scroll).
- [ ] **Step 3: Green + mutation** — drop the height bound: overflow test RED (root clamp would mask at the frame level, so assert on the tab body string, not the root frame). Restore.
- [ ] **Step 4: Package run; commit** — `feat(dashboard): doctor and activity tabs scroll (viewport convention)`

### Task 7: gh-auth staleness — continuous detection, hub attention state, one-key re-auth handoff

**Why this task exists (verified live at the user's machine):** the memories checkout's remote is
SSH (`git@github.com:Sawmonabo/agent-brain-memories.git`), so sync cycles never touch the gh
OAuth token — an invalid token silently breaks ONLY the gh-dependent features: the update
banner/self-update (ghx release calls), init/re-provisioning, and doctor's gh row. Today the
user discovers an invalid token only by opening the Doctor tab; the update-check path fails
silently (no banner, no error). GitHub's design makes silent re-mint IMPOSSIBLE — `gh auth
login`/`gh auth refresh` are interactive device/browser flows, so no daemon may attempt one;
what the product owes the user is instant loud detection and a one-keypress remediation.

**Files:**
- Modify: the update-check call path (P5 T18 seam — locate the periodic release check and its error handling; it is the natural detector cadence, no new timer), `internal/ghx` (failure classification: auth-invalid vs offline vs other — corpus includes the real stderr `The token in keyring is invalid` and `Failed to log in to github.com account`), `internal/cli/dashboard/dashboard.go` (persistent header/status attention segment per spec §2), the doctor tab's fix routing (T19 r/f/s machinery) + the terminal-handoff seam the $EDITOR flow uses
- Test: ghx classification corpus tests; attention-state render pins; doctor-row handoff wiring (fake `gh` script on PATH, e2e `fakegh` precedent); registry shape rows if new bindings

**Interfaces:**
- Produces: an auth-attention state set when any gh call classifies as auth-invalid (sticky until a probe succeeds — never cleared by mere time passing); hub header renders it loudly (`gh auth invalid — Doctor tab: f re-authenticates`); doctor's fix action on the gh row hands the terminal to interactive `gh auth login -h github.com` (same suspend/resume seam as the editor handoff, including the 1007 re-assert on return — Task ADR 21 contract), re-probes on return, clears the attention state on success, honest failure toast otherwise.

- [ ] **Step 1: Failing classification tests** — table over real gh stderr corpora: keyring-invalid and failed-login lines → auth-invalid; the existing offline signatures stay offline; unknown stays other. (Extend the phantom-seam fail-closed classifier idiom; never regex-match locale-dependent text without `LC_ALL=C` precedent.)
- [ ] **Step 2: Failing surfacing test** — model with auth-attention set renders the header segment; cleared state does not; the segment survives frame recompute (sticky, not a TTL toast).
- [ ] **Step 3: Implement detection** — classify at the shared ghx error seam so update-check, init-time calls, and doctor all feed the same state; the daemon/update path must NOT retry-storm on auth failures (respect the existing backoff).
- [ ] **Step 4: Implement the handoff fix** — doctor gh row's `f` triggers the terminal handoff running `gh auth login -h github.com`; on return re-run the gh probe; success clears attention + ok toast, failure keeps state + failure toast naming the manual command. Wire the 1007 re-assert exactly like the editor return path.
- [ ] **Step 5: Green + mutations** — (a) misroute auth-invalid to offline: classification RED; (b) drop the sticky render: surfacing RED; (c) fake-gh handoff test proves the child ran and the re-probe fired (marker file from the fake script). Restore all.
- [ ] **Step 6: Package runs + commit** — `feat(dashboard): loud gh-auth attention state with one-key re-auth handoff`

### Task 8: Projects table reflows with toast occupancy (full-parity polish)

**Why this task exists:** Task 1b made every frame fill the terminal exactly, but
`ProjectsView.SetSize` (`views/projects.go`) keeps its own static height-14 reservation, set only
on WindowSizeMsg and blind to toast occupancy — proven safe (the dynamic budget is pointwise >=
the old ceiling, root padding covers the gap), yet the table renders up to 4 rows shorter than a
toast-free budget allows: once enough projects exist, that is a blank band above the footer where
rows could be. Full parity = the table reflows with actual occupancy like Browser/Reading/Conflicts
now do.

**Files:**
- Modify: `internal/cli/dashboard/views/projects.go` (SetSize reservation → dynamic, passed or derived), `internal/cli/dashboard/dashboard.go` (re-size the Projects view when toast occupancy changes — the same seam WindowSizeMsg uses today, extended to the toast push/expiry transitions)
- Test: `internal/cli/dashboard/views/projects_test.go`, occupancy rows in the frame exact-fill table

**Known hazard (name it in the code comment):** the bubbles table resize-crash class — rows and
columns must change atomically (`setColumns` capture/restore pattern from the resize-crash fix in
`projects.go`); any resize call landing while wide rows are live must go through that pattern.

- [ ] **Step 1: Failing test** — toast-free: the Projects table's visible window equals the dynamic budget (taller than the old static reservation allows); one/two toasts: the window shrinks by the same lines; footer remains on the last row at every occupancy (extend the existing exact-fill occupancy table).
- [ ] **Step 2: Implement** — thread the actual body budget into SetSize (or a new resize entry point) and invoke it on toast transitions; all row/column mutations go through the atomic pattern.
- [ ] **Step 3: Green + mutation** — restore the static 14: Step-1 RED. Restore.
- [ ] **Step 4: Package + battery runs; commit** — `fix(dashboard): projects table reflows with toast occupancy`

### Task 9: Width-aware footer fitting (no silent clipping)

**Files:**
- Modify: `internal/cli/dashboard/dashboard.go` (the footer line builders — the stack-footer and tab-footer seams share one fitting helper)
- Test: `dashboard_test.go`

**Interfaces:**
- Produces: a fitting step applied to every footer line at render — rows keep registry order (registry order IS priority among droppable rows: earlier rows are more important, and exit affordances never drop regardless of position — Decision 10), whole unprotected rows are dropped from the tail when the line exceeds the terminal width behind a `… ?` continuation marker rendered at the elision point (the `?` help overlay is the full-truth surface), and state cues (the mouse-off cue; any future sticky state) plus exit affordances always render (cues lead the line per Decision 8). No wrapping, no mid-row truncation, frame height unchanged.

- [ ] **Step 1: Failing tests** — at width 120, armed browser footer: printable width ≤ 120, last visible row intact (no mid-row cut), `… ?` marker present when rows were dropped; at width 80: same invariants with fewer rows; at width 80 while mouse-off: the state cue fully visible AND the marker present; at width 200: all rows, no marker. Measure with the same ANSI-aware width helpers the wave already uses.
- [ ] **Step 2: Implement** — the fitting helper at the footer seams (both call sites), lipgloss-width-aware; state cues excluded from droppable rows.
- [ ] **Step 3: Green + mutations** — remove the fitting → the 120-width test RED (over-wide line); drop the always-keep-cue rule → the off-80 test RED. Restore both.
- [ ] **Step 4: Package + battery runs; commit** — dashboard tree `-race`; full PTY battery `-race` (footer content changes; the battery's assertions must be verified by run, not inference). Commit: `fix(dashboard): fit footer rows to terminal width instead of clipping (state cues always visible)`

---

## Verification (whole-branch, before merge)

- [ ] `go build ./...`
- [ ] `(ulimit -u 1400; go test ./... -race -count=1)` — foreground
- [ ] `gofumpt -l .` → empty; `golangci-lint run` → 0 issues; `go tool deadcode -test ./...` → empty
- [ ] Boundary greps: engine/provider charm-free and UI-free; xpty/vt10x confined to `test/e2e`
- [ ] `(ulimit -u 1400; go test ./test/e2e/ -race -count=3 -run 'TestPTY')` — three consecutive clean runs
- [ ] Final whole-branch review (fable) over `scripts/review-package BASE HEAD`; every finding built or explicitly adjudicated with the ruling persisted here in an Execution deltas section
- [ ] Reinstall dev build via the same-dir rename pattern (this wave changes product code) + daemon restart for same-build consistency

## Explicitly recorded non-goals (with reasons — not silent deferrals)

- Silent gh token re-mint: impossible by GitHub's design — the device/browser flow requires the human; the daemon never attempts one. Task 7 builds everything buildable around that constraint (instant detection, loud surfacing, one-key interactive handoff); the interactive mint itself stays the user's keypress.
- Emulator-side CI jobs (Xvfb/xterm, Playwright/xterm.js) and lychee link CI: recurring-cost CI infrastructure testing third parties; queued as the explicit close-out housekeeping decision (board #26) for the user's go/no-go — surfaced, not silently dropped.
- Scoped/partial mouse capture (capture clicks but allow native selection simultaneously): not expressible in the terminal protocol — capture is terminal-global; the toggle is the honest primitive.
