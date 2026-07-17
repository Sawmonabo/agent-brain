package actions

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

// idsOf projects a result slice down to its IDs, in order, so ranking
// assertions read as a plain slice comparison instead of walking Action
// structs field by field.
func idsOf(list []Action) []string {
	ids := make([]string, len(list))
	for i, a := range list {
		ids[i] = a.ID
	}
	return ids
}

// TestRegistryIDsUniqueAndKeysDisjointPerScope pins the two structural
// invariants the footer/palette/help unification depends on: no two rows can
// answer to the same ID (dispatch resolves by ID), and within a single scope
// no two rows can claim the same key (the active surface would not know
// which one a keypress meant). Cross-scope key reuse is fine — Global and a
// tab scope are never both "the matched scope" for the same footer render in
// a way that lets one key mean two things at once, and the registry's own
// key sets never actually overlap that way (verified by inspection, not this
// test's job to enforce).
func TestRegistryIDsUniqueAndKeysDisjointPerScope(t *testing.T) {
	t.Parallel()

	seenIDs := make(map[string]bool)
	for _, a := range Registry() {
		if seenIDs[a.ID] {
			t.Errorf("duplicate action ID %q", a.ID)
		}
		seenIDs[a.ID] = true
	}

	for _, scope := range AllScopes() {
		t.Run(scope.String(), func(t *testing.T) {
			t.Parallel()
			owner := make(map[string]string)
			for _, a := range ForScope(scope) {
				for _, k := range a.Keys {
					if prior, ok := owner[k]; ok {
						t.Errorf("key %q bound to both %q and %q in scope %s", k, prior, a.ID, scope)
					}
					owner[k] = a.ID
				}
			}
		})
	}
}

// TestForScopePreservesRenderOrder pins that ForScope filters without
// reordering — the footer and help overlay both depend on registry
// declaration order surviving the scope filter.
func TestForScopePreservesRenderOrder(t *testing.T) {
	t.Parallel()
	want := idsOf(Registry())
	var gotInterleaved []string
	for _, scope := range AllScopes() {
		gotInterleaved = append(gotInterleaved, idsOf(ForScope(scope))...)
	}
	// Every action belongs to exactly one scope, so re-flattening ForScope
	// across every scope in AllScopes order must reproduce a stable
	// partition of Registry(); within each partition, relative order must
	// match Registry()'s own order. Assert the weaker, directly useful
	// property: each per-scope slice is itself a (order-preserving)
	// subsequence of Registry().
	for _, scope := range AllScopes() {
		rows := idsOf(ForScope(scope))
		idx := 0
		for _, id := range want {
			if idx < len(rows) && rows[idx] == id {
				idx++
			}
		}
		if idx != len(rows) {
			t.Errorf("ForScope(%s) = %v is not an order-preserving subsequence of Registry()", scope, rows)
		}
	}
	if len(gotInterleaved) != len(want) {
		t.Errorf("ForScope across AllScopes covers %d actions, want %d (every action in exactly one scope)", len(gotInterleaved), len(want))
	}
}

