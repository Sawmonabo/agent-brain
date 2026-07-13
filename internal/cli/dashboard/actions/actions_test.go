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
// "sy" is a literal prefix of both sync actions' titles/IDs and nothing else
// in the registry contains a "y" at all, so it must return exactly those two,
// prefix-ranked. "qt" is not a prefix or substring of "quit" — it only
// appears in order (q, then t skipping u/i) — so it must still surface quit
// via the weaker subsequence tier, proving that tier actually runs.
func TestFuzzyRanksPrefixOverSubsequence(t *testing.T) {
	t.Parallel()

	t.Run("sy ranks both sync actions by prefix", func(t *testing.T) {
		t.Parallel()
		got := idsOf(Fuzzy("sy"))
		want := []string{"sync-project", "sync-fleet"}
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

// TestSeedRegistryShape pins the seed rows this task owns (later tasks
// append their own, spec plan Task 5): stable IDs every later task and the
// root's dispatch/available/runners keys off of, plus the two rows this task
// deliberately leaves inert (sync-fleet has no direct key; search has a key
// reserved but arrives fully in Task 15 — this test only pins the shape the
// registry declares, not availability, which is root-private).
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
	if len(Registry()) != len(tests) {
		t.Errorf("Registry() has %d actions, want exactly %d seed rows this task owns", len(Registry()), len(tests))
	}
}
