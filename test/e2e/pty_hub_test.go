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
	"time"
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
//  1. A single-notch pin (below) proves ONE notch reveals EXACTLY one new
//     line, not merely "at least one": send ONE notch, wait for the
//     previously off-screen line one past everything CURRENTLY visible to
//     appear (the POSITIVE signal a scroll produces — never line-001's
//     absence, see below), then, on that SAME snapshot, assert the line TWO
//     past pre-notch's max is still absent. That negative half is what earns
//     "exactly one": without it, a wheel notch that decoded into two KeyDown
//     events would satisfy the positive half just as well, and the unit-layer
//     magnitude pin can't catch that either — it pins the translation of a
//     single KeyDown, not how many KeyDowns arrive per physical notch. The
//     long memory is 200 lines against a viewport showing roughly two dozen,
//     so the "two past max" line always names a real line still in the
//     document body; its absence is a fact about this notch, never a false
//     pass from running off the document's end. line-001's own absence is
//     the wrong predicate for one notch: ADR 21
//     (docs/decisions/21-adr-alternate-scroll.md)'s "Automated wire-contract
//     coverage" amendment persists the finding that it stays on screen for
//     several notches purely from render geometry (glamour's "# H1" heading
//     plus a blank line ahead of the fence, plus the reading view's own two
//     chrome lines — views/reading.go's own chromeLines — put line-001
//     several rows below the viewport's top edge), NOT because any notch was
//     dropped; every notch reacts on the wire identically from the first.
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
			// Precondition: the fresh reading view is showing the document
			// top, settled — not merely the first frame in which line-001
			// happens to appear (waitStableMaxVisibleLineNumber's own doc has
			// the full reasoning: a mid-paint read here would under-read the
			// max below and make the pin measure the paint, not the notch).
			preNotch := waitStableMaxVisibleLineNumber(t, s)

			// Single-notch pin: send exactly ONE notch, then wait for the
			// previously off-screen line one past everything CURRENTLY visible
			// (derived from preNotch, not hardcoded — line-026 under today's
			// 120x40 geometry, but this holds under any viewport height) to
			// appear. That is the positive proof a single notch scrolls AT
			// LEAST one line; see the doc above for why line-001's absence is
			// the wrong predicate at this grain.
			maxVisibleBeforeNotch := maxVisibleLineNumber(preNotch)
			revealed := fmt.Sprintf("line-%03d", maxVisibleBeforeNotch+1)
			// secondNotchReveal is the line a SECOND notch would newly
			// reveal. Asserting its absence right after the FIRST notch is
			// the negative half that upgrades the pin from "at least one
			// line" to "exactly one": a wheel notch that decoded into two
			// KeyDown events would still satisfy `revealed` above, and the
			// unit-layer magnitude pin (views/reading_test.go's
			// TestReadingViewportScroll) can't catch that either, since it
			// pins the translation of a single KeyDown, not how many
			// KeyDowns arrive per physical notch. The document is 200 lines
			// and the viewport shows roughly two dozen, so this line number
			// always names a real line still in the body — its absence here
			// is a fact about THIS notch, never a false pass from running
			// past the document's end.
			secondNotchReveal := fmt.Sprintf("line-%03d", maxVisibleBeforeNotch+2)
			s.send(encoding.down)
			// postNotch is the SAME mutex-consistent snapshot that satisfied
			// the wait: readLoop (ptyharness_test.go) holds s.mu across both
			// the raw-log append and the screen write, and waitScreen returns
			// the grid it matched against rather than a fresh read, so this
			// checks the identical frame the wait already proved contains
			// `revealed` — never a later poll that could have scrolled
			// further still.
			postNotch := s.waitScreen(func(screen string) bool { return strings.Contains(screen, revealed) })
			if strings.Contains(postNotch, secondNotchReveal) {
				t.Errorf("one wheel notch revealed more than one line: both %s and %s present\nscreen:\n%s",
					revealed, secondNotchReveal, postNotch)
			}

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

