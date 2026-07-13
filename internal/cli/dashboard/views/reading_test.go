package views

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/links"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/memoryfs"
)

// readingFixtureBodies is the canonical body set the reading fixtures share:
// alpha carries three resolvable links (beta, gamma, delta — cycling order),
// beta and gamma each link back to alpha (its backlinks), delta links
// nowhere.
func readingFixtureBodies() map[string]string {
	return map[string]string{
		"claude/alpha.md": "intro [[beta]] then [[gamma]] then [[delta]] outro\n",
		"claude/beta.md":  "back to [[alpha]]\n",
		"claude/gamma.md": "see [[alpha]] again\n",
		"claude/delta.md": "no links here\n",
	}
}

// readingFixtureMemories builds the four in-memory fixtures behind
// readingFixtureBodies — no real files: ReadBody is injected everywhere the
// reading screen reads, so the whole battery stays filesystem-free except
// where a test deliberately wants a real path.
func readingFixtureMemories() []memoryfs.Memory {
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	names := []string{"alpha", "beta", "gamma", "delta"}
	memories := make([]memoryfs.Memory, len(names))
	for i, name := range names {
		memories[i] = memoryfs.Memory{
			Provider: "claude",
			Folder:   "acme",
			LocalDir: "/enrolled/acme/claude",
			RelPath:  name + ".md",
			RepoPath: "claude/" + name + ".md",
			Name:     strings.ToUpper(name[:1]) + name[1:],
			ModTime:  base.Add(time.Duration(i) * time.Hour),
			Size:     int64(len(readingFixtureBodies()["claude/"+name+".md"])),
		}
	}
	return memories
}

// newReadingFixture builds a Reading over alpha — the fixture memory whose
// body carries the links — with the shared link index and body set,
// applying overrides to the deps before construction. Tests that read a
// different memory (or a different body set) construct their ReadingDeps
// directly instead.
func newReadingFixture(t *testing.T, override func(*ReadingDeps)) *Reading {
	t.Helper()
	memories := readingFixtureMemories()
	readBody := fakeReadBody(readingFixtureBodies())
	deps := ReadingDeps{
		Memory:   memories[0],
		Index:    links.BuildIndex(memories, readBody),
		ReadBody: readBody,
	}
	if override != nil {
		override(&deps)
	}
	return NewReading(deps)
}

// shiftTab builds the KeyPressMsg a real shift+tab press produces: Code
// KeyTab with the shift modifier and no Text, which String()s to
// "shift+tab" (verified against ultraviolet's Key.Keystroke, the encoder
// bubbletea v2.0.8 delegates to).
func shiftTab() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
}

// TestReadingHeaderLine pins spec §4's header line: name · class · absolute
// modified time · human size, with the body rendered through the injected
// Render seam underneath.
func TestReadingHeaderLine(t *testing.T) {
	t.Parallel()
	reading := newReadingFixture(t, func(deps *ReadingDeps) {
		deps.Render = func(md string, _ int) string { return "RENDERED:" + md }
	})

	got := plain(reading.View(120, 30))
	for _, want := range []string{
		"Alpha",
		"fact",
		"modified 2026-07-01 12:00",
		"51 B",
		"RENDERED:intro",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("view missing %q; got:\n%s", want, got)
		}
	}
}

// TestReadingTitleIsMemoryName pins the breadcrumb segment contract.
func TestReadingTitleIsMemoryName(t *testing.T) {
	t.Parallel()
	reading := newReadingFixture(t, nil)
	if got := reading.Title(); got != "Alpha" {
		t.Errorf("Title() = %q, want %q", got, "Alpha")
	}
}

// TestReadingLoadErrorSurfaces covers a memory whose body cannot be read at
// construction (vanished mid-browse, or over memoryfs's size cap): the
// screen must show the error, never a blank viewport or a panic.
func TestReadingLoadErrorSurfaces(t *testing.T) {
	t.Parallel()
	reading := newReadingFixture(t, func(deps *ReadingDeps) {
		deps.ReadBody = func(memoryfs.Memory) (string, error) {
			return "", errors.New("boom: file exceeds the read cap")
		}
	})

	got := plain(reading.View(120, 30))
	if !strings.Contains(got, "boom: file exceeds the read cap") {
		t.Errorf("load-error view missing the error text; got:\n%s", got)
	}
}

