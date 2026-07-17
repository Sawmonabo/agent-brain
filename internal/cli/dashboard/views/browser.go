package views

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	keybinding "charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

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

// mouseWheelScrollLines is how many preview lines one wheel notch scrolls — a
// few lines per notch, the terminal-native step a side preview scrolls by (the
// convention gh-dash and lazygit use for a wheel over a preview beside a list).
const mouseWheelScrollLines = 3

// descriptionTruncateAt bounds a row's rendered description so one
// unusually long frontmatter line cannot blow out the list pane's width. It is
// the coarse upper bound fitListRow applies ON TOP of the per-row budget, so a
// very wide pane shows a readable preview rather than an endless one.
const descriptionTruncateAt = 40

// descriptionMinWidth is the fewest columns a row's description is worth
// showing: any narrower and the ellipsised sliver reads as noise, so the row
// drops the description and lets the name and age keep the space (spec §3).
const descriptionMinWidth = 8

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
	// (dashboard.go), threaded onward into every Reading screen this
	// browser pushes (openReading). A nil Render (as some tests that do not
	// exercise the preview pane leave it) falls back to the raw body text.
	Render func(md string, width int) string
	// Data is the read-only version surface (spec §6): threaded into every
	// History screen this browser opens (h on a row, or a deleted-recovery
	// row) and used directly for the folder-wide scan that finds deleted
	// memories (x). A nil Data disables both without a panic — the h/x rows
	// still render, the scan Cmd is a no-op (deletedScanCmd), and a pushed
	// History issues no fetch either (its versionsCmd/blobCmd nil-guard), so it
	// simply sits on its loading notice; production always wires it
	// (dashboard.go's buildBrowserDeps).
	Data HistoryDataSource
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
	// retained (not just consumed) because openReading threads the
	// identical graph into every pushed Reading screen for link/backlink
	// navigation, and because lint.Check below takes it as an input rather
	// than building its own.
	index *links.Index
	// lintFlags is the ⚠ badge's actual source of truth: RepoPath ->
	// "present in lint.Check's results" — spec §8's advisory findings
	// (frontmatter/dangling-link/stale/index-drift), not just dangling
	// links. Derived once per (fingerprint-gated) relint rather than
	// scanned from lint.Check's own []Result on every row render.
	lintFlags       map[string]bool
	lintFingerprint string // last (memories, StaleAfterDays, now-day-bucket) set the lint scan ran over
	// lintResults retains the full lint.Check output the badge flags are derived
	// from, not just the RepoPath set: the insights screen (i) tallies issues
	// per rule from it (spec §9), handed the browser's current results rather
	// than re-scanning. Refreshed on the same fingerprint gate as lintFlags.
	lintResults []lint.Result
	// lintIssues indexes lintResults by RepoPath so the preview pane's header
	// zone (previewHeaderLines) can look up the hovered memory's own issues
	// without a linear scan over lintResults on every render. Built in the
	// same loop, over the same results slice, as lintFlags — so the ⚠ badge
	// and the reasons displayed for it can never disagree about which
	// memories are flagged.
	lintIssues map[string][]lint.Issue

	// preview gates the glamour render feeding previewViewport: View runs on
	// every keypress and every RefreshMsg (roughly every 2s while idle), and
	// without this cache each of those would re-read the selected memory's full
	// body (up to memoryfs.ReadBody's size cap) and re-run glamour over it even
	// when nothing about the selection has changed. On a cache hit the viewport
	// already holds the render, so nothing re-renders.
	preview previewCache
	// previewViewport is the scrollable preview pane: a long memory's
	// body scrolls WITHIN this window instead of growing the frame past its
	// height budget and shoving the root's footer off the terminal. It mirrors
	// the reading view's viewport (reading.go). Its keymap is swapped by focus:
	// the unfocused browserPreviewKeyMap (ctrl+d/u + pgup/pgdown only, so j/k
	// stay the list cursor's) while the list is focused, and the fuller
	// browserPreviewFocusedKeyMap (the reading view's j/k + ctrl+d/u +
	// pgup/pgdown) while previewFocused.
	previewViewport viewport.Model
	// previewShown records whether the last View drew the preview pane (width
	// cleared previewMinWidth, in the normal non-filter/non-deleted body).
	// updateKey reads it to route the scroll keys: Update has no width of its
	// own — View is the only place the pane's visibility is known — so this
	// bridges them, keeping the scroll keys inert whenever no pane is on screen.
	previewShown bool
	// previewFocused records whether the preview pane holds keyboard focus:
	// Tab toggles it (only when a preview is on screen), and while set the
	// reading view's full scroll keymap drives the pane (j/k, ctrl+d/u,
	// pgup/pgdown, g/G) instead of the list cursor — the lazygit-style
	// focus-the-preview idiom. Effective focus is always previewFocused AND
	// previewShown: a narrow resize that drops the pane leaves this bool set but
	// inert, so updateKey gates the focused key block on both and the list can
	// never go dead under a dangling focus. A click-to-focus affordance reads it
	// too.
	previewFocused bool

	// listLineRows maps each rendered list-block line to the visibleRows index it
	// displays — -1 for a provider-header line, which selects nothing — and
	// listTopLines is how many browser-body lines sit above that block. Both are
	// recorded during View (renderList fills the map; a frame that draws no list —
	// a notice or the deleted view — leaves it empty), the same last-rendered-frame
	// contract as previewShown: the map describes the frame the user is looking at,
	// so a click and the pixels under it can never disagree. updateMouseClick reads
	// them to turn a screen-local click Y into the memory it landed on.
	listLineRows []int
	listTopLines int

	// Deleted-recovery mode (spec §6's x): a folder-wide history scan surfaces
	// every path that some past version touched but HEAD no longer has, so a
	// deleted memory stays reachable — enter/h on one of these rows opens its
	// History screen (with no live side) to restore it. showDeleted swaps the
	// browser body for this list; the scan is issued on toggle-on and its
	// result lands as a folder-wide HistoryVersionsMsg (RepoPath "").
	showDeleted   bool
	deletedLoaded bool
	deletedErr    error
	// deletedVersions is the last folder-wide scan's versions, retained (not
	// just consumed into deletedPaths) so a RefreshMsg can re-subtract the
	// CURRENT on-disk listing without a fresh daemon scan — a restored memory
	// drops out of the deleted list within a tick (redetectDeleted) — and so its
	// length drives the truncation disclosure (deletedView).
	deletedVersions []api.HistoryVersion
	deletedPaths    []string
	deletedCursor   int
}

