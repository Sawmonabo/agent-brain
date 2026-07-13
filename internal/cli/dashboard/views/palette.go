package views

import (
	"strings"

	keybinding "charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/actions"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/theme"
)

// PaletteChoiceMsg reports the action ID chosen from the ctrl+k palette. The
// root's dispatch(id) is the only consumer, and it is the SAME dispatch a
// direct keypress runs through — the mechanism that makes "key and palette
// behavior cannot diverge" (spec §14) true by construction rather than by
// discipline.
type PaletteChoiceMsg struct{ ID string }

// PaletteModel is the ctrl+k command palette: a fuzzy-filtered, keyboard-
// driven list over actions.Registry(). It holds no daemon knowledge of its
// own — availability (which rows a not-yet-wired build can actually run) and
// the current quiesce posture are both injected at construction, the same
// TrackActions-style seam the add flow uses, because runners and daemon
// status are root-private (views must not import the root package).
type PaletteModel struct {
	styles theme.Styles
	input  textinput.Model

	available   func(id string) bool
	quiescedNow bool // Mutates rows render greyed while true (spec §15); snapshotted at open, not live

	filtered []actions.Action
	cursor   int

	// Closed latches true once esc or enter has resolved the palette's
	// lifecycle; the root reads it right after Update to decide whether to
	// un-mount the overlay, without needing a message round-trip just to
	// learn "the user is done with this".
	Closed bool
}

// NewPaletteModel opens a palette scoped by available (hides a row with no
// registered runner — spec plan Task 5's "invisible everywhere" rule) and
// quiescedNow (greys, but does not hide, a Mutates row — dispatch still
// refuses it with a toast if chosen, spec §15). It returns the focus Cmd the
// embedded text input needs to start its cursor blinking, so a caller never
// has to know that detail to avoid silently dropping it.
func NewPaletteModel(styles theme.Styles, available func(id string) bool, quiescedNow bool) (PaletteModel, tea.Cmd) {
	input := textinput.New()
	input.Placeholder = "type to search actions…"
	cmd := input.Focus()

	p := PaletteModel{styles: styles, input: input, available: available, quiescedNow: quiescedNow}
	p.refilter()
	return p, cmd
}

// SetStyles installs a new theme, the same propagation every other view
// receives from the root's withStyles on a tea.BackgroundColorMsg — included
// here even though the palette is normally closed by the time that message
// could arrive, so a background-color change while it happens to be open is
// never missed rather than silently stuck on the theme it opened with.
func (p *PaletteModel) SetStyles(styles theme.Styles) {
	p.styles = styles
}

// refilter recomputes the visible list from the input's current value:
// actions.Fuzzy ranks the whole registry, then availability drops any row
// with no registered runner. The cursor is clamped into range rather than
// reset to 0 on every keystroke, so continuing to type does not keep
// yanking a deliberately-moved selection back to the top — except that in
// practice the filtered set usually shrinks as a query narrows, and a
// clamp-not-reset policy still lands on the last row rather than losing the
// selection outright when it does.
func (p *PaletteModel) refilter() {
	filtered := make([]actions.Action, 0, len(actions.Registry()))
	for _, a := range actions.Fuzzy(p.input.Value()) {
		if p.available == nil || p.available(a.ID) {
			filtered = append(filtered, a)
		}
	}
	p.filtered = filtered
	p.cursor = min(p.cursor, max(len(p.filtered)-1, 0))
}

// Update handles one keypress while the palette owns the keyboard: esc
// closes with no choice, enter surfaces the highlighted row's ID as a
// PaletteChoiceMsg-producing Cmd, up/down move the cursor, and everything
// else is forwarded to the text input as a query keystroke.
func (p PaletteModel) Update(msg tea.KeyPressMsg) (PaletteModel, tea.Cmd) {
	switch {
	case keybinding.Matches(msg, DashboardKeys.Cancel):
		p.Closed = true
		return p, nil
	case keybinding.Matches(msg, DashboardKeys.Accept):
		if len(p.filtered) == 0 {
			return p, nil
		}
		p.Closed = true
		id := p.filtered[p.cursor].ID
		return p, func() tea.Msg { return PaletteChoiceMsg{ID: id} }
	}

	// Arrow keys only — never k/j. Unlike the add picker's pure list-nav
	// modal, the palette also owns a free-text query, so j and k must stay
	// typable characters rather than DashboardKeys.Select's bundled
	// up/down/k/j (that binding would swallow a query containing either
	// letter).
	switch msg.String() {
	case "up":
		if p.cursor > 0 {
			p.cursor--
		}
		return p, nil
	case "down":
		if p.cursor < len(p.filtered)-1 {
			p.cursor++
		}
		return p, nil
	}

	var cmd tea.Cmd
	p.input, cmd = p.input.Update(msg)
	p.refilter()
	return p, cmd
}

// View renders the query input over the filtered, cursor-highlighted action
// list. A Mutates row is annotated while quiescedNow — visible and still
// choosable (dispatch is what actually refuses it, with a toast), never
// hidden, so the user learns why nothing happened instead of wondering
// whether the palette saw the keystroke at all.
func (p PaletteModel) View() string {
	var b strings.Builder
	b.WriteString(p.styles.Title.Render("Command palette"))
	b.WriteString("\n\n")
	b.WriteString(p.input.View())
	b.WriteString("\n\n")

	if len(p.filtered) == 0 {
		b.WriteString(p.styles.Dim.Render("no matching actions"))
	}
	for i, action := range p.filtered {
		marker := "  "
		if i == p.cursor {
			marker = "> "
		}
		line := marker + action.Title
		if action.Mutates && p.quiescedNow {
			line += "  (quiesced)"
		}
		if i == p.cursor {
			line = p.styles.Selected.Render(line)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(p.styles.Dim.Render("↑/↓ move · enter choose · esc close"))
	return strings.TrimRight(b.String(), "\n")
}
