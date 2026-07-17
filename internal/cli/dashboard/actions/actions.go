// Package actions is the dashboard's single source of truth for every
// user-invocable operation (spec §14): one Action row backs the ctrl+k
// command palette, the active view's footer, and the ? help overlay, so a
// key can never mean one thing in the footer and another in the palette —
// they render from the identical Registry(), never their own copies.
//
// This package knows nothing about the daemon, runners, or quiesce state —
// that is root-private (internal/cli/dashboard), which is why Action has no
// "is this wired yet" field of its own. A row with no registered runner is
// still declared here (documentation of the eventual surface, which is why
// the help overlay lists every row unconditionally); the root's own
// available(id) gate is what makes an unbuilt feature's row inert in the
// footer and the palette until its task lands.
package actions

import (
	"sort"
	"strings"

	keybinding "charm.land/bubbles/v2/key"
)

// Scope names where an Action applies. The root's footer renders a view's
// own scope plus ScopeGlobal; the palette ignores Scope entirely (a command
// palette reaches every action regardless of what is on screen, spec §14).
type Scope int

// Scopes in declaration order — AllScopes and the help overlay's group
// ordering both depend on this exact sequence.
const (
	ScopeGlobal                Scope = iota // any root view
	ScopeProjects                           // Projects tab
	ScopeDoctor                             // Doctor tab
	ScopeActivity                           // Activity tab
	ScopeBrowser                            // memory browser (Task 11+)
	ScopeBrowserPreviewFocused              // memory browser while the preview pane holds keyboard focus
	ScopeReading                            // reading view (Task 12+)
	ScopeHistory                            // history view (Task 14+)
	ScopeInsights                           // project insights screen, pushed from the browser (Task 16+)
	ScopeConflicts                          // conflicts tab
	ScopeConflictDetail                     // conflict detail screen (pushed from the tab)
)

// String names a Scope for the help overlay's group headers.
func (s Scope) String() string {
	switch s {
	case ScopeGlobal:
		return "Global"
	case ScopeProjects:
		return "Projects"
	case ScopeDoctor:
		return "Doctor"
	case ScopeActivity:
		return "Activity"
	case ScopeBrowser:
		return "Memory browser"
	case ScopeBrowserPreviewFocused:
		return "Memory browser (preview focused)"
	case ScopeReading:
		return "Reading"
	case ScopeHistory:
		return "History"
	case ScopeInsights:
		return "Insights"
	case ScopeConflicts:
		return "Conflicts"
	case ScopeConflictDetail:
		return "Conflict detail"
	default:
		return "Unknown"
	}
}

// AllScopes lists every Scope in declaration order. Exported rather than
// left as an implicit "iterate the iota range" contract, so the help
// overlay's group ordering does not depend on ScopeConflicts happening to be
// the last constant — a scope inserted later stays correct by construction.
func AllScopes() []Scope {
	return []Scope{ScopeGlobal, ScopeProjects, ScopeDoctor, ScopeActivity, ScopeBrowser, ScopeBrowserPreviewFocused, ScopeReading, ScopeHistory, ScopeInsights, ScopeConflicts, ScopeConflictDetail}
}

// Action is one user-invokable operation. The SAME rows drive the palette
// list, the per-view footer, and the help overlay (spec §14's single
// source).
type Action struct {
	ID      string // stable identifier ("sync-project", "quit", …)
	Title   string // palette/help/footer label ("sync")
	Keys    []string
	KeyHint string // footer/help key column ("s")
	Scope   Scope
	Mutates bool // greyed in the palette + refused while the daemon is quiesced (spec §15)
}