// TestReadingLinkCycling pins tab/shift+tab link navigation over alpha's
// three resolvable links: no link is active at construction, tab advances
// in body order and wraps past the last link, shift+tab reverses and wraps
// from the first back to the last — with the active link visible in the
// display body as its ▶target◀ substitution.
func TestReadingLinkCycling(t *testing.T) {
	t.Parallel()
	reading := newReadingFixture(t, nil)

	if got := plain(reading.View(120, 30)); strings.Contains(got, "▶") {
		t.Fatalf("no link may be active before the first tab; got:\n%s", got)
	}

	steps := []struct {
		key        tea.KeyPressMsg
		wantActive string
	}{
		{key("tab"), "▶beta◀"},
		{key("tab"), "▶gamma◀"},
		{key("tab"), "▶delta◀"},
		{key("tab"), "▶beta◀"},  // wraps past the last link
		{shiftTab(), "▶delta◀"}, // reverses, wrapping from the first
		{shiftTab(), "▶gamma◀"},
	}
	for i, step := range steps {
		next, _ := reading.Update(step.key)
		reading = next.(*Reading)
		got := plain(reading.View(120, 30))
		if !strings.Contains(got, step.wantActive) {
			t.Fatalf("step %d: view missing active marker %q; got:\n%s", i, step.wantActive, got)
		}
		if strings.Count(got, "▶") != 1 {
			t.Fatalf("step %d: want exactly one active link, got %d in:\n%s", i, strings.Count(got, "▶"), got)
		}
	}
}

// TestReadingInactiveResolvedLinksStayVerbatim pins the substitution's
// leave-alone half: a resolved link that is not the active one renders as
// its original [[target]] span, so the reader can still see every link.
func TestReadingInactiveResolvedLinksStayVerbatim(t *testing.T) {
	t.Parallel()
	reading := newReadingFixture(t, nil)
	next, _ := reading.Update(key("tab")) // beta active; gamma/delta inactive
	reading = next.(*Reading)

	got := plain(reading.View(120, 30))
	for _, want := range []string{"[[gamma]]", "[[delta]]"} {
		if !strings.Contains(got, want) {
			t.Errorf("inactive resolved link %q not left verbatim; got:\n%s", want, got)
		}
	}
}

// TestReadingEnterOnResolvedLinkPushesReading pins the navigation stack jump:
// enter with a resolved link active produces a PushScreenMsg carrying a new
// *Reading for the resolved memory, built over the SAME shared index — never
// a rebuilt one.
func TestReadingEnterOnResolvedLinkPushesReading(t *testing.T) {
	t.Parallel()
	reading := newReadingFixture(t, nil)

	// enter with no active link is inert — there is nothing to follow.
	if _, cmd := reading.Update(key("enter")); cmd != nil {
		t.Fatalf("enter with no active link produced a message: %#v", cmd())
	}

	next, _ := reading.Update(key("tab")) // beta active
	reading = next.(*Reading)
	_, cmd := reading.Update(key("enter"))
	if cmd == nil {
		t.Fatal("enter on a resolved link produced no Cmd")
	}
	push, ok := cmd().(PushScreenMsg)
	if !ok {
		t.Fatalf("enter on a resolved link produced %#v, want PushScreenMsg", cmd())
	}
	pushed, ok := push.Screen.(*Reading)
	if !ok {
		t.Fatalf("pushed screen is %T, want *Reading", push.Screen)
	}
	if pushed.deps.Memory.RepoPath != "claude/beta.md" {
		t.Errorf("pushed reading is for %q, want %q", pushed.deps.Memory.RepoPath, "claude/beta.md")
	}
	if pushed.deps.Index != reading.deps.Index {
		t.Error("pushed reading rebuilt the link index instead of sharing the browser's")
	}
	if got := plain(pushed.View(120, 30)); !strings.Contains(got, "back to [[alpha]]") {
		t.Errorf("pushed reading did not load the target's body; got:\n%s", got)
	}
}

