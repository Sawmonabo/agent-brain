package views

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	keybinding "charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/links"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/lint"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/memoryfs"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/theme"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/provider"
)

// previewMinWidth is the narrowest content width that still gets a preview
// pane; below it, the list alone gets the full width rather than squeezing
// an unreadably narrow preview beside it (spec §3).
const previewMinWidth = 100

// listPaneWidth is the browser's list-pane width once a preview pane is
// showing; the remainder goes to the preview, on the right (spec §3).
const listPaneWidth = 46

// descriptionTruncateAt bounds a row's rendered description so one
// unusually long frontmatter line cannot blow out the list pane's width.
const descriptionTruncateAt = 40

// BrowserDeps is everything the browser screen needs, injected once (the
// consumer-side-seam idiom): no globals, fakeable in tests.
type BrowserDeps struct {
	Registry *provider.Registry
	Units    []api.UnitInfo // the folder's units
	Folder   string
	Styles   theme.Styles
	// Now seeds the Browser's own stored clock (Browser.now) at construction
	// — a plain value, not a func() time.Time, so it is never mistaken for a
	// live source of truth: after construction, every render reads the
	// stored field, kept current by RefreshMsg's own Now (screen.go), not by
	// re-invoking anything here. Deterministic in tests the same way a live
	// closure would have been, without the staleness risk one carries once
	// Model's value semantics are in play (dashboard.go's buildBrowserDeps).
	Now time.Time
	// ReadBody reads one memory's full content — memoryfs.ReadBody in
	// production. Called synchronously: see refresh's doc for why a local
	// file read, unlike every daemon-backed Cmd elsewhere in this package,
	// does not need to go through a returned tea.Cmd.
	ReadBody func(memoryfs.Memory) (string, error)
	// List enumerates the folder's memories — memoryfs.List(Registry, Units)
	// bound once in production, so the browser and its tests never touch
	// Registry/Units directly.
	List func() ([]memoryfs.Memory, error)
	// StaleAfterDays is the configured `lint.stale_after_days` staleness
	// threshold (spec §8, internal/config/settings.go) — production wiring
	// reads it from the loaded config.toml (dashboard.go's
	// buildBrowserDeps); 0 disables the staleness rule entirely
	// (lint.Check's own contract), which is also what a zero-value
	// BrowserDeps in a test that does not care about staleness gets for
	// free.
	StaleAfterDays int
	// Render markdown-renders md at width — the root-owned glamour seam
	// (dashboard.go), shared with the later Reading screen. A nil Render
	// (as some tests that do not exercise the preview pane leave it) falls
	// back to the raw body text.
	Render func(md string, width int) string
}

// Browser is the memory browser screen (spec §3): every enrolled unit's
// memory files under one project folder, grouped by provider, with an
// in-browser filter and a glamour preview pane. It satisfies Screen via
// pointer-receiver methods that mutate in place and return the same
// *Browser as their "replacement screen" — the simplest faithful shape for
// a screen whose own state (a textinput, slices, caches) is naturally
// mutable, in contrast to the root Model's own value-semantics choice one
// layer up.
type Browser struct {
	deps BrowserDeps

	memories []memoryfs.Memory
	loadErr  error
	loaded   bool

	orderByRecency bool // o toggles; default true (newest first)

	filtering bool
	filter    textinput.Model

	cursor int // index into the current visibleRows() order

	// now is the Browser's own stored clock: seeded from BrowserDeps.Now at
	// construction, then kept current by every RefreshMsg's own Now field
	// (screen.go) — never read through a closure, so a background tick's
	// advanced clock is always what the next render actually sees.
	now time.Time

	// index is the wiki-link graph over the current listing (Task 7),
	// retained (not just consumed) because Task 12's Reading screen will
	// need the identical graph for link navigation, and because lint.Check
	// below takes it as an input rather than building its own.
	index *links.Index
	// lintFlags is the ⚠ badge's actual source of truth: RepoPath ->
	// "present in lint.Check's results" — spec §8's advisory findings
	// (frontmatter/dangling-link/stale/index-drift), not just dangling
	// links. Derived once per (fingerprint-gated) relint rather than
	// scanned from lint.Check's own []Result on every row render.
	lintFlags       map[string]bool
	lintFingerprint string // last (memories, StaleAfterDays, now-day-bucket) set the lint scan ran over

	// preview memoizes the last glamour-rendered preview: View runs on every
	// keypress and every RefreshMsg (roughly every 2s while idle), and
	// without this, each of those would re-read the selected memory's full
	// body (up to memoryfs.ReadBody's size cap) and re-run glamour over it
	// even when nothing about the selection has changed.
	preview previewCache
}