// registry is the full static table, in the order every surface renders it.
// Seed rows land with this task; later tasks append theirs as their screens
// land (spec plan). sync-fleet has no Keys — palette/help-only for now, no
// direct keyboard shortcut — so Binding builds it a disabled binding that
// can never match a keypress. search opens the root's global search overlay
// (spec §7) — dispatched directly by the root like help, with no runners()
// entry (dashboard.go's dispatch); its key was declared here ahead of the
// overlay, kept inert until it landed by the root's available(id) gate.
var registry = []Action{
	{ID: "switch-tabs", Title: "switch", Keys: []string{"tab", "shift+tab", "right", "left", "l", "h", "1", "2", "3", "4"}, KeyHint: "tab/1–4", Scope: ScopeGlobal},
	{ID: "select", Title: "select", Keys: []string{"up", "down", "k", "j"}, KeyHint: "↑/↓", Scope: ScopeProjects},
	{ID: "sync-project", Title: "sync", Keys: []string{"s"}, KeyHint: "s", Scope: ScopeProjects, Mutates: true},
	{ID: "untrack", Title: "untrack", Keys: []string{"u"}, KeyHint: "u", Scope: ScopeProjects, Mutates: true},
	{ID: "add-project", Title: "add", Keys: []string{"a"}, KeyHint: "a", Scope: ScopeProjects, Mutates: true},
	// migrate (m) drives the spec §10 bash-era importer through the daemon —
	// like add-project it opens a modal flow ProjectsView.Update owns (no root
	// runner), and Mutates because it seeds and enrolls a live dir, so it greys
	// and is refused while the daemon is quiesced. Its live availability is the
	// root's migrate-closures-wired gate (available("migrate")), so the row stays
	// inert in the footer and palette on a build that did not inject them.
	{ID: "migrate", Title: "migrate", Keys: []string{"m"}, KeyHint: "m", Scope: ScopeProjects, Mutates: true},
	{ID: "open-browser", Title: "open", Keys: []string{"enter"}, KeyHint: "enter", Scope: ScopeProjects},
	// ScopeDoctor rows (Task 19, spec §11/§12): the Doctor tab's own actions.
	// re-run refetches the read-only battery on demand — matched directly by
	// handleDoctorKey like the Projects/Conflicts cursor rows (no root runner),
	// so it stays out of the palette. fix runs the quiesce-aware `doctor --fix`
	// (Mutates — it holds the daemon's cycles during the wiring surgery, so it
	// greys and is refused while quiesced); its live availability is the root's
	// report-failed ∧ fixable-row gate (available("doctor-fix")). scan runs the
	// advisory gitleaks sweep — never Mutates (it touches no daemon state and
	// never joins SafetyGate, spec §12), available once its runner is wired.
	{ID: "doctor-rerun", Title: "re-run", Keys: []string{"r"}, KeyHint: "r", Scope: ScopeDoctor},
	{ID: "doctor-fix", Title: "fix", Keys: []string{"f"}, KeyHint: "f", Scope: ScopeDoctor, Mutates: true},
	{ID: "scan", Title: "scan", Keys: []string{"s"}, KeyHint: "s", Scope: ScopeDoctor},
	// doctor-scroll / activity-scroll: the two status tabs render
	// their bodies — the Doctor battery, the Activity snapshot — through a
	// height-bounded viewport, so a long body scrolls in place instead of being
	// clipped by the root frame. Matched by the pane's own restricted keymap in
	// the root's tab-key dispatch (like browser-scroll-preview), no root runner
	// and never Mutates. g/G jump to the ends — handled directly and, as
	// table-stakes viewport keys, left off the registry; only the non-obvious
	// half/full-page keys the footer must name are declared (spec §14).
	{ID: "doctor-scroll", Title: "scroll", Keys: []string{"ctrl+d", "ctrl+u", "pgup", "pgdown"}, KeyHint: "ctrl+d/u", Scope: ScopeDoctor},
	{ID: "activity-scroll", Title: "scroll", Keys: []string{"ctrl+d", "ctrl+u", "pgup", "pgdown"}, KeyHint: "ctrl+d/u", Scope: ScopeActivity},
	{ID: "sync-fleet", Title: "sync fleet", Scope: ScopeGlobal, Mutates: true},
	{ID: "search", Title: "search", Keys: []string{"/"}, KeyHint: "/", Scope: ScopeGlobal},
	{ID: "open-palette", Title: "palette", Keys: []string{"ctrl+k"}, KeyHint: "ctrl+k", Scope: ScopeGlobal},
	{ID: "help", Title: "help", Keys: []string{"?"}, KeyHint: "?", Scope: ScopeGlobal},
	{ID: "quit", Title: "quit", Keys: []string{"q"}, KeyHint: "q", Scope: ScopeGlobal},
	// update-agent-brain (Task 18, spec §11) surfaces the one-key self-update
	// when a newer release is available. NOT Mutates — a binary swap is not a
	// daemon mutation, so quiesce never refuses it; its live availability is
	// the root's updatePhase == updateOffered gate (available/paletteAvailable),
	// and U is dispatched directly by the root like help/search (a pure state
	// flip into the confirm prompt, no Cmd), so it carries no runners() entry.
	// The banner itself is the primary surface; this row keeps the palette/help/
	// footer honest about the same key.
	{ID: "update-agent-brain", Title: "update agent-brain", Keys: []string{"U"}, KeyHint: "U", Scope: ScopeGlobal},
	// ScopeBrowser rows (Task 11 seeded o///esc; Task 12 added enter-to-read):
	// the memory browser's own in-screen keys, matched directly by
	// Browser.updateKey — no root-level runner, same as select/switch-tabs.
	{ID: "browser-read", Title: "read", Keys: []string{"enter"}, KeyHint: "enter", Scope: ScopeBrowser},
	{ID: "browser-order", Title: "order", Keys: []string{"o"}, KeyHint: "o", Scope: ScopeBrowser},
	{ID: "browser-filter", Title: "filter", Keys: []string{"/"}, KeyHint: "/", Scope: ScopeBrowser},
	// Edit-flow rows (Task 13, spec §5): all Mutates — they land provider-
	// file writes — and, like every stack-scope row, runner-less: the views
	// match the keys directly and emit flow-request messages the root
	// handles. Their live availability (editor resolves, fact-class
	// selection, no active handoff) is the root's available(id); the stack
	// footer renders an unavailable row visibly struck rather than hidden.
	{ID: "browser-edit", Title: "edit", Keys: []string{"e"}, KeyHint: "e", Scope: ScopeBrowser, Mutates: true},
	{ID: "browser-new", Title: "new", Keys: []string{"n"}, KeyHint: "n", Scope: ScopeBrowser, Mutates: true},
	{ID: "browser-rename", Title: "rename", Keys: []string{"r"}, KeyHint: "r", Scope: ScopeBrowser, Mutates: true},
	{ID: "browser-delete", Title: "delete", Keys: []string{"d"}, KeyHint: "d", Scope: ScopeBrowser, Mutates: true},
	// history (h) and show-deleted (x) are Task 14 read surfaces, matched
	// directly by Browser.updateKey like read/order/filter — no root runner,
	// never Mutates. h drills into the selected memory's version history; x
	// toggles the list into deleted-memory recovery mode (spec §6).
	{ID: "browser-history", Title: "history", Keys: []string{"h"}, KeyHint: "h", Scope: ScopeBrowser},
	{ID: "browser-show-deleted", Title: "show deleted", Keys: []string{"x"}, KeyHint: "x", Scope: ScopeBrowser},
	// i opens the project insights screen (Task 16, spec §9) — a read surface
	// matched directly by Browser.updateKey like read/history/show-deleted: no
	// root runner, never Mutates.
	{ID: "browser-insights", Title: "insights", Keys: []string{"i"}, KeyHint: "i", Scope: ScopeBrowser},
	// copy (y) writes the selected memory's raw body to the system clipboard via
	// OSC52 — the feature-full remedy for native drag-select being suppressed
	// while the preview pane holds mouse mode (browser.go's WantsMouse), and the
	// only copy that also reaches over SSH/tmux/WSL2. Matched directly by
	// Browser.updateKey like the other read surfaces, no root runner and never
	// Mutates (a clipboard write touches no provider file).
	{ID: "browser-copy", Title: "copy", Keys: []string{"y"}, KeyHint: "y", Scope: ScopeBrowser},
	// scroll-preview reaches the preview pane's own viewport straight from the
	// list: ctrl+d/u half-page and pgup/pgdown full-page, the quick scroll of a
	// long memory's preview without leaving the list — j/k stay the LIST cursor
	// here, unlike the reading view where they scroll its full-screen viewport.
	// For the full reading toolkit (j/k line scroll, g/G ends) tab focuses the
	// pane (browser-focus-preview below). Matched by the pane's own restricted
	// keymap inside Browser.updateKey (browserPreviewKeyMap), no root runner and
	// never Mutates, like every other stack-scope navigation row. It is declared
	// here — not left as the reading view's table-stakes viewport keys are, off
	// the registry — precisely because the keys are non-obvious in a split where
	// j/k mean something else: the footer/help must name them or the rest of a
	// long memory looks unreachable (spec §14's honesty contract).
	{ID: "browser-scroll-preview", Title: "scroll", Keys: []string{"ctrl+d", "ctrl+u", "pgup", "pgdown"}, KeyHint: "ctrl+d/u", Scope: ScopeBrowser},
	// focus-preview (tab) gives the preview pane keyboard focus so the reading
	// view's full scroll keymap (j/k, ctrl+d/u, pgup/pgdown, g/G) drives it — the
	// lazygit-style focus-the-preview idiom; tab/esc hand focus back to the list.
	// Matched directly by Browser.updateKey, no root runner and never Mutates,
	// like every other stack-scope navigation row. Inert when no preview is on
	// screen (narrow width) yet unconditionally available in the footer — the
	// same honesty parity as scroll-preview: the key is real, its effect is
	// situational (dashboard.go's available()).
	{ID: "browser-focus-preview", Title: "focus preview", Keys: []string{"tab"}, KeyHint: "tab", Scope: ScopeBrowser},
	// mouse-capture-toggle (m) disarms the preview pane's cell-motion mouse
	// capture so the terminal's own drag-select works in the pane. Mouse capture
	// is terminal-global — scoped capture is not expressible — so the only honest
	// remedy is a runtime switch that turns it off entirely, with the state
	// disclosed every frame it is off (dashboard.go's mouseCaptureDisclosure).
	// Runner-less like every other browser navigation row, but NOT root-matched:
	// m must stay a typable letter while the in-browser filter owns input, so
	// Browser.updateKey matches m in its mode dispatch (skipping the filter
	// branch) and emits MouseCaptureToggleMsg; the root flips its own
	// mouseCaptureOff in that message's handler. Never a daemon mutation, so it
	// never Mutates and a quiesce leaves it alone. Its m is free within
	// ScopeBrowser; migrate binds m too but under ScopeProjects, and cross-scope
	// reuse is legal (only one scope is ever the matched footer scope at a time).
	{ID: "mouse-capture-toggle", Title: "mouse capture", Keys: []string{"m"}, KeyHint: "m", Scope: ScopeBrowser},
	{ID: "browser-back", Title: "back", Keys: []string{"esc"}, KeyHint: "esc", Scope: ScopeBrowser},
	// ScopeBrowserPreviewFocused rows: while the preview pane holds
	// keyboard focus (tab, or a click into it) the browser swaps its footer to
	// THIS set — exactly the keys that do something in that mode — so a focused
	// pane is never a silent dead-key state (the live-hub "freeze" where every
	// list key read as inert with no cue why). All are matched directly by
	// Browser.updateKey's focused block — esc/tab hand focus back, g/G jump the
	// ends, j/k + ctrl+d/u + pgup/pgdown scroll the viewport, y copies the
	// previewed body, m flips mouse capture (browser-preview-mouse-capture, the
	// focused-scope twin of mouse-capture-toggle, so the focused footer names the
	// key that already works there) — so like every other stack-scope row they
	// carry no root
	// runner. None Mutates: the scroll/return keys touch no daemon state and the
	// copy is a clipboard write (OSC52), not a provider-file mutation. The keys
	// deliberately re-use ScopeBrowser's own bindings under distinct IDs
	// (cross-scope key reuse is fine — only one scope is ever the matched footer
	// scope at a time); giving them their own scope is what lets the footer swap
	// be a single scope switch (dashboard.go's stackFooterRows) rather than a
	// hand-filtered binding list. esc/tab leads the set: the freeze was a user
	// who could not tell how to get BACK to the list, so the footer names that
	// first. These rows are runner-less, so paletteAvailable keeps them out of
	// the command palette exactly as it does the other browser navigation rows.
	{ID: "browser-preview-list", Title: "list", Keys: []string{"esc", "tab"}, KeyHint: "esc/tab", Scope: ScopeBrowserPreviewFocused},
	{ID: "browser-preview-scroll", Title: "scroll", Keys: []string{"j", "k"}, KeyHint: "j/k", Scope: ScopeBrowserPreviewFocused},
	{ID: "browser-preview-half-page", Title: "half page", Keys: []string{"ctrl+d", "ctrl+u"}, KeyHint: "ctrl+d/u", Scope: ScopeBrowserPreviewFocused},
	{ID: "browser-preview-page", Title: "page", Keys: []string{"pgup", "pgdown"}, KeyHint: "pgup/pgdn", Scope: ScopeBrowserPreviewFocused},
	{ID: "browser-preview-ends", Title: "ends", Keys: []string{"g", "G"}, KeyHint: "g/G", Scope: ScopeBrowserPreviewFocused},
	{ID: "browser-preview-copy", Title: "copy", Keys: []string{"y"}, KeyHint: "y", Scope: ScopeBrowserPreviewFocused},
	{ID: "browser-preview-mouse-capture", Title: "mouse capture", Keys: []string{"m"}, KeyHint: "m", Scope: ScopeBrowserPreviewFocused},
	// ScopeReading rows (Task 12; Task 13 added reading-edit): the reading
	// view's own in-screen keys, matched directly by Reading.updateKey.
	// h-history is deliberately absent — Task 14 declares that row together
	// with its screen, so no dead row advertises an unbuilt key.
	// scroll documents the viewport keys (spec §4) in the footer and help; the
	// viewport's own keymap matches them (readingViewportKeyMap), no root
	// runner. The wheel reaches the same path: alternate scroll (ADR 21)
	// delivers wheel notches as these arrow keys.
	{ID: "reading-scroll", Title: "scroll", Keys: []string{"j", "k"}, KeyHint: "j/k", Scope: ScopeReading},
	{ID: "reading-links", Title: "links", Keys: []string{"tab", "shift+tab"}, KeyHint: "tab", Scope: ScopeReading},
	{ID: "reading-follow", Title: "follow", Keys: []string{"enter"}, KeyHint: "enter", Scope: ScopeReading},
	{ID: "reading-backlinks", Title: "backlinks", Keys: []string{"b"}, KeyHint: "b", Scope: ScopeReading},
	{ID: "reading-copy-path", Title: "copy path", Keys: []string{"y"}, KeyHint: "y", Scope: ScopeReading},
	// copy-body (Y) writes the open memory's raw markdown source to the clipboard
	// via OSC52 — the reading-view twin of browser-copy, y's capital so it sits
	// beside copy-path without shadowing it. Matched directly by Reading.updateKey,
	// no root runner and never Mutates.
	{ID: "reading-copy-body", Title: "copy body", Keys: []string{"Y"}, KeyHint: "Y", Scope: ScopeReading},
	{ID: "reading-edit", Title: "edit", Keys: []string{"e"}, KeyHint: "e", Scope: ScopeReading, Mutates: true},
	// h drills into the open memory's version history (Task 14), matched
	// directly by Reading.updateKey — the same key the reading viewport's own
	// keymap deliberately left unbound for exactly this row.
	{ID: "reading-history", Title: "history", Keys: []string{"h"}, KeyHint: "h", Scope: ScopeReading},
	{ID: "reading-back", Title: "back", Keys: []string{"esc"}, KeyHint: "esc", Scope: ScopeReading},
	// ScopeHistory rows (Task 14, spec §6): the version-history screen's own
	// in-screen keys, matched directly by History.updateKey — no root runner,
	// like every other stack scope. view/diff/diff-older/back are read
	// surfaces; restore lands a NEW version (Mutates) through the edit flow's
	// finish machinery, so it greys while quiesced and its availability is the
	// root's own fact-class ∧ no-active-session gate (available(id)).
	{ID: "history-view", Title: "view", Keys: []string{"enter"}, KeyHint: "enter", Scope: ScopeHistory},
	{ID: "history-diff", Title: "diff vs live", Keys: []string{"d"}, KeyHint: "d", Scope: ScopeHistory},
	{ID: "history-diff-older", Title: "diff older", Keys: []string{"D"}, KeyHint: "D", Scope: ScopeHistory},
	{ID: "history-restore", Title: "restore", Keys: []string{"R"}, KeyHint: "R", Scope: ScopeHistory, Mutates: true},
	{ID: "history-back", Title: "back", Keys: []string{"esc"}, KeyHint: "esc", Scope: ScopeHistory},
	// ScopeInsights row (Task 16, spec §9): the pushed insights screen scrolls
	// its stat sections through the reading viewport's own keymap (table-stakes
	// scroll keys, not registry actions, like every other screen's viewport), so
	// esc is its only registry row — matched directly by Insights.updateKey, no
	// root runner.
	{ID: "insights-back", Title: "back", Keys: []string{"esc"}, KeyHint: "esc", Scope: ScopeInsights},
	// ScopeConflicts rows: the Conflicts tab's own list cursor + drill-in,
	// matched directly by ConflictsView.Update exactly as Projects' select/open
	// are — no root runner, unconditionally available so the tab footer keeps
	// naming them (dashboard.go's available()).
	{ID: "conflicts-select", Title: "select", Keys: []string{"up", "down", "k", "j"}, KeyHint: "↑/↓", Scope: ScopeConflicts},
	{ID: "conflicts-open", Title: "open", Keys: []string{"enter"}, KeyHint: "enter", Scope: ScopeConflicts},
	// ScopeConflictDetail rows: the pushed conflict-detail screen's own keys
	// (spec §10), matched directly by ConflictDetail.updateKey. read/back are
	// structural navigation; edit only EMITS EditRequestMsg — the root's
	// startEditFlow owns every gate (cleaning up a merge IS an edit) — so its
	// live availability is flowAvailable (editor resolves ∧ fact-class ∧ no
	// active handoff ∧ the recorded path still maps to an enrolled unit),
	// rendered struck rather than hidden when false. read is likewise gated on
	// the path still mapping: an unmapped record offers nothing to read. history
	// is gated one notch wider — on the path resolving to an enrolled unit at all,
	// mapped OR enrolled-but-deleted — since a since-deleted file can still own a
	// version chain to browse and restore an earlier version from; it only pushes
	// the History screen, so the root gates it on the detail's own resolution.
	{ID: "conflictdetail-read", Title: "read", Keys: []string{"enter"}, KeyHint: "enter", Scope: ScopeConflictDetail},
	{ID: "conflictdetail-edit", Title: "edit", Keys: []string{"e"}, KeyHint: "e", Scope: ScopeConflictDetail, Mutates: true},
	{ID: "conflictdetail-history", Title: "history", Keys: []string{"h"}, KeyHint: "h", Scope: ScopeConflictDetail},
	{ID: "conflictdetail-back", Title: "back", Keys: []string{"esc"}, KeyHint: "esc", Scope: ScopeConflictDetail},
}