// TestReadingDanglingLink pins both dangling behaviors: the display body
// renders the unresolved target struck-through (GFM ~~strikethrough~~, so
// the themed glamour renderer styles it) with the " (dangling)" marker, and
// enter on it produces a ToastMsg — never a push.
func TestReadingDanglingLink(t *testing.T) {
	t.Parallel()
	bodies := readingFixtureBodies()
	bodies["claude/alpha.md"] = "see [[ghost]] for details\n"
	memories := readingFixtureMemories()
	readBody := fakeReadBody(bodies)
	reading := NewReading(ReadingDeps{
		Memory:   memories[0],
		Index:    links.BuildIndex(memories, readBody),
		ReadBody: readBody,
	})

	got := plain(reading.View(120, 30))
	if !strings.Contains(got, "~~ghost~~ (dangling)") {
		t.Fatalf("dangling link not struck through with the marker; got:\n%s", got)
	}

	next, _ := reading.Update(key("tab")) // the dangling link is the only one
	reading = next.(*Reading)
	if got := plain(reading.View(120, 30)); !strings.Contains(got, "▶~~ghost~~ (dangling)◀") {
		t.Fatalf("active dangling link missing its combined marker; got:\n%s", got)
	}

	_, cmd := reading.Update(key("enter"))
	if cmd == nil {
		t.Fatal("enter on a dangling link produced no Cmd; want a ToastMsg")
	}
	msg := cmd()
	if _, isPush := msg.(PushScreenMsg); isPush {
		t.Fatal("enter on a dangling link pushed a screen; want a toast only")
	}
	toast, ok := msg.(ToastMsg)
	if !ok {
		t.Fatalf("enter on a dangling link produced %#v, want ToastMsg", msg)
	}
	if !strings.Contains(toast.Text, "ghost") {
		t.Errorf("toast %q does not name the dangling target", toast.Text)
	}
}

