# ADR 21: Alternate-scroll wheel across the hub (DECSET 1007)

- **Status:** Accepted
- **Date:** 2026-07-16
- **Deciders:** Sawmon (directive: the hub is the single interactive surface —
  the wheel must scroll it, feature-rich, without breaking terminal-native
  text selection)
- **Related:** ADR 05 (CLI/TUI stack — bubbletea/lipgloss), ADR 20 (dashboard
  hub), spec §3 (browser), §4 (reading view)
- **Spec:** `docs/01-dashboard-hub-spec.md`

## Context

The hub renders in the alternate screen. Two things a user reaches for by
reflex there — spinning the mouse wheel to scroll, and dragging to select
text — are in direct tension in a terminal TUI:

- To make the **wheel** scroll a view, the classic move is to enable mouse
  tracking (SGR 1000/1002/1003/1006) and route wheel events to the focused
  viewport. But mouse tracking captures *all* pointer events, which suppresses
  the terminal's own drag-select — the user can no longer sweep-select and
  copy body text with the pointer (the lazygit/k9s complaint class). Our
  browser preview already pays exactly this cost while it is shown: it arms
  cell-motion tracking so hover-scroll works, and spec §3 documents the
  `y`/OSC52 copy and modifier-drag escapes that buys back.
- Leaving the wheel alone (**keyboard-only**) keeps drag-select but means the
  wheel does the terminal's default thing. On iTerm2-class terminals inside
  the alternate screen that is *scrollback*, not content scroll — the user
  spins the wheel over a long memory and the terminal scrolls its own history
  instead of the viewport. A surprising dead wheel on the hub's primary
  reading surface.

There is a third mode built for exactly this: **DECSET 1007**, "alternate
scroll." While the alternate screen is active and no mouse tracking is armed,
the terminal itself translates wheel notches into arrow-key presses. Every
list and viewport that already scrolls on `↑/↓` (or `j/k`, which the reading
viewport binds) scrolls under the wheel — and because no tracking is enabled,
native drag-select keeps working on every screen. This is the posture the
Codex CLI ships (see Peer evidence). Each fork below was checked against the
terminal specs and shipping tools before deciding (see Sources).

## Decision

Set DECSET 1007 hub-wide, once, for the whole session, gated on a config
setting that defaults on.

- **Generic mode machinery, no dependency bump.** bubbletea v2.0.8 has no
  named API for mode 1007, and its renderer's mode lifecycle only knows the
  enumerated modes it manages. The set/reset sequences are built from
  `x/ansi`'s generic `ansi.DECMode(1007)` via `ansi.SetMode`/`ansi.ResetMode`
  (`internal/cli/dashboard/altscroll.go`), so no renderer change or dependency
  addition is involved.
- **Delivered through `tea.Raw`.** The sequences reach the terminal through
  bubbletea's supported raw-escape seam —
  `tea.Raw(saveAlternateScrollState+setAlternateScroll)` is batched into the
  model's `Init` as a single payload, so the mode is armed once as the
  program starts. **Amended 2026-07-16:** a PTY battery pinned exactly where
  this payload lands relative to entering the alternate screen: `1007s` <
  `1007h` < `1049h` on the wire, the arm preceding the alt-screen enter —
  the reverse of what a first reading of "the hub renders in the alternate
  screen" (Context, above) might suggest. The two halves of that order carry
  different weight: `1007s` < `1007h` is the semantic core — the save must
  capture the pre-hub state before the set overwrites it — and holds
  wherever the alt-screen switch falls, because DEC private-mode state is
  terminal-global and only the wheel's *effect* is alt-screen-conditional;
  `1007h` < `1049h` is a snapshot of bubbletea v2's flush order, a fact to
  re-verify on a framework bump rather than a designed contract. See
  "Automated wire-contract coverage," below.
