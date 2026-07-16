package e2e

// The six alternate-scroll wire scenarios. Each spawns a hub on its own pty
// against the shared daemon (ptyharness_test.go), drives it with terminal-real
// input bytes, and asserts on the raw render stream (byte order) or the vt10x
// screen (rendered state) — the two things no unit test can reach. Navigation
// helpers below are shared; the transport (spawn, capture, waits, quit, wheel
// driver) lives in ptyharness_test.go.

import (
	"fmt"
	"strings"
	"testing"
)

// openBrowser takes the hub from its opening Projects tab into the seeded
// project's memory browser and returns the two-pane frame. The hub opens with
// the sole seeded unit selected (the Projects table default-selects row 0), so
// enter opens ITS browser; waiting for the long memory's name confirms the
// browser listed the seeded memories before the caller acts on the frame.
func openBrowser(t *testing.T, s *hubSession) string {
	t.Helper()
	s.waitScreen(func(screen string) bool { return strings.Contains(screen, seededFolder) })
	s.send("\r")
	return s.waitScreen(func(screen string) bool { return strings.Contains(screen, longMemoryName) })
}

// openLongMemory drives from the browser into the long memory's reading view: j
// moves the selection off the index (row 0) onto the long memory, enter opens
// it. It waits for the selection to actually land on the long memory before
// opening (so a mis-timed enter cannot open the index instead), then for the
// reading view's own footer token — "backlinks" appears in the reading view's
// key hints and never the browser's — so callers know the reading view, not the
// browser preview (which also shows line-001 once the long memory is selected),
// is the live screen.
func openLongMemory(t *testing.T, s *hubSession) {
	t.Helper()
	openBrowser(t, s)
	s.send("j")
	s.waitScreen(func(screen string) bool { return lineHasMarker(screen, ">", longMemoryName) })
	s.send("\r")
	s.waitScreen(func(screen string) bool { return strings.Contains(screen, "backlinks") })
}

// lineHasMarker reports whether one rendered line's LIST-PANE columns (see
// listPaneOf, ptyharness_test.go) contain BOTH marker and token — the test for
// "the selection cursor (>) is on the row that names X", as distinct from "> and
// X each appear somewhere on screen". Anchoring to the list pane matters
// concretely here: the index's own previewed body contains the prose "See
// long-scroll-target.", so an unanchored scan of the FULL screen could satisfy
// "X appears on this row" from the preview pane rather than the list row this
// helper is meant to test for.
func lineHasMarker(screen, marker, token string) bool {
	for line := range strings.SplitSeq(screen, "\n") {
		pane := listPaneOf(line)
		if strings.Contains(pane, marker) && strings.Contains(pane, token) {
			return true
		}
	}
	return false
}

// TestPTYHubArmsAlternateScrollInOrder proves where the alternate-scroll arm
// lands in the real render stream relative to the alternate-screen entry —
// something only a PTY sees, because bubbletea's diff renderer decides where the
// Init tea.Raw payload is flushed. Unit tests pin the payload bytes
// ("\x1b[?1007s\x1b[?1007h"); they cannot see that the renderer emits it before
// entering the alternate screen.
//
// Observed order, deterministic across runs: XTSAVE(1007s) < DECSET(1007h) <
// alt-screen(1049h). Two distinct facts ride on that:
//   - save < set is the correctness core: XTSAVE must capture the terminal's
//     pre-hub 1007 state BEFORE our own DECSET arms it, so the exit XTRESTORE
//     can hand the user's own preference back. The two are byte-adjacent
//     (one Init payload), but the ORDER within it is the thing that matters.
//   - set < altScreen is bubbletea v2's flush order: it drains the program
//     output buffer (where an executed tea.Raw lands) before the renderer flush
//     (where the alternate-screen enter lands) on each frame, so the arm
//     precedes 1049h. This is the reverse of the direction a naive reading
//     assumes (alt-screen first); the save still captures pre-hub state
//     regardless, because the 1007 mode XTSAVE reads is terminal-global and
//     unchanged until our DECSET, wherever the alt-screen switch falls.
//
// Synchronization waits for 1049h specifically: it is the LAST of the three to
// arrive, so its presence guarantees all three are on the wire before the order
// is read (waiting only for 1007h can sample a stream that has not yet emitted
// 1049h and read a spurious "missing" alt-screen).
func TestPTYHubArmsAlternateScrollInOrder(t *testing.T) {
	t.Parallel()
	s := startHubSession(t, defaultSessionConfig())
	raw := s.waitRaw(func(raw string) bool {
		return strings.Contains(raw, "\x1b[?1049h") &&
			strings.Contains(raw, "\x1b[?1007s") &&
			strings.Contains(raw, "\x1b[?1007h")
	})
	save := strings.Index(raw, "\x1b[?1007s")
	set := strings.Index(raw, "\x1b[?1007h")
	altScreen := strings.Index(raw, "\x1b[?1049h")
	armInOrder := save < set && set < altScreen
	if !armInOrder {
		t.Errorf("arm order wrong: want 1007s < 1007h < 1049h, got 1007s@%d 1007h@%d 1049h@%d\nraw tail: %q",
			save, set, altScreen, tail(raw))
	}
	s.quitAndDrain()
}

