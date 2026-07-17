package views

import (
	"fmt"

	keybinding "charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/theme"
)

// sectionChromeLines is the section title plus its trailing blank line that a
// tab body keeps FIXED above its scroll pane — the "Doctor"/"Activity" header
// never scrolls out from under the reader, matching the reading view's
// header/viewport split (reading.go). A tab view subtracts it from the height
// budget the root hands it before sizing the pane.
const sectionChromeLines = 2

// scrollPane wraps a bubbles viewport in the dashboard's tab-body scrolling
// convention (spec §7), shared by the Doctor and Activity tabs so the two
// height-bounded bodies behave identically and reuse one implementation:
//
//   - content is (re)installed every render through refresh, which resets the
//     scroll to the top only when the document's IDENTITY changes — a periodic
//     poll that leaves the identity unchanged never yanks the reader's position
//     (the browser preview's GotoTop-on-selection-change rule);
//   - at render the pane is bounded to a height budget (fit), spending its
//     bottom line on the shared overflow hint (scrollHintLine) only when the
//     content overflows and there is room for both a content line and the hint;
//   - the standard scroll keys drive it (scroll): ctrl+d/u half page and
//     pgup/pgdown page through the viewport's own restricted keymap, g/G to the
//     ends handled directly — the viewport exposes GotoTop/GotoBottom but binds
//     no keys to them, exactly as the reading view and browser preview do.
//
// It is cursorless — there is no selection, only a scroll offset — so unlike
// the browser list none of its keys move a cursor.
type scrollPane struct {
	viewport viewport.Model
	identity string
	ready    bool
}

// newScrollPane builds a pane with the restricted scroll keymap installed. A
// zero viewport RENDERS correctly (SetContent/SetHeight/View need no
// initialization), but its Update matches nothing without a keymap, so a pane
// that will ever SCROLL — every pane the root owns — must be built here rather
// than zero-valued.
func newScrollPane() scrollPane {
	pane := viewport.New()
	pane.KeyMap = scrollPaneKeyMap()
	return scrollPane{viewport: pane}
}

// scrollPaneKeyMap binds only the half-page and page keys, matching the browser
// preview's unfocused keymap (browserPreviewKeyMap) and the overflow hint's own
// advertised keys. Up/Down/Left/Right stay unbound — a tab body is cursorless,
// and line-by-line j/k are deliberately omitted so the bound set matches exactly
// what the hint names (── … ctrl+d/u pgup/pgdn scroll ──); g/G are handled by
// scroll directly, not bound here.
func scrollPaneKeyMap() viewport.KeyMap {
	return viewport.KeyMap{
		HalfPageUp:   keybinding.NewBinding(keybinding.WithKeys("ctrl+u")),
		HalfPageDown: keybinding.NewBinding(keybinding.WithKeys("ctrl+d")),
		PageUp:       keybinding.NewBinding(keybinding.WithKeys("pgup")),
		PageDown:     keybinding.NewBinding(keybinding.WithKeys("pgdown")),
		Up:           keybinding.NewBinding(),
		Down:         keybinding.NewBinding(),
		Left:         keybinding.NewBinding(),
		Right:        keybinding.NewBinding(),
	}
}

// refresh installs body as the pane's content and, when identity marks a
// materially changed document, scrolls back to the top. body is re-set every
// call so a live part (Activity's ticking uptime) stays current; identity is
// the change key — the body itself for a now-free document (Doctor), or a
// now-invariant projection for one whose body carries live durations
// (Activity) — so a once-a-second re-render is not mistaken for a new document
// and the reader's scroll survives it, while a genuine change starts at the top.
// SetContent preserves the scroll offset across an unchanged identity (it clamps
// only if the content shrank past the current offset).
func (p *scrollPane) refresh(body, identity string) {
	p.viewport.SetContent(body)
	if !p.ready || p.identity != identity {
		p.viewport.GotoTop()
		p.identity = identity
		p.ready = true
	}
}

// fit sizes the viewport to the height budget and reports whether the content
// overflows AND leaves room for the hint line. When the content fits, the pane
// is sized to its own line count so a short body never strands blank rows above
// the footer (the root's fitAndFillHeight pads the frame). When it overflows,
// the bottom line is reserved for the hint unless the budget is a single line,
// which keeps that lone line for content. Called before both render and a
// scroll, so the half/full-page math and the AtTop/AtBottom guards see the same
// height the frame draws.
func (p *scrollPane) fit(width, height int) (showHint bool) {
	p.viewport.SetWidth(width)
	budget := max(height, 1)
	total := p.viewport.TotalLineCount()
	switch {
	case total <= budget:
		p.viewport.SetHeight(total)
		return false
	case budget < 2:
		p.viewport.SetHeight(budget)
		return false
	default:
		p.viewport.SetHeight(budget - 1)
		return true
	}
}

// render draws the pane bounded to the budget, appending the overflow hint on
// its own bottom line when the content runs past the fold.
func (p *scrollPane) render(styles theme.Styles, width, height int) string {
	showHint := p.fit(width, height)
	view := p.viewport.View()
	if !showHint {
		return view
	}
	percent := int(p.viewport.ScrollPercent() * 100)
	return view + "\n" + scrollHintLine(styles, percent, width)
}

// scroll applies a scroll key after sizing the pane to the current budget so the
// page math sees the real height. It reports whether msg was a scroll key it
// consumed, so a caller can fall through to its own keys on a miss. g/G jump to
// the ends (the viewport binds no keys to GotoTop/GotoBottom); every other
// scroll key runs through the restricted keymap.
func (p *scrollPane) scroll(msg tea.KeyPressMsg, width, height int) bool {
	if !isScrollKey(msg.String()) {
		return false
	}
	p.fit(width, height)
	switch msg.String() {
	case "g":
		p.viewport.GotoTop()
	case "G":
		p.viewport.GotoBottom()
	default:
		p.viewport, _ = p.viewport.Update(msg)
	}
	return true
}

// isScrollKey reports whether key is one a bounded tab pane consumes: the
// half/full-page keys the restricted keymap binds, plus g/G handled directly.
// The half/full-page keys match the doctor-scroll/activity-scroll registry rows;
// g/G are the conventional ends, off the registry as table-stakes viewport keys.
func isScrollKey(key string) bool {
	switch key {
	case "ctrl+d", "ctrl+u", "pgup", "pgdown", "g", "G":
		return true
	default:
		return false
	}
}

// scrollHintLine is the one-line overflow affordance shared by every
// height-bounded pane — the browser preview (browser.go's previewScrollHint)
// and the Doctor/Activity tabs — so the percent-through readout and the keys
// that move the window are written in exactly one place. Dim-styled and fit to
// width (plain text measured first, styled after — a styled string is never
// width-sliced, fitWidth's rule) so the affordance can never itself overflow the
// pane it labels.
func scrollHintLine(styles theme.Styles, percent, width int) string {
	return styles.Dim.Render(fitWidth(fmt.Sprintf("── %d%% · ctrl+d/u pgup/pgdn scroll ──", percent), width))
}