// TestFuzzyRanksPrefixOverSubsequence pins the palette's ranking contract:
// "sy" is a literal prefix of both sync actions' IDs (the top tier) and also a
// subsequence of every action whose ID carries "history" (hi·s·tor·y — the
// weaker tier), so it must return the two sync actions FIRST, then the
// history-bearing actions in registry order, proving prefix strictly outranks
// subsequence. "qt" is not a prefix or substring of "quit" — it only appears in
// order (q, then t skipping u/i) — so it must still surface quit via that same
// subsequence tier.
func TestFuzzyRanksPrefixOverSubsequence(t *testing.T) {
	t.Parallel()

	t.Run("sy ranks sync actions by prefix above history by subsequence", func(t *testing.T) {
		t.Parallel()
		got := idsOf(Fuzzy("sy"))
		want := []string{
			"sync-project", "sync-fleet", // prefix tier
			// subsequence tier, registry order — browser-copy and the focused
			// preview's own copy row carry "sy" too (brow·s·er…cop·y); in registry
			// order browser-preview-copy trails browser-copy, amongst the history rows.
			"browser-history", "browser-copy", "browser-preview-copy", "reading-history",
			"history-view", "history-diff", "history-diff-older", "history-restore", "history-back",
			"conflictdetail-history",
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("Fuzzy(\"sy\") mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("qt still finds quit via subsequence", func(t *testing.T) {
		t.Parallel()
		got := idsOf(Fuzzy("qt"))
		want := []string{"quit"}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("Fuzzy(\"qt\") mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("empty query returns everything in registry order", func(t *testing.T) {
		t.Parallel()
		if diff := cmp.Diff(idsOf(Registry()), idsOf(Fuzzy(""))); diff != "" {
			t.Errorf("Fuzzy(\"\") mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("no match returns nothing", func(t *testing.T) {
		t.Parallel()
		if got := Fuzzy("zzzzznomatch"); len(got) != 0 {
			t.Errorf("Fuzzy(nonsense) = %v, want empty", idsOf(got))
		}
	})
}

// TestBindingCarriesKeysAndHelp pins that Binding is a faithful translation
// of an Action's Keys/KeyHint/Title — the footer, palette, and keymap.go all
// render bubbles key.Binding values built exclusively through this function,
// so a bug here would silently wrong-foot every surface at once.
func TestBindingCarriesKeysAndHelp(t *testing.T) {
	t.Parallel()
	action := Action{ID: "sync-project", Title: "sync", Keys: []string{"s"}, KeyHint: "s", Scope: ScopeProjects, Mutates: true}
	binding := Binding(action)

	if diff := cmp.Diff(action.Keys, binding.Keys()); diff != "" {
		t.Errorf("Binding keys mismatch (-want +got):\n%s", diff)
	}
	help := binding.Help()
	if help.Key != action.KeyHint || help.Desc != action.Title {
		t.Errorf("Binding help = %+v, want {Key:%q Desc:%q}", help, action.KeyHint, action.Title)
	}
	if !binding.Enabled() {
		t.Error("a binding with real keys must be enabled")
	}
}

// TestBindingWithNoKeysIsDisabled pins sync-fleet's shape: an Action with no
// Keys (palette/help-only, no keyboard shortcut) must build a DISABLED
// binding, so it can never accidentally match a keypress and so the footer's
// key-matching loop can rely on Enabled() rather than a separate len check.
func TestBindingWithNoKeysIsDisabled(t *testing.T) {
	t.Parallel()
	binding := Binding(Action{ID: "sync-fleet", Title: "sync fleet", Scope: ScopeGlobal, Mutates: true})
	if binding.Enabled() {
		t.Error("a binding built from a keyless Action must be disabled")
	}
}

// TestSeedRegistryShape pins the seed rows Task 5 introduced: stable IDs
// every later task and the root's dispatch/available/runners keys off of,
// plus the two rows that task deliberately left inert at the time
// (sync-fleet has no direct key; search's key was reserved ahead of the
// overlay that now dispatches it — this test only pins the shape the
// registry declares, not availability, which is root-private). Later tasks
// append their own rows as their screens land
// (spec plan) — TestBrowserRegistryRowsShape below pins Task 11's — so this
// only asserts Task 5's rows are present with the right shape, not that
// they are the registry's entire contents.
func TestSeedRegistryShape(t *testing.T) {
	t.Parallel()
	tests := []struct {
		id      string
		keys    []string
		mutates bool
		scope   Scope
	}{
		{id: "switch-tabs", keys: []string{"tab", "shift+tab", "right", "left", "l", "h", "1", "2", "3", "4"}, scope: ScopeGlobal},
		{id: "select", keys: []string{"up", "down", "k", "j"}, scope: ScopeProjects},
		{id: "sync-project", keys: []string{"s"}, mutates: true, scope: ScopeProjects},
		{id: "untrack", keys: []string{"u"}, mutates: true, scope: ScopeProjects},
		{id: "add-project", keys: []string{"a"}, mutates: true, scope: ScopeProjects},
		{id: "sync-fleet", keys: nil, mutates: true, scope: ScopeGlobal},
		{id: "search", keys: []string{"/"}, scope: ScopeGlobal},
		{id: "open-palette", keys: []string{"ctrl+k"}, scope: ScopeGlobal},
		{id: "help", keys: []string{"?"}, scope: ScopeGlobal},
		{id: "quit", keys: []string{"q"}, scope: ScopeGlobal},
	}

	byID := make(map[string]Action)
	for _, a := range Registry() {
		byID[a.ID] = a
	}

	for _, testCase := range tests {
		t.Run(testCase.id, func(t *testing.T) {
			t.Parallel()
			action, ok := byID[testCase.id]
			if !ok {
				t.Fatalf("registry missing action %q", testCase.id)
			}
			if diff := cmp.Diff(testCase.keys, action.Keys); diff != "" {
				t.Errorf("Keys mismatch (-want +got):\n%s", diff)
			}
			if action.Mutates != testCase.mutates {
				t.Errorf("Mutates = %v, want %v", action.Mutates, testCase.mutates)
			}
			if action.Scope != testCase.scope {
				t.Errorf("Scope = %v, want %v", action.Scope, testCase.scope)
			}
			if action.Title == "" {
				t.Error("Title must not be empty — it is the palette/help label")
			}
		})
	}
}

// TestUpdateRegistryRowShape pins Task 18's registry addition (spec §11): the
// global update-agent-brain row. NOT Mutates — a self-update is not a daemon
// mutation, so quiesce never refuses it — and, like help/search, it carries no
// runner (the root dispatches it directly into the confirm prompt). Its live
// availability (updatePhase == updateOffered) is root-private, pinned in the
// dashboard package.
func TestUpdateRegistryRowShape(t *testing.T) {
	t.Parallel()
	byID := make(map[string]Action)
	for _, a := range Registry() {
		byID[a.ID] = a
	}
	action, ok := byID["update-agent-brain"]
	if !ok {
		t.Fatal("registry missing action \"update-agent-brain\"")
	}
	if diff := cmp.Diff([]string{"U"}, action.Keys); diff != "" {
		t.Errorf("Keys mismatch (-want +got):\n%s", diff)
	}
	if action.KeyHint != "U" {
		t.Errorf("KeyHint = %q, want \"U\"", action.KeyHint)
	}
	if action.Mutates {
		t.Error("Mutates = true, want false — a self-update is not a daemon mutation")
	}
	if action.Scope != ScopeGlobal {
		t.Errorf("Scope = %v, want ScopeGlobal", action.Scope)
	}
	if action.Title == "" {
		t.Error("Title must not be empty — it is the palette/help label")
	}
}

// TestBrowserRegistryRowsShape pins Task 11's own registry additions: the
// Projects tab's entry point into the memory browser (open-browser) and the
// browser's own in-screen keys (ScopeBrowser). None Mutates, and none has a
// root-level runner — they are handled by direct view-level key routing,
// the same "select"/"switch-tabs" discipline Task 5 established — so they
// are footer/help-only and correctly absent from the palette
// (TestPaletteListsOnlyDispatchableActions, in the dashboard package, pins
// that half; this test only pins the declared shape).
func TestBrowserRegistryRowsShape(t *testing.T) {
	t.Parallel()
	tests := []struct {
		id    string
		keys  []string
		scope Scope
	}{
		{id: "open-browser", keys: []string{"enter"}, scope: ScopeProjects},
		{id: "browser-order", keys: []string{"o"}, scope: ScopeBrowser},
		{id: "browser-filter", keys: []string{"/"}, scope: ScopeBrowser},
		{id: "browser-copy", keys: []string{"y"}, scope: ScopeBrowser},
		{id: "browser-back", keys: []string{"esc"}, scope: ScopeBrowser},
	}

	byID := make(map[string]Action)
	for _, a := range Registry() {
		byID[a.ID] = a
	}

	for _, testCase := range tests {
		t.Run(testCase.id, func(t *testing.T) {
			t.Parallel()
			action, ok := byID[testCase.id]
			if !ok {
				t.Fatalf("registry missing action %q", testCase.id)
			}
			if diff := cmp.Diff(testCase.keys, action.Keys); diff != "" {
				t.Errorf("Keys mismatch (-want +got):\n%s", diff)
			}
			if action.Mutates {
				t.Error("Mutates = true, want false")
			}
			if action.Scope != testCase.scope {
				t.Errorf("Scope = %v, want %v", action.Scope, testCase.scope)
			}
			if action.Title == "" {
				t.Error("Title must not be empty — it is the palette/help label")
			}
		})
	}
}

// TestReadingRegistryRowsShape pins Task 12's registry additions (plus the
// later reading-scroll row, ADR 21): the browser's enter-to-read row and the
// reading view's own in-screen keys (ScopeReading). Same discipline as the
// browser rows above: none Mutates, none has a root-level runner (direct
// view-level key routing), so they are footer/help-only and absent from the
// palette. h-history is deliberately NOT here — Task 14 registers that row
// together with its screen; the edit flow's own rows (e/n/r/d, all Mutates)
// are pinned separately by TestFlowRegistryRowsShape below.
func TestReadingRegistryRowsShape(t *testing.T) {
	t.Parallel()
	tests := []struct {
		id    string
		keys  []string
		scope Scope
	}{
		{id: "browser-read", keys: []string{"enter"}, scope: ScopeBrowser},
		{id: "reading-scroll", keys: []string{"j", "k"}, scope: ScopeReading},
		{id: "reading-links", keys: []string{"tab", "shift+tab"}, scope: ScopeReading},
		{id: "reading-follow", keys: []string{"enter"}, scope: ScopeReading},
		{id: "reading-backlinks", keys: []string{"b"}, scope: ScopeReading},
		{id: "reading-copy-path", keys: []string{"y"}, scope: ScopeReading},
		{id: "reading-copy-body", keys: []string{"Y"}, scope: ScopeReading},
		{id: "reading-back", keys: []string{"esc"}, scope: ScopeReading},
	}

	byID := make(map[string]Action)
	for _, a := range Registry() {
		byID[a.ID] = a
	}

	for _, testCase := range tests {
		t.Run(testCase.id, func(t *testing.T) {
			t.Parallel()
			action, ok := byID[testCase.id]
			if !ok {
				t.Fatalf("registry missing action %q", testCase.id)
			}
			if diff := cmp.Diff(testCase.keys, action.Keys); diff != "" {
				t.Errorf("Keys mismatch (-want +got):\n%s", diff)
			}
			if action.Mutates {
				t.Error("Mutates = true, want false")
			}
			if action.Scope != testCase.scope {
				t.Errorf("Scope = %v, want %v", action.Scope, testCase.scope)
			}
			if action.Title == "" {
				t.Error("Title must not be empty — it is the palette/help label")
			}
		})
	}
}

// TestReadingScrollLeadsReadingScope pins reading-scroll's position, not just
// its shape (TestReadingRegistryRowsShape above covers that): the footer
// renders a scope's rows in registry order (spec §14), and reading-scroll is
// the reading view's primary navigation affordance, so it must lead the
// reading footer rather than land wherever registry insertion happened to
// put it.
func TestReadingScrollLeadsReadingScope(t *testing.T) {
	t.Parallel()
	rows := ForScope(ScopeReading)
	if len(rows) == 0 {
		t.Fatal("ForScope(ScopeReading) returned no rows")
	}
	if got := rows[0].ID; got != "reading-scroll" {
		t.Errorf("first ScopeReading row = %q, want %q (the scroll hint must lead the reading footer)", got, "reading-scroll")
	}
}

// TestFlowRegistryRowsShape pins Task 13's registry additions: the edit
// flow's mutation keys in the browser (e/n/r/d) and the reading view's e.
// All Mutates — they land provider-file writes — which is what makes the
// stack footer grey them while the daemon is quiesced (spec §15). Like
// every other stack-scope row they have no root-level runner: the views
// match the keys directly and emit flow-request messages the root handles.
func TestFlowRegistryRowsShape(t *testing.T) {
	t.Parallel()
	tests := []struct {
		id    string
		keys  []string
		scope Scope
	}{
		{id: "browser-edit", keys: []string{"e"}, scope: ScopeBrowser},
		{id: "browser-new", keys: []string{"n"}, scope: ScopeBrowser},
		{id: "browser-rename", keys: []string{"r"}, scope: ScopeBrowser},
		{id: "browser-delete", keys: []string{"d"}, scope: ScopeBrowser},
		{id: "reading-edit", keys: []string{"e"}, scope: ScopeReading},
	}

	byID := make(map[string]Action)
	for _, a := range Registry() {
		byID[a.ID] = a
	}

	for _, testCase := range tests {
		t.Run(testCase.id, func(t *testing.T) {
			t.Parallel()
			action, ok := byID[testCase.id]
			if !ok {
				t.Fatalf("registry missing action %q", testCase.id)
			}
			if diff := cmp.Diff(testCase.keys, action.Keys); diff != "" {
				t.Errorf("Keys mismatch (-want +got):\n%s", diff)
			}
			if !action.Mutates {
				t.Error("Mutates = false, want true — the flow rows land provider-file writes")
			}
			if action.Scope != testCase.scope {
				t.Errorf("Scope = %v, want %v", action.Scope, testCase.scope)
			}
			if action.Title == "" {
				t.Error("Title must not be empty — it is the palette/help label")
			}
		})
	}
}

// TestDoctorRegistryRowsShape pins Task 19's registry additions (spec §11/§12):
// the Doctor tab's r/f/s actions. The Mutates flags are the load-bearing part —
// doctor-fix quiesces the daemon, so it MUST be Mutates (greyed and refused
// while quiesced); re-run (a read-only refetch) and scan (an advisory sweep
// that never joins SafetyGate) MUST NOT be, or a quiesce would wrongly refuse
// them.
func TestDoctorRegistryRowsShape(t *testing.T) {
	t.Parallel()
	tests := []struct {
		id      string
		keys    []string
		mutates bool
	}{
		{id: "doctor-rerun", keys: []string{"r"}, mutates: false},
		{id: "doctor-fix", keys: []string{"f"}, mutates: true},
		{id: "scan", keys: []string{"s"}, mutates: false},
	}

	byID := make(map[string]Action)
	for _, a := range Registry() {
		byID[a.ID] = a
	}

	for _, testCase := range tests {
		t.Run(testCase.id, func(t *testing.T) {
			t.Parallel()
			action, ok := byID[testCase.id]
			if !ok {
				t.Fatalf("registry missing action %q", testCase.id)
			}
			if diff := cmp.Diff(testCase.keys, action.Keys); diff != "" {
				t.Errorf("Keys mismatch (-want +got):\n%s", diff)
			}
			if action.Mutates != testCase.mutates {
				t.Errorf("Mutates = %v, want %v", action.Mutates, testCase.mutates)
			}
			if action.Scope != ScopeDoctor {
				t.Errorf("Scope = %v, want %v", action.Scope, ScopeDoctor)
			}
			if action.Title == "" {
				t.Error("Title must not be empty — it is the palette/help label")
			}
		})
	}
}

// TestBrowserPreviewFocusedRegistryRowsShape pins this wave's ScopeBrowserPreviewFocused
// rows: the footer set the browser swaps to while the preview pane holds keyboard
// focus (dashboard.go's stackFooterRows consults Browser.PreviewFocused()). Each
// row re-uses a key ScopeBrowser also binds under a different ID — legal because
// only one scope is ever the matched footer scope at a time — so this asserts the
// exact per-row shape the footer renders. None Mutates: the scroll/return keys
// touch no daemon state, and copy is an OSC52 clipboard write, never a
// provider-file mutation, so a quiesce must not grey any of them. Runner-less like
// every stack-scope row, so they stay out of the palette
// (TestPaletteListsOnlyDispatchableActions pins that half in the dashboard package).
func TestBrowserPreviewFocusedRegistryRowsShape(t *testing.T) {
	t.Parallel()
	tests := []struct {
		id      string
		keys    []string
		keyHint string
	}{
		{id: "browser-preview-list", keys: []string{"esc", "tab"}, keyHint: "esc/tab"},
		{id: "browser-preview-scroll", keys: []string{"j", "k"}, keyHint: "j/k"},
		{id: "browser-preview-half-page", keys: []string{"ctrl+d", "ctrl+u"}, keyHint: "ctrl+d/u"},
		{id: "browser-preview-page", keys: []string{"pgup", "pgdown"}, keyHint: "pgup/pgdn"},
		{id: "browser-preview-ends", keys: []string{"g", "G"}, keyHint: "g/G"},
		{id: "browser-preview-copy", keys: []string{"y"}, keyHint: "y"},
	}

	byID := make(map[string]Action)
	for _, a := range Registry() {
		byID[a.ID] = a
	}

	for _, testCase := range tests {
		t.Run(testCase.id, func(t *testing.T) {
			t.Parallel()
			action, ok := byID[testCase.id]
			if !ok {
				t.Fatalf("registry missing action %q", testCase.id)
			}
			if diff := cmp.Diff(testCase.keys, action.Keys); diff != "" {
				t.Errorf("Keys mismatch (-want +got):\n%s", diff)
			}
			if action.KeyHint != testCase.keyHint {
				t.Errorf("KeyHint = %q, want %q", action.KeyHint, testCase.keyHint)
			}
			if action.Mutates {
				t.Error("Mutates = true, want false — a focused-preview key touches no daemon state")
			}
			if action.Scope != ScopeBrowserPreviewFocused {
				t.Errorf("Scope = %v, want %v", action.Scope, ScopeBrowserPreviewFocused)
			}
			if action.Title == "" {
				t.Error("Title must not be empty — it is the palette/help label")
			}
		})
	}
}

// TestBrowserPreviewFocusedReturnLeadsScope pins that browser-preview-list (esc/tab
// back to the list) leads its scope, not just belongs to it: the footer renders a
// scope's rows in registry order, and the whole point of this footer swap is a user
// who cannot tell how to get BACK from a focused pane — so "return to list" must be
// the first thing the focused footer names.
func TestBrowserPreviewFocusedReturnLeadsScope(t *testing.T) {
	t.Parallel()
	rows := ForScope(ScopeBrowserPreviewFocused)
	if len(rows) == 0 {
		t.Fatal("ForScope(ScopeBrowserPreviewFocused) returned no rows")
	}
	if got := rows[0].ID; got != "browser-preview-list" {
		t.Errorf("first ScopeBrowserPreviewFocused row = %q, want %q (return-to-list must lead the focused footer)", got, "browser-preview-list")
	}
}

// TestScrollRegistryRowsShape pins this wave's registry additions: the bounded
// tab bodies' scroll rows — doctor-scroll in the Doctor scope and activity-scroll
// in the new Activity scope. Both carry exactly the half/full-page keys (g/G are
// table-stakes viewport keys handled directly, off the registry), never Mutates
// (a scroll touches no daemon state, so a quiesce must not refuse it), and have
// no root runner — routed straight to the pane's own keymap in the root's
// tab-key dispatch, the same discipline as browser-scroll-preview.
func TestScrollRegistryRowsShape(t *testing.T) {
	t.Parallel()
	tests := []struct {
		id    string
		scope Scope
	}{
		{id: "doctor-scroll", scope: ScopeDoctor},
		{id: "activity-scroll", scope: ScopeActivity},
	}

	byID := make(map[string]Action)
	for _, a := range Registry() {
		byID[a.ID] = a
	}

	for _, testCase := range tests {
		t.Run(testCase.id, func(t *testing.T) {
			t.Parallel()
			action, ok := byID[testCase.id]
			if !ok {
				t.Fatalf("registry missing action %q", testCase.id)
			}
			if diff := cmp.Diff([]string{"ctrl+d", "ctrl+u", "pgup", "pgdown"}, action.Keys); diff != "" {
				t.Errorf("Keys mismatch (-want +got):\n%s", diff)
			}
			if action.KeyHint != "ctrl+d/u" {
				t.Errorf("KeyHint = %q, want %q", action.KeyHint, "ctrl+d/u")
			}
			if action.Mutates {
				t.Error("Mutates = true, want false — a scroll touches no daemon state")
			}
			if action.Scope != testCase.scope {
				t.Errorf("Scope = %v, want %v", action.Scope, testCase.scope)
			}
			if action.Title == "" {
				t.Error("Title must not be empty — it is the palette/help label")
			}
		})
	}
}
