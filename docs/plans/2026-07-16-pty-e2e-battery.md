# PTY E2E Battery: Alternate-Scroll Wire Contract Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Automate the our-side cells of the alternate-scroll manual smoke matrix by driving the real `agent-brain` binary on a real PTY and asserting both the raw escape-byte wire contract and the rendered screen state.

**Architecture:** A shared PTY harness in `test/e2e` spawns the built binary on a `charmbracelet/x/xpty` unix PTY, tees the master output into (a) a raw byte log for wire-order assertions and (b) a `hinshun/vt10x` virtual screen for rendered-state assertions. Six scenarios cover arm order, wheel-byte scrolling (both arrow encodings), SGR click-to-select, editor-handoff re-assert (exactly one XTSAVE per session), quit teardown tail order, and the kill-switch emitting zero 1007 bytes. The suite validates a spec-correct terminal's view of our program — it deliberately does not test third-party emulators.

**Tech Stack:** Go stdlib `testing` + go-cmp, `github.com/charmbracelet/x/xpty` v0.1.3 (charm's PTY harness; already in the module graph as huh v2's test transport — huh_test.go is the in-graph existence proof that bubbletea v2 programs run on a non-responding PTY), `github.com/hinshun/vt10x` (the VT emulator go-expect/survey standardized on; bubbletea's diff-renderer output is only assertable through a screen model, never by grepping the raw stream).

## Global Constraints

- Never commit to `main` (ADR 11); work lands on `feat/pty-e2e-battery`, merged to `develop`.
- Tests: stdlib `testing` + `google/go-cmp` only for assertions; table-driven where natural; `t.Parallel()`; `t.TempDir()` for filesystem needs (ADR 15). The two new harness dependencies (xpty, vt10x — both MIT) are test-only imports confined to `test/e2e`; they must not appear in any non-test package.
- No `//nolint`; fix lint findings structurally.
- No PR numbers, reviewer names, review timestamps, dates, or commit hashes in code comments or test names.
- Comment density and idiom match the surrounding e2e files (heavy rationale comments; every non-obvious decision gets its WHY).
- `internal/cli/dashboard` remains the only TUI-importing package tree (spec §15); `test/e2e` importing xpty/vt10x is a TEST harness, not a TUI import — record this adjudication in the ADR amendment so the boundary grep's scope stays honest.
- Full gate before merge: `go build ./...`, `(ulimit -u 1400; go test ./... -race -count=1)` FOREGROUND, `gofumpt -l .` empty, `golangci-lint run` 0 issues, `go tool deadcode -test ./...` empty, boundary greps clean.
- Every new assertion must be proven load-bearing: either a built-in negative control (the kill-switch scenario is the natural negative for every 1007-presence assertion) or a temporary mutation (invert the expected order / point the assertion at the wrong byte) shown red then reverted.
- Timeout discipline: no bare sleeps as synchronization. Every wait is a poll-until-predicate with a deadline (10 ms poll, ≤30 s deadline per wait) reusing the suite's context conventions. A test that cannot reach its predicate must fail with the tail of the raw log and the current screen dump in the message — never hang.
- Cross-OS: unix PTY only (macOS/Linux/WSL2 are the supported targets; the e2e package is already unix-shaped via its shell-script fakegh). No Windows build tags needed beyond what xpty provides.
- Commit messages end with the `Co-Authored-By:` trailer the harness specifies.

## Orientation (verified facts, with sources)

- The e2e suite builds the real binary once in `TestMain` (`test/e2e/harness_test.go:50`, `binPath`) and boots a real daemon against a hermetic HOME/XDG environment with a fake `gh` (`fakegh_test.go`, `hub_semantics_test.go`). Reuse that machinery; do not invent a second bootstrap.
- `decideHubEntry(initialized, tty, agentEnv)` (`internal/cli/hub.go:87-98`) returns `hubOpen` whenever initialized && tty — the agent-env fingerprint check only gates guided init. A PTY makes tty true; the test process's inherited `CLAUDECODE` env is therefore harmless, but scrub agent-env vars from the child env anyway so the suite doesn't depend on that ordering.
- Daemon-down replaces the entire hub body (`internal/cli/dashboard/dashboard.go:2126-2127`), so browsing/reading requires the daemon RUNNING. All scenarios run against the shared harness daemon.
- Quit path: `q` sets `quitPrompt` (`dashboard.go:1038`, `:1194`); the prompt's confirm/deny keys are handled at `dashboard.go:1113-1125` — read that block and drive the confirm key it actually binds.
- The startup arm is one `tea.Raw` payload `"\x1b[?1007s\x1b[?1007h"` batched in `Init` (`dashboard.go:518-528`), gated on `dashboard.alternate_scroll` (default true). The renderer may split writes arbitrarily — assert ORDER of the three markers (`?1049h` then `?1007s` then `?1007h`), never adjacency.
- Exit teardown writes `"\x1b[?1007l\x1b[?1007r"` to stdout AFTER `program.Run()` returns (`internal/cli/dashboard.go:159-164`) — i.e., after bubbletea restores the primary screen. The tail assertion (last `?1049l` precedes the final `?1007l` which precedes `?1007r`) is exactly the property no unit test can see.
- The reading viewport binds arrows alongside j/k (`internal/cli/dashboard/views/reading.go:177-178`); ultraviolet decodes both CSI (`\x1b[B`) and SS3 (`\x1bOB`) arrow encodings. A 1007 terminal sends CSI form in normal cursor-key mode and SS3 in application mode — send both, as subtests.
- Browser click handling: SGR press at a list row moves the cursor (`views/browser.go:791-794` via the `tea.MouseClickMsg` case at `:446`); clicks only arrive while the preview's cell-motion capture is armed, which requires a browser-with-preview frame at two-pane width — use a 120×40 PTY.
- The browser sorts the index memory first and default-selects it (cursor 0 = MEMORY.md); seed one project with an index plus one long memory whose body is `line-001` … `line-200` so scroll position is assertable by which line numbers are on screen and by the reading view's `── N% ──` hint.
- Editor handoff: with `EDITOR=true` (or a `#!/bin/sh\nexit 0` script), the edit flow's scratch round-trip exits unchanged → "no changes" outcome; the `editorFinishedMsg` re-assert fires regardless of outcome (`dashboard.go:938-947`) and deliberately does NOT re-save. Wire consequence: a session containing one edit round-trip has exactly ONE `?1007s` and at least TWO `?1007h`.
- bubbletea v2 emits terminal capability queries at startup; huh v2's xpty-driven tests prove the program proceeds without a responding terminal. Set `TERM=xterm-256color` in the child env. If any scenario stalls on an unanswered query, the harness reader may answer the specific query it observes — document any such response with its WHY.
- vt10x usage: `term := vt10x.New(vt10x.WithSize(cols, rows))`; it implements `io.Writer`; `term.String()` dumps the grid. All access from the harness goes through one mutex (reader goroutine writes, assertions read).

## File Structure

- `test/e2e/ptyharness_test.go` — the `hubSession` harness: spawn, teed capture (raw + vt10x), send, waitRaw/waitScreen, quit, cleanup. One responsibility: PTY transport + capture.
- `test/e2e/pty_hub_test.go` — the six scenarios, each a focused test function over the shared fixture.
- `go.mod` / `go.sum` — xpty and vt10x become direct (test-only) requirements.
- `docs/decisions/21-adr-alternate-scroll.md` — append-only "Automated wire-contract coverage" section; manual smoke matrix narrowed to emulator/config cells.

---

### Task 1: PTY harness + six-scenario wire battery

**Files:**
- Create: `test/e2e/ptyharness_test.go`
- Create: `test/e2e/pty_hub_test.go`
- Modify: `go.mod`, `go.sum` (add `github.com/charmbracelet/x/xpty` v0.1.3, `github.com/hinshun/vt10x` latest)
- Read first: `test/e2e/harness_test.go`, `test/e2e/hub_semantics_test.go`, `test/e2e/fakegh_test.go` (bootstrap to reuse), `internal/cli/dashboard/dashboard.go:1113-1125` (quit confirm key)

**Interfaces:**
- Consumes: `binPath` + the suite's hermetic env/daemon bootstrap from `harness_test.go`/`hub_semantics_test.go`; the wire sequences pinned by `internal/cli/dashboard/altscroll_test.go`.
- Produces: `startHubSession(t, sessionConfig) *hubSession` with `send(string)`, `waitRaw(func(string) bool) string`, `waitScreen(func(string) bool) string`, `quitAndDrain() string` — Task 2 does not consume code, only the battery's existence and names for the ADR text.

- [ ] **Step 1: Bootstrap + harness + smoke assertion (arm order).** Wire the shared fixture (hermetic env, daemon up, one project seeded with MEMORY.md + `long-scroll-target.md` containing lines `line-001`…`line-200`), implement `hubSession` (xpty spawn of `binPath` with cols/rows, env `TERM=xterm-256color` + hermetic vars, reader goroutine teeing into raw buffer + vt10x under one mutex), and the first test:

```go
// TestPTYHubArmsAlternateScrollInOrder proves the arm sequence's position in
// the real render stream: alt-screen entry first, then XTSAVE, then DECSET —
// the ordering a terminal needs for the save to capture pre-hub state. Unit
// tests pin the payload bytes; only the PTY sees where the renderer puts them.
func TestPTYHubArmsAlternateScrollInOrder(t *testing.T) {
	t.Parallel()
	s := startHubSession(t, defaultSessionConfig())
	raw := s.waitRaw(func(raw string) bool {
		return strings.Contains(raw, "\x1b[?1007h")
	})
	altScreen := strings.Index(raw, "\x1b[?1049h")
	save := strings.Index(raw, "\x1b[?1007s")
	set := strings.Index(raw, "\x1b[?1007h")
	if altScreen == -1 || save == -1 || set == -1 {
		t.Fatalf("missing arm markers: 1049h=%d 1007s=%d 1007h=%d\nraw tail: %q", altScreen, save, set, tail(raw))
	}
	if !(altScreen < save && save < set) {
		t.Errorf("arm order wrong: 1049h@%d 1007s@%d 1007h@%d", altScreen, save, set)
	}
	s.quitAndDrain()
}
```

Prove the harness is honest before moving on: run once with the assertion inverted (expect `save < altScreen`) and capture the red.

- [ ] **Step 2: Wheel bytes scroll the reading view (both encodings).** Navigate: wait for the browser screen to show the seeded project's memories, `j` to `long-scroll-target`, `enter`, wait for `line-001` on screen. Then per subtest send three wheel-down translations and assert the screen scrolled:

```go
func TestPTYWheelBytesScrollReadingView(t *testing.T) {
	t.Parallel()
	encodings := []struct {
		name string
		down string
	}{
		{name: "csi-cursor-keys", down: "\x1b[B"},
		{name: "ss3-application-mode", down: "\x1bOB"},
	}
	for _, encoding := range encodings {
		t.Run(encoding.name, func(t *testing.T) {
			t.Parallel()
			s := startHubSession(t, defaultSessionConfig())
			openLongMemory(t, s) // browser → j → enter → wait "line-001"
			for i := 0; i < 3; i++ {
				s.send(encoding.down)
			}
			s.waitScreen(func(screen string) bool {
				return !strings.Contains(screen, "line-001") &&
					strings.Contains(screen, "line-004")
			})
			s.quitAndDrain()
		})
	}
}
```

(If the reading view keeps `line-001` visible after 3 notches because the viewport top padding differs, adjust the expected line pair from the observed geometry and say so in the report — the assertion's substance is "top line advanced by the wheel notches", not a magic number.)

- [ ] **Step 3: SGR click bytes select a browser row.** At 120×40 (two-pane), wait for the browser list, compute a target row's screen line from the rendered frame (find the row's memory name in the vt10x dump and use its 1-based row), send SGR press+release at column 3 of that line: `fmt.Sprintf("\x1b[<0;%d;%dM\x1b[<0;%d;%dm", col, row, col, row)`. Assert the selection marker (`>` cursor) moved to that memory's row on screen and the preview pane re-targeted (its title shows the clicked memory's name).