- **Config-gated, default-on.** Every set and the teardown are guarded by
  `settings.Dashboard.AlternateScroll` (`internal/config/settings.go`,
  `toml:"alternate_scroll"`, default `true`).
  `dashboard.alternate_scroll = false` restores the terminal's raw wheel and
  skips the reset entirely.
- **Reset-then-restore on every exit path.** The wheel must not keep
  translating to arrows for the shell after the hub quits — and a user whose
  own terminal config (an xterm `alternateScroll` resource, an iTerm2
  profile toggle) already had 1007 armed before the hub ever started should
  not lose that preference either. `RestoreAlternateScroll` (`altscroll.go`)
  writes `DECRST` followed by `XTRESTORE` from the one choke point every quit
  shares — the CLI command runs it right after `program.Run()` returns,
  whatever the exit path — paired with the `XTSAVE` issued alongside the set
  in `Init`. Terminals that support the XTSAVE/XTRESTORE round-trip recover
  the user's pre-hub state on exit; every other terminal has nothing saved to
  restore, so `XTRESTORE` is silently ignored there and the plain `DECRST`
  alone stands. It is an **explicit** call, not a `defer`: the self-update
  handoff below it may `syscall.Exec`, which replaces the process image
  before any deferred cleanup could run.
- **Re-assert after the editor handoff.** The in-terminal `$EDITOR` handoff
  (ADR 20 decision 2) hands the terminal to a child process that may reset
  private modes. On the editor's return, the `editorFinishedMsg` handler
  re-asserts 1007. It is keyed off the exit itself, not the edit outcome: the
  sequence is idempotent when the child left the mode alone (4 bytes) and
  harmless on the GUI-editor path, which never released the terminal. The
  handler re-emits only the set, never a second `XTSAVE`: a mid-session save
  would overwrite the captured pre-hub state with the hub's own armed mode,
  and the exit restore would then faithfully "restore" the wrong thing.

## Alternatives rejected

- **(a) Mouse capture in the reader.** Enable mouse tracking on the reading
  view and route wheel events to its viewport. Rejected: tracking suppresses
  native drag-select — the reading view's whole reason to exist is being the
  screen where a user *can* sweep-select and copy body text with the pointer
  (spec §3 sends users there from the browser precisely for this). Buying the
  wheel by re-imposing the browser's own copy tax on the one screen that
  escapes it is the lazygit trade, backwards. 1007 gets the wheel *and* keeps
  selection.
- **(c) Keyboard-only status quo.** Ship no wheel handling. Rejected: on
  iTerm2-class terminals the alternate-screen wheel scrolls scrollback, so the
  primary reading surface has a wheel that appears dead (or worse, scrolls the
  wrong thing). 1007 is the minimal, native fix — set-and-forget, no per-view
  routing code, no capture.

(Option **(b)**, a hub-wide mouse-capture toggle, was the earlier design this
decision supersedes; it is the mouse-capture family (a) generalized and loses
selection the same way.)

## The precedence rule (and how the two mouse modes compose)

The terminal's own precedence resolves the one place 1007 and mouse tracking
coexist: **mouse tracking beats alternate scroll.** When any tracking mode is
armed, the terminal reports wheel events as mouse events and ignores 1007;
when none is, 1007 governs and the wheel sends arrows. This is documented
behavior, not an accident we lean on:

- terminalguide, mode p1007: alternate scroll applies only when no mouse
  reporting is active.
- iTerm2 feature-reporting / terminal preferences: mouse reporting takes
  precedence over the "scroll wheel sends arrow keys" behavior.
- microsoft/terminal PR #16535: Windows Terminal's alternate-scroll fix
  explicitly defers to active mouse tracking.

So the browser preview and the hub-wide 1007 **compose without any arbitration
code of ours**: while the preview pane holds cell-motion tracking, its capture
takes precedence and the wheel hover-scrolls the preview; the moment focus
leaves the pane (or on every other screen, including the reading view), no
tracking is armed and 1007 turns the wheel into arrow keys. The terminal picks,
per the rule above.

## Terminal matrix

