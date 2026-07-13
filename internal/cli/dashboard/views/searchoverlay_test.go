package views

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/memoryfs"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/search"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/theme"
)

// searchOverlayMemory builds the minimal Memory a search-overlay fixture
// needs: identity fields only — the overlay renders Folder/Provider/Name and
// hands the whole value back through SearchChoiceMsg untouched.
func searchOverlayMemory(folder, name string) memoryfs.Memory {
	return memoryfs.Memory{
		Provider: "claude",
		Folder:   folder,
		RelPath:  name + ".md",
		RepoPath: "claude/" + name + ".md",
		Name:     name,
	}
}

// staticCollect is a Collect fake over a canned fleet listing.
func staticCollect(memories []memoryfs.Memory) func() ([]memoryfs.Memory, error) {
	return func() ([]memoryfs.Memory, error) { return memories, nil }
}

// emptyReadBody is a ReadBody fake for fixtures whose bodies never matter.
func emptyReadBody(memoryfs.Memory) (string, error) { return "", nil }

// newSearchOverlayForTest wires an overlay whose Collect returns an empty
// fleet — enough for every test that exercises input/cursor/render state
// without caring what a query would enumerate; tests that do care build
// their own deps inline.
func newSearchOverlayForTest() *SearchOverlay {
	return NewSearchOverlay(SearchOverlayDeps{
		Collect:  staticCollect(nil),
		ReadBody: emptyReadBody,
		Styles:   theme.Default(true),
	})
}

// deliverHits injects hits as a completed query for the overlay's CURRENT
// generation — the shortcut the render/choice tests use instead of running
// the full type→tick→query pipeline the debounce tests already cover.
func deliverHits(t *testing.T, overlay *SearchOverlay, hits []search.Hit) {
	t.Helper()
	if cmd := overlay.Update(SearchResultsMsg{Generation: overlay.generation, Hits: hits}); cmd != nil {
		t.Fatal("adopting results produced a Cmd; want none")
	}
}

// nameTierHits builds count name-tier hits named memory-01, memory-02, … —
// the display-cap fixtures.
func nameTierHits(count int) []search.Hit {
	hits := make([]search.Hit, 0, count)
	for i := range count {
		name := fmt.Sprintf("memory-%02d", i+1)
		hits = append(hits, search.Hit{
			Memory:   searchOverlayMemory("acme", name),
			Tier:     search.TierName,
			Fragment: name,
		})
	}
	return hits
}

// TestSearchOverlayKeystrokeSchedulesGenerationStampedTick proves the
// debounce is a REAL tea.Tick, not a hand-rolled scheduler the fake
// SearchDebounceMsg values elsewhere in this file could not tell apart:
// executing the keystroke's returned Cmd genuinely waits out the 250ms
// timer and yields a SearchDebounceMsg stamped with the keystroke's own
// generation. The one place this suite pays real wall-clock time.
func TestSearchOverlayKeystrokeSchedulesGenerationStampedTick(t *testing.T) {
	t.Parallel()
	overlay := newSearchOverlayForTest()

	cmd := overlay.Update(key("a"))
	if cmd == nil {
		t.Fatal("a value-changing keystroke returned no Cmd; want the debounce tick")
	}
	if overlay.generation == 0 {
		t.Fatal("a value-changing keystroke did not stamp a fresh generation")
	}

	var stamped *SearchDebounceMsg
	for _, msg := range drain(cmd) {
		if debounce, ok := msg.(SearchDebounceMsg); ok {
			stamped = &debounce
		}
	}
	if stamped == nil {
		t.Fatal("executing the keystroke's Cmd produced no SearchDebounceMsg")
	}
	if stamped.Generation != overlay.generation {
		t.Errorf("tick carries generation %d, want the keystroke's own %d", stamped.Generation, overlay.generation)
	}
}

