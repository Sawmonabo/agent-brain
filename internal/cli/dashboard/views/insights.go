package views

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	keybinding "charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/lint"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/memoryfs"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/theme"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
)

// insightsHistoryLimit bounds the one folder-wide /v0/history fetch the Insights
// screen tallies its edit- and machine-activity sections from (spec §9). Deeper
// than the browser's per-memory/deleted-scan cap (historyVersionLimit): those
// answer "the newest slice of one file's timeline," while these stats summarise
// the whole folder's activity — most-edited paths and per-machine capture counts
// want a broad sample of the entire history, not just the newest commits — so
// this asks a generous ceiling that still bounds a pathological history to a
// finite transfer.
const insightsHistoryLimit = 500

// insightsTopN is how many rows the "most edited" and "stalest" sections list —
// the busiest and the most-neglected handful, enough to act on without turning
// the screen into a full table (spec §9).
const insightsTopN = 5

// insightsChromeLines is everything Insights renders above its scroll viewport:
// the title line and its trailing blank. The viewport fills the remaining
// height exactly (the reading/history honest-height contract).
const insightsChromeLines = 2

// InsightsDataMsg carries the Insights screen's one folder-wide /v0/history
// fetch back to it. Folder is the key the fetch ran for and doubles as a
// staleness guard: the root forwards this to the stack top, and Insights drops
// any whose Folder is not its own, so a fetch that outlived its screen (the user
// popped and opened a different folder's insights) never lands in the wrong one.
// A DISTINCT type from HistoryVersionsMsg on purpose — this fetch and the
// browser's folder-wide deleted scan both run with path "", so a shared type
// would let each adopt the other's result; the type is the discriminator the
// RepoPath key cannot be here, both being folder-wide.
type InsightsDataMsg struct {
	Folder   string
	Versions []api.HistoryVersion
	Err      error
}

// InsightsDeps is everything the Insights screen needs, injected once — the
// consumer-side-seam idiom shared with BrowserDeps/HistoryDeps.
type InsightsDeps struct {
	// Folder is the project folder these insights summarise; it keys the
	// folder-wide history fetch and every InsightsDataMsg the screen accepts.
	Folder string
	// Memories is the browser's CURRENT listing, passed in rather than re-walked
	// (the brief's "pass, don't re-walk"): the filesystem sections (counts, total
	// size, stalest) read straight from it, frozen at push time — the browser
	// stays the live view of the tree, the insights a snapshot of it.
	Memories []memoryfs.Memory
	// Lint is the browser's current lint results (spec §8), the source of the
	// per-rule issue tally — passed in for the same reason as Memories.
	Lint []lint.Result
	// Data is the read-only version surface (spec §6). Insights calls only
	// History(folder, "", insightsHistoryLimit) once, at push time, for the
	// edit/machine sections. A nil Data (some tests) yields no fetch — the
	// history-derived sections hold their loading notice — never a panic.
	Data HistoryDataSource
	// Styles is the theme's style set, re-installed on a background swap
	// (SetStyles); feeds only chrome, re-rendered every View.
	Styles theme.Styles
	// Now seeds the screen's own stored clock (Insights.now) at construction — a
	// plain value, not a func() time.Time: once Insights is the stack top the
	// browser it was opened from stops receiving RefreshMsg, so a closure over
	// the browser's clock would freeze. Every render reads the stored field,
	// advanced only by this screen's own RefreshMsg (screen.go's clock-in-the-
	// tick contract, the History/Browser precedent).
	Now time.Time
}

// Insights is the project-insights screen (spec §9): a scrollable set of summary
// sections over one project folder — memory counts and total size per provider,
// the newest capture, the most-edited and stalest memories, a lint issue tally,
// and per-machine capture counts. Pointer-receiver Screen mutating in place (the
// Browser/History precedent), owning a scroll viewport and the one folder-wide
// history fetch its activity sections derive from.
//
// The filesystem sections read the injected memory/lint snapshots and render
// immediately; the history sections wait on the fetch and, if it fails, show the
// error inline while the filesystem sections still render — a daemon that is down
// must never blank the local facts (spec §9).
type Insights struct {
	deps InsightsDeps

	// now is the screen's own stored clock, seeded from deps.Now and advanced by
	// every RefreshMsg — never time.Now(), so relative ages render
	// deterministically and stay live against the root's tick (screen.go).
	now time.Time

	// loaded latches once the history fetch answers (success or failure); loadErr
	// holds a failure so the history sections show why rather than an empty tally.
	// The filesystem sections never gate on either.
	loaded  bool
	loadErr error

	// The history-derived tallies, computed once when the fetch is adopted — they
	// do not depend on the clock, only their rendered relative ages do (View).
	captured    bool               // a capture-subject commit was found
	lastCapture api.HistoryVersion // the newest one (valid only when captured)
	mostEdited  []labelCount       // top insightsTopN paths by version count
	machines    []labelCount       // per-host capture counts, busiest first
	// truncated means the scan came back at the fetch cap — the tallies below are
	// over the newest insightsHistoryLimit commits only, disclosed as such.
	truncated bool

	viewport viewport.Model
}

