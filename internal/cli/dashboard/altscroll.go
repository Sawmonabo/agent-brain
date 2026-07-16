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