// Registry returns the full static table, defensively copied so a caller
// mutating its slice (e.g. sorting it in place) can never corrupt the
// package's own copy.
func Registry() []Action {
	out := make([]Action, len(registry))
	copy(out, registry)
	return out
}

// ForScope returns the Registry() rows whose Scope matches s, render order
// preserved.
func ForScope(s Scope) []Action {
	var out []Action
	for _, a := range registry {
		if a.Scope == s {
			out = append(out, a)
		}
	}
	return out
}

// Binding builds the bubbles key.Binding a real keypress is matched against
// and a footer/help row is rendered from — the sole translation from Action
// to key.Binding, so every surface derives from one function. An Action with
// no Keys (sync-fleet) yields a binding whose Enabled() is false: bubbles'
// own key.Binding.Enabled() requires non-nil keys, so it can never match a
// keypress and a rendering loop can skip it with the same check it already
// uses for every other disabled binding.
func Binding(a Action) keybinding.Binding {
	return keybinding.NewBinding(keybinding.WithKeys(a.Keys...), keybinding.WithHelp(a.KeyHint, a.Title))
}

// Fuzzy filters Registry() to actions whose Title or ID contains query
// case-insensitively, ranked prefix > substring > subsequence, stable within
// a rank so ties preserve registry order. An empty query matches everything
// at the same (lowest) rank, so the whole registry comes back in declared
// order — the palette's "nothing typed yet" state falls out of this for
// free rather than needing a separate branch.
func Fuzzy(query string) []Action {
	query = strings.ToLower(strings.TrimSpace(query))

	type ranked struct {
		action Action
		rank   int
	}
	matches := make([]ranked, 0, len(registry))
	for _, a := range registry {
		if rank, ok := matchRank(query, a); ok {
			matches = append(matches, ranked{action: a, rank: rank})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].rank < matches[j].rank })

	out := make([]Action, len(matches))
	for i, m := range matches {
		out[i] = m.action
	}
	return out
}

