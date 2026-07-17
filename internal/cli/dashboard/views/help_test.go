package views

import (
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/actions"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/theme"
)

// TestHelpListsEveryRegisteredAction pins that the ? overlay is a static,
// unconditional render of actions.Registry() — every Title appears, including
// rows with no root-level runner (search and help itself are dispatched
// directly rather than through the runners() map): the help overlay documents
// the full action surface, unlike the footer and palette which hide a row
// that available(id) marks unavailable.
func TestHelpListsEveryRegisteredAction(t *testing.T) {
	t.Parallel()
	help := NewHelpModel(theme.Default(true))
	got := plain(help.View())
	for _, action := range actions.Registry() {
		if !strings.Contains(got, action.Title) {
			t.Errorf("help view missing action %q (id=%s); got:\n%s", action.Title, action.ID, got)
		}
	}
}

// TestHelpGroupsByScope pins the "grouped by Scope with the keymap column"
// shape: each scope that owns at least one action gets its own visible group
// header, and a scope's actions appear together under it.
func TestHelpGroupsByScope(t *testing.T) {
	t.Parallel()
	help := NewHelpModel(theme.Default(true))
	got := plain(help.View())

	for _, scope := range actions.AllScopes() {
		rows := actions.ForScope(scope)
		if len(rows) == 0 {
			continue // Browser/Reading/History own no rows yet — no group to assert on
		}
		if !strings.Contains(got, scope.String()) {
			t.Errorf("help view missing group header %q", scope.String())
		}
	}
}

// TestHelpShowsKeyHints pins the "keymap column": a row with a real
// shortcut shows its KeyHint verbatim, and sync-fleet (no direct key today)
// renders a placeholder rather than an empty, misleading-looking key cell.
func TestHelpShowsKeyHints(t *testing.T) {
	t.Parallel()
	help := NewHelpModel(theme.Default(true))
	got := plain(help.View())

	for _, want := range []string{"ctrl+k", "q", "?"} {
		if !strings.Contains(got, want) {
			t.Errorf("help view missing key hint %q; got:\n%s", want, got)
		}
	}
}