// TestSearchOverlayStaleTickIgnoredOneQueryWithFinalText is the debounce
// contract end to end: two quick keystrokes each stamp a new generation;
// the first keystroke's (now stale) tick is dropped without querying, the
// second's runs exactly one query — with the FINAL text — and the query
// itself executes inside the returned Cmd, never during Update.
func TestSearchOverlayStaleTickIgnoredOneQueryWithFinalText(t *testing.T) {
	t.Parallel()
	memories := []memoryfs.Memory{
		searchOverlayMemory("acme", "abacus"),
		searchOverlayMemory("acme", "alpha"),
	}
	collectCalls := 0
	overlay := NewSearchOverlay(SearchOverlayDeps{
		Collect: func() ([]memoryfs.Memory, error) {
			collectCalls++
			return memories, nil
		},
		ReadBody: emptyReadBody,
		Styles:   theme.Default(true),
	})

	overlay.Update(key("a"))
	staleGeneration := overlay.generation
	overlay.Update(key("b"))
	if overlay.generation == staleGeneration {
		t.Fatal("the second value-changing keystroke did not advance the generation")
	}

	if cmd := overlay.Update(SearchDebounceMsg{Generation: staleGeneration}); cmd != nil {
		t.Fatal("a stale generation's tick still produced a query Cmd")
	}
	if collectCalls != 0 {
		t.Fatalf("Collect ran %d times off a stale tick; want 0", collectCalls)
	}

	queryCmd := overlay.Update(SearchDebounceMsg{Generation: overlay.generation})
	if queryCmd == nil {
		t.Fatal("the current generation's tick produced no query Cmd")
	}
	if collectCalls != 0 {
		t.Fatal("Update itself ran the query; it must only execute inside the returned Cmd")
	}

	resultMsg := queryCmd()
	if collectCalls != 1 {
		t.Fatalf("Collect calls = %d after running the one current tick's Cmd, want exactly 1", collectCalls)
	}
	results, ok := resultMsg.(SearchResultsMsg)
	if !ok {
		t.Fatalf("query Cmd produced %T, want SearchResultsMsg", resultMsg)
	}
	if results.Err != nil {
		t.Fatalf("query returned err %v", results.Err)
	}

	// The final text is "ab": a fuzzy name match for abacus (a…b) but not
	// for alpha (no b at all) — a query run with just "a" would match both.
	var gotNames []string
	for _, hit := range results.Hits {
		gotNames = append(gotNames, hit.Memory.Name)
	}
	if diff := cmp.Diff([]string{"abacus"}, gotNames); diff != "" {
		t.Errorf("hits for the final text \"ab\" (-want +got):\n%s", diff)
	}

	overlay.Update(results)
	if view := plain(overlay.View(120, 40)); !strings.Contains(view, "abacus") {
		t.Errorf("adopted results are not rendered; view:\n%s", view)
	}
}

// TestSearchOverlayNonValueKeysDoNotRestamp pins the stamp's trigger: only a
// keystroke that actually changes the query's text schedules a new debounce
// generation — cursor movement inside the input and result-list navigation
// must not keep invalidating a tick that would still run the right text.
func TestSearchOverlayNonValueKeysDoNotRestamp(t *testing.T) {
	t.Parallel()
	overlay := newSearchOverlayForTest()
	overlay.Update(key("a"))
	generationAfterTyping := overlay.generation

	for _, msg := range []tea.KeyPressMsg{
		{Code: tea.KeyLeft},
		{Code: tea.KeyRight},
		key("up"),
		key("down"),
	} {
		overlay.Update(msg)
	}
	if overlay.generation != generationAfterTyping {
		t.Errorf("generation = %d after value-preserving keys, want %d unchanged", overlay.generation, generationAfterTyping)
	}
}

// TestSearchOverlayRendersTierTaggedRows pins the row shape (spec §7:
// project · provider · memory · matched fragment): each row carries the
// memory's fleet identity and its fragment tagged with the tier it matched
// at — the body tier including the 1-based matched line.
func TestSearchOverlayRendersTierTaggedRows(t *testing.T) {
	t.Parallel()
	overlay := newSearchOverlayForTest()
	deliverHits(t, overlay, []search.Hit{
		{Memory: searchOverlayMemory("acme", "needle-guide"), Tier: search.TierName, Fragment: "needle-guide"},
		{Memory: searchOverlayMemory("acme", "sewing"), Tier: search.TierDescription, Fragment: "how to thread a needle"},
		{Memory: searchOverlayMemory("zenith", "journal"), Tier: search.TierBody, Fragment: "the needle hides here", Line: 12},
	})

	view := plain(overlay.View(160, 40))
	wantRows := []string{
		"> acme · claude · needle-guide · name: needle-guide",
		"  acme · claude · sewing · description: how to thread a needle",
		"  zenith · claude · journal · body:12: the needle hides here",
	}
	for _, want := range wantRows {
		if !strings.Contains(view, want) {
			t.Errorf("view missing row %q; got:\n%s", want, view)
		}
	}
}

