package views

import (
	"strings"

	// Imported as keybinding, not the package's own default name "key": this
	// package's test suite has a package-level key(name string) helper
	// (projects_test.go) that builds a tea.KeyPressMsg, and Go forbids an
	// import's file-block name from colliding with a top-level identifier
	// declared anywhere else in the package. Aliasing here — rather than
	// renaming the widely-used test helper — keeps the collision fix local to
	// the files that reference the key package.
	keybinding "charm.land/bubbles/v2/key"
)

// DashboardKeymap is the dashboard's single keymap: every key the root
// reducer and the views dispatch, with the help text the footer advertises.
// ProjectsView.Update and updateAdd MATCH against these bindings, and the
// root's footer() RENDERS them through ForTab (tab-level) or ForModal (while
// a Projects modal owns the keyboard) — so an advertised key the active
// surface ignores is structurally impossible, and the inverse (a working key
// left unadvertised) is pinned by TestProjectsKeysStayDeadOffProjectsTab.
type DashboardKeymap struct {
	// TabSwitch bundles every tab-navigation key under one advertised hint.
	// The root's handleKey matches the binding for membership, then picks the
	// direction from the concrete key — the binding stays the single gate for
	// whether the key does anything at all.
	TabSwitch keybinding.Binding
	// Select advertises the bubbles table's own ↑/↓/k/j navigation on the
	// Projects tab; the table consumes the keys itself in Update's
	// passthrough. The add picker reuses it (same keys) to move its cursor.
	Select  keybinding.Binding
	Sync    keybinding.Binding // Projects tab only
	Untrack keybinding.Binding // Projects tab only
	Add     keybinding.Binding // Projects tab only
	Quit    keybinding.Binding
	// Modal bindings own the keyboard while a Projects modal (the untrack
	// confirm or the add flow) is open; ForModal advertises exactly the
	// subset each modal state honors, and the tab-level set above never
	// renders there.
	Cancel keybinding.Binding // every modal state: esc backs out
	Accept keybinding.Binding // add picker/input stages: enter advances
	// ConfirmDecision bundles the untrack confirm's y/Y/n/N under one hint;
	// the confirm handler matches it for membership, then branches on the
	// concrete key (the TabSwitch idiom) — the binding is the single gate for
	// whether a keystroke decides the confirm at all.
	ConfirmDecision keybinding.Binding
}

// DashboardKeys is package-level because the keymap is static configuration —
// bindings never change at runtime; per-tab availability is ForTab's job.
var DashboardKeys = DashboardKeymap{
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

// ForTab returns the bindings the root's footer advertises on the active
// tab, in render order — the SAME availability rule the root's handleKey
// enforces. isProjectsTab takes a bool rather than the root's tab enum
// because that type is root-private (views must not import the dashboard
// root package, spec §15); the root passes `m.active == tabProjects`.
// addAvailable gates the Add binding: a build missing either add closure
// (discovery or identity) must not advertise a key that answers
// "unavailable". (DashboardKeys is shared package state — never toggle
// availability by mutating a binding's Enabled flag; filter here instead.)
func (k DashboardKeymap) ForTab(isProjectsTab, addAvailable bool) []keybinding.Binding {
	bindings := []keybinding.Binding{k.TabSwitch}
	if isProjectsTab {
		bindings = append(bindings, k.Select, k.Sync, k.Untrack)
		if addAvailable {
			bindings = append(bindings, k.Add)
		}
	}
	return append(bindings, k.Quit)
}

// ForModal returns the bindings the footer advertises while a Projects modal
// owns the keyboard, in render order — the SAME subset the confirm handler
// and updateAdd honor at each stage, so the modal footer can never name a key
// its state ignores. It is the modal-state analogue of ForTab: the root's
// footer() calls one or the other, never both, so the tab-level set never
// leaks into a modal. Text-input runes (a typed path or folder name) are the
// input's, not advertised keys, so the input stages surface only their
// control keys (Accept, Cancel).
func (k DashboardKeymap) ForModal(confirming bool, stage AddStage) []keybinding.Binding {
	if confirming {
		return []keybinding.Binding{k.ConfirmDecision, k.Cancel}
	}
	switch stage {
	case AddPicking:
		return []keybinding.Binding{k.Select, k.Accept, k.Cancel}
	case AddConfirmPath, AddNamingFolder:
		return []keybinding.Binding{k.Accept, k.Cancel}
	default: // AddDiscovering, AddIdentifying, AddTracking: waiting on a Cmd
		return []keybinding.Binding{k.Cancel}
	}
}

// HelpLine renders bindings as the footer/help string ("↑/↓ select · enter
// confirm · esc cancel"). Both the root's global footer and the add flow's
// inline hints render through it from the same ForModal bindings, so the two
// surfaces cannot drift apart. Styling stays with the caller.
func HelpLine(bindings []keybinding.Binding) string {
	parts := make([]string, len(bindings))
	for i, binding := range bindings {
		help := binding.Help()
		parts[i] = help.Key + " " + help.Desc
	}
	return strings.Join(parts, " · ")
}
