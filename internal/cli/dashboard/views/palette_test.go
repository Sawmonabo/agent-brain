package views

import (
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/theme"
)

// alwaysAvailable is the palette's available predicate for tests that do not
// care about availability gating.
func alwaysAvailable(string) bool { return true }

// newPalette builds a PaletteModel for tests that do not care about the
// textinput focus/blink Cmd NewPaletteModel returns alongside it.
func newPalette(available func(string) bool, quiescedNow bool) PaletteModel {
	p, _ := NewPaletteModel(theme.Default(true), available, quiescedNow)
	return p
}

// typeString feeds one key press per rune through a PaletteModel, the same
// way a running program delivers a fast typist's keystrokes one at a time.
func typeString(p PaletteModel, s string) PaletteModel {
	for _, r := range s {
		p, _ = p.Update(key(string(r)))
	}
	return p
}

// TestPaletteFiltersAndChooses pins the palette's core contract: typing
// filters the list, up/down moves the cursor, enter surfaces the highlighted
// row's ID as a PaletteChoiceMsg (the ONLY thing dispatch consumes), and esc
// closes with no choice at all. "sync" prefix-matches both sync-project and
// sync-fleet titles/IDs equally, so both tie at the best rank; the stable
// sort within a tie preserves registry order (sync-project, then
// sync-fleet), which is what makes "cursor down once" land on sync-fleet.
func TestPaletteFiltersAndChooses(t *testing.T) {
	t.Parallel()

	p := newPalette(alwaysAvailable, false)
	p = typeString(p, "sync")
	p, cmd := p.Update(key("down"))
	if cmd != nil {
		t.Fatal("moving the cursor produced a Cmd; want none")
	}
	p, cmd = p.Update(key("enter"))
	if !p.Closed {
		t.Fatal("enter did not close the palette")
	}
	if cmd == nil {
		t.Fatal("enter produced no Cmd")
	}
	choice, ok := cmd().(PaletteChoiceMsg)
	if !ok {
		t.Fatalf("Cmd produced %T, want PaletteChoiceMsg", cmd())
	}
	if choice.ID != "sync-fleet" {
		t.Errorf("choice.ID = %q, want %q", choice.ID, "sync-fleet")
	}

	esc := newPalette(alwaysAvailable, false)
	esc, cmd = esc.Update(key("esc"))
	if !esc.Closed {
		t.Fatal("esc did not close the palette")
	}
	if cmd != nil {
		t.Error("esc produced a Cmd; want none — no choice was made")
	}
}

// TestPaletteEnterWithNoMatchesDoesNothing guards the empty-results edge:
// pressing enter when the query matched nothing must not panic on an
// out-of-range cursor index and must not close the palette (there is
// nothing to choose).
func TestPaletteEnterWithNoMatchesDoesNothing(t *testing.T) {
	t.Parallel()
	p := newPalette(alwaysAvailable, false)
	p = typeString(p, "zzzznomatch")
	p, cmd := p.Update(key("enter"))
	if p.Closed {
		t.Error("enter with no matches closed the palette; want it to stay open")
	}
	if cmd != nil {
		t.Error("enter with no matches produced a Cmd; want none")
	}
}

// TestPaletteHidesUnavailableActions pins the availability gate: an action
// with no registered runner (search, until Task 15) must never appear in the
// palette, even though it is declared in Registry() — the help overlay is
// the only surface that lists it regardless (TestHelpListsEveryRegisteredAction).
func TestPaletteHidesUnavailableActions(t *testing.T) {
	t.Parallel()
	available := func(id string) bool { return id != "search" }
	p := newPalette(available, false)

	got := plain(p.View())
	if strings.Contains(got, "search") {
		t.Errorf("palette view %q lists the unavailable search action", got)
	}
	if !strings.Contains(got, "sync fleet") {
		t.Errorf("palette view %q missing an available action (sync fleet)", got)
	}
}

// TestPaletteGreysMutatingActionsWhileQuiesced pins the "greyed" half of
// spec §15's Mutates contract: while quiesced, a mutating row still appears
// (choosing it gets a real refusal + toast from dispatch, not silence) but
// is visibly marked so the user is not surprised.
func TestPaletteGreysMutatingActionsWhileQuiesced(t *testing.T) {
	t.Parallel()
	notQuiesced := newPalette(alwaysAvailable, false)
	if got := plain(notQuiesced.View()); strings.Contains(got, "quiesced") {
		t.Errorf("view %q marks an action quiesced when the daemon is not", got)
	}

	quiesced := newPalette(alwaysAvailable, true)
	got := plain(quiesced.View())
	if !strings.Contains(got, "quiesced") {
		t.Errorf("view %q does not grey any Mutates action while quiesced", got)
	}
	// select/switch-tabs never mutate, so they must render unmarked even
	// while quiesced.
	for line := range strings.SplitSeq(got, "\n") {
		if strings.Contains(line, "select") && strings.Contains(line, "quiesced") {
			t.Errorf("non-mutating action line %q marked quiesced", line)
		}
	}
}

// TestPaletteSetAvailableRefreshesFiltering pins the fix for the frozen-
// predicate hazard: the root's paletteAvailable is a bound method value on
// a value-semantics Model, so the copy captured once at NewPaletteModel
// construction time would otherwise never see any later change — harmless
// today only because nothing paletteAvailable reads is itself mutable while
// the palette is open. SetAvailable is how the root re-binds it on every
// forwarded keypress; refilter (which every real keystroke triggers) must
// honor whatever was most recently bound, not the original.
func TestPaletteSetAvailableRefreshesFiltering(t *testing.T) {
	t.Parallel()
	p := newPalette(alwaysAvailable, false)
	if got := plain(p.View()); !strings.Contains(got, "sync fleet") {
		t.Fatalf("setup: view %q should list sync fleet before narrowing availability", got)
	}

	hideSyncFleet := func(id string) bool { return id != "sync-fleet" }
	p.SetAvailable(hideSyncFleet)
	p.refilter()

	if got := plain(p.View()); strings.Contains(got, "sync fleet") {
		t.Errorf("view %q still lists sync fleet after SetAvailable rebound the predicate", got)
	}
}

// TestPaletteTypingIsNeverInterceptedAsNavigation guards the reason the
// palette cannot reuse DashboardKeys.Select (up/down/k/j): j and k must stay
// literal characters while a query is being typed, unlike the add picker's
// pure list-nav modal which never carries free text.
func TestPaletteTypingIsNeverInterceptedAsNavigation(t *testing.T) {
	t.Parallel()
	p := newPalette(alwaysAvailable, false)
	p = typeString(p, "jk")
	if got := p.input.Value(); got != "jk" {
		t.Errorf("input value = %q, want %q (j/k must type, not move the cursor)", got, "jk")
	}
}