// TestSearchOverlayEnterEmitsChoiceForCursorRow pins enter's emission to the
// CURSOR row — not row zero — and the overlay latching Closed alongside it,
// so the root drops it before the choice message even lands.
func TestSearchOverlayEnterEmitsChoiceForCursorRow(t *testing.T) {
	t.Parallel()
	hits := []search.Hit{
		{Memory: searchOverlayMemory("acme", "first"), Tier: search.TierName, Fragment: "first"},
		{Memory: searchOverlayMemory("zenith", "second"), Tier: search.TierName, Fragment: "second"},
	}
	overlay := newSearchOverlayForTest()
	deliverHits(t, overlay, hits)

	overlay.Update(key("down"))
	cmd := overlay.Update(key("enter"))
	if cmd == nil {
		t.Fatal("enter on a result row produced no Cmd")
	}
	choice, ok := cmd().(SearchChoiceMsg)
	if !ok {
		t.Fatalf("enter's Cmd produced %T, want SearchChoiceMsg", cmd())
	}
	if diff := cmp.Diff(hits[1].Memory, choice.Memory); diff != "" {
		t.Errorf("enter must choose the cursor row's memory (-want +got):\n%s", diff)
	}
	if !overlay.Closed {
		t.Error("choosing a result did not close the overlay")
	}
}

// TestSearchOverlayEnterWithNoResultsDoesNothing: with nothing to choose,
// enter neither closes the overlay nor emits anything.
func TestSearchOverlayEnterWithNoResultsDoesNothing(t *testing.T) {
	t.Parallel()
	overlay := newSearchOverlayForTest()
	if cmd := overlay.Update(key("enter")); cmd != nil {
		t.Error("enter with no results produced a Cmd; want none")
	}
	if overlay.Closed {
		t.Error("enter with no results closed the overlay")
	}
}

// TestSearchOverlayEscClosesAndNothingElse pins esc's whole contract: it
// latches Closed and produces NO Cmd — no PopScreenMsg, no choice — even
// with text already typed (it never merely clears the query the way the
// browser filter's esc does).
func TestSearchOverlayEscClosesAndNothingElse(t *testing.T) {
	t.Parallel()
	overlay := newSearchOverlayForTest()
	overlay.Update(key("a"))

	if cmd := overlay.Update(key("esc")); cmd != nil {
		t.Error("esc produced a Cmd; closing is a latch the root reads, not a message")
	}
	if !overlay.Closed {
		t.Error("esc did not close the overlay")
	}
}

// TestSearchOverlayFindsBodyNeedleAcrossTwoFolders runs the spec §17 seed at
// the overlay level: a body-text needle present in two different projects'
// memories surfaces one row per project, each tagged with its matched body
// line, through the real type→tick→query pipeline.
func TestSearchOverlayFindsBodyNeedleAcrossTwoFolders(t *testing.T) {
	t.Parallel()
	bodies := map[string]string{
		"deploy": "steps\nthe needle hides here\n",
		"notes":  "another needle line\n",
	}
	memories := []memoryfs.Memory{
		searchOverlayMemory("acme", "deploy"),
		searchOverlayMemory("zenith", "notes"),
	}
	overlay := NewSearchOverlay(SearchOverlayDeps{
		Collect: staticCollect(memories),
		ReadBody: func(memory memoryfs.Memory) (string, error) {
			return bodies[memory.Name], nil
		},
		Styles: theme.Default(true),
	})

	for _, r := range "needle" {
		overlay.Update(key(string(r)))
	}
	queryCmd := overlay.Update(SearchDebounceMsg{Generation: overlay.generation})
	if queryCmd == nil {
		t.Fatal("the current generation's tick produced no query Cmd")
	}
	overlay.Update(queryCmd())

	view := plain(overlay.View(160, 40))
	wantRows := []string{
		"acme · claude · deploy · body:2: the needle hides here",
		"zenith · claude · notes · body:1: another needle line",
	}
	for _, want := range wantRows {
		if !strings.Contains(view, want) {
			t.Errorf("view missing cross-project row %q; got:\n%s", want, view)
		}
	}

	// The needle itself must carry spec §7's highlight all the way through
	// the real pipeline — engine-computed span in, Badge-emphasized split
	// out. Asserted on the second row: the first is the cursor row, whose
	// Selected wrap this assertion is not about.
	raw := overlay.View(160, 40)
	styles := theme.Default(true)
	wantSegment := styles.Dim.Render("body:1: another ") + styles.Badge.Render("needle") + styles.Dim.Render(" line")
	if !strings.Contains(raw, wantSegment) {
		t.Errorf("view does not Badge-highlight the engine-reported span; want segment %q in:\n%q", wantSegment, raw)
	}
}