// previewCache is renderPreview's memoized result, valid only for the exact
// (RepoPath, ModTime, width) it was computed from — any of the three
// changing (a different row selected, the file rewritten, or the pane
// resized) is a cache miss. It deliberately does not key on the Render seam
// itself: a func value has no cheap, correct equality check, so instead
// SetRender clears validity unconditionally, guaranteeing a theme swap
// always forces exactly one fresh render rather than risking a silent stale
// hit keyed on inputs that did not change.
type previewCache struct {
	valid    bool
	repoPath string
	modTime  time.Time
	width    int
	rendered string
}

// NewBrowser builds a ready Browser and performs its first load. Construction
// is this package's one documented exception to "no I/O outside a Cmd":
// deps.List is a local directory walk (memoryfs.List), not a daemon round
// trip, so paying microseconds of synchronous I/O here buys an
// always-populated first frame instead of a guaranteed-empty one until the
// first tick — the same trade the brief calls out for RefreshMsg (see
// refresh).
func NewBrowser(deps BrowserDeps) *Browser {
	filter := textinput.New()
	filter.Placeholder = "filter by name or description…"
	b := &Browser{deps: deps, orderByRecency: true, filter: filter, now: deps.Now}
	b.refresh()
	return b
}

// Title names the browser's breadcrumb segment: the project folder.
func (b *Browser) Title() string {
	return b.deps.Folder
}

// SetStyles installs a new theme. Not part of the Screen interface (which
// stays exactly Update/View/Title so the stack contract cannot drift) —
// the root type-asserts to *Browser and calls this directly on a
// tea.BackgroundColorMsg, the same way it already propagates styles to
// every tab view, so a pushed browser is never left rendering a stale
// palette after a background-color swap.
func (b *Browser) SetStyles(styles theme.Styles) {
	b.deps.Styles = styles
}

// SetRender installs a new markdown-render seam. Not part of the Screen
// interface for the same reason as SetStyles, and propagated the same
// way: the root's glamour renderer is keyed to dark/light (styleName in
// dashboard.go), so a tea.BackgroundColorMsg rebuilds it there and must
// reach an already-pushed browser's preview pane, not just a freshly
// constructed one, or the preview would keep rendering through the style
// that was current when the browser was opened.
func (b *Browser) SetRender(render func(md string, width int) string) {
	b.deps.Render = render
	// A func value cannot be compared for equality, so renderPreview's
	// cache cannot tell "same Render" from "different Render" on its own —
	// unconditionally invalidating here is what forces the very next
	// preview render to actually re-run the new renderer instead of
	// serving a hit computed under the old theme.
	b.preview.valid = false
}

// refresh re-lists the folder's memories and clamps the cursor into range.
// Called at construction and on every RefreshMsg (the root's tick forward):
// listing a memory dir is cheap and keeps the browser live against writes
// an external agent makes while the user is browsing.
//
// The lint pass (frontmatter/dangling-link/stale/index-drift, spec §8)
// additionally reads every memory's full body, which is not free at any
// real project scale — so it only actually re-runs when lintFingerprint
// has changed since the last scan. An idle browsing session then costs one
// relist per tick, not a full lint.Check pass every two seconds.
func (b *Browser) refresh() {
	memories, err := b.deps.List()
	if err != nil {
		b.loadErr = err
		b.loaded = true
		return
	}
	b.memories = memories
	b.loadErr = nil
	b.loaded = true
	b.cursor = clampCursor(b.cursor, len(b.visibleRows()))

	if fingerprint := lintFingerprint(memories, b.deps.StaleAfterDays, b.now); fingerprint != b.lintFingerprint {
		b.index = links.BuildIndex(memories, b.deps.ReadBody)
		results := lint.Check(memories, b.index, b.deps.ReadBody, b.deps.StaleAfterDays, b.now)
		b.lintFlags = make(map[string]bool, len(results))
		for _, result := range results {
			b.lintFlags[result.Memory.RepoPath] = true
		}
		b.lintFingerprint = fingerprint
	}
}

// clampCursor keeps cursor within [0, count) — 0 when count is 0 — the same
// clamp-not-reset policy PaletteModel.refilter applies to its own cursor, so
// a shrinking (or filtered) row set never leaves a stale out-of-range index
// behind.
func clampCursor(cursor, count int) int {
	if count == 0 {
		return 0
	}
	return min(max(cursor, 0), count-1)
}

