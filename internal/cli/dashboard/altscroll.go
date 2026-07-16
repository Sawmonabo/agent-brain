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
// seam — so no renderer change or dependency bump is involved. x/ansi has no
// save/restore-mode helpers either (checked against v0.11.7), so the
// XTSAVE/XTRESTORE sequences below are hand-built literals for the same
// reason: save captures the terminal's pre-hub 1007 state before we arm it,
// and restore replays that saved state at exit, so a user whose own terminal
// config already armed 1007 gets it back instead of losing it to our reset.
// ADR 21 holds the full decision trail.
var (
	setAlternateScroll   = ansi.SetMode(ansi.DECMode(1007))
	resetAlternateScroll = ansi.ResetMode(ansi.DECMode(1007))
)

const (
	saveAlternateScrollState    = "\x1b[?1007s"
	restoreAlternateScrollState = "\x1b[?1007r"
)

// RestoreAlternateScroll writes the mode teardown after the program has
// returned, whatever the exit path — the one choke point every quit shares.
// It must run before any re-exec: syscall.Exec replaces the process image,
// so nothing deferred survives it.
//
// The enabled path writes DECRST then XTRESTORE as one string, in that
// order: DECRST first returns terminals with no XTSAVE/XTRESTORE support to
// the plain-reset posture — they ignore the trailing XTRESTORE as an
// unimplemented sequence. XTRESTORE then overrides that reset with the
// saved pre-hub state on terminals that do support the round-trip, so a
// user's own alternate-scroll preference — armed by their own xterm
// resource or iTerm2 profile before the hub ever ran — survives the hub
// instead of being clobbered by an unconditional reset. If the program
// failed before Init ever emitted the paired XTSAVE, a round-trip terminal
// restores its startup value instead — the user's own default, still no
// worse than the bare reset.
//
// The disabled path stays a full no-op: when the kill-switch is off we never
// emitted the paired XTSAVE in Init, so there is nothing of ours to restore,
// and writing XTRESTORE anyway could stomp a runtime state the user's own
// tooling armed after we started.
func RestoreAlternateScroll(w io.Writer, enabled bool) {
	if !enabled {
		return
	}
	_, _ = io.WriteString(w, resetAlternateScroll+restoreAlternateScrollState)
}