// previewCache is syncPreview's refresh gate, valid only for the exact
// (RepoPath, ModTime, width) the viewport's current content was rendered from —
// any of the three changing (a different row selected, the file rewritten, or
// the pane resized) is a cache miss that re-renders and re-fills the viewport.
// It holds the cache KEY only, not the rendered string: the previewViewport owns
// the content now, so on a hit there is nothing to return, only a re-render to
// skip. It deliberately does not key on the Render seam itself: a func value has
// no cheap, correct equality check, so instead SetRender clears validity
// unconditionally, guaranteeing a theme swap always forces exactly one fresh
// render rather than risking a silent stale hit keyed on inputs that did not
// change.
type previewCache struct {
	valid    bool
	repoPath string
	modTime  time.Time
	width    int
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
	previewViewport := viewport.New()
	previewViewport.KeyMap = browserPreviewKeyMap()
	b := &Browser{deps: deps, orderByRecency: true, filter: filter, previewViewport: previewViewport, now: deps.Now}
	b.refresh()
	return b
}

// browserPreviewKeyMap binds only the preview pane's own scroll set: ctrl+d/u
// (half page) and pgup/pgdown (page), the gh-dash convention for a preview
// beside a focused list and the same keys the reading view's viewport takes.
// Up/Down/Left/Right are left unbound (a keyless binding never matches) — the
// deliberate deviation from the reading view's keymap, whose j/k DO scroll its
// full-screen viewport: here j/k/up/down are the list cursor's (Select), so the
// preview must never claim them.
func browserPreviewKeyMap() viewport.KeyMap {
	return viewport.KeyMap{
		HalfPageUp:   keybinding.NewBinding(keybinding.WithKeys("ctrl+u")),
		HalfPageDown: keybinding.NewBinding(keybinding.WithKeys("ctrl+d")),
		PageUp:       keybinding.NewBinding(keybinding.WithKeys("pgup")),
		PageDown:     keybinding.NewBinding(keybinding.WithKeys("pgdown")),
		Up:           keybinding.NewBinding(),
		Down:         keybinding.NewBinding(),
		Left:         keybinding.NewBinding(),
		Right:        keybinding.NewBinding(),
	}
}

// browserPreviewFocusedKeyMap is the preview pane's keymap while it holds
// keyboard focus (Tab). It mirrors the reading view's viewport keymap
// (reading.go's readingViewportKeyMap) exactly — up/k line up, down/j line
// down, ctrl+u/d half page, pgup/pgdown page — so a focused preview scrolls
// with the same full toolkit the reading view offers. It is installed only
// while previewFocused; the unfocused browserPreviewKeyMap (no j/k) is restored
// the moment focus returns to the list, so j/k drive the list cursor whenever
// the list is focused. Left/Right stay unbound (a keyless binding never
// matches); g/G are handled by updateKey directly (GotoTop/GotoBottom), exactly
// as reading.go handles them — the viewport exposes the methods but binds no
// keys to them.
func browserPreviewFocusedKeyMap() viewport.KeyMap {
	return viewport.KeyMap{
		Up:           keybinding.NewBinding(keybinding.WithKeys("up", "k")),
		Down:         keybinding.NewBinding(keybinding.WithKeys("down", "j")),
		HalfPageUp:   keybinding.NewBinding(keybinding.WithKeys("ctrl+u")),
		HalfPageDown: keybinding.NewBinding(keybinding.WithKeys("ctrl+d")),
		PageUp:       keybinding.NewBinding(keybinding.WithKeys("pgup")),
		PageDown:     keybinding.NewBinding(keybinding.WithKeys("pgdown")),
		Left:         keybinding.NewBinding(),
		Right:        keybinding.NewBinding(),
	}
}

// blurPreview returns keyboard focus to the list and restores the unfocused
// preview keymap, so j/k drive the list cursor again. Called on Tab/Esc out of
// a focused pane and defensively wherever a transition leaves the normal body
// (entering the filter or the deleted-recovery list): a dangling focus must
// never outlive the pane it scrolled. Restoring the keymap here is what lets
// the focused block install browserPreviewFocusedKeyMap freely — it is always
// wound back the moment focus clears.
func (b *Browser) blurPreview() {
	b.previewFocused = false
	b.previewViewport.KeyMap = browserPreviewKeyMap()
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
		b.lintResults = results
		b.lintFlags = make(map[string]bool, len(results))
		b.lintIssues = make(map[string][]lint.Issue, len(results))
		for _, result := range results {
			b.lintFlags[result.Memory.RepoPath] = true
			b.lintIssues[result.Memory.RepoPath] = result.Issues
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
		// While the deleted list is showing, re-subtract the freshly relisted
		// on-disk set from the retained scan so a memory restored (or otherwise
		// recreated) since the scan drops off within a tick, without waiting for
		// an x-toggle rescan. Only over a successful scan — an errored or not-
		// yet-loaded one has no versions to re-derive from.
		if b.showDeleted && b.deletedLoaded && b.deletedErr == nil {
			b.redetectDeleted()
		}
		return b, nil
	case HistoryVersionsMsg:
		// Only the folder-wide deleted scan (RepoPath "") is ours: a per-memory
		// History screen's own version fetch (RepoPath set) can be forwarded
		// here after that screen pops, and must be dropped, never mistaken for
		// the deleted set — the staleness guard the History screen applies in
		// the other direction.
		if msg.Folder != b.deps.Folder || msg.RepoPath != "" {
			return b, nil
		}
		b.adoptDeletedScan(msg)
		return b, nil
	case tea.KeyPressMsg:
		return b.updateKey(msg)
	case tea.MouseWheelMsg:
		b.updateMouseWheel(msg)
		return b, nil
	case tea.MouseClickMsg:
		b.updateMouseClick(msg)
		return b, nil
	}
	return b, nil
}