// TestPTYWheelBytesScrollReadingView proves the wheel actually scrolls the
// reading view through the alternate-scroll contract: with 1007 armed a wheel
// notch reaches the app as a cursor-DOWN escape, and the reading viewport
// (which binds down/j) scrolls. Both wire encodings a 1007 terminal can emit are
// exercised — CSI (\x1b[B) in normal cursor-key mode, SS3 (\x1bOB) in
// application cursor-key mode. ultraviolet's key table decodes both to the same
// KeyDown, so each is the genuine wheel output for its mode; running them as
// sibling subtests documents that neither wire form is special-cased.
//
// The long memory's body is a fenced block of line-001…line-200, so the code
// fence renders each on its own row and "the top line advanced" is a legible
// screen fact. Each subtest pins the contract at two grains:
//
//  1. A single-notch pin (below) proves ONE notch produces a genuine one-line
//     scroll, via the POSITIVE signal that scroll produces — a previously
//     off-screen line appearing at the viewport's new bottom edge — never via
//     line-001's absence. line-001 alone is the wrong predicate for one notch:
//     a probe-investigation (.superpowers/sdd/probe-investigation.md) proved it
//     stays on screen for several notches purely from render geometry (glamour's
//     "# H1" heading plus a blank line ahead of the fence, plus the reading
//     view's own two chrome lines — views/reading.go's own chromeLines — put
//     line-001 several rows below the viewport's top edge), NOT because any
//     notch was dropped; every notch reacts on the wire identically from the
//     first.
//  2. scrollByWheel then drives to a deeper-scroll outcome (line-001 gone,
//     line-004 present) with no fixed notch count assumed, because exactly how
//     many notches clear line-001 is a function of that same render geometry —
//     heading size, chrome, terminal height — not a constant this test should
//     hardcode or a sign of lost input (scrollByWheel's own doc has the full
//     mechanism).
func TestPTYWheelBytesScrollReadingView(t *testing.T) {
	t.Parallel()
	encodings := []struct {
		name string
		down string
	}{
		{name: "csi-normal-cursor-keys", down: "\x1b[B"},
		{name: "ss3-application-cursor-keys", down: "\x1bOB"},
	}
	for _, encoding := range encodings {
		t.Run(encoding.name, func(t *testing.T) {
			t.Parallel()
			s := startHubSession(t, defaultSessionConfig())
			openLongMemory(t, s)
			// Precondition: the fresh reading view is showing the document top.
			preNotch := s.waitScreen(func(screen string) bool { return strings.Contains(screen, "line-001") })

			// Single-notch pin: send exactly ONE notch, then wait for the
			// previously off-screen line one past everything CURRENTLY visible
			// (derived from preNotch, not hardcoded — line-026 under today's
			// 120x40 geometry, but this holds under any viewport height) to
			// appear. That is the positive proof a single notch scrolls exactly
			// one line; see the doc above for why line-001's absence is the
			// wrong predicate at this grain.
			revealed := fmt.Sprintf("line-%03d", maxVisibleLineNumber(preNotch)+1)
			s.send(encoding.down)
			s.waitScreen(func(screen string) bool { return strings.Contains(screen, revealed) })

			s.scrollByWheel(encoding.down, func(screen string) bool {
				return !strings.Contains(screen, "line-001") && strings.Contains(screen, "line-004")
			})
			s.quitAndDrain()
		})
	}
}

