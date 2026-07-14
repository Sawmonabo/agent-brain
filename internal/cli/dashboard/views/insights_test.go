package views

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/lint"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/memoryfs"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/theme"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
)

// insightsBase is the fixed clock the insights tests reckon relative ages
// against — no wall-clock dependency (test standard).
var insightsBase = time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

// insightsAt is a *time.Time offset from insightsBase — a capture instant for
// the fake history.
func insightsAt(offset time.Duration) *time.Time {
	instant := insightsBase.Add(offset)
	return &instant
}

// insightsMemories is the two-provider filesystem snapshot the insights tests
// read their counts/size/stalest sections from: known sizes and modtimes so
// every rendered number is exact. Total size 6500 bytes (6.5 KB); stalest order
// (oldest first) is cursor/four.md, cursor/three.md, claude/two.md, claude/one.md.
func insightsMemories() []memoryfs.Memory {
	return []memoryfs.Memory{
		{Provider: "claude", Folder: "acme", RepoPath: "claude/one.md", Name: "one", ModTime: insightsBase.Add(-1 * time.Hour), Size: 1000},
		{Provider: "claude", Folder: "acme", RepoPath: "claude/two.md", Name: "two", ModTime: insightsBase.Add(-48 * time.Hour), Size: 2000},
		{Provider: "cursor", Folder: "acme", RepoPath: "cursor/three.md", Name: "three", ModTime: insightsBase.Add(-72 * time.Hour), Size: 500},
		{Provider: "cursor", Folder: "acme", RepoPath: "cursor/four.md", Name: "four", ModTime: insightsBase.Add(-240 * time.Hour), Size: 3000},
	}
}

// insightsLint is the lint snapshot: per-rule counts stale 2, frontmatter 1,
// dangling-link 1 (one result carries two issues of different rules).
func insightsLint(memories []memoryfs.Memory) []lint.Result {
	return []lint.Result{
		{Memory: memories[0], Issues: []lint.Issue{{Rule: "stale"}, {Rule: "frontmatter"}}},
		{Memory: memories[3], Issues: []lint.Issue{{Rule: "stale"}}},
		{Memory: memories[2], Issues: []lint.Issue{{Rule: "dangling-link"}}},
	}
}

// insightsVersions is the fake folder-wide history, newest first. Path tally
// (all versions, foreign included): claude/one.md 3, cursor/four.md 3,
// claude/two.md 1, cursor/three.md 1. Capture hosts (foreign r7 excluded):
// workstation 4, laptop 2. Newest capture: r1 (workstation, 30m before base).
func insightsVersions() []api.HistoryVersion {
	return []api.HistoryVersion{
		{Rev: "r1", Host: "workstation", Timestamp: insightsAt(-30 * time.Minute), Paths: []string{"claude/one.md"}},
		{Rev: "r2", Host: "workstation", Timestamp: insightsAt(-2 * time.Hour), Paths: []string{"claude/one.md", "cursor/four.md"}},
		{Rev: "r3", Host: "laptop", Timestamp: insightsAt(-5 * time.Hour), Paths: []string{"claude/one.md"}},
		{Rev: "r4", Host: "workstation", Timestamp: insightsAt(-26 * time.Hour), Paths: []string{"cursor/four.md"}},
		{Rev: "r5", Host: "laptop", Timestamp: insightsAt(-50 * time.Hour), Paths: []string{"claude/two.md"}},
		{Rev: "r6", Host: "workstation", Timestamp: insightsAt(-100 * time.Hour), Paths: []string{"cursor/three.md"}},
		{Rev: "r7", Paths: []string{"cursor/four.md"}}, // foreign: no host, no timestamp
	}
}

func newInsightsFixture(data HistoryDataSource) *Insights {
	memories := insightsMemories()
	return NewInsights(InsightsDeps{
		Folder:   "acme",
		Memories: memories,
		Lint:     insightsLint(memories),
		Data:     data,
		Styles:   theme.Default(true),
		Now:      insightsBase,
	})
}

// adoptInsights runs the screen's init fetch through the fake and feeds the
// result back the way the root forwards it, returning the updated screen.
func adoptInsights(t *testing.T, screen *Insights) *Insights {
	t.Helper()
	cmd := screen.InitCmd()
	if cmd == nil {
		t.Fatal("InitCmd returned nil; want a fetch Cmd")
	}
	next, _ := screen.Update(cmd())
	return next.(*Insights)
}