// lintFingerprint summarizes everything a relint's verdicts can depend on:
// the listing's identity (memoryfs.List's own deterministic (Folder,
// RepoPath) sort order makes a straight concatenation of RepoPath+ModTime
// stable across calls that see the same files), staleAfterDays (a settings
// reload must retrigger the same tick it takes effect), and now's own
// calendar-day bucket (UTC).
//
// The day bucket exists because lint.Check's staleness rule crosses its
// threshold purely as a function of elapsed wall-clock time — a memory can
// go from "not stale" to "stale" while the browser sits open and its own
// ModTime never changes at all. Without something keyed to now in this
// fingerprint, the listing+ModTime component alone would stay identical
// forever once first computed, and the (correctly) skipped relint would
// never run again to notice the crossing — freezing every staleness
// verdict at whatever it was the moment the browser opened. A day-level
// bucket matches the rule's own day-level granularity: it guarantees a
// relint at least once per calendar day even when nothing else changed,
// without forcing the (expensive, full-body) lint pass to re-run on every
// single tick the way keying on the exact instant would.
func lintFingerprint(memories []memoryfs.Memory, staleAfterDays int, now time.Time) string {
	var b strings.Builder
	for _, m := range memories {
		b.WriteString(m.RepoPath)
		b.WriteByte(0)
		b.WriteString(strconv.FormatInt(m.ModTime.UnixNano(), 10))
		b.WriteByte(0)
	}
	b.WriteString(strconv.Itoa(staleAfterDays))
	b.WriteByte(0)
	b.WriteString(now.UTC().Format(time.DateOnly))
	return b.String()
}

// Update handles one message. RefreshMsg (the root's tick forward) stores
// the live clock it carries — see screen.go's RefreshMsg doc for why this,
// not a closure, is how a pushed screen ever observes a later tick's
// advanced time — before re-listing; everything else that is not a
// recognized key is left unhandled, matching the Screen contract's "usually
// itself, nil Cmd" default.
func (b *Browser) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case RefreshMsg:
		b.now = msg.Now
		b.refresh()
		return b, nil
	case tea.KeyPressMsg:
		return b.updateKey(msg)
	}
	return b, nil
}

func (b *Browser) updateKey(msg tea.KeyPressMsg) (Screen, tea.Cmd) {
	if b.filtering {
		return b.updateFiltering(msg)
	}

	switch {
	case keybinding.Matches(msg, DashboardKeys.BrowserBack):
		return b, func() tea.Msg { return PopScreenMsg{} }
	case keybinding.Matches(msg, DashboardKeys.BrowserFilter):
		b.filtering = true
		return b, b.filter.Focus()
	case keybinding.Matches(msg, DashboardKeys.BrowserOrder):
		b.orderByRecency = !b.orderByRecency
		b.cursor = clampCursor(b.cursor, len(b.visibleRows()))
		return b, nil
	case keybinding.Matches(msg, DashboardKeys.Select):
		b.moveCursor(msg.String())
		return b, nil
	}
	return b, nil
}

// updateFiltering handles a keypress while the in-browser filter owns
// input focus: esc clears and exits (consuming the key — no PopScreenMsg,
// so the root does not also pop the screen); arrow keys navigate the
// filtered list, the same "arrows only, letters stay typable" rule
// PaletteModel.Update applies for the identical reason (the filter also
// owns a free-text query); everything else is forwarded to the text input.
func (b *Browser) updateFiltering(msg tea.KeyPressMsg) (Screen, tea.Cmd) {
	if keybinding.Matches(msg, DashboardKeys.Cancel) {
		b.filtering = false
		b.filter.Reset()
		b.filter.Blur()
		b.cursor = clampCursor(b.cursor, len(b.visibleRows()))
		return b, nil
	}
	switch msg.String() {
	case "up", "down":
		b.moveCursor(msg.String())
		return b, nil
	}
	var cmd tea.Cmd
	b.filter, cmd = b.filter.Update(msg)
	b.cursor = clampCursor(b.cursor, len(b.visibleRows()))
	return b, cmd
}

// moveCursor advances or retreats the list cursor for an up/down/k/j key,
// clamped to the current visible row count.
func (b *Browser) moveCursor(key string) {
	rows := len(b.visibleRows())
	switch key {
	case "up", "k":
		if b.cursor > 0 {
			b.cursor--
		}
	case "down", "j":
		if b.cursor < rows-1 {
			b.cursor++
		}
	}
}

