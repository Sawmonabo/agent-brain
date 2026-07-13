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

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/actions"
)

// DashboardKeymap is the dashboard's keymap: every key ProjectsView.Update
// and updateAdd MATCH against, resolved from the actions registry (spec
// §14's single source of truth) rather than declared by hand — so the
// keymap can never silently drift from what the footer, the ctrl+k palette,
// and the ? help overlay advertise for the same action ID. Cancel/Accept/
// ConfirmDecision are modal-input controls, not dispatchable actions (a
// modal owns the keyboard for itself, not through the registry), so those
// three stay hand-declared below, same as before this task.
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
	Untrack keybinding.Binding // Projects tab only — spec §13 rebind: u, not t
	Add     keybinding.Binding // Projects tab only
	// Open enters the memory browser for the selected unit row's folder
	// (Projects tab only). Unlike Sync/Untrack/Add it has no root-level
	// dispatch runner — see actions.go's "open-browser" row comment — it is
	// matched directly by ProjectsView.Update the same way Select is.
	Open keybinding.Binding
	Quit keybinding.Binding
	// Browser* bindings own the keyboard while a memory browser Screen is
	// on the root's navigation stack (Task 11+); Select above is reused
	// verbatim for its list cursor, the same cross-context reuse ForModal
	// already applies to the add picker. Edit/New/Rename/Delete (Task 13,
	// spec §5) only EMIT flow-request messages — every gate and modal is
	// the root's — and stay typable literals while the in-browser filter
	// owns input focus (updateFiltering forwards them to the text input
	// before these bindings are ever matched).
	BrowserRead        keybinding.Binding
	BrowserOrder       keybinding.Binding
	BrowserFilter      keybinding.Binding
	BrowserEdit        keybinding.Binding
	BrowserNew         keybinding.Binding
	BrowserRename      keybinding.Binding
	BrowserDelete      keybinding.Binding
	BrowserHistory     keybinding.Binding
	BrowserShowDeleted keybinding.Binding
	// BrowserInsights (i) opens the project insights screen (Task 16), matched
	// directly by Browser.updateKey like the other browser read surfaces.
	BrowserInsights keybinding.Binding
	BrowserBack     keybinding.Binding
	// Reading* bindings own the keyboard while a reading view Screen is on
	// the stack (Task 12+). ReadingCycleLinks bundles tab/shift+tab under
	// one hint; Reading.updateKey matches it for membership, then picks the
	// direction from the concrete key — the TabSwitch idiom. The viewport's
	// scroll keys (spec §4's j/k, ctrl+d/u, g/G) are not registry actions,
	// exactly as the browser's own up/down cursor keys are not: table-stakes
	// navigation the screen routes itself.
	ReadingCycleLinks keybinding.Binding
	ReadingFollow     keybinding.Binding
	ReadingBacklinks  keybinding.Binding
	ReadingCopyPath   keybinding.Binding
	ReadingEdit       keybinding.Binding
	ReadingHistory    keybinding.Binding
	ReadingBack       keybinding.Binding
	// History* bindings own the keyboard while a version-history Screen is on
	// the stack (Task 14). View/Diff/DiffOlder/Restore/Back are matched
	// directly by History.updateKey; the list cursor reuses Select verbatim
	// (the browser precedent) and the restore confirm reuses ConfirmDecision
	// (y/Y/n/N), so neither needs a binding of its own here.
	HistoryView      keybinding.Binding
	HistoryDiff      keybinding.Binding
	HistoryDiffOlder keybinding.Binding
	HistoryRestore   keybinding.Binding
	HistoryBack      keybinding.Binding
	// InsightsBack owns the pushed insights screen's esc (Task 16). The screen's
	// stat sections scroll through the viewport's own keys (not registry
	// actions, like every screen's table-stakes scroll set), so esc is its only
	// binding here, matched directly by Insights.updateKey.
	InsightsBack keybinding.Binding
	// ConflictsSelect/ConflictsOpen own the Conflicts tab's flat list cursor
	// and its drill-in to a detail screen; ConflictsSelect reuses the same
	// ↑/↓/k/j keys as Select, kept a distinct binding so its footer row scopes
	// to Conflicts rather than Projects. ConflictDetail* own the pushed detail
	// screen (Task 17): Read jumps to the reading view, Edit only EMITS
	// EditRequestMsg (the root owns the handoff and every gate), History pushes
	// the version-history screen when the path resolves to a unit, Back pops.
	ConflictsSelect       keybinding.Binding
	ConflictsOpen         keybinding.Binding
	ConflictDetailRead    keybinding.Binding
	ConflictDetailEdit    keybinding.Binding
	ConflictDetailHistory keybinding.Binding
	ConflictDetailBack    keybinding.Binding
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
// bindings never change at runtime; per-tab, per-quiesce availability is the
// root's footer/palette job, not this package's.
var DashboardKeys = DashboardKeymap{
	TabSwitch:             bindingFor("switch-tabs"),
	Select:                bindingFor("select"),
	Sync:                  bindingFor("sync-project"),
	Untrack:               bindingFor("untrack"),
	Add:                   bindingFor("add-project"),
	Open:                  bindingFor("open-browser"),
	Quit:                  bindingFor("quit"),
	BrowserRead:           bindingFor("browser-read"),
	BrowserOrder:          bindingFor("browser-order"),
	BrowserFilter:         bindingFor("browser-filter"),
	BrowserEdit:           bindingFor("browser-edit"),
	BrowserNew:            bindingFor("browser-new"),
	BrowserRename:         bindingFor("browser-rename"),
	BrowserDelete:         bindingFor("browser-delete"),
	BrowserHistory:        bindingFor("browser-history"),
	BrowserShowDeleted:    bindingFor("browser-show-deleted"),
	BrowserInsights:       bindingFor("browser-insights"),
	BrowserBack:           bindingFor("browser-back"),
	ReadingCycleLinks:     bindingFor("reading-links"),
	ReadingFollow:         bindingFor("reading-follow"),
	ReadingBacklinks:      bindingFor("reading-backlinks"),
	ReadingCopyPath:       bindingFor("reading-copy-path"),
	ReadingEdit:           bindingFor("reading-edit"),
	ReadingHistory:        bindingFor("reading-history"),
	ReadingBack:           bindingFor("reading-back"),
	HistoryView:           bindingFor("history-view"),
	HistoryDiff:           bindingFor("history-diff"),
	HistoryDiffOlder:      bindingFor("history-diff-older"),
	HistoryRestore:        bindingFor("history-restore"),
	HistoryBack:           bindingFor("history-back"),
	InsightsBack:          bindingFor("insights-back"),
	ConflictsSelect:       bindingFor("conflicts-select"),
	ConflictsOpen:         bindingFor("conflicts-open"),
	ConflictDetailRead:    bindingFor("conflictdetail-read"),
	ConflictDetailEdit:    bindingFor("conflictdetail-edit"),
	ConflictDetailHistory: bindingFor("conflictdetail-history"),
	ConflictDetailBack:    bindingFor("conflictdetail-back"),
	Cancel:                keybinding.NewBinding(keybinding.WithKeys("esc"), keybinding.WithHelp("esc", "cancel")),
	Accept:                keybinding.NewBinding(keybinding.WithKeys("enter"), keybinding.WithHelp("enter", "confirm")),
	ConfirmDecision: keybinding.NewBinding(
		keybinding.WithKeys("y", "Y", "n", "N"),
		keybinding.WithHelp("y/n", "decide"),
	),
}