// maxVisibleLineNumber returns the highest N such that "line-NNN" is currently
// on screen, or 0 if none is. The single-notch pin uses it to derive, from the
// ACTUAL pre-notch geometry rather than a hardcoded row count, exactly which
// line-NNN a one-line scroll must newly reveal at the viewport's bottom edge —
// the one line number past everything already visible. Cheap by construction:
// longMemoryLineCount (200) Contains checks against a ~4800-byte screen
// string, negligible next to the PTY round trip the caller already pays for.
func maxVisibleLineNumber(screen string) int {
	highest := 0
	for n := 1; n <= longMemoryLineCount; n++ {
		if strings.Contains(screen, fmt.Sprintf("line-%03d", n)) {
			highest = n
		}
	}
	return highest
}

// TestPTYClickBytesSelectBrowserRow proves an SGR mouse click selects the
// browser row it lands on — the click half of the alternate-scroll story, where
// the browser preview's cell-motion capture (armed only at two-pane width, which
// is why the session is 120 cols) takes precedence over 1007 so a click reports
// as a real mouse event rather than translating to a wheel arrow. It aims at the
// long memory's ACTUAL rendered row (read from the vt10x grid, since the chrome
// height above the list is not a constant) and sends an SGR press+release there.
//
// Before the click the selection sits on the index (row 0) and the preview shows
// the index body (line-001 absent); after, the selection cursor (>) is on the
// long memory's row AND the preview has re-targeted to its body (line-001 now on
// screen). Asserting both proves the click moved the selection and drove the
// preview, not merely that > appears somewhere.
func TestPTYClickBytesSelectBrowserRow(t *testing.T) {
	t.Parallel()
	s := startHubSession(t, defaultSessionConfig())
	browser := openBrowser(t, s)
	// Baseline: the index is selected and previewed, so the long memory is
	// neither the marked row nor the previewed body yet — without this the
	// post-click assertion could pass on a hub that opened already selecting it.
	if lineHasMarker(browser, ">", longMemoryName) {
		t.Fatalf("setup: long memory already selected before the click\nscreen:\n%s", browser)
	}
	if strings.Contains(browser, "line-001") {
		t.Fatalf("setup: long memory body already previewed before the click\nscreen:\n%s", browser)
	}
	row := lineRowOf(browser, longMemoryName)
	if row == 0 {
		t.Fatalf("setup: long memory row not found in browser frame\nscreen:\n%s", browser)
	}
	// SGR press (M) then release (m) of button 0 at column 3 of the memory's
	// row — the byte form xterm sends for a left click under SGR mouse
	// reporting (mode 1006).
	s.send(fmt.Sprintf("\x1b[<0;%d;%dM\x1b[<0;%d;%dm", 3, row, 3, row))
	// line-001 (the BODY) is the stronger of the two available re-target
	// signals, deliberately stronger than the memory's TITLE: the title also
	// appears in the list row itself, so it would be satisfied by the
	// selection move alone, whereas line-001 can only come from the preview
	// pane actually rendering the long memory's body.
	s.waitScreen(func(screen string) bool {
		return lineHasMarker(screen, ">", longMemoryName) && strings.Contains(screen, "line-001")
	})
	s.quitAndDrain()
}

