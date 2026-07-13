package views

import (
	"fmt"
	"strings"
	"time"

	keybinding "charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/links"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/memoryfs"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/theme"
)

// backlinksPanelMaxRows caps how many referrer rows the open backlinks panel
// shows at once; longer lists are windowed around the cursor (visibleWindow,
// shared with the browser's list) so the panel can never crowd the viewport
// out of the height budget.
const backlinksPanelMaxRows = 8

// readingModifiedLayout formats the header's absolute modified stamp —
// spec §4 wants the exact instant here, unlike the browser rows' relative
// labels, so a reading view never needs a live clock at all.
const readingModifiedLayout = "2006-01-02 15:04"

// CopyPathMsg asks the root to surface Path — the memory's absolute
// provider-file path — as the reading view's copy affordance (spec §4's y).
// The root toasts the path verbatim (guaranteed to work in every terminal)
// and issues bubbletea's OSC52 clipboard write alongside it as best effort:
// OSC52 support varies by terminal and the write has no delivery ack, so
// the visible toast, not the silent escape sequence, is what the binding
// promises.
type CopyPathMsg struct{ Path string }

// ReadingDeps is everything the reading screen needs, injected once — the
// same consumer-side-seam idiom as BrowserDeps. The pushing browser fills
// it from its own deps, so a reading view and the browser under it always
// agree about how bodies are read and markdown is rendered.
type ReadingDeps struct {
	Memory memoryfs.Memory
	// Index is the pushing browser's own link graph — shared, never
	// rebuilt: link resolution and backlinks both answer from the exact
	// snapshot the browser indexed, and link-to-link navigation threads the
	// same pointer into every deeper Reading. It is therefore as fresh as
	// the browser's last relint, no fresher — acceptable for a drill-in
	// surface whose parent re-indexes on its own next tick after popping
	// back.
	Index *links.Index
	// ReadBody reads one memory's full content — memoryfs.ReadBody in
	// production. Called synchronously at construction and on each
	// RefreshMsg: a local file read, the same documented exception to "no
	// I/O outside a Cmd" that Browser's refresh already relies on.
	ReadBody func(memoryfs.Memory) (string, error)
	// Render markdown-renders a body at a width — the root-owned glamour
	// seam shared with the browser's preview pane. nil (some tests) falls
	// back to the raw display body.
	Render func(string, int) string
	Styles theme.Styles
	// Data is the read-only version surface threaded into any History screen
	// this reading view pushes (spec §6's h). The reading view never fetches
	// through it itself — it only hands it to a constructed History — so a nil
	// Data is harmless: even a History pushed with a nil Data issues no fetch
	// (its versionsCmd/blobCmd nil-guard) and sits on its loading notice rather
	// than nil-dereferencing, so a test that presses h without wiring Data is
	// safe, not just one that never presses it.
	Data HistoryDataSource
	// Now seeds the clock a pushed History screen renders its relative ages
	// against (screen.go's seed-at-construction rule): the reading view has no
	// relative-time surface of its own, but the History it opens does, and that
	// clock must be right on the History's very first frame — before its own
	// first RefreshMsg tick arrives. Advanced by every RefreshMsg the reading
	// view receives, so a jump made many ticks after opening still seeds fresh.
	Now time.Time
}

// Reading is the full-screen reading view for one memory (spec §4): a
// scrollable glamour render with [[wiki-link]] navigation and a backlinks
// panel. Pointer-receiver Screen, mutating in place, for the same reason as
// Browser: a viewport, slices, and a render cache are naturally mutable
// state.
type Reading struct {
	deps ReadingDeps

	body      string
	bodyLinks []links.Link
	loadErr   error

	// linkCursor is the one shared cursor (spec §4): an index into
	// bodyLinks while the backlinks panel is closed, an index into
	// backlinkMemories while it is open, -1 for "nothing selected" in
	// either mode.
	linkCursor    int
	backlinksOpen bool

	// backlinkMemories is resolved once at construction: Index is a fixed
	// snapshot (see ReadingDeps.Index), so its Backlinks answer can never
	// change within this screen's lifetime.
	backlinkMemories []memoryfs.Memory

	viewport viewport.Model

	renderedFor readingRenderState

	// now is the stored clock forwarded to a pushed History screen's seed. It
	// is seeded from ReadingDeps.Now and advanced by every RefreshMsg; the
	// reading view renders nothing relative-time-shaped itself (its header
	// stamp is absolute), so it is used only at the moment h opens a History.
	now time.Time
}

