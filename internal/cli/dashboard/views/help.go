package views

import (
	"fmt"
	"strings"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/actions"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/theme"
)

// HelpModel is the `?` help overlay: a static, unconditional render of every
// actions.Registry() row grouped by Scope, including a row with no runner
// registered yet — it documents the eventual action surface (spec §14),
// unlike the footer and palette which hide what does not work yet. It holds
// no interactive state of its own: any key press closes it (root's
// handleKey), so it has no Update method.
type HelpModel struct {
	styles theme.Styles
}

// NewHelpModel builds a ready-to-render help overlay.
func NewHelpModel(styles theme.Styles) HelpModel {
	return HelpModel{styles: styles}
}

// View renders every actions.Registry() row grouped by Scope, in registry
// order within each group and AllScopes order across groups, so the overlay
// always matches the running binary's actual (and planned) action set
// rather than a hand-maintained list that can drift from the registry.
func (h HelpModel) View() string {
	var b strings.Builder
	b.WriteString(h.styles.Title.Render("Keymap"))
	b.WriteString("\n\n")

	for _, scope := range actions.AllScopes() {
		rows := actions.ForScope(scope)
		if len(rows) == 0 {
			continue // no rows land in this scope yet (Browser/Reading/History, Task 11+)
		}
		b.WriteString(h.styles.Header.Render(scope.String()))
		b.WriteString("\n")
		for _, row := range rows {
			keyHint := row.KeyHint
			if keyHint == "" {
				keyHint = "—" // palette/help-only today (sync-fleet): no direct shortcut to show
			}
			fmt.Fprintf(&b, "  %-10s %s\n", keyHint, row.Title)
		}
		b.WriteString("\n")
	}

	b.WriteString(h.styles.Dim.Render("press any key to close"))
	return strings.TrimRight(b.String(), "\n")
}