func (b *Browser) updateKey(msg tea.KeyPressMsg) (Screen, tea.Cmd) {
	if b.filtering {
		return b.updateFiltering(msg)
	}
	if b.showDeleted {
		return b.updateDeleted(msg)
	}

	// Preview-focus mode (spec §3): while the preview holds focus AND is
	// actually on screen, the reading view's full scroll keymap drives the pane
	// instead of the list. Gated on previewShown too — a narrow resize drops the
	// pane while previewFocused lingers, and without the previewShown half of the
	// guard this block would keep swallowing j/k/Tab/Esc into an off-screen
	// viewport, leaving the list cursor dead. Tab and Esc hand focus back to the
	// list; g/G jump the pane's ends (GotoTop/GotoBottom — the viewport exposes
	// the methods but binds no keys, exactly as reading.go handles them); y copies
	// the previewed body, reached here rather than only in the list-focused switch
	// below so the focused footer's advertised "y copy" is honest and not a dead
	// key; every other key runs through the focused keymap installed just below.
	if b.previewFocused && b.previewShown {
		switch {
		case keybinding.Matches(msg, DashboardKeys.BrowserFocusPreview),
			keybinding.Matches(msg, DashboardKeys.BrowserBack):
			b.blurPreview()
			return b, nil
		case msg.String() == "g":
			b.previewViewport.GotoTop()
			return b, nil
		case msg.String() == "G":
			b.previewViewport.GotoBottom()
			return b, nil
		case keybinding.Matches(msg, DashboardKeys.BrowserCopy):
			return b, b.copyRequest()
		}
		b.previewViewport.KeyMap = browserPreviewFocusedKeyMap()
		var cmd tea.Cmd
		b.previewViewport, cmd = b.previewViewport.Update(msg)
		return b, cmd
	}

	switch {
	case keybinding.Matches(msg, DashboardKeys.BrowserBack):
		return b, func() tea.Msg { return PopScreenMsg{} }
	case keybinding.Matches(msg, DashboardKeys.BrowserFocusPreview):
		// Tab focuses the preview pane for full-key scrolling, but only when a
		// pane is on screen; with no preview (narrow width) it stays inert rather
		// than arming a focus the user cannot see. The focused keymap is installed
		// lazily on the first focused keystroke (the block above), so entering
		// focus is a pure state flip here.
		if b.previewShown {
			b.previewFocused = true
		}
		return b, nil
	case keybinding.Matches(msg, DashboardKeys.BrowserRead):
		return b, b.openReading()
	case keybinding.Matches(msg, DashboardKeys.BrowserHistory):
		return b, b.openHistory()
	case keybinding.Matches(msg, DashboardKeys.BrowserShowDeleted):
		return b, b.enterDeleted()
	case keybinding.Matches(msg, DashboardKeys.BrowserInsights):
		return b, b.openInsights()
	case keybinding.Matches(msg, DashboardKeys.BrowserCopy):
		// y copies the selected memory's raw body to the system clipboard via
		// OSC52 (the root's CopyMemoryMsg handler) — the feature-full remedy for
		// native drag-select being suppressed while the preview holds mouse mode,
		// and a copy that also carries over SSH/tmux/WSL2.
		return b, b.copyRequest()
	case keybinding.Matches(msg, DashboardKeys.BrowserFilter):
		// A dangling preview focus (previewFocused set while previewShown was
		// false — a narrow resize) must not survive into filter mode, where the
		// text input owns every key.
		b.blurPreview()
		b.filtering = true
		return b, b.filter.Focus()
	case keybinding.Matches(msg, DashboardKeys.BrowserOrder):
		b.orderByRecency = !b.orderByRecency
		b.cursor = clampCursor(b.cursor, len(b.visibleRows()))
		return b, nil
	case keybinding.Matches(msg, DashboardKeys.BrowserEdit):
		return b, b.selectedRequest(func(memory memoryfs.Memory) tea.Msg { return EditRequestMsg{Memory: memory} })
	case keybinding.Matches(msg, DashboardKeys.BrowserRename):
		return b, b.selectedRequest(func(memory memoryfs.Memory) tea.Msg { return RenameRequestMsg{Memory: memory} })
	case keybinding.Matches(msg, DashboardKeys.BrowserDelete):
		return b, b.selectedRequest(func(memory memoryfs.Memory) tea.Msg { return DeleteRequestMsg{Memory: memory} })
	case keybinding.Matches(msg, DashboardKeys.BrowserNew):
		return b, b.newRequest()
	case keybinding.Matches(msg, DashboardKeys.Select):
		b.moveCursor(msg.String())
		return b, nil
	}
	// Preview-pane scroll: ctrl+d/u and pgup/pgdown reach the preview viewport
	// (its restricted keymap binds only those; j/k/up/down were consumed by
	// Select above for the list cursor). Gated on previewShown so the keys stay
	// inert with no pane on screen — this branch is reached only in the normal
	// body (filtering and deleted modes returned above), so it never contends
	// with the filter input or the deleted list.
	if b.previewShown {
		var cmd tea.Cmd
		b.previewViewport, cmd = b.previewViewport.Update(msg)
		return b, cmd
	}
	return b, nil
}

// Selected reports the memory under the cursor in the current visible
// order, or ok=false with nothing to select (an empty project, or a filter
// matching nothing). Exported for the root, which reads it two ways: the
// flow-availability gates (fact-class ∧ …) and nothing else — it is not
// part of the Screen interface, the same root-reaches-the-concrete-type
// seam as SetStyles/SetRender.
func (b *Browser) Selected() (memoryfs.Memory, bool) {
	rows := b.visibleRows()
	if len(rows) == 0 {
		return memoryfs.Memory{}, false
	}
	return rows[b.cursor], true
}

// Units exposes the folder's enrolled units for the root's browser-new
// availability gate: a folder with no units has nowhere to receive a new
// memory, so the n row renders struck instead of lit-but-refusing. Same
// root-reaches-the-concrete-type seam as Selected.
func (b *Browser) Units() []api.UnitInfo {
	return b.deps.Units
}

// WantsMouse reports whether the browser currently wants mouse reporting turned
// on — true exactly while a preview pane is on screen, so the wheel can scroll it
// and a click can focus it (spec §3). It reflects the LAST View: previewShown is
// set during render, which is precisely when the root reads this — the root calls
// top.View before it builds the tea.View whose MouseMode this gates, so the flag
// and the frame it scopes always describe the same render. The same
// root-reaches-the-concrete-type seam as Selected/Units, not part of the Screen
// interface.
func (b *Browser) WantsMouse() bool {
	return b.previewShown
}

// PreviewFocused reports whether the preview pane holds EFFECTIVE keyboard focus
// — previewFocused AND previewShown, the exact compound gate updateKey's focused
// block uses (a focus armed then stranded off-screen by a narrow resize reads as
// false here, never advertising a focus the reader cannot act on). The root reads
// it to swap the footer to the preview-focused binding set and the pane's own
// render reads it to draw the focus cue, so the footer, the cue, and the block
// that actually routes the keys can never disagree about whether the preview is
// focused. Same root-reaches-the-concrete-type seam as Selected/WantsMouse, not
// part of the Screen interface.
func (b *Browser) PreviewFocused() bool {
	return b.previewFocused && b.previewShown
}

// selectedRequest builds the Cmd that emits wrap's flow-request message for
// the selected memory — e/r/d share it — or nil with no row selected. The
// browser only ever emits; the root's handler owns every gate (class,
// editor, session, quiesce) and the flow itself (screen.go's request-message
// docs).
func (b *Browser) selectedRequest(wrap func(memoryfs.Memory) tea.Msg) tea.Cmd {
	selected, ok := b.Selected()
	if !ok {
		return nil
	}
	return func() tea.Msg { return wrap(selected) }
}

// copyRequest builds the Cmd that reads the selected memory's body and emits
// CopyMemoryMsg for the root to write to the clipboard (spec §3's y) — or nil
// with no row selected. Unlike selectedRequest (e/r/d, which carry only the
// already-listed memory), copy needs the full body, and ReadBody is fallible
// I/O — a file deleted or made unreadable since the listing — so the read runs
// INSIDE the Cmd and a failure surfaces as a toast rather than silently copying
// an empty body. The raw source is copied, never the glamour-styled preview:
// raw markdown is what a user expects to paste elsewhere. This is the async
// twin of the reading view's Y, whose body is already resident (reading.go).
func (b *Browser) copyRequest() tea.Cmd {
	selected, ok := b.Selected()
	if !ok {
		return nil
	}
	return func() tea.Msg {
		body, err := b.deps.ReadBody(selected)
		if err != nil {
			return ToastMsg{Text: fmt.Sprintf("copy failed: %v", err)}
		}
		return CopyMemoryMsg{Body: body, Label: selected.Name}
	}
}