// readingRenderState records the exact inputs the viewport's current
// content was rendered from: the memory's identity and mod time, the render
// width, and the active-link index — the substitution rewrites the body
// around the active link BEFORE rendering, so the cursor is as much a
// render input as the width is. The body itself is not a key component:
// adoptBody (the only place it changes) clears valid explicitly, and
// SetRender clears it unconditionally for the same
// func-values-have-no-equality reason previewCache documents.
type readingRenderState struct {
	valid      bool
	repoPath   string
	modTime    time.Time
	width      int
	activeLink int
}

// NewReading builds a ready Reading and synchronously reads its body —
// construction I/O under the same documented exception as NewBrowser: a
// local file read buys a populated first frame instead of a guaranteed-blank
// one until the first tick.
func NewReading(deps ReadingDeps) *Reading {
	readingViewport := viewport.New()
	readingViewport.KeyMap = readingViewportKeyMap()
	r := &Reading{deps: deps, linkCursor: -1, viewport: readingViewport, now: deps.Now}
	if deps.Index != nil {
		r.backlinkMemories = deps.Index.Backlinks(deps.Memory)
	}
	body, err := deps.ReadBody(deps.Memory)
	if err != nil {
		r.loadErr = err
		return r
	}
	r.adoptBody(body)
	return r
}

// readingViewportKeyMap binds exactly spec §4's scroll keys (j/k and
// ctrl+d/u; pgup/pgdown kept as their conventional aliases). The viewport's
// own defaults would swallow reading-view keys: b (its page-up) is the
// backlinks toggle, u/d (half page) shadow future bindings, f/space page
// keys and h/l horizontal scrolling serve no purpose in a render already
// wrapped to the viewport width — h in particular is history's reserved key
// (Task 14). Keyless bindings never match (bubbles' Enabled contract), so
// Left/Right are disabled outright. g/G are handled by Reading.updateKey
// directly: the viewport has GotoTop/GotoBottom methods but no bindings for
// them.
func readingViewportKeyMap() viewport.KeyMap {
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

// Title names the breadcrumb segment: the memory's display name.
func (r *Reading) Title() string {
	return r.deps.Memory.Name
}

// Memory reports the memory this screen renders — the browser-listing
// snapshot it was opened from. Exported for the root's flow-availability
// gates (fact-class ∧ …), outside the Screen interface for the same reason
// as Browser.Selected: the root reaches the concrete type, the stack
// contract stays Update/View/Title.
func (r *Reading) Memory() memoryfs.Memory {
	return r.deps.Memory
}

// SetStyles installs a new theme — root-propagated via applyStackTheme on a
// background-color swap, outside the Screen interface for the same reason
// as Browser.SetStyles. Styles feed only the header/panel chrome, which
// re-renders on every View, so no cache invalidation is needed here.
func (r *Reading) SetStyles(styles theme.Styles) {
	r.deps.Styles = styles
}

// SetRender installs a new markdown-render seam (the theme-keyed glamour
// renderer), invalidating the render cache unconditionally: a func value
// has no equality check, so clearing here is the only way a theme swap
// reliably forces the next View through the new renderer instead of a
// cached string rendered under the old one.
func (r *Reading) SetRender(render func(md string, width int) string) {
	r.deps.Render = render
	r.renderedFor.valid = false
}

// adoptBody installs body as the screen's current content: links re-parsed,
// the shared cursor clamped against whichever list it currently indexes,
// and the render cache invalidated (body is not part of the cache key — see
// readingRenderState).
func (r *Reading) adoptBody(body string) {
	r.body = body
	r.bodyLinks = links.Parse(body)
	r.loadErr = nil
	if !r.backlinksOpen && r.linkCursor >= len(r.bodyLinks) {
		r.linkCursor = len(r.bodyLinks) - 1 // -1 when the new body has no links
	}
	r.renderedFor.valid = false
}

// Update handles one message. RefreshMsg (the root's tick forward) re-reads
// the body so the open document stays live against writes an external agent
// makes to the same file (screen.go's RefreshMsg contract); its Now is stored
// only to seed a History screen h opens later — nothing the reading view
// itself renders is relative-time-shaped (the header's modified stamp is
// absolute by design).
func (r *Reading) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case RefreshMsg:
		r.now = msg.Now
		r.refreshBody()
		return r, nil
	case tea.KeyPressMsg:
		return r.updateKey(msg)
	}
	return r, nil
}

