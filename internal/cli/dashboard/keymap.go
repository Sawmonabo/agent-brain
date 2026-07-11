package dashboard

import (
	"strings"

	// Imported as keybinding, not the package's own default name "key": this
	// package's test suite already has a package-level key(name string) helper
	// (dashboard_test.go) that builds a tea.KeyPressMsg, and Go forbids an
	// import's file-block name from colliding with a top-level identifier
	// declared anywhere else in the package. Aliasing here — rather than
	// renaming the widely-used test helper — keeps the collision fix local to
	// the files that reference the key package.
	keybinding "charm.land/bubbles/v2/key"
)

// dashboardKeymap is the dashboard's single keymap: every key the root reducer and
// the views dispatch, with the help text the footer advertises. handleKey,
// projectsView.update, and updateAdd MATCH against these bindings and footer()
// RENDERS them through forTab (tab-level) or forModal (while a Projects modal
// owns the keyboard) — so an advertised key the active surface ignores is
// structurally impossible, and the inverse (a working key left unadvertised) is
// pinned by TestProjectsKeysStayDeadOffProjectsTab.
type dashboardKeymap struct {
	// TabSwitch bundles every tab-navigation key under one advertised hint.
	// handleKey matches the binding for membership, then picks the direction
	// from the concrete key — the binding stays the single gate for whether
	// the key does anything at all.
	TabSwitch keybinding.Binding
	// Select advertises the bubbles table's own ↑/↓/k/j navigation on the
	// Projects tab; the table consumes the keys itself in update's
	// passthrough. The add picker reuses it (same keys) to move its cursor.
	Select  keybinding.Binding
	Sync    keybinding.Binding // Projects tab only
	Untrack keybinding.Binding // Projects tab only
	Add     keybinding.Binding // Projects tab only
	Quit    keybinding.Binding
	// Modal bindings own the keyboard while a Projects modal (the untrack
	// confirm or the add flow) is open; forModal advertises exactly the subset
	// each modal state honors, and the tab-level set above never renders there.
	Cancel keybinding.Binding // every modal state: esc backs out
	Accept keybinding.Binding // add picker/input stages: enter advances
	// ConfirmDecision bundles the untrack confirm's y/Y/n/N under one hint; the
	// confirm handler matches it for membership, then branches on the concrete
	// key (the TabSwitch idiom) — the binding is the single gate for whether a
	// keystroke decides the confirm at all.
	ConfirmDecision keybinding.Binding
}

// dashboardKeys is package-level because the keymap is static configuration —
// bindings never change at runtime; per-tab availability is forTab's job.
var dashboardKeys = dashboardKeymap{
	TabSwitch: keybinding.NewBinding(
		keybinding.WithKeys("tab", "shift+tab", "right", "left", "l", "h", "1", "2", "3", "4"),
		keybinding.WithHelp("tab/1–4", "switch"),
	),
	Select:  keybinding.NewBinding(keybinding.WithKeys("up", "down", "k", "j"), keybinding.WithHelp("↑/↓", "select")),
	Sync:    keybinding.NewBinding(keybinding.WithKeys("s"), keybinding.WithHelp("s", "sync")),
	Untrack: keybinding.NewBinding(keybinding.WithKeys("t"), keybinding.WithHelp("t", "untrack")),
	Add:     keybinding.NewBinding(keybinding.WithKeys("a"), keybinding.WithHelp("a", "add")),
	Quit:    keybinding.NewBinding(keybinding.WithKeys("q"), keybinding.WithHelp("q", "quit")),
	Cancel:  keybinding.NewBinding(keybinding.WithKeys("esc"), keybinding.WithHelp("esc", "cancel")),
	Accept:  keybinding.NewBinding(keybinding.WithKeys("enter"), keybinding.WithHelp("enter", "confirm")),
	ConfirmDecision: keybinding.NewBinding(
		keybinding.WithKeys("y", "Y", "n", "N"),
		keybinding.WithHelp("y/n", "decide"),
	),
}

// forTab returns the bindings the footer advertises on t, in render order —
// the SAME availability rule handleKey enforces. addAvailable gates the Add
// binding: a build missing either add closure (discovery or identity) must not
// advertise a key that answers "unavailable". (dashboardKeys is shared package
// state — never toggle availability by mutating a binding's Enabled flag;
// filter here instead.)
func (k dashboardKeymap) forTab(t tab, addAvailable bool) []keybinding.Binding {
	bindings := []keybinding.Binding{k.TabSwitch}
	if t == tabProjects {
		bindings = append(bindings, k.Select, k.Sync, k.Untrack)
		if addAvailable {
			bindings = append(bindings, k.Add)
		}
	}
	return append(bindings, k.Quit)
}

// forModal returns the bindings the footer advertises while a Projects modal
// owns the keyboard, in render order — the SAME subset the confirm handler and
// updateAdd honor at each stage, so the modal footer can never name a key its
// state ignores. It is the modal-state analogue of forTab: footer() calls one
// or the other, never both, so the tab-level set never leaks into a modal.
// Text-input runes (a typed path or folder name) are the input's, not advertised
// keys, so the input stages surface only their control keys (Accept, Cancel).
func (k dashboardKeymap) forModal(confirming bool, stage addStage) []keybinding.Binding {
	if confirming {
		return []keybinding.Binding{k.ConfirmDecision, k.Cancel}
	}
	switch stage {
	case addPicking:
		return []keybinding.Binding{k.Select, k.Accept, k.Cancel}
	case addConfirmPath, addNamingFolder:
		return []keybinding.Binding{k.Accept, k.Cancel}
	default: // addDiscovering, addIdentifying, addTracking: waiting on a Cmd
		return []keybinding.Binding{k.Cancel}
	}
}

// helpLine renders bindings as the footer/help string ("↑/↓ select · enter
// confirm · esc cancel"). Both the global footer and the add flow's inline
// hints render through it from the same forModal bindings, so the two
// surfaces cannot drift apart. Styling stays with the caller.
func helpLine(bindings []keybinding.Binding) string {
	parts := make([]string, len(bindings))
	for i, binding := range bindings {
		help := binding.Help()
		parts[i] = help.Key + " " + help.Desc
	}
	return strings.Join(parts, " · ")
}