// assertBefore fails unless both needles are present and first precedes second.
func assertBefore(t *testing.T, haystack, first, second string) {
	t.Helper()
	i, j := strings.Index(haystack, first), strings.Index(haystack, second)
	switch {
	case i < 0:
		t.Errorf("missing %q; got:\n%s", first, haystack)
	case j < 0:
		t.Errorf("missing %q; got:\n%s", second, haystack)
	case i >= j:
		t.Errorf("%q should precede %q; got:\n%s", first, second, haystack)
	}
}

// insightsWithVersions builds the standard fixture but with an arbitrary
// folder-wide history, for driving the fetch-cap boundary directly.
func insightsWithVersions(versions []api.HistoryVersion) *Insights {
	return newInsightsFixture(&fakeHistoryData{historyResp: api.HistoryResponse{Versions: versions}})
}

// manyVersions fabricates n commits touching one path, for exercising the
// fetch-cap edge without hand-writing hundreds of rows. foreignOnly commits
// carry no host/timestamp (no capture subject), which drives the
// empty-captures-at-cap path.
func manyVersions(n int, foreignOnly bool) []api.HistoryVersion {
	out := make([]api.HistoryVersion, n)
	for idx := range n {
		version := api.HistoryVersion{Rev: fmt.Sprintf("r%d", idx), Paths: []string{"claude/one.md"}}
		if !foreignOnly {
			stamp := insightsBase.Add(-time.Duration(idx) * time.Minute)
			version.Host, version.Timestamp = "workstation", &stamp
		}
		out[idx] = version
	}
	return out
}

// sectionLines returns the rows rendered under a section header (matched by its
// trimmed text) up to the blank line that separates sections — so a section's
// exact row set can be asserted, not merely membership.
func sectionLines(rendered, header string) []string {
	lines := strings.Split(rendered, "\n")
	for idx, line := range lines {
		if strings.TrimSpace(line) != header {
			continue
		}
		var rows []string
		for _, row := range lines[idx+1:] {
			if strings.TrimSpace(row) == "" {
				break // the blank line (or height padding) after the last row
			}
			rows = append(rows, strings.TrimRight(row, " ")) // drop viewport width padding
		}
		return rows
	}
	return nil
}

func TestInsightsSectionsExactNumbers(t *testing.T) {
	t.Parallel()
	fake := &fakeHistoryData{historyResp: api.HistoryResponse{Versions: insightsVersions()}}
	got := plain(adoptInsights(t, newInsightsFixture(fake)).View(100, 60))

	wants := []string{
		// counts: memories per provider · total size
		"claude  2", "cursor  2", "4 memories", "2 providers", "6.5 KB",
		// last capture: newest capture-subject timestamp + host
		"30m ago", "2026-07-13 11:30", "workstation",
		// most edited: top paths by version count (all versions, foreign included)
		"claude/one.md  3", "cursor/four.md  3", "claude/two.md  1", "cursor/three.md  1",
		// stalest: bottom by ModTime
		"cursor/four.md  10d ago", "cursor/three.md  3d ago", "claude/two.md  2d ago", "claude/one.md  1h ago",
		// lint: issue count per rule
		"stale  2", "dangling-link  1", "frontmatter  1",
		// machines: per-host capture counts (foreign r7 excluded)
		"workstation  4", "laptop  2",
	}
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("insights view missing %q; got:\n%s", want, got)
		}
	}

	// most edited ranks busiest first; stalest lists oldest first.
	assertBefore(t, got, "claude/one.md  3", "cursor/three.md  1")
	assertBefore(t, got, "cursor/four.md  10d ago", "claude/one.md  1h ago")
}

// TestInsightsTruncationDisclosesItsOwnCap pins that a history scan at the fetch
// cap discloses insights' real 500-commit window, never history's unrelated 200:
// the two screens fetch different caps, so a shared hardcoded notice would state
// a false count. Pinned at both edges — at the cap the notice fires with the real
// number, one under it fires not at all.
func TestInsightsTruncationDisclosesItsOwnCap(t *testing.T) {
	t.Parallel()
	atCap := adoptInsights(t, insightsWithVersions(manyVersions(insightsHistoryLimit, false)))
	got := plain(atCap.View(100, 200))
	if !strings.Contains(got, "newest 500 commits — older history not scanned") {
		t.Errorf("truncation notice missing the real 500-commit disclosure; got:\n%s", got)
	}
	if strings.Contains(got, "newest 200 commits") {
		t.Errorf("truncation notice claims history's 200-commit window over a 500-commit scan; got:\n%s", got)
	}

	// One under the cap: the scan saw the whole timeline, so no disclosure at all.
	below := adoptInsights(t, insightsWithVersions(manyVersions(insightsHistoryLimit-1, false)))
	if b := plain(below.View(100, 200)); strings.Contains(b, "older history not scanned") {
		t.Errorf("a %d-version scan (one under the cap) rendered a truncation notice; want none; got:\n%s", insightsHistoryLimit-1, b)
	}
}