- [ ] **Step 4: Editor round-trip re-asserts without re-saving.** Session config sets `EDITOR` to a `t.TempDir()` script `#!/bin/sh\nexit 0`. Open the long memory, press the edit key, wait for the post-editor frame (the "no changes" outcome toast or the reading view back), then assert on raw: `strings.Count(raw, "\x1b[?1007s") == 1` and `strings.Count(raw, "\x1b[?1007h") >= 2`. This is the wire-level negative pin for the no-re-save decision.

- [ ] **Step 5: Quit teardown tail order.** Fresh session; quit via `q` + the confirm key from `dashboard.go:1113-1125`; `quitAndDrain` waits for process exit and returns the complete raw log. Assert: `lastIndex(raw, "\x1b[?1049l") < lastIndex(raw, "\x1b[?1007l") < lastIndex(raw, "\x1b[?1007r")` and all three exist — the reset-then-restore lands after the framework's primary-screen restore, on every path our users quit through.

- [ ] **Step 6: Kill-switch emits zero 1007 bytes.** Session config writes `dashboard.alternate_scroll = false` into the hermetic config.toml. Full open → browse → quit cycle; assert `!strings.Contains(raw, "[?1007")` over the entire session. This test doubles as the standing negative control proving every presence assertion in Steps 1-5 is load-bearing.