// newRequest builds the Cmd that emits NewRequestMsg. Unlike e/r/d it needs
// no selection — n on an empty project is exactly how its first memory gets
// created — and it carries the cursor row's provider ("" with no rows) so
// the root places the new file in the unit the user is actually looking at.
func (b *Browser) newRequest() tea.Cmd {
	request := NewRequestMsg{Folder: b.deps.Folder, Units: b.deps.Units}
	if selected, ok := b.Selected(); ok {
		request.Provider = selected.Provider
	}
	return func() tea.Msg { return request }
}

// openReading pushes the selected memory's reading view (spec §4's enter),
// or does nothing with no row to open. ReadingDeps is filled from the
// browser's OWN deps — including the link index, shared rather than
// rebuilt, so the reading view resolves links and backlinks from exactly
// the graph the browser last indexed. Constructing the Reading here (its
// synchronous body read included) mirrors how the root itself builds a
// Browser on OpenFolderMsg; the Cmd merely delivers the finished screen to
// the root's stack.
func (b *Browser) openReading() tea.Cmd {
	rows := b.visibleRows()
	if len(rows) == 0 {
		return nil
	}
	reading := NewReading(ReadingDeps{
		Memory:   rows[b.cursor],
		Index:    b.index,
		ReadBody: b.deps.ReadBody,
		Render:   b.deps.Render,
		Styles:   b.deps.Styles,
		Data:     b.deps.Data,
		Now:      b.now,
	})
	return func() tea.Msg { return PushScreenMsg{Screen: reading} }
}

// openHistory pushes the selected memory's version-history screen (spec §6's
// h), or nothing with no row to open. Its Live seam reads the memory's
// provider file through the browser's own ReadBody, so the diff-vs-live view
// sees exactly what the browser preview does. No I/O here: the version fetch
// runs later as the root-issued InitCmd.
func (b *Browser) openHistory() tea.Cmd {
	memory, ok := b.Selected()
	if !ok {
		return nil
	}
	history := NewHistory(HistoryDeps{
		Memory:   memory,
		Folder:   b.deps.Folder,
		RepoPath: memory.RepoPath,
		Live:     func() (string, error) { return b.deps.ReadBody(memory) },
		Data:     b.deps.Data,
		Render:   b.deps.Render,
		Styles:   b.deps.Styles,
		Now:      b.now,
	})
	return func() tea.Msg { return PushScreenMsg{Screen: history} }
}

// openInsights pushes the project's insights screen (spec §9's i): fleet-wide
// stats over this one folder. It hands the browser's OWN current listing and
// lint results (the brief's "pass, don't re-walk" — the browser stays the live
// view of the tree, the insights a snapshot of it) plus the version seam the
// activity stats derive from. No I/O here: the one folder-wide history fetch
// runs later as the root-issued InitCmd. Unlike openHistory it needs no
// selection — insights summarise the whole folder, even an empty one.
func (b *Browser) openInsights() tea.Cmd {
	insights := NewInsights(InsightsDeps{
		Folder:   b.deps.Folder,
		Memories: b.memories,
		Lint:     b.lintResults,
		Data:     b.deps.Data,
		Styles:   b.deps.Styles,
		Now:      b.now,
	})
	return func() tea.Msg { return PushScreenMsg{Screen: insights} }
}