// labelCount is a (label, count) tally row — a provider and its memory count, a
// path and its version count, a host and its capture count. Shared by every
// Insights section that ranks or lists a grouping.
type labelCount struct {
	label string
	count int
}

// NewInsights builds a ready Insights screen. It performs no I/O: the activity
// stats are a daemon fetch, issued as InitCmd after the root pushes the screen
// (like History), so the history sections show a loading notice until it answers
// while the filesystem sections render from the injected snapshot at once.
func NewInsights(deps InsightsDeps) *Insights {
	vp := viewport.New()
	vp.KeyMap = readingViewportKeyMap()
	return &Insights{deps: deps, now: deps.Now, viewport: vp}
}

// Title names the breadcrumb segment.
func (i *Insights) Title() string {
	return "insights"
}

// InitCmd is the one folder-wide history fetch, issued by the root right after
// it pushes the screen (the initScreen seam) — the same push-then-fetch shape as
// History, so the screen is on the stack before its own result can arrive.
func (i *Insights) InitCmd() tea.Cmd {
	return i.fetchCmd()
}

// SetStyles installs a new theme — root-propagated on a background-color swap,
// the same treatment every pushed screen gets (dashboard.go's applyStackTheme).
// Styles feed only chrome, re-rendered every View, so nothing pairs with this.
// Insights renders no markdown, so unlike Browser/Reading/History it exposes no
// render seam. Not part of the Screen interface (kept to Update/View/Title).
func (i *Insights) SetStyles(styles theme.Styles) {
	i.deps.Styles = styles
}

func (i *Insights) fetchCmd() tea.Cmd {
	data, folder := i.deps.Data, i.deps.Folder
	if data == nil {
		return nil // nothing to fetch through (see InsightsDeps.Data) — never a closure that would nil-deref when run
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
		defer cancel()
		resp, err := data.History(ctx, folder, "", insightsHistoryLimit)
		return InsightsDataMsg{Folder: folder, Versions: resp.Versions, Err: err}
	}
}

// Update handles one message. RefreshMsg advances the stored clock but does NOT
// re-fetch: the whole screen is a point-in-time snapshot (the memories are
// frozen too), so only its relative ages track the tick. InsightsDataMsg is
// matched on Folder and dropped if not this screen's — a fetch forwarded here
// after the screen changed belongs to a different one.
func (i *Insights) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case RefreshMsg:
		i.now = msg.Now
		return i, nil
	case InsightsDataMsg:
		if msg.Folder != i.deps.Folder {
			return i, nil
		}
		i.adopt(msg)
		return i, nil
	case tea.KeyPressMsg:
		return i.updateKey(msg)
	}
	return i, nil
}

// updateKey handles a keypress: esc pops the screen (no internal sub-mode to
// consume first, so the esc-ordering rule collapses to a direct pop), g/G jump
// the scroll viewport to top/bottom, and everything else is the viewport's own
// scroll set (readingViewportKeyMap).
func (i *Insights) updateKey(msg tea.KeyPressMsg) (Screen, tea.Cmd) {
	switch {
	case keybinding.Matches(msg, DashboardKeys.InsightsBack):
		return i, func() tea.Msg { return PopScreenMsg{} }
	case msg.String() == "g":
		i.viewport.GotoTop()
		return i, nil
	case msg.String() == "G":
		i.viewport.GotoBottom()
		return i, nil
	}
	var cmd tea.Cmd
	i.viewport, cmd = i.viewport.Update(msg)
	return i, cmd
}