// refreshBody re-reads the memory and adopts the result if it actually
// changed. A read error keeps the last good body: a transient failure — or
// the file being deleted mid-read — must degrade to "stale but readable",
// never blank an open document (deleted memories remain recoverable via
// history, spec §6, so keeping the content visible is honest). A successful
// read also heals a construction-time load error.
func (r *Reading) refreshBody() {
	body, err := r.deps.ReadBody(r.deps.Memory)
	if err != nil {
		return
	}
	if r.loadErr != nil || body != r.body {
		r.adoptBody(body)
	}
}

func (r *Reading) updateKey(msg tea.KeyPressMsg) (Screen, tea.Cmd) {
	switch {
	case keybinding.Matches(msg, DashboardKeys.ReadingBack):
		// esc closes internal state first: an open backlinks panel is
		// consumed locally; only a bare esc signals the root to pop.
		if r.backlinksOpen {
			r.closeBacklinks()
			return r, nil
		}
		return r, func() tea.Msg { return PopScreenMsg{} }
	case keybinding.Matches(msg, DashboardKeys.ReadingCycleLinks):
		// The binding gates membership; the concrete key picks the
		// direction (the TabSwitch idiom).
		r.cycleCursor(msg.String())
		return r, nil
	case keybinding.Matches(msg, DashboardKeys.ReadingFollow):
		return r, r.follow()
	case keybinding.Matches(msg, DashboardKeys.ReadingBacklinks):
		r.toggleBacklinks()
		return r, nil
	case keybinding.Matches(msg, DashboardKeys.ReadingCopyPath):
		path := r.deps.Memory.Path()
		return r, func() tea.Msg { return CopyPathMsg{Path: path} }
	case keybinding.Matches(msg, DashboardKeys.ReadingEdit):
		// Emit-only, like the browser's e: the root owns the class/editor/
		// session gates and the handoff (screen.go's EditRequestMsg doc).
		memory := r.deps.Memory
		return r, func() tea.Msg { return EditRequestMsg{Memory: memory} }
	case keybinding.Matches(msg, DashboardKeys.ReadingHistory):
		return r, r.openHistory()
	case msg.String() == "g":
		r.viewport.GotoTop()
		return r, nil
	case msg.String() == "G":
		r.viewport.GotoBottom()
		return r, nil
	}
	var cmd tea.Cmd
	r.viewport, cmd = r.viewport.Update(msg)
	return r, cmd
}

// cursorRowCount reports how many rows the shared cursor currently cycles
// over: backlink rows while the panel is open, body links otherwise.
func (r *Reading) cursorRowCount() int {
	if r.backlinksOpen {
		return len(r.backlinkMemories)
	}
	return len(r.bodyLinks)
}

// cycleCursor advances (tab) or retreats (shift+tab) the shared cursor,
// wrapping at both ends. From the -1 "nothing selected" state, tab lands on
// the first row and shift+tab on the last.
func (r *Reading) cycleCursor(key string) {
	count := r.cursorRowCount()
	if count == 0 {
		return
	}
	switch key {
	case "tab":
		r.linkCursor = (r.linkCursor + 1) % count
	case "shift+tab":
		if r.linkCursor <= 0 {
			r.linkCursor = count - 1
		} else {
			r.linkCursor--
		}
	}
}