// updateFiltering handles a keypress while the in-browser filter owns
// input focus: esc clears and exits (consuming the key — no PopScreenMsg,
// so the root does not also pop the screen); arrow keys navigate the
// filtered list, the same "arrows only, letters stay typable" rule
// PaletteModel.Update applies for the identical reason (the filter also
// owns a free-text query); enter opens the selected match — esc clears the
// whole filter, so it must never be the only path from a filtered row to
// reading it; everything else is forwarded to the text input.
func (b *Browser) updateFiltering(msg tea.KeyPressMsg) (Screen, tea.Cmd) {
	if keybinding.Matches(msg, DashboardKeys.Cancel) {
		b.filtering = false
		b.filter.Reset()
		b.filter.Blur()
		b.cursor = clampCursor(b.cursor, len(b.visibleRows()))
		return b, nil
	}
	if keybinding.Matches(msg, DashboardKeys.BrowserRead) {
		return b, b.openReading()
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

// overPreview reports whether column x falls in the preview pane. Column geometry
// mirrors View: the list occupies [0, listPaneWidth) and the preview begins after
// the two-space gap at listPaneWidth+2. Both the root and the browser share an
// X-origin of 0 — the root joins its own chrome vertically, adding no horizontal
// offset — so a raw event column routes with no absolute offset. Gated on
// previewShown so a column in the preview band while no pane is drawn (narrow
// width, or the deleted list) reads as over the list, never a phantom pane.
func (b *Browser) overPreview(x int) bool {
	return b.previewShown && x >= listPaneWidth+2
}

// updateMouseWheel routes a wheel notch by the column it happened over. Over the
// preview it hover-scrolls the pane a few lines straight on the viewport —
// bypassing the focus keymap, so it works whether or not the pane holds focus,
// and deliberately never changing focus: wheel is a hover affordance, only a
// click focuses (the terminal-native "scroll what is under the pointer"). Over
// the list it nudges the list cursor one row, exactly as j/k do.
func (b *Browser) updateMouseWheel(msg tea.MouseWheelMsg) {
	// Inert outside the normal body — the filter input and the deleted list own
	// their own keys and cursor, the two modes updateKey itself bails to before
	// the list/preview. A stale previewShown from the last normal render must not
	// let a wheel scroll the hidden preview or leak the memory-list cursor.
	if b.filtering || b.showDeleted {
		return
	}
	mouse := msg.Mouse()
	if b.overPreview(mouse.X) {
		switch mouse.Button {
		case tea.MouseWheelDown:
			b.previewViewport.ScrollDown(mouseWheelScrollLines)
		case tea.MouseWheelUp:
			b.previewViewport.ScrollUp(mouseWheelScrollLines)
		}
		return
	}
	switch mouse.Button {
	case tea.MouseWheelDown:
		b.moveCursor("down")
	case tea.MouseWheelUp:
		b.moveCursor("up")
	}
}

// updateMouseClick handles a mouse button click. Only a left-click matters. A
// click over the preview focuses that pane (the mouse counterpart of Tab); a
// click over the list blurs the preview and moves the list cursor to the memory
// on the clicked row. The focus arms mirror Task 2's focus primitives rather than
// a bare `previewFocused = overPreview(x)` assignment — on a click-to-blur that
// bare form would strand the lazily-installed focused keymap and break the
// invariant that leaving focus restores the unfocused keymap (so j/k drive the
// list again).
//
// Row selection reads the render-time hit-map renderList recorded for the last
// frame (listLineRows/listTopLines). The browser is composed by the root, which
// now rebases a stack-bound mouse event to screen-local rows (translateStackMouse)
// before forwarding it, so the browser's own chrome above the list is all that
// stands between the event's Y and the map — no absolute Y origin to learn. A
// provider-header line (-1) or an out-of-range Y selects nothing.
func (b *Browser) updateMouseClick(msg tea.MouseClickMsg) {
	// Inert outside the normal body, for the same reason as the wheel: the filter
	// input and the deleted list own their own keys, so a click must not reach in
	// to focus a preview pane the reader cannot act on there.
	if b.filtering || b.showDeleted {
		return
	}
	mouse := msg.Mouse()
	if mouse.Button != tea.MouseLeft {
		return
	}
	if b.overPreview(mouse.X) {
		b.previewFocused = true // like Tab: a pure flip; the focused keymap installs lazily on the next focused key
		return
	}
	b.blurPreview() // like Esc: clears focus AND restores the unfocused keymap (Task 2's blur invariant)
	// Screen-local Y minus the browser's own chrome above the list is the index
	// into the recorded hit-map; a header line (-1) or an out-of-range click
	// (below the last row, or a stale line past this frame's list) selects nothing.
	line := mouse.Y - b.listTopLines
	if line < 0 || line >= len(b.listLineRows) {
		return
	}
	if row := b.listLineRows[line]; row >= 0 {
		b.cursor = row
	}
}

// updateDeleted owns the keyboard while the deleted-recovery list is showing:
// esc or x returns to the normal browser (consumed locally — esc must not also
// pop the browser off the stack, the screen.go ordering rule), enter or h
// opens the selected deleted path's History screen, and the cursor keys move
// within the list.
func (b *Browser) updateDeleted(msg tea.KeyPressMsg) (Screen, tea.Cmd) {
	switch {
	case keybinding.Matches(msg, DashboardKeys.BrowserBack),
		keybinding.Matches(msg, DashboardKeys.BrowserShowDeleted):
		b.showDeleted = false
		return b, nil
	case keybinding.Matches(msg, DashboardKeys.BrowserRead),
		keybinding.Matches(msg, DashboardKeys.BrowserHistory):
		return b, b.openDeletedHistory()
	case keybinding.Matches(msg, DashboardKeys.Select):
		b.moveDeletedCursor(msg.String())
		return b, nil
	}
	return b, nil
}

// enterDeleted switches to the deleted-recovery list and issues the folder-
// wide history scan that populates it. The scan runs even if one ran before —
// a memory deleted since the last scan should appear.
func (b *Browser) enterDeleted() tea.Cmd {
	// Leaving the normal body drops any lingering preview focus (a narrow-resize
	// dangling focus): the deleted-recovery list owns its own cursor keys.
	b.blurPreview()
	b.showDeleted = true
	b.deletedLoaded = false
	b.deletedErr = nil
	b.deletedCursor = 0
	return b.deletedScanCmd()
}

// deletedScanCmd fetches the folder's whole history (path "" — folder-wide
// mode, which populates each version's changed Paths) so adoptDeletedScan can
// subtract the on-disk set. A nil Data (a test that never wired it) yields no
// Cmd, leaving the list on its loading notice rather than panicking.
func (b *Browser) deletedScanCmd() tea.Cmd {
	data, folder := b.deps.Data, b.deps.Folder
	if data == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
		defer cancel()
		resp, err := data.History(ctx, folder, "", historyVersionLimit)
		return HistoryVersionsMsg{Folder: folder, RepoPath: "", Versions: resp.Versions, Err: err}
	}
}

// adoptDeletedScan records a folder-wide history scan and derives the deleted
// set from it. It retains the scan's versions (redetectDeleted re-subtracts the
// live on-disk set from them on later refreshes) rather than only the derived
// paths.
func (b *Browser) adoptDeletedScan(msg HistoryVersionsMsg) {
	b.deletedLoaded = true
	if msg.Err != nil {
		b.deletedErr = msg.Err
		return
	}
	b.deletedErr = nil
	b.deletedVersions = msg.Versions
	b.redetectDeleted()
}

// redetectDeleted derives the deleted set from the retained scan: every path
// any scanned version touched, minus the paths HEAD still has on disk
// (b.memories, the current listing). The result is the memories that once
// existed here but no longer do — recoverable via their History screen (spec
// §6). Split from adoptDeletedScan so a RefreshMsg can re-run just the
// subtraction over the freshly relisted on-disk set, no daemon round trip, and
// so a restored memory stops being listed as deleted within a tick.
func (b *Browser) redetectDeleted() {
	onDisk := make(map[string]bool, len(b.memories))
	for _, memory := range b.memories {
		onDisk[memory.RepoPath] = true
	}
	seen := make(map[string]bool)
	var deleted []string
	for _, version := range b.deletedVersions {
		for _, repoPath := range version.Paths {
			if onDisk[repoPath] || seen[repoPath] {
				continue
			}
			seen[repoPath] = true
			deleted = append(deleted, repoPath)
		}
	}
	sort.Strings(deleted)
	b.deletedPaths = deleted
	b.deletedCursor = clampCursor(b.deletedCursor, len(deleted))
}

// openDeletedHistory pushes the selected deleted path's History screen, or
// nothing with no row. The screen has no listing snapshot — the memory is gone
// from HEAD — so Memory is zero and the header falls back to the path's base
// name. Live is bound to the CURRENT on-disk file via memoryfs.LiveContent (the
// same LocalTarget mapping restore writes through), not a frozen empty: while
// the memory is still deleted it reads absent and diff-vs-live shows the whole
// blob as removed, but the moment a restore lands the file the diff reflects
// its real content — no stale empty side outliving the resurrection it exists
// to show.
func (b *Browser) openDeletedHistory() tea.Cmd {
	if b.deletedCursor < 0 || b.deletedCursor >= len(b.deletedPaths) {
		return nil
	}
	repoPath := b.deletedPaths[b.deletedCursor]
	folder, units := b.deps.Folder, b.deps.Units
	history := NewHistory(HistoryDeps{
		Folder:   folder,
		RepoPath: repoPath,
		Live:     func() (string, error) { return memoryfs.LiveContent(units, folder, repoPath) },
		Data:     b.deps.Data,
		Render:   b.deps.Render,
		Styles:   b.deps.Styles,
		Now:      b.now,
	})
	return func() tea.Msg { return PushScreenMsg{Screen: history} }
}

func (b *Browser) moveDeletedCursor(key string) {
	switch key {
	case "up", "k":
		if b.deletedCursor > 0 {
			b.deletedCursor--
		}
	case "down", "j":
		if b.deletedCursor < len(b.deletedPaths)-1 {
			b.deletedCursor++
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
		// Within a provider the primary index (claude's MEMORY.md, codex's
		// memories/MEMORY.md) always sorts first: it is the map of the group, so
		// it is the memory to open first, and cursor 0 then makes it the default
		// selection. Keyed on the provider-declared index memoryfs marked via
		// PrimaryIndexPath — a display fact, never a name match and never a merge
		// class — so it holds for every provider that declares an index and stays
		// on top under both order modes o toggles between (recency would sink an
		// older index, name order an alphabetically-late one).
		if iIndex, jIndex := isIndex(filtered[i]), isIndex(filtered[j]); iIndex != jIndex {
			return iIndex
		}
		if b.orderByRecency {
			return filtered[i].ModTime.After(filtered[j].ModTime)
		}
		return strings.ToLower(filtered[i].Name) < strings.ToLower(filtered[j].Name)
	})
	return filtered
}

// isIndex reports whether m is its provider's primary index file (claude's
// MEMORY.md, codex's memories/MEMORY.md), marked as such by memoryfs at
// enumeration when its path equals the provider's PrimaryIndexPath (spec §6).
// visibleRows sorts these first within their provider group off this display
// identity, never a name match and never a merge class — so the rule holds for
// every provider that declares an index regardless of the Class its file
// carries (codex's index is ClassRegenerated, not ClassDerivedIndex).
func isIndex(m memoryfs.Memory) bool {
	return m.IsIndex
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
	// Cleared here and set true only in the preview-split branch below, so a
	// path that draws no preview (deleted list, error/loading/empty/no-match
	// notices, or narrow no-preview mode) leaves the scroll keys inert
	// (updateKey reads this).
	b.previewShown = false
	// Clear this frame's click hit-map for the same last-rendered-frame reason:
	// renderList refills it below for the frames that draw the list, and a frame
	// that returns early with a notice — or the deleted view just below — leaves
	// it empty, so a click on such a frame maps to no row.
	b.listLineRows = nil
	b.listTopLines = 0
	if b.showDeleted {
		return b.deletedView(width, height)
	}
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

	rowBudget := b.listRowBudget(rows, height)
	// The list block is written next in both branches below, and body — the title
	// and its blank, plus the filter line and its blank when open — is identical
	// for each at this point, so record the browser-body lines above the list once
	// here. body ends on a blank join line, so its newline count is the 0-based
	// index of the list's first line; updateMouseClick subtracts this to turn a
	// screen-local click Y into a hit-map index. Same last-rendered-frame contract
	// as the map itself.
	b.listTopLines = strings.Count(body.String(), "\n")
	if width < previewMinWidth {
		// No preview pane: the list owns the full content width, so fit rows to
		// it directly.
		body.WriteString(b.renderList(rows, rowBudget, width))
		return strings.TrimRight(body.String(), "\n")
	}

	// Preview split: the list is confined to listPaneWidth, so rows are fit to
	// that — the MaxWidth pane then has nothing to wrap. The preview is a
	// height-bounded scrollable viewport, so a long body scrolls within
	// the pane instead of growing the JoinHorizontal block past the height
	// budget and shoving the root's footer off the terminal.
	b.previewShown = true
	previewWidth := width - listPaneWidth - 2
	preview := b.renderPreviewPane(rows[b.cursor], previewWidth, height)
	listBlock := lipgloss.NewStyle().Width(listPaneWidth).MaxWidth(listPaneWidth).Render(b.renderList(rows, rowBudget, listPaneWidth))
	previewBlock := lipgloss.NewStyle().Width(previewWidth).MaxWidth(previewWidth).Render(preview)
	body.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, listBlock, "  ", previewBlock))
	return strings.TrimRight(body.String(), "\n")
}