// TestPTYEditorRoundTripReAssertsWithoutReSaving pins the no-re-save decision on
// the wire: an edit handoff re-arms DECSET on return (so the wheel keeps working
// after the editor briefly owned the terminal) but does NOT re-emit XTSAVE (a
// second save would overwrite the pre-hub state the first one captured). With an
// $EDITOR that exits without changing the scratch, the flow reaches its "no
// changes" outcome, and across the whole session the wire carries exactly one
// XTSAVE and at least two DECSETs (Init's arm plus the post-editor re-assert).
//
// The "edit cancelled, no changes made" toast is the independent completion
// signal — it renders from the same editorFinishedMsg cycle that emits the
// re-assert — so waiting for it before quitting guarantees the re-assert is on
// the wire without the assertion waiting on the very counts it checks. The exit
// teardown writes 1007l/1007r, never 1007s/1007h, so counting over the complete
// drained log is exact.
func TestPTYEditorRoundTripReAssertsWithoutReSaving(t *testing.T) {
	t.Parallel()
	cfg := defaultSessionConfig()
	cfg.editorScript = writeNoopEditor(t)
	s := startHubSession(t, cfg)
	openLongMemory(t, s)
	s.send("e")
	s.waitScreen(func(screen string) bool { return strings.Contains(screen, "no changes") })
	raw := s.quitAndDrain()
	if got := strings.Count(raw, "\x1b[?1007s"); got != 1 {
		t.Errorf("XTSAVE count = %d, want exactly 1 (one save per session, never re-saved on editor return)\nraw tail: %q", got, tail(raw))
	}
	if got := strings.Count(raw, "\x1b[?1007h"); got < 2 {
		t.Errorf("DECSET count = %d, want >= 2 (Init arm + post-editor re-assert)\nraw tail: %q", got, tail(raw))
	}
}

// TestPTYQuitRestoresAlternateScrollTail drives the interactive top-level quit
// confirmation — esc raises "quit agent-brain? (y/n)", y confirms — the ONE
// scenario in this battery that ever exercises that route; every other
// scenario ends inside a pushed screen and quits with ctrl+c instead
// (quitAndDrain's own doc explains why: a pushed screen consumes esc to pop
// itself and consumes q whole, so the top-level confirm is only reachable from
// the Projects tab).
//
// The actual teardown-tail assertion — after bubbletea restores the primary
// screen (1049l), launchHub writes DECRST(1007l) then XTRESTORE(1007r), reset
// first so terminals without XTSAVE/XTRESTORE support land on a plain reset —
// is NOT manual here. It lives in the shared drainAfterQuit path
// (assertTeardownTailOrder, ptyharness_test.go), which quitViaPromptAndDrain
// below runs identically to every ctrl+c scenario's quitAndDrain. Driving THIS
// one scenario through esc→y is what proves the shared assertion also holds on
// the documented interactive path, not only the unconditional-quit shortcut.
func TestPTYQuitRestoresAlternateScrollTail(t *testing.T) {
	t.Parallel()
	s := startHubSession(t, defaultSessionConfig())
	// Wait until the hub is fully up on the Projects tab with the daemon
	// connected — the seeded unit's folder is on screen only once Projects has
	// data. Quitting earlier, during the brief daemon-down startup window, finds
	// esc inert: the daemon-down screen consumes it without raising the quit
	// prompt (dashboard.go handleKey), and the confirm below would then wait on a
	// prompt that never opens. Reaching a data-bearing Projects frame also implies
	// the arm already happened, since Init emits 1007 before the daemon ever
	// answers, so the shared tail assertion runs over a session that armed it.
	s.waitScreen(func(screen string) bool { return strings.Contains(screen, seededFolder) })
	s.quitViaPromptAndDrain()
}

// TestPTYKillSwitchEmitsNoAlternateScrollBytes is the standing negative control
// for every 1007-presence assertion in this battery: with dashboard.
// alternate_scroll = false, a full open→browse→read→quit cycle must put ZERO
// 1007 bytes on the wire — Init emits neither XTSAVE nor DECSET, and
// RestoreAlternateScroll is a no-op, so there is no 1007l/1007r either. A
// refactor that armed 1007 unconditionally would trip this; and this test's
// silence is what makes the other scenarios' "1007 present" assertions
// load-bearing rather than vacuous.
func TestPTYKillSwitchEmitsNoAlternateScrollBytes(t *testing.T) {
	t.Parallel()
	store := ensureHubStore(t)
	cfg := defaultSessionConfig()
	cfg.configDirOverride = store.killSwitchConfigDir(t)
	s := startHubSession(t, cfg)
	// Exercise the same surfaces the armed scenarios do, so a stray 1007 from
	// any of them would surface here: open the browser, drop into the reading
	// view, then quit.
	openLongMemory(t, s)
	raw := s.quitAndDrain()
	if strings.Contains(raw, "[?1007") {
		t.Errorf("kill-switch session emitted 1007 bytes on the wire; want none\nraw tail: %q", tail(raw))
	}
}