// follow acts on enter: with the backlinks panel open it jumps to the
// selected referrer; otherwise it resolves the active body link and pushes
// its reading view — or, for a dangling target, produces the toast that
// explains why nothing opened. No active row means nothing to follow.
func (r *Reading) follow() tea.Cmd {
	if r.backlinksOpen {
		if r.linkCursor < 0 || r.linkCursor >= len(r.backlinkMemories) {
			return nil
		}
		return r.pushReading(r.backlinkMemories[r.linkCursor])
	}
	if r.linkCursor < 0 || r.linkCursor >= len(r.bodyLinks) {
		return nil
	}
	target := r.bodyLinks[r.linkCursor].Target
	resolved, ok := r.resolve(target)
	if !ok {
		text := fmt.Sprintf("dangling link: no memory named %q", target)
		return func() tea.Msg { return ToastMsg{Text: text} }
	}
	return r.pushReading(resolved)
}

// resolve looks target up in the shared index; a nil index (only ever a
// stripped-down test fixture — the browser always passes its own) resolves
// nothing, which renders every link dangling rather than pretending links
// work without a graph to answer from.
func (r *Reading) resolve(target string) (memoryfs.Memory, bool) {
	if r.deps.Index == nil {
		return memoryfs.Memory{}, false
	}
	return r.deps.Index.Resolve(target)
}

// pushReading builds the target's reading view over the SAME shared deps —
// only the memory changes — and hands it to the root as a push: the
// navigation stack IS the history (spec §4), so each jump is one more level
// esc walks back out of.
func (r *Reading) pushReading(target memoryfs.Memory) tea.Cmd {
	deps := r.deps
	deps.Memory = target
	next := NewReading(deps)
	return func() tea.Msg { return PushScreenMsg{Screen: next} }
}

// openHistory pushes the current memory's version-history screen (spec §6's
// h). Its Live seam reads the memory's provider file through the shared
// ReadBody, so the diff-vs-live view sees exactly what this reading view
// shows; Now seeds the History's relative-age clock from the reading view's
// own stored tick (see the now field). Constructing the History here — no I/O,
// its version fetch runs later as the root-issued InitCmd — mirrors how
// pushReading builds the next Reading.
func (r *Reading) openHistory() tea.Cmd {
	memory := r.deps.Memory
	history := NewHistory(HistoryDeps{
		Memory:   memory,
		Folder:   memory.Folder,
		RepoPath: memory.RepoPath,
		Live:     func() (string, error) { return r.deps.ReadBody(memory) },
		Data:     r.deps.Data,
		Render:   r.deps.Render,
		Styles:   r.deps.Styles,
		Now:      r.now,
	})
	return func() tea.Msg { return PushScreenMsg{Screen: history} }
}

// toggleBacklinks opens or closes the backlinks panel, re-seeding the
// shared cursor for whichever list it now indexes: the first referrer on
// open (or -1 for none), and no active body link on close — the previous
// body-link selection is deliberately not restored, because the one cursor
// is shared, not saved per mode (spec §4's "cursor shared with linkCursor
// while open").
func (r *Reading) toggleBacklinks() {
	if r.backlinksOpen {
		r.closeBacklinks()
		return
	}
	r.backlinksOpen = true
	r.linkCursor = -1
	if len(r.backlinkMemories) > 0 {
		r.linkCursor = 0
	}
}

func (r *Reading) closeBacklinks() {
	r.backlinksOpen = false
	r.linkCursor = -1
}

// activeLinkIndex reports which body link the substitution should mark
// active: the shared cursor only indexes bodyLinks while the panel is
// closed — while it is open the cursor means a backlink row, so no body
// link is active at all.
func (r *Reading) activeLinkIndex() int {
	if r.backlinksOpen {
		return -1
	}
	return r.linkCursor
}