// deletedView renders the deleted-recovery list: a title, then a scan error, a
// scanning notice, an empty notice, or the cursor-windowed deleted paths. Rows
// are windowed to the height budget the same way the memory list is, so a long
// deletion history never overflows the pane.
func (b *Browser) deletedView(width, height int) string {
	var body strings.Builder
	body.WriteString(sectionTitle(b.deps.Styles, "Deleted memories: "+b.deps.Folder))
	body.WriteString("\n\n")

	// A scan that came back at the fetch cap read only the newest slice of
	// history, so the deleted set below (and any "nothing deleted" claim) is
	// bounded to that slice, not the whole timeline — disclosed either way.
	truncated := len(b.deletedVersions) == historyVersionLimit

	switch {
	case b.deletedErr != nil:
		fmt.Fprintf(&body, "history unavailable: %v", b.deletedErr)
		return strings.TrimRight(body.String(), "\n")
	case !b.deletedLoaded:
		body.WriteString(b.deps.Styles.Dim.Render("scanning history…"))
		return strings.TrimRight(body.String(), "\n")
	case len(b.deletedPaths) == 0:
		if truncated {
			body.WriteString(b.deps.Styles.Dim.Render("no deleted memories in the newest " +
				strconv.Itoa(historyVersionLimit) + " commits — older history not scanned"))
		} else {
			body.WriteString(b.deps.Styles.Dim.Render("no deleted memories in this project's history"))
		}
		return strings.TrimRight(body.String(), "\n")
	}

	budget := max(height-2, 1) // title line + its trailing blank
	if truncated {
		budget = max(budget-1, 1) // the disclosure row
	}
	start, end := visibleWindow(b.deletedCursor, len(b.deletedPaths), budget)
	lines := make([]string, 0, end-start+1)
	for row := start; row < end; row++ {
		marker := "  "
		if row == b.deletedCursor {
			marker = "> "
		}
		// A deleted path is a plain name with no badge/description/age, fit to
		// width through the same shared row helper the live list uses so a long
		// nested path never soft-wraps past this pane's own height budget.
		line := fitListRow(b.deps.Styles, width, marker, b.deletedPaths[row], "", "", "")
		if row == b.deletedCursor {
			line = b.deps.Styles.Selected.Render(line)
		}
		lines = append(lines, line)
	}
	if truncated {
		lines = append(lines, b.deps.Styles.Dim.Render(fitWidth(historyTruncationNotice(historyVersionLimit), width)))
	}
	body.WriteString(strings.Join(lines, "\n"))
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
	budget := height - b.chromeLines() - countDistinctProviders(rows)
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

// chromeLines is the fixed vertical overhead every non-deleted browser body
// renders above its list/preview area: the title line and its trailing blank,
// plus the in-browser filter line and its own trailing blank whenever the
// filter is open or holds a query. Both the list windowing (listRowBudget) and
// the preview pane (renderPreviewPane) size their content areas by subtracting
// it from the height the root passes, so the two panes share one definition of
// the chrome above them — the split's height contract, chromeLines + max(list,
// preview) <= height, then holds by construction rather than by two call sites
// happening to subtract the same number. The preview pane's own header zone
// (previewHeaderLines — a leading alignment blank, plus any lint-issue lines)
// is additional overhead on top of this, subtracted separately by
// renderPreviewPane alone: it is local to the pane's own content and, unlike
// this shared chrome, never shrinks the list's row budget.
func (b *Browser) chromeLines() int {
	chrome := 2 // title line + its trailing blank
	if b.filtering || b.filter.Value() != "" {
		chrome += 2 // filter line + its own trailing blank
	}
	return chrome
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
func (b *Browser) renderList(rows []memoryfs.Memory, rowBudget, width int) string {
	start, end := visibleWindow(b.cursor, len(rows), rowBudget)
	rows = rows[start:end]

	var lines []string
	// lineRows records, per emitted line, the visibleRows index it shows so a
	// click can map back to a row (updateMouseClick): -1 for a provider header,
	// and the row's ABSOLUTE index (start+i — the space b.cursor and Selected read,
	// never the window-relative i) for a memory line, so a click in a scrolled list
	// still lands on the memory drawn there. Built in lockstep with lines and
	// published to the field View reset for this frame.
	lineRows := make([]int, 0, len(rows)+1)
	lastProvider := ""
	for i, m := range rows {
		row := start + i
		if m.Provider != lastProvider {
			lines = append(lines, b.deps.Styles.Header.Render(fitWidth(m.Provider, width)))
			lineRows = append(lineRows, -1)
			lastProvider = m.Provider
		}
		marker := "  "
		if row == b.cursor {
			marker = "> "
		}
		badge := ""
		if b.lintFlags[m.RepoPath] {
			badge = "⚠"
		}
		line := fitListRow(b.deps.Styles, width, marker, m.Name, badge, m.Description, relativeTime(m.ModTime, b.now))
		if row == b.cursor {
			line = b.deps.Styles.Selected.Render(line)
		}
		lines = append(lines, line)
		lineRows = append(lineRows, row)
	}
	b.listLineRows = lineRows
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

// renderPreviewPane renders the selected memory into the height-bounded,
// scrollable preview viewport and returns the pane's lines, prefixed by a
// header zone (previewHeaderLines): an unconditional leading blank line,
// keeping the pane's first body line level with the list column's first row
// — renderList's own top-of-window provider header always claims the list's
// row 0, so the preview needs the same one-line offset for the two columns to
// align — followed by one Warn-styled reason line per lint issue on the
// hovered memory, if any. The header zone itself always renders — it is the
// BODY that adds nothing when empty: a body that fits sizes the viewport to
// its own content, so a short preview lets the list drive the joined height
// and an empty body contributes nothing past the header zone above it; a
// body taller than the pane is bounded to the height budget and scrolls in
// place, its bottom line spent on an overflow hint. Bounding the pane is the
// whole fix: without it a long memory grew the
// JoinHorizontal block past height and shoved the root's footer — the option
// keys — off the terminal, hiding both the keys and the text past the fold
// (the exact defect live-hub testing surfaced).
//
// width is the preview column's width (the glamour render/wrap width, and the
// header zone's own truncation width); height is the whole browser-body
// budget the root passed, from which the chrome above the split (chromeLines)
// AND the header zone's own line count are both subtracted for the pane's
// remaining viewport height — the header renders unconditionally, so it must
// come out of the same budget the viewport is bounded to, never sit on top of
// it.
func (b *Browser) renderPreviewPane(selected memoryfs.Memory, width, height int) string {
	header := b.previewHeaderLines(selected, width)
	paneHeight := max(height-b.chromeLines()-len(header), 1)
	b.previewViewport.SetWidth(width)
	b.syncPreview(selected, width)

	headerBlock := strings.Join(header, "\n")

	total := b.previewViewport.TotalLineCount()
	if total <= paneHeight {
		// The body fits: size the viewport to the content itself so the pane is
		// only as tall as its lines — never space-filled out to the full budget,
		// which would strand a one-row list beside a wall of blank preview and
		// let the empty pane, not the list, set the joined height.
		b.previewViewport.SetHeight(total)
		return headerBlock + "\n" + b.previewViewport.View()
	}
	// The body overflows: bound the viewport to the budget so it scrolls within
	// the pane instead of growing the frame past height. Reserve the bottom line
	// for the scroll hint whenever the pane is at least two lines tall (one for
	// content, one for the hint); a one-line pane keeps its single line for
	// content rather than showing only an affordance.
	if paneHeight < 2 {
		b.previewViewport.SetHeight(paneHeight)
		return headerBlock + "\n" + b.previewViewport.View()
	}
	b.previewViewport.SetHeight(paneHeight - 1)
	return headerBlock + "\n" + b.previewViewport.View() + "\n" + b.previewScrollHint(width)
}

// previewHeaderLines composes the preview pane's header zone: an unconditional
// leading chrome line — the focus cue while the preview holds focus
// (previewCueLine), otherwise an empty alignment-padding line — so the pane's
// first body line always sits one row below the pane's top, level with the list
// column's first row (always a provider-header line; see renderList's own doc).
// The cue occupies that SAME single row the padding blank otherwise would, so
// focusing the pane changes what the top row says without moving the body: the
// Task-2 alignment invariant and the pane's height budget hold identically
// whether or not the preview is focused. One Warn-styled "⚠ <Rule>: <Detail>"
// line then follows per lint issue recorded against selected (b.lintIssues,
// keyed by RepoPath), each measured and ansi-truncated to width as PLAIN text
// before styling — exactly fitListRow's rule for its own badge — so a long
// Detail sentence can never wrap the pane. More than three issues collapse the
// remainder into one trailing "⚠ +N more" line rather than growing the header
// zone without bound, which would crowd the body entirely out of a short pane.
//
// Returned as a slice, not pre-joined, so renderPreviewPane can both render it
// and size the viewport's remaining budget off its exact line count (len) —
// the two can never disagree — and so a later line added to this zone is one
// more slice entry, not a change to the height arithmetic.
func (b *Browser) previewHeaderLines(selected memoryfs.Memory, width int) []string {
	const maxShownIssues = 3
	lines := []string{b.previewCueLine(width)}
	issues := b.lintIssues[selected.RepoPath]
	if len(issues) == 0 {
		return lines
	}
	shown, overflow := issues, 0
	if len(issues) > maxShownIssues {
		shown, overflow = issues[:maxShownIssues], len(issues)-maxShownIssues
	}
	for _, issue := range shown {
		reason := fitWidth(fmt.Sprintf("⚠ %s: %s", issue.Rule, issue.Detail), width)
		lines = append(lines, b.deps.Styles.Warn.Render(reason))
	}
	if overflow > 0 {
		lines = append(lines, b.deps.Styles.Warn.Render(fitWidth(fmt.Sprintf("⚠ +%d more", overflow), width)))
	}
	return lines
}

// previewFocusCue is the text of the pane's focus cue, before styling and width
// truncation. Kept a package constant so the render and its tests name the same
// string. It leads with an accent glyph and states the ONE thing the live-hub
// "freeze" report proved a focused reader could not tell: that the preview holds
// the keys now and esc (or tab) hands them back to the list — the full focused
// keyset lives in the footer, so the cue stays a short "you are here, here is the
// way out" banner rather than repeating it.
const previewFocusCue = "▶ preview focused — esc/tab returns to list"

// previewCueLine renders the focus cue for the preview pane's top chrome row, or
// the empty alignment-padding line when the pane is not focused. It is the pane's
// half of the focus-honesty fix: an earlier design surfaced the focus state only
// through the overflow scroll hint, so a SHORT (fitting) preview — which renders
// no scroll hint at all — entered focus with no on-screen cue and every keypress
// read as dead (the reported "freeze"). Rendering the cue here, from the header
// zone that ALWAYS renders, discloses focus for a fitting and an overflowing
// preview alike; the cue occupies the same single row the alignment blank
// otherwise would, so it costs no extra pane height. Accent-styled (Header, the
// same emphasis the reading view gives its own chrome) so it reads as a state
// banner, not body text. Gated on PreviewFocused (not the bare previewFocused
// bool) so a focus stranded off-screen by a narrow resize shows no cue, matching
// the key routing.
func (b *Browser) previewCueLine(width int) string {
	if !b.PreviewFocused() {
		return ""
	}
	return b.deps.Styles.Header.Render(fitWidth(previewFocusCue, width))
}

// syncPreview brings the preview viewport's content up to date with the
// selected memory at the given render width, gated by previewCache so the
// glamour render — and the body read behind it — runs only on an actual change.
// View runs on every keypress and every ~2s RefreshMsg tick, so without this
// gate each of those would re-read the selected body (up to ReadBody's size cap)
// and re-run glamour over it even when nothing changed; on a cache hit the
// viewport already holds the render and there is nothing to do.
//
// When the SELECTION changed (a different RepoPath than the viewport last held)
// the pane scrolls back to the top — a newly opened memory always starts at its
// head, never inheriting the previous selection's scroll offset — whereas a
// same-file re-render (a width change, or a rewrite landing a new ModTime) keeps
// the reader where they were. A read/render failure is shown in the pane but
// deliberately never cached (see previewCache's doc): an error is rare enough
// that re-attempting each render costs nothing, and caching it would risk a
// stale notice outliving a since-fixed transient failure — the notice is fed
// through the viewport, not returned bare, so even the error pane obeys the
// height budget.
func (b *Browser) syncPreview(selected memoryfs.Memory, width int) {
	if b.preview.valid && b.preview.repoPath == selected.RepoPath &&
		b.preview.modTime.Equal(selected.ModTime) && b.preview.width == width {
		return
	}
	selectionChanged := b.preview.repoPath != selected.RepoPath

	content, err := b.deps.ReadBody(selected)
	if err != nil {
		b.previewViewport.SetContent(b.deps.Styles.Fail.Render(fmt.Sprintf("preview unavailable: %v", err)))
		b.preview.valid = false
		if selectionChanged {
			b.previewViewport.GotoTop()
		}
		return
	}

	rendered := content
	if b.deps.Render != nil {
		rendered = b.deps.Render(content, width)
	}
	b.previewViewport.SetContent(rendered)
	if selectionChanged {
		b.previewViewport.GotoTop()
	}
	b.preview = previewCache{
		valid:    true,
		repoPath: selected.RepoPath,
		modTime:  selected.ModTime,
		width:    width,
	}
}

// previewScrollHint is the one-line overflow affordance rendered at the bottom
// of the preview pane when the body is taller than the pane. The reading
// view needs none — its viewport owns the whole screen, so its cut edge IS the
// screen edge — but the browser's preview is one column of a split, with content
// beside and below it and no other signal that text continues past the fold. It
// reports how far through the body the window sits (ScrollPercent) and names the
// scroll keys through the shared scrollHintLine — the same percent-through
// readout and key list the Doctor/Activity tabs render, built in one place so
// the format never drifts between the panes.
//
// It reports scroll POSITION only, in both focus states: the focus cue is the
// header zone's own row now (previewCueLine), so the reader learns the preview
// is focused from the top of the pane whether or not the body overflows — this
// affordance no longer has to double as the focus banner, which is what left a
// short preview with no cue at all when it was the only place focus was ever
// disclosed.
func (b *Browser) previewScrollHint(width int) string {
	percent := int(b.previewViewport.ScrollPercent() * 100)
	return scrollHintLine(b.deps.Styles, percent, width)
}

// fitWidth truncates s to at most width display cells, marking any cut with an
// ellipsis. Display-width-aware (ansi): wide runes and the ellipsis are counted
// as the cells they occupy, unlike the rune-counting truncate this subsumed
// (which split wide runes and mismeasured them against a column budget). It is
// the shared width primitive fitListRow and the provider-header/notice lines
// all fit through, so every line the browser emits obeys one truncation regime.
// width <= 0 yields "".
func fitWidth(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if ansi.StringWidth(s) <= width {
		return s
	}
	return ansi.Truncate(s, width, "…")
}

// fitListRow composes one list row — a marker, a name, an optional lint badge,
// an optional description, and an always-visible trailing relative-age suffix —
// into a single line whose visible (display-cell) width never exceeds width, so
// a fixed-width list pane has nothing to wrap. That is what keeps one logical
// row rendering as exactly one physical line, and with it the height budget's
// rows-equal-lines arithmetic honest (spec §3): the defect this fixes was long
// real memory names folding the fixed-width list pane into a multi-line soup.
//
// The marker and the "(age)" suffix always render in full; the badge renders in
// full when present (a lint ⚠ the user must not miss). The name takes the
// remaining budget first and is ellipsis-truncated if it still overflows, which
// drops the description for that row. Otherwise the description fills what is
// left, prefixed " — " and ellipsis-truncated, and is dropped rather than shown
// as a useless sliver when fewer than descriptionMinWidth columns remain. Every
// segment is measured and cut as PLAIN text, then styled — styled (ANSI-wrapped)
// text is never width-sliced. Callers with no badge/description/age pass "".
func fitListRow(styles theme.Styles, width int, marker, name, badge, description, age string) string {
	ageSuffix := ""
	if age != "" {
		ageSuffix = " (" + age + ")"
	}
	badgeSuffix, badgeRendered := "", ""
	if badge != "" {
		badgeSuffix = " " + badge
		badgeRendered = " " + styles.Warn.Render(badge)
	}
	// The age and badge are reserved off the top: name and description only ever
	// compete for what is left, so the age can never be pushed off the row.
	nameBudget := width - ansi.StringWidth(marker) - ansi.StringWidth(badgeSuffix) - ansi.StringWidth(ageSuffix)

	var out strings.Builder
	out.WriteString(marker)
	if ansi.StringWidth(name) > nameBudget {
		out.WriteString(fitWidth(name, nameBudget)) // name alone overflows: truncate it, drop the description
		out.WriteString(badgeRendered)
	} else {
		out.WriteString(name)
		out.WriteString(badgeRendered)
		descBudget := min(nameBudget-ansi.StringWidth(name)-ansi.StringWidth(" — "), descriptionTruncateAt)
		if description != "" && descBudget >= descriptionMinWidth {
			out.WriteString(" — ")
			out.WriteString(styles.Dim.Render(fitWidth(description, descBudget)))
		}
	}
	out.WriteString(ageSuffix)
	return out.String()
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
