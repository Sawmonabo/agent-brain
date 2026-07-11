package dashboard

// Imported as keybinding, not the package's own default name "key": this
// package's test suite already has a package-level key(name string) helper
// (dashboard_test.go) that builds a tea.KeyPressMsg, and Go forbids an
// import's file-block name from colliding with a top-level identifier
// declared anywhere else in the package. Aliasing here — rather than
// renaming the widely-used test helper — keeps the collision fix local to
// the three files this task touches.
import keybinding "charm.land/bubbles/v2/key"

// dashboardKeymap is the dashboard's single keymap: every key the root reducer and
// the views dispatch, with the help text the footer advertises. handleKey and
// projectsView.update MATCH against these bindings and footer() RENDERS them
// through forTab — so an advertised key the active tab ignores is structurally
// impossible, and the inverse (a working key left unadvertised) is pinned by
// TestProjectsKeysStayDeadOffProjectsTab.
type dashboardKeymap struct {
	// TabSwitch bundles every tab-navigation key under one advertised hint.
	// handleKey matches the binding for membership, then picks the direction
	// from the concrete key — the binding stays the single gate for whether
	// the key does anything at all.
	TabSwitch keybinding.Binding
	// Select advertises the bubbles table's own ↑/↓/k/j navigation on the
	// Projects tab; the table consumes the keys itself in update's
	// passthrough.
	Select  keybinding.Binding
	Sync    keybinding.Binding // Projects tab only
	Untrack keybinding.Binding // Projects tab only
	Quit    keybinding.Binding
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
	Quit:    keybinding.NewBinding(keybinding.WithKeys("q"), keybinding.WithHelp("q", "quit")),
}

// forTab returns the bindings the footer advertises on t, in render order —
// mirroring the availability rule handleKey enforces by routing view keys
// to the Projects view only while it is active.
func (k dashboardKeymap) forTab(t tab) []keybinding.Binding {
	bindings := []keybinding.Binding{k.TabSwitch}
	if t == tabProjects {
		bindings = append(bindings, k.Select, k.Sync, k.Untrack)
	}
	return append(bindings, k.Quit)
}