// TestReadingBacklinksPanel pins the b toggle: the panel lists referrers by
// name (alpha's backlinks: Beta and Gamma, index-sorted), tab moves the
// shared cursor within it, enter jumps to the selected referrer's reading
// view, and b again closes the panel.
func TestReadingBacklinksPanel(t *testing.T) {
	t.Parallel()
	reading := newReadingFixture(t, nil)

	if got := plain(reading.View(120, 30)); strings.Contains(got, "Backlinks") {
		t.Fatalf("backlinks panel visible before b; got:\n%s", got)
	}

	next, _ := reading.Update(key("b"))
	reading = next.(*Reading)
	got := plain(reading.View(120, 30))
	for _, want := range []string{"Backlinks", "Beta", "Gamma"} {
		if !strings.Contains(got, want) {
			t.Fatalf("open panel missing %q; got:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "> Beta") {
		t.Fatalf("panel cursor not on the first referrer; got:\n%s", got)
	}

	next, _ = reading.Update(key("tab"))
	reading = next.(*Reading)
	if got := plain(reading.View(120, 30)); !strings.Contains(got, "> Gamma") {
		t.Fatalf("tab did not advance the panel cursor; got:\n%s", got)
	}

	_, cmd := reading.Update(key("enter"))
	if cmd == nil {
		t.Fatal("enter on a backlink row produced no Cmd")
	}
	push, ok := cmd().(PushScreenMsg)
	if !ok {
		t.Fatalf("enter on a backlink row produced %#v, want PushScreenMsg", cmd())
	}
	pushed, ok := push.Screen.(*Reading)
	if !ok {
		t.Fatalf("pushed screen is %T, want *Reading", push.Screen)
	}
	if pushed.deps.Memory.RepoPath != "claude/gamma.md" {
		t.Errorf("backlink jump pushed %q, want %q", pushed.deps.Memory.RepoPath, "claude/gamma.md")
	}

	next, _ = reading.Update(key("b"))
	reading = next.(*Reading)
	if got := plain(reading.View(120, 30)); strings.Contains(got, "Backlinks") {
		t.Errorf("b did not close the panel; got:\n%s", got)
	}
}

// TestReadingBacklinksPanelEmpty covers a memory nothing links to: the open
// panel must say so rather than render an empty hole. Needs its own body
// set — in the canonical fixture every memory has at least one referrer
// (alpha itself links to delta).
func TestReadingBacklinksPanelEmpty(t *testing.T) {
	t.Parallel()
	bodies := map[string]string{
		"claude/alpha.md": "no links at all\n",
		"claude/beta.md":  "still none\n",
		"claude/gamma.md": "none here either\n",
		"claude/delta.md": "and none here\n",
	}
	memories := readingFixtureMemories()
	readBody := fakeReadBody(bodies)
	reading := NewReading(ReadingDeps{
		Memory:   memories[3],
		Index:    links.BuildIndex(memories, readBody),
		ReadBody: readBody,
	})
	next, _ := reading.Update(key("b"))
	reading = next.(*Reading)
	if got := plain(reading.View(120, 30)); !strings.Contains(got, "no memories link here") {
		t.Errorf("empty panel missing its guidance line; got:\n%s", got)
	}
}

// TestReadingEscClosesBacklinksThenPops pins the Screen contract's
// consumption rule: esc with the backlinks panel open closes it and
// produces no pop; the next esc, with nothing left to close, pops.
func TestReadingEscClosesBacklinksThenPops(t *testing.T) {
	t.Parallel()
	reading := newReadingFixture(t, nil)
	next, _ := reading.Update(key("b"))
	reading = next.(*Reading)

	next, cmd := reading.Update(key("esc"))
	reading = next.(*Reading)
	if cmd != nil {
		if _, isPop := cmd().(PopScreenMsg); isPop {
			t.Fatal("esc that closed the backlinks panel must not also signal a pop")
		}
	}
	if got := plain(reading.View(120, 30)); strings.Contains(got, "Backlinks") {
		t.Fatalf("esc did not close the backlinks panel; got:\n%s", got)
	}

	_, cmd = reading.Update(key("esc"))
	if cmd == nil {
		t.Fatal("esc with nothing open must signal a pop")
	}
	if _, isPop := cmd().(PopScreenMsg); !isPop {
		t.Fatal("esc with nothing open did not produce a PopScreenMsg")
	}
}

// TestReadingCopyPathEmitsAbsolutePath pins y: a CopyPathMsg carrying the
// memory's absolute provider-file path (LocalDir joined with RelPath).
func TestReadingCopyPathEmitsAbsolutePath(t *testing.T) {
	t.Parallel()
	reading := newReadingFixture(t, nil)
	_, cmd := reading.Update(key("y"))
	if cmd == nil {
		t.Fatal("y produced no Cmd")
	}
	copyPath, ok := cmd().(CopyPathMsg)
	if !ok {
		t.Fatalf("y produced %#v, want CopyPathMsg", cmd())
	}
	want := filepath.Join("/enrolled/acme/claude", "alpha.md")
	if copyPath.Path != want {
		t.Errorf("CopyPathMsg.Path = %q, want %q", copyPath.Path, want)
	}
}

// hundredLineBody builds a body of uniquely named rows ("row 001" …
// "row 100") so scroll assertions can pick individual lines without prefix
// collisions.
func hundredLineBody() string {
	var b strings.Builder
	for i := 1; i <= 100; i++ {
		fmt.Fprintf(&b, "row %03d\n", i)
	}
	return b.String()
}

// TestReadingViewportScroll is the brief's scroll smoke: on a 100-line body
// at height 20, g/G move between the top and bottom slices, and j advances
// one line.
func TestReadingViewportScroll(t *testing.T) {
	t.Parallel()
	bodies := readingFixtureBodies()
	bodies["claude/alpha.md"] = hundredLineBody()
	memories := readingFixtureMemories()
	readBody := fakeReadBody(bodies)
	reading := NewReading(ReadingDeps{
		Memory:   memories[0],
		Index:    links.BuildIndex(memories, readBody),
		ReadBody: readBody,
	})

	top := plain(reading.View(80, 20))
	if !strings.Contains(top, "row 001") {
		t.Fatalf("initial view missing the top of the body; got:\n%s", top)
	}
	if strings.Contains(top, "row 099") {
		t.Fatalf("initial view already shows the bottom of the body; got:\n%s", top)
	}

	next, _ := reading.Update(key("G"))
	reading = next.(*Reading)
	bottom := plain(reading.View(80, 20))
	if !strings.Contains(bottom, "row 100") {
		t.Fatalf("G did not scroll to the bottom; got:\n%s", bottom)
	}
	if strings.Contains(bottom, "row 001") {
		t.Fatalf("G still shows the top of the body; got:\n%s", bottom)
	}

	next, _ = reading.Update(key("g"))
	reading = next.(*Reading)
	if got := plain(reading.View(80, 20)); !strings.Contains(got, "row 001") {
		t.Fatalf("g did not scroll back to the top; got:\n%s", got)
	}

	next, _ = reading.Update(key("j"))
	reading = next.(*Reading)
	stepped := plain(reading.View(80, 20))
	if strings.Contains(stepped, "row 001") {
		t.Errorf("j did not advance past the first line; got:\n%s", stepped)
	}
	if !strings.Contains(stepped, "row 002") {
		t.Errorf("j scrolled past more than one line; got:\n%s", stepped)
	}
}

// TestReadingTabScrollsActiveLinkIntoView pins that cycling to a link far
// below the fold brings its line into the viewport — an active-link
// highlight the user cannot see would make tab look broken.
func TestReadingTabScrollsActiveLinkIntoView(t *testing.T) {
	t.Parallel()
	bodies := readingFixtureBodies()
	bodies["claude/alpha.md"] = hundredLineBody() + "final [[beta]] link\n"
	memories := readingFixtureMemories()
	readBody := fakeReadBody(bodies)
	reading := NewReading(ReadingDeps{
		Memory:   memories[0],
		Index:    links.BuildIndex(memories, readBody),
		ReadBody: readBody,
	})

	if got := plain(reading.View(80, 20)); strings.Contains(got, "[[beta]]") {
		t.Fatal("setup: the link must start below the fold")
	}

	next, _ := reading.Update(key("tab"))
	reading = next.(*Reading)
	if got := plain(reading.View(80, 20)); !strings.Contains(got, "▶beta◀") {
		t.Errorf("tab did not scroll the active link into view; got:\n%s", got)
	}
}

// TestReadingRefreshAdoptsExternalRewrite pins the drill-in liveness
// contract (screen.go's RefreshMsg): a body rewritten externally is adopted
// on the next tick — content and links both — while a refresh that reads
// the identical body changes nothing.
func TestReadingRefreshAdoptsExternalRewrite(t *testing.T) {
	t.Parallel()
	memories := readingFixtureMemories()
	body := "first draft [[beta]]\n"
	readBody := func(m memoryfs.Memory) (string, error) {
		if m.RepoPath == "claude/alpha.md" {
			return body, nil
		}
		return readingFixtureBodies()[m.RepoPath], nil
	}
	reading := NewReading(ReadingDeps{
		Memory:   memories[0],
		Index:    links.BuildIndex(memories, readBody),
		ReadBody: readBody,
	})

	if got := plain(reading.View(120, 30)); !strings.Contains(got, "first draft") {
		t.Fatalf("initial view missing the first body; got:\n%s", got)
	}

	body = "second draft, links gone\n"
	next, cmd := reading.Update(RefreshMsg{Now: time.Now()})
	reading = next.(*Reading)
	if cmd != nil {
		t.Fatal("RefreshMsg produced a Cmd; want none")
	}

	got := plain(reading.View(120, 30))
	if !strings.Contains(got, "second draft, links gone") {
		t.Fatalf("refresh did not adopt the rewritten body; got:\n%s", got)
	}
	if strings.Contains(got, "first draft") {
		t.Fatalf("refresh left the stale body visible; got:\n%s", got)
	}

	// The old body's link is gone with it: tab must find nothing to activate.
	next, _ = reading.Update(key("tab"))
	reading = next.(*Reading)
	if got := plain(reading.View(120, 30)); strings.Contains(got, "▶") {
		t.Errorf("tab activated a link parsed from the stale body; got:\n%s", got)
	}
}

// TestReadingRefreshReadErrorKeepsBody pins the degraded half of the
// liveness contract: a read failure on a later tick (file deleted, or a
// transient error) must not blank an open document — the last good body
// stays readable.
func TestReadingRefreshReadErrorKeepsBody(t *testing.T) {
	t.Parallel()
	memories := readingFixtureMemories()
	readErr := error(nil)
	readBody := func(m memoryfs.Memory) (string, error) {
		if readErr != nil {
			return "", readErr
		}
		return readingFixtureBodies()[m.RepoPath], nil
	}
	reading := NewReading(ReadingDeps{
		Memory:   memories[0],
		Index:    links.BuildIndex(memories, readBody),
		ReadBody: readBody,
	})

	readErr = errors.New("open alpha.md: no such file or directory")
	next, _ := reading.Update(RefreshMsg{Now: time.Now()})
	reading = next.(*Reading)

	if got := plain(reading.View(120, 30)); !strings.Contains(got, "intro") {
		t.Errorf("a refresh read error blanked the open document; got:\n%s", got)
	}
}

// TestReadingRenderIsCached pins the render cache: View runs on every
// keypress and every ~2s RefreshMsg tick, and a glamour render over a full
// body is real cost — so an unchanged (body, width, active link) render
// must not re-run the Render seam, while each of the three changing forces
// exactly one more run, and SetRender invalidates unconditionally.
func TestReadingRenderIsCached(t *testing.T) {
	t.Parallel()
	var renderCalls int
	reading := newReadingFixture(t, func(deps *ReadingDeps) {
		deps.Render = func(md string, _ int) string {
			renderCalls++
			return md
		}
	})

	_ = reading.View(120, 30)
	if renderCalls != 1 {
		t.Fatalf("renderCalls = %d after the first View, want 1", renderCalls)
	}
	_ = reading.View(120, 30)
	if renderCalls != 1 {
		t.Errorf("renderCalls = %d after an identical View, want still 1 (cache hit)", renderCalls)
	}

	_ = reading.View(100, 30)
	if renderCalls != 2 {
		t.Errorf("renderCalls = %d after the width changed, want 2", renderCalls)
	}

	next, _ := reading.Update(key("tab")) // active link changes the display body
	reading = next.(*Reading)
	_ = reading.View(100, 30)
	if renderCalls != 3 {
		t.Errorf("renderCalls = %d after the active link changed, want 3", renderCalls)
	}

	next, _ = reading.Update(RefreshMsg{Now: time.Now()}) // body unchanged
	reading = next.(*Reading)
	_ = reading.View(100, 30)
	if renderCalls != 3 {
		t.Errorf("renderCalls = %d after a no-op refresh, want still 3 (cache hit)", renderCalls)
	}

	var swappedCalls int
	reading.SetRender(func(md string, _ int) string {
		swappedCalls++
		return md
	})
	_ = reading.View(100, 30)
	if swappedCalls != 1 {
		t.Errorf("swappedCalls = %d after SetRender, want 1 (setter must invalidate unconditionally)", swappedCalls)
	}
}

// newReadingWithBody builds a Reading over alpha with the given body,
// optionally with the backlinks panel opened. Alpha's two referrers (Beta
// and Gamma) come from the OTHER fixture bodies, so overriding alpha's own
// body never changes the panel's three lines (title + 2 rows).
func newReadingWithBody(t *testing.T, body string, backlinksOpen bool) *Reading {
	t.Helper()
	bodies := readingFixtureBodies()
	bodies["claude/alpha.md"] = body
	memories := readingFixtureMemories()
	readBody := fakeReadBody(bodies)
	reading := NewReading(ReadingDeps{
		Memory:   memories[0],
		Index:    links.BuildIndex(memories, readBody),
		ReadBody: readBody,
	})
	if backlinksOpen {
		next, _ := reading.Update(key("b"))
		reading = next.(*Reading)
	}
	return reading
}

// newHundredLineReading builds a Reading over the 100-line alpha body —
// the shared fixture for the height and half-page assertions below —
// optionally with the backlinks panel opened.
func newHundredLineReading(t *testing.T, backlinksOpen bool) *Reading {
	t.Helper()
	return newReadingWithBody(t, hundredLineBody(), backlinksOpen)
}

// TestReadingViewFillsHeightExactlyAtAndAboveTheChromeFloor pins the height
// contract in BOTH directions — never more than height lines, and never a
// wasted row either. The viewport pads its content area to its full height
// with space-FILLED lines (lipgloss width-aligns the height padding), which
// survive View's trailing newline trim — so at or above the screen's honest
// chrome floor View renders EXACTLY height lines for short bodies too, not
// only ones that over-fill the viewport; the short-body rows pin that half.
// The floor is chrome+1: header line + one blank (2 lines) with the panel
// closed, plus the panel's own lines and the ONE blank line its "\n\n"
// separator opens when it is open — with this fixture's two referrers the
// panel is 3 lines (title + 2 rows), so the open floor is 2+3+1+1 = 7.
func TestReadingViewFillsHeightExactlyAtAndAboveTheChromeFloor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		height        int
		backlinksOpen bool
		shortBody     bool
	}{
		{"panel closed, at the floor", 3, false, false},
		{"panel closed, one above the floor", 4, false, false},
		{"panel closed, roomy", 20, false, false},
		{"panel closed, roomy, short body", 20, false, true},
		{"panel open, at the floor", 7, true, false},
		{"panel open, one above the floor", 8, true, false},
		{"panel open, roomy", 20, true, false},
		{"panel open, roomy, short body", 20, true, true},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			body := hundredLineBody()
			if testCase.shortBody {
				body = "short body line one\nshort body line two"
			}
			reading := newReadingWithBody(t, body, testCase.backlinksOpen)

			got := reading.View(80, testCase.height)
			if lineCount := strings.Count(got, "\n") + 1; lineCount != testCase.height {
				t.Errorf("View rendered %d lines, want exactly %d; got:\n%s",
					lineCount, testCase.height, plain(got))
			}
		})
	}
}