// TestInsightsEmptyHistoryAtCapIsQualified pins that when the scan hits the cap
// yet holds no captures, the empty-state notices carry the truncation disclosure
// too: "no captures recorded" over a truncated window would be a false
// whole-history claim. Below the cap the same states make the plain whole-history
// statement with no hedge.
func TestInsightsEmptyHistoryAtCapIsQualified(t *testing.T) {
	t.Parallel()
	atCap := adoptInsights(t, insightsWithVersions(manyVersions(insightsHistoryLimit, true)))
	got := plain(atCap.View(100, 200))
	for _, want := range []string{
		"no captures in the newest 500 commits — older history not scanned",          // last capture
		"no captures recorded in the newest 500 commits — older history not scanned", // machines
	} {
		if !strings.Contains(got, want) {
			t.Errorf("empty-state over a truncated window is unqualified; missing %q; got:\n%s", want, got)
		}
	}

	// A window of pathless commits (a sync-merge yields no changed files, so
	// git log --name-only returns zero paths) makes most-edited itself empty at
	// the cap — its empty-state must carry the disclosure too. Pins the
	// most-edited call site specifically; the machines/last-capture asserts above
	// exercise the shared helper's other two branches.
	merges := manyVersions(insightsHistoryLimit, true)
	for idx := range merges {
		merges[idx].Paths = nil
	}
	mergeCap := plain(adoptInsights(t, insightsWithVersions(merges)).View(100, 200))
	if want := "no edits recorded in the newest 500 commits — older history not scanned"; !strings.Contains(mergeCap, want) {
		t.Errorf("most-edited empty-state over a truncated pathless window is unqualified; missing %q; got:\n%s", want, mergeCap)
	}

	small := adoptInsights(t, insightsWithVersions([]api.HistoryVersion{{Rev: "r0", Paths: []string{"claude/one.md"}}}))
	s := plain(small.View(100, 200))
	if !strings.Contains(s, "no captures in this project's history") {
		t.Errorf("untruncated empty last-capture lost its whole-history wording; got:\n%s", s)
	}
	if strings.Contains(s, "older history not scanned") {
		t.Errorf("untruncated empty history should carry no truncation notice; got:\n%s", s)
	}
}

// TestInsightsMachinesExcludesForeignHost pins that the machines tally skips the
// foreign commit's blank host (spec §9: machines are capture subjects). Asserts
// the exact row set, not membership — a Contains("workstation  4") check survives
// a stray blank-label row, but the machines section owning exactly the two real
// hosts does not.
func TestInsightsMachinesExcludesForeignHost(t *testing.T) {
	t.Parallel()
	screen := adoptInsights(t, newInsightsFixture(&fakeHistoryData{historyResp: api.HistoryResponse{Versions: insightsVersions()}}))
	got := plain(screen.View(100, 60))
	rows := sectionLines(got, "Machines")
	want := []string{tallyRow("workstation", 4), tallyRow("laptop", 2)}
	if !slices.Equal(rows, want) {
		t.Errorf("machines rows = %q, want %q (foreign r7's blank host must not be tallied)", rows, want)
	}
}

// TestInsightsIgnoresHistoryVersionsMsg pins the distinct-message-type guard: the
// browser's deleted-scan result (HistoryVersionsMsg) rides the same folder-wide
// path "" that insights' own fetch does, so the message type is the only thing
// keeping the wires apart. An adopted HistoryVersionsMsg would wrongly load the
// activity sections; the screen must stay pending.
func TestInsightsIgnoresHistoryVersionsMsg(t *testing.T) {
	t.Parallel()
	screen := newInsightsFixture(&fakeHistoryData{})
	next, _ := screen.Update(HistoryVersionsMsg{Folder: "acme", RepoPath: "", Versions: insightsVersions()})
	if got := plain(next.(*Insights).View(100, 60)); !strings.Contains(got, "loading history…") {
		t.Errorf("Insights adopted a foreign HistoryVersionsMsg; want still loading; got:\n%s", got)
	}
}