// View renders the header line, the backlinks panel when open, and the
// body viewport in the remaining height. width/height come fresh from the
// root on every call, so a resize is handled by construction.
func (r *Reading) View(width, height int) string {
	var view strings.Builder
	view.WriteString(r.headerLine())
	view.WriteString("\n\n")
	chromeLines := 2

	if r.backlinksOpen {
		panel := r.renderBacklinksPanel()
		view.WriteString(panel)
		view.WriteString("\n\n")
		// The "\n\n" adds ONE blank line, not two: the first newline
		// terminates the panel's last line, only the second opens a blank.
		panelLineCount := strings.Count(panel, "\n") + 1
		chromeLines += panelLineCount + 1
	}

	if r.loadErr != nil {
		view.WriteString(r.deps.Styles.Fail.Render(fmt.Sprintf("memory unavailable: %v", r.loadErr)))
		return view.String()
	}

	// One viewport row survives even below the chrome floor
	// (height < chromeLines+1): a zero-height viewport would render the
	// document as nothing at all, which is the one outcome worse than
	// overflowing an already-impossible budget.
	r.viewport.SetWidth(width)
	r.viewport.SetHeight(max(height-chromeLines, 1))
	r.ensureRendered(width)
	view.WriteString(r.viewport.View())
	return strings.TrimRight(view.String(), "\n")
}

// headerLine summarizes the frontmatter facts in one line (spec §4): name ·
// class · absolute modified stamp · human size. All values come from the
// browser-listing snapshot the screen was opened from; the body underneath
// is what RefreshMsg keeps live, and popping back to the browser re-lists
// the metadata on its next tick.
func (r *Reading) headerLine() string {
	memory := r.deps.Memory
	modified := "—"
	if !memory.ModTime.IsZero() {
		modified = memory.ModTime.Format(readingModifiedLayout)
	}
	details := fmt.Sprintf(" · %s · modified %s · %s",
		memory.Class, modified, humanReadableSize(memory.Size))
	return sectionTitle(r.deps.Styles, memory.Name) + r.deps.Styles.Dim.Render(details)
}

// humanReadableSize renders a byte count in the coarsest binary unit that
// keeps it short — memory files are capped at 1 MiB by ReadBody, so this
// tops out around MiB in practice, but the loop is total anyway.
func humanReadableSize(byteCount int64) string {
	const unit = 1024
	if byteCount < unit {
		return fmt.Sprintf("%d B", byteCount)
	}
	divisor, exponent := int64(unit), 0
	for scaled := byteCount / unit; scaled >= unit; scaled /= unit {
		divisor *= unit
		exponent++
	}
	return fmt.Sprintf("%.1f %ciB", float64(byteCount)/float64(divisor), "KMGTPE"[exponent])
}