// Rank tiers Fuzzy sorts by — lower is a better match.
const (
	rankPrefix = iota
	rankSubstring
	rankSubsequence
)

// matchRank reports the best rank at which query matches a's Title or ID, or
// ok=false if neither matches at all. An empty query matches everything at
// rankSubsequence — the weakest tier, but since every row ties there, the
// caller's stable sort leaves registry order untouched.
func matchRank(query string, a Action) (int, bool) {
	if query == "" {
		return rankSubsequence, true
	}
	found := false
	best := rankSubsequence
	for _, haystack := range [...]string{strings.ToLower(a.Title), strings.ToLower(a.ID)} {
		switch {
		case strings.HasPrefix(haystack, query):
			return rankPrefix, true // best possible rank for this action; no need to check the other haystack
		case strings.Contains(haystack, query):
			found = true
			best = min(best, rankSubstring)
		case isSubsequence(query, haystack):
			found = true
		}
	}
	return best, found
}

// isSubsequence reports whether every byte of query appears in haystack in
// order (not necessarily adjacent). Action titles/IDs are plain ASCII, so a
// byte-wise scan is exact — no need for rune-aware matching in this domain.
func isSubsequence(query, haystack string) bool {
	i := 0
	for j := range len(haystack) {
		if i == len(query) {
			return true
		}
		if haystack[j] == query[i] {
			i++
		}
	}
	return i == len(query)
}