// TestReadingViewClampsBelowTheChromeFloor pins the floor itself: below
// chrome+1 the viewport is clamped to a single row rather than zero — a
// zero-height viewport would render nothing at all — so the total is the
// irreducible chrome+1 frame, larger than the requested height by design,
// for both panel states.
func TestReadingViewClampsBelowTheChromeFloor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		height        int
		backlinksOpen bool
		wantLineCount int
	}{
		{"panel closed", 1, false, 3}, // header, blank, one viewport row
		{"panel open", 5, true, 7},    // + panel title, 2 referrer rows, separating blank
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			reading := newHundredLineReading(t, testCase.backlinksOpen)

			got := plain(reading.View(80, testCase.height))
			if !strings.Contains(got, "row 001") {
				t.Fatalf("clamped view lost the viewport's single row; got:\n%s", got)
			}
			if strings.Contains(got, "row 002") {
				t.Fatalf("clamped view rendered more than the single clamped row; got:\n%s", got)
			}
			if lineCount := strings.Count(got, "\n") + 1; lineCount != testCase.wantLineCount {
				t.Errorf("clamped view has %d lines, want exactly %d (chrome + one viewport row); got:\n%s",
					lineCount, testCase.wantLineCount, got)
			}
		})
	}
}

// controlKey builds the KeyPressMsg a real ctrl+letter press produces: the
// letter's code with the ctrl modifier and NO Text — a populated Text would
// override the keystroke form (ultraviolet's Key.String prefers Text), so
// "ctrl+d" only ever comes from the modifier field.
func controlKey(letter rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: letter, Mod: tea.ModCtrl}
}