// bindingFor resolves a registry action's key.Binding by ID. A miss panics
// at package init — the only way to hit it is a typo in the id literal
// above, a programming error that should never reach a running binary,
// rather than silently building a dead, keyless binding.
func bindingFor(id string) keybinding.Binding {
	for _, a := range actions.Registry() {
		if a.ID == id {
			return actions.Binding(a)
		}
	}
	panic("views: no registered action " + id)
}

// ForModal returns the bindings the footer advertises while a Projects modal
// owns the keyboard, in render order — the SAME subset the confirm handler
// and updateAdd honor at each stage, so the modal footer can never name a key
// its state ignores. Unlike the bare-tab footer (which the root now renders
// straight from the action registry), a modal is an input-owned state
// machine, not a set of dispatchable actions, so it keeps its own hand-
// declared bindings here. Text-input runes (a typed path or folder name) are
// the input's, not advertised keys, so the input stages surface only their
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
// confirm · esc cancel"). The root's registry-driven footer, its modal
// footer, and the add flow's inline hints all render through it, so no
// surface can drift from any other in how a binding's help text reads.
// Styling stays with the caller.
func HelpLine(bindings []keybinding.Binding) string {
	parts := make([]string, len(bindings))
	for i, binding := range bindings {
		help := binding.Help()
		parts[i] = help.Key + " " + help.Desc
	}
	return strings.Join(parts, " · ")
}