// TestSearchOverlayCapsDisplayWithMoreLine pins the no-silent-truncation
// rule: at most searchDisplayCap rows render, and any hits beyond the cap
// are declared by a "+N more" line — appearing at exactly cap+1 hits, never
// at the cap itself.
func TestSearchOverlayCapsDisplayWithMoreLine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		hitCount     int
		wantMoreLine string
		wantAbsent   string
	}{
		{name: "thirty hits fit with no marker", hitCount: 30},
		{name: "thirty-one hits show thirty plus the marker", hitCount: 31, wantMoreLine: "+1 more", wantAbsent: "memory-31"},
		{name: "eighty hits declare the fifty hidden", hitCount: 80, wantMoreLine: "+50 more", wantAbsent: "memory-31"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			overlay := newSearchOverlayForTest()
			deliverHits(t, overlay, nameTierHits(testCase.hitCount))

			view := plain(overlay.View(120, 60))
			// Each row names its memory once in the identity segment; the
			// fragment repeats the name, so count rows by the segment.
			if got := strings.Count(view, "acme · claude · memory-"); got != min(testCase.hitCount, 30) {
				t.Errorf("rendered %d rows, want %d", got, min(testCase.hitCount, 30))
			}
			if testCase.wantMoreLine == "" {
				if strings.Contains(view, "more") {
					t.Errorf("view shows an overflow marker with nothing hidden:\n%s", view)
				}
				return
			}
			if !strings.Contains(view, testCase.wantMoreLine) {
				t.Errorf("view missing %q — hits beyond the cap were silently truncated:\n%s", testCase.wantMoreLine, view)
			}
			if strings.Contains(view, testCase.wantAbsent) {
				t.Errorf("view renders %q, a row past the display cap:\n%s", testCase.wantAbsent, view)
			}
		})
	}
}

// TestSearchOverlayHeightWindowsRowsAroundCursor pins the height budget: the
// full render never exceeds the given height (chrome + windowed rows +
// marker), and the window follows the cursor so a selection deep in the
// list stays visible instead of walking off-screen.
func TestSearchOverlayHeightWindowsRowsAroundCursor(t *testing.T) {
	t.Parallel()
	overlay := newSearchOverlayForTest()
	deliverHits(t, overlay, nameTierHits(31))

	const height = 12
	view := plain(overlay.View(120, height))
	if lineCount := strings.Count(view, "\n") + 1; lineCount > height {
		t.Errorf("view renders %d lines, over the %d-line budget:\n%s", lineCount, height, view)
	}

	for range 10 {
		overlay.Update(key("down"))
	}
	scrolled := plain(overlay.View(120, height))
	if !strings.Contains(scrolled, "> acme · claude · memory-11") {
		t.Errorf("cursor row memory-11 not visible after scrolling:\n%s", scrolled)
	}
	if strings.Contains(scrolled, "memory-01") {
		t.Errorf("window did not follow the cursor — the top row is still visible:\n%s", scrolled)
	}
	if lineCount := strings.Count(scrolled, "\n") + 1; lineCount > height {
		t.Errorf("scrolled view renders %d lines, over the %d-line budget:\n%s", lineCount, height, scrolled)
	}
}

// TestSearchOverlayHeightFloorKeepsOneRow states the budget's floor
// honestly: below the ~9 lines the chrome itself needs, the overlay keeps
// the cursor's own row plus the overflow marker and overflows the
// impossible budget — the same degrade-don't-blank rule the browser and
// reading screens apply.
func TestSearchOverlayHeightFloorKeepsOneRow(t *testing.T) {
	t.Parallel()
	overlay := newSearchOverlayForTest()
	deliverHits(t, overlay, nameTierHits(31))

	view := plain(overlay.View(120, 1))
	if !strings.Contains(view, "> acme · claude · memory-01") {
		t.Errorf("floor did not keep the cursor row visible:\n%s", view)
	}
	if lineCount := strings.Count(view, "\n") + 1; lineCount > 9 {
		t.Errorf("floor view renders %d lines, want at most 9 (chrome + one row + marker):\n%s", lineCount, view)
	}
}

