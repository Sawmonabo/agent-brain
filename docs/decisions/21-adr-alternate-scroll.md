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
  program starts.
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
  harmless on the GUI-editor path, which never released the terminal.

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

## Consequences

- **No per-view wheel code.** Scroll works under the wheel on every current and
  future viewport for free, because the terminal delivers arrow keys the
  existing keymaps already handle. Screens that do not yet scroll (Activity /
  Doctor) gain nothing today and lose nothing; the wheel becomes useful there
  the instant their own scrolling lands.
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
- **One irreducible manual smoke cell.** No unit test has a real tty, so the
  wheel/selection behavior is verified by hand across the cross-OS matrix:
  wheel scrolls the reading view on iTerm2 with the setting on; wheel still
  hover-scrolls the browser preview; drag-select works in the reading view
  *without* modifier keys on 1007-honoring terminals; `alternate_scroll =
  false` restores the old wheel; quitting leaves the shell's wheel normal; a
  terminal-level alternate-scroll preference armed before launching the hub
  (e.g. iTerm2's profile toggle) still holds after quitting, on
  XTSAVE/XTRESTORE-supporting terminals; and the xterm.js/Cursor cell is
  checked explicitly.
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