// waitStableMaxVisibleLineNumber blocks until the reading view has painted
// line-001, then further until maxVisibleLineNumber reads the SAME value on
// two consecutive polls, returning the screen that confirmed the second read.
// The single-notch pin (TestPTYWheelBytesScrollReadingView) derives its whole
// predicate — which line a notch must reveal, and which line it must NOT —
// from this snapshot, so the snapshot has to be the fully-painted frame, not
// whichever chunk of it happened to have landed when line-001 first showed up.
// readLoop (ptyharness_test.go) locks s.mu only per read chunk, so a poll can
// land between two chunks of the SAME still-arriving frame and observe
// line-001 alongside only a partial run of the lines below it. Deriving
// `revealed` from that under-read screen would still make the pin pass — but
// `revealed` appearing would then be timing the paint finishing, not the
// notch that has not been sent yet: a silent degradation from measuring the
// notch to measuring the render, one that would never surface as a test
// FAILURE, only as a pin that quietly stopped proving what its comments
// claim. Two consecutive equal reads is a quiet-frame gate consistent with
// the no-bare-sleeps discipline: a screen still being painted keeps producing
// larger values and never repeats, while a settled one trivially does. Polls
// at the package's standard pollInterval cadence (ptyharness_test.go); this
// is a predicate wait, never a bare sleep.
func waitStableMaxVisibleLineNumber(t *testing.T, s *hubSession) string {
	t.Helper()
	screen := s.waitScreen(func(screen string) bool { return strings.Contains(screen, "line-001") })
	previousMax := maxVisibleLineNumber(screen)
	deadline := time.Now().Add(waitDeadline)
	for {
		time.Sleep(pollInterval)
		screen = s.snapshotScreen()
		currentMax := maxVisibleLineNumber(screen)
		if currentMax == previousMax {
			return screen
		}
		previousMax = currentMax
		if time.Now().After(deadline) {
			t.Fatalf("maxVisibleLineNumber never stabilized within %s (last read line-%03d)\nscreen:\n%s\nraw tail: %q",
				waitDeadline, currentMax, screen, tail(s.snapshotRaw()))
		}
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
// alternate_scroll = false, a full open→browse→read→edit→quit cycle must put
// ZERO 1007 bytes on the wire — Init emits neither XTSAVE nor DECSET, the
// editorFinishedMsg handler skips its re-assert, and RestoreAlternateScroll is
// a no-op, so there is no 1007l/1007r either. A refactor that armed 1007
// unconditionally would trip this; and this test's silence is what makes the
// other scenarios' "1007 present" assertions load-bearing rather than vacuous.
func TestPTYKillSwitchEmitsNoAlternateScrollBytes(t *testing.T) {
	t.Parallel()
	store := ensureHubStore(t)
	cfg := defaultSessionConfig()
	cfg.configDirOverride = store.killSwitchConfigDir(t)
	cfg.editorScript = writeNoopEditor(t)
	s := startHubSession(t, cfg)
	// Exercise the same surfaces the armed scenarios do — browser, reading
	// view, and one $EDITOR round-trip, whose editorFinishedMsg cycle must
	// keep the 1007 re-assert behind the disabled gate — so a stray 1007 from
	// any of them would surface here.
	openLongMemory(t, s)
	s.send("e")
	s.waitScreen(func(screen string) bool { return strings.Contains(screen, "no changes") })
	raw := s.quitAndDrain()
	if strings.Contains(raw, "[?1007") {
		t.Errorf("kill-switch session emitted 1007 bytes on the wire; want none\nraw tail: %q", tail(raw))
	}
}

// TestPTYFooterStaysAnchoredAcrossSelections proves the root fill-to-budget
// fix (dashboard.go's fitAndFillHeight) holds over a REAL rendered frame, not
// just the dashboard package's own Model.View() unit pins
// (TestRootPadsShortPushedBodyToFillTerminalHeight and
// TestRootFooterRowFixedAcrossShortAndTallPreview): the footer must render on
// the identical screen row whether the selected memory's preview is short
// (the index, previewing "See long-scroll-target.") or tall (the 200-line
// long-scroll-target), never float up and down the way the live-hub bug
// report described.
//
// "enter read" is the browser scope's FIRST key hint (actions.go's
// browser-read row, rendered by stackFooterLine in registry order) —
// distinctive against both seeded bodies' text, so a match can only be the
// footer's own row, never a coincidence in the previewed markdown. It has to
// be the first hint, not a later one: the full footer line is wider than
// this session's 120-column width and the terminal clips rather than wraps
// it, so a hint late in registry order (e.g. "esc back") never reaches the
// visible grid at all.
//
// The row is asserted against the LITERAL last row of the 40-row session (39,
// 0-indexed) rather than a row derived from the first captured frame:
// stackBodyHeight now reserves the header's actual current height, not a
// two-toast-blind maximum (dashboard.go's frameChromeLines), so with no toast
// ever pushed in this scenario the frame still fills the terminal exactly and
// the footer sits on the true bottom row — the same row a toast-occupied
// frame would also reach. Asserting the literal row, not just the
// short-vs-tall equality, is what makes a reservation that under-fills (the
// footer settling short of the bottom on every selection alike, equality
// preserved but wrong) fail here.
func TestPTYFooterStaysAnchoredAcrossSelections(t *testing.T) {
	t.Parallel()
	const lastRow = 39 // 0-indexed bottom row of the 40-row session (defaultSessionConfig)
	s := startHubSession(t, defaultSessionConfig())
	shortFrame := openBrowser(t, s) // index selected, its one-line body previewed
	shortRow := footerLineIndex(t, shortFrame, "enter read")

	s.send("j") // selection: index (short preview) -> long-scroll-target (tall preview)
	tallFrame := s.waitScreen(func(screen string) bool { return strings.Contains(screen, "line-001") })
	tallRow := footerLineIndex(t, tallFrame, "enter read")

	if shortRow != tallRow {
		t.Errorf("footer row changed with the preview height: row %d (short preview) vs row %d (tall preview), want identical\nshort frame:\n%s\ntall frame:\n%s",
			shortRow, tallRow, shortFrame, tallFrame)
	}
	if shortRow != lastRow {
		t.Errorf("footer row = %d, want %d — the terminal's true last row\nshort frame:\n%s", shortRow, lastRow, shortFrame)
	}
	s.quitAndDrain()
}

// footerLineIndex returns the 0-based row index of screen's one line
// containing fragment, failing if none does.
func footerLineIndex(t *testing.T, screen, fragment string) int {
	t.Helper()
	for i, line := range strings.Split(screen, "\n") {
		if strings.Contains(line, fragment) {
			return i
		}
	}
	t.Fatalf("no screen line contains %q\nscreen:\n%s", fragment, screen)
	return -1
}