func TestInsightsHistoryErrorKeepsFilesystemSections(t *testing.T) {
	t.Parallel()
	fake := &fakeHistoryData{historyErr: errors.New("daemon unreachable")}
	got := plain(adoptInsights(t, newInsightsFixture(fake)).View(100, 60))

	if !strings.Contains(got, "history unavailable: daemon unreachable") {
		t.Errorf("want the history error rendered inline; got:\n%s", got)
	}
	// A down daemon must never blank the local facts (spec §9).
	for _, want := range []string{"4 memories", "6.5 KB", "cursor/four.md  10d ago", "stale  2", "dangling-link  1"} {
		if !strings.Contains(got, want) {
			t.Errorf("filesystem section missing %q after history error; got:\n%s", want, got)
		}
	}
	// The machine tally is never fabricated from a failed fetch.
	if strings.Contains(got, "workstation  4") {
		t.Errorf("machines tally rendered despite the history error; got:\n%s", got)
	}
}

func TestInsightsFillsHeightExactly(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		screen *Insights
	}{
		{"loaded", adoptInsights(t, newInsightsFixture(&fakeHistoryData{historyResp: api.HistoryResponse{Versions: insightsVersions()}}))},
		{"loading", newInsightsFixture(&fakeHistoryData{})},
		{"empty", NewInsights(InsightsDeps{Folder: "empty", Styles: theme.Default(true), Now: insightsBase})},
	}
	for _, tc := range cases {
		for _, height := range []int{3, 8, 24, 60} {
			got := tc.screen.View(100, height)
			if lineCount := strings.Count(got, "\n") + 1; lineCount != height {
				t.Errorf("%s: view at height %d rendered %d lines, want exact fill; got:\n%s", tc.name, height, lineCount, plain(got))
			}
		}
	}
}

func TestInsightsFolderGuardDropsForeignFetch(t *testing.T) {
	t.Parallel()
	screen := newInsightsFixture(&fakeHistoryData{})

	// A fetch addressed to a different folder must not load this screen.
	next, _ := screen.Update(InsightsDataMsg{Folder: "other", Versions: insightsVersions()})
	screen = next.(*Insights)
	if got := plain(screen.View(100, 60)); !strings.Contains(got, "loading history…") {
		t.Errorf("foreign-folder fetch was adopted; want still loading; got:\n%s", got)
	}
	if strings.Contains(plain(screen.View(100, 60)), "workstation  4") {
		t.Error("foreign-folder fetch populated the machines tally")
	}

	// The matching folder's fetch adopts.
	next, _ = screen.Update(InsightsDataMsg{Folder: "acme", Versions: insightsVersions()})
	screen = next.(*Insights)
	if got := plain(screen.View(100, 60)); !strings.Contains(got, "workstation  4") {
		t.Errorf("matching-folder fetch was not adopted; got:\n%s", got)
	}
}

func TestInsightsInitCmdFetchesFolderWide(t *testing.T) {
	t.Parallel()
	fake := &fakeHistoryData{historyResp: api.HistoryResponse{Versions: insightsVersions()}}
	screen := newInsightsFixture(fake)

	cmd := screen.InitCmd()
	if cmd == nil {
		t.Fatal("InitCmd returned nil; want a folder-wide history fetch")
	}
	msg, ok := cmd().(InsightsDataMsg)
	if !ok {
		t.Fatalf("InitCmd Cmd produced %#v, want InsightsDataMsg", cmd())
	}
	if msg.Folder != "acme" || msg.Err != nil {
		t.Errorf("InsightsDataMsg = %+v, want folder acme with no error", msg)
	}
	if len(fake.historyCalls) != 1 {
		t.Fatalf("history fetched %d times, want exactly 1", len(fake.historyCalls))
	}
	if call := fake.historyCalls[0]; call.folder != "acme" || call.path != "" || call.limit != insightsHistoryLimit {
		t.Errorf("history call = %+v, want {folder:acme path:\"\" limit:%d}", call, insightsHistoryLimit)
	}
}

func TestInsightsEscPops(t *testing.T) {
	t.Parallel()
	screen := newInsightsFixture(&fakeHistoryData{})

	_, cmd := screen.Update(key("esc"))
	if cmd == nil {
		t.Fatal("esc produced no Cmd; want PopScreenMsg")
	}
	if _, ok := cmd().(PopScreenMsg); !ok {
		t.Fatalf("esc produced %#v, want PopScreenMsg", cmd())
	}

	// A scroll key belongs to the viewport, never a pop.
	if _, cmd := screen.Update(key("j")); cmd != nil {
		if _, ok := cmd().(PopScreenMsg); ok {
			t.Error("j (scroll) popped the screen; want it consumed by the viewport")
		}
	}
}