// TestReadingViewportHalfPageKeys pins spec §4's ctrl+d/ctrl+u: this
// package's own keymap wires them (readingViewportKeyMap — the viewport's
// stock defaults are overridden, so upstream's own tests prove nothing
// about these two bindings). ctrl+d moves the visible slice down about
// half a page; ctrl+u returns to the top — asserted directionally, so both
// a deleted binding (no movement) and swapped bindings (wrong direction)
// fail.
func TestReadingViewportHalfPageKeys(t *testing.T) {
	t.Parallel()
	reading := newHundredLineReading(t, false)

	if got := plain(reading.View(80, 20)); !strings.Contains(got, "row 001") {
		t.Fatalf("setup: initial view missing the top row; got:\n%s", got)
	}

	next, _ := reading.Update(controlKey('d'))
	reading = next.(*Reading)
	halfDown := plain(reading.View(80, 20))
	if strings.Contains(halfDown, "row 001") {
		t.Fatalf("ctrl+d did not scroll away from the top; got:\n%s", halfDown)
	}
	if !strings.Contains(halfDown, "row 019") {
		t.Fatalf("ctrl+d did not move about half a page down; got:\n%s", halfDown)
	}

	next, _ = reading.Update(controlKey('u'))
	reading = next.(*Reading)
	if got := plain(reading.View(80, 20)); !strings.Contains(got, "row 001") {
		t.Errorf("ctrl+u did not scroll back to the top; got:\n%s", got)
	}
}