- [ ] **Step 7: Full package run + lint.**

Run: `go test ./test/e2e/ -race -count=1 -run 'TestPTY' -v` — all six green, then the whole e2e package, then `gofumpt -l .` and `golangci-lint run`.

- [ ] **Step 8: Commit.**

```bash
git add test/e2e/ptyharness_test.go test/e2e/pty_hub_test.go go.mod go.sum
git commit -m "test(e2e): PTY battery pins the alternate-scroll wire contract

Drives the real binary on a real pty and asserts what unit tests
cannot: arm order relative to alt-screen entry, wheel-byte scrolling
in both arrow encodings, SGR click row selection, a single XTSAVE per
session across editor handoffs, the reset-then-restore teardown tail
after the framework's restore, and a kill-switch session with zero
1007 bytes on the wire."
```

### Task 2: ADR 21 amendment — automated coverage narrows the manual matrix

**Files:**
- Modify: `docs/decisions/21-adr-alternate-scroll.md`

**Interfaces:**
- Consumes: the six test names and their one-line contracts from Task 1 (read them from `test/e2e/pty_hub_test.go`, never invent).
- Produces: documentation only.

- [ ] **Step 1: Append an "Automated wire-contract coverage" section** (append-only; do not rewrite existing decisions): list the six PTY scenarios by test name with a one-line statement of what each pins; record the two test-only dependencies (xpty — charm's PTY harness, the same transport huh v2 tests with; vt10x — the VT screen model the Go ecosystem's expect-style tests standardized on; both MIT) and the buy-over-build reasoning; state the boundary adjudication (test/e2e importing them is harness, not TUI).