// renderBacklinksPanel renders the referrer list under the header: a title,
// then either a guidance line or the cursor-windowed rows, each naming the
// referrer and its repo path.
func (r *Reading) renderBacklinksPanel() string {
	lines := []string{r.deps.Styles.Header.Render("Backlinks")}
	if len(r.backlinkMemories) == 0 {
		lines = append(lines, r.deps.Styles.Dim.Render("no memories link here"))
		return strings.Join(lines, "\n")
	}
	start, end := visibleWindow(r.linkCursor, len(r.backlinkMemories), backlinksPanelMaxRows)
	for row := start; row < end; row++ {
		referrer := r.backlinkMemories[row]
		marker := "  "
		if row == r.linkCursor {
			marker = "> "
		}
		line := marker + referrer.Name + " " + r.deps.Styles.Dim.Render("("+referrer.RepoPath+")")
		if row == r.linkCursor {
			line = r.deps.Styles.Selected.Render(line)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// ensureRendered brings the viewport's content up to date with the current
// (body, width, active link) and, when the active link just moved, scrolls
// its line into view — an off-screen highlight would make tab look broken.
// Cache-hit renders cost nothing: View runs on every keypress and every
// ~2s tick, and a glamour render over a full body is the expensive step.
func (r *Reading) ensureRendered(width int) {
	activeLink := r.activeLinkIndex()
	if r.renderedFor.valid && r.renderedFor.width == width &&
		r.renderedFor.activeLink == activeLink &&
		r.renderedFor.repoPath == r.deps.Memory.RepoPath &&
		r.renderedFor.modTime.Equal(r.deps.Memory.ModTime) {
		return
	}
	cursorMoved := r.renderedFor.valid && r.renderedFor.activeLink != activeLink

	display := substituteLinks(r.body, r.bodyLinks, r.resolve, activeLink)
	rendered := display
	if r.deps.Render != nil {
		rendered = r.deps.Render(display, width)
	}
	r.viewport.SetContent(rendered)
	r.renderedFor = readingRenderState{
		valid:      true,
		repoPath:   r.deps.Memory.RepoPath,
		modTime:    r.deps.Memory.ModTime,
		width:      width,
		activeLink: activeLink,
	}
	if cursorMoved && activeLink >= 0 {
		r.scrollToActiveLink(rendered)
	}
}

// scrollToActiveLink brings the first rendered line carrying the active
// marker into the viewport. Searching the RENDERED text for the marker rune
// is deliberate: mapping a byte offset in the markdown source through
// glamour's re-wrapping is exactly the fragile positional bookkeeping the
// pre-render substitution exists to avoid, and ▶ appearing in a body's own
// prose merely scrolls to that earlier line — a cosmetic miss, not a
// correctness one.
func (r *Reading) scrollToActiveLink(rendered string) {
	for lineIndex, line := range strings.Split(rendered, "\n") {
		if strings.Contains(line, activeLinkOpenMarker) {
			r.viewport.EnsureVisible(lineIndex, 0, 0)
			return
		}
	}
}

// Active-link markers, substituted around the target BEFORE markdown
// rendering (substituteLinks) — spec §4's "highlighted in the render".
const (
	activeLinkOpenMarker  = "▶"
	activeLinkCloseMarker = "◀"
)

// substituteLinks rewrites body's [[target]] spans for display, BEFORE the
// markdown render. Rewriting the source is the only reliable channel
// through glamour (verified against charm.land/glamour/v2 v2.0.1):
//
//   - a post-render overlay would have to re-locate each link inside
//     glamour's re-wrapped, ANSI-decorated output — fragile positional
//     bookkeeping that breaks on any style change;
//   - raw ANSI in the SOURCE is no better: glamour escapes the ESC byte,
//     splitting the sequence into visible garbage, so the theme's inverse/
//     strikethrough styles cannot be injected directly.
//
// Plain-text markers and GFM strikethrough both survive rendering intact,
// so per span:
//
//	resolved, inactive  →  left verbatim ("[[target]]")
//	dangling            →  "~~target~~ (dangling)" — glamour's themed
//	                       strikethrough style renders the strike
//	active              →  wrapped "▶…◀" around either form above
//
// resolve is a func seam (not *links.Index) so the caller decides what a
// missing index means.
func substituteLinks(body string, bodyLinks []links.Link, resolve func(string) (memoryfs.Memory, bool), activeLink int) string {
	if len(bodyLinks) == 0 {
		return body
	}
	var display strings.Builder
	display.Grow(len(body))
	previousEnd := 0
	for i, link := range bodyLinks {
		display.WriteString(body[previousEnd:link.Start])

		_, resolved := resolve(link.Target)
		span := body[link.Start:link.End]
		if !resolved {
			span = "~~" + link.Target + "~~ (dangling)"
		}
		if i == activeLink {
			if resolved {
				span = link.Target // brackets replaced by the markers
			}
			span = activeLinkOpenMarker + span + activeLinkCloseMarker
		}
		display.WriteString(span)
		previousEnd = link.End
	}
	display.WriteString(body[previousEnd:])
	return display.String()
}