func TestInsightsRefreshAdvancesClock(t *testing.T) {
	t.Parallel()
	fake := &fakeHistoryData{historyResp: api.HistoryResponse{Versions: insightsVersions()}}
	screen := adoptInsights(t, newInsightsFixture(fake))

	if got := plain(screen.View(100, 60)); !strings.Contains(got, "30m ago") {
		t.Fatalf("want last capture 30m ago at base; got:\n%s", got)
	}

	// Advance the clock a full hour via the root's tick forward.
	next, _ := screen.Update(RefreshMsg{Now: insightsBase.Add(time.Hour)})
	screen = next.(*Insights)

	got := plain(screen.View(100, 60))
	if strings.Contains(got, "30m ago") {
		t.Errorf("last capture age did not advance with the clock; got:\n%s", got)
	}
	if !strings.Contains(got, "1h ago") {
		t.Errorf("want last capture 1h ago after +1h; got:\n%s", got)
	}
}

func TestInsightsStylesSectionHeaders(t *testing.T) {
	t.Parallel()
	styles := theme.Default(true)
	styles.Header = lipgloss.NewStyle().Transform(func(s string) string { return "⟦" + s + "⟧" })
	memories := insightsMemories()
	fake := &fakeHistoryData{historyResp: api.HistoryResponse{Versions: insightsVersions()}}
	screen := adoptInsights(t, NewInsights(InsightsDeps{
		Folder: "acme", Memories: memories, Lint: insightsLint(memories),
		Data: fake, Styles: styles, Now: insightsBase,
	}))

	got := plain(screen.View(100, 60))
	for _, header := range []string{"Memories", "Last capture", "Most edited", "Stalest", "Lint", "Machines"} {
		if !strings.Contains(got, "⟦"+header+"⟧") {
			t.Errorf("section header %q not rendered through the Header style; got:\n%s", header, got)
		}
	}
}

func TestInsightsSetStylesReTheme(t *testing.T) {
	t.Parallel()
	screen := adoptInsights(t, newInsightsFixture(&fakeHistoryData{historyResp: api.HistoryResponse{Versions: insightsVersions()}}))

	swapped := theme.Default(true)
	swapped.Header = lipgloss.NewStyle().Transform(func(s string) string { return "H<" + s + ">" })
	screen.SetStyles(swapped)

	if got := plain(screen.View(100, 60)); !strings.Contains(got, "H<Memories>") {
		t.Errorf("SetStyles did not take effect on the next render; got:\n%s", got)
	}
}

func TestBrowserInsightsKeyPushesScreen(t *testing.T) {
	t.Parallel()
	memories := insightsMemories()
	browser := NewBrowser(BrowserDeps{
		Folder: "acme",
		Units:  []api.UnitInfo{{Folder: "acme", Provider: "claude"}},
		Styles: theme.Default(true),
		Now:    insightsBase,
		// A dangling wiki-link makes the construction-time lint pass emit a real
		// finding, so the hand-across below compares NON-empty slices — with an
		// empty body the tally would be 0 and a Lint:nil regression would tie
		// 0 == 0, proving nothing.
		ReadBody: func(memoryfs.Memory) (string, error) { return "see [[does-not-exist]]", nil },
		List:     func() ([]memoryfs.Memory, error) { return memories, nil },
		Data:     &fakeHistoryData{},
	})
	if len(browser.lintResults) == 0 {
		t.Fatal("setup: fixture produced no lint results; the lint hand-across assertion would be vacuous")
	}

	_, cmd := browser.Update(key("i"))
	if cmd == nil {
		t.Fatal("i produced no Cmd; want PushScreenMsg")
	}
	push, ok := cmd().(PushScreenMsg)
	if !ok {
		t.Fatalf("i produced %#v, want PushScreenMsg", cmd())
	}
	insights, ok := push.Screen.(*Insights)
	if !ok {
		t.Fatalf("pushed %#v, want *Insights", push.Screen)
	}
	if insights.deps.Folder != "acme" {
		t.Errorf("insights folder = %q, want acme", insights.deps.Folder)
	}
	if len(insights.deps.Memories) != len(memories) {
		t.Errorf("insights got %d memories, want the browser's %d", len(insights.deps.Memories), len(memories))
	}
	// The browser hands its own current lint results across, not a re-scan.
	if len(insights.deps.Lint) != len(browser.lintResults) {
		t.Errorf("insights got %d lint results, want the browser's %d", len(insights.deps.Lint), len(browser.lintResults))
	}
}