// TestSearchOverlayCollectErrorSurfaces: a fleet-enumeration failure renders
// verbatim instead of masquerading as an empty result list.
func TestSearchOverlayCollectErrorSurfaces(t *testing.T) {
	t.Parallel()
	overlay := NewSearchOverlay(SearchOverlayDeps{
		Collect:  func() ([]memoryfs.Memory, error) { return nil, errors.New("boom") },
		ReadBody: emptyReadBody,
		Styles:   theme.Default(true),
	})
	overlay.Update(key("a"))
	queryCmd := overlay.Update(SearchDebounceMsg{Generation: overlay.generation})
	if queryCmd == nil {
		t.Fatal("the current generation's tick produced no query Cmd")
	}
	overlay.Update(queryCmd())

	if view := plain(overlay.View(120, 40)); !strings.Contains(view, "search unavailable: boom") {
		t.Errorf("Collect failure not surfaced; view:\n%s", view)
	}
}

// TestSearchOverlayStaleResultsDropped covers the results side of the
// generation guard: a query that completes after a newer keystroke has
// already superseded it must not clobber the overlay's state.
func TestSearchOverlayStaleResultsDropped(t *testing.T) {
	t.Parallel()
	overlay := newSearchOverlayForTest()
	overlay.Update(key("a"))
	staleResults := SearchResultsMsg{
		Generation: overlay.generation,
		Hits:       nameTierHits(3),
	}
	overlay.Update(key("b")) // supersedes the in-flight query

	if cmd := overlay.Update(staleResults); cmd != nil {
		t.Error("dropping stale results produced a Cmd; want none")
	}
	if len(overlay.hits) != 0 {
		t.Errorf("stale results were adopted: %d hits", len(overlay.hits))
	}
}

// TestSearchOverlayNoMatchNotice separates "answered, nothing matched" from
// the blank instant before the first query completes: only the former earns
// the notice.
func TestSearchOverlayNoMatchNotice(t *testing.T) {
	t.Parallel()
	overlay := newSearchOverlayForTest()
	overlay.Update(key("z"))

	if view := plain(overlay.View(120, 40)); strings.Contains(view, "no memories match") {
		t.Errorf("no-match notice shown before any query answered:\n%s", view)
	}

	queryCmd := overlay.Update(SearchDebounceMsg{Generation: overlay.generation})
	if queryCmd == nil {
		t.Fatal("the current generation's tick produced no query Cmd")
	}
	overlay.Update(queryCmd())
	if view := plain(overlay.View(120, 40)); !strings.Contains(view, "no memories match") {
		t.Errorf("an answered empty query shows no notice:\n%s", view)
	}
}

// TestSearchOverlayHighlightsMatchedSpanWithinFragment pins spec §7's match
// highlight to the exact runes Hit.MatchStart/MatchEnd address: the row's
// fragment must render as a dim tag+prefix, the Badge-emphasized match, and
// a dim suffix — byte-for-byte, computed through the same theme styles the
// overlay holds, so a highlight that drifts even one rune off the span (or
// skips the split entirely) fails. Asserted on a NON-cursor row: the cursor
// row additionally wraps in Selected, which would bury the segment
// boundaries this test is about.
func TestSearchOverlayHighlightsMatchedSpanWithinFragment(t *testing.T) {
	t.Parallel()
	overlay := newSearchOverlayForTest()
	deliverHits(t, overlay, []search.Hit{
		{Memory: searchOverlayMemory("acme", "apple"), Tier: search.TierName, Fragment: "apple", MatchStart: 0, MatchEnd: 1},
		{Memory: searchOverlayMemory("zenith", "guide"), Tier: search.TierBody, Fragment: "the needle hides here", MatchStart: 4, MatchEnd: 10, Line: 2},
	})

	raw := overlay.View(120, 40)
	styles := theme.Default(true)
	want := styles.Dim.Render("body:2: the ") + styles.Badge.Render("needle") + styles.Dim.Render(" hides here")
	if !strings.Contains(raw, want) {
		t.Errorf("view does not render the fragment as dim/Badge/dim split exactly at the match span\nwant segment: %q\nview:\n%q", want, raw)
	}
}
