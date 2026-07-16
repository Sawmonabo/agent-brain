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

// lineHasMarker reports whether one rendered line contains BOTH marker and
// token — the test for "the selection cursor (>) is on the row that names X",
// as distinct from "> and X each appear somewhere on screen".
func lineHasMarker(screen, marker, token string) bool {
	for line := range strings.SplitSeq(screen, "\n") {
		if strings.Contains(line, marker) && strings.Contains(line, token) {
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
// screen fact. line-001 sits at the top of the fresh viewport; after the wheel
// has scrolled, it is gone while line-004 (still within the document) is on
// screen — top advanced, with no magic notch count (scrollByWheel drives to the
// outcome because the just-pushed viewport drops its first notches).
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
			s.waitScreen(func(screen string) bool { return strings.Contains(screen, "line-001") })
			s.scrollByWheel(encoding.down, func(screen string) bool {
				return !strings.Contains(screen, "line-001") && strings.Contains(screen, "line-004")
			})
			s.quitAndDrain()
		})
	}
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

// TestPTYQuitRestoresAlternateScrollTail pins the exit teardown's tail order on
// the documented interactive quit: after bubbletea restores the primary screen
// on quit, launchHub writes DECRST(1007l) then XTRESTORE(1007r) — reset first so
// terminals without XTSAVE/XTRESTORE support land on a plain reset, restore
// second so terminals that support it get the user's pre-hub 1007 preference
// back. Only a PTY that reads past process exit sees these bytes: they are
// written AFTER program.Run returns. The tail is asserted by LAST occurrence
// (the session armed 1007h earlier; the teardown's 1007l/1007r are the final
// ones), with the framework's primary-screen restore (1049l) preceding both.
//
// The session stays on the Projects tab and quits through esc→prompt→y — the
// interactive quit-with-confirmation, reachable only from the top level (a
// pushed screen consumes esc to pop itself, and consumes q whole). That is
// exactly why the scenarios ending inside a pushed screen quit with ctrl+c
// instead (quitAndDrain); the teardown is identical on every path.
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
	// answers, so the tail assertion is over a session that armed it.
	s.waitScreen(func(screen string) bool { return strings.Contains(screen, seededFolder) })
	raw := s.quitViaPromptAndDrain()
	restorePrimary := strings.LastIndex(raw, "\x1b[?1049l")
	resetScroll := strings.LastIndex(raw, "\x1b[?1007l")
	restoreScroll := strings.LastIndex(raw, "\x1b[?1007r")
	if restorePrimary == -1 || resetScroll == -1 || restoreScroll == -1 {
		t.Fatalf("missing teardown markers: 1049l=%d 1007l=%d 1007r=%d\nraw tail: %q",
			restorePrimary, resetScroll, restoreScroll, tail(raw))
	}
	tailInOrder := restorePrimary < resetScroll && resetScroll < restoreScroll
	if !tailInOrder {
		t.Errorf("teardown tail order wrong: want 1049l < 1007l < 1007r, got 1049l@%d 1007l@%d 1007r@%d\nraw tail: %q",
			restorePrimary, resetScroll, restoreScroll, tail(raw))
	}
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