- [ ] **Step 2: Narrow the manual smoke matrix in place.** Rewrite the matrix passage so the cells the battery now pins are marked automated, and the remaining manual cells are exactly the emulator/config residue: does a given emulator translate under 1007 (iTerm2/Terminal.app/Windows Terminal by hand; kitty documented always-translates; tmux documented swallow + user binding), the user's own terminal config (pre-armed preference round-trip on XTSAVE terminals), the Cursor/xterm.js cell, and OSC52 clipboard reception. Keep the ADR's prevailing wrap width; no code spans broken across lines.

- [ ] **Step 3: Commit.**

```bash
git add docs/decisions/21-adr-alternate-scroll.md
git commit -m "docs(adr): record PTY battery coverage; narrow the manual matrix

ADR 21's smoke matrix now distinguishes what the PTY battery pins
automatically from the emulator- and config-side cells only a human
at a real terminal can verify."
```

---

## Verification (whole-branch, before merge)

- [ ] `go build ./...`
- [ ] `(ulimit -u 1400; go test ./... -race -count=1)` — foreground
- [ ] `gofumpt -l .` → empty; `golangci-lint run` → 0 issues; `go tool deadcode -test ./...` → empty
- [ ] Boundary greps: engine/provider charm-free; xpty/vt10x imports confined to `test/e2e`
- [ ] Flake check: `go test ./test/e2e/ -race -count=3 -run 'TestPTY'` — three consecutive clean runs (PTY timing is the classic flake source; the poll-until-predicate discipline is the defense, this run is the proof)

## Decision records