// visibleRows returns the filtered memories in render order: grouped by
// provider (alphabetical), newest-first within each group unless
// orderByRecency is false (name order). Recomputed on demand rather than
// cached — cheap at a per-project memory count, and it keeps the filter/
// order toggles simple field flips with no separate invalidation to track.
func (b *Browser) visibleRows() []memoryfs.Memory {
	query := strings.TrimSpace(b.filter.Value())
	filtered := make([]memoryfs.Memory, 0, len(b.memories))
	for _, m := range b.memories {
		if query == "" || fuzzyMatches(query, m.Name, m.Description) {
			filtered = append(filtered, m)
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if filtered[i].Provider != filtered[j].Provider {
			return filtered[i].Provider < filtered[j].Provider
		}
		if b.orderByRecency {
			return filtered[i].ModTime.After(filtered[j].ModTime)
		}
		return strings.ToLower(filtered[i].Name) < strings.ToLower(filtered[j].Name)
	})
	return filtered
}

// fuzzyMatches reports whether query is a case-insensitive subsequence of
// name or description — spec §3's "fuzzy on name + description" for the
// in-browser filter (a distinct, looser contract from §7's global search,
// which fuzzies only its name tier). isSubsequence duplicates the actions
// package's unexported helper of the same shape rather than importing it:
// two independent, small fuzzy matchers over different domains (registry
// actions vs. memory rows), neither depending on the other's package-
// private helper.
func fuzzyMatches(query, name, description string) bool {
	query = strings.ToLower(query)
	return isSubsequence(query, strings.ToLower(name)) || isSubsequence(query, strings.ToLower(description))
}

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

// View renders the browser: a title line, the in-browser filter input when
// active, then either an error/loading/empty notice or the grouped list —
// joined with a glamour preview pane on the right once width clears
// previewMinWidth. width/height come fresh from the root on every call, so
// a terminal resize is handled by construction rather than any cached
// dimension going stale.
func (b *Browser) View(width, height int) string {
	var body strings.Builder
	body.WriteString(sectionTitle(b.deps.Styles, "Memory browser: "+b.deps.Folder))
	body.WriteString("\n\n")
	if b.filtering || b.filter.Value() != "" {
		body.WriteString(b.deps.Styles.Dim.Render("filter: " + b.filter.View()))
		body.WriteString("\n\n")
	}

	switch {
	case b.loadErr != nil:
		fmt.Fprintf(&body, "memories unavailable: %v", b.loadErr)
		return strings.TrimRight(body.String(), "\n")
	case !b.loaded:
		body.WriteString(b.deps.Styles.Dim.Render("loading memories…"))
		return strings.TrimRight(body.String(), "\n")
	case len(b.memories) == 0:
		body.WriteString(b.deps.Styles.Dim.Render("no memories yet in this project"))
		return strings.TrimRight(body.String(), "\n")
	}

	rows := b.visibleRows()
	if len(rows) == 0 {
		body.WriteString(b.deps.Styles.Dim.Render("no memories match the filter"))
		return strings.TrimRight(body.String(), "\n")
	}

	listContent := b.renderList(rows, b.listRowBudget(rows, height))
	if width < previewMinWidth {
		body.WriteString(listContent)
		return strings.TrimRight(body.String(), "\n")
	}

	previewWidth := width - listPaneWidth - 2
	preview := b.renderPreview(rows[b.cursor], previewWidth)
	listBlock := lipgloss.NewStyle().Width(listPaneWidth).MaxWidth(listPaneWidth).Render(listContent)
	previewBlock := lipgloss.NewStyle().Width(previewWidth).MaxWidth(previewWidth).Render(preview)
	body.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, listBlock, "  ", previewBlock))
	return strings.TrimRight(body.String(), "\n")
}

// listRowBudget computes how many rows renderList may window over so that
// View's total rendered line count — title line + its trailing blank,
// optionally the filter line + its own trailing blank, the windowed rows,
// and however many provider-group header lines the visible window turns
// out to include — never exceeds height.
//
// The header count is circular: it depends on which rows land in the
// window, which depends on the row budget being computed here. Rather than
// iterate to a fixed point, this reserves the worst case up front: no
// contiguous window can show more provider-header transitions than there
// are distinct providers in the full (unwindowed) listing, so subtracting
// that count once is always sufficient to keep the total within height,
// even though it is occasionally more conservative than the tightest
// possible fit (a window touching only one or two of several providers
// could have shown a couple more rows).
func (b *Browser) listRowBudget(rows []memoryfs.Memory, height int) int {
	chrome := 2 // title line + its trailing blank
	if b.filtering || b.filter.Value() != "" {
		chrome += 2 // filter line + its own trailing blank
	}
	budget := height - chrome - countDistinctProviders(rows)
	// Always show at least the cursor's own row: a budget of zero would
	// otherwise hit visibleWindow's "height <= 0" identity-window branch
	// and show everything, which is the one outcome guaranteed to
	// overflow an already-tight height rather than degrade gracefully.
	return max(budget, 1)
}