| Terminal | 1007 behavior |
|---|---|
| xterm, iTerm2, VTE (GNOME/Tilix), alacritty, Windows Terminal | Honor 1007: wheel → arrows in the alt screen, no tracking, selection intact. |
| kitty | Translates wheel → arrows in the alternate screen **unconditionally** — it does not parse 1007, but the end behavior matches, so the mode is a harmless no-op there. |
| tmux | Swallows the inner program's 1007. The user restores it with a `WheelUpPane`/`WheelDownPane` binding conditioned on `#{alternate_on}` that sends arrow keys — documented in the spec beside the OSC52 `allow-passthrough` note (both are the same "tmux needs one config line" caveat). |
| Terminal.app (modern) | Already sends arrow keys for the wheel in the alternate screen by default, so the wheel scrolls with or without the mode. |
| xterm.js (Cursor / VS Code integrated terminal) | Partial / in flux (upstream issues open). Treated as a **manual smoke cell** in the cross-OS matrix rather than assumed. |

The default-on setting is safe across this matrix because a terminal that does
not implement 1007 simply ignores both the set and the reset — there is no
terminal the mode *breaks*, only ones it does not help.

## Peer evidence

- **Codex** ships this exact posture. Its binary carries the `[?1007h` /
  `[?1007l` set/reset sequences and **no** mouse-capture escapes
  (`1000`/`1002`/`1003`/`1006` absent from the binary sweep); openai/codex#2836
  confirms the mode is emitted at runtime. Independent confirmation that
  alternate-scroll, not mouse capture, is the right tool for a wheel-in-a-TUI
  that must preserve selection.
- **Claude Code** takes the opposite architecture and needs none of this: it
  renders **inline in the primary buffer** via Ink, captures no mouse, and
  relies entirely on the terminal's native selection. It has **no app-level
  copy** to emulate. This corrected a stale claim in the reading view's
  `CopyMemoryMsg` doc, which had attributed our OSC52 copy to "the same thing
  Claude Code's CLI does" — Claude Code does no such thing; our OSC52 copy is
  our own answer to the browser preview's suppressed selection, not a Claude
  Code emulation.

## Automated wire-contract coverage

`test/e2e/pty_hub_test.go` (scenarios) and `test/e2e/ptyharness_test.go`
(harness) added a PTY battery that drives the real compiled binary on a real
pseudo-terminal, proving wire-level facts no unit test can reach: a unit
test pins the bytes a call site emits, never their place in the actual
render stream or their effect on a real screen model. Seven scenarios:

- `TestPTYHubArmsAlternateScrollInOrder` pins the arm order on the wire:
  `1007s` < `1007h` < `1049h` — XTSAVE and DECSET both land before the
  alternate-screen enter (see the Decision section's amendment, above).
- `TestPTYWheelBytesScrollReadingView` pins that a single wheel notch
  scrolls the reading viewport by exactly one line, not merely "at least
  one," over both wire encodings a 1007 terminal can emit — sibling
  subtests `csi-normal-cursor-keys` (`\x1b[B`) and
  `ss3-application-cursor-keys` (`\x1bOB`) — then drives to a
  deeper-scroll outcome with no fixed notch count assumed.
- `TestPTYHoverScrollWheelScrollsPreviewWithoutFocusChange` pins the
  browser preview's hover-scroll — the OTHER half of the precedence rule
  above. With the preview's cell-motion capture armed, a wheel notch
  arrives as an SGR mouse event (mouse tracking beats alternate scroll, so
  1007 does NOT translate it to an arrow key here) and scrolls the pane a
  few lines WITHOUT moving focus, which only a click may change. Together
  with the reading-view scenario above — where no capture is armed and
  1007 turns the same notch into an arrow — the two compose the wheel
  contract on the wire: mouse event over the armed preview, arrow key
  everywhere else.
- `TestPTYClickBytesSelectBrowserRow` pins that an SGR mouse click on a
  browser row moves the selection cursor AND re-targets the preview to
  that row's body — the body is the deliberately stronger of the two
  signals, since the row's title alone also appears in the list row and
  would pass on selection movement alone.
- `TestPTYEditorRoundTripReAssertsWithoutReSaving` pins the no-re-save
  decision (above) on the wire: exactly one XTSAVE and at least two
  DECSETs across a session that round-trips through the `$EDITOR`
  handoff (Init's arm plus the post-editor re-assert).
- `TestPTYQuitRestoresAlternateScrollTail` drives the one interactive
  esc→y quit-confirmation route this battery exercises (every other
  scenario quits via ctrl+c), proving the shared teardown-tail assertion
  below also holds on the documented interactive path, not only the
  unconditional-quit shortcut.
- `TestPTYKillSwitchEmitsNoAlternateScrollBytes` is the standing negative
  control: with `alternate_scroll = false`, a full open→browse→read→edit→quit
  cycle — including one $EDITOR round-trip, so the re-assert's disabled gate
  is exercised too — puts ZERO 1007 bytes on the wire, which is what makes
  every other scenario's "1007 present" assertion load-bearing rather than
  vacuous.

The teardown-tail order — `1049l` < `1007l` < `1007r` — is pinned once, in
a shared helper, and runs on every armed session's quit: the unconditional
ctrl+c route and the interactive esc→y route both funnel through the same
drain path, so the order is proven on both exit routes independently
rather than resting on one dedicated scenario alone.

**The scroll-geometry finding.** An investigation into an apparent "dropped
first notches" anomaly found no drop at all: every wheel notch scrolls the
reading viewport by exactly one line from the very first notch — over both
wire encodings a 1007 notch can take (CSI `\x1b[B` and SS3 `\x1bOB`) and
identically for a plain `j` keypress on the keyboard path — no input is
ever dropped.
The anomaly was a test-predicate illusion: glamour renders an H1 heading
plus a blank line ahead of the long memory's fenced body, and the reading
view adds two more chrome lines on top of that, so the body's first line
sits roughly four rows below the viewport's top edge — an absence-based
predicate ("line-001 is gone") needs four-plus notches to observe it
leaving, even though the wire already carried an identical one-line
scroll burst on notch one. This is the reasoning behind the wheel
scenario's two-grain design above: the single-notch pin asserts the wire
fact directly (one notch, one new line, and the second notch's line
provably absent on that same snapshot), while the deeper-scroll drive
targets an observable outcome instead of a hardcoded notch count that
render geometry would make fragile.

**Harness posture.** The battery spawns the real `agent-brain` binary on a
real pseudo-terminal, never the package under test in-process, and
reconstructs the visible screen with a VT emulator rather than grepping
the raw escape stream — bubbletea v2's renderer emits cell diffs, not
whole frames, so a screen model is the only way to reconstruct what is
currently visible. Every wait is a poll-until-predicate over a bounded
deadline, never a bare sleep, and session cleanup has a hard-kill
fallback, so a child that outlives its pty becomes a fast, diagnosable
test failure instead of a hung suite.

**Dependencies (test-only).** `github.com/charmbracelet/x/xpty` v0.1.3 —
charm's own PTY harness, the same transport huh v2's own tests use — and
`github.com/hinshun/vt10x`, the VT screen model the Go expect/survey
lineage standardized on. Both MIT. Buy over build: `xpty` and
`creack/pty` share identical unix mechanics, but `xpty` is charm-maintained
and its use in huh's own test suite is an existence proof for the whole
stack; a hand-rolled screen model would have to reimplement what `vt10x`
already does just to make "what is on screen" assertable at all.

**Boundary adjudication.** Both imports resolve to
`test/e2e/ptyharness_test.go` alone — no other package in the module
imports either — and even there they never reach
`internal/cli/dashboard`: the battery drives the hub as a subprocess of
the compiled binary over its own pty, never as an in-process import. Spec
§15's "`internal/cli/dashboard` remains the only TUI-importing package" is
unaffected: `test/e2e` is harness, exercising a compiled artifact from
outside, not a member of that package tree.

## Consequences

- **No per-view wheel code — for the viewports that bind arrow keys.** Scroll
  works under the wheel for free on every viewport whose keymap already handles
  the arrow keys the terminal delivers: the reading view and the browser
  preview. The Activity and Doctor tab bodies now scroll too
  (`views/scrollpane.go`), but they are cursorless and deliberately leave
  `↑/↓` unbound, so the wheel notches 1007 translates into arrow keys are
  ignored there — the wheel does not move a tab pane; `ctrl+d/u`, `pgup/pgdn`,
  and `g/G` scroll them (spec §2), matching the overflow hint. That is by
  design: a cursorless pane has no selection for the wheel to nudge line by
  line.
- **A hard kill leaks a dormant mode — accepted.** `RestoreAlternateScroll`
  covers every graceful exit (clean quit, ctrl+c, context cancel,
  `ErrProgramKilled`) and runs before the self-update `syscall.Exec`. A
  SIGKILL (kill -9) bypasses all cleanup and leaves 1007 set — but the mode is
  **dormant outside the alternate screen**: it governs the wheel only while the
  alternate buffer is active, and a killed hub leaves the shell in the primary
  buffer where 1007 has no effect. The leak is invisible, so we accept it
  rather than add signal handlers that cannot fire on SIGKILL anyway.
- **XTSAVE/XTRESTORE round-trips gracefully.** Terminals that support saving
  and restoring DEC private mode state recover the user's own pre-hub 1007
  preference on exit; every other terminal has nothing saved to restore, so
  `XTRESTORE` is a no-op there and the plain `DECRST` stands alone.
- **Automated by the PTY battery.** The arm order, wheel-to-scroll at
  exactly one line per notch, SGR click re-targeting of the browser
  preview, the browser preview's hover-scroll (a wheel mouse event
  scrolling the pane without moving focus), the editor round trip's
  no-re-save guarantee, the teardown-tail order on every armed quit route,
  and the kill switch's zero-1007 guarantee are now proven on the wire by
  the PTY battery (see "Automated wire-contract coverage," above) rather
  than resting on manual spot checks alone.
- **Manual smoke matrix, narrowed to emulator and config residue.** What
  remains is exactly the residue no PTY harness can reach — behavior that
  depends on a real terminal's own choices, not on how our program answers
  a given input byte: whether a given emulator translates the wheel to
  arrows under 1007 and composes that with the mouse-tracking precedence
  rule — reporting a wheel MOUSE event, not an arrow, while the preview's
  capture is armed (our own response to each is now automated, above: an
  arrow scrolls a viewport, a preview-armed mouse event hover-scrolls the
  pane without moving focus — what the emulator still owns by hand is
  emitting the right one), and leaves drag-select alone with no modifier
  key required — checked by hand on
  iTerm2, Terminal.app, and Windows Terminal (kitty is documented to
  always-translate regardless of 1007, and tmux is documented to swallow
  the inner program's 1007 behind its own
  `WheelUpPane`/`WheelDownPane` user binding, so neither needs a fresh
  by-hand check beyond confirming the documented behavior); whether a
  pre-armed 1007 preference in the user's own terminal config (an xterm
  `alternateScroll` resource, an iTerm2 profile toggle) round-trips
  through a real XTSAVE/XTRESTORE-supporting terminal's own state storage
  (the wire order of the save/restore itself is automated, above; a
  terminal's own storage of it is not); the xterm.js/Cursor cell, tracked
  as partial/in-flux upstream (terminal matrix, above); and OSC52
  clipboard reception — the emitted payload is unit-pinned, but whether a
  given terminal actually places it on the system clipboard is a
  terminal-side effect no PTY can observe.
- **DECRQM support detection is deliberately absent.** Nothing branches on
  whether the terminal implements 1007; set-and-forget is strictly simpler and
  equally safe, since unsupported terminals ignore the sequence.

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