// adopt tallies the folder-wide history fetch into the activity sections. A
// failure records loadErr (the history sections show it inline) and leaves the
// tallies empty; the filesystem sections are unaffected either way. The tallies
// are computed once here, not per render, because they do not depend on the
// clock — only their rendered relative ages do (View).
func (i *Insights) adopt(msg InsightsDataMsg) {
	i.loaded = true
	if msg.Err != nil {
		i.loadErr = msg.Err
		return
	}
	i.loadErr = nil
	i.truncated = len(msg.Versions) == insightsHistoryLimit
	i.lastCapture, i.captured = newestCapture(msg.Versions)
	i.mostEdited = topPaths(msg.Versions, insightsTopN)
	i.machines = hostCounts(msg.Versions)
}

// View renders the insights: a title line, then the sections in a scroll
// viewport that fills the remaining height exactly (the reading/history honest-
// height contract — viewport.View pads to its SetHeight, so no trailing trim).
// Content is rebuilt every render — cheap string formatting, no glamour, and the
// relative ages must track the live clock — and re-set on the viewport, which
// preserves the scroll offset across a same-length rebuild (SetContent only
// clamps, never resets), so an advancing "Xm ago" never jumps the user to the top.
func (i *Insights) View(width, height int) string {
	var view strings.Builder
	view.WriteString(sectionTitle(i.deps.Styles, "Insights: "+i.deps.Folder))
	view.WriteString("\n\n")
	i.viewport.SetWidth(width)
	i.viewport.SetHeight(max(height-insightsChromeLines, 1))
	i.viewport.SetContent(i.sections())
	view.WriteString(i.viewport.View())
	return view.String()
}

// sections builds the scrollable body: the six summary blocks in spec §9 order,
// each a styled header over its rows, joined by a blank line.
func (i *Insights) sections() string {
	blocks := []string{
		i.countsBlock(),
		i.lastCaptureBlock(),
		i.mostEditedBlock(),
		i.stalestBlock(),
		i.lintBlock(),
		i.machinesBlock(),
	}
	return strings.Join(blocks, "\n\n")
}

// block renders a section header over its rows (or a single dim notice line).
func (i *Insights) block(title string, rows []string) string {
	lines := append([]string{i.deps.Styles.Header.Render(title)}, rows...)
	return strings.Join(lines, "\n")
}

// dimLine is a single dim notice row (a loading/empty placeholder).
func (i *Insights) dimLine(text string) []string {
	return []string{i.deps.Styles.Dim.Render(text)}
}

// countsBlock is the per-provider memory counts and the fleet-footprint summary
// (spec §9's counts · total size), read from the filesystem snapshot.
func (i *Insights) countsBlock() string {
	memories := i.deps.Memories
	if len(memories) == 0 {
		return i.block("Memories", i.dimLine("no memories in this project"))
	}
	providers := providerCounts(memories)
	rows := make([]string, 0, len(providers)+1)
	for _, provider := range providers {
		rows = append(rows, tallyRow(provider.label, provider.count))
	}
	rows = append(rows, i.deps.Styles.Dim.Render(fmt.Sprintf("%s · %s · %s total",
		quantity(len(memories), "memory", "memories"),
		quantity(len(providers), "provider", "providers"),
		humanBytes(totalSize(memories)))))
	return i.block("Memories", rows)
}

// lastCaptureBlock is the newest capture-subject commit (spec §9's last capture):
// relative age · absolute stamp · host. Waits on the history fetch.
func (i *Insights) lastCaptureBlock() string {
	if placeholder, pending := i.historyPlaceholder(); pending {
		return i.block("Last capture", []string{placeholder})
	}
	if !i.captured {
		return i.block("Last capture", i.historyEmptyNotice("no captures"))
	}
	host := i.lastCapture.Host
	if host == "" {
		host = "—"
	}
	line := fmt.Sprintf("  %s · %s · %s",
		relativeTime(*i.lastCapture.Timestamp, i.now),
		i.lastCapture.Timestamp.Format(historyStampLayout), host)
	return i.block("Last capture", []string{line})
}

// mostEditedBlock ranks the folder's changed paths by how many versions touched
// each (spec §9's most edited). Waits on the history fetch.
func (i *Insights) mostEditedBlock() string {
	if placeholder, pending := i.historyPlaceholder(); pending {
		return i.block("Most edited", []string{placeholder})
	}
	if len(i.mostEdited) == 0 {
		return i.block("Most edited", i.historyEmptyNotice("no edits recorded"))
	}
	rows := make([]string, len(i.mostEdited))
	for idx, row := range i.mostEdited {
		rows[idx] = tallyRow(row.label, row.count)
	}
	return i.block("Most edited", i.withTruncation(rows))
}