// countDistinctProviders reports how many distinct Provider values appear
// across rows — listRowBudget's worst-case bound on header lines any
// window into rows could contain.
func countDistinctProviders(rows []memoryfs.Memory) int {
	seen := make(map[string]struct{})
	for _, m := range rows {
		seen[m.Provider] = struct{}{}
	}
	return len(seen)
}

// renderList renders rows grouped by provider (a header line whenever the
// provider changes from the previous row — safe because visibleRows always
// sorts provider-major) with a cursor marker, an optional ⚠ lint badge, a
// truncated description, and a relative modified time.
//
// rowBudget (from listRowBudget) windows rows around the cursor
// (visibleWindow) so a project with more memories than fit the pane never
// lets the cursor walk off-screen (spec §3). The provider-transition
// tracking below runs fresh over just the visible window, so the top
// visible row always gets a header naming its provider — even if that
// repeats one that scrolled past several screens up. A provider group that
// lies entirely outside the window (both its rows and its header) simply
// is not shown; nothing tracks that separately, so a header can scroll off
// exactly like any other row.
func (b *Browser) renderList(rows []memoryfs.Memory, rowBudget int) string {
	start, end := visibleWindow(b.cursor, len(rows), rowBudget)
	rows = rows[start:end]

	var lines []string
	lastProvider := ""
	for i, m := range rows {
		row := start + i
		if m.Provider != lastProvider {
			lines = append(lines, b.deps.Styles.Header.Render(m.Provider))
			lastProvider = m.Provider
		}
		marker := "  "
		if row == b.cursor {
			marker = "> "
		}
		badge := ""
		if b.lintFlags[m.RepoPath] {
			badge = " " + b.deps.Styles.Warn.Render("⚠")
		}
		line := fmt.Sprintf("%s%s%s — %s (%s)", marker, m.Name, badge,
			b.deps.Styles.Dim.Render(truncate(m.Description, descriptionTruncateAt)),
			relativeTime(m.ModTime, b.now))
		if row == b.cursor {
			line = b.deps.Styles.Selected.Render(line)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// visibleWindow returns the [start, end) bounds of a height-row slice of a
// total-length list that keeps index cursor inside it, centering cursor
// within the window when the full list overflows height and clamping both
// ends to [0, total). height <= 0 or a list that already fits within it is
// the identity window (the whole list — no scrolling needed).
func visibleWindow(cursor, total, height int) (start, end int) {
	if height <= 0 || total <= height {
		return 0, total
	}
	start = max(cursor-height/2, 0)
	end = start + height
	if end > total {
		end = total
		start = end - height
	}
	return start, end
}

// renderPreview markdown-renders the selected memory's body through the
// injected Render seam, or a plain unavailable notice if reading its body
// failed (a file removed mid-browse, or over memoryfs's size cap).
//
// Checks the render cache first (see previewCache's doc for the key and why
// Render itself is not part of it). A read/render failure is deliberately
// never cached: an error is rare enough that re-attempting on every render
// costs nothing worth memoizing, and caching it would risk a stale error
// notice outliving a since-fixed transient failure.
func (b *Browser) renderPreview(selected memoryfs.Memory, width int) string {
	if b.preview.valid && b.preview.repoPath == selected.RepoPath &&
		b.preview.modTime.Equal(selected.ModTime) && b.preview.width == width {
		return b.preview.rendered
	}

	content, err := b.deps.ReadBody(selected)
	if err != nil {
		return b.deps.Styles.Fail.Render(fmt.Sprintf("preview unavailable: %v", err))
	}

	rendered := content
	if b.deps.Render != nil {
		rendered = b.deps.Render(content, width)
	}
	b.preview = previewCache{
		valid:    true,
		repoPath: selected.RepoPath,
		modTime:  selected.ModTime,
		width:    width,
		rendered: rendered,
	}
	return rendered
}

// truncate shortens s to at most maxRunes runes, marking the cut with an
// ellipsis — rune-aware so a multi-byte character is never split.
func truncate(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}

// relativeTime renders t relative to now in the coarsest unit that keeps it
// a single digit-friendly number — "—" for a zero ModTime (should not
// happen for a real file, but a defensive fallback costs nothing).
func relativeTime(t, now time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
	}
}