1. **xpty over creack/pty directly:** identical unix mechanics, but xpty is charm-maintained, already in the module graph via huh v2, and its huh test usage is the existence proof the whole charm stack runs on it. creack/pty arrives transitively either way.
2. **vt10x over raw-stream grepping for screen state:** bubbletea v2's renderer emits cell diffs, not frames; only a screen model makes "what is visible" assertable. vt10x is the ecosystem-standard choice (go-expect/survey lineage).
3. **Shared daemon + per-test hub sessions:** all six scenarios are read-only against the store (the editor scenario exits unchanged by construction), so one hermetic daemon serves parallel PTY sessions — matching the suite's existing shared-fixture economics.
4. **This battery tests OUR program against a spec-correct terminal, not emulators:** validating iTerm2/kitty/WT behavior is out of scope here (Xvfb+xterm and Playwright+xterm.js jobs were considered and deferred to close-out housekeeping — they test third parties, rank below the our-side contract, and carry CI weight).

## Explicitly recorded non-goals

- No Xvfb/xterm or Playwright/xterm.js emulator-side jobs (deferred with reasons above; candidate for close-out housekeeping alongside lychee).
- No Windows ConPTY leg — supported targets are macOS/Linux/WSL2, all unix PTYs.
- No OSC52 clipboard assertion — the terminal-side effect is unobservable from a PTY; the emitted OSC52 payload is already unit-pinned.

## Execution deltas (empirical corrections + review adjudications, recorded before merge)

The plan's own Orientation and skeletons predicted two wire facts wrong; the battery's first live
runs corrected them, and the shipped tests plus ADR 21's dated amendment pin reality. Recorded here
so this plan is never read as truth against the code — the superseded lines above stay as written,
per the append-only plan-history convention:

- **Arm order.** Orientation's "assert ORDER of the three markers (`?1049h` then `?1007s` then
  `?1007h`)" and Step 1's skeleton (its comment and the `altScreen < save && save < set` assertion)
  are empirically false: the real wire order is `1007s` < `1007h` < `1049h` — the single `tea.Raw`
  arm payload flushes BEFORE bubbletea's alt-screen entry. `1007s` < `1007h` is the semantic core
  (the save must capture pre-hub state before the set overwrites it; DEC private-mode state is
  terminal-global), while `1007h` < `1049h` is a version-coupled snapshot of bubbletea v2's flush
  order. `TestPTYHubArmsAlternateScrollInOrder` pins the corrected order, red-proven by inversion.
- **Quit model.** Orientation's "`q` sets `quitPrompt`" and Step 5's "quit via `q` + the confirm
  key" inverted the routes: `q` is an immediate global quit; `esc` raises the confirm prompt, and
  only from the top-level Projects tab (pushed screens consume `esc` to pop themselves). The
  battery quits pushed-screen scenarios via ctrl+c and drives the interactive route as esc→y in
  `TestPTYQuitRestoresAlternateScrollTail`; both funnel through the shared teardown-tail assertion.
- **Wheel geometry.** The "dropped first notches" appearance during development was a
  test-predicate illusion, not input loss: every notch scrolls exactly one line from the very
  first, but glamour renders heading chrome ahead of the body, so the body's first line sits ~4
  rows below the viewport top and absence-predicates need 4+ notches to observe anything leave.
  The shipped wheel test pairs a positive single-notch pin (derived line numbers, with the
  second notch's line provably absent on the same snapshot) with a drive-to-outcome deeper
  scroll; ADR 21's "Automated wire-contract coverage" section carries the full finding.
- **Final-review fix round.** The kill-switch scenario additionally drives one $EDITOR
  round-trip, covering the `editorFinishedMsg` re-assert's disabled gate — previously the one
  mutation (hoisting the re-assert out of its config gate) the whole suite could not catch. The
  alt-scroll plan's supersession note was corrected to say the battery drives the binary its
  TestMain builds, not "the installed binary" — that word belongs to the self-update vocabulary.
- **Accepted residual.** `waitStableMaxVisibleLineNumber`'s two-consecutive-poll quiet-frame gate
  carries a theoretical under-read window if a frame stalls longer than one poll interval
  mid-scroll; proven green across repeated `-race -count=3` rounds. Harden to three equal reads on
  the first observed flake — not before, since each extra sample widens every wheel test's floor
  latency to defend against a window nobody has observed.