// stalestBlock lists the memories with the oldest ModTime (spec §9's stalest),
// read from the filesystem snapshot: repo path · relative age, oldest first.
func (i *Insights) stalestBlock() string {
	memories := i.deps.Memories
	if len(memories) == 0 {
		return i.block("Stalest", i.dimLine("no memories in this project"))
	}
	stale := stalest(memories, insightsTopN)
	rows := make([]string, len(stale))
	for idx, memory := range stale {
		rows[idx] = fmt.Sprintf("  %s  %s", memory.RepoPath, relativeTime(memory.ModTime, i.now))
	}
	return i.block("Stalest", rows)
}

// lintBlock tallies lint issues per rule across the snapshot (spec §9's lint
// summary), read from the filesystem snapshot.
func (i *Insights) lintBlock() string {
	rules := lintCounts(i.deps.Lint)
	if len(rules) == 0 {
		return i.block("Lint", i.dimLine("no lint issues"))
	}
	rows := make([]string, len(rules))
	for idx, rule := range rules {
		rows[idx] = tallyRow(rule.label, rule.count)
	}
	return i.block("Lint", rows)
}

// machinesBlock tallies capture commits per machine (spec §9's machines). Waits
// on the history fetch.
func (i *Insights) machinesBlock() string {
	if placeholder, pending := i.historyPlaceholder(); pending {
		return i.block("Machines", []string{placeholder})
	}
	if len(i.machines) == 0 {
		return i.block("Machines", i.historyEmptyNotice("no captures recorded"))
	}
	rows := make([]string, len(i.machines))
	for idx, machine := range i.machines {
		rows[idx] = tallyRow(machine.label, machine.count)
	}
	return i.block("Machines", i.withTruncation(rows))
}

// withTruncation appends the fetch-cap disclosure to a history-derived section's
// rows when the scan hit insightsHistoryLimit, so a tally over only the newest
// slice of a long history is never passed off as the whole timeline.
func (i *Insights) withTruncation(rows []string) []string {
	if !i.truncated {
		return rows
	}
	return append(rows, i.deps.Styles.Dim.Render(historyTruncationNotice(insightsHistoryLimit)))
}

// historyEmptyNotice qualifies a history section's empty-state line. Below the
// cap the scan saw the whole timeline, so "<subject> in this project's history"
// is the literal truth; at the cap that same phrasing would be a false
// whole-history claim over only the newest slice, so the empty state carries the
// same disclosure the populated sections do (withTruncation). subject is the
// section's own noun phrase ("no captures", "no edits recorded").
func (i *Insights) historyEmptyNotice(subject string) []string {
	if i.truncated {
		return i.dimLine(fmt.Sprintf("%s in the newest %d commits — older history not scanned", subject, insightsHistoryLimit))
	}
	return i.dimLine(subject + " in this project's history")
}

// historyPlaceholder returns the inline line the history-derived sections show
// instead of their tally when the one folder-wide fetch has not produced a
// usable result: the daemon error verbatim (rendered plain — the filesystem
// sections still stand, so it is an inline notice, not a screen-wide failure),
// or a loading notice while it is still in flight. ok=false means a successful
// fetch is in hand and the caller should render its real rows.
func (i *Insights) historyPlaceholder() (string, bool) {
	switch {
	case i.loadErr != nil:
		return fmt.Sprintf("history unavailable: %v", i.loadErr), true
	case !i.loaded:
		return i.deps.Styles.Dim.Render("loading history…"), true
	}
	return "", false
}

// tallyRow renders one "  <label>  <count>" row — the shared shape of the
// provider, most-edited, lint, and machine rows. The label is rendered as-is;
// the viewport clips a pathological over-width line at the pane edge rather than
// letting it break the frame, so no truncation math lives here.
func tallyRow(label string, count int) string {
	return fmt.Sprintf("  %s  %d", label, count)
}

// quantity renders a pluralised count ("1 memory", "4 memories").
func quantity(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}