// TestReadingEditKeyEmitsRequest pins the reading view's half of spec §5's
// e: it emits EditRequestMsg for the memory it renders — the root owns the
// class/editor/session gates and the handoff itself. Memory() is the
// root-facing accessor those availability gates read.
func TestReadingEditKeyEmitsRequest(t *testing.T) {
	t.Parallel()
	reading := newReadingFixture(t, nil)

	_, cmd := reading.Update(key("e"))
	if cmd == nil {
		t.Fatal("e produced no Cmd")
	}
	request, ok := cmd().(EditRequestMsg)
	if !ok {
		t.Fatalf("e produced %#v, want EditRequestMsg", cmd())
	}
	if request.Memory.RepoPath != "claude/alpha.md" {
		t.Errorf("EditRequestMsg carries %q, want the open memory %q", request.Memory.RepoPath, "claude/alpha.md")
	}
	if got := reading.Memory().RepoPath; got != "claude/alpha.md" {
		t.Errorf("Memory() = %q, want %q", got, "claude/alpha.md")
	}
}

// TestReadingHistoryKeyPushesHistory pins spec §6's h from the reading view:
// it opens the open memory's version-history screen, threading the shared Data
// seam and seeding the History's relative-age clock from the reading view's
// own stored now.
func TestReadingHistoryKeyPushesHistory(t *testing.T) {
	t.Parallel()
	reading := newReadingFixture(t, func(deps *ReadingDeps) {
		deps.Data = &fakeHistoryData{}
		deps.Now = historyNow
	})

	_, cmd := reading.Update(key("h"))
	if cmd == nil {
		t.Fatal("h produced no Cmd")
	}
	push, ok := cmd().(PushScreenMsg)
	if !ok {
		t.Fatalf("h produced %#v, want PushScreenMsg", cmd())
	}
	history, ok := push.Screen.(*History)
	if !ok {
		t.Fatalf("pushed screen is %T, want *History", push.Screen)
	}
	if _, repoPath := history.Target(); repoPath != "claude/alpha.md" {
		t.Errorf("pushed history targets %q, want the open memory %q", repoPath, "claude/alpha.md")
	}
	if history.now != historyNow {
		t.Errorf("pushed history clock = %v, want the reading view's seeded now %v", history.now, historyNow)
	}
}

var _ Screen = (*Reading)(nil)