// newestCapture is the capture-subject commit (Timestamp set) with the latest
// Timestamp across versions — spec §9's "last capture". Foreign commits (nil
// Timestamp) are skipped. ok=false when no version is a capture (a folder whose
// whole history is merges/foreign edits, or an empty scan).
func newestCapture(versions []api.HistoryVersion) (api.HistoryVersion, bool) {
	var newest api.HistoryVersion
	found := false
	for _, version := range versions {
		if version.Timestamp == nil {
			continue
		}
		if !found || version.Timestamp.After(*newest.Timestamp) {
			newest, found = version, true
		}
	}
	return newest, found
}

// topPaths ranks the folder's changed paths by how many versions touched each —
// spec §9's "most edited" — returning the busiest n. Every version's Paths are
// counted (folder-wide history populates them), so a path's count is the number
// of commits that changed it, foreign commits included: a merge that touched a
// file did edit it.
func topPaths(versions []api.HistoryVersion, n int) []labelCount {
	counts := make(map[string]int)
	for _, version := range versions {
		for _, path := range version.Paths {
			counts[path]++
		}
	}
	return topCounts(counts, n)
}

// hostCounts tallies capture commits per machine — spec §9's "machines" — over
// the capture-subject commits only (a foreign commit has no host). Busiest host
// first; no cap, since a fleet's machine count is small and each is worth showing.
func hostCounts(versions []api.HistoryVersion) []labelCount {
	counts := make(map[string]int)
	for _, version := range versions {
		if version.Host == "" {
			continue
		}
		counts[version.Host]++
	}
	return topCounts(counts, 0)
}

// providerCounts tallies memories per provider from the filesystem snapshot,
// alphabetical by provider — spec §9's per-provider counts (an inventory, so
// ordered by name rather than ranked by count).
func providerCounts(memories []memoryfs.Memory) []labelCount {
	counts := make(map[string]int)
	for _, memory := range memories {
		counts[memory.Provider]++
	}
	rows := make([]labelCount, 0, len(counts))
	for label, count := range counts {
		rows = append(rows, labelCount{label: label, count: count})
	}
	sort.Slice(rows, func(a, b int) bool { return rows[a].label < rows[b].label })
	return rows
}

// lintCounts tallies lint issues per rule across the snapshot's results — spec
// §9's lint summary. One result can carry several issues of different rules; each
// counts under its own rule. Busiest rule first, ties by rule name.
func lintCounts(results []lint.Result) []labelCount {
	counts := make(map[string]int)
	for _, result := range results {
		for _, issue := range result.Issues {
			counts[issue.Rule]++
		}
	}
	return topCounts(counts, 0)
}

// topCounts sorts a label→count map into descending-count order, ties broken by
// label ascending (a stable, deterministic order for the view and its tests),
// and returns the top n rows. n <= 0 returns all rows.
func topCounts(counts map[string]int, n int) []labelCount {
	rows := make([]labelCount, 0, len(counts))
	for label, count := range counts {
		rows = append(rows, labelCount{label: label, count: count})
	}
	sort.Slice(rows, func(a, b int) bool {
		if rows[a].count != rows[b].count {
			return rows[a].count > rows[b].count
		}
		return rows[a].label < rows[b].label
	})
	if n > 0 && len(rows) > n {
		rows = rows[:n]
	}
	return rows
}

// totalSize sums the snapshot's memory sizes — spec §9's total footprint.
func totalSize(memories []memoryfs.Memory) int64 {
	var total int64
	for _, memory := range memories {
		total += memory.Size
	}
	return total
}

// stalest is the n memories with the oldest ModTime — spec §9's "stalest", the
// most-neglected handful worth revisiting. Oldest first; ties broken by RepoPath
// for a deterministic order. A zero ModTime (should not occur for a real file)
// sorts oldest, surfacing the anomaly rather than hiding it.
func stalest(memories []memoryfs.Memory, n int) []memoryfs.Memory {
	sorted := make([]memoryfs.Memory, len(memories))
	copy(sorted, memories)
	sort.SliceStable(sorted, func(a, b int) bool {
		if !sorted[a].ModTime.Equal(sorted[b].ModTime) {
			return sorted[a].ModTime.Before(sorted[b].ModTime)
		}
		return sorted[a].RepoPath < sorted[b].RepoPath
	})
	if len(sorted) > n {
		sorted = sorted[:n]
	}
	return sorted
}

// humanBytes renders a byte count as a short human-readable size (B/KB/MB/…),
// one decimal past the KB threshold — the fleet's memory footprint at a glance,
// not an exact byte count. Base-1000 (KB, not KiB): a rough orientation reads
// more naturally in decimal units.
func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
